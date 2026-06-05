// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Edge writes for the Matrix cortex (Phase 6).
//
// Spec: research/04-cortex.md §5 (taxonomy, EdgeRecord shape) and §11.1
// (atomic batch — edge keys + journal entry commit-or-abort together).
//
// Every AddEdge writes both directions in one atomic Pebble batch:
//
//	e/from/<src:16>/<t:1>/<dst:16> -> canonical CBOR EdgeRecord
//	e/to/<dst:16>/<t:1>/<src:16>   -> SAME canonical bytes
//	j/<seq>                         -> KindAddEdge journal entry
//	meta/journal_head               -> seq+1
//
// Idempotency: AddEdge on an existing live (src,type,dst) is a no-op
// (returns nil without journaling). Re-adding a tombstoned edge un-
// tombstones it (rewrites with Tombstoned=false); this is the only way
// to "revive" a removed edge so replay reproduces the live state.
//
// Removal: RemoveEdge rewrites both keys with Tombstoned=true rather
// than deleting them. The audit trail lives in two places — the e/
// record itself and the KindRemoveEdge journal entry. Default traversal
// (query.EdgeExpr) skips tombstoned edges.

package cortex

import (
	"bytes"
	"errors"
	"fmt"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
)

// AddEdgeMeta carries the per-edge metadata supplied to AddEdge.
type AddEdgeMeta struct {
	CreatedBy string
	Weight    float32
	// Data is opaque CBOR carried verbatim in EdgeRecord.Data. Reserved
	// for edge-type-specific payloads (e.g. a `contradicts` edge naming
	// the conflicting fields). Empty for the default Phase 6 callers.
	Data []byte
}

