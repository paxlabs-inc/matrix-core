// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package records

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"testing/quick"
)

func TestCanonicalRecordCodecRoundTripsRemainStable(t *testing.T) {
	property := func(seed uint64, value string) bool {
		raw := json.RawMessage(fmt.Sprintf(`{"seed":%d}`, seed))
		turn := TurnRecord{
			LogicalTurnID:          fmt.Sprintf("turn-%d", seed),
			ConversationID:         fmt.Sprintf("conversation-%d", seed),
			RequestIdentity:        fmt.Sprintf("request-%d", seed),
			Objective:              value,
			LatestGenuineMessageID: fmt.Sprintf("message-%d", seed),
			CurrentState:           StateGenerating,
			CumulativeBudgets:      BudgetCounters{ProviderCalls: seed},
			SynthesisDebt:          SynthesisDebt{Owed: true, UnconsumedEvidence: []string{fmt.Sprintf("evidence-%d", seed)}},
		}
		cycle := CycleRecord{
			GenerationNumber:     seed,
			ContextManifest:      ContextManifest{Entries: []ContextManifestEntry{{SourceNamespace: "test", SourceID: fmt.Sprint(seed), SemanticKind: "conversation", ContentHash: fmt.Sprint(seed), Included: true, Reason: "selected"}}},
			ProviderRequest:      raw,
			StreamedOutputState:  []StreamedOutput{{AttemptID: fmt.Sprint(seed), Sequence: seed, Channel: StreamAnswer, Status: StreamCommitted, Content: value}},
			ProposedToolCalls:    []ProposedToolCall{{CallID: fmt.Sprint(seed), Operation: "read", NormalizedArguments: raw}},
			ObservedToolOutcomes: []ToolOutcome{{FailureLayer: FailureNone, EffectStatus: EffectCompleted}},
			NextIntendedAction:   value,
		}
		effect := EffectRecord{Operation: "read", NormalizedArguments: raw, SideEffectClass: SideEffectReadOnly, IdempotencyStrategy: "retry", ReconciliationStrategy: "authoritative-result", EffectState: EffectCompleted}
		convergence := ConvergenceRecord{FailureFingerprints: []FailureFingerprint{{Operation: "read", NormalizedArguments: raw, FailureLayer: FailureNone, EffectStatus: EffectCompleted}}, AttemptCounts: []AttemptCount{}, StrategyChanges: []string{}, ProviderUsage: []ProviderUsage{{Provider: "local", Model: "test", RequestCount: seed}}, RepairCounters: map[string]uint64{"repair": seed}}
		answer := AnswerRecord{GeneratedAnswer: value, SupportingEvidenceIDs: []string{fmt.Sprint(seed)}, CompletionAssessment: CompletionAssessment{Ready: true}, StreamCommitState: StreamCommitted}
		delivery := DeliveryRecord{Channel: "session", Attempts: []DeliveryAttempt{{Number: seed}}, Acknowledgement: value}

		checks := []func() bool{
			func() bool { return stableRoundTrip(turn, EncodeTurn, DecodeTurn) },
			func() bool { return stableRoundTrip(cycle, EncodeCycle, DecodeCycle) },
			func() bool { return stableRoundTrip(effect, EncodeEffect, DecodeEffect) },
			func() bool { return stableRoundTrip(convergence, EncodeConvergence, DecodeConvergence) },
			func() bool { return stableRoundTrip(answer, EncodeAnswer, DecodeAnswer) },
			func() bool { return stableRoundTrip(delivery, EncodeDelivery, DecodeDelivery) },
		}
		for _, check := range checks {
			if !check() {
				return false
			}
		}
		return true
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}

func TestCanonicalCodecRejectsWrongKindAndFutureSchema(t *testing.T) {
	encoded, err := EncodeTurn(TurnRecord{CurrentState: StateAccepted})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeAnswer(encoded); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("DecodeAnswer error = %v, want ErrInvalidRecord", err)
	}
	var wrapped map[string]any
	if err := json.Unmarshal(encoded, &wrapped); err != nil {
		t.Fatal(err)
	}
	wrapped["schema_version"] = float64(SchemaVersion + 1)
	future, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTurn(future); !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("DecodeTurn error = %v, want ErrInvalidRecord", err)
	}
}

func TestCanonicalCodecAcceptsAdditiveFields(t *testing.T) {
	encoded, err := EncodeTurn(TurnRecord{CurrentState: StateAccepted})
	if err != nil {
		t.Fatal(err)
	}
	var wrapped map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wrapped); err != nil {
		t.Fatal(err)
	}
	var record map[string]json.RawMessage
	if err := json.Unmarshal(wrapped["record"], &record); err != nil {
		t.Fatal(err)
	}
	record["future_additive_field"] = json.RawMessage(`{"accepted":true}`)
	payload, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	wrapped["record"] = payload
	additive, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTurn(additive); err != nil {
		t.Fatalf("DecodeTurn rejected additive field: %v", err)
	}
}

func TestFrozenCanonicalRecordFieldsRemainPresent(t *testing.T) {
	required := map[reflect.Type][]string{
		reflect.TypeOf(TurnRecord{}):        {"logical_turn_id", "conversation_id", "request_identity", "objective", "latest_genuine_message_id", "current_state", "cumulative_budgets", "synthesis_debt", "answer_identity", "delivery_identity"},
		reflect.TypeOf(CycleRecord{}):       {"generation_number", "context_manifest", "provider_request", "streamed_output_state", "proposed_tool_calls", "observed_tool_outcomes", "next_intended_action"},
		reflect.TypeOf(EffectRecord{}):      {"operation", "normalized_arguments", "side_effect_class", "idempotency_strategy", "reconciliation_strategy", "effect_state", "authoritative_dispatch_evidence", "result", "unknown_effect_status"},
		reflect.TypeOf(ConvergenceRecord{}): {"failure_fingerprints", "attempt_counts", "strategy_changes", "provider_usage", "cumulative_input_tokens", "cumulative_output_tokens", "tool_calls", "repair_counters"},
		reflect.TypeOf(AnswerRecord{}):      {"generated_answer", "supporting_evidence_ids", "completion_assessment", "stream_commit_state"},
		reflect.TypeOf(DeliveryRecord{}):    {"channel", "attempts", "acknowledgement", "last_delivery_error"},
	}
	for recordType, names := range required {
		present := make(map[string]bool, recordType.NumField())
		for index := 0; index < recordType.NumField(); index++ {
			name := recordType.Field(index).Tag.Get("json")
			if comma := bytes.IndexByte([]byte(name), ','); comma >= 0 {
				name = name[:comma]
			}
			present[name] = true
		}
		for _, name := range names {
			if !present[name] {
				t.Errorf("%s missing frozen field %q", recordType.Name(), name)
			}
		}
	}
}

func stableRoundTrip[T any](record T, encode func(T) ([]byte, error), decode func([]byte) (T, error)) bool {
	first, err := encode(record)
	if err != nil {
		return false
	}
	restored, err := decode(first)
	if err != nil || !reflect.DeepEqual(record, restored) {
		return false
	}
	second, err := encode(restored)
	return err == nil && bytes.Equal(first, second)
}
