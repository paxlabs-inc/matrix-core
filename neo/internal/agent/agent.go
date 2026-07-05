// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package agent is Neo's control loop: a recursive LLM tool-calling loop in
// the conventional ("normal agent") shape. The conversation transcript IS the
// state; the model emits text + tool-call intents; the harness is the only
// effector. This is deliberately NOT the MCL compile→plan→execute machine —
// MCL is reached only through the core_execute tool for rigorous / monetary
// tasks.
//
// The loop implements the frozen spec's [control.loop] and [loop_discipline]:
// pack window → (compact if over budget) → call model → run tool calls →
// loop, with a per-turn step budget, no-progress stall detection, a bounded
// recovery ladder, and honest partials on exhaustion (never fabricated
// success).
package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/cassandra"
	"matrix/neo/internal/config"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/recall"
	"matrix/neo/internal/tools"
)

// ErrIncomplete marks a turn that ended WITHOUT completing the task: the loop
// stalled (no progress) or exhausted its step budget. It is distinct from a
// model/transport error and from a genuine completion (nil). The task
// supervisor (internal/server) treats it as "not done — keep going": it
// respawns a fresh agent over durable state and continues, rather than
// surfacing a fake "Done" to the user. The wrapped text carries a short,
// honest where-I-got-stuck digest for the next attempt's catch-up.
// Detect with errors.Is(err, agent.ErrIncomplete).
var ErrIncomplete = errors.New("neo: turn incomplete (task not finished)")

// gateNotAcceptedToolResult is the neutral, non-narratable token used to answer
// a rejected (or prematurely-batched) task_complete call. Every tool_call must
// receive a tool result or a strict provider 400s, but the ACTIONABLE rejection
// reason no longer rides this in-band result (where the model would narrate it
// into the user-visible reasoning stream, req.1.2) — it travels the guidance
// channel instead (llm.GuidanceMessage). The model still learns the turn is not
// done; the steering detail stays off every user surface.
const gateNotAcceptedToolResult = "Not accepted yet — keep working."

// Consolidator is the background write-back hook: it receives a completed
// turn's transcript and promotes durable learnings to cortex out-of-band.
// Implemented by internal/writeback; optional (nil disables write-back).
type Consolidator interface {
	Consolidate(transcript string)
	// ConsolidateSync runs the same extraction synchronously on the caller's
	// goroutine. Called from compact() before evicting older turns so durable
	// facts/events/patterns reach cortex before the turns are lost.
	ConsolidateSync(ctx context.Context, transcript string)
}

// ConvRecaller surfaces the most relevant PAST turns of this conversation
// (beyond the live transcript / resume seed) for a given query. Implemented by
// internal/recall; optional (nil disables conversational recall). It is the
// additive read-lane that lets an unbounded thread stay coherent — relevance
// over raw recency — without growing the in-window transcript.
type ConvRecaller interface {
	Relevant(ctx context.Context, query string) []recall.Hit
}

// ToolEvent is a single observed tool call and its result, surfaced to the
// harness so the product can SHOW THE WORK — e.g. render live web-search
// snippets and source cards, or "fetched <url>" activity. This is the
// transparency differentiator: users see the real evidence behind an answer,
// not just a synthesized paragraph.
type ToolEvent struct {
	ID     string                 // tool-call id — stable across the start/end pair so the UI updates one step
	Name   string                 // function name dispatched (e.g. "web-search__web_search")
	Args   map[string]interface{} // parsed call arguments
	Result string                 // tool result content (raw text/JSON the tool returned); empty at start
	IsErr  bool                   // the tool reported an error result
	Phase  ToolPhase              // start (dispatched, no result yet) or end (completed)
}

// ToolPhase distinguishes the two observer callbacks for one tool call: a
// start (so the surface can paint a live "running" viewport immediately) and
// an end (the result lands). The harness correlates them by ToolEvent.ID.
type ToolPhase string

const (
	ToolStart ToolPhase = "start"
	ToolEnd   ToolPhase = "end"
)

// ToolObserver receives every tool result as it happens. Optional; nil
// disables surfacing. The harness (CLI or SSE server) decides how to render —
// the agent loop stays oblivious to the presentation layer.
type ToolObserver func(ToolEvent)

// Agent wires the model, tools, and memory into one conversational loop.
type Agent struct {
	cfg          config.Config
	main         *llm.Client
	cheap        *llm.Client
	tools        *tools.Manager
	pager        *memory.Pager
	out          Reporter
	consolidator Consolidator
	recaller     ConvRecaller
	observer     ToolObserver

	// adjudicator is the shared Cassandra epistemic-completeness faculty
	// consulted at the completion gate on state-touching turns (Phase 3). nil
	// falls back to the deterministic local grounding check. auditObserver
	// streams cassandra.* audit events to the harness; nil discards them.
	adjudicator   *cassandra.Adjudicator
	auditObserver AuditObserver

	// memObserver receives this turn's continuous-memory activation summary
	// (durable story-so-far + coarse timeline) so the harness can surface the
	// memory Neo carries to the user (continuous-memory task 7.1). nil disables
	// surfacing; the agent loop stays oblivious to the presentation layer
	// (mirrors auditObserver). Only fires under the continuous-memory collapse.
	memObserver MemoryObserver

	schemas      []llm.Tool
	schemaTokens int
	schemaBytes  int

	// working is the live transcript (user / assistant / tool messages). The
	// system block (identity + rules + retrieved memory + budget stat) is
	// re-derived every turn and never stored here, so it can't drift.
	working []llm.Message
	// summary is the consolidated story-so-far produced by compaction; it
	// stands in for evicted working history and is re-derivable (not ground
	// truth — cortex is).
	summary string
	// activeGoal is THIS conversation's task, pinned every turn. Held on the
	// agent (not the pager) so many conversations can share one cortex store
	// without clobbering each other's goal.
	activeGoal string

	// persona, when set, frames this agent as a task-scoped SUB-AGENT with a
	// specific role. Sub-agents run headless (no human in the loop): they get
	// a restricted tool surface, never ask the user questions, and end by
	// reporting their findings back to the orchestrating agent.
	persona string

	// convID scopes the per-turn attestation IntentID for audit; empty on the
	// CLI path (falls back to "cli"). turnSeq counts user turns this session so
	// each attest has a distinct, stable IntentID.
	convID  string
	turnSeq int

	// generation is the per-session command-queue tag for the current turn
	// (P2-6 lane-based concurrency). Set by the session before each Chat call
	// so superseded (late) results can be detected and discarded — a result
	// whose generation is no longer current is stale. Zero when unset (CLI
	// path, tests). Not goroutine-safe: accessed only from the Chat goroutine.
	generation uint64

	// skillIndex is the names-only skill list injected into the STABLE system
	// prefix (P2-2). Token-bounded: only names, never bodies. Set by the
	// session/harness from the consolidator's ProposedSkills(). Lives in the
	// cacheable prefix so it stays byte-identical across turns (consistent
	// with P1-2). Empty = no skill section emitted.
	skillIndex []string

	// topic tracks the rolling topic centroid so a pivot to an unrelated
	// subject can reset the per-turn retrieved working set (Phase 3). nil when
	// no embedder is available (topic detection is inherently semantic).
	topic *topicTracker

	// inbox, when set, returns user messages queued (by the session) WHILE
	// this turn is in flight — mid-task messages the user sent without
	// interrupting (F5). The loop drains it at each tool-call boundary and
	// appends them to the transcript, so the agent picks them up on its next
	// step instead of the message cancelling the run. nil on the CLI path.
	inbox func() []string

	// lastFailureClass records the shared FailureClass of the most recent
	// classified tool FAILURE this turn (delegate.ClassNone when none). It lets
	// the task supervisor read the SAME classification the dispatch ladder used
	// (via LastFailureClass) so a deterministic blocker is not respawned over.
	// Reset at the top of every Chat turn; written only from the single-
	// threaded result-assembly path (never from the concurrent dispatch
	// goroutines), so it is race-free.
	lastFailureClass delegate.FailureClass

	// overflow holds this turn's oversized tool results that were spilled to
	// run-scoped files (neo-smoothness req.4): the transcript keeps a bounded
	// head+tail + a truncation notice, and the model pages the rest back in via
	// read_overflow. The loop will not let the turn finish while any overflow is
	// unread. Created lazily on the first overflow and cleaned up at turn end.
	// Goroutine-safe (the concurrent dispatch path may both cap and read).
	overflow *overflowStore

	// userProfile carries the onboarding profile (preferred_name,
	// expertise_domains) fetched from the daemon's /profile endpoint. It is
	// per-user-stable (set once at onboarding, rarely changed) so it lives
	// in the STABLE system prefix (prompt-cache byte-stability invariant).
	// agent_name flows through cfg.AgentName separately. Empty = no
	// user-specific identity section (clean fallback to default "Neo").
	preferredName    string
	expertiseDomains []string
}

// Options configures New.
type Options struct {
	Config       config.Config
	Main         *llm.Client // required: the conversational tool-calling model
	Cheap        *llm.Client // optional: cheap model for compaction (falls back to Main)
	Tools        *tools.Manager
	Pager        *memory.Pager
	Reporter     Reporter
	Consolidator Consolidator // optional: background write-back
	Recaller     ConvRecaller // optional: relevant past-turn recall (additive read-lane)
	Observer     ToolObserver // optional: per-tool-result surfacing (show the work)

	// Adjudicator is the shared Cassandra completeness faculty consulted at the
	// completion gate on state-touching turns: it checks the OUTCOME against the
	// task GOAL over the executed transcript. nil fails open on the strict path
	// (i_cass_5) — there is no local word-matching stand-in. AuditObserver
	// streams cassandra.* audit events to the harness; nil discards them.
	Adjudicator   *cassandra.Adjudicator
	AuditObserver AuditObserver

	// MemoryObserver receives this turn's continuous-memory activation summary
	// (durable story-so-far + coarse timeline) so the harness can surface the
	// memory Neo carries to the user (continuous-memory task 7.1). nil discards.
	MemoryObserver MemoryObserver

	// Persona frames this as a task-scoped sub-agent with a specific role
	// (empty = the top-level conversational agent).
	Persona string
	// ConvID is the conversation this agent serves; stamped on the per-turn
	// attestation IntentID for audit. Empty on the CLI path.
	ConvID string
	// RestrictTools advertises the SUB-AGENT tool surface (full Natural set,
	// minus core_execute / memory_recall / spawn_subagents) instead of the
	// full one. Set for sub-agents so money stays with the parent and a
	// sub-agent can't spawn its own sub-agents.
	RestrictTools bool

	// Inbox, when set, returns user messages queued while a turn is in flight
	// (mid-task messages sent without interrupting). The loop folds them into
	// the transcript at each tool-call boundary (F5). nil disables (CLI path).
	Inbox func() []string
}

