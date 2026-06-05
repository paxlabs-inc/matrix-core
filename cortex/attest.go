// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Phase 11.5 + Phase 12: intent-attestation cortex-side primitive.
//
// Spec: research/04-cortex.md §8.3 "Per-actor weight learning":
//
//	Two updates fire on intent.attest:
//
//	On success:
//	- For each cited memory in the plan, increment cite_in_successful_plans.
//	- Compute the actual ranking that produced this success ...
//	- EMA-update the actor's weights toward the high-performing weighting.
//
//	On failure with reason factual_error or wrong_assumption:
//	- For memories cited at the failed step, decrement
//	  cite_in_successful_plans (or accrue a separate cited_in_failed_plans).
//	- EMA-update weights away from the weighting that ranked the bad
//	  memory highly.
//
// Phase 11.5 implemented the salience-side half (Citations + AccessCount
// bumps + KindAttest journal-of-record). Phase 12 wires the second half:
// after the citation bumps land, the per-cited-memory factor profile is
// averaged and ColdScoreWith-style EMA-pulled into the actor's persisted
// Weights record at meta/salience_weights. A second journal entry
// (KindLearnWeights) is appended in the SAME batch so the (citation
// bumps, weight update) pair is atomic and replay-deterministic.
//
// MCL message kind boundary. The MCL intent.attest message kind
// (research/02-protocol.md §3) lives in the agent runtime. Agents call
// cortex.Attest as the cortex-side primitive after they've validated the
// signed envelope + resolved the cited URIs to memory IDs. This package
// does not parse MCL envelopes.
//
// Atomicity. cortex.Attest commits TWO journal entries (KindAttest at
// seq=N then KindLearnWeights at seq=N+1) plus one salience write per
// cited memory plus one meta/salience_weights write in a single atomic
// batch via store.BeginWrite, matching the §11.1 batch-atomicity pattern
// used by Write/Update/Tombstone/Compact/UpdateHead/AddEdge/RemoveEdge.
// When the EMA step degenerates (empty cited set survives tombstone-skip,
// or all-zero factor profile), the weight update is still journaled with
// LearnWeightsPayload.Skipped=true and NewW*==PrevW* so the seq-pairing
// invariant (KindAttest immediately followed by KindLearnWeights) holds
// unconditionally.
//
// Replay determinism. AttestPayload.CitedIDs is the post-validation
// (tombstone-skipped) memory ID list. The replay harness walks j/ for
// KindAttest entries and re-applies the same BumpForCitation /
// DecrementCitation calls to reconstruct salience.Citations exactly. For
// KindLearnWeights, the replay harness re-applies the EMA step at the
// matching seq=N+1 to reconstruct meta/salience_weights byte-identically;
// because the EMA target is the post-bump factor profile and the bumps
// landed atomically at seq=N, the profile is reproducible from the same
// starting state + same KindAttest payload.
//
// MMR + SMT participation. KindAttest AND KindLearnWeights are BOTH
// staged into the journal MMR (the JournalHook installed in cortex.New
// fires unconditionally on every j/ write). Cortex.Attest does NOT stage
// a memories-SMT update — the Head canonical bytes are untouched (Attest
// only mutates salience + meta/salience_weights, which are derived not
// canonical; see Phase 7 anchor-namespace decision). Phase 7 only anchors
// memories+edges; salience.Cached / AccessCount / Citations /
// meta/salience_weights are not in OverallRoot. Replay determinism still
// holds because both KindAttest and KindLearnWeights leaves are anchored
// in journal_root (which IS in OverallRoot).

package cortex

import (
	"errors"
	"fmt"

	"matrix/cortex/journal"
	"matrix/cortex/keys"
	"matrix/cortex/memory"
	"matrix/cortex/salience"
)

// LearnWeightsSchemaVersion is stamped on every emitted LearnWeightsPayload.
const LearnWeightsSchemaVersion uint8 = 1

// AttestSchemaVersion is stamped on every emitted AttestPayload. Bumping
// requires a journal-kind migration (writers honor the new shape, the
// replay harness recognizes both old and new for one transition window).
const AttestSchemaVersion uint8 = 1

