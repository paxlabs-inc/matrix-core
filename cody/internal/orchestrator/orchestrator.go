// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package orchestrator is Cody prime: it Plans, Specs, and Delegates — and it
// NEVER writes code. Its cycle: understand (workspace model + cortex recall)
// -> plan (waved task list) -> spec the next eligible task into a
// self-contained TaskSheet -> delegate to exactly ONE worker -> independently
// verify the turn-in (re-running the sheet's verification commands itself)
// -> accept (checkpoint to cortex, discard the worker transcript) or
// re-dispatch bounded with classified feedback -> loop. Implementation noise
// never enters its window: after N accepted tasks the context holds the plan,
// the sheets, and the turn-in reports — nothing else. Structurally, the
// orchestrator owns no edit engine and no exec tool; its only executions are
// verification re-runs and read-only inspection.
package orchestrator

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"matrix/cassandra"
	"matrix/cody/internal/checkpoint"
	"matrix/cody/internal/contract"
	"matrix/cody/internal/delegate"
	"matrix/cody/internal/gate"
	"matrix/cody/internal/llm"
	"matrix/cody/internal/policy"
	"matrix/cody/internal/verify"
	"matrix/cody/internal/workspace"
)

// WorkerFunc dispatches ONE fresh-context worker for one sheet and returns
// its turn-in report. The worker's transcript lives and dies inside the call
// — only the report crosses this seam, which is what keeps the orchestrator
// lean by construction.
type WorkerFunc func(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error)

// Options configures the orchestrator.
type Options struct {
	// Root is the workspace root.
	Root string
	// Plan is the waved task list to execute.
	Plan *Plan
	// Store persists sheets + reports (+ the plan) durably.
	Store *contract.Store
	// Progress writes per-accepted-task checkpoints to cortex. Optional but
	// strongly recommended; resume depends on it.
	Progress *checkpoint.Progress
	// Worker dispatches one worker per sheet.
	Worker WorkerFunc
	// Rules, when set, injects the applicable rule files into every sheet's
	// constraints.
	Rules *policy.Rules
	// ModePolicy is the active mode rendered as prose for sheet constraints.
	ModePolicy string
	// SpecFiles, when true (Architect mode), keeps durable in-workspace spec
	// files (.cody/spec/requirements.md + tasks.md) in sync with the plan.
	SpecFiles bool
	// Adjudicator, when set, renders the Cassandra-style goal-vs-outcome
	// verdict on every done claim (after the green re-run). Optional: without
	// it the gate is the structural floor alone (re-run + screens).
	Adjudicator *cassandra.Adjudicator
	// MaxAttempts bounds re-dispatch per task (default 3).
	MaxAttempts int
	// VerifyTimeout bounds one independent verification command.
	VerifyTimeout time.Duration
	// Observer, when set, receives loop progress events (task started, sheet
	// authored, turn-in, accepted, rejected, failed) for surfacing. It is a
	// pure side-channel: it never influences the loop.
	Observer func(event string, fields map[string]interface{})
}

// Result is the loop outcome — always honest: done, failed, and why it
// stopped if it stopped early.
type Result struct {
	Done   []string
	Failed []string
	// StopAsk carries the stop-and-ask reason for a deterministic failure;
	// empty when the loop ran to completion or a bounded failure.
	StopAsk string
	// Window is the orchestrator's final lean context.
	Window []llm.Message
}

// Orchestrator drives the plan. It deliberately owns NO edit engine and NO
// exec surface — only a verification runner (re-runs) and read-only
// workspace inspection.
type Orchestrator struct {
	opts   Options
	runner *verify.Runner
	window []llm.Message
	// inFlight guards the one-worker-at-a-time invariant.
	inFlight int32
}

