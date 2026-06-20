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
	"time"

	"matrix/cassandra"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/recall"
	"matrix/neo/internal/tools"
)

// Consolidator is the background write-back hook: it receives a completed
// turn's transcript and promotes durable learnings to cortex out-of-band.
// Implemented by internal/writeback; optional (nil disables write-back).
type Consolidator interface {
	Consolidate(transcript string)
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

	// topic tracks the rolling topic centroid so a pivot to an unrelated
	// subject can reset the per-turn retrieved working set (Phase 3). nil when
	// no embedder is available (topic detection is inherently semantic).
	topic *topicTracker
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
	// guidance for this ask (once/turn).
	retrieved := a.faultMemory(ctx, userInput)
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

	for step := 0; step < a.cfg.StepBudget; step++ {
		// Mid-turn page-fault refresh: long tool loops drift away from the
		// opening ask, so periodically re-fault against the latest assistant
		// narration. Injection stays system-block-only — the transcript
		// never pays for it.
		if step > 0 && step%refaultEvery == 0 {
			q := userInput
			if c := lastAssistantText(a.working, 400); c != "" {
				q = q + "\n" + c
			}
			retrieved = a.faultMemory(ctx, q)
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
		system := baseSystem + fmt.Sprintf("\n\n[context: %d%% used]\n", pct)

		window := append([]llm.Message{llm.SystemMessage(system)}, a.working...)

		res, err := a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas})
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
					window = append([]llm.Message{llm.SystemMessage(system)}, a.working...)
					res, err = a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas})
				}
				if err != nil && errors.Is(err, llm.ErrRequestTooLarge) {
					a.compact(ctx, "hard")
					baseSystem = a.buildSystem(pinned, retrieved, procedural, triggered, recalled)
					system = baseSystem + fmt.Sprintf("\n\n[context: %d%% used]\n", a.budgetPct(baseSystem))
					window = append([]llm.Message{llm.SystemMessage(system)}, a.working...)
					res, err = a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas})
				}
			}
			if err != nil {
				return fmt.Errorf("neo: model call failed: %w", err)
			}
		}
		a.working = append(a.working, res.Message)
		if batchTouchesState(res.Message.ToolCalls) {
			stateTouched = true
		}

		// Show SOME of the thinking: surface a trimmed glimpse of this turn's
		// chain-of-thought as a secondary channel so the user sees how Neo is
		// reasoning before it acts. Never the answer, never persisted.
		if think := glimpseReasoning(res.Message.Reasoning); think != "" {
			a.out.Think(think)
		}

		// Positive-proof termination (Cassandra Phase 1). A turn no longer ends
		// on the mere ABSENCE of tool calls (i_cass_1): a bare final message may
		// close only a reversible, non-state-touching turn (the light path). A
		// turn that reached the irreversible seam must finish through the
		// validated task_complete gate handled below.
		if !res.HasToolCalls() {
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
			// State-touching turns may not end with an unaudited bare message:
			// nudge toward the completion gate so completion is proven, not
			// assumed. Reversible chat rides the light path (i_cass_5,
			// placement-by-reversibility) and ends frictionlessly.
			if stateTouched {
				a.working = append(a.working, llm.UserMessage("(you took an action this turn, so don't finish with a plain message — call task_complete with an honest completeness object: a summary for the user, coverage, the real evidence behind your claims, and anything still open.)"))
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
				if batchTouchesState(rest) {
					stateTouched = true
				}
				a.working = append(a.working, llm.ToolResult(cc.ID, tools.TaskCompleteTool, "You called task_complete alongside other tools. Review their results above, then call task_complete on its own once you are actually done."))
				continue
			}
			verdict := a.validateCompletion(ctx, cc, stateTouched, userInput)
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
			a.out.Say("I'm repeating the same step without making progress, so I'm stopping rather than spinning. Here's where I got stuck:\n" + a.lastToolSummary())
			return nil
		}

		a.runToolCalls(ctx, res.Message.ToolCalls)
	}

	// [loop_discipline] step budget exhausted → honest partial, never fabricate.
	a.out.Say("I've reached my step budget for this turn without fully finishing, and I don't want to keep going blindly. Here's where I am:\n" + a.lastToolSummary() + "\n\nTell me how you'd like me to proceed.")
	return nil
}

// Reset clears the live transcript + summary + goal (new conversation).
func (a *Agent) Reset() {
	a.working = nil
	a.summary = ""
	a.activeGoal = ""
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
	}
	return nil, lastErr
}

func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall) {
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
