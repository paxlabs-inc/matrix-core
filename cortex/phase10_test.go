// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 10 integration tests:
//   10a CortexScope (sub-agent Merkle-proof scoping) — Find/Context/
//       ResolveScoped + multi-proof + violation journaling
//   10b UpdateHead (mutate Tags/Frames/Importance/Visibility without
//       bumping Data version) + idx/* hard-delete diff + replay
//
// Spec citations are inline per test. Conventions:
//   - Each test stands alone via openCortex (fresh Pebble DB).
//   - Scope-builder / signing helpers live at the bottom; tests up
//     top read declaratively.
//   - For determinism, tests pin the ed25519 keypair via deterministic
//     RNG (the test seed). Cross-actor determinism tests use ed25519
//     keys generated once and reused across actors.

package cortex

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"

	"matrix/cortex/journal"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/scope"
	"matrix/cortex/snapshot"
)

// --- Phase 10a helpers ----------------------------------------------------

const testParentRef = "did:pax:0xparent-test"

// scopeKeypair returns a fresh ed25519 keypair for tests. Real RNG —
// the StaticKeyResolver maps testParentRef → pub for verification.
func scopeKeypair(t *testing.T) (ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	return pub, priv
}

// withResolver returns a Cortex with the test keyResolver wired so
// scoped calls verify successfully. Reuses the openCortex base.
func withResolver(t *testing.T, c *Cortex, pub ed25519.PublicKey) {
	t.Helper()
	c.keyResolver = scope.StaticKeyResolver{testParentRef: pub}
}

// freshSnapshot takes a Snapshot and returns its OverallRoot — used as
// the pinned scope.SnapshotHash.
func freshSnapshot(t *testing.T, c *Cortex) [32]byte {
	t.Helper()
	m, err := c.Snapshot(snapshot.TriggerExplicit)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	return m.OverallRoot
}

// signScope is a thin wrapper around scope.Sign that fails the test
// on error.
func signScope(t *testing.T, s *scope.Scope, priv ed25519.PrivateKey) {
	t.Helper()
	if err := scope.Sign(s, priv); err != nil {
		t.Fatalf("scope.Sign: %v", err)
	}
}

// buildTestScope builds and signs a generic test scope. mod lets each
// test override fields before signing.
func buildTestScope(t *testing.T, c *Cortex, priv ed25519.PrivateKey, mod func(*scope.Scope)) *scope.Scope {
	t.Helper()
	root := freshSnapshot(t, c)
	s := &scope.Scope{
		SchemaVersion: scope.SchemaVersion,
		Actor:         c.s.Actor(),
		SnapshotHash:  root,
		Include: scope.Selector{
			Types: []memory.Type{memory.TypePreference},
		},
		ExpiresAt: time.Now().Add(time.Hour),
		GrantedBy: testParentRef,
		GrantedTo: "did:pax:0xchild-test",
	}
	if mod != nil {
		mod(s)
	}
	signScope(t, s, priv)
	return s
}

// countKindEntries scans the journal forward and counts entries whose
// Kind matches kind.
func countKindEntries(t *testing.T, c *Cortex, kind journal.Kind) int {
	t.Helper()
	var count int
	if err := c.s.IterJournal(func(e *journal.Entry) error {
		if e.Kind == kind {
			count++
		}
		return nil
	}); err != nil {
		t.Fatalf("IterJournal: %v", err)
	}
	return count
}

// --- 10a: Find with scope -------------------------------------------------

func TestPhase10FindWithScopeFiltersByType(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)

	// Mix: 2 prefs + 1 fact. Scope only allows Preference.
	writePref(t, c, "tone", 5)
	writePref(t, c, "tempo", 5)
	_, _ = c.Write(memory.Head{ActorScope: "andrew"}, memory.FactData{
		SchemaVersion: 1,
		Subject:       "andrew",
		Predicate:     "knows",
		Statement:     "go",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})

	s := buildTestScope(t, c, priv, nil) // Include.Types=[Preference]

	res, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference, memory.TypeFact},
		Limit: 10,
		Scope: s,
	})
	if err != nil {
		t.Fatalf("Find scoped: %v", err)
	}
	// Fact filtered out by scope; only 2 prefs survive.
	if len(res.Memories) != 2 {
		t.Errorf("got %d memories, want 2 (Fact should be scope-filtered)", len(res.Memories))
	}
	for _, m := range res.Memories {
		if m.Head.Type != memory.TypePreference {
			t.Errorf("non-Preference leaked through scope: %s", m.Head.Type)
		}
	}
}