// AddEdge inserts a typed edge from src to dst. Atomic per §11.1: both
// directional keys plus a KindAddEdge journal entry commit together.
//
// Idempotency rule: if a live edge already exists at (t, src, dst),
// AddEdge returns nil without journaling. If a tombstoned edge exists,
// it is REVIVED (rewritten Tombstoned=false) — this is intentional so
// replay of an out-of-order remove+add sequence reproduces the live
// state byte-identically.
func (c *Cortex) AddEdge(src memory.ID, t memory.EdgeType, dst memory.ID, meta AddEdgeMeta) error {
	if !t.Valid() {
		return memory.ErrInvalidEdgeType
	}
	if src == dst {
		return memory.ErrSelfEdge
	}
	if src.IsZero() || dst.IsZero() {
		return errors.New("cortex.AddEdge: zero memory ID")
	}

	srcU := toKeysULID(src)
	dstU := toKeysULID(dst)
	fromKey := keys.EdgeFromKey(srcU, byte(t), dstU)
	toKey := keys.EdgeToKey(dstU, byte(t), srcU)

	// Existence check: if a live edge is already there, no-op.
	existing, ok, err := c.s.Get(fromKey)
	if err != nil {
		return fmt.Errorf("cortex.AddEdge: get existing: %w", err)
	}
	if ok {
		var prev memory.EdgeRecord
		if err := memory.DecodeEdge(existing, &prev); err != nil {
			return fmt.Errorf("cortex.AddEdge: decode existing: %w", err)
		}
		if !prev.Tombstoned {
			return nil // already live, nothing to do
		}
		// Tombstoned → revive below.
	}

	now := c.now()
	rec := memory.EdgeRecord{
		Type:      t,
		Src:       src,
		Dst:       dst,
		CreatedAt: now,
		CreatedBy: meta.CreatedBy,
		Weight:    meta.Weight,
		Data:      meta.Data,
	}
	enc, err := memory.EncodeEdge(&rec)
	if err != nil {
		return fmt.Errorf("cortex.AddEdge: encode: %w", err)
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()

	if err := wb.Set(fromKey, enc); err != nil {
		return err
	}
	if err := wb.Set(toKey, enc); err != nil {
		return err
	}
	jp := &journal.EdgePayload{
		SchemaVersion: 1,
		Type:          uint8(t),
		Src:           src,
		Dst:           dst,
		Weight:        meta.Weight,
	}
	jpBytes, err := journal.EncodeEdgePayload(jp)
	if err != nil {
		return fmt.Errorf("cortex.AddEdge: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindAddEdge,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(meta.CreatedBy),
		Payload:   jpBytes,
	}
	if err := wb.AppendJournal(je); err != nil {
		return err
	}
	// Phase 7: stage edges-namespace SMT update with the new EdgeRecord
	// canonical bytes. Forward direction only — reverse e/to is byte-
	// identical and would double-anchor the same fact.
	if err := c.snap.StageEdgeUpdate(wb, src, byte(t), dst, enc); err != nil {
		return fmt.Errorf("cortex.AddEdge: stage SMT: %w", err)
	}
	return wb.Commit()
}

// RemoveEdge soft-deletes the edge at (src, t, dst). Both directional
// records are rewritten with Tombstoned=true; the keys remain in the
// store so traversal callers that opt in via EdgeExpr.IncludeTombstoned
// can audit historical adjacency.
//
// Idempotency rule: missing edge → nil (no journal); already-tombstoned
// edge → nil (no journal). This matches the Tombstone(memory) contract
// in cortex.go.
func (c *Cortex) RemoveEdge(src memory.ID, t memory.EdgeType, dst memory.ID, reason, by string) error {
	if !t.Valid() {
		return memory.ErrInvalidEdgeType
	}
	srcU := toKeysULID(src)
	dstU := toKeysULID(dst)
	fromKey := keys.EdgeFromKey(srcU, byte(t), dstU)
	toKey := keys.EdgeToKey(dstU, byte(t), srcU)

	existing, ok, err := c.s.Get(fromKey)
	if err != nil {
		return fmt.Errorf("cortex.RemoveEdge: get: %w", err)
	}
	if !ok {
		return nil // never existed
	}
	var rec memory.EdgeRecord
	if err := memory.DecodeEdge(existing, &rec); err != nil {
		return fmt.Errorf("cortex.RemoveEdge: decode: %w", err)
	}
	if rec.Tombstoned {
		return nil // already removed
	}

	now := c.now()
	rec.Tombstoned = true
	rec.TombstonedAt = &now
	rec.TombstonedReason = reason
	rec.TombstonedBy = by

	enc, err := memory.EncodeEdge(&rec)
	if err != nil {
		return fmt.Errorf("cortex.RemoveEdge: encode: %w", err)
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(fromKey, enc); err != nil {
		return err
	}
	if err := wb.Set(toKey, enc); err != nil {
		return err
	}
	jp := &journal.EdgePayload{
		SchemaVersion: 1,
		Type:          uint8(t),
		Src:           src,
		Dst:           dst,
		Weight:        rec.Weight,
		Tombstoned:    true,
		Reason:        reason,
		By:            by,
	}
	jpBytes, err := journal.EncodeEdgePayload(jp)
	if err != nil {
		return fmt.Errorf("cortex.RemoveEdge: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindRemoveEdge,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(by),
		Payload:   jpBytes,
	}
	if err := wb.AppendJournal(je); err != nil {
		return err
	}
	// Phase 7: stage edges SMT update — EdgeRecord now carries
	// Tombstoned=true; the value-hash changes and the snapshot edges
	// root reflects the soft-delete.
	if err := c.snap.StageEdgeUpdate(wb, src, byte(t), dst, enc); err != nil {
		return fmt.Errorf("cortex.RemoveEdge: stage SMT: %w", err)
	}
	return wb.Commit()
}

// GetEdge returns the EdgeRecord at (src, t, dst). Returns memory.ErrNotFound
// when the edge has never existed. Tombstoned edges ARE returned (callers
// inspect rec.Tombstoned); use this for audit reads.
func (c *Cortex) GetEdge(src memory.ID, t memory.EdgeType, dst memory.ID) (*memory.EdgeRecord, error) {
	if !t.Valid() {
		return nil, memory.ErrInvalidEdgeType
	}
	srcU := toKeysULID(src)
	dstU := toKeysULID(dst)
	raw, ok, err := c.s.Get(keys.EdgeFromKey(srcU, byte(t), dstU))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, memory.ErrNotFound
	}
	var rec memory.EdgeRecord
	if err := memory.DecodeEdge(raw, &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// IterEdgesOptions tunes IterEdgesOut / IterEdgesIn.
type IterEdgesOptions struct {
	// Types restricts the scan to the listed edge types. Empty means all.
	// When exactly one type is provided, the scan uses the per-type
	// prefix (e/from/<src>/<t>) for tighter selectivity.
	Types []memory.EdgeType
	// IncludeTombstoned, when true, surfaces edges with Tombstoned=true.
	// Default false (live edges only).
	IncludeTombstoned bool
}

// IterEdgesOut walks outgoing edges from src in (edge_type, dst) byte
// order. The visitor returns a stop sentinel by returning a non-nil
// error; that error is propagated unless it equals errStopIter, which
// is treated as "stop, success".
func (c *Cortex) IterEdgesOut(src memory.ID, opts IterEdgesOptions, fn func(*memory.EdgeRecord) error) error {
	srcU := toKeysULID(src)
	return c.iterEdges(opts, fn, true, srcU)
}

// IterEdgesIn walks incoming edges into dst in (edge_type, src) byte order.
func (c *Cortex) IterEdgesIn(dst memory.ID, opts IterEdgesOptions, fn func(*memory.EdgeRecord) error) error {
	dstU := toKeysULID(dst)
	return c.iterEdges(opts, fn, false, dstU)
}

func (c *Cortex) iterEdges(opts IterEdgesOptions, fn func(*memory.EdgeRecord) error, outgoing bool, anchor keys.ULID) error {
	// Decide the prefix(es) to scan. With one type filter we use the
	// tighter per-type prefix; otherwise the full anchor prefix and a
	// type post-filter (cheap — just a byte check on the decoded record).
	var prefixes [][]byte
	switch {
	case len(opts.Types) == 1:
		t := opts.Types[0]
		if !t.Valid() {
			return memory.ErrInvalidEdgeType
		}
		if outgoing {
			prefixes = append(prefixes, keys.EdgeFromTypePrefix(anchor, byte(t)))
		} else {
			prefixes = append(prefixes, keys.EdgeToTypePrefix(anchor, byte(t)))
		}
	default:
		if outgoing {
			prefixes = append(prefixes, keys.EdgeFromPrefix(anchor))
		} else {
			prefixes = append(prefixes, keys.EdgeToPrefix(anchor))
		}
	}

	allowed := edgeTypeSet(opts.Types)
	for _, p := range prefixes {
		err := c.s.PrefixIter(p, func(_, value []byte) error {
			var rec memory.EdgeRecord
			if err := memory.DecodeEdge(value, &rec); err != nil {
				return fmt.Errorf("cortex.iterEdges: decode: %w", err)
			}
			if !opts.IncludeTombstoned && rec.Tombstoned {
				return nil
			}
			if allowed != nil {
				if _, ok := allowed[rec.Type]; !ok {
					return nil
				}
			}
			return fn(&rec)
		})
		if err != nil && !errors.Is(err, errStopIter) {
			return err
		}
		if errors.Is(err, errStopIter) {
			return nil
		}
	}
	return nil
}

// edgeTypeSet returns nil if the slice is empty (meaning "any type"); a
// set otherwise. Tiny helper so the iterator's hot loop is one map lookup
// rather than a linear scan.
func edgeTypeSet(types []memory.EdgeType) map[memory.EdgeType]struct{} {
	if len(types) == 0 {
		return nil
	}
	out := make(map[memory.EdgeType]struct{}, len(types))
	for _, t := range types {
		out[t] = struct{}{}
	}
	return out
}

// edgesEqualBytes compares two encoded edge records by raw bytes. Used in
// tests to assert forward/reverse parity. Defined here so test files can
// remain in the cortex_test package without re-exporting helpers.
func edgesEqualBytes(a, b []byte) bool { return bytes.Equal(a, b) }

// Copyright © 2026 Paxlabs Inc. All rights reserved.
