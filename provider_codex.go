package main

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CodexProvider handles OpenAI Codex accounts.
type CodexProvider struct {
	responsesBase *url.URL
	whamBase      *url.URL
	refreshBase   *url.URL
}

// NewCodexProvider creates a new Codex provider.
func NewCodexProvider(responsesBase, whamBase, refreshBase *url.URL) *CodexProvider {
	return &CodexProvider{
		responsesBase: responsesBase,
		whamBase:      whamBase,
		refreshBase:   refreshBase,
	}
}

const defaultCodexOpenRouterBaseURL = "https://openrouter.ai/api/v1"

func isCodexOpenRouterAccount(acc *Account) bool {
	return acc != nil && acc.Type == AccountTypeCodex && acc.Backend == AccountBackendOpenRouter && strings.TrimSpace(acc.AccessToken) != ""
}

func codexOpenRouterBaseURL(raw string) *url.URL {
	candidate := strings.TrimSpace(raw)
	if candidate == "" {
		candidate = defaultCodexOpenRouterBaseURL
	}
	u, err := url.Parse(candidate)
	if err == nil && u != nil && u.Scheme != "" && u.Host != "" {
		return u
	}
	fallback, _ := url.Parse(defaultCodexOpenRouterBaseURL)
	return fallback
}

func normalizeCodexOpenRouterPath(path string) string {
	if strings.HasPrefix(path, "/backend-api/codex") {
		trimmed := strings.TrimPrefix(path, "/backend-api/codex")
		if trimmed == "" {
			return "/"
		}
		if !strings.HasPrefix(trimmed, "/") {
			trimmed = "/" + trimmed
		}
		if strings.HasPrefix(trimmed, "/responses") {
			return mapResponsesPath(trimmed)
		}
		return trimmed
	}
	if strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/responses") {
		return mapResponsesPath(path)
	}
	if strings.HasPrefix(path, "/backend-api/") {
		trimmed := strings.TrimPrefix(path, "/backend-api")
		if trimmed == "" {
			return "/"
		}
		return trimmed
	}
	return path
}

func (p *CodexProvider) UpstreamURLForAccount(acc *Account, path string) *url.URL {
	if isCodexOpenRouterAccount(acc) {
		return codexOpenRouterBaseURL(acc.BaseURL)
	}
	return p.UpstreamURL(path)
}

func (p *CodexProvider) NormalizePathForAccount(acc *Account, path string) string {
	if isCodexOpenRouterAccount(acc) {
		return normalizeCodexOpenRouterPath(path)
	}
	return p.NormalizePath(path)
}

func (p *CodexProvider) Type() AccountType {
	return AccountTypeCodex
}

