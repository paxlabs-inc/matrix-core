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
	// MaxAttempts bounds per-task re-dispatch (default 3).
	MaxAttempts int
	// MaxRespawns bounds orchestrator respawns per run (default 2).
	MaxRespawns int
	// VerifyTimeout bounds one verification command.
	VerifyTimeout time.Duration
	// DisableAdjudication turns the goal-vs-outcome LLM verdict off (the
	// structural gate still holds). For constrained deployments.
	DisableAdjudication bool
	// Logf receives diagnostics; nil discards.
	Logf func(format string, args ...interface{})
}

// Engine owns the sessions, the broker, and the durable trace.
type Engine struct {
	opts   EngineOptions
	broker *broker
	trace  *traceStore

	mu       sync.Mutex
	sessions map[string]*session
	runs     map[string]*run
}

// run is one live plan execution.
type run struct {
	id     string
	convID string
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	status string // running | completed | failed | stopped | needs_input
}

func (r *run) setStatus(s string) { r.mu.Lock(); r.status = s; r.mu.Unlock() }
func (r *run) getStatus() string  { r.mu.Lock(); defer r.mu.Unlock(); return r.status }

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
		sessions: map[string]*session{},
		runs:     map[string]*run{},
	}
	e.broker.setTap(e.recordTrace)
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

// Submit dispatches (or attaches to) the conversation's plan run. Returns the
// run id and whether it was freshly dispatched.
func (e *Engine) Submit(convID, message, modeName string) (string, bool, error) {
	m, err := mode.Parse(modeName)
	if err != nil {
		return "", false, err
	}
	if modeName == "" {
		m = e.opts.DefaultMode
	}
	s := e.session(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.getStatus() == "running" {
		// One live plan per conversation: attach to the work already underway.
		return s.active.id, false, nil
	}
	r := &run{id: newRunID(), convID: convID, done: make(chan struct{}), status: "running"}
	e.registerRun(r)
	e.broker.ensure(r.id) // before Submit returns: no dispatch->subscribe race
	s.active = r
	if err := e.writeLedger(convID, ledger{RunID: r.id, Message: message, Mode: string(m), Status: "running", UpdatedAt: time.Now().UTC()}); err != nil {
		return "", false, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go e.drive(ctx, r, message, m, false)
	return r.id, true, nil
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
		m, err := mode.Parse(led.Mode)
		if err != nil {
			m = e.opts.DefaultMode
		}
		s := e.session(convID)
		s.mu.Lock()
		if s.active != nil && s.active.getStatus() == "running" {
			s.mu.Unlock()
			continue
		}
		r := &run{id: led.RunID, convID: convID, done: make(chan struct{}), status: "running"}
		e.registerRun(r)
		e.broker.ensure(r.id)
		s.active = r
		ctx, cancel := context.WithCancel(context.Background())
		r.cancel = cancel
		go e.drive(ctx, r, led.Message, m, true)
		s.mu.Unlock()
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
	Status    string    `json:"status"`
	UpdatedAt time.Time `json:"updated_at"`
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
func (e *Engine) drive(ctx context.Context, r *run, message string, m mode.Mode, resume bool) {
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
	model, err := workspace.LoadOrScan(e.opts.WorkspaceRoot)
	if err != nil {
		e.finish(r, "failed", "I could not read the workspace: "+err.Error())
		return
	}

	// The plan: resumed from the durable store, or authored by the planner.
	plan, err := orchestrator.LoadPlan(st.Root())
	if err != nil || plan == nil {
		if resume {
			e.finish(r, "failed", "I could not resume: the durable plan is missing.")
			return
		}
		orchClient, cerr := llm.New(pol.OrchestratorLLM(e.opts.GatewayURL, e.opts.ActorDID))
		if cerr != nil {
			e.finish(r, "failed", "I could not reach the model: "+cerr.Error())
			return
		}
		plan, err = orchestrator.PlanFromModel(ctx, orchClient, message, model.Summary(), pol.Render())
		if err != nil {
			e.finish(r, "failed", "I could not plan that request: "+err.Error())
			return
		}
	}
	e.publish(r, "plan.created", map[string]interface{}{
		"goal": plan.Goal, "tasks": planTasks(plan), "mode": string(m),
	})

	var rules *policy.Rules
	if e.opts.RulesDir != "" {
		if loaded, err := policy.LoadRules(e.opts.RulesDir, model); err == nil {
			rules = loaded
		}
	}

	// The supervised loop: fresh orchestrator over the same durable state per
	// attempt; classification decides retry vs stop (never blind respawn).
	for attempt := 1; ; attempt++ {
		res, err := e.runOrchestrator(ctx, r, st, plan, pol, rules)
		switch {
		case err == nil && res.StopAsk != "":
			e.say(r, "I need your input before continuing: "+res.StopAsk)
			e.finish(r, "needs_input", "")
			return
		case err == nil:
			e.publish(r, "plan.completed", map[string]interface{}{
				"done": res.Done, "failed": res.Failed,
			})
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
func (e *Engine) runOrchestrator(ctx context.Context, r *run, st *contract.Store, plan *orchestrator.Plan, pol mode.Policy, rules *policy.Rules) (*orchestrator.Result, error) {
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
		Root:          e.opts.WorkspaceRoot,
		Plan:          plan,
		Store:         st,
		Progress:      progress,
		Worker:        e.workerFunc(pol),
		Rules:         rules,
		ModePolicy:    pol.Render(),
		Adjudicator:   adjudicator,
		SpecFiles:     pol.PlanningDepth == mode.PlanSpecFiles,
		MaxAttempts:   e.opts.MaxAttempts,
		VerifyTimeout: e.opts.VerifyTimeout,
		Observer: func(event string, fields map[string]interface{}) {
			e.publish(r, event, fields)
		},
	})
	if err != nil {
		return nil, delegate.Mark(delegate.ClassDeterministic, err)
	}
	return o.Run(ctx)
}

// workerFunc dispatches one REAL fresh-context worker per sheet.
func (e *Engine) workerFunc(pol mode.Policy) orchestrator.WorkerFunc {
	return func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
		client, err := llm.New(pol.WorkerLLM(e.opts.GatewayURL, e.opts.ActorDID))
		if err != nil {
			return nil, delegate.Mark(delegate.ClassDeterministic, err)
		}
		w, err := worker.New(worker.Options{
			Sheet:         sheet,
			Grounding:     grounding,
			Root:          e.opts.WorkspaceRoot,
			Client:        client,
			VerifyTimeout: e.opts.VerifyTimeout,
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

func newRunID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return "run-" + hex.EncodeToString(b[:])
}
