// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package compilecache

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/cortex/keys"
	"matrix/cortex/store"
)

// openTestStore creates a real per-actor Pebble store in a tempdir.
// Cleanup happens via t.Cleanup so a panic in the test still releases
// the disk resources.
func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	root := filepath.Join(t.TempDir(), "matrix-test")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatalf("mkdir root: %v", err)
	}
	s, err := store.Open(root, "test-actor", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// ---------------------------------------------------------------------
// Key derivation
// ---------------------------------------------------------------------

func TestKey_IsDeterministic(t *testing.T) {
	k1 := Key("sk", "p", "snap", "build", "model")
	k2 := Key("sk", "p", "snap", "build", "model")
	if k1 != k2 {
		t.Fatalf("Key non-deterministic: %s vs %s", k1, k2)
	}
	if len(k1) != 64 {
		t.Fatalf("Key length = %d, want 64", len(k1))
	}
	for i, c := range k1 {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("Key[%d]=%q is not lowercase hex", i, c)
		}
	}
}

func TestKey_VariesByComponent(t *testing.T) {
	base := Key("sk", "p", "snap", "build", "model")
	cases := []struct {
		name string
		k    string
	}{
		{"skill", Key("sk2", "p", "snap", "build", "model")},
		{"prose", Key("sk", "p2", "snap", "build", "model")},
		{"snap", Key("sk", "p", "snap2", "build", "model")},
		{"verb", Key("sk", "p", "snap", "build2", "model")},
		{"model", Key("sk", "p", "snap", "build", "model2")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.k == base {
				t.Fatalf("Key collision: %s component change did not alter hash", c.name)
			}
		})
	}
}

func TestKey_SeparatorPreventsAmbiguity(t *testing.T) {
	// "abc"+""+""+""+"" must hash differently from ""+"abc"+""+""+""
	// thanks to the US separator anchoring component positions.
	a := Key("abc", "", "", "", "")
	b := Key("", "abc", "", "", "")
	if a == b {
		t.Fatalf("US separator ineffective: positional collision %s == %s", a, b)
	}
}

// ---------------------------------------------------------------------
// Encode / Decode roundtrip
// ---------------------------------------------------------------------

func TestEncodeDecode_Roundtrip(t *testing.T) {
	orig := &Entry{
		SchemaVersion: SchemaVersion,
		IntentJSON:    []byte(`{"id":"01ABC","verb":"build"}`),
		IntentHash:    "deadbeef",
		ModelDigest:   "cafef00d",
		CachedAt:      1700000000000000000,
		Verb:          "build",
		SkillDigest:   "abcd1234",
		SnapHash:      strings.Repeat("0", 64),
	}
	enc, err := Encode(orig)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	var got Entry
	if err := Decode(enc, &got); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.SchemaVersion != orig.SchemaVersion {
		t.Errorf("SchemaVersion mismatch: %d != %d", got.SchemaVersion, orig.SchemaVersion)
	}
	if !bytes.Equal(got.IntentJSON, orig.IntentJSON) {
		t.Errorf("IntentJSON mismatch: %q != %q", got.IntentJSON, orig.IntentJSON)
	}
	if got.IntentHash != orig.IntentHash {
		t.Errorf("IntentHash mismatch")
	}
	if got.ModelDigest != orig.ModelDigest {
		t.Errorf("ModelDigest mismatch")
	}
	if got.CachedAt != orig.CachedAt {
		t.Errorf("CachedAt mismatch")
	}
}

func TestEncodeDecode_DeterministicBytes(t *testing.T) {
	e := &Entry{
		SchemaVersion: SchemaVersion,
		IntentJSON:    []byte(`{"a":1}`),
		IntentHash:    "abc",
		ModelDigest:   "def",
		CachedAt:      42,
	}
	b1, err := Encode(e)
	if err != nil {
		t.Fatalf("Encode 1: %v", err)
	}
	b2, err := Encode(e)
	if err != nil {
		t.Fatalf("Encode 2: %v", err)
	}
	if !bytes.Equal(b1, b2) {
		t.Fatalf("Encode non-deterministic: %x != %x", b1, b2)
	}
}

func TestEncode_NilRejected(t *testing.T) {
	if _, err := Encode(nil); err == nil {
		t.Fatal("Encode(nil) should error")
	}
}

// ---------------------------------------------------------------------
// Lookup / Store roundtrip against a real Pebble store
// ---------------------------------------------------------------------