// New assembles an Agent.
func New(o Options) *Agent {
	out := o.Reporter
	if out == nil {
		out = nopReporter{}
	}
	a := &Agent{
		cfg:           o.Config,
		main:          o.Main,
		cheap:         o.Cheap,
		tools:         o.Tools,
		pager:         o.Pager,
		out:           out,
		consolidator:  o.Consolidator,
		recaller:      o.Recaller,
		observer:      o.Observer,
		adjudicator:   o.Adjudicator,
		auditObserver: o.AuditObserver,
		memObserver:   o.MemoryObserver,
		persona:       strings.TrimSpace(o.Persona),
		convID:        strings.TrimSpace(o.ConvID),
		inbox:         o.Inbox,
	}
	if a.tools != nil {
		if o.RestrictTools {
			a.schemas = a.tools.SubagentSchemas()
		} else {
			a.schemas = a.tools.Schemas()
		}
	}
	// read_overflow is always advertised (top-level and sub-agent): any tool
	// result can overflow the inline budget, and the read-full discipline
	// (neo-smoothness req.4) needs the model able to page the remainder back in.
	// It is synthetic (intercepted in the loop, not routed through the Manager),
	// so it never enters the manifest tool-bijection check.
	a.schemas = append(a.schemas, readOverflowSchema())
	if o.Pager != nil {
		a.topic = newTopicTracker(o.Pager.Embedder())
	}
	a.schemaTokens = estimateToolTokens(a.schemas)
	a.schemaBytes = estimateToolBytes(a.schemas)
	return a
}

// SetGeneration tags this agent's current turn with a generation number from
// the session's command queue (P2-6). A result whose generation has been
// superseded by a newer dispatch is discarded as stale. Call before Chat.
func (a *Agent) SetGeneration(gen uint64) { a.generation = gen }

// Generation returns the current generation tag (0 = unset, e.g. CLI path).
func (a *Agent) Generation() uint64 { return a.generation }

// drainInbox folds any user messages queued while this turn is in flight (F5:
// non-interrupting mid-task messages) into the working transcript, so the agent
// addresses them on its next step instead of the message cancelling the run.
// Drained at each tool-call boundary; returns true if it injected at least one.
// No-op when no inbox is wired (CLI path). Injection happens at a clean
// boundary (loop top / pre-finish), where the transcript ends on a tool result
// or assistant turn, so appending a user message keeps it well-formed.
func (a *Agent) drainInbox() bool {
	if a.inbox == nil {
		return false
	}
	injected := false
	for _, m := range a.inbox() {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		a.working = append(a.working, llm.UserMessage(m))
		injected = true
	}
	return injected
}

// SetSkillIndex injects a names-only skill index into the STABLE system prefix
// (P2-2). The index lists available skills by NAME only — never full bodies
// (steps/gotchas/criteria), which are pulled on demand via memory_recall. This
// keeps the cacheable prefix token-bounded and byte-stable across turns
// (consistent with P1-2). Call after the consolidator proposes skills.
func (a *Agent) SetSkillIndex(names []string) {
	a.skillIndex = names
}

// SetUserProfile injects the onboarding profile (agent_name, preferred_name,
// expertise_domains) into the STABLE system prefix. Called after agent
// construction (per session rebuild) so a later profile edit is reflected on
// subsequent turns (req 8.2). The values are per-user-stable so they preserve
// the prompt-cache byte-stability invariant (req 2.4): a non-empty agentName
// overrides cfg.AgentName, and empty preferredName + nil expertiseDomains
// produce no identity section (clean fallback to default "Neo").
func (a *Agent) SetUserProfile(agentName, preferredName string, expertiseDomains []string) {
	if n := strings.TrimSpace(agentName); n != "" {
		a.cfg.AgentName = n
	}
	a.preferredName = strings.TrimSpace(preferredName)
	a.expertiseDomains = expertiseDomains
}

// effectiveBudgetSignals carries the observable turn-complexity signals the
// adaptive step-budget computation reads. It is populated by the Chat loop as
// the turn progresses. The primary signal is distinctTools — the count of
// DIFFERENT tool names dispatched so far — which proxies turn breadth: a turn
// using one tool repeatedly is simple; a turn that fans across many tools is
// complex and warrants more steps.
type effectiveBudgetSignals struct {
	// step is the current loop iteration index (0-based).
	step int
	// distinctTools is the number of distinct tool names dispatched so far
	// this turn. 0 or 1 = simple turn; >1 signals multi-step breadth.
	distinctTools int
	// stallRepeats is the current no-progress repeat count (how many
	// consecutive identical batches have been seen). Not used to scale the
	// budget UP — a stalling turn should terminate, not get more room —
	// but tracked so the signal bundle is self-contained for observability.
	stallRepeats int
}

// effectiveStepBudget computes the adaptive per-turn step budget within the
// configured [StepBudgetMin, StepBudgetMax] band, derived from observable
// turn-complexity signals (tool-call breadth so far).
//
// When adaptation is disabled (StepBudgetMin == 0, the default), it returns
// StepBudgetMax unchanged — today's fixed-budget behavior (no scope creep,
// default behavior matches exactly).
//
// When enabled (StepBudgetMin > 0), the budget scales UP from the floor
// toward the ceiling as the turn shows breadth (more distinct tools). The
// scaling is conservative:
//
//   - 0-1 distinct tools → floor (simple turn, no breadth signal).
//   - Each additional distinct tool adds a proportional slice of the
//     [floor, ceiling] range, so 2 tools gets one step up, 3 gets more, etc.
//   - The result is clamped to [floor, ceiling] — never below the floor
//     for simple turns, never above the ceiling.
//
// The no-progress stall path is independent and unaffected: a stalling turn
// terminates via NoProgressStall regardless of the effective budget (m9).
func (a *Agent) effectiveStepBudget(sig effectiveBudgetSignals) int {
	min := a.cfg.StepBudgetMin
	max := a.cfg.StepBudgetMax

	// Adaptation disabled (the default): return the fixed ceiling. This is
	// today's behavior — StepBudgetMax defaults to StepBudget (50).
	if min <= 0 {
		if max <= 0 {
			return a.cfg.StepBudget
		}
		return max
	}

	// Sanitize the band: max must be >= min. If misconfigured, fall back
	// to the fixed StepBudget (safe, no surprise).
	if max < min {
		return a.cfg.StepBudget
	}

	// Conservative scaling: the floor is the starting point. Each distinct
	// tool beyond the first adds a proportional slice of the available range.
	// breadth = distinctTools - 1 (so 1 tool = 0 breadth = floor).
	// slices = the number of steps to divide the range into; we use a
	// generous divisor so it takes real breadth (not just 2 tools) to reach
	// the ceiling.
	const breadthDivisor = 6 // 6 distinct tools → full range
	breadth := sig.distinctTools - 1
	if breadth < 0 {
		breadth = 0
	}
	if breadth > breadthDivisor {
		breadth = breadthDivisor
	}

	range_ := max - min
	scaled := min + (range_*breadth)/breadthDivisor

	// Clamp to [min, max] (defensive — should already be in range).
	if scaled < min {
		scaled = min
	}
	if scaled > max {
		scaled = max
	}
	return scaled
}

