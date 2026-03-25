package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveConfigSecretsReadsFiles(t *testing.T) {
	configDir := t.TempDir()
	adminPath := filepath.Join(configDir, "admin-token.txt")
	friendPath := filepath.Join(configDir, "friend-code.txt")
	jwtPath := filepath.Join(configDir, "jwt-secret.txt")

	if err := os.WriteFile(adminPath, []byte("admin-secret\n"), 0o600); err != nil {
		t.Fatalf("write admin secret: %v", err)
	}
	if err := os.WriteFile(friendPath, []byte("friend-secret\n"), 0o600); err != nil {
		t.Fatalf("write friend secret: %v", err)
	}
	if err := os.WriteFile(jwtPath, []byte("jwt-secret\n"), 0o600); err != nil {
		t.Fatalf("write jwt secret: %v", err)
	}

	cfg := &ConfigFile{
		AdminTokenFile: filepath.Base(adminPath),
		FriendCodeFile: filepath.Base(friendPath),
		PoolUsers: PoolUsersConfig{
			JWTSecretFile: filepath.Base(jwtPath),
		},
	}

	if err := resolveConfigSecrets(cfg, filepath.Join(configDir, "config.toml")); err != nil {
		t.Fatalf("resolveConfigSecrets: %v", err)
	}

	if cfg.AdminToken != "admin-secret" {
		t.Fatalf("admin token = %q", cfg.AdminToken)
	}
	if cfg.FriendCode != "friend-secret" {
		t.Fatalf("friend code = %q", cfg.FriendCode)
	}
	if cfg.PoolUsers.JWTSecret != "jwt-secret" {
		t.Fatalf("jwt secret = %q", cfg.PoolUsers.JWTSecret)
	}
}

func TestResolveConfigSecretsPrefersInlineValues(t *testing.T) {
	configDir := t.TempDir()
	secretPath := filepath.Join(configDir, "secret.txt")
	if err := os.WriteFile(secretPath, []byte("from-file\n"), 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	cfg := &ConfigFile{
		AdminToken:     "inline-admin",
		AdminTokenFile: filepath.Base(secretPath),
		FriendCode:     "inline-friend",
		FriendCodeFile: filepath.Base(secretPath),
		PoolUsers: PoolUsersConfig{
			JWTSecret:     "inline-jwt",
			JWTSecretFile: filepath.Base(secretPath),
		},
	}

	if err := resolveConfigSecrets(cfg, filepath.Join(configDir, "config.toml")); err != nil {
		t.Fatalf("resolveConfigSecrets: %v", err)
	}

	if cfg.AdminToken != "inline-admin" {
		t.Fatalf("admin token = %q", cfg.AdminToken)
	}
	if cfg.FriendCode != "inline-friend" {
		t.Fatalf("friend code = %q", cfg.FriendCode)
	}
	if cfg.PoolUsers.JWTSecret != "inline-jwt" {
		t.Fatalf("jwt secret = %q", cfg.PoolUsers.JWTSecret)
	}
}

func TestGetConfigSecretPrefersEnvFileOverConfig(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "env-secret.txt")
	if err := os.WriteFile(secretPath, []byte("env-file-secret\n"), 0o600); err != nil {
		t.Fatalf("write env secret: %v", err)
	}

	t.Setenv("ADMIN_TOKEN_FILE", secretPath)

	secret, err := getConfigSecret("ADMIN_TOKEN", "ADMIN_TOKEN_FILE", "config-secret")
	if err != nil {
		t.Fatalf("getConfigSecret: %v", err)
	}
	if secret != "env-file-secret" {
		t.Fatalf("secret = %q", secret)
	}
}

func TestGetConfigSecretPrefersInlineEnvOverEnvFile(t *testing.T) {
	secretPath := filepath.Join(t.TempDir(), "env-secret.txt")
	if err := os.WriteFile(secretPath, []byte("env-file-secret\n"), 0o600); err != nil {
		t.Fatalf("write env secret: %v", err)
	}

	t.Setenv("ADMIN_TOKEN", "inline-env-secret")
	t.Setenv("ADMIN_TOKEN_FILE", secretPath)

	secret, err := getConfigSecret("ADMIN_TOKEN", "ADMIN_TOKEN_FILE", "config-secret")
	if err != nil {
		t.Fatalf("getConfigSecret: %v", err)
	}
	if secret != "inline-env-secret" {
		t.Fatalf("secret = %q", secret)
	}
}
