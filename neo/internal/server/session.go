// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"matrix/construct/schema/primitives"
	"matrix/neo/internal/agent"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/recall"
	"matrix/neo/internal/task"
)

// A session is one conversation thread: its own agent loop (transcript +
// summary + goal) over the engine's shared models, tools, pager, and
// consolidator. Turns within a conversation are serialized; distinct
// conversations run concurrently and share one cortex store safely (the goal
// lives on the agent, not the pager).
type session struct {
	id     string // conversation_id
	engine *Engine
	agent  *agent.Agent

	mu  sync.Mutex // serializes turns in this conversation
	cur *run       // the in-flight turn (read by the reporter/observer)

	actMu  sync.Mutex // guards active; held only briefly (never across a turn)
	active *run       // the in-flight run for this conversation (one at a time)

	// inbox holds user messages received WHILE a turn is in flight (F5):
	// mid-task messages the user sent without interrupting. The live agent
	// drains it at each tool-call boundary (agent.Options.Inbox -> drainInput),
	// so a mid-task message is delivered on the agent's next step instead of
	// cancelling the run. Guarded by inboxMu (a distinct, briefly-held lock).
	inboxMu sync.Mutex
	inbox   []string

	gatesMu sync.Mutex
	gates   map[string]chan gateAnswer // node_id -> waiter, for delegated MCL gates

	asksMu sync.Mutex
	asks   map[string]*askWaiter // ask surface id -> waiter, for Construct Ask back-channel
}

// askWaiter parks a construct_render(kind=ask) tool call until the human posts
// a typed response. It carries the originating Ask so the HTTP receiver can
// validate the posted response against its contract before delivering it.
type askWaiter struct {
	ask *primitives.Ask
	ch  chan *primitives.AskResponse
}

// run is a single user turn. id doubles as the SSE topic + the intent_id the
// client subscribes to.
type run struct {
	id       string
	convID   string
	sess     *session
	closed   bool // a closing (final) turn has been emitted
	narrated bool // at least one Status narration turn was persisted this run

	cancel  context.CancelFunc // cancels this turn's ctx (barge-in / explicit stop)
	stopped atomic.Bool        // set when interrupted, so drive closes quietly
}

type gateAnswer struct {
	approved bool
	answer   string
}

type ctxKey int

const runCtxKey ctxKey = iota

func withRun(ctx context.Context, r *run) context.Context {
	return context.WithValue(ctx, runCtxKey, r)
}

func runFromContext(ctx context.Context) *run {
	r, _ := ctx.Value(runCtxKey).(*run)
	return r
}

// sessionRegistry maps conversation_id -> session, minting on first use.
type sessionRegistry struct {
	engine *Engine
	mu     sync.Mutex
	byID   map[string]*session
}

func newSessionRegistry(e *Engine) *sessionRegistry {
	return &sessionRegistry{engine: e, byID: map[string]*session{}}
}

func (sr *sessionRegistry) get(convID string) *session {
	sr.mu.Lock()
	defer sr.mu.Unlock()
	if s, ok := sr.byID[convID]; ok {
		return s
	}
	s := sr.engine.newSession(convID)
	sr.byID[convID] = s
	return s
}

// newSession builds a fresh agent bound to this conversation, with a reporter
// and tool-observer that stream onto whichever run is currently in flight.
func (e *Engine) newSession(convID string) *session {
	s := &session{
		id:     convID,
		engine: e,
		gates:  map[string]chan gateAnswer{},
		asks:   map[string]*askWaiter{},
	}
	s.rebuildAgent()
	return s
}