// Chat runs one user turn through the recursive loop until the model yields a
// final answer (no tool calls), the loop stalls/exhausts its budget, or it is
// blocked needing the human. Conversation state persists across calls.
func (a *Agent) Chat(ctx context.Context, userInput string) error {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return nil
	}
	if a.activeGoal == "" {
		a.activeGoal = userInput
		// Cassandra logs the task GOAL quietly at dispatch (side-channel only).
		// The completion gate later checks the OUTCOME Neo returns against this
		// goal over the executed transcript — Neo never has to cite evidence.
		a.emitAudit(auditEventGoal, map[string]interface{}{"goal": a.activeGoal})
	}
	a.turnSeq++
	a.lastFailureClass = delegate.ClassNone
	// Run-scoped overflow files (neo-smoothness req.4.3) are ephemeral to this
	// turn: clean them up on every exit (completion, stall, budget, error).
	defer func() {
		if a.overflow != nil {
			a.overflow.cleanup()
		}
	}()
	a.working = append(a.working, llm.UserMessage(userInput))
	// Continuous-memory (task 6.1): record the user message to the durable
	// cortex transcript so cortex — not the in-memory working slice — owns the
	// turn-by-turn record. No-op when the feature flag is off.
	a.cmRecordUser(userInput)

	// cm selects the continuous-memory collapse path (tasks 6.1–6.3): the
	// per-turn working set comes from cortex.Activate (rendered as a trailing
	// USER-role message) instead of the agent-side pager selection/ranking.
	cm := a.continuousMemory()

	// Topic-shift detection (Phase 3): if this turn pivots to an unrelated
	// subject, the previous topic's recalled past-turns must not bleed into it.
	// The pinned tier and the durable cortex retrieval (re-faulted fresh below)
	// are preserved; only the conversational-recall working set is reset.
	pivoted := a.observeTopic(userInput)

	// Page-fault relevant memory + proven patterns + trigger-matched behavioral
	// guidance for this ask (once/turn). The bulk semantic memory is now only a
	// THIN ambient seed (v3 #1: reasoning-time retrieval) — capped to
	// cfg.AmbientRetrievalTopK, or off entirely at 0 so the model pulls what it
	// needs mid-thought with memory_recall instead of being force-fed a blob.
	// Proven patterns and trigger-matched guidance are targeted (not the bulk
	// blob) and stay on the push path.
	//
	// Under the continuous-memory collapse (cm) these agent-side lanes are
	// retired: cortex.Activate is the single source and cmTail holds its
	// rendered bundle, computed ONCE per turn (Pinned computed once — NE-7).
	var (
		retrieved  []memory.Snippet
		procedural []memory.Pattern
		triggered  []memory.Snippet
		recalled   []recall.Hit
		cmTail     string
	)
	if cm {
		bundle := a.cmActivate(userInput)
		// Surface the memory Neo carries to the harness (task 7.1) from the
		// same single bundle before rendering it — no extra Activate call, so
		// the pinned-once-per-turn discipline (NE-7) holds.
		a.emitMemory(bundle)
		cmTail = a.renderActivationBundle(bundle)
		// Q2 first-message relevance push: Activate's tiers are recency-based
		// + query-independent, so on the OPENING turn also inject a bounded
		// relevance retrieval keyed on the message — the agent gets
		// relevance-matched memory without a reactive memory_recall call.
		// retrieved is otherwise nil on the cm path; setting it here feeds the
		// existing collectSurfaced/collectSurfacedSnips usage-salience loop.
		if pushSnips, pushBlock := a.cmRelevancePush(ctx, userInput); pushBlock != "" {
			cmTail += pushBlock
			retrieved = pushSnips
		}
	} else {
		retrieved = a.ambientMemory(ctx, userInput)
		procedural = a.faultPatterns(ctx, userInput)
		triggered = a.faultTriggers(ctx, userInput)
		// Conversational recall: relevant PAST turns beyond the live transcript
		// — the additive read-lane that keeps an unbounded thread coherent.
		// Reset on a topic pivot so a fresh subject starts clean.
		recalled = a.recallTurns(ctx, userInput)
		if pivoted {
			recalled = nil
		}
	}
	// Track every cortex memory surfaced this turn so a successful completion
	// can attest them as USED — the usage-salience + EMA learning signal that
	// keeps Neo's durable store ranking by what actually helps. surfacedSnips
	// keeps the retrieved snippets' text/type too, so completion can also send
	// the NEGATIVE signal for memories that were surfaced but demonstrably
	// ignored (off-topic to the produced turn).
	surfaced := map[string]struct{}{}
	surfacedSnips := map[string]memory.Snippet{}
	collectSurfaced(surfaced, retrieved, procedural)
	collectSurfaced(surfaced, triggered, nil)
	collectSurfacedSnips(surfacedSnips, retrieved)

	repeats := 0
	// unproductive is the ONE unified unproductive-attempt counter (N2/req.8.1):
	// completion-gate rejections, no-progress repeats, and guidance nudges all
	// feed it, and the loop escalates to an honest stop-and-ask once it passes
	// the bound rather than re-nudging (or re-attempting a premature completion)
	// unbounded (req.1.5). It is reset ONLY by genuine ACCEPTED progress (an
	// accepted completion, which ends the turn) — a plain tool call interleaved
	// between completion rejections does NOT reset it (req.8.2), so a
	// work->premature-complete->reject loop is bounded well before the step
	// budget instead of running to it.
	unproductive := 0
	prevSig := ""
	// prevCalls is the previous turn's tool-call batch, kept so the no-progress
	// guard can detect a SEMANTIC repeat (same operation, reworded arguments),
	// not just a byte-identical batch signature (NE-4).
	var prevCalls []llm.ToolCall
	// recentSigs is a small window of recent batch signatures, so a rotating set
	// of already-seen operations (A → B → A → B) that introduces no NEW tool is
	// caught as a spiral even though consecutive signatures differ. convergeNudged
	// gates the one-time "stop re-doing completed work and finish" steer.
	recentSigs := make([]string, 0, stallWindow)
	convergeNudged := false
	// stateTouched latches once this turn reaches the irreversible seam
	// (core_execute). It selects the strict completion path (a full grounded
	// completeness object) over the reversible light path — the placement rule
	// of the Cassandra Phase 1 completion gate.
	stateTouched := false
	// workTouched latches once this turn runs ANY non-trivial tool (not just
	// the money/chain seam). Under GateAllWork it promotes a substantial
	// reversible deliverable (a researched paper, a built site, generated code)
	// onto the grounded completion path too — so "done" is proven, not
	// self-asserted — while a pure conversational turn (no tools) still ends on
	// the frictionless light path. This is the "highest standard" gate.
	workTouched := false

	// gateSurfaced holds the last plain answer we committed as durable narration
	// when the completion gate forced task_complete. It dedupes so a model that
	// re-issues the same prose (before it finally calls task_complete) does not
	// stack duplicate bubbles.
	gateSurfaced := ""

	// P2-7: adaptive step budget. Track distinct tool names dispatched so far
	// (tool-call breadth) as the complexity signal. The effective budget is
	// recomputed each iteration from [StepBudgetMin, StepBudgetMax] via
	// effectiveStepBudget, so a turn that fans out to many tools earns more
	// room, while a simple/stalling turn stays at the floor. When adaptation
	// is disabled (StepBudgetMin==0, the default), effectiveStepBudget returns
	// StepBudgetMax (== StepBudget == 50) and the loop is byte-identical to
	// the pre-P2-7 fixed-budget loop.
	distinctToolSet := map[string]struct{}{}

	// noteBatch advances the no-progress/stall read for ONE assistant
	// tool-calling batch — a normal tool dispatch OR a premature task_complete.
	// Sharing it between the dispatch path and the completion branch is what
	// stops the completion branch from bypassing stall detection (N2/req.8.3):
	// a repeated premature task_complete now trips the same repeat guard as a
	// repeated tool call. A byte-identical or SEMANTIC repeat, or a rotating
	// A→B→A→B cycle that introduces no new tool, increments the repeat count;
	// a distinct operation resets it (NE-4). Returns whether the batch was a
	// repeat and whether the pure-repeat stall bound (NoProgressStall) is met.
	noteBatch := func(calls []llm.ToolCall) (repeat, stalled bool) {
		sig := batchSignature(calls)
		introducedNewTool := false
		for _, c := range calls {
			if _, seen := distinctToolSet[c.Function.Name]; !seen {
				introducedNewTool = true
				break
			}
		}
		cyclic := !introducedNewTool && sigInWindow(recentSigs, sig)
		if sig == prevSig || a.semanticRepeat(prevCalls, calls) || cyclic {
			repeats++
			repeat = true
		} else {
			repeats = 0
		}
		recentSigs = pushWindow(recentSigs, sig, stallWindow)
		prevSig = sig
		prevCalls = calls
		return repeat, repeats >= a.cfg.NoProgressStall
	}

	for step := 0; ; step++ {
		budget := a.effectiveStepBudget(effectiveBudgetSignals{
			step:          step,
			distinctTools: len(distinctToolSet),
			stallRepeats:  repeats,
		})
		if step >= budget {
			break
		}
		// F5: fold in any messages the user queued mid-task (sent without
		// interrupting) so the agent picks them up on THIS step — delivered at
		// the tool-call boundary, never cancelling the in-flight run.
		a.drainInbox()
		// Mid-turn page-fault refresh: long tool loops drift away from the
		// opening ask, so periodically re-fault against the latest assistant
		// narration. Injection stays system-block-only — the transcript
		// never pays for it. v3 #1: this ambient refresh is opt-in — it runs
		// only when AmbientRetrievalTopK > 0. Fully tool-driven (0), the model
		// re-pulls mid-thought with memory_recall instead.
		if !cm && a.cfg.AmbientRetrievalTopK > 0 && step > 0 && step%refaultEvery == 0 {
			q := userInput
			if c := lastAssistantText(a.working, 400); c != "" {
				q = q + "\n" + c
			}
			retrieved = a.ambientMemory(ctx, q)
			procedural = a.faultPatterns(ctx, q)
			triggered = a.faultTriggers(ctx, q)
			recalled = a.recallTurns(ctx, q)
			if pivoted {
				recalled = nil
			}
			collectSurfaced(surfaced, retrieved, procedural)
			collectSurfaced(surfaced, triggered, nil)
			collectSurfacedSnips(surfacedSnips, retrieved)
		}

		var (
			pinned     string
			baseSystem string
			pct        int
			tail       string
			window     []llm.Message
		)
		if cm {
			// Continuous-memory window (task 6.2): the byte-stable charter
			// prefix + the live transcript + the rendered Activate bundle as a
			// trailing USER-role message (req.9.4 — Qwen-template portability).
			// a.compact is retired (req.9.3): over-budget trimming is
			// NON-summarizing (cmTrimWorking), because the older turns are
			// durable in cortex and the coarse history rides the durable
			// story-so-far already surfaced in cmTail.
			baseSystem = a.stableSystem() + cmTail
			if a.budgetPct(baseSystem) >= a.cfg.HardPct {
				a.cmTrimWorking()
			}
			if a.windowBytes(baseSystem) >= maxRequestBodyBytes {
				a.stripOldImages()
				if a.windowBytes(baseSystem) >= maxRequestBodyBytes {
					a.cmTrimWorking()
				}
			}
			pct = a.budgetPct(baseSystem)
			tail = cmTail + fmt.Sprintf("\n\n[context: %d%% used]\n", pct)
			window = assembleWindowUserTail(a.stableSystem(), a.working, tail)
		} else {
			if a.pager != nil {
				pinned = a.pager.Pinned(ctx, a.activeGoal)
			}
			baseSystem = a.buildSystem(pinned, retrieved, procedural, triggered, recalled)

			// [control.loop] step_3: forced compaction if over the hard threshold.
			if a.budgetPct(baseSystem) >= a.cfg.HardPct {
				a.compact(ctx, "hard")
				baseSystem = a.buildSystem(pinned, retrieved, procedural, triggered, recalled)
			}
			// Byte-budget backstop: the provider enforces a hard request-BODY
			// byte cap (Fireworks: 1 MiB) that is independent of the token budget
			// above. The token estimate undercounts the serialized JSON (message
			// envelope + tool schemas + escaping), so a window within token budget
			// can still exceed the byte cap and 413. Force a compaction when the
			// approximate body size crosses the ceiling.
			if a.windowBytes(baseSystem) >= maxRequestBodyBytes {
				// Drop dead-weight inline image payloads from older turns first — a
				// cheaper, less lossy step than a full compaction (Phase 4.2).
				a.stripOldImages()
				if a.windowBytes(baseSystem) >= maxRequestBodyBytes {
					a.compact(ctx, "hard")
					baseSystem = a.buildSystem(pinned, retrieved, procedural, triggered, recalled)
				}
			}
			pct = a.budgetPct(baseSystem)
			// P1-2: the byte-stable system prefix (charter + ground truth) is the
			// FIRST message; the turn-varying memory block + context-budget stat
			// move into ONE trailing message AFTER the append-only transcript, so
			// the prefix stays byte-identical turn-over-turn and rides the
			// provider's longest-stable-prefix cache. baseSystem (stable + tail)
			// still drives the budget stat above.
			tail = a.dynamicTail(pinned, retrieved, procedural, triggered, recalled) + fmt.Sprintf("\n\n[context: %d%% used]\n", pct)
			window = assembleWindow(a.stableSystem(), a.working, tail)
		}

		// Live "typing" channel: stream the model's incremental fragments as
		// they generate so the user sees Neo thinking + answering in real time
		// instead of staring at a blank surface until the whole turn lands.
		// reasoning → the live thinking channel; content → the answer being
		// typed. step segments the stream so the client resets per turn.
		streamedReasoning := false
		onDelta := func(d llm.Delta) {
			if d.Reasoning != "" {
				streamedReasoning = true
				a.out.Delta(step, "reasoning", a.nameReasoning(d.Reasoning))
			}
			if d.Content != "" {
				// Identity net on the live answer stream (best-effort; the
				// settled answer is re-scrubbed at delivery). A model name split
				// across fragments can evade this, which is why finishTurn is the
				// authoritative choke point.
				a.out.Delta(step, "content", a.cleanContent(d.Content))
			}
		}

		res, err := a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas, OnDelta: onDelta})
		if err != nil {
			// HTTP 413 (provider request-body byte cap) is recoverable: the
			// window serialized past the byte limit even though it was within
			// the token budget. Force a compaction and retry once with the
			// shrunken window rather than failing the whole turn.
			if errors.Is(err, llm.ErrRequestTooLarge) {
				// Recover in two escalating steps (Phase 4.2): first strip
				// dead-weight inline images from older turns and retry — far less
				// lossy than discarding context. Only if that still 413s do we
				// fall back to a full compaction and retry once more.
				if a.stripOldImages() > 0 {
					// Image stripping only shrinks the transcript; the tail
					// (turn-varying memory + budget stat) is unchanged.
					if cm {
						window = assembleWindowUserTail(a.stableSystem(), a.working, tail)
					} else {
						window = assembleWindow(a.stableSystem(), a.working, tail)
					}
					res, err = a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas, OnDelta: onDelta})
				}
				if err != nil && errors.Is(err, llm.ErrRequestTooLarge) {
					if cm {
						// a.compact retired (req.9.3): non-summarizing trim.
						a.cmTrimWorking()
						baseSystem = a.stableSystem() + cmTail
						tail = cmTail + fmt.Sprintf("\n\n[context: %d%% used]\n", a.budgetPct(baseSystem))
						window = assembleWindowUserTail(a.stableSystem(), a.working, tail)
					} else {
						a.compact(ctx, "hard")
						baseSystem = a.buildSystem(pinned, retrieved, procedural, triggered, recalled)
						tail = a.dynamicTail(pinned, retrieved, procedural, triggered, recalled) + fmt.Sprintf("\n\n[context: %d%% used]\n", a.budgetPct(baseSystem))
						window = assembleWindow(a.stableSystem(), a.working, tail)
					}
					res, err = a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas, OnDelta: onDelta})
				}
			}
			if err != nil {
				a.consolidateWorking()
				return fmt.Errorf("neo: model call failed: %w", err)
			}
		}
		// A truncated generation (finish_reason=length) that ALSO carries tool
		// calls is a half-formed call: the model almost certainly inlined a
		// large payload as an argument and got cut off mid-JSON. Persisting it
		// would poison the transcript — it is re-sent verbatim every turn and a
		// strict provider 400s the malformed function, wedging the whole
		// conversation. Drop the cut-off turn and nudge for a compact retry.
		if res.FinishReason == "length" && res.HasToolCalls() {
			a.working = append(a.working, llm.UserMessage("(your last tool call was cut off by the output limit before its arguments finished — don't inline large content as a tool argument. Write large files in chunks/appends, or call the tool with compact arguments.)"))
			continue
		}
		a.working = append(a.working, res.Message)
		// Continuous-memory (task 6.1): record the assistant turn (content +
		// tool calls) to the durable cortex transcript. No-op when off.
		a.cmRecordAssistant(res.Message)
		if batchTouchesState(res.Message.ToolCalls) {
			stateTouched = true
		}

		// Show SOME of the thinking: surface a trimmed glimpse of this turn's
		// chain-of-thought as a secondary channel so the user sees how Neo is
		// reasoning before it acts. Never the answer, never persisted. Skip it
		// when the reasoning already streamed live (above) — the surface holds
		// the full thinking and a post-hoc glimpse would only truncate it. This
		// fallback covers models that return reasoning at fold time (inline
		// <think>) rather than as a separate streamed channel.
		if !streamedReasoning {
			if think := a.nameReasoning(glimpseReasoning(res.Message.Reasoning)); think != "" {
				a.out.Think(think)
			}
		}

		// Positive-proof termination (Cassandra Phase 1). A turn no longer ends
		// on the mere ABSENCE of tool calls (i_cass_1): a bare final message may
		// close only a reversible, non-state-touching turn (the light path). A
		// turn that reached the irreversible seam must finish through the
		// validated task_complete gate handled below.
		if !res.HasToolCalls() {
			// F5: a message queued mid-turn may have arrived during this model
			// call, just as the agent is about to finish. Address it instead of
			// closing the turn, so a non-interrupting mid-task message is never
			// dropped at the turn boundary.
			if a.drainInbox() {
				continue
			}
			answer := strings.TrimSpace(res.Message.Content)
			// Truncated generation (finish_reason=length) is NEVER a final
			// answer: the cut-off text is half-formed monologue/payload (the
			// model may have been inlining a large blob). Saying it raw leaks
			// internal thoughts into the chat. Nudge and let it retry compactly.
			if res.FinishReason == "length" {
				// Guidance channel + bounded: this is system steering the model
				// must act on (not a user turn), and a model that keeps getting
				// cut off can't re-nudge forever.
				if a.pushGuidanceNudge("Your last message was cut off by the output limit — don't inline large payloads in prose; call a tool with compact arguments, or give a concise final answer.", &unproductive) {
					return a.escalateGuidance(unproductive)
				}
				continue
			}
			if answer == "" {
				// anti-premature: empty AND no tools → steer to continue via the
				// guidance channel, bounded so a model that keeps returning
				// nothing escalates rather than looping forever.
				if a.pushGuidanceNudge("Continue: either call a tool to make progress, or give the final answer.", &unproductive) {
					return a.escalateGuidance(unproductive)
				}
				continue
			}
			// Read-full discipline (req.4.2): a tool result too large to show
			// inline was spilled to an overflow file. Don't let the turn end on
			// a bare answer while that output is still unread — steer (via the
			// guidance channel) to read it first, so the answer can't be drawn
			// from a truncated result.
			if a.overflowUnread() {
				if a.pushGuidanceNudge(a.overflowUnreadNudge(), &unproductive) {
					return a.escalateGuidance(unproductive)
				}
				continue
			}
			// Identity-compliance canary (P0): if the settled answer broke
			// character and self-identified as the underlying LLM, treat it as a
			// charter breach, not a typo — a model that won't hold its own name
			// is signalling it may ignore the harder rules too. Surface it on the
			// audit side-channel (never blind), then re-anchor via the guidance
			// channel so the model regenerates a compliant answer. finishTurn
			// still scrubs at delivery, so once the nudge budget is spent the
			// honest, scrubbed answer is shipped rather than looping forever.
			if scrubbed, leaked := scrubIdentity(a.agentName(), answer); leaked {
				a.emitAudit(auditEventIdentityLeak, map[string]interface{}{"where": "answer"})
				answer = scrubbed
				if a.pushGuidanceNudge(identityReanchorNudge(a.agentName()), &unproductive) {
					a.finishTurn(ctx, answer, surfaced, surfacedSnips, userInput, false)
					return nil
				}
				continue
			}
			// A turn that did real work may not end with an unaudited bare
			// message: nudge toward the completion gate so completion is proven,
			// not assumed. A pure conversational turn (no tools) rides the light
			// path (i_cass_5, placement-by-reversibility) and ends frictionlessly.
			if a.gateStrict(stateTouched, workTouched) {
				// Don't discard the model's natural answer while steering it to the
				// completion gate: it IS the user-facing reply (the gate only needs
				// task_complete as internal completion proof). Commit it as durable
				// narration so it survives — the redundant task_complete summary then
				// gets hidden by the client (narration already produced), instead of
				// the answer vanishing and the raw completion object surfacing in its
				// place. Dedupe so a re-issued identical answer doesn't stack.
				if answer != gateSurfaced {
					a.out.Status(answer)
					gateSurfaced = answer
				}
				// Route the completion steer through the GUIDANCE channel, not a
				// raw user turn: the model must recognize this as private system
				// steering to act on (call task_complete) and never surface it —
				// delivered as a user message it reads like a contentless user
				// turn and greets back instead, the bare-message spiral. Bounded:
				// once a model keeps ending with a plain answer past the nudge cap,
				// accept THAT answer as the honest final (it already did the work
				// and the reply is surfaced) rather than re-steering forever.
				if a.pushGuidanceNudge("You did real work this turn, so don't finish with a plain message — call task_complete with the outcome for the user: a summary, coverage, and anything still open. You don't need to cite evidence; just give the honest result.", &unproductive) {
					a.finishTurn(ctx, answer, surfaced, surfacedSnips, userInput, false)
					return nil
				}
				continue
			}
			a.finishTurn(ctx, answer, surfaced, surfacedSnips, userInput, false)
			// [control.loop] step_6: cooperative compaction at a clean boundary.
			// Retired on the continuous-memory path (req.9.3): story-so-far is
			// durable in cortex, so no agent-side summarization runs.
			if !cm && a.budgetPct(a.buildSystem(pinned, retrieved, procedural, triggered, recalled)) >= a.cfg.SoftPct {
				a.compact(ctx, "soft")
			}
			return nil
		}

		// Surface any preamble the model wrote alongside its tool calls as
		// DURABLE narration — Neo "thinking out loud" before it acts. This must
		// run for EVERY tool-calling turn, including the task_complete gate
		// below: a turn that goes straight to task_complete (e.g. it already had
		// the data) would otherwise commit nothing, and with the completion
		// summary kept out of the thread the user would be left with an empty
		// thread once the run settled. Committing the preamble here is what makes
		// Neo's running commentary the durable thread content.
		if c := strings.TrimSpace(res.Message.Content); c != "" {
			a.out.Status(a.cleanContent(c))
		}

		// Completion gate (Cassandra Phase 1): the model called task_complete.
		// Adjudicate the completeness object against the working transcript (the
		// ground truth — real tool results) before the turn may end. Sibling
		// tool calls in the same batch run FIRST so their results are in the
		// transcript before the claim is judged; every tool_call receives a
		// result so the transcript stays well-formed.
		if cc, rest := splitCompletion(res.Message.ToolCalls); cc != nil {
			// req.8.3 (N2): the completion branch no longer bypasses stall
			// detection. Advance the shared no-progress read on the completion
			// batch BEFORE the branch can continue, so a repeated premature
			// task_complete trips the same stall as a repeated tool call instead
			// of riding to the step budget. The reject path below additionally
			// folds each rejection into the unified unproductive counter.
			if _, stalled := noteBatch(res.Message.ToolCalls); stalled {
				a.consolidateWorking()
				return fmt.Errorf("%w: repeating the same completion claim without progress. Where it got stuck: %s", ErrIncomplete, oneLine(a.lastToolSummary()))
			}
			// Run any sibling tool calls, then FALL THROUGH to adjudicate the
			// completion against the transcript that now includes their results.
			// This replaces the old "call task_complete on its own" bounce: grok
			// naturally bundles a final bookkeeping call (e.g. todo) WITH
			// task_complete, and bouncing it made grok answer with a bare
			// greeting, which spiralled — bundled-complete and bare-message
			// alternated forever, the bounce reset the nudge budget (so the cap
			// never tripped), and both branches continue'd before the no-progress
			// stall, so nothing bounded it. A premature claim is still caught: a
			// spilled/unread sibling result trips the overflow gate below, and a
			// claim contradicted by what actually ran is rejected by adjudication
			// (which then rides the bounded guidance channel).
			if len(rest) > 0 {
				a.runToolCalls(ctx, rest)
				workTouched = true
				if batchTouchesState(rest) {
					stateTouched = true
				}
			}
			// Read-full discipline (req.4.2): block completion while a spilled
			// overflow result is unread, steering through the guidance channel —
			// completion can't be proven from a truncated result.
			if a.overflowUnread() {
				a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, gateNotAcceptedToolResult))
				if a.pushGuidanceNudge(a.overflowUnreadNudge(), &unproductive) {
					return a.escalateGuidance(unproductive)
				}
				continue
			}
			verdict := a.validateCompletion(ctx, cc, a.gateStrict(stateTouched, workTouched), stateTouched, userInput)
			if verdict.ok {
				a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, "Completion accepted."))
				a.finishTurn(ctx, verdict.answer, surfaced, surfacedSnips, userInput, true)
				// Cooperative compaction is retired on the continuous-memory
				// path (req.9.3): story-so-far is durable in cortex.
				if !cm && a.budgetPct(a.buildSystem(pinned, retrieved, procedural, triggered, recalled)) >= a.cfg.SoftPct {
					a.compact(ctx, "soft")
				}
				return nil
			}
			// Rejected: answer the task_complete call with a neutral token (so
			// the transcript stays well-formed — every tool_call needs a result)
			// and ride the actionable reason on the guidance channel rather than
			// as an in-band tool result whose text the model narrates into the
			// user-visible reasoning stream (req.1.2). The agent still learns it
			// must keep working and how to close the gap — in clean terms it does
			// not echo (advisor-not-effector — the agent enacts the fix). If the
			// gate keeps rejecting without progress, escalate to a stop-and-ask
			// rather than re-nudging unbounded (req.1.5).
			a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, gateNotAcceptedToolResult))
			if a.pushGuidanceNudge(verdict.feedback, &unproductive) {
				return a.escalateGuidance(unproductive)
			}
			continue
		}

		// No-progress detection (shared with the completion branch above via
		// noteBatch): a repeat is a byte-identical batch signature, a SEMANTIC
		// repeat (same operation reworded — a cosmetic reword can't reset the
		// counter and loop forever, NE-4), or a rotating A→B→A→B cycle that
		// introduces no new tool. Distinct operations reset the repeat read.
		repeat, stalled := noteBatch(res.Message.ToolCalls)
		if stalled {
			// No-progress stall: do NOT fabricate a close. Return an incomplete
			// signal so the supervisor can respawn a fresh agent and continue —
			// the task is not done. (On the bare CLI path, with no supervisor, the
			// wrapped reason is printed.)
			a.consolidateWorking()
			return fmt.Errorf("%w: repeating the same step without progress. Where it got stuck: %s", ErrIncomplete, oneLine(a.lastToolSummary()))
		}
		// req.8.1: a no-progress repeat is itself an unproductive attempt — fold
		// it into the ONE unified counter (alongside completion rejections and
		// guidance nudges) so an interleaved mix that never trips the pure-repeat
		// stall is still bounded, and escalate to an honest stop-and-ask past the
		// bound rather than running to the step budget.
		if repeat {
			unproductive++
			if a.capExceeded(unproductive) {
				return a.escalateGuidance(unproductive)
			}
		}
		// One firm convergence steer just before the hard stall: grok in particular
		// re-does work it already completed (re-fetching/re-rendering a value it
		// already has) instead of closing out. Injected AFTER this step's tool
		// results below so the transcript stays well-formed.
		injectConvergeNudge := a.cfg.NoProgressStall >= 2 && repeats == a.cfg.NoProgressStall-1 && !convergeNudged

		// P2-7: record distinct tool names for the adaptive budget signal.
		for _, c := range res.Message.ToolCalls {
			distinctToolSet[c.Function.Name] = struct{}{}
		}

		// Narrate-before-act (req.2): if the model went straight to tools
		// without writing its own preamble this step (that preamble was already
		// surfaced above), synthesize ONE concise, action-specific intent line
		// from the real operation so the user can always follow along — at most
		// one per action, never a fixed boilerplate. Distinct per-action content
		// (do_2) lets neo-execution-reliability's coalescing collapse only
		// genuine consecutive repeats. This is a SYNTHETIC stub (Neo generated
		// it from the tool name, e.g. "Layerx deposit."), so it rides the
		// EPHEMERAL Progress channel — never persisted, never counted as the
		// delivered answer. Routing it through durable Status was the bug where a
		// straight-to-task_complete turn marked itself "already narrated" and the
		// real task_complete summary was hidden, so the user saw only the stub.
		if strings.TrimSpace(res.Message.Content) == "" {
			if line := narrateBatch(res.Message.ToolCalls); line != "" {
				a.out.Progress(line)
			}
		}

		a.runToolCalls(ctx, res.Message.ToolCalls)
		workTouched = true
		// req.8.2 (N2): a plain tool dispatch does NOT reset the unified
		// unproductive counter — only genuine ACCEPTED progress does (an accepted
		// completion, which ends the turn). This is the fix for the
		// work->premature-complete->reject loop, where an interleaved real tool
		// call used to silently reset the completion-reject bound and let the
		// loop run to the step budget.
		if injectConvergeNudge {
			convergeNudged = true
			a.working = append(a.working, llm.GuidanceMessage("You already obtained the result and showed it — do NOT fetch or render it again. Give the user the answer now and call task_complete with it."))
		}
	}

	// [loop_discipline] step budget exhausted → NOT done. Never fabricate a
	// close: return an incomplete signal so the supervisor keeps going with a
	// fresh window rather than stopping with a partial.
	a.consolidateWorking()
	return fmt.Errorf("%w: reached the step budget without finishing. Progress so far: %s", ErrIncomplete, oneLine(a.lastToolSummary()))
}

