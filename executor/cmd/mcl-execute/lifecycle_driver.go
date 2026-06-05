// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// lifecycle_driver.go wraps an envelopeStream + lifecycle.Machine into
// the production lifecycle driver. Mirrors cmd/mcl-e2e/lifecycle.go
// but lives in the production CLI rather than the harness.
//
// Sess#23 Q-lock: full graph (Andrew confirm 2026-05-24) — every state
// transition is signed + applied + journaled; the canonical invariant
// asserted by sess#22b is preserved here in the production binary.
//
// Lifecycle path produced by the `walk` subcommand:
//
//   drafting
//      ↓ intent.compiled
//   proposed
//      ↓ intent.accept
//   accepted
//      ↓ plan.proposed
//   executing
//      ↓ intent.attest (success) | intent.fail (typed reason)
//   completed | failed

import (
	"fmt"
	"time"

	"matrix/executor/lifecycle"
	"matrix/mcl/envelope"
)

// lifecycleDriver wraps a lifecycle.Machine and an envelopeStream so
// each lifecycle transition is paired with its signed envelope.
type lifecycleDriver struct {
	machine *lifecycle.Machine
	stream  *envelopeStream
	t       *transcript
}

// newLifecycleDriver constructs a driver starting at lifecycle.StateDrafting.
func newLifecycleDriver(intentID string, stream *envelopeStream, t *transcript) (*lifecycleDriver, error) {
	m, err := lifecycle.New(intentID, lifecycle.StateDrafting)
	if err != nil {
		return nil, fmt.Errorf("lifecycle: new machine: %w", err)
	}
	return &lifecycleDriver{
		machine: m,
		stream:  stream,
		t:       t,
	}, nil
}

// State returns the current lifecycle state.
func (d *lifecycleDriver) State() lifecycle.State { return d.machine.State() }

// History dumps the full transition log (audit access).
func (d *lifecycleDriver) History() []lifecycle.Event { return d.machine.History() }

// drive signs an envelope of the supplied kind+body, applies it to the
// lifecycle machine, journals the result, and surfaces transition errors
// up to the caller. The expectedState argument is logged + cross-checked
// with the actual transition; mismatch is a programmer error.
func (d *lifecycleDriver) drive(kind string, body interface{}, expected lifecycle.State, opts lifecycle.ApplyOpts, correlation string) (*envelope.Envelope, error) {
	prev := d.machine.State()
	causation := d.stream.LastID()

	env, err := d.stream.SignAndPersist(kind, body, correlation, causation)
	if err != nil {
		return nil, fmt.Errorf("lifecycle %s→? via %s: sign: %w", prev, kind, err)
	}

	if opts.Now.IsZero() {
		opts.Now = time.Now().UTC()
	}
	state, evt, aerr := d.machine.Apply(env, opts)
	if aerr != nil {
		d.t.Event("lifecycle.transition.error", "lifecycle", map[string]interface{}{
			"kind":  kind,
			"from":  string(prev),
			"want":  string(expected),
			"error": aerr.Error(),
		})
		return env, fmt.Errorf("lifecycle apply %s on state=%s: %w", kind, prev, aerr)
	}
	if state != expected {
		d.t.Event("lifecycle.transition.unexpected", "lifecycle", map[string]interface{}{
			"kind": kind,
			"from": string(prev),
			"got":  string(state),
			"want": string(expected),
		})
		return env, fmt.Errorf("lifecycle %s via %s: want=%s got=%s",
			prev, kind, expected, state)
	}

	d.t.Event("lifecycle.transition", "lifecycle", map[string]interface{}{
		"kind":     kind,
		"from":     string(evt.From),
		"to":       string(evt.To),
		"material": evt.Material,
		"env_id":   env.ID,
	})
	return env, nil
}

// DriveCompiled emits intent.compiled (drafting → proposed).
func (d *lifecycleDriver) DriveCompiled(intentJSON []byte, latencyMs int64) (*envelope.Envelope, error) {
	body := envelope.IntentCompiledBody{
		IntentJSON:       intentJSON,
		CompileLatencyMs: latencyMs,
	}
	return d.drive(envelope.KindIntentCompiled, body, lifecycle.StateProposed,
		lifecycle.ApplyOpts{Notes: "compiler emitted typed IR"}, "")
}

// DriveClarify emits intent.clarify (proposed → clarifying).
func (d *lifecycleDriver) DriveClarify(questions []envelope.ClarifyQuestion) (*envelope.Envelope, error) {
	body := envelope.IntentClarifyBody{Questions: questions}
	return d.drive(envelope.KindIntentClarify, body, lifecycle.StateClarifying,
		lifecycle.ApplyOpts{Notes: "agent asks for clarification"}, "")
}

