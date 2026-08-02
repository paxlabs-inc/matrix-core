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

	exectool "matrix/executor/tool"
	"matrix/neo/internal/config"
	"matrix/neo/internal/consolidation"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/o1"
	"matrix/neo/internal/recall"
	"matrix/neo/internal/sessionjournal"
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

// Consolidator is the background write-back hook: it receives a structured
// evidence job and promotes only cited durable learnings to cortex out-of-band.
// Implemented by internal/writeback; optional (nil disables write-back).
type Consolidator interface {
	Consolidate(consolidation.Job)
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
	ID           string                 // tool-call id — stable across the start/end pair so the UI updates one step
	Name         string                 // function name dispatched (e.g. "web-search__web_search")
	Args         map[string]interface{} // parsed call arguments
	Result       string                 // tool result content (raw text/JSON the tool returned); empty at start
	IsErr        bool                   // the tool reported an error result
	FailureClass string                 // normalized failure layer; empty on success/start
	Phase        ToolPhase              // start (dispatched, no result yet) or end (completed)

	// ScreenshotURL is the out-of-band /media URL of a page still captured for
	// this call — either the image a browser_take_screenshot returned, or the
	// deterministic auto-capture fired after a view-changing browser action
	// (BROWSER-FILMSTRIP). It NEVER enters the model transcript (that stays a
	// terse placeholder); it rides only this observer event so the surface can
	// render the browsing filmstrip. Empty for non-browser / non-captured calls.
	ScreenshotURL string

	// Live file-typing channel (NEO-WORKBENCH, Phase==ToolStream only): while
	// the model is still GENERATING a write_file call, decoded fragments of the
	// file content stream out so an open editor renders Neo typing. StreamPath
	// is the target file, StreamDelta the next decoded content fragment, and
	// StreamOffset the number of decoded bytes that preceded it (gap/order
	// detection). Best-effort and bounded: a dropped or capped stream degrades
	// to the final ToolEnd state, never a corrupted buffer.
	StreamPath   string
	StreamDelta  string
	StreamOffset int
}

// ToolPhase distinguishes the two observer callbacks for one tool call: a
// start (so the surface can paint a live "running" viewport immediately) and
// an end (the result lands). The harness correlates them by ToolEvent.ID.
type ToolPhase string

const (
	ToolStart ToolPhase = "start"
	ToolEnd   ToolPhase = "end"
	// ToolStream is the mid-flight live-typing fragment for a file write the
	// model is still generating (StreamPath/StreamDelta/StreamOffset). It fires
	// BEFORE the call's ToolStart (the model hasn't finished the call yet) and
	// never carries Args/Result.
	ToolStream ToolPhase = "stream"
)

// ToolObserver receives every tool result as it happens. Optional; nil
// disables surfacing. The harness (CLI or SSE server) decides how to render —
// the agent loop stays oblivious to the presentation layer.
type ToolObserver func(ToolEvent)

