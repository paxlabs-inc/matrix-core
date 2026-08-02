// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// turn.go — the reified turn (MORPHEUS req.2): every piece of run state with
// turn lifetime is owned by a turn struct born at Chat entry, so per-turn
// resets cannot be forgotten and cross-turn leaks are impossible by
// construction. Constructing the turn IS the reset — Chat entry replaces the
// handle instead of zeroing fields one by one.
package agent

import (
	"sync"

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
	// objective is the original user-authored task. inputOrigin distinguishes a
	// genuine user turn from a supervisor resume instruction; resume guidance
	// enters the cognitive window only and is never durable evidence.
	objective       string
	turnObjective   string
	inputOrigin     inputOrigin
	attempt         int
	posture         InteractionPosture
	promoted        bool
	localRecoveries int

	// contract is Architect O1's executable, versioned statement of the
	// complete request. It is compiled before model execution and revised
	// monotonically when genuine user steering arrives.
	contract o1.TaskContract
	// runLedger is Architect O1's authoritative causal state for this turn.
	// It records real dispatch attempts, effects, verifier evidence, and the
	// terminal proof consumed by the supervisor.
	runLedger *o1.RunLedger
	verifiers o1.VerifierGraph
	manifests map[string]o1.OperationManifest

	// lastOutcome is the only piece of turn state written from the CONCURRENT
	// tool-dispatch goroutines (runToolCalls fans out up to
	// ToolDispatchConcurrency calls, each reaching recordO1ToolOutcome), so it
	// is the one field that needs its own lock — o1.RunLedger already carries
	// an internal mutex, and every other turn field is touched only by the loop
	// goroutine. Always go through commitOutcome/outcome; a bare field access
	// from dispatch is the 2026-07-25 data race.
	outcomeMu   sync.Mutex
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

	// expectRefusals bounds the ground-or-hypothesize refusal (req.6.1) per
	// probe strategy per turn. The refusal TEACHES — it hands back a corrected
	// call skeleton — but a model that does not take the lesson must not be
	// refused forever: each refusal is an unproductive step, so an unbounded
	// gate spends the whole nudge budget and kills the turn on a tool that was
	// never actually broken (2026-07-25: sub-agent 1 died after four
	// consecutive web_search refusals). Keyed by probeStrategy, which ignores
	// the varying argument, so re-wording the same query is the same strategy.
	expectRefusals map[string]int

	// graph is Mechanism 4's reified task graph (epistemic-core req.7): goal →
	// subgoals → evidence, with convergence computed as evidence-set delta.
	graph *taskGraph

	// revisionPending, when non-empty, forces a tools-stripped reasoning-only
	// revision step at the next loop iteration (epistemic-core req.5.2/6.3/
	// 7.2); revisionsThisTurn bounds them (maxRevisionsPerTurn).
	revisionPending   string
	revisionsThisTurn int
	// localFinalizePending holds a one-shot, tools-stripped close instruction
	// after an expected application conflict that cannot be repaired by
	// repeating the same operation (for example, a stale durable-memory
	// target). The next model call must turn the useful work already gathered
	// into an honest answer or focused clarification instead of letting the
	// conflict grow into a governor death.
	localFinalizePending string

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

	// overflowNudges counts how many times the unread-overflow close guard has
	// steered THIS turn. Past overflowNudgeCap the guard stands down and the
	// composed answer delivers: the read-full discipline exists to stop
	// reasoning over truncated data, never to discard finished work (the
	// 2026-07-22 loopty-loop incident).
	overflowNudges int

	// sourceFetchNudges counts how many times the source-fetch close guard has
	// steered THIS turn. Past sourceFetchNudgeCap it stands down for the same
	// reason as overflowNudges: when every fetch of a discovered URL fails, the
	// guard's precondition is unachievable and holding the answer hostage only
	// spends the turn.
	sourceFetchNudges int

	// stepRefused/stepDispatched are the per-STEP dispatch-gate read (reset at act
	// entry): stepRefused is set when the check-before-act gate or the missing-
	// prediction gate refuses a call this step; stepDispatched is set when any call
	// actually reached the tool. refusalRun is the CROSS-step counter of
	// consecutive wholly-refused steps that were NOT already a batch-repeat — the
	// gap every existing stall read misses (a step that refused a call and
	// dispatched nothing produces no dispatched failure, and under varied arguments
	// no batch-repeat either). An IDENTICAL refused batch is excluded (it is a
	// repeat, owned by the stall path, so those spirals still die via
	// no_progress_stall); a genuine dispatch resets the run. At the stall bound it
	// escalates to an honest stop-and-ask — the research.txt failure: the same
	// ungrounded probe re-issued with a new query every step, each refused before
	// dispatch, previously never escalating.
	stepRefused    bool
	stepDispatched bool
	refusalRun     int

	// closeChurn/lastCloseContent are the reasoning-churn read across consecutive
	// bare-answer (non-dispatching) closes: the tool-batch repeat detectors compare
	// only tool calls, so a close attempt that re-loops and re-derives the same
	// prose is invisible to them and to the evidence-convergence meter. closeTurn
	// counts consecutive re-looped closes whose answers repeat substantially the
	// same content and escalates past the stall bound. Reset on a delivered answer.
	closeChurn       int
	lastCloseContent string

	// answerTokenBudget/answerTokenBumps drive the truncated-answer output-limit
	// escalation (synthesis-survives-death): a generation cut off by the output
	// limit (finish_reason=length) bumps the per-request output-token budget one
	// rung (doubling from the floor toward the ceiling) so a long final synthesis
	// can actually finish instead of re-truncating at the same limit. answerToken
	// Budget=0 means "use the client default"; bumps are bounded by maxAnswerToken
	// Bumps. Applied to every generate call this turn once bumped.
	answerTokenBudget int
	answerTokenBumps  int
	projection        string
	projectionFrozen  bool

	// curStep/stepBudget carry this step's index and the effective step budget so
	// the budget tail can surface steps-remaining to the model (start writing
	// before the cliff, not after it). Set at the top of each Chat iteration.
	curStep    int
	stepBudget int

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

type inputOrigin uint8

const (
	originUser inputOrigin = iota
	originSupervisorResume
)

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

// commitOutcome records the most recent O1 operation outcome. It is safe to
// call from the concurrent tool-dispatch goroutines. Semantics are unchanged
// from the single-threaded form — last writer wins — so a serial sequence of
// calls still ends on the last call's outcome; under concurrent dispatch "last"
// means last to COMPLETE, which is why the supervisor reads the run ledger (a
// complete, ordered record) for anything order-sensitive and treats this field
// only as the most recent signal.
func (t *turn) commitOutcome(outcome o1.OperationOutcome) {
	if t == nil {
		return
	}
	t.outcomeMu.Lock()
	t.lastOutcome = &outcome
	t.outcomeMu.Unlock()
}

// outcome reads the most recent O1 operation outcome under the same lock, so a
// read racing an in-flight dispatch is safe.
func (t *turn) outcome() *o1.OperationOutcome {
	if t == nil {
		return nil
	}
	t.outcomeMu.Lock()
	defer t.outcomeMu.Unlock()
	return t.lastOutcome
}

// commitOutcomeIfStandingIsNotTerminal records an outcome unless a TERMINAL
// failure is already standing, so the synthetic turn-death outcome never
// masks the real tool failure that caused the death. Same precedence rule the
// governor has always applied, moved under the lock with it.
func (t *turn) commitOutcomeIfStandingIsNotTerminal(outcome o1.OperationOutcome) {
	if t == nil {
		return
	}
	t.outcomeMu.Lock()
	defer t.outcomeMu.Unlock()
	if t.lastOutcome == nil || t.lastOutcome.Success || !t.lastOutcome.IsTerminal() {
		t.lastOutcome = &outcome
	}
}
