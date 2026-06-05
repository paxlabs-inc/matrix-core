// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package lifecycle

import (
	"errors"
	"sort"
	"testing"
	"time"

	"matrix/mcl/envelope"
)

func TestStates_All8Closed(t *testing.T) {
	if len(AllStates) != 8 {
		t.Fatalf("want 8 states, got %d", len(AllStates))
	}
	seen := make(map[State]bool)
	for _, s := range AllStates {
		if seen[s] {
			t.Errorf("duplicate state %q", s)
		}
		seen[s] = true
		if !ValidState(s) {
			t.Errorf("ValidState(%q) should be true", s)
		}
	}
}

func TestIsTerminal(t *testing.T) {
	cases := map[State]bool{
		StateDrafting:   false,
		StateProposed:   false,
		StateClarifying: false,
		StateAccepted:   false,
		StateExecuting:  false,
		StateCompleted:  true,
		StateFailed:     true,
		StateCancelled:  true,
	}
	for s, want := range cases {
		if got := s.IsTerminal(); got != want {
			t.Errorf("%s.IsTerminal()=%v want %v", s, got, want)
		}
	}
}

func TestTransition_LegalMoves(t *testing.T) {
	// All legal transitions per allowedTransitions.
	legal := []struct{ from, to State }{
		{StateDrafting, StateProposed},
		{StateDrafting, StateFailed},
		{StateDrafting, StateCancelled},
		{StateProposed, StateClarifying},
		{StateProposed, StateAccepted},
		{StateProposed, StateCancelled},
		{StateProposed, StateFailed},
		{StateClarifying, StateProposed},
		{StateClarifying, StateCancelled},
		{StateClarifying, StateFailed},
		{StateAccepted, StateExecuting},
		{StateAccepted, StateCancelled},
		{StateAccepted, StateFailed},
		{StateExecuting, StateCompleted},
		{StateExecuting, StateFailed},
		{StateExecuting, StateCancelled},
		{StateExecuting, StateAccepted},  // material correction
		{StateExecuting, StateExecuting}, // non-material correction
	}
	for _, tc := range legal {
		if !Transition(tc.from, tc.to) {
			t.Errorf("legal transition rejected: %s → %s", tc.from, tc.to)
		}
	}
}

func TestTransition_IllegalMoves(t *testing.T) {
	illegal := []struct{ from, to State }{
		{StateDrafting, StateExecuting},
		{StateDrafting, StateAccepted},
		{StateProposed, StateExecuting},
		{StateAccepted, StateProposed},
		{StateAccepted, StateClarifying},
		{StateExecuting, StateClarifying},
		{StateExecuting, StateProposed},
		{StateExecuting, StateDrafting},
		// Terminal: no outgoing
		{StateCompleted, StateExecuting},
		{StateFailed, StateExecuting},
		{StateCancelled, StateExecuting},
		{StateCompleted, StateCompleted},
		// Self-transition on non-executing rejected
		{StateProposed, StateProposed},
		{StateAccepted, StateAccepted},
		// Unknown state values
		{State("nonsense"), StateProposed},
		{StateProposed, State("nonsense")},
	}
	for _, tc := range illegal {
		if Transition(tc.from, tc.to) {
			t.Errorf("illegal transition allowed: %s → %s", tc.from, tc.to)
		}
	}
}

func TestAllowedNext_Deterministic(t *testing.T) {
	a := AllowedNext(StateExecuting)
	b := AllowedNext(StateExecuting)
	if len(a) != len(b) {
		t.Fatalf("len drift")
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("order drift at %d: %s vs %s", i, a[i], b[i])
		}
	}
	if !sort.SliceIsSorted(a, func(i, j int) bool { return a[i] < a[j] }) {
		t.Fatal("AllowedNext should return sorted slice")
	}
	if got := AllowedNext(StateCompleted); got != nil {
		t.Fatalf("terminal state should return nil, got %v", got)
	}
}

func TestNew_Validation(t *testing.T) {
	if _, err := New("", StateDrafting); err == nil {
		t.Fatal("expected error on empty intent_id")
	}
	if _, err := New("intent_id", State("nonsense")); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("want ErrUnknownState, got %v", err)
	}
	m, err := New("01HZ", StateDrafting)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if m.State() != StateDrafting {
		t.Fatalf("initial state drift: %s", m.State())
	}
	if m.IntentID() != "01HZ" {
		t.Fatalf("intent_id: %s", m.IntentID())
	}
}

