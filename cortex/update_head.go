// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 10b: UpdateHead — mutate Head-only fields without bumping the
// Data version.
//
// Spec lock (Phase 10 Q4-Q8 in matrix.ctx phase10_locked_design):
//
//   Q4 new KindUpdateHead journal kind (cleaner replay than reusing
//      KindUpdate which means "Data version bump"). Mirrors Phase 9
//      KindCompact + Phase 5 KindEmbed cadence of dedicated kinds for
//      distinct semantics.
//
//   Q5 NO Version bump. Head bytes change → memories_root advances via
//      Phase 7 StageMemoryUpdate. mv/<id>/v/<n> records untouched.
//      URI scheme matrix://cortex/<type>/<id>#<n> still means Data
//      version n. Citations:
//        @/root/matrix/research/04-cortex.md:349 "Writes that change
//          Data create a new version" — UpdateHead by definition
//          doesn't change Data.
//        @/root/matrix/research/04-cortex.md:85 "MemoryHead is the
//          only mutable record per memory — and MemoryHead is small,
//          so writes are cheap" — explicit license for Head-only
//          mutation.
//
//   Q6 idx/* removal is HARD-DELETE (not soft tombstone). idx/* are
//      derived projections per the load-bearing replay invariant
//      ("drop indexes/, replay journal → byte-identical"). Audit trail
//      lives in the KindUpdateHead journal entry. Edges differ
//      because edges are user-facing FACTS with audit trail; idx/* are
//      pure derived metadata.
//
//   Q7 sub-agent UpdateHead requires scope.writable=true. Default-deny.
//      Spec @/root/matrix/research/04-cortex.md:554.
//
//   Q8 mutable fields = {Tags, Frames, DeclaredImportance, Visibility}.
//      ID/Type/CurrentVersion/ActorScope/LastUpdatedAt/EmbeddingRef/
//      Forms/Tombstoned are auto-managed and rejected at the API
//      boundary if the caller tries to set them.
//
// idx-key mechanics:
//
//   - idx/tag has shape <tag_hash:8>/<created:8>/<id:16>. The created
//     component pins the original Write time, so deletion requires a
//     prefix scan to find the key whose id-suffix matches our memory.
//   - idx/frame has shape <verb:1>/<kind:1>/<obj_hash:16>/<id:16>. NO
//     created component → direct delete by reconstruction.
//   - idx/actor_obj has shape <verb:1>/<obj_hash:16>/<created:8>/<id:16>.
//     Like idx/tag, deletion needs a prefix scan to recover the
//     created bytes.
//
// Replay determinism: KindUpdateHead carries the new HeadHash; replay
// re-runs the same patch logic against the prior Head loaded from
// the store at this seq, applies the diff, and re-derives identical
// idx/* writes. The journal entry is the canonical truth.
//
// Atomic batch:
//
//   m/<id>                              ← canonical CBOR new Head
//   idx/tag/<old>/...                   ← Delete (per removed tag)
//   idx/tag/<new>/...                   ← Set (per added tag)
//   idx/frame/<old>/...                 ← Delete (per removed frame)
//   idx/frame/<new>/...                 ← Set (per added frame)
//   idx/actor_obj/<old>/...             ← Delete (per removed Event frame)
//   idx/actor_obj/<new>/...             ← Set (per added Event frame)
//   salience/<id>                       ← Bump LastUsed (mirrors Update)
//   j/<seq>                             ← KindUpdateHead entry
//   accum/mmr/...                       ← MMR leaf (via JournalHook)
//   idx/smt/memories/...                ← SMT update (StageMemoryUpdate)

package cortex

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
	"matrix/cortex/scope"
)

