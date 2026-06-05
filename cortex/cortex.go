// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package cortex is the top-level facade implementing the cortex Read/Write
// API surface defined in research/04-cortex.md §11–12.
//
// Phase 2 scope: Write, Update, Tombstone, Resolve, plus the matching URI
// parsing helpers. Find/Context/Compact and snapshots/proofs land in later
// phases.
//
// Atomicity model (§11.1): every mutating call composes one Pebble batch via
// store.BeginWrite. The batch contains:
//   - the journal entry  (j/<seq>)
//   - meta/journal_head bump
//   - the new MemoryHead  (m/<id>)
//   - the new MemoryVersion  (mv/<id>/v/<n>)
//   - any predicate index keys touched by this write
//
// Either everything commits or nothing does. There is no path that mutates
// store/ without a corresponding journal entry, satisfying the replay
// invariant.
package cortex

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/cortex/forms"
	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/salience"
	"matrix/cortex/scope"
	"matrix/cortex/snapshot"
	"matrix/cortex/store"
)

// Cortex wraps a per-actor store.Store and exposes the §11–12 API.
type Cortex struct {
	s     *store.Store
	now   func() time.Time // overridable for tests
	idGen func() memory.ID

	// embed holds the async embedding worker state when StartEmbedder is
	// called. Nil when no worker is running (the default). All access
	// goes through helpers in embedder.go.
	embed *embedderState

	// snap holds the journal MMR + per-namespace SMTs (Phase 7). Always
	// constructed in New; the MMRHook is installed on the underlying
	// Store so every journal entry contributes a leaf transparently.
	// SMT updates are staged explicitly by Write/Update/Tombstone/AddEdge/
	// RemoveEdge and the embedder.
	snap *snapshot.State

	// keyResolver is used by Phase 10 scope verification (sub-agent
	// CortexScope reads/writes). nil means scoped calls are rejected
	// with ErrNoKeyResolver. Cortex never holds key material itself
	// (D4); the resolver is plumbed in by the agent runtime.
	keyResolver scope.KeyResolver

	// rl holds the Phase 14 token buckets gating
	// (1) cortex.logScopeViolation journal writes (R5 DoS surface)
	// (2) cortex.Attest entry (R3b DoS surface).
	// Always non-nil after New; never persisted (runtime policy state,
	// not memory data — see ratelimit.go Q4 lock).
	rl *rateLimiter
}

// Option configures a Cortex on construction. Reserved for clock and ID
// injection in tests; production callers pass nothing.
type Option func(*Cortex)

// WithClock overrides the wallclock used for CreatedAt / LastUpdatedAt.
func WithClock(now func() time.Time) Option { return func(c *Cortex) { c.now = now } }

// WithIDGen overrides the ULID generator (only useful in tests).
func WithIDGen(gen func() memory.ID) Option { return func(c *Cortex) { c.idGen = gen } }

// New wraps an open store.Store as a Cortex. Constructs the snapshot
// State and installs its MMRHook on s so every journal entry — whether
// emitted by Cortex.Write, the async embedder, the late-binding Find
// audit, or the cortex-shell smoke commands — appends an MMR leaf in
// the same atomic batch as the j/<seq> write.
func New(s *store.Store, opts ...Option) *Cortex {
	c := &Cortex{
		s:     s,
		now:   func() time.Time { return time.Now().UTC() },
		idGen: memory.NewID,
		snap:  snapshot.New(s),
		rl:    newRateLimiter(),
	}
	s.SetJournalHook(c.snap.MMRHook())
	for _, o := range opts {
		o(c)
	}
	return c
}

// Snap returns the snapshot State. Reserved for sibling packages
// (embedder, query) and the CLI snapshot/prove subcommands.
func (c *Cortex) Snap() *snapshot.State { return c.snap }

// Snapshot persists a SnapshotManifest at the current state and returns
// it. Pull-driven (§7.4): callers (skill compiler, tools/attest, periodic
// snapshotter) decide when to take one. Reason is recorded in the
// manifest's Trigger field for ops visibility but does NOT participate
// in OverallRoot.
//
// The actor name comes from the underlying store; SignedBy is left
// blank for the runtime to fill in before chain anchoring (cortex
// holds no key material per D4).
func (c *Cortex) Snapshot(reason string) (*snapshot.Manifest, error) {
	return c.snap.Snapshot(c.s.Actor(), reason, c.now())
}

