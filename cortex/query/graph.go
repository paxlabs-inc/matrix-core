// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Graph traversal for typed Find queries (Phase 6).
//
// Spec: research/04-cortex.md §5 (edge taxonomy) and §12.2 (Query.From /
// Query.Follow). When Query.From is set we run a bounded BFS from the
// resolved entry vertex along edges matching Query.Follow, returning a
// per-ID hop count. Where / Type / IncludeTombstoned filters apply
// post-BFS in Run; this file is concerned only with graph reachability.
//
// Determinism: BFS visits neighbours in byte-ascending (edge_type, dst-or-
// src) order — the same order Pebble surfaces them — so results are
// reproducible across runs given the same store state. Cycles terminate
// because every visited ID is marked in `seen` before its neighbours
// expand. The MaxHops cap (default 1, hard ceiling MaxHopsCap=6) is the
// other backstop.

package query

import (
	"errors"
	"fmt"
	"strings"

	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/store"
)

// validateEdgeExpr checks the expression's bounds and edge types. Called
// from Run after From is asserted non-nil; nil EdgeExpr means "default
// expression: 1 hop out, any edge type" and is valid.
func validateEdgeExpr(e *EdgeExpr) error {
	if e == nil {
		return nil
	}
	for _, t := range e.Types {
		if !t.Valid() {
			return fmt.Errorf("query: invalid edge type %d in Follow.Types", t)
		}
	}
	if e.MinHops < 0 {
		return errors.New("query: Follow.MinHops must be >= 0")
	}
	if e.MaxHops < 0 {
		return errors.New("query: Follow.MaxHops must be >= 0")
	}
	if e.MaxHops > MaxHopsCap {
		return fmt.Errorf("query: Follow.MaxHops %d exceeds MaxHopsCap %d", e.MaxHops, MaxHopsCap)
	}
	if e.MinHops > 0 && e.MaxHops > 0 && e.MinHops > e.MaxHops {
		return fmt.Errorf("query: Follow.MinHops %d > MaxHops %d", e.MinHops, e.MaxHops)
	}
	switch e.Direction {
	case "", DirOut, DirIn, DirBoth:
		// ok; "" is the default → DirOut
	default:
		return fmt.Errorf("query: invalid Follow.Direction %q", e.Direction)
	}
	return nil
}

// resolvedEdgeExpr returns the EdgeExpr with all defaults filled in.
func resolvedEdgeExpr(e *EdgeExpr) EdgeExpr {
	if e == nil {
		return EdgeExpr{MinHops: 1, MaxHops: 1, Direction: DirOut}
	}
	out := *e
	if out.MinHops <= 0 {
		out.MinHops = 1
	}
	if out.MaxHops <= 0 {
		out.MaxHops = 1
	}
	if out.Direction == "" {
		out.Direction = DirOut
	}
	return out
}

// parseFromURI extracts the memory ID from Query.From. We accept the
// canonical matrix://cortex/<Type>/<id>#<version> form because that's
// what the rest of cortex emits; the Type and Version are advisory in
// this context (BFS only needs the ID). Reusing the cortex package's
// ParseURI would create an import cycle (cortex imports query), so we
// re-implement a tolerant variant inline.
func parseFromURI(uri memory.URI) (memory.ID, error) {
	const prefix = "matrix://cortex/"
	s := string(uri)
	if !strings.HasPrefix(s, prefix) {
		return memory.ID{}, fmt.Errorf("%w: From: missing %q prefix", memory.ErrBadURI, prefix)
	}
	rest := s[len(prefix):]
	hash := strings.IndexByte(rest, '#')
	if hash < 0 {
		return memory.ID{}, fmt.Errorf("%w: From: missing #version", memory.ErrBadURI)
	}
	pathPart := rest[:hash]
	slash := strings.IndexByte(pathPart, '/')
	if slash < 0 {
		return memory.ID{}, fmt.Errorf("%w: From: missing /id", memory.ErrBadURI)
	}
	idStr := pathPart[slash+1:]
	id, err := memory.ParseID(idStr)
	if err != nil {
		return memory.ID{}, fmt.Errorf("%w: From: bad ulid %q: %v", memory.ErrBadURI, idStr, err)
	}
	return id, nil
}

