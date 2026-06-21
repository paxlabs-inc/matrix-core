// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package server turns Neo into a production conversational service: it speaks
// the daemon's POST /chat + GET /events SSE contract (so the existing web and
// Telegram clients work unchanged), streams the agent loop's work — including
// live web-search snippets and source cards — and reverse-proxies everything
// else to the co-located MCL daemon. core_execute delegates rigorous / money
// tasks to that daemon over HTTP exactly as the frozen spec's
// [relation.delegation] prescribes.
package server

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/cassandra"
	"matrix/construct/backchannel"
	"matrix/construct/schema"
	"matrix/construct/schema/primitives"
	"matrix/construct/transport"
	"matrix/neo/internal/agent"
	"matrix/neo/internal/config"
	"matrix/neo/internal/conversation"
	"matrix/neo/internal/delegate"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// Engine holds the process-wide shared dependencies (models, the one MCP tool
// surface, the one cortex pager, the background consolidator) and hands each
// conversation its own agent loop over them.
type Engine struct {
	cfg          config.Config
	main         *llm.Client
	cheap        *llm.Client
	tools        *tools.Manager
	pager        *memory.Pager
	consolidator agent.Consolidator
	adjudicator  *cassandra.Adjudicator // shared Cassandra completeness faculty (Phase 3); nil = deterministic fallback
	conv         *conversation.Store    // durable chat-thread history (per conversation_id)
	mediaDir     string                 // machine-volume dir for generated + uploaded media ("" disables)

	backendURL   string // co-located MCL daemon (core_execute + reverse proxy)
	backendToken string // optional bearer for the daemon

	broker   *broker
	sessions *sessionRegistry

	mu   sync.Mutex
	runs map[string]*run // active runs by id, for gate-answer routing
}

// EngineOptions configures NewEngine. Main + Tools are required; the rest are
// optional (a nil pager/consolidator degrades gracefully).
type EngineOptions struct {
	Config          config.Config
	Main            *llm.Client
	Cheap           *llm.Client
	Tools           *tools.Manager
	Pager           *memory.Pager
	Consolidator    agent.Consolidator
	Adjudicator     *cassandra.Adjudicator // shared Cassandra completeness faculty (Phase 3)
	ConversationDir string                 // durable conversation store dir ("" disables persistence)
	MediaDir        string                 // machine-volume media dir ("" disables image/video/audio I/O)
	BackendURL      string
	BackendToken    string
}

// NewEngine assembles the engine and wires core_execute delegation through the
// shared tool manager.
func NewEngine(o EngineOptions) *Engine {
	e := &Engine{
		cfg:          o.Config,
		main:         o.Main,
		cheap:        o.Cheap,
		tools:        o.Tools,
		pager:        o.Pager,
		consolidator: o.Consolidator,
		adjudicator:  o.Adjudicator,
		conv:         conversation.Open(o.ConversationDir),
		mediaDir:     strings.TrimRight(o.MediaDir, "/"),
		backendURL:   strings.TrimRight(o.BackendURL, "/"),
		backendToken: o.BackendToken,
		broker:       newBroker(),
		runs:         map[string]*run{},
	}
	e.sessions = newSessionRegistry(e)
	if e.tools != nil {
		e.tools.SetDelegate(e.coreExecute)
		// Task-scoped concurrent sub-agents (the Agent Swarm). Capped per call
		// by config so one spawn can't fan out unbounded.
		e.tools.SetSwarm(e.runSwarm, o.Config.MaxSubagents)
		// Construct ACTIVE tier: let the agent render typed surfaces onto the
		// user's screen via the construct_render tool (pure side-channel).
		e.tools.SetSurfaceEmitter(e.emitConstructSurface)
		// Construct Ask back-channel: an Ask surface BLOCKS the agent for a
		// typed human answer (invariant i5), delivered over Neo's gate-style
		// answer endpoint and returned to the tool call as its result.
		e.tools.SetAskResponder(e.respondAsk)
	}
	return e
}

// neoSurfaceSink adapts the engine's per-run event broker to the construct
// transport EventSink interface, so the Construct emit logic stays
// single-source (transport.EmitSurface validates the surface + builds the
// event fields).
type neoSurfaceSink struct {
	e     *Engine
	runID string
}

func (s neoSurfaceSink) Event(typ, phase string, fields map[string]interface{}) {
	s.e.broker.publish(s.runID, typ, phase, fields)
}

// emitConstructSurface is the ACTIVE-tier Construct emitter wired into the tool
// manager: it streams an agent-authored surface onto the active run's event
// stream as a construct.surface event. Pure side-channel — it only publishes a
// transcript event, exactly like surfaceTool / notifyFor; it never signs,
// writes cortex, or touches the plan/walk.
func (e *Engine) emitConstructSurface(ctx context.Context, s *schema.Surface) error {
	r := runFromContext(ctx)
	if r == nil {
		return fmt.Errorf("construct: no active run on context")
	}
	return transport.EmitSurface(neoSurfaceSink{e: e, runID: r.id}, r.id, r.convID, s)
}

