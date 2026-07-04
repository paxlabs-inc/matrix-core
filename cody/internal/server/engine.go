// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package server is codyd: the Cody engine served the proven Neo way —
// engine + session supervisor + SSE broker + durable trace — mounted behind
// the router under the user's JWT. codyd holds NO signing key, routes NOTHING
// through MCL, and never touches Neo, the Liaison boundary, or the wallet
// seam: coding is not value-moving. A run's lifecycle is decoupled from the
// client connection (Task Durability): the plan executes on a background
// context, progress is durable in cortex + the contract store, and a codyd
// restart resumes orphaned plans at the correct next task.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/cassandra"
	"matrix/cody/internal/checkpoint"
	"matrix/cody/internal/contract"
	"matrix/cody/internal/delegate"
	"matrix/cody/internal/gate"
	"matrix/cody/internal/llm"
	"matrix/cody/internal/mode"
	"matrix/cody/internal/orchestrator"
	"matrix/cody/internal/policy"
	"matrix/cody/internal/preview"
	"matrix/cody/internal/sandbox"
	"matrix/cody/internal/tools"
	"matrix/cody/internal/worker"
	"matrix/cody/internal/workspace"
	cortex "matrix/cortex"
)

// EngineOptions configures codyd's engine.
type EngineOptions struct {
	// WorkspaceRoot is the user's code root (/workspace in the environment).
	WorkspaceRoot string
	// DataDir holds codyd durable state (plans, traces) — /data/cody.
	DataDir string
	// Cortex is the per-user cortex instance (progress checkpoints).
	Cortex *cortex.Cortex
	// GatewayURL + ActorDID meter every LLM call on the cody slot.
	GatewayURL string
	ActorDID   string
	// DefaultMode when a request names none (engineer).
	DefaultMode mode.Mode
	// Model overrides (empty = the mode's defaults).
	OrchestratorModel string
	WorkerModel       string
	// RulesDir / SkillsDir surface the standards library; empty disables.
	RulesDir  string
	SkillsDir string
	// ScaffoldDir points at the scaffolder suite (scaffold-<stack>.sh) workers
	// run for greenfield project structure; empty disables the prompt section.
	ScaffoldDir string
	// TitleModel, when set, generates the conversation title with a small
	// bounded LLM call (async, fallback = first message line). Empty disables.
	TitleModel string
	// Worker ExtraTools bridges (req 13.1): the shared browser (for screenshot
	// evidence), fetch, and web search. Each is boot-safe when unset.
	BrowserURL   string
	BrowserToken string
	SearxngURL   string
	SearxngToken string
	// MaxAttempts bounds per-task re-dispatch (default 3).
	MaxAttempts int
	// MaxRespawns bounds orchestrator respawns per run (default 2).
	MaxRespawns int
	// VerifyTimeout bounds one verification command.
	VerifyTimeout time.Duration
	// DisableAdjudication turns the goal-vs-outcome LLM verdict off (the
	// structural gate still holds). For constrained deployments.
	DisableAdjudication bool
	// Preview wiring (req 7): a configured Railway sandbox client plus the
	// router-facing coordinates turn "plan completed" into an on-demand preview.
	// When Sandbox is nil (or the coordinates are empty) previews are disabled
	// and the client shows "no preview yet". PreviewUserID is the supabase user
	// id the router /preview proxy keys on; it MUST match the JWT subject.
	Sandbox            sandbox.Client
	PreviewUserID      string
	RouterInternalURL  string
	RouterPreviewToken string
	PreviewPublicBase  string
	PreviewTTL         time.Duration
	// PreviewImage overrides the sandbox base image (default: a Node runtime).
	PreviewImage string
	// Logf receives diagnostics; nil discards.
	Logf func(format string, args ...interface{})
}

// Engine owns the sessions, the broker, and the durable trace.
type Engine struct {
	opts     EngineOptions
	broker   *broker
	trace    *traceStore
	projects *projectRegistry
	// skills is the loaded skills library (index + on-demand loader); nil when
	// SkillsDir is unset or unreadable.
	skills *policy.Skills
	// preview is the on-demand sandbox preview manager (req 7); nil when
	// previews are not configured. previewCancel stops its TTL reaper on Close.
	preview       *preview.Manager
	previewCancel context.CancelFunc

	mu       sync.Mutex
	sessions map[string]*session
	runs     map[string]*run

	// inboxMu serializes read-modify-write of every conversation's durable
	// answer/steer inbox (inbox.go).
	inboxMu sync.Mutex
	// ledgerMu serializes read-modify-write of the per-conversation run
	// ledgers (the async title generator writes concurrently with the drive
	// goroutine's status updates).
	ledgerMu sync.Mutex
}

// run is one live plan execution.
type run struct {
	id        string
	convID    string
	projectID string
	root      string // the project's workspace subtree
	cancel    context.CancelFunc
	done      chan struct{}
	// started is when this run began — the baseline for run.activity elapsed_ms.
	started time.Time

	mu     sync.Mutex
	status string // running | completed | failed | stopped | needs_input
	// awaiting + answers implement the needs_input pause: while parked, awaiting
	// is true and an /answer is delivered on the buffered channel. Guarded by mu.
	awaiting bool
	answers  chan directive
	// phase + detail track the run's current activity (set by Engine.activity)
	// so the liveness heartbeat re-emits genuine current state. Guarded by mu.
	phase  string
	detail string
	// usage accumulates the run's worker LLM token spend. Guarded by mu.
	usage contract.Usage
}