// rebuildAgent mints a FRESH agent for this conversation over a clean context
// window and seeds it from durable history. It runs when the session is first
// minted AND on every task-supervisor respawn: a hard failure, a stall, or a
// wedged transcript never carries into the next attempt, because the new agent
// starts from durable TEXT history (no prior tool_calls to re-poison a strict
// provider) plus the resume prime and the work already on the volume.
func (s *session) rebuildAgent() {
	e := s.engine
	// Conversational recall lane: relevant PAST turns of this (now unbounded)
	// thread, beyond the live transcript + resume seed. Reuses the pager's
	// embedder so the whole agent shares one embedding model; a disabled store
	// or absent embedder yields a nil recaller (no-op).
	var recaller agent.ConvRecaller
	if e.conv.Enabled() && e.pager != nil {
		if emb := e.pager.Embedder(); emb != nil {
			recaller = recall.New(e.conv, s.id, emb, e.cfg.RecallTopK, e.cfg.RecallBudgetTokens)
		}
	}
	s.agent = agent.New(agent.Options{
		Config:       e.cfg,
		Main:         e.main,
		Cheap:        e.cheap,
		Tools:        e.tools,
		Pager:        e.pager,
		Reporter:     &sseReporter{sess: s},
		Consolidator: e.consolidator,
		Recaller:     recaller,
		Observer:     func(ev agent.ToolEvent) { e.surfaceTool(s.cur, ev) },
		// Cassandra Phase 3: the shared completeness faculty audits the
		// completion gate on state-touching turns; its verdicts stream as
		// cassandra.* events onto the live run.
		Adjudicator:   e.adjudicator,
		AuditObserver: func(ev agent.AuditEvent) { e.publishAudit(s.cur, ev) },
		ConvID:        s.id,
		// F5: non-interrupting mid-task messages. The loop drains this at each
		// tool-call boundary and folds any queued user messages into the
		// transcript, so a message sent mid-task is delivered on the agent's
		// next step instead of cancelling the run.
		Inbox: s.drainInput,
	})
	// Resume continuity: if this conversation already has durable turns (a
	// reopened thread, one that outlived a restart, or a respawn mid-task),
	// seed the fresh agent's transcript so it remembers the thread instead of
	// starting blank.
	if e.conv.Enabled() {
		if turns := e.conv.Recent(s.id, conversation.DefaultRecallTurns); len(turns) > 0 {
			s.agent.Seed(seedMessages(turns), firstUserText(turns))
		}
	}
}

// seedMessages converts durable turns into transcript messages (oldest-first),
// keeping only the user/assistant text turns that prime context.
func seedMessages(turns []conversation.Turn) []llm.Message {
	out := make([]llm.Message, 0, len(turns))
	for _, t := range turns {
		text := strings.TrimSpace(t.Text)
		if text == "" {
			continue
		}
		if t.Role == "assistant" {
			out = append(out, llm.AssistantMessage(text))
		} else {
			out = append(out, llm.UserMessage(text))
		}
	}
	return out
}

func firstUserText(turns []conversation.Turn) string {
	for _, t := range turns {
		if t.Role == "user" && strings.TrimSpace(t.Text) != "" {
			return t.Text
		}
	}
	return ""
}

// submit routes a fresh user message (F5: non-interrupting). If a turn is
// already in flight for this conversation, the message is QUEUED into the live
// agent's inbox — delivered at the agent's NEXT tool-call boundary without
// interrupting the run — and submit returns the active run's id with fresh
// false (the client keeps watching the same live stream). Otherwise it
// dispatches a fresh run and returns its id with fresh true. The active check
// and the dispatch are atomic under actMu so two near-simultaneous messages
// never both spawn a run (the second folds into the first).
func (s *session) submit(message string) (runID string, fresh bool) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if r := s.active; r != nil && !r.stopped.Load() {
		s.enqueueInput(message)
		return r.id, false
	}
	r := s.dispatchLocked(message, false)
	return r.id, true
}

// startResume re-dispatches an orphaned task (the boot reaper after a restart /
// Fly suspend): it drives the original objective with the catch-up prime from
// attempt one, since work may already exist on the volume.
func (s *session) startResume(objective string) *run {
	return s.dispatch(objective, true)
}

// dispatch mints a run and drives it on a background goroutine. It does NOT
// interrupt any in-flight turn (F5 removed barge-in from the message path —
// mid-task messages queue via submit instead; the only deliberate interrupt is
// POST /intents/{id}/stop). Locks actMu so `active` is published synchronously.
func (s *session) dispatch(message string, resume bool) *run {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.dispatchLocked(message, resume)
}

