//go:build e2e

package main

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// e2eJWTSecret is the shared secret for pool user JWT tokens in E2E tests.
const e2eJWTSecret = "e2e-test-secret-do-not-use-in-production"

// setE2EJWTSecret sets the POOL_JWT_SECRET env var for the duration of the test.
func setE2EJWTSecret(t *testing.T) {
	t.Helper()
	old := os.Getenv("POOL_JWT_SECRET")
	os.Setenv("POOL_JWT_SECRET", e2eJWTSecret)
	t.Cleanup(func() {
		if old == "" {
			os.Unsetenv("POOL_JWT_SECRET")
		} else {
			os.Setenv("POOL_JWT_SECRET", old)
		}
	})
}

// makePoolUserJWT creates a minimal HMAC-SHA256 JWT that the proxy accepts
// as a valid pool user token. The issuer must be one of the recognized OAuth
// issuers (OpenAI, Google, Anthropic).
func makePoolUserJWT(t *testing.T, issuer string) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256","typ":"JWT"}`))
	claims := map[string]any{
		"iss": issuer,
		"sub": "pool|e2e-test-user",
		"exp": time.Now().Add(1 * time.Hour).Unix(),
		"iat": time.Now().Unix(),
	}
	claimsJSON, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	payload := base64.RawURLEncoding.EncodeToString(claimsJSON)
	signingInput := header + "." + payload
	mac := hmac.New(sha256.New, []byte(e2eJWTSecret))
	mac.Write([]byte(signingInput))
	sig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	return signingInput + "." + sig
}

// ----- credential helpers -----

// claudeSetupToken returns the Claude OAuth token if available and valid for
// direct Anthropic API use. Pool-generated tokens (sk-ant-oat01-pool-*) are
// NOT valid for direct API calls and are skipped.
func claudeSetupToken(t *testing.T) string {
	t.Helper()
	tok := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN")
	if tok == "" {
		t.Skip("CLAUDE_CODE_OAUTH_TOKEN not set; skipping Claude E2E test")
	}
	if !strings.HasPrefix(tok, "sk-ant-oat") {
		t.Skip("CLAUDE_CODE_OAUTH_TOKEN does not look like an OAuth token; skipping")
	}
	// Pool-generated tokens look like sk-ant-oat01-pool-*; these are fake tokens
	// that authenticate against a codex-pool proxy, not the Anthropic API directly.
	if strings.Contains(tok, "-pool-") {
		t.Skip("CLAUDE_CODE_OAUTH_TOKEN is a pool-generated token, not valid for direct API calls; skipping")
	}
	return tok
}

// codexCredPath returns the path to ~/.codex/auth.json if it exists.
func codexCredPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory; skipping Codex E2E test")
	}
	p := filepath.Join(home, ".codex", "auth.json")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("codex credential file not found at %s; skipping", p)
	}
	return p
}

// geminiCredPath returns the path to ~/.gemini/oauth_creds.json if it exists.
func geminiCredPath(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory; skipping Gemini E2E test")
	}
	p := filepath.Join(home, ".gemini", "oauth_creds.json")
	if _, err := os.Stat(p); err != nil {
		t.Skipf("gemini credential file not found at %s; skipping", p)
	}
	return p
}

// ----- pool file creation -----

// writeClaudePoolFile writes a Claude pool account using the setup-token.
// The setup-token is a long-lived access token; it doesn't have a refresh token.
func writeClaudePoolFile(t *testing.T, poolDir string, token string) {
	t.Helper()
	claudeDir := filepath.Join(poolDir, "claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":      token,
			"subscriptionType": "max",
		},
	}
	writeJSON(t, filepath.Join(claudeDir, "e2e-test.json"), data)
}

