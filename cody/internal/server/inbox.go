// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"matrix/cody/internal/contract"
	"matrix/cody/internal/orchestrator"
)

// The answer + steer channel. A run that stalls on needs_input pauses (its
// goroutine stays alive — Task Durability decouples the run from the client,
// not from the process) until POST /intents/{id}/answer resolves it. A live
// correction arrives via POST /intents/{id}/steer and is folded at the next
// orchestrator boundary — never interrupting a mid-flight worker. Both ride a
// durable per-conversation inbox so a codyd restart mid-pause still resolves.

// errRunNotFound is the sentinel the routes map to 404.
var errRunNotFound = errors.New("unknown intent")

// verdict is a decision answer: approve, or override with a payload (the SDR/DLR
// gate consumer lands in wave 2; the channel carries it now).
type verdict struct {
	Decision string          `json:"decision"` // "approve" | "override"
	Payload  json.RawMessage `json:"payload,omitempty"`
}

// directive is one folded human input on the durable inbox: a stop-and-ask
// answer or a live steer.
type directive struct {
	Kind    string    `json:"kind"` // "answer" | "steer"
	Text    string    `json:"text,omitempty"`
	Verdict *verdict  `json:"verdict,omitempty"`
	At      time.Time `json:"at"`
}

// question records the run's open needs_input while it is paused.
type question struct {
	Kind   string    `json:"kind"` // "stop_ask" | "decision"
	Prompt string    `json:"prompt"`
	At     time.Time `json:"at"`
}

// inbox is the per-conversation durable answer/steer channel.
type inbox struct {
	Pending    *question   `json:"pending,omitempty"`
	Directives []directive `json:"directives,omitempty"`
}

func (e *Engine) inboxPath(convID string) string {
	return filepath.Join(e.planDir(convID), "inbox.json")
}

// readInboxLocked reads the durable inbox; a missing inbox is the zero value.
// Caller holds e.inboxMu.
func (e *Engine) readInboxLocked(convID string) inbox {
	var in inbox
	data, err := os.ReadFile(e.inboxPath(convID))
	if err != nil {
		return in
	}
	_ = json.Unmarshal(data, &in)
	return in
}

