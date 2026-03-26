package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestLoadPoolWithStateDir verifies that loadPool merges seed + state files.
func TestLoadPoolWithStateDir(t *testing.T) {
	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// Create seed file for Claude OAuth
	claudeSeedDir := filepath.Join(poolDir, "claude")
	if err := os.MkdirAll(claudeSeedDir, 0700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"claudeAiOauth": map[string]any{
			"refreshToken":     "seed-refresh",
			"subscriptionType": "max",
		},
	}
	writeTempJSON(t, filepath.Join(claudeSeedDir, "acct1.json"), seed)

	// Create state file with overrides
	claudeStateDir := filepath.Join(stateDir, "claude")
	if err := os.MkdirAll(claudeStateDir, 0700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"claudeAiOauth": map[string]any{
			"accessToken":  "state-access",
			"refreshToken": "state-refresh-rotated",
			"expiresAt":    float64(time.Now().Add(1 * time.Hour).UnixMilli()),
		},
		"last_refresh":      time.Now().UTC().Format(time.RFC3339Nano),
		"seed_refresh_hash": seedRefreshHash("seed-refresh"),
	}
	writeTempJSON(t, filepath.Join(claudeStateDir, "acct1.json"), state)

	registry := newTestRegistry(t)
	accs, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}

	acc := accs[0]
	if acc.AccessToken != "state-access" {
		t.Fatalf("expected state access token, got %q", acc.AccessToken)
	}
	// Seed unchanged, so state's rotated refresh token should be used
	if acc.RefreshToken != "state-refresh-rotated" {
		t.Fatalf("expected state refresh token, got %q", acc.RefreshToken)
	}
	if acc.SeedRefreshToken != "seed-refresh" {
		t.Fatalf("expected seed refresh token stored, got %q", acc.SeedRefreshToken)
	}
	if acc.StateFile == "" {
		t.Fatal("expected StateFile to be set")
	}
	if acc.PlanType != "max" {
		t.Fatalf("expected plan type 'max', got %q", acc.PlanType)
	}
}

// TestLoadPoolSeedOnly verifies backward compat when no state file exists.
func TestLoadPoolSeedOnly(t *testing.T) {
	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// Create a Gemini seed file with only refresh_token (no access_token)
	geminiSeedDir := filepath.Join(poolDir, "gemini")
	if err := os.MkdirAll(geminiSeedDir, 0700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"refresh_token": "gem-refresh",
		"token_type":    "Bearer",
	}
	writeTempJSON(t, filepath.Join(geminiSeedDir, "acct1.json"), seed)

	registry := newTestRegistry(t)
	accs, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 {
		t.Fatalf("expected 1 account (seed-only with refresh_token), got %d", len(accs))
	}
	acc := accs[0]
	if acc.RefreshToken != "gem-refresh" {
		t.Fatalf("expected seed refresh token, got %q", acc.RefreshToken)
	}
	if acc.AccessToken != "" {
		t.Fatalf("expected empty access token on cold start, got %q", acc.AccessToken)
	}
}

// TestLoadPoolNoStateDir verifies behavior when stateDir is empty (backward compat).
func TestLoadPoolNoStateDir(t *testing.T) {
	poolDir := t.TempDir()

	codexDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexDir, 0700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"tokens": map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"id_token":      "old-id",
		},
	}
	writeTempJSON(t, filepath.Join(codexDir, "acct1.json"), seed)

	registry := newTestRegistry(t)
	accs, err := loadPool(poolDir, "", registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}
	acc := accs[0]
	if acc.StateFile != "" {
		t.Fatalf("expected empty StateFile, got %q", acc.StateFile)
	}
	if acc.AccessToken != "old-access" {
		t.Fatalf("expected old-access, got %q", acc.AccessToken)
	}
}