func TestPhase10FindWithoutResolverRejectsScopedCall(t *testing.T) {
	_, priv := scopeKeypair(t)
	c := openCortex(t)
	// Note: NO withResolver call.
	writePref(t, c, "tone", 5)
	s := buildTestScope(t, c, priv, nil)

	_, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
		Scope: s,
	})
	if !errors.Is(err, ErrNoKeyResolver) {
		t.Errorf("got err=%v want ErrNoKeyResolver", err)
	}
}

func TestPhase10FindRejectsExpiredScope(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	writePref(t, c, "tone", 5)

	s := buildTestScope(t, c, priv, func(sc *scope.Scope) {
		sc.ExpiresAt = time.Now().Add(-time.Hour)
	})

	_, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
		Scope: s,
	})
	if !errors.Is(err, scope.ErrScopeExpired) {
		t.Errorf("got err=%v want ErrScopeExpired", err)
	}
}

func TestPhase10FindRejectsBadSignature(t *testing.T) {
	pub, _ := scopeKeypair(t)
	_, otherPriv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub) // pub belongs to a different priv
	writePref(t, c, "tone", 5)

	s := buildTestScope(t, c, otherPriv, nil) // signed by otherPriv

	_, err := c.Find(query.Query{
		Type:  []memory.Type{memory.TypePreference},
		Limit: 10,
		Scope: s,
	})
	if !errors.Is(err, scope.ErrSignatureInvalid) {
		t.Errorf("got err=%v want ErrSignatureInvalid", err)
	}
}

// --- 10a: Context with scope ----------------------------------------------

func TestPhase10ContextWithScopeFiltersFrameTier(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)

	// One pref with a frame, one without. Scope includes Preference
	// only — both prefs should be allowed structurally; verify the
	// scope allows the frame-relevant pref.
	matched := writePrefWithFrames(t, c, "precision", 5, frameAcquireGPU())
	_ = matched

	s := buildTestScope(t, c, priv, nil)

	bundle, err := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		BudgetTokens: 2000,
		Scope:        s,
	})
	if err != nil {
		t.Fatalf("Context scoped: %v", err)
	}
	if len(bundle.FrameRelevant) == 0 {
		t.Errorf("expected matched pref in FrameRelevant, got empty")
	}
}

func TestPhase10ContextScopeBudgetCapHonoured(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	writePref(t, c, "tone", 5)

	s := buildTestScope(t, c, priv, func(sc *scope.Scope) {
		sc.BudgetTokens = 100 // tight cap
	})

	// Caller asks for 3000; scope cap is 100 → should reject.
	_, err := c.Context(ContextOpts{
		BudgetTokens: 3000,
		Scope:        s,
	})
	if !errors.Is(err, scope.ErrBudgetExceeded) {
		t.Errorf("got err=%v want ErrBudgetExceeded", err)
	}
}

// --- 10a: ResolveScoped ---------------------------------------------------

func TestPhase10ResolveScopedHappyPath(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	uri := writePref(t, c, "tone", 5)

	s := buildTestScope(t, c, priv, nil) // Include=[Preference]

	mem, err := c.ResolveScoped(uri, s, time.Time{})
	if err != nil {
		t.Fatalf("ResolveScoped: %v", err)
	}
	if mem.Head.Type != memory.TypePreference {
		t.Errorf("Type=%s want Preference", mem.Head.Type)
	}
}

