// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package scope

import (
	"crypto/ed25519"
	"crypto/rand"
	"path/filepath"
	"testing"
	"time"

	"matrix/cortex/memory"
	"matrix/cortex/snapshot"
	"matrix/cortex/store"
)

// --- helpers --------------------------------------------------------------

func mustKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

func openStore(t *testing.T, name string) *store.Store {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "scope-store-"+name)
	s, err := store.Open(dir, name, nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func newID(b byte) memory.ID {
	var id memory.ID
	for i := range id {
		id[i] = b
	}
	return id
}

// signedScope builds a Scope with default fields, signs it under priv,
// and returns it. Caller is expected to overwrite the SnapshotHash and
// Include before signing in real tests.
func signedScope(t *testing.T, priv ed25519.PrivateKey, mod func(*Scope)) *Scope {
	t.Helper()
	s := &Scope{
		SchemaVersion: SchemaVersion,
		Actor:         "alice",
		SnapshotHash:  [32]byte{1, 2, 3},
		Include: Selector{
			Types: []memory.Type{memory.TypeFact},
		},
		ExpiresAt: time.Now().Add(time.Hour),
		GrantedBy: "did:pax:0xparent",
		GrantedTo: "did:pax:0xchild",
	}
	if mod != nil {
		mod(s)
	}
	if err := Sign(s, priv); err != nil {
		t.Fatalf("Sign: %v", err)
	}
	return s
}

// --- selector / matcher ---------------------------------------------------

func TestSelectorEmpty(t *testing.T) {
	var sel Selector
	if !sel.IsEmpty() {
		t.Errorf("zero-value Selector reported non-empty")
	}
	h := &memory.Head{Type: memory.TypeFact}
	if sel.Matches(h) {
		t.Errorf("empty selector matched a Head")
	}
}

func TestSelectorTypeMatch(t *testing.T) {
	sel := Selector{Types: []memory.Type{memory.TypeFact, memory.TypePattern}}
	if !sel.Matches(&memory.Head{Type: memory.TypeFact}) {
		t.Errorf("Type=Fact should match")
	}
	if sel.Matches(&memory.Head{Type: memory.TypeBelief}) {
		t.Errorf("Type=Belief should not match")
	}
}

func TestSelectorTagMatch(t *testing.T) {
	sel := Selector{Tags: []memory.Tag{"code", "ops"}}
	if !sel.Matches(&memory.Head{Tags: []memory.Tag{"code"}}) {
		t.Errorf("Tag=code should match")
	}
	if sel.Matches(&memory.Head{Tags: []memory.Tag{"private"}}) {
		t.Errorf("Tag=private should not match")
	}
}

func TestSelectorIDMatch(t *testing.T) {
	id := newID(0x42)
	sel := Selector{IDs: []memory.ID{id}}
	if !sel.Matches(&memory.Head{ID: id}) {
		t.Errorf("ID-match should hit")
	}
	if sel.Matches(&memory.Head{ID: newID(0xFF)}) {
		t.Errorf("ID-mismatch should miss")
	}
}

func TestSelectorFrameMatch(t *testing.T) {
	objHash := memory.ObjHash("matrix://service/example@1")
	sel := Selector{
		Frame: &FrameFilter{
			Verb:      memory.VerbFind,
			ObjHashes: [][memory.ObjHashSize]byte{objHash},
		},
	}
	h := &memory.Head{
		Frames: []memory.FrameRef{
			{Verb: memory.VerbFind, ObjKind: memory.KindService, ObjRef: "matrix://service/example@1"},
		},
	}
	if !sel.Matches(h) {
		t.Errorf("frame match should hit")
	}
	// Verb mismatch.
	h2 := &memory.Head{
		Frames: []memory.FrameRef{
			{Verb: memory.VerbAcquire, ObjKind: memory.KindService, ObjRef: "matrix://service/example@1"},
		},
	}
	if sel.Matches(h2) {
		t.Errorf("verb mismatch should miss")
	}
}

func TestScopeAllowsIncludeMinusExclude(t *testing.T) {
	s := &Scope{
		Include: Selector{Types: []memory.Type{memory.TypeFact}},
		Exclude: Selector{Tags: []memory.Tag{"private"}},
	}
	// Fact + no private tag → allowed.
	if !s.Allows(&memory.Head{Type: memory.TypeFact}) {
		t.Errorf("fact should be allowed")
	}
	// Fact + private tag → denied.
	if s.Allows(&memory.Head{Type: memory.TypeFact, Tags: []memory.Tag{"private"}}) {
		t.Errorf("fact with private tag should be denied")
	}
	// Belief → denied (not in include).
	if s.Allows(&memory.Head{Type: memory.TypeBelief}) {
		t.Errorf("belief should be denied (not in include)")
	}
}

// --- canonical CBOR encode / decode ---------------------------------------

func TestEncodeDecodeRoundTrip(t *testing.T) {
	_, priv := mustKeypair(t)
	s := signedScope(t, priv, nil)

	enc, err := EncodeScope(s)
	if err != nil {
		t.Fatalf("EncodeScope: %v", err)
	}
	var got Scope
	if err := DecodeScope(enc, &got); err != nil {
		t.Fatalf("DecodeScope: %v", err)
	}
	if got.SchemaVersion != s.SchemaVersion {
		t.Errorf("SchemaVersion mismatch")
	}
	if got.Actor != s.Actor {
		t.Errorf("Actor mismatch")
	}
	if got.GrantedBy != s.GrantedBy {
		t.Errorf("GrantedBy mismatch")
	}
	if string(got.Signature) != string(s.Signature) {
		t.Errorf("Signature mismatch")
	}
}

func TestUnsignedBytesIgnoresSignatureField(t *testing.T) {
	_, priv := mustKeypair(t)
	s := signedScope(t, priv, nil)

	a, err := UnsignedBytes(s)
	if err != nil {
		t.Fatalf("UnsignedBytes(a): %v", err)
	}
	// Mutate signature; unsigned bytes should not change.
	s.Signature = []byte("totally different signature bytes here") // 38B, deliberately not 64
	b, err := UnsignedBytes(s)
	if err != nil {
		t.Fatalf("UnsignedBytes(b): %v", err)
	}
	if string(a) != string(b) {
		t.Errorf("UnsignedBytes changed when only Signature mutated")
	}
}

// --- signatures -----------------------------------------------------------

func TestSignAndVerifyRoundTrip(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := signedScope(t, priv, nil)
	if err := VerifySignature(s, pub); err != nil {
		t.Errorf("VerifySignature: %v", err)
	}
}

func TestVerifySignatureRejectsTamperedField(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := signedScope(t, priv, nil)
	s.Actor = "mallory"
	if err := VerifySignature(s, pub); err == nil {
		t.Errorf("VerifySignature accepted tampered Actor field")
	}
}

func TestVerifySignatureRejectsWrongPubkey(t *testing.T) {
	_, priv := mustKeypair(t)
	other, _ := mustKeypair(t)
	s := signedScope(t, priv, nil)
	if err := VerifySignature(s, other); err == nil {
		t.Errorf("VerifySignature accepted wrong pubkey")
	}
}

// --- full Verify chain ----------------------------------------------------

func TestVerifyHappyPath(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := openStore(t, "verify-happy")
	st := snapshot.New(s)

	now := time.Unix(1700000000, 0).UTC()
	manifest, err := st.Snapshot("alice", snapshot.TriggerExplicit, now)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}

	scope := signedScope(t, priv, func(sc *Scope) {
		sc.Actor = "alice"
		sc.SnapshotHash = manifest.OverallRoot
		sc.Include = Selector{Types: []memory.Type{memory.TypeFact}}
	})
	resolver := StaticKeyResolver{"did:pax:0xparent": pub}

	if err := Verify(scope, st, resolver, VerifyOpts{Now: now}); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

func TestVerifyRejectsExpiredScope(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := openStore(t, "verify-expired")
	st := snapshot.New(s)
	now := time.Unix(1700000000, 0).UTC()
	m, _ := st.Snapshot("alice", snapshot.TriggerExplicit, now)

	scope := signedScope(t, priv, func(sc *Scope) {
		sc.Actor = "alice"
		sc.SnapshotHash = m.OverallRoot
		sc.ExpiresAt = now.Add(-time.Hour) // already expired
	})
	resolver := StaticKeyResolver{"did:pax:0xparent": pub}

	err := Verify(scope, st, resolver, VerifyOpts{Now: now})
	if err == nil {
		t.Errorf("Verify accepted expired scope")
	}
}

func TestVerifyRejectsUnresolvableSnapshot(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := openStore(t, "verify-unresolved")
	st := snapshot.New(s)
	now := time.Unix(1700000000, 0).UTC()
	_, _ = st.Snapshot("alice", snapshot.TriggerExplicit, now)

	scope := signedScope(t, priv, func(sc *Scope) {
		sc.Actor = "alice"
		// Random snapshot hash that doesn't exist in any snap/<seq>.
		for i := range sc.SnapshotHash {
			sc.SnapshotHash[i] = byte(i + 1)
		}
	})
	resolver := StaticKeyResolver{"did:pax:0xparent": pub}

	err := Verify(scope, st, resolver, VerifyOpts{Now: now})
	if err == nil {
		t.Errorf("Verify accepted unresolvable snapshot hash")
	}
}

func TestVerifyRejectsEmptyInclude(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := openStore(t, "verify-empty-include")
	st := snapshot.New(s)
	now := time.Unix(1700000000, 0).UTC()
	m, _ := st.Snapshot("alice", snapshot.TriggerExplicit, now)

	scope := signedScope(t, priv, func(sc *Scope) {
		sc.Actor = "alice"
		sc.SnapshotHash = m.OverallRoot
		sc.Include = Selector{} // empty
	})
	resolver := StaticKeyResolver{"did:pax:0xparent": pub}

	err := Verify(scope, st, resolver, VerifyOpts{Now: now})
	if err == nil {
		t.Errorf("Verify accepted empty include")
	}
}

func TestVerifyRejectsUnknownAgent(t *testing.T) {
	_, priv := mustKeypair(t)
	s := openStore(t, "verify-unknown-agent")
	st := snapshot.New(s)
	now := time.Unix(1700000000, 0).UTC()
	m, _ := st.Snapshot("alice", snapshot.TriggerExplicit, now)
	scope := signedScope(t, priv, func(sc *Scope) {
		sc.Actor = "alice"
		sc.SnapshotHash = m.OverallRoot
	})
	resolver := StaticKeyResolver{} // empty

	err := Verify(scope, st, resolver, VerifyOpts{Now: now})
	if err == nil {
		t.Errorf("Verify accepted unknown granter")
	}
}

func TestVerifyRejectsBadSchemaVersion(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := openStore(t, "verify-schema")
	st := snapshot.New(s)
	now := time.Unix(1700000000, 0).UTC()
	m, _ := st.Snapshot("alice", snapshot.TriggerExplicit, now)

	scope := signedScope(t, priv, func(sc *Scope) {
		sc.Actor = "alice"
		sc.SnapshotHash = m.OverallRoot
		sc.SchemaVersion = 99
	})
	resolver := StaticKeyResolver{"did:pax:0xparent": pub}

	err := Verify(scope, st, resolver, VerifyOpts{Now: now})
	if err == nil {
		t.Errorf("Verify accepted unknown schema")
	}
}

func TestVerifyHonoursSkipSnapshotResolution(t *testing.T) {
	pub, priv := mustKeypair(t)
	s := openStore(t, "verify-skip-snap")
	st := snapshot.New(s)
	now := time.Unix(1700000000, 0).UTC()
	scope := signedScope(t, priv, func(sc *Scope) {
		sc.Actor = "alice"
		// Bogus root, but Skip is on.
		sc.SnapshotHash = [32]byte{0xFF}
	})
	resolver := StaticKeyResolver{"did:pax:0xparent": pub}

	err := Verify(scope, st, resolver, VerifyOpts{Now: now, SkipSnapshotResolution: true})
	if err != nil {
		t.Errorf("Verify with SkipSnapshotResolution: %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
