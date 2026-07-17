package config

import (
	"testing"
	"time"
)

// isolateEnv blanks every LAYERX_* knob this package reads so a test starts from
// a known-clean environment regardless of the host shell.
func isolateEnv(t *testing.T) {
	t.Helper()
	for _, k := range []string{
		"LAYERX_CONFIG", "LAYERX_DEV", "LAYERX_PORT", "LAYERX_POSTGRES_URI",
		"LAYERX_MIGRATIONS_DIR", "LAYERX_TOKEN", "LAYERX_AGENT_AUTH_SECRET",
		"LAYERX_SEQUENCER_KEY", "LAYERX_CHAIN_RPC", "LAYERX_ANCHOR_HISTORY_URL", "LAYERX_VAULT_ADDR",
		"LAYERX_ANCHOR_ADDR", "LAYERX_DEX_ROUTER", "LAYERX_USDL_ADDR",
		"LAYERX_WINDOW_SECONDS", "LAYERX_MICRO_THRESHOLD",
		"LAYERX_PERPS_MODE", "LAYERX_PERPS_MARKET_MODES",
	} {
		t.Setenv(k, "")
	}
	t.Setenv("LAYERX_CONFIG", "/nonexistent/layerx.config.kvx") // force pure-env load
}

func TestLoadDevDefaults(t *testing.T) {
	isolateEnv(t)
	t.Setenv("LAYERX_DEV", "1")
	t.Setenv("LAYERX_POSTGRES_URI", "postgres://layerx@127.0.0.1/layerx")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != defaultPort {
		t.Errorf("Port = %d, want %d", cfg.Port, defaultPort)
	}
	if cfg.BindAddr != defaultBindAddr {
		t.Errorf("BindAddr = %q, want %q (loopback-only behind nginx)", cfg.BindAddr, defaultBindAddr)
	}
	if cfg.Window != defaultWindow {
		t.Errorf("Window = %v, want %v", cfg.Window, defaultWindow)
	}
	if cfg.MicroThresholdUSDX != defaultMicroThreshold {
		t.Errorf("MicroThreshold = %d, want %d", cfg.MicroThresholdUSDX, defaultMicroThreshold)
	}
	if cfg.ReserveAsset() != "USDL" {
		t.Errorf("ReserveAsset = %q, want USDL", cfg.ReserveAsset())
	}
	if cfg.PerpsMode != "OFF" {
		t.Errorf("PerpsMode = %q, want OFF", cfg.PerpsMode)
	}
	if len(cfg.PerpsMarketModes) != 0 {
		t.Errorf("PerpsMarketModes = %#v, want empty", cfg.PerpsMarketModes)
	}
	if cfg.AgentAuthSecret == "" {
		t.Error("dev AgentAuthSecret should fall back to a non-empty value")
	}
	if !cfg.Dev {
		t.Error("Dev should be true")
	}
}

func TestLoadEnvOverrides(t *testing.T) {
	isolateEnv(t)
	t.Setenv("LAYERX_DEV", "1")
	t.Setenv("LAYERX_POSTGRES_URI", "postgres://x")
	t.Setenv("LAYERX_PORT", "9999")
	t.Setenv("LAYERX_BIND_ADDR", "0.0.0.0")
	t.Setenv("LAYERX_WINDOW_SECONDS", "60")
	t.Setenv("LAYERX_MICRO_THRESHOLD", "500000")
	t.Setenv("LAYERX_ANCHOR_HISTORY_URL", "https://history.example")
	t.Setenv("LAYERX_PERPS_MODE", "SHADOW")
	t.Setenv("LAYERX_PERPS_MARKET_MODES", "BTC=CANARY,ETH=REDUCE_ONLY")

	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Port != 9999 {
		t.Errorf("Port = %d, want 9999", cfg.Port)
	}
	if cfg.BindAddr != "0.0.0.0" {
		t.Errorf("BindAddr = %q, want 0.0.0.0", cfg.BindAddr)
	}
	if cfg.Window != 60*time.Second {
		t.Errorf("Window = %v, want 60s", cfg.Window)
	}
	if cfg.MicroThresholdUSDX != 500000 {
		t.Errorf("MicroThreshold = %d, want 500000", cfg.MicroThresholdUSDX)
	}
	if cfg.AnchorHistoryURL != "https://history.example" {
		t.Errorf("AnchorHistoryURL = %q, want https://history.example", cfg.AnchorHistoryURL)
	}
	if cfg.PerpsMode != "SHADOW" {
		t.Errorf("PerpsMode = %q, want SHADOW", cfg.PerpsMode)
	}
	if got := string(cfg.PerpsMarketModes["BTC"]); got != "CANARY" {
		t.Errorf("BTC perps mode = %q, want CANARY", got)
	}
	if got := string(cfg.PerpsMarketModes["ETH"]); got != "REDUCE_ONLY" {
		t.Errorf("ETH perps mode = %q, want REDUCE_ONLY", got)
	}
}

func TestLoadRejectsInvalidPerpsModes(t *testing.T) {
	isolateEnv(t)
	t.Setenv("LAYERX_DEV", "1")
	t.Setenv("LAYERX_POSTGRES_URI", "postgres://x")
	t.Setenv("LAYERX_PERPS_MODE", "enabled")
	if _, err := Load(); err == nil {
		t.Fatal("Load must reject an invalid global perps mode")
	}

	t.Setenv("LAYERX_PERPS_MODE", "OFF")
	t.Setenv("LAYERX_PERPS_MARKET_MODES", "DOGE=ACTIVE")
	if _, err := Load(); err == nil {
		t.Fatal("Load must reject an unknown perps market")
	}
}

func TestLoadRequiresPostgres(t *testing.T) {
	isolateEnv(t)
	t.Setenv("LAYERX_DEV", "1")
	if _, err := Load(); err == nil {
		t.Fatal("Load must fail without LAYERX_POSTGRES_URI")
	}
}

func TestLoadProdFailClosed(t *testing.T) {
	isolateEnv(t)
	// Not dev, has DB, but no transport token -> must fail closed.
	t.Setenv("LAYERX_POSTGRES_URI", "postgres://x")
	if _, err := Load(); err == nil {
		t.Fatal("Load must fail closed without LAYERX_TOKEN in production")
	}

	// With transport token but no agent secret -> still fail closed.
	t.Setenv("LAYERX_TOKEN", "transport-secret")
	if _, err := Load(); err == nil {
		t.Fatal("Load must fail closed without LAYERX_AGENT_AUTH_SECRET in production")
	}

	// Fully configured prod -> ok.
	t.Setenv("LAYERX_AGENT_AUTH_SECRET", "agent-secret")
	if _, err := Load(); err != nil {
		t.Fatalf("fully-configured prod Load should succeed: %v", err)
	}
}