// Agent wires the model, tools, and memory into one conversational loop.
// Every field carries a documented lifetime (MORPHEUS req.2.2): construction
// (set at New or by the harness immediately after, then stable), session
// (evolves across turns within one agent), or the turn-scoped handle (the
// reified turn, replaced at Chat entry).
type Agent struct {
	// Lifetime: construction — wiring set at New.
	cfg                config.Config
	main               *llm.Client
	cheap              *llm.Client
	tools              *tools.Manager
	pager              *memory.Pager
	runtime            *ResurrectionRuntime
	runtimeMode        string
	runtimeErr         error
	runtimeLast        string
	runtimeFailure     delegate.FailureClass
	out                Reporter
	consolidator       Consolidator
	recaller           ConvRecaller
	observer           ToolObserver
	journal            *sessionjournal.Store
	restoredReplay     sessionjournal.Replay
	semanticCheckpoint sessionjournal.SemanticCheckpoint

	// auditObserver streams Cassandra 2.0 controller audit events (cassandra.mod)
	// to the harness on a pure observability side-channel; nil discards them.
	// Lifetime: construction.
	auditObserver AuditObserver

	// captureFn overrides the browser viewport auto-capture (default: the tool
	// manager's real CaptureViewport, which fires a screenshot on the live MCP
	// browser session). Injected only by tests that exercise the gating/cap/
	// attachment logic without a live browser; nil in production.
	// Lifetime: construction (test seam).
	captureFn func(ctx context.Context, sourceFunc string) string

	// memObserver receives this turn's continuous-memory activation summary
	// (durable story-so-far + coarse timeline) so the harness can surface the
	// memory Neo carries to the user (continuous-memory task 7.1). nil disables
	// surfacing; the agent loop stays oblivious to the presentation layer
	// (mirrors auditObserver). Only fires under the continuous-memory collapse.
	// Lifetime: construction.
	memObserver      MemoryObserver
	episodicSurfaced map[string]struct{}

	// allSchemas is the live bound surface fixed at construction. schemas is
	// the O1 contract-selected subset for the current turn.
	allSchemas   []llm.Tool
	schemas      []llm.Tool
	schemaTokens int
	schemaBytes  int

	// working is the live transcript (user / assistant / tool messages). The
	// system block (identity + rules + retrieved memory + budget stat) is
	// re-derived every turn and never stored here, so it can't drift.
	// Lifetime: session (append-only across turns; Reset/Seed manage it).
	working []llm.Message
	// activeGoal is THIS conversation's task, pinned every turn. Held on the
	// agent (not the pager) so many conversations can share one cortex store
	// without clobbering each other's goal. Lifetime: session.
	activeGoal     string
	intentMu       sync.RWMutex
	persistentGoal *PersistentGoal
	openLoops      map[string]OpenLoop
	intentSequence uint64

	// persona, when set, frames this agent as a task-scoped SUB-AGENT with a
	// specific role. Sub-agents run headless (no human in the loop): they get
	// a restricted tool surface, never ask the user questions, and end by
	// reporting their findings back to the orchestrating agent.
	// Lifetime: construction.
	persona string

	// capability is the resolved capability-surface material rendered resident
	// in the byte-stable prefix (epistemic-core req.2): construction-time
	// state, so the rendered section is byte-identical across a turn's steps.
	// Lifetime: construction.
	capability *CapabilitySurface

	// selfModel is the shared self-model this sub-agent inherited at spawn (the
	// structural self-summary + relevant how-I-fail patterns), injected into its
	// charter so it reasons as the same mind on a scoped slice (self-model task
	// 4.3). Empty on the top-level agent. Lifetime: construction.
	selfModel string

	// turn is the turn-scoped handle to the reified run state (MORPHEUS
	// req.2): everything with turn lifetime — the epistemic mechanisms' state,
	// the Cassandra controller's per-turn state, the overflow latch, the
	// autoshot/unproductive counters, the stall bookkeeping, the surfaced-
	// memory sets, the failure-class scratch, and the loop-death capture —
	// lives on it. Replaced with a fresh turn at Chat entry — constructing the
	// turn IS the per-turn reset (zero manual field zeroing). Non-nil from New
	// on; retained after Chat returns so the supervisor's cross-turn reads
	// (LastFailureClass, LastDeath) reach this turn's records until the next
	// turn begins. Lifetime: turn-scoped handle.
	turn *turn

	// activationAssemblies counts how many times the per-turn activation
	// bundle was assembled this session — the NE-7 observable: exactly once
	// per turn, never per step. Lifetime: session (accumulates across turns).
	activationAssemblies int

	// windowAssemblies counts how many times the window was assembled this
	// session — the MORPHEUS req.3.2 observable: exactly once per step on a
	// healthy turn (prepareWindow is the ONE assembly site; the 413 recovery
	// re-enters it, adding one per retry). Lifetime: session.
	windowAssemblies int

	// convID scopes the per-turn attestation IntentID for audit; empty on the
	// CLI path (falls back to "cli"). turnSeq counts user turns this session so
	// each attest has a distinct, stable IntentID. Lifetime: convID
	// construction, turnSeq session.
	convID  string
	turnSeq int

	// logicalIntentID is the server-owned task identity. It stays constant
	// across supervised respawns, unlike turnSeq on a freshly rebuilt Agent.
	logicalIntentID   string
	supervisedAttempt int

	// generation is the per-session command-queue tag for the current turn
	// (P2-6 lane-based concurrency). Set by the session before each Chat call
	// so superseded (late) results can be detected and discarded — a result
	// whose generation is no longer current is stale. Zero when unset (CLI
	// path, tests). Not goroutine-safe: accessed only from the Chat goroutine.
	// Lifetime: session.
	generation uint64

	// skillIndex is the names-only skill list injected into the STABLE system
	// prefix (P2-2). Token-bounded: only names, never bodies. Set by the
	// session/harness from the consolidator's ProposedSkills(). Lives in the
	// cacheable prefix so it stays byte-identical across turns (consistent
	// with P1-2). Empty = no skill section emitted. Lifetime: session.
	skillIndex []string

	// inbox, when set, returns user messages queued (by the session) WHILE
	// this turn is in flight — mid-task messages the user sent without
	// interrupting (F5). The loop drains it at each tool-call boundary and
	// appends them to the transcript, so the agent picks them up on its next
	// step instead of the message cancelling the run. nil on the CLI path.
	// Lifetime: construction (explicitly cross-turn seam).
	inbox func() []string

	// automatrix marks an autonomous Automatrix run (Options.Automatrix): the
	// user is away, the run must be self-contained, and no screen surface or
	// question back to the user is appropriate. Lifetime: construction.
	automatrix bool
	// interview marks a personalization-interview conversation (ORACLE task
	// 5.3): the prompt gains the guided-interview charter and the agent
	// advertises save_personalization_profile. Lifetime: construction.
	interview bool
	// interviewExisting is the rendered existing-answers block a REPEAT
	// interview re-enters with (req 12.1); empty on a first interview.
	// Lifetime: construction.
	interviewExisting string
	// advertised, when non-nil (restricted agents), is the exact advertised
	// function-name set; dispatch rejects any name outside it so synthetic
	// tools the Manager would otherwise serve stay structurally unreachable.
	// Lifetime: construction.
	advertised map[string]struct{}

	// userProfile carries the onboarding profile (preferred_name,
	// expertise_domains) fetched from the daemon's /profile endpoint. It is
	// per-user-stable (set once at onboarding, rarely changed) so it lives
	// in the STABLE system prefix (prompt-cache byte-stability invariant).
	// agent_name flows through cfg.AgentName separately. Empty = no
	// user-specific identity section (clean fallback to default "Neo").
	// Lifetime: session (set per session rebuild via SetUserProfile).
	preferredName    string
	expertiseDomains []string

	// Coding workspace context (NEO-WORKBENCH): the daemon's workspace root
	// and this conversation's active project, injected per session rebuild
	// like the user profile. Per-conversation-stable (a conversation's project
	// tag is fixed), so it lives in the STABLE system prefix. Empty wsRoot =
	// no workbench on this daemon, no coding-workspace section.
	// Lifetime: session (set per session rebuild via SetWorkspace).
	wsRoot        string
	wsProjectID   string
	wsProjectName string
	wsProjectRoot string
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
	Runtime      *ResurrectionRuntime
	Journal      *sessionjournal.Store

	// AuditObserver streams Cassandra 2.0 controller audit events (cassandra.mod)
	// to the harness; nil discards them.
	AuditObserver AuditObserver

	// MemoryObserver receives this turn's continuous-memory activation summary
	// (durable story-so-far + coarse timeline) so the harness can surface the
	// memory Neo carries to the user (continuous-memory task 7.1). nil discards.
	MemoryObserver MemoryObserver

	// Persona frames this as a task-scoped sub-agent with a specific role
	// (empty = the top-level conversational agent).
	Persona string
	// Capability is the resolved capability-surface material (epistemic-core
	// req.2): the agent's true external API surface, is/is-not architectural
	// facts, and failure patterns, resolved from the durable self-model. It
	// renders resident in the byte-stable prefix; nil renders honest UNKNOWN
	// gaps (never fabricated facts).
	Capability *CapabilitySurface
	// SelfModel is the shared self-model a sub-agent INHERITS from the agent that
	// spawned it (self-model task 4.3, req.9.1): a compact rendering of the
	// structural self-summary plus the relevant how-I-fail patterns, injected into
	// the sub-agent's charter so it acts as the SAME mind (Neo) on a scoped slice
	// — knowing how it is built and how it tends to fail — rather than a blank
	// helper. Empty on the top-level agent (which carries its self-model through
	// its own memory surface) and on any sub-agent spawned before the self-model
	// is populated.
	SelfModel string
	// ConvID is the conversation this agent serves; stamped on the per-turn
	// attestation IntentID for audit. Empty on the CLI path.
	ConvID string
	// RestrictTools advertises the SUB-AGENT tool surface (full Natural set,
	// minus core_execute / memory_recall / spawn_subagents) instead of the
	// full one. Set for sub-agents so money stays with the parent and a
	// sub-agent can't spawn its own sub-agents.
	RestrictTools bool
	// Automatrix marks an autonomous Automatrix run: the user is AWAY and did
	// not initiate this turn. The system prompt gains an autonomous-mode
	// section (work self-contained, no screen surfaces, no questions back) and
	// tool calls are held to the advertised restricted surface. Set by
	// NewAutomatrix, never directly.
	Automatrix bool
	// Brief marks an autonomous MORNING_BRIEF run (ORACLE req 15): like
	// Automatrix the user is AWAY and did not initiate this turn, but the tool
	// surface is TIGHTER — a POSITIVE allowlist of read-only information tools
	// (web_news/web_search/fetch) plus memory_recall only (Manager.BriefSchemas).
	// Financial, signing, filesystem, deployment, and arbitrary-execution tools
	// (including core_execute) are structurally absent. Set by NewMorningBrief,
	// never directly.
	Brief bool
	// Interview marks a personalization-interview conversation (ORACLE task
	// 5.3, req 12): the system prompt gains the guided-interview charter (five
	// skippable question groups, one at a time, plain-language summary +
	// explicit confirmation before saving, no sensitive traits) and the agent
	// additionally advertises save_personalization_profile — the ONLY surface
	// that tool is advertised on. Set by NewInterview, never directly.
	Interview bool
	// InterviewExisting is the rendered existing-answers block a REPEAT
	// interview re-enters with (req 12.1); empty on a first interview.
	InterviewExisting string

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
		cfg:              o.Config,
		main:             o.Main,
		cheap:            o.Cheap,
		tools:            o.Tools,
		pager:            o.Pager,
		runtime:          o.Runtime,
		runtimeMode:      strings.TrimSpace(o.Config.AgentRuntime),
		out:              out,
		consolidator:     o.Consolidator,
		recaller:         o.Recaller,
		observer:         o.Observer,
		journal:          o.Journal,
		auditObserver:    o.AuditObserver,
		memObserver:      o.MemoryObserver,
		episodicSurfaced: map[string]struct{}{},
		persona:          strings.TrimSpace(o.Persona),
		capability:       o.Capability,
		selfModel:        strings.TrimSpace(o.SelfModel),
		convID:           strings.TrimSpace(o.ConvID),
		inbox:            o.Inbox,
		automatrix:       o.Automatrix,
		interview:        o.Interview,
		turn:             newTurn(),
	}
	if a.runtimeMode == "" {
		a.runtimeMode = "legacy"
	}
	if a.runtimeMode == "resurrection" && a.runtime == nil {
		a.runtimeErr = fmt.Errorf(
			"neo: resurrection runtime is selected but unavailable",
		)
	} else if a.runtimeMode != "legacy" &&
		a.runtimeMode != "resurrection" {
		a.runtimeErr = fmt.Errorf(
			"neo: unsupported runtime %q", a.runtimeMode,
		)
	}
	if a.interview {
		a.interviewExisting = strings.TrimSpace(o.InterviewExisting)
	}
	if a.tools != nil {
		switch {
		case o.Brief:
			// Positive allowlist: read-only information tools + memory_recall
			// only (ORACLE req 15.1). Tighter than the sub-agent surface.
			a.schemas = a.tools.BriefSchemas()
		case o.RestrictTools:
			a.schemas = a.tools.SubagentSchemas()
		default:
			a.schemas = a.tools.Schemas()
		}
	}
	// An interview agent additionally advertises the confirmation-gated profile
	// save tool — the ONLY surface it is advertised on (ORACLE req 13.3).
	if o.Interview {
		a.schemas = append(a.schemas, tools.PersonalizationSchema())
	}
	// read_overflow is always advertised (top-level and sub-agent): any tool
	// result can overflow the inline budget, and the read-full discipline
	// (neo-smoothness req.4) needs the model able to page the remainder back in.
	// It is synthetic (intercepted in the loop, not routed through the Manager),
	// so it never enters the manifest tool-bijection check.
	a.schemas = append(a.schemas, readOverflowSchema())
	// Epistemic-core Mechanism 3, universal: every advertised tool schema
	// carries the `expect` parameter so the prediction discipline is enforced
	// by the schema the model reads, not just charter prose. Copy-on-write —
	// the Manager's shared parameter maps are never mutated.
	if a.cfg.EpistemicPredictions {
		a.schemas = injectExpectParam(a.schemas)
	}
	a.allSchemas = append([]llm.Tool(nil), a.schemas...)
	// A restricted agent is held to its ADVERTISED surface at dispatch time
	// too: the Manager's dispatch switch handles synthetic tools (e.g.
	// construct_render) whether or not they were advertised, so a model that
	// guesses an unadvertised name would otherwise reach past the restriction.
	a.advertised = make(map[string]struct{}, len(a.schemas))
	for _, s := range a.schemas {
		a.advertised[s.Function.Name] = struct{}{}
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
func (a *Agent) drainInbox(ctx context.Context) (bool, error) {
	if a.inbox == nil {
		return false, nil
	}
	injected := false
	for _, m := range a.inbox() {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		if err := a.journalUser(ctx, llm.UserMessage(m)); err != nil {
			return injected, err
		}
		if a.cfg.SessionCurrentIntent {
			a.setTurnObjective(m)
		}
		if err := a.promoteForIntent(m); err != nil {
			return injected, err
		}
		a.working = append(a.working, llm.UserMessage(m))
		if a.executionPosture() {
			a.turn.contract = a.turn.contract.Revise(m)
		}
		injected = true
	}
	return injected, nil
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

// SetWorkspace injects the coding-workspace context (workspace root + the
// conversation's active project) into the STABLE system prefix. Called after
// agent construction (per session rebuild), mirroring SetUserProfile. The
// values are per-conversation-stable (the project tag is fixed for a
// conversation), preserving the prompt-cache byte-stability invariant. An
// empty root disables the section (no workbench on this daemon).
func (a *Agent) SetWorkspace(root, projectID, projectName, projectRoot string) {
	a.wsRoot = strings.TrimSpace(root)
	a.wsProjectID = strings.TrimSpace(projectID)
	a.wsProjectName = strings.TrimSpace(projectName)
	a.wsProjectRoot = strings.TrimSpace(projectRoot)
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
	if a.cfg.InteractionPosture && a.turn != nil {
		switch a.turn.posture {
		case PostureConversation:
			return 1
		case PostureExploration:
			if a.cfg.StepBudget > 0 && a.cfg.StepBudget < 8 {
				return a.cfg.StepBudget
			}
			return 8
		}
	}
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

// Chat runs one user turn through the staged loop (MORPHEUS req.3) until the
// model yields a final answer (no tool calls), the loop stalls/exhausts its
// budget, or it is blocked needing the human. Chat itself is the skeleton: it
// constructs the reified turn, runs the per-turn prepare, then iterates steps
// — prepare → generate → deliberate → act | close — each a named stage with a
// documented contract. Conversation state persists across calls.
func (a *Agent) Chat(ctx context.Context, userInput string) error {
	if a.runtimeMode == "resurrection" || a.runtimeErr != nil {
		return a.chatResurrection(ctx, userInput)
	}
	return a.chat(ctx, userInput, nil, "")
}

// ChatAudio runs the same staged turn with one audio-native user message. The
// ASR result is produced concurrently with the first model generation; before
// any downstream deliberation/close, the visible user content is replaced by
// the durable transcript plus sealed media ref and recorded to cortex.
func (a *Agent) ChatAudio(ctx context.Context, userInput string, audio *AudioTurn) error {
	if a.runtimeMode == "resurrection" || a.runtimeErr != nil {
		return a.chatAudioResurrection(ctx, userInput, audio)
	}
	return a.chat(ctx, userInput, audio, "")
}

// ChatResume enters supervisor guidance into the cognitive window without
// recording it as a user message or using it as the activation query.
func (a *Agent) ChatResume(ctx context.Context, objective, guidance string) error {
	goalID := a.RegisterPersistentGoal(objective)
	var err error
	if a.runtimeMode == "resurrection" || a.runtimeErr != nil {
		err = a.chatResurrectionResume(ctx, objective, guidance)
	} else {
		err = a.chat(ctx, objective, nil, guidance)
	}
	if err == nil && goalID != "" {
		a.CompletePersistentGoal(goalID)
	}
	return err
}

// SetRunIdentity binds this Agent attempt to the stable server run identity.
// A session rebuild must call it before Chat or ChatResume.
func (a *Agent) SetRunIdentity(intentID string, attempt int) {
	a.logicalIntentID = strings.TrimSpace(intentID)
	a.supervisedAttempt = attempt
}

func (a *Agent) chat(ctx context.Context, userInput string, audio *AudioTurn, resumeGuidance string) error {
	userInput = strings.TrimSpace(userInput)
	if userInput == "" {
		return nil
	}
	if !a.cfg.SessionCurrentIntent && a.activeGoal == "" {
		a.activeGoal = userInput
	}
	a.turnSeq++
	// The reified turn (MORPHEUS req.2): constructing the fresh turn IS the
	// reset of everything with turn lifetime — the epistemic run state, the
	// Cassandra controller state, the overflow latch, the autoshot/
	// unproductive counters, the stall bookkeeping, the surfaced sets, the
	// failure-class scratch, and the death capture all scope to it by
	// construction. Zero manual field zeroing.
	t := newTurn()
	t.objective = userInput
	t.turnObjective = userInput
	t.posture = ClassifyInteractionPosture(a.cfg, userInput)
	if a.cfg.SessionCurrentIntent {
		a.activeGoal = userInput
	}
	t.attempt = a.supervisedAttempt
	if strings.TrimSpace(resumeGuidance) != "" {
		t.inputOrigin = originSupervisorResume
	}
	a.turn = t
	// Run-scoped overflow files (neo-smoothness req.4.3) are ephemeral to this
	// turn: clean them up on every exit (completion, stall, budget, error).
	defer func() {
		if t.overflow != nil {
			t.overflow.cleanup()
		}
	}()
	audioIndex := -1
	if audio != nil {
		audioIndex = len(a.working)
	}
	cmTail, err := a.prepareTurn(ctx, userInput, audioData(audio), resumeGuidance)
	if err != nil {
		return err
	}
	audioFinalized := audio == nil
	finalizeAudio := func() {
		if audioFinalized {
			return
		}
		userInput = a.finalizeAudioTurn(ctx, audioIndex, audio)
		audioFinalized = true
	}
	defer finalizeAudio()

	for step := 0; ; step++ {
		budget := a.effectiveStepBudget(effectiveBudgetSignals{
			step:          step,
			distinctTools: len(t.distinctToolSet),
			stallRepeats:  t.repeats,
		})
		if step >= budget {
			break
		}
		// Surface this step's position in the budget to the model (via budgetTail)
		// so it starts writing its final answer BEFORE the step cliff, not after —
		// the synthesis-survives-death discipline at the tail.
		t.curStep = step
		t.stepBudget = budget
		// F5: fold in any messages the user queued mid-task (sent without
		// interrupting) so the agent picks them up on THIS step — delivered at
		// the tool-call boundary, never cancelling the in-flight run.
		if _, err := a.drainInbox(ctx); err != nil {
			a.consolidateWorking("", false)
			return fmt.Errorf("neo: persist queued user message: %w", err)
		}

		window, tail, pct, windowErr := a.prepareWindow(cmTail)
		if windowErr != nil {
			return windowErr
		}
		res, streamedReasoning, proceed, err := a.generate(ctx, step, cmTail, window, tail)
		if err != nil {
			finalizeAudio()
			a.consolidateWorking("", false)
			return fmt.Errorf("neo: model call failed: %w", err)
		}
		if !proceed {
			continue
		}
		finalizeAudio()
		if err := a.journalAssistant(ctx, res.Message); err != nil {
			a.consolidateWorking("", false)
			return err
		}
		casMod := a.deliberate(step, res, streamedReasoning)
		if !res.HasToolCalls() {
			finished, cerr := a.closeTurn(ctx, res, casMod, userInput)
			if cerr != nil {
				return cerr
			}
			if finished {
				return nil
			}
			continue
		}
		if aerr := a.act(ctx, step, pct, res); aerr != nil {
			return aerr
		}
	}

	// [loop_discipline] step budget exhausted → NOT done. Never fabricate a
	// close: the governor's terminal verdict returns an incomplete signal so
	// the supervisor keeps going with a fresh window rather than stopping
	// with a partial.
	return a.governDeath(DeathReasonStepBudget, "reached the step budget without finishing. Progress so far:")
}

// prepareTurn is the turn half of the prepare stage (MORPHEUS req.3.1), run
// ONCE at Chat entry. Contract — inputs: the trimmed user input; mutates: the
// working transcript (appends the user message), the durable cortex transcript
// (records it), the turn's surfaced-memory sets, and activationAssemblies;
// returns: the rendered activation tail (cmTail) frozen for the whole turn.
//
// The single memory path (MORPHEUS req.1): the per-turn working set comes
// from cortex.Activate, computed ONCE per turn and rendered as the trailing
// USER-role tail (the activation snapshot is frozen for the whole turn — the
// NE-7 discipline). The Q2 first-turn relevance push rides the same tail so
// the usage-salience attestation loop still learns from what surfaced.
func (a *Agent) prepareTurn(ctx context.Context, userInput, audioData, resumeGuidance string) (string, error) {
	if err := a.preparePosture(); err != nil {
		return "", err
	}
	if guidance := strings.TrimSpace(resumeGuidance); guidance != "" {
		if err := a.journalRecovery(ctx, "resuming", guidance); err != nil {
			return "", err
		}
		a.pushGuidance(guidance)
	} else {
		var userMessage llm.Message
		if audioData != "" {
			userMessage = llm.UserAudioMessage(userInput, audioData)
		} else {
			userMessage = llm.UserMessage(userInput)
		}
		if err := a.journalUser(ctx, userMessage); err != nil {
			return "", err
		}
		a.working = append(a.working, userMessage)
		// Record only genuine user input. Supervisor resume guidance is
		// cognitive state and must never become a durable user assertion.
		if audioData == "" {
			a.cmRecordUser(userInput)
		}
	}

	bundle := a.cmActivate(userInput)
	a.activationAssemblies++
	cmTail := a.renderActivationBundle(bundle)
	if a.executionPosture() {
		cmTail += a.turn.contract.Render()
	}
	// Q2 first-message relevance push: Activate's tiers are recency-based
	// + query-independent, so on the OPENING turn also inject a bounded
	// relevance retrieval keyed on the message — the agent gets
	// relevance-matched memory without a reactive memory_recall call.
	var retrieved []memory.Snippet
	if pushSnips, pushBlock := a.cmRelevancePush(ctx, userInput); pushBlock != "" {
		cmTail += pushBlock
		retrieved = pushSnips
	}
	triggerClass := ""
	var episodic []memory.EpisodicExcerpt
	if trigger := classifyEpisodicUserMessage(userInput); trigger.Fired {
		started := time.Now()
		a.turn.episodicMark(trigger.Class)
		triggerClass = trigger.Class
		extracted := a.extractEpisodic(ctx, userInput)
		var current []memory.EpisodicCurrentHit
		if a.recaller != nil {
			for _, hit := range a.recaller.Relevant(ctx, extracted.Referent) {
				current = append(current, memory.EpisodicCurrentHit{Role: hit.Role, Text: hit.Text})
			}
		}
		if a.pager != nil {
			episodic = a.pager.EpisodicRetrieve(ctx, extracted.Referent, memory.EpisodicTimeWindow{From: extracted.Window.From, Until: extracted.Window.Until}, memory.EpisodicBudget{ExcludeConversation: a.cmConvID()}, current)
		}
		episodic = a.filterNewEpisodic(episodic)
		if len(episodic) > 0 {
			cmTail += renderEpisodicBlock(episodic)
			a.turn.episodicGrounded()
			for _, ex := range episodic {
				collectSurfaced(a.turn.surfaced, ex.RelatedMemories, nil)
				collectSurfacedSnips(a.turn.surfacedSnips, ex.RelatedMemories)
			}
		}
		injectedTokens := 0
		for _, ex := range episodic {
			injectedTokens += (len(ex.Text) + 3) / 4
		}
		logEpisodic(trigger.Class, len(episodic), injectedTokens, started)
	}
	a.emitMemory(bundle, triggerClass, episodic)
	// Track every cortex memory surfaced this turn so a successful completion
	// can attest them as USED — the usage-salience + EMA learning signal that
	// keeps Neo's durable store ranking by what actually helps. The snippets
	// keep text/type too, so completion can also send the NEGATIVE signal for
	// memories that were surfaced but demonstrably ignored.
	collectSurfaced(a.turn.surfaced, retrieved, nil)
	collectSurfacedSnips(a.turn.surfacedSnips, retrieved)
	return cmTail, nil
}

// prepareWindow is the step half of the prepare stage and the ONE window-
// assembly site per step (MORPHEUS req.3.2) — the oversize-recovery path
// re-enters it rather than assembling its own window. Contract — inputs: the
// turn-frozen activation tail; mutates: the working transcript ONLY via the
// trim/strip recovery helpers (cmTrimWorking, stripOldImages) when over
// budget; returns: the assembled window, the full trailing tail, and the
// context-fill percentage for the budget stat.
//
// The single window law (MORPHEUS req.1.3): the byte-stable charter prefix at
// index 0 + the append-only live transcript + the rendered Activate bundle as
// ONE trailing USER-role message (Qwen-template portability). Over-budget
// trimming is NON-summarizing (cmTrimWorking) because the older turns are
// durable in cortex and the coarse history rides the durable story-so-far
// already surfaced in cmTail. The request-body byte cap is independent of the
// token budget: the token estimate undercounts the serialized JSON, so a
// window within token budget can still 413 — images strip first (cheaper,
// less lossy), then the trim.
func (a *Agent) prepareWindow(cmTail string) (window []llm.Message, tail string, pct int, err error) {
	a.repairWorkingProtocol()
	if err := o1.ValidateConversationTruth(a.working); err != nil {
		return nil, "", 0, fmt.Errorf("o1 context truth: %w", err)
	}
	checkpointTail := ""
	if a.cfg.SessionExactProjection {
		checkpointTail = a.semanticCheckpoint.Render()
	}
	baseSystem := a.stableSystem() + cmTail + checkpointTail
	if a.overHardBudget(baseSystem) {
		a.cmTrimWorking()
		checkpointTail = a.semanticCheckpoint.Render()
		baseSystem = a.stableSystem() + cmTail + checkpointTail
	}
	if a.windowBytes(baseSystem) >= maxRequestBodyBytes {
		a.stripOldImages()
		if a.windowBytes(baseSystem) >= maxRequestBodyBytes {
			a.cmTrimWorking()
			checkpointTail = a.semanticCheckpoint.Render()
			baseSystem = a.stableSystem() + cmTail + checkpointTail
		}
	}
	pct = a.budgetPct(baseSystem)
	tail = cmTail + checkpointTail + a.epistemicTail() + a.budgetTail(pct)
	if a.cfg.SessionExactProjection {
		if !a.turn.projectionFrozen {
			a.turn.projection = tail
			a.turn.projectionFrozen = true
		}
		tail = a.turn.projection
		window = assembleWindowContextSidecar(a.stableSystem(), a.working, tail)
	} else {
		window = assembleWindowUserTail(a.stableSystem(), a.working, tail)
	}
	a.windowAssemblies++
	return window, tail, pct, nil
}

// generate is the generate stage (MORPHEUS req.3.1): one model call with live
// streaming, the forced-revision variant, and the 413 oversize recovery.
// Contract — inputs: the step index, the turn-frozen activation tail, and this
// step's prepared window+tail; mutates: the working transcript only on a
// forced revision (via forcedRevisionStep) or a cut-off tool-call batch (the
// compact-retry nudge), plus trim/strip during 413 recovery; returns: the
// model result, whether reasoning streamed live, and proceed=false when the
// loop must re-enter its next iteration without a result (forced revision ran,
// or the cut-off batch was dropped). err is a terminal model failure.
func (a *Agent) generate(ctx context.Context, step int, cmTail string, window []llm.Message, tail string) (res *llm.ChatResult, streamedReasoning, proceed bool, err error) {
	// Live "typing" channel: stream the model's incremental fragments as
	// they generate so the user sees Neo thinking + answering in real time
	// instead of staring at a blank surface until the whole turn lands.
	// reasoning → the live thinking channel; content → the answer being
	// typed. step segments the stream so the client resets per turn.
	// Live file-typing channel (NEO-WORKBENCH): tool-call argument fragments
	// stream through the typer, which decodes write_file path/content
	// incrementally and emits bounded ToolStream observer events. Fresh per
	// model call — each turn's calls index from 0.
	typer := newLiveTyper(func(ev ToolEvent) {
		if a.observer != nil {
			a.observer(ev)
		}
	})
	onDelta := func(d llm.Delta) {
		if d.Tool != nil {
			typer.feed(d.Tool)
		}
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

	// Governor fire position 1 — the epistemic layer (req.5.2, epistemic-core
	// req.5.2/6.3/7.2): a pending forced revision runs as a tools-stripped
	// reasoning-only step BEFORE any further dispatch — the model must revise
	// the plan in text first.
	if _, due := a.governEpistemic(); due {
		if rerr := a.forcedRevisionStep(ctx, window, onDelta); rerr != nil {
			return nil, streamedReasoning, false, rerr
		}
		return nil, streamedReasoning, false, nil
	}

	schemas := a.schemas
	if guidance := strings.TrimSpace(a.turn.localFinalizePending); guidance != "" {
		a.turn.localFinalizePending = ""
		window = append(window, a.pushGuidance(guidance))
		schemas = nil
	}
	res, err = a.chatWithRetry(ctx, a.providerRequest(ctx, window, schemas, onDelta, 0))
	if err != nil {
		// HTTP 413 (provider request-body byte cap) is recoverable: the
		// window serialized past the byte limit even though it was within
		// the token budget. Recover in two escalating steps (Phase 4.2),
		// each re-entering prepareWindow — the ONE assembly site (req.3.2):
		// first strip dead-weight inline images from older turns and retry —
		// far less lossy than discarding context. Only if that still 413s do
		// we fall back to the non-summarizing trim (the older turns are
		// durable in cortex) and retry once more.
		if errors.Is(err, llm.ErrRequestTooLarge) {
			if a.stripOldImages() > 0 {
				var prepareErr error
				window, _, _, prepareErr = a.prepareWindow(cmTail)
				if prepareErr != nil {
					return nil, streamedReasoning, false, prepareErr
				}
				res, err = a.chatWithRetry(ctx, a.providerRequest(ctx, window, a.schemas, onDelta, a.turn.answerTokenBudget))
			}
			if err != nil && errors.Is(err, llm.ErrRequestTooLarge) {
				a.cmTrimWorking()
				var prepareErr error
				window, _, _, prepareErr = a.prepareWindow(cmTail)
				if prepareErr != nil {
					return nil, streamedReasoning, false, prepareErr
				}
				res, err = a.chatWithRetry(ctx, a.providerRequest(ctx, window, a.schemas, onDelta, a.turn.answerTokenBudget))
			}
		}
		if err != nil {
			return nil, streamedReasoning, false, err
		}
	}
	// A truncated generation (finish_reason=length) that ALSO carries tool
	// calls is a half-formed call: the model almost certainly inlined a
	// large payload as an argument and got cut off mid-JSON. Persisting it
	// would poison the transcript — it is re-sent verbatim every turn and a
	// strict provider 400s the malformed function, wedging the whole
	// conversation. Drop the cut-off turn and nudge for a compact retry.
	if res.FinishReason == "length" && res.HasToolCalls() {
		// Raise the output-token budget one rung so the retried call has room to
		// finish (Claude Code's 8K→64K escalation), then drop the cut-off batch.
		a.bumpAnswerBudget()
		nudge := "(your last tool call was cut off by the output limit before its arguments finished — don't inline large content as a tool argument. Write large files in chunks/appends, or call the tool with compact arguments.)"
		if a.cfg.SessionExactProjection {
			a.pushGuidance(nudge)
		} else {
			a.working = append(a.working, llm.UserMessage(nudge))
		}
		return nil, streamedReasoning, false, nil
	}
	return res, streamedReasoning, true, nil
}

// deliberate is the deliberate stage (MORPHEUS req.3.1): the assistant turn
// is committed and the Cassandra controller reads the behavioral signals.
// Contract — inputs: the step index, the model result, and whether reasoning
// already streamed; mutates: the working transcript (appends the assistant
// message; Cassandra may edit that message's Content in place) and the
// Cassandra per-turn state; returns: whether the controller modified the
// window this step (the close self-heal read).
func (a *Agent) deliberate(step int, res *llm.ChatResult, streamedReasoning bool) (casMod bool) {
	t := a.turn
	a.working = append(a.working, res.Message)
	// Continuous-memory (task 6.1): record a TOOL-CALLING assistant turn
	// (content + calls) to the durable cortex transcript. No-op when off. The
	// ORIGINAL content is recorded here BEFORE the Cassandra controller may
	// edit the in-window copy, so cortex stays ground truth (req.7.1). A BARE
	// answer is NOT recorded here: its fate is decided by the close chain, and
	// recording it before the verdict wrote guard-rejected, never-delivered
	// answers into durable memory as if the user had seen them — the model
	// then believed it had already answered and dismissed every steer (the
	// 2026-07-22 loopty-loop incident). Bare answers are recorded at the
	// delivery choke point in closeTurn instead.
	if res.HasToolCalls() {
		a.cmRecordAssistant(res.Message)
	}

	// The unified per-step signal state (MORPHEUS req.5.1): computed ONCE
	// here — the one behavioral read every self-correction consumer shares.
	// The stall bookkeeping still reflects the previous committed batch
	// (noteBatch commits in act), so the repeat fields carry the same
	// one-step-early verdict the controller has always read.
	t.signals = a.computeStepSignals(step, res.Message)

	// Governor fire position 2 — Cassandra 2.0 (the silent-voice controller):
	// immediately after the assistant turn is appended and BEFORE the next
	// model call, the controller may edit that message's OWN Content in place
	// — folding in doubt / assurance so the model re-reads it as its own
	// emerging thought next turn. It is a no-op when disabled, before
	// min_step, when the per-turn/step budget is spent, or when no behavioral
	// trigger fires (the healthy case). The signals are the unified state's
	// controller projection. casMod records whether a mod fired THIS step so
	// a would-be close can re-loop (self-heal).
	casMod = a.governVoice(t.signals)

	// Show SOME of the thinking: surface a trimmed glimpse of this turn's
	// chain-of-thought as a secondary channel so the user sees how Neo is
	// reasoning before it acts. Never the answer, never persisted. Skip it
	// when the reasoning already streamed live — the surface holds the full
	// thinking and a post-hoc glimpse would only truncate it. This fallback
	// covers models that return reasoning at fold time (inline <think>)
	// rather than as a separate streamed channel.
	if !streamedReasoning {
		if think := a.nameReasoning(glimpseReasoning(res.Message.Reasoning)); think != "" {
			a.out.Think(think)
		}
	}
	return casMod
}

// closeTurn is the close stage (MORPHEUS req.3.1) and the ONE evaluation site
// of the termination guard chain (req.4.1): a bare-answer turn (no tool calls)
// walks closeGuardChain in table order and the first firing guard decides —
// deliver | nudge-and-continue | suppress — with finishTurn remaining the
// single delivery choke point. Contract — inputs: the model result, whether
// Cassandra modified this step, and the turn's user input; mutates: whatever
// the one firing guard mutates (guidance nudges on the working transcript, the
// unified unproductive counter, the scrubbed answer); returns finished=true
// when the turn delivered (Chat returns nil), err on the unproductive-cap
// escalation, and (false, nil) when the loop must re-enter (a nudge or
// suppression asked the model to continue).
//
// Termination (Cassandra 2.0: the proof-of-work completion gate is retired):
// a turn ends when the model emits NO tool calls — the single completion path
// — once no guard in the chain blocks the close. Honesty emerges from the
// agent re-verifying under Cassandra's injected doubt, backstopped by the
// loop-discipline guarantees, not from a terminal adjudicator.
func (a *Agent) closeTurn(ctx context.Context, res *llm.ChatResult, casMod bool, userInput string) (finished bool, err error) {
	t := a.turn
	cc := &closeContext{res: res, answer: strings.TrimSpace(res.Message.Content), casMod: casMod}
	_, dec := a.evalCloseChain(cc)
	if dec.err != nil {
		return false, dec.err
	}
	if dec.verdict != verdictDeliver {
		// Reasoning-churn fold (item 2): this close is re-looping (a guard nudged
		// or suppressed it). If its prose substantially repeats the previous
		// re-looped close, the model is re-deriving the same answer without acting
		// or progressing — invisible to the tool-batch repeat reads (a bare-answer
		// step carries no tool calls to compare) and to the evidence-convergence
		// meter. Past the stall bound it escalates to an honest stop through the
		// SAME terminal funnel as every other unproductive death (escalateGuidance
		// → governDeath), adding no new terminal site. A dedicated counter keeps it
		// from double-counting with the guard nudges that share t.unproductive.
		if a.noteCloseChurn(cc.answer) {
			return false, a.escalateGuidance(t.closeChurn)
		}
		return false, nil
	}
	// Delivered: genuine progress resets the reasoning-churn read.
	t.closeChurn = 0
	t.lastCloseContent = ""
	// The answer is really shipping: record the ORIGINAL bare-answer turn to
	// the durable cortex transcript now (deliberate defers bare answers to
	// this delivery verdict so a rejected close never poisons durable memory
	// with an answer the user was never shown).
	a.cmRecordAssistant(res.Message)
	a.finishTurn(ctx, cc.answer, t.surfaced, t.surfacedSnips, userInput, false)
	return true, nil
}

// noteCloseChurn tracks reasoning churn across consecutive re-looped bare-answer
// closes (item 2): a close that re-loops and whose prose substantially repeats
// the previous re-looped close is the model re-deriving the same answer without
// progressing. The tool-batch repeat detectors miss it — a bare answer carries no
// tool calls to compare. It counts consecutive such closes and reports whether
// the count met the no-progress stall bound. The empty-answer case is owned by
// guardEmptyAnswer and skipped here; distinct prose resets the run.
func (a *Agent) noteCloseChurn(answer string) bool {
	t := a.turn
	answer = strings.TrimSpace(answer)
	if answer == "" {
		return false
	}
	if t.lastCloseContent != "" && a.contentChurn(t.lastCloseContent, answer) {
		t.closeChurn++
	} else {
		t.closeChurn = 0
	}
	t.lastCloseContent = answer
	bound := a.cfg.NoProgressStall
	return bound > 0 && t.closeChurn >= bound
}

// act is the act stage (MORPHEUS req.3.1): narration, the governor's outer
// failsafe commit for this batch, the check-before-act gate, and tool
// dispatch with result assembly. Contract — inputs: the step index, this
// step's context-fill pct, and the tool-calling model result; mutates: the
// working transcript (tool results, guidance), the turn's stall bookkeeping
// and counters, the loop snapshot, and the epistemic run state (via the
// observe seams); returns a terminal error (via governDeath) on a stall death
// or the unproductive-cap escalation, nil otherwise. (The failsafe commit
// stays after the preamble narration for order fidelity; its verdict comes
// from the unified signal state computed at deliberate.)
func (a *Agent) act(ctx context.Context, step, pct int, res *llm.ChatResult) error {
	t := a.turn
	if a.cfg.InteractionPosture && t.posture != PostureExecution && callsRequireExecution(res.Message.ToolCalls) {
		if err := a.promoteToExecution(); err != nil {
			return err
		}
		a.pushGuidance("This turn crossed into state-changing execution. Apply the execution contract and guards before continuing.\n" + t.contract.Render())
	}
	// Per-step dispatch-gate read (item 1c): reset before the gates run this step,
	// so the end-of-act fold can tell a wholly-refused step (refused a call, then
	// dispatched nothing) from a step that made real tool progress.
	t.stepRefused = false
	t.stepDispatched = false
	for _, call := range res.Message.ToolCalls {
		if call.Function.Name == tools.MemoryRecallTool {
			t.episodicGrounded()
		}
	}
	// Surface any preamble the model wrote alongside its tool calls as
	// DURABLE narration — Neo "thinking out loud" before it acts. This runs
	// for EVERY tool-calling turn: it is what makes Neo's running commentary
	// the durable thread content.
	if c := strings.TrimSpace(res.Message.Content); c != "" {
		a.out.Status(a.cleanContent(c))
		// Epistemic-core Mechanism 1 (req.4.1): the first committing
		// assistant turn IS plan formation — extract its load-bearing
		// premises with provenance into the resident ledger.
		a.premiseObservePlan(ctx, c)
	}

	// Governor fire position 3 — the outer no-progress failsafe: a repeat is
	// a byte-identical batch signature, a SEMANTIC repeat (same operation
	// reworded — a cosmetic reword can't reset the counter and loop forever,
	// NE-4), or a rotating A→B→A→B cycle that introduces no new tool.
	// Distinct operations reset the repeat read.
	repeat, stalled := a.governFailsafes(res.Message.ToolCalls)
	// Self-model task 2.2: refresh the live loop-state snapshot for THIS
	// step so any death exit below captures the real state at death.
	a.snapshotLoop(step, pct, t.repeats, t.recentSigs, res.Message.ToolCalls, t.distinctToolSet)
	if stalled {
		// No-progress stall: do NOT fabricate a close. Return an incomplete
		// signal so the supervisor can respawn a fresh agent and continue —
		// the task is not done. (On the bare CLI path, with no supervisor, the
		// wrapped reason is printed.)
		return a.governDeath(DeathReasonStall, "repeating the same step without progress. Where it got stuck:")
	}
	// req.8.1: a no-progress repeat is itself an unproductive attempt — fold
	// it into the ONE unified counter (alongside completion rejections and
	// guidance nudges) so an interleaved mix that never trips the pure-repeat
	// stall is still bounded, and escalate to an honest stop-and-ask past the
	// bound rather than running to the step budget.
	if repeat {
		t.unproductive++
		if a.capExceeded(t.unproductive) {
			return a.escalateGuidance(t.unproductive)
		}
	}
	// One firm convergence steer just before the hard stall: the model
	// re-does work it already completed (re-fetching/re-rendering a value it
	// already has) instead of closing out. SUPERSEDED by Cassandra 2.0
	// (req.6.3): when the controller is enabled, its loop trigger fires at the
	// same point (loop_threshold defaults to no_progress_stall-1) and delivers
	// the same "you're re-doing completed work — step back" intent as the
	// agent's OWN doubt inside its assistant channel, one step before the hard
	// stall. This legacy GuidanceMessage steer only runs when the controller is
	// DISABLED, keeping that path byte-identical to the pre-feature loop
	// (req.10.2). Injected AFTER this step's tool results below so the
	// transcript stays well-formed.
	injectConvergeNudge := !a.cfg.CassandraEnabled && a.cfg.NoProgressStall >= 2 && t.repeats == a.cfg.NoProgressStall-1 && !t.convergeNudged

	// P2-7: record distinct tool names for the adaptive budget signal.
	for _, c := range res.Message.ToolCalls {
		t.distinctToolSet[c.Function.Name] = struct{}{}
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
	// straight-to-tools turn marked itself "already narrated" and a later
	// bare-answer surface was hidden, so the user saw only the stub.
	if strings.TrimSpace(res.Message.Content) == "" {
		if line := narrateBatch(res.Message.ToolCalls); line != "" {
			a.out.Progress(line)
		}
	}

	// Epistemic-core check-before-act (req.5): the gate validates the
	// plan's self-claims against the resident capability surface and
	// refuses dependent dispatches while a refuted premise stands
	// (introspection tools stay allowed — they are the discharge path).
	allowed := res.Message.ToolCalls
	if a.executionPosture() {
		var refusedByGate bool
		allowed, refusedByGate = a.checkBeforeAct(res.Message.ToolCalls)
		if refusedByGate {
			t.stepRefused = true
		}
		if !t.contract.ReadyForMutation() {
			a.pushGuidance("The task contract has an unresolved material input. Do not call tools or mutate state. Ask only for the missing required value recorded in the contract.")
			return nil
		}
	}
	if err := a.runToolCalls(ctx, allowed); err != nil {
		return err
	}
	// A step whose calls were wholly refused at a dispatch gate (a missing-
	// prediction probe refusal or a refuted-premise dependent refusal) and that
	// dispatched NOTHING makes no progress, yet produces no dispatched failure and
	// — when the model varies the guessed argument each time — no batch-repeat, so
	// it evades every existing stall read. Count it on the dedicated refusal run so
	// a refusal LOOP escalates to an honest stop-and-ask instead of running
	// silently to the step budget. An IDENTICAL refused batch is EXCLUDED (!repeat):
	// it is a byte/semantic/cyclic repeat the stall path already counts and dies on
	// at NoProgressStall, so this owns only the gap that path misses — varied-
	// argument refusals — and never races the stall verdict for identical spirals.
	// A genuine dispatch resets the run (real progress). It is kept off the shared
	// t.unproductive counter so it never interacts with the repeat/guard-nudge
	// bookkeeping.
	switch {
	case t.stepRefused && !t.stepDispatched && !repeat:
		t.refusalRun++
		if a.cfg.NoProgressStall > 0 && t.refusalRun >= a.cfg.NoProgressStall {
			return a.escalateGuidance(t.refusalRun)
		}
	case t.stepDispatched:
		t.refusalRun = 0
	}
	// req.8.2 (N2): a plain tool dispatch does NOT reset the unified
	// unproductive counter — only genuine accepted progress does. This keeps
	// an interleaved real tool call from silently resetting the loop-discipline
	// bound and letting the loop run to the step budget.
	if injectConvergeNudge {
		t.convergeNudged = true
		a.pushGuidance("You already obtained the result and showed it — do NOT fetch or render it again. Give the user the answer now and finish.")
	}
	return nil
}

func (a *Agent) armMemoryMutationFinalization(name, content string, isErr bool) {
	if a == nil || a.turn == nil || !a.cfg.InteractionPosture || !isErr || name != tools.MemoryMutateTool {
		return
	}
	normalized := strings.ToLower(content)
	if !strings.Contains(normalized, "semantic target matched no current memory") &&
		!strings.Contains(normalized, "semantic target matched no live memory") &&
		!strings.Contains(normalized, "semantic target is ambiguous") {
		return
	}
	if a.turn.localFinalizePending == "" {
		a.turn.localRecoveries++
	}
	a.turn.localFinalizePending = "The durable-memory change could not be applied because its target is missing, stale, or ambiguous. Do not retry memory_mutate or call another tool in this response. Preserve and deliver the useful answer or work already completed. If changing durable memory was the user's explicit primary request, say plainly that no record was changed and ask one precise question that identifies the intended current record; otherwise do not let this optional memory conflict block the user's actual task."
}

// noteBatch commits the no-progress/stall read for ONE assistant tool-calling
// batch from the unified per-step signal state (MORPHEUS req.5.1): the repeat
// verdict (byte-identical, SEMANTIC reword, or a rotating A→B→A→B cycle that
// introduces no new tool — a distinct operation resets it, NE-4) was computed
// ONCE in computeStepSignals at deliberate entry; noteBatch advances the
// committed stall bookkeeping from it. Sharing one read between the dispatch
// path and the Cassandra controller is what keeps the two from drifting
// (N2/req.8.3 heritage: nothing bypasses stall detection). Contract — mutates
// only the turn's stall bookkeeping; returns whether the batch was a repeat
// and whether the pure-repeat stall bound (NoProgressStall) is met.
func (a *Agent) noteBatch(calls []llm.ToolCall) (repeat, stalled bool) {
	t := a.turn
	s := t.signals
	// Defensive: a caller outside the staged loop (no stored state for THIS
	// batch) gets a fresh read over the live turn state — same computation,
	// same verdict.
	if s == nil || s.sig != batchSignature(calls) {
		s = a.computeStepSignals(t.curLoop.step, llm.Message{ToolCalls: calls})
	}
	if s.repeat {
		t.repeats++
	} else {
		t.repeats = 0
	}
	t.recentSigs = pushWindow(t.recentSigs, s.sig, stallWindow)
	t.prevSig = s.sig
	t.prevCalls = calls
	return s.repeat, t.repeats >= a.cfg.NoProgressStall
}

// pushGuidance is THE guidance choke point (MORPHEUS req.3.3): every
// guidance-channel injection — the close-chain nudges, the legacy converge
// steer, the self-claim contradiction and forced-revision directives, and the
// prediction-mismatch guidance — flows through this one function. It builds
// the turn via llm.GuidanceMessage (the Guidance flag + envelope are what keep
// steering out of the durable cortex transcript — cmRecordAssistant's
// IsGuidance gate — and off every user-facing surface — StripGuidance at the
// harness) and appends it to the working transcript. Contract — mutates: the
// working transcript only; blank text appends nothing. Returns the built
// message (zero when nothing was appended) so a caller that must also thread
// the steer into an in-flight window copy (forcedRevisionStep) reuses the SAME
// turn instead of minting a second one.
func (a *Agent) pushGuidance(text string) llm.Message {
	g := llm.GuidanceMessage(text)
	if g.Content == "" {
		return g
	}
	a.working = append(a.working, g)
	return g
}

// pushGuidanceNudge routes a system-steering nudge through the guidance choke
// point (req.1.1) and folds it into the ONE unified unproductive-attempt
// counter (req.8.1): it increments the counter and reports whether the bound is
// now exceeded. The close-chain guards (truncation/empty/read-full/identity
// steers) all push through here, so they share ONE bound with the no-progress
// repeats fed in the loop. The counter is reset ONLY on genuine accepted
// progress (req.8.2) — never by a plain tool dispatch. A cap of 0 disables the
// bound (unbounded nudging).
func (a *Agent) pushGuidanceNudge(text string, counter *int) (capExceeded bool) {
	a.pushGuidance(text)
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
// user rather than respawning into the same loop) and ends the turn through
// the governor's terminal verdict with an honest where-it-stands digest.
func (a *Agent) escalateGuidance(attempts int) error {
	a.turn.lastFailureClass = delegate.ClassDeterministic
	return a.governDeath(DeathReasonUnproductive, fmt.Sprintf("I made no productive progress after %d unproductive attempts (repeated steer/reject without closing it). Where it stands:", attempts))
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

// Reset clears the live transcript + goal (new conversation).
func (a *Agent) Reset() {
	a.working = nil
	a.activeGoal = ""
	a.resetSessionIntent()
	a.semanticCheckpoint = sessionjournal.SemanticCheckpoint{}
	if a.cfg.SessionCurrentIntent {
		a.turn = newTurn()
	}
	a.runtimeLast = ""
	a.runtimeFailure = delegate.ClassNone
	a.episodicReset()
}

// BestEffort returns the most honest "where things stand" digest the agent can
// produce WITHOUT having finished — the latest assistant narration, else the
// last tool summary. The task supervisor uses it to deliver a truthful partial
// when a task hits its hard ceiling; it is never a fabricated success. Empty
// when there is genuinely nothing to report.
func (a *Agent) BestEffort() string {
	if a.runtimeMode == "resurrection" {
		return strings.TrimSpace(a.runtimeLast)
	}
	if s := strings.TrimSpace(lastAssistantText(a.working, 1600)); s != "" {
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
	if !a.cfg.SessionCurrentIntent && a.activeGoal == "" {
		a.activeGoal = strings.TrimSpace(goal)
	}
}

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

// nameReasoning applies the identity net to visible reasoning. Preferred-name
// capitalization is preserved by the prompt, but names are not injected
// mechanically: deterministic replacement made every update sound repetitive.
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
	return text
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
	if id := strings.TrimSpace(a.logicalIntentID); id != "" {
		return id
	}
	cid := a.convID
	if cid == "" {
		cid = "cli"
	}
	return fmt.Sprintf("neo-turn:%s:%d", cid, a.turnSeq)
}

// Output-token escalation ladder for a truncated generation (synthesis-survives-
// death): a generation cut off by the output limit (finish_reason=length) is
// re-attempted with a higher per-request output-token budget so a long final
// synthesis converges instead of re-truncating at the same limit — Claude Code's
// 8K→64K posture. The floor matches the client's own default; each bump doubles
// toward the ceiling, bounded so a pathologically long turn can't grow unbounded.
const (
	answerTokenFloor    = 8192
	answerTokenCeiling  = 65536
	maxAnswerTokenBumps = 3
)

// auditEventBudgetOverage records a generation that exceeded the O1
// representation budget. It is observability ONLY — the generation is still
// used — so an oversized-but-complete answer is visible for diagnosis without
// being destroyed.
const auditEventBudgetOverage = "o1.budget_overage"

// bumpAnswerBudget raises this turn's per-request output-token budget one rung.
// The bumped budget rides every subsequent generate call this turn (turn.answer
// TokenBudget threads into the ChatRequest); 0 leaves the client default. Bounded
// by maxAnswerTokenBumps so a model that keeps getting cut off escalates through
// the truncated-answer guard rather than growing the budget forever.
func (a *Agent) bumpAnswerBudget() {
	if a.turn.answerTokenBumps >= maxAnswerTokenBumps {
		return
	}
	a.turn.answerTokenBumps++
	next := a.turn.answerTokenBudget
	if next < answerTokenFloor {
		next = answerTokenFloor
	}
	next *= 2
	if next > answerTokenCeiling {
		next = answerTokenCeiling
	}
	a.turn.answerTokenBudget = next
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
			repairable := a.normalizeProviderResult(res)
			if repairable && res != nil {
				return res, nil
			}
			if conformErr := o1.ConformChatResult(res, o1.DefaultBudget(), o1.DialectForProvider(a.main.Provider())); conformErr != nil {
				// A SIZE verdict is not a failure: the generation is complete and
				// usable, merely long. Discarding it here destroyed finished
				// answers and killed the run (2026-07-25) — the length ladder in
				// the close chain (guardTruncatedAnswer) and the tools layer's own
				// graceful payload limit are the surfaces that handle size, and
				// neither can act on a generation this function threw away. Audit
				// it and hand the result on.
				if errors.Is(conformErr, o1.ErrBudgetExceeded) {
					a.emitAudit(auditEventBudgetOverage, map[string]interface{}{"detail": oneLine(conformErr.Error())})
				} else {
					lastErr = fmt.Errorf("o1 provider protocol: %w", conformErr)
					if !a.cfg.InteractionPosture {
						return nil, lastErr
					}
					a.turn.localRecoveries++
					continue
				}
			}
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

func (a *Agent) runToolCalls(ctx context.Context, calls []llm.ToolCall) error {
	// P2-5: dispatch INDEPENDENT tool calls in a turn concurrently (bounded
	// by cfg.ToolDispatchConcurrency), preserving result ordering + per-tool
	// observer events. When concurrency <=0 or there's a single call, the
	// path degenerates to the legacy serial loop (no goroutine overhead).
	// Determinism + i6 are preserved: results are assembled in CALL order
	// (index i) regardless of completion order, and the observer start/end
	// events fire in order so the UI's per-call viewport correlation holds.
	n := len(calls)
	if n <= 1 || a.cfg.ToolDispatchConcurrency <= 0 {
		return a.runToolCallsSerial(ctx, calls)
	}
	if err := a.journalToolCalls(ctx, calls); err != nil {
		return err
	}

	conc := a.cfg.ToolDispatchConcurrency
	if conc > n {
		conc = n
	}

	type dispatchResult struct {
		content      string
		evidence     string
		shot         string
		isErr        bool
		class        delegate.FailureClass
		failureClass exectool.FailureClass
	}

	// Fire ALL ToolStart observer events up front, in call order, so the
	// surface paints every live viewport the instant the batch is dispatched
	// (matching the serial path's per-call start event). stepIDs are computed
	// once here and reused for the end events.
	stepIDs := make([]string, n)
	parsedArgs := make([]map[string]interface{}, n)
	expects := make([]string, n)
	// One verdict per strategy per BATCH, so a call and its deduped duplicate
	// are never split by the bounded expectation gate.
	batchRefused := map[string]bool{}
	for i, call := range calls {
		name := call.Function.Name
		args, perr := call.ParseArgs()
		if perr != nil {
			// Parse failure: surface immediately and mark so dispatch skips it.
			content := a.malformedArgumentsObservation(call, perr)
			if err := a.journalToolResult(ctx, call, i, content, content, true, exectool.FailureValidation); err != nil {
				return err
			}
			a.working = append(a.working, llm.ToolResult(call.ID, name, content))
			parsedArgs[i] = nil // sentinel: already handled, do not dispatch
			stepIDs[i] = ""
			continue
		}
		// Epistemic-core Mechanism 3: lift the stated expectation off the call
		// (the tool never sees it); the belief update runs at result assembly.
		// A probe with NO expectation is a guess — refused at the seam with
		// the ground-or-hypothesize directive (req.6.1), never dispatched.
		expects[i] = popExpect(args)
		if directive, refused := a.refuseUnstatedExpectation(name, args, expects[i], batchRefused); refused {
			if err := a.journalToolResult(ctx, call, i, directive, directive, true, exectool.FailureValidation); err != nil {
				return err
			}
			a.working = append(a.working, llm.ToolResult(call.ID, name, directive))
			a.cmRecordToolResult(name, directive)
			a.turn.stepRefused = true
			parsedArgs[i] = nil
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
			content, evidence, shot, isErr, class, failureClass := a.dispatchWithRetry(ctx, call.Function.Name, args)
			results[i] = dispatchResult{content: content, evidence: evidence, shot: shot, isErr: isErr, class: class, failureClass: failureClass}
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
	var convergenceErr error
	for i, call := range calls {
		if parsedArgs[i] == nil {
			continue // parse-failed: already appended above
		}
		// A call reaching here was dispatched (not parse-failed, not gate-refused):
		// the step made real progress, so it is not a wholly-refused step (item 1c).
		a.turn.stepDispatched = true
		name := call.Function.Name
		content := results[i].content
		evidence := results[i].evidence
		isErr := results[i].isErr
		a.armMemoryMutationFinalization(name, evidence, isErr)
		// Record the shared class of this dispatch's outcome (a success clears a
		// prior recorded failure) for the supervisor. Single-threaded here (the
		// concurrent goroutines only wrote results[i]), so no race on
		// a.turn.lastFailureClass.
		a.noteFailureClass(results[i].class, results[i].isErr)
		if convergenceErr == nil {
			convergenceErr = a.noteNormalizedFailure(name, parsedArgs[i], results[i].failureClass, results[i].class, isErr)
		}
		// Cap the transcript copy: a single oversized tool result can blow
		// the provider's request-body byte cap on its own. The observer
		// below still gets the full, untruncated content so the product
		// shows real evidence.
		capped := a.capToolResult(name, content)
		if err := a.journalUncertainEffect(ctx, call, i, parsedArgs[i], evidence, results[i].failureClass); err != nil {
			return err
		}
		if err := a.journalToolResult(ctx, call, i, capped, evidence, isErr, results[i].failureClass); err != nil {
			return err
		}
		a.working = append(a.working, llm.ToolResult(call.ID, name, capped))
		// Continuous-memory (task 6.1): record the FULL tool result to the
		// durable cortex transcript (cortex spills oversized payloads itself).
		a.cmRecordToolResult(name, evidence)
		a.noteWebEvidence(name, parsedArgs[i], evidence, isErr)
		// Epistemic-core Mechanisms 3+4 (req.6.2/7.1): the belief-update seam —
		// the probe's expectation is checked against the real outcome, then the
		// action links to the task graph and the evidence delta is computed.
		missed := a.predictionObserve(ctx, name, parsedArgs[i], expects[i], evidence, isErr)
		a.graphObserve(name, parsedArgs[i], evidence, isErr, missed)
		if a.observer != nil {
			// Resolve the browsing filmstrip still for this call (a direct
			// screenshot's URL, or a deterministic auto-capture after a
			// view-changing action). Single-threaded here, so the per-turn cap
			// counter is safe. Best-effort: "" when there is no frame.
			shot := a.screenshotForCall(ctx, name, results[i].shot, isErr)
			a.observer(ToolEvent{ID: stepIDs[i], Name: name, Args: parsedArgs[i], Result: evidence, IsErr: isErr, FailureClass: string(results[i].failureClass), Phase: ToolEnd, ScreenshotURL: shot})
		}
	}
	return convergenceErr
}

// runToolCallsSerial is the legacy single-threaded dispatch path. It's
// kept for the concurrency<=0 config and for single-call batches (where
// goroutine spin-up would be pure overhead). Behaviour is byte-identical
// to the pre-P2-5 loop.
func (a *Agent) runToolCallsSerial(ctx context.Context, calls []llm.ToolCall) error {
	if err := a.journalToolCalls(ctx, calls); err != nil {
		return err
	}
	// Batch idempotency (NE-3, req 5.x), serial path: an equivalent
	// state-touching duplicate (core_execute with the same resolved intent)
	// joins the canonical call's cached result instead of running again. The
	// canonical index is always earlier, so its result is ready by the time a
	// duplicate is reached.
	joinTo := dedupStateTouching(calls)
	type dispatchResult struct {
		content      string
		evidence     string
		shot         string
		isErr        bool
		class        delegate.FailureClass
		failureClass exectool.FailureClass
	}
	results := make([]dispatchResult, len(calls))
	var convergenceErr error
	batchRefused := map[string]bool{}
	for i, call := range calls {
		name := call.Function.Name
		args, perr := call.ParseArgs()
		if perr != nil {
			content := a.malformedArgumentsObservation(call, perr)
			if err := a.journalToolResult(ctx, call, i, content, content, true, exectool.FailureValidation); err != nil {
				return err
			}
			a.working = append(a.working, llm.ToolResult(call.ID, name, content))
			continue
		}
		// Epistemic-core Mechanism 3: lift the stated expectation off the call
		// (the tool never sees it); the belief update runs after dispatch.
		// A probe with NO expectation is a guess — refused at the seam with
		// the ground-or-hypothesize directive (req.6.1), never dispatched.
		expect := popExpect(args)
		if directive, refused := a.refuseUnstatedExpectation(name, args, expect, batchRefused); refused {
			if err := a.journalToolResult(ctx, call, i, directive, directive, true, exectool.FailureValidation); err != nil {
				return err
			}
			a.working = append(a.working, llm.ToolResult(call.ID, name, directive))
			a.cmRecordToolResult(name, directive)
			a.turn.stepRefused = true
			continue
		}
		a.turn.stepDispatched = true // dispatched here: real step progress (item 1c)
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
		var content, evidence, shot string
		var isErr bool
		var class delegate.FailureClass
		var failureClass exectool.FailureClass
		if joinTo[i] >= 0 {
			// Deduped duplicate: reuse the canonical call's result; do not
			// submit the same state-touching work twice.
			content, evidence, shot, isErr, class, failureClass = results[joinTo[i]].content, results[joinTo[i]].evidence, results[joinTo[i]].shot, results[joinTo[i]].isErr, results[joinTo[i]].class, results[joinTo[i]].failureClass
		} else {
			content, evidence, shot, isErr, class, failureClass = a.dispatchWithRetry(ctx, name, args)
		}
		results[i] = dispatchResult{content: content, evidence: evidence, shot: shot, isErr: isErr, class: class, failureClass: failureClass}
		a.armMemoryMutationFinalization(name, evidence, isErr)
		// Record the shared class of this dispatch's outcome (a success clears a
		// prior recorded failure) so the supervisor reads the SAME
		// classification (NE-5).
		a.noteFailureClass(class, isErr)
		if convergenceErr == nil {
			convergenceErr = a.noteNormalizedFailure(name, args, failureClass, class, isErr)
		}
		// Cap the transcript copy: a single oversized tool result (large
		// fetch / file read / MCP payload) can blow the provider's request-
		// body byte cap on its own. The observer below still gets the full,
		// untruncated content so the product shows real evidence.
		capped := a.capToolResult(name, content)
		if err := a.journalUncertainEffect(ctx, call, i, args, evidence, failureClass); err != nil {
			return err
		}
		if err := a.journalToolResult(ctx, call, i, capped, evidence, isErr, failureClass); err != nil {
			return err
		}
		a.working = append(a.working, llm.ToolResult(call.ID, name, capped))
		// Continuous-memory (task 6.1): record the FULL tool result to the
		// durable cortex transcript (cortex spills oversized payloads itself).
		a.cmRecordToolResult(name, evidence)
		a.noteWebEvidence(name, args, evidence, isErr)
		// Epistemic-core Mechanisms 3+4 (req.6.2/7.1): the belief-update seam,
		// then the task-graph action link + evidence delta.
		missed := a.predictionObserve(ctx, name, args, expect, evidence, isErr)
		a.graphObserve(name, args, evidence, isErr, missed)
		// Surface the completed work (command output, fetched page, file
		// contents, web-search snippets, …) so the product renders real
		// evidence, not just a synthesized answer.
		if a.observer != nil {
			// Attach the browsing filmstrip still (direct screenshot URL, or a
			// deterministic auto-capture after a view-changing action). A joined
			// duplicate reuses the canonical still and does NOT re-capture.
			shotURL := shot
			if joinTo[i] < 0 {
				shotURL = a.screenshotForCall(ctx, name, shot, isErr)
			}
			a.observer(ToolEvent{ID: stepID, Name: name, Args: args, Result: evidence, IsErr: isErr, FailureClass: string(failureClass), Phase: ToolEnd, ScreenshotURL: shotURL})
		}
	}
	return convergenceErr
}

// noteFailureClass records the classified outcome of the most recent tool
// dispatch this turn so the task supervisor (LastFailureClass) reads the SAME
// taxonomy the dispatch ladder used. A SUCCESSFUL dispatch CLEARS a prior
// failure: recovery is real progress, so a turn that later dies did NOT die on
// a wall it already got past — without the clear, one recovered exploration
// miss (a 404 read of a guessed path) converts any later unrelated death into
// a false deterministic stop-and-ask (the misleading "permission or limit"
// terminal over a healthy turn). A content-level tool error that carries no
// classified failure (isErr with ClassNone) leaves the record untouched: the
// agent may still be stuck on the recorded wall. Called only from the
// single-threaded result-assembly paths, so it never races the concurrent
// dispatch goroutines.
func (a *Agent) noteFailureClass(class delegate.FailureClass, isErr bool) {
	switch {
	case class != delegate.ClassNone:
		a.turn.lastFailureClass = class
	case !isErr:
		a.turn.lastFailureClass = delegate.ClassNone
	}
}

// noteNormalizedFailure bounds repeated deterministic failures by the
// invariant strategy and semantic failure layer. Changing a guessed URL/path
// while repeating the same shell/search/fetch strategy cannot reset the bound.
func (a *Agent) noteNormalizedFailure(name string, args map[string]interface{}, failureClass exectool.FailureClass, recoveryClass delegate.FailureClass, isErr bool) error {
	t := a.turn
	// Exact/semantic/cyclic repeats already belong to the unified governor
	// signal and its hard-stall verdict. This fine-grained key closes only the
	// gap where varying arguments evade that existing behavioral read.
	if t.signals != nil && t.signals.repeat {
		t.normalizedFailureKey = ""
		t.normalizedFailureCount = 0
		return nil
	}
	if !isErr || recoveryClass != delegate.ClassDeterministic || failureClass == exectool.FailureNone {
		t.normalizedFailureKey = ""
		t.normalizedFailureCount = 0
		return nil
	}
	strategy, ok := probeStrategy(name, args)
	if !ok {
		strategy = name
	}
	key := strategy + "|" + string(failureClass)
	if key == t.normalizedFailureKey {
		t.normalizedFailureCount++
	} else {
		t.normalizedFailureKey = key
		t.normalizedFailureCount = 1
	}
	bound := a.cfg.NoProgressStall
	if bound <= 0 {
		bound = a.cfg.MaxGuidanceNudges
	}
	if bound <= 0 || t.normalizedFailureCount < bound {
		return nil
	}
	t.lastFailureClass = delegate.ClassDeterministic
	t.curLoop.lastTool = name
	t.curLoop.repeats = t.normalizedFailureCount
	return a.escalateGuidance(t.normalizedFailureCount)
}

// LastFailureClass reports the shared FailureClass of the most recent
// classified tool failure in the current turn (delegate.ClassNone when none).
// The task supervisor reads it after Chat returns to decide whether a
// non-clean exit is a deterministic blocker (stop-and-ask, no respawn) or a
// transient/model/stall failure (the existing respawn path).
func (a *Agent) LastFailureClass() delegate.FailureClass {
	if a.runtimeMode == "resurrection" {
		return a.runtimeFailure
	}
	return a.turn.lastFailureClass
}

func (a *Agent) LastO1Decision() (o1.SupervisorDecision, bool) {
	if a.runtimeMode == "resurrection" {
		return o1.SupervisorDecision{}, false
	}
	if a.turn == nil || a.turn.runLedger == nil {
		return o1.SupervisorDecision{}, false
	}
	var proof *o1.ProofManifest
	if a.turn.verifiers.AllClosed() {
		candidate := a.turn.verifiers.GenerateProofManifest(
			a.turn.runLedger.RunID, a.turn.contract.ID,
			len(a.turn.runLedger.Effects) == 0 || a.turn.runLedger.EffectsReconciled,
			true,
		)
		proof = &candidate
	}
	decision := (&o1.Supervisor{}).Evaluate(
		a.turn.contract, a.turn.runLedger, &a.turn.verifiers, proof, a.turn.outcome(),
	)
	return decision, true
}

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
func (a *Agent) dispatchWithRetry(ctx context.Context, name string, args map[string]interface{}) (content, evidence, shot string, isErr bool, recoveryClass delegate.FailureClass, failureClass exectool.FailureClass) {
	// read_overflow is synthetic and agent-owned (the overflow store lives on
	// the agent, not the Manager): page a truncated result back in and mark it
	// read (the read-full latch). It never retries or routes to the Manager.
	if name == readOverflowTool {
		content, isErr := a.readOverflow(args)
		class := exectool.FailureNone
		if isErr {
			class = exectool.FailureValidation
		}
		a.recordO1ToolOutcome(name, args, content, isErr, class, false)
		return content, content, "", isErr, normalizedRecoveryClass(class, false), class
	}
	if a.tools == nil {
		content := "no tools are available in this session."
		a.recordO1ToolOutcome(name, args, content, true, exectool.FailureInvocation, false)
		return content, content, "", true, delegate.ClassDeterministic, exectool.FailureInvocation
	}
	// Restricted agents are held to their advertised surface: the Manager's
	// dispatch switch serves synthetic tools regardless of advertisement, so an
	// unadvertised name must be rejected HERE for the restriction to be
	// structural rather than advisory.
	if a.advertised != nil {
		if _, ok := a.advertised[name]; !ok {
			content := a.unavailableToolObservation(name)
			a.recordO1ToolOutcome(name, args, content, true, exectool.FailureInvocation, false)
			return content, content, "", true, delegate.ClassDeterministic, exectool.FailureInvocation
		}
	}
	manifest := a.o1Manifest(name)
	if manifest.Effects != o1.EffectReadOnly && a.turn != nil && a.turn.runLedger != nil {
		argsJSON, _ := json.Marshal(args)
		preState := o1.ContentHash(argsJSON)
		if a.turn.runLedger.HasUnreconciledEffect(name, "uncertain:"+preState) {
			content := fmt.Sprintf("class=%s: prior transport outcome is uncertain; reconcile authoritative state before retrying this operation", exectool.FailureConflict)
			a.recordO1ToolOutcome(name, args, content, true, exectool.FailureConflict, false)
			return content, content, "", true, delegate.ClassDeterministic, exectool.FailureConflict
		}
		if prior, ok := a.turn.runLedger.SuccessfulAttempt(name, preState); ok {
			a.turn.commitOutcome(prior.Outcome)
			return prior.Evidence, prior.Evidence, "", false, delegate.ClassNone, exectool.FailureNone
		}
	}
	var lastErr error
	for attempt := 0; attempt <= a.cfg.MaxRetriesPerTool; attempt++ {
		if attempt > 0 {
			if !backoff(ctx, attempt) {
				break
			}
		}
		raw, shot, isErr, failureClass, retryable, failureMessage, err := a.tools.DispatchMediaClassified(ctx, name, args)
		if err == nil {
			if raw == "" {
				raw = failureMessage
			}
			modelContent := raw
			recoveryClass := delegate.ClassNone
			if isErr {
				if failureMessage != "" {
					modelContent = fmt.Sprintf("class=%s: %s", failureClass, failureMessage)
				}
				recoveryClass = normalizedRecoveryClass(failureClass, retryable)
			}
			outcome := a.recordO1ToolOutcome(name, args, raw, isErr, failureClass, retryable)
			if isErr {
				decision := o1.SelectRecoveryTransition(a.o1Manifest(name), outcome, attempt+1)
				a.recordO1Recovery(name, decision, attempt+1)
				if decision.Transition == o1.TransitionRetry && !decision.Terminal {
					lastErr = fmt.Errorf("%s", failureMessage)
					continue
				}
				if decision.Transition == o1.TransitionReconcile {
					modelContent = fmt.Sprintf("class=%s: state is uncertain and must be reconciled before retry; %s", failureClass, failureMessage)
				}
			}
			return modelContent, raw, shot, isErr, recoveryClass, failureClass
		}
		lastErr = err
		failureClass = exectool.FailureClassOf(err)
		retryable = delegate.ClassOf(err) != delegate.ClassDeterministic
		outcome := a.recordO1ToolOutcome(name, args, err.Error(), true, failureClass, retryable)
		if ctx.Err() != nil {
			break
		}
		// A deterministic failure will recur identically on a retry of the same
		// request — stop the ladder now and return a single terminal result so
		// the agent stops-and-asks instead of looping (and, on a metered
		// gateway, re-spending). Mirrors chatWithRetry's ErrProviderRejected
		// short-circuit.
		if delegate.ClassOf(err) == delegate.ClassDeterministic {
			content := fmt.Sprintf("tool %q failed and this is a permanent error that will not change on a retry: %v. Don't repeat this call — adjust the approach or ask the user how they'd like to proceed.", name, lastErr)
			return content, lastErr.Error(), "", true, delegate.ClassDeterministic, exectool.FailureClassOf(lastErr)
		}
		decision := o1.SelectRecoveryTransition(a.o1Manifest(name), outcome, attempt+1)
		a.recordO1Recovery(name, decision, attempt+1)
		if decision.Transition != o1.TransitionRetry || decision.Terminal {
			break
		}
	}
	content = fmt.Sprintf("tool %q failed after %d attempts: %v. Consider a different approach.", name, a.cfg.MaxRetriesPerTool+1, lastErr)
	evidence = content
	if lastErr != nil {
		evidence = lastErr.Error()
	}
	return content, evidence, "", true, delegate.ClassOf(lastErr), exectool.FailureClassOf(lastErr)
}

func (a *Agent) recordO1Recovery(name string, decision o1.RecoveryDecision, attempts int) {
	if a.turn == nil || a.turn.runLedger == nil {
		return
	}
	a.turn.runLedger.RecordRecovery(o1.RecoveryRecord{
		Operation: name, Transition: decision.Transition, Attempts: attempts,
		Terminal: decision.Terminal, Reason: decision.Reason,
	})
	a.persistO1State()
}

func (a *Agent) o1Manifest(name string) o1.OperationManifest {
	if a.turn != nil {
		if manifest, ok := a.turn.manifests[name]; ok {
			return manifest
		}
	}
	return o1.OperationManifest{
		Operation: name, Effects: o1.EffectReadOnly,
		Recovery: o1.RecoverySpec{Bound: a.cfg.MaxRetriesPerTool + 1},
	}
}

func (a *Agent) recordO1ToolOutcome(name string, args map[string]interface{}, evidence string, isErr bool, class exectool.FailureClass, retryable bool) o1.OperationOutcome {
	builder := o1.NewOutcome(name).Evidence(evidence)
	if isErr {
		builder.Fail(o1FailureLayer(class), string(class)).Retryable(retryable)
		if retryable {
			if o1FailureLayer(class) == o1.LayerTransport && a.o1Manifest(name).Effects != o1.EffectReadOnly {
				builder.AllowTransition(o1.TransitionReconcile)
			} else {
				builder.AllowTransition(o1.TransitionRetry)
			}
		}
	} else {
		builder.Success().PostSatisfied("operation returned a normalized successful result")
	}
	outcome, err := builder.Build()
	if err != nil {
		outcome = o1.NewOutcome(name).Fail(o1.LayerProtocol, "invalid_normalized_outcome").
			Evidence(err.Error()).MustBuild()
	}
	if a.turn == nil || a.turn.runLedger == nil {
		return outcome
	}
	argsJSON, _ := json.Marshal(args)
	preState := o1.ContentHash(argsJSON)
	postState := preState
	if !isErr {
		postState = o1.ContentHash([]byte(evidence))
	}
	_, _ = a.turn.runLedger.RecordAttempt(o1.AttemptRecord{
		Operation: name, PreState: preState, PostState: postState,
		Outcome: outcome, Evidence: evidence,
	})
	manifest := a.o1Manifest(name)
	if !isErr && manifest.Effects != o1.EffectReadOnly {
		effect := "result:" + postState
		a.turn.runLedger.RecordEffect(name, effect, manifest.Reversibility == o1.Reversible)
		_ = a.turn.runLedger.ReconcileEffect(name, effect)
	} else if isErr && outcome.FailureLayer == o1.LayerTransport && manifest.Effects != o1.EffectReadOnly {
		a.turn.runLedger.RecordEffect(name, "uncertain:"+preState, manifest.Reversibility == o1.Reversible)
	}
	a.turn.commitOutcome(outcome)
	a.persistO1State()
	return outcome
}

func o1FailureLayer(class exectool.FailureClass) o1.FailureLayer {
	switch class {
	case exectool.FailureInvocation:
		return o1.LayerInvocation
	case exectool.FailureProcess:
		return o1.LayerProcess
	case exectool.FailureProtocol:
		return o1.LayerProtocol
	case exectool.FailureHTTP:
		return o1.LayerHTTP
	case exectool.FailureApplication:
		return o1.LayerApplication
	case exectool.FailureValidation:
		return o1.LayerValidation
	case exectool.FailurePolicy:
		return o1.LayerPolicy
	case exectool.FailureAuthorization:
		return o1.LayerAuthorization
	case exectool.FailureConflict:
		return o1.LayerConflict
	case exectool.FailureCancellation:
		return o1.LayerCancellation
	default:
		return o1.LayerTransport
	}
}

func normalizedRecoveryClass(class exectool.FailureClass, retryable bool) delegate.FailureClass {
	if class == exectool.FailureNone {
		return delegate.ClassNone
	}
	if retryable || class == exectool.FailureTransport {
		return delegate.ClassTransient
	}
	return delegate.ClassDeterministic
}

// screenshotForCall resolves the page still to attach to a completed browser
// call's observer event (BROWSER-FILMSTRIP). A direct browser_take_screenshot
// already carries its persisted URL (directShot); otherwise, after a SUCCESSFUL
// view-changing browser action, it fires the deterministic, model-invisible
// auto-capture — gated by NEO_BROWSER_AUTOSHOT and bounded by the per-turn cap.
// Best-effort throughout: it returns "" (no frame) rather than ever disturbing
// the call's result or the turn. Called only from the single-threaded result-
// assembly paths, so the per-turn counter never races the dispatch goroutines.
func (a *Agent) screenshotForCall(ctx context.Context, name, directShot string, isErr bool) string {
	if directShot != "" {
		return directShot // the call itself returned an image (browser_take_screenshot)
	}
	if isErr || !a.cfg.BrowserAutoshot {
		return ""
	}
	// Nowhere to persist a still → skip cheaply (the test seam bypasses this).
	if a.captureFn == nil && (a.tools == nil || !a.tools.MediaPersistEnabled()) {
		return ""
	}
	if !isViewChangingBrowser(name) {
		return ""
	}
	if a.cfg.BrowserAutoshotMax > 0 && a.turn.autoshotCount >= a.cfg.BrowserAutoshotMax {
		return ""
	}
	url := a.captureViewport(ctx, name)
	if url != "" {
		a.turn.autoshotCount++
	}
	return url
}

// captureViewport fires the browser viewport auto-capture, dispatching to the
// test override when set and otherwise to the tool manager's real MCP-session
// capture.
func (a *Agent) captureViewport(ctx context.Context, sourceFunc string) string {
	if a.captureFn != nil {
		return a.captureFn(ctx, sourceFunc)
	}
	if a.tools == nil {
		return ""
	}
	return a.tools.CaptureViewport(ctx, sourceFunc)
}

// isViewChangingBrowser reports whether a browser tool changed what the page
// shows — a navigation, a back, a click, or a form submit — the moments a fresh
// still makes the filmstrip feel live (BROWSER-FILMSTRIP req.3 ac_1). Read-only
// browser tools (snapshot, console/network reads, waits) and the screenshot
// tool itself are excluded so they neither double-capture nor spend the cap.
func isViewChangingBrowser(name string) bool {
	i := strings.Index(name, "__")
	if i < 0 {
		return false
	}
	switch name[i+2:] {
	case "browser_navigate", "browser_navigate_back", "browser_click",
		"browser_fill_form", "browser_select_option":
		return true
	case "browser_type", "browser_press_key":
		// Typing / a key press is view-changing only when it submits (Enter) —
		// but the page snapshot the action returns already reflects the new
		// state, and a following navigate/click will capture; treat a submit-y
		// key press as view-changing so an Enter-to-search still films.
		return true
	}
	return false
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
	pct := a.usedTokens(system) * 100 / a.cfg.ContextWindowTokens
	if pct < 0 {
		pct = 0
	}
	if pct > 100 {
		pct = 100
	}
	return pct
}

func (a *Agent) usedTokens(system string) int {
	return memory.EstimateTokens(system) + estimateMessagesTokens(a.working) + a.schemaTokens
}

// overSoftBudget / overHardBudget are the trim/compaction trigger reads: the
// window is compared against the headroom-derived token budgets (window minus
// capped headroom), not a raw percentage, so at 1M scale trimming fires only
// near true pressure while small-window overrides keep their prior behavior.
func (a *Agent) overSoftBudget(system string) bool {
	return a.cfg.ContextWindowTokens > 0 && a.usedTokens(system) >= a.cfg.SoftBudgetTokens()
}

func (a *Agent) overHardBudget(system string) bool {
	return a.cfg.ContextWindowTokens > 0 && a.usedTokens(system) >= a.cfg.HardBudgetTokens()
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
