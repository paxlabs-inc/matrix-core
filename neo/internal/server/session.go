// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"matrix/construct/schema/primitives"
	"matrix/neo/internal/agent"
	"matrix/neo/internal/config"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/recall"
	"matrix/neo/internal/runrecord"
	"matrix/neo/internal/sessionjournal"
	"matrix/neo/internal/task"
)

// A session is one conversation thread: its own agent loop (transcript +
// summary + goal) over the engine's shared models, tools, pager, and
// consolidator. Turns within a conversation are serialized; distinct
// conversations run concurrently and share one Neocortex store safely (the goal
// lives on the agent, not the pager).
type session struct {
	id     string // conversation_id
	engine *Engine
	agent  *agent.Agent

	// automatrix marks this as an ephemeral session driving an autonomous
	// Automatrix opportunity (task 4.1). It changes exactly two things: the
	// agent is built on the RESTRICTED (no-money) tool surface via
	// agent.NewAutomatrix (so a proactive run physically cannot reach the
	// signing path, req 3.2), and the run is driven by the quiet
	// superviseAutomatrixTask loop (no SSE/broker surfacing — req 5.5 surfaces
	// NOTHING; the surprise result is delivered out of band by task 5.3). A
	// normal session leaves this false and is byte-identical to before.
	automatrix bool

	// automatrixOut is the quiet capture reporter for an Automatrix run (set
	// only when automatrix is true). It surfaces NOTHING to the user (req 5.5)
	// but retains the agent's final user-facing answer so the completion path
	// (task 5.3) can use it as the result-not-protocol summary of what Neo
	// produced. Reused across supervisor respawns so the last attempt's answer
	// is the captured result.
	automatrixOut *automatrixReporter

	// brief marks this as an ephemeral session driving an autonomous
	// MORNING_BRIEF run (ORACLE task 5.4). Like automatrix it surfaces NOTHING
	// live and reuses automatrixOut for quiet result capture, but its agent is
	// built on the TIGHTER read-only brief surface (agent.NewMorningBrief:
	// web_news/web_search/fetch + memory_recall only — money/signing/fs/deploy/
	// exec structurally absent, req 15.1) rather than the automatrix restricted
	// surface. A normal session leaves this false.
	brief bool

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
	id             string
	convID         string
	idempotencyKey string
	sess           *session
	closed         bool // a closing (final) turn has been emitted
	narrated       bool // accepted streamed assistant content was committed this run
	// lastText is the most recent DURABLE assistant text shown to the user this
	// run (an accepted streamed answer or a Say). The ceiling / deterministic-stop
	// closing turns compare BestEffort against it so they never re-paste an
	// answer that is already the last bubble on screen (the double-render).
	lastText string
	started  time.Time

	cancel  context.CancelFunc // cancels this turn's ctx (barge-in / explicit stop)
	stopped atomic.Bool        // set when interrupted, so drive closes quietly

	// outageNotified is set once the user has been told the run is still working
	// through a provider OUTAGE streak (an unreachable/rate-limited upstream), so
	// the reassurance bubble is emitted ONCE per streak instead of on every
	// futile respawn — a down gateway must not spam a fresh "still working"
	// message every few seconds. Cleared the moment an attempt fails for any
	// other reason (real, distinct progress the user should see).
	outageNotified bool

	// onFinish, when set, is invoked exactly once by drive with the run's
	// terminal task status, after the durable ledger Finish. Used by the
	// Automatrix approve path to settle the opportunity that dispatched this
	// run (done -> completion record; failed -> re-pend). Best-effort observer;
	// it must not block.
	onFinish func(task.Status)
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
// starts from the exact session journal when enabled, with the text-only
// conversation store retained as a degraded fallback.
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
	opts := agent.Options{
		Config:  e.cfg,
		Main:    e.main,
		Cheap:   e.cheap,
		Tools:   e.tools,
		Pager:   e.pager,
		Runtime: e.runtime,
		Journal: e.journal,
		// Epistemic-core req.2: the resolved capability surface renders
		// resident in the agent's byte-stable prefix.
		Capability:   e.capabilitySurface(context.Background()),
		Reporter:     &sseReporter{sess: s},
		Consolidator: e.consolidator,
		Recaller:     recaller,
		Observer:     func(ev agent.ToolEvent) { e.surfaceTool(s.cur, ev) },
		// Cassandra 2.0: the silent-voice controller streams its cassandra.mod
		// audit events onto the live run via the AuditObserver side-channel.
		AuditObserver: func(ev agent.AuditEvent) { e.publishAudit(s.cur, ev) },
		// Continuous-memory task 7.1: surface the memory Neo carries (durable
		// story-so-far + coarse timeline) onto the live run as a memory.activation
		// event, so the client can show the RESULT (not the protocol).
		MemoryObserver: func(ev agent.MemoryEvent) { e.publishMemory(s.cur, ev) },
		ConvID:         s.id,
		// F5: non-interrupting mid-task messages. The loop drains this at each
		// tool-call boundary and folds any queued user messages into the
		// transcript, so a message sent mid-task is delivered on the agent's
		// next step instead of cancelling the run.
		Inbox: s.drainInput,
	}
	// An Automatrix session runs on the RESTRICTED (no-money) tool surface so a
	// proactive run physically cannot reach the signing path (req 3.2). The
	// restriction is a property of the constructor — NewAutomatrix forces it —
	// and it holds across every supervisor respawn because rebuildAgent is the
	// single place an agent is minted for this session.
	switch {
	case s.brief:
		// A morning-brief session runs on the TIGHT read-only information
		// surface (agent.NewMorningBrief: web_news/web_search/fetch +
		// memory_recall only; money/signing/fs/deploy/exec + core_execute
		// structurally absent, req 15.1). It reuses the quiet capture reporter
		// so nothing surfaces live (req 15.2 delivers out of band) while the
		// composed brief is retained for durable persistence + notify.
		if s.automatrixOut == nil {
			s.automatrixOut = &automatrixReporter{}
		}
		opts.Reporter = s.automatrixOut
		s.agent = agent.NewMorningBrief(opts)
	case s.automatrix:
		// Quiet capture reporter: surfaces nothing (req 5.5) but retains the
		// final answer for the completion ping (task 5.3). Reused across
		// respawns so the captured result is the last (successful) attempt's.
		if s.automatrixOut == nil {
			s.automatrixOut = &automatrixReporter{}
		}
		opts.Reporter = s.automatrixOut
		s.agent = agent.NewAutomatrix(opts)
	case e.conv.IsInterview(s.id):
		// A personalization-interview conversation (ORACLE task 5.3): the
		// normal live surface plus the interview charter and the confirmation-
		// gated save_personalization_profile tool. Writeback extraction is
		// STRUCTURALLY excluded — no consolidator is passed, so individual
		// interview answers can never become fragmented inferred memories
		// (req 12.3). A repeat interview re-enters with the saved answers.
		opts.Consolidator = nil
		existing := ""
		if e.pager != nil {
			if prof, _, ok, err := e.pager.PersonalizationProfile(context.Background()); err == nil && ok {
				existing = prof.RenderForInterview()
			}
		}
		s.agent = agent.NewInterview(opts, existing)
	default:
		s.agent = agent.New(opts)
	}
	// Inject the onboarding profile (agent_name, preferred_name,
	// expertise_domains) into the stable system prompt prefix (req 2.4/2.5).
	// Refresh first (TTL-bounded) so a Settings edit reflects on subsequent
	// conversations/respawns without a process restart (req 8.2); the
	// values are per-user-stable, preserving the prompt-cache invariant.
	e.maybeRefreshProfile()
	agentName, preferredName, expertiseDomains := e.profileSnapshot()
	s.agent.SetUserProfile(agentName, preferredName, expertiseDomains)
	// A user-requested recenter starts a visually empty conversation but carries
	// one sealed, bounded handoff from the immediately prior thread. Inject it
	// before the first real user turn so the latest request and recent context
	// remain primary without copying old turns into the new chat surface.
	if _, handoff := e.conv.RecoveryHandoff(s.id); handoff != "" {
		s.agent.SetRecoveryHandoff(handoff)
	}
	// Coding-workspace context (NEO-WORKBENCH): tell the agent where the
	// workspace root is and which project this conversation is tagged to, so
	// its file writes land where the workbench actually looks. An unknown or
	// absent tag resolves to the synthesized default (the bare root); a
	// daemon with no workspace configured injects nothing.
	if e.workspaceRoot != "" {
		proj, err := e.resolveProjectRecord(e.conv.Project(s.id))
		if err != nil {
			proj, err = e.resolveProjectRecord("")
		}
		if err == nil {
			s.agent.SetWorkspace(e.workspaceRoot, proj.ID, proj.Name, proj.Root)
		}
	}
	seededFromJournal := false
	if e.cfg.SessionExactProjection && e.journal != nil {
		if _, _, err := e.journal.FinalizeTail(context.Background(), s.id, "restart or supervisor rebuild finalized an abnormal protocol tail"); err != nil {
			log.Printf("neo/session: finalize journal tail for %s: %v", s.id, err)
		} else if replay, err := e.journal.Replay(context.Background(), s.id); err != nil {
			log.Printf("neo/session: replay journal for %s: %v", s.id, err)
		} else if seeded, err := s.agent.SeedReplay(replay); err != nil {
			log.Printf("neo/session: reconstruct journal for %s: %v", s.id, err)
		} else {
			seededFromJournal = seeded
		}
	}
	// Resume continuity fallback: conversations without journal history retain
	// the existing bounded role+text seed.
	if !seededFromJournal && e.conv.Enabled() {
		if turns := e.conv.Recent(s.id, conversation.DefaultRecallTurns); len(turns) > 0 {
			// On a supervisor respawn, the durable history already contains the
			// CURRENT run's own persisted narration ("Still on it…", status
			// lines, partial answers). Seeding the successor with its own
			// narration primes it to re-derive — and re-emit — the same turns
			// verbatim on the same run id with fresh seqs, which the client
			// cannot dedup (it keys on intent_id:seq). Drop this run's
			// assistant turns from the seed; the user's turns (including
			// mid-run messages) are kept so nothing the user said is lost.
			turns = dropRunNarration(turns, s.cur)
			s.agent.Seed(seedMessages(turns), firstUserText(turns))
		}
	}
}