// dispatchLocked is dispatch's body; the caller MUST hold actMu. It records the
// run as the conversation's active run synchronously (so submit's active check
// is race-free), creates the SSE topic before returning, then drives the turn.
func (s *session) dispatchLocked(message string, resume bool) *run {
	r := &run{id: synthRunID(message), convID: s.id, sess: s}
	s.active = r
	s.engine.registerRun(r)
	// Create the SSE topic NOW, before returning the dispatch (and before the
	// background goroutine's first publish). This closes the dispatch→subscribe
	// race: the client connects to /events the moment it has the intent_id, and
	// must find a Neo-owned topic or the request is reverse-proxied to the
	// daemon's empty stream. The replay buffer then backfills any events
	// published between this point and the client's connect.
	s.engine.broker.ensure(r.id)
	go s.drive(r, message, resume)
	return r
}

// enqueueInput appends a mid-run user message to the session inbox (F5). The
// live agent drains it at its next tool-call boundary via drainInput.
func (s *session) enqueueInput(message string) {
	s.inboxMu.Lock()
	s.inbox = append(s.inbox, message)
	s.inboxMu.Unlock()
}

// drainInput returns and clears any queued mid-run messages (F5). It is the
// agent's Inbox hook (drained each tool-call boundary) and is also swept at the
// end of a turn to catch a message that landed in the finishing window. Returns
// nil when the inbox is empty.
func (s *session) drainInput() []string {
	s.inboxMu.Lock()
	defer s.inboxMu.Unlock()
	if len(s.inbox) == 0 {
		return nil
	}
	out := s.inbox
	s.inbox = nil
	return out
}

// drive owns one task's whole lifecycle. The task is DECOUPLED from the HTTP
// request that dispatched it (it runs on context.Background, bounded only by a
// generous wall-clock) — closing the app or dropping the SSE stream never ends
// it. It is recorded in the durable task ledger so even a daemon restart / Fly
// suspend can resume it. The supervisor inside keeps at least one agent on the
// task, respawning across model errors / tool failures / stalls, until the
// objective is met to standard or a hard ceiling is hit. (The Task Durability
// Rule.)
func (s *session) drive(r *run, message string, resume bool) {
	defer s.engine.unregisterRun(r.id)

	wall := s.engine.cfg.TaskMaxWall
	if wall <= 0 {
		wall = 20 * time.Minute
	}
	// The cancel is registered as THIS run's stop handle BEFORE the turn lock,
	// so an explicit POST /intents/{id}/stop can cancel it even while it holds
	// the turn lock. `active` was already published synchronously in dispatch.
	ctx, cancel := context.WithTimeout(context.Background(), wall)
	defer cancel()
	r.cancel = cancel
	defer s.clearActive(r)

	s.mu.Lock()
	s.cur = r
	defer func() {
		s.cur = nil
		s.mu.Unlock()
	}()

	// Record the task durably so a restart/suspend can pick it back up. Begin
	// is keyed by conversation and owned by this run id (a new dispatch
	// supersedes it; a superseded run's later Finish is a CAS no-op).
	s.engine.tasks.Begin(s.id, r.id, message)

	status := s.superviseTask(ctx, r, message, resume)

	// Barge-in / explicit stop: close quietly as "interrupted" — the user moved
	// on; the next turn is taking over. Mark the task interrupted so the reaper
	// never resumes it. (CAS on run id: a no-op if a newer run already owns the
	// conversation's task record.)
	if r.stopped.Load() {
		// The user explicitly stopped; abandon any messages they had queued
		// mid-task (F5) so they don't leak into a later, unrelated turn.
		s.drainInput()
		s.engine.tasks.Finish(s.id, r.id, task.StatusInterrupted)
		if !r.closed {
			s.engine.broker.publish(r.id, "message.complete", "neo", map[string]interface{}{"status": "interrupted"})
			r.closed = true
		}
		s.engine.broker.closeRun(r.id)
		return
	}

	s.engine.tasks.Finish(s.id, r.id, status)
	// Defensive backstop: the supervisor always emits a terminal (Say on a
	// genuine completion, or deliverCeiling at the ceiling), so r.closed is
	// normally true here. Synthesize one only if somehow it isn't, so the
	// client's stream always terminates deterministically.
	if !r.closed {
		s.engine.broker.publish(r.id, "message.complete", "neo", map[string]interface{}{"status": "completed"})
		s.engine.broker.publish(r.id, "chat.assistant", "neo", s.chatFields(r, "Done.", true))
		s.engine.conv.AppendAssistant(s.id, r.id, "Done.")
		r.closed = true
	}
	s.engine.broker.closeRun(r.id)

	// F5: a mid-task message can land in the tiny window AFTER the agent's
	// final inbox drain but BEFORE this run closed — the agent's loop has
	// already ended, so it will never see it. Deliver it as a fresh
	// continuation run rather than stranding it. The deferred clearActive(r)
	// still points at r, so this dispatch atomically takes over as the new
	// active run (clearActive then no-ops).
	if leftover := s.drainInput(); len(leftover) > 0 {
		s.dispatch(strings.Join(leftover, "\n\n"), false)
	}
}

