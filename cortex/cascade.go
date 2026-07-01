// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 2.2: the temporal-ladder CASCADE that rolls finer
// windows into coarser ones (hour -> day -> week/epoch), each level built with
// the SAME deterministic, derived-lane discipline as BuildRollup (rollup.go).
//
// # Shape (req.4.4)
//
// Cascade(upTo, now) walks the ladder bottom-up over CLOSED windows only (a
// window is closed when its End <= now, so a still-open window is never rolled
// and results are stable):
//
//	TierHour  : ensure an hour rollup exists for every closed hour window that
//	            has journal entries — built by BuildRollup over the JOURNAL.
//	TierDay   : each closed day window is built by AGGREGATING the hour rollups
//	            whose windows fall inside it (the finer rollups), NOT by
//	            re-scanning the journal.
//	TierEpoch : each closed epoch (7-day) window is built by aggregating the
//	            DAY rollups inside it.
//
// # Coarse members resolve to finer rollups (req.3.3)
//
// A coarse RollupRecord's Members are Refs to its constituent FINER rollups —
// Ref.URI = the child rollup's BuildRollupURI, Ref.Kind = RefKindRollup. So a
// day rollup's members are its hour rollups; an epoch rollup's members are its
// day rollups. This is the record-level recursive-descent substrate: resolve a
// coarse rollup, follow a member Ref to a finer rollup, and so on down to the
// hour rollups whose own members are memory URIs.
//
// # Determinism (req.4.2)
//
// A coarse rollup is a PURE function of its child records + the fixed bucket
// boundaries. Aggregation (EntryCount / KindTally / OutcomeTally sums, SeqLo /
// SeqHi span, max child Salience) is order-independent. The member ranking and
// ShortForm are a total, stable order — child Salience DESC, then child window
// Start ASC — with no wall-clock or map-iteration dependence (map tallies are
// encoded via the canonical CoreDetEnc encoder and rendered with sorted keys).
// No wall-now leaks into any stored coarse record: the journal Entry.CreatedAt
// is the only wall-clock value, exactly as BuildRollup, and it is never part
// of the integrity-hashed RollupRecord bytes.
//
// # Idempotence strategy (req.4.1)
//
// Cascade only rolls CLOSED windows, and a closed window's constituent facts
// (journal entries, or finer rollups over already-closed sub-windows) are
// immutable. So an existing rollup for a closed window is authoritative:
//
//   - Hour tier: if LoadRollup finds a record for the closed hour window, skip
//     it (BuildRollup would otherwise append a fresh KindRollup entry every
//     call). Missing -> BuildRollup. This is "build only if missing".
//   - Coarse tiers: build the aggregate record in memory, encode it, and
//     compare against any existing record's bytes. Byte-identical -> skip (no
//     journal append). Missing OR stale (child set changed) -> write. This is
//     "build only if missing OR stale".
//
// Both branches make a second Cascade call over UNCHANGED store state append
// ZERO new journal entries, while a coarse record whose children changed (or a
// deleted record — task 3.1 lazy repair) is rebuilt byte-identically. The
// cascade is therefore fully rebuildable from the journal: hours derive from
// journal entries, days from hours, epochs from days.

package cortex

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
)

// RefKindRollup is the Ref.Kind tag for a member that resolves to a FINER
// rollup record (a roll/ URI via BuildRollupURI), as opposed to RefKindMemory
// which resolves to a cortex memory. Coarse-tier rollups (day, epoch) carry
// RefKindRollup members; hour rollups carry RefKindMemory members (req.3.3).
const RefKindRollup = "rollup"

// windowClosed reports whether w is closed as of now: a window is closed once
// its End boundary is at or before now, so its contents are final and safe to
// roll. Cascade never rolls a still-open window.
func windowClosed(w Window, now int64) bool { return w.End <= now }

