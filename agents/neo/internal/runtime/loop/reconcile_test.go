// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"centra/agents/neo/internal/o1"
	"centra/agents/neo/internal/runtime/turnstate"
	"centra/agents/neo/internal/task"
	"centra/packages/vault"
)

const (
	resumeTestTurn = "resurrection-resume-turn"
	resumeTestConv = "resurrection-resume-conversation"
	resumeTestUser = "did:matrix:resurrection-resume-test"
)

func TestProviderTimeoutReturnsTypedIncompleteAndRespawnDelivers(
	t *testing.T,
) {
	manager := realExecManager(t)
	workdir := t.TempDir()
	effectPath := filepath.Join(workdir, "provider-timeout-effect")
	var resumeAllowed atomic.Bool
	var taskCalls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			call := taskCalls.Add(1)
			if call == 1 {
				writeSSETool(
					writer, "timeout-evidence", "exec__shell",
					map[string]interface{}{
						"command": fmt.Sprintf(
							"printf evidence >> %q", effectPath,
						),
						"cwd":    workdir,
						"expect": "appends one evidence marker",
					},
				)
				return
			}
			if !resumeAllowed.Load() {
				<-request.Context().Done()
				return
			}
			writeSSEText(
				writer,
				"The saved evidence is complete and the report is delivered.",
			)
		},
	))
	t.Cleanup(gateway.Close)

	userContent := "Run the evidence step and deliver the report."
	store := realTurnStore(t, resumeTestTurn, userContent)
	toolAdapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	replay := &O1FailureReplayLane{}
	first, err := New(
		realMiMoGenerator(t, gateway.URL), toolAdapter, store,
		Config{
			TurnID: resumeTestTurn, ConversationID: resumeTestConv,
			Model: "mimo-v2", IdleTimeout: time.Second,
		},
		Dependencies{
			Incompletes: replay,
			Delivery: &DeliveryChoke{
				Reporter: reporter,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, turnErr := first.Turn(t.Context(), userContent)
	var incomplete *Incomplete
	if !errors.As(turnErr, &incomplete) ||
		incomplete.Phase != "provider" ||
		incomplete.Checkpoint.PendingCall != nil ||
		len(response.ToolEvents) != 1 {
		t.Fatalf("timeout response=%+v incomplete=%+v err=%v",
			response, incomplete, turnErr)
	}
	if content, err := os.ReadFile(effectPath); err != nil ||
		string(content) != "evidence" {
		t.Fatalf("effect before resume=%q err=%v", content, err)
	}
	if said := reporter.said(); len(said) != 1 ||
		!strings.Contains(said[0], "completed work is saved") {
		t.Fatalf("main-run death was not surfaced honestly: %v", said)
	}
	records := replay.Records()
	if len(records) != 1 ||
		!o1.EvaluateFailureReplay(records).AllPassed {
		t.Fatalf("O1 incomplete replay records=%+v", records)
	}

	resumeAllowed.Store(true)
	next, err := New(
		realMiMoGenerator(t, gateway.URL), toolAdapter, store,
		Config{
			TurnID: resumeTestTurn, ConversationID: resumeTestConv,
			Model: "mimo-v2", IdleTimeout: 5 * time.Second,
		},
		Dependencies{
			Incompletes: replay,
			Delivery: &DeliveryChoke{
				Reporter: reporter,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	resumed, err := ResumeIncomplete(
		t.Context(), userContent, incomplete, next,
	)
	if err != nil ||
		!strings.Contains(resumed.Content, "report is delivered") ||
		len(resumed.ToolEvents) != 1 {
		t.Fatalf("resumed response=%+v err=%v", resumed, err)
	}
	if content, err := os.ReadFile(effectPath); err != nil ||
		string(content) != "evidence" {
		t.Fatalf("effect replayed during resume=%q err=%v", content, err)
	}
	if said := reporter.said(); len(said) != 2 ||
		said[1] != resumed.Content {
		t.Fatalf("delivery sequence=%v", said)
	}
}

func TestKill9MidReportBootResumePreservesEvidenceSet(t *testing.T) {
	if os.Getenv("RESURRECTION_KILL_HELPER") == "1" {
		runResurrectionKillHelper(t)
		return
	}
	root := t.TempDir()
	effectPath := filepath.Join(root, "evidence-effect")
	var phase atomic.Int32
	var phaseCalls atomic.Int32
	blocked := make(chan struct{})
	var blockedOnce sync.Once
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayRequest
			_ = json.Unmarshal(body, &decoded)
			if handleCapabilityCanary(writer, decoded) {
				return
			}
			call := phaseCalls.Add(1)
			switch phase.Load() {
			case 1, 2:
				if call == 1 {
					writeSSETool(
						writer, "report-evidence", "exec__shell",
						map[string]interface{}{
							"command": fmt.Sprintf(
								"printf 'evidence\\n' >> %q", effectPath,
							),
							"cwd":    root,
							"expect": "appends one report evidence line",
						},
					)
					return
				}
				if phase.Load() == 2 {
					blockedOnce.Do(func() { close(blocked) })
					<-request.Context().Done()
					return
				}
				writeSSEText(
					writer,
					"The report is complete from the saved evidence.",
				)
			case 3:
				writeSSEText(
					writer,
					"The report is complete from the saved evidence.",
				)
			default:
				http.Error(writer, "test phase not selected", http.StatusConflict)
			}
		},
	))
	t.Cleanup(gateway.Close)

	phase.Store(1)
	baseline := runUninterruptedEvidenceReport(
		t, root, gateway.URL, effectPath,
	)
	phase.Store(2)
	phaseCalls.Store(0)
	command := exec.Command(
		os.Args[0], "-test.run=TestKill9MidReportBootResumePreservesEvidenceSet",
	)
	command.Env = append(os.Environ(),
		"RESURRECTION_KILL_HELPER=1",
		"RESURRECTION_KILL_ROOT="+root,
		"RESURRECTION_KILL_GATEWAY="+gateway.URL,
		"RESURRECTION_KILL_EFFECT="+effectPath,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		_ = command.Process.Kill()
		t.Fatal("child did not reach the post-evidence provider call")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	if lines := evidenceLines(t, effectPath); lines != 2 {
		t.Fatalf("baseline + killed turn effects=%d, want 2", lines)
	}
	session := bootResumeVault(t, root)
	store, err := turnstate.Open(
		t.Context(), filepath.Join(root, "turnstate.db"), session,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := store.Close(ctx); err != nil {
			t.Errorf("close resumed store: %v", err)
		}
	})
	killed, err := store.LoadTurnState(t.Context(), resumeTestTurn)
	if err != nil || killed.Checkpoint == nil ||
		killed.Checkpoint.PendingCall != nil ||
		roleSequence(killed.Checkpoint.Messages) != "user,assistant,tool" {
		t.Fatalf("killed checkpoint=%+v err=%v", killed, err)
	}
	killedEvidence := decodeEvidence(t, killed.Checkpoint.ToolEvents)
	if !sameEvidence(baseline.ToolEvents, killedEvidence) {
		t.Fatalf("evidence mismatch baseline=%+v killed=%+v",
			baseline.ToolEvents, killedEvidence)
	}

	phase.Store(3)
	phaseCalls.Store(0)
	manager := realExecManager(t)
	toolAdapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &recordingReporter{}
	reconciler := &TaskLedgerReconciler{
		Tasks:    task.Open(filepath.Join(root, "tasks")),
		Turns:    store,
		Reporter: reporter,
		NewLoop: func(state turnstate.TurnState) (*Loop, error) {
			return New(
				realMiMoGenerator(t, gateway.URL),
				toolAdapter, store,
				Config{
					TurnID:         state.TurnID,
					ConversationID: state.SessionID,
					Model:          "mimo-v2",
					IdleTimeout:    5 * time.Second,
				},
				Dependencies{Delivery: &DeliveryChoke{
					Reporter: reporter,
				}},
			)
		},
	}
	result, err := reconciler.Reconcile(t.Context())
	if err != nil || result.Examined != 1 || result.Resumed != 1 ||
		result.Completed != 1 || result.Incomplete != 0 ||
		result.Surfaced != 0 {
		t.Fatalf("boot reconciliation=%+v err=%v", result, err)
	}
	if lines := evidenceLines(t, effectPath); lines != 2 {
		t.Fatalf("resume replayed completed evidence effect: lines=%d", lines)
	}
	finished, err := store.LoadTurnState(t.Context(), resumeTestTurn)
	if err != nil || finished.Status != turnstate.StatusCompleted ||
		finished.Checkpoint == nil {
		t.Fatalf("finished turn=%+v err=%v", finished, err)
	}
	finishedEvidence := decodeEvidence(
		t, finished.Checkpoint.ToolEvents,
	)
	if !sameEvidence(baseline.ToolEvents, finishedEvidence) {
		t.Fatalf("resumed evidence mismatch baseline=%+v resumed=%+v",
			baseline.ToolEvents, finishedEvidence)
	}
	taskState, ok := reconciler.Tasks.Get(resumeTestConv)
	if !ok || taskState.Status != task.StatusDone {
		t.Fatalf("task ledger state=%+v ok=%t", taskState, ok)
	}
	if said := reporter.said(); len(said) != 1 ||
		!strings.Contains(said[0], "report is complete") {
		t.Fatalf("resumed delivery=%v", said)
	}
}

func runUninterruptedEvidenceReport(
	t *testing.T,
	root string,
	gatewayURL string,
	effectPath string,
) Response {
	t.Helper()
	turnID := "resurrection-baseline-turn"
	userContent := "Gather the evidence and deliver the report."
	session := bootResumeVault(t, root)
	store, err := turnstate.Open(
		t.Context(), filepath.Join(root, "baseline.db"), session,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := store.Close(ctx); err != nil {
			t.Fatal(err)
		}
	}()
	if err := store.CreateTurnState(
		t.Context(),
		turnstate.TurnState{
			TurnID: turnID, ActorID: "resurrection-test",
			SessionID: "baseline-conversation",
			Content:   userContent, Status: turnstate.StatusRunning,
			UpdatedAt: time.Now().UTC(),
		},
	); err != nil {
		t.Fatal(err)
	}
	manager := realExecManager(t)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gatewayURL), adapter, store,
		Config{
			TurnID: turnID, Model: "mimo-v2",
			IdleTimeout: 5 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(t.Context(), userContent)
	if err != nil {
		t.Fatal(err)
	}
	if lines := evidenceLines(t, effectPath); lines != 1 {
		t.Fatalf("uninterrupted evidence effects=%d, want 1", lines)
	}
	return response
}

func runResurrectionKillHelper(t *testing.T) {
	root := os.Getenv("RESURRECTION_KILL_ROOT")
	gatewayURL := os.Getenv("RESURRECTION_KILL_GATEWAY")
	effectPath := os.Getenv("RESURRECTION_KILL_EFFECT")
	if root == "" || gatewayURL == "" || effectPath == "" {
		os.Exit(2)
	}
	store, err := turnstate.Open(
		context.Background(), filepath.Join(root, "turnstate.db"),
		bootResumeVault(t, root),
	)
	if err != nil {
		os.Exit(3)
	}
	userContent := "Gather the evidence and deliver the report."
	if err := store.CreateTurnState(
		context.Background(),
		turnstate.TurnState{
			TurnID: resumeTestTurn, ActorID: "resurrection-test",
			SessionID: resumeTestConv,
			Content:   userContent, Status: turnstate.StatusRunning,
			UpdatedAt: time.Now().UTC(),
		},
	); err != nil {
		os.Exit(4)
	}
	task.Open(filepath.Join(root, "tasks")).Begin(
		resumeTestConv, resumeTestTurn, userContent,
	)
	manager := realExecManager(t)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		os.Exit(5)
	}
	runtimeLoop, err := New(
		realMiMoGenerator(t, gatewayURL), adapter, store,
		Config{
			TurnID: resumeTestTurn, ConversationID: resumeTestConv,
			Model: "mimo-v2", IdleTimeout: 30 * time.Second,
		},
		Dependencies{},
	)
	if err != nil {
		os.Exit(6)
	}
	_, _ = runtimeLoop.Turn(context.Background(), userContent)
	os.Exit(7)
}

func bootResumeVault(t *testing.T, root string) *vault.Session {
	t.Helper()
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: root, UserDID: resumeTestUser,
		KEKHex: hex.EncodeToString(bytes.Repeat([]byte{0x6d}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func evidenceLines(t *testing.T, path string) int {
	t.Helper()
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return len(strings.Fields(string(content)))
}

func decodeEvidence(
	t *testing.T,
	raw []json.RawMessage,
) []ToolExecution {
	t.Helper()
	result := make([]ToolExecution, 0, len(raw))
	for _, encoded := range raw {
		var event ToolExecution
		if err := json.Unmarshal(encoded, &event); err != nil {
			t.Fatal(err)
		}
		result = append(result, event)
	}
	return result
}

func sameEvidence(left, right []ToolExecution) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index].Call.Name != right[index].Call.Name ||
			string(left[index].Call.Arguments) !=
				string(right[index].Call.Arguments) ||
			normalizedEvidenceResult(left[index].Result) !=
				normalizedEvidenceResult(right[index].Result) ||
			left[index].Error != right[index].Error ||
			left[index].Expect != right[index].Expect {
			return false
		}
	}
	return true
}

func normalizedEvidenceResult(raw json.RawMessage) string {
	var decoded interface{}
	if json.Unmarshal(raw, &decoded) != nil {
		return string(raw)
	}
	stripVolatileEvidenceFields(decoded)
	normalized, err := json.Marshal(decoded)
	if err != nil {
		return string(raw)
	}
	return string(normalized)
}

func stripVolatileEvidenceFields(value interface{}) {
	switch current := value.(type) {
	case map[string]interface{}:
		delete(current, "duration_ms")
		for _, nested := range current {
			stripVolatileEvidenceFields(nested)
		}
	case []interface{}:
		for _, nested := range current {
			stripVolatileEvidenceFields(nested)
		}
	}
}

func TestTaskLedgerReconcilerSurfacesUnsafeOrphan(t *testing.T) {
	root := t.TempDir()
	tasks := task.Open(filepath.Join(root, "tasks"))
	tasks.Begin("unsafe-conversation", "missing-turn", "Do not replay me.")
	reporter := &recordingReporter{}
	store, err := turnstate.Open(
		t.Context(), filepath.Join(root, "turnstate.db"),
		bootResumeVault(t, root),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close(context.Background())
	reconciler := &TaskLedgerReconciler{
		Tasks: tasks, Turns: store, Reporter: reporter,
		NewLoop: func(turnstate.TurnState) (*Loop, error) {
			t.Fatal("unsafe orphan must not construct a loop")
			return nil, nil
		},
	}
	result, err := reconciler.Reconcile(t.Context())
	if err != nil || result.Surfaced != 1 || result.Resumed != 0 {
		t.Fatalf("unsafe orphan result=%+v err=%v", result, err)
	}
	state, ok := tasks.Get("unsafe-conversation")
	if !ok || state.Status != task.StatusCeiling ||
		len(reporter.said()) != 1 {
		t.Fatalf("unsafe orphan task=%+v reporter=%v", state, reporter.said())
	}
}
