// Package config loads layerxd configuration from the environment with an
// optional layerx.config.kvx overlay (env always wins). Mirrors chronos/uwac/
// tachyon's config layering so operators get one consistent knob story. See
// layerx.frozen.kvx [config].
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
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

	// TransportToken is the shared bearer the MCP proxy may present
	// (= MATRIX_LAYERX_TOKEN on the daemon side). Since PHASE 4 (full-transparency
	// rollup) it is an OPTIONAL fleet gate, not the public gate: the public RPC /
	// explorer read surface and DID-signed writes never require it. It is enforced
	// on the principal/write endpoints only when RequireTransport is set.
	TransportToken string
	// RequireTransport enforces the transport bearer on the non-public endpoints
	// (legacy private-fleet mode). Default false: the public RPC route does not
	// gate on it (the DID signature IS the authorization, invariant i6).
	RequireTransport bool
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
	// OperatorKeyPresent records whether the operator key env was set at boot
	// (presence only; the secret value is never read into config).
	OperatorKeyPresent bool

	// DepositStartBlock is the block the deposit watcher begins its first scan
	// from (the Vault deploy block); 0 scans from genesis. Ignored once a cursor
	// has been persisted.
	DepositStartBlock uint64
	// ChainConfirmations is the block lag before a head block is treated final.
	// Paxeer has instant CometBFT finality (no reorgs), so 0 is correct; kept
	// configurable for safety.
	ChainConfirmations uint64
	// DepositPollInterval is how often the deposit watcher polls for new events.
	DepositPollInterval time.Duration

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
	defaultDepositPoll    = 15 * time.Second
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
	requireTransport := isTruthy(pick("LAYERX_REQUIRE_TRANSPORT", doc.str("auth", "require_transport"), ""))

	cfg := &Config{
		Port:                int(pickUint("LAYERX_PORT", doc.uint64Or("server", "port", defaultPort), defaultPort)),
		PostgresURI:         pick("LAYERX_POSTGRES_URI", doc.str("store", "postgres_uri"), ""),
		MigrationsDir:       pick("LAYERX_MIGRATIONS_DIR", doc.str("store", "migrations_dir"), "migrations"),
		TransportToken:      pick("LAYERX_TOKEN", doc.str("auth", "transport_token"), ""),
		RequireTransport:    requireTransport,
		AgentAuthSecret:     pick("LAYERX_AGENT_AUTH_SECRET", doc.str("auth", "agent_secret"), ""),
		ChallengeTTL:        time.Duration(doc.uint64Or("auth", "challenge_ttl_seconds", uint64(defaultChallengeTTL/time.Second))) * time.Second,
		TokenTTL:            time.Duration(doc.uint64Or("auth", "token_ttl_seconds", uint64(defaultTokenTTL/time.Second))) * time.Second,
		SequencerSeedHex:    pick("LAYERX_SEQUENCER_KEY", doc.str("auth", "sequencer_seed"), ""),
		ChainRPC:            pick("LAYERX_CHAIN_RPC", doc.str("chain", "rpc"), ""),
		VaultAddr:           pick("LAYERX_VAULT_ADDR", doc.str("chain", "vault"), ""),
		AnchorAddr:          pick("LAYERX_ANCHOR_ADDR", doc.str("chain", "anchor"), ""),
		DEXRouter:           pick("LAYERX_DEX_ROUTER", doc.str("chain", "dex_router"), ""),
		USDLAddr:            pick("LAYERX_USDL_ADDR", doc.str("chain", "usdl"), ""),
		OperatorKeyEnv:      "LAYERX_OPERATOR_KEY",
		OperatorKeyPresent:  os.Getenv("LAYERX_OPERATOR_KEY") != "",
		DepositStartBlock:   pickUint("LAYERX_DEPOSIT_START_BLOCK", doc.uint64Or("chain", "deposit_start_block", 0), 0),
		ChainConfirmations:  pickUint("LAYERX_CHAIN_CONFIRMATIONS", doc.uint64Or("chain", "confirmations", 0), 0),
		DepositPollInterval: time.Duration(pickUint("LAYERX_DEPOSIT_POLL_SECONDS", doc.uint64Or("chain", "deposit_poll_seconds", uint64(defaultDepositPoll/time.Second)), uint64(defaultDepositPoll/time.Second))) * time.Second,
		Window:              time.Duration(pickUint("LAYERX_WINDOW_SECONDS", doc.uint64Or("settlement", "window_seconds", uint64(defaultWindow/time.Second)), uint64(defaultWindow/time.Second))) * time.Second,
		MicroThresholdUSDX:  int64(pickUint("LAYERX_MICRO_THRESHOLD", doc.uint64Or("settlement", "micro_threshold_micro_usdx", defaultMicroThreshold), defaultMicroThreshold)),
		Dev:                 dev,
	}

	if cfg.PostgresURI == "" {
		return nil, fmt.Errorf("layerx config: LAYERX_POSTGRES_URI is required")
	}
	if !dev {
		// The transport bearer is only mandatory in legacy private-fleet mode
		// (RequireTransport). The default public RPC route authorizes writes by
		// the DID signature alone (PHASE 4), so LAYERX_TOKEN is optional there.
		if cfg.RequireTransport && cfg.TransportToken == "" {
			return nil, fmt.Errorf("layerx config: LAYERX_TOKEN is required when LAYERX_REQUIRE_TRANSPORT=1 (set LAYERX_DEV=1 for local skeleton boot)")
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
	if cfg.DepositPollInterval <= 0 {
		cfg.DepositPollInterval = defaultDepositPoll
	}
	if cfg.MicroThresholdUSDX <= 0 {
		cfg.MicroThresholdUSDX = defaultMicroThreshold
	}
	return cfg, nil
}

// ReserveAsset is the canonical reserve symbol (USDL) — fixed by the frozen spec.
func (c *Config) ReserveAsset() string { return defaultReserveAsset }

// isTruthy reports whether a string is an affirmative flag value.
func isTruthy(s string) bool {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "1", "true", "yes", "on":
		return true
	}
	return false
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
