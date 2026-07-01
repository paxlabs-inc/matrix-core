// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 5.1: recursive recall — RLM (Recursive Language
// Models, Zhang/Kraska/Khattab, arXiv 2512.24601) applied to TIME (req.8).
//
// # What this is
//
// The pre-existing recall surface is a single FLAT lookup: cortex.Find scans
// one index and returns matches. RecallDescend upgrades that into a
// DECOMPOSABLE RECURSIVE DESCENT over the temporal ladder — the exact shape
// the design specifies (design.body.md §4):
//
//	list T0 windows  →  pick a window  →  resolve its member Refs  →  sub-query within
//
// Concretely, RecallDescend:
//
//  1. LISTS the T0 (epoch) rollups within the as_of horizon — the coarse
//     timeline (c.Rollups(TierEpoch, ...)).
//  2. PICKS windows in Salience-DESC order (deterministic; window Start ASC
//     tiebreak), descending them until the result cap or the per-descent
//     token budget is hit.
//  3. RESOLVES each picked rollup's member Refs one ladder level at a time —
//     epoch → day → hour (RefKindRollup members, resolved via LoadRollup) →
//     memory (RefKindMemory members, resolved via ResolveLatest). This is the
//     descent: T0 → T1 → finer → the exact T3 specific, paged in on demand
//     rather than resident in the Activate bundle.
//  4. SUB-QUERIES at the leaf: each reached memory is filtered by the type
//     set, the bi-temporal as_of validity window (preserved at EVERY level —
//     req.8.3), and (when a non-empty query is given) a case-insensitive
//     substring match against its rendered forms, then ranked by salience.
//
// # Bounds (req.8.2 — depth cap 4, per-descent budget, MaxHopsCap ceiling)
//
// The ladder has exactly four resolutions from the coarse timeline down to the
// exact memory: epoch (depth 1) → day (2) → hour (3) → memory leaf (4). So the
// default RecallDefaultMaxDepth = 4 reaches the leaves, mirroring T0→T3. A
// caller may lower it (a shallower descent that stops before the leaves) but
// NOT raise it past RecallMaxDepthCap = query.MaxHopsCap = 6 — the same hard
// ceiling the graph-traversal Follow uses (req.8.2). Each top-level window
// descent carries its own token budget (DescentBudgetTokens); a descent stops
// and marks the result Truncated once the rendered-token cost of the hits it
// has reached exceeds that budget.
//
// # Off the per-turn hot path (req.8.4)
//
// RecallDescend is NOT called by Activate. It is a tool-invoked, on-demand
// pull (the neo memory_recall tool, task 6.3) — the model descends the
// timeline when it needs an exact specific, never on every turn, so it can
// never threaten the Activate latency budget.
//
// # Replay-safety
//
// RecallDescend is a PURE READ: it performs no snap.StageMemoryUpdate /
// StageEdgeUpdate and appends nothing to the journal (unlike cortex.Find's
// LateBinding path, which this deliberately does NOT use — recall descent is a
// read composer like Context/RecentEpisodes). It therefore leaves every
// anchored root untouched.
package cortex

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/salience"
)

const (
	// RecallDefaultMaxDepth is the default descent depth: epoch(1) → day(2)
	// → hour(3) → memory leaf(4), i.e. T0 → T3 (req.8.2).
	RecallDefaultMaxDepth = 4
	// RecallMaxDepthCap is the hard ceiling on descent depth, reusing the
	// graph-traversal MaxHopsCap = 6 discipline (req.8.2). A RecallOpts
	// MaxDepth above this is rejected.
	RecallMaxDepthCap = query.MaxHopsCap
	// RecallDefaultK caps the number of leaf memory hits returned when the
	// caller leaves RecallOpts.K unset.
	RecallDefaultK = 16
	// RecallDefaultDescentBudgetTokens is the default per-descent token
	// budget: a single top-level window descent stops once the rendered
	// token cost of the memories it has reached exceeds this.
	RecallDefaultDescentBudgetTokens = 2048
)

// ErrRecallDepthTooDeep is returned when RecallOpts.MaxDepth exceeds
// RecallMaxDepthCap.
var ErrRecallDepthTooDeep = errors.New("cortex.RecallDescend: MaxDepth exceeds RecallMaxDepthCap")