// OverallRoot returns the current cortex_snapshot_hash without persisting
// a SnapshotManifest. Used by the compiler determinism seed (D11): the
// seed is hash(intent.id || actor || OverallRoot()).
func (c *Cortex) OverallRoot() ([32]byte, error) {
	_, _, root, err := c.snap.CurrentRoots()
	return root, err
}

// Store returns the underlying store handle. Reserved for sibling packages.
func (c *Cortex) Store() *store.Store { return c.s }

// Write inserts a new memory at version 1.
//
// Inputs:
//   - data: a typed Data struct (one of the nine memory.*Data shapes)
//   - h: caller-supplied Head fields. Required: ActorScope, Visibility (or
//     leave zero for VisPrivate). ID and CurrentVersion are filled in.
//   - meta: per-version metadata (CreatedBy, Confidence, Provenance, Forms).
//
// Returns the canonical URI matrix://cortex/<type>/<id>#1.
func (c *Cortex) Write(h memory.Head, data memory.TypedData, meta WriteMeta) (memory.URI, error) {
	if data == nil {
		return "", memory.ErrEmptyData
	}
	encoded, err := memory.EncodeData(data)
	if err != nil {
		return "", fmt.Errorf("cortex.Write: encode data: %w", err)
	}

	if h.ID.IsZero() {
		h.ID = c.idGen()
	}
	h.Type = memory.TypeOf(data)
	h.CurrentVersion = 1
	if h.Visibility == 0 {
		h.Visibility = memory.VisPrivate
	}
	now := c.now()
	h.LastUpdatedAt = now

	// Forms (§9). Skill overrides go through meta.FormsOverride=true and are
	// validated for budget by memory.ValidateMemory below; the auto path
	// renders via forms.Render which truncates to budget by construction.
	renderedForms := meta.Forms
	if !meta.FormsOverride {
		renderedForms = forms.Render(&h, data)
	}
	h.Forms = renderedForms

	v := memory.Version{
		ID:            h.ID,
		Version:       1,
		Type:          h.Type,
		Data:          encoded,
		CreatedAt:     now,
		CreatedBy:     meta.CreatedBy,
		Confidence:    meta.Confidence,
		Provenance:    meta.Provenance,
		Forms:         renderedForms,
		FormsOverride: meta.FormsOverride,
	}
	if v.Confidence == 0 {
		v.Confidence = 1.0
	}

	if err := memory.ValidateMemory(&h, &v, data); err != nil {
		return "", fmt.Errorf("cortex.Write: validate: %w", err)
	}

	hash, err := memory.HashVersion(&v)
	if err != nil {
		return "", fmt.Errorf("cortex.Write: hash: %w", err)
	}
	v.Hash = hash

	headBytes, err := memory.EncodeHead(&h)
	if err != nil {
		return "", fmt.Errorf("cortex.Write: encode head: %w", err)
	}
	versionBytes, err := memory.EncodeVersion(&v)
	if err != nil {
		return "", fmt.Errorf("cortex.Write: encode version: %w", err)
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()

	if err := wb.Set(keys.MemoryHeadKey(toKeysULID(h.ID)), headBytes); err != nil {
		return "", err
	}
	if err := wb.Set(keys.MemoryVersionKey(toKeysULID(h.ID), v.Version), versionBytes); err != nil {
		return "", err
	}
	// idx/type/<t>/<created_unix_nano>/<id> -> nil. created is uint64 BE; we
	// reinterpret int64 nanos as uint64 so byte-sort=time-sort over the
	// plausible range (post-1970).
	if err := wb.Set(
		keys.IdxTypeKey(byte(h.Type), uint64(now.UnixNano()), toKeysULID(h.ID)),
		nil,
	); err != nil {
		return "", err
	}
	// idx/tag/<tag_hash:8>/<created:8>/<id:16> for each tag on the head.
	// Tags are immutable across Update in Phase 3 (Update only replaces
	// Data); changing tags requires a future UpdateHead surface.
	for _, t := range h.Tags {
		if err := wb.Set(
			keys.IdxTagKey(hashTag(string(t)), uint64(now.UnixNano()), toKeysULID(h.ID)),
			nil,
		); err != nil {
			return "", err
		}
	}
	// idx/frame and idx/actor_obj (Phase 8): emit one entry per
	// FrameRef on the head. Auto-derivation rule:
	//   - idx/frame for ALL memory types — so prefs/beliefs/etc.
	//     stamped with frame relevance are discoverable by the
	//     Frame-relevant tier in cortex.context.
	//   - idx/actor_obj only for h.Type == TypeEvent — outcomes
	//     history is an Event-only concept per
	//     research/03-retrieval-patterns.md §2.1 ("1–3 prior similar
	//     intents and their outcomes").
	// Both indexes participate in this same atomic Pebble batch with
	// the journal entry + Head + Version + SMT update; either the
	// memory write succeeds in full or no idx/* keys appear (§11.1).
	// Frames are immutable across Update (mirrors Tags); UpdateHead
	// for mutation lands in Phase 10.
	for _, fr := range h.Frames {
		objHash := fr.Hash()
		if err := wb.Set(
			keys.IdxFrameKey(byte(fr.Verb), byte(fr.ObjKind), objHash, toKeysULID(h.ID)),
			nil,
		); err != nil {
			return "", err
		}
		if h.Type == memory.TypeEvent {
			if err := wb.Set(
				keys.IdxActorObjKey(byte(fr.Verb), objHash, uint64(now.UnixNano()), toKeysULID(h.ID)),
				nil,
			); err != nil {
				return "", err
			}
		}
	}
	// salience/<id>: initial cold score so Find can rank without a live
	// recompute on the first read of every memory.
	score := salience.NewForWrite(h.DeclaredImportance, now)
	scoreBytes, err := salience.Encode(&score)
	if err != nil {
		return "", fmt.Errorf("cortex.Write: encode salience: %w", err)
	}
	if err := wb.Set(keys.SalienceKey(toKeysULID(h.ID)), scoreBytes); err != nil {
		return "", err
	}

	wp := &journal.WritePayload{
		SchemaVersion: 1,
		ID:            h.ID,
		Version:       v.Version,
		Type:          uint8(h.Type),
		Hash:          v.Hash,
	}
	wpBytes, err := journal.EncodeWritePayload(wp)
	if err != nil {
		return "", fmt.Errorf("cortex.Write: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindWrite,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(meta.CreatedBy),
		Payload:   wpBytes,
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", err
	}
	// Phase 7: stage the memories-namespace SMT update with the new Head
	// canonical bytes so the snapshot StateRoot["memories"] commits to
	// this write.
	if err := c.snap.StageMemoryUpdate(wb, h.ID, headBytes); err != nil {
		return "", fmt.Errorf("cortex.Write: stage SMT: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.Write: commit: %w", err)
	}
	c.notifyEmbedder()

	return BuildURI(h.Type, h.ID, v.Version), nil
}

// WriteMeta carries the per-version metadata supplied to Write.
type WriteMeta struct {
	CreatedBy     string
	Confidence    float32
	Provenance    memory.Provenance
	Forms         memory.Forms
	FormsOverride bool
}

// Resolve fetches the memory pointed to by uri. The URI must include an
// explicit version; #latest is resolved by the compiler before sign-off
// (D13), not here. Returns ErrNotFound if the head or version is missing.
func (c *Cortex) Resolve(uri memory.URI) (*memory.Memory, error) {
	t, id, version, err := ParseURI(uri)
	if err != nil {
		return nil, err
	}

	headBytes, ok, err := c.s.Get(keys.MemoryHeadKey(toKeysULID(id)))
	if err != nil {
		return nil, fmt.Errorf("cortex.Resolve: get head: %w", err)
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var h memory.Head
	if err := memory.DecodeHead(headBytes, &h); err != nil {
		return nil, fmt.Errorf("cortex.Resolve: decode head: %w", err)
	}
	if h.Type != t {
		return nil, fmt.Errorf("cortex.Resolve: type mismatch in URI vs head: uri=%s head=%s",
			t, h.Type)
	}

	verBytes, ok, err := c.s.Get(keys.MemoryVersionKey(toKeysULID(id), version))
	if err != nil {
		return nil, fmt.Errorf("cortex.Resolve: get version: %w", err)
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var v memory.Version
	if err := memory.DecodeVersion(verBytes, &v); err != nil {
		return nil, fmt.Errorf("cortex.Resolve: decode version: %w", err)
	}

	return &memory.Memory{Head: h, Version: v}, nil
}

// ResolveLatest fetches the latest version of the memory identified by id.
// Provided for convenience but discouraged in production paths — the
// compiler should pin a version before sign-off (D13).
func (c *Cortex) ResolveLatest(id memory.ID) (*memory.Memory, error) {
	headBytes, ok, err := c.s.Get(keys.MemoryHeadKey(toKeysULID(id)))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var h memory.Head
	if err := memory.DecodeHead(headBytes, &h); err != nil {
		return nil, err
	}
	verBytes, ok, err := c.s.Get(keys.MemoryVersionKey(toKeysULID(id), h.CurrentVersion))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var v memory.Version
	if err := memory.DecodeVersion(verBytes, &v); err != nil {
		return nil, err
	}
	return &memory.Memory{Head: h, Version: v}, nil
}

// ResolveScoped is the sub-agent variant of Resolve. It (a) verifies
// the supplied CortexScope (signature, schema, expiry, snapshot
// resolvability, multi-proof) ONCE and then (b) checks per-target
// scope.Allows on the loaded Head before returning. Single-target
// reads journal a KindScopeViolation entry on Allows miss (unlike
// Find/Context which filter silently — research/06-agents.md §7.2).
//
// `now` is the wall-clock used for scope expiry; pass time.Time{} to
// defer to c.now().
//
// Spec: research/04-cortex.md §10 + §12 ("Scope *CortexScope on
// Query"); Phase 10 Q3 lock — Scope rides on read calls, NOT a wrapper
// type.
func (c *Cortex) ResolveScoped(uri memory.URI, s *scope.Scope, now time.Time) (*memory.Memory, error) {
	if s == nil {
		return c.Resolve(uri)
	}
	if now.IsZero() {
		now = c.now()
	}
	if err := c.VerifyScope(s, now); err != nil {
		return nil, fmt.Errorf("cortex.ResolveScoped: %w", err)
	}
	mem, err := c.Resolve(uri)
	if err != nil {
		return nil, err
	}
	if err := c.enforceRead(s, &mem.Head); err != nil {
		return nil, fmt.Errorf("cortex.ResolveScoped: %w", err)
	}
	return mem, nil
}

// Proof returns a multi-proof bundle over the memories namespace for
// the supplied URIs at the manifest's pinned snapshot. Mirrors the
// research/04-cortex.md §12 surface ("Proof(actor, keys, manifest) →
// MerkleProof"). Used by sub-dispatch executors that want to ship a
// CortexScope.Proofs payload alongside the dispatch envelope.
//
// Builds a Snapshot lookup against the pinned manifest's
// StateRoots["memories"] root: for each URI we (a) resolve current
// canonical Head bytes, (b) compute the SMT key hash via the snapshot
// package's HashMemoryKey, (c) Prove against the namespace SMT.
//
// Caller-supplied manifest pins the root the proofs MUST verify
// against. Cortex does NOT re-snapshot inside Proof — the caller has
// already chosen the snapshot it wants the scope anchored to (typical
// flow: Snapshot() then Proof()).
//
// Returns ErrManifestRootMismatch (wrapped via snapshot.ErrInvalidProof)
// if the current memories SMT root has drifted away from the
// manifest's root since manifest was taken — the caller must
// re-Snapshot before re-Proof.
func (c *Cortex) Proof(uris []memory.URI, manifest *snapshot.Manifest) (*snapshot.MultiProof, error) {
	if manifest == nil {
		return nil, errors.New("cortex.Proof: nil manifest")
	}
	want, ok := manifest.StateRoots["memories"]
	if !ok {
		return nil, errors.New("cortex.Proof: manifest has no memories state root")
	}
	cur, err := c.snap.SMT("memories").Root()
	if err != nil {
		return nil, fmt.Errorf("cortex.Proof: load current memories root: %w", err)
	}
	if cur != want {
		return nil, fmt.Errorf("%w: memories root drifted since manifest seq=%d (re-Snapshot before Proof)", snapshot.ErrInvalidProof, manifest.SeqAtSnapshot)
	}
	items := make([]snapshot.MultiProofItem, len(uris))
	for i, uri := range uris {
		_, id, _, err := ParseURI(uri)
		if err != nil {
			return nil, fmt.Errorf("cortex.Proof: parse uri %d: %w", i, err)
		}
		headBytes, ok, err := c.s.Get(keys.MemoryHeadKey(toKeysULID(id)))
		if err != nil {
			return nil, fmt.Errorf("cortex.Proof: get head %d: %w", i, err)
		}
		var canonical []byte
		if ok {
			canonical = headBytes // present → membership proof
		}
		items[i] = snapshot.MultiProofItem{
			KeyHash:   snapshot.HashMemoryKey(toFixed16(id)),
			Canonical: canonical,
		}
	}
	return c.snap.BuildMultiProofWithValues("memories", items)
}

// Verify is a thin wrapper around scope.Verify that uses this Cortex's
// snap state and KeyResolver. Reserved for callers (typically
// tools/attest, sub-dispatch verifiers) that want the same chain a
// scoped read would run, without performing a read. Returns nil iff
// the scope is fully verified at `now`.
func (c *Cortex) Verify(s *scope.Scope, now time.Time) error {
	return c.VerifyScope(s, now)
}

// toFixed16 copies a memory.ID into a [16]byte (alias). Both are
// 16-byte arrays; the snapshot package keys on [16]byte directly.
func toFixed16(id memory.ID) [16]byte {
	var out [16]byte
	copy(out[:], id[:])
	return out
}

// Update creates a new version of an existing memory by replacing its Data
// with newData. SlotPatch typed-patch compilation (D8 → RFC 6902) lands in
// Phase 3; for now Update takes whole-Data replacement, which is
// semantically a "patch every field" operation.
//
// Returns the new URI matrix://cortex/<type>/<id>#<n+1>.
func (c *Cortex) Update(uri memory.URI, newData memory.TypedData, meta WriteMeta) (memory.URI, error) {
	if newData == nil {
		return "", memory.ErrEmptyData
	}
	t, id, _, err := ParseURI(uri)
	if err != nil {
		return "", err
	}
	if memory.TypeOf(newData) != t {
		return "", memory.ErrTypeDataMismatch
	}

	prev, err := c.ResolveLatest(id)
	if err != nil {
		return "", fmt.Errorf("cortex.Update: resolve latest: %w", err)
	}
	if prev.Head.Tombstoned != nil {
		return "", memory.ErrTombstoned
	}

	encoded, err := memory.EncodeData(newData)
	if err != nil {
		return "", fmt.Errorf("cortex.Update: encode: %w", err)
	}

	now := c.now()
	nextVer := prev.Head.CurrentVersion + 1

	h := prev.Head
	h.CurrentVersion = nextVer
	h.LastUpdatedAt = now

	// Re-render forms against the new Data unless the caller supplied an
	// override. Head.Forms is updated alongside Version.Forms so the next
	// list/Find pass on this memory reads the latest scaffold without an
	// extra Pebble Get on mv/.
	renderedForms := meta.Forms
	if !meta.FormsOverride {
		renderedForms = forms.Render(&h, newData)
	}
	h.Forms = renderedForms

	v := memory.Version{
		ID:            id,
		Version:       nextVer,
		Type:          t,
		Data:          encoded,
		CreatedAt:     now,
		CreatedBy:     meta.CreatedBy,
		Confidence:    meta.Confidence,
		Provenance:    meta.Provenance,
		Forms:         renderedForms,
		FormsOverride: meta.FormsOverride,
	}
	if v.Confidence == 0 {
		v.Confidence = 1.0
	}
	if err := memory.ValidateMemory(&h, &v, newData); err != nil {
		return "", fmt.Errorf("cortex.Update: validate: %w", err)
	}

	hash, err := memory.HashVersion(&v)
	if err != nil {
		return "", err
	}
	v.Hash = hash

	headBytes, err := memory.EncodeHead(&h)
	if err != nil {
		return "", err
	}
	verBytes, err := memory.EncodeVersion(&v)
	if err != nil {
		return "", err
	}

	// Bump salience LastUsed and recompute Cached. If the cache entry is
	// missing (e.g. memory created before Phase 3 wiring), seed a fresh one.
	scorePtr, ok, err := salience.Read(c.s, id)
	if err != nil {
		return "", err
	}
	var score salience.Score
	if ok {
		score = *scorePtr
		salience.BumpForUpdate(&score, h.DeclaredImportance, now)
	} else {
		score = salience.NewForWrite(h.DeclaredImportance, now)
	}
	scoreBytes, err := salience.Encode(&score)
	if err != nil {
		return "", fmt.Errorf("cortex.Update: encode salience: %w", err)
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(keys.MemoryHeadKey(toKeysULID(id)), headBytes); err != nil {
		return "", err
	}
	if err := wb.Set(keys.MemoryVersionKey(toKeysULID(id), nextVer), verBytes); err != nil {
		return "", err
	}
	if err := wb.Set(keys.SalienceKey(toKeysULID(id)), scoreBytes); err != nil {
		return "", err
	}
	up := &journal.WritePayload{
		SchemaVersion: 1,
		ID:            id,
		Version:       nextVer,
		Type:          uint8(t),
		Hash:          v.Hash,
	}
	upBytes, err := journal.EncodeWritePayload(up)
	if err != nil {
		return "", fmt.Errorf("cortex.Update: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindUpdate,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(meta.CreatedBy),
		Payload:   upBytes,
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", err
	}
	// Phase 7: stage memories SMT update with the rewritten Head bytes.
	if err := c.snap.StageMemoryUpdate(wb, id, headBytes); err != nil {
		return "", fmt.Errorf("cortex.Update: stage SMT: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", err
	}
	c.notifyEmbedder()
	return BuildURI(t, id, nextVer), nil
}

// Tombstone soft-deletes the memory identified by uri. The MemoryHead is
// rewritten with Tombstoned set; mv/<id>/v/* records are retained (audit
// trail per §6, §11). A tomb/<id> marker is also written.
func (c *Cortex) Tombstone(uri memory.URI, reason, by string) error {
	_, id, _, err := ParseURI(uri)
	if err != nil {
		return err
	}
	prev, err := c.ResolveLatest(id)
	if err != nil {
		return err
	}
	if prev.Head.Tombstoned != nil {
		return nil // idempotent
	}

	now := c.now()
	h := prev.Head
	h.Tombstoned = &memory.Tombstone{Reason: reason, At: now, By: by}
	h.LastUpdatedAt = now

	headBytes, err := memory.EncodeHead(&h)
	if err != nil {
		return err
	}

	// Collapse the salience cache so Find ranks this memory at zero (per
	// §8.2 tombstone ceiling). Factor inputs are preserved so a hypothetical
	// un-tombstone path could recompute correctly.
	scorePtr, hadScore, err := salience.Read(c.s, id)
	if err != nil {
		return err
	}
	var scoreBytes []byte
	if hadScore {
		salience.ZeroForTombstone(scorePtr, now)
		b, err := salience.Encode(scorePtr)
		if err != nil {
			return fmt.Errorf("cortex.Tombstone: encode salience: %w", err)
		}
		scoreBytes = b
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(keys.MemoryHeadKey(toKeysULID(id)), headBytes); err != nil {
		return err
	}
	tombMarker := []byte(reason)
	if err := wb.Set(keys.TombstoneKey(toKeysULID(id)), tombMarker); err != nil {
		return err
	}
	if scoreBytes != nil {
		if err := wb.Set(keys.SalienceKey(toKeysULID(id)), scoreBytes); err != nil {
			return err
		}
	}
	je := &journal.Entry{
		Kind:      journal.KindTombstone,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(by),
		Payload:   id[:],
	}
	if err := wb.AppendJournal(je); err != nil {
		return err
	}
	// Phase 7: stage memories SMT update — the Head bytes now carry
	// Head.Tombstoned set, so the SMT value-hash changes and the snapshot
	// memories root reflects the soft-delete.
	if err := c.snap.StageMemoryUpdate(wb, id, headBytes); err != nil {
		return fmt.Errorf("cortex.Tombstone: stage SMT: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return err
	}
	c.notifyEmbedder()
	return nil
}

// Find runs a typed query against the actor's cortex. Thin facade over
// query.Run that lives at the cortex package boundary so callers don't
// need to thread the underlying *store.Store.
//
// See research/04-cortex.md §12 for the full query model. As of Phase 5
// Find honors Type, Where, OrderBy, Limit, Offset, IncludeTombstoned,
// LateBinding, Form, BudgetTokens, Near, NearURI. Graph traversal
// (From/Follow) still returns query.ErrUnsupported (Phase 6).
//
// Near / NearURI resolution happens here rather than in query.Run because
// the embedder and the HNSW index are cortex-level state. We translate
// the sugar fields into (NearVector, NearIndex) before delegating so
// query.Run remains a pure store→result function (no embedder or vector
// state in its closure of dependencies).
func (c *Cortex) Find(q query.Query) (*query.Result, error) {
	// Phase 10: scope verification once at entry. q.Scope is then
	// authenticated for the per-candidate filter inside query.Run
	// (which calls q.Scope.Allows(&head) silently — Find is a multi-
	// target read, no per-candidate violation log).
	if q.Scope != nil {
		if err := c.VerifyScope(q.Scope, c.now()); err != nil {
			return nil, fmt.Errorf("cortex.Find: %w", err)
		}
	}
	if q.Near != "" || q.NearURI != nil {
		if err := c.resolveNear(&q); err != nil {
			return nil, err
		}
	}
	return query.Run(c.s, q)
}

// resolveNear translates Query.Near (text) / Query.NearURI (memory URI)
// into (Query.NearVector, Query.NearIndex). The original Near / NearURI
// fields are cleared so query.Run treats the call as "already resolved";
// this avoids accidental double-resolution if the same Query is reused.
func (c *Cortex) resolveNear(q *query.Query) error {
	if c.embed == nil {
		return fmt.Errorf("cortex.Find: Near/NearURI requires StartEmbedder (no embedder running)")
	}
	idx := c.embed.index
	if idx == nil {
		return fmt.Errorf("cortex.Find: embedder running but index nil (bug)")
	}
	var qvec []float32
	switch {
	case q.Near != "":
		v, err := c.embed.embedder.Embed(q.Near)
		if err != nil {
			return fmt.Errorf("cortex.Find: embed query text: %w", err)
		}
		qvec = v
	case q.NearURI != nil:
		// Resolve URI → memory ID → vec/meta.
		_, id, _, err := ParseURI(*q.NearURI)
		if err != nil {
			return fmt.Errorf("cortex.Find: parse NearURI: %w", err)
		}
		var u keys.ULID
		copy(u[:], id[:])
		raw, ok, err := c.s.Get(keys.VecMetaKey(u))
		if err != nil {
			return fmt.Errorf("cortex.Find: read vec/meta: %w", err)
		}
		if !ok {
			return fmt.Errorf("cortex.Find: NearURI %s has no embedding yet (call DrainEmbedder?)", *q.NearURI)
		}
		var m memory.VectorMeta
		if err := memory.DecodeVectorMeta(raw, &m); err != nil {
			return fmt.Errorf("cortex.Find: decode vec/meta: %w", err)
		}
		qvec = m.Vector
	}
	q.NearVector = qvec
	q.NearIndex = idx
	// Don't clear Near/NearURI strings — callers may want them for audit
	// payloads. query.Run skips re-resolution when NearVector is non-nil.
	return nil
}

// hashTag returns the 8-byte sha256 prefix used as the idx/tag bucket key.
// Defined here in addition to query.HashTag so the cortex package's write
// path doesn't import its own consumer (avoids a needless dependency edge).
func hashTag(tag string) [keys.TagHashSize]byte {
	sum := sha256.Sum256([]byte(tag))
	var out [keys.TagHashSize]byte
	copy(out[:], sum[:keys.TagHashSize])
	return out
}

// ListByType returns all memory IDs of type t in idx/type insertion order
// (i.e. ascending by CreatedAt). Tombstoned memories are NOT filtered here;
// callers can filter via ResolveLatest. limit==0 means unlimited.
//
// Phase 2 helper for the cortex-shell CLI and tests; the §12 Find query
// engine subsumes this in Phase 3.
func (c *Cortex) ListByType(t memory.Type, limit int) ([]memory.ID, error) {
	if !t.Valid() {
		return nil, memory.ErrInvalidType
	}
	prefix := keys.IdxTypePrefix(byte(t))
	var ids []memory.ID
	err := c.s.PrefixIter(prefix, func(k, _ []byte) error {
		// key shape: idx/type/<t:1>/<created:8>/<id:16>
		if len(k) < len(prefix)+8+16 {
			return errors.New("cortex.ListByType: malformed idx/type key")
		}
		idStart := len(prefix) + 8
		var id memory.ID
		copy(id[:], k[idStart:idStart+16])
		ids = append(ids, id)
		if limit > 0 && len(ids) >= limit {
			return errStopIter
		}
		return nil
	})
	if err != nil && !errors.Is(err, errStopIter) {
		return nil, err
	}
	return ids, nil
}

var errStopIter = errors.New("stop iteration")

// --- URI helpers ----------------------------------------------------------

// BuildURI returns matrix://cortex/<Type>/<id-base32>#<version>.
func BuildURI(t memory.Type, id memory.ID, version uint64) memory.URI {
	return memory.URI(fmt.Sprintf("matrix://cortex/%s/%s#%d", t.String(), id.String(), version))
}

// ParseURI splits a canonical cortex URI. Format:
//
//	matrix://cortex/<Type>/<crockford-ulid>#<version>
//
// Returns ErrBadURI on any deviation. #latest is rejected; pre-resolution
// (D13) is the compiler's job.
func ParseURI(uri memory.URI) (memory.Type, memory.ID, uint64, error) {
	const prefix = "matrix://cortex/"
	s := string(uri)
	if !strings.HasPrefix(s, prefix) {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: missing %q prefix", memory.ErrBadURI, prefix)
	}
	rest := s[len(prefix):]
	hash := strings.IndexByte(rest, '#')
	if hash < 0 {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: missing #version", memory.ErrBadURI)
	}
	pathPart := rest[:hash]
	versionPart := rest[hash+1:]
	if versionPart == "latest" {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: #latest forbidden (D13)", memory.ErrBadURI)
	}
	slash := strings.IndexByte(pathPart, '/')
	if slash < 0 {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: missing /id", memory.ErrBadURI)
	}
	typeStr := pathPart[:slash]
	idStr := pathPart[slash+1:]

	t := parseTypeName(typeStr)
	if !t.Valid() {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: unknown type %q", memory.ErrBadURI, typeStr)
	}
	id, err := memory.ParseID(idStr)
	if err != nil {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: bad ulid %q: %v", memory.ErrBadURI, idStr, err)
	}
	var version uint64
	if _, err := fmt.Sscanf(versionPart, "%d", &version); err != nil {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: bad version %q", memory.ErrBadURI, versionPart)
	}
	if version == 0 {
		return 0, memory.ID{}, 0, fmt.Errorf("%w: version must be >=1", memory.ErrBadURI)
	}
	return t, id, version, nil
}

func parseTypeName(name string) memory.Type {
	switch name {
	case "Identity":
		return memory.TypeIdentity
	case "Fact":
		return memory.TypeFact
	case "Preference":
		return memory.TypePreference
	case "Belief":
		return memory.TypeBelief
	case "Event":
		return memory.TypeEvent
	case "Goal":
		return memory.TypeGoal
	case "Constraint":
		return memory.TypeConstraint
	case "Capability":
		return memory.TypeCapability
	case "Pattern":
		return memory.TypePattern
	}
	return 0
}

// toKeysULID converts memory.ID to keys.ULID (same byte layout, distinct
// types so packages don't have to import each other).
func toKeysULID(id memory.ID) keys.ULID {
	var u keys.ULID
	copy(u[:], id[:])
	return u
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
