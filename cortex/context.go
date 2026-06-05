// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 8: cold-start context bundle composer (research/04-cortex.md §12.1,
// research/03-retrieval-patterns.md §2). cortex.Context() is the consumer-
// side Phase-1 primitive: hydrate a fresh agent's working memory with
// {Pinned, Frame-relevant, Outcomes} in one call, under a token budget,
// without any vector search.
//
// API refusal enforced at the type level (research/03 §8 row 1): there is
// NO Near / NearURI / NearVector field on ContextOpts. Cold-start with
// vector recall is a forbidden combination; eliminating the field at
// compile time is the strongest possible enforcement.
//
// Tier composition (research/03 §2.3 table line):
//   "30% pinned, 50% frame-relevant, 20% outcomes  |  flexible"
// We honour `flexible` literally: no hard per-tier ratios. All three
// tiers are loaded fully, then a single global salience-asc trim brings
// total rendered tokens under BudgetTokens. Pinned-tier members receive
// a salience floor of PinnedFloor (§8.2) so they survive a tight budget
// preferentially over Frame/Outcomes peers — that's how the tier label
// translates into ranking pressure without locking in rigid ratios.
//
// Latency budget (research/03 §2.3 table):
//   p50 < 80 ms, hard ceiling < 250 ms.
// All three tier scans are sequential at v1 — Pebble point reads are
// 10–20 µs warm and the scans are bounded (Identity is O(1); Constraint
// and Goal are typically small; idx/frame/idx/actor_obj scans are
// pinned to (verb, kind, obj) prefixes). Parallelizing the three tiers
// is a §12.1 hint, not a requirement; revisit if profiling demands it.
//
// Error semantics: hard fail on any tier scan error (no partial bundle).
// Mirrors query.Run; matches research/03 §8 enforcement discipline
// (no silent degradation).
//
// Replay invariant: this file emits NOTHING that mutates the store —
// it is a pure read-side composer. No journal entries, no salience
// bumps, no idx/* writes. Calling Context never changes any cortex_
// snapshot_hash root.

package cortex

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"matrix/cortex/forms"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/query"
	"matrix/cortex/salience"
	"matrix/cortex/scope"
)

// Tier identifies one of the three cold-start tiers from research/03 §2.1.
// Used in ContextOpts.IncludeTiers to opt tiers in/out (e.g. for tests or
// budget-pressed callers).
type Tier byte

const (
	// TierPinned: Identity ∪ Constraint{Strength=Hard} ∪ Goal{Status=Active}.
	// Spec: research/04 §12.1 ("Pinned tier: idx/type/<Identity|Constraint|
	// Goal>/...") + research/03 §2.1 ("identity, active hard rules, active
	// hard constraints, currently-running intents").
	TierPinned Tier = 1
	// TierFrameRelevant: idx/frame scan by (verb, kind, obj) tuples.
	// Spec: research/04 §12.1 ("idx/frame/<verb>/<obj_kind>/<obj_id>/...
	// scan, then salience-rank").
	TierFrameRelevant Tier = 2
	// TierOutcomes: idx/actor_obj scan by (verb, obj) tuples, top-N most
	// recent. Spec: research/04 §12.1 + research/03 §2.1 ("1-3 prior
	// similar intents and their outcomes").
	TierOutcomes Tier = 3
)

// String returns the lower-case tier name used in API surfaces and CLI
// flags. Matches the research/03 §2.4 enum literals exactly.
func (t Tier) String() string {
	switch t {
	case TierPinned:
		return "pinned"
	case TierFrameRelevant:
		return "frame_relevant"
	case TierOutcomes:
		return "outcomes"
	}
	return "unknown"
}

// ParseTier returns the Tier for name (lower-case research/03 §2.4
// spelling). The CLI uses this to translate -include-tier flags.
func ParseTier(name string) (Tier, bool) {
	switch name {
	case "pinned":
		return TierPinned, true
	case "frame_relevant":
		return TierFrameRelevant, true
	case "outcomes":
		return TierOutcomes, true
	}
	return 0, false
}