// writeCodexPoolFile copies the host Codex credentials into the pool directory.
func writeCodexPoolFile(t *testing.T, poolDir, credPath string) {
	t.Helper()
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	codexDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	// Write the codex auth.json as-is; the pool loader handles the format
	if err := os.WriteFile(filepath.Join(codexDir, "e2e-test.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// writeGeminiPoolFile copies the host Gemini credentials into the pool directory.
func writeGeminiPoolFile(t *testing.T, poolDir, credPath string) {
	t.Helper()
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	geminiDir := filepath.Join(poolDir, "gemini")
	if err := os.MkdirAll(geminiDir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(geminiDir, "e2e-test.json"), raw, 0600); err != nil {
		t.Fatal(err)
	}
}

// ----- server helpers -----

// startE2EProxy creates a fully functional codex-pool proxy backed by real
// credentials in tmpdir. Returns the httptest server and tmpdir paths.
func startE2EProxy(t *testing.T, poolDir, stateDir string) *httptest.Server {
	t.Helper()

	// Create data directories for BoltDB and analytics
	dataDir := filepath.Join(stateDir, "data")
	if err := os.MkdirAll(dataDir, 0755); err != nil {
		t.Fatal(err)
	}

	// Build provider registry with real upstream URLs
	codexProvider := NewCodexProvider(
		mustParse("https://chatgpt.com/backend-api/codex"),
		mustParse("https://chatgpt.com/backend-api"),
		mustParse("https://auth.openai.com"),
	)
	claudeProvider := NewClaudeProvider(mustParse("https://api.anthropic.com"))
	geminiProvider := NewGeminiProvider(
		mustParse("https://cloudcode-pa.googleapis.com"),
		mustParse("https://generativelanguage.googleapis.com"),
	)
	kimiProvider := NewKimiProvider(mustParse("https://api.kimi.com"))
	minimaxProvider := NewMinimaxProvider(mustParse("https://api.minimax.io"))
	registry := NewProviderRegistry(codexProvider, claudeProvider, geminiProvider, kimiProvider, minimaxProvider)

	// Load the pool
	accounts, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatalf("loadPool: %v", err)
	}
	if len(accounts) == 0 {
		t.Fatal("loadPool returned 0 accounts")
	}
	t.Logf("loaded %d accounts from pool", len(accounts))
	for _, a := range accounts {
		t.Logf("  account %s: type=%s plan=%s hasAccess=%v hasRefresh=%v",
			a.ID, a.Type, a.PlanType, a.AccessToken != "", a.RefreshToken != "")
	}

	pool := newPoolState(accounts, true /* debug */)

	// Open usage store in tmpdir
	storePath := filepath.Join(dataDir, "proxy.db")
	store, err := newUsageStore(storePath, 30)
	if err != nil {
		t.Fatalf("newUsageStore: %v", err)
	}
	t.Cleanup(func() { store.Close() })

	cfg := &config{
		poolDir:              poolDir,
		stateDir:             stateDir,
		disableRefresh:       false,
		logBodies:            false,
		bodyLogLimit:         16 * 1024,
		maxInMemoryBodyBytes: 16 * 1024 * 1024,
		flushInterval:        200 * time.Millisecond,
		usageRefresh:         0, // disable periodic usage polling in tests
		maxAttempts:          3,
		storePath:            storePath,
		retentionDays:        30,
		tierThreshold:        0.50,
	}
	cfg.debug.Store(true)

	h := &proxyHandler{
		cfg:        cfg,
		transport:  http.DefaultTransport,
		refreshTransport: http.DefaultTransport,
		pool:       pool,
		registry:   registry,
		store:      store,
		pricing:    newPricingData(),
		aliases:    newModelAliases(nil),
		bruteForce: newBruteForceTracker(),
		metrics:    newMetrics(),
		recent:     newRecentErrors(50),
		startTime:  time.Now(),
	}

	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	t.Logf("E2E proxy started at %s", srv.URL)
	return srv
}

// ----- API call helpers -----

// callClaudeAPI sends a minimal /v1/messages request through the proxy.
// Uses claude-haiku as the cheapest model, max_tokens=1 to minimize cost.
func callClaudeAPI(t *testing.T, proxyURL string) *http.Response {
	t.Helper()
	body := map[string]any{
		"model":      "claude-haiku-4-5-20250514",
		"max_tokens": 1,
		"messages": []map[string]string{
			{"role": "user", "content": "Say hi"},
		},
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(bodyJSON))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	// Use pool user JWT for auth (issuer must be Anthropic for Claude path)
	req.Header.Set("Authorization", "Bearer "+makePoolUserJWT(t, "https://auth.anthropic.com"))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Claude API call through proxy failed: %v", err)
	}
	return resp
}

// callGeminiAPI sends a minimal generateContent request through the proxy.
// Uses the /v1internal: path which is the Code Assist API used by Gemini CLI
// OAuth tokens (with cloud-platform scope). The /v1beta/ path requires
// generative-language scope which Gemini CLI OAuth tokens don't have.
// The /v1internal: API uses a custom envelope format where the standard
// request body is wrapped in a "request" field.
func callGeminiAPI(t *testing.T, proxyURL string) *http.Response {
	t.Helper()
	body := map[string]any{
		"model": "models/gemini-2.0-flash",
		"request": map[string]any{
			"contents": []map[string]any{
				{
					"parts": []map[string]string{
						{"text": "Say hi"},
					},
				},
			},
			"generationConfig": map[string]any{
				"maxOutputTokens": 1,
			},
		},
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST",
		proxyURL+"/v1internal:generateContent",
		bytes.NewReader(bodyJSON))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Use pool user JWT for auth (issuer must be Google for Gemini path)
	req.Header.Set("Authorization", "Bearer "+makePoolUserJWT(t, "https://accounts.google.com"))

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Gemini API call through proxy failed: %v", err)
	}
	return resp
}