// makeEnv constructs a minimal envelope for the lifecycle Apply tests.
func makeEnv(kind, intentID string) *envelope.Envelope {
	env, _ := envelope.NewEnvelope(kind, envelopeBodyFor(kind))
	env.ID = "01HZ-env"
	env.At = "2026-05-24T12:00:00Z"
	env.From = "matrix://agent/x"
	env.Intent = "matrix://intent/" + intentID
	return env
}

func envelopeBodyFor(kind string) interface{} {
	switch kind {
	case envelope.KindIntentDraft:
		return envelope.IntentDraftBody{Prose: "x"}
	case envelope.KindIntentCompiled:
		return envelope.IntentCompiledBody{IntentJSON: []byte(`{}`)}
	case envelope.KindIntentClarify:
		return envelope.IntentClarifyBody{Questions: []envelope.ClarifyQuestion{{UnknownID: "u1", Field: "f", Prompt: "?"}}}
	case envelope.KindIntentAnswer:
		return envelope.IntentAnswerBody{Patches: []byte(`[]`), AnswerOf: "01HZ"}
	case envelope.KindIntentAccept:
		return envelope.IntentAcceptBody{IntentHash: "abc", AcceptedAt: "t"}
	case envelope.KindPlanProposed:
		return envelope.PlanProposedBody{PlanJSON: []byte(`{}`)}
	case envelope.KindPlanStep:
		return envelope.PlanStepBody{PlanID: "p", NodeID: "n", Status: "completed"}
	case envelope.KindPlanOutput:
		return envelope.PlanOutputBody{PlanID: "p", NodeID: "n", Sequence: 1, Chunk: []byte("x")}
	case envelope.KindIntentCorrect:
		return envelope.IntentCorrectBody{Target: "intent", Patches: []byte(`[]`)}
	case envelope.KindIntentDispatch:
		return envelope.IntentDispatchBody{SubIntentJSON: []byte(`{}`)}
	case envelope.KindIntentAttest:
		return envelope.IntentAttestBody{Outcome: "success", CompletedAt: "t"}
	case envelope.KindIntentFail:
		return envelope.IntentFailBody{Reason: "tool_error", FailedAt: "t"}
	case envelope.KindIntentCancel:
		return envelope.IntentCancelBody{CancelledAt: "t"}
	case envelope.KindPolicyGate:
		return envelope.PolicyGateBody{RuleRef: "matrix://rule/x", Question: "?"}
	case envelope.KindPolicyGateResolve:
		return envelope.PolicyGateResolveBody{GateOf: "g", Decision: "approve", ResolvedAt: "t"}
	}
	return nil
}

func TestApply_HappyPath(t *testing.T) {
	// Walk through the canonical lifecycle:
	// drafting → proposed → accepted → executing → completed
	intentID := "01HZ-happy"
	m, _ := New(intentID, StateDrafting)
	now := time.Date(2026, 5, 24, 12, 0, 0, 0, time.UTC)

	steps := []struct {
		kind     string
		wantNext State
	}{
		{envelope.KindIntentCompiled, StateProposed},
		{envelope.KindIntentAccept, StateAccepted},
		{envelope.KindPlanProposed, StateExecuting},
		{envelope.KindIntentAttest, StateCompleted},
	}
	for _, step := range steps {
		env := makeEnv(step.kind, intentID)
		got, evt, err := m.Apply(env, ApplyOpts{Now: now})
		if err != nil {
			t.Fatalf("Apply %s: %v", step.kind, err)
		}
		if got != step.wantNext {
			t.Fatalf("Apply %s: want %s got %s", step.kind, step.wantNext, got)
		}
		if evt == nil {
			t.Fatalf("Apply %s: nil event", step.kind)
		}
		if !evt.At.Equal(now) {
			t.Errorf("Event.At drift: %v vs %v", evt.At, now)
		}
	}

	if !m.State().IsTerminal() {
		t.Fatalf("expected terminal, got %s", m.State())
	}
	if len(m.History()) != 4 {
		t.Fatalf("history len %d", len(m.History()))
	}
}