func (p *CodexProvider) LoadAccount(name, path string, seedData []byte, stateData []byte) (*Account, error) {
	var aj CodexAuthJSON
	if err := json.Unmarshal(seedData, &aj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	acc := &Account{
		Type: AccountTypeCodex,
		ID:   strings.TrimSuffix(name, filepath.Ext(name)),
		File: path,
		Dead: aj.Dead,
	}

	if aj.LastRefresh != nil {
		acc.LastRefresh = *aj.LastRefresh
	}

	openAIKey := ""
	if aj.OpenAIKey != nil {
		openAIKey = strings.TrimSpace(*aj.OpenAIKey)
	}
	backend := strings.ToLower(strings.TrimSpace(aj.Backend))
	if backend == string(AccountBackendOpenRouter) || (backend == "" && openAIKey != "" && aj.Tokens == nil) {
		if openAIKey == "" {
			return nil, nil
		}
		acc.Backend = AccountBackendOpenRouter
		acc.AccessToken = openAIKey
		acc.BaseURL = strings.TrimSpace(aj.BaseURL)
		if acc.BaseURL == "" {
			acc.BaseURL = defaultCodexOpenRouterBaseURL
		}
		acc.PlanType = strings.TrimSpace(aj.PlanType)
		if acc.PlanType == "" {
			acc.PlanType = "api"
		}
		// Apply state for OpenRouter (dead flag only)
		if stateData != nil {
			applyCodexOpenRouterState(acc, stateData)
		}
		return acc, nil
	}

	// For OAuth accounts, allow seed-only mode (refresh_token but no access_token)
	if aj.Tokens == nil {
		return nil, nil
	}

	acc.AccessToken = aj.Tokens.AccessToken
	acc.RefreshToken = aj.Tokens.RefreshToken
	acc.SeedRefreshToken = aj.Tokens.RefreshToken
	acc.IDToken = aj.Tokens.IDToken
	if aj.Tokens.AccountID != nil {
		acc.AccountID = strings.TrimSpace(*aj.Tokens.AccountID)
	}
	claims := parseCodexClaims(aj.Tokens.IDToken)
	acc.IDTokenChatGPTAccountID = claims.ChatGPTAccountID
	if acc.AccountID == "" && acc.IDTokenChatGPTAccountID != "" {
		acc.AccountID = acc.IDTokenChatGPTAccountID
	}
	acc.PlanType = claims.PlanType
	acc.ExpiresAt = claims.ExpiresAt
	if acc.ExpiresAt.IsZero() && aj.LastRefresh != nil {
		acc.ExpiresAt = aj.LastRefresh.Add(20 * time.Hour)
	}

	// Apply state overrides if present
	if stateData != nil {
		applyCodexState(acc, stateData)
	}

	return acc, nil
}

// applyCodexState merges mutable state from the state file onto a Codex OAuth account.
func applyCodexState(acc *Account, stateData []byte) {
	var stateRoot map[string]any
	if err := json.Unmarshal(stateData, &stateRoot); err != nil {
		log.Printf("warning: corrupt state file for Codex account %s: %v (using seed only)", acc.ID, err)
		return
	}

	// Parse tokens from state
	if tokensAny, ok := stateRoot["tokens"].(map[string]any); ok {
		if at, ok := tokensAny["access_token"].(string); ok && at != "" {
			acc.AccessToken = at
		}
		if idt, ok := tokensAny["id_token"].(string); ok && idt != "" {
			acc.IDToken = idt
			// Re-parse claims from state's id_token
			claims := parseCodexClaims(idt)
			if !claims.ExpiresAt.IsZero() {
				acc.ExpiresAt = claims.ExpiresAt
			}
			if claims.ChatGPTAccountID != "" {
				acc.IDTokenChatGPTAccountID = claims.ChatGPTAccountID
				if acc.AccountID == "" {
					acc.AccountID = claims.ChatGPTAccountID
				}
			}
			if claims.PlanType != "" {
				acc.PlanType = claims.PlanType
			}
		}

		// Refresh token precedence: use seed fingerprint
		if rt, ok := tokensAny["refresh_token"].(string); ok && rt != "" {
			storedHash, _ := stateRoot["seed_refresh_hash"].(string)
			currentSeedHash := seedRefreshHash(acc.SeedRefreshToken)
			if storedHash == currentSeedHash {
				// Seed unchanged: state's rotated refresh token is current
				acc.RefreshToken = rt
			}
			// If hashes differ: seed was re-seeded, keep seed's refresh_token
		}
	}

	// Override last_refresh from state
	if lr, ok := stateRoot["last_refresh"].(string); ok && lr != "" {
		if t, err := time.Parse(time.RFC3339Nano, lr); err == nil {
			acc.LastRefresh = t
		} else if t, err := time.Parse(time.RFC3339, lr); err == nil {
			acc.LastRefresh = t
		}
		// Recompute expiry if last_refresh changed and no JWT exp available
		if acc.ExpiresAt.IsZero() && !acc.LastRefresh.IsZero() {
			acc.ExpiresAt = acc.LastRefresh.Add(20 * time.Hour)
		}
	}

	if dead, ok := stateRoot["dead"].(bool); ok {
		acc.Dead = dead
	}
}

// applyCodexOpenRouterState merges state for OpenRouter Codex accounts (dead flag only).
func applyCodexOpenRouterState(acc *Account, stateData []byte) {
	var stateRoot map[string]any
	if err := json.Unmarshal(stateData, &stateRoot); err != nil {
		return
	}
	if dead, ok := stateRoot["dead"].(bool); ok {
		acc.Dead = dead
	}
}

func (p *CodexProvider) SetAuthHeaders(req *http.Request, acc *Account) {
	req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
	if isCodexOpenRouterAccount(acc) {
		req.Header.Del("ChatGPT-Account-ID")
		return
	}
	// ChatGPT Account ID needed for some endpoints
	chatgptAccID := acc.AccountID
	if chatgptAccID == "" {
		chatgptAccID = acc.IDTokenChatGPTAccountID
	}
	if chatgptAccID != "" {
		req.Header.Set("ChatGPT-Account-ID", chatgptAccID)
	}
}

func (p *CodexProvider) RefreshToken(ctx context.Context, acc *Account, transport http.RoundTripper) error {
	if isCodexOpenRouterAccount(acc) {
		return nil
	}

	acc.mu.Lock()
	refreshTok := acc.RefreshToken
	acc.mu.Unlock()

	if refreshTok == "" {
		return errors.New("no refresh token")
	}

	// Match Codex behavior: JSON body, Content-Type: application/json
	body := map[string]string{
		"client_id":     "app_EMoamEEZ73f0CkXaXp7hrann",
		"grant_type":    "refresh_token",
		"refresh_token": refreshTok,
		"scope":         "openid profile email",
	}
	bodyJSON, err := json.Marshal(body)
	if err != nil {
		return err
	}
	refreshURL := p.refreshBase.ResolveReference(&url.URL{Path: "/oauth/token"})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL.String(), bytes.NewReader(bodyJSON))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "codex-pool-proxy")

	resp, err := transport.RoundTrip(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		if len(bytes.TrimSpace(msg)) > 0 {
			return fmt.Errorf("refresh unauthorized: %s: %s", resp.Status, safeText(msg))
		}
		return fmt.Errorf("refresh unauthorized: %s", resp.Status)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
		if len(bytes.TrimSpace(msg)) > 0 {
			return fmt.Errorf("refresh failed: %s: %s", resp.Status, safeText(msg))
		}
		return fmt.Errorf("refresh failed: %s", resp.Status)
	}

	var payload struct {
		IDToken      string `json:"id_token"`
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return err
	}
	if payload.AccessToken == "" {
		return errors.New("empty access token after refresh")
	}

	acc.mu.Lock()
	acc.AccessToken = payload.AccessToken
	if payload.RefreshToken != "" {
		acc.RefreshToken = payload.RefreshToken
	}
	if payload.IDToken != "" {
		acc.IDToken = payload.IDToken
		claims := parseCodexClaims(payload.IDToken)
		if !claims.ExpiresAt.IsZero() {
			acc.ExpiresAt = claims.ExpiresAt
		}
		if claims.ChatGPTAccountID != "" {
			acc.IDTokenChatGPTAccountID = claims.ChatGPTAccountID
			if acc.AccountID == "" {
				acc.AccountID = claims.ChatGPTAccountID
			}
		}
		if claims.PlanType != "" {
			acc.PlanType = claims.PlanType
		}
	}
	acc.LastRefresh = time.Now().UTC()
	acc.Dead = false
	acc.mu.Unlock()

	return saveAccount(acc)
}

