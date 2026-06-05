// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package keys

import (
	"bytes"
	"crypto/rand"
	"sort"
	"testing"
)

// randULID returns a non-zero random ULID. We do not need monotonicity here.
func randULID(t *testing.T) ULID {
	t.Helper()
	var u ULID
	if _, err := rand.Read(u[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	return u
}

func TestJournalKeyRoundTrip(t *testing.T) {
	seqs := []uint64{0, 1, 42, 1<<32 - 1, 1 << 63, ^uint64(0)}
	for _, s := range seqs {
		k := JournalKey(s)
		got, err := ParseJournalKey(k)
		if err != nil {
			t.Fatalf("ParseJournalKey(%x): %v", k, err)
		}
		if got != s {
			t.Fatalf("ParseJournalKey: want %d got %d", s, got)
		}
	}
}

// TestJournalKeyOrdering proves byte-sort == numeric-sort under Pebble's
// default comparator. This is the load-bearing property for monotonic
// journal scans.
func TestJournalKeyOrdering(t *testing.T) {
	seqs := []uint64{0, 1, 2, 10, 99, 100, 1000, 1 << 20, 1 << 40, ^uint64(0)}
	keys := make([][]byte, len(seqs))
	for i, s := range seqs {
		keys[i] = JournalKey(s)
	}
	// shuffle then sort byte-wise; expect ascending numeric order
	shuffled := make([][]byte, len(keys))
	copy(shuffled, keys)
	// reverse to disturb input
	for i, j := 0, len(shuffled)-1; i < j; i, j = i+1, j-1 {
		shuffled[i], shuffled[j] = shuffled[j], shuffled[i]
	}
	sort.Slice(shuffled, func(i, j int) bool { return bytes.Compare(shuffled[i], shuffled[j]) < 0 })
	for i := range keys {
		if !bytes.Equal(shuffled[i], keys[i]) {
			t.Fatalf("byte-sort != numeric-sort at i=%d: got seq=%v want seq=%v",
				i, mustParse(t, shuffled[i]), seqs[i])
		}
	}
}

func mustParse(t *testing.T, k []byte) uint64 {
	t.Helper()
	v, err := ParseJournalKey(k)
	if err != nil {
		t.Fatalf("parse %x: %v", k, err)
	}
	return v
}

func TestMemoryHeadKey(t *testing.T) {
	id := randULID(t)
	k := MemoryHeadKey(id)
	if !bytes.HasPrefix(k, PrefixMemoryHead) {
		t.Fatalf("missing prefix")
	}
	if len(k) != len(PrefixMemoryHead)+ULIDSize {
		t.Fatalf("len: got %d want %d", len(k), len(PrefixMemoryHead)+ULIDSize)
	}
	if !bytes.Equal(k[len(PrefixMemoryHead):], id[:]) {
		t.Fatalf("id mismatch")
	}
}

func TestMemoryVersionContiguous(t *testing.T) {
	id := randULID(t)
	versions := []uint64{1, 2, 3, 10, 1000}
	prev := MemoryVersionKey(id, 0)
	for _, v := range versions {
		k := MemoryVersionKey(id, v)
		if bytes.Compare(prev, k) >= 0 {
			t.Fatalf("version keys not ascending: prev>=k at v=%d", v)
		}
		// all share the same prefix (so prefix-scan picks up all versions of one id)
		if !bytes.HasPrefix(k, MemoryVersionPrefix(id)) {
			t.Fatalf("version key missing memory-version prefix")
		}
		prev = k
	}
}

func TestEdgeKeysFixedWidth(t *testing.T) {
	src := randULID(t)
	dst := randULID(t)
	edge := byte(0x07) // dispatched_to

	kf := EdgeFromKey(src, edge, dst)
	kt := EdgeToKey(dst, edge, src)

	wantFrom := len(PrefixEdgeFrom) + ULIDSize + 1 + ULIDSize
	wantTo := len(PrefixEdgeTo) + ULIDSize + 1 + ULIDSize
	if len(kf) != wantFrom {
		t.Fatalf("EdgeFromKey len: got %d want %d", len(kf), wantFrom)
	}
	if len(kt) != wantTo {
		t.Fatalf("EdgeToKey len: got %d want %d", len(kt), wantTo)
	}
	if !bytes.HasPrefix(kf, EdgeFromPrefix(src)) {
		t.Fatalf("EdgeFromKey missing EdgeFromPrefix")
	}
	if !bytes.HasPrefix(kt, EdgeToPrefix(dst)) {
		t.Fatalf("EdgeToKey missing EdgeToPrefix")
	}
	// Edge byte position is right after the ULID inside the prefix
	if kf[len(PrefixEdgeFrom)+ULIDSize] != edge {
		t.Fatalf("EdgeFromKey edge byte wrong")
	}
}

// TestEdgeKeyParseRoundTrip: Phase 6 ParseEdgeFromKey / ParseEdgeToKey
// recover (src, edge, dst) byte-identically.
func TestEdgeKeyParseRoundTrip(t *testing.T) {
	src := randULID(t)
	dst := randULID(t)
	const edge = byte(0x0B) // part_of

	kf := EdgeFromKey(src, edge, dst)
	gotSrc, gotEdge, gotDst, err := ParseEdgeFromKey(kf)
	if err != nil {
		t.Fatalf("ParseEdgeFromKey: %v", err)
	}
	if gotSrc != src || gotEdge != edge || gotDst != dst {
		t.Fatalf("from round-trip: %v %v %v", gotSrc, gotEdge, gotDst)
	}

	kt := EdgeToKey(dst, edge, src)
	gotDst2, gotEdge2, gotSrc2, err := ParseEdgeToKey(kt)
	if err != nil {
		t.Fatalf("ParseEdgeToKey: %v", err)
	}
	if gotDst2 != dst || gotEdge2 != edge || gotSrc2 != src {
		t.Fatalf("to round-trip: %v %v %v", gotDst2, gotEdge2, gotSrc2)
	}
}

// TestEdgeFromTypePrefix: per-type prefix is a strict prefix of the full
// edge key and a strict extension of the per-src prefix.
func TestEdgeFromTypePrefix(t *testing.T) {
	src := randULID(t)
	dst := randULID(t)
	const edge = byte(0x03) // references

	full := EdgeFromKey(src, edge, dst)
	srcOnly := EdgeFromPrefix(src)
	typed := EdgeFromTypePrefix(src, edge)

	if !bytes.HasPrefix(typed, srcOnly) {
		t.Fatalf("typed prefix not extension of src prefix")
	}
	if !bytes.HasPrefix(full, typed) {
		t.Fatalf("full key not extension of typed prefix")
	}
	if len(typed) != len(srcOnly)+1 {
		t.Fatalf("typed prefix wrong length: %d vs srcOnly+1=%d", len(typed), len(srcOnly)+1)
	}
}

func TestPutLPString(t *testing.T) {
	// happy path
	out, err := PutLPString(nil, "hello")
	if err != nil {
		t.Fatalf("PutLPString: %v", err)
	}
	if out[0] != 5 || string(out[1:]) != "hello" {
		t.Fatalf("PutLPString bad encoding: %v", out)
	}
	s, rest, err := ReadLPString(out)
	if err != nil {
		t.Fatalf("ReadLPString: %v", err)
	}
	if s != "hello" || len(rest) != 0 {
		t.Fatalf("ReadLPString round trip: %q rest=%v", s, rest)
	}
	// too long
	big := make([]byte, 256)
	for i := range big {
		big[i] = 'a'
	}
	if _, err := PutLPString(nil, string(big)); err == nil {
		t.Fatalf("PutLPString: expected ErrBadStrLen for 256-byte string")
	}
}

func TestValidateNoSeparator(t *testing.T) {
	if err := ValidateNoSeparator("ok"); err != nil {
		t.Fatalf("ok rejected: %v", err)
	}
	if err := ValidateNoSeparator("a/b"); err == nil {
		t.Fatalf("a/b accepted")
	}
}

func TestIdxTagKeyRoundTrip(t *testing.T) {
	id := randULID(t)
	var hash [TagHashSize]byte
	for i := range hash {
		hash[i] = byte(0xA0 | i)
	}
	created := uint64(1700000000_000000000)

	k := IdxTagKey(hash, created, id)
	wantLen := len(PrefixIdxTag) + TagHashSize + 8 + ULIDSize
	if len(k) != wantLen {
		t.Fatalf("IdxTagKey len: got %d want %d", len(k), wantLen)
	}
	if !bytes.HasPrefix(k, IdxTagPrefix(hash)) {
		t.Fatalf("IdxTagKey missing IdxTagPrefix")
	}

	gotCreated, gotID, err := ParseIdxTagKey(k)
	if err != nil {
		t.Fatalf("ParseIdxTagKey: %v", err)
	}
	if gotCreated != created {
		t.Fatalf("created: got %d want %d", gotCreated, created)
	}
	if gotID != id {
		t.Fatalf("id mismatch")
	}
}

// TestIdxTagKeyOrdersByTime proves that within one tag bucket, byte-sort
// equals time-ascending. This is the property the Find planner depends on
// for chronological scans.
func TestIdxTagKeyOrdersByTime(t *testing.T) {
	id := randULID(t)
	var hash [TagHashSize]byte
	for i := range hash {
		hash[i] = byte(i)
	}
	times := []uint64{1, 2, 1000, 1 << 32, 1 << 40, ^uint64(0)}
	prev := IdxTagKey(hash, 0, id)
	for _, ts := range times {
		k := IdxTagKey(hash, ts, id)
		if bytes.Compare(prev, k) >= 0 {
			t.Fatalf("idx/tag keys not ascending across time: t=%d", ts)
		}
		prev = k
	}
}

func TestParseIdxTypeKey(t *testing.T) {
	id := randULID(t)
	created := uint64(1234567890)
	k := IdxTypeKey(0x05, created, id) // TypeEvent
	gotT, gotC, gotID, err := ParseIdxTypeKey(k)
	if err != nil {
		t.Fatalf("ParseIdxTypeKey: %v", err)
	}
	if gotT != 0x05 || gotC != created || gotID != id {
		t.Fatalf("round trip mismatch: %v %v %v", gotT, gotC, gotID)
	}
}

// TestCheckpointKeyRoundTrip — Phase 9. chk/<lpstr>/<lpstr> survives
// encode→parse with byte-identical intent_id and step_id, and rejects
// '/' inside either component.
func TestCheckpointKeyRoundTrip(t *testing.T) {
	cases := []struct {
		intent, step string
	}{
		{"intent_01HABC", "step_3"},
		{"i", "s"},
		{"a-b_c.d", "x"},
		{"intent with spaces", "step:1"},
	}
	for _, c := range cases {
		k, err := CheckpointKey(c.intent, c.step)
		if err != nil {
			t.Fatalf("CheckpointKey(%q,%q): %v", c.intent, c.step, err)
		}
		if !bytes.HasPrefix(k, PrefixCheckpoint) {
			t.Fatalf("missing chk/ prefix for %q/%q", c.intent, c.step)
		}
		gotI, gotS, err := ParseCheckpointKey(k)
		if err != nil {
			t.Fatalf("ParseCheckpointKey: %v", err)
		}
		if gotI != c.intent || gotS != c.step {
			t.Fatalf("round trip: got (%q,%q) want (%q,%q)",
				gotI, gotS, c.intent, c.step)
		}
	}
}

// TestCheckpointKeyRejectsSlash — invariant from research/04 §2 "the
// path separator '/' (0x2F) is forbidden inside path components".
func TestCheckpointKeyRejectsSlash(t *testing.T) {
	if _, err := CheckpointKey("bad/intent", "step"); err == nil {
		t.Fatalf("expected error for slash in intent_id")
	}
	if _, err := CheckpointKey("intent", "bad/step"); err == nil {
		t.Fatalf("expected error for slash in step_id")
	}
}

// TestCheckpointIntentPrefix — distinct intents yield distinct prefixes;
// CheckpointKey(intent,step) starts with CheckpointIntentPrefix(intent).
func TestCheckpointIntentPrefix(t *testing.T) {
	intent := "intent_A"
	prefix, err := CheckpointIntentPrefix(intent)
	if err != nil {
		t.Fatalf("CheckpointIntentPrefix: %v", err)
	}
	for _, step := range []string{"s1", "s2", "step_with_underscores"} {
		k, err := CheckpointKey(intent, step)
		if err != nil {
			t.Fatalf("CheckpointKey: %v", err)
		}
		if !bytes.HasPrefix(k, prefix) {
			t.Fatalf("CheckpointKey(%q,%q) does not start with intent prefix",
				intent, step)
		}
	}
	other, _ := CheckpointIntentPrefix("intent_B")
	if bytes.Equal(prefix, other) {
		t.Fatalf("distinct intents produced identical prefixes")
	}
}

// TestParseCheckpointKeyBadPrefix — defensive parse error path.
func TestParseCheckpointKeyBadPrefix(t *testing.T) {
	_, _, err := ParseCheckpointKey([]byte("m/foo"))
	if err == nil {
		t.Fatalf("expected ErrBadPrefix")
	}
}

func TestPrefixRange(t *testing.T) {
	lo, hi := PrefixRange([]byte("j/"))
	if !bytes.Equal(lo, []byte("j/")) {
		t.Fatalf("lo: %q", lo)
	}
	// 'j' (0x6A) followed by byte '/' (0x2F) incremented -> '0' (0x30) -> "j0"
	if !bytes.Equal(hi, []byte("j0")) {
		t.Fatalf("hi: %q", hi)
	}
	// a key starting with j/ must lie within [lo, hi)
	k := JournalKey(123456789)
	if bytes.Compare(k, lo) < 0 || bytes.Compare(k, hi) >= 0 {
		t.Fatalf("journal key %x out of range [%q,%q)", k, lo, hi)
	}
	// all-0xff prefix yields nil upper
	_, hi2 := PrefixRange([]byte{0xff, 0xff})
	if hi2 != nil {
		t.Fatalf("PrefixRange all-0xff: hi should be nil, got %x", hi2)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