func TestStoreLookup_HitMiss(t *testing.T) {
	s := openTestStore(t)
	hex := Key("sk", "prose", "snap", "build", "model")

	// Cold cache: miss.
	got, ok, err := Lookup(s, hex)
	if err != nil {
		t.Fatalf("Lookup cold: %v", err)
	}
	if ok || got != nil {
		t.Fatalf("Lookup cold: expected miss, got ok=%v entry=%v", ok, got)
	}

	// Warm cache.
	want := &Entry{
		SchemaVersion: SchemaVersion,
		IntentJSON:    []byte(`{"hello":"world"}`),
		IntentHash:    "hashvalue",
		ModelDigest:   "modeldigest",
		Verb:          "build",
	}
	if err := Store(s, hex, want); err != nil {
		t.Fatalf("Store: %v", err)
	}

	// Hit.
	got, ok, err = Lookup(s, hex)
	if err != nil {
		t.Fatalf("Lookup warm: %v", err)
	}
	if !ok || got == nil {
		t.Fatalf("Lookup warm: expected hit, got ok=%v entry=%v", ok, got)
	}
	if !bytes.Equal(got.IntentJSON, want.IntentJSON) {
		t.Errorf("IntentJSON: %q != %q", got.IntentJSON, want.IntentJSON)
	}
	if got.IntentHash != want.IntentHash {
		t.Errorf("IntentHash: %q != %q", got.IntentHash, want.IntentHash)
	}
	if got.CachedAt == 0 {
		t.Errorf("CachedAt should be auto-populated when zero in")
	}
}

func TestStore_RejectsBadSchemaVersion(t *testing.T) {
	s := openTestStore(t)
	hex := Key("sk", "p", "snap", "v", "m")
	bad := &Entry{
		SchemaVersion: 99,
		IntentJSON:    []byte(`{}`),
		IntentHash:    "h",
		ModelDigest:   "m",
		CachedAt:      time.Now().UnixNano(),
	}
	if err := Store(s, hex, bad); err == nil {
		t.Fatal("Store should reject SchemaVersion=99")
	}
}

func TestStore_NilArgsRejected(t *testing.T) {
	s := openTestStore(t)
	if err := Store(s, "key", nil); err == nil {
		t.Fatal("Store(nil entry) should error")
	}
	if err := Store(s, "", &Entry{SchemaVersion: SchemaVersion}); err == nil {
		t.Fatal("Store(empty key) should error")
	}
	if err := Store(nil, "key", &Entry{SchemaVersion: SchemaVersion}); err == nil {
		t.Fatal("Store(nil store) should error")
	}
}

func TestLookup_NilArgsRejected(t *testing.T) {
	if _, _, err := Lookup(nil, "key"); err == nil {
		t.Fatal("Lookup(nil store) should error")
	}
	s := openTestStore(t)
	if _, _, err := Lookup(s, ""); err == nil {
		t.Fatal("Lookup(empty key) should error")
	}
}

func TestStoreLookup_OverwriteIsClean(t *testing.T) {
	s := openTestStore(t)
	hex := Key("sk", "p", "snap", "v", "m")

	first := &Entry{
		SchemaVersion: SchemaVersion,
		IntentJSON:    []byte(`{"v":1}`),
		IntentHash:    "h1",
		ModelDigest:   "m",
	}
	if err := Store(s, hex, first); err != nil {
		t.Fatalf("Store 1: %v", err)
	}

	second := &Entry{
		SchemaVersion: SchemaVersion,
		IntentJSON:    []byte(`{"v":2}`),
		IntentHash:    "h2",
		ModelDigest:   "m",
	}
	if err := Store(s, hex, second); err != nil {
		t.Fatalf("Store 2: %v", err)
	}

	got, ok, err := Lookup(s, hex)
	if err != nil || !ok {
		t.Fatalf("Lookup after overwrite: ok=%v err=%v", ok, err)
	}
	if !bytes.Equal(got.IntentJSON, second.IntentJSON) {
		t.Errorf("overwrite did not replace: got %q want %q", got.IntentJSON, second.IntentJSON)
	}
}

func TestLookup_OldSchemaVersionTreatedAsCold(t *testing.T) {
	s := openTestStore(t)
	hex := Key("sk", "p", "snap", "v", "m")
	// Hand-encode an Entry with a stale SchemaVersion to simulate a
	// pre-upgrade cache blob sitting in the store.
	stale := &Entry{
		SchemaVersion: 99, // intentionally invalid; Encode succeeds
		IntentJSON:    []byte(`{"stale":true}`),
		IntentHash:    "stale",
		ModelDigest:   "m",
	}
	enc, err := Encode(stale)
	if err != nil {
		t.Fatalf("Encode: %v", err)
	}
	// Bypass Store's SchemaVersion validation by writing directly.
	if err := s.SetMeta(keys.MetaCompileCacheKey(hex), enc); err != nil {
		t.Fatalf("SetMeta direct: %v", err)
	}
	got, ok, err := Lookup(s, hex)
	if err != nil {
		t.Fatalf("Lookup: %v", err)
	}
	if ok || got != nil {
		t.Fatalf("stale-schema entry should be cold-treated; got ok=%v", ok)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
