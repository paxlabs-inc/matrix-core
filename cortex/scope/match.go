// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package scope

import (
	"matrix/cortex/memory"
)

// Matches returns true if the candidate Head satisfies AT LEAST ONE
// criterion populated on sel. Empty selectors NEVER match anything —
// callers must populate at least one of {Types, Tags, IDs, Frame} for
// a Selector to be useful.
//
// Set-union semantics across populated criteria; per-criterion
// semantics:
//
//   - Types: head.Type ∈ sel.Types
//   - Tags:  any tag in head.Tags appears in sel.Tags (case-sensitive)
//   - IDs:   head.ID ∈ sel.IDs
//   - Frame: any FrameRef on head.Frames matches sel.Frame.Verb AND
//     its 16-byte ObjHash is in sel.Frame.ObjHashes
//
// Empty-selector returns false. For include semantics this manifests
// as "no access"; for exclude semantics as "no extra deny".
func (sel *Selector) Matches(h *memory.Head) bool {
	if sel == nil || h == nil {
		return false
	}
	if sel.IsEmpty() {
		return false
	}
	for _, t := range sel.Types {
		if h.Type == t {
			return true
		}
	}
	for _, id := range sel.IDs {
		if h.ID == id {
			return true
		}
	}
	for _, st := range sel.Tags {
		for _, ht := range h.Tags {
			if st == ht {
				return true
			}
		}
	}
	if sel.Frame != nil {
		for _, fr := range h.Frames {
			if fr.Verb != sel.Frame.Verb {
				continue
			}
			fh := fr.Hash()
			for _, oh := range sel.Frame.ObjHashes {
				if oh == fh {
					return true
				}
			}
		}
	}
	return false
}

// IsEmpty reports whether the selector has no populated criterion.
func (sel *Selector) IsEmpty() bool {
	if sel == nil {
		return true
	}
	if len(sel.Types) > 0 || len(sel.Tags) > 0 || len(sel.IDs) > 0 {
		return false
	}
	if sel.Frame != nil && (sel.Frame.Verb != 0 || len(sel.Frame.ObjHashes) > 0) {
		return false
	}
	return true
}

// Allows returns true if h is permitted under s — included by Include
// AND not excluded by Exclude. Pure function; the caller is
// responsible for having already verified the scope's signature,
// expiry, snapshot resolvability, and multi-proof.
//
// Tombstoned memories are NOT special-cased here; the cortex read path
// filters them out before consulting Allows (default IncludeTombstoned
// false). If a future caller wants tombstoned memories it must opt in
// at the query layer; scope semantics are orthogonal to lifecycle.
func (s *Scope) Allows(h *memory.Head) bool {
	if s == nil || h == nil {
		return false
	}
	if !s.Include.Matches(h) {
		return false
	}
	if !s.Exclude.IsEmpty() && s.Exclude.Matches(h) {
		return false
	}
	return true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
