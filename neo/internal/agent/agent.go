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
	"fmt"
	"sort"
	"strings"
	"time"

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
	Name   string                 // function name dispatched (e.g. "web-search__web_search")
	Args   map[string]interface{} // parsed call arguments
	Result string                 // tool result content (raw text/JSON the tool returned)
	IsErr  bool                   // the tool reported an error result
}

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

	schemas      []llm.Tool
	schemaTokens int

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
}

// New assembles an Agent.
func New(o Options) *Agent {
	out := o.Reporter
	if out == nil {
		out = nopReporter{}
	}
	a := &Agent{
		cfg:          o.Config,
		main:         o.Main,
		cheap:        o.Cheap,
		tools:        o.Tools,
		pager:        o.Pager,
		out:          out,
		consolidator: o.Consolidator,
		recaller:     o.Recaller,
		observer:     o.Observer,
	}
	if a.tools != nil {
		a.schemas = a.tools.Schemas()
	}
	a.schemaTokens = estimateToolTokens(a.schemas)
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
	a.working = append(a.working, llm.UserMessage(userInput))

	// Page-fault relevant memory + proven patterns for this ask (once/turn).
	retrieved := a.faultMemory(ctx, userInput)
	procedural := a.faultPatterns(ctx, userInput)
	// Conversational recall: relevant PAST turns beyond the live transcript —
	// the additive read-lane that keeps an unbounded thread coherent.
	recalled := a.recallTurns(ctx, userInput)

	repeats := 0
	prevSig := ""

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
			recalled = a.recallTurns(ctx, q)
		}

		pinned := ""
		if a.pager != nil {
			pinned = a.pager.Pinned(ctx, a.activeGoal)
		}
		baseSystem := a.buildSystem(pinned, retrieved, procedural, recalled)

		// [control.loop] step_3: forced compaction if over the hard threshold.
		if a.budgetPct(baseSystem) >= a.cfg.HardPct {
			a.compact(ctx, "hard")
			baseSystem = a.buildSystem(pinned, retrieved, procedural, recalled)
		}
		pct := a.budgetPct(baseSystem)
		system := baseSystem + fmt.Sprintf("\n\n[context: %d%% used]\n", pct)

		window := append([]llm.Message{llm.SystemMessage(system)}, a.working...)

		res, err := a.chatWithRetry(ctx, llm.ChatRequest{Messages: window, Tools: a.schemas})
		if err != nil {
			return fmt.Errorf("neo: model call failed: %w", err)
		}
		a.working = append(a.working, res.Message)

		// No tool calls → the model decided it is done. (Termination.)
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
			a.out.Say(answer)
			// [memory.writeback] step_5: hand the completed turn to the
			// background consolidation pass before any compaction nils it.
			if a.consolidator != nil {
				a.consolidator.Consolidate(renderTranscript(a.working))
			}
			// [control.loop] step_6: cooperative compaction at a clean boundary.
			if a.budgetPct(a.buildSystem(pinned, retrieved, procedural, recalled)) >= a.cfg.SoftPct {
				a.compact(ctx, "soft")
			}
			return nil
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
	for _, call := range calls {
		name := call.Function.Name
		args, perr := call.ParseArgs()
		if perr != nil {
			a.working = append(a.working, llm.ToolResult(call.ID, name, fmt.Sprintf("could not parse arguments (%v). Re-issue the call with valid JSON arguments.", perr)))
			continue
		}
		a.out.Status("• " + name)
		content, isErr := a.dispatchWithRetry(ctx, name, args)
		a.working = append(a.working, llm.ToolResult(call.ID, name, content))
		// Surface the work (web-search snippets, fetched pages, …) so the
		// product can show real evidence, not just a synthesized answer.
		if a.observer != nil {
			a.observer(ToolEvent{Name: name, Args: args, Result: content, IsErr: isErr})
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

func estimateMessagesTokens(msgs []llm.Message) int {
	total := 0
	for _, m := range msgs {
		total += memory.EstimateTokens(m.Content) + 4
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
