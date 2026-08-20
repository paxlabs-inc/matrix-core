// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package cmharness is the reusable replay-safety harness for the
// continuous-memory feature. It proves that every continuous-memory write
// rides the DERIVED lane and never perturbs the anchored world-state, and it
// is the baseline the temporal ladder + activation (later waves) must also
// satisfy.
//
// # The OverallRoot / journal-MMR nuance (read this first)
//
// cortex's OverallRoot is
//
//	snapshot.ComputeOverallRoot(journalRoot, stateRoots{"memories","edges"})
//
// (snapshot.go:218) and therefore COMMITS to the journal MMR root as well as
// to the two anchored SMT roots. The continuous-memory derived lane (the
// session/transcript store, cortex.Compact, and the later rollup/enrichment
// tiers) DOES append journal entries, which move the journal MMR root via the
// installed JournalHook. A LITERAL "byte-identical OverallRoot with the
// derived lane's journal entries present vs absent" is therefore FALSE and is
// deliberately NOT asserted anywhere in this harness.
//
// The genuinely-true, provable invariants this harness asserts are:
//
//  1. ANCHORED SMT ROOT IDENTITY — the real "anchored world-state root".
//     snapshot.AnchoredNamespaces = {"edges","memories"}. Derived-lane writes
//     (session records now; rollups/enrichment/caches later) call NO
//     snap.StageMemoryUpdate / StageEdgeUpdate, so the "memories" and "edges"
//     SMT roots are byte-identical whether or not the continuous-memory lane
//     is active. Proved two ways:
//     (a) AssertNoAnchoredDrift — same instance, capture roots, run a pure
//     derived write, re-capture, assert unchanged (req.11.1).
//     (b) CompareAnchoredRoots — two instances driven with the SAME anchored
//     ops (identical deterministic clock + idGen so memory IDs match),
//     one of them ALSO carrying derived-lane writes, assert both anchored
//     roots byte-identical to each other (req.11.2).
//
//  2. REPLAY INVARIANT HOLDS WITH DERIVED ENTRIES PRESENT — ReplayPreservesRoot
//     wraps replay.Rebuild + replay.VerifyPreservesRoot. Rebuild drops the
//     derived indexes and rebuilds from canonical state, asserting
//     PreOverallRoot == PostOverallRoot byte-identical. Because the session
//     journal entries live in j/ (canonical, kept — they are NOT in
//     replay/drop.go's derivedPrefixes) and rebuildJournalMMR re-hashes j/
//     bytes generically via journal.LeafHash, the FULL OverallRoot (including
//     the derived-entry MMR leaves) rebuilds byte-identically. This is the
//     load-bearing D11 proof and the reusable baseline (req.11.2).
//
// # req.11.3 holds by construction
//
// req.11.3 forbids any modification to the signed MCL walk and any Liaison
// cortex-write / signing / plan-walk capability. This package — and the
// continuous-memory feature as a whole — adds ONLY derived-lane cortex
// surfaces (session store, rollups, activation composer). cortex has no MCL
// walk and holds no key material; the Liaison remains a pure read-only
// observability side-channel. Nothing here writes to, signs, or walks a plan.
// req.11.3 is therefore satisfied with no code change; this harness documents
// and does not weaken that invariant.
package cmharness

import (
	"fmt"

	"centra/core/cortex"
	"centra/core/cortex/replay"
	"centra/core/cortex/snapshot"

	"time"
)

// AnchoredRoots returns the current SMT root for each anchored namespace
// (snapshot.AnchoredNamespaces = {"edges","memories"}), keyed by namespace.
// These are the byte-strings that define the anchored world-state; a
// derived-lane write must leave every one of them unchanged.
func AnchoredRoots(c *cortex.Cortex) (map[string][32]byte, error) {
	if c == nil {
		return nil, fmt.Errorf("cmharness.AnchoredRoots: nil cortex")
	}
	snap := c.Snap()
	if snap == nil {
		return nil, fmt.Errorf("cmharness.AnchoredRoots: nil snapshot state")
	}
	out := make(map[string][32]byte, len(snapshot.AnchoredNamespaces))
	for _, ns := range snapshot.AnchoredNamespaces {
		smt := snap.SMT(ns)
		if smt == nil {
			return nil, fmt.Errorf("cmharness.AnchoredRoots: no SMT for anchored namespace %q", ns)
		}
		root, err := smt.Root()
		if err != nil {
			return nil, fmt.Errorf("cmharness.AnchoredRoots: %q root: %w", ns, err)
		}
		out[ns] = root
	}
	return out, nil
}