// MaxCitedURIsPerAttest caps the number of URIs in one cortex.Attest call.
// 256 is generous compared to typical plan depth (3–10 cited memories per
// step per research/03 §2.4 worked examples) but bounds journal payload
// growth at ~4 KiB per attest. Callers attesting more than 256 memories
// should split the attest across steps.
const MaxCitedURIsPerAttest = 256

// AttestOutcome re-exports the journal-package enum so call sites depend
// only on cortex.{Attest, AttestOutcomeSuccess, AttestOutcomeFailure}
// rather than reaching into the journal package.
type AttestOutcome = journal.AttestOutcome

const (
	// AttestOutcomeSuccess: the intent's success criteria were met.
	// Cited memories' Citations and AccessCount both increment by 1.
	AttestOutcomeSuccess = journal.AttestOutcomeSuccess

	// AttestOutcomeFailure: the intent failed. If Reason ∈
	// {AttestReasonFactualError, AttestReasonWrongAssumption} then
	// Citations decrements (floor 0) per §8.3; otherwise salience is
	// untouched but the attest is still journaled for audit.
	AttestOutcomeFailure = journal.AttestOutcomeFailure
)

// AttestReason* re-exports for the same ergonomic reason as
// AttestOutcome*. Any string is accepted as Reason; only these two
// trigger Citations decrement per spec §8.3.
const (
	AttestReasonFactualError    = journal.AttestReasonFactualError
	AttestReasonWrongAssumption = journal.AttestReasonWrongAssumption
)

// Errors raised by Cortex.Attest.
var (
	// ErrEmptyCitations is returned when Cited is empty. An attest with
	// no cited URIs cannot inform salience and is rejected at the API
	// boundary rather than journaled as a no-op.
	ErrEmptyCitations = errors.New("cortex.Attest: empty Cited")

	// ErrTooManyCitations is returned when len(Cited) >
	// MaxCitedURIsPerAttest.
	ErrTooManyCitations = errors.New("cortex.Attest: too many cited URIs")

	// ErrInvalidOutcome is returned when AttestOutcome is not one of
	// the closed-enum values.
	ErrInvalidOutcome = errors.New("cortex.Attest: invalid Outcome")

	// ErrAttestEmptyIntentID is returned when IntentID is empty. An
	// attest without an intent reference cannot be audited and is
	// rejected. Named distinctly from ErrEmptyIntentID (Compact) so
	// caller error chains can distinguish the source primitive.
	ErrAttestEmptyIntentID = errors.New("cortex.Attest: empty IntentID")
)

// AttestOpts is the input shape for cortex.Attest.
//
// IntentID identifies the intent whose attestation this records. Must
// match the IntentID embedded in the signed MCL envelope; cortex does
// not validate the relationship (that's the agent runtime's job).
//
// Outcome is success or failure. Reason is free-form for audit but only
// matches AttestReasonFactualError or AttestReasonWrongAssumption trigger
// the Citations decrement on failure (§8.3).
//
// Cited is the list of memory URIs that the plan cited. Each URI must
// resolve to a Head that is currently live (not tombstoned). Versions in
// the URI are used to locate the memory but the salience bump targets
// the Head (not the version) — salience is a per-memory signal, not
// per-version.
//
// CreatedBy is the agent ref recorded in the journal entry for audit.
// Empty is allowed.
type AttestOpts struct {
	IntentID  string
	Outcome   AttestOutcome
	Reason    string
	Cited     []memory.URI
	CreatedBy string
}