// ----- utility -----

func writeJSON(t *testing.T, path string, data any) {
	t.Helper()
	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		t.Fatal(err)
	}
}

func readJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var result map[string]any
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// fileExists returns true if the path exists and is a regular file.
func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

// dirFiles returns JSON filenames in a directory.
func dirFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatal(err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".json") {
			names = append(names, e.Name())
		}
	}
	return names
}

// ----- E2E tests -----

// TestE2EClaude tests that the proxy correctly routes Claude API calls
// using a real setup-token, and that the seed file is not modified.
func TestE2EClaude(t *testing.T) {
	token := claudeSetupToken(t)
	setE2EJWTSecret(t)

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	writeClaudePoolFile(t, poolDir, token)

	// Record the seed file content before the test
	seedPath := filepath.Join(poolDir, "claude", "e2e-test.json")
	seedBefore, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	srv := startE2EProxy(t, poolDir, stateDir)

	// Make a cheap Claude API call
	resp := callClaudeAPI(t, srv.URL)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	t.Logf("Claude response status: %d", resp.StatusCode)
	t.Logf("Claude response body (truncated): %.500s", string(body))

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 from Claude API, got %d: %s", resp.StatusCode, string(body))
	}

	// Verify: seed file was NOT modified
	seedAfter, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seedBefore, seedAfter) {
		t.Error("SEED FILE WAS MODIFIED -- this violates the seed/state split contract")
	}

	// Note: Claude setup-token accounts don't have a refresh token,
	// so no state file is expected unless the proxy explicitly creates one.
	// We check if state was created and log it, but don't require it.
	claudeStateDir := filepath.Join(stateDir, "claude")
	stateFiles := dirFiles(t, claudeStateDir)
	t.Logf("Claude state files after API call: %v", stateFiles)
}

// TestE2EGemini tests that the proxy correctly routes Gemini API calls
// through a pool account using real OAuth credentials.
func TestE2EGemini(t *testing.T) {
	credPath := geminiCredPath(t)
	setE2EJWTSecret(t)

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	writeGeminiPoolFile(t, poolDir, credPath)

	// Record the seed file content before the test
	seedPath := filepath.Join(poolDir, "gemini", "e2e-test.json")
	seedBefore, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	srv := startE2EProxy(t, poolDir, stateDir)

	// Make a Gemini API call. The /v1internal: endpoint uses an undocumented
	// envelope format from Google's Code Assist API, so the upstream may
	// return 400/500 for our test request format. What matters for this test
	// is that the proxy:
	// 1. Accepts the pool user JWT
	// 2. Selects the Gemini pool account
	// 3. Forwards the request to cloudcode-pa.googleapis.com (not rejects it)
	// 4. Does not modify the seed file
	resp := callGeminiAPI(t, srv.URL)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	t.Logf("Gemini response status: %d", resp.StatusCode)
	t.Logf("Gemini response body (truncated): %.500s", string(body))

	// The proxy should NOT return 401 (auth rejected) or 404 (no upstream).
	// 502 wrapping a 400/500 from the upstream is acceptable for this test.
	if resp.StatusCode == http.StatusUnauthorized {
		t.Error("proxy rejected request with 401 -- pool user JWT auth failed")
	}
	if resp.StatusCode == http.StatusNotFound {
		t.Error("proxy returned 404 -- no upstream matched the Gemini path")
	}

	// Verify: seed file was NOT modified
	seedAfter, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seedBefore, seedAfter) {
		t.Error("SEED FILE WAS MODIFIED -- this violates the seed/state split contract")
	}

	// Check state file: after a successful proxied request, if refresh was
	// needed (expired access token), a state file should be written.
	geminiStateDir := filepath.Join(stateDir, "gemini")
	stateFiles := dirFiles(t, geminiStateDir)
	t.Logf("Gemini state files after API call: %v", stateFiles)
}

