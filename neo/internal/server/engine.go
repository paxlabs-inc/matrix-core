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
	"matrix/neo/internal/task"
	"matrix/neo/internal/tools"
	"matrix/neo/internal/trace"
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
	tasks        *task.Store            // durable task-supervision ledger (survives restart/suspend)
	trace        *trace.Store           // durable per-run workspace timeline ("Neo's Computer"); sidecar, never cortex
	mediaDir     string                 // machine-volume dir for generated + uploaded media ("" disables)

	backendURL   string // co-located MCL daemon (core_execute + reverse proxy)
	backendToken string // optional bearer for the daemon

	broker   *broker
	sessions *sessionRegistry

	mu   sync.Mutex
	runs map[string]*run // active runs by id, for gate-answer routing

	// gateClaims serialises approval-gate answering across the per-call
	// delegate clients (engine.coreExecute builds a fresh delegate.Client per
	// call). Keyed on MCL intent id + gate node id, it lets only ONE delegate
	// service a given gate, so two concurrent attaches to the same in-flight
	// intent (a re-dispatch joining a parked launch) cannot double-answer it
	// (req 2.4). This is the delegate-side reconcile chosen over a daemon-side
	// idempotent-create: it lives entirely on the conversational seam and never
	// touches the signed MCL path.
	gateClaimsMu sync.Mutex
	gateClaims   map[string]bool
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
	TaskDir         string                 // durable task-ledger dir ("" disables; reaper needs it to resume after restart)
	TraceDir        string                 // durable workspace-trace dir ("" disables; the reopen-survives-reload store, F3)
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
		tasks:        task.Open(o.TaskDir),
		trace:        trace.Open(o.TraceDir),
		mediaDir:     strings.TrimRight(o.MediaDir, "/"),
		backendURL:   strings.TrimRight(o.BackendURL, "/"),
		backendToken: o.BackendToken,
		broker:       newBroker(),
		runs:         map[string]*run{},
		gateClaims:   map[string]bool{},
	}
	e.sessions = newSessionRegistry(e)
	// Persist the durable workspace timeline (F3): the broker tap routes every
	// workspace event to the sidecar trace store so "Neo's Computer" survives a
	// reload / reopen-from-history, not just the 2-minute in-memory replay
	// window. The tap is a non-blocking enqueue, off the publish hot path.
	if e.trace.Enabled() {
		e.broker.setTap(e.recordTrace)
	}
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
		// Live task-list (neo-smoothness req.3): the todo tool streams the
		// ordered checklist onto the run's event stream as a tool.todo event
		// (pure side-channel) and the trace persists it so it survives reopen.
		e.tools.SetTodo(e.emitTodo)
	}
	return e
}

// ResumeOrphanedTasks re-dispatches every task left "running" in the durable
// ledger — tasks orphaned when the daemon was restarted or Fly-suspended
// mid-work. Each resumes on its own conversation with the catch-up prime, so
// the Task Durability Rule holds across a process death: a dropped task is
// picked back up and driven to completion rather than silently lost. Called
// once at boot (cmd/neo serve). Best-effort + idempotent — a task that actually
// finished is no longer "running" and is skipped, and the ledger's CAS-on-run-id
// keeps a stale record from clobbering a live one. Returns the count resumed.
func (e *Engine) ResumeOrphanedTasks() int {
	if e.tasks == nil || !e.tasks.Enabled() {
		return 0
	}
	orphans := e.tasks.Running()
	for _, t := range orphans {
		e.sessions.get(t.ConvID).startResume(t.Objective)
	}
	return len(orphans)
}