func (p *CodexProvider) ParseUsage(obj map[string]any) *RequestUsage {
	// Try token_count event format first
	if ru := p.parseTokenCountEvent(obj); ru != nil {
		return ru
	}
	// Then try response usage format
	return p.parseResponseUsage(obj)
}

// parseTokenCountEvent delegates to the shared implementation in usage.go.
func (p *CodexProvider) parseTokenCountEvent(obj map[string]any) *RequestUsage {
	return parseTokenCountEvent(obj)
}

// parseResponseUsage extracts usage from Codex SSE response events.
func (p *CodexProvider) parseResponseUsage(obj map[string]any) *RequestUsage {
	usageMap, ok := obj["usage"].(map[string]any)
	if !ok || usageMap == nil {
		if resp, ok := obj["response"].(map[string]any); ok {
			usageMap, ok = resp["usage"].(map[string]any)
			if !ok || usageMap == nil {
				return nil
			}
		} else {
			return nil
		}
	}

	ru := &RequestUsage{Timestamp: time.Now()}
	ru.InputTokens = readInt64(usageMap, "input_tokens")
	ru.OutputTokens = readInt64(usageMap, "output_tokens")

	if details, ok := usageMap["input_tokens_details"].(map[string]any); ok {
		ru.CachedInputTokens = readInt64(details, "cached_tokens")
	}
	if ru.CachedInputTokens == 0 {
		ru.CachedInputTokens = readInt64(usageMap, "cache_read_input_tokens")
	}

	if details, ok := usageMap["output_tokens_details"].(map[string]any); ok {
		ru.ReasoningTokens = readInt64(details, "reasoning_tokens")
	}

	ru.BillableTokens = ru.InputTokens - ru.CachedInputTokens + ru.OutputTokens

	if ru.InputTokens == 0 && ru.OutputTokens == 0 {
		return nil
	}

	if v, ok := obj["prompt_cache_key"].(string); ok {
		ru.PromptCacheKey = v
	}

	// Extract model from response object or top-level
	if m, ok := obj["model"].(string); ok && m != "" {
		ru.Model = m
	} else if resp, ok := obj["response"].(map[string]any); ok {
		if m, ok := resp["model"].(string); ok && m != "" {
			ru.Model = m
		}
	}

	return ru
}