// Defaults applied when ContextOpts fields are zero.
const (
	// DefaultBudgetTokens matches the research/03 §2.3 "Tokens returned
	// (auto form): 1.5–3k" target band; 3000 is the upper end so callers
	// who omit budget get the maximum spec-allowed cold-start size.
	DefaultBudgetTokens = 3000
	// DefaultOutcomeLimit matches research/03 §2.1 "1–3 prior similar
	// intents and their outcomes": 3 is the upper end.
	DefaultOutcomeLimit = 3
	// MaxBudgetTokens is the research/03 §2.3 hard ceiling. Requests
	// above this saturate at MaxBudgetTokens; we don't error so the
	// caller can pass a generous "everything you have" budget and let
	// the cortex cap it.
	MaxBudgetTokens = 4000
	// MaxReachableURIs caps the ReachableURIs response so a tight
	// budget over a huge pool doesn't return tens of thousands of
	// trimmed pointers. Picked at 64 to comfortably exceed any
	// realistic working-set (research/03 §9 worked example shows ~20
	// reachable pointers). Callers that need more should narrow the
	// frame instead.
	MaxReachableURIs = 64
)

// ContextOpts is the input to Cortex.Context. Mirrors the research/03
// §2.4 call shape; `actor` is implicit (taken from c.s.Actor()) since
// every cortex is per-actor by construction (§1 "single Pebble DB per
// actor"). NO near / NearVector field by design (research/03 §8).
type ContextOpts struct {
	// Verb is the closed D7 verb the agent is about to execute. Zero
	// (memory.Verb(0)) means "no verb supplied" — only TierPinned runs;
	// Frame and Outcomes are silently skipped (they require a verb to
	// key into idx/frame and idx/actor_obj).
	Verb memory.Verb

	// Objects maps a closed ObjKind name (lower-case D7 spelling, see
	// memory.ParseObjKind) to a free-form reference string. Each entry
	// drives one (verb, kind, ref) tuple for idx/frame and one
	// (verb, ref) tuple for idx/actor_obj. Empty map → Frame and
	// Outcomes tiers run with no candidates and return empty. Unknown
	// kinds return memory.ErrInvalidObjKind.
	Objects map[string]string

	// BudgetTokens caps total rendered tokens across all surviving
	// memories. Zero defaults to DefaultBudgetTokens (3000). Values
	// above MaxBudgetTokens are clamped (no error).
	BudgetTokens int

	// IncludeTiers opts tiers in. Nil/empty means {Pinned, Frame,
	// Outcomes} — the default. Useful for test isolation and for
	// callers that want a Pinned-only bundle at very tight budgets.
	IncludeTiers []Tier

	// OutcomeLimit caps the Outcomes tier at top-N most-recent events.
	// Zero defaults to DefaultOutcomeLimit (3) per research/03 §2.1.
	OutcomeLimit int

	// Form selects the rendered granularity. Defaults to query.FormMedium
	// per research/03 §7 ("medium ≤ 200 tok | normal cold-start bundle
	// inclusion"). FormShort and FormFull are also accepted; FormFull
	// is rendered live from typed Data (the same code path as query).
	Form query.FormKind

	// Scope is the optional CortexScope (Phase 10). When non-nil it is
	// applied as a per-tier filter: only memories whose Head satisfies
	// scope.Allows survive into the bundle. Verification is performed
	// once at the entry to Context (signature, snapshot resolvability,
	// multi-proof) via VerifyScope; subsequent tier scans treat the
	// scope as authenticated.
	//
	// Scope.BudgetTokens, when non-zero, caps opts.BudgetTokens
	// regardless of caller request — the scope's cap is always
	// honored (research/06-agents.md §7.1).
	Scope *scope.Scope

	// Now is the wall-clock used for scope expiry comparison. Zero
	// defers to c.now(). Tests pass a fixed time.
	Now time.Time
}

