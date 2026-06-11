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
	"strings"
	"sync"
	"time"

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
	conv         *conversation.Store // durable chat-thread history (per conversation_id)
	mediaDir     string              // machine-volume dir for generated + uploaded media ("" disables)

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
	ConversationDir string // durable conversation store dir ("" disables persistence)
	MediaDir        string // machine-volume media dir ("" disables image/video/audio I/O)
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
	}
	return e
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

// surfaceTool turns a tool result into a user-facing event. Web search/news
// become rich source+snippet cards (the transparency differentiator); other
// tools get a compact activity line.
func (e *Engine) surfaceTool(r *run, ev agent.ToolEvent) {
	if r == nil {
		return
	}
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
			return
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
		return
	}
	// Generic: a compact "did X" so even non-search tools show their work.
	e.broker.publish(r.id, "tool.result", "neo", map[string]interface{}{
		"intent_id":       r.id,
		"conversation_id": r.convID,
		"tool":            ev.Name,
		"label":           toolLabel(ev.Name),
		"ok":              !ev.IsErr,
		"summary":         firstLine(ev.Result, 240),
	})
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

// toolLabel maps a function name to a friendly present-tense activity label.
func toolLabel(name string) string {
	switch {
	case strings.HasSuffix(name, "web_search"):
		return "Searched the web"
	case strings.HasSuffix(name, "web_news"):
		return "Searched the news"
	case strings.Contains(name, "fetch"):
		return "Read a page"
	case strings.Contains(name, "read_file"), strings.Contains(name, "directory"), strings.Contains(name, "list"):
		return "Read files"
	case strings.Contains(name, "git"):
		return "Checked the repository"
	case strings.Contains(name, "shell"), strings.Contains(name, "exec"):
		return "Ran a command"
	case strings.HasSuffix(name, "generate_image"):
		return "Created an image"
	case strings.HasSuffix(name, "edit_image"):
		return "Edited an image"
	case strings.HasSuffix(name, "generate_video"):
		return "Generated a video"
	case strings.HasSuffix(name, "transcribe_audio"):
		return "Transcribed audio"
	case name == tools.CoreExecuteTool:
		return "Routed to secure execution"
	default:
		return "Used " + humanizeTool(name)
	}
}

func humanizeTool(name string) string {
	if i := strings.Index(name, "__"); i >= 0 {
		name = name[i+2:]
	}
	return strings.ReplaceAll(name, "_", " ")
}

func firstLine(s string, max int) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}