func TestApply_ClarifyLoop(t *testing.T) {
	// proposed → clarifying → proposed (re-compile loop)
	intentID := "01HZ-clarify"
	m, _ := New(intentID, StateProposed)

	env := makeEnv(envelope.KindIntentClarify, intentID)
	env.From = "matrix://agent/x"
	if got, _, err := m.Apply(env, ApplyOpts{}); err != nil || got != StateClarifying {
		t.Fatalf("clarify: state=%s err=%v", got, err)
	}

	env = makeEnv(envelope.KindIntentAnswer, intentID)
	if got, _, err := m.Apply(env, ApplyOpts{}); err != nil || got != StateProposed {
		t.Fatalf("answer back to proposed: state=%s err=%v", got, err)
	}
}

func TestApply_CorrectionMaterialVsNot(t *testing.T) {
	// Material correction during executing → accepted (rewind)
	intentID := "01HZ-correct"
	m, _ := New(intentID, StateExecuting)
	env := makeEnv(envelope.KindIntentCorrect, intentID)

	got, _, err := m.Apply(env, ApplyOpts{Material: true})
	if err != nil || got != StateAccepted {
		t.Fatalf("material correct: state=%s err=%v", got, err)
	}

	// Non-material correction during executing → executing (no-op)
	m2, _ := New(intentID, StateExecuting)
	got, _, err = m2.Apply(env, ApplyOpts{Material: false})
	if err != nil || got != StateExecuting {
		t.Fatalf("non-material correct: state=%s err=%v", got, err)
	}
}

func TestApply_CancelAtAnyNonTerminal(t *testing.T) {
	intentID := "01HZ-cancel"
	cases := []State{StateDrafting, StateProposed, StateClarifying, StateAccepted, StateExecuting}
	for _, start := range cases {
		t.Run(string(start), func(t *testing.T) {
			m, _ := New(intentID, start)
			env := makeEnv(envelope.KindIntentCancel, intentID)
			got, _, err := m.Apply(env, ApplyOpts{})
			if err != nil {
				t.Fatalf("cancel from %s: %v", start, err)
			}
			if got != StateCancelled {
				t.Fatalf("want cancelled, got %s", got)
			}
		})
	}
}

func TestApply_RejectsTerminalReentry(t *testing.T) {
	intentID := "01HZ-terminal"
	for _, term := range []State{StateCompleted, StateFailed, StateCancelled} {
		t.Run(string(term), func(t *testing.T) {
			m, _ := New(intentID, term)
			env := makeEnv(envelope.KindIntentCancel, intentID)
			_, _, err := m.Apply(env, ApplyOpts{})
			if !errors.Is(err, ErrAlreadyAtTerminal) {
				t.Fatalf("want ErrAlreadyAtTerminal, got %v", err)
			}
			if m.State() != term {
				t.Fatalf("state changed from terminal: %s", m.State())
			}
		})
	}
}

func TestApply_RejectsInvalidTransition(t *testing.T) {
	// intent.attest from proposed (must come from executing) — invalid
	intentID := "01HZ-invalid"
	m, _ := New(intentID, StateProposed)
	env := makeEnv(envelope.KindIntentAttest, intentID)
	_, _, err := m.Apply(env, ApplyOpts{})
	if !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("want ErrInvalidTransition, got %v", err)
	}
	if m.State() != StateProposed {
		t.Fatalf("state changed despite invalid transition: %s", m.State())
	}
}

func TestApply_RejectsUnknownKind(t *testing.T) {
	// plan.step + plan.output + policy.gate + policy.gate.resolve do not drive transitions
	intentID := "01HZ-info"
	m, _ := New(intentID, StateExecuting)
	for _, kind := range []string{envelope.KindPlanStep, envelope.KindPlanOutput, envelope.KindPolicyGate, envelope.KindPolicyGateResolve} {
		env := makeEnv(kind, intentID)
		_, _, err := m.Apply(env, ApplyOpts{})
		if !errors.Is(err, ErrUnknownKind) {
			t.Errorf("kind=%s: want ErrUnknownKind, got %v", kind, err)
		}
	}
	// State should be unchanged after info-only kinds
	if m.State() != StateExecuting {
		t.Fatalf("info kinds changed state: %s", m.State())
	}
}

func TestApply_RejectsEnvelopeMismatch(t *testing.T) {
	m, _ := New("intent_a", StateDrafting)
	env := makeEnv(envelope.KindIntentCompiled, "intent_b")
	_, _, err := m.Apply(env, ApplyOpts{})
	if !errors.Is(err, ErrEnvelopeMismatch) {
		t.Fatalf("want ErrEnvelopeMismatch, got %v", err)
	}
}