// Bundle is the response shape from Cortex.Context. Mirrors research/03
// §2.4 ({pinned, frame_relevant, outcomes, reachable_uris,
// compile_metadata}).
//
// Memories in each tier slice are sorted by their tier's natural order:
//   - Pinned: salience desc
//   - FrameRelevant: salience desc
//   - Outcomes: created desc (then salience desc as tiebreak)
//
// Rendered + Tokens are keyed by memory ID and parallel-indexed across
// tiers (a memory is only reachable through one tier — see deduplication
// in Cortex.Context). Scores carries the (possibly floor-boosted)
// salience used for trim, exposed for debugging and audit.
type Bundle struct {
	Pinned        []*memory.Memory
	FrameRelevant []*memory.Memory
	Outcomes      []*memory.Memory

	// Rendered[id] is the per-memory rendered form (Form-selected).
	// Always populated for every memory in the three tier slices.
	Rendered map[memory.ID]string
	// Tokens[id] is memory.CountTokens(Rendered[id]), captured pre-trim
	// (so the audit arithmetic adds up).
	Tokens map[memory.ID]int
	// Scores[id] is the cold salience value used for trim. Pinned-tier
	// members carry salience.PinnedFloor as a floor (so they survive
	// budget pressure preferentially).
	Scores map[memory.ID]float32
	// ReachableURIs lists memories that matched a tier scan but did
	// NOT survive the budget trim. Capped at MaxReachableURIs. Callers
	// resolve these lazily via cortex.Resolve (research/03 §2.4 phrasing
	// "agent can resolve() them lazily").
	ReachableURIs []memory.URI

	// TotalTokens is the final post-trim sum of Tokens across surviving
	// memories.
	TotalTokens int
	// Trimmed is the count of memories dropped by budget enforcement.
	Trimmed int
	// LatencyMS is wall-clock from the start of Cortex.Context to the
	// return statement, populated by the composer. Reported in the
	// research/03 §2.4 compile_metadata field shape.
	LatencyMS int64
	// Form mirrors the FormKind used.
	Form query.FormKind
}

// Errors raised by Context.
var (
	// ErrNoTiersIncluded means IncludeTiers explicitly listed none of
	// the three tiers (after defaulting). Returned because an empty
	// bundle is almost certainly a caller mistake.
	ErrNoTiersIncluded = errors.New("cortex.Context: at least one tier must be included")
)

