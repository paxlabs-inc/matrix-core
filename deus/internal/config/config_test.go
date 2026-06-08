package config

import (
	"os"
	"testing"
)

func TestLoadRequiresPostgresURI(t *testing.T) {
	t.Setenv("DEUS_POSTGRES_URI", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when DEUS_POSTGRES_URI empty")
	}
}

func TestLoadDevRelaxesOptional(t *testing.T) {
	t.Setenv("DEUS_POSTGRES_URI", "postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable")
	t.Setenv("DEUS_DEV", "1")
	t.Setenv("PAXEER_RPC_URL", "")
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.Dev {
		t.Fatal("expected dev mode")
	}
	if cfg.Port != 9095 {
		t.Fatalf("port = %d, want 9095", cfg.Port)
	}
}

func TestLoadProdRequiresRPC(t *testing.T) {
	t.Setenv("DEUS_POSTGRES_URI", "postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable")
	t.Setenv("DEUS_DEV", "0")
	t.Setenv("PAXEER_RPC_URL", "")
	_, err := Load()
	if err == nil {
		t.Fatal("expected error when PAXEER_RPC_URL empty in prod mode")
	}
}

func TestMain(m *testing.M) {
	keys := []string{
		"DEUS_POSTGRES_URI", "DEUS_DEV", "PAXEER_RPC_URL",
		"DEUS_OBJSTORE_ENDPOINT", "DEUS_OBJSTORE_ACCESS_KEY",
		"DEUS_OBJSTORE_SECRET_KEY", "DEUS_OBJSTORE_BUCKET",
		"DEUS_GATEWAY_SIGNING_KEY_REF", "MATRIX_WALLET_API_URL",
		"DEUS_SERVICE_REGISTRY_ADDR",
	}
	prev := make(map[string]string, len(keys))
	for _, k := range keys {
		prev[k] = os.Getenv(k)
		_ = os.Unsetenv(k)
	}
	code := m.Run()
	for k, v := range prev {
		if v == "" {
			_ = os.Unsetenv(k)
		} else {
			_ = os.Setenv(k, v)
		}
	}
	os.Exit(code)
}