// dropRunNarration filters a respawn seed: assistant turns persisted by the
// run r itself are removed (they are this task's own in-flight narration, not
// prior-thread history), while every user turn and every turn from earlier
// runs is kept. A nil run (first mint, no task in flight) filters nothing.
func dropRunNarration(turns []conversation.Turn, r *run) []conversation.Turn {
	if r == nil {
		return turns
	}
	out := make([]conversation.Turn, 0, len(turns))
	for _, t := range turns {
		if t.Role == "assistant" && t.IntentID == r.id {
			continue
		}
		out = append(out, t)
	}
	return out
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
	runID, fresh, _, _ = s.submitIdempotent(message, "", nil)
	return runID, fresh
}

func (s *session) submitAudio(message string, audio *agent.AudioTurn) (runID string, fresh bool) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	if r := s.active; r != nil && !r.stopped.Load() {
		s.enqueueInput(message)
		s.engine.conv.AppendUser(s.id, r.id, message)
		return r.id, false
	}
	r := s.dispatchLocked(message, false, nil, audio)
	return r.id, true
}

// submitWith is submit with a terminal observer: when the message dispatches a
// FRESH run, onFinish is attached to it (invoked once with the run's terminal
// task status). When the message is queued into an already-live run, no hook is
// attached (the live run belongs to other work) and fresh=false tells the
// caller so.
func (s *session) submitWith(message string, onFinish func(task.Status)) (runID string, fresh bool) {
	runID, fresh, _, _ = s.submitIdempotent(message, "", onFinish)
	return runID, fresh
}

func (s *session) submitIdempotent(message, key string, onFinish func(task.Status)) (runID string, fresh, duplicate bool, err error) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	key = strings.TrimSpace(key)
	if key != "" && s.engine.runRecords != nil && s.engine.runRecords.Enabled() {
		if rec, ok, findErr := s.engine.runRecords.FindByIdempotency(key); findErr != nil {
			return "", false, false, findErr
		} else if ok {
			if rec.ConversationID != s.id || rec.Request != strings.TrimSpace(message) {
				return rec.IntentID, false, true, runrecord.ErrIdempotencyConflict
			}
			return rec.IntentID, false, true, nil
		}
	}
	if r := s.active; r != nil && !r.stopped.Load() {
		s.enqueueInput(message)
		return r.id, false, false, nil
	}
	r, created, dispatchErr := s.dispatchLockedAs(message, false, onFinish, nil, "", key)
	if dispatchErr != nil {
		return "", false, false, dispatchErr
	}
	if !created {
		return r.id, false, true, nil
	}
	return r.id, true, false, nil
}

// submitSystemWake dispatches a durable internal wake only while the
// conversation is idle. Unlike a user message it never joins the live inbox:
// the durable Build wake outbox retries later, keeping unrelated user work and
// background completion narration as separate turns.
func (s *session) submitSystemWake(message, key string) (runID string, fresh, duplicate, busy bool, err error) {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	key = strings.TrimSpace(key)
	if key != "" && s.engine.runRecords != nil && s.engine.runRecords.Enabled() {
		if rec, ok, findErr := s.engine.runRecords.FindByIdempotency(key); findErr != nil {
			return "", false, false, false, findErr
		} else if ok {
			if rec.ConversationID != s.id || rec.Request != strings.TrimSpace(message) {
				return rec.IntentID, false, true, false, runrecord.ErrIdempotencyConflict
			}
			return rec.IntentID, false, true, false, nil
		}
	}
	if r := s.active; r != nil && !r.stopped.Load() {
		return r.id, false, false, true, nil
	}
	r, created, dispatchErr := s.dispatchLockedAs(message, false, nil, nil, "", key)
	if dispatchErr != nil {
		return "", false, false, false, dispatchErr
	}
	if !created {
		return r.id, false, true, false, nil
	}
	return r.id, true, false, false, nil
}

