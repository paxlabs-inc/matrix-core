// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Continuous-memory task 4.1: the activation composer — cortex.Activate
// extends cortex.Context (req.7).
//
// # What this is
//
// Activate is the single call a turn loop makes to get its WHOLE hot
// working set: {Pinned, Timeline(T0), Recent(T1), Transcript(T2 slice),
// StorySoFar, ReachableURIs(T3 handles)}, budget-trimmed by one global
// salience-ascending trim — the same shape Context already applies to its
// three tiers (context.go's trimContextByBudget), generalized here across
// four heterogeneous tiers instead of memory.ID-keyed candidates alone.
//
// # Tier sourcing (req.7.2 — "materialized, no live recompute")
//
//   - Pinned: c.cachedTierPinned() + the SAME resolve/score/PinnedFloor
//     steps Context.go's Step 4/6 perform — but the ID SCAN (tierPinned's
//     idx/type walk) is served from an in-memory, journal-seq-keyed cache
//     (fixes audit finding NE-7: the per-STEP pinned rescan collapses to
//     once per turn, since nothing invalidates the cache unless cortex was
//     actually written to in between).
//   - Timeline (T0): c.Rollups(TierEpoch, since, until) — a plain prefix
//     scan over ALREADY-PERSISTED roll/ records (rollup.go). No BuildRollup/
//     Cascade call happens here.
//   - Recent (T1): c.RecentEpisodes (recent.go, task 3.2) — task 4.1
//     explicitly depends on 3.2 because this is exactly why: Recent is
//     served by the T1 reader that already reads only the materialized hour
//     rollups, never re-windowing the journal.
//   - Transcript (T2): c.Transcript(conv, 0, 0) tail-sliced to the most
//     recent DefaultTranscriptTailLimit messages — straight from the
//     session store (session.go, task 1.1), no ladder involved.
//   - StorySoFar: c.LoadStorySoFar(conv), with an inline LAZY REPAIR
//     (rebuild via c.BuildStorySoFar) when the persisted record is missing
//     or stale relative to the transcript's current head seq — the exact
//     "missing or stale coarser tier -> idempotent rebuild" discipline
//     repair.go established for the rollup ladder (task 3.1), applied here
//     to the story record (task 4.2).
//
// # Clock choice (deliberate, documented deviation from Context)
//
// cortex.Context (context.go:311) reads the real wall clock directly
// (`time.Now().UTC()`) rather than the injectable c.now, because its
// salience recompute is meant to track true wall time regardless of any
// test clock override elsewhere in a call chain. Activate instead uses
// c.now() for every salience/recency/window computation, matching every
// OTHER continuous-memory primitive (BuildRollup, Cascade, RepairRollup,
// RecentEpisodes all either take `now` as a parameter or are driven through
// c.now()) — so Activate is deterministically testable with the same
// mutClock harness the rest of this feature's test suite already uses. Only
// the LatencyMS instrumentation uses the real wall clock (time.Now()),
// exactly like Context does, since that measures actual call latency, not
// domain time.
//
// # Replay-safety (req.7.6)
//
// Activate performs NO snap.StageMemoryUpdate / StageEdgeUpdate — every
// read it does (Context-style tier scan, Rollups, RecentEpisodes,
// Transcript) is already documented pure-read in its own file. Its ONE
// side effect, the lazy StorySoFar repair, rides the exact same
// derived-lane posture as BuildRollup/RepairRollup (a journal entry + a
// non-anchored record). The Pinned cache is pure in-memory runtime state
// (cortex.go) and touches no lane at all.
package cortex

import (
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/salience"
)

// Budget bounds the total rendered-token size of an ActivationBundle. Zero
// defaults to DefaultActivateBudgetTokens; values above
// MaxActivateBudgetTokens are clamped (mirrors ContextOpts.BudgetTokens).
type Budget struct {
	Tokens int
}

const (
	// DefaultActivateBudgetTokens mirrors Context's DefaultBudgetTokens.
	DefaultActivateBudgetTokens = DefaultBudgetTokens
	// MaxActivateBudgetTokens caps a caller-supplied activation budget. It is
	// deliberately DECOUPLED from Context's MaxBudgetTokens (4000): the
	// activation bundle is the agent's whole durable-memory working set, and a
	// large-window caller (Neo at 1M) legitimately asks for a far richer
	// bundle than the legacy Context composer ever serves. The ceiling bounds
	// pathological requests, not normal ones.
	MaxActivateBudgetTokens = 32000

	// DefaultTimelineLookbackNanos bounds the T0 Timeline tier to the last
	// N nanoseconds of epoch rollups. Anchored to salience.HalfLifeNanos
	// (90 days) — the same constant this whole system already uses to
	// define "the recent past" via the recency substrate, so the Timeline
	// horizon and the recency decay this feature ranks everything else by
	// are the same 90-day notion of "the general past", not an arbitrary
	// second number.
	DefaultTimelineLookbackNanos = salience.HalfLifeNanos

	// DefaultRecentEpisodesN caps the T1 Recent tier's episode count.
	DefaultRecentEpisodesN = 16

	// DefaultTranscriptTailLimit caps the T2 Transcript slice to the most
	// recent N messages of the conversation.
	DefaultTranscriptTailLimit = 64
)

