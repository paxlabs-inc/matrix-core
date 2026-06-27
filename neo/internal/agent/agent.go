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
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/cassandra"
	"matrix/neo/internal/config"
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
	// completion gate on state-touching turns (Phase 3). nil falls back to the
	// deterministic local grounding check. AuditObserver streams cassandra.*
	// audit events to the harness; nil discards them.
	Adjudicator   *cassandra.Adjudicator
	AuditObserver AuditObserver

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
	}
	a.turnSeq++
	a.working = append(a.working, llm.UserMessage(userInput))

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
	retrieved := a.ambientMemory(ctx, userInput)
	procedural := a.faultPatterns(ctx, userInput)
	triggered := a.faultTriggers(ctx, userInput)
	// Conversational recall: relevant PAST turns beyond the live transcript —
	// the additive read-lane that keeps an unbounded thread coherent. Reset on a
	// topic pivot so a fresh subject starts clean.
	recalled := a.recallTurns(ctx, userInput)
	if pivoted {
		recalled = nil
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
	prevSig := ""
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

	// P2-7: adaptive step budget. Track distinct tool names dispatched so far
	// (tool-call breadth) as the complexity signal. The effective budget is
	// recomputed each iteration from [StepBudgetMin, StepBudgetMax] via
	// effectiveStepBudget, so a turn that fans out to many tools earns more
	// room, while a simple/stalling turn stays at the floor. When adaptation
	// is disabled (StepBudgetMin==0, the default), effectiveStepBudget returns
	// StepBudgetMax (== StepBudget == 50) and the loop is byte-identical to
	// the pre-P2-7 fixed-budget loop.
	distinctToolSet := map[string]struct{}{}

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
		if a.cfg.AmbientRetrievalTopK > 0 && step > 0 && step%refaultEvery == 0 {
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

		pinned := ""
		if a.pager != nil {
			pinned = a.pager.Pinned(ctx, a.activeGoal)
		}
		baseSystem := a.buildSystem(pinned, retrieved, procedural, triggered, recalled)

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
		pct := a.budgetPct(baseSystem)
		// P1-2: the byte-stable system prefix (charter + ground truth) is the
		// FIRST message; the turn-varying memory block + context-budget stat
		// move into ONE trailing message AFTER the append-only transcript, so
		// the prefix stays byte-identical turn-over-turn and rides the
		// provider's longest-stable-prefix cache. baseSystem (stable + tail)
		// still drives the budget stat above.
		tail := a.dynamicTail(pinned, retrieved, procedural, triggered, recalled) + fmt.Sprintf("\n\n[context: %d%% used]\n", pct)

		window := assembleWindow(a.stableSystem(), a.working, tail)

		// Live "typing" channel: stream the model's incremental fragments as
		// they generate so the user sees Neo thinking + answering in real time
		// instead of staring at a blank surface until the whole turn lands.
		// reasoning → the live thinking channel; content → the answer being
		// typed. step segments the stream so the client resets per turn.
		streamedReasoning := false
		onDelta := func(d llm.Delta) {
			if d.Reasoning != "" {
				streamedReasoning = true
				a.out.Delta(step, "reasoning", d.Reasoning)
			}
			if d.Content != "" {
				a.out.Delta(step, "content", d.Content)
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
					window = assembleWindow(a.stableSystem(), a.working, tail)
					res, err = a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas, OnDelta: onDelta})
				}
				if err != nil && errors.Is(err, llm.ErrRequestTooLarge) {
					a.compact(ctx, "hard")
					baseSystem = a.buildSystem(pinned, retrieved, procedural, triggered, recalled)
					tail = a.dynamicTail(pinned, retrieved, procedural, triggered, recalled) + fmt.Sprintf("\n\n[context: %d%% used]\n", a.budgetPct(baseSystem))
					window = assembleWindow(a.stableSystem(), a.working, tail)
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
			if think := glimpseReasoning(res.Message.Reasoning); think != "" {
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
				a.working = append(a.working, llm.UserMessage("(your last message was cut off by the output limit — don't inline large payloads in prose; call a tool with compact arguments, or give a concise final answer)"))
				continue
			}
			if answer == "" {
				// anti-premature: empty AND no tools → nudge once to continue.
				a.working = append(a.working, llm.UserMessage("(continue: either call a tool to make progress, or give the final answer)"))
				continue
			}
			// A turn that did real work may not end with an unaudited bare
			// message: nudge toward the completion gate so completion is proven,
			// not assumed. A pure conversational turn (no tools) rides the light
			// path (i_cass_5, placement-by-reversibility) and ends frictionlessly.
			if a.gateStrict(stateTouched, workTouched) {
				a.working = append(a.working, llm.UserMessage("(you did real work this turn, so don't finish with a plain message — call task_complete with an honest completeness object: a summary for the user, coverage, the real evidence behind your claims, and anything still open.)"))
				continue
			}
			a.finishTurn(ctx, answer, surfaced, surfacedSnips, userInput)
			// [control.loop] step_6: cooperative compaction at a clean boundary.
			if a.budgetPct(a.buildSystem(pinned, retrieved, procedural, triggered, recalled)) >= a.cfg.SoftPct {
				a.compact(ctx, "soft")
			}
			return nil
		}

		// Completion gate (Cassandra Phase 1): the model called task_complete.
		// Adjudicate the completeness object against the working transcript (the
		// ground truth — real tool results) before the turn may end. Sibling
		// tool calls in the same batch still run (they make progress); every
		// tool_call receives a result so the transcript stays well-formed.
		if cc, rest := splitCompletion(res.Message.ToolCalls); cc != nil {
			if len(rest) > 0 {
				a.runToolCalls(ctx, rest)
				workTouched = true
				if batchTouchesState(rest) {
					stateTouched = true
				}
				a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, "You called task_complete alongside other tools. Review their results above, then call task_complete on its own once you are actually done."))
				continue
			}
			verdict := a.validateCompletion(ctx, cc, a.gateStrict(stateTouched, workTouched), stateTouched, userInput)
			if verdict.ok {
				a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, "Completion accepted."))
				a.finishTurn(ctx, verdict.answer, surfaced, surfacedSnips, userInput)
				if a.budgetPct(a.buildSystem(pinned, retrieved, procedural, triggered, recalled)) >= a.cfg.SoftPct {
					a.compact(ctx, "soft")
				}
				return nil
			}
			// Rejected: feed the actionable reason back as the tool result and
			// keep working (advisor-not-effector — the agent enacts the fix).
			a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, verdict.feedback))
			continue
		}

		// Surface any preamble the model wrote alongside its tool calls.
		if c := strings.TrimSpace(res.Message.Content); c != "" {
			a.out.Status(c)
		}

		// No-progress detection: identical consecutive tool-call batches.
		sig := batchSignature(res.Message.ToolCalls)
		if sig == prevSig {
			repeats++
		} else {
			repeats = 0
			prevSig = sig
		}
		if repeats >= a.cfg.NoProgressStall {
			// No-progress stall: do NOT fabricate a close. Return an incomplete
			// signal so the supervisor can respawn a fresh agent and continue —
			// the task is not done. (On the bare CLI path, with no supervisor, the
			// wrapped reason is printed.)
			a.consolidateWorking()
			return fmt.Errorf("%w: repeating the same step without progress. Where it got stuck: %s", ErrIncomplete, oneLine(a.lastToolSummary()))
		}

		// P2-7: record distinct tool names for the adaptive budget signal.
		for _, c := range res.Message.ToolCalls {
			distinctToolSet[c.Function.Name] = struct{}{}
		}

		a.runToolCalls(ctx, res.Message.ToolCalls)
		workTouched = true
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

	// Dispatch the parseable calls concurrently through a bounded semaphore.
	results := make([]dispatchResult, n)
	sem := make(chan struct{}, conc)
	var wg sync.WaitGroup
	for i, call := range calls {
		if parsedArgs[i] == nil {
			// Parse-failed calls were already appended above; skip dispatch.
			continue
		}
		wg.Add(1)
		go func(i int, call llm.ToolCall, args map[string]interface{}) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			content, isErr := a.dispatchWithRetry(ctx, call.Function.Name, args)
			results[i] = dispatchResult{content: content, isErr: isErr}
		}(i, call, parsedArgs[i])
	}
	wg.Wait()

	// Append results + fire ToolEnd events in CALL order so the transcript
	// and the observer stream stay deterministic regardless of completion order.
	for i, call := range calls {
		if parsedArgs[i] == nil {
			continue // parse-failed: already appended above
		}
		name := call.Function.Name
		content := results[i].content
		isErr := results[i].isErr
		// Cap the transcript copy: a single oversized tool result can blow
		// the provider's request-body byte cap on its own. The observer
		// below still gets the full, untruncated content so the product
		// shows real evidence.
		a.working = append(a.working, llm.ToolResult(call.ID, name, capToolResult(content)))
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
		content, isErr := a.dispatchWithRetry(ctx, name, args)
		// Cap the transcript copy: a single oversized tool result (large
		// fetch / file read / MCP payload) can blow the provider's request-
		// body byte cap on its own. The observer below still gets the full,
		// untruncated content so the product shows real evidence.
		a.working = append(a.working, llm.ToolResult(call.ID, name, capToolResult(content)))
		// Surface the completed work (command output, fetched page, file
		// contents, web-search snippets, …) so the product renders real
		// evidence, not just a synthesized answer.
		if a.observer != nil {
			a.observer(ToolEvent{ID: stepID, Name: name, Args: args, Result: content, IsErr: isErr, Phase: ToolEnd})
		}
	}
}