// Context composes the three-tier cold-start bundle described in
// research/03-retrieval-patterns.md §2 and research/04-cortex.md §12.1.
//
// Algorithm:
//  1. Normalize opts (defaults applied).
//  2. Run each requested tier sequentially:
//     - Pinned: type-scan Identity ∪ Constraint{Hard} ∪ Goal{Active}.
//     - FrameRelevant: idx/frame scan per (verb, kind, obj) tuple.
//     - Outcomes: idx/actor_obj scan per (verb, obj) tuple, top-N.
//  3. Deduplicate across tiers (a memory appears in exactly one tier:
//     earlier tier wins; Pinned > FrameRelevant > Outcomes).
//  4. Load Head + Version + cold salience for every surviving ID.
//  5. Render to opts.Form (default FormMedium).
//  6. Apply PinnedFloor to Pinned-tier members in the scores map.
//  7. Global salience-asc trim to BudgetTokens.
//  8. Trimmed-but-scanned IDs become ReachableURIs.
//
// Pure read path: no journal entries, no salience bumps. cortex_
// snapshot_hash is unchanged across the call.
func (c *Cortex) Context(opts ContextOpts) (*Bundle, error) {
	start := time.Now()

	// --- Step 0: scope verification (Phase 10) --------------------
	// VerifyScope runs the full chain (signature, schema version,
	// expiry, snapshot resolvability, multi-proof) ONCE at call
	// entry. Subsequent tier scans use opts.Scope.Allows for
	// per-candidate filtering without re-verifying.
	if opts.Scope != nil {
		nowForScope := opts.Now
		if nowForScope.IsZero() {
			nowForScope = c.now()
		}
		if err := c.VerifyScope(opts.Scope, nowForScope); err != nil {
			return nil, fmt.Errorf("cortex.Context: %w", err)
		}
		// Scope.BudgetTokens caps caller-requested budget. Spec
		// research/06-agents.md §7.1 ("hard cap on cortex.context
		// budget for this scope").
		if err := c.enforceContextBudget(opts.Scope, opts.BudgetTokens); err != nil {
			return nil, err
		}
	}

	// --- Step 1: normalize opts -----------------------------------
	if opts.BudgetTokens <= 0 {
		opts.BudgetTokens = DefaultBudgetTokens
	}
	if opts.BudgetTokens > MaxBudgetTokens {
		opts.BudgetTokens = MaxBudgetTokens
	}
	if opts.Scope != nil && opts.Scope.BudgetTokens > 0 && opts.BudgetTokens > opts.Scope.BudgetTokens {
		opts.BudgetTokens = opts.Scope.BudgetTokens
	}
	if opts.OutcomeLimit <= 0 {
		opts.OutcomeLimit = DefaultOutcomeLimit
	}
	if opts.Form == "" {
		opts.Form = query.FormMedium
	}
	includeSet := normalizeIncludeTiers(opts.IncludeTiers)
	if len(includeSet) == 0 {
		return nil, ErrNoTiersIncluded
	}

	// Validate the Objects map kinds up-front. Caller mistakes here
	// would otherwise surface as silently-empty Frame/Outcomes tiers.
	tuples, err := parseObjectTuples(opts.Objects)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()

	// Phase 12: load per-actor learned weights once for the live cold
	// score recompute below. Cold start (no key) returns DefaultWeights.
	weights, _, err := salience.ReadWeights(c.s)
	if err != nil {
		return nil, fmt.Errorf("cortex.Context: read weights: %w", err)
	}

	// --- Step 2 & 3: tier candidate IDs, with cross-tier dedup ----
	// dedup tracks which tier "owns" a memory (Pinned > Frame > Outcomes).
	// Avoids loading + rendering the same memory twice.
	dedup := map[memory.ID]Tier{}

	var (
		pinnedIDs   []memory.ID
		frameIDs    []memory.ID
		outcomeIDs  []memory.ID
		outcomeMeta map[memory.ID]int64 // id → createdUnixNano (for ordering)
	)

	if _, ok := includeSet[TierPinned]; ok {
		ids, err := c.tierPinned()
		if err != nil {
			return nil, fmt.Errorf("cortex.Context: pinned tier: %w", err)
		}
		for _, id := range ids {
			if _, dup := dedup[id]; dup {
				continue
			}
			dedup[id] = TierPinned
			pinnedIDs = append(pinnedIDs, id)
		}
	}

	// Dedup priority: Pinned > Outcomes > FrameRelevant. Outcomes runs
	// BEFORE FrameRelevant so an Event memory that carries both an
	// idx/frame entry and an idx/actor_obj entry lands in Outcomes
	// (time-ordered) rather than FrameRelevant (salience-ordered). This
	// matches the research/03 §9 worked-example narrative where
	// Frame-relevant held aggregate prefs/history-summaries and
	// Outcomes held discrete prior Events.
	if _, ok := includeSet[TierOutcomes]; ok && opts.Verb.Valid() && len(tuples) > 0 {
		ids, meta, err := c.tierOutcomes(opts.Verb, tuples, opts.OutcomeLimit)
		if err != nil {
			return nil, fmt.Errorf("cortex.Context: outcomes tier: %w", err)
		}
		outcomeMeta = meta
		for _, id := range ids {
			if _, dup := dedup[id]; dup {
				continue
			}
			dedup[id] = TierOutcomes
			outcomeIDs = append(outcomeIDs, id)
		}
	}

	if _, ok := includeSet[TierFrameRelevant]; ok && opts.Verb.Valid() && len(tuples) > 0 {
		ids, err := c.tierFrameRelevant(opts.Verb, tuples)
		if err != nil {
			return nil, fmt.Errorf("cortex.Context: frame-relevant tier: %w", err)
		}
		for _, id := range ids {
			if _, dup := dedup[id]; dup {
				continue
			}
			dedup[id] = TierFrameRelevant
			frameIDs = append(frameIDs, id)
		}
	}

	// --- Step 4: load memories + salience for every surviving ID --
	allIDs := make([]memory.ID, 0, len(pinnedIDs)+len(frameIDs)+len(outcomeIDs))
	allIDs = append(allIDs, pinnedIDs...)
	allIDs = append(allIDs, frameIDs...)
	allIDs = append(allIDs, outcomeIDs...)

	loaded := map[memory.ID]*memory.Memory{}
	scores := map[memory.ID]float32{}
	for _, id := range allIDs {
		mem, err := c.ResolveLatest(id)
		if err != nil {
			// A missing memory is a stale idx entry (e.g. tombstoned-
			// then-gc'd in a hypothetical future world). Skip silently;
			// stale idx entries are bug-class, not call-fail-class.
			if errors.Is(err, memory.ErrNotFound) {
				delete(dedup, id)
				continue
			}
			return nil, fmt.Errorf("cortex.Context: load %s: %w", id, err)
		}
		// Phase 10: scope post-filter. opts.Scope was VerifyScope'd
		// at Step 0; per-candidate Allows is silent (no journal
		// violation entry) since Context is a multi-target read,
		// not a single-target attempt.
		if opts.Scope != nil && !opts.Scope.Allows(&mem.Head) {
			delete(dedup, id)
			continue
		}
		// Tombstone post-filter: Constraint/Goal scans pre-filter,
		// but FrameRelevant/Outcomes idx entries are not deleted on
		// tombstone (idx/* derived, see Tombstone in cortex.go), so
		// we filter here as well. Defensive double-check on Pinned.
		if mem.Head.Tombstoned != nil {
			delete(dedup, id)
			continue
		}
		loaded[id] = mem

		// Phase 12: salience computed live from the persisted factor
		// inputs using the actor's current learned weights (see find.go
		// comment block for why sc.Cached is not consulted directly).
		sc, ok, err := salience.Read(c.s, id)
		if err != nil {
			return nil, fmt.Errorf("cortex.Context: salience read %s: %w", id, err)
		}
		var score float32
		if ok {
			score = salience.ColdScoreWith(sc, weights, now)
		} else {
			seed := salience.Score{
				LastUsed:   mem.Version.CreatedAt.UnixNano(),
				Importance: mem.Head.DeclaredImportance,
			}
			score = salience.ColdScoreWith(&seed, weights, now)
		}
		// Step 6 (eager): Pinned-tier floor. Applied here so the
		// trim step downstream sees a single unified score map.
		// Spec: research/04 §8.2 "Pinned floor 0.7" — applied via
		// tier membership since salience.Score.Pinned is unset in
		// Phase 8 (pin/demote UI ships in Phase 12).
		if dedup[id] == TierPinned && score < salience.PinnedFloor {
			score = salience.PinnedFloor
		}
		scores[id] = score
	}

	// --- Step 5: render forms ------------------------------------
	rendered := map[memory.ID]string{}
	tokens := map[memory.ID]int{}
	for id, mem := range loaded {
		text, err := renderForBundle(mem, opts.Form)
		if err != nil {
			return nil, fmt.Errorf("cortex.Context: render %s: %w", id, err)
		}
		rendered[id] = text
		tokens[id] = memory.CountTokens(text)
	}

	// --- Step 7: budget trim --------------------------------------
	// Survivor IDs (those that survive the budget) split back into
	// their tier slices in the natural per-tier order.
	survivors, reachable, total, trimmedCount := trimContextByBudget(
		loaded, scores, tokens, opts.BudgetTokens,
	)

	// --- Step 8: assemble Bundle ---------------------------------
	bundle := &Bundle{
		Rendered:    rendered,
		Tokens:      tokens,
		Scores:      scores,
		Form:        opts.Form,
		TotalTokens: total,
		Trimmed:     trimmedCount,
	}
	salienceCmp := makeSalienceDescOrder(scores)
	bundle.Pinned = orderByTier(loaded, intersect(survivors, pinnedIDs), salienceCmp)
	bundle.FrameRelevant = orderByTier(loaded, intersect(survivors, frameIDs), salienceCmp)
	bundle.Outcomes = orderByTier(loaded, intersect(survivors, outcomeIDs), makeCreatedDescOrder(outcomeMeta, scores))

	bundle.ReachableURIs = buildReachableURIs(loaded, reachable)
	bundle.LatencyMS = time.Since(start).Milliseconds()
	return bundle, nil
}