// New builds an orchestrator.
func New(opts Options) (*Orchestrator, error) {
	if opts.Plan == nil {
		return nil, errors.New("orchestrator: nil plan")
	}
	if err := opts.Plan.Validate(); err != nil {
		return nil, err
	}
	if opts.Worker == nil {
		return nil, errors.New("orchestrator: nil worker func")
	}
	if opts.Store == nil {
		return nil, errors.New("orchestrator: nil contract store")
	}
	runner, err := verify.NewRunner(opts.Root)
	if err != nil {
		return nil, err
	}
	if opts.VerifyTimeout > 0 {
		runner.Timeout = opts.VerifyTimeout
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = 3
	}
	return &Orchestrator{opts: opts, runner: runner}, nil
}

// Run executes the loop until the plan completes, a task exhausts its
// attempts, or a deterministic failure requires the user.
func (o *Orchestrator) Run(ctx context.Context) (*Result, error) {
	res := &Result{}

	// --- understand: workspace model + cortex recall ---------------------
	model, err := workspace.LoadOrScan(o.opts.Root)
	if err != nil {
		return nil, fmt.Errorf("orchestrator: workspace model: %w", err)
	}
	grounding := model.Summary()

	// A task caught in_progress by a crash is re-dispatched from its durable
	// sheet: reset it to pending so NextEligible picks it back up.
	for _, t := range o.opts.Plan.Tasks {
		if t.Status == TaskInProgress {
			t.Status = TaskPending
		}
	}
	if o.opts.Progress != nil {
		done, err := o.opts.Progress.Done()
		if err != nil {
			return nil, fmt.Errorf("orchestrator: cortex recall: %w", err)
		}
		for id := range done {
			if t := o.opts.Plan.Get(id); t != nil && t.Status == TaskPending {
				t.Status = TaskDone
			}
		}
	}

	o.window = []llm.Message{
		llm.SystemMessage("You are Cody prime: plan, spec, delegate, verify. You never write code."),
		llm.UserMessage(o.opts.Plan.Render()),
	}
	if err := o.reconstructWindow(); err != nil {
		return nil, err
	}
	if err := o.savePlan(); err != nil {
		return nil, err
	}

	// --- the loop ---------------------------------------------------------
	for {
		if err := ctx.Err(); err != nil {
			return nil, err // killed mid-plan; durable state carries the resume
		}
		task := o.opts.Plan.NextEligible()
		if task == nil {
			break
		}
		task.Status = TaskInProgress
		if err := o.savePlan(); err != nil {
			return nil, err
		}
		o.emit("task.started", map[string]interface{}{"task_id": task.ID, "title": task.Title, "wave": task.Wave})

		accepted, stopAsk, err := o.runTask(ctx, task, grounding)
		if err != nil {
			return nil, err
		}
		switch {
		case accepted:
			task.Status = TaskDone
			res.Done = append(res.Done, task.ID)
			o.emit("task.accepted", map[string]interface{}{"task_id": task.ID, "title": task.Title})
		case stopAsk != "":
			task.Status = TaskFailed
			res.Failed = append(res.Failed, task.ID)
			res.StopAsk = stopAsk
			o.emit("task.failed", map[string]interface{}{"task_id": task.ID, "reason": stopAsk, "stop_ask": true})
		default:
			task.Status = TaskFailed
			res.Failed = append(res.Failed, task.ID)
			o.emit("task.failed", map[string]interface{}{"task_id": task.ID, "reason": "attempt ceiling reached"})
		}
		if err := o.savePlan(); err != nil {
			return nil, err
		}
		if task.Status == TaskFailed {
			break // honest stop: dependents must not build on a failed task
		}
	}

	res.Window = o.window
	return res, nil
}