// gateStrict reports whether THIS turn must finish through the grounded
// completion gate rather than the light structural path: always for a
// state-touching (money/chain) turn, and — under GateAllWork — for any turn
// that did substantive tool work. A pure conversational turn (no tools) stays
// on the light path.
func (a *Agent) gateStrict(stateTouched, workTouched bool) bool {
	return stateTouched || (a.cfg.GateAllWork && workTouched)
}

// pushGuidanceNudge routes a system-steering nudge through the guidance channel
// (req.1.1) and folds it into the ONE unified unproductive-attempt counter
// (req.8.1): it increments the counter and reports whether the bound is now
// exceeded. Completion-gate rejections and read-full/identity steers all push
// through here, so they share ONE bound with the no-progress repeats fed in the
// loop. The counter is reset ONLY on genuine accepted progress (req.8.2) — never
// by a plain tool dispatch. A cap of 0 disables the bound (unbounded nudging).
func (a *Agent) pushGuidanceNudge(text string, counter *int) (capExceeded bool) {
	if g := llm.GuidanceMessage(text); g.Content != "" {
		a.working = append(a.working, g)
	}
	*counter++
	return a.capExceeded(*counter)
}

// capExceeded reports whether the unified unproductive-attempt counter has
// passed its bound (config MaxGuidanceNudges, the shared unproductive-attempt
// cap). A cap of 0 disables the bound (unbounded).
func (a *Agent) capExceeded(counter int) bool {
	return a.cfg.MaxGuidanceNudges > 0 && counter > a.cfg.MaxGuidanceNudges
}

