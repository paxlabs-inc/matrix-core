// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/turnstate"
)

type reconciliationOnlyTools struct {
	status ReconcileStatus
}

func (tools reconciliationOnlyTools) Surface(context.Context) []protocol.ToolDefinition {
	return nil
}

func (tools reconciliationOnlyTools) Execute(
	context.Context, protocol.NormalizedToolCall, string,
) (ToolResult, error) {
	return ToolResult{}, nil
}

func (tools reconciliationOnlyTools) Reconcile(
	context.Context, string,
) (ReconcileResult, error) {
	return ReconcileResult{Status: tools.status}, nil
}

func TestDurableEffectJournalReconcilesCompletedAndRetrySafeRealDispatches(
	t *testing.T,
) {
	store := realTurnStore(
		t, "effect-journal-turn", "Exercise effect reconciliation.",
	)
	journal := &DurableEffectJournal{Store: store}
	manager := realExecManager(t)
	adapter, err := NewToolManagerAdapter(manager, journal)
	if err != nil {
		t.Fatal(err)
	}
	workdir := t.TempDir()
	completed, err := adapter.Execute(
		t.Context(),
		protocol.NormalizedToolCall{
			ID: "effect-completed", Name: "exec__shell",
			Arguments: json.RawMessage(
				`{"command":"printf durable-effect","cwd":"` +
					filepath.ToSlash(workdir) +
					`","expect":"prints durable-effect"}`,
			),
		},
		"effect-completed-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := journal.ReconcileEffect(
		t.Context(), "effect-completed-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != ReconcileCompleted ||
		string(reconciled.Result.Content) != string(completed.Content) ||
		reconciled.Result.IsError != completed.IsError {
		t.Fatalf("completed reconciliation = %+v want %+v", reconciled, completed)
	}

	if _, err := adapter.Execute(
		t.Context(),
		protocol.NormalizedToolCall{
			ID: "effect-read-dispatch", Name: "exec__service_list",
			Arguments: json.RawMessage(`{"expect":"lists managed services"}`),
		},
		"effect-read-dispatch-key",
	); err != nil {
		t.Fatal(err)
	}
	readRecord, err := store.LoadEffect(
		t.Context(), "effect-read-dispatch-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !readRecord.RetrySafe {
		t.Fatal("real read-only dispatch was not persisted as retry-safe")
	}

	if err := journal.BeginEffect(
		t.Context(),
		"effect-read-key",
		"exec__service_list",
		json.RawMessage(`{"expect":"lists services"}`),
		true,
	); err != nil {
		t.Fatal(err)
	}
	reconciled, err = journal.ReconcileEffect(
		t.Context(), "effect-read-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != ReconcileRetrySafe {
		t.Fatalf("read-only started effect = %+v", reconciled)
	}

	if err := journal.BeginEffect(
		t.Context(),
		"effect-write-key",
		"exec__shell",
		json.RawMessage(`{"command":"true","expect":"exit 0"}`),
		false,
	); err != nil {
		t.Fatal(err)
	}
	reconciled, err = journal.ReconcileEffect(
		t.Context(), "effect-write-key",
	)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.Status != ReconcileUnknown {
		t.Fatalf("uncertain write effect = %+v", reconciled)
	}

	err = journal.BeginEffect(
		t.Context(),
		"effect-write-key",
		"exec__shell",
		json.RawMessage(`{"command":"printf changed","expect":"changed"}`),
		false,
	)
	if err == nil || !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("effect identity conflict = %v", err)
	}
}

func TestDurableEffectJournalNotStartedForMissingKey(t *testing.T) {
	store := realTurnStore(
		t, "effect-missing-turn", "Check a missing effect.",
	)
	journal := &DurableEffectJournal{Store: store}
	result, err := journal.ReconcileEffect(t.Context(), "never-recorded")
	if err != nil || result.Status != ReconcileNotStarted {
		t.Fatalf("missing effect = %+v, %v", result, err)
	}
}

func TestPredispatchValidationFailureIsObservationWithoutPendingEffect(t *testing.T) {
	manager, _ := realNativeManager(t)
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded gatewayRequest
		_ = json.Unmarshal(body, &decoded)
		if handleCapabilityCanary(writer, decoded) {
			return
		}
		if calls.Add(1) == 1 {
			writeSSETool(writer, "missing-path", "read_text_file", map[string]interface{}{
				"expect": "reads the requested file",
			})
			return
		}
		writeSSEText(writer, "I could not read the requested file because its required path was missing.")
	}))
	t.Cleanup(gateway.Close)
	turnID := "predispatch-validation-turn"
	userContent := "Read the requested file and report the result."
	store := realTurnStore(t, turnID, userContent)
	adapter, err := NewToolManagerAdapter(manager, &DurableEffectJournal{Store: store})
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{TurnID: turnID, Model: "mimo-v2", IdleTimeout: 5 * time.Second}, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(t.Context(), userContent)
	if err != nil || len(response.ToolEvents) != 1 {
		t.Fatalf("response=%+v err=%v", response, err)
	}
	var outcome map[string]interface{}
	if err := json.Unmarshal(response.ToolEvents[0].Result, &outcome); err != nil ||
		outcome["outcome"] != "error" || outcome["effect_status"] != "not_started" {
		t.Fatalf("pre-dispatch outcome=%+v err=%v", outcome, err)
	}
	state, err := store.LoadTurnState(t.Context(), turnID)
	if err != nil || state.Checkpoint == nil || state.Checkpoint.PendingCall != nil {
		t.Fatalf("validation left pending state=%+v err=%v", state, err)
	}
	key := makeIdempotencyKey(turnID, protocol.NormalizedToolCall{
		ID: "missing-path", Name: "read_text_file", Arguments: json.RawMessage(`{}`),
	})
	if _, err := store.LoadEffect(t.Context(), key); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("validation created an effect record: %v", err)
	}
}