// TestE2EGeminiRefreshWritesState forces a Gemini token refresh by using
// an expired access token, verifying that:
// 1. The refresh succeeds with real credentials
// 2. State is written to stateDir (not poolDir)
// 3. The seed file is not modified
//
// Note: The upstream API call may fail (400/500) due to the undocumented
// Code Assist request format. The test validates state file behavior, not
// upstream API success.
func TestE2EGeminiRefreshWritesState(t *testing.T) {
	credPath := geminiCredPath(t)
	setE2EJWTSecret(t)

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// Read the real credentials
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]any
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatal(err)
	}

	refreshToken, ok := creds["refresh_token"].(string)
	if !ok || refreshToken == "" {
		t.Skip("Gemini credentials missing refresh_token; skipping")
	}

	// Write a seed file with only the refresh token and an EXPIRED access token.
	// This forces the proxy to refresh the token on first request.
	geminiDir := filepath.Join(poolDir, "gemini")
	if err := os.MkdirAll(geminiDir, 0700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"refresh_token": refreshToken,
		"access_token":  "ya29.EXPIRED_PLACEHOLDER",
		"token_type":    "Bearer",
		"expiry_date":   float64(time.Now().Add(-1 * time.Hour).UnixMilli()), // expired 1 hour ago
	}
	seedPath := filepath.Join(geminiDir, "e2e-refresh.json")
	writeJSON(t, seedPath, seed)

	seedBefore, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	srv := startE2EProxy(t, poolDir, stateDir)

	// Make an API call -- this should trigger a token refresh
	resp := callGeminiAPI(t, srv.URL)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	t.Logf("Gemini refresh test response status: %d", resp.StatusCode)
	t.Logf("Gemini refresh test response body (truncated): %.500s", string(body))

	if resp.StatusCode != http.StatusOK {
		// Upstream API errors (400/500) are acceptable for this test --
		// the /v1internal: endpoint has an undocumented request format.
		// The key assertion is that the proxy dispatched the request
		// (not rejected with 401) and wrote state correctly.
		t.Logf("NOTE: upstream returned %d (expected for undocumented API format); checking state file behavior", resp.StatusCode)
	}

	// Verify: seed file was NOT modified
	seedAfter, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seedBefore, seedAfter) {
		t.Error("SEED FILE WAS MODIFIED after token refresh -- violates seed/state split")
	}

	// Verify: state file WAS created in stateDir
	geminiStateDir := filepath.Join(stateDir, "gemini")
	stateFiles := dirFiles(t, geminiStateDir)
	t.Logf("Gemini state files after refresh: %v", stateFiles)

	statePath := filepath.Join(geminiStateDir, "e2e-refresh.json")
	if !fileExists(statePath) {
		t.Error("expected state file at", statePath, "but it does not exist")
	} else {
		stateData := readJSONFile(t, statePath)
		// Verify state file has a fresh access token
		if at, ok := stateData["access_token"].(string); !ok || at == "" {
			t.Error("state file missing access_token")
		} else if at == "ya29.EXPIRED_PLACEHOLDER" {
			t.Error("state file still has the expired placeholder token -- refresh state was not saved")
		} else {
			t.Logf("state file has refreshed access_token (prefix: %.10s...)", at)
		}

		// Verify state file has seed_refresh_hash
		if h, ok := stateData["seed_refresh_hash"].(string); !ok || h == "" {
			t.Error("state file missing seed_refresh_hash")
		} else {
			expectedHash := seedRefreshHash(refreshToken)
			if h != expectedHash {
				t.Errorf("seed_refresh_hash mismatch: got %s, expected %s", h, expectedHash)
			}
		}
	}
}