// superviseTask keeps at least one agent on the task until it is genuinely
// complete (the agent's own completion gate accepts and it Says its answer), is
// interrupted by the user, or hits a hard ceiling. Every non-clean exit — a
// model/transport error, a no-progress stall, an exhausted step budget, a
// stuck attempt that timed out — is treated as "not done": it checkpoints, tells
// the user it is still working (never a fake "done"), backs off, respawns a
// FRESH agent over durable state, and goes again.
func (s *session) superviseTask(ctx context.Context, r *run, objective string, resume bool) task.Status {
	cfg := s.engine.cfg
	maxRespawns := cfg.TaskMaxRespawns
	if maxRespawns < 0 || !cfg.SuperviseTasks {
		// Supervisor off: a single attempt, then an honest partial — never a
		// fabricated "done" and never a bare "failed".
		maxRespawns = 0
	}

	for attempt := 1; ; attempt++ {
		// A fresh first dispatch runs the user's message verbatim (the session
		// is already seeded from durable history WITHOUT it). A reaper resume,
		// or any respawn, uses the catch-up prime over the rebuilt agent.
		prompt := objective
		if resume || attempt > 1 {
			prompt = resumePrime(objective, attempt)
		}

		actx, acancel := s.attemptContext(ctx)
		err := s.agent.Chat(withRun(actx, r), prompt)
		acancel()

		// The agent carries the shared failure class of this attempt's most
		// recent classified tool failure, so the supervisor reads the SAME
		// taxonomy the dispatch ladder used (NE-5) rather than re-guessing.
		failClass := s.agent.LastFailureClass()

		switch superviseDecision(r.stopped.Load(), err, failClass, ctx.Err(), attempt, maxRespawns) {
		case actInterrupted:
			// User stop / barge-in: drive() emits the interrupted terminal.
			return task.StatusInterrupted
		case actDone:
			// Genuine completion: the agent's Say already emitted the terminal.
			return task.StatusDone
		case actStop:
			// Deterministic blocker: a fresh agent would hit the same wall.
			// Stop-and-ask honestly instead of burning the respawn budget on a
			// loop the user already saw fail.
			return s.deliverDeterministicStop(r)
		case actCeiling:
			if ctx.Err() != nil {
				return s.deliverCeiling(r, "reached the time limit I had for this task")
			}
			return s.deliverCeiling(r, "couldn't fully verify it was done after several attempts")
		}

		// actRespawn — not done, keep going. Checkpoint, reassure the user (no
		// fake done), back off with jitter, then respawn a fresh agent over
		// durable state.
		s.engine.tasks.Checkpoint(s.id, r.id, attempt+1, friendlyErr(err))
		s.emitProgress(r, attempt, err)
		if !superviseBackoff(ctx, attempt) {
			if r.stopped.Load() {
				return task.StatusInterrupted
			}
			return s.deliverCeiling(r, "reached the time limit I had for this task")
		}
		s.rebuildAgent()
	}
}