// AttestResult summarizes what cortex.Attest did. Useful for tests, CLI
// dumps, and the agent runtime's attestation flow that wants to log the
// outcome.
type AttestResult struct {
	// Seq is the journal seq allocated to the KindAttest entry. The
	// matching MMR leaf hash is committed at j/<seq> + accum/mmr/n/<pos>.
	// The matching KindLearnWeights entry lives at Seq+1.
	Seq uint64

	// LearnSeq is the journal seq allocated to the KindLearnWeights
	// entry emitted in the same batch as KindAttest. Always Seq+1
	// when AttestResult is non-nil. Exposed for tests + audit tooling
	// that needs to fetch the matching EMA record without scanning.
	LearnSeq uint64

	// AffectedIDs is the per-URI memory ID list whose salience was
	// mutated. Excludes URIs that pointed to tombstoned memories or
	// missing heads (silently skipped — agent runtime is responsible
	// for not citing nonexistent memories, but if it does we don't
	// fail the attest just because one URI is stale; the audit still
	// captures the rest of the citations).
	AffectedIDs []memory.ID

	// SkippedURIs is the per-URI list that was dropped: tombstoned
	// memory, missing head, or malformed URI. Returned so callers can
	// surface a warning to the agent. Order matches Opts.Cited.
	SkippedURIs []memory.URI

	// CitationsDelta is +1 on success, -1 on failure-with-{factual_error,
	// wrong_assumption}, 0 otherwise. AccessCount delta is always +1 per
	// AffectedID on success (citation implies access); on failure
	// AccessCount is NOT changed. Reported for ops visibility.
	CitationsDelta int

	// PrevWeights is the per-actor salience.Weights at the moment of
	// this attest, before the EMA step. Equal to salience.DefaultWeights()
	// when meta/salience_weights was absent (cold start). Exposed so
	// callers can log/inspect the weight transition without re-reading
	// the journal.
	PrevWeights salience.Weights

	// NewWeights is the per-actor salience.Weights persisted to
	// meta/salience_weights by this attest. Equal to PrevWeights when
	// WeightsUpdated is false (degenerate EMA: empty post-skip cited set
	// or all-zero factor profile).
	NewWeights salience.Weights

	// WeightsUpdated is true iff the EMA step was applied (i.e. it was
	// not a no-op due to degenerate inputs). When false, NewWeights ==
	// PrevWeights and the journal entry has Skipped=true.
	WeightsUpdated bool
}