// addUsage folds one turn-in's token accounting into the run total.
func (r *run) addUsage(u contract.Usage) {
	r.mu.Lock()
	r.usage.Add(u)
	r.mu.Unlock()
}

// getUsage returns the run's accumulated token accounting.
func (r *run) getUsage() contract.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.usage
}

// setActivity records the run's current phase + detail for the heartbeat.
func (r *run) setActivity(phase, detail string) {
	r.mu.Lock()
	r.phase, r.detail = phase, detail
	r.mu.Unlock()
}

// currentActivity returns the run's current phase + detail.
func (r *run) currentActivity() (string, string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.phase, r.detail
}

func (r *run) setStatus(s string) { r.mu.Lock(); r.status = s; r.mu.Unlock() }
func (r *run) getStatus() string  { r.mu.Lock(); defer r.mu.Unlock(); return r.status }

// beginAwait arms the run to receive an answer and returns the delivery channel.
func (r *run) beginAwait() chan directive {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.awaiting = true
	r.answers = make(chan directive, 1)
	return r.answers
}

// isAwaiting reports whether the run is currently parked on the answer channel.
func (r *run) isAwaiting() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.awaiting
}

// endAwait disarms the run's answer wait (deferred after the select unblocks).
func (r *run) endAwait() {
	r.mu.Lock()
	r.awaiting = false
	r.mu.Unlock()
}

// deliverAnswer hands an answer to a parked run. It reports whether the run was
// actually awaiting (so the caller can fall back to the cold path otherwise).
func (r *run) deliverAnswer(d directive) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.awaiting || r.answers == nil {
		return false
	}
	select {
	case r.answers <- d:
		r.awaiting = false
		return true
	default:
		return false
	}
}

// session serializes runs per conversation: one live plan at a time.
type session struct {
	convID string
	mu     sync.Mutex
	active *run
}

// NewEngine builds the engine and wires the broker tap into the durable trace.
func NewEngine(opts EngineOptions) (*Engine, error) {
	if opts.WorkspaceRoot == "" {
		return nil, errors.New("codyd: empty workspace root")
	}
	if opts.DataDir == "" {
		return nil, errors.New("codyd: empty data dir")
	}
	if opts.DefaultMode == "" {
		opts.DefaultMode = mode.Engineer
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	if opts.MaxRespawns <= 0 {
		opts.MaxRespawns = 2
	}
	if opts.Logf == nil {
		opts.Logf = func(string, ...interface{}) {}
	}
	trace, err := newTraceStore(filepath.Join(opts.DataDir, "trace"))
	if err != nil {
		return nil, err
	}
	e := &Engine{
		opts:     opts,
		broker:   newBroker(),
		trace:    trace,
		projects: newProjectRegistry(filepath.Join(opts.DataDir, "projects.json")),
		sessions: map[string]*session{},
		runs:     map[string]*run{},
	}
	e.broker.setTap(e.recordTrace)

	// The skills library loads once at boot: the index (names + descriptions)
	// rides every worker's prompt; bodies are pulled on demand via skill_load.
	if opts.SkillsDir != "" {
		if skills, err := policy.LoadSkills(opts.SkillsDir); err == nil && len(skills.Names) > 0 {
			e.skills = skills
		} else if err != nil {
			opts.Logf("codyd: skills dir %s: %v", opts.SkillsDir, err)
		}
	}

	// Preview manager (req 7): built only when a sandbox client and the router
	// coordinates are present. Its TTL reaper runs for the engine's lifetime.
	if opts.Sandbox != nil && opts.PreviewUserID != "" && opts.RouterInternalURL != "" {
		e.preview = preview.New(opts.Sandbox, e.publishToConversation, preview.Config{
			UserID:            opts.PreviewUserID,
			RouterInternalURL: opts.RouterInternalURL,
			RouterToken:       opts.RouterPreviewToken,
			PublicBase:        opts.PreviewPublicBase,
			TTL:               opts.PreviewTTL,
			Image:             opts.PreviewImage,
			Logf:              opts.Logf,
		})
		ctx, cancel := context.WithCancel(context.Background())
		e.previewCancel = cancel
		go e.preview.StartReaper(ctx)
	}
	return e, nil
}

// recordTrace is the broker tap: whitelisted workspace events persist so the
// client rebuilds the timeline on reopen. Heartbeat run.activity ticks are
// live-only liveness signals — milestone activities persist, ticks never do.
func (e *Engine) recordTrace(id string, ev Event) {
	if !traceWorkspaceTypes[ev.Type] {
		return
	}
	if hb, _ := ev.Fields["heartbeat"].(bool); hb {
		return
	}
	if err := e.trace.record(id, ev); err != nil {
		e.opts.Logf("codyd: trace record %s: %v", id, err)
	}
}

func (e *Engine) session(convID string) *session {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, ok := e.sessions[convID]
	if !ok {
		s = &session{convID: convID}
		e.sessions[convID] = s
	}
	return s
}

func (e *Engine) lookupRun(id string) *run {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.runs[id]
}

func (e *Engine) registerRun(r *run) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.runs[r.id] = r
}