// DriveAnswer emits intent.answer (clarifying → proposed).
func (d *lifecycleDriver) DriveAnswer(patches []byte, answerOf string) (*envelope.Envelope, error) {
	body := envelope.IntentAnswerBody{
		Patches:  patches,
		AnswerOf: answerOf,
	}
	return d.drive(envelope.KindIntentAnswer, body, lifecycle.StateProposed,
		lifecycle.ApplyOpts{Notes: "user answer; re-compile loop"}, answerOf)
}

// DriveAccept emits intent.accept (proposed → accepted).
func (d *lifecycleDriver) DriveAccept(intentHash string, anchorRequested bool) (*envelope.Envelope, error) {
	body := envelope.IntentAcceptBody{
		IntentHash:      intentHash,
		AcceptedAt:      time.Now().UTC().Format(time.RFC3339Nano),
		AnchorRequested: anchorRequested,
	}
	return d.drive(envelope.KindIntentAccept, body, lifecycle.StateAccepted,
		lifecycle.ApplyOpts{Notes: "user signed IR"}, "")
}

// DrivePlanProposed emits plan.proposed (accepted → executing).
func (d *lifecycleDriver) DrivePlanProposed(planJSON []byte) (*envelope.Envelope, error) {
	body := envelope.PlanProposedBody{PlanJSON: planJSON}
	return d.drive(envelope.KindPlanProposed, body, lifecycle.StateExecuting,
		lifecycle.ApplyOpts{Notes: "executor begins plan walk"}, "")
}

// DriveCorrectNonMaterial emits intent.correct (executing → executing).
// Q11 lock: non-material correction does NOT halt execution.
func (d *lifecycleDriver) DriveCorrectNonMaterial(reason string, patches []byte) (*envelope.Envelope, error) {
	body := envelope.IntentCorrectBody{
		Target:  "plan",
		Patches: patches,
		Reason:  reason,
	}
	return d.drive(envelope.KindIntentCorrect, body, lifecycle.StateExecuting,
		lifecycle.ApplyOpts{
			Material: false,
			Notes:    "non-material correction (D9 classifier returned non-material)",
		}, "")
}

// DriveCorrectMaterial emits intent.correct (executing → accepted).
// Q11 lock: material correction halts and rewinds to accept.
func (d *lifecycleDriver) DriveCorrectMaterial(reason string, patches []byte) (*envelope.Envelope, error) {
	body := envelope.IntentCorrectBody{
		Target:  "plan",
		Patches: patches,
		Reason:  reason,
	}
	return d.drive(envelope.KindIntentCorrect, body, lifecycle.StateAccepted,
		lifecycle.ApplyOpts{
			Material: true,
			Notes:    "material correction (D9 classifier returned material) — rewinds to accept",
		}, "")
}

// DriveAttest emits intent.attest (executing → completed).
func (d *lifecycleDriver) DriveAttest(citedURIs []string, evidence []byte) (*envelope.Envelope, error) {
	body := envelope.IntentAttestBody{
		Outcome:      "success",
		CitedURIs:    citedURIs,
		EvidenceJSON: evidence,
		CompletedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	return d.drive(envelope.KindIntentAttest, body, lifecycle.StateCompleted,
		lifecycle.ApplyOpts{Notes: "intent.attest success"}, "")
}

// DriveFail emits intent.fail (executing → failed). Reason should match
// the closed taxonomy from research/02-protocol.md §13.
func (d *lifecycleDriver) DriveFail(reason, message string, evidence []byte) (*envelope.Envelope, error) {
	body := envelope.IntentFailBody{
		Reason:       reason,
		Message:      message,
		EvidenceJSON: evidence,
		FailedAt:     time.Now().UTC().Format(time.RFC3339Nano),
	}
	return d.drive(envelope.KindIntentFail, body, lifecycle.StateFailed,
		lifecycle.ApplyOpts{Notes: "intent.fail (" + reason + ")"}, "")
}

// DriveCancel emits intent.cancel from any non-terminal state.
func (d *lifecycleDriver) DriveCancel(reason string) (*envelope.Envelope, error) {
	body := envelope.IntentCancelBody{
		Reason:      reason,
		CancelledAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	return d.drive(envelope.KindIntentCancel, body, lifecycle.StateCancelled,
		lifecycle.ApplyOpts{Notes: "intent.cancel (" + reason + ")"}, "")
}

// Summary returns a human-readable arrow-trace of every transition.
// Useful for the walk subcommand's terminal status report.
func (d *lifecycleDriver) Summary() string {
	hist := d.machine.History()
	if len(hist) == 0 {
		return "(no transitions recorded)"
	}
	out := string(hist[0].From)
	for _, e := range hist {
		out += fmt.Sprintf(" --[%s]--> %s", e.Kind, e.To)
	}
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