// TestSeedFingerprintReseed verifies that re-seeded refresh tokens override state.
func TestSeedFingerprintReseed(t *testing.T) {
	poolDir := t.TempDir()
	stateDir := t.TempDir()

	// Create Codex seed with a new refresh token
	codexSeedDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexSeedDir, 0700); err != nil {
		t.Fatal(err)
	}
	seed := map[string]any{
		"tokens": map[string]any{
			"refresh_token": "new-seed-refresh",
			"account_id":    "acct_123",
		},
	}
	writeTempJSON(t, filepath.Join(codexSeedDir, "acct1.json"), seed)

	// Create state file with old rotated token and hash of OLD seed
	codexStateDir := filepath.Join(stateDir, "codex")
	if err := os.MkdirAll(codexStateDir, 0700); err != nil {
		t.Fatal(err)
	}
	state := map[string]any{
		"tokens": map[string]any{
			"access_token":  "state-access",
			"refresh_token": "state-rotated-refresh",
			"id_token":      "state-id",
		},
		"last_refresh":      time.Now().UTC().Format(time.RFC3339Nano),
		"seed_refresh_hash": seedRefreshHash("old-seed-refresh"), // hash of the OLD seed
	}
	writeTempJSON(t, filepath.Join(codexStateDir, "acct1.json"), state)

	registry := newTestRegistry(t)
	accs, err := loadPool(poolDir, stateDir, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(accs) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accs))
	}

	acc := accs[0]
	// Hash mismatch: seed was re-seeded, should use seed's refresh_token
	if acc.RefreshToken != "new-seed-refresh" {
		t.Fatalf("expected seed refresh_token after re-seed, got %q", acc.RefreshToken)
	}
	// But access token should still come from state
	if acc.AccessToken != "state-access" {
		t.Fatalf("expected state access_token, got %q", acc.AccessToken)
	}
}

// TestSaveAccountWritesToStateFile verifies that save writes to StateFile when set.
func TestSaveAccountWritesToStateFile(t *testing.T) {
	seedDir := t.TempDir()
	stateDir := t.TempDir()

	// Create seed file
	seedPath := filepath.Join(seedDir, "acct.json")
	writeTempJSON(t, seedPath, map[string]any{
		"tokens": map[string]any{
			"refresh_token": "orig-refresh",
		},
	})

	codexStateDir := filepath.Join(stateDir, "codex")

	acc := &Account{
		Type:             AccountTypeCodex,
		ID:               "acct",
		File:             seedPath,
		StateFile:        filepath.Join(codexStateDir, "acct.json"),
		AccessToken:      "new-access",
		RefreshToken:     "rotated-refresh",
		IDToken:          "new-id",
		SeedRefreshToken: "orig-refresh",
		LastRefresh:      time.Now().UTC(),
	}

	if err := saveAccount(acc); err != nil {
		t.Fatal(err)
	}

	// Verify state file was created
	stateRaw, err := os.ReadFile(acc.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var stateRoot map[string]any
	if err := json.Unmarshal(stateRaw, &stateRoot); err != nil {
		t.Fatal(err)
	}
	tokens, ok := stateRoot["tokens"].(map[string]any)
	if !ok {
		t.Fatal("expected tokens in state file")
	}
	if tokens["access_token"] != "new-access" {
		t.Fatalf("state access_token = %v", tokens["access_token"])
	}
	if tokens["refresh_token"] != "rotated-refresh" {
		t.Fatalf("state refresh_token = %v", tokens["refresh_token"])
	}
	if stateRoot["seed_refresh_hash"] != seedRefreshHash("orig-refresh") {
		t.Fatalf("expected seed_refresh_hash, got %v", stateRoot["seed_refresh_hash"])
	}

	// Verify seed file was NOT modified
	seedRaw, err := os.ReadFile(seedPath)
	if err != nil {
		t.Fatal(err)
	}
	var seedRoot map[string]any
	if err := json.Unmarshal(seedRaw, &seedRoot); err != nil {
		t.Fatal(err)
	}
	seedTokens, ok := seedRoot["tokens"].(map[string]any)
	if !ok {
		t.Fatal("expected tokens in seed")
	}
	if seedTokens["refresh_token"] != "orig-refresh" {
		t.Fatalf("seed refresh_token was modified: %v", seedTokens["refresh_token"])
	}
	if _, exists := seedTokens["access_token"]; exists {
		t.Fatal("seed should not have access_token")
	}
}

// TestSaveAccountBackwardCompat verifies save writes to File when StateFile is empty.
func TestSaveAccountBackwardCompat(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "auth.json")

	original := map[string]any{
		"tokens": map[string]any{
			"access_token":  "old-access",
			"refresh_token": "old-refresh",
			"id_token":      "old-id",
		},
	}
	writeTempJSON(t, path, original)

	acc := &Account{
		Type:         AccountTypeCodex,
		ID:           "acct",
		File:         path,
		StateFile:    "", // backward compat
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
		IDToken:      "new-id",
		LastRefresh:  time.Now().UTC(),
	}

	if err := saveAccount(acc); err != nil {
		t.Fatal(err)
	}

	afterRaw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(afterRaw, &after); err != nil {
		t.Fatal(err)
	}
	tokens := after["tokens"].(map[string]any)
	if tokens["access_token"] != "new-access" {
		t.Fatalf("expected updated access_token, got %v", tokens["access_token"])
	}
}

