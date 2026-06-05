// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 11 — replay package primitive tests.
//
// These tests exercise DropDerived + the rebuild functions in isolation
// using the snapshot.State + a hand-rolled minimum-viable journal driver
// (no cortex layer involvement). The cortex-level integration tests
// live in cortex/rebuild_test.go and exercise the full Cortex.Rebuild
// surface end-to-end.

package replay

import (
	"path/filepath"
	"testing"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/snapshot"
	"matrix/cortex/store"
)

func openStore(t *testing.T, name string) *store.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "replay-store-"+name)
	s, err := store.Open(dir, name, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// TestDropDerivedRemovesAll seeds every derived prefix with a
// stand-in key/value, runs DropDerived, and asserts CountDerived = 0.
func TestDropDerivedRemovesAll(t *testing.T) {
	s := openStore(t, "drop")

	// Seed each derived prefix with one key.
	mustSet := func(k, v []byte) {
		if err := s.DB().Set(k, v, nil); err != nil {
			t.Fatalf("seed %x: %v", k, err)
		}
	}
	mustSet(append(append([]byte{}, keys.PrefixVecMeta...), 0x01), []byte("vm"))
	mustSet(append(append([]byte{}, keys.PrefixIdxType...), 0x01, 0x02), []byte("type"))
	mustSet(append(append([]byte{}, keys.PrefixIdxTag...), 0x03), []byte("tag"))
	mustSet(append(append([]byte{}, keys.PrefixIdxFrame...), 0x04), []byte("frame"))
	mustSet(append(append([]byte{}, keys.PrefixIdxActorObj...), 0x05), []byte("actorobj"))
	mustSet(append(append([]byte{}, keys.PrefixIdxSMT...), 0x06), []byte("smt"))
	mustSet(append(append([]byte{}, keys.PrefixSalience...), 0x07), []byte("sal"))
	mustSet(append(append([]byte{}, keys.PrefixAccum...), 0x08), []byte("accum"))
	mustSet(metaEmbedCursor, []byte{0, 0, 0, 0, 0, 0, 0, 1})
	mustSet(metaEmbedVertexNext, []byte{0, 0, 0, 0, 0, 0, 0, 9})

	pre, err := CountDerived(s)
	if err != nil {
		t.Fatalf("CountDerived pre: %v", err)
	}
	if pre == 0 {
		t.Fatalf("seed didn't take")
	}

	if err := DropDerived(s); err != nil {
		t.Fatalf("DropDerived: %v", err)
	}
	post, err := CountDerived(s)
	if err != nil {
		t.Fatalf("CountDerived post: %v", err)
	}
	if post != 0 {
		t.Fatalf("post-drop derived count = %d, want 0", post)
	}
}

// TestDropDerivedKeepsCanonical seeds canonical prefixes, drops, and
// asserts those keys are unchanged.
func TestDropDerivedKeepsCanonical(t *testing.T) {
	s := openStore(t, "keep")

	canonicalSeeds := map[string][]byte{
		string(append(append([]byte{}, keys.PrefixMemoryHead...), 0xa1)):    []byte("head"),
		string(append(append([]byte{}, keys.PrefixMemoryVersion...), 0xa2)): []byte("ver"),
		string(append(append([]byte{}, keys.PrefixEdgeFrom...), 0xa3)):      []byte("edgefrom"),
		string(append(append([]byte{}, keys.PrefixEdgeTo...), 0xa4)):        []byte("edgeto"),
		string(append(append([]byte{}, keys.PrefixJournal...), 0xa5)):       []byte("journ"),
		string(append(append([]byte{}, keys.PrefixTombstone...), 0xa6)):     []byte("tomb"),
		string(append(append([]byte{}, keys.PrefixSnapshot...), 0xa7)):      []byte("snap"),
		string(append(append([]byte{}, keys.PrefixCheckpoint...), 0xa8)):    []byte("chk"),
		string(keys.MetaJournalHead):                                        []byte("head_meta"),
	}
	for k, v := range canonicalSeeds {
		if err := s.DB().Set([]byte(k), v, nil); err != nil {
			t.Fatalf("seed %x: %v", k, err)
		}
	}

	if err := DropDerived(s); err != nil {
		t.Fatalf("DropDerived: %v", err)
	}

	for k, want := range canonicalSeeds {
		got, ok, err := s.Get([]byte(k))
		if err != nil {
			t.Fatalf("get %x: %v", k, err)
		}
		if !ok {
			t.Fatalf("canonical key %x missing post-drop", k)
		}
		if string(got) != string(want) {
			t.Fatalf("canonical key %x changed: got %x want %x", k, got, want)
		}
	}
}

// TestDropDerivedIdempotent — running DropDerived twice is the same as
// running it once.
func TestDropDerivedIdempotent(t *testing.T) {
	s := openStore(t, "idem")
	if err := DropDerived(s); err != nil {
		t.Fatalf("first DropDerived: %v", err)
	}
	if err := DropDerived(s); err != nil {
		t.Fatalf("second DropDerived: %v", err)
	}
}

// TestRebuildEmptyStoreReproducesEmptyRoot — empty input produces
// empty MMR + empty SMTs + recognisable empty OverallRoot.
func TestRebuildEmptyStoreReproducesEmptyRoot(t *testing.T) {
	s := openStore(t, "empty")
	st := snapshot.New(s)
	s.SetJournalHook(st.MMRHook())

	preRoot, _, _ := mustCurrentRoots(t, st)

	res, err := Rebuild(s, st, Options{})
	if err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	if err := VerifyPreservesRoot(res); err != nil {
		t.Fatalf("VerifyPreservesRoot: %v", err)
	}
	if res.PostOverallRoot != preRoot {
		t.Fatalf("empty rebuild changed root: pre=%x post=%x", preRoot, res.PostOverallRoot)
	}
}

// TestRebuildNilStoreErrors — defensive validation.
func TestRebuildNilStoreErrors(t *testing.T) {
	if _, err := Rebuild(nil, nil, Options{}); err != ErrNilStore {
		t.Fatalf("nil store: got %v want ErrNilStore", err)
	}
	s := openStore(t, "nil-snap")
	if _, err := Rebuild(s, nil, Options{}); err != ErrNilSnapshot {
		t.Fatalf("nil snapshot: got %v want ErrNilSnapshot", err)
	}
}

// TestVerifyPreservesRootMismatch — surface the root drift in the error
// message.
func TestVerifyPreservesRootMismatch(t *testing.T) {
	r := &Result{
		PreOverallRoot:  [32]byte{1, 2, 3},
		PostOverallRoot: [32]byte{4, 5, 6},
	}
	err := VerifyPreservesRoot(r)
	if err == nil {
		t.Fatalf("expected error on root mismatch")
	}
}

// mustCurrentRoots is a t.Fatalf-on-error helper.
func mustCurrentRoots(t *testing.T, st *snapshot.State) (overall [32]byte, journalRoot [32]byte, stateRoots map[string][32]byte) {
	t.Helper()
	jr, sr, ovr, err := st.CurrentRoots()
	if err != nil {
		t.Fatalf("CurrentRoots: %v", err)
	}
	return ovr, jr, sr
}

// TestJournalLeafHashMatchesPackage — sanity: our leaf hash must match
// what the journal package emits, otherwise the rebuilt MMR diverges.
func TestJournalLeafHashMatchesPackage(t *testing.T) {
	enc := []byte{0x01, 0x02, 0x03}
	if journalLeafHash(enc) != journal.LeafHash(enc) {
		t.Fatalf("journalLeafHash != journal.LeafHash")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