// TestE2EClaudeOAuthRefreshWritesState tests Claude OAuth token refresh
// with seed/state split, using real credentials from the pool directory.
// This test only runs if we find a Claude OAuth credential with a refresh token.
func TestE2EClaudeOAuthRefreshWritesState(t *testing.T) {
	setE2EJWTSecret(t)
	// This test requires a Claude OAuth credential with a refresh token.
	// The CLAUDE_CODE_OAUTH_TOKEN env var is a setup-token (no refresh token).
	// Check if there's a pool file with OAuth refresh credentials.
	// If not, skip.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skip("cannot determine home directory")
	}

	// Look for a pool file with Claude OAuth credentials that has a refresh token.
	// Try common locations. If none exist, skip.
	var refreshToken string
	candidatePaths := []string{
		filepath.Join(home, ".claude", "claude-oauth.json"),
	}
	for _, p := range candidatePaths {
		raw, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cj ClaudeAuthJSON
		if err := json.Unmarshal(raw, &cj); err != nil {
			continue
		}
		if cj.ClaudeAiOauth != nil && cj.ClaudeAiOauth.RefreshToken != "" {
			refreshToken = cj.ClaudeAiOauth.RefreshToken
			break
		}
	}
	if refreshToken == "" {
		t.Skip("no Claude OAuth credential with refresh_token found; skipping")
	}

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// Write seed with refresh token and expired access token
	claudeDir := filepath.Join(poolDir, "claude")
	if err := os.MkdirAll(claudeDir, 0700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"claudeAiOauth": map[string]any{
			"refreshToken":     refreshToken,
			"accessToken":      "sk-ant-oat01-EXPIRED_PLACEHOLDER",
			"expiresAt":        float64(time.Now().Add(-1 * time.Hour).UnixMilli()),
			"subscriptionType": "max",
		},
	}
	seedPath := filepath.Join(claudeDir, "e2e-refresh.json")
	writeJSON(t, seedPath, seed)

	seedBefore, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	srv := startE2EProxy(t, poolDir, stateDir)

	// Make API call that should trigger refresh
	resp := callClaudeAPI(t, srv.URL)
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	t.Logf("Claude OAuth refresh test response status: %d", resp.StatusCode)
	t.Logf("Claude OAuth refresh test body (truncated): %.500s", string(body))

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 after Claude OAuth refresh, got %d: %s", resp.StatusCode, string(body))
	}

	// Verify seed not modified
	seedAfter, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seedBefore, seedAfter) {
		t.Error("SEED FILE WAS MODIFIED after Claude OAuth refresh -- violates seed/state split")
	}

	// Verify state file
	claudeStateDir := filepath.Join(stateDir, "claude")
	statePath := filepath.Join(claudeStateDir, "e2e-refresh.json")
	if !fileExists(statePath) {
		t.Error("expected state file at", statePath, "but it does not exist")
	} else {
		stateData := readJSONFile(t, statePath)
		oauth, ok := stateData["claudeAiOauth"].(map[string]any)
		if !ok {
			t.Error("state file missing claudeAiOauth")
		} else {
			if at, ok := oauth["accessToken"].(string); !ok || at == "" {
				t.Error("state file missing accessToken")
			} else if at == "sk-ant-oat01-EXPIRED_PLACEHOLDER" {
				t.Error("state file still has expired placeholder -- refresh state not saved")
			}
		}
		if h, ok := stateData["seed_refresh_hash"].(string); !ok || h == "" {
			t.Error("state file missing seed_refresh_hash")
		}
	}
}

// TestE2EHealthz verifies the /healthz endpoint works.
func TestE2EHealthz(t *testing.T) {
	setE2EJWTSecret(t)
	// This is a basic smoke test that doesn't require real credentials.
	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// We need at least one account for the proxy to start. Use a dummy.
	// But the ticket requires real credentials, so let's use Gemini if available.
	credPath := geminiCredPath(t)
	writeGeminiPoolFile(t, poolDir, credPath)

	srv := startE2EProxy(t, poolDir, stateDir)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("healthz returned %d: %s", resp.StatusCode, string(body))
	}

	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["status"] != "ok" {
		t.Fatalf("expected status=ok, got %v", result["status"])
	}
}