// RecallOpts parameterizes a recursive-recall descent.
type RecallOpts struct {
	// Types, when non-empty, restricts leaf hits to these memory types
	// (mirrors query.Query.Type). Empty = any type.
	Types []memory.Type
	// K caps the number of leaf hits returned. Zero → RecallDefaultK.
	K int
	// AsOf is the bi-temporal valid-time the descent reads against, preserved
	// at every level (req.8.3). nil → now (c.now()). A past AsOf answers
	// "what was true then".
	AsOf *time.Time
	// MaxDepth caps the descent depth. Zero → RecallDefaultMaxDepth; a value
	// above RecallMaxDepthCap returns ErrRecallDepthTooDeep.
	MaxDepth int
	// DescentBudgetTokens is the per-top-level-window token budget. Zero →
	// RecallDefaultDescentBudgetTokens.
	DescentBudgetTokens int
}

// RecallHit is one leaf memory reached by the descent, with the provenance
// window that surfaced it and the depth at which it was reached.
type RecallHit struct {
	URI      memory.URI     // the memory URI (Kind == RefKindMemory)
	Memory   *memory.Memory // resolved head+version (as_of-validity-filtered)
	Salience float64        // recency-scored at the descent clock
	Window   Window         // provenance: the T0 window whose descent reached it
	Depth    int            // ladder depth the leaf was reached at (epoch=1..memory=4)
}

// RecallResult is the outcome of a recursive-recall descent.
type RecallResult struct {
	// Hits are the leaf memories reached, ranked Salience DESC then URI ASC,
	// capped at K.
	Hits []RecallHit
	// Windows lists the T0 (epoch) windows the descent visited, in the order
	// they were descended (provenance for "which parts of the timeline were
	// searched").
	Windows []Window
	// MaxDepthReached is the deepest ladder level any hit was reached at.
	MaxDepthReached int
	// Truncated is true when the depth cap or a per-descent token budget
	// forced the descent to stop before exhausting the timeline.
	Truncated bool
}

// RecallDescend runs the recursive-recall descent described in the file
// header (req.8.1–8.4). query is an optional narrowing phrase matched
// case-insensitively against each leaf memory's rendered forms; empty means
// "every reachable leaf under the bounds".
//
// It is a pure read: no journal append, no SMT write.
func (c *Cortex) RecallDescend(query string, opts RecallOpts) (*RecallResult, error) {
	maxDepth := opts.MaxDepth
	if maxDepth <= 0 {
		maxDepth = RecallDefaultMaxDepth
	}
	if maxDepth > RecallMaxDepthCap {
		return nil, fmt.Errorf("%w: %d > %d", ErrRecallDepthTooDeep, maxDepth, RecallMaxDepthCap)
	}
	k := opts.K
	if k <= 0 {
		k = RecallDefaultK
	}
	descentBudget := opts.DescentBudgetTokens
	if descentBudget <= 0 {
		descentBudget = RecallDefaultDescentBudgetTokens
	}

	now := c.now()
	asOf := now
	if opts.AsOf != nil {
		asOf = opts.AsOf.UTC()
	}

	weights, _, err := salience.ReadWeights(c.s)
	if err != nil {
		return nil, fmt.Errorf("cortex.RecallDescend: read weights: %w", err)
	}

	// --- Step 1: list T0 (epoch) windows in the as_of horizon ------------
	// The horizon is anchored to the same 90-day recency notion Activate's
	// Timeline uses, ending at asOf (so a past as_of lists the timeline as it
	// stood then).
	until := asOf.UnixNano()
	since := until - DefaultTimelineLookbackNanos
	epochs, err := c.Rollups(TierEpoch, since, until)
	if err != nil {
		return nil, fmt.Errorf("cortex.RecallDescend: list T0 windows: %w", err)
	}

	// --- Step 2: pick windows Salience DESC, then Start ASC (deterministic)
	sort.SliceStable(epochs, func(i, j int) bool {
		if epochs[i].Salience != epochs[j].Salience {
			return epochs[i].Salience > epochs[j].Salience
		}
		return epochs[i].Window.Start < epochs[j].Window.Start
	})

	queryLower := strings.ToLower(strings.TrimSpace(query))
	typeFilter := opts.Types

	result := &RecallResult{}
	// dedup leaf memories across windows by URI (a memory can appear in more
	// than one window's members after supersession); first-seen wins.
	seen := map[memory.URI]struct{}{}
	hits := make([]RecallHit, 0, k)

	for wi := range epochs {
		if len(hits) >= k {
			result.Truncated = true
			break
		}
		root := epochs[wi]
		result.Windows = append(result.Windows, root.Window)

		// --- Step 3+4: descend this window's members, sub-querying leaves.
		st := &descentState{
			c:          c,
			weights:    weights,
			now:        now,
			asOf:       asOf,
			queryLower: queryLower,
			typeFilter: typeFilter,
			maxDepth:   maxDepth,
			budget:     descentBudget,
			rootWindow: root.Window,
			seen:       seen,
		}
		windowHits, err := st.descend(&root, 1)
		if err != nil {
			return nil, err
		}
		hits = append(hits, windowHits...)
		if st.truncated {
			result.Truncated = true
		}
	}

	// Rank + cap the collected leaf hits (Salience DESC, URI ASC).
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].Salience != hits[j].Salience {
			return hits[i].Salience > hits[j].Salience
		}
		return hits[i].URI < hits[j].URI
	})
	if len(hits) > k {
		hits = hits[:k]
		result.Truncated = true
	}
	result.Hits = hits
	for _, h := range hits {
		if h.Depth > result.MaxDepthReached {
			result.MaxDepthReached = h.Depth
		}
	}
	return result, nil
}