// HeadPatch carries the per-field diff applied by UpdateHead. Nil
// pointer fields mean "no change"; non-nil means "replace wholesale
// with this value" (including the empty slice = "remove all").
//
// The pointer-vs-zero discipline is deliberate: a zero-valued
// memory.Visibility (== 0) is not a valid Visibility, so we can't use
// it as a sentinel; pointers make the "unset" case explicit at every
// call site.
type HeadPatch struct {
	// Tags, when non-nil, replaces Head.Tags. Set to a non-nil empty
	// slice (`&[]memory.Tag{}`) to remove all tags.
	Tags *[]memory.Tag

	// Frames, when non-nil, replaces Head.Frames.
	Frames *[]memory.FrameRef

	// DeclaredImportance, when non-nil, replaces
	// Head.DeclaredImportance. Range 0..10 per memory.ValidateMemory.
	DeclaredImportance *uint8

	// Visibility, when non-nil, replaces Head.Visibility. Must be one
	// of memory.{VisPrivate, VisScoped, VisActorPublic}.
	Visibility *memory.Visibility
}

// IsEmpty reports whether the patch would change anything.
func (p *HeadPatch) IsEmpty() bool {
	if p == nil {
		return true
	}
	return p.Tags == nil && p.Frames == nil && p.DeclaredImportance == nil && p.Visibility == nil
}

// UpdateHeadMeta carries provenance for the journal entry. Mirrors
// the WriteMeta / Tombstone pattern: who is doing this, optionally
// the scope under which they're authorized.
type UpdateHeadMeta struct {
	// CreatedBy is recorded on the journal entry's CreatedBy field.
	CreatedBy string

	// Scope, when non-nil, is verified once at entry; if present and
	// not Writable, the call returns scope.ErrNotWritable. If present
	// and the current Head is outside scope.Allows, returns
	// scope.ErrViolation. Both failures journal a KindScopeViolation
	// entry (severity:high per research/06 §7.2).
	Scope *scope.Scope
}

// ErrNoOp is returned when UpdateHead is called with a HeadPatch that
// contains no actual changes (all pointer fields nil). Callers
// usually want to short-circuit on this rather than journal a no-op
// audit row.
var ErrNoOp = errors.New("cortex.UpdateHead: patch is empty")

