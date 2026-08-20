package vault

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func devKEKHex() string { return hex.EncodeToString(bytes.Repeat([]byte{0x5c}, keyLen)) }

func TestBootRefusesWhenRequiredAndNoKey(t *testing.T) {
	cfg := Config{Required: true, DataDir: t.TempDir(), UserDID: "did:matrix:alice"}
	if _, err := Boot(context.Background(), cfg); !errors.Is(err, ErrVaultRequired) {
		t.Fatalf("boot want ErrVaultRequired, got %v", err)
	}
}

func TestBootProvisionsAndReloads(t *testing.T) {
	dir := t.TempDir()
	cfg := Config{Required: true, DataDir: dir, UserDID: "did:matrix:alice", KEKHex: devKEKHex()}
	ctx := context.Background()

	s1, err := Boot(ctx, cfg)
	if err != nil {
		t.Fatalf("boot 1: %v", err)
	}
	if !s1.Encrypting() {
		t.Fatal("session 1 not encrypting")
	}
	kfPath := KeyfilePath(dir)
	if fi, err := os.Stat(kfPath); err != nil {
		t.Fatalf("keyfile missing: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Fatalf("keyfile mode = %o, want 0600", fi.Mode().Perm())
	}

	ad := recAD("did:matrix:alice", 1)
	obj, err := s1.MaybeSealRecord(ad, []byte("boot payload"))
	if err != nil {
		t.Fatalf("seal: %v", err)
	}

	// A second boot loads the SAME key and decrypts the first session's object.
	s2, err := Boot(ctx, cfg)
	if err != nil {
		t.Fatalf("boot 2: %v", err)
	}
	got, err := s2.UserVault().OpenRecord(ad, obj)
	if err != nil || string(got) != "boot payload" {
		t.Fatalf("reload open: got %q err %v", got, err)
	}
}

func TestBootWrongKEKRefusesWhenRequired(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	// Provision under KEK A.
	if _, err := Boot(ctx, Config{Required: true, DataDir: dir, UserDID: "did:matrix:alice", KEKHex: devKEKHex()}); err != nil {
		t.Fatalf("provision boot: %v", err)
	}
	// Reboot with a different KEK: the wrapped key cannot be unwrapped -> refuse.
	otherKEK := hex.EncodeToString(bytes.Repeat([]byte{0x11}, keyLen))
	if _, err := Boot(ctx, Config{Required: true, DataDir: dir, UserDID: "did:matrix:alice", KEKHex: otherKEK}); !errors.Is(err, ErrVaultRequired) {
		t.Fatalf("wrong-KEK boot want ErrVaultRequired, got %v", err)
	}
}

func TestBootWrongUserKeyfileRefuses(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	if _, err := Boot(ctx, Config{Required: true, DataDir: dir, UserDID: "did:matrix:alice", KEKHex: devKEKHex()}); err != nil {
		t.Fatalf("provision: %v", err)
	}
	// A daemon for a different user pointed at alice's keyfile must refuse.
	if _, err := Boot(ctx, Config{Required: true, DataDir: dir, UserDID: "did:matrix:mallory", KEKHex: devKEKHex()}); !errors.Is(err, ErrVaultRequired) {
		t.Fatalf("cross-user boot want ErrVaultRequired, got %v", err)
	}
}

func TestBootPlaintextOnlyWhenExplicitlyDisabled(t *testing.T) {
	dir := t.TempDir()
	s, err := Boot(context.Background(), Config{Required: false, DataDir: dir, UserDID: "did:matrix:alice"})
	if err != nil {
		t.Fatalf("dev boot: %v", err)
	}
	if s.Encrypting() {
		t.Fatal("dev session should not be encrypting without a key")
	}
	// A dev session passes writes through as plaintext (no keyfile is minted).
	pt := []byte("dev plaintext line")
	out, err := s.MaybeSealRecord(recAD("did:matrix:alice", 1), pt)
	if err != nil || !bytes.Equal(out, pt) {
		t.Fatalf("dev passthrough: got %q err %v", out, err)
	}
	if _, err := os.Stat(KeyfilePath(dir)); !os.IsNotExist(err) {
		t.Fatalf("dev session minted a keyfile: err %v", err)
	}
}

func TestMaybeSealHardErrorsWhenRequiredWithoutKey(t *testing.T) {
	// A required session that has no vault must fail writes hard — never plaintext.
	s := &Session{required: true}
	if _, err := s.MaybeSealRecord(recAD("u", 1), []byte("secret")); !errors.Is(err, ErrVaultRequired) {
		t.Fatalf("record want ErrVaultRequired, got %v", err)
	}
	if _, err := s.MaybeSealFile(AD{User: "u", Store: "s", Stream: "f", Schema: "v"}, []byte("secret")); !errors.Is(err, ErrVaultRequired) {
		t.Fatalf("file want ErrVaultRequired, got %v", err)
	}
}

func TestResolveProviderFromKEKFile(t *testing.T) {
	dir := t.TempDir()
	kekFile := filepath.Join(dir, "kek.bin")
	if err := os.WriteFile(kekFile, bytes.Repeat([]byte{0x77}, keyLen), 0o600); err != nil {
		t.Fatalf("write kek: %v", err)
	}
	s, err := Boot(context.Background(), Config{Required: true, DataDir: dir, UserDID: "did:matrix:alice", KEKFile: kekFile})
	if err != nil {
		t.Fatalf("boot with kek file: %v", err)
	}
	if !s.Encrypting() {
		t.Fatal("expected encrypting session from kek file")
	}
}

func TestEnvRequiredIsOptIn(t *testing.T) {
	// Enforcement is opt-in: only an explicit truthy value requires the vault, so
	// an un-provisioned machine boots plaintext instead of bricking. The platform
	// makes prod fail-closed by injecting a truthy value alongside a KEK.
	for v, want := range map[string]bool{"1": true, "true": true, "TRUE": true, "yes": true, "on": true, "": false, "0": false, "false": false, "off": false, "no": false, "garbage": false} {
		if got := envRequired(v); got != want {
			t.Fatalf("envRequired(%q) = %v, want %v", v, got, want)
		}
	}
}