// escalateGuidance ends the turn with an honest stop-and-ask once the unified
// unproductive-attempt bound is exceeded (req.1.5, req.8): re-nudging — or
// re-attempting a premature completion — indefinitely is not the answer, so it
// marks a DETERMINISTIC blocker (the task supervisor then stops-and-asks the
// user rather than respawning into the same loop) and returns ErrIncomplete
// with an honest where-it-stands digest.
func (a *Agent) escalateGuidance(attempts int) error {
	a.lastFailureClass = delegate.ClassDeterministic
	a.consolidateWorking()
	return fmt.Errorf("%w: I made no productive progress after %d unproductive attempts (repeated steer/reject without closing it). Where it stands: %s", ErrIncomplete, attempts, oneLine(a.lastToolSummary()))
}

// oneLine collapses whitespace and clamps a digest to a single readable line
// for an error/diagnostic string.
func oneLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		s = s[:400] + "…"
	}
	return s
}

// Reset clears the live transcript + summary + goal (new conversation).
func (a *Agent) Reset() {
	a.working = nil
	a.summary = ""
	a.activeGoal = ""
}

// BestEffort returns the most honest "where things stand" digest the agent can
// produce WITHOUT having finished — the latest assistant narration, else the
// consolidated summary, else the last tool summary. The task supervisor uses
// it to deliver a truthful partial when a task hits its hard ceiling; it is
// never a fabricated success. Empty when there is genuinely nothing to report.
func (a *Agent) BestEffort() string {
	if s := strings.TrimSpace(lastAssistantText(a.working, 1600)); s != "" {
		return s
	}
	if s := strings.TrimSpace(a.summary); s != "" {
		return s
	}
	return strings.TrimSpace(a.lastToolSummary())
}