// Submit dispatches (or attaches to) the conversation's plan run in the named
// project (empty = the default /workspace project). Mode is the project's mode;
// for the default project a per-message mode override is honored for
// retro-compat. When spec is non-empty the planner adopts it as the plan
// (req 11); specSource records its origin for the plan.adopted event. Returns
// the run id and whether it was freshly dispatched.
func (e *Engine) Submit(convID, message, modeName, projectID, spec, specSource string) (string, bool, error) {
	proj, err := e.resolveProject(projectID)
	if err != nil {
		return "", false, err
	}
	m := proj.modeOr(e.opts.DefaultMode)
	// Mode is a project-level setting (req 2.3); the default project honors a
	// per-message mode for retro-compat with the pre-projects /chat contract.
	if proj.ID == defaultProjectID && strings.TrimSpace(modeName) != "" {
		pm, perr := mode.Parse(modeName)
		if perr != nil {
			return "", false, perr
		}
		m = pm
	}
	s := e.session(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil {
		switch s.active.getStatus() {
		case "running", "needs_input":
			// One live plan per conversation: attach to the work already
			// underway (a parked run resolves via /answer, not a re-dispatch).
			return s.active.id, false, nil
		}
	}
	e.ledgerMu.Lock()
	runID := newRunID()
	fresh := true
	prior, perr := e.readLedger(convID)
	if perr == nil && prior.RunID != "" {
		// Continue (req 3.2): a re-dispatch on an existing conversation resumes
		// the same durable plan under the SAME run id, so the durable trace and
		// the client's attach point (events/trace URLs) stay continuous instead
		// of starting over.
		runID = prior.RunID
		fresh = false
	}
	r := &run{id: runID, convID: convID, projectID: proj.ID, root: proj.Root, done: make(chan struct{}), status: "running", started: time.Now()}
	e.registerRun(r)
	e.broker.ensure(r.id) // before Submit returns: no dispatch->subscribe race
	e.broker.reopen(r.id) // a continued run streams live on the same topic
	s.active = r
	led := ledger{RunID: r.id, Message: message, Title: conversationTitle(message), Mode: string(m), ProjectID: proj.ID, Root: proj.Root, Spec: spec, SpecSource: specSource, Status: "running", UpdatedAt: time.Now().UTC()}
	if perr == nil {
		// A continued conversation keeps its original title and its
		// accumulated token accounting.
		if prior.Title != "" {
			led.Title = prior.Title
		}
		led.Usage = prior.Usage
	}
	if err := e.writeLedger(convID, led); err != nil {
		e.ledgerMu.Unlock()
		return "", false, err
	}
	e.ledgerMu.Unlock()
	if fresh && e.opts.TitleModel != "" {
		// The durable title upgrades asynchronously from first-line-of-message
		// to a small-model one-liner; failure keeps the fallback silently.
		go e.generateTitle(convID, message)
	}
	// The initiating message is a durable user turn (req 4.1): the transcript
	// shows BOTH sides on reopen. Emitted before drive so it precedes
	// run.started in the timeline.
	e.publishUserTurn(r.id, "message", message, "")
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go e.drive(ctx, r, message, m, false, spec, specSource)
	return r.id, true, nil
}

// publishUserTurn emits the durable chat.user event — the user's side of the
// transcript (the initiating message, a steer, or an answer) so reopen
// rebuilds the full conversation, not just Cody's half.
func (e *Engine) publishUserTurn(runID, kind, text, decision string) {
	fields := map[string]interface{}{"kind": kind}
	if strings.TrimSpace(text) != "" {
		fields["text"] = text
	}
	if decision != "" {
		fields["decision"] = decision
	}
	e.broker.publish(runID, "chat.user", "cody", fields)
}

// parseModeOrDefault parses a ledger's mode string, falling back to def.
func parseModeOrDefault(name string, def mode.Mode) mode.Mode {
	m, err := mode.Parse(name)
	if err != nil {
		return def
	}
	return m
}

// Stop interrupts a live run.
func (e *Engine) Stop(runID string) bool {
	r := e.lookupRun(runID)
	if r == nil {
		return false
	}
	r.setStatus("stopped")
	r.cancel()
	return true
}

// Close cancels every live run and waits briefly for them to unwind.
func (e *Engine) Close() {
	e.mu.Lock()
	runs := make([]*run, 0, len(e.runs))
	for _, r := range e.runs {
		runs = append(runs, r)
	}
	e.mu.Unlock()
	for _, r := range runs {
		r.cancel()
	}
	for _, r := range runs {
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	}
	// Stop the preview reaper and tear down every live preview sandbox so a
	// graceful shutdown never leaks Railway services.
	if e.previewCancel != nil {
		e.previewCancel()
	}
	if e.preview != nil {
		e.preview.Close()
	}
}

// ResumeOrphanedPlans re-dispatches every conversation whose ledger says a
// plan was running when the process died — the boot reaper (Task Durability).
func (e *Engine) ResumeOrphanedPlans() int {
	entries, err := os.ReadDir(filepath.Join(e.opts.DataDir, "plans"))
	if err != nil {
		return 0
	}
	n := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		convID := entry.Name()
		led, err := e.readLedger(convID)
		if err != nil || led.Status != "running" {
			continue
		}
		s := e.session(convID)
		s.mu.Lock()
		already := s.active != nil && s.active.getStatus() == "running"
		s.mu.Unlock()
		if already {
			continue
		}
		e.launch(convID, led, true)
		n++
	}
	return n
}

// planDir is the conversation's durable contract-store root.
func (e *Engine) planDir(convID string) string {
	return filepath.Join(e.opts.DataDir, "plans", sanitizeRunID(convID))
}