// superviseAction is the policy outcome for one finished supervised attempt.
type superviseAction int

const (
	actRespawn     superviseAction = iota // not done; rebuild a fresh agent and try again
	actDone                               // genuine completion (the agent finished + Said its answer)
	actInterrupted                        // user stop / barge-in
	actCeiling                            // hard ceiling reached — deliver an honest partial
	actStop                               // deterministic blocker — stop-and-ask, no respawn
)

// superviseDecision is the PURE policy for one finished attempt, given whether
// the run was stopped, the attempt's error (nil = genuine completion), the
// shared failure class of the attempt's most recent classified tool failure,
// the task context's error (non-nil = wall-clock blown), the attempt number,
// and the respawn budget. Order matters: a user stop wins over everything; a
// clean finish is done; a DETERMINISTIC blocker stops-and-asks (a fresh agent
// would hit the same wall, so don't respawn or consume the budget); a blown
// wall-clock or an exhausted respawn budget hits the ceiling; otherwise
// respawn. Every other non-clean exit (model/transport error, a stall, an
// exhausted step budget, a timed-out attempt, or a transient/conflict/pending
// failure) flows to respawn until the ceiling.
func superviseDecision(stopped bool, attemptErr error, failClass delegate.FailureClass, taskCtxErr error, attempt, maxRespawns int) superviseAction {
	switch {
	case stopped:
		return actInterrupted
	case attemptErr == nil:
		return actDone
	case failClass == delegate.ClassDeterministic:
		return actStop
	case taskCtxErr != nil:
		return actCeiling
	case attempt > maxRespawns:
		return actCeiling
	default:
		return actRespawn
	}
}

// attemptContext bounds a SINGLE supervised attempt so one stuck agent run
// can't consume the whole task wall-clock; the parent (task) context still
// governs the overall ceiling and barge-in cancellation.
func (s *session) attemptContext(parent context.Context) (context.Context, context.CancelFunc) {
	if t := s.engine.cfg.TaskAttemptTimeout; t > 0 {
		return context.WithTimeout(parent, t)
	}
	return context.WithCancel(parent)
}

// resumePrime is the catch-up instruction for a respawned / resumed agent: the
// objective is unchanged, prior work may already be on the volume, build on it,
// and keep going until it is genuinely done. After a few stuck attempts it also
// nudges decomposition / delegation.
func resumePrime(objective string, attempt int) string {
	var b strings.Builder
	b.WriteString("[continue this task] A previous attempt was interrupted before the task was finished. Your objective is unchanged:\n\n")
	b.WriteString(strings.TrimSpace(objective))
	b.WriteString("\n\nWork from a previous attempt may already exist — check your workspace (list/read the relevant files, re-run a quick status check) and BUILD ON what is already done instead of restarting from scratch. Keep going until the objective is fully achieved to a high standard, then call task_complete with honest coverage and the real evidence behind it.")
	if attempt >= 3 {
		b.WriteString(" If you keep getting stuck on one piece, break it into smaller concrete steps, or delegate parallel parts with spawn_subagents.")
	}
	return b.String()
}

// emitProgress tells the user the task is STILL IN PROGRESS after a failed
// attempt — honest and non-terminal, never a fake completion. The machine
// detail (the real neo/llm error) goes to the logs for diagnosis.
func (s *session) emitProgress(r *run, attempt int, err error) {
	msg := "Still on it — that pass hit a snag, so I'm taking another run at it."
	s.engine.broker.publish(r.id, "chat.assistant", "neo", s.chatFields(r, msg, false))
	fmt.Fprintf(os.Stderr, "neo/supervisor: conv=%s run=%s attempt=%d not done yet: %v\n", s.id, r.id, attempt, err)
}