// Seed primes a freshly-minted agent with a resumed conversation's durable
// history (user/assistant text turns, oldest-first) and goal, so reopening a
// past thread — or continuing one after a restart — retains context instead of
// starting blank. No-op once the live transcript has any content, so it never
// clobbers an in-flight conversation.
func (a *Agent) Seed(history []llm.Message, goal string) {
	if len(a.working) > 0 || len(history) == 0 {
		return
	}
	a.working = append(a.working, history...)
	if a.activeGoal == "" {
		a.activeGoal = strings.TrimSpace(goal)
	}
}

// refaultEvery is how many loop steps pass between mid-turn page-fault
// refreshes. Small enough to track sub-goal drift in long tool loops,
// large enough that retrieval cost (one embed call) stays negligible.
const refaultEvery = 6

// lastAssistantText returns the most recent non-empty assistant content in
// the working transcript, truncated to maxLen bytes — the freshest signal of
// what the agent is currently pursuing.
func lastAssistantText(working []llm.Message, maxLen int) string {
	for i := len(working) - 1; i >= 0; i-- {
		m := working[i]
		if m.Role != llm.RoleAssistant {
			continue
		}
		c := strings.TrimSpace(m.Content)
		if c == "" {
			continue
		}
		if len(c) > maxLen {
			c = c[:maxLen]
		}
		return c
	}
	return ""
}

// maxThinkChars bounds the reasoning glimpse surfaced to the UI — enough to
// read the gist of the current thought, never the whole monologue.
const maxThinkChars = 480

// glimpseReasoning condenses a turn's chain-of-thought into a short, readable
// glimpse for the "thinking" channel: collapse whitespace, keep the leading
// substance, and cap the length. Returns "" when there is nothing worth
// showing so the surface stays quiet on non-reasoning models.
func glimpseReasoning(reasoning string) string {
	s := strings.TrimSpace(reasoning)
	if s == "" {
		return ""
	}
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > maxThinkChars {
		s = strings.TrimSpace(s[:maxThinkChars]) + "…"
	}
	return s
}

// theUserRe matches the generic third-person reference "the user" (and its
// possessive "the user's"), case-insensitive on the leading "the", with word
// boundaries so "users" (plural) and mid-word matches are left alone.
var theUserRe = regexp.MustCompile(`\b[Tt]he user('s)?\b`)

// nameReasoning personalises the model's VISIBLE reasoning (Q1): the user can
// read Neo's chain-of-thought, so a generic "the user asked …" reads coldly
// where "Andrew asked …" reads like Neo actually knows them. It rewrites the
// "the user" / "the user's" reference to the user's preferred name. This is a
// DISPLAY-only transform on the thinking channel — the model's underlying
// reasoning tokens are untouched and reasoning is never persisted, so there is
// no determinism/replay concern. It only fires when a preferred name is known;
// otherwise the text is returned verbatim. The prompt already steers the model
// to name the user in its thinking (systemPrompt), so this is the deterministic
// backstop for the complete-text (fold-time) reasoning path.
func (a *Agent) nameReasoning(text string) string {
	if text == "" {
		return text
	}
	// Identity net on the VISIBLE reasoning channel (P0): the model's private
	// chain-of-thought is streamed to the user, so a self-identification as the
	// underlying LLM ("I'm Grok", "as Grok") would leak straight through here.
	// Scrub it deterministically before anything renders — reasoning is never
	// persisted, so this display-only rewrite carries no replay concern.
	text, _ = scrubIdentity(a.agentName(), text)
	name := strings.TrimSpace(a.preferredName)
	if name == "" {
		return text
	}
	return theUserRe.ReplaceAllStringFunc(text, func(m string) string {
		if strings.HasSuffix(m, "'s") {
			return name + "'s"
		}
		return name
	})
}

// collectSurfaced records the cortex URIs injected into this turn so the loop
// can attest them as USED on a successful completion — the cortex usage-salience
// + EMA learning signal. Accumulates across the opening fault and every
// mid-turn refault (a set, so re-surfacing across refaults isn't double-counted
// within the turn).
func collectSurfaced(set map[string]struct{}, retrieved []memory.Snippet, procedural []memory.Pattern) {
	for _, s := range retrieved {
		if s.URI != "" {
			set[s.URI] = struct{}{}
		}
	}
	for _, p := range procedural {
		if p.URI != "" {
			set[p.URI] = struct{}{}
		}
	}
}

// collectSurfacedSnips records the retrieved snippets (URI -> text/type) so the
// completion's rejection gate can score them for off-topic relevance. Keyed by
// URI so re-surfacing across refaults dedups within the turn. Patterns are
// intentionally excluded: they are coverage-gated and never penalized.
func collectSurfacedSnips(set map[string]memory.Snippet, retrieved []memory.Snippet) {
	for _, s := range retrieved {
		if s.URI != "" {
			set[s.URI] = s
		}
	}
}

// attestTurn closes the usage-learning loop for a successful turn. It splits
// the surfaced set into USED (reinforced) and IGNORED (penalized): a
// surfaced-but-ignored memory is one the rejection gate flags as off-topic to
// the produced turn AND of a non-pinned type. The two attests are disjoint so
// no memory is both bumped and decremented in the same turn. Best-effort: a
// nil pager or empty set is a no-op, and neither attest blocks the turn.
func (a *Agent) attestTurn(ctx context.Context, surfaced map[string]struct{}, surfacedSnips map[string]memory.Snippet, turnInput, answer string) {
	if a.pager == nil || len(surfaced) == 0 {
		return
	}
	// Determine which surfaced memories were demonstrably ignored (off-topic
	// to the produced turn, non-pinned type). turnText pairs the ask with the
	// produced answer so a memory's relevance is judged against what the turn
	// actually delivered.
	turnText := strings.TrimSpace(turnInput + "\n" + answer)
	snips := make([]memory.Snippet, 0, len(surfacedSnips))
	for _, s := range surfacedSnips {
		snips = append(snips, s)
	}
	ignored := a.pager.RejectionCandidates(turnText, snips)
	ignoredSet := make(map[string]bool, len(ignored))
	for _, u := range ignored {
		ignoredSet[u] = true
	}

	used := make([]string, 0, len(surfaced))
	for u := range surfaced {
		if ignoredSet[u] {
			continue
		}
		used = append(used, u)
	}
	sort.Strings(used) // deterministic order (map iteration is randomized)

	intentID := a.turnIntentID()
	if len(used) > 0 {
		a.pager.AttestUsed(ctx, intentID, used, true)
	}
	if len(ignored) > 0 {
		a.pager.AttestRejected(ctx, intentID, ignored)
	}
}

// turnIntentID is the per-turn attestation IntentID, conversation-scoped for
// audit. Falls back to "cli" when there is no conversation (the CLI path).
func (a *Agent) turnIntentID() string {
	cid := a.convID
	if cid == "" {
		cid = "cli"
	}
	return fmt.Sprintf("neo-turn:%s:%d", cid, a.turnSeq)
}

func (a *Agent) faultMemory(ctx context.Context, q string) []memory.Snippet {
	if a.pager == nil {
		return nil
	}
	snips, err := a.pager.Retrieve(ctx, q)
	if err != nil {
		return nil
	}
	return snips
}

// ambientMemory returns the THIN ambient memory seed injected into the system
// block this turn (v3 #1: reasoning-time retrieval). It is the bulk semantic
// retrieval demoted from a forced blob to a small seed, capped to
// cfg.AmbientRetrievalTopK. A cap of 0 means fully tool-driven retrieval: no
// ambient seed at all — the model pulls exactly what it needs mid-thought with
// the memory_recall tool. The pinned tier (identity, hard rules, learned
// guidance, active goal, user profile) is injected separately and is NEVER
// gated by this cap.
func (a *Agent) ambientMemory(ctx context.Context, q string) []memory.Snippet {
	if a.cfg.AmbientRetrievalTopK <= 0 {
		return nil
	}
	snips := a.faultMemory(ctx, q)
	if len(snips) > a.cfg.AmbientRetrievalTopK {
		snips = snips[:a.cfg.AmbientRetrievalTopK]
	}
	return snips
}

