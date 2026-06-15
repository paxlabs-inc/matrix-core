// Package config loads layerxd configuration from the environment with an
// optional layerx.config.kvx overlay (env always wins). Mirrors chronos/uwac/
// tachyon's config layering so operators get one consistent knob story. See
// layerx.frozen.kvx [config].
package config

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

// Config is the resolved layerxd runtime configuration.
type Config struct {
	// Port is the box-local listen port (default 9098).
	Port int
	// PostgresURI is the layerx ledger DB DSN. Required.
	PostgresURI string
	// MigrationsDir holds the forward-only SQL migrations (default "migrations").
	MigrationsDir string

	// TransportToken is the shared bearer the MCP proxy must present
	// (= MATRIX_LAYERX_TOKEN on the daemon side). Empty disables transport auth
	// (loopback/dev only — logged as a warning).
	TransportToken string
	// AgentAuthSecret keys the agent-DID nonces + principal tokens (HMAC).
	AgentAuthSecret string
	// ChallengeTTL bounds how long an agent-auth nonce stays valid.
	ChallengeTTL time.Duration
	// TokenTTL bounds how long a minted principal token stays valid.
	TokenTTL time.Duration
	// SequencerSeedHex is the 64-hex ed25519 seed the sequencer signs receipts
	// with. Empty in dev -> an ephemeral key is generated at boot.
	SequencerSeedHex string

	// Chain / contract knobs (used by the settlement worker + deposit watcher).
	ChainRPC       string
	VaultAddr      string
	AnchorAddr     string
	DEXRouter      string
	USDLAddr       string
	OperatorKeyEnv string // name only — the key value is read at use time, never stored in config logs

	// Window is the micropayment net-settlement window (default 12h).
	Window time.Duration
	// MicroThresholdUSDX is the micro-USDX value below which a transfer is a
	// batched micropayment; at/above it auto-promotes to force-settle.
	MicroThresholdUSDX int64

	// Dev relaxes prod fail-closed secret checks (LAYERX_DEV=1).
	Dev bool
}

const (
	defaultPort           = 9098
	defaultChallengeTTL   = 120 * time.Second
	defaultTokenTTL       = 24 * time.Hour
	defaultWindow         = 12 * time.Hour
	defaultMicroThreshold = 1_000_000 // 1 USDX in micro-USDX
	defaultReserveAsset   = "USDL"
)

// Load resolves configuration: kvx overlay first, env overrides, then defaults.
func Load() (*Config, error) {
	path := os.Getenv("LAYERX_CONFIG")
	if path == "" {
		path = "layerx.config.kvx"
	}
	doc, _, err := parseKVXFile(path)
	if err != nil {
		return nil, fmt.Errorf("layerx config: %w", err)
	}

	dev := pick("LAYERX_DEV", doc.str("server", "dev"), "") == "1"

	cfg := &Config{
		Port:               int(pickUint("LAYERX_PORT", doc.uint64Or("server", "port", defaultPort), defaultPort)),
		PostgresURI:        pick("LAYERX_POSTGRES_URI", doc.str("store", "postgres_uri"), ""),
		MigrationsDir:      pick("LAYERX_MIGRATIONS_DIR", doc.str("store", "migrations_dir"), "migrations"),
		TransportToken:     pick("LAYERX_TOKEN", doc.str("auth", "transport_token"), ""),
		AgentAuthSecret:    pick("LAYERX_AGENT_AUTH_SECRET", doc.str("auth", "agent_secret"), ""),
		ChallengeTTL:       time.Duration(doc.uint64Or("auth", "challenge_ttl_seconds", uint64(defaultChallengeTTL/time.Second))) * time.Second,
		TokenTTL:           time.Duration(doc.uint64Or("auth", "token_ttl_seconds", uint64(defaultTokenTTL/time.Second))) * time.Second,
		SequencerSeedHex:   pick("LAYERX_SEQUENCER_KEY", doc.str("auth", "sequencer_seed"), ""),
		ChainRPC:           pick("LAYERX_CHAIN_RPC", doc.str("chain", "rpc"), ""),
		VaultAddr:          pick("LAYERX_VAULT_ADDR", doc.str("chain", "vault"), ""),
		AnchorAddr:         pick("LAYERX_ANCHOR_ADDR", doc.str("chain", "anchor"), ""),
		DEXRouter:          pick("LAYERX_DEX_ROUTER", doc.str("chain", "dex_router"), ""),
		USDLAddr:           pick("LAYERX_USDL_ADDR", doc.str("chain", "usdl"), ""),
		OperatorKeyEnv:     "LAYERX_OPERATOR_KEY",
		Window:             time.Duration(pickUint("LAYERX_WINDOW_SECONDS", doc.uint64Or("settlement", "window_seconds", uint64(defaultWindow/time.Second)), uint64(defaultWindow/time.Second))) * time.Second,
		MicroThresholdUSDX: int64(pickUint("LAYERX_MICRO_THRESHOLD", doc.uint64Or("settlement", "micro_threshold_micro_usdx", defaultMicroThreshold), defaultMicroThreshold)),
		Dev:                dev,
	}

	if cfg.PostgresURI == "" {
		return nil, fmt.Errorf("layerx config: LAYERX_POSTGRES_URI is required")
	}
	if !dev {
		if cfg.TransportToken == "" {
			return nil, fmt.Errorf("layerx config: LAYERX_TOKEN is required in production (set LAYERX_DEV=1 for local skeleton boot)")
		}
		if cfg.AgentAuthSecret == "" {
			return nil, fmt.Errorf("layerx config: LAYERX_AGENT_AUTH_SECRET is required in production")
		}
	}
	if cfg.AgentAuthSecret == "" {
		cfg.AgentAuthSecret = "layerx-dev-agent-secret-do-not-use-in-prod"
	}
	if cfg.ChallengeTTL <= 0 {
		cfg.ChallengeTTL = defaultChallengeTTL
	}
	if cfg.TokenTTL <= 0 {
		cfg.TokenTTL = defaultTokenTTL
	}
	if cfg.Window <= 0 {
		cfg.Window = defaultWindow
	}
	if cfg.MicroThresholdUSDX <= 0 {
		cfg.MicroThresholdUSDX = defaultMicroThreshold
	}
	return cfg, nil
}

// ReserveAsset is the canonical reserve symbol (USDL) — fixed by the frozen spec.
func (c *Config) ReserveAsset() string { return defaultReserveAsset }

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
