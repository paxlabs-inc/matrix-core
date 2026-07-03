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
}

// run is one live plan execution.
type run struct {
	id        string
	convID    string
	projectID string
	root      string // the project's workspace subtree
	cancel    context.CancelFunc
	done      chan struct{}

	mu     sync.Mutex
	status string // running | completed | failed | stopped | needs_input
	// awaiting + answers implement the needs_input pause: while parked, awaiting
	// is true and an /answer is delivered on the buffered channel. Guarded by mu.
	awaiting bool
	answers  chan directive
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
// client rebuilds the timeline on reopen.
func (e *Engine) recordTrace(id string, ev Event) {
	if !traceWorkspaceTypes[ev.Type] {
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
	if s.active != nil && s.active.getStatus() == "running" {
		// One live plan per conversation: attach to the work already underway.
		return s.active.id, false, nil
	}
	r := &run{id: newRunID(), convID: convID, projectID: proj.ID, root: proj.Root, done: make(chan struct{}), status: "running"}
	e.registerRun(r)
	e.broker.ensure(r.id) // before Submit returns: no dispatch->subscribe race
	s.active = r
	if err := e.writeLedger(convID, ledger{RunID: r.id, Message: message, Mode: string(m), ProjectID: proj.ID, Root: proj.Root, Spec: spec, SpecSource: specSource, Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go e.drive(ctx, r, message, m, false, spec, specSource)
	return r.id, true, nil
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
	RunID     string    `json:"run_id"`
	Message   string    `json:"message"`
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

// drive executes one plan run on a background context, supervised: the
// orchestrator is respawned over the same durable state on transient
// failures, deterministic failures stop-and-ask, and every terminal is
// honest. The client connection is never load-bearing.
func (e *Engine) drive(ctx context.Context, r *run, message string, m mode.Mode, resume bool, spec, specSource string) {
	defer close(r.done)
	defer func() {
		e.broker.closeRun(r.id)
		time.AfterFunc(2*time.Minute, func() {
			e.broker.drop(r.id)
			e.mu.Lock()
			delete(e.runs, r.id)
			e.mu.Unlock()
		})
	}()

	pol := mode.For(m)
	if e.opts.OrchestratorModel != "" {
		pol.OrchestratorModel = e.opts.OrchestratorModel
	}
	if e.opts.WorkerModel != "" {
		pol.WorkerModel = e.opts.WorkerModel
	}

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
			plan, err = orchestrator.AdoptSpecDivergent(ctx, hotClient, coldClient, spec, model.Summary(), pol.Render(), pol.DecisionCandidates)
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
				addendum, ok := e.resolveStackDecision(ctx, r, pol, model, message, hotClient, coldClient)
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
			plan, err = orchestrator.PlanFromModelDivergent(ctx, hotClient, coldClient, planMessage, model.Summary(), pol.Render(), pol.DecisionCandidates)
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
		dc, ok := e.resolveDesignDecision(ctx, r, pol, model, plan, message, hotClient, coldClient)
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
		res, err := e.runOrchestrator(ctx, r, st, plan, pol, rules, designLanguage)
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
			if led, lerr := e.readLedger(r.convID); lerr == nil {
				led.Status = "running"
				led.UpdatedAt = time.Now().UTC()
				_ = e.writeLedger(r.convID, led)
			}
			// A human answer is new information, not a failure: restart the
			// attempt budget for the answered retry.
			attempt = 0
			continue
		case err == nil:
			e.publish(r, "plan.completed", map[string]interface{}{
				"done": res.Done, "failed": res.Failed,
			})
			// Preview is a deliverable (req 7.1): a completed plan provisions an
			// on-demand sandbox preview in the background so the client shows a
			// working app the moment it's ready. A failed plan is not previewed.
			if len(res.Failed) == 0 && e.preview.Enabled() {
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
func (e *Engine) runOrchestrator(ctx context.Context, r *run, st *contract.Store, plan *orchestrator.Plan, pol mode.Policy, rules *policy.Rules, designLanguage string) (*orchestrator.Result, error) {
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
	o, err := orchestrator.New(orchestrator.Options{
		Root:          r.root,
		Plan:          plan,
		Store:         st,
		Progress:      progress,
		Worker:        e.workerFunc(pol, r.root),
		Rules:         rules,
		ModePolicy:    pol.Render(),
		Adjudicator:    adjudicator,
		SpecFiles:      pol.PlanningDepth == mode.PlanSpecFiles,
		DesignLanguage: designLanguage,
		MaxAttempts:    e.opts.MaxAttempts,
		VerifyTimeout:  e.opts.VerifyTimeout,
		Observer: func(event string, fields map[string]interface{}) {
			e.publish(r, event, fields)
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
func (e *Engine) workerFunc(pol mode.Policy, root string) orchestrator.WorkerFunc {
	return func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
		client, err := llm.New(pol.WorkerLLM(e.opts.GatewayURL, e.opts.ActorDID))
		if err != nil {
			return nil, delegate.Mark(delegate.ClassDeterministic, err)
		}
		bridge := tools.New(tools.Config{
			Root:         root,
			BrowserURL:   e.opts.BrowserURL,
			BrowserToken: e.opts.BrowserToken,
			SearxngURL:   e.opts.SearxngURL,
			SearxngToken: e.opts.SearxngToken,
		})
		w, err := worker.New(worker.Options{
			Sheet:         sheet,
			Grounding:     grounding,
			Root:          root,
			Client:        client,
			VerifyTimeout: e.opts.VerifyTimeout,
			ExtraTools:    bridge.Tools(),
			ExtraDispatch: bridge.Dispatch,
		})
		if err != nil {
			return nil, delegate.Mark(delegate.ClassDeterministic, err)
		}
		return w.Run(ctx)
	}
}

// publish emits one event on the run topic (phase "cody").
func (e *Engine) publish(r *run, typ string, fields map[string]interface{}) {
	e.broker.publish(r.id, typ, "cody", fields)
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

// finish records the terminal, updates the ledger, and closes the topic.
func (e *Engine) finish(r *run, status, note string) {
	if note != "" {
		e.say(r, note)
	}
	if r.getStatus() != "stopped" || status == "stopped" {
		r.setStatus(status)
	}
	if led, err := e.readLedger(r.convID); err == nil {
		led.Status = status
		led.UpdatedAt = time.Now().UTC()
		if err := e.writeLedger(r.convID, led); err != nil {
			e.opts.Logf("codyd: ledger %s: %v", r.convID, err)
		}
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