// TestE2ECodexPoolLoad verifies that Codex credentials load correctly
// into the pool with the seed/state split.
// Note: We do NOT make an actual Codex API call because the Codex
// backend uses chatgpt.com which requires complex WebSocket/SSE handling
// and is not trivially testable with a simple HTTP POST. Instead we verify
// the account loads and token refresh writes to the correct location.
func TestE2ECodexPoolLoad(t *testing.T) {
	credPath := codexCredPath(t)

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	writeCodexPoolFile(t, poolDir, credPath)

	// Record seed file
	seedPath := filepath.Join(poolDir, "codex", "e2e-test.json")
	seedBefore, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	// Load pool and verify account parsed correctly
	registry := newTestRegistry(t)
	accounts, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatalf("loadPool: %v", err)
	}

	var codexAcc *Account
	for _, a := range accounts {
		if a.Type == AccountTypeCodex {
			codexAcc = a
			break
		}
	}
	if codexAcc == nil {
		t.Fatal("no codex account loaded from pool")
	}

	t.Logf("Codex account loaded: id=%s plan=%s hasAccess=%v hasRefresh=%v stateFile=%s",
		codexAcc.ID, codexAcc.PlanType, codexAcc.AccessToken != "",
		codexAcc.RefreshToken != "", codexAcc.StateFile)

	// Verify seed file was not modified by loadPool
	seedAfter, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seedBefore, seedAfter) {
		t.Error("seed file was modified by loadPool")
	}

	// Verify StateFile is set to the state directory
	expectedStateFile := filepath.Join(stateDir, "codex", "e2e-test.json")
	if codexAcc.StateFile != expectedStateFile {
		t.Errorf("StateFile = %q, expected %q", codexAcc.StateFile, expectedStateFile)
	}

	// Verify the account has expected fields from the credential file
	if codexAcc.AccessToken == "" {
		t.Error("codex account missing access_token")
	}
	if codexAcc.RefreshToken == "" {
		t.Error("codex account missing refresh_token")
	}
	if codexAcc.SeedRefreshToken == "" {
		t.Error("codex account missing seed_refresh_token")
	}
	if codexAcc.SeedRefreshToken != codexAcc.RefreshToken {
		// On cold start (no state file), seed and current should match
		t.Errorf("on cold start, SeedRefreshToken (%q) should equal RefreshToken (%q)",
			codexAcc.SeedRefreshToken, codexAcc.RefreshToken)
	}
}

// TestE2ECodexSaveStateFile verifies that saving a Codex account writes
// state to stateDir, not to the seed file.
func TestE2ECodexSaveStateFile(t *testing.T) {
	credPath := codexCredPath(t)

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	writeCodexPoolFile(t, poolDir, credPath)

	seedPath := filepath.Join(poolDir, "codex", "e2e-test.json")
	seedBefore, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	registry := newTestRegistry(t)
	accounts, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatal(err)
	}

	var codexAcc *Account
	for _, a := range accounts {
		if a.Type == AccountTypeCodex {
			codexAcc = a
			break
		}
	}
	if codexAcc == nil {
		t.Fatal("no codex account loaded")
	}

	// Simulate what happens after a token refresh: update the account
	// and call saveAccount.
	codexAcc.mu.Lock()
	codexAcc.AccessToken = "simulated-new-access-token"
	codexAcc.RefreshToken = "simulated-rotated-refresh-token"
	codexAcc.LastRefresh = time.Now().UTC()
	codexAcc.mu.Unlock()

	if err := saveAccount(codexAcc); err != nil {
		t.Fatalf("saveAccount: %v", err)
	}

	// Verify: seed file NOT modified
	seedAfter, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seedBefore, seedAfter) {
		t.Error("SEED FILE WAS MODIFIED by saveAccount -- violates seed/state split")
	}

	// Verify: state file created
	statePath := filepath.Join(stateDir, "codex", "e2e-test.json")
	if !fileExists(statePath) {
		t.Fatal("state file not created at", statePath)
	}

	stateData := readJSONFile(t, statePath)
	tokens, ok := stateData["tokens"].(map[string]any)
	if !ok {
		t.Fatal("state file missing tokens object")
	}
	if tokens["access_token"] != "simulated-new-access-token" {
		t.Errorf("state access_token = %v", tokens["access_token"])
	}
	if tokens["refresh_token"] != "simulated-rotated-refresh-token" {
		t.Errorf("state refresh_token = %v", tokens["refresh_token"])
	}
	if stateData["seed_refresh_hash"] == nil || stateData["seed_refresh_hash"] == "" {
		t.Error("state file missing seed_refresh_hash")
	}
}

