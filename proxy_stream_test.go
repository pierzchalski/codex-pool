package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestProxyStreamedRequestClaude(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	receivedCh := make(chan int64, 1)
	keyCh := make(chan string, 1)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		receivedCh <- int64(len(body))
		keyCh <- r.Header.Get("X-Api-Key")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer upstream.Close()

	baseURL, _ := url.Parse(upstream.URL)
	codex := NewCodexProvider(baseURL, baseURL, baseURL)
	claude := NewClaudeProvider(baseURL)
	gemini := NewGeminiProvider(baseURL, baseURL)
	registry := NewProviderRegistry(codex, claude, gemini)

	acc := &Account{Type: AccountTypeClaude, ID: "claude_test", AccessToken: "sk-ant-api-test"}
	pool := newPoolState([]*Account{acc}, false)

	h := &proxyHandler{
		cfg: &config{
			requestTimeout:       5 * time.Second,
			streamTimeout:        5 * time.Second,
			maxInMemoryBodyBytes: 1024,
		},
		transport: http.DefaultTransport,
		pool:      pool,
		registry:  registry,
		metrics:   newMetrics(),
		recent:    newRecentErrors(5),
	}

	proxy := httptest.NewServer(h)
	defer proxy.Close()

	body := bytes.Repeat([]byte("a"), 2048)
	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "stream-user"))

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	select {
	case got := <-receivedCh:
		if got != int64(len(body)) {
			t.Fatalf("upstream received %d bytes, want %d", got, len(body))
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream body size")
	}

	select {
	case got := <-keyCh:
		if got == "" {
			t.Fatalf("expected X-Api-Key to be set for Claude API key")
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream header")
	}
}

func TestProxyCodexOpenRouterRequestRewritesModelAndAuth(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	type upstreamReq struct {
		path      string
		auth      string
		accountID string
		body      []byte
	}

	upstreamReqCh := make(chan upstreamReq, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamReqCh <- upstreamReq{
			path:      r.URL.Path,
			auth:      r.Header.Get("Authorization"),
			accountID: r.Header.Get("ChatGPT-Account-ID"),
			body:      body,
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"resp_123","object":"response","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	baseURL, _ := url.Parse(upstream.URL)
	codex := NewCodexProvider(baseURL, baseURL, baseURL)
	claude := NewClaudeProvider(baseURL)
	gemini := NewGeminiProvider(baseURL, baseURL)
	registry := NewProviderRegistry(codex, claude, gemini)

	acc := &Account{
		Type:        AccountTypeCodex,
		ID:          "openrouter",
		Backend:     AccountBackendOpenRouter,
		AccessToken: "sk-or-test",
		BaseURL:     upstream.URL,
		PlanType:    "api",
	}
	pool := newPoolState([]*Account{acc}, false)

	h := &proxyHandler{
		cfg: &config{
			requestTimeout:       5 * time.Second,
			maxInMemoryBodyBytes: 1024,
		},
		transport: http.DefaultTransport,
		pool:      pool,
		registry:  registry,
		metrics:   newMetrics(),
		recent:    newRecentErrors(5),
	}

	proxy := httptest.NewServer(h)
	defer proxy.Close()

	user := &PoolUser{
		ID:        "abcdef1234567890abcdef1234567890",
		Email:     "test@example.com",
		PlanType:  "pro",
		CreatedAt: time.Now(),
	}
	auth, err := generateCodexAuth("test-secret", user)
	if err != nil {
		t.Fatalf("generateCodexAuth: %v", err)
	}

	req, err := http.NewRequest(http.MethodPost, proxy.URL+"/v1/responses", bytes.NewReader([]byte(`{"model":"gpt-5.4-mini","input":"hi"}`)))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+auth.Tokens.AccessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}

	select {
	case got := <-upstreamReqCh:
		if got.path != "/responses" {
			t.Fatalf("upstream path = %q, want /responses", got.path)
		}
		if got.auth != "Bearer sk-or-test" {
			t.Fatalf("upstream auth = %q", got.auth)
		}
		if got.accountID != "" {
			t.Fatalf("expected ChatGPT-Account-ID to be omitted, got %q", got.accountID)
		}
		var payload map[string]any
		if err := json.Unmarshal(got.body, &payload); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if payload["model"] != "openai/gpt-5.4-mini" {
			t.Fatalf("upstream model = %v", payload["model"])
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for upstream request")
	}
}
