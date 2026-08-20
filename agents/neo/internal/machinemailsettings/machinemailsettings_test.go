package machinemailsettings

import (
	"context"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"

	"centra/packages/vault"
)

func machineMailVault(t *testing.T, user string) *vault.Session {
	t.Helper()
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true,
		DataDir:  t.TempDir(),
		UserDID:  user,
		KEKHex:   hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatalf("boot vault: %v", err)
	}
	return session
}

func TestPersistentStoreRequiresEncryption(t *testing.T) {
	store := Open(t.TempDir())
	if err := store.Replace("mm_abcdefghijklmnopqrstuvwxyz123456"); !errors.Is(err, ErrEncryptionRequired) {
		t.Fatalf("expected ErrEncryptionRequired, got %v", err)
	}
}

func TestEncryptedRoundTripAndClear(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	session := machineMailVault(t, user)
	store := Open(dir)
	store.SetVault(session, user)
	key := "mm_abcdefghijklmnopqrstuvwxyz123456"
	if err := store.Replace(key); err != nil {
		t.Fatalf("replace: %v", err)
	}
	raw, err := os.ReadFile(store.path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !vault.IsVault(raw) || strings.Contains(string(raw), key) {
		t.Fatal("MachineMail API key was not sealed at rest")
	}
	reopened := Open(dir)
	reopened.SetVault(session, user)
	if reopened.View().APIKey != key {
		t.Fatal("encrypted API key did not round trip")
	}
	if err := reopened.Clear(); err != nil {
		t.Fatalf("clear: %v", err)
	}
	if _, err := os.Stat(reopened.path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("settings file still exists: %v", err)
	}
}