// AssertNoAnchoredDrift captures the anchored roots, runs mutate, re-captures,
// and returns a non-nil error if any anchored root changed. This is the
// reusable req.11.1 assertion for ANY derived-lane write: pass an
// AppendMessage (now) or a rollup/enrichment/cache write (later) as mutate.
//
// mutate must not be nil. Any error mutate returns is propagated (wrapped)
// before the after-capture, so a failed derived write is surfaced honestly
// rather than masquerading as "no drift".
func AssertNoAnchoredDrift(c *cortex.Cortex, mutate func() error) error {
	if c == nil {
		return fmt.Errorf("cmharness.AssertNoAnchoredDrift: nil cortex")
	}
	if mutate == nil {
		return fmt.Errorf("cmharness.AssertNoAnchoredDrift: nil mutate")
	}
	before, err := AnchoredRoots(c)
	if err != nil {
		return fmt.Errorf("cmharness.AssertNoAnchoredDrift: capture before: %w", err)
	}
	if err := mutate(); err != nil {
		return fmt.Errorf("cmharness.AssertNoAnchoredDrift: mutate: %w", err)
	}
	after, err := AnchoredRoots(c)
	if err != nil {
		return fmt.Errorf("cmharness.AssertNoAnchoredDrift: capture after: %w", err)
	}
	for _, ns := range snapshot.AnchoredNamespaces {
		if before[ns] != after[ns] {
			return fmt.Errorf("cmharness.AssertNoAnchoredDrift: anchored namespace %q root drifted across derived write: %x -> %x",
				ns, before[ns], after[ns])
		}
	}
	return nil
}

// CompareAnchoredRoots returns a non-nil error unless both instances' anchored
// roots (memories AND edges) are byte-identical. This is the reusable
// active-vs-inactive req.11.2 assertion: drive two cortex instances with the
// SAME anchored ops (identical deterministic clock + idGen so memory IDs
// match), have one of them additionally carry continuous-memory derived-lane
// writes, then assert their anchored world-state is identical.
func CompareAnchoredRoots(a, b *cortex.Cortex) error {
	ra, err := AnchoredRoots(a)
	if err != nil {
		return fmt.Errorf("cmharness.CompareAnchoredRoots: instance a: %w", err)
	}
	rb, err := AnchoredRoots(b)
	if err != nil {
		return fmt.Errorf("cmharness.CompareAnchoredRoots: instance b: %w", err)
	}
	for _, ns := range snapshot.AnchoredNamespaces {
		if ra[ns] != rb[ns] {
			return fmt.Errorf("cmharness.CompareAnchoredRoots: anchored namespace %q differs across instances: a=%x b=%x",
				ns, ra[ns], rb[ns])
		}
	}
	return nil
}

// ReplayPreservesRoot runs replay.Rebuild against the cortex's own store +
// snapshot state and then replay.VerifyPreservesRoot on the result. It returns
// the *replay.Result on success, or the wrapped root-mismatch error on
// failure. This is the reusable D11 proof entrypoint: it proves the FULL
// OverallRoot (including the derived-lane journal-MMR leaves) rebuilds
// byte-identically from canonical state after dropping the derived indexes.
//
// now is the clock used for salience recomputation during rebuild (salience is
// NOT an OverallRoot input, so any clock preserves the root); nil falls back to
// the wallclock inside replay.Rebuild.
//
// NOTE on lane discipline: this must live in cmharness (not in package replay)
// because it drives a *cortex.Cortex and replay cannot import cortex (cortex
// imports replay).
func ReplayPreservesRoot(c *cortex.Cortex, now func() time.Time) (*replay.Result, error) {
	if c == nil {
		return nil, fmt.Errorf("cmharness.ReplayPreservesRoot: nil cortex")
	}
	res, err := replay.Rebuild(c.Store(), c.Snap(), replay.Options{Now: now})
	if err != nil {
		return nil, fmt.Errorf("cmharness.ReplayPreservesRoot: rebuild: %w", err)
	}
	if err := replay.VerifyPreservesRoot(res); err != nil {
		return res, fmt.Errorf("cmharness.ReplayPreservesRoot: %w", err)
	}
	return res, nil
}
