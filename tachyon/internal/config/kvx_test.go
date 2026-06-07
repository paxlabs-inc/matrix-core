package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestParseKVX(t *testing.T) {
	t.Setenv("KVX_SECRET", "0xdeadbeef")
	src := `
# top comment
[server]
addr       = "127.0.0.1:9000"   # inline comment
auth_token = "${KVX_SECRET}"

[wallet]
mode = "self_hosted"

[wallet.self_hosted]
signer = "raw"

[policy.default]
spend_cap_wei = "1000"
allow         = ["0xAAA", "0xBBB"]
chains        = ["paxeer-mainnet"]

[chains.anvil]
rpc_url  = "http://127.0.0.1:8545"
chain_id = 31337
`
	dir := t.TempDir()
	path := filepath.Join(dir, "tachyon.config.kvx")
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	doc, ok, err := parseKVXFile(path)
	if err != nil || !ok {
		t.Fatalf("parse: ok=%v err=%v", ok, err)
	}

	if got := doc.str("server", "addr"); got != "127.0.0.1:9000" {
		t.Errorf("addr = %q", got)
	}
	if got := doc.str("server", "auth_token"); got != "0xdeadbeef" {
		t.Errorf("interpolated auth_token = %q", got)
	}
	if got := doc.str("wallet", "mode"); got != "self_hosted" {
		t.Errorf("mode = %q", got)
	}
	allow := doc.list("policy.default", "allow")
	if len(allow) != 2 || allow[0] != "0xAAA" || allow[1] != "0xBBB" {
		t.Errorf("allow = %v", allow)
	}
	if got := doc.uint64Or("chains.anvil", "chain_id", 0); got != 31337 {
		t.Errorf("chain_id = %d", got)
	}
	if subs := doc.sectionsWithPrefix("chains"); len(subs) != 1 || subs[0] != "anvil" {
		t.Errorf("chains subsections = %v", subs)
	}
}

func TestParseKVXMissingFile(t *testing.T) {
	doc, ok, err := parseKVXFile(filepath.Join(t.TempDir(), "nope.kvx"))
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if ok {
		t.Fatal("ok should be false for missing file")
	}
	if doc.str("server", "addr") != "" {
		t.Fatal("expected empty doc")
	}
}

func TestLoadKVXPrecedence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "tachyon.config.kvx")
	if err := os.WriteFile(path, []byte("[server]\naddr = \":7000\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig, _ := os.Getwd()
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(orig) })

	// kvx wins over default
	t.Setenv("TACHYON_API_ADDR", "")
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != ":7000" {
		t.Fatalf("kvx addr = %q, want :7000", cfg.APIAddr)
	}

	// env wins over kvx
	t.Setenv("TACHYON_API_ADDR", ":6000")
	cfg, err = Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.APIAddr != ":6000" {
		t.Fatalf("env addr = %q, want :6000", cfg.APIAddr)
	}
}