func TestResurrectionAdapterDispatchesNativeReadWriteShellAndServiceTools(t *testing.T) {
	store := realTurnStore(t, "native-effect-turn", "exercise native tools")
	journal := &DurableEffectJournal{Store: store}
	manager, _ := realNativeManager(t)
	adapter, err := NewToolManagerAdapter(manager, journal)
	if err != nil {
		t.Fatal(err)
	}
	calls := []struct {
		name      string
		arguments json.RawMessage
		retrySafe bool
	}{
		{name: "write_file", arguments: json.RawMessage(`{"path":"note.txt","content":"native"}`)},
		{name: "read_text_file", arguments: json.RawMessage(`{"path":"note.txt"}`), retrySafe: true},
		{name: "shell", arguments: json.RawMessage(`{"command":"printf native-shell"}`)},
		{name: "service_list", arguments: json.RawMessage(`{}`), retrySafe: true},
	}
	for index, call := range calls {
		idempotencyKey := "native-effect-" + call.name
		result, err := adapter.Execute(t.Context(), protocol.NormalizedToolCall{
			ID: fmt.Sprintf("native-call-%d", index), Name: call.name,
			Arguments: call.arguments,
		}, idempotencyKey)
		if err != nil || result.IsError {
			t.Fatalf("native %s failed: result=%+v err=%v", call.name, result, err)
		}
		record, err := store.LoadEffect(t.Context(), idempotencyKey)
		if err != nil || record.Status != turnstate.EffectCompleted ||
			record.RetrySafe != call.retrySafe {
			t.Fatalf("native %s effect=%+v err=%v", call.name, record, err)
		}
	}
}

func TestPendingReconciliationBecomesObservationWithoutIncompleteLoop(t *testing.T) {
	for _, status := range []ReconcileStatus{ReconcileNotStarted, ReconcileUnknown} {
		t.Run(string(status), func(t *testing.T) {
			turnID := "reconcile-observation-" + string(status)
			store := realTurnStore(t, turnID, "recover pending effect")
			runtimeLoop := &Loop{
				tools:  reconciliationOnlyTools{status: status},
				store:  store,
				config: Config{TurnID: turnID},
			}
			checkpoint := turnstate.Checkpoint{
				Messages: []protocol.Message{{
					Role: protocol.RoleUser, Content: "recover pending effect",
				}},
				PendingCall: &turnstate.PendingCall{
					CallID: "call-1", IdempotencyKey: "effect-1",
					ToolName: "fs__write_file", Arguments: json.RawMessage(`{"path":"a"}`),
					DispatchedAt: time.Now().UTC(),
				},
			}
			response := Response{}
			state := cursor{}
			if err := runtimeLoop.reconcilePending(
				t.Context(), &checkpoint, &response, &state,
			); err != nil {
				t.Fatalf("reconcile returned a terminal error: %v", err)
			}
			if checkpoint.PendingCall != nil || len(checkpoint.Messages) != 2 ||
				checkpoint.Messages[1].Role != protocol.RoleTool {
				t.Fatalf("pending effect was not converted to an observation: %+v", checkpoint)
			}
			var outcome map[string]any
			if err := json.Unmarshal([]byte(checkpoint.Messages[1].Content), &outcome); err != nil {
				t.Fatal(err)
			}
			want := "not_started"
			if status == ReconcileUnknown {
				want = "outcome_unknown"
			}
			if outcome["effect_status"] != want || outcome["outcome"] != "error" {
				t.Fatalf("structured outcome = %+v", outcome)
			}
		})
	}
}

func TestIdenticalFailureFingerprintRequiresStrategyChange(t *testing.T) {
	state := cursor{}
	call := protocol.NormalizedToolCall{
		ID: "call-1", Name: "fs__read_file",
		Arguments: json.RawMessage(`{"path":"missing"}`),
	}
	failure := structuredToolFailure(
		"application", true, "completed", "path does not exist", "retry later",
	)
	first := state.annotateFailure(call, failure)
	second := state.annotateFailure(call, failure)
	if state.FailureFingerprint == "" || state.FailureRepeats != 2 ||
		!first.Retryable || second.Retryable {
		t.Fatalf("fingerprint containment state=%+v first=%+v second=%+v", state, first, second)
	}
	var payload map[string]any
	if err := json.Unmarshal(second.Content, &payload); err != nil {
		t.Fatal(err)
	}
	if payload["strategy_change_required"] != true || payload["repeat_count"] != float64(2) {
		t.Fatalf("second failure did not require a strategy change: %+v", payload)
	}
}