// ActivationBundle is the response shape from Cortex.Activate (req.7.1).
type ActivationBundle struct {
	// Pinned mirrors Bundle.Pinned: Identity ∪ Constraint{Hard} ∪
	// Goal{Active}, salience-desc, PinnedFloor-protected.
	Pinned []*memory.Memory
	// Timeline is the T0 coarse tier: materialized epoch rollups within
	// DefaultTimelineLookbackNanos of now, ascending window-start order
	// (survivors after trim).
	Timeline []RollupRecord
	// Recent is the T1 mid-range tier: c.RecentEpisodes' materialized
	// hour-rollup-backed episodes, salience-desc (survivors after trim).
	Recent []Episode
	// Transcript is the T2 slice: the tail of this conversation's own
	// session-store transcript, ascending seq order (survivors after trim).
	Transcript []Message
	// StorySoFar is the durable per-conversation rolling summary (task 4.2),
	// the replacement for Neo's ephemeral a.summary. Empty when the
	// conversation has no transcript yet.
	StorySoFar string

	// ReachableURIs lists tier candidates that were computed but did NOT
	// survive the budget trim (T3 handles — resolve lazily), capped at
	// MaxReachableURIs. Transcript members trim to their session URI
	// (session.go's BuildSessionURI); Timeline/Recent to their rollup /
	// memory URI; Pinned to its cortex memory URI.
	ReachableURIs []memory.URI

	// TotalTokens is the final post-trim rendered-token sum (including
	// StorySoFar, which is never itself trimmed — see activationTrim).
	TotalTokens int
	// Trimmed is the count of tier candidates dropped by budget enforcement.
	Trimmed int
	// LatencyMS is wall-clock from the start of Activate to the return
	// statement (real time.Now(), not c.now() — see file header).
	LatencyMS int64
	// SelectionReason explains an empty activation without exposing content or
	// internal identifiers. It is populated when relevant live memory exists
	// outside the materialized activation tiers.
	SelectionReason string
}

// cachedTierPinned returns the Pinned-tier candidate ID list, served from
// the in-memory per-turn cache (cortex.go's pinnedCache* fields) when the
// journal head hasn't advanced since it was last computed, or freshly
// scanned (and cached) otherwise. See cortex.go's field doc for the full
// invalidation rationale (req.7.2, audit NE-7).
func (c *Cortex) cachedTierPinned() ([]memory.ID, error) {
	seq := c.s.NextSeq()

	c.pinnedMu.Lock()
	defer c.pinnedMu.Unlock()
	if c.pinnedCacheOK && c.pinnedCacheSeq == seq {
		return c.pinnedCacheIDs, nil
	}

	ids, err := c.tierPinned()
	if err != nil {
		return nil, err
	}
	c.pinnedCacheSeq = seq
	c.pinnedCacheIDs = ids
	c.pinnedCacheOK = true
	return ids, nil
}

// activationItem is the internal, tier-agnostic row the single global trim
// (req.7.1) ranks and drops from. tier+idx point back into the per-tier
// candidate slices assembled before trimming, so survivors can be spliced
// back into ActivationBundle's typed fields in their original per-tier
// order.
type activationItem struct {
	tier   string
	idx    int
	uri    memory.URI
	score  float64
	tokens int
}