// writeInboxLocked persists the inbox atomically. Caller holds e.inboxMu.
func (e *Engine) writeInboxLocked(convID string, in inbox) error {
	dir := e.planDir(convID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(in, "", "  ")
	if err != nil {
		return err
	}
	path := e.inboxPath(convID)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// readInbox reads the durable inbox under the lock (test/inspection helper).
func (e *Engine) readInbox(convID string) inbox {
	e.inboxMu.Lock()
	defer e.inboxMu.Unlock()
	return e.readInboxLocked(convID)
}

// appendDirective durably records one steer/answer directive.
func (e *Engine) appendDirective(convID string, d directive) error {
	e.inboxMu.Lock()
	defer e.inboxMu.Unlock()
	in := e.readInboxLocked(convID)
	in.Directives = append(in.Directives, d)
	return e.writeInboxLocked(convID, in)
}

// setPending records (or clears with nil) the run's open needs_input.
func (e *Engine) setPending(convID string, q *question) error {
	e.inboxMu.Lock()
	defer e.inboxMu.Unlock()
	in := e.readInboxLocked(convID)
	in.Pending = q
	return e.writeInboxLocked(convID, in)
}

// directiveTexts renders every folded directive as text — the orchestrator's
// Steers provider. A verdict answer is flattened to text so it, too, folds into
// subsequent sheets.
func (e *Engine) directiveTexts(convID string) []string {
	e.inboxMu.Lock()
	defer e.inboxMu.Unlock()
	in := e.readInboxLocked(convID)
	out := make([]string, 0, len(in.Directives))
	for _, d := range in.Directives {
		t := strings.TrimSpace(d.Text)
		if d.Verdict != nil {
			v := "decision: " + d.Verdict.Decision
			if len(d.Verdict.Payload) > 0 {
				v += " " + string(d.Verdict.Payload)
			}
			if t == "" {
				t = v
			} else {
				t = t + " (" + v + ")"
			}
		}
		if t != "" {
			out = append(out, t)
		}
	}
	return out
}

// --- run pause / resume ---------------------------------------------------

// pauseForInput parks a run on a needs_input stall and blocks until an answer
// arrives (delivered to the run's channel) or the run is cancelled. It emits
// the durable run.needs_input event on entry and run.answered on resolution
// (req 12.3). Returns (answer, true) on resolution, (_, false) on cancel.
func (e *Engine) pauseForInput(ctx context.Context, r *run, prompt string) (directive, bool) {
	return e.awaitDirective(ctx, r, &question{Kind: "stop_ask", Prompt: prompt, At: time.Now().UTC()},
		"I need your input before continuing: "+prompt)
}

// awaitDirective is the shared needs_input pause: it records the open question
// durably, flips the run + ledger to needs_input, emits the run.needs_input
// event, and blocks on the run's answer channel until an answer arrives or the
// run is cancelled. The SDR/DLR decision gates ride the SAME channel with a
// "decision" question kind — the human checkpoint is one mechanism, not two.
// Returns (answer, true) on resolution, (_, false) on cancel.
func (e *Engine) awaitDirective(ctx context.Context, r *run, q *question, say string) (directive, bool) {
	_ = e.setPending(r.convID, q)
	r.setStatus("needs_input")
	_ = e.mutateLedger(r.convID, func(led *ledger) { led.Status = "needs_input" })
	if say != "" {
		e.say(r, say)
	}
	e.publish(r, "run.needs_input", map[string]interface{}{"kind": q.Kind, "prompt": q.Prompt})

	ch := r.beginAwait()
	defer r.endAwait()
	select {
	case <-ctx.Done():
		return directive{}, false
	case d := <-ch:
		_ = e.setPending(r.convID, nil)
		e.publish(r, "run.answered", answerFields(d))
		return d, true
	}
}

// applyAnswer records the answer durably and resets any failed task to pending
// so the orchestrator re-picks it with the answer folded onto the sheet.
func (e *Engine) applyAnswer(convID string, st *contract.Store, plan *orchestrator.Plan, d directive) error {
	if err := e.appendDirective(convID, d); err != nil {
		return err
	}
	changed := false
	for _, t := range plan.Tasks {
		if t.Status == orchestrator.TaskFailed {
			t.Status = orchestrator.TaskPending
			changed = true
		}
	}
	if changed {
		return orchestrator.SavePlan(st.Root(), plan)
	}
	return nil
}

func answerFields(d directive) map[string]interface{} {
	f := map[string]interface{}{"kind": "answer"}
	if d.Text != "" {
		f["text"] = d.Text
	}
	if d.Verdict != nil {
		f["decision"] = d.Verdict.Decision
	}
	return f
}

// --- engine entry points (routes) -----------------------------------------

// Answer resolves a needs_input stall. A live paused run receives the answer on
// its channel; a cold run (paused across a codyd restart) records the answer
// durably, resets its failed task, and re-dispatches to consume it.
func (e *Engine) Answer(runID string, d directive) error {
	d.Kind = "answer"
	if d.At.IsZero() {
		d.At = time.Now().UTC()
	}
	if r := e.lookupRun(runID); r != nil {
		if r.getStatus() != "needs_input" {
			return fmt.Errorf("run is not awaiting input (status %s)", r.getStatus())
		}
		if r.deliverAnswer(d) {
			e.publishUserTurn(runID, "answer", d.Text, directiveDecision(d))
			return nil
		}
		// needs_input but not yet parked on the channel: fall to the cold path.
	}
	convID, led, ok := e.findConvByRun(runID)
	if !ok {
		return errRunNotFound
	}
	if led.Status != "needs_input" {
		return fmt.Errorf("run is not awaiting input (status %s)", led.Status)
	}
	if err := e.appendDirective(convID, d); err != nil {
		return err
	}
	e.publishUserTurn(runID, "answer", d.Text, directiveDecision(d))
	_ = e.setPending(convID, nil)
	if st, err := contract.OpenStore(e.planDir(convID)); err == nil {
		if plan, err := orchestrator.LoadPlan(st.Root()); err == nil && plan != nil {
			changed := false
			for _, t := range plan.Tasks {
				if t.Status == orchestrator.TaskFailed {
					t.Status = orchestrator.TaskPending
					changed = true
				}
			}
			if changed {
				_ = orchestrator.SavePlan(st.Root(), plan)
			}
		}
	}
	e.launch(convID, led, true)
	return nil
}

// Steer folds a mid-run correction into the run: it is recorded on the durable
// inbox and the running orchestrator picks it up at its next boundary. A steer
// never interrupts a mid-flight worker.
func (e *Engine) Steer(runID, text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("steer text is required")
	}
	convID, ok := e.convForRun(runID)
	if !ok {
		return errRunNotFound
	}
	if err := e.appendDirective(convID, directive{Kind: "steer", Text: text, At: time.Now().UTC()}); err != nil {
		return err
	}
	e.publishUserTurn(runID, "steer", text, "")
	return nil
}

// directiveDecision extracts a verdict decision for the chat.user event.
func directiveDecision(d directive) string {
	if d.Verdict != nil {
		return d.Verdict.Decision
	}
	return ""
}

// convForRun resolves a run id to its conversation: the live run when present,
// else a durable-ledger scan (post-restart).
func (e *Engine) convForRun(runID string) (string, bool) {
	if r := e.lookupRun(runID); r != nil {
		return r.convID, true
	}
	convID, _, ok := e.findConvByRun(runID)
	return convID, ok
}

// findConvByRun scans the durable plan dirs for the conversation whose ledger
// carries runID (mirrors ResumeOrphanedPlans' scan).
func (e *Engine) findConvByRun(runID string) (string, ledger, bool) {
	entries, err := os.ReadDir(filepath.Join(e.opts.DataDir, "plans"))
	if err != nil {
		return "", ledger{}, false
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		convID := entry.Name()
		led, err := e.readLedger(convID)
		if err == nil && led.RunID == runID {
			return convID, led, true
		}
	}
	return "", ledger{}, false
}

// launch drives a run over durable state (fresh or resumed) under the
// one-live-plan-per-conversation invariant. Shared by ResumeOrphanedPlans and
// the answer cold-path.
func (e *Engine) launch(convID string, led ledger, resume bool) *run {
	m := parseModeOrDefault(led.Mode, e.opts.DefaultMode)
	root := led.Root
	if root == "" {
		root, _ = filepath.Abs(e.opts.WorkspaceRoot)
	}
	s := e.session(convID)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.getStatus() == "running" {
		return s.active
	}
	r := &run{id: led.RunID, convID: convID, projectID: led.ProjectID, root: root, done: make(chan struct{}), status: "running", started: time.Now()}
	e.registerRun(r)
	e.broker.ensure(r.id)
	e.broker.reopen(r.id)
	s.active = r
	if led.Status != "running" {
		_ = e.mutateLedger(convID, func(l *ledger) { l.Status = "running" })
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel = cancel
	go e.drive(ctx, r, led.Message, m, resume, led.Spec, led.SpecSource)
	return r
}
