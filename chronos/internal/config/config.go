// Package config loads chronosd configuration from the environment with an
// optional chronos.config.kvx overlay (env always wins). Mirrors uwac/tachyon's
// config layering so operators get one consistent knob story.
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the resolved chronosd runtime configuration.
type Config struct {
	// Port is the box-local listen port (default 9096).
	Port int
	// PostgresURI is the shared matrix DB DSN on the box (db=matrix). Required.
	PostgresURI string
	// MigrationsDir holds the forward-only SQL migrations (default "migrations").
	MigrationsDir string

	// TransportToken is the shared bearer the MCP proxy must present
	// (= MATRIX_CHRONOS_TOKEN on the daemon side). Empty disables transport
	// auth (loopback/dev only — logged as a warning).
	TransportToken string
	// AgentAuthSecret keys the agent-DID nonces + principal tokens (HMAC).
	AgentAuthSecret string
	// ChallengeTTL bounds how long an agent-auth nonce stays valid.
	ChallengeTTL time.Duration
	// TokenTTL bounds how long a minted principal token stays valid.
	TokenTTL time.Duration

	// RouterWakeURL is the router internal wake endpoint.
	RouterWakeURL string
	// WakeToken is the shared secret matching the router's ROUTER_WAKE_TOKEN.
	WakeToken string

	// Tick is the dispatch worker poll interval (default 1s).
	Tick time.Duration
	// MaxFailures is the default wake-delivery retry ceiling per alarm.
	MaxFailures int
	// ClaimLease is how long a claimed-but-unfinished alarm stays leased before
	// another worker may reclaim it (at-least-once safety on a crash mid-fire).
	ClaimLease time.Duration
	// ClaimBatch is the max alarms claimed per tick.
	ClaimBatch int

	// Dev relaxes prod fail-closed secret checks (CHRONOS_DEV=1).
	Dev bool
}

const (
	defaultPort          = 9096
	defaultChallengeTTL  = 120 * time.Second
	defaultTokenTTL      = 24 * time.Hour
	defaultTick          = time.Second
	defaultMaxFailures   = 5
	defaultClaimLease    = 2 * time.Minute
	defaultClaimBatch    = 100
	defaultRouterWakeURL = "http://127.0.0.1:8088/internal/wake"
)

// Load resolves configuration: kvx overlay first, env overrides, then defaults.
func Load() (*Config, error) {
	path := os.Getenv("CHRONOS_CONFIG")
	if path == "" {
		path = "chronos.config.kvx"
	}
	doc, _, err := parseKVXFile(path)
	if err != nil {
		return nil, fmt.Errorf("chronos config: %w", err)
	}

	dev := pick("CHRONOS_DEV", doc.str("server", "dev"), "") == "1"

	cfg := &Config{
		Port:            int(pickUint("CHRONOS_PORT", doc.uint64Or("server", "port", defaultPort), defaultPort)),
		PostgresURI:     pick("CHRONOS_POSTGRES_URI", doc.str("store", "postgres_uri"), ""),
		MigrationsDir:   pick("CHRONOS_MIGRATIONS_DIR", doc.str("store", "migrations_dir"), "migrations"),
		TransportToken:  pick("CHRONOS_TOKEN", doc.str("auth", "transport_token"), ""),
		AgentAuthSecret: pick("CHRONOS_AGENT_AUTH_SECRET", doc.str("auth", "agent_secret"), ""),
		ChallengeTTL:    time.Duration(doc.uint64Or("auth", "challenge_ttl_seconds", uint64(defaultChallengeTTL/time.Second))) * time.Second,
		TokenTTL:        time.Duration(doc.uint64Or("auth", "token_ttl_seconds", uint64(defaultTokenTTL/time.Second))) * time.Second,
		RouterWakeURL:   pick("CHRONOS_ROUTER_WAKE_URL", doc.str("wake", "router_url"), defaultRouterWakeURL),
		WakeToken:       pick("CHRONOS_WAKE_TOKEN", doc.str("wake", "token"), ""),
		Tick:            time.Duration(pickUint("CHRONOS_TICK_MS", doc.uint64Or("dispatch", "tick_ms", uint64(defaultTick/time.Millisecond)), uint64(defaultTick/time.Millisecond))) * time.Millisecond,
		MaxFailures:     int(pickUint("CHRONOS_MAX_FAILURES", doc.uint64Or("dispatch", "max_failures", defaultMaxFailures), defaultMaxFailures)),
		ClaimLease:      time.Duration(doc.uint64Or("dispatch", "claim_lease_seconds", uint64(defaultClaimLease/time.Second))) * time.Second,
		ClaimBatch:      int(doc.uint64Or("dispatch", "claim_batch", defaultClaimBatch)),
		Dev:             dev,
	}

	if cfg.PostgresURI == "" {
		return nil, fmt.Errorf("chronos config: CHRONOS_POSTGRES_URI is required")
	}
	if !dev {
		if cfg.TransportToken == "" {
			return nil, fmt.Errorf("chronos config: CHRONOS_TOKEN is required in production (set CHRONOS_DEV=1 for local skeleton boot)")
		}
		if cfg.AgentAuthSecret == "" {
			return nil, fmt.Errorf("chronos config: CHRONOS_AGENT_AUTH_SECRET is required in production")
		}
		if cfg.WakeToken == "" {
			return nil, fmt.Errorf("chronos config: CHRONOS_WAKE_TOKEN is required in production")
		}
	}
	if cfg.AgentAuthSecret == "" {
		cfg.AgentAuthSecret = "chronos-dev-agent-secret-do-not-use-in-prod"
	}

	if cfg.ChallengeTTL <= 0 {
		cfg.ChallengeTTL = defaultChallengeTTL
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = defaultTokenTTL
	}
	if cfg.Tick <= 0 {
		cfg.Tick = defaultTick
	}
	if cfg.MaxFailures <= 0 {
		cfg.MaxFailures = defaultMaxFailures
	}
	if cfg.ClaimLease <= 0 {
		cfg.ClaimLease = defaultClaimLease
	}
	if cfg.ClaimBatch <= 0 {
		cfg.ClaimBatch = defaultClaimBatch
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

// pickUint returns the env value (parsed as uint64) when set + valid, else the
// already-resolved kvx/default fallback.
func pickUint(envKey string, fallback, def uint64) uint64 {
	if v := os.Getenv(envKey); v != "" {
		if n, err := strconv.ParseUint(v, 10, 64); err == nil {
			return n
		}
	}
	if fallback != 0 {
		return fallback
	}
	return def
}
