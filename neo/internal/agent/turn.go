// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// turn.go — the reified turn (MORPHEUS req.2): every piece of run state with
// turn lifetime is owned by a turn struct born at Chat entry, so per-turn
// resets cannot be forgotten and cross-turn leaks are impossible by
// construction. Constructing the turn IS the reset — Chat entry replaces the
// handle instead of zeroing fields one by one.
package agent

import (
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/o1"
)

// turn owns the run state scoped to ONE Chat turn. Contract: it is created by
// newTurn at Chat entry (and once at New so pre-turn calls are safe), mutated
// only from the single-threaded loop goroutine (the overflow store locks
// internally for the concurrent dispatch path), and abandoned wholesale when
// the next turn begins. The handle (Agent.turn) is retained AFTER Chat
// returns, so the turn-scoped records the supervisor reads between turns
// (lastFailureClass, lastDeath) stay reachable through Agent.LastFailureClass
// and Agent.LastDeath until the next turn replaces them.
type turn struct {
	// contract is Architect O1's executable, versioned statement of the
	// complete request. It is compiled before model execution and revised
	// monotonically when genuine user steering arrives.
	contract o1.TaskContract
	// runLedger is Architect O1's authoritative causal state for this turn.
	// It records real dispatch attempts, effects, verifier evidence, and the
	// terminal proof consumed by the supervisor.
	runLedger   *o1.RunLedger
	verifiers   o1.VerifierGraph
	manifests   map[string]o1.OperationManifest
	lastOutcome *o1.OperationOutcome

	// ledger is Mechanism 1's premise ledger — the current plan's load-bearing
	// factual premises with provenance (epistemic-core req.4). Seeded at plan
	// formation, nil until the first committing assistant turn, replaced on
	// plan change.
	ledger *premiseLedger

	// mismatchMeter / hypotheses are Mechanism 3's run state (epistemic-core
	// req.6): consecutive prediction mismatches per probe strategy, and each
	// strategy's live hypothesis premise on the ledger. Lazily allocated.
	mismatchMeter map[string]int
	hypotheses    map[string]*Premise

	// graph is Mechanism 4's reified task graph (epistemic-core req.7): goal →
	// subgoals → evidence, with convergence computed as evidence-set delta.
	graph *taskGraph

	// revisionPending, when non-empty, forces a tools-stripped reasoning-only
	// revision step at the next loop iteration (epistemic-core req.5.2/6.3/
	// 7.2); revisionsThisTurn bounds them (maxRevisionsPerTurn).
	revisionPending   string
	revisionsThisTurn int

	// premiseTail / graphTail are the FIXED epistemic tail slots (epistemic-core
	// req.3.1): after the activation/memory block, the premise ledger renders,
	// then the task graph — always in that order. The mechanisms render into
	// these slots the step a transition occurs.
	premiseTail string
	graphTail   string

	// Cassandra 2.0 silent-voice controller per-turn state: casModsThisTurn
	// caps modifications per turn (max_mods_per_turn); casCooldown records the
	// step each trigger CLASS last fired (per-trigger cooldown, lazily
	// allocated); casRecord is the dual-record audit ground truth —
	// {original_content, cassandra_mod, trigger, side, step, target} for every
	// modification (guardrail 3). None of it is durable: cortex keeps the
	// original (req.7.1) and the audit rides the side-channel.
	casModsThisTurn int
	casCooldown     map[modTrigger]int
	casRecord       []cassandraMod

	// autoshotCount bounds deterministic browser auto-captures this turn
	// (BROWSER-FILMSTRIP req.3 ac_3 — per-run cap).
	autoshotCount int

	// overflow holds this turn's oversized tool results spilled to run-scoped
	// files (neo-smoothness req.4) — the read-full latch. Created lazily on
	// the first overflow and cleaned up at turn end. Goroutine-safe internally
	// (the concurrent dispatch path may both cap and read).
	overflow *overflowStore

	// lastFailureClass records the shared FailureClass of the most recent
	// classified tool FAILURE this turn (delegate.ClassNone when none) —
	// the turn's failure-class scratch state, read by the task supervisor
	// through Agent.LastFailureClass after Chat returns. Written only from
	// the single-threaded result-assembly path.
	lastFailureClass delegate.FailureClass

	// normalizedFailureKey/count bound repeated deterministic failures by the
	// invariant probe strategy plus the shared semantic failure layer. Varying
	// guessed arguments cannot reset this counter.
	normalizedFailureKey   string
	normalizedFailureCount int

	// webSourceURLs are the relevance-validated URLs returned by web_search or
	// web_news this turn; webFetchedURLs records which of those were actually
	// read through fetch. Search snippets are discovery, never factual evidence.
	webSourceURLs  map[string]struct{}
	webFetchedURLs map[string]struct{}

	// curLoop is the live per-turn loop-state snapshot (self-model task 2.2),
	// refreshed each Chat iteration; lastDeath is the structured record
	// finalized at a loop-affecting death (nil = the turn did not die in a
	// loop-affecting way), read by the supervisor through Agent.LastDeath.
	curLoop   loopSnapshot
	lastDeath *LoopDeath

	// unproductive is the ONE unified unproductive-attempt counter (N2/
	// req.8.1): completion rejections, guidance nudges, and no-progress
	// repeats all feed it; it is never reset by a plain tool dispatch.
	unproductive int

	// signals is the unified per-step signal state (MORPHEUS req.5.1),
	// computed ONCE at deliberate entry and replaced every step. All
	// self-correction consumers — the stall commit (noteBatch), the Cassandra
	// controller, the governor — read this one state.
	signals *stepSignals

	// Stall bookkeeping: repeats counts consecutive repeated batches; prevSig/
	// prevCalls hold the previous committed batch (byte + semantic repeat
	// reads); recentSigs is the rotating-cycle window; convergeNudged gates the
	// one-time legacy convergence steer; distinctToolSet tracks tool breadth
	// (the adaptive-budget signal). The repeat VERDICT over this state is
	// computed only in readStepSignals; noteBatch commits from it.
	repeats         int
	prevSig         string
	prevCalls       []llm.ToolCall
	recentSigs      []string
	convergeNudged  bool
	distinctToolSet map[string]struct{}

	// Session seq range (DEJA-VU req 1.1): the inclusive span of cortex
	// session seqs appended during THIS turn (via cmAppend), tracked so
	// end-of-turn consolidation can stamp a derived_from provenance edge from
	// every memory it writes back to the exact transcript slice it came from.
	// haveSeq gates the range — false means nothing was appended this turn (no
	// pager, or a bare test agent), so provenance linking is skipped.
	seqLo   uint64
	seqHi   uint64
	haveSeq bool

	// Surfaced-memory sets: every cortex memory surfaced this turn, so a
	// successful completion can attest USED vs IGNORED (the usage-salience +
	// EMA learning signal). Keyed by URI; snippets keep text/type for the
	// rejection gate.
	surfaced         map[string]struct{}
	surfacedSnips    map[string]memory.Snippet
	episodicDetected bool
	episodicPending  bool
	episodicTrigger  string
}

// newTurn constructs a fresh turn — the reset state. Maps the loop reads
// unconditionally are allocated here; the rest stay lazily allocated by their
// mechanisms exactly as before the reification.
func newTurn() *turn {
	return &turn{
		recentSigs:      make([]string, 0, stallWindow),
		distinctToolSet: map[string]struct{}{},
		surfaced:        map[string]struct{}{},
		surfacedSnips:   map[string]memory.Snippet{},
		manifests:       map[string]o1.OperationManifest{},
	}
}

// noteSessionSeq folds one appended cortex session seq into the turn's
// inclusive [seqLo, seqHi] range (DEJA-VU req 1.1). The first call opens the
// range; subsequent calls widen it.
func (t *turn) noteSessionSeq(seq uint64) {
	if !t.haveSeq {
		t.seqLo, t.seqHi, t.haveSeq = seq, seq, true
		return
	}
	if seq < t.seqLo {
		t.seqLo = seq
	}
	if seq > t.seqHi {
		t.seqHi = seq
	}
}