// TestE2EGeminiSeedStateRoundtrip verifies the full load-save-reload cycle:
// 1. Load from seed only (cold start)
// 2. Simulate refresh and save state
// 3. Reload and verify state overrides seed
func TestE2EGeminiSeedStateRoundtrip(t *testing.T) {
	credPath := geminiCredPath(t)

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	writeGeminiPoolFile(t, poolDir, credPath)

	registry := newTestRegistry(t)

	// Step 1: Initial load (cold start, no state)
	accounts, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatal(err)
	}
	var geminiAcc *Account
	for _, a := range accounts {
		if a.Type == AccountTypeGemini {
			geminiAcc = a
			break
		}
	}
	if geminiAcc == nil {
		t.Fatal("no gemini account loaded")
	}

	originalRefresh := geminiAcc.RefreshToken
	t.Logf("Cold start: access=%v refresh=%v stateFile=%s",
		geminiAcc.AccessToken != "", geminiAcc.RefreshToken != "", geminiAcc.StateFile)

	// Step 2: Simulate token refresh and save
	geminiAcc.mu.Lock()
	geminiAcc.AccessToken = "ya29.SIMULATED_REFRESHED_TOKEN"
	geminiAcc.ExpiresAt = time.Now().Add(1 * time.Hour)
	geminiAcc.LastRefresh = time.Now().UTC()
	geminiAcc.mu.Unlock()

	if err := saveAccount(geminiAcc); err != nil {
		t.Fatal(err)
	}

	statePath := filepath.Join(stateDir, "gemini", "e2e-test.json")
	if !fileExists(statePath) {
		t.Fatal("state file not created at", statePath)
	}

	// Step 3: Reload pool -- state should override seed
	accounts2, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatal(err)
	}
	var reloaded *Account
	for _, a := range accounts2 {
		if a.Type == AccountTypeGemini {
			reloaded = a
			break
		}
	}
	if reloaded == nil {
		t.Fatal("gemini account not found on reload")
	}

	if reloaded.AccessToken != "ya29.SIMULATED_REFRESHED_TOKEN" {
		t.Errorf("after reload, access_token = %q, expected ya29.SIMULATED_REFRESHED_TOKEN",
			reloaded.AccessToken)
	}
	// Refresh token should still be the original seed value
	// (Google doesn't rotate, and we didn't change it)
	if reloaded.RefreshToken != originalRefresh {
		t.Errorf("after reload, refresh_token changed unexpectedly: got %q, expected %q",
			reloaded.RefreshToken, originalRefresh)
	}
	if reloaded.SeedRefreshToken != originalRefresh {
		t.Errorf("seed_refresh_token = %q, expected %q",
			reloaded.SeedRefreshToken, originalRefresh)
	}
}

// TestE2EMultiProviderPool tests loading a pool with multiple provider types.
func TestE2EMultiProviderPool(t *testing.T) {
	setE2EJWTSecret(t)
	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// Set up accounts for each available provider
	providerCount := 0

	if tok := os.Getenv("CLAUDE_CODE_OAUTH_TOKEN"); tok != "" && strings.HasPrefix(tok, "sk-ant-oat") {
		writeClaudePoolFile(t, poolDir, tok)
		providerCount++
		t.Log("Claude credentials available")
	}

	if credPath := filepath.Join(os.Getenv("HOME"), ".codex", "auth.json"); fileExists(credPath) {
		writeCodexPoolFile(t, poolDir, credPath)
		providerCount++
		t.Log("Codex credentials available")
	}

	if credPath := filepath.Join(os.Getenv("HOME"), ".gemini", "oauth_creds.json"); fileExists(credPath) {
		writeGeminiPoolFile(t, poolDir, credPath)
		providerCount++
		t.Log("Gemini credentials available")
	}

	if providerCount == 0 {
		t.Skip("no credentials available for any provider")
	}

	srv := startE2EProxy(t, poolDir, stateDir)

	// Verify healthz
	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz returned %d", resp.StatusCode)
	}

	t.Logf("Multi-provider pool started with %d providers", providerCount)
}

