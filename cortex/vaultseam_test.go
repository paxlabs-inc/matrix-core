// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

// vaultseam_test.go — ORACLE task 2.3 (cortex half): the full cortex runs
// green over a sealed store, and the hash boundary holds — OverallRoot and
// D11 replay-rebuild are byte-identical over encrypted values, including
// across an in-place plaintext-to-encrypted migration. No fakes: real vault,
// real Pebble, the real Cortex mutation surface and replay machinery.

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/cockroachdb/pebble"

	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/store"
	"matrix/vault"
)

func encryptingSession(t *testing.T, user string) *vault.Session {
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

// openSealedCortex returns a Cortex over a store sealed for alice.
func openSealedCortex(t *testing.T) *Cortex {
	t.Helper()
	s, err := store.Open(t.TempDir(), "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	s.SetVault(encryptingSession(t, "did:matrix:alice"), "did:matrix:alice")
	return New(s)
}

// driveMutations exercises the main mutation surface: writes across types,
// an update, edges, and a tombstone — enough to populate j/, m/, mv/, the
// SMT-anchored namespaces, and derived indexes.
func driveMutations(t *testing.T, c *Cortex) memory.URI {
	t.Helper()
	uri1 := writePref(t, c, "tone", 5, "personal")
	uri2 := writePref(t, c, "verbosity", 3, "voice")
	if _, err := c.Update(uri1, memory.PreferenceData{
		SchemaVersion: 1, Topic: "tone",
		Polarity: memory.PolarityPrefer, StrengthVal: 0.95,
	}, WriteMeta{
		CreatedBy:  "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := c.AddEdge(idOf(uri1), memory.EdgeReferences, idOf(uri2), AddEdgeMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("AddEdge: %v", err)
	}
	tombURI := writePref(t, c, "tombed", 1)
	if err := c.Tombstone(tombURI, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	return uri1
}

// TestVaultCortexSealedLiveGreen proves the cortex mutation + read surface
// works over a sealed store and the values on disk are ciphertext.
func TestVaultCortexSealedLiveGreen(t *testing.T) {
	c := openSealedCortex(t)
	uri := driveMutations(t, c)

	// Read-back through the real resolve path returns plaintext content.
	m, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	decoded, err := memory.DecodeData(m.Version.Type, m.Version.Data)
	if err != nil {
		t.Fatalf("DecodeData: %v", err)
	}
	pref, ok := decoded.(memory.PreferenceData)
	if !ok || pref.Topic != "tone" {
		t.Fatalf("resolve mismatch: %+v", decoded)
	}

	// On disk: j/ and m/ values are sealed, no plaintext topic string.
	sealedCount := 0
	for _, prefix := range [][]byte{keys.PrefixJournal, keys.PrefixMemoryHead, keys.PrefixMemoryVersion} {
		lo, hi := keys.PrefixRange(prefix)
		it, err := c.s.DB().NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			t.Fatalf("raw iter: %v", err)
		}
		for it.First(); it.Valid(); it.Next() {
			v, verr := it.ValueAndErr()
			if verr != nil {
				t.Fatalf("raw value: %v", verr)
			}
			if !vault.IsVault(v) {
				t.Fatalf("value at %q not sealed on disk", it.Key())
			}
			if strings.Contains(string(v), "verbosity") {
				t.Fatalf("plaintext leaked at %q", it.Key())
			}
			sealedCount++
		}
		it.Close()
	}
	if sealedCount == 0 {
		t.Fatal("no sealed values found — seam not engaged")
	}
}

// TestVaultRebuildPreservesOverallRootSealed proves D11: dropping every
// derived namespace and replaying the SEALED journal reproduces the exact
// OverallRoot — all hashes are over the decrypted canonical plaintext.
func TestVaultRebuildPreservesOverallRootSealed(t *testing.T) {
	c := openSealedCortex(t)
	driveMutations(t, c)

	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}
	res, err := c.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild over sealed store: %v", err)
	}
	if res.PostOverallRoot != preRoot {
		t.Fatalf("OverallRoot drift across sealed replay: pre=%x post=%x", preRoot, res.PostOverallRoot)
	}
	postRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot post: %v", err)
	}
	if postRoot != preRoot {
		t.Fatalf("live OverallRoot drift: pre=%x post=%x", preRoot, postRoot)
	}
}

