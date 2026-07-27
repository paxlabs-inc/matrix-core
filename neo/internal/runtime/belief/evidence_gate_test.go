// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package belief

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	cortexstore "matrix/cortex/store"
	executortool "matrix/executor/tool"
	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/turnstate"
	neotools "matrix/neo/internal/tools"
	"matrix/vault"
)

// Drives one real failing tool call and one real succeeding call through the
// real loop against the real exec bridge, and proves the failure is journaled
// as evidence yet cannot complete its subgoal, cannot become capability
// evidence, and lands as a refuted premise instead.
func TestRealToolFailureIsJournaledButCannotCompleteSubgoalOrCapability(
	t *testing.T,
) {
	ctx := t.Context()
	root := t.TempDir()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: root,
		UserDID: "did:matrix:evidence-gate-test",
		KEKHex:  hex.EncodeToString(bytes.Repeat([]byte{0x37}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	journalStore, err := cortexstore.Open(
		filepath.Join(root, "cortex"), "evidence-gate-test", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	journalStore.SetVault(session, "did:matrix:evidence-gate-test")
	t.Cleanup(func() { _ = journalStore.Close() })
	cx := cortex.New(journalStore)

	turnID := "evidence-gate-turn"
	userContent := "Probe the service, then record the verified evidence."
	turns := openTurnStore(t, session, root, turnID, userContent)
	state, err := New("evidence-gate-session", cx, turns)
	if err != nil {
		t.Fatal(err)
	}

	workdir := t.TempDir()
	manager := execManager(t)
	var step int
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayProbe
			_ = json.Unmarshal(body, &decoded)
			if answerCapabilityCanary(writer, decoded) {
				return
			}
			step++
			switch step {
			case 1:
				// No command: the bridge's own argument validation rejects
				// this before any round trip, returning a real is_error
				// result with a real failure class.
				writeToolFrame(
					writer, "broken-probe-call", "exec__shell",
					map[string]interface{}{
						"cwd":    workdir,
						"expect": "prints the probe payload",
					},
				)
			case 2:
				writeToolFrame(
					writer, "verified-call", "exec__shell",
					map[string]interface{}{
						"command": "printf verified-evidence",
						"cwd":     workdir,
						"expect":  "prints verified-evidence",
					},
				)
			default:
				writeTextFrame(
					writer,
					"The probe call failed; the verified-evidence step "+
						"completed and is reported here.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)

	tools, err := loop.NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := loop.New(
		realGenerator(t, gateway.URL), tools, turns,
		loop.Config{
			TurnID: turnID, ConversationID: "evidence-gate-conversation",
			Model: "mimo-v2", IdleTimeout: 20 * time.Second,
		},
		loop.Dependencies{
			EvidenceJournal: &loop.CortexToolJournal{
				Cortex: cx, CreatedBy: "did:matrix:evidence-gate-test",
			},
			EvidenceObserver: state,
			Subgoals: callSubgoals{
				"broken-probe-call": "probe-subgoal",
				"verified-call":     "report-subgoal",
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(ctx, userContent)
	if err != nil || len(response.ToolEvents) != 2 {
		t.Fatalf("response=%+v err=%v", response, err)
	}

	failed, verified := response.ToolEvents[0], response.ToolEvents[1]
	if failed.Call.ID != "broken-probe-call" ||
		verified.Call.ID != "verified-call" {
		t.Fatalf("unexpected evidence order: %+v", response.ToolEvents)
	}
	if failed.Error == "" ||
		failed.MatchVerdict != cortex.ToolMatchMismatched {
		t.Fatalf("failed evidence=%+v", failed)
	}
	if verified.Error != "" ||
		verified.MatchVerdict != cortex.ToolMatchMatched {
		t.Fatalf("verified evidence=%+v", verified)
	}
	// Both executions are journaled — a failure is evidence, not a gap.
	for _, event := range []loop.ToolExecution{failed, verified} {
		if event.Citation == nil {
			t.Fatalf("%s committed no citation", event.Call.ID)
		}
		payload, err := cx.VerifyToolEventCitation(*event.Citation)
		if err != nil || payload.CallID != event.Call.ID ||
			payload.MatchVerdict != event.MatchVerdict ||
			payload.Error != event.Error ||
			payload.SubgoalID != event.SubgoalID {
			t.Fatalf("%s citation payload=%+v err=%v",
				event.Call.ID, payload, err)
		}
	}

	snapshot := state.Snapshot()
	if snapshot.Subgoals["probe-subgoal"].Completed ||
		len(snapshot.Subgoals["probe-subgoal"].Evidence) != 0 {
		t.Fatalf("failed evidence completed its subgoal: %+v", snapshot)
	}
	if !snapshot.Subgoals["report-subgoal"].Completed {
		t.Fatalf("verified evidence did not complete its subgoal: %+v",
			snapshot)
	}
	if snapshot.Capabilities["exec__shell"].VerifiedSuccesses != 1 {
		t.Fatalf("capability counted the failure: %+v", snapshot)
	}
	refuted := snapshot.Premises["prediction:broken-probe-call"]
	if refuted.Status != PremiseRefuted ||
		refuted.Statement != "prints the probe payload" {
		t.Fatalf("failed prediction premise=%+v", refuted)
	}
	if err := state.CanAct("prediction:broken-probe-call"); err == nil {
		t.Fatal("refuted prediction premise did not block dependent action")
	}
	if err := state.CitePremise(
		ctx, "prediction:broken-probe-call", *failed.Citation,
	); err == nil {
		t.Fatal("mismatched citation was accepted as support")
	}
}

type callSubgoals map[string]string

func (subgoals callSubgoals) SubgoalFor(
	call protocol.NormalizedToolCall,
) string {
	return subgoals[call.ID]
}

func execManager(t *testing.T) *neotools.Manager {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for real exec dispatch: %v", err)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve belief test source")
	}
	execBridge := filepath.Clean(filepath.Join(
		filepath.Dir(thisFile), "..", "..", "..", "..",
		"tools", "exec", "exec.mjs",
	))
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/evidence-gate-test",
		Servers: []executortool.ServerEntry{{
			Alias: "exec", Transport: "stdio",
			Command: "node", Args: []string{execBridge},
			PackageDigest: "sha256:" + strings.Repeat("c", 64),
			Version:       "0.1.0",
			Tools: []executortool.ToolEntry{
				{
					Name:            "shell",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_start",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_list",
					SideEffectClass: executortool.SideEffectRead,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_logs",
					SideEffectClass: executortool.SideEffectRead,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_stop",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
				{
					Name:            "service_restart",
					SideEffectClass: executortool.SideEffectShell,
					TimeoutMs:       10_000,
				},
			},
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(
		t.Context(),
		neotools.Options{
			ManifestPath: manifestPath,
			SpawnTimeout: 20 * time.Second,
			StderrSink:   io.Discard,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if warnings := manager.Warnings(); len(warnings) != 0 {
		_ = manager.Close()
		t.Fatalf("real exec bridge warnings: %v", warnings)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func openTurnStore(
	t *testing.T,
	session *vault.Session,
	root string,
	turnID string,
	userContent string,
) *turnstate.Store {
	t.Helper()
	store, err := turnstate.Open(
		t.Context(), filepath.Join(root, "turnstate.db"), session,
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTurnState(
		t.Context(),
		turnstate.TurnState{
			TurnID: turnID, ActorID: "evidence-gate-test",
			SessionID: "evidence-gate-session",
			Content:   userContent, Status: turnstate.StatusRunning,
			UpdatedAt: time.Now().UTC(),
		},
	); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		closeCtx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := store.Close(closeCtx); err != nil {
			t.Errorf("close turn store: %v", err)
		}
	})
	return store
}

func realGenerator(
	t *testing.T,
	gatewayURL string,
) *provider.MiMoGenerator {
	t.Helper()
	t.Setenv("MATRIX_GATEWAY_TOKEN", "test-gateway-token")
	adapter := &provider.MiMoAdapter{}
	client, err := provider.New(adapter, provider.Config{
		GatewayURL:     gatewayURL,
		BearerEnv:      "MATRIX_GATEWAY_TOKEN",
		ActorDID:       "did:matrix:evidence-gate-test",
		MaxAttempts:    3,
		BackoffInitial: time.Millisecond,
		BackoffMax:     2 * time.Millisecond,
		IdleTimeout:    20 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	generator, err := provider.NewMiMoGenerator(client, adapter)
	if err != nil {
		t.Fatal(err)
	}
	return generator
}

type gatewayProbe struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func answerCapabilityCanary(
	writer http.ResponseWriter,
	request gatewayProbe,
) bool {
	canary := false
	for _, tool := range request.Tools {
		if tool.Function.Name == "matrix_runtime_capability_echo" {
			canary = true
			break
		}
	}
	if !canary {
		return false
	}
	latest := request.Messages[len(request.Messages)-1].Content
	if strings.Contains(latest, "Reply with READY") {
		writeTextFrame(writer, "READY")
		return true
	}
	writeToolFrame(
		writer, "capability-call", "matrix_runtime_capability_echo",
		map[string]interface{}{
			"value": "READY", "expect": "returns READY",
		},
	)
	return true
}

func writeToolFrame(
	writer http.ResponseWriter,
	id string,
	name string,
	arguments map[string]interface{},
) {
	writer.Header().Set("Content-Type", "text/event-stream")
	rawArguments, _ := json.Marshal(arguments)
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{
					"index": 0, "id": id, "type": "function",
					"function": map[string]interface{}{
						"name": name, "arguments": string(rawArguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7,
		},
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func writeTextFrame(writer http.ResponseWriter, content string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]interface{}{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 4, "completion_tokens": 3, "total_tokens": 7,
		},
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}
