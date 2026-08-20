// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package snapshot

// vault_test.go — ORACLE task 2.4 (snapshot half): the backup tarball is
// sealed with chunked streaming AEAD before leaving the machine, restores
// decrypt-verify (bootstrapping the key from the mirrored keyfile on a fresh
// volume), and truncation / chunk reordering / wrong-user all fail closed.
// No fakes: real vault sessions over a real KEK, real tar -I zstd archives of
// a real data tree, the real seal/open code paths Push and BootPull use. The
// mc transport (byte-copy of the object) is simulated by a filesystem copy —
// the bytes on the wire are exactly the file bytes either way.

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"

	"centra/packages/vault"
)

const testKEKHex = "3031323334353637383961626364656630313233343536373839616263646566" // "0123456789abcdef" x2

// bootVaultAt provisions (or loads) an encrypting session whose keyfile lives
// at vault.KeyfilePath(dataDir) — exactly like the daemon's own vault boot.
func bootVaultAt(t *testing.T, dataDir, user string) *vault.Session {
	t.Helper()
	sess, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: dataDir, UserDID: user, KEKHex: testKEKHex,
	})
	if err != nil {
		t.Fatalf("vault boot: %v", err)
	}
	if !sess.Encrypting() {
		t.Fatal("expected an encrypting session")
	}
	return sess
}

// seedDataTree writes a small realistic tree (nested dirs + content) and
// returns the path of a distinctive file for later comparison.
func seedDataTree(t *testing.T, dataDir string) string {
	t.Helper()
	sub := filepath.Join(dataDir, "conversations")
	if err := os.MkdirAll(sub, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	target := filepath.Join(sub, "conv-1.jsonl")
	if err := os.WriteFile(target, []byte(`{"content":"the private history"}`+"\n"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "executor.key"), []byte("aa11"), 0o600); err != nil {
		t.Fatalf("seed key: %v", err)
	}
	return target
}

func mgrOver(t *testing.T, dataDir string) *Manager {
	t.Helper()
	m, err := New(Config{
		DataDir:  dataDir,
		Endpoint: "http://127.0.0.1:9000",
		Bucket:   "matrix-state",
		UserID:   "u-test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m
}

// TestVaultSnapshotSealedRoundTrip drives the exact Push-side pipeline
// (tar -I zstd, then sealFile) and the exact BootPull-side pipeline
// (isVaultFile sniff, openSealedTarball via a keyfile on the restore volume,
// untar) across a "remote copy", asserting ciphertext on the wire and a
// byte-identical restored tree.
func TestVaultSnapshotSealedRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcData := t.TempDir()
	target := seedDataTree(t, srcData)
	user := "did:matrix:alice"

	m := mgrOver(t, srcData)
	m.SetVault(bootVaultAt(t, srcData, user), user)

	// Push half: archive + seal (what Push does before mc cp).
	tarPath := filepath.Join(t.TempDir(), "snap.tar.zst")
	if err := tarZst(ctx, srcData, tarPath); err != nil {
		t.Fatalf("tarZst: %v", err)
	}
	encPath := tarPath + ".enc"
	if err := m.sealFile(tarPath, encPath); err != nil {
		t.Fatalf("sealFile: %v", err)
	}
	sealed, err := isVaultFile(encPath)
	if err != nil || !sealed {
		t.Fatalf("sealed object not recognized: sealed=%v err=%v", sealed, err)
	}
	enc, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("read sealed: %v", err)
	}
	if bytes.Contains(enc, []byte("private history")) {
		t.Fatal("plaintext leaked into the sealed tarball")
	}

	// "mc cp" both ways is a byte copy; restore onto a FRESH volume that has
	// the keyfile sidecar in place (ensureKeyfile's outcome).
	restData := t.TempDir()
	kb, err := os.ReadFile(vault.KeyfilePath(srcData))
	if err != nil {
		t.Fatalf("read keyfile: %v", err)
	}
	if err := os.WriteFile(vault.KeyfilePath(restData), kb, 0o600); err != nil {
		t.Fatalf("mirror keyfile: %v", err)
	}
	t.Setenv(vault.EnvKEKHex, testKEKHex)
	t.Setenv(vault.EnvRequired, "true")

	m2 := mgrOver(t, restData)
	plainPath := encPath + ".plain"
	if err := m2.openSealedTarball(ctx, encPath, plainPath); err != nil {
		t.Fatalf("openSealedTarball on fresh volume: %v", err)
	}
	if err := untarZst(ctx, plainPath, restData); err != nil {
		t.Fatalf("untarZst: %v", err)
	}

	want, _ := os.ReadFile(target)
	got, err := os.ReadFile(filepath.Join(restData, "conversations", "conv-1.jsonl"))
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("restored tree differs: err=%v got=%q want=%q", err, got, want)
	}
}