// Cascade rolls finer windows into coarser ones up to and including upTo,
// considering only CLOSED windows as of now (req.4.4). It is idempotent: a
// second call over unchanged store state appends ZERO journal entries (see the
// idempotence strategy in the file header).
//
// upTo selects how far up the ladder to build: TierHour builds only hour
// rollups; TierDay additionally builds day rollups from the hour rollups;
// TierEpoch additionally builds epoch rollups from the day rollups.
func (c *Cortex) Cascade(upTo RollupTier, now int64) error {
	if upTo != TierHour && upTo != TierDay && upTo != TierEpoch {
		return fmt.Errorf("cortex.Cascade: unknown tier %d", uint8(upTo))
	}

	// --- Tier 1: ensure hour rollups for closed hour windows -------------
	if err := c.cascadeHours(now); err != nil {
		return err
	}
	if upTo == TierHour {
		return nil
	}

	// --- Tier 2: aggregate hour rollups into day rollups -----------------
	if err := c.cascadeCoarse(TierHour, TierDay, now, DayWindow); err != nil {
		return err
	}
	if upTo == TierDay {
		return nil
	}

	// --- Tier 3: aggregate day rollups into epoch rollups ----------------
	return c.cascadeCoarse(TierDay, TierEpoch, now, EpochWindow)
}

// cascadeHours ensures an hour rollup exists for every closed hour window that
// carries journal entries. It collects the distinct hour-window starts that
// have at least one non-KindRollup entry in a SINGLE journal scan, then builds
// each closed window that has no rollup yet (build-only-if-missing → zero
// journal appends when re-run over unchanged state).
func (c *Cortex) cascadeHours(now int64) error {
	starts := map[int64]struct{}{}
	iterErr := c.s.IterJournal(func(e *journal.Entry) error {
		// A rollup never summarizes rollups-of-itself; the hour tier windows
		// the raw journal (same exclusion as BuildRollup).
		if e.Kind == journal.KindRollup {
			return nil
		}
		starts[HourWindow(e.CreatedAt).Start] = struct{}{}
		return nil
	})
	if iterErr != nil {
		return fmt.Errorf("cortex.Cascade: scan journal for hour windows: %w", iterErr)
	}

	// Deterministic build order (ascending window start). Order does not
	// affect the roll/ storage order — the key layout sorts by start — but a
	// stable order keeps the journal-append sequence reproducible.
	ordered := make([]int64, 0, len(starts))
	for s := range starts {
		ordered = append(ordered, s)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })

	for _, s := range ordered {
		w := HourWindow(s)
		if !windowClosed(w, now) {
			continue // never roll a still-open hour window
		}
		// Build only if missing: a closed hour window's journal facts are
		// immutable, so an existing rollup is authoritative. Re-running is a
		// no-op (BuildRollup would otherwise append a fresh KindRollup entry).
		if _, err := c.LoadRollup(TierHour, w.Start); err == nil {
			continue
		} else if !errors.Is(err, memory.ErrNotFound) {
			return fmt.Errorf("cortex.Cascade: load hour rollup start=%d: %w", w.Start, err)
		}
		if _, err := c.BuildRollup(w); err != nil {
			return fmt.Errorf("cortex.Cascade: build hour rollup start=%d: %w", w.Start, err)
		}
	}
	return nil
}

// cascadeCoarse aggregates every finer-tier rollup into its parent coarse-tier
// window (bucketOf maps a child window start to its parent Window), building a
// coarse RollupRecord for each closed parent window. Only closed parent windows
// are built. Writes are idempotent via buildCoarseRollup's compute-compare-skip.
func (c *Cortex) cascadeCoarse(finer, coarse RollupTier, now int64, bucketOf func(int64) Window) error {
	children, err := c.Rollups(finer, minWindowStart, maxWindowStart)
	if err != nil {
		return fmt.Errorf("cortex.Cascade: load %s rollups: %w", finer.String(), err)
	}

	// Group finer rollups by their parent coarse window start.
	groups := map[int64][]RollupRecord{}
	order := make([]int64, 0)
	for _, child := range children {
		parent := bucketOf(child.Window.Start)
		if parent.Tier != coarse {
			// Defensive: bucketOf must return the coarse tier's window.
			return fmt.Errorf("cortex.Cascade: bucket tier %d != coarse %d", uint8(parent.Tier), uint8(coarse))
		}
		if !windowClosed(parent, now) {
			continue // never roll a still-open coarse window
		}
		if _, seen := groups[parent.Start]; !seen {
			order = append(order, parent.Start)
		}
		groups[parent.Start] = append(groups[parent.Start], child)
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })

	for _, start := range order {
		kids := groups[start]
		parent := bucketOf(kids[0].Window.Start)
		if _, err := c.buildCoarseRollup(parent, kids); err != nil {
			return fmt.Errorf("cortex.Cascade: build %s rollup start=%d: %w", coarse.String(), start, err)
		}
	}
	return nil
}

