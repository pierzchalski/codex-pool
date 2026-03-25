package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// ConfigFile represents the config.toml structure.
type ConfigFile struct {
	ListenAddr      string  `toml:"listen_addr"`
	PoolDir         string  `toml:"pool_dir"`
	DBPath          string  `toml:"db_path"`
	MaxAttempts     int     `toml:"max_attempts"`
	DisableRefresh  bool    `toml:"disable_refresh"`
	RefreshProxyURL string  `toml:"refresh_proxy_url"` // HTTP proxy for refresh operations
	Debug           bool    `toml:"debug"`
	PublicURL       string  `toml:"public_url"`
	FriendCode      string  `toml:"friend_code"`
	FriendCodeFile  string  `toml:"friend_code_file"`
	FriendName      string  `toml:"friend_name"`
	FriendTagline   string  `toml:"friend_tagline"`
	AdminToken      string  `toml:"admin_token"`
	AdminTokenFile  string  `toml:"admin_token_file"`
	TierThreshold   float64 `toml:"tier_threshold"` // Secondary usage % threshold for tier preference (default 0.15)

	ModelAliases map[string]string `toml:"model_aliases"`

	PoolUsers PoolUsersConfig `toml:"pool_users"`
}

// getFriendName returns the configured friend name for the landing page.
func getFriendName() string {
	if v := os.Getenv("FRIEND_NAME"); v != "" {
		return v
	}
	if globalConfigFile != nil && globalConfigFile.FriendName != "" {
		return globalConfigFile.FriendName
	}
	return "PP" // default
}

// getFriendTagline returns the configured tagline for the landing page.
func getFriendTagline() string {
	if v := os.Getenv("FRIEND_TAGLINE"); v != "" {
		return v
	}
	if globalConfigFile != nil && globalConfigFile.FriendTagline != "" {
		return globalConfigFile.FriendTagline
	}
	return "For the few who know, the pool awaits. Unlimited resources. Zero friction."
}

// PoolUsersConfig is the [pool_users] section.
type PoolUsersConfig struct {
	JWTSecret     string `toml:"jwt_secret"`
	JWTSecretFile string `toml:"jwt_secret_file"`
	StoragePath   string `toml:"storage_path"`
}

// loadConfigFile loads config.toml if it exists.
// Returns nil if the file doesn't exist.
func loadConfigFile(path string) (*ConfigFile, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	var cfg ConfigFile
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// getConfigString returns the config value with priority: env var > config file > default.
func getConfigString(envKey string, configValue string, defaultValue string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if configValue != "" {
		return configValue
	}
	return defaultValue
}

// getConfigInt returns the config value with priority: env var > config file > default.
func getConfigInt(envKey string, configValue int, defaultValue int) int {
	if v := os.Getenv(envKey); v != "" {
		if n, err := parseInt64(v); err == nil && n > 0 {
			return int(n)
		}
	}
	if configValue > 0 {
		return configValue
	}
	return defaultValue
}

// getConfigFloat64 returns the config value with priority: env var > config file > default.
func getConfigFloat64(envKey string, configValue float64, defaultValue float64) float64 {
	if v := os.Getenv(envKey); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	if configValue > 0 {
		return configValue
	}
	return defaultValue
}

// getConfigBool returns the config value with priority: env var > config file > default.
func getConfigBool(envKey string, configValue bool, defaultValue bool) bool {
	if v := os.Getenv(envKey); v != "" {
		return v == "1" || v == "true"
	}
	if configValue {
		return true
	}
	return defaultValue
}

// getConfigSecret returns the secret value with priority:
// inline env var > env file > resolved config value.
func getConfigSecret(envKey string, envFileKey string, configValue string) (string, error) {
	if v := strings.TrimSpace(os.Getenv(envKey)); v != "" {
		return v, nil
	}
	if path := strings.TrimSpace(os.Getenv(envFileKey)); path != "" {
		return readSecretFile(path, "")
	}
	return strings.TrimSpace(configValue), nil
}

// resolveConfigSecrets reads any configured *_file secret values.
// Inline values win over file-backed values.
func resolveConfigSecrets(cfg *ConfigFile, configPath string) error {
	if cfg == nil {
		return nil
	}

	baseDir := configBaseDir(configPath)

	secret, err := resolveConfiguredSecret(cfg.AdminToken, cfg.AdminTokenFile, baseDir)
	if err != nil {
		return fmt.Errorf("resolve admin_token: %w", err)
	}
	cfg.AdminToken = secret

	secret, err = resolveConfiguredSecret(cfg.FriendCode, cfg.FriendCodeFile, baseDir)
	if err != nil {
		return fmt.Errorf("resolve friend_code: %w", err)
	}
	cfg.FriendCode = secret

	secret, err = resolveConfiguredSecret(cfg.PoolUsers.JWTSecret, cfg.PoolUsers.JWTSecretFile, baseDir)
	if err != nil {
		return fmt.Errorf("resolve pool_users.jwt_secret: %w", err)
	}
	cfg.PoolUsers.JWTSecret = secret

	return nil
}

func resolveConfiguredSecret(inlineValue string, filePath string, baseDir string) (string, error) {
	if v := strings.TrimSpace(inlineValue); v != "" {
		return v, nil
	}
	if path := strings.TrimSpace(filePath); path != "" {
		return readSecretFile(path, baseDir)
	}
	return "", nil
}

func readSecretFile(secretPath string, baseDir string) (string, error) {
	resolvedPath := resolveSecretPath(secretPath, baseDir)
	data, err := os.ReadFile(resolvedPath)
	if err != nil {
		return "", fmt.Errorf("read secret file %s: %w", resolvedPath, err)
	}
	return strings.TrimSpace(string(data)), nil
}

func resolveSecretPath(secretPath string, baseDir string) string {
	if secretPath == "" || filepath.IsAbs(secretPath) || baseDir == "" {
		return secretPath
	}
	return filepath.Join(baseDir, secretPath)
}

func configBaseDir(configPath string) string {
	if configPath == "" {
		return ""
	}
	return filepath.Dir(configPath)
}