// dispatchWithRetry runs one tool call with the recovery ladder: bounded
// retries for transport/invocation errors (ladder 1); on exhaustion it
// returns a descriptive failure as the tool result so the model can adapt
// (ladder 2/4) rather than the harness crashing.
func (a *Agent) dispatchWithRetry(ctx context.Context, name string, args map[string]interface{}) (string, bool) {
	if a.tools == nil {
		return "no tools are available in this session.", true
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
			return content, isErr
		}
		lastErr = err
		if ctx.Err() != nil {
			break
		}
	}
	return fmt.Sprintf("tool %q failed after %d attempts: %v. Consider a different approach.", name, a.cfg.MaxRetriesPerTool+1, lastErr), true
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

func batchSignature(calls []llm.ToolCall) string {
	parts := make([]string, 0, len(calls))
	for _, c := range calls {
		parts = append(parts, c.Function.Name+"("+c.Function.Arguments+")")
	}
	sort.Strings(parts)
	return strings.Join(parts, "|")
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

// capToolResult bounds a single tool result to maxToolResultChars for the
// transcript, keeping a head + tail so both the leading structure and any
// trailing digest/summary survive (some tools place the salient result at the
// end). Cuts are byte-wise; a split multibyte rune is harmless (JSON marshal
// replaces it) and the marker makes the truncation explicit.
func capToolResult(s string) string {
	if len(s) <= maxToolResultChars {
		return s
	}
	head := maxToolResultChars * 3 / 4
	tail := maxToolResultChars - head
	return s[:head] +
		fmt.Sprintf("\n…(tool result truncated for working memory: %d of %d bytes shown)…\n", maxToolResultChars, len(s)) +
		s[len(s)-tail:]
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