// minWindowStart / maxWindowStart bound the Rollups prefix scan to "all
// windows of the tier". Window starts are Unix-nanosecond bucket boundaries;
// these cover the full int64 range so no built child is filtered out.
const (
	minWindowStart int64 = -1 << 62
	maxWindowStart int64 = 1<<62 - 1
)

// buildCoarseRollup constructs and (idempotently) persists a coarse-tier
// RollupRecord that aggregates the given finer-resolution child rollups, in the
// derived lane (roll/ record + KindRollup journal entry, NO SMT write) — the
// SAME posture as BuildRollup. Members reference the child rollups by URI with
// Ref.Kind == RefKindRollup (req.3.3).
//
// Determinism: aggregation is a pure sum/max over the children; member ranking
// and ShortForm order the children by Salience DESC then window Start ASC — a
// total, stable order. No wall-now enters the stored record.
//
// Idempotence: the record is built and encoded in memory first; if an existing
// record at roll/<tier><start> is byte-identical, the write (and its journal
// append) is skipped. Missing OR stale -> write. Returns the rollup URI.
func (c *Cortex) buildCoarseRollup(w Window, children []RollupRecord) (memory.URI, error) {
	if w.End <= w.Start {
		return "", fmt.Errorf("cortex.buildCoarseRollup: empty window [%d,%d)", w.Start, w.End)
	}
	if len(children) == 0 {
		return "", nil
	}

	// Rank children: Salience DESC, then window Start ASC (total + stable).
	ranked := make([]RollupRecord, len(children))
	copy(ranked, children)
	sort.Slice(ranked, func(i, j int) bool {
		if ranked[i].Salience != ranked[j].Salience {
			return ranked[i].Salience > ranked[j].Salience
		}
		return ranked[i].Window.Start < ranked[j].Window.Start
	})

	// Aggregate tallies + counts + seq span over ALL children (not just the
	// capped member set), and take max child salience as the coarse salience.
	kindTally := map[string]uint32{}
	outcomeTally := map[string]uint32{}
	var entryCount uint32
	var seqLo, seqHi uint64
	var topSalience float64
	haveSeq := false
	for i := range ranked {
		child := &ranked[i]
		entryCount += child.EntryCount
		for k, v := range child.KindTally {
			kindTally[k] += v
		}
		for k, v := range child.OutcomeTally {
			outcomeTally[k] += v
		}
		if !haveSeq {
			seqLo, seqHi = child.SeqLo, child.SeqHi
			haveSeq = true
		} else {
			if child.SeqLo < seqLo {
				seqLo = child.SeqLo
			}
			if child.SeqHi > seqHi {
				seqHi = child.SeqHi
			}
		}
		if child.Salience > topSalience {
			topSalience = child.Salience
		}
	}

	// Event-count floor: consistent with BuildRollup — never emit a degenerate
	// record (children each pass the floor, so this holds in practice).
	if entryCount < DefaultRollupEventCountFloor {
		return "", nil
	}

	// Members: reference the constituent FINER rollups (req.3.3), capped, in
	// the ranked order.
	memberCount := len(ranked)
	if memberCount > RollupMaxMembers {
		memberCount = RollupMaxMembers
	}
	refs := make([]Ref, 0, memberCount)
	for i := 0; i < memberCount; i++ {
		child := &ranked[i]
		refs = append(refs, Ref{
			URI:  BuildRollupURI(child.Window.Tier, child.Window.Start),
			Kind: RefKindRollup,
		})
	}

	shortForm := buildCoarseShortForm(w, len(ranked), entryCount, kindTally, outcomeTally, ranked, refs)

	record := &RollupRecord{
		SchemaVersion: RollupSchemaVersion,
		Window:        w,
		SeqLo:         seqLo,
		SeqHi:         seqHi,
		EntryCount:    entryCount,
		KindTally:     kindTally,
		OutcomeTally:  outcomeTally,
		Members:       refs,
		ShortForm:     shortForm,
		Salience:      topSalience,
		EnrichRef:     "",
	}
	encodedRec, err := EncodeRollupRecord(record)
	if err != nil {
		return "", fmt.Errorf("cortex.buildCoarseRollup: encode record: %w", err)
	}

	// Idempotence: skip the write (and journal append) when an identical
	// record already exists for this closed window.
	if existing, lerr := c.LoadRollup(w.Tier, w.Start); lerr == nil {
		if prevBytes, eerr := EncodeRollupRecord(existing); eerr == nil && bytes.Equal(prevBytes, encodedRec) {
			return BuildRollupURI(w.Tier, w.Start), nil
		}
	} else if !errors.Is(lerr, memory.ErrNotFound) {
		return "", fmt.Errorf("cortex.buildCoarseRollup: load existing: %w", lerr)
	}

	recordHash := sha256.Sum256(encodedRec)
	rp := &journal.RollupPayload{
		SchemaVersion: RollupSchemaVersion,
		Tier:          uint8(w.Tier),
		Start:         w.Start,
		End:           w.End,
		EntryCount:    entryCount,
		RecordHash:    recordHash,
	}
	rpBytes, err := journal.EncodeRollupPayload(rp)
	if err != nil {
		return "", fmt.Errorf("cortex.buildCoarseRollup: encode payload: %w", err)
	}
	je := &journal.Entry{
		Kind:      journal.KindRollup,
		CreatedAt: c.now().UnixNano(),
		Payload:   rpBytes,
	}

	// Atomic derived-lane batch: roll/<tier><start> record + journal entry.
	// NO SMT update — coarse rollups are derived audit / working index, never
	// canonical world-state (compact.go:426-442 posture).
	rollKey := keys.RollupKey(uint8(w.Tier), uint64(w.Start))
	wb := c.s.BeginWrite()
	defer wb.Abort()
	if err := wb.Set(rollKey, encodedRec); err != nil {
		return "", fmt.Errorf("cortex.buildCoarseRollup: set roll: %w", err)
	}
	if err := wb.AppendJournal(je); err != nil {
		return "", fmt.Errorf("cortex.buildCoarseRollup: append journal: %w", err)
	}
	if err := wb.Commit(); err != nil {
		return "", fmt.Errorf("cortex.buildCoarseRollup: commit: %w", err)
	}
	return BuildRollupURI(w.Tier, w.Start), nil
}