// runTask specs, delegates, verifies, and accepts (or re-dispatches) one
// task. Returns (accepted, stopAskReason, err).
func (o *Orchestrator) runTask(ctx context.Context, task *Task, grounding string) (bool, string, error) {
	// Fingerprint the tests as of TASK START: every attempt's turn-in is
	// screened against this one baseline (tests may never be weakened or
	// deleted to pass), so a rejected attempt's mutations can never poison
	// the reference for the next attempt.
	baseline, err := gate.CaptureTests(o.opts.Root)
	if err != nil {
		return false, "", fmt.Errorf("orchestrator: test baseline: %w", err)
	}

	feedback := ""
	for attempt := 1; attempt <= o.opts.MaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return false, "", err
		}
		// --- spec: author the self-contained sheet -----------------------
		sheet := o.specSheet(task, attempt, feedback)
		if err := o.opts.Store.SaveSheet(sheet); err != nil {
			return false, "", fmt.Errorf("orchestrator: persist sheet: %w", err)
		}
		o.append(llm.AssistantMessage(renderSheet(sheet)))
		o.emit("sheet.authored", map[string]interface{}{
			"task_id": sheet.TaskID, "attempt": attempt, "title": sheet.Title,
			"goal": sheet.Goal, "verify": strings.Join(sheet.Verify.Commands, "; "),
		})

		// --- delegate: exactly ONE worker ---------------------------------
		report, err := o.dispatch(ctx, sheet, grounding)
		if err != nil {
			switch delegate.ClassOf(err) {
			case delegate.ClassDeterministic:
				return false, "worker failed deterministically on task " + task.ID + ": " + err.Error(), nil
			default:
				// Transient (incl. a crashed/lost worker): a fresh worker is
				// dispatched from the same durable sheet on the next attempt.
				feedback = "the previous worker run was lost before turn-in (" + err.Error() + "); execute the sheet from scratch"
				o.append(llm.UserMessage(fmt.Sprintf("turn-in %s attempt %d: worker lost (%s); re-dispatching", task.ID, attempt, delegate.ClassOf(err))))
				continue
			}
		}
		report.Attempt = attempt
		if err := o.opts.Store.SaveReport(report); err != nil {
			// An invalid report is the worker's failure, not a crash.
			feedback = "your turn-in report was invalid: " + err.Error()
			o.append(llm.UserMessage(fmt.Sprintf("turn-in %s attempt %d: invalid report rejected", task.ID, attempt)))
			continue
		}
		o.append(llm.UserMessage(renderReport(report)))
		o.emit("task.turnin", map[string]interface{}{
			"task_id": report.TaskID, "attempt": attempt,
			"status": string(report.Status), "summary": report.Summary,
			"changes": changeRecords(report), "verification": evidenceRecords(report),
			"gaps": append([]string{}, report.Gaps...),
		})

		// --- verify: independent re-run; never take the worker's word ----
		verdict, rerun, err := o.adjudicate(ctx, sheet, report, baseline)
		if err != nil {
			return false, "", err
		}
		if verdict == "" {
			// Accepted: checkpoint to cortex; the worker transcript was
			// never here to discard — only the report persists.
			if o.opts.Progress != nil {
				if err := o.opts.Progress.Record(checkpoint.Checkpoint{
					TaskID:  task.ID,
					Attempt: attempt,
					Status:  "done",
					Summary: report.Summary,
				}); err != nil {
					return false, "", fmt.Errorf("orchestrator: checkpoint: %w", err)
				}
			}
			return true, "", nil
		}

		// --- reject: concrete feedback, bounded ---------------------------
		feedback = verdict
		if rerun != "" {
			feedback += "\nIndependent verification output:\n" + rerun
		}
		o.append(llm.UserMessage(fmt.Sprintf("turn-in %s attempt %d REJECTED: %s", task.ID, attempt, verdict)))
		o.emit("task.rejected", map[string]interface{}{"task_id": task.ID, "attempt": attempt, "verdict": verdict})
	}

	if o.opts.Progress != nil {
		_ = o.opts.Progress.Record(checkpoint.Checkpoint{
			TaskID:  task.ID,
			Attempt: o.opts.MaxAttempts,
			Status:  "failed",
			Summary: "attempt ceiling reached: " + strings.TrimSpace(feedback),
		})
	}
	return false, "", nil
}

// savePlan persists the plan durably, and — in Architect mode — keeps the
// in-workspace spec files in sync with it.
func (o *Orchestrator) savePlan() error {
	if err := SavePlan(o.opts.Store.Root(), o.opts.Plan); err != nil {
		return err
	}
	if o.opts.SpecFiles {
		return WriteSpecFiles(o.opts.Root, o.opts.Plan)
	}
	return nil
}