// UpdateHead rewrites mutable fields on an existing Head without
// bumping the Data version. Returns the unchanged URI on success
// (matrix://cortex/<type>/<id>#<n> where n is the same Data version
// as before).
//
// The URI passed in MAY include any version (the cortex looks up the
// memory by ID); the version on the URI is NOT enforced to match the
// CurrentVersion. This lets callers reuse a versioned URI from a
// prior compile-time pin without first re-resolving #latest.
func (c *Cortex) UpdateHead(uri memory.URI, patch HeadPatch, meta UpdateHeadMeta) (memory.URI, error) {
	if patch.IsEmpty() {
		return "", ErrNoOp
	}

	_, id, _, err := ParseURI(uri)
	if err != nil {
		return "", err
	}

	// Phase 10 scope verification ONCE up-front.
	if meta.Scope != nil {
		if err := c.VerifyScope(meta.Scope, c.now()); err != nil {
			return "", fmt.Errorf("cortex.UpdateHead: %w", err)
		}
	}

	// Load the current Head + Version. We need the existing Head to
	// compute the idx-key diff, and we need Version to write a fresh
	// salience.Cached value (mirrors cortex.Update: bump LastUsed).
	prev, err := c.ResolveLatest(id)
	if err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: load: %w", err)
	}
	if prev.Head.Tombstoned != nil {
		return "", memory.ErrTombstoned
	}

	// Per-target scope enforcement (write).
	if meta.Scope != nil {
		if err := c.enforceWrite(meta.Scope, &prev.Head); err != nil {
			return "", fmt.Errorf("cortex.UpdateHead: %w", err)
		}
	}

	// Build the new Head by applying the patch onto a copy of prev.
	newHead := prev.Head
	if patch.Tags != nil {
		newHead.Tags = append([]memory.Tag(nil), (*patch.Tags)...)
	}
	if patch.Frames != nil {
		newHead.Frames = append([]memory.FrameRef(nil), (*patch.Frames)...)
	}
	if patch.DeclaredImportance != nil {
		newHead.DeclaredImportance = *patch.DeclaredImportance
	}
	if patch.Visibility != nil {
		newHead.Visibility = *patch.Visibility
	}

	now := c.now()
	newHead.LastUpdatedAt = now

	// Validate the new Head against the existing Version + Data.
	// Validation needs the Data, so decode it once.
	prevData, err := memory.DecodeData(prev.Version.Type, prev.Version.Data)
	if err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: decode existing data: %w", err)
	}
	if err := memory.ValidateMemory(&newHead, &prev.Version, prevData); err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: validate: %w", err)
	}

	newHeadBytes, err := memory.EncodeHead(&newHead)
	if err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: encode head: %w", err)
	}
	headHash := sha256.Sum256(newHeadBytes)

	// Compute idx diffs.
	addedTags, removedTags := diffTags(prev.Head.Tags, newHead.Tags)
	addedFrames, removedFrames := diffFrames(prev.Head.Frames, newHead.Frames)

	// Build the atomic batch.
	wb := c.s.BeginWrite()
	defer wb.Abort()

	// 1. Rewrite m/<id>.
	if err := wb.Set(keys.MemoryHeadKey(toKeysULID(id)), newHeadBytes); err != nil {
		return "", err
	}

	// 2. idx/tag deletions: scan to recover the original created
	// component, then delete the matching key by id-suffix.
	for _, t := range removedTags {
		k, err := c.findIdxTagKey(t, id)
		if err != nil {
			return "", fmt.Errorf("cortex.UpdateHead: find idx/tag for delete: %w", err)
		}
		if k != nil {
			if err := wb.Delete(k); err != nil {
				return "", err
			}
		}
	}
	// 3. idx/tag additions: emit at the current `now` (no original
	// time to preserve since these are new-as-of-this-UpdateHead).
	for _, t := range addedTags {
		if err := wb.Set(
			keys.IdxTagKey(hashTag(string(t)), uint64(now.UnixNano()), toKeysULID(id)),
			nil,
		); err != nil {
			return "", err
		}
	}

	// 4. idx/frame deletions (direct — no created component to
	// recover) + idx/actor_obj deletions (scan needed).
	for _, fr := range removedFrames {
		if err := wb.Delete(
			keys.IdxFrameKey(byte(fr.Verb), byte(fr.ObjKind), fr.Hash(), toKeysULID(id)),
		); err != nil {
			return "", err
		}
		if newHead.Type == memory.TypeEvent {
			k, err := c.findIdxActorObjKey(fr.Verb, fr.Hash(), id)
			if err != nil {
				return "", fmt.Errorf("cortex.UpdateHead: find idx/actor_obj for delete: %w", err)
			}
			if k != nil {
				if err := wb.Delete(k); err != nil {
					return "", err
				}
			}
		}
	}

	// 5. idx/frame + idx/actor_obj additions for newly-added frames.
	for _, fr := range addedFrames {
		objHash := fr.Hash()
		if err := wb.Set(
			keys.IdxFrameKey(byte(fr.Verb), byte(fr.ObjKind), objHash, toKeysULID(id)),
			nil,
		); err != nil {
			return "", err
		}
		if newHead.Type == memory.TypeEvent {
			if err := wb.Set(
				keys.IdxActorObjKey(byte(fr.Verb), objHash, uint64(now.UnixNano()), toKeysULID(id)),
				nil,
			); err != nil {
				return "", err
			}
		}
	}

	// 6. salience: BumpForUpdate (mirrors cortex.Update). LastUsed
	// advances; cached score recomputes; factor inputs preserved.
	scorePtr, ok, err := salience.Read(c.s, id)
	if err != nil {
		return "", err
	}
	var sc salience.Score
	if ok {
		sc = *scorePtr
		salience.BumpForUpdate(&sc, newHead.DeclaredImportance, now)
	} else {
		sc = salience.NewForWrite(newHead.DeclaredImportance, now)
	}
	scoreBytes, err := salience.Encode(&sc)
	if err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: encode salience: %w", err)
	}
	if err := wb.Set(keys.SalienceKey(toKeysULID(id)), scoreBytes); err != nil {
		return "", err
	}

	// 7. KindUpdateHead journal entry.
	payload := &journal.UpdateHeadPayload{
		SchemaVersion: 1,
		ID:            [16]byte(id),
		Version:       prev.Head.CurrentVersion,
		HeadHash:      headHash,
	}
	enc, err := journal.EncodeUpdateHeadPayload(payload)
	if err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindUpdateHead,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(meta.CreatedBy),
		Payload:   enc,
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", err
	}

	// 8. Phase 7 SMT stage with the new Head bytes.
	if err := c.snap.StageMemoryUpdate(wb, id, newHeadBytes); err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: stage SMT: %w", err)
	}

	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.UpdateHead: commit: %w", err)
	}

	return BuildURI(newHead.Type, id, prev.Head.CurrentVersion), nil
}

