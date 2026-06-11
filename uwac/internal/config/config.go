// Package config loads uwacd configuration from the environment with an
// optional uwac.config.kvx overlay (env always wins). Mirrors tachyon's
// config layering so operators get one consistent knob story.
package config

import (
	"crypto/sha256"
	"fmt"
	"os"
	"time"
)

// Config is the resolved uwacd runtime configuration.
type Config struct {
	// APIAddr is the listen address (default :8646).
	APIAddr string
	// AuthToken is the shared transport bearer (MATRIX_UWAC_TOKEN). Empty
	// disables transport auth (loopback/dev only — logged as a warning).
	AuthToken string
	// VaultKey is the 32-byte AES-256-GCM key used to encrypt provider tokens
	// at rest. Derived (sha256) from UWAC_VAULT_KEY.
	VaultKey []byte
	// SupabaseURL / SupabaseAnonKey point at the self-hosted GoTrue we extend
	// for the OAuth scope-elevation flow.
	SupabaseURL     string
	SupabaseAnonKey string
	// PublicBaseURL is uwacd's externally reachable base, used to build the
	// OAuth redirect_to (e.g. https://uwac.matrix.paxeer.app).
	PublicBaseURL string
	// DatabaseURI is the Postgres DSN for the token vault (empty = in-memory,
	// dev only).
	DatabaseURI string
	// ConnectorsDir is where connector specs are loaded from.
	ConnectorsDir string
	// ChallengeTTL bounds how long an agent-auth nonce stays valid.
	ChallengeTTL time.Duration
	// Dev relaxes prod fail-closed checks (UWAC_ENV=development).
	Dev bool
}

const (
	defaultAPIAddr      = ":8646"
	defaultChallengeTTL = 120 * time.Second
	devVaultSecret      = "uwac-dev-vault-secret-do-not-use-in-prod"
)

// Load resolves configuration: kvx overlay first, env overrides, then defaults.
func Load() (*Config, error) {
	path := os.Getenv("UWAC_CONFIG")
	if path == "" {
		path = "uwac.config.kvx"
	}
	doc, _, err := parseKVXFile(path)
	if err != nil {
		return nil, fmt.Errorf("uwac config: %w", err)
	}

	dev := pick("UWAC_ENV", doc.str("server", "env"), "") == "development"

	cfg := &Config{
		APIAddr:         pick("UWAC_ADDR", doc.str("server", "addr"), defaultAPIAddr),
		AuthToken:       pick("MATRIX_UWAC_TOKEN", doc.str("server", "auth_token"), ""),
		SupabaseURL:     trimSlash(pick("UWAC_SUPABASE_URL", doc.str("gotrue", "url"), "")),
		SupabaseAnonKey: pick("UWAC_SUPABASE_ANON_KEY", doc.str("gotrue", "anon_key"), ""),
		PublicBaseURL:   trimSlash(pick("UWAC_PUBLIC_BASE_URL", doc.str("server", "public_base_url"), "")),
		DatabaseURI:     pick("UWAC_DATABASE_URI", doc.str("vault", "database_uri"), ""),
		ConnectorsDir:   pick("UWAC_CONNECTORS_DIR", doc.str("connectors", "dir"), "connectors"),
		ChallengeTTL:    time.Duration(doc.uint64Or("auth", "challenge_ttl_seconds", uint64(defaultChallengeTTL/time.Second))) * time.Second,
		Dev:             dev,
	}

	secret := pick("UWAC_VAULT_KEY", doc.str("vault", "key"), "")
	if secret == "" {
		if !dev {
			return nil, fmt.Errorf("uwac config: UWAC_VAULT_KEY is required in production (set UWAC_ENV=development to use a dev key)")
		}
		secret = devVaultSecret
	}
	key := sha256.Sum256([]byte(secret))
	cfg.VaultKey = key[:]

	if cfg.ChallengeTTL <= 0 {
		cfg.ChallengeTTL = defaultChallengeTTL
	}
	return cfg, nil
}

// pick returns the first non-empty of env[key], kvxVal, def.
func pick(envKey, kvxVal, def string) string {
	if v := os.Getenv(envKey); v != "" {
		return v
	}
	if kvxVal != "" {
		return kvxVal
	}
	return def
}

func trimSlash(s string) string {
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}