// deliverCeiling emits the ONE allowed terminal-without-completion: an honest
// partial at the hard ceiling. It hands the user the best real work so far
// (never fabricated) and invites them to continue. Idempotent if already closed.
func (s *session) deliverCeiling(r *run, reason string) task.Status {
	if r.closed {
		return task.StatusCeiling
	}
	best := strings.TrimSpace(s.agent.BestEffort())
	text := "I've been working hard on this and made real progress, but I " + reason +
		", and I won't hand you something I can't stand behind."
	if best != "" {
		text += " Here's exactly where it stands:\n\n" + best
	}
	text += "\n\nTell me how you'd like me to continue and I'll pick it right back up."
	s.engine.broker.publish(r.id, "message.complete", "neo", map[string]interface{}{"status": "completed"})
	s.engine.broker.publish(r.id, "chat.assistant", "neo", s.chatFields(r, text, true))
	s.engine.conv.AppendAssistant(s.id, r.id, text)
	r.closed = true
	return task.StatusCeiling
}

// deliverDeterministicStop emits the terminal for a task blocked by a
// DETERMINISTIC failure — one that recurs identically (a denied permission, a
// limit, an invalid request, a detail only the user can change). A fresh agent
// would hit the same wall, so the supervisor does NOT respawn and does NOT
// consume the respawn budget. Unlike the generic "still on it, taking another
// run at it" progress copy, this states plainly that the task is blocked and
// hands the user the next step, with the best real work so far. Idempotent if
// already closed.
func (s *session) deliverDeterministicStop(r *run) task.Status {
	if r.closed {
		return task.StatusCeiling
	}
	best := strings.TrimSpace(s.agent.BestEffort())
	text := "I couldn't complete this — I ran into something I can't get past on my own (a permission, a limit, or a detail that needs to change), and trying the same thing again wouldn't help, so I stopped rather than spin on it."
	if best != "" {
		text += " Here's where it got to:\n\n" + best
	}
	text += "\n\nTell me how you'd like to adjust it and I'll pick it right back up."
	s.engine.broker.publish(r.id, "message.complete", "neo", map[string]interface{}{"status": "completed"})
	s.engine.broker.publish(r.id, "chat.assistant", "neo", s.chatFields(r, text, true))
	s.engine.conv.AppendAssistant(s.id, r.id, text)
	r.closed = true
	return task.StatusCeiling
}

