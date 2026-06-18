// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package transport

import (
	"encoding/json"
	"fmt"

	"matrix/construct/schema"
	"matrix/construct/schema/primitives"
)

// ApplyPatch folds a construct.surface.patch increment onto an existing
// surface, returning a NEW surface (the base is never mutated). It is the
// canonical progressive-merge definition; the client renderer mirrors it so
// the live tap and replay converge to the same state. Semantics by kind:
//
//   - Stream:   APPEND the patch chunks (dedup by chunk seq -> idempotent on
//     replay); Closed latches true; non-empty Source/Title overwrite.
//   - Timeline: UPSERT the patch steps by step id (status/detail/ref update,
//     new ids append); non-empty Title overwrites.
//   - all other kinds: REPLACE the payload wholesale (full re-render). This is
//     how an answered Ask attaches its Response, or a Metric updates its value.
//
// Envelope decoration carried on the patch (ref/seq/parent/attributes) is
// applied when present. The patch must match the base kind.
func ApplyPatch(base, patch *schema.Surface) (*schema.Surface, error) {
	if base == nil || patch == nil {
		return nil, fmt.Errorf("construct/transport: nil surface in patch")
	}
	if base.Kind != patch.Kind {
		return nil, fmt.Errorf("construct/transport: patch kind %q does not match base kind %q", patch.Kind, base.Kind)
	}
	out, err := clone(base)
	if err != nil {
		return nil, err
	}

	// Envelope decoration: apply only what the patch supplies.
	if patch.Ref != "" {
		out.Ref = patch.Ref
	}
	if patch.Seq != 0 {
		out.Seq = patch.Seq
	}
	if patch.Parent != "" {
		out.Parent = patch.Parent
	}
	if patch.Attributes != nil {
		out.Attributes = patch.Attributes
	}

	switch base.Kind {
	case schema.KindStream:
		mergeStream(out.Stream, patch.Stream)
	case schema.KindTimeline:
		mergeTimeline(out.Timeline, patch.Timeline)
	default:
		// Full-replace kinds: copy the patch payload pointer for the active
		// kind (the others are nil by construction of a valid surface).
		out.Narration = patch.Narration
		out.Metric = patch.Metric
		out.Entity = patch.Entity
		out.Structure = patch.Structure
		out.Canvas = patch.Canvas
		out.Ask = patch.Ask
	}
	return out, nil
}

// mergeStream appends patch chunks to base, deduping by chunk seq so a
// re-emitted patch (replay) is idempotent. base is non-nil (valid Stream
// surface); patch may carry a nil Stream (no-op envelope-only patch).
func mergeStream(base, patch *primitives.Stream) {
	if patch == nil {
		return
	}
	if patch.Source != "" {
		base.Source = patch.Source
	}
	if patch.Title != "" {
		base.Title = patch.Title
	}
	seen := make(map[uint64]bool, len(base.Chunks))
	for _, c := range base.Chunks {
		seen[c.Seq] = true
	}
	for _, c := range patch.Chunks {
		if seen[c.Seq] {
			continue
		}
		seen[c.Seq] = true
		base.Chunks = append(base.Chunks, c)
	}
	if patch.Closed {
		base.Closed = true
	}
}

// mergeTimeline upserts patch steps into base by step id (idempotent), and
// overwrites a non-empty title.
func mergeTimeline(base, patch *primitives.Timeline) {
	if patch == nil {
		return
	}
	if patch.Title != "" {
		base.Title = patch.Title
	}
	idx := make(map[string]int, len(base.Steps))
	for i, s := range base.Steps {
		idx[s.ID] = i
	}
	for _, s := range patch.Steps {
		if i, ok := idx[s.ID]; ok {
			base.Steps[i] = s
			continue
		}
		idx[s.ID] = len(base.Steps)
		base.Steps = append(base.Steps, s)
	}
}

// clone deep-copies a surface via a JSON round-trip so ApplyPatch never
// mutates its input.
func clone(s *schema.Surface) (*schema.Surface, error) {
	b, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	return schema.Unmarshal(b)
}