// Attest records an intent.attestation outcome and applies the
// per-memory salience updates from research/04 §8.3.
//
// Side effects per AffectedID:
//   - Outcome=Success: salience.BumpForCitation (Citations++ AccessCount++)
//   - Outcome=Failure + Reason ∈ §8.3 set: salience.DecrementCitation
//   - Outcome=Failure + other Reason: no salience mutation
//
// Plus one journal entry (KindAttest) carrying AffectedIDs for replay
// determinism. All writes commit atomically via store.BeginWrite.
//
// Latency target: O(len(Cited)) Pebble point reads + writes; expected
// p50 < 5 ms for typical 3–10 citation attests.
func (c *Cortex) Attest(opts AttestOpts) (*AttestResult, error) {
	if opts.IntentID == "" {
		return nil, ErrAttestEmptyIntentID
	}
	if len(opts.Cited) == 0 {
		return nil, ErrEmptyCitations
	}
	if len(opts.Cited) > MaxCitedURIsPerAttest {
		return nil, fmt.Errorf("%w: got %d, max %d", ErrTooManyCitations, len(opts.Cited), MaxCitedURIsPerAttest)
	}
	switch opts.Outcome {
	case AttestOutcomeSuccess, AttestOutcomeFailure:
	default:
		return nil, fmt.Errorf("%w: %d", ErrInvalidOutcome, opts.Outcome)
	}

	now := c.now()

	// Phase 14 R3b gate: bound the Pebble-sync + dual-journal-entry +
	// salience-write cost a malicious sub-agent can impose by looping
	// Attest on the same intent. Token bucket keyed by (actor,
	// intent_id) per matrix.ctx OPEN_Q_LOCKS line 603. Over-rate calls
	// fail fast with ErrAttestRateLimited; no journal entries are
	// emitted, no salience bumps fire, no EMA step is applied.
	// Replay determinism is preserved because the dropped attest was
	// never journaled, so the replay walk never re-derives it. See
	// ratelimit.go Q5 lock for the error-sentinel rationale.
	if !c.rl.allowAttest(c.s.Actor(), opts.IntentID, now) {
		return nil, ErrAttestRateLimited
	}

	// Pre-resolve every cited URI to a live Head. URIs that can't be
	// parsed, point to missing/tombstoned heads, or whose ID has
	// already appeared in this batch are dropped into SkippedURIs.
	// We do this BEFORE BeginWrite so we don't hold the seqMu across
	// I/O reads.
	seen := make(map[memory.ID]bool, len(opts.Cited))
	affected := make([]memory.ID, 0, len(opts.Cited))
	skipped := make([]memory.URI, 0)
	for _, uri := range opts.Cited {
		_, id, _, perr := ParseURI(uri)
		if perr != nil {
			skipped = append(skipped, uri)
			continue
		}
		if seen[id] {
			// Same memory cited twice in one attest: dedup. Salience
			// is per-memory; double-citing should not double-bump.
			continue
		}
		headBytes, ok, err := c.s.Get(keys.MemoryHeadKey(toKeysULID(id)))
		if err != nil {
			return nil, fmt.Errorf("cortex.Attest: get head: %w", err)
		}
		if !ok {
			skipped = append(skipped, uri)
			continue
		}
		var h memory.Head
		if err := memory.DecodeHead(headBytes, &h); err != nil {
			return nil, fmt.Errorf("cortex.Attest: decode head: %w", err)
		}
		if h.Tombstoned != nil {
			skipped = append(skipped, uri)
			continue
		}
		seen[id] = true
		affected = append(affected, id)
	}

	// Determine the salience-side effect from (Outcome, Reason).
	delta := citationsDelta(opts.Outcome, opts.Reason)

	// Build the journal payload. CitedIDs uses [16]byte so canonical
	// CBOR encoding is byte-stable regardless of slice header.
	citedIDs := make([][16]byte, 0, len(affected))
	for _, id := range affected {
		var arr [16]byte
		copy(arr[:], id[:])
		citedIDs = append(citedIDs, arr)
	}
	payload := &journal.AttestPayload{
		SchemaVersion: AttestSchemaVersion,
		IntentID:      opts.IntentID,
		Outcome:       opts.Outcome,
		Reason:        opts.Reason,
		CitedIDs:      citedIDs,
	}
	body, err := journal.EncodeAttestPayload(payload)
	if err != nil {
		return nil, fmt.Errorf("cortex.Attest: encode payload: %w", err)
	}

	// Load actor weights (cold-start fallback baked into ReadWeights).
	// Captured pre-batch so PrevWeights in the AttestResult reflects the
	// authoritative state at the moment of attest.
	prevWeights, _, err := salience.ReadWeights(c.s)
	if err != nil {
		return nil, fmt.Errorf("cortex.Attest: read weights: %w", err)
	}

	wb := c.s.BeginWrite()
	defer wb.Abort()

	// Apply salience deltas. Cache-miss falls back to NewForWrite so a
	// memory written pre-Phase-11.5 doesn't get skipped on its first
	// observed attest. Post-bump Scores are retained so the EMA step can
	// average their factor profiles without re-reading from store.
	postBumpScores := make([]salience.Score, 0, len(affected))
	for _, id := range affected {
		var u keys.ULID
		copy(u[:], id[:])
		var sc salience.Score
		raw, ok, err := c.s.Get(keys.SalienceKey(u))
		if err != nil {
			return nil, fmt.Errorf("cortex.Attest: read salience: %w", err)
		}
		if ok {
			if err := salience.Decode(raw, &sc); err != nil {
				return nil, fmt.Errorf("cortex.Attest: decode salience: %w", err)
			}
		} else {
			// Importance recovered from the Head we already read above
			// would require a second decode; the more conservative path
			// is to seed from zero-importance — the next Update/Touch
			// will refresh from Head.DeclaredImportance via BumpForUpdate.
			sc = salience.NewForWrite(0, now)
		}
		switch delta {
		case +1:
			salience.BumpForCitation(&sc, now)
		case -1:
			salience.DecrementCitation(&sc, now)
		default:
			// No salience mutation but we still write to refresh
			// LastUsed — the attest IS a touch of these memories.
			sc.LastUsed = now.UnixNano()
			sc.Cached = salience.ColdScore(&sc, now)
			sc.ComputedAt = now.UnixNano()
		}
		encoded, err := salience.Encode(&sc)
		if err != nil {
			return nil, fmt.Errorf("cortex.Attest: encode salience: %w", err)
		}
		if err := wb.Set(keys.SalienceKey(u), encoded); err != nil {
			return nil, fmt.Errorf("cortex.Attest: set salience: %w", err)
		}
		postBumpScores = append(postBumpScores, sc)
	}

	je := &journal.Entry{
		Kind:      journal.KindAttest,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(opts.CreatedBy),
		Payload:   body,
	}
	if err := wb.AppendJournal(je); err != nil {
		return nil, fmt.Errorf("cortex.Attest: append journal: %w", err)
	}
	attestSeq := wb.Seq()

	// Compute and stage the EMA weight update. The EMA pull direction
	// matches the salience.Citations delta: success (+1) and
	// non-decrement-reason failure (delta=0) both pull TOWARD the
	// cited-memories' profile, while decrement-reason failure (-1)
	// pulls AWAY. This keeps the spec's bandit-lite shape: success +
	// citations evidence both reinforce the actor's current weighting;
	// a decrement-reason failure is the only direction-reversing signal.
	decrementOnFailure := delta == -1
	newWeights := prevWeights
	weightsUpdated := salience.UpdateWeightsEMA(&newWeights, postBumpScores, salience.EMARate, decrementOnFailure, now)
	lwPayload := &journal.LearnWeightsPayload{
		SchemaVersion:      LearnWeightsSchemaVersion,
		SourceSeq:          attestSeq,
		Alpha:              salience.EMARate,
		DecrementOnFailure: decrementOnFailure,
		Skipped:            !weightsUpdated,
		PrevWR:             prevWeights.WR,
		PrevWA:             prevWeights.WA,
		PrevWC:             prevWeights.WC,
		PrevWD:             prevWeights.WD,
		PrevWV:             prevWeights.WV,
		NewWR:              newWeights.WR,
		NewWA:              newWeights.WA,
		NewWC:              newWeights.WC,
		NewWD:              newWeights.WD,
		NewWV:              newWeights.WV,
	}
	lwBody, err := journal.EncodeLearnWeightsPayload(lwPayload)
	if err != nil {
		return nil, fmt.Errorf("cortex.Attest: encode learn-weights payload: %w", err)
	}
	if weightsUpdated {
		encodedWeights, err := salience.EncodeWeights(&newWeights)
		if err != nil {
			return nil, fmt.Errorf("cortex.Attest: encode weights: %w", err)
		}
		if err := wb.Set(keys.MetaSalienceWeights, encodedWeights); err != nil {
			return nil, fmt.Errorf("cortex.Attest: set weights: %w", err)
		}
	}
	lwEntry := &journal.Entry{
		Kind:      journal.KindLearnWeights,
		CreatedAt: now.UnixNano(),
		CreatedBy: []byte(opts.CreatedBy),
		Payload:   lwBody,
	}
	if err := wb.AppendJournal(lwEntry); err != nil {
		return nil, fmt.Errorf("cortex.Attest: append learn-weights journal: %w", err)
	}
	learnSeq := wb.Seq()

	if err := wb.Commit(); err != nil {
		return nil, fmt.Errorf("cortex.Attest: commit: %w", err)
	}

	return &AttestResult{
		Seq:            attestSeq,
		LearnSeq:       learnSeq,
		AffectedIDs:    affected,
		SkippedURIs:    skipped,
		CitationsDelta: delta,
		PrevWeights:    prevWeights,
		NewWeights:     newWeights,
		WeightsUpdated: weightsUpdated,
	}, nil
}

// citationsDelta returns the §8.3 salience.Citations delta for the given
// (outcome, reason) pair.
//
//	(success, *)                                          -> +1
//	(failure, factual_error)                              -> -1
//	(failure, wrong_assumption)                           -> -1
//	(failure, any other reason or empty)                  ->  0
func citationsDelta(outcome AttestOutcome, reason string) int {
	if outcome == AttestOutcomeSuccess {
		return +1
	}
	if outcome == AttestOutcomeFailure {
		switch reason {
		case AttestReasonFactualError, AttestReasonWrongAssumption:
			return -1
		}
	}
	return 0
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