// askWaitTimeout bounds how long a parked Ask waits for a human answer before
// it expires and the agent is told to proceed without it (still inside the
// run's own 20-minute ceiling). Long enough for a deliberate human decision
// (read a tx, pick an option), short enough not to wedge a run forever.
const askWaitTimeout = 10 * time.Minute

// respondAsk is the ACTIVE-tier Construct back-channel (invariant i5): it shows
// an Ask surface, PARKS the agent's construct_render tool call on a per-ask
// waiter keyed by the surface id, and returns the human's typed response once
// it is posted to /intents/{id}/asks/{ask_id}/answer. It mirrors approverFor
// (the in-walk gate) exactly — register the waiter FIRST so a fast answer can't
// race the park, then emit; on answer, patch the Ask surface to its settled
// state so the rendered control resolves. Pure side-channel: it publishes
// transcript events and returns the answer as a tool result (an agent INPUT);
// it never signs, writes cortex, or touches the plan/walk.
func (e *Engine) respondAsk(ctx context.Context, s *schema.Surface) (*primitives.AskResponse, error) {
	r := runFromContext(ctx)
	if r == nil {
		return nil, fmt.Errorf("construct: no active run on context")
	}
	askID := s.ID
	ch := r.sess.registerAsk(askID, s.Ask)
	sink := neoSurfaceSink{e: e, runID: r.id}

	// Show the question (the renderer paints the control), then announce the
	// run is parked awaiting a human answer.
	if err := transport.EmitSurface(sink, r.id, r.convID, s); err != nil {
		r.sess.clearAsk(askID)
		return nil, err
	}
	e.broker.publish(r.id, "ask.awaiting", transport.Phase, map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"ask_id":          askID,
		"ask_kind":        string(s.Ask.AskKind),
	})

	select {
	case <-ctx.Done():
		r.sess.clearAsk(askID)
		return nil, ctx.Err()
	case <-time.After(askWaitTimeout):
		r.sess.clearAsk(askID)
		e.broker.publish(r.id, "ask.expired", transport.Phase, map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"ask_id":          askID,
		})
		return nil, fmt.Errorf("timed out after %s waiting for an answer", askWaitTimeout)
	case resp := <-ch:
		// Settle the rendered control: patch the Ask with its response so the
		// client flips from the live control to the answered state (mirrors
		// gate.decided). A patch failure is non-fatal — the answer still flows.
		if answered, err := backchannel.Answered(s, resp); err == nil {
			_ = transport.PatchSurface(sink, r.id, r.convID, askID, answered)
		}
		e.broker.publish(r.id, "ask.answered", transport.Phase, map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"ask_id":          askID,
		})
		return resp, nil
	}
}

// coreExecute is the in-conversation bridge to the MCL pipeline. It reads the
// active run from ctx so it can surface the delegated run's approval gates and
// status onto that conversation's event stream, then delegates over HTTP to
// the daemon (the only thing that can move funds), servicing gates inline.
func (e *Engine) coreExecute(ctx context.Context, intent string) (string, error) {
	r := runFromContext(ctx)
	dele := delegate.New(delegate.Options{
		BaseURL:   e.backendURL,
		Token:     e.backendToken,
		CallerDID: e.cfg.ActorDID,
		Approver:  e.approverFor(r),
		Notify:    e.notifyFor(r),
	})
	return dele.Run(ctx, intent)
}

// approverFor surfaces a delegated MCL gate as a gate.invoked event on the
// conversation stream and blocks until the user answers via Neo's gate-answer
// endpoint (or the context is cancelled, which denies).
func (e *Engine) approverFor(r *run) delegate.Approver {
	return func(ctx context.Context, nodeID, question string, options []string) (bool, string) {
		if r == nil {
			return false, "" // no conversation context: deny rather than auto-spend
		}
		ans := r.sess.registerGate(nodeID)
		e.broker.publish(r.id, "gate.invoked", "neo", map[string]interface{}{
			"intent_id": r.id,
			"node_id":   nodeID,
			"question":  question,
			"options":   options,
		})
		select {
		case <-ctx.Done():
			r.sess.clearGate(nodeID)
			return false, ""
		case a := <-ans:
			e.broker.publish(r.id, "gate.decided", "neo", map[string]interface{}{
				"intent_id": r.id,
				"node_id":   nodeID,
				"approved":  a.approved,
			})
			return a.approved, a.answer
		}
	}
}

// publishAudit streams a Cassandra audit event onto the run's event stream as
// a cassandra.* event (cassandra.frozen.kvx [audit].events). Pure observability
// side-channel — it only publishes a transcript event, exactly like surfaceTool
// / notifyFor; the adjudicator signs nothing and writes no cortex (i_cass_4,
// i_cass_6).
func (e *Engine) publishAudit(r *run, ev agent.AuditEvent) {
	if r == nil {
		return
	}
	f := map[string]interface{}{"intent_id": r.id, "conversation_id": r.convID}
	for k, v := range ev.Fields {
		f[k] = v
	}
	e.broker.publish(r.id, ev.Type, "cassandra", f)
}