// planCandidatesGraph runs a hop-bounded BFS from Query.From and returns
// the deduped reachable IDs (excluding the From vertex itself), the
// number of edge records scanned, and a per-ID hop map.
//
// The visited set is keyed by memory ID; once an ID is popped at hop h,
// its hop count is fixed at h (BFS gives shortest hop in unweighted
// graphs by construction). Edges with Tombstoned=true are skipped
// unless Follow.IncludeTombstoned=true. Edges of types not in
// Follow.Types are skipped (empty Types means "any of the 14").
//
// Direction:
//   - DirOut  → walk e/from/<src>/...
//   - DirIn   → walk e/to/<dst>/...
//   - DirBoth → both, deduplicating by neighbour ID before adding to the
//     queue (a neighbour reachable both directions is one neighbour).
func planCandidatesGraph(s *store.Store, q Query) ([]memory.ID, int, map[memory.ID]int, error) {
	if q.From == nil {
		return nil, 0, nil, errors.New("query: planCandidatesGraph called without From")
	}
	startID, err := parseFromURI(*q.From)
	if err != nil {
		return nil, 0, nil, err
	}
	expr := resolvedEdgeExpr(q.Follow)
	allowedTypes := edgeTypeSet(expr.Types)

	// hops[id] = shortest hop distance from startID; the start vertex is
	// at hop 0 but is excluded from the candidate list (callers want
	// "neighbours of From", not From itself).
	hops := map[memory.ID]int{startID: 0}
	queue := []memory.ID{startID}
	scanned := 0

	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		curHop := hops[current]
		if curHop >= expr.MaxHops {
			continue
		}

		// Collect neighbours by direction. For DirBoth we union; the
		// hop distance is identical so the order doesn't matter.
		neighbours := map[memory.ID]struct{}{}
		walk := func(outgoing bool) error {
			anchor := keys.ULID{}
			copy(anchor[:], current[:])
			var prefix []byte
			if outgoing {
				prefix = keys.EdgeFromPrefix(anchor)
			} else {
				prefix = keys.EdgeToPrefix(anchor)
			}
			return s.PrefixIter(prefix, func(_, value []byte) error {
				scanned++
				var rec memory.EdgeRecord
				if err := memory.DecodeEdge(value, &rec); err != nil {
					return fmt.Errorf("query: decode edge: %w", err)
				}
				if !expr.IncludeTombstoned && rec.Tombstoned {
					return nil
				}
				if allowedTypes != nil {
					if _, ok := allowedTypes[rec.Type]; !ok {
						return nil
					}
				}
				var nb memory.ID
				if outgoing {
					nb = rec.Dst
				} else {
					nb = rec.Src
				}
				neighbours[nb] = struct{}{}
				return nil
			})
		}

		switch expr.Direction {
		case DirOut:
			if err := walk(true); err != nil {
				return nil, scanned, nil, err
			}
		case DirIn:
			if err := walk(false); err != nil {
				return nil, scanned, nil, err
			}
		case DirBoth:
			if err := walk(true); err != nil {
				return nil, scanned, nil, err
			}
			if err := walk(false); err != nil {
				return nil, scanned, nil, err
			}
		}

		nextHop := curHop + 1
		for nb := range neighbours {
			if _, seen := hops[nb]; seen {
				continue
			}
			hops[nb] = nextHop
			queue = append(queue, nb)
		}
	}

	// Build the candidate list: every visited ID at hop ∈ [MinHops,
	// MaxHops], excluding the start vertex. Order doesn't matter here —
	// orderResults handles final ordering — but we sort by ULID for
	// deterministic CandidatesScanned-vs-list correspondence in tests.
	out := make([]memory.ID, 0, len(hops))
	hopOut := make(map[memory.ID]int, len(hops))
	for id, h := range hops {
		if id == startID {
			continue
		}
		if h < expr.MinHops || h > expr.MaxHops {
			continue
		}
		out = append(out, id)
		hopOut[id] = h
	}
	sortByULID(out)
	return out, scanned, hopOut, nil
}

// sortByULID puts ids in byte-ascending ULID order (= creation-time order
// since ULID encodes ms timestamp in its high bytes).
func sortByULID(ids []memory.ID) {
	// Simple sort.Slice would import sort; this file already uses
	// fmt/strings — keeping the import surface tight by writing a tiny
	// insertion sort. Phase 6 BFS sets are bounded by reachable
	// neighbours over <= 6 hops; usually tens, not thousands.
	for i := 1; i < len(ids); i++ {
		j := i
		for j > 0 && lessULID(ids[j], ids[j-1]) {
			ids[j], ids[j-1] = ids[j-1], ids[j]
			j--
		}
	}
}

func lessULID(a, b memory.ID) bool {
	for k := 0; k < len(a); k++ {
		if a[k] != b[k] {
			return a[k] < b[k]
		}
	}
	return false
}

// edgeTypeSet returns nil if types is empty (meaning "any"); otherwise a
// set keyed by edge type byte. Mirrors cortex.edgeTypeSet but lives in
// this package so query/graph.go has zero dep on the cortex top-level.
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

// Copyright © 2026 Paxlabs Inc. All rights reserved.