// Activate composes the whole per-turn working set described in req.7.1.
// query is accepted per the design's call shape but is currently UNUSED: no
// acceptance criterion in req.7 assigns it tier-selection behavior (Pinned/
// Timeline/Recent/Transcript are all query-independent — unlike Context's
// Frame/Outcomes tiers, none of Activate's tiers are verb/object-keyed). It
// is reserved for a future extension rather than guessed at here.
func (c *Cortex) Activate(conv, queryText string, budget Budget) (*ActivationBundle, error) {
	start := time.Now()

	if conv == "" {
		return nil, ErrEmptyConversationID
	}

	tokBudget := budget.Tokens
	if tokBudget <= 0 {
		tokBudget = DefaultActivateBudgetTokens
	}
	if tokBudget > MaxActivateBudgetTokens {
		tokBudget = MaxActivateBudgetTokens
	}

	now := c.now() // see file header: deterministic clock choice
	weights, _, err := salience.ReadWeights(c.s)
	if err != nil {
		return nil, fmt.Errorf("cortex.Activate: read weights: %w", err)
	}

	// --- Pinned (cached ID scan + fresh resolve/score) -------------------
	pinnedIDs, err := c.cachedTierPinned()
	if err != nil {
		return nil, fmt.Errorf("cortex.Activate: pinned tier: %w", err)
	}
	pinnedMems := make([]*memory.Memory, 0, len(pinnedIDs))
	items := make([]activationItem, 0, len(pinnedIDs)+32)
	for _, id := range pinnedIDs {
		mem, rerr := c.ResolveLatest(id)
		if rerr != nil {
			if errors.Is(rerr, memory.ErrNotFound) {
				continue
			}
			return nil, fmt.Errorf("cortex.Activate: resolve pinned %s: %w", id, rerr)
		}
		if current, _ := memory.CurrentTruthAt(&mem.Head, &mem.Version, now); !current {
			continue
		}
		var score float32
		sc, ok, serr := salience.Read(c.s, id)
		if serr != nil {
			return nil, fmt.Errorf("cortex.Activate: salience read %s: %w", id, serr)
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
		if score < salience.PinnedFloor {
			score = salience.PinnedFloor
		}
		pinnedMems = append(pinnedMems, mem)
		uri := BuildURI(mem.Head.Type, mem.Head.ID, mem.Head.CurrentVersion)
		items = append(items, activationItem{
			tier: "pinned", idx: len(pinnedMems) - 1, uri: uri,
			score: float64(score), tokens: memory.CountTokens(mem.Version.Forms.Medium),
		})
	}

	// --- Timeline (T0): materialized epoch rollups, no recompute --------
	until := now.UnixNano()
	timelineSince := until - DefaultTimelineLookbackNanos
	timeline, err := c.Rollups(TierEpoch, timelineSince, until)
	if err != nil {
		return nil, fmt.Errorf("cortex.Activate: timeline tier: %w", err)
	}
	for i := range timeline {
		rec := &timeline[i]
		items = append(items, activationItem{
			tier: "timeline", idx: i, uri: BuildRollupURI(rec.Window.Tier, rec.Window.Start),
			score: rec.Salience, tokens: memory.CountTokens(rec.ShortForm),
		})
	}

	// --- Recent (T1): the materialized T1 reader (task 3.2), no recompute
	recent, err := c.RecentEpisodes(DefaultRecentEpisodesN, now)
	if err != nil {
		return nil, fmt.Errorf("cortex.Activate: recent tier: %w", err)
	}
	for i, ep := range recent {
		items = append(items, activationItem{
			tier: "recent", idx: i, uri: ep.Ref.URI,
			score: ep.Salience, tokens: memory.CountTokens(string(ep.Ref.URI)),
		})
	}

	// --- Transcript (T2): tail of the session store ----------------------
	allMsgs, err := c.Transcript(conv, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("cortex.Activate: transcript: %w", err)
	}
	// The tail CANDIDATE cap scales with the budget (a 20K budget can hold
	// hundreds of messages, not 64) — the global salience trim below is what
	// actually enforces the token budget; this cap only bounds candidate
	// scoring work. At the default budget it is exactly the old limit.
	tailLimit := DefaultTranscriptTailLimit
	if scaled := tokBudget / 50; scaled > tailLimit {
		tailLimit = scaled
	}
	transcript := allMsgs
	tailStart := 0
	if len(allMsgs) > tailLimit {
		tailStart = len(allMsgs) - tailLimit
		transcript = allMsgs[tailStart:]
	}
	for i := range transcript {
		m := &transcript[i]
		seed := salience.Score{LastUsed: m.TS}
		score := salience.ColdScoreWith(&seed, weights, now)
		items = append(items, activationItem{
			tier: "transcript", idx: i, uri: BuildSessionURI(conv, m.Seq),
			score: float64(score), tokens: memory.CountTokens(m.Content),
		})
	}

	// --- StorySoFar (task 4.2): load, lazily repairing if stale ----------
	storySoFar, err := c.loadOrRepairStorySoFar(conv, allMsgs)
	if err != nil {
		return nil, fmt.Errorf("cortex.Activate: story so far: %w", err)
	}
	storyTokens := memory.CountTokens(storySoFar)

	// --- single global salience-ascending trim (req.7.1) ------------------
	survive, reachable, total, trimmed := trimActivationItems(items, storyTokens, tokBudget)

	bundle := &ActivationBundle{
		StorySoFar:  storySoFar,
		TotalTokens: total,
		Trimmed:     trimmed,
	}
	for i, it := range items {
		if !survive[i] {
			continue
		}
		switch it.tier {
		case "pinned":
			bundle.Pinned = append(bundle.Pinned, pinnedMems[it.idx])
		case "timeline":
			bundle.Timeline = append(bundle.Timeline, timeline[it.idx])
		case "recent":
			bundle.Recent = append(bundle.Recent, recent[it.idx])
		case "transcript":
			bundle.Transcript = append(bundle.Transcript, transcript[it.idx])
		}
	}
	if len(reachable) > MaxReachableURIs {
		reachable = reachable[:MaxReachableURIs]
	}
	bundle.ReachableURIs = reachable
	if len(bundle.Pinned) == 0 && len(bundle.Timeline) == 0 && len(bundle.Recent) == 0 && len(bundle.Transcript) == 0 && strings.TrimSpace(bundle.StorySoFar) == "" {
		bundle.SelectionReason = c.emptyActivationReason(queryText)
	}

	bundle.LatencyMS = time.Since(start).Milliseconds()
	return bundle, nil
}

func (c *Cortex) emptyActivationReason(queryText string) string {
	queryText = strings.TrimSpace(queryText)
	if queryText == "" {
		return "no current memory was selected by the materialized activation tiers"
	}
	res, err := c.Find(query.Query{Near: queryText, Limit: 1, Form: query.FormMedium})
	if err == nil && res != nil && len(res.Memories) > 0 {
		return "relevant current memory exists outside the materialized activation tiers; use memory_recall"
	}
	res, err = c.Find(query.Query{
		Type: []memory.Type{
			memory.TypeIdentity, memory.TypeFact, memory.TypePreference,
			memory.TypeBelief, memory.TypeEvent, memory.TypeGoal,
			memory.TypeConstraint, memory.TypeCapability, memory.TypePattern,
		},
		Limit: 64,
		Form:  query.FormMedium,
	})
	if err == nil && res != nil {
		needle := strings.ToLower(queryText)
		for i, mem := range res.Memories {
			text := mem.Version.Forms.Medium
			if i < len(res.Rendered) && res.Rendered[i] != "" {
				text = res.Rendered[i]
			}
			if strings.Contains(strings.ToLower(text), needle) {
				return "relevant current memory exists outside the materialized activation tiers; use memory_recall"
			}
		}
	}
	return "no current memory matched the activation query"
}

// loadOrRepairStorySoFar returns the ShortForm of conv's story-so-far
// record, performing the lazy read-repair described in the file header when
// the persisted record is missing or stale relative to msgs' current head
// seq. Returns "" when the conversation has no transcript yet (req.7.5
// graceful degradation — never an error).
func (c *Cortex) loadOrRepairStorySoFar(conv string, msgs []Message) (string, error) {
	if len(msgs) == 0 {
		return "", nil
	}
	headSeq := msgs[len(msgs)-1].Seq

	rec, err := c.LoadStorySoFar(conv)
	if err != nil {
		if !errors.Is(err, memory.ErrNotFound) {
			return "", err
		}
		rec = nil
	}
	if rec == nil || rec.UpToSeq < headSeq {
		if _, berr := c.BuildStorySoFar(conv); berr != nil {
			return "", berr
		}
		rec, err = c.LoadStorySoFar(conv)
		if err != nil {
			return "", err
		}
	}
	return rec.ShortForm, nil
}

// trimActivationItems applies the single global salience-ascending trim
// (req.7.1): if the pooled token total (storyTokens + every item's tokens)
// already fits budget, nothing is dropped. Otherwise items are dropped
// lowest-score-first (URI ascending as a deterministic tiebreak), always
// keeping at least one item overall (the same "≥1 relief valve" convention
// trimContextByBudget/context.go uses), until the total fits or one item
// remains. storyTokens counts toward the total but StorySoFar is never
// itself a drop candidate (it is a single scalar summary, not a list of
// independent members).
func trimActivationItems(items []activationItem, storyTokens, budget int) (survive []bool, reachable []memory.URI, total, trimmed int) {
	survive = make([]bool, len(items))
	for i := range survive {
		survive[i] = true
	}
	total = storyTokens
	for _, it := range items {
		total += it.tokens
	}
	if total <= budget || len(items) == 0 {
		return
	}

	order := make([]int, len(items))
	for i := range order {
		order[i] = i
	}
	sort.SliceStable(order, func(a, b int) bool {
		ia, ib := items[order[a]], items[order[b]]
		if ia.score != ib.score {
			return ia.score < ib.score
		}
		return ia.uri < ib.uri
	})

	surviving := len(items)
	for _, idx := range order {
		if total <= budget || surviving <= 1 {
			break
		}
		survive[idx] = false
		surviving--
		total -= items[idx].tokens
		reachable = append(reachable, items[idx].uri)
		trimmed++
	}
	return
}