func TestPhase10ResolveScopedViolationJournals(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)

	// Memory is a Fact; scope only allows Preference → violation.
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.FactData{
		SchemaVersion: 1, Subject: "andrew", Predicate: "knows", Statement: "go",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	s := buildTestScope(t, c, priv, nil) // Include=[Preference]

	preCount := countKindEntries(t, c, journal.KindScopeViolation)
	_, err = c.ResolveScoped(uri, s, time.Time{})
	if !errors.Is(err, scope.ErrViolation) {
		t.Errorf("got err=%v want ErrViolation", err)
	}
	postCount := countKindEntries(t, c, journal.KindScopeViolation)
	if postCount != preCount+1 {
		t.Errorf("KindScopeViolation entries: pre=%d post=%d (expected +1)", preCount, postCount)
	}
}

// --- 10a: Multi-proof against snapshot -----------------------------------

func TestPhase10ProofVerifiesAgainstManifest(t *testing.T) {
	c := openCortex(t)
	uri1 := writePref(t, c, "tone", 5)
	uri2 := writePref(t, c, "tempo", 5)

	manifest, err := c.Snapshot(snapshot.TriggerExplicit)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	mp, err := c.Proof([]memory.URI{uri1, uri2}, manifest)
	if err != nil {
		t.Fatalf("Proof: %v", err)
	}
	if len(mp.Proofs) != 2 {
		t.Errorf("Proofs len=%d want 2", len(mp.Proofs))
	}
	if err := mp.VerifyAgainstManifest(manifest); err != nil {
		t.Errorf("VerifyAgainstManifest: %v", err)
	}
}

func TestPhase10ProofRejectsAfterRootDrift(t *testing.T) {
	c := openCortex(t)
	writePref(t, c, "tone", 5)

	manifest, _ := c.Snapshot(snapshot.TriggerExplicit)
	// Drift: another write changes the memories root.
	writePref(t, c, "drift", 5)

	_, err := c.Proof([]memory.URI{}, manifest)
	if !errors.Is(err, snapshot.ErrInvalidProof) {
		t.Errorf("got err=%v want snapshot.ErrInvalidProof (root drift)", err)
	}
}

// --- 10b: UpdateHead ------------------------------------------------------

func TestPhase10UpdateHeadAddsTags(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)
	_, id, _, _ := ParseURI(uri)

	newTags := []memory.Tag{"audio", "preference"}
	if _, err := c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}

	mem, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(mem.Head.Tags) != 2 {
		t.Errorf("Tags len=%d want 2: %v", len(mem.Head.Tags), mem.Head.Tags)
	}

	// Find by tag should hit.
	res, err := c.Find(query.Query{
		Where: query.HasTag{Tag: "audio"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	found := false
	for _, m := range res.Memories {
		if m.Head.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("idx/tag for new tag not emitted (Find by tag missed)")
	}
}

func TestPhase10UpdateHeadRemovesTags(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5, "audio", "preference")
	_, id, _, _ := ParseURI(uri)

	emptyTags := []memory.Tag{}
	if _, err := c.UpdateHead(uri, HeadPatch{Tags: &emptyTags}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}

	// idx/tag entries should be hard-deleted.
	res, err := c.Find(query.Query{
		Where: query.HasTag{Tag: "audio"},
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Find: %v", err)
	}
	for _, m := range res.Memories {
		if m.Head.ID == id {
			t.Errorf("idx/tag for removed tag not deleted (Find by tag still hits)")
		}
	}
}

func TestPhase10UpdateHeadNoVersionBump(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)

	// Capture pre-update version state.
	preMem, _ := c.Resolve(uri)
	preVersion := preMem.Head.CurrentVersion

	newTags := []memory.Tag{"newtag"}
	gotURI, err := c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{CreatedBy: "andrew"})
	if err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}
	// URI must be unchanged version-wise.
	if gotURI != uri {
		t.Errorf("URI changed: pre=%s post=%s", uri, gotURI)
	}
	postMem, _ := c.Resolve(uri)
	if postMem.Head.CurrentVersion != preVersion {
		t.Errorf("CurrentVersion changed: pre=%d post=%d (UpdateHead should NOT bump Version)",
			preVersion, postMem.Head.CurrentVersion)
	}
	// mv/<id>/v/<n> should have IDENTICAL bytes (Data unchanged).
	if !bytes.Equal(preMem.Version.Data, postMem.Version.Data) {
		t.Errorf("Version.Data changed under UpdateHead")
	}
	if preMem.Version.Hash != postMem.Version.Hash {
		t.Errorf("Version.Hash changed under UpdateHead")
	}
}