// descentState carries the per-top-level-window descent context so the
// recursion stays a small pure function of (rollup, depth).
type descentState struct {
	c          *Cortex
	weights    salience.Weights
	now        time.Time
	asOf       time.Time
	queryLower string
	typeFilter []memory.Type
	maxDepth   int
	budget     int
	rootWindow Window
	seen       map[memory.URI]struct{}

	spent     int  // rendered tokens accumulated in THIS window descent
	truncated bool // budget/depth stopped this descent early
}

// descend resolves rec's member Refs at the given ladder depth. RefKindRollup
// members recurse one level deeper (bounded by maxDepth); RefKindMemory
// members are leaves — resolved, as_of-filtered, type-filtered, sub-query
// matched, and salience-scored. Returns the leaf hits reached under rec.
func (s *descentState) descend(rec *RollupRecord, depth int) ([]RecallHit, error) {
	// Depth cap (req.8.2): never descend past maxDepth. A memory leaf sits at
	// depth 4 by default, so this only trips when the caller lowered MaxDepth.
	if depth > s.maxDepth {
		s.truncated = true
		return nil, nil
	}

	var out []RecallHit
	for _, ref := range rec.Members {
		if s.spent >= s.budget {
			s.truncated = true
			break
		}
		switch ref.Kind {
		case RefKindRollup:
			// A coarser member points at a finer rollup; descend one level.
			if depth+1 > s.maxDepth {
				s.truncated = true
				continue
			}
			tier, start, ok := parseRollupRef(ref.URI)
			if !ok {
				continue // malformed member URI: skip, don't fail the descent
			}
			child, err := s.c.LoadRollup(tier, start)
			if err != nil {
				if errors.Is(err, memory.ErrNotFound) {
					continue // a lost finer rollup is skipped, not fatal
				}
				return nil, fmt.Errorf("cortex.RecallDescend: load %s rollup start=%d: %w", tier.String(), start, err)
			}
			childHits, err := s.descend(child, depth+1)
			if err != nil {
				return nil, err
			}
			out = append(out, childHits...)
		case RefKindMemory:
			// A memory leaf: the exact specific paged in on demand (T3). A
			// member sits one ladder level below rec, so its depth is depth+1
			// (hour rollup at depth 3 → memory leaf at depth 4).
			if depth+1 > s.maxDepth {
				s.truncated = true
				continue
			}
			hit, ok, err := s.leaf(ref.URI, depth+1)
			if err != nil {
				return nil, err
			}
			if ok {
				out = append(out, hit)
			}
		default:
			// Unknown ref kind: skip defensively.
		}
	}
	return out, nil
}