// TestSaveClaudeStateFile verifies Claude state file writing.
func TestSaveClaudeStateFile(t *testing.T) {
	seedDir := t.TempDir()
	stateDir := t.TempDir()

	seedPath := filepath.Join(seedDir, "acct.json")
	writeTempJSON(t, seedPath, map[string]any{
		"claudeAiOauth": map[string]any{
			"refreshToken": "seed-refresh",
			"scopes":       []string{"user:inference"},
		},
	})

	claudeStateDir := filepath.Join(stateDir, "claude")

	acc := &Account{
		Type:             AccountTypeClaude,
		ID:               "acct",
		File:             seedPath,
		StateFile:        filepath.Join(claudeStateDir, "acct.json"),
		AccessToken:      "new-access",
		RefreshToken:     "rotated-refresh",
		SeedRefreshToken: "seed-refresh",
		ExpiresAt:        time.Now().Add(1 * time.Hour),
		PlanType:         "max",
		RateLimitTier:    "default_claude_max_20x",
		LastRefresh:      time.Now().UTC(),
	}

	if err := saveAccount(acc); err != nil {
		t.Fatal(err)
	}

	stateRaw, err := os.ReadFile(acc.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var stateRoot map[string]any
	if err := json.Unmarshal(stateRaw, &stateRoot); err != nil {
		t.Fatal(err)
	}
	oauth, ok := stateRoot["claudeAiOauth"].(map[string]any)
	if !ok {
		t.Fatal("expected claudeAiOauth in state")
	}
	if oauth["accessToken"] != "new-access" {
		t.Fatalf("state accessToken = %v", oauth["accessToken"])
	}
	if oauth["refreshToken"] != "rotated-refresh" {
		t.Fatalf("state refreshToken = %v", oauth["refreshToken"])
	}
	if stateRoot["seed_refresh_hash"] != seedRefreshHash("seed-refresh") {
		t.Fatalf("expected seed_refresh_hash, got %v", stateRoot["seed_refresh_hash"])
	}
}

// TestSaveGeminiStateFile verifies Gemini state file writing.
func TestSaveGeminiStateFile(t *testing.T) {
	seedDir := t.TempDir()
	stateDir := t.TempDir()

	seedPath := filepath.Join(seedDir, "acct.json")
	writeTempJSON(t, seedPath, map[string]any{
		"refresh_token": "gem-refresh",
	})

	geminiStateDir := filepath.Join(stateDir, "gemini")

	acc := &Account{
		Type:         AccountTypeGemini,
		ID:           "acct",
		File:         seedPath,
		StateFile:    filepath.Join(geminiStateDir, "acct.json"),
		AccessToken:  "ya29.new-access",
		RefreshToken: "gem-refresh",
		ExpiresAt:    time.Now().Add(1 * time.Hour),
		LastRefresh:  time.Now().UTC(),
	}

	if err := saveAccount(acc); err != nil {
		t.Fatal(err)
	}

	stateRaw, err := os.ReadFile(acc.StateFile)
	if err != nil {
		t.Fatal(err)
	}
	var stateRoot map[string]any
	if err := json.Unmarshal(stateRaw, &stateRoot); err != nil {
		t.Fatal(err)
	}
	if stateRoot["access_token"] != "ya29.new-access" {
		t.Fatalf("state access_token = %v", stateRoot["access_token"])
	}
}

// TestSeedRefreshHash verifies the hash function.
func TestSeedRefreshHash(t *testing.T) {
	h1 := seedRefreshHash("token-a")
	h2 := seedRefreshHash("token-b")
	h3 := seedRefreshHash("token-a")

	if h1 == h2 {
		t.Fatal("different tokens should have different hashes")
	}
	if h1 != h3 {
		t.Fatal("same token should produce same hash")
	}
	if seedRefreshHash("") != "" {
		t.Fatal("empty token should produce empty hash")
	}
}

// --- helpers ---

func newTestRegistry(t *testing.T) *ProviderRegistry {
	t.Helper()
	codex := NewCodexProvider(mustParse("https://api.openai.com"), mustParse("https://api.openai.com"), mustParse("https://auth.openai.com"))
	claude := NewClaudeProvider(mustParse("https://api.anthropic.com"))
	gemini := NewGeminiProvider(mustParse("https://cloudcode-pa.googleapis.com"), mustParse("https://generativelanguage.googleapis.com"))
	kimi := NewKimiProvider(mustParse("https://api.kimi.com"))
	minimax := NewMinimaxProvider(mustParse("https://api.minimax.io"))
	return NewProviderRegistry(codex, claude, gemini, kimi, minimax)
}

func writeTempJSON(t *testing.T, path string, data any) {
	t.Helper()
	buf, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf, 0600); err != nil {
		t.Fatal(err)
	}
}