// reconstructWindow rebuilds the lean context after a restart: for every task
// checkpointed done in cortex, the persisted sheet digest and the latest
// turn-in report digest are replayed into the window in checkpoint order — so
// a resumed orchestrator holds exactly what a never-interrupted one would
// (plan + sheets + reports), and no worker transcript is ever reconstructable
// because none was ever persisted.
func (o *Orchestrator) reconstructWindow() error {
	if o.opts.Progress == nil {
		return nil
	}
	cps, err := o.opts.Progress.All()
	if err != nil {
		return fmt.Errorf("orchestrator: reconstruct window: %w", err)
	}
	seen := map[string]bool{}
	for _, cp := range cps {
		if cp.Status != "done" || seen[cp.TaskID] {
			continue
		}
		seen[cp.TaskID] = true
		if sheet, err := o.opts.Store.LoadSheet(cp.TaskID); err == nil {
			o.append(llm.AssistantMessage(renderSheet(sheet)))
		}
		if reports, err := o.opts.Store.LoadReports(cp.TaskID); err == nil && len(reports) > 0 {
			o.append(llm.UserMessage(renderReport(reports[len(reports)-1])))
		}
	}
	return nil
}

// dispatch runs the worker func under the one-at-a-time invariant.
func (o *Orchestrator) dispatch(ctx context.Context, sheet *contract.TaskSheet, grounding string) (*contract.TurnInReport, error) {
	if !atomic.CompareAndSwapInt32(&o.inFlight, 0, 1) {
		return nil, errors.New("orchestrator: a worker is already alive")
	}
	defer atomic.StoreInt32(&o.inFlight, 0)
	return o.opts.Worker(ctx, sheet, grounding)
}

// adjudicate decides a turn-in. Empty verdict = accept. The gate is layered:
// (1) the independent verification re-run — a report claiming done is never
// trusted, and acceptance is impossible without the orchestrator's own green
// record; (2) the deterministic constitution screens (weakened/deleted tests,
// do-not-touch); (3) the Cassandra-style goal-vs-outcome adjudication of the
// turn-in against the sheet's acceptance criteria.
func (o *Orchestrator) adjudicate(ctx context.Context, sheet *contract.TaskSheet, report *contract.TurnInReport, baseline gate.TestBaseline) (verdict, rerunOutput string, err error) {
	if report.TaskID != sheet.TaskID {
		return fmt.Sprintf("report task id %q does not match the sheet %q", report.TaskID, sheet.TaskID), "", nil
	}
	if report.Status != contract.StatusDone {
		gaps := strings.Join(report.Gaps, "; ")
		return "the worker reported " + string(report.Status) + " (honest partial): " + gaps, "", nil
	}
	// Layer 1: independent verification re-run — the orchestrator's ONLY
	// execution. The structural floor: no green record, no acceptance.
	results, err := o.runner.Run(ctx, verify.FromStrings(sheet.Verify.Commands))
	if err != nil {
		return "", "", fmt.Errorf("orchestrator: independent verify: %w", err)
	}
	if !verify.AllGreen(results) {
		var b strings.Builder
		for _, r := range results {
			if r.Green {
				continue
			}
			fmt.Fprintf(&b, "[RED exit %d] %s\n%s\n", r.Exit, r.Command.Cmd, r.Output)
		}
		return "the done claim did not survive independent verification", b.String(), nil
	}
	// Layer 2: deterministic constitution screens over the real workspace.
	if v := gate.Screen(o.opts.Root, baseline, sheet, report); v != "" {
		return v, "", nil
	}
	// Layer 3: goal-vs-outcome adjudication (never string-matching the
	// report). Judged against the orchestrator's own green record as evidence.
	if v := gate.Adjudicate(ctx, o.opts.Adjudicator, sheet, report, renderRerun(results)); v != "" {
		return v, "", nil
	}
	return "", "", nil
}