// superviseBackoff sleeps an attempt-scaled, jittered interval before a
// respawn, honoring task cancellation. Jitter avoids a synchronized retry
// stampede when many users' daemons hit the same upstream hiccup at once.
func superviseBackoff(ctx context.Context, attempt int) bool {
	base := time.Duration(attempt) * 750 * time.Millisecond
	if base > 8*time.Second {
		base = 8 * time.Second
	}
	d := base + time.Duration(rand.Int63n(int64(500*time.Millisecond)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// setActive / clearActive / interruptActive coordinate barge-in. active is the
// latest dispatched run; interruptActive cancels it (LIFO), so each new message
// supersedes the one before whether it is running or still queued. clearActive
// only nils active when it still points at r, so a turn that has already been
// superseded never clears its successor.
func (s *session) setActive(r *run) {
	s.actMu.Lock()
	s.active = r
	s.actMu.Unlock()
}

func (s *session) clearActive(r *run) {
	s.actMu.Lock()
	if s.active == r {
		s.active = nil
	}
	s.actMu.Unlock()
}

// interruptActive stops the latest dispatched turn (if any). Returns its id, or
// "" when nothing was in flight.
func (s *session) interruptActive() string {
	s.actMu.Lock()
	r := s.active
	s.actMu.Unlock()
	s.interrupt(r)
	if r == nil {
		return ""
	}
	return r.id
}

// interrupt marks a specific run stopped and cancels its context, so its agent
// loop unwinds at the next cancellation checkpoint and drive closes it quietly.
// Safe on a nil run or a run whose cancel is not yet set.
func (s *session) interrupt(r *run) {
	if r == nil {
		return
	}
	r.stopped.Store(true)
	if r.cancel != nil {
		r.cancel()
	}
}

func (s *session) chatFields(r *run, text string, final bool) map[string]interface{} {
	f := map[string]interface{}{
		"role":            "assistant",
		"text":            strings.TrimSpace(text),
		"conversation_id": s.id,
		"intent_id":       r.id,
	}
	if final {
		f["final"] = true
	}
	return f
}

// --- gate waiters (delegated MCL approval gates) ---

func (s *session) registerGate(nodeID string) chan gateAnswer {
	ch := make(chan gateAnswer, 1)
	s.gatesMu.Lock()
	s.gates[nodeID] = ch
	s.gatesMu.Unlock()
	return ch
}

func (s *session) answerGate(nodeID string, a gateAnswer) bool {
	s.gatesMu.Lock()
	ch, ok := s.gates[nodeID]
	if ok {
		delete(s.gates, nodeID)
	}
	s.gatesMu.Unlock()
	if !ok {
		return false
	}
	select {
	case ch <- a:
	default:
	}
	return true
}

func (s *session) clearGate(nodeID string) {
	s.gatesMu.Lock()
	delete(s.gates, nodeID)
	s.gatesMu.Unlock()
}

// --- ask waiters (Construct Ask back-channel) ---
//
// Mirrors the gate waiters: a construct_render(kind=ask) tool call registers a
// waiter keyed by the Ask surface id and parks on the channel; the HTTP
// receiver (POST /intents/{id}/asks/{ask_id}/answer) validates the posted
// response and delivers it. The answer re-enters the agent as the tool result
// — an INPUT on the same footing as a user message, never a plan/walk/cortex
// mutation.

func (s *session) registerAsk(askID string, ask *primitives.Ask) <-chan *primitives.AskResponse {
	w := &askWaiter{ask: ask, ch: make(chan *primitives.AskResponse, 1)}
	s.asksMu.Lock()
	s.asks[askID] = w
	s.asksMu.Unlock()
	return w.ch
}

// pendingAsk returns the Ask a waiter is parked on, so the receiver can
// validate a posted response against its contract before delivering it.
func (s *session) pendingAsk(askID string) (*primitives.Ask, bool) {
	s.asksMu.Lock()
	defer s.asksMu.Unlock()
	w, ok := s.asks[askID]
	if !ok {
		return nil, false
	}
	return w.ask, true
}

// answerAsk delivers a validated response to the parked tool call and retires
// the waiter. Reports whether a waiter was actually waiting (so a stale or
// duplicate post is rejected).
func (s *session) answerAsk(askID string, resp *primitives.AskResponse) bool {
	s.asksMu.Lock()
	w, ok := s.asks[askID]
	if ok {
		delete(s.asks, askID)
	}
	s.asksMu.Unlock()
	if !ok {
		return false
	}
	select {
	case w.ch <- resp:
	default:
	}
	return true
}

func (s *session) clearAsk(askID string) {
	s.asksMu.Lock()
	delete(s.asks, askID)
	s.asksMu.Unlock()
}

// sseReporter maps the agent's Reporter calls onto the conversation's event
// stream. Say is always the closing turn (the agent only Says to end), so it
// emits the terminal sequence; Status is progress; Notice is a visible spoken
// promise (compaction / escalation).
type sseReporter struct {
	sess *session
}

func (r *sseReporter) Say(text string, completion bool) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	fields := s.chatFields(run, text, true)
	if completion {
		// Mark the validated task_complete summary so the client keeps it out of
		// the thread — it is a redundant recap on top of the narration the user
		// already read. It is STILL sent here, and message.complete still fires,
		// so Cassandra's accepted completion deterministically closes the task.
		fields["completion"] = true
	}
	s.engine.broker.publish(run.id, "message.complete", "neo", map[string]interface{}{"status": "completed"})
	s.engine.broker.publish(run.id, "chat.assistant", "neo", fields)
	// Persist conversational / ceiling answers (the durable thread content). The
	// task_complete completion summary is normally NOT persisted: Neo's narration
	// (Status) is the durable thread now, so persisting the summary too would
	// re-surface it as a duplicate closing bubble on reopen. The exception is a
	// run that committed NO narration at all (e.g. it went straight to
	// task_complete) — persisting the summary there is what keeps the reopened
	// thread from being empty, mirroring the client's live safety net.
	if !completion || !run.narrated {
		s.engine.conv.AppendAssistant(s.id, run.id, text)
	}
	run.closed = true
}

func (r *sseReporter) Status(text string) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	// Tool-start markers ("• <tool>") are now driven by the tool observer,
	// which paints a rich animated workspace step (terminal / browser / editor)
	// from the call's real arguments — so we drop the bare marker here. Only
	// genuine model narration becomes an assistant turn.
	if strings.HasPrefix(text, "• ") {
		return
	}
	s.engine.broker.publish(run.id, "chat.assistant", "neo", s.chatFields(run, text, false))
	// Persist genuine model narration: it is the durable thread content now (the
	// task_complete summary is no longer persisted), so Neo's running commentary
	// survives a reopen instead of vanishing the moment the run settles.
	s.engine.conv.AppendAssistant(s.id, run.id, text)
	run.narrated = true
}

func (r *sseReporter) Notice(text string) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	s.engine.broker.publish(run.id, "chat.assistant", "neo", s.chatFields(run, text, false))
}