// leaf resolves a RefKindMemory member into a RecallHit, applying the
// type filter, the bi-temporal as_of validity filter (req.8.3), and the
// optional sub-query substring match, and charging the per-descent token
// budget. Returns ok=false when the member is filtered out or already seen.
func (s *descentState) leaf(uri memory.URI, depth int) (RecallHit, bool, error) {
	if _, dup := s.seen[uri]; dup {
		return RecallHit{}, false, nil
	}

	_, id, _, perr := ParseURI(uri)
	if perr != nil {
		return RecallHit{}, false, nil // malformed leaf URI: skip
	}
	mem, rerr := s.c.ResolveLatest(id)
	if rerr != nil {
		if errors.Is(rerr, memory.ErrNotFound) {
			return RecallHit{}, false, nil
		}
		return RecallHit{}, false, rerr
	}
	if mem.Head.Tombstoned != nil {
		return RecallHit{}, false, nil
	}

	// Type filter (leaf sub-query, mirrors query.Query.Type).
	if len(s.typeFilter) > 0 && !typeInSet(mem.Head.Type, s.typeFilter) {
		return RecallHit{}, false, nil
	}

	// Bi-temporal as_of validity filter, preserved at the leaf (req.8.3). Same
	// half-open [ValidFrom, ValidUntil) + ExpiresAt semantics as
	// query.withinValidity — a superseded/expired truth is never surfaced.
	if !versionValidAt(&mem.Version, s.asOf) {
		return RecallHit{}, false, nil
	}

	// Optional sub-query: case-insensitive substring over the rendered forms.
	rendered := mem.Version.Forms.Medium
	if rendered == "" {
		rendered = mem.Version.Forms.Short
	}
	if s.queryLower != "" && !strings.Contains(strings.ToLower(rendered), s.queryLower) {
		return RecallHit{}, false, nil
	}

	// Charge the per-descent token budget with this leaf's rendered cost.
	s.spent += memory.CountTokens(rendered)
	s.seen[uri] = struct{}{}

	// Score by the recency substrate at the descent clock (same seeding as
	// RecentEpisodes / Activate when persisted factors are absent).
	var score float32
	sc, ok, serr := salience.Read(s.c.s, id)
	if serr != nil {
		return RecallHit{}, false, serr
	}
	if ok {
		score = salience.ColdScoreWith(sc, s.weights, s.now)
	} else {
		seed := salience.Score{
			LastUsed:   mem.Version.CreatedAt.UnixNano(),
			Importance: mem.Head.DeclaredImportance,
		}
		score = salience.ColdScoreWith(&seed, s.weights, s.now)
	}

	return RecallHit{
		URI:      uri,
		Memory:   mem,
		Salience: float64(score),
		Window:   s.rootWindow,
		Depth:    depth,
	}, true, nil
}

// typeInSet reports whether t is one of the allowed types.
func typeInSet(t memory.Type, allowed []memory.Type) bool {
	for _, a := range allowed {
		if t == a {
			return true
		}
	}
	return false
}

// versionValidAt reports whether v's bi-temporal valid interval + expiry
// contain asOf. Mirrors query.withinValidity exactly (half-open
// [ValidFrom, ValidUntil); ValidFrom defaults to CreatedAt; ExpiresAt honored
// with at/after-close semantics) so recall descent and cortex.Find agree on
// "what was true then" (req.8.3).
func versionValidAt(v *memory.Version, asOf time.Time) bool {
	from := v.CreatedAt
	if v.ValidFrom != nil {
		from = *v.ValidFrom
	}
	if asOf.Before(from) {
		return false
	}
	if v.ValidUntil != nil && !asOf.Before(*v.ValidUntil) {
		return false
	}
	if v.ExpiresAt != nil && !asOf.Before(*v.ExpiresAt) {
		return false
	}
	return true
}

// parseRollupRef parses a rollup member URI (matrix://cortex/rollup/<tier>/<start>)
// back into its (tier, start). Returns ok=false for any non-rollup or
// malformed URI. The inverse of BuildRollupURI.
func parseRollupRef(uri memory.URI) (RollupTier, int64, bool) {
	const prefix = "matrix://cortex/rollup/"
	s := string(uri)
	if !strings.HasPrefix(s, prefix) {
		return 0, 0, false
	}
	rest := s[len(prefix):]
	slash := strings.IndexByte(rest, '/')
	if slash < 0 {
		return 0, 0, false
	}
	tierName := rest[:slash]
	startStr := rest[slash+1:]
	var tier RollupTier
	switch tierName {
	case "hour":
		tier = TierHour
	case "day":
		tier = TierDay
	case "epoch":
		tier = TierEpoch
	default:
		return 0, 0, false
	}
	start, err := strconv.ParseInt(startStr, 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return tier, start, true
}