func (e *Engine) notifyFor(r *run) func(string) {
	return func(msg string) {
		if r == nil {
			return
		}
		e.broker.publish(r.id, "chat.assistant", "neo", map[string]interface{}{
			"role":            "assistant",
			"text":            msg,
			"conversation_id": r.convID,
			"intent_id":       r.id,
		})
	}
}

// registerRun / lookupRun / unregisterRun index active runs so the gate-answer
// route can find the waiting approver by run id.
func (e *Engine) registerRun(r *run) {
	e.mu.Lock()
	e.runs[r.id] = r
	e.mu.Unlock()
}

func (e *Engine) lookupRun(id string) *run {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs[id]
}

func (e *Engine) unregisterRun(id string) {
	e.mu.Lock()
	delete(e.runs, id)
	e.mu.Unlock()
	// Keep the event topic briefly for late reconnects, then reclaim it.
	go func() {
		time.Sleep(2 * time.Minute)
		e.broker.drop(id)
	}()
}

// surfaceTool turns a tool call into the live "Neo Workspace" — a single
// `tool.step` event the client renders as an ANIMATED VIEWPORT of the action
// itself: a terminal when Neo runs a command, a browser window when it
// browses, an editor when it reads/writes files. The same step id is updated
// from running→done across the start/end pair, so the viewport fills in place
// rather than appending a new card. Web-search results and generated media ALSO
// keep their dedicated rich events (cards / media grid) on completion.
func (e *Engine) surfaceTool(r *run, ev agent.ToolEvent) {
	if r == nil {
		return
	}
	// The animated workspace step (start paints "running"; end fills it in).
	step := describeStep(ev)
	step["intent_id"] = r.id
	step["conversation_id"] = r.convID
	e.broker.publish(r.id, "tool.step", "neo", step)

	if ev.Phase != agent.ToolEnd {
		return
	}
	// On completion, web search/news also emit rich source+snippet cards.
	if isSearchTool(ev.Name) {
		if s, ok := parseSearch(ev.Result); ok {
			e.broker.publish(r.id, "tool.search", "neo", map[string]interface{}{
				"intent_id":       r.id,
				"conversation_id": r.convID,
				"tool":            ev.Name,
				"provider":        s.Provider,
				"query":           s.Query,
				"answer":          s.Answer,
				"results":         s.cards(),
			})
		}
	}
	// Generated/edited media → a rich media card the client renders inline
	// (image thumbnail / video player). Transcripts are plain text and flow
	// through the model's answer, so they don't get a media card.
	if m, ok := parseMedia(ev.Result); ok && (m.Kind == "image" || m.Kind == "video") {
		e.broker.publish(r.id, "tool.media", "neo", map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"tool":            ev.Name,
			"kind":            m.Kind,
			"url":             m.URL,
			"mime":            m.MIME,
			"prompt":          m.Prompt,
		})
	}
}

func isSearchTool(name string) bool {
	return strings.HasSuffix(name, "web_search") || strings.HasSuffix(name, "web_news")
}

// searchPayload mirrors the web-search MCP tool's JSON result.
type searchPayload struct {
	Tool     string `json:"tool"`
	Provider string `json:"provider"`
	Query    string `json:"query"`
	Answer   string `json:"answer"`
	Results  []struct {
		Title     string `json:"title"`
		URL       string `json:"url"`
		Snippet   string `json:"snippet"`
		Published string `json:"published"`
	} `json:"results"`
	OK *bool `json:"ok"` // present (false) only on a structured error
}

func parseSearch(raw string) (searchPayload, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return searchPayload{}, false
	}
	var s searchPayload
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return searchPayload{}, false
	}
	if s.OK != nil && !*s.OK {
		return searchPayload{}, false // error result: not a card set
	}
	if len(s.Results) == 0 {
		return searchPayload{}, false
	}
	return s, true
}

// mediaPayload mirrors the media MCP tool's JSON result for image/video output.
type mediaPayload struct {
	OK     bool   `json:"ok"`
	Kind   string `json:"kind"`
	URL    string `json:"url"`
	MIME   string `json:"mime"`
	Prompt string `json:"prompt"`
}

// parseMedia recognises a successful media-tool result ({ok:true, kind, url}).
func parseMedia(raw string) (mediaPayload, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return mediaPayload{}, false
	}
	var m mediaPayload
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return mediaPayload{}, false
	}
	if !m.OK || m.URL == "" || m.Kind == "" {
		return mediaPayload{}, false
	}
	return m, true
}

func (s searchPayload) cards() []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(s.Results))
	for _, r := range s.Results {
		out = append(out, map[string]interface{}{
			"title":     r.Title,
			"url":       r.URL,
			"snippet":   r.Snippet,
			"published": r.Published,
		})
	}
	return out
}

func humanizeTool(name string) string {
	if i := strings.Index(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	return strings.ReplaceAll(name, "_", " ")
}