// startResume re-dispatches an orphaned task (the boot reaper after a restart /
// Fly suspend): it drives the original objective with the catch-up prime from
// attempt one, since work may already exist on the volume.
func (s *session) startResume(runID, objective string) *run {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	r, _, _ := s.dispatchLockedAs(objective, true, nil, nil, runID, "")
	return r
}

// dispatch mints a run and drives it on a background goroutine. It does NOT
// interrupt any in-flight turn (F5 removed barge-in from the message path —
// mid-task messages queue via submit instead; the only deliberate interrupt is
// POST /intents/{id}/stop). Locks actMu so `active` is published synchronously.
func (s *session) dispatch(message string, resume bool) *run {
	s.actMu.Lock()
	defer s.actMu.Unlock()
	return s.dispatchLocked(message, resume, nil, nil)
}

// dispatchLocked is dispatch's body; the caller MUST hold actMu. It records the
// run as the conversation's active run synchronously (so submit's active check
// is race-free), creates the SSE topic before returning, then drives the turn.
func (s *session) dispatchLocked(message string, resume bool, onFinish func(task.Status), audio *agent.AudioTurn) *run {
	r, _, _ := s.dispatchLockedAs(message, resume, onFinish, audio, "", "")
	return r
}

func (s *session) dispatchLockedAs(message string, resume bool, onFinish func(task.Status), audio *agent.AudioTurn, forcedRunID, idempotencyKey string) (*run, bool, error) {
	runID := strings.TrimSpace(forcedRunID)
	if runID == "" {
		runID = synthRunID(message)
	}
	created := true
	if s.engine.runRecords != nil && s.engine.runRecords.Enabled() {
		rec, made, err := s.engine.runRecords.Begin(runID, s.id, idempotencyKey, message)
		if err != nil {
			return nil, false, err
		}
		if !made && !resume {
			return &run{id: rec.IntentID, convID: rec.ConversationID, idempotencyKey: rec.IdempotencyKey, sess: s}, false, nil
		}
		if rec.Terminal() {
			return &run{id: rec.IntentID, convID: rec.ConversationID, idempotencyKey: rec.IdempotencyKey, sess: s, closed: true}, false, nil
		}
		created = made
	}
	r := &run{id: runID, convID: s.id, idempotencyKey: strings.TrimSpace(idempotencyKey), sess: s, onFinish: onFinish, started: time.Now().UTC()}
	if audio != nil {
		audio.OnTranscript = func(text string) { s.engine.conv.AppendUser(s.id, r.id, text) }
	}
	// Mint the task context HERE, before the drive goroutine exists: r.cancel
	// is then published under actMu together with `active`, so interrupt()
	// (which reads the run via actMu) always sees a set cancel — no window
	// where a barge-in races drive's own late assignment.
	wall := s.engine.cfg.TaskMaxWall
	if wall <= 0 {
		wall = 20 * time.Minute
	}
	ctx, cancel := context.WithTimeout(context.Background(), wall)
	r.cancel = cancel
	s.active = r
	s.engine.registerRun(r)
	// Create the SSE topic NOW, before returning the dispatch (and before the
	// background goroutine's first publish). This closes the dispatch→subscribe
	// race: the client connects to /events the moment it has the intent_id, and
	// must find a Neo-owned topic or the request is reverse-proxied to the
	// daemon's empty stream. The replay buffer then backfills any events
	// published between this point and the client's connect.
	s.engine.broker.ensure(r.id)
	s.engine.logLifecycle("run.dispatch", r.id, r.convID, "running", 0, nil)
	go s.drive(ctx, r, message, resume, audio)
	return r, created, nil
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
func (s *session) drive(ctx context.Context, r *run, message string, resume bool, audio *agent.AudioTurn) {
	defer s.engine.unregisterRun(r.id)

	// The wall-clock-bounded ctx and r.cancel were minted in dispatchLocked
	// (published under actMu together with `active`), so an explicit
	// POST /intents/{id}/stop can cancel this run from its very first moment
	// with no unsynchronized window. This defer releases the timer.
	defer r.cancel()
	defer s.clearActive(r)

	// Panic backstop: a panic anywhere in the supervised turn must never
	// strand the client on a stream with no terminal (an open SSE topic the
	// UI waits on forever). Recover, mark the task honestly, and emit a real
	// closing turn + terminal so the stream always settles deterministically.
	defer func() {
		p := recover()
		if p == nil {
			return
		}
		fmt.Fprintf(os.Stderr, "neo/drive: conv=%s run=%s recovered panic: %v\n", s.id, r.id, p)
		s.engine.tasks.Finish(s.id, r.id, task.StatusCeiling)
		if r.onFinish != nil {
			r.onFinish(task.StatusCeiling)
		}
		if !r.closed {
			text := "Something went wrong on my end in the middle of this — I had to stop. Nothing you did; tell me to keep going and I'll pick it back up."
			s.finishRun(r, runrecord.StatusFailed, text, "run panicked", nil, true)
		}
		s.engine.broker.closeRun(r.id)
	}()

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

	status := s.superviseTask(ctx, r, message, resume, audio)

	// Barge-in / explicit stop: close quietly as "interrupted" — the user moved
	// on; the next turn is taking over. Mark the task interrupted so the reaper
	// never resumes it. (CAS on run id: a no-op if a newer run already owns the
	// conversation's task record.)
	if r.stopped.Load() {
		// The user explicitly stopped; abandon any messages they had queued
		// mid-task (F5) so they don't leak into a later, unrelated turn.
		s.drainInput()
		s.engine.tasks.Finish(s.id, r.id, task.StatusInterrupted)
		if r.onFinish != nil {
			r.onFinish(task.StatusInterrupted)
		}
		if !r.closed {
			// A stop is a normal, user-driven outcome — close it with a calm
			// final turn, never a bare terminal. Without a closing turn the
			// client's grace window expires into the alarming "task stopped,
			// try again" copy for something the user did on purpose.
			text := "Stopped. Say the word and I'll pick this back up where it left off."
			s.finishRun(r, runrecord.StatusInterrupted, text, "", nil, true)
		}
		s.engine.broker.closeRun(r.id)
		return
	}

	s.engine.tasks.Finish(s.id, r.id, status)
	if r.onFinish != nil {
		r.onFinish(status)
	}
	// Defensive backstop: the supervisor always emits a terminal (Say on a
	// genuine completion, or deliverCeiling at the ceiling), so r.closed is
	// normally true here. Synthesize one only if somehow it isn't, so the
	// client's stream always terminates deterministically.
	if !r.closed {
		s.finishRun(r, runrecord.StatusCompleted, "Done.", "", nil, true)
	}
	s.engine.broker.closeRun(r.id)

	// F5: a mid-task message can land AFTER the agent's final inbox drain but
	// BEFORE this run is fully retired. The leftover sweep lives in the
	// deferred clearActive(r): it runs under actMu — the same lock submit
	// holds to route a message — so a message either lands in the inbox
	// before the sweep (and is re-dispatched as a continuation run) or
	// arrives after `active` is cleared (and dispatches fresh). There is no
	// window in which a message can be enqueued onto a run that will never
	// drain it.
}

// superviseTask keeps at least one agent on the task until it is genuinely
// complete (the agent's own completion gate accepts and it Says its answer), is
// interrupted by the user, or hits a hard ceiling. Every non-clean exit — a
// model/transport error, a no-progress stall, an exhausted step budget, a
// stuck attempt that timed out — is treated as "not done": it checkpoints, tells
// the user it is still working (never a fake "done"), backs off, respawns a
// FRESH agent over durable state, and goes again.
func (s *session) superviseTask(ctx context.Context, r *run, objective string, resume bool, audio *agent.AudioTurn) task.Status {
	cfg := s.engine.cfg
	maxRespawns := postureRespawnLimit(cfg, objective)
	if maxRespawns < 0 || !cfg.SuperviseTasks {
		// Supervisor off: a single attempt, then an honest partial — never a
		// fabricated "done" and never a bare "failed".
		maxRespawns = 0
	}

	// lastErr carries the PREVIOUS attempt's death (the ErrIncomplete
	// where-it-got-stuck digest) across the respawn boundary so the successor's
	// catch-up prime is born knowing how its predecessor died — the immediate
	// death-journal read path. nil on the first attempt (no predecessor).
	var lastErr error
	for attempt := 1; ; attempt++ {
		s.appendSupervisorEvent(ctx, r, attempt, "attempt_start", "")
		// A fresh first dispatch runs the user's message verbatim (the session
		// is already seeded from durable history WITHOUT it). A reaper resume,
		// or any respawn, uses the catch-up prime over the rebuilt agent.
		prompt := objective
		if resume || attempt > 1 {
			prompt = resumePrime(objective, attempt, lastErr)
		}

		actx, acancel := s.attemptContext(ctx)
		var err error
		s.agent.SetRunIdentity(r.id, attempt)
		// The conversation store, not ambient memory, owns the current thread.
		// Refresh the exact visible turns before every attempt and exclude this
		// run's own user/narration records; the active objective is appended once
		// by the runtime as the newest and authoritative user message.
		if s.engine.conv.Enabled() {
			turns := s.engine.conv.Recent(s.id, conversation.DefaultRecallTurns)
			s.agent.SetRecentConversationHistory(seedMessages(dropCurrentRun(turns, r)))
		}
		if resume || attempt > 1 {
			err = s.agent.ChatResume(withRun(actx, r), objective, prompt)
		} else if attempt == 1 && audio != nil {
			err = s.agent.ChatAudio(withRun(actx, r), prompt, audio)
		} else {
			err = s.agent.Chat(withRun(actx, r), prompt)
		}
		acancel()
		if r.closed {
			s.appendSupervisorEvent(ctx, r, attempt, "complete", "")
			return task.StatusDone
		}
		if r.narrated && strings.TrimSpace(r.lastText) != "" {
			s.finishRun(
				r, runrecord.StatusCompleted, r.lastText, "", nil, false,
			)
			s.appendSupervisorEvent(ctx, r, attempt, "complete", "")
			return task.StatusDone
		}

		// The agent carries the shared failure class of this attempt's most
		// recent classified tool failure, so the supervisor reads the SAME
		// taxonomy the dispatch ladder used (NE-5) rather than re-guessing.
		failClass := s.agent.LastFailureClass()

		action := superviseDecision(r.stopped.Load(), err, failClass, ctx.Err(), attempt, maxRespawns)
		switch action {
		case actInterrupted:
			// User stop / barge-in: drive() emits the interrupted terminal.
			return task.StatusInterrupted
		case actDone:
			// Genuine completion: the agent's Say already emitted the terminal.
			s.appendSupervisorEvent(ctx, r, attempt, "complete", "")
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

		// actRespawn — not done, keep going. Checkpoint, journal the death,
		// reassure the user (no fake done), back off with jitter, then respawn a
		// fresh agent over durable state.
		//
		// A provider OUTAGE (unreachable/rate-limited upstream) is a special
		// respawn: a fresh agent cannot reach the same dead gateway, so it must
		// back off HARD (drain pressure off the recovering upstream, don't join a
		// connection storm) and reassure the user only ONCE per streak instead of
		// spamming a "still working" bubble every few seconds.
		outage := errors.Is(err, llm.ErrRateLimited) || errors.Is(err, llm.ErrProviderUnavailable)
		s.engine.tasks.Checkpoint(s.id, r.id, attempt+1, friendlyErr(err))
		s.recordLoopDeath(ctx, r.id, objective, attempt, err, failClass)
		s.appendSupervisorEvent(ctx, r, attempt, "respawn", friendlyErr(err))
		s.appendRecoveryEvent(ctx, r, attempt, "resumable", friendlyErr(err))
		s.emitProgress(r, attempt, err, outage)
		lastErr = err
		if !superviseBackoff(ctx, attempt, outage) {
			if r.stopped.Load() {
				return task.StatusInterrupted
			}
			return s.deliverCeiling(r, "reached the time limit I had for this task")
		}
		s.rebuildAgent()
	}
}

func dropCurrentRun(turns []conversation.Turn, r *run) []conversation.Turn {
	if r == nil {
		return turns
	}
	result := make([]conversation.Turn, 0, len(turns))
	droppedObjective := false
	for _, turn := range turns {
		if turn.IntentID == r.id {
			if turn.Role == "assistant" {
				continue
			}
			if turn.Role == "user" && !droppedObjective {
				droppedObjective = true
				continue
			}
		}
		result = append(result, turn)
	}
	return result
}

func postureRespawnLimit(cfg config.Config, objective string) int {
	if cfg.InteractionPosture &&
		agent.ClassifyInteractionPosture(cfg, objective) == agent.PostureConversation {
		return 0
	}
	// Read-only exploration uses real tools and can fail for the same transient
	// provider/protocol reasons as execution. It receives the configured retry
	// budget; only ordinary conversation remains single-attempt.
	return cfg.TaskMaxRespawns
}

func (s *session) appendSupervisorEvent(ctx context.Context, r *run, attempt int, action, reason string) {
	if s == nil || s.engine == nil || s.engine.journal == nil || r == nil {
		return
	}
	_, err := s.engine.journal.Append(ctx, sessionjournal.Event{
		ConversationID: s.id, TurnID: r.id, Attempt: attempt,
		Kind: sessionjournal.KindSupervisor, DisplayContent: reason,
		Supervisor: &sessionjournal.Supervisor{
			IntentID: r.id, Attempt: attempt, Action: action, Reason: reason,
		},
	})
	if err != nil {
		s.engine.logLifecycle("journal.supervisor", r.id, s.id, action, time.Since(r.started), err)
	}
}

func (s *session) appendRecoveryEvent(ctx context.Context, r *run, attempt int, state, reason string) {
	if s == nil || s.engine == nil || s.engine.journal == nil || r == nil {
		return
	}
	if strings.TrimSpace(reason) == "" {
		reason = "supervised attempt requires recovery"
	}
	_, err := s.engine.journal.Append(ctx, sessionjournal.Event{
		ConversationID: s.id, TurnID: r.id, Attempt: attempt,
		Kind: sessionjournal.KindRecovery, DisplayContent: reason,
		Recovery: &sessionjournal.Recovery{State: state, Reason: reason},
	})
	if err != nil {
		s.engine.logLifecycle("journal.recovery", r.id, s.id, state, time.Since(r.started), err)
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
	case taskCtxErr != nil:
		return actCeiling
	case attempt > maxRespawns:
		return actCeiling
	case failClass != delegate.ClassTransient:
		return actStop
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
// and keep going until it is genuinely done. When the predecessor died with a
// where-it-got-stuck digest (prev != nil), that is folded in so the successor is
// born knowing how the last attempt failed and is told not to repeat it — the
// immediate death-journal read path. After a few stuck attempts it also nudges
// decomposition / delegation.
func resumePrime(objective string, attempt int, prev error) string {
	var b strings.Builder
	b.WriteString("[continue this task] A previous attempt was interrupted before the task was finished. Your objective is unchanged:\n\n")
	b.WriteString(strings.TrimSpace(objective))
	if d := deathDigest(prev); d != "" {
		b.WriteString("\n\nHow the previous attempt ended (learn from this — do NOT repeat the same move that got it stuck): ")
		b.WriteString(d)
	}
	b.WriteString("\n\nWork from a previous attempt may already exist — check your workspace (list/read the relevant files, re-run a quick status check) and BUILD ON what is already done instead of restarting from scratch. If the prior work ALREADY gathered what the task needs and only the write-up or delivery remains, do NOT re-run the research or re-do the gathering — synthesize and deliver the final answer directly from what you already have. Keep going until the objective is fully achieved to a high standard, then give your final answer with honest coverage and the real evidence behind it.")
	if attempt >= 3 {
		b.WriteString(" If you keep getting stuck on one piece, break it into smaller concrete steps, or delegate parallel parts with spawn_subagents.")
	}
	return b.String()
}

// deathDigest extracts a concise, model-readable reason a prior supervised
// attempt ended, for the successor's catch-up prime. It strips the ErrIncomplete
// sentinel prefix so only the where-it-got-stuck digest remains. Returns "" when
// there is no prior failure (the first attempt).
func deathDigest(prev error) string {
	if prev == nil {
		return ""
	}
	msg := strings.TrimSpace(prev.Error())
	sentinel := agent.ErrIncomplete.Error() + ": "
	if i := strings.Index(msg, sentinel); i >= 0 {
		msg = strings.TrimSpace(msg[i+len(sentinel):])
	}
	return clip(msg, 400)
}

// deathEntry is the SINGLE description of one supervised-attempt death — the
// shared source for BOTH death-journal read paths (self-model task 3.1,
// req.4.3), so the immediate successor-prime digest and the durable Neocortex
// record cannot describe the same death differently. The where-it-got-stuck
// Digest is the same deathDigest(err) both paths read; Class and State live on
// the durable record (the richer surface), while the prime folds in the Digest.
type deathEntry struct {
	Objective string
	Attempt   int
	Class     delegate.FailureClass
	Digest    string // where-it-got-stuck, ErrIncomplete sentinel already stripped
	State     string // the agent's rich loop-state line ("" when unavailable)
}

// newDeathEntry builds the shared death descriptor from one attempt's outcome.
// The Digest is exactly the deathDigest(err) the successor's resume prime folds
// in; when that is empty (a non-ErrIncomplete death) it falls back to the
// friendly error so the durable record is never blank.
func newDeathEntry(objective string, attempt int, err error, class delegate.FailureClass, state string) deathEntry {
	digest := deathDigest(err)
	if digest == "" {
		digest = friendlyErr(err)
	}
	return deathEntry{
		Objective: objective,
		Attempt:   attempt,
		Class:     class,
		Digest:    digest,
		State:     state,
	}
}

// durableSummary renders the entry as the durable Neocortex death-journal line: the
// unchanged prefix from last session (objective, attempt, class, digest) plus
// the agent's rich loop-state suffix (self-model task 2.2), so the record
// carries the actual failure MODE, not just a sentence.
func (e deathEntry) durableSummary() string {
	return fmt.Sprintf(
		"Loop death (attempt %d, class=%s): objective %q did not finish. Where it got stuck: %s",
		e.Attempt, e.Class, clip(e.Objective, 160), e.Digest,
	) + e.State
}

// recordLoopDeath persists a structured death-journal entry to Neocortex when a
// supervised attempt died and forced a respawn — the DURABLE read path of the
// death journal (the immediate path is the successor's resume prime). It is
// built from the shared deathEntry so it describes the SAME death the successor's
// prime folds in (req.4.3). Best-effort: a nil pager, a clean exit, or a write
// error never blocks the respawn.
func (s *session) recordLoopDeath(ctx context.Context, intentID, objective string, attempt int, err error, failClass delegate.FailureClass) {
	if s.engine == nil || s.engine.pager == nil || err == nil {
		return
	}
	// Fold the agent's RICH loop-state capture — the real repeat count,
	// recent-signature shape, last tool, context fill, faculty, and death reason
	// at death — onto the durable record (self-model task 2.2). LastDeath is
	// per-turn and cleared on the next Chat, so it MUST be read here, before the
	// supervisor rebuilds the agent.
	state := ""
	if s.agent != nil {
		if d, ok := s.agent.LastDeath(); ok {
			state = d.StateLine()
		}
	}
	entry := newDeathEntry(objective, attempt, err, failClass, state)
	_, _ = s.engine.pager.RecordLoopDeath(ctx, entry.durableSummary(), intentID)
}

// emitProgress tells the user the task is STILL IN PROGRESS after a failed
// attempt — honest and non-terminal, never a fake completion. The machine
// detail (the real neo/llm error) goes to the logs for diagnosis.
//
// During a provider OUTAGE streak the user-facing bubble is emitted ONCE: a
// gateway that is down (or hard rate-limiting) fails every futile respawn in
// quick succession, and a fresh "still working" message every few seconds is
// spam, not reassurance. The retry is always logged for diagnosis regardless.
// Any non-outage failure clears the latch so genuine, distinct progress is
// always shown.
func (s *session) emitProgress(r *run, attempt int, err error, outage bool) {
	if !outage {
		r.outageNotified = false
	}
	if !outage || !r.outageNotified {
		msg := s.progressMessage(r)
		s.engine.broker.publish(r.id, "chat.assistant", "neo", s.chatFields(r, msg, false))
		if outage {
			r.outageNotified = true
		}
	}
	// The user-facing bubble stays a calm progress line, but the death itself
	// must be VISIBLE on the diagnostic side-channel. Sub-agent deaths already
	// publish subagent.status=failed; a main-run death published nothing, so a
	// silent respawn was indistinguishable from an agent that simply kept
	// working — the 2026-07-25 research task burned two lives and a full user
	// session before the real reason (an O1 budget rejection) was recoverable
	// at all, and only from a memory-activation seed. Same broker as every
	// other event, so it lands in the durable trace.
	fields := map[string]interface{}{
		"conversation_id": s.id,
		"intent_id":       r.id,
		"attempt":         attempt + 1,
		"outage":          outage,
	}
	if err != nil {
		fields["error"] = clip(strings.Join(strings.Fields(err.Error()), " "), 400)
	}
	s.engine.broker.publish(r.id, "run.retry", "neo", fields)
	s.engine.logLifecycle("run.retry", r.id, s.id, fmt.Sprintf("attempt_%d", attempt+1), time.Since(r.started), err)
}

// progressMessage builds a SUBSTANTIVE still-working update (item 6b): instead of
// a contentless "hit a snag, taking another run", it tells the user where things
// actually stand — the agent's honest best-effort digest so far, clipped — then
// that it is continuing from there. When that digest IS the message already on
// screen it references it rather than re-pasting (the double-render), and when
// there is nothing concrete yet it falls back to a plain, honest line. This is
// how a peer reports progress: what's done, and what's next.
func (s *session) progressMessage(r *run) string {
	best := strings.TrimSpace(s.agent.BestEffort())
	switch {
	case best == "":
		return "Still on it — I hit a snag on that pass and I'm picking it straight back up now."
	case best == r.lastText:
		return "Still on it — my last message has where things stand; I'm continuing from there now."
	default:
		return "Still on it. Where I've gotten to: " + clip(best, 320) + "\n\nContinuing from here now."
	}
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
	switch {
	case best != "" && best == r.lastText:
		// The best-effort digest IS the last bubble on screen — pointing at it
		// beats re-pasting it (the double-render).
		text += " My last message has exactly where it stands."
	case best != "":
		text += " Here's exactly where it stands:\n\n" + best
	}
	text += "\n\nTell me how you'd like me to continue and I'll pick it right back up."
	fields := s.chatFields(r, text, true)
	fields["honest_partial"] = true
	fields["incomplete"] = true
	fields["resumable"] = true
	if diagnostic, ok := s.agent.LastRuntimeIncomplete(); ok {
		fields["runtime_incomplete"] = diagnostic
	}
	s.finishRun(
		r, runrecord.StatusFailed, text, reason, fields, true,
	)
	return task.StatusCeiling
}

// deliverDeterministicStop emits the terminal for a task blocked by a
// DETERMINISTIC failure — one that recurs identically (a denied permission, a
// limit, an invalid request, a detail only the user can change). A fresh agent
// would hit the same wall, so the supervisor does NOT respawn and does NOT
// consume the respawn budget. Unlike the generic "still on it, taking another
// run at it" progress copy, this states plainly that the task is blocked and
// hands the user the next step, with the best real work so far.
//
// The copy tells the truth about WHY it stopped: an unproductive-cap death
// (the agent spun without closing — often because what it has for the user is
// a question, not a completion) reads as "I need your direction", never as a
// permission/limit wall that doesn't exist. And a best-effort digest that IS
// the last bubble already on screen is referenced, not re-pasted (the
// double-render). Idempotent if already closed.
func (s *session) deliverDeterministicStop(r *run) task.Status {
	if r.closed {
		return task.StatusCeiling
	}
	best := strings.TrimSpace(s.agent.BestEffort())
	shown := best != "" && best == r.lastText
	unproductive := false
	if d, ok := s.agent.LastDeath(); ok && d.Reason == agent.DeathReasonUnproductive {
		unproductive = true
	}
	var text string
	switch {
	case unproductive && shown:
		text = "I've taken this as far as I can on my own — my last message has where things stand."
	case unproductive && best != "":
		text = "I've taken this as far as I can on my own. Here's where it stands:\n\n" + best
	case unproductive:
		text = "I've taken this as far as I can on my own."
	default:
		text = "I couldn't complete this — I ran into something I can't get past on my own (a permission, a limit, or a detail that needs to change), and trying the same thing again wouldn't help, so I stopped rather than spin on it."
		if shown {
			text += " My last message has where it got to."
		} else if best != "" {
			text += " Here's where it got to:\n\n" + best
		}
	}
	text += "\n\nTell me how you'd like to adjust it and I'll pick it right back up."
	fields := s.chatFields(r, text, true)
	fields["incomplete"] = true
	fields["resumable"] = true
	if best != "" {
		fields["honest_partial"] = true
	}
	s.finishRun(
		r, runrecord.StatusFailed, text,
		"deterministic blocker", fields, true,
	)
	return task.StatusCeiling
}

// superviseBackoff sleeps an attempt-scaled, jittered interval before a
// respawn, honoring task cancellation. Jitter avoids a synchronized retry
// stampede when many users' daemons hit the same upstream hiccup at once.
//
// rateLimited stretches the interval hard: an upstream 429 means the provider
// is overloaded, so respawning quickly across many daemons is a self-amplifying
// request storm. On a 429 the base grows to ~15s/attempt (cap 90s) with a full
// 15s of jitter, draining pressure off the upstream instead of adding to it,
// while still eventually retrying (the Task Durability Rule is preserved — we
// keep going, just far more slowly).
func superviseBackoff(ctx context.Context, attempt int, rateLimited bool) bool {
	step, ceil, jitter := 750*time.Millisecond, 8*time.Second, 500*time.Millisecond
	if rateLimited {
		step, ceil, jitter = 15*time.Second, 90*time.Second, 15*time.Second
	}
	base := time.Duration(attempt) * step
	if base > ceil {
		base = ceil
	}
	d := base + time.Duration(rand.Int63n(int64(jitter)))
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

// superviseAutomatrixTask is the QUIET sibling of superviseTask for an
// autonomous Automatrix run (task 4.1, req 5.1/5.3/5.4/5.5). It mirrors the
// real supervisor's policy EXACTLY — the same pure superviseDecision over the
// agent's clean finish / failure class / wall-clock, the same jittered backoff,
// the same FRESH-agent respawn over durable state across a non-clean exit — so
// a proactive run is held to the SAME loop discipline (inside agent.Chat:
// no-progress stall / step budget / the unified unproductive counter, plus the
// Cassandra 2.0 silent-voice controller) as any interactive turn. The two
// differences from superviseTask are deliberate: (1) the agent is the
// RESTRICTED no-money surface (the session was minted with automatrix=true), and
// (2) it surfaces NOTHING — no SSE/broker terminals, no honest-partial bubble in
// the thread (req 5.5: a partial/failed proactive attempt is silent, never
// failed-surprise spam). It returns the terminal task.Status so the runner can
// settle the opportunity (done vs pending+attempts). A user stop cannot happen
// here (no live run / no barge-in), so actInterrupted is unreachable.
func (s *session) superviseAutomatrixTask(ctx context.Context, objective string) task.Status {
	cfg := s.engine.cfg
	maxRespawns := cfg.TaskMaxRespawns
	if maxRespawns < 0 || !cfg.SuperviseTasks {
		maxRespawns = 0
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		prompt := objective
		if attempt > 1 {
			prompt = resumePrime(objective, attempt, lastErr)
		}
		actx, acancel := s.attemptContext(ctx)
		s.agent.SetRunIdentity(s.id, attempt)
		var err error
		if attempt > 1 {
			err = s.agent.ChatResume(actx, objective, prompt)
		} else {
			err = s.agent.Chat(actx, prompt)
		}
		acancel()

		failClass := s.agent.LastFailureClass()
		if err != nil {
			if incomplete, ok := s.agent.LastRuntimeIncomplete(); ok {
				fmt.Fprintf(os.Stderr, "neo/automatrix: canonical attempt %d failed in %s: %v (%s)\n", attempt, incomplete.Phase, err, incomplete.CauseDetail)
			} else {
				fmt.Fprintf(os.Stderr, "neo/automatrix: canonical attempt %d failed: %v\n", attempt, err)
			}
		}
		switch superviseDecision(false, err, failClass, ctx.Err(), attempt, maxRespawns) {
		case actDone:
			// Genuine completion: the agent's own gate accepted.
			return task.StatusDone
		case actStop:
			// Deterministic blocker — a fresh agent would hit the same wall.
			return task.StatusCeiling
		case actCeiling:
			return task.StatusCeiling
		}
		// actRespawn — not done. Journal the death, back off (honoring
		// cancellation), then respawn a FRESH restricted agent over durable state
		// and try again. No progress is surfaced to the user (req 5.5).
		s.recordLoopDeath(ctx, s.id, objective, attempt, err, failClass)
		lastErr = err
		if !superviseBackoff(ctx, attempt, errors.Is(err, llm.ErrRateLimited) || errors.Is(err, llm.ErrProviderUnavailable)) {
			return task.StatusCeiling
		}
		s.rebuildAgent()
	}
}

// superviseBriefTask is the QUIET sibling of superviseTask for an autonomous
// MORNING_BRIEF run (ORACLE task 5.4, req 15.1/15.2). It mirrors the real
// supervisor's policy EXACTLY — the same pure superviseDecision over the agent's
// clean finish / failure class / wall-clock, the same jittered backoff, the same
// FRESH-agent respawn over durable state across a non-clean exit — so a brief is
// held to the SAME loop discipline as any interactive turn. The two differences
// are deliberate: (1) the agent is the TIGHT read-only brief surface (the
// session was minted with brief=true, so it physically cannot reach money /
// signing / filesystem / deploy / exec tools, req 15.1), and (2) it surfaces
// NOTHING live — the composed brief is delivered out of band as a durable
// conversation turn + inbox record before any notification (req 15.2). It
// returns the terminal task.Status so the runner can settle the ledger (done vs
// ceiling). A user stop cannot happen here (no live run / no barge-in).
func (s *session) superviseBriefTask(ctx context.Context, objective string) task.Status {
	cfg := s.engine.cfg
	maxRespawns := cfg.TaskMaxRespawns
	if maxRespawns < 0 || !cfg.SuperviseTasks {
		maxRespawns = 0
	}
	var lastErr error
	for attempt := 1; ; attempt++ {
		prompt := objective
		if attempt > 1 {
			prompt = resumePrime(objective, attempt, lastErr)
		}
		actx, acancel := s.attemptContext(ctx)
		s.agent.SetRunIdentity(s.id, attempt)
		var err error
		if attempt > 1 {
			err = s.agent.ChatResume(actx, objective, prompt)
		} else {
			err = s.agent.Chat(actx, prompt)
		}
		acancel()

		failClass := s.agent.LastFailureClass()
		if err != nil {
			if incomplete, ok := s.agent.LastRuntimeIncomplete(); ok {
				fmt.Fprintf(os.Stderr, "neo/brief: canonical attempt %d failed in %s: %v (%s)\n", attempt, incomplete.Phase, err, incomplete.CauseDetail)
			} else {
				fmt.Fprintf(os.Stderr, "neo/brief: canonical attempt %d failed: %v\n", attempt, err)
			}
		}
		switch superviseDecision(false, err, failClass, ctx.Err(), attempt, maxRespawns) {
		case actDone:
			return task.StatusDone
		case actStop:
			return task.StatusCeiling
		case actCeiling:
			return task.StatusCeiling
		}
		// actRespawn — not done. Journal the death, back off (honoring
		// cancellation), then respawn a FRESH brief-surface agent over durable
		// state and try again. Nothing is surfaced to the user (req 15.2).
		s.recordLoopDeath(ctx, s.id, objective, attempt, err, failClass)
		lastErr = err
		if !superviseBackoff(ctx, attempt, errors.Is(err, llm.ErrRateLimited) || errors.Is(err, llm.ErrProviderUnavailable)) {
			return task.StatusCeiling
		}
		s.rebuildAgent()
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
	defer s.actMu.Unlock()
	if s.active != r {
		return
	}
	s.active = nil
	// Finishing-window sweep (F5): submit enqueues to the inbox under this
	// same actMu whenever `active` was still r, so any message that raced the
	// run's shutdown is guaranteed to be visible here. Re-dispatch it as a
	// continuation run instead of stranding it — a message the client already
	// acked with a 202 must never silently vanish. An explicitly stopped run
	// is the one exception: the user superseded the task, so queued mid-task
	// messages are deliberately abandoned (they belonged to the old intent).
	if leftover := s.drainInput(); len(leftover) > 0 && !r.stopped.Load() && s.engine != nil {
		s.dispatchLocked(strings.Join(leftover, "\n\n"), false, nil, nil)
	}
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

func (s *session) finishRun(r *run, status, text, failure string, fields map[string]interface{}, persist bool) bool {
	if r == nil || r.closed {
		return false
	}
	text = strings.TrimSpace(llm.StripGuidance(text))
	if fields == nil {
		fields = s.chatFields(r, text, true)
	}
	if persist && text != "" {
		s.engine.conv.AppendAssistant(s.id, r.id, text)
	}
	_, _, published := s.engine.broker.publishTerminal(r.id, status, "neo", fields, func(lastSeq int) bool {
		if s.engine.runRecords == nil || !s.engine.runRecords.Enabled() {
			return !r.closed
		}
		var diagnostics json.RawMessage
		if detail, ok := fields["runtime_incomplete"]; ok {
			diagnostics, _ = json.Marshal(detail)
		}
		_, transitioned, err := s.engine.runRecords.FinishDetailed(
			r.id, status, text, failure, lastSeq, diagnostics,
		)
		if err != nil {
			s.engine.logLifecycle("run.final_persist", r.id, s.id, status, time.Since(r.started), err)
			return false
		}
		s.engine.logLifecycle("run.final_persist", r.id, s.id, status, time.Since(r.started), nil)
		return transitioned
	})
	if !published {
		return false
	}
	r.lastText = text
	r.closed = true
	s.engine.logLifecycle("run.terminal", r.id, s.id, status, time.Since(r.started), nil)
	return true
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
// — an INPUT on the same footing as a user message, never a plan/walk/Neocortex
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

	mu        sync.Mutex
	attempts  map[int]*streamedAttempt
	committed string
}

type streamedAttempt struct {
	content   strings.Builder
	reasoning strings.Builder
}

// automatrixReporter is the QUIET reporter for an autonomous Automatrix run. It
// surfaces NOTHING to the user (req 5.5: a proactive run has no live SSE/thread
// presence — the surprise result is delivered out of band by the completion
// path, task 5.3) but CAPTURES the agent's final user-facing answer so that
// path can use it as the result-not-protocol summary of what Neo produced. Only
// the latest Say text is retained; on a genuine gate-passed completion that is
// the finished result, and on a partial/ceiling terminal the captured text is
// never used (settle announces nothing). Guarded by a mutex because the
// supervisor may rebuild the agent (and thus Say from a fresh attempt) while
// the runner goroutine reads the captured result after the run returns.
type automatrixReporter struct {
	mu     sync.Mutex
	result string
}

func (r *automatrixReporter) Say(text string, _ bool) {
	text = strings.TrimSpace(llm.StripGuidance(text))
	if text == "" {
		return
	}
	r.mu.Lock()
	r.result = text
	r.mu.Unlock()
}

func (r *automatrixReporter) Status(string)             {}
func (r *automatrixReporter) Progress(string)           {}
func (r *automatrixReporter) Notice(string)             {}
func (r *automatrixReporter) Think(string)              {}
func (r *automatrixReporter) Delta(int, string, string) {}

// lastResult returns the most recent captured final answer ("" if none).
func (r *automatrixReporter) lastResult() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.result
}

// automatrixResult returns the final user-facing answer captured by the quiet
// Automatrix reporter for this session ("" for a non-Automatrix session or a
// run that produced no answer).
func (s *session) automatrixResult() string {
	if s.automatrixOut == nil {
		return ""
	}
	return s.automatrixOut.lastResult()
}

func (r *sseReporter) Say(text string, completion bool) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	// Defense-in-depth (req.1.3): never let guidance-channel steering reach the
	// user, even if the model echoed it into its answer.
	text = llm.StripGuidance(text)
	fields := s.chatFields(run, text, true)
	if completion {
		// Legacy completion marker: kept for wire compatibility with the client's
		// "redundant recap" handling. With the proof-of-work gate retired (Cassandra
		// 2.0), Neo's own loop delivers every answer with completion=false, so this
		// path is dormant; message.complete still fires to close the run.
		fields["completion"] = true
	}
	r.mu.Lock()
	alreadyPersisted := strings.TrimSpace(text) != "" &&
		strings.TrimSpace(text) == r.committed
	r.committed = ""
	r.mu.Unlock()
	// Persist conversational / ceiling answers unless the accepted streamed
	// answer was already committed. This keeps reopen durable without duplicating
	// the same final bubble.
	s.finishRun(
		run, runrecord.StatusCompleted, text, "", fields,
		(!completion || !run.narrated) && !alreadyPersisted,
	)
}

func (r *sseReporter) SayHonestPartial(text string) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	text = strings.TrimSpace(llm.StripGuidance(text))
	if text == "" {
		return
	}
	fields := s.chatFields(run, text, true)
	fields["honest_partial"] = true
	fields["incomplete"] = true
	fields["resumable"] = true
	r.mu.Lock()
	alreadyPersisted := text == r.committed
	r.committed = ""
	r.mu.Unlock()
	s.finishRun(
		run, runrecord.StatusFailed, text,
		"saved partial work is ready to resume", fields,
		!alreadyPersisted,
	)
}

func (r *sseReporter) Status(text string) {
	// Free-form status is model-authored working text and is never public. Tool
	// execution progress is emitted through Progress from deterministic runtime
	// milestones; accepted final content is committed through Delta/Say.
}

// Progress surfaces deterministic runtime milestones. It is EPHEMERAL: shown
// live but never persisted and never treated as delivered answer content.
func (r *sseReporter) Progress(text string) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	text = strings.TrimSpace(llm.StripGuidance(text))
	if text == "" || strings.HasPrefix(text, "• ") {
		return
	}
	fields := s.chatFields(run, text, false)
	fields["ephemeral"] = true
	s.engine.broker.publish(run.id, "chat.assistant", "neo", fields)
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
	text = strings.TrimSpace(llm.StripGuidance(text))
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
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	switch channel {
	case "retraction":
		r.mu.Lock()
		delete(r.attempts, turn)
		r.mu.Unlock()
		s.engine.broker.publish(
			run.id, "chat.retraction", "neo",
			map[string]interface{}{
				"intent_id":       run.id,
				"conversation_id": s.id,
				"turn":            turn,
			},
		)
		return
	case "commit":
		r.mu.Lock()
		attempt := r.attempts[turn]
		delete(r.attempts, turn)
		content, reasoning := "", ""
		if attempt != nil {
			content = strings.TrimSpace(attempt.content.String())
			reasoning = strings.TrimSpace(attempt.reasoning.String())
		}
		r.committed = content
		r.mu.Unlock()
		fields := map[string]interface{}{
			"intent_id":       run.id,
			"conversation_id": s.id,
			"turn":            turn,
		}
		if content != "" {
			fields["text"] = content
		}
		if reasoning != "" {
			fields["reasoning"] = reasoning
		}
		s.engine.broker.publish(
			run.id, "chat.attempt.commit", "neo", fields,
		)
		if content != "" {
			s.engine.conv.AppendAssistant(s.id, run.id, content)
			run.narrated = true
			run.lastText = content
		}
		return
	}
	// Defense-in-depth (req.1.3): strip any guidance-channel envelope the model
	// echoed before it streams to the live reasoning/content channel.
	text = llm.StripGuidance(text)
	if text == "" {
		return
	}
	r.mu.Lock()
	if r.attempts == nil {
		r.attempts = make(map[int]*streamedAttempt)
	}
	attempt := r.attempts[turn]
	if attempt == nil {
		attempt = &streamedAttempt{}
		r.attempts[turn] = attempt
	}
	if channel == "reasoning" {
		attempt.reasoning.WriteString(text)
	} else if channel == "content" {
		attempt.content.WriteString(text)
	}
	r.mu.Unlock()
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