// Close flushes and stops the engine's durable sidecar stores (today: the
// workspace trace writer). Called on graceful shutdown so any queued workspace
// events are persisted before exit. Idempotent + safe on a disabled store.
func (e *Engine) Close() {
	if e == nil {
		return
	}
	e.trace.Close()
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

// emitTodo is the live task-list emitter wired into the tool manager
// (neo-smoothness req.3): it streams the agent's ordered checklist onto the
// active run's event stream as a tool.todo event. Pure side-channel — it only
// publishes a transcript event (like emitConstructSurface / surfaceTool); it
// never signs, writes cortex, or touches the plan/walk. The trace tap persists
// tool.todo (traceWorkspaceTypes) so the checklist survives reopen + respawn.
func (e *Engine) emitTodo(ctx context.Context, items []tools.TodoItem) error {
	r := runFromContext(ctx)
	if r == nil {
		return fmt.Errorf("todo: no active run on context")
	}
	list := make([]map[string]interface{}, len(items))
	for i, it := range items {
		list[i] = map[string]interface{}{"text": it.Text, "status": string(it.Status)}
	}
	e.broker.publish(r.id, "tool.todo", "neo", map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"items":           list,
	})
	return nil
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
		GateClaim: e.claimGate,
	})
	return dele.Run(ctx, intent)
}