// --- Tier scanners --------------------------------------------------------

// tierPinned returns the union of Identity ∪ Constraint{Strength=Hard}
// ∪ Goal{Status=Active} IDs, with tombstoned heads filtered. Ordering
// is the union scan order (caller orders by salience desc downstream).
//
// Constraint and Goal need a Data decode to read StrengthVal / Status;
// the cost is one Pebble Get per type-scan hit. Acceptable: realistic
// pinned populations are O(10s) per actor (one identity, a handful of
// hard constraints, a few active goals).
func (c *Cortex) tierPinned() ([]memory.ID, error) {
	var out []memory.ID
	seen := map[memory.ID]struct{}{}

	add := func(id memory.ID) {
		if _, dup := seen[id]; dup {
			return
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}

	// Identity: every memory of type Identity is pinned.
	identityIDs, err := c.scanTypePrefix(memory.TypeIdentity)
	if err != nil {
		return nil, err
	}
	for _, id := range identityIDs {
		add(id)
	}

	// Constraint: pinned only if StrengthVal == StrengthHard.
	constraintIDs, err := c.scanTypePrefix(memory.TypeConstraint)
	if err != nil {
		return nil, err
	}
	for _, id := range constraintIDs {
		mem, err := c.ResolveLatest(id)
		if err != nil {
			if errors.Is(err, memory.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if mem.Head.Tombstoned != nil {
			continue
		}
		data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
		if err != nil {
			return nil, fmt.Errorf("cortex.tierPinned: decode constraint %s: %w", id, err)
		}
		cd, ok := data.(memory.ConstraintData)
		if !ok {
			continue
		}
		if cd.StrengthVal == memory.StrengthHard {
			add(id)
		}
	}

	// Goal: pinned only if Status == GoalActive.
	goalIDs, err := c.scanTypePrefix(memory.TypeGoal)
	if err != nil {
		return nil, err
	}
	for _, id := range goalIDs {
		mem, err := c.ResolveLatest(id)
		if err != nil {
			if errors.Is(err, memory.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if mem.Head.Tombstoned != nil {
			continue
		}
		data, err := memory.DecodeData(mem.Version.Type, mem.Version.Data)
		if err != nil {
			return nil, fmt.Errorf("cortex.tierPinned: decode goal %s: %w", id, err)
		}
		gd, ok := data.(memory.GoalData)
		if !ok {
			continue
		}
		if gd.Status == memory.GoalActive {
			add(id)
		}
	}

	return out, nil
}

// tierFrameRelevant scans idx/frame for each (verb, kind, ref) tuple
// and returns the deduped ID list. Order: idx scan order, which is
// byte-sort over the trailing memory ULID — equivalent to creation
// order under ULID monotonicity. Caller re-orders by salience.
func (c *Cortex) tierFrameRelevant(verb memory.Verb, tuples []objectTuple) ([]memory.ID, error) {
	var out []memory.ID
	seen := map[memory.ID]struct{}{}
	for _, t := range tuples {
		prefix := keys.IdxFramePrefixVerbKindObj(byte(verb), byte(t.kind), t.hash)
		err := c.s.PrefixIter(prefix, func(k, _ []byte) error {
			_, _, _, id, perr := keys.ParseIdxFrameKey(k)
			if perr != nil {
				return fmt.Errorf("cortex.tierFrameRelevant: parse idx/frame: %w", perr)
			}
			var mid memory.ID
			copy(mid[:], id[:])
			if _, dup := seen[mid]; dup {
				return nil
			}
			seen[mid] = struct{}{}
			out = append(out, mid)
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// tierOutcomes scans idx/actor_obj for each (verb, ref) tuple, dedups,
// and returns the top-OutcomeLimit IDs ordered by created desc. Also
// returns a map[id]→createdUnixNano so the caller can sort the Outcomes
// slice deterministically (idx scan order is created asc; we reverse).
func (c *Cortex) tierOutcomes(verb memory.Verb, tuples []objectTuple, limit int) ([]memory.ID, map[memory.ID]int64, error) {
	type entry struct {
		id      memory.ID
		created int64
	}
	var hits []entry
	seen := map[memory.ID]int64{} // memory ID → most-recent created seen for it
	for _, t := range tuples {
		prefix := keys.IdxActorObjPrefixVerbObj(byte(verb), t.hash)
		err := c.s.PrefixIter(prefix, func(k, _ []byte) error {
			_, _, created, id, perr := keys.ParseIdxActorObjKey(k)
			if perr != nil {
				return fmt.Errorf("cortex.tierOutcomes: parse idx/actor_obj: %w", perr)
			}
			var mid memory.ID
			copy(mid[:], id[:])
			if prev, dup := seen[mid]; dup {
				if int64(created) > prev {
					seen[mid] = int64(created)
				}
				return nil
			}
			seen[mid] = int64(created)
			hits = append(hits, entry{id: mid, created: int64(created)})
			return nil
		})
		if err != nil {
			return nil, nil, err
		}
	}
	// Sort by created desc (most recent first), tiebreak by ULID asc.
	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].created != hits[j].created {
			return hits[i].created > hits[j].created
		}
		// ULID byte order ascending — deterministic tiebreak.
		for k := 0; k < len(hits[i].id); k++ {
			if hits[i].id[k] != hits[j].id[k] {
				return hits[i].id[k] < hits[j].id[k]
			}
		}
		return false
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	out := make([]memory.ID, 0, len(hits))
	meta := make(map[memory.ID]int64, len(hits))
	for _, h := range hits {
		out = append(out, h.id)
		meta[h.id] = h.created
	}
	return out, meta, nil
}

// scanTypePrefix returns every memory ID under idx/type/<t>. Shared by
// the Pinned-tier helpers (Identity/Constraint/Goal). Distinct from
// the public ListByType only in that it returns an internal slice
// without bounds (the Pinned-tier sizes are inherently small).
func (c *Cortex) scanTypePrefix(t memory.Type) ([]memory.ID, error) {
	if !t.Valid() {
		return nil, memory.ErrInvalidType
	}
	prefix := keys.IdxTypePrefix(byte(t))
	var out []memory.ID
	err := c.s.PrefixIter(prefix, func(k, _ []byte) error {
		_, _, id, perr := keys.ParseIdxTypeKey(k)
		if perr != nil {
			return fmt.Errorf("cortex.scanTypePrefix: parse idx/type: %w", perr)
		}
		var mid memory.ID
		copy(mid[:], id[:])
		out = append(out, mid)
		return nil
	})
	return out, err
}

// --- Helpers --------------------------------------------------------------

// objectTuple is the resolved (kind, ref, hash) form of one entry in
// ContextOpts.Objects, computed once at the top of Context so each
// tier scan reuses the same hash.
type objectTuple struct {
	kind memory.ObjKind
	ref  string
	hash [memory.ObjHashSize]byte
}

// parseObjectTuples converts the input map (kind name → ref string)
// into typed objectTuples. Returns memory.ErrInvalidObjKind for any
// unknown kind name (research/03 §8 enforcement discipline: no silent
// drop).
func parseObjectTuples(objs map[string]string) ([]objectTuple, error) {
	if len(objs) == 0 {
		return nil, nil
	}
	// Deterministic order: sort by kind name then by ref. This keeps
	// idx scan order independent of map iteration randomness and makes
	// test assertions stable.
	names := make([]string, 0, len(objs))
	for k := range objs {
		names = append(names, k)
	}
	sort.Strings(names)

	out := make([]objectTuple, 0, len(objs))
	for _, name := range names {
		kind, ok := memory.ParseObjKind(name)
		if !ok {
			return nil, fmt.Errorf("%w: %q", memory.ErrInvalidObjKind, name)
		}
		ref := objs[name]
		if ref == "" {
			return nil, fmt.Errorf("%w: kind=%s", memory.ErrEmptyObjRef, name)
		}
		out = append(out, objectTuple{
			kind: kind,
			ref:  ref,
			hash: memory.ObjHash(ref),
		})
	}
	return out, nil
}

// normalizeIncludeTiers turns the caller-provided IncludeTiers slice
// into a set. Empty/nil defaults to all three tiers. Unknown tier
// values are silently ignored (extension-friendly: a v2 tier added
// here wouldn't break v1 callers).
func normalizeIncludeTiers(in []Tier) map[Tier]struct{} {
	out := map[Tier]struct{}{}
	if len(in) == 0 {
		out[TierPinned] = struct{}{}
		out[TierFrameRelevant] = struct{}{}
		out[TierOutcomes] = struct{}{}
		return out
	}
	for _, t := range in {
		switch t {
		case TierPinned, TierFrameRelevant, TierOutcomes:
			out[t] = struct{}{}
		}
	}
	return out
}

// renderForBundle returns the rendered form for the chosen FormKind.
// Mirrors query.renderMemory (kept private to avoid the cortex package
// importing into query for one function). Short and Medium come from
// the persisted Forms on the Version (byte-stable since write-time
// render); Full is rendered live from typed Data.
func renderForBundle(m *memory.Memory, form query.FormKind) (string, error) {
	switch form {
	case query.FormShort:
		return m.Version.Forms.Short, nil
	case query.FormMedium:
		return m.Version.Forms.Medium, nil
	case query.FormFull:
		data, err := memory.DecodeData(m.Version.Type, m.Version.Data)
		if err != nil {
			return "", fmt.Errorf("decode data: %w", err)
		}
		return forms.RenderFull(&m.Head, data), nil
	}
	return "", fmt.Errorf("unknown FormKind %q", form)
}

// trimContextByBudget drops low-salience memories until the rendered
// token sum is ≤ budget. Mirrors query.trimByBudget but returns the
// survivor set + reachable set rather than mutating slices in place,
// because the bundle composer splits survivors back into per-tier
// slices afterwards.
//
// Always retains at least one memory when the input is non-empty (the
// Find-engine "≥1 relief valve" convention; matches matrix.ctx
// invariant). Reachable IDs are returned in trim order (= drop order
// = salience asc), capped at MaxReachableURIs.
func trimContextByBudget(
	loaded map[memory.ID]*memory.Memory,
	scores map[memory.ID]float32,
	tokens map[memory.ID]int,
	budget int,
) (survivors map[memory.ID]struct{}, reachable []memory.ID, total int, trimmed int) {
	survivors = map[memory.ID]struct{}{}
	for id := range loaded {
		survivors[id] = struct{}{}
		total += tokens[id]
	}
	if total <= budget {
		return
	}

	// Order all loaded IDs by salience asc (= drop priority); ULID
	// asc tiebreak for determinism. We must list IDs in a stable
	// order BEFORE sorting because map iteration in Go is random.
	ids := make([]memory.ID, 0, len(loaded))
	for id := range loaded {
		ids = append(ids, id)
	}
	sort.SliceStable(ids, func(i, j int) bool {
		si, sj := scores[ids[i]], scores[ids[j]]
		if si != sj {
			return si < sj
		}
		// Tiebreak: ULID asc (oldest first dropped at equal salience).
		for k := 0; k < len(ids[i]); k++ {
			if ids[i][k] != ids[j][k] {
				return ids[i][k] < ids[j][k]
			}
		}
		return false
	})

	for _, id := range ids {
		if total <= budget {
			break
		}
		// Always keep at least one memory.
		if len(survivors) <= 1 {
			break
		}
		delete(survivors, id)
		total -= tokens[id]
		reachable = append(reachable, id)
		trimmed++
	}
	return
}

// --- ordering helpers (in-package, not exported) -------------------------

// orderFn returns true if a should sort before b. Mirrors sort.Less
// semantics so it plugs directly into sort.SliceStable.
type orderFn func(a, b memory.ID) bool

// idLess returns ULID-ascending order; used as the final tiebreak so
// all sort orderings remain deterministic across runs.
func idLess(a, b memory.ID) bool {
	for k := 0; k < len(a); k++ {
		if a[k] != b[k] {
			return a[k] < b[k]
		}
	}
	return false
}

// makeSalienceDescOrder returns a comparator that ranks by salience
// descending, ULID ascending as tiebreak. Used by Pinned and
// FrameRelevant tiers.
func makeSalienceDescOrder(scores map[memory.ID]float32) orderFn {
	return func(a, b memory.ID) bool {
		sa, sb := scores[a], scores[b]
		if sa != sb {
			return sa > sb
		}
		return idLess(a, b)
	}
}

// makeCreatedDescOrder returns a comparator that ranks by created
// timestamp descending (most recent first), salience desc as secondary,
// ULID ascending as tertiary. Used by the Outcomes tier per
// research/04 §12.1 "scan, top-N" with most-recent-first semantics.
func makeCreatedDescOrder(meta map[memory.ID]int64, scores map[memory.ID]float32) orderFn {
	return func(a, b memory.ID) bool {
		ca, cb := meta[a], meta[b]
		if ca != cb {
			return ca > cb
		}
		sa, sb := scores[a], scores[b]
		if sa != sb {
			return sa > sb
		}
		return idLess(a, b)
	}
}

// orderByTier filters and orders a list of survivor IDs into a slice
// of *memory.Memory using the provided comparator. IDs absent from
// `loaded` (rare race with stale idx entries) are silently dropped.
func orderByTier(loaded map[memory.ID]*memory.Memory, ids []memory.ID, cmp orderFn) []*memory.Memory {
	if len(ids) == 0 {
		return nil
	}
	sorted := append([]memory.ID(nil), ids...)
	sort.SliceStable(sorted, func(i, j int) bool { return cmp(sorted[i], sorted[j]) })
	out := make([]*memory.Memory, 0, len(sorted))
	for _, id := range sorted {
		if m, ok := loaded[id]; ok {
			out = append(out, m)
		}
	}
	return out
}

// intersect returns the elements of ids that survive (= are present in
// the survivor set). Preserves input order so per-tier natural order
// is retained after the cross-tier trim. O(n) in the input slice.
func intersect(survivors map[memory.ID]struct{}, ids []memory.ID) []memory.ID {
	out := make([]memory.ID, 0, len(ids))
	for _, id := range ids {
		if _, ok := survivors[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

// buildReachableURIs converts the trimmed-but-loaded IDs into canonical
// matrix://cortex/... URIs, capped at MaxReachableURIs to keep response
// size bounded.
func buildReachableURIs(loaded map[memory.ID]*memory.Memory, trimmed []memory.ID) []memory.URI {
	if len(trimmed) == 0 {
		return nil
	}
	limit := len(trimmed)
	if limit > MaxReachableURIs {
		limit = MaxReachableURIs
	}
	out := make([]memory.URI, 0, limit)
	for i := 0; i < limit; i++ {
		mem, ok := loaded[trimmed[i]]
		if !ok {
			continue
		}
		out = append(out, BuildURI(mem.Head.Type, mem.Head.ID, mem.Head.CurrentVersion))
	}
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