func TestPhase10UpdateHeadAdvancesMemoriesRoot(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)

	preRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot pre: %v", err)
	}

	newTags := []memory.Tag{"newtag"}
	if _, err := c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}

	postRoot, err := c.OverallRoot()
	if err != nil {
		t.Fatalf("OverallRoot post: %v", err)
	}
	if preRoot == postRoot {
		t.Errorf("OverallRoot did not advance after UpdateHead (memories_root should change)")
	}
}

func TestPhase10UpdateHeadJournalsKindUpdateHead(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)

	pre := countKindEntries(t, c, journal.KindUpdateHead)
	newTags := []memory.Tag{"newtag"}
	if _, err := c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}
	post := countKindEntries(t, c, journal.KindUpdateHead)
	if post != pre+1 {
		t.Errorf("KindUpdateHead entries: pre=%d post=%d (want +1)", pre, post)
	}
}

func TestPhase10UpdateHeadAddsAndRemovesFrames(t *testing.T) {
	c := openCortex(t)
	id := writePrefWithFrames(t, c, "tone", 5, frameAcquireGPU())
	uri := BuildURI(memory.TypePreference, id, 1)

	// Replace single frame with a different frame.
	newFrames := []memory.FrameRef{frameAcquireModel()}
	if _, err := c.UpdateHead(uri, HeadPatch{Frames: &newFrames}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}

	mem, err := c.Resolve(uri)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(mem.Head.Frames) != 1 {
		t.Fatalf("Frames len=%d want 1", len(mem.Head.Frames))
	}
	if mem.Head.Frames[0].ObjKind != memory.KindModel {
		t.Errorf("Frame ObjKind=%v want KindModel", mem.Head.Frames[0].ObjKind)
	}

	// Context with the OLD frame tuple → should NOT find this memory.
	bundle, err := c.Context(ContextOpts{
		Verb:    memory.VerbAcquire,
		Objects: map[string]string{"service": "gpu_inference"}, // old tuple
	})
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	for _, m := range bundle.FrameRelevant {
		if m.Head.ID == id {
			t.Errorf("old idx/frame not deleted: memory %s found by GPU tuple", id)
		}
	}

	// Context with the NEW frame tuple → should find.
	bundle2, err := c.Context(ContextOpts{
		Verb:    memory.VerbAcquire,
		Objects: map[string]string{"model": "llama-405b"},
	})
	if err != nil {
		t.Fatalf("Context2: %v", err)
	}
	found := false
	for _, m := range bundle2.FrameRelevant {
		if m.Head.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("new idx/frame not emitted: memory %s missing under model tuple", id)
	}
}

func TestPhase10UpdateHeadEventDiffsActorObj(t *testing.T) {
	c := openCortex(t)
	id := writeEventWithFrames(t, c, "ev", memory.OutcomeSuccess, frameAcquireGPU())
	uri := BuildURI(memory.TypeEvent, id, 1)

	// Replace frame: old idx/actor_obj should hard-delete, new should emit.
	newFrames := []memory.FrameRef{frameAcquireModel()}
	if _, err := c.UpdateHead(uri, HeadPatch{Frames: &newFrames}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}

	// Outcomes scan with old tuple → empty.
	bundle, _ := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"service": "gpu_inference"},
		IncludeTiers: []Tier{TierOutcomes},
	})
	for _, m := range bundle.Outcomes {
		if m.Head.ID == id {
			t.Errorf("old idx/actor_obj not deleted: event %s still in Outcomes for old tuple", id)
		}
	}

	// Outcomes scan with new tuple → finds.
	bundle2, _ := c.Context(ContextOpts{
		Verb:         memory.VerbAcquire,
		Objects:      map[string]string{"model": "llama-405b"},
		IncludeTiers: []Tier{TierOutcomes},
	})
	found := false
	for _, m := range bundle2.Outcomes {
		if m.Head.ID == id {
			found = true
		}
	}
	if !found {
		t.Errorf("new idx/actor_obj missing: event %s not in Outcomes for new tuple", id)
	}
}