// buildCoarseShortForm renders the deterministic extractive summary for a
// coarse rollup over its child rollups. Tallies render with alphabetically
// sorted keys and the child window range + top child refs are already in the
// deterministic ranked order, so the output is byte-stable for the same inputs
// (req.4.2).
//
// Format:
//
//	[<tier> <startRFC3339>..<endRFC3339>] rolled C children (N entries) spanning <childStartRFC3339>..<childEndRFC3339>; kinds: ..; outcomes: ..; children: <uri1>, <uri2>
func buildCoarseShortForm(w Window, childCount int, entryCount uint32, kindTally, outcomeTally map[string]uint32, ranked []RollupRecord, members []Ref) string {
	var b strings.Builder
	start := time.Unix(0, w.Start).UTC().Format(time.RFC3339)
	end := time.Unix(0, w.End).UTC().Format(time.RFC3339)

	// Child window range = [min child Start, max child End) over ALL children,
	// computed independently of the salience ranking so it is order-stable.
	var childLo, childHi int64
	haveRange := false
	for i := range ranked {
		cw := ranked[i].Window
		if !haveRange {
			childLo, childHi = cw.Start, cw.End
			haveRange = true
			continue
		}
		if cw.Start < childLo {
			childLo = cw.Start
		}
		if cw.End > childHi {
			childHi = cw.End
		}
	}
	childStart := time.Unix(0, childLo).UTC().Format(time.RFC3339)
	childEnd := time.Unix(0, childHi).UTC().Format(time.RFC3339)

	fmt.Fprintf(&b, "[%s %s..%s] rolled %d children (%d entries) spanning %s..%s; kinds: %s; outcomes: %s",
		w.Tier.String(), start, end, childCount, entryCount, childStart, childEnd,
		renderTally(kindTally), renderTally(outcomeTally))
	if len(members) > 0 {
		b.WriteString("; children: ")
		for i, m := range members {
			if i > 0 {
				b.WriteString(", ")
			}
			b.WriteString(string(m.URI))
		}
	}
	return b.String()
}