// TestE2EGeminiRefreshDirectly tests the Gemini token refresh flow directly
// (not through the proxy), verifying the provider.RefreshToken method works
// with real credentials and writes state correctly.
func TestE2EGeminiRefreshDirectly(t *testing.T) {
	credPath := geminiCredPath(t)

	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// Read real credentials
	raw, err := os.ReadFile(credPath)
	if err != nil {
		t.Fatal(err)
	}
	var creds map[string]any
	if err := json.Unmarshal(raw, &creds); err != nil {
		t.Fatal(err)
	}
	refreshToken, _ := creds["refresh_token"].(string)
	if refreshToken == "" {
		t.Skip("no refresh_token in Gemini credentials")
	}

	// Write seed with expired access token
	geminiDir := filepath.Join(poolDir, "gemini")
	if err := os.MkdirAll(geminiDir, 0700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"refresh_token": refreshToken,
		"access_token":  "ya29.EXPIRED",
		"token_type":    "Bearer",
		"expiry_date":   float64(time.Now().Add(-1 * time.Hour).UnixMilli()),
	}
	seedPath := filepath.Join(geminiDir, "direct-refresh.json")
	writeJSON(t, seedPath, seed)

	seedBefore, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}

	// Load the account
	registry := newTestRegistry(t)
	accounts, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatal(err)
	}
	var acc *Account
	for _, a := range accounts {
		if a.Type == AccountTypeGemini {
			acc = a
			break
		}
	}
	if acc == nil {
		t.Fatal("gemini account not loaded")
	}

	// Call RefreshToken directly
	provider := NewGeminiProvider(
		mustParse("https://cloudcode-pa.googleapis.com"),
		mustParse("https://generativelanguage.googleapis.com"),
	)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	err = provider.RefreshToken(ctx, acc, http.DefaultTransport)
	if err != nil {
		t.Fatalf("RefreshToken failed: %v", err)
	}

	acc.mu.Lock()
	newAccessToken := acc.AccessToken
	newExpiresAt := acc.ExpiresAt
	acc.mu.Unlock()

	if newAccessToken == "ya29.EXPIRED" || newAccessToken == "" {
		t.Error("access token was not refreshed")
	}
	if newExpiresAt.Before(time.Now()) {
		t.Error("expires_at is still in the past after refresh")
	}
	t.Logf("Gemini refresh succeeded: new token prefix=%.10s... expires=%v",
		newAccessToken, newExpiresAt)

	// Save the account -- this should write to stateDir
	if err := saveAccount(acc); err != nil {
		t.Fatal(err)
	}

	// Verify seed NOT modified
	seedAfter, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(seedBefore, seedAfter) {
		t.Error("seed file modified by RefreshToken+saveAccount")
	}

	// Verify state file created
	statePath := filepath.Join(stateDir, "gemini", "direct-refresh.json")
	if !fileExists(statePath) {
		t.Fatal("state file not created after refresh+save")
	}

	stateData := readJSONFile(t, statePath)
	stateAccessToken, _ := stateData["access_token"].(string)
	if stateAccessToken != newAccessToken {
		t.Errorf("state access_token=%q, expected %q", stateAccessToken, newAccessToken)
	}
	seedHash, _ := stateData["seed_refresh_hash"].(string)
	if seedHash == "" {
		t.Error("state file missing seed_refresh_hash")
	}
	expectedHash := seedRefreshHash(refreshToken)
	if seedHash != expectedHash {
		t.Errorf("seed_refresh_hash=%q, expected=%q", seedHash, expectedHash)
	}

	t.Logf("State file written correctly to %s", statePath)
}

// ----- helpers for Codex API call (if needed in future) -----

// callCodexResponsesAPI sends a minimal /v1/responses request through the proxy.
// NOTE: The Codex backend uses chatgpt.com which requires OAuth session tokens.
// This will likely fail with auth errors unless the tokens are very fresh.
// Keeping this as a helper for completeness; the main Codex E2E test focuses
// on pool loading and state file writing.
func callCodexResponsesAPI(t *testing.T, proxyURL string) *http.Response {
	t.Helper()
	body := map[string]any{
		"model": "gpt-4o-mini",
		"input": "Say hi",
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest("POST", proxyURL+"/v1/responses", bytes.NewReader(bodyJSON))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test") // proxy replaces this

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("Codex API call through proxy failed: %v", err)
	}
	return resp
}