// diffTags returns (added, removed): tags in `next` but not in `prev`,
// and tags in `prev` but not in `next`. Order in the input slices is
// preserved in the output (preserves caller intent for the rare case
// of duplicates).
func diffTags(prev, next []memory.Tag) (added, removed []memory.Tag) {
	prevSet := make(map[memory.Tag]struct{}, len(prev))
	for _, t := range prev {
		prevSet[t] = struct{}{}
	}
	nextSet := make(map[memory.Tag]struct{}, len(next))
	for _, t := range next {
		nextSet[t] = struct{}{}
	}
	for _, t := range next {
		if _, ok := prevSet[t]; !ok {
			added = append(added, t)
		}
	}
	for _, t := range prev {
		if _, ok := nextSet[t]; !ok {
			removed = append(removed, t)
		}
	}
	return
}

// diffFrames returns (added, removed) over FrameRef equality. Two
// FrameRefs are equal iff (Verb, ObjKind, ObjRef) all match — this
// matches the canonical CBOR equality on Head.Frames.
func diffFrames(prev, next []memory.FrameRef) (added, removed []memory.FrameRef) {
	key := func(fr memory.FrameRef) string {
		return fmt.Sprintf("%d|%d|%s", fr.Verb, fr.ObjKind, fr.ObjRef)
	}
	prevSet := make(map[string]struct{}, len(prev))
	for _, fr := range prev {
		prevSet[key(fr)] = struct{}{}
	}
	nextSet := make(map[string]struct{}, len(next))
	for _, fr := range next {
		nextSet[key(fr)] = struct{}{}
	}
	for _, fr := range next {
		if _, ok := prevSet[key(fr)]; !ok {
			added = append(added, fr)
		}
	}
	for _, fr := range prev {
		if _, ok := nextSet[key(fr)]; !ok {
			removed = append(removed, fr)
		}
	}
	return
}

// findIdxTagKey scans idx/tag/<hash(t)>/* for the key whose id-suffix
// matches mid. Returns nil (no error) if no key found — the prior
// Head's tags MAY include a tag whose idx entry was already
// hard-deleted by an earlier UpdateHead, so missing-on-delete is not
// fatal.
func (c *Cortex) findIdxTagKey(t memory.Tag, mid memory.ID) ([]byte, error) {
	hash := hashTag(string(t))
	prefix := keys.IdxTagPrefix(hash)
	var found []byte
	suffix := id16(mid)
	err := c.s.PrefixIter(prefix, func(k, _ []byte) error {
		if len(k) < 16 {
			return nil
		}
		if bytes.Equal(k[len(k)-16:], suffix[:]) {
			found = append([]byte(nil), k...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// findIdxActorObjKey scans idx/actor_obj/<verb>/<obj_hash>/* for the
// key whose id-suffix matches mid. Same missing-on-delete tolerance
// as findIdxTagKey.
func (c *Cortex) findIdxActorObjKey(verb memory.Verb, objHash [memory.ObjHashSize]byte, mid memory.ID) ([]byte, error) {
	prefix := keys.IdxActorObjPrefixVerbObj(byte(verb), objHash)
	var found []byte
	suffix := id16(mid)
	err := c.s.PrefixIter(prefix, func(k, _ []byte) error {
		if len(k) < 16 {
			return nil
		}
		if bytes.Equal(k[len(k)-16:], suffix[:]) {
			found = append([]byte(nil), k...)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return found, nil
}

// id16 returns mid as a [16]byte. Local helper to avoid an extra
// memory.ID-to-[16]byte conversion at every call site.
func id16(mid memory.ID) [16]byte {
	var out [16]byte
	copy(out[:], mid[:])
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
