// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"fmt"
	"time"

	"matrix/executor/lifecycle"
	"matrix/mcl/envelope"
)

// LifecycleDriver wraps a lifecycle.Machine + EnvelopeStream so the harness
// can sign + apply in one step and the assertion layer can ask "what state
// are we in?"
type LifecycleDriver struct {
	machine *lifecycle.Machine
	stream  *EnvelopeStream
	assert  *AssertCtx
	t       *Transcript
}

func NewLifecycleDriver(intentID string, stream *EnvelopeStream, assert *AssertCtx, t *Transcript) (*LifecycleDriver, error) {
	m, err := lifecycle.New(intentID, lifecycle.StateDrafting)
	if err != nil {
		return nil, err
	}
	return &LifecycleDriver{machine: m, stream: stream, assert: assert, t: t}, nil
}

// State returns the current lifecycle state (test asserts read this).
func (d *LifecycleDriver) State() lifecycle.State { return d.machine.State() }

// drive signs an envelope of kind+body, applies it to the lifecycle machine,
// and asserts the transition reached `wantState`. Used internally by all the
// public Drive* helpers below for a single uniform error path.
func (d *LifecycleDriver) drive(kind string, body interface{}, wantState lifecycle.State, opts lifecycle.ApplyOpts, correlation string) (*envelope.Envelope, error) {
	prev := d.machine.State()
	causation := d.stream.LastID()

	env, err := d.stream.SignAndPersist(kind, body, correlation, causation)
	if err != nil {
		return nil, err
	}
	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	state, evt, err := d.machine.Apply(env, opts)
	if err != nil {
		d.assert.True("lifecycle "+string(prev)+"→"+string(wantState)+" via "+kind, false, err.Error())
		return env, err
	}
	d.t.Event("lifecycle.transition", "lifecycle", map[string]interface{}{
		"kind":     kind,
		"from":     string(evt.From),
		"to":       string(evt.To),
		"material": evt.Material,
	})
	d.assert.Equal("lifecycle "+string(prev)+"→"+string(wantState)+" via "+kind, string(wantState), string(state))
	return env, nil
}

// DriveDraft transitions drafting (initial) by emitting intent.compiled.
// Lifecycle: drafting → proposed.
func (d *LifecycleDriver) DriveCompiled(intentJSON []byte, latencyMs int64) (*envelope.Envelope, error) {
	body := envelope.IntentCompiledBody{
		IntentJSON:       intentJSON,
		CompileLatencyMs: latencyMs,
	}
	return d.drive(envelope.KindIntentCompiled, body, lifecycle.StateProposed, lifecycle.ApplyOpts{Notes: "compiler emitted typed IR"}, "")
}

// DriveClarify transitions proposed → clarifying (Q2 full-surface).
func (d *LifecycleDriver) DriveClarify(questions []envelope.ClarifyQuestion) (*envelope.Envelope, error) {
	body := envelope.IntentClarifyBody{Questions: questions}
	return d.drive(envelope.KindIntentClarify, body, lifecycle.StateClarifying, lifecycle.ApplyOpts{Notes: "agent asks for clarification"}, "")
}

// DriveAnswer transitions clarifying → proposed.
func (d *LifecycleDriver) DriveAnswer(patches []byte, answerOf string) (*envelope.Envelope, error) {
	body := envelope.IntentAnswerBody{
		Patches:  patches,
		AnswerOf: answerOf,
	}
	return d.drive(envelope.KindIntentAnswer, body, lifecycle.StateProposed, lifecycle.ApplyOpts{Notes: "user answer; re-compile loop"}, answerOf)
}

// DriveAccept transitions proposed → accepted.
func (d *LifecycleDriver) DriveAccept(intentHash string, anchorRequested bool) (*envelope.Envelope, error) {
	body := envelope.IntentAcceptBody{
		IntentHash:      intentHash,
		AcceptedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		AnchorRequested: anchorRequested,
	}
	return d.drive(envelope.KindIntentAccept, body, lifecycle.StateAccepted, lifecycle.ApplyOpts{Notes: "user signed IR"}, "")
}

// DrivePlanProposed transitions accepted → executing.
func (d *LifecycleDriver) DrivePlanProposed(planJSON []byte) (*envelope.Envelope, error) {
	body := envelope.PlanProposedBody{PlanJSON: planJSON}
	return d.drive(envelope.KindPlanProposed, body, lifecycle.StateExecuting, lifecycle.ApplyOpts{Notes: "executor begins plan walk"}, "")
}

// DriveCorrectNonMaterial fires an executing→executing self-transition.
// Q11 lock: non-material correction does NOT halt execution.
func (d *LifecycleDriver) DriveCorrectNonMaterial(reason string, patches []byte) (*envelope.Envelope, error) {
	body := envelope.IntentCorrectBody{
		Target:  "plan",
		Patches: patches,
		Reason:  reason,
	}
	return d.drive(envelope.KindIntentCorrect, body, lifecycle.StateExecuting, lifecycle.ApplyOpts{
		Material: false,
		Notes:    "non-material correction (D9 classifier returned non-material)",
	}, "")
}

// DriveCorrectMaterial fires executing→accepted (re-sign loop).
// Q11 lock: material correction halts and rewinds to accept.
func (d *LifecycleDriver) DriveCorrectMaterial(reason string, patches []byte) (*envelope.Envelope, error) {
	body := envelope.IntentCorrectBody{
		Target:  "plan",
		Patches: patches,
		Reason:  reason,
	}
	return d.drive(envelope.KindIntentCorrect, body, lifecycle.StateAccepted, lifecycle.ApplyOpts{
		Material: true,
		Notes:    "material correction (D9 classifier returned material) — rewinds to accept",
	}, "")
}

// DriveAttest transitions executing → completed.
func (d *LifecycleDriver) DriveAttest(citedURIs []string, evidence []byte) (*envelope.Envelope, error) {
	body := envelope.IntentAttestBody{
		Outcome:      "success",
		CitedURIs:    citedURIs,
		EvidenceJSON: evidence,
		CompletedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	return d.drive(envelope.KindIntentAttest, body, lifecycle.StateCompleted, lifecycle.ApplyOpts{Notes: "intent.attest success"}, "")
}

// History returns the recorded lifecycle events (audit dump).
func (d *LifecycleDriver) History() []lifecycle.Event { return d.machine.History() }

// Summary prints a human-readable summary of the lifecycle path.
func (d *LifecycleDriver) Summary() string {
	hist := d.machine.History()
	if len(hist) == 0 {
		return "(no transitions recorded)"
	}
	out := string(hist[0].From)
	for i := range hist {
		e := &hist[i]
		out += fmt.Sprintf(" --[%s]--> %s", e.Kind, e.To)
	}
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