// claimGate is the shared cross-attach gate guard handed to every per-call
// delegate client. The FIRST caller to claim (intentID, nodeID) wins and may
// answer the gate; later callers (a concurrent attach to the same in-flight
// intent) lose the claim and skip answering, so a single approval gate is
// serviced exactly once even when two core_execute delegations join the same
// run. Safe for concurrent use.
func (e *Engine) claimGate(intentID, nodeID string) bool {
	key := intentID + "\x00" + nodeID
	e.gateClaimsMu.Lock()
	defer e.gateClaimsMu.Unlock()
	if e.gateClaims[key] {
		return false
	}
	if e.gateClaims == nil {
		e.gateClaims = map[string]bool{}
	}
	e.gateClaims[key] = true
	return true
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

// activeRunForConv reports the in-flight run id for one conversation, or "" if
// nothing is live. Unlike broker.has(id) (which stays true for 2 minutes after
// settlement so late reconnects can replay), this consults the authoritative
// runs registry: a run is here iff registerRun has fired and unregisterRun has
// NOT — i.e. iff drive() is still executing. handleConversations uses it to
// stamp `live_run` on the GET /conversations/{id} response so a fresh tab /
// post-relogin client can subscribe(replay:true) without waiting for the poll.
func (e *Engine) activeRunForConv(convID string) string {
	if e == nil || convID == "" {
		return ""
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	for id, r := range e.runs {
		if r != nil && r.convID == convID {
			return id
		}
	}
	return ""
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

// traceWorkspaceTypes is the set of event types that make up the durable "Neo's
// Computer" workspace timeline (F3). Only these are persisted to the sidecar
// trace store; transient channels (chat.delta, chat.thinking), gate/ask control
// events, and terminals are NOT — they are either re-derived on reopen or
// irrelevant to the rebuilt workspace. chat.assistant is handled specially in
// recordTrace (only the non-final "thinking out loud" narration is persisted;
// the final answer + bubbles come from the durable conversation store).
var traceWorkspaceTypes = map[string]bool{
	"tool.step":               true,
	"tool.search":             true,
	"tool.media":              true,
	"tool.artifact":           true,
	"tool.todo":               true,
	"construct.surface":       true,
	"construct.surface.patch": true,
	"swarm.started":           true,
	"subagent.created":        true,
	"subagent.step":           true,
	"subagent.note":           true,
	"subagent.status":         true,
	"swarm.completed":         true,
}

// recordTrace is the broker tap (F3): it persists the workspace-relevant slice
// of the live event stream to the durable sidecar trace store so "Neo's
// Computer" can be rebuilt when a thread is reopened — after the run settles,
// scrolls past the 512-event broker buffer, or the topic is reclaimed. It runs
// on every publish, so it stays cheap: a map lookup, then a NON-BLOCKING
// enqueue (trace.Record drops rather than blocks). It never signs, never writes
// cortex, never touches plan/walk (pure sidecar — m9/i_trace).
func (e *Engine) recordTrace(id string, ev Event) {
	if e.trace == nil || !e.trace.Enabled() || id == "" {
		return
	}
	keep := traceWorkspaceTypes[ev.Type]
	if !keep && ev.Type == "chat.assistant" {
		// Persist only the non-final narration ("thinking out loud") turns as
		// workspace steps; the final answer is the conversation store's job.
		keep = ev.Fields == nil || ev.Fields["final"] != true
	}
	if !keep {
		return
	}
	e.trace.Record(id, trace.Event{
		Seq:    ev.Seq,
		Ts:     ev.Ts,
		Phase:  ev.Phase,
		Type:   ev.Type,
		Fields: ev.Fields,
	})
}

// latestRunForConv resolves the run id whose workspace trace a reopened thread
// should rebuild: the in-flight run if one is live, else the most recent run on
// the conversation (the last persisted turn carrying an intent_id). Returns ""
// when the conversation has no run-bearing turn.
func (e *Engine) latestRunForConv(convID string) string {
	if live := e.activeRunForConv(convID); live != "" {
		return live
	}
	if e.conv == nil {
		return ""
	}
	rec := e.conv.Get(convID)
	if rec == nil {
		return ""
	}
	for i := len(rec.Turns) - 1; i >= 0; i-- {
		if rec.Turns[i].IntentID != "" {
			return rec.Turns[i].IntentID
		}
	}
	return ""
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
	// F6: a deliverable artifact (a downloadable file/archive, or a deployed
	// site URL) → a first-class `tool.artifact` event the client renders as an
	// in-app download card / open-and-preview card, instead of a raw bucket URL
	// pasted into the answer text. ADDITIVE + ignore-safe: older clients that
	// don't know the event simply skip it. A tool opts in by returning the
	// documented artifact shape (see parseArtifact); the blob itself stays in
	// object storage as a content-addressed ref (boundaries: never bytes in the
	// conversation store or cortex).
	if a, ok := parseArtifact(ev.Result); ok {
		e.broker.publish(r.id, "tool.artifact", "neo", map[string]interface{}{
			"intent_id":       r.id,
			"conversation_id": r.convID,
			"tool":            ev.Name,
			"kind":            a.Kind,
			"url":             a.URL,
			"mime":            a.MIME,
			"name":            a.Name,
			"size":            a.Size,
			"preview":         a.Preview,
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

// artifactPayload is a deliverable hand-off (F6): a downloadable file/archive
// or a deployed-site URL the client renders as an in-app card. Kind is one of
// "file" | "archive" | "site" (image/video stay on the media plane). URL is a
// content-addressed / public ref into object storage — never inline bytes.
type artifactPayload struct {
	OK      bool   `json:"ok"`
	Kind    string `json:"kind"`
	URL     string `json:"url"`
	MIME    string `json:"mime"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Preview string `json:"preview"`
}

// artifactEnvelope lets a tool advertise its artifact either at the top level
// ({ok,kind,url,...}) or nested under an "artifact" key ({ok:true,artifact:{…}}),
// matching the two shapes tools actually emit.
type artifactEnvelope struct {
	OK       bool             `json:"ok"`
	Artifact *artifactPayload `json:"artifact"`
}

// validArtifactKind gates the deliverable kinds so an artifact result never
// collides with the media plane (image/video) or a search payload.
func validArtifactKind(k string) bool {
	return k == "file" || k == "archive" || k == "site"
}

// parseArtifact recognises a successful artifact-tool result. It accepts both
// the nested ({ok:true,artifact:{kind,url,…}}) and the flat ({ok,kind,url,…})
// shapes, requires a deliverable kind + a URL ref, and never matches a media or
// search result (kind gating). Returns (payload, false) on any other shape so
// the additive event is emitted ONLY for genuine deliverables.
func parseArtifact(raw string) (artifactPayload, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw[0] != '{' {
		return artifactPayload{}, false
	}
	// Nested shape first ({ok:true, artifact:{…}}).
	var env artifactEnvelope
	if err := json.Unmarshal([]byte(raw), &env); err == nil && env.Artifact != nil {
		a := *env.Artifact
		if validArtifactKind(a.Kind) && a.URL != "" {
			return a, true
		}
	}
	// Flat shape ({ok:true, kind, url, …}).
	var a artifactPayload
	if err := json.Unmarshal([]byte(raw), &a); err != nil {
		return artifactPayload{}, false
	}
	if !a.OK || a.URL == "" || !validArtifactKind(a.Kind) {
		return artifactPayload{}, false
	}
	return a, true
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