// TestVaultMigrationPreservesOverallRoot proves the plaintext-to-encrypted
// boundary property end-to-end: values written plaintext, then sealed IN
// PLACE (what the per-user migrator does), replay-rebuild to the byte-
// identical OverallRoot, and reads keep returning the same content.
func TestVaultMigrationPreservesOverallRoot(t *testing.T) {
	dir := t.TempDir()
	s, err := store.Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	c := New(s)
	uri := driveMutations(t, c)
	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot plaintext: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// Reopen sealed and encrypt every user-content value in place.
	s2, err := store.Open(dir, "andrew", nil)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.SetVault(encryptingSession(t, "did:matrix:alice"), "did:matrix:alice")
	for _, prefix := range [][]byte{
		keys.PrefixJournal, keys.PrefixMemoryHead, keys.PrefixMemoryVersion,
		keys.PrefixSession, keys.PrefixSessionBlob, keys.PrefixRollup,
		keys.PrefixEnrich, keys.PrefixStory, keys.PrefixCheckpoint, keys.PrefixVecMeta,
	} {
		type kv struct{ k, v []byte }
		var rows []kv
		lo, hi := keys.PrefixRange(prefix)
		it, err := s2.DB().NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			t.Fatalf("raw iter: %v", err)
		}
		for it.First(); it.Valid(); it.Next() {
			v, verr := it.ValueAndErr()
			if verr != nil {
				t.Fatalf("raw value: %v", verr)
			}
			rows = append(rows, kv{
				k: append([]byte{}, it.Key()...),
				v: append([]byte{}, v...),
			})
		}
		it.Close()
		for _, r := range rows {
			sealed, serr := s2.SealValue(r.k, r.v)
			if serr != nil {
				t.Fatalf("SealValue %q: %v", r.k, serr)
			}
			if err := s2.DB().Set(r.k, sealed, pebble.Sync); err != nil {
				t.Fatalf("write sealed %q: %v", r.k, err)
			}
		}
	}

	// Spot-check: migrated values are ciphertext now.
	if v, _, _ := func() ([]byte, interface{}, error) {
		v, closer, err := s2.DB().Get(keys.JournalKey(0))
		if err != nil {
			t.Fatalf("raw j/0: %v", err)
		}
		defer closer.Close()
		return append([]byte{}, v...), nil, nil
	}(); !vault.IsVault(v) {
		t.Fatal("j/0 not sealed after in-place migration")
	}

	// Full replay-rebuild over the sealed store must land on the exact root
	// the plaintext store had.
	c2 := New(s2)
	res, err := c2.Rebuild(RebuildOptions{})
	if err != nil {
		t.Fatalf("Rebuild after migration: %v", err)
	}
	if res.PostOverallRoot != preRoot {
		t.Fatalf("OverallRoot drift across migration: plaintext=%x sealed=%x", preRoot, res.PostOverallRoot)
	}

	// The same memory resolves with the same content.
	m, err := c2.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve after migration: %v", err)
	}
	decoded, err := memory.DecodeData(m.Version.Type, m.Version.Data)
	if err != nil {
		t.Fatalf("DecodeData after migration: %v", err)
	}
	pref, ok := decoded.(memory.PreferenceData)
	if !ok || pref.Topic != "tone" {
		t.Fatalf("post-migration resolve mismatch: %+v", decoded)
	}
}

// TestVaultSessionActivateStorySealed proves the continuous-memory lanes —
// session transcript (sess/), story-so-far (story/), and Activate — run green
// over a sealed store, with every sess/ and story/ value ciphertext on disk.
func TestVaultSessionActivateStorySealed(t *testing.T) {
	c := openSealedCortex(t)
	const conv = "conv-sealed"

	msgs := []Message{
		{ConversationID: conv, Role: RoleUser, Content: "my private question"},
		{ConversationID: conv, Role: RoleAssistant, Content: "a private answer"},
		{ConversationID: conv, Role: RoleUser, Content: "and a follow-up"},
	}
	for i, m := range msgs {
		if _, err := c.AppendMessage(m); err != nil {
			t.Fatalf("AppendMessage[%d]: %v", i, err)
		}
	}

	got, err := c.Transcript(conv, 0, 100)
	if err != nil {
		t.Fatalf("Transcript over sealed store: %v", err)
	}
	if len(got) != len(msgs) || got[1].Content != "a private answer" {
		t.Fatalf("transcript mismatch: %+v", got)
	}

	if _, err := c.BuildStorySoFar(conv); err != nil {
		t.Fatalf("BuildStorySoFar over sealed store: %v", err)
	}
	bundle, err := c.Activate(conv, "", Budget{})
	if err != nil {
		t.Fatalf("Activate over sealed store: %v", err)
	}
	if bundle == nil {
		t.Fatal("nil activation bundle")
	}

	// sess/ and story/ values are ciphertext on disk, no message content.
	for _, prefix := range [][]byte{keys.PrefixSession, keys.PrefixStory} {
		lo, hi := keys.PrefixRange(prefix)
		it, err := c.s.DB().NewIter(&pebble.IterOptions{LowerBound: lo, UpperBound: hi})
		if err != nil {
			t.Fatalf("raw iter: %v", err)
		}
		n := 0
		for it.First(); it.Valid(); it.Next() {
			v, verr := it.ValueAndErr()
			if verr != nil {
				t.Fatalf("raw value: %v", verr)
			}
			if !vault.IsVault(v) {
				t.Fatalf("value at %q not sealed", it.Key())
			}
			if strings.Contains(string(v), "private answer") {
				t.Fatalf("plaintext leaked at %q", it.Key())
			}
			n++
		}
		it.Close()
		if n == 0 {
			t.Fatalf("no values under %q — lane not exercised", prefix)
		}
	}
}