func TestPhase10UpdateHeadImportanceAndVisibility(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)

	imp := uint8(9)
	vis := memory.VisActorPublic
	if _, err := c.UpdateHead(uri, HeadPatch{
		DeclaredImportance: &imp,
		Visibility:         &vis,
	}, UpdateHeadMeta{CreatedBy: "andrew"}); err != nil {
		t.Fatalf("UpdateHead: %v", err)
	}
	mem, _ := c.Resolve(uri)
	if mem.Head.DeclaredImportance != 9 {
		t.Errorf("DeclaredImportance=%d want 9", mem.Head.DeclaredImportance)
	}
	if mem.Head.Visibility != memory.VisActorPublic {
		t.Errorf("Visibility=%v want VisActorPublic", mem.Head.Visibility)
	}
}

func TestPhase10UpdateHeadEmptyPatchReturnsErrNoOp(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)

	_, err := c.UpdateHead(uri, HeadPatch{}, UpdateHeadMeta{CreatedBy: "andrew"})
	if !errors.Is(err, ErrNoOp) {
		t.Errorf("got err=%v want ErrNoOp", err)
	}
}

func TestPhase10UpdateHeadRejectsTombstoned(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)
	if err := c.Tombstone(uri, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}

	newTags := []memory.Tag{"newtag"}
	_, err := c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{CreatedBy: "andrew"})
	if !errors.Is(err, memory.ErrTombstoned) {
		t.Errorf("got err=%v want ErrTombstoned", err)
	}
}

func TestPhase10UpdateHeadNonWritableScopeRejected(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	uri := writePref(t, c, "tone", 5)

	s := buildTestScope(t, c, priv, func(sc *scope.Scope) {
		sc.Writable = false // explicit
	})

	preCount := countKindEntries(t, c, journal.KindScopeViolation)
	newTags := []memory.Tag{"newtag"}
	_, err := c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{
		CreatedBy: "child",
		Scope:     s,
	})
	if !errors.Is(err, scope.ErrNotWritable) {
		t.Errorf("got err=%v want ErrNotWritable", err)
	}
	post := countKindEntries(t, c, journal.KindScopeViolation)
	if post != preCount+1 {
		t.Errorf("KindScopeViolation: pre=%d post=%d (expected +1)", preCount, post)
	}
}

func TestPhase10UpdateHeadWritableScopeAllowed(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)
	uri := writePref(t, c, "tone", 5)

	s := buildTestScope(t, c, priv, func(sc *scope.Scope) {
		sc.Writable = true
	})

	newTags := []memory.Tag{"newtag"}
	if _, err := c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{
		CreatedBy: "child",
		Scope:     s,
	}); err != nil {
		t.Errorf("UpdateHead writable scope: %v", err)
	}
}

func TestPhase10UpdateHeadOutOfScopeRejected(t *testing.T) {
	pub, priv := scopeKeypair(t)
	c := openCortex(t)
	withResolver(t, c, pub)

	// Memory is a Fact; scope writable but only allows Preference.
	uri, err := c.Write(memory.Head{ActorScope: "andrew"}, memory.FactData{
		SchemaVersion: 1, Subject: "andrew", Predicate: "knows", Statement: "go",
	}, WriteMeta{CreatedBy: "andrew",
		Provenance: memory.Provenance{Source: memory.SourceUserInput}})
	if err != nil {
		t.Fatalf("Write: %v", err)
	}

	s := buildTestScope(t, c, priv, func(sc *scope.Scope) {
		sc.Writable = true // writable but Include=[Preference]
	})

	newTags := []memory.Tag{"newtag"}
	_, err = c.UpdateHead(uri, HeadPatch{Tags: &newTags}, UpdateHeadMeta{
		CreatedBy: "child",
		Scope:     s,
	})
	if !errors.Is(err, scope.ErrViolation) {
		t.Errorf("got err=%v want ErrViolation", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
