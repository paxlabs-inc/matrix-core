package automatrixsettings

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"centra/packages/vault"
)

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

// TestVaultSettingsRoundTrip proves the settings file is sealed on disk yet
// reads back verbatim through a fresh store under the same user's vault.
func TestVaultSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)

	s := Open(dir)
	s.SetVault(sess, user)
	if err := s.SetEnabled(true, "alarm-secret-1"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	raw, err := os.ReadFile(s.path())
	if err != nil {
		t.Fatalf("read settings file: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("settings file is not sealed")
	}
	if strings.Contains(string(raw), "alarm-secret-1") {
		t.Fatal("plaintext leaked into the sealed settings file")
	}

	s2 := Open(dir)
	s2.SetVault(sess, user)
	if !s2.Enabled() || s2.AlarmID() != "alarm-secret-1" {
		t.Fatalf("round trip mismatch: enabled=%v alarm=%q", s2.Enabled(), s2.AlarmID())
	}
}

// TestVaultSettingsWrongUser proves another user's key cannot read the file —
// the store falls back to its zero (disabled) state.
func TestVaultSettingsWrongUser(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if err := s.SetEnabled(true, "alarm-1"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	s2 := Open(dir)
	s2.SetVault(vaultSessionFor(t, "did:matrix:mallory"), "did:matrix:mallory")
	if s2.Enabled() || s2.AlarmID() != "" {
		t.Fatalf("wrong user read sealed settings: enabled=%v alarm=%q", s2.Enabled(), s2.AlarmID())
	}
}

// TestVaultSettingsLegacyPlaintext proves a pre-vault plaintext file stays
// readable after the vault is wired, and the next write re-seals it.
func TestVaultSettingsLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	plain := Open(dir)
	if err := plain.SetEnabled(true, "alarm-legacy"); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if b, _ := os.ReadFile(plain.path()); !json.Valid(b) {
		t.Fatal("seed file should be plaintext JSON")
	}

	s := Open(dir)
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if !s.Enabled() || s.AlarmID() != "alarm-legacy" {
		t.Fatalf("legacy plaintext unreadable: enabled=%v alarm=%q", s.Enabled(), s.AlarmID())
	}
	if err := s.RecordTaskStarted(); err != nil {
		t.Fatalf("RecordTaskStarted: %v", err)
	}
	raw, _ := os.ReadFile(s.path())
	if !vault.IsVault(raw) {
		t.Fatal("settings not re-sealed after a vault write")
	}
}