// Think surfaces a glimpse of the model's reasoning as a dedicated, secondary
// "thinking" channel. It is NEVER persisted to the durable thread (unlike Say)
// and carries no role — the client renders it as a dismissible thought, not an
// answer turn.
func (r *sseReporter) Think(text string) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return
	}
	s.engine.broker.publish(run.id, "chat.thinking", "neo", map[string]interface{}{
		"intent_id":       run.id,
		"conversation_id": s.id,
		"text":            text,
	})
}

// Delta streams an incremental fragment of the CURRENT turn as it generates —
// the live "typing" channel. channel is "content" (the visible answer being
// typed) or "reasoning" (the live thinking). turn is the agent-loop step index
// so the client can reset its per-channel buffer when a new turn begins. It is
// NEVER persisted (the durable thread is written from Say); the authoritative
// final text always follows as a chat.assistant turn that the client commits.
func (r *sseReporter) Delta(turn int, channel, text string) {
	if text == "" {
		return
	}
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	s.engine.broker.publish(run.id, "chat.delta", "neo", map[string]interface{}{
		"intent_id":       run.id,
		"conversation_id": s.id,
		"channel":         channel,
		"turn":            turn,
		"text":            text,
	})
}

func synthRunID(seed string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("neo|%d|%s", time.Now().UnixNano(), seed)))
	return "neo_" + hex.EncodeToString(h[:10])
}

func synthConvID(seed string) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("conv|%d|%s", time.Now().UnixNano(), seed)))
	return "conv_" + hex.EncodeToString(h[:10])
}

func friendlyErr(err error) string {
	if err == nil {
		return ""
	}
	m := err.Error()
	// Never surface raw provider/transport detail (JSON error bodies, HTTP
	// status lines, internal package paths) to the user. Map the known failure
	// shapes to a plain, honest sentence; the machine detail stays in the logs.
	switch {
	case strings.Contains(m, "neo/llm:") || strings.Contains(m, "model call failed") || strings.Contains(m, "provider rejected"):
		return "I had trouble reaching my model just now and couldn't finish that. Give it another go in a moment."
	case strings.Contains(m, "context deadline") || strings.Contains(m, "timeout") || strings.Contains(m, "deadline exceeded"):
		return "that ran longer than I could wait and timed out before I finished. Try again, or let's narrow the task."
	}
	// Unknown error: trim to the leading human-readable sentence and drop any
	// structured noise (a '{' JSON body or a newline-delimited stack tail).
	if i := strings.IndexAny(m, "{\n"); i > 0 {
		m = strings.TrimSpace(m[:i])
	}
	if len(m) > 160 {
		m = m[:160] + "…"
	}
	return m
}
