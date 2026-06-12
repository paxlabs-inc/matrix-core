// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/recall"
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

	gatesMu sync.Mutex
	gates   map[string]chan gateAnswer // node_id -> waiter, for delegated MCL gates
}

// run is a single user turn. id doubles as the SSE topic + the intent_id the
// client subscribes to.
type run struct {
	id     string
	convID string
	sess   *session
	closed bool // a closing (final) turn has been emitted
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
	}
	// Conversational recall lane: relevant PAST turns of this (now unbounded)
	// thread, beyond the live transcript + resume seed. Reuses the pager's
	// embedder so the whole agent shares one embedding model; a disabled store
	// or absent embedder yields a nil recaller (no-op).
	var recaller agent.ConvRecaller
	if e.conv.Enabled() && e.pager != nil {
		if emb := e.pager.Embedder(); emb != nil {
			recaller = recall.New(e.conv, convID, emb, e.cfg.RecallTopK, e.cfg.RecallBudgetTokens)
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
	})
	// Resume continuity: if this conversation already has durable turns (a
	// reopened thread, or one that outlived a restart), seed the fresh agent's
	// transcript so it remembers the thread instead of starting blank.
	if e.conv.Enabled() {
		if turns := e.conv.Recent(convID, conversation.DefaultRecallTurns); len(turns) > 0 {
			s.agent.Seed(seedMessages(turns), firstUserText(turns))
		}
	}
	return s
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

// start kicks off a turn: it returns the run (so the handler can reply with the
// intent_id immediately) and drives agent.Chat on a background goroutine,
// streaming every event to the run's topic.
func (s *session) start(message string) *run {
	r := &run{id: synthRunID(message), convID: s.id, sess: s}
	s.engine.registerRun(r)
	// Create the SSE topic NOW, before returning the dispatch (and before the
	// background goroutine's first publish). This closes the dispatch→subscribe
	// race: the client connects to /events the moment it has the intent_id, and
	// must find a Neo-owned topic or the request is reverse-proxied to the
	// daemon's empty stream. The replay buffer then backfills any events
	// published between this point and the client's connect.
	s.engine.broker.ensure(r.id)
	go s.drive(r, message)
	return r
}

func (s *session) drive(r *run, message string) {
	defer s.engine.unregisterRun(r.id)

	s.mu.Lock()
	s.cur = r
	defer func() {
		s.cur = nil
		s.mu.Unlock()
	}()

	// Detached from any HTTP request; bounded so a stuck turn can't run forever.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Minute)
	defer cancel()

	err := s.agent.Chat(withRun(ctx, r), message)

	// The agent emits its closing turn via Reporter.Say (always terminal). If
	// it returned without one (a model-call error), synthesize an honest close
	// so the client's stream terminates deterministically.
	if !r.closed {
		status, text := "completed", "Done."
		if err != nil {
			status, text = "failed", "I hit a problem and couldn't finish that — "+friendlyErr(err)
		}
		s.engine.broker.publish(r.id, "message.complete", "neo", map[string]interface{}{"status": status})
		s.engine.broker.publish(r.id, "chat.assistant", "neo", s.chatFields(r, text, true))
		s.engine.conv.AppendAssistant(s.id, r.id, text)
		r.closed = true
	}
	s.engine.broker.closeRun(r.id)
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

// sseReporter maps the agent's Reporter calls onto the conversation's event
// stream. Say is always the closing turn (the agent only Says to end), so it
// emits the terminal sequence; Status is progress; Notice is a visible spoken
// promise (compaction / escalation).
type sseReporter struct {
	sess *session
}

func (r *sseReporter) Say(text string) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	s.engine.broker.publish(run.id, "message.complete", "neo", map[string]interface{}{"status": "completed"})
	s.engine.broker.publish(run.id, "chat.assistant", "neo", s.chatFields(run, text, true))
	// Persist the closing answer so the thread is durable and reopenable.
	s.engine.conv.AppendAssistant(s.id, run.id, text)
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
	// Tool-start markers ("• <tool>") become a compact activity chip; the
	// observer emits the rich result. Everything else is model narration.
	if strings.HasPrefix(text, "• ") {
		name := strings.TrimSpace(strings.TrimPrefix(text, "• "))
		s.engine.broker.publish(run.id, "tool.activity", "neo", map[string]interface{}{
			"intent_id":       run.id,
			"conversation_id": s.id,
			"tool":            name,
			"label":           toolLabel(name),
		})
		return
	}
	s.engine.broker.publish(run.id, "chat.assistant", "neo", s.chatFields(run, text, false))
}

func (r *sseReporter) Notice(text string) {
	s := r.sess
	run := s.cur
	if run == nil {
		return
	}
	s.engine.broker.publish(run.id, "chat.assistant", "neo", s.chatFields(run, text, false))
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
	if len(m) > 200 {
		m = m[:200] + "…"
	}
	return m
}
