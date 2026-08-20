package task

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"centra/packages/vault"
)

// vaultSessionFor boots a real encrypting vault.Session for a user, keyed by a
// per-call temp DataDir so each user gets a distinct wrapped user key.
func vaultSessionFor(t *testing.T, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")) // 32 bytes
	sess, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: user, KEKHex: kek,
	})
	if err != nil {
		t.Fatalf("vault boot: %v", err)
	}
	if !sess.Encrypting() {
		t.Fatal("expected an encrypting session")
	}
	return sess
}

// TestVaultTaskRoundTrip proves the task ledger seals records on disk (no
// plaintext) while Begin/Get/Running keep working through the store.
func TestVaultTaskRoundTrip(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)

	s := Open(dir)
	s.SetVault(sess, user)
	s.Begin("conv-1", "run-1", "the secret objective")

	raw, err := os.ReadFile(filepath.Join(dir, "conv-1.json"))
	if err != nil {
		t.Fatalf("read task file: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("task file is not sealed")
	}
	if strings.Contains(string(raw), "secret objective") {
		t.Fatal("plaintext leaked into the sealed task file")
	}

	got, ok := s.Get("conv-1")
	if !ok || got.Objective != "the secret objective" || got.RunID != "run-1" {
		t.Fatalf("Get mismatch: ok=%v task=%+v", ok, got)
	}
	// Running derives the conv id from the file name — the AD must line up.
	running := s.Running()
	if len(running) != 1 || running[0].ConvID != "conv-1" {
		t.Fatalf("Running mismatch: %+v", running)
	}
}

// TestVaultTaskWrongUser proves another user's key cannot read the ledger.
func TestVaultTaskWrongUser(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	s.Begin("conv-1", "run-1", "private")

	s2 := Open(dir)
	s2.SetVault(vaultSessionFor(t, "did:matrix:mallory"), "did:matrix:mallory")
	if _, ok := s2.Get("conv-1"); ok {
		t.Fatal("wrong user read a sealed task record")
	}
	if running := s2.Running(); len(running) != 0 {
		t.Fatalf("wrong user listed sealed tasks: %+v", running)
	}
}

// TestVaultTaskLegacyPlaintext proves a pre-vault plaintext record stays
// readable after the vault is wired (sniffing reader).
func TestVaultTaskLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	legacy := Task{
		ConvID: "conv-old", RunID: "run-old", Objective: "old plain objective",
		Status: StatusRunning, Created: time.Now().UTC(), Updated: time.Now().UTC(),
	}
	data, _ := json.Marshal(legacy)
	if err := os.WriteFile(filepath.Join(dir, "conv-old.json"), data, 0o600); err != nil {
		t.Fatalf("seed legacy file: %v", err)
	}

	s := Open(dir)
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	got, ok := s.Get("conv-old")
	if !ok || got.Objective != "old plain objective" {
		t.Fatalf("legacy plaintext unreadable: ok=%v task=%+v", ok, got)
	}
	// A write under the vault re-seals the record.
	s.Checkpoint("conv-old", "run-old", 1, "note")
	raw, _ := os.ReadFile(filepath.Join(dir, "conv-old.json"))
	if !vault.IsVault(raw) {
		t.Fatal("record not re-sealed after a vault write")
	}
}