// ledger is the per-conversation run record that survives restarts.
type ledger struct {
	RunID   string `json:"run_id"`
	Message string `json:"message"`
	// Title is the conversation's durable display title (the first message,
	// trimmed to one readable line) so the history list reads well (req 4.3).
	Title     string    `json:"title,omitempty"`
	Mode      string    `json:"mode"`
	ProjectID string    `json:"project_id,omitempty"`
	Root      string    `json:"root,omitempty"`
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
	// Spec is the adopted specification document (req 11): when set, the planner
	// adopts it as the plan instead of decomposing the prose message. It is
	// durable so a resumed run re-adopts the same spec (req 11.2). SpecSource
	// records its origin (a workspace path, or "pasted") for the plan.adopted
	// event.
	Spec       string `json:"spec,omitempty"`
	SpecSource string `json:"spec_source,omitempty"`
	// Usage accumulates the conversation's worker LLM token spend across runs
	// (each run's total is folded in once at its terminal).
	Usage contract.Usage `json:"usage,omitempty"`
}

// mutateLedger atomically read-modify-writes a conversation's ledger — the
// serialization point between the drive goroutine's status updates and the
// async title generator.
func (e *Engine) mutateLedger(convID string, fn func(*ledger)) error {
	e.ledgerMu.Lock()
	defer e.ledgerMu.Unlock()
	led, err := e.readLedger(convID)
	if err != nil {
		return err
	}
	fn(&led)
	led.UpdatedAt = time.Now().UTC()
	return e.writeLedger(convID, led)
}

