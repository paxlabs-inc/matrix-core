// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 3.2: the T1 "recent episodes" reader (req.6).
//
// RecentEpisodes returns the last N episodes ACROSS memory types, ranked by
// the salience recency substrate (R = exp(-dt/90d), salience.go:209-216)
// evaluated at the caller's `now`. It is a pure READ composer — like
// cortex.Context, it writes nothing.
//
// # Served from MATERIALIZED rollups — NOT a per-call scan (req.6.2)
//
// The reader touches ONLY the small, pre-built roll/ record set for the recent
// horizon plus a point salience read + head resolve per capped member. It
// NEVER re-windows the journal (contrast: BuildRollup's IterJournal in
// rollup.go) and NEVER scans idx/type or all m/ memories. Concretely it calls
// c.Rollups(TierHour, since, until) — a single bounded ascending prefix scan
// over roll/<hour> — and reads each surfaced member's salience + head. This is
// the whole acceptance point: on a cortex with real memories written but NO
// hour rollups materialized yet, RecentEpisodes returns EMPTY, because there is
// nothing to read from the materialized lane; only after Cascade/BuildRollup
// materializes the hour rollup(s) do the episodes appear (proved by the
// materialization test). tierOutcomes (context.go:612) is a different beast: it
// is Event-only and verb/object-keyed (scans idx/actor_obj per (verb, object)
// tuple); RecentEpisodes is type-agnostic and served from the rollup members.
//
// # Why only the HOUR tier
//
// Hour rollups carry RefKindMemory members (top-salience memory URIs in the
// window — rollup.go:364-367). Coarse tiers (day/epoch) carry RefKindRollup
// members (references to finer rollups — cascade.go:295-302), which are NOT
// memory URIs and so cannot be resolved to episodes. RecentEpisodes therefore
// reads the HOUR tier only; the hour rollups fully cover the default 24h
// horizon, and the coarse tiers exist for narrative descent, not recent-episode
// enumeration.
//
// # Determinism posture
//
// The rollup RECORDS are deterministic (a pure function of journal facts +
// stored salience; rollup.go header). This live reader is NOT a stored record
// and intentionally depends on wall-clock `now`: it applies the recency
// substrate at read time so "recent" tracks the caller's clock. That is fine
// and expected for a pure read composer — it stages no anchored SMT write and
// appends nothing to the journal (proved by the read-only-safety test via
// cmharness.AssertNoAnchoredDrift).

package cortex

import (
	"errors"
	"sort"
	"time"

	"matrix/cortex/memory"
	"matrix/cortex/salience"
)

// Episode is one recent memory surfaced by RecentEpisodes: the memory Ref
// (Kind == RefKindMemory), the rollup Window that surfaced it (provenance),
// and its recency-scored Salience at the caller's `now`.
type Episode struct {
	Ref      Ref     // the memory URI (Kind from the rollup member)
	Window   Window  // provenance: which materialized rollup window surfaced it
	Salience float64 // recency-scored at `now` via salience.ColdScoreWith
}

// DefaultRecentLookbackNanos is the default T1 horizon: the last 24h of hour
// rollups. RecentEpisodes surfaces members from hour rollups whose window
// overlaps [now-DefaultRecentLookbackNanos, now].
const DefaultRecentLookbackNanos int64 = 24 * 3600 * 1_000_000_000

// RecentEpisodes returns up to n recent episodes across all memory types,
// ranked by the salience recency substrate at `now` (req.6.1), served purely
// from the materialized hour rollups in the default 24h horizon (req.6.2).
//
// Ranking is Salience DESC, then memory ID ASC (idLess) as a stable, total
// tiebreak. Tombstoned members are skipped; a member whose head can no longer
// be resolved (ErrNotFound) is skipped rather than erroring.
func (c *Cortex) RecentEpisodes(n int, now time.Time) ([]Episode, error) {
	if n <= 0 {
		return nil, nil
	}

	until := now.UnixNano()
	since := until - DefaultRecentLookbackNanos

	weights, _, err := salience.ReadWeights(c.s)
	if err != nil {
		return nil, err
	}

	// Read only the MATERIALIZED hour rollups overlapping [since, until]. The
	// lower bound is pulled back one hour so a window that started just before
	// `since` but still overlaps is included; the overlap check below discards
	// any window that ends at or before `since`.
	recs, err := c.Rollups(TierHour, since-hourNanos, until)
	if err != nil {
		return nil, err
	}

	// Collect + dedup members by memory URI, keeping the first (earliest-window)
	// occurrence's window as provenance. Duplicates re-score identically at
	// `now`, so first-seen is sufficient and deterministic.
	type memberScore struct {
		id     memory.ID
		uri    memory.URI
		window Window
		score  float32
	}
	seen := map[memory.URI]struct{}{}
	scored := make([]memberScore, 0, RollupMaxMembers)

	for i := range recs {
		rec := &recs[i]
		// Overlap of [rec.Start, rec.End) with [since, until]: half-open, so a
		// window ending exactly at `since` does not overlap.
		if rec.Window.End <= since || rec.Window.Start > until {
			continue
		}
		for _, m := range rec.Members {
			if m.Kind != RefKindMemory {
				continue // coarse-tier refs are not memory episodes
			}
			if _, dup := seen[m.URI]; dup {
				continue
			}
			seen[m.URI] = struct{}{}

			_, id, _, perr := ParseURI(m.URI)
			if perr != nil {
				// A malformed member URI is skipped, not fatal — the reader
				// degrades gracefully rather than failing the whole call.
				continue
			}

			// Skip tombstoned / vanished members. Resolving the head is the
			// same cheap tombstone check context.go performs (context.go:414).
			mem, rerr := c.ResolveLatest(id)
			if rerr != nil {
				if errors.Is(rerr, memory.ErrNotFound) {
					continue
				}
				return nil, rerr
			}
			if mem.Head.Tombstoned != nil {
				continue
			}

			// Apply the recency substrate at `now`. When persisted salience
			// factors are missing, seed deterministically from the memory's
			// own CreatedAt + declared importance (mirrors context.go:431-435),
			// so the score is a pure function of the memory + weights + now.
			var score float32
			sc, ok, serr := salience.Read(c.s, id)
			if serr != nil {
				return nil, serr
			}
			if ok {
				score = salience.ColdScoreWith(sc, weights, now)
			} else {
				seed := salience.Score{
					LastUsed:   mem.Version.CreatedAt.UnixNano(),
					Importance: mem.Head.DeclaredImportance,
				}
				score = salience.ColdScoreWith(&seed, weights, now)
			}

			scored = append(scored, memberScore{
				id:     id,
				uri:    m.URI,
				window: rec.Window,
				score:  score,
			})
		}
	}

	// Rank: Salience DESC, then memory ID ASC (stable, total order).
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return idLess(scored[i].id, scored[j].id)
	})
	if len(scored) > n {
		scored = scored[:n]
	}

	out := make([]Episode, 0, len(scored))
	for _, s := range scored {
		out = append(out, Episode{
			Ref:      Ref{URI: s.uri, Kind: RefKindMemory},
			Window:   s.window,
			Salience: float64(s.score),
		})
	}
	return out, nil
}