// recallTurns asks the optional conversational recaller for the most relevant
// PAST turns of this thread (beyond the live transcript). Best-effort: a nil
// recaller or empty result simply yields no recall section.
func (a *Agent) recallTurns(ctx context.Context, q string) []recall.Hit {
	if a.recaller == nil {
		return nil
	}
	return a.recaller.Relevant(ctx, q)
}

func (a *Agent) faultPatterns(ctx context.Context, q string) []memory.Pattern {
	if a.pager == nil {
		return nil
	}
	pats, err := a.pager.Procedural(ctx, q)
	if err != nil {
		return nil
	}
	return pats
}

// faultTriggers surfaces behavioral guidance whose trigger matches this turn by
// embedding similarity, independent of global salience (Phase 3). This is the
// structural fix for "Neo forgets a learned behavior": a learned constraint or
// trigger-bearing pattern fires on the turns it is ABOUT even after its
// salience has decayed below the pinned learned-guidance cap. Best-effort.
func (a *Agent) faultTriggers(ctx context.Context, q string) []memory.Snippet {
	if a.pager == nil {
		return nil
	}
	return a.pager.TriggeredGuidance(ctx, q)
}

func (a *Agent) chatWithRetry(ctx context.Context, req llm.ChatRequest) (*llm.ChatResult, error) {
	var lastErr error
	for attempt := 0; attempt <= 2; attempt++ {
		if attempt > 0 {
			if !backoff(ctx, attempt) {
				break
			}
		}
		res, err := a.main.Chat(ctx, req)
		if err == nil {
			return res, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		// A deterministic 4xx validation reject won't change on a retry of the
		// identical body — stop the ladder rather than burn calls (and metered
		// spend) re-sending it.
		if errors.Is(err, llm.ErrProviderRejected) {
			break
		}
	}
	return nil, lastErr
}

func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall) {
	// P2-5: dispatch INDEPENDENT tool calls in a turn concurrently (bounded
	// by cfg.ToolDispatchConcurrency), preserving result ordering + per-tool
	// observer events. When concurrency <=0 or there's a single call, the
	// path degenerates to the legacy serial loop (no goroutine overhead).
	// Determinism + i6 are preserved: results are assembled in CALL order
	// (index i) regardless of completion order, and the observer start/end
	// events fire in order so the UI's per-call viewport correlation holds.
	n := len(calls)
	if n <= 1 || a.cfg.ToolDispatchConcurrency <= 0 {
		a.runToolCallsSerial(ctx, calls)
		return
	}

	conc := a.cfg.ToolDispatchConcurrency
	if conc > n {
		conc = n
	}

	type dispatchResult struct {
		content string
		isErr   bool
		class   delegate.FailureClass
	}

	// Fire ALL ToolStart observer events up front, in call order, so the
	// surface paints every live viewport the instant the batch is dispatched
	// (matching the serial path's per-call start event). stepIDs are computed
	// once here and reused for the end events.
	stepIDs := make([]string, n)
	parsedArgs := make([]map[string]interface{}, n)
	for i, call := range calls {
		name := call.Function.Name
		args, perr := call.ParseArgs()
		if perr != nil {
			// Parse failure: surface immediately and mark so dispatch skips it.
			a.working = append(a.working, llm.ToolResult(call.ID, name, fmt.Sprintf("could not parse arguments (%v). Re-issue the call with valid JSON arguments.", perr)))
			parsedArgs[i] = nil // sentinel: already handled, do not dispatch
			stepIDs[i] = ""
			continue
		}
		parsedArgs[i] = args
		stepID := call.ID
		if stepID == "" {
			stepID = fmt.Sprintf("call-%d", i)
		}
		stepIDs[i] = stepID
		a.out.Status("• " + name)
		if a.observer != nil {
			a.observer(ToolEvent{ID: stepID, Name: name, Args: args, Phase: ToolStart})
		}
	}

	// Batch idempotency (NE-3, req 5.x): collapse equivalent non-idempotent
	// state-touching calls (core_execute with the same resolved intent) so two
	// of them in one batch don't both submit and deterministically 409 against
	// each other. Only the canonical (first) occurrence is dispatched; each
	// duplicate joins the canonical's result. Independent reversible calls are
	// untouched and keep their concurrent dispatch.
	joinTo := dedupStateTouching(calls)

	// Dispatch the parseable, canonical calls concurrently through a bounded
	// semaphore. Joined duplicates are skipped here and filled after the wait.
	results := make([]dispatchResult, n)
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, call := range calls {
		if parsedArgs[i] == nil || joinTo[i] >= 0 {
			// Parse-failed calls were already appended above; joined duplicates
			// take the canonical call's result after the wait. Skip dispatch.
			continue
		}
		wg.Add(1)
		go func(i int, call llm.ToolCall, args map[string]interface{}) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content, isErr, class := a.dispatchWithRetry(ctx, call.Function.Name, args)
			results[i] = dispatchResult{content: content, isErr: isErr, class: class}
		}(i, call, parsedArgs[i])
	}
	wg.Wait()

	// Join each deduped duplicate to its canonical call's result (the canonical
	// index is always earlier and was dispatched above, so it is ready now).
	for i := range calls {
		if parsedArgs[i] != nil && joinTo[i] >= 0 {
			results[i] = results[joinTo[i]]
		}
	}

	// Append results + fire ToolEnd events in CALL order so the transcript
	// and the observer stream stay deterministic regardless of completion order.
	for i, call := range calls {
		if parsedArgs[i] == nil {
			continue // parse-failed: already appended above
		}
		name := call.Function.Name
		content := results[i].content
		isErr := results[i].isErr
		// Record the shared failure class (most recent classified failure wins)
		// for the supervisor. Single-threaded here (the concurrent goroutines
		// only wrote results[i]), so no race on a.lastFailureClass.
		a.noteFailureClass(results[i].class)
		// Cap the transcript copy: a single oversized tool result can blow
		// the provider's request-body byte cap on its own. The observer
		// below still gets the full, untruncated content so the product
		// shows real evidence.
		a.working = append(a.working, llm.ToolResult(call.ID, name, a.capToolResult(content)))
		// Continuous-memory (task 6.1): record the FULL tool result to the
		// durable cortex transcript (cortex spills oversized payloads itself).
		a.cmRecordToolResult(name, content)
		if a.observer != nil {
			a.observer(ToolEvent{ID: stepIDs[i], Name: name, Args: parsedArgs[i], Result: content, IsErr: isErr, Phase: ToolEnd})
		}
	}
}

// runToolCallsSerial is the legacy single-threaded dispatch path. It's
// kept for the concurrency<=0 config and for single-call batches (where
// goroutine spin-up would be pure overhead). Behaviour is byte-identical
// to the pre-P2-5 loop.
func (a *Agent) runToolCallsSerial(ctx context.Context, calls []llm.ToolCall) {
	// Batch idempotency (NE-3, req 5.x), serial path: an equivalent
	// state-touching duplicate (core_execute with the same resolved intent)
	// joins the canonical call's cached result instead of running again. The
	// canonical index is always earlier, so its result is ready by the time a
	// duplicate is reached.
	joinTo := dedupStateTouching(calls)
	type dispatchResult struct {
		content string
		isErr   bool
		class   delegate.FailureClass
	}
	results := make([]dispatchResult, len(calls))
	for i, call := range calls {
		name := call.Function.Name
		args, perr := call.ParseArgs()
		if perr != nil {
			a.working = append(a.working, llm.ToolResult(call.ID, name, fmt.Sprintf("could not parse arguments (%v). Re-issue the call with valid JSON arguments.", perr)))
			continue
		}
		// Stable surface id for this call: some providers omit tool_call ids, so
		// fall back to a per-turn index. Shared across the start/end pair below
		// so the UI updates ONE viewport (running→done) instead of dropping the
		// event (empty id) or stacking duplicates.
		stepID := call.ID
		if stepID == "" {
			stepID = fmt.Sprintf("call-%d", i)
		}
		a.out.Status("• " + name)
		// Paint the live viewport the instant the call is dispatched (no result
		// yet) so the surface shows Neo at work — a terminal opening, a browser
		// navigating — before the tool returns.
		if a.observer != nil {
			a.observer(ToolEvent{ID: stepID, Name: name, Args: args, Phase: ToolStart})
		}
		var content string
		var isErr bool
		var class delegate.FailureClass
		if joinTo[i] >= 0 {
			// Deduped duplicate: reuse the canonical call's result; do not
			// submit the same state-touching work twice.
			content, isErr, class = results[joinTo[i]].content, results[joinTo[i]].isErr, results[joinTo[i]].class
		} else {
			content, isErr, class = a.dispatchWithRetry(ctx, name, args)
		}
		results[i] = dispatchResult{content: content, isErr: isErr, class: class}
		// Record the shared failure class (most recent classified failure wins)
		// so the supervisor reads the SAME classification (NE-5).
		a.noteFailureClass(class)
		// Cap the transcript copy: a single oversized tool result (large
		// fetch / file read / MCP payload) can blow the provider's request-
		// body byte cap on its own. The observer below still gets the full,
		// untruncated content so the product shows real evidence.
		a.working = append(a.working, llm.ToolResult(call.ID, name, a.capToolResult(content)))
		// Continuous-memory (task 6.1): record the FULL tool result to the
		// durable cortex transcript (cortex spills oversized payloads itself).
		a.cmRecordToolResult(name, content)
		// Surface the completed work (command output, fetched page, file
		// contents, web-search snippets, …) so the product renders real
		// evidence, not just a synthesized answer.
		if a.observer != nil {
			a.observer(ToolEvent{ID: stepID, Name: name, Args: args, Result: content, IsErr: isErr, Phase: ToolEnd})
		}
	}
}

// noteFailureClass records the most recent classified tool FAILURE this turn so
// the task supervisor (LastFailureClass) reads the SAME taxonomy the dispatch
// ladder used. A clean dispatch (ClassNone) does not clear a prior failure: a
// deterministic blocker remains the reason the turn is stuck even if later
// reversible calls succeed. Called only from the single-threaded result-
// assembly paths, so it never races the concurrent dispatch goroutines.
func (a *Agent) noteFailureClass(class delegate.FailureClass) {
	if class != delegate.ClassNone {
		a.lastFailureClass = class
	}
}