// TestVaultSnapshotTruncationAndReorderFailClosed proves a truncated sealed
// object and a chunk-reordered sealed object both refuse to restore.
func TestVaultSnapshotTruncationAndReorderFailClosed(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seedDataTree(t, dataDir)
	user := "did:matrix:alice"
	m := mgrOver(t, dataDir)
	m.SetVault(bootVaultAt(t, dataDir, user), user)
	t.Setenv(vault.EnvKEKHex, testKEKHex)
	t.Setenv(vault.EnvRequired, "true")

	// Build a sealed tarball big enough for multiple chunks (>64 KiB).
	big := make([]byte, 300*1024)
	for i := range big {
		big[i] = byte(i % 251)
	}
	if err := os.WriteFile(filepath.Join(dataDir, "blob.bin"), big, 0o600); err != nil {
		t.Fatalf("seed blob: %v", err)
	}
	tarPath := filepath.Join(t.TempDir(), "snap.tar.zst")
	if err := tarZst(ctx, dataDir, tarPath); err != nil {
		t.Fatalf("tarZst: %v", err)
	}
	encPath := tarPath + ".enc"
	if err := m.sealFile(tarPath, encPath); err != nil {
		t.Fatalf("sealFile: %v", err)
	}
	enc, err := os.ReadFile(encPath)
	if err != nil {
		t.Fatalf("read sealed: %v", err)
	}

	// Truncation: drop the tail (kills the final-flagged chunk).
	trunc := filepath.Join(t.TempDir(), "trunc.enc")
	if err := os.WriteFile(trunc, enc[:len(enc)-37], 0o600); err != nil {
		t.Fatalf("write trunc: %v", err)
	}
	if err := m.openSealedTarball(ctx, trunc, trunc+".plain"); err == nil {
		t.Fatal("truncated sealed snapshot restored")
	}

	// Bit-flip mid-stream: any chunk tamper fails authentication.
	flip := append([]byte{}, enc...)
	flip[len(flip)/2] ^= 0x01
	flipPath := filepath.Join(t.TempDir(), "flip.enc")
	if err := os.WriteFile(flipPath, flip, 0o600); err != nil {
		t.Fatalf("write flip: %v", err)
	}
	if err := m.openSealedTarball(ctx, flipPath, flipPath+".plain"); err == nil {
		t.Fatal("tampered sealed snapshot restored")
	}
}

// TestVaultSnapshotWrongUserFailsClosed proves another user's keyfile cannot
// open the sealed tarball.
func TestVaultSnapshotWrongUserFailsClosed(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seedDataTree(t, dataDir)
	m := mgrOver(t, dataDir)
	m.SetVault(bootVaultAt(t, dataDir, "did:matrix:alice"), "did:matrix:alice")

	tarPath := filepath.Join(t.TempDir(), "snap.tar.zst")
	if err := tarZst(ctx, dataDir, tarPath); err != nil {
		t.Fatalf("tarZst: %v", err)
	}
	encPath := tarPath + ".enc"
	if err := m.sealFile(tarPath, encPath); err != nil {
		t.Fatalf("sealFile: %v", err)
	}

	// Restore volume holds MALLORY's keyfile (a different user's key).
	restData := t.TempDir()
	bootVaultAt(t, restData, "did:matrix:mallory")
	t.Setenv(vault.EnvKEKHex, testKEKHex)
	t.Setenv(vault.EnvRequired, "true")
	m2 := mgrOver(t, restData)
	if err := m2.openSealedTarball(ctx, encPath, encPath+".plain"); err == nil {
		t.Fatal("wrong user's keyfile opened the sealed snapshot")
	}
}

// TestVaultSnapshotLegacyPlaintextSniff proves the pull path recognizes a
// legacy plaintext tarball (no vault magic) so pre-vault snapshots restore.
func TestVaultSnapshotLegacyPlaintextSniff(t *testing.T) {
	ctx := context.Background()
	dataDir := t.TempDir()
	seedDataTree(t, dataDir)
	tarPath := filepath.Join(t.TempDir(), "snap.tar.zst")
	if err := tarZst(ctx, dataDir, tarPath); err != nil {
		t.Fatalf("tarZst: %v", err)
	}
	sealed, err := isVaultFile(tarPath)
	if err != nil {
		t.Fatalf("sniff: %v", err)
	}
	if sealed {
		t.Fatal("plaintext tarball sniffed as sealed")
	}
	restData := t.TempDir()
	if err := untarZst(ctx, tarPath, restData); err != nil {
		t.Fatalf("legacy untar: %v", err)
	}
	if _, err := os.Stat(filepath.Join(restData, "conversations", "conv-1.jsonl")); err != nil {
		t.Fatalf("legacy restore missing file: %v", err)
	}
}