func (p *CodexProvider) ParseUsageHeaders(acc *Account, headers http.Header) {
	primaryStr := headers.Get("X-Codex-Primary-Used-Percent")
	secondaryStr := headers.Get("X-Codex-Secondary-Used-Percent")
	if primaryStr == "" && secondaryStr == "" {
		return
	}

	acc.mu.Lock()
	defer acc.mu.Unlock()

	snap := acc.Usage
	snap.RetrievedAt = time.Now()
	snap.Source = "headers"

	if primaryStr != "" {
		if f, err := strconv.ParseFloat(primaryStr, 64); err == nil {
			snap.PrimaryUsedPercent = f / 100.0
			snap.PrimaryUsed = snap.PrimaryUsedPercent
		}
	}
	if secondaryStr != "" {
		if f, err := strconv.ParseFloat(secondaryStr, 64); err == nil {
			snap.SecondaryUsedPercent = f / 100.0
			snap.SecondaryUsed = snap.SecondaryUsedPercent
		}
	}

	if v := headers.Get("X-Codex-Primary-Window-Minutes"); v != "" {
		snap.PrimaryWindowMinutes, _ = strconv.Atoi(v)
	}
	if v := headers.Get("X-Codex-Secondary-Window-Minutes"); v != "" {
		snap.SecondaryWindowMinutes, _ = strconv.Atoi(v)
	}
	if v := headers.Get("X-Codex-Primary-Reset-At"); v != "" {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			snap.PrimaryResetAt = time.Unix(ts, 0)
		}
	}
	if v := headers.Get("X-Codex-Secondary-Reset-At"); v != "" {
		if ts, err := strconv.ParseInt(v, 10, 64); err == nil {
			snap.SecondaryResetAt = time.Unix(ts, 0)
		}
	}
	if v := headers.Get("X-Codex-Credits-Balance"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			snap.CreditsBalance = f
		}
	}
	snap.HasCredits = strings.EqualFold(headers.Get("X-Codex-Credits-Has-Credits"), "true")
	snap.CreditsUnlimited = strings.EqualFold(headers.Get("X-Codex-Credits-Unlimited"), "true")

	acc.Usage = mergeUsage(acc.Usage, snap)
}

func (p *CodexProvider) UpstreamURL(path string) *url.URL {
	if strings.HasPrefix(path, "/backend-api/") {
		return p.whamBase
	}
	return p.responsesBase
}

func (p *CodexProvider) MatchesPath(path string) bool {
	return strings.HasPrefix(path, "/v1/") ||
		strings.HasPrefix(path, "/responses") ||
		strings.HasPrefix(path, "/ws") ||
		strings.HasPrefix(path, "/backend-api/") ||
		strings.HasPrefix(path, "/api/codex/")
}

func (p *CodexProvider) NormalizePath(path string) string {
	// Map /v1/responses/* and /responses/* to the correct upstream path
	if strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/responses") {
		return mapResponsesPath(path)
	}
	// If caller already included /backend-api in the request path, avoid
	// duplicating it when we join against upstreams that also include /backend-api.
	if strings.HasPrefix(path, "/backend-api/") {
		trimmed := strings.TrimPrefix(path, "/backend-api")
		if trimmed == "" {
			return "/"
		}
		return trimmed
	}
	return path
}

func (p *CodexProvider) DetectsSSE(path string, contentType string) bool {
	// Responses paths are always SSE
	if strings.HasPrefix(path, "/responses/") || strings.HasPrefix(path, "/v1/") {
		return true
	}
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// parseCodexClaims extracts claims from a Codex JWT ID token.
func parseCodexClaims(idToken string) codexJWTClaims {
	var out codexJWTClaims
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return out
	}
	payloadB64 := parts[1]
	payloadBytes, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return out
	}
	var payload map[string]any
	if err := json.Unmarshal(payloadBytes, &payload); err != nil {
		return out
	}
	if exp, ok := payload["exp"].(float64); ok {
		out.ExpiresAt = time.Unix(int64(exp), 0)
	}
	if acc, ok := payload["chatgpt_account_id"].(string); ok {
		out.ChatGPTAccountID = acc
	}
	if auth, ok := payload["https://api.openai.com/auth"].(map[string]any); ok {
		if acc, ok := auth["chatgpt_account_id"].(string); ok && acc != "" {
			out.ChatGPTAccountID = acc
		}
		if plan, ok := auth["chatgpt_plan_type"].(string); ok {
			out.PlanType = plan
		}
	}
	if out.PlanType == "" {
		out.PlanType = "pro"
	}
	return out
}

type codexJWTClaims struct {
	ExpiresAt        time.Time
	ChatGPTAccountID string
	PlanType         string
}