// renderRerun digests the orchestrator's own verification results as
// adjudication evidence.
func renderRerun(results []verify.Result) string {
	var b strings.Builder
	for _, r := range results {
		state := "GREEN"
		if !r.Green {
			state = "RED"
		}
		out := r.Output
		if len(out) > 2048 {
			out = out[:2048] + "..."
		}
		fmt.Fprintf(&b, "[%s exit %d] %s\n%s\n", state, r.Exit, r.Command.Cmd, out)
	}
	return b.String()
}

// specSheet authors the self-contained sheet for a plan task.
func (o *Orchestrator) specSheet(task *Task, attempt int, feedback string) *contract.TaskSheet {
	constitution := append([]string{}, defaultConstitution...)
	var rulesRefs []string
	if o.opts.Rules != nil {
		rulesRefs = o.opts.Rules.Refs()
	}
	return &contract.TaskSheet{
		TaskID:     task.ID,
		Title:      task.Title,
		Goal:       task.Goal,
		Acceptance: task.Acceptance,
		Grounding:  task.Grounding,
		Constraints: contract.Constraints{
			Constitution: constitution,
			ModePolicy:   o.opts.ModePolicy,
			RulesRefs:    rulesRefs,
		},
		Verify:      contract.Verify{Commands: task.Verify, MustBeGreen: true},
		Deliverable: task.Deliverable,
		Attempt:     attempt,
		Feedback:    feedback,
	}
}

// defaultConstitution is the sheet-carried rendering of the engine-enforced
// invariants (the worker engine enforces them; the sheet states them).
var defaultConstitution = []string{
	"NO FAKES: never introduce stub/mock/fake doubles or placeholder implementations to make verification pass.",
	"VERIFY BEFORE DONE: done requires the sheet's verification commands green after your last change.",
	"READ FULL: truncated tool output must be read to the end before reasoning over it.",
	"NO FALSE SUCCESS: report partials as partials with honest gaps.",
	"COMPLETE ARTIFACTS: deliver runnable code, never fragments.",
	"USER DRIVES GIT: never git commit or push.",
	"RESPECT THE PROJECT: follow existing repo style; no out-of-scope changes; never weaken or delete tests to pass.",
}

// changeRecords renders the report's factual change record as event fields.
func changeRecords(r *contract.TurnInReport) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(r.Changes))
	for _, c := range r.Changes {
		out = append(out, map[string]interface{}{"path": c.Path, "kind": c.Kind, "why": c.Why})
	}
	return out
}

// evidenceRecords renders the report's verification evidence as event fields.
func evidenceRecords(r *contract.TurnInReport) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(r.Verification))
	for _, ev := range r.Verification {
		out = append(out, map[string]interface{}{"command": ev.Command, "exit": ev.Exit})
	}
	return out
}

func (o *Orchestrator) append(m llm.Message) { o.window = append(o.window, m) }

// emit forwards a progress event to the observer side-channel.
func (o *Orchestrator) emit(event string, fields map[string]interface{}) {
	if o.opts.Observer != nil {
		o.opts.Observer(event, fields)
	}
}

// renderSheet is the compact sheet digest kept in the window.
func renderSheet(s *contract.TaskSheet) string {
	return fmt.Sprintf("SHEET %s (attempt %d): %s\nGoal: %s\nVerify: %s",
		s.TaskID, s.Attempt, s.Title, s.Goal, strings.Join(s.Verify.Commands, "; "))
}

// renderReport is the compact turn-in digest kept in the window.
func renderReport(r *contract.TurnInReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "TURN-IN %s (attempt %d): %s\nSummary: %s\n", r.TaskID, r.Attempt, r.Status, r.Summary)
	for _, c := range r.Changes {
		fmt.Fprintf(&b, "- %s %s: %s\n", c.Kind, c.Path, c.Why)
	}
	for _, ev := range r.Verification {
		fmt.Fprintf(&b, "- verify %q exit %d\n", ev.Command, ev.Exit)
	}
	if len(r.Gaps) > 0 {
		fmt.Fprintf(&b, "Gaps: %s\n", strings.Join(r.Gaps, "; "))
	}
	return b.String()
}
