package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDefaults(t *testing.T) {
	t.Setenv("TACHYON_API_ADDR", "")
	t.Setenv("TACHYON_PROJECT_ROOT", "")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != defaultAPIAddr {
		t.Fatalf("APIAddr = %q, want %q", cfg.APIAddr, defaultAPIAddr)
	}
	if cfg.ForgePath != "forge" {
		t.Fatalf("ForgePath = %q", cfg.ForgePath)
	}
	if cfg.ProjectRoot == "" {
		t.Fatal("expected non-empty ProjectRoot")
	}
}

func TestLoadDotEnv(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")
	if err := os.WriteFile(envPath, []byte("TACHYON_API_ADDR=:9999\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != ":9999" {
		t.Fatalf("APIAddr = %q, want :9999", cfg.APIAddr)
	}
}