func TestApply_AcceptsBareIntentID(t *testing.T) {
	// Some callers may set env.Intent to just the ULID rather than the
	// matrix://intent/ URI. Lifecycle accepts both forms.
	intentID := "01HZ-bare"
	m, _ := New(intentID, StateDrafting)
	env := makeEnv(envelope.KindIntentCompiled, intentID)
	env.Intent = intentID // bare form
	_, _, err := m.Apply(env, ApplyOpts{})
	if err != nil {
		t.Fatalf("bare form rejected: %v", err)
	}
}

func TestApply_HistoryOrder(t *testing.T) {
	intentID := "01HZ-history"
	m, _ := New(intentID, StateDrafting)
	now := time.Now()

	_, _, _ = m.Apply(makeEnv(envelope.KindIntentCompiled, intentID), ApplyOpts{Now: now})
	_, _, _ = m.Apply(makeEnv(envelope.KindIntentClarify, intentID), ApplyOpts{Now: now.Add(1 * time.Millisecond)})
	_, _, _ = m.Apply(makeEnv(envelope.KindIntentAnswer, intentID), ApplyOpts{Now: now.Add(2 * time.Millisecond)})
	_, _, _ = m.Apply(makeEnv(envelope.KindIntentAccept, intentID), ApplyOpts{Now: now.Add(3 * time.Millisecond)})

	h := m.History()
	if len(h) != 4 {
		t.Fatalf("history len %d", len(h))
	}
	wantStates := []State{StateProposed, StateClarifying, StateProposed, StateAccepted}
	for i, want := range wantStates {
		if h[i].To != want {
			t.Errorf("history[%d].To=%s want %s", i, h[i].To, want)
		}
	}
	// Monotonic At ordering
	for i := 1; i < len(h); i++ {
		if !h[i-1].At.Before(h[i].At) {
			t.Errorf("history not monotonic: [%d]=%v [%d]=%v", i-1, h[i-1].At, i, h[i].At)
		}
	}
}

func TestApply_MaxHistoryCap(t *testing.T) {
	intentID := "01HZ-cap"
	m, _ := New(intentID, StateExecuting)
	m.MaxHistory = 2

	env := makeEnv(envelope.KindIntentCorrect, intentID)
	// Three non-material corrections; only 2 most recent should remain in memory
	now := time.Now()
	for i := 0; i < 3; i++ {
		_, _, err := m.Apply(env, ApplyOpts{Now: now.Add(time.Duration(i) * time.Millisecond), Material: false})
		if err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}
	if got := len(m.History()); got != 2 {
		t.Fatalf("history cap not respected: %d", got)
	}
}

func TestMessageKindTransition_CoversExpectedKinds(t *testing.T) {
	want := []string{
		envelope.KindIntentCompiled, envelope.KindIntentClarify, envelope.KindIntentAnswer,
		envelope.KindIntentAccept, envelope.KindPlanProposed, envelope.KindIntentAttest,
		envelope.KindIntentFail, envelope.KindIntentCancel, envelope.KindIntentCorrect,
	}
	for _, k := range want {
		if _, ok := MessageKindTransition[k]; !ok {
			t.Errorf("MessageKindTransition missing kind %q", k)
		}
	}
	// Kinds that explicitly do NOT drive transitions
	notWant := []string{
		envelope.KindIntentDraft, // draft never produces a transition (no agent yet)
		envelope.KindPlanStep, envelope.KindPlanOutput,
		envelope.KindPolicyGate, envelope.KindPolicyGateResolve,
		envelope.KindIntentDispatch,
	}
	for _, k := range notWant {
		if _, ok := MessageKindTransition[k]; ok {
			t.Errorf("MessageKindTransition unexpectedly contains kind %q", k)
		}
	}
}

func TestApply_NilEnvelopeRejected(t *testing.T) {
	m, _ := New("x", StateDrafting)
	_, _, err := m.Apply(nil, ApplyOpts{})
	if err == nil {
		t.Fatal("expected error on nil envelope")
	}
}

func TestEvent_MaterialFlagOnlyForCorrect(t *testing.T) {
	intentID := "01HZ-material"
	m, _ := New(intentID, StateExecuting)
	// Apply non-correct kind with Material=true: flag must NOT be set on the event
	env := makeEnv(envelope.KindIntentAttest, intentID)
	_, evt, err := m.Apply(env, ApplyOpts{Material: true})
	if err != nil {
		t.Fatal(err)
	}
	if evt.Material {
		t.Fatalf("Material flag set on non-correct kind: %+v", evt)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
