// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package store

// vaultseam_test.go — ORACLE task 2.3 (store half): Pebble values in user-
// content namespaces are sealed at rest through the real vault while every
// read path returns the canonical plaintext. No fakes: a real vault.Session
// over a real KEK, a real Pebble DB, the real Get/PrefixIter/IterJournal
// paths.

import (
	"bytes"
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"

	"centra/core/cortex/journal"
	"centra/core/cortex/keys"
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

func openSealedStore(t *testing.T, user string) *Store {
	t.Helper()
	s, err := Open(t.TempDir(), "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetVault(vaultSessionFor(t, user), user)
	return s
}

// rawGet reads a value straight off Pebble, bypassing the decrypting seam.
func rawGet(t *testing.T, s *Store, key []byte) []byte {
	t.Helper()
	v, closer, err := s.DB().Get(key)
	if err != nil {
		t.Fatalf("raw get %q: %v", key, err)
	}
	defer closer.Close()
	out := make([]byte, len(v))
	copy(out, v)
	return out
}

// TestVaultJournalSealedOnDisk proves j/ values are sealed ciphertext on disk
// while AppendJournal/IterJournal round-trip plaintext, and the MMR leaf hash
// stays computed over the canonical plaintext encoding.
func TestVaultJournalSealedOnDisk(t *testing.T) {
	s := openSealedStore(t, "did:matrix:alice")

	var hookLeaf [32]byte
	s.SetJournalHook(func(b *pebble.Batch, seq uint64, leafHash [32]byte) error {
		hookLeaf = leafHash
		return nil
	})

	e := &journal.Entry{Kind: journal.KindRaw, Payload: []byte(`{"content":"the secret turn"}`)}
	seq, err := s.AppendJournal(e)
	if err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}

	raw := rawGet(t, s, keys.JournalKey(seq))
	if !vault.IsVault(raw) {
		t.Fatal("j/ value is not sealed on disk")
	}
	if strings.Contains(string(raw), "the secret turn") {
		t.Fatal("plaintext leaked into the sealed j/ value")
	}

	// The hook's leaf hash is over the plaintext canonical encoding — the
	// value the decrypting read path returns, NOT the ciphertext.
	var got *journal.Entry
	if err := s.IterJournal(func(je *journal.Entry) error {
		cp := *je
		got = &cp
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if got == nil || !bytes.Contains(got.Payload, []byte("the secret turn")) {
		t.Fatalf("journal round trip mismatch: %+v", got)
	}
	enc, err := got.Encode()
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	if journal.LeafHash(enc) != hookLeaf {
		t.Fatal("leaf hash is not over the canonical plaintext encoding")
	}
	if journal.LeafHash(raw) == hookLeaf {
		t.Fatal("leaf hash matches the ciphertext — hash boundary violated")
	}
}

// TestVaultWriteBatchSealsUserNamespaces proves WriteBatch.Set seals values in
// user-content namespaces, leaves out-of-scope namespaces plaintext, and Get
// decrypts.
func TestVaultWriteBatchSealsUserNamespaces(t *testing.T) {
	s := openSealedStore(t, "did:matrix:alice")

	var id keys.ULID
	copy(id[:], "0123456789abcdef")
	mKey := keys.MemoryHeadKey(id)
	salKey := keys.SalienceKey(id)

	wb := s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(mKey, []byte("private head bytes")); err != nil {
		t.Fatalf("Set m/: %v", err)
	}
	if err := wb.Set(salKey, []byte{1, 2, 3}); err != nil {
		t.Fatalf("Set salience/: %v", err)
	}
	if err := wb.AppendJournal(&journal.Entry{Kind: journal.KindRaw, Payload: []byte("{}")}); err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if err := wb.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	if raw := rawGet(t, s, mKey); !vault.IsVault(raw) {
		t.Fatal("m/ value not sealed")
	}
	if raw := rawGet(t, s, salKey); vault.IsVault(raw) {
		t.Fatal("salience/ value sealed — out-of-scope namespace must stay plaintext")
	}
	v, ok, err := s.Get(mKey)
	if err != nil || !ok || string(v) != "private head bytes" {
		t.Fatalf("Get m/ round trip: ok=%v err=%v v=%q", ok, err, v)
	}
}

// TestVaultKeyBinding proves the AD binds the full key: sealed ciphertext
// copied to a different key in a sealed namespace fails authentication.
func TestVaultKeyBinding(t *testing.T) {
	s := openSealedStore(t, "did:matrix:alice")

	var id, id2 keys.ULID
	copy(id[:], "0123456789abcdef")
	copy(id2[:], "fedcba9876543210")

	wb := s.BeginWrite()
	if err := wb.Set(keys.MemoryHeadKey(id), []byte("bound to id")); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if err := wb.AppendJournal(&journal.Entry{Kind: journal.KindRaw, Payload: []byte("{}")}); err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if err := wb.Commit(); err != nil {
		t.Fatalf("Commit: %v", err)
	}

	sealed := rawGet(t, s, keys.MemoryHeadKey(id))
	if err := s.DB().Set(keys.MemoryHeadKey(id2), sealed, pebble.Sync); err != nil {
		t.Fatalf("raw cross-copy: %v", err)
	}
	if _, _, err := s.Get(keys.MemoryHeadKey(id2)); err == nil {
		t.Fatal("cross-key moved ciphertext still opened")
	}
}

// TestVaultWrongUserFailsClosed proves another user's key cannot read sealed
// values — a hard error, never silent ciphertext.
func TestVaultWrongUserFailsClosed(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if _, err := s.AppendJournal(&journal.Entry{Kind: journal.KindRaw, Payload: []byte(`{"c":"private"}`)}); err != nil {
		t.Fatalf("AppendJournal: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.SetVault(vaultSessionFor(t, "did:matrix:mallory"), "did:matrix:mallory")
	if err := s2.IterJournal(func(*journal.Entry) error { return nil }); err == nil {
		t.Fatal("wrong user iterated a sealed journal")
	}
	if _, _, err := s2.Get(keys.JournalKey(0)); err == nil {
		t.Fatal("wrong user read a sealed j/ value")
	}
}

// TestVaultLegacyPlaintextReadable proves pre-vault plaintext values keep
// reading after the vault is wired (sniffing reader), and new writes seal.
func TestVaultLegacyPlaintextReadable(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	if _, err := s.AppendJournal(&journal.Entry{Kind: journal.KindRaw, Payload: []byte(`{"c":"old plain"}`)}); err != nil {
		t.Fatalf("seed legacy: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	s2, err := Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if _, err := s2.AppendJournal(&journal.Entry{Kind: journal.KindRaw, Payload: []byte(`{"c":"new sealed"}`)}); err != nil {
		t.Fatalf("sealed append: %v", err)
	}

	var payloads []string
	if err := s2.IterJournal(func(e *journal.Entry) error {
		payloads = append(payloads, string(e.Payload))
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	if len(payloads) != 2 || !strings.Contains(payloads[0], "old plain") || !strings.Contains(payloads[1], "new sealed") {
		t.Fatalf("mixed store read mismatch: %v", payloads)
	}
	if raw := rawGet(t, s2, keys.JournalKey(0)); vault.IsVault(raw) {
		t.Fatal("legacy value unexpectedly sealed")
	}
	if raw := rawGet(t, s2, keys.JournalKey(1)); !vault.IsVault(raw) {
		t.Fatal("new value not sealed")
	}
}