// LastFailureClass reports the shared FailureClass of the most recent
// classified tool failure in the current turn (delegate.ClassNone when none).
// The task supervisor reads it after Chat returns to decide whether a
// non-clean exit is a deterministic blocker (stop-and-ask, no respawn) or a
// transient/model/stall failure (the existing respawn path).
func (a *Agent) LastFailureClass() delegate.FailureClass { return a.lastFailureClass }

// dispatchWithRetry runs one tool call with the recovery ladder: bounded
// retries for transport/invocation errors (ladder 1); on exhaustion it
// returns a descriptive failure as the tool result so the model can adapt
// (ladder 2/4) rather than the harness crashing.
//
// It reads the shared failure taxonomy (delegate.ClassOf): a DETERMINISTIC
// failure (a 4xx that recurs identically — a denied permission, an invalid or
// unparseable request) is returned as a SINGLE terminal result with no further
// attempts, mirroring chatWithRetry's ErrProviderRejected short-circuit. This
// stops the blind-retry amplifier (NE-1). Conflict (already-in-flight 409) and
// pending (structured clarify) are deliberately NOT short-circuited here: they
// are left to their dedicated handlers (attach-to-existing / slot-fill); for
// now they keep the bounded-retry behavior. The returned FailureClass is the
// class of the failure that ended the dispatch (delegate.ClassNone on success),
// surfaced so the supervisor reads the SAME classification.
func (a *Agent) dispatchWithRetry(ctx context.Context, name string, args map[string]interface{}) (string, bool, delegate.FailureClass) {
	// read_overflow is synthetic and agent-owned (the overflow store lives on
	// the agent, not the Manager): page a truncated result back in and mark it
	// read (the read-full latch). It never retries or routes to the Manager.
	if name == readOverflowTool {
		content, isErr := a.readOverflow(args)
		return content, isErr, delegate.ClassNone
	}
	if a.tools == nil {
		return "no tools are available in this session.", true, delegate.ClassNone
	}
	var lastErr error
	for attempt := 0; attempt <= a.cfg.MaxRetriesPerTool; attempt++ {
		if attempt > 0 {
			if !backoff(ctx, attempt) {
				break
			}
		}
		content, isErr, err := a.tools.Dispatch(ctx, name, args)
		if err == nil {
			return content, isErr, delegate.ClassNone
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
		// A deterministic failure will recur identically on a retry of the same
		// request — stop the ladder now and return a single terminal result so
		// the agent stops-and-asks instead of looping (and, on a metered
		// gateway, re-spending). Mirrors chatWithRetry's ErrProviderRejected
		// short-circuit.
		if delegate.ClassOf(err) == delegate.ClassDeterministic {
			return fmt.Sprintf("tool %q failed and this is a permanent error that will not change on a retry: %v. Don't repeat this call — adjust the approach or ask the user how they'd like to proceed.", name, lastErr), true, delegate.ClassDeterministic
		}
	}
	return fmt.Sprintf("tool %q failed after %d attempts: %v. Consider a different approach.", name, a.cfg.MaxRetriesPerTool+1, lastErr), true, delegate.ClassOf(lastErr)
}

func (a *Agent) lastToolSummary() string {
	var lines []string
	for i := len(a.working) - 1; i >= 0 && len(lines) < 3; i-- {
		m := a.working[i]
		if m.Role == llm.RoleTool {
			lines = append([]string{"- " + m.Name + ": " + truncate(strings.TrimSpace(m.Content), 280)}, lines...)
		}
	}
	if len(lines) == 0 {
		return "(no tool results yet)"
	}
	return strings.Join(lines, "\n")
}

func (a *Agent) budgetPct(system string) int {
	if a.cfg.ContextWindowTokens <= 0 {
		return 0
	}
	used := memory.EstimateTokens(system) + estimateMessagesTokens(a.working) + a.schemaTokens
	pct := used * 100 / a.cfg.ContextWindowTokens
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

// stateTouchDedupKey returns a dedup key for a non-idempotent, state-touching
// tool call (core_execute), keyed on its RESOLVED intent so two equivalent
// calls in one assistant batch collapse to a single run (NE-3, req 5.1/5.3).
// The daemon mints a deterministic intent id from the prose, so two
// core_execute calls with identical intent text would otherwise either both
// submit the same work or deterministically 409 against each other. Returns
// ("", false) for reversible / independent calls, which always keep their
// concurrent dispatch (req 5.2 — no latency regression for the common case).
func stateTouchDedupKey(call llm.ToolCall) (string, bool) {
	if call.Function.Name != tools.CoreExecuteTool {
		return "", false
	}
	args, err := call.ParseArgs()
	if err != nil {
		return "", false // unparseable: let the call surface its own parse error
	}
	intent, _ := args["intent"].(string)
	intent = strings.TrimSpace(intent)
	if intent == "" {
		return "", false
	}
	return tools.CoreExecuteTool + "\x00" + intent, true
}

// dedupStateTouching returns, for each call index, the index of an EARLIER call
// in the same batch it is an equivalent duplicate of (-1 when it is canonical
// or not a dedup candidate). Only non-idempotent state-touching calls are
// deduped; independent reversible calls are always -1 so they keep dispatching
// concurrently. The canonical (first) occurrence runs once and the duplicates
// join its result.
func dedupStateTouching(calls []llm.ToolCall) []int {
	joinTo := make([]int, len(calls))
	firstByKey := map[string]int{}
	for i := range calls {
		joinTo[i] = -1
		key, ok := stateTouchDedupKey(calls[i])
		if !ok {
			continue
		}
		if j, seen := firstByKey[key]; seen {
			joinTo[i] = j
		} else {
			firstByKey[key] = i
		}
	}
	return joinTo
}

func batchSignature(calls []llm.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Function.Name+"("+c.Function.Arguments+")")
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
}

// stallWindow bounds the recent-signature ring the cycle-aware no-progress guard
// scans to catch a rotating (A → B → A → B) spiral.
const stallWindow = 6

// sigInWindow reports whether sig already appears in the recent-signature window.
func sigInWindow(win []string, sig string) bool {
	for _, s := range win {
		if s == sig {
			return true
		}
	}
	return false
}

// pushWindow appends sig to the recent-signature ring, capping it at max entries.
func pushWindow(win []string, sig string, max int) []string {
	win = append(win, sig)
	if len(win) > max {
		win = win[len(win)-max:]
	}
	return win
}

func estimateToolTokens(schemas []llm.Tool) int {
	if len(schemas) == 0 {
		return 0
	}
	b, err := json.Marshal(schemas)
	if err != nil {
		return 0
	}
	return memory.EstimateTokens(string(b))
}

// maxRequestBodyBytes is the approximate serialized request-body ceiling Neo
// keeps the outgoing window under. It sits below the provider's hard cap
// (Fireworks rejects bodies over 1,048,576 bytes with HTTP 413
// "body_too_large") with headroom for JSON structure + escaping that the byte
// estimate does not model exactly.
const maxRequestBodyBytes = 900000

// maxToolResultChars caps how much of a single tool result enters the working
// transcript. A large fetch / file read / MCP payload appended verbatim can
// blow the provider's request-body byte cap on its own; the MCL walker bounds
// tool output for the same reason (runtime/walker.go). The full, untruncated
// result is still handed to the observer so the product can show real
// evidence — only the model-facing transcript copy is bounded.
const maxToolResultChars = 32000

// estimateToolBytes returns the serialized byte size of the tool schemas,
// which ride along on every chat request. Byte proxy for windowBytes,
// distinct from estimateToolTokens (token proxy).
func estimateToolBytes(schemas []llm.Tool) int {
	if len(schemas) == 0 {
		return 0
	}
	b, err := json.Marshal(schemas)
	if err != nil {
		return 0
	}
	return len(b)
}

// windowBytes approximates the serialized chat-completions request body in
// BYTES: the system block + every working message's content and tool-call
// arguments + the tool schemas. It is a byte proxy (NOT a token estimate)
// because the provider's 413 cap is on raw body bytes, which the token budget
// does not track. The per-message constant covers JSON structural overhead
// (role/keys/braces/quoting).
func (a *Agent) windowBytes(system string) int {
	total := len(system) + a.schemaBytes
	for _, m := range a.working {
		total += len(m.Content) + 48
		for _, tc := range m.ToolCalls {
			total += len(tc.Function.Name) + len(tc.Function.Arguments) + 48
		}
	}
	return total
}

// assembleWindow builds the prompt-cache-friendly window (P1-2): a byte-stable
// system prefix, the append-only transcript, then ONE trailing dynamic tail
// (turn-varying memory + context-budget stat). The stable prefix is identical
// across every turn of a session so the provider's longest-stable-prefix cache
// stays warm; only the trailing tail (and the growing transcript) change turn
// to turn. Window order: [stable system] + [transcript] + [dynamic tail].
//
// The dynamic block keeps its system role and exact rendered content — only its
// POSITION moves (formerly concatenated into the front system message, now a
// trailing message after the transcript). The transcript slice is never mutated:
// the inner append allocates a fresh backing array, so the outer append onto it
// cannot alias a.working.
func assembleWindow(stableSystem string, transcript []llm.Message, dynamicTail string) []llm.Message {
	return append(append([]llm.Message{llm.SystemMessage(stableSystem)}, transcript...), llm.SystemMessage(dynamicTail))
}

func estimateMessagesTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		// Image-aware: an inline base64 image is charged a flat per-image cost
		// rather than its (5-10x larger) base64 length, so a single thumbnail
		// can't trip a spurious compaction (Phase 4.2).
		total += imageAwareTokens(m.Content) + 4
		for _, tc := range m.ToolCalls {
			total += memory.EstimateTokens(tc.Function.Name) + memory.EstimateTokens(tc.Function.Arguments) + 4
		}
	}
	return total
}

// backoff sleeps a bounded, attempt-scaled interval, honoring ctx
// cancellation. Returns false if the context was canceled during the wait.
func backoff(ctx context.Context, attempt int) bool {
	d := time.Duration(attempt) * 300 * time.Millisecond
	if d > 2*time.Second {
		d = 2 * time.Second
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