func (e *Engine) writeLedger(convID string, led ledger) error {
	dir := e.planDir(convID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(led, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "run.json")
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (e *Engine) readLedger(convID string) (ledger, error) {
	var led ledger
	data, err := os.ReadFile(filepath.Join(e.planDir(convID), "run.json"))
	if err != nil {
		return led, err
	}
	return led, json.Unmarshal(data, &led)
}

// titleMessageCap bounds how much of the initiating message the title
// generator sees.
const titleMessageCap = 600

// generateTitle asks a small bounded LLM call for a short conversation title
// and upgrades the ledger's fallback title. Best-effort: any failure keeps
// the first-line fallback; the ledger mutation is serialized against the
// drive goroutine's status updates.
func (e *Engine) generateTitle(convID, message string) {
	cfg := mode.For(e.opts.DefaultMode).WorkerLLM(e.opts.GatewayURL, e.opts.ActorDID)
	cfg.Model = e.opts.TitleModel
	cfg.Temperature = 0.2
	cfg.MaxTokens = 60
	client, err := llm.New(cfg)
	if err != nil {
		return
	}
	msg := strings.TrimSpace(message)
	if len(msg) > titleMessageCap {
		msg = msg[:titleMessageCap]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := client.Chat(ctx, llm.ChatRequest{Messages: []llm.Message{
		llm.SystemMessage("You title coding conversations. Reply with ONLY a title for the request: max 8 words, no quotes, no trailing punctuation."),
		llm.UserMessage(msg),
	}})
	if err != nil {
		return
	}
	title := sanitizeTitle(res.Message.Content)
	if title == "" {
		return
	}
	if err := e.mutateLedger(convID, func(led *ledger) { led.Title = title }); err != nil {
		e.opts.Logf("codyd: title %s: %v", convID, err)
	}
}

// sanitizeTitle normalizes a generated title to one clean line, empty when
// the generation is unusable.
func sanitizeTitle(s string) string {
	line := strings.TrimSpace(s)
	if i := strings.IndexByte(line, '\n'); i >= 0 {
		line = strings.TrimSpace(line[:i])
	}
	line = strings.Trim(line, `"'`+" \t")
	line = strings.TrimRight(line, ".!")
	runes := []rune(line)
	if len(runes) > 80 {
		line = strings.TrimSpace(string(runes[:80]))
	}
	return line
}

// conversationTitle derives the durable display title from the initiating
// message: its first non-empty line, trimmed to one readable length.
func conversationTitle(message string) string {
	for _, line := range strings.Split(message, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		runes := []rune(line)
		if len(runes) > 80 {
			return strings.TrimSpace(string(runes[:80]))
		}
		return line
	}
	return ""
}

// conversationSummary is one row of the server-side history list (req 4.3).
type conversationSummary struct {
	ID        string    `json:"id"`
	Title     string    `json:"title"`
	Status    string    `json:"status"`
	Mode      string    `json:"mode"`
	Project   string    `json:"project,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ListConversations returns the user's conversations from the durable ledgers,
// newest first — the cross-device history source of truth. A pre-title ledger
// falls back to deriving the title from its stored message.
func (e *Engine) ListConversations() []conversationSummary {
	entries, err := os.ReadDir(filepath.Join(e.opts.DataDir, "plans"))
	if err != nil {
		return nil
	}
	out := make([]conversationSummary, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		convID := entry.Name()
		led, err := e.readLedger(convID)
		if err != nil {
			continue
		}
		title := led.Title
		if title == "" {
			title = conversationTitle(led.Message)
		}
		out = append(out, conversationSummary{
			ID:        convID,
			Title:     title,
			Status:    led.Status,
			Mode:      led.Mode,
			Project:   led.ProjectID,
			UpdatedAt: led.UpdatedAt,
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	return out
}

// drive executes one plan run on a background context, supervised: the
// orchestrator is respawned over the same durable state on transient
// failures, deterministic failures stop-and-ask, and every terminal is
// honest. The client connection is never load-bearing.
func (e *Engine) drive(ctx context.Context, r *run, message string, m mode.Mode, resume bool, spec, specSource string) {
	defer close(r.done)
	defer func() {
		e.broker.closeRun(r.id)
		time.AfterFunc(2*time.Minute, func() {
			// A continued run reuses this run id (Submit/launch): when a newer
			// run instance owns the id, skip the cleanup so the drop never
			// yanks a live topic or deregisters the live run.
			e.mu.Lock()
			if e.runs[r.id] != r {
				e.mu.Unlock()
				return
			}
			delete(e.runs, r.id)
			e.mu.Unlock()
			e.broker.drop(r.id)
		})
	}()

	if r.started.IsZero() {
		r.started = time.Now()
	}
	e.publish(r, "run.started", map[string]interface{}{"mode": string(m)})

	// The liveness heartbeat spans the whole run: between milestone boundaries
	// it re-emits the current phase so long LLM stretches (SDR/plan/DLR
	// authoring, worker execution) never look frozen. It is cancel-safe (ctx)
	// and stops before the topic closes.
	stopHeartbeat := e.heartbeat(ctx, r)
	defer stopHeartbeat()

	pol := mode.For(m)
	if e.opts.OrchestratorModel != "" {
		pol.OrchestratorModel = e.opts.OrchestratorModel
	}
	if e.opts.WorkerModel != "" {
		pol.WorkerModel = e.opts.WorkerModel
	}

	e.activity(r, phaseUnderstanding, "")
	st, err := contract.OpenStore(e.planDir(r.convID))
	if err != nil {
		e.finish(r, "failed", "I could not open the plan store: "+err.Error())
		return
	}
	model, err := workspace.LoadOrScan(r.root)
	if err != nil {
		e.finish(r, "failed", "I could not read the workspace: "+err.Error())
		return
	}

	// Project memory (cortex): the project's prior stack/design decisions and
	// accepted turn-ins, recalled as grounding so a later conversation plans
	// from what the project IS instead of re-deriving it from a file scan.
	projectMemory := e.recallProjectMemory(r.projectID)
	summary := model.Summary()
	if projectMemory != "" {
		summary += "\n\nPROJECT MEMORY (durable context from prior work on this project):\n" + projectMemory
	}

	// Decision-phase clients (hot generation + cold adjudication), lazily built
	// once and reused by the SDR, plan-shape authoring, and the DLR.
	var hotClient, coldClient *llm.Client
	ensureDecisionClients := func() error {
		if hotClient != nil && coldClient != nil {
			return nil
		}
		var herr, cerr error
		hotClient, herr = llm.New(pol.DecisionLLM(e.opts.GatewayURL, e.opts.ActorDID))
		coldClient, cerr = llm.New(pol.OrchestratorLLM(e.opts.GatewayURL, e.opts.ActorDID))
		return firstErr(herr, cerr)
	}

	// The plan: resumed from the durable store, or authored by the planner.
	plan, err := orchestrator.LoadPlan(st.Root())
	adopted := false
	if err != nil || plan == nil {
		if cerr := ensureDecisionClients(); cerr != nil {
			e.finish(r, "failed", "I could not reach the model: "+cerr.Error())
			return
		}
		switch {
		case strings.TrimSpace(spec) != "":
			// Spec ingestion (req 11): adopt the handed spec document as the plan
			// rather than decomposing the prose message. The spec is the source of
			// truth — including its own stack choices — so the SDR does not gate an
			// adopted plan.
			e.activity(r, phasePlanning, "Adopting your spec")
			plan, err = orchestrator.AdoptSpecDivergent(ctx, hotClient, coldClient, spec, summary, pol.Render(), pol.DecisionCandidates)
			if err != nil {
				e.finish(r, "failed", "I could not adopt that spec: "+err.Error())
				return
			}
			adopted = true
		default:
			planMessage := message
			// Greenfield Engineer/Architect: the Stack Decision Record gates
			// planning (req 8.1-8.4). Wave 1 is structurally unreachable until the
			// human resolves it — because no plan (hence no wave) exists until this
			// returns. Prototype skips it and defaults to the classic stack.
			if pol.Mode != mode.Prototype && model.Greenfield() {
				e.activity(r, phaseStack, "")
				addendum, ok := e.resolveStackDecision(ctx, r, pol, model, message, projectMemory, hotClient, coldClient)
				if !ok {
					e.say(r, "Stopped. Progress so far is saved — say continue when you want me to pick it back up.")
					e.finish(r, "stopped", "")
					return
				}
				planMessage += addendum
			} else if resume {
				e.finish(r, "failed", "I could not resume: the durable plan is missing.")
				return
			}
			// Plan-shape authoring is a decision phase: N divergent candidates
			// generated hot, judged cold (req 10.2). Workers implement cold.
			e.activity(r, phasePlanning, "")
			plan, err = orchestrator.PlanFromModelDivergent(ctx, hotClient, coldClient, planMessage, summary, pol.Render(), pol.DecisionCandidates)
			if err != nil {
				e.finish(r, "failed", "I could not plan that request: "+err.Error())
				return
			}
		}
	}
	e.publish(r, "plan.created", map[string]interface{}{
		"goal": plan.Goal, "tasks": planTasks(plan), "mode": string(m),
	})
	if adopted {
		// The ledger records the source (req 11.1): the plan was adopted from a
		// spec document, not authored from the prose request.
		src := specSource
		if src == "" {
			src = "pasted"
		}
		e.publish(r, "plan.adopted", map[string]interface{}{"source": src, "goal": plan.Goal, "tasks": len(plan.Tasks)})
	}

	// UI-bearing runs author a Design Language Record before the first UI task
	// (req 9). Engineer/Architect block on approve/override; Prototype surfaces
	// an informational card and proceeds. The resolved record binds every UI
	// sheet and the gate screens turn-ins for drift. Durable, so a resume never
	// re-asks.
	var designLanguage string
	if uiBearing(plan, model) {
		if cerr := ensureDecisionClients(); cerr != nil {
			e.finish(r, "failed", "I could not reach the model: "+cerr.Error())
			return
		}
		e.activity(r, phaseDesign, "")
		dc, ok := e.resolveDesignDecision(ctx, r, pol, model, plan, message, projectMemory, hotClient, coldClient)
		if !ok {
			e.say(r, "Stopped. Progress so far is saved — say continue when you want me to pick it back up.")
			e.finish(r, "stopped", "")
			return
		}
		designLanguage = dc
	}

	var rules *policy.Rules
	if e.opts.RulesDir != "" {
		if loaded, err := policy.LoadRules(e.opts.RulesDir, model); err == nil {
			rules = loaded
		}
	}

	// The supervised loop: fresh orchestrator over the same durable state per
	// attempt; classification decides retry vs stop (never blind respawn).
	for attempt := 1; ; attempt++ {
		res, err := e.runOrchestrator(ctx, r, st, plan, pol, rules, designLanguage, projectMemory)
		switch {
		case err == nil && res.StopAsk != "":
			// Pause on the answer channel rather than ending: the run stays
			// alive awaiting the user's answer (req 12.1). ctx cancel while
			// parked is an honest stop.
			d, ok := e.pauseForInput(ctx, r, res.StopAsk)
			if !ok {
				e.say(r, "Stopped. Progress so far is saved — say continue when you want me to pick it back up.")
				e.finish(r, "stopped", "")
				return
			}
			if aerr := e.applyAnswer(r.convID, st, plan, d); aerr != nil {
				e.finish(r, "failed", "I could not apply your answer: "+aerr.Error())
				return
			}
			if reloaded, lerr := orchestrator.LoadPlan(st.Root()); lerr == nil {
				plan = reloaded
			}
			r.setStatus("running")
			e.activity(r, phaseContinuing, "")
			_ = e.mutateLedger(r.convID, func(led *ledger) { led.Status = "running" })
			// A human answer is new information, not a failure: restart the
			// attempt budget for the answered retry.
			attempt = 0
			continue
		case err == nil:
			usage := r.getUsage()
			e.publish(r, "plan.completed", map[string]interface{}{
				"done": res.Done, "failed": res.Failed,
				"usage": map[string]interface{}{
					"prompt_tokens":     usage.PromptTokens,
					"completion_tokens": usage.CompletionTokens,
					"total_tokens":      usage.TotalTokens,
				},
			})
			// Preview is a deliverable (req 7.1): a completed plan provisions an
			// on-demand sandbox preview in the background so the client shows a
			// working app the moment it's ready. A failed plan is not previewed.
			if len(res.Failed) == 0 && e.preview.Enabled() {
				e.activity(r, phasePreviewing, "")
				go e.preview.Provision(context.Background(), preview.Request{ConvID: r.convID, Root: r.root})
			}
			e.say(r, completionMessage(pol, plan, res))
			status := "completed"
			if len(res.Failed) > 0 {
				status = "failed"
			}
			e.finish(r, status, "")
			return
		case errors.Is(err, context.Canceled) || r.getStatus() == "stopped":
			e.say(r, "Stopped. Progress so far is saved — say continue when you want me to pick it back up.")
			e.finish(r, "stopped", "")
			return
		case delegate.ClassOf(err) == delegate.ClassDeterministic:
			e.say(r, "I hit a wall that retrying will not fix: "+err.Error())
			e.finish(r, "failed", "")
			return
		case attempt > e.opts.MaxRespawns:
			e.say(r, "That kept failing after several attempts. Progress so far is saved. Last error: "+err.Error())
			e.finish(r, "failed", "")
			return
		default:
			e.opts.Logf("codyd: run %s attempt %d: %v — respawning over durable state", r.id, attempt, err)
			select {
			case <-ctx.Done():
				e.finish(r, "stopped", "")
				return
			case <-time.After(time.Duration(attempt) * 750 * time.Millisecond):
			}
			// Reload the durable plan so the fresh orchestrator resumes at
			// the correct next task with its lean window reconstructed.
			if reloaded, lerr := orchestrator.LoadPlan(st.Root()); lerr == nil {
				plan = reloaded
			}
		}
	}
}

// runOrchestrator wires one orchestrator instance over the durable state.
func (e *Engine) runOrchestrator(ctx context.Context, r *run, st *contract.Store, plan *orchestrator.Plan, pol mode.Policy, rules *policy.Rules, designLanguage, projectMemory string) (*orchestrator.Result, error) {
	var adjudicator *cassandra.Adjudicator
	if !e.opts.DisableAdjudication {
		cfg := pol.OrchestratorLLM(e.opts.GatewayURL, e.opts.ActorDID)
		cfg.Temperature = 0
		cfg.MaxTokens = 1024
		if client, err := llm.New(cfg); err == nil {
			adjudicator = &cassandra.Adjudicator{Primary: gate.NewLLMDecoder(client)}
		}
	}
	var progress *checkpoint.Progress
	if e.opts.Cortex != nil {
		progress = checkpoint.NewProgress(e.opts.Cortex, r.convID)
	}
	pm := e.projectMemory(r.projectID)
	o, err := orchestrator.New(orchestrator.Options{
		Root:          r.root,
		Plan:          plan,
		Store:         st,
		Progress:      progress,
		Worker:        e.workerFunc(pol, r),
		Rules:         rules,
		ModePolicy:    pol.Render(),
		Adjudicator:    adjudicator,
		SpecFiles:      pol.PlanningDepth == mode.PlanSpecFiles,
		DesignLanguage: designLanguage,
		ExtraGrounding: projectMemory,
		OnAccepted: func(taskID, summary string) {
			if pm == nil {
				return
			}
			if err := pm.Record("task", taskID+": "+summary); err != nil {
				e.opts.Logf("codyd: project memory %s: %v", r.projectID, err)
			}
		},
		MaxAttempts:   e.opts.MaxAttempts,
		VerifyTimeout: e.opts.VerifyTimeout,
		Observer: func(event string, fields map[string]interface{}) {
			e.publish(r, event, fields)
			// Mirror the loop's milestones onto the live activity spine so the
			// client shows "Building <task>" / "Checking the work" without any
			// orchestrator/worker/sheet jargon.
			switch event {
			case "task.started":
				title, _ := fields["title"].(string)
				e.activity(r, phaseWorking, title)
			case "task.turnin":
				e.activity(r, phaseVerifying, "")
			}
		},
		Steers: func() []string { return e.directiveTexts(r.convID) },
	})
	if err != nil {
		return nil, delegate.Mark(delegate.ClassDeterministic, err)
	}
	return o.Run(ctx)
}

// workerFunc dispatches one REAL fresh-context worker per sheet. Each worker is
// handed the ExtraTools bridge (browser/fetch/web-search) so a UI task can
// capture a real screenshot as gate evidence (req 13.1); the bridge is
// boot-safe, so an unconfigured service simply omits or degrades a tool.
func (e *Engine) workerFunc(pol mode.Policy, r *run) orchestrator.WorkerFunc {
	return func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
		client, err := llm.New(pol.WorkerLLM(e.opts.GatewayURL, e.opts.ActorDID))
		if err != nil {
			return nil, delegate.Mark(delegate.ClassDeterministic, err)
		}
		bridge := tools.New(tools.Config{
			Root:         r.root,
			BrowserURL:   e.opts.BrowserURL,
			BrowserToken: e.opts.BrowserToken,
			SearxngURL:   e.opts.SearxngURL,
			SearxngToken: e.opts.SearxngToken,
		})
		w, err := worker.New(worker.Options{
			Sheet:         sheet,
			Grounding:     grounding,
			Root:          r.root,
			Client:        client,
			VerifyTimeout: e.opts.VerifyTimeout,
			ExtraTools:    bridge.Tools(),
			ExtraDispatch: bridge.Dispatch,
			Skills:        e.skills,
			ScaffoldDir:   e.opts.ScaffoldDir,
		})
		if err != nil {
			return nil, delegate.Mark(delegate.ClassDeterministic, err)
		}
		report, err := w.Run(ctx)
		if report != nil {
			// The run ledger accumulates worker token spend across every
			// attempt — accepted or not, real spend is real.
			r.addUsage(report.Usage)
		}
		return report, err
	}
}

// publish emits one event on the run topic (phase "cody").
func (e *Engine) publish(r *run, typ string, fields map[string]interface{}) {
	e.broker.publish(r.id, typ, "cody", fields)
}

// Run activity phases — the plain-language lifecycle the client's live spine
// renders so the surface is never blank while Cody works. Copy is
// result-oriented: no orchestrator/worker/sheet jargon ever reaches the user.
const (
	phaseUnderstanding = "understanding"
	phaseStack         = "stack"
	phasePlanning      = "planning"
	phaseDesign        = "design"
	phaseWorking       = "working"
	phaseVerifying     = "verifying"
	phasePreviewing    = "previewing"
	phaseContinuing    = "continuing"
)

var phaseLabels = map[string]string{
	phaseUnderstanding: "Reading your workspace",
	phaseStack:         "Choosing the best stack",
	phasePlanning:      "Planning the work",
	phaseDesign:        "Designing the interface",
	phaseWorking:       "Building",
	phaseVerifying:     "Checking the work",
	phasePreviewing:    "Preparing a preview",
	phaseContinuing:    "Continuing",
}

// activity publishes a milestone run.activity event: the current phase, a
// plain-language label, an optional detail (e.g. the task being built), and
// elapsed time since the run began. It is a pure side-channel — no loop, gate,
// or plan-determinism effect — and (once whitelisted in trace.go) persists so
// the client rebuilds the last-known phase on reopen.
func (e *Engine) activity(r *run, phase, detail string) {
	r.setActivity(phase, detail)
	e.publish(r, "run.activity", map[string]interface{}{
		"phase":      phase,
		"label":      phaseLabels[phase],
		"detail":     detail,
		"elapsed_ms": time.Since(r.started).Milliseconds(),
	})
}

// heartbeatInterval paces the liveness heartbeat during long LLM phases. A
// package var so tests can compress time.
var heartbeatInterval = 10 * time.Second

// heartbeat starts the run's cancel-safe liveness ticker: while a long phase
// (SDR / plan / DLR authoring, worker execution) runs between milestone
// boundaries, it periodically re-emits the run's CURRENT phase as a
// run.activity event carrying heartbeat=true, so the client shows continuous
// progress instead of a frozen state. Heartbeats are live-only liveness
// signals — the heartbeat flag keeps them out of the durable trace (milestone
// activities persist; ticks do not). Ticks are suppressed while the run is
// parked awaiting human input (needs_input is its own honest surface) and
// before the first boundary sets a phase. The ticker stops on ctx cancel or
// when the returned stop func runs at the run's terminal; stop is idempotent
// and returns after the goroutine has fully unwound.
func (e *Engine) heartbeat(ctx context.Context, r *run) (stop func()) {
	hctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		t := time.NewTicker(heartbeatInterval)
		defer t.Stop()
		for {
			select {
			case <-hctx.Done():
				return
			case <-t.C:
				phase, detail := r.currentActivity()
				if phase == "" || r.isAwaiting() || r.getStatus() != "running" {
					continue
				}
				e.publish(r, "run.activity", map[string]interface{}{
					"phase":      phase,
					"label":      phaseLabels[phase],
					"detail":     detail,
					"elapsed_ms": time.Since(r.started).Milliseconds(),
					"heartbeat":  true,
				})
			}
		}
	}()
	return func() { cancel(); <-done }
}

// projectMemory returns the project's cortex-backed memory surface, nil when
// cortex is not wired.
func (e *Engine) projectMemory(projectID string) *checkpoint.ProjectMemory {
	if e.opts.Cortex == nil {
		return nil
	}
	return checkpoint.NewProjectMemory(e.opts.Cortex, projectID)
}

// projectMemoryRecall is how many recent project records ground a run.
const projectMemoryRecall = 12

// recallProjectMemory renders the project's recent memory (stack/design
// decisions + accepted turn-ins) as the grounding section, "" when there is
// none.
func (e *Engine) recallProjectMemory(projectID string) string {
	pm := e.projectMemory(projectID)
	if pm == nil {
		return ""
	}
	recs, err := pm.Recent(projectMemoryRecall)
	if err != nil {
		e.opts.Logf("codyd: project memory recall %s: %v", projectID, err)
		return ""
	}
	return checkpoint.RenderProjectMemory(recs)
}

// projectHasLiveRun reports whether any run rooted at root is currently running
// — the "a worker is alive" guard that refuses a destructive restore (req 14.1).
func (e *Engine) projectHasLiveRun(root string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for _, r := range e.runs {
		if r.root == root && r.getStatus() == "running" {
			return true
		}
	}
	return false
}

// publishToConversation best-effort emits an event on a conversation's run topic
// so a workspace-level action (snapshot/restore) lands in the live timeline and
// the durable trace. It is a no-op when the conversation has no live topic — the
// action still succeeds; only the trace annotation is skipped.
func (e *Engine) publishToConversation(convID, typ string, fields map[string]interface{}) {
	if strings.TrimSpace(convID) == "" {
		return
	}
	led, err := e.readLedger(convID)
	if err != nil || !e.broker.has(led.RunID) {
		return
	}
	e.broker.publish(led.RunID, typ, "cody", fields)
}

// say publishes a user-facing assistant message.
func (e *Engine) say(r *run, text string) {
	e.publish(r, "chat.assistant", map[string]interface{}{"text": text, "final": true})
}

// finish records the terminal, updates the ledger (status + this run's token
// spend folded into the conversation total), and closes the topic.
func (e *Engine) finish(r *run, status, note string) {
	if note != "" {
		e.say(r, note)
	}
	if r.getStatus() != "stopped" || status == "stopped" {
		r.setStatus(status)
	}
	usage := r.getUsage()
	if err := e.mutateLedger(r.convID, func(led *ledger) {
		led.Status = status
		led.Usage.Add(usage)
	}); err != nil {
		e.opts.Logf("codyd: ledger %s: %v", r.convID, err)
	}
	e.publish(r, "message.complete", map[string]interface{}{"status": status})
}

// completionMessage renders the terminal report in the mode's register —
// result, not protocol: no orchestrator/worker/sheet jargon reaches the user.
func completionMessage(pol mode.Policy, plan *orchestrator.Plan, res *orchestrator.Result) string {
	if len(res.Failed) > 0 {
		var b strings.Builder
		b.WriteString("Partly done — honest status: ")
		fmt.Fprintf(&b, "%d of %d steps landed.", len(res.Done), len(res.Done)+len(res.Failed))
		for _, id := range res.Failed {
			if t := plan.Get(id); t != nil {
				b.WriteString(" Could not finish: " + t.Title + ".")
			}
		}
		b.WriteString(" Everything completed so far is saved.")
		return b.String()
	}
	if pol.Register == mode.RegisterOutcome {
		return "Done — " + plan.Goal + ". Everything is verified and ready to use."
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Done: %s. Completed %d task(s), each independently verified", plan.Goal, len(res.Done))
	var cmds []string
	seen := map[string]bool{}
	for _, t := range plan.Tasks {
		for _, v := range t.Verify {
			if !seen[v] {
				seen[v] = true
				cmds = append(cmds, v)
			}
		}
	}
	if len(cmds) > 0 {
		b.WriteString(" (" + strings.Join(cmds, "; ") + ")")
	}
	b.WriteString(".")
	return b.String()
}

func planTasks(p *orchestrator.Plan) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(p.Tasks))
	for _, t := range p.Tasks {
		out = append(out, map[string]interface{}{
			"id": t.ID, "title": t.Title, "wave": t.Wave, "status": string(t.Status),
		})
	}
	return out
}

// firstErr returns the first non-nil error, or nil.
func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}
