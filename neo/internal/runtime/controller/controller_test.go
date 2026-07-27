// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package controller

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
	"sync"
	"testing"
	"time"

	"matrix/cortex"
	cortexstore "matrix/cortex/store"
	executortool "matrix/executor/tool"
	"matrix/neo/internal/config"
	neomemory "matrix/neo/internal/memory"
	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/turnstate"
	neotools "matrix/neo/internal/tools"
	"matrix/vault"
)

const doubtMarker = "not what I predicted it would return"

// A real prediction mismatch arms the silent voice: the next provider call is
// a tools-stripped revision carrying the doubt edit, the model recommits a
// revised plan, and the following call is tool-exposed again with no trace of
// the guidance. The doubt line rides request-time clones only — it never
// reaches the durable transcript or the cortex transcript.
func TestPredictionMismatchDrivesToolsStrippedRevisionAndPlanRecommit(
	t *testing.T,
) {
	ctx := t.Context()
	root := t.TempDir()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: root,
		UserDID: "did:matrix:controller-test",
		KEKHex:  hex.EncodeToString(bytes.Repeat([]byte{0x21}, 32)),
	})
	if err != nil {
		t.Fatal(err)
	}
	journalStore, err := cortexstore.Open(
		filepath.Join(root, "cortex-journal"), "controller-test", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	journalStore.SetVault(session, "did:matrix:controller-test")
	t.Cleanup(func() { _ = journalStore.Close() })
	cx := cortex.New(journalStore)

	conversationID := "controller-conversation"
	recorder, pager := realRecorder(t, conversationID)
	audit := &recordingAuditor{}
	silentVoice := New(Cadence{}, audit, nil)

	workdir := t.TempDir()
	manager := execManager(t)
	var (
		mu       sync.Mutex
		requests []gatewayProbe
		step     int
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			body, _ := io.ReadAll(request.Body)
			var decoded gatewayProbe
			_ = json.Unmarshal(body, &decoded)
			if answerCapabilityCanary(writer, decoded) {
				return
			}
			mu.Lock()
			requests = append(requests, decoded)
			step++
			current := step
			mu.Unlock()
			switch current {
			case 1:
				writeToolFrame(
					writer, "baseline-call", "exec__shell",
					map[string]interface{}{
						"command": "printf baseline-ready",
						"cwd":     workdir,
						"expect":  "prints baseline-ready",
					},
				)
			case 2:
				writeToolFrame(
					writer, "mismatch-call", "exec__shell",
					map[string]interface{}{
						"command": "exit 7",
						"cwd":     workdir,
						"expect":  "exit 0 and prints the manifest",
					},
				)
			case 3:
				writeTextFrame(
					writer,
					"Revised plan: the manifest check exited non-zero, so I "+
						"will report that failure instead of building on it.",
				)
			default:
				writeTextFrame(
					writer,
					"The baseline step printed baseline-ready and the "+
						"manifest check exited non-zero, so the manifest is "+
						"reported as unavailable rather than assumed.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)

	turnID := "controller-turn"
	userContent := "Check the manifest and report what the evidence shows."
	turns := openTurnStore(t, session, root, turnID, userContent)
	tools, err := loop.NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	runtimeLoop, err := loop.New(
		realGenerator(t, gateway.URL), tools, turns,
		loop.Config{
			TurnID: turnID, ConversationID: conversationID,
			Model: "mimo-v2", IdleTimeout: 20 * time.Second,
		},
		loop.Dependencies{
			EvidenceJournal: &loop.CortexToolJournal{
				Cortex: cx, CreatedBy: "did:matrix:controller-test",
			},
			Doubt:    silentVoice,
			Recorder: recorder,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtimeLoop.Turn(ctx, userContent)
	if err != nil {
		t.Fatalf("turn err=%v response=%+v", err, response)
	}
	if !strings.Contains(response.Content, "exited non-zero") {
		t.Fatalf("delivered content=%q", response.Content)
	}

	mu.Lock()
	captured := append([]gatewayProbe(nil), requests...)
	mu.Unlock()
	if len(captured) != 4 {
		t.Fatalf("provider calls=%d want 4", len(captured))
	}
	// The mismatch fired at provider step 2, so call 3 is the revision.
	revision := captured[2]
	if len(revision.Tools) != 0 {
		t.Fatalf("revision step was not tools-stripped: %d tools",
			len(revision.Tools))
	}
	if !messagesContain(revision.Messages, doubtMarker) {
		t.Fatalf("revision step carried no doubt edit: %+v",
			revision.Messages)
	}
	folded := 0
	for _, message := range revision.Messages {
		if strings.Contains(message.Content, doubtMarker) {
			folded++
			if message.Role != "assistant" {
				t.Fatalf("doubt edit landed on a %s message", message.Role)
			}
			if !strings.Contains(message.Content, "exit 0 and prints") &&
				strings.TrimSpace(message.Content) == doubtMarker {
				t.Fatal("doubt edit replaced the original content")
			}
		}
	}
	if folded != 1 {
		t.Fatalf("doubt edit folded into %d messages, want 1", folded)
	}
	// Request-local: exactly one provider call ever saw the guidance, and the
	// recommit call is tool-exposed again.
	recommit := captured[3]
	if len(recommit.Tools) == 0 {
		t.Fatal("plan recommit did not restore the tool surface")
	}
	if messagesContain(recommit.Messages, doubtMarker) {
		t.Fatalf("doubt edit leaked into a later request: %+v",
			recommit.Messages)
	}
	if !messagesContain(recommit.Messages, "Revised plan:") {
		t.Fatalf("revised plan was not recommitted: %+v", recommit.Messages)
	}

	// Clean-window law: guidance never reached durable state.
	if response.Checkpoint == nil {
		t.Fatal("no durable checkpoint")
	}
	for _, message := range response.Checkpoint.Messages {
		if strings.Contains(message.Content, doubtMarker) {
			t.Fatalf("doubt edit reached the durable transcript: %+v",
				message)
		}
	}
	transcript, err := pager.Transcript(conversationID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(transcript) == 0 {
		t.Fatal("cortex transcript was never written")
	}
	for _, message := range transcript {
		if strings.Contains(message.Content, doubtMarker) {
			t.Fatalf("doubt edit reached the cortex transcript: %+v",
				message)
		}
	}

	// Provenance: one dual record, anchored to the evidence that armed it.
	mods := silentVoice.Mods()
	if len(mods) != 1 || mods[0].Trigger != TriggerPredictionMismatch ||
		mods[0].Side != SideDoubt || mods[0].State != EditActive ||
		mods[0].Citation == nil || mods[0].Step != 2 {
		t.Fatalf("controller provenance=%+v", mods)
	}
	if _, err := cx.VerifyToolEventCitation(*mods[0].Citation); err != nil {
		t.Fatalf("armed citation did not verify: %v", err)
	}
	if recorded := audit.events(); len(recorded) != 1 ||
		recorded[0].ID != mods[0].ID {
		t.Fatalf("auditor events=%+v", recorded)
	}
	undone, err := silentVoice.Undo(mods[0].ID)
	if err != nil || undone.State != EditUndone || undone.UndoneAt == nil {
		t.Fatalf("undo=%+v err=%v", undone, err)
	}
	if again := silentVoice.Mods(); again[0].State != EditUndone {
		t.Fatalf("undo was not durable in provenance: %+v", again)
	}
	if _, err := silentVoice.Undo(mods[0].ID); err == nil {
		t.Fatal("an undone edit was undone twice")
	}
}

// The cadence bounds are the matrix Cassandra 2.0 values, and each one is
// enforced by the controller rather than requested of it.
func TestCadenceBoundsAreEnforcedVerbatim(t *testing.T) {
	silentVoice := New(Cadence{}, nil, nil)
	cadence := silentVoice.Cadence()
	if cadence.MinStep != 2 || cadence.MaxModsPerTurn != 3 ||
		cadence.CooldownSteps != 2 {
		t.Fatalf("cadence=%+v", cadence)
	}
	citation := &cortex.ToolEventCitation{Seq: 1, LeafCount: 2}
	mismatch := func(step int) Signals {
		return Signals{
			Step: step, MismatchedEvidence: 1, Citation: citation,
		}
	}
	if _, armed := silentVoice.Observe(mismatch(1)); armed {
		t.Fatal("controller fired before min_step")
	}
	if _, armed := silentVoice.Observe(mismatch(2)); !armed {
		t.Fatal("controller stayed silent at min_step")
	}
	if _, armed := silentVoice.Observe(mismatch(3)); armed {
		t.Fatal("controller fired inside the cooldown window")
	}
	if _, armed := silentVoice.Observe(mismatch(4)); !armed {
		t.Fatal("controller stayed silent after the cooldown elapsed")
	}
	if _, armed := silentVoice.Observe(mismatch(6)); !armed {
		t.Fatal("controller stayed silent on the third mod")
	}
	if _, armed := silentVoice.Observe(mismatch(8)); armed {
		t.Fatal("controller exceeded max_mods_per_turn")
	}
	if mods := silentVoice.Mods(); len(mods) != 3 {
		t.Fatalf("mods=%d want 3", len(mods))
	}
	silentVoice.NewTurn()
	if _, armed := silentVoice.Observe(mismatch(2)); !armed {
		t.Fatal("the per-turn budget did not reset")
	}
	// Silence when healthy: matched evidence never arms the voice.
	if _, armed := silentVoice.Observe(
		Signals{Step: 9, Citation: citation},
	); armed {
		t.Fatal("controller fired on a healthy step")
	}
}

// Evidence with no verified citation cannot arm the voice: the mismatch
// verdict itself would be uncited.
func TestUncitedEvidenceCannotArmTheVoice(t *testing.T) {
	silentVoice := New(Cadence{}, nil, nil)
	if _, armed := silentVoice.ObserveMismatch(
		context.Background(), 2,
		loop.ToolExecution{
			Call:         protocol.NormalizedToolCall{ID: "x", Name: "y"},
			MatchVerdict: cortex.ToolMatchMismatched,
		},
	); armed {
		t.Fatal("uncited mismatch armed the voice")
	}
	if mods := silentVoice.Mods(); len(mods) != 0 {
		t.Fatalf("uncited mismatch recorded provenance: %+v", mods)
	}
}

type recordingAuditor struct {
	mu       sync.Mutex
	recorded []Mod
}

func (auditor *recordingAuditor) RecordDoubtEdit(mod Mod) error {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	auditor.recorded = append(auditor.recorded, mod)
	return nil
}

func (auditor *recordingAuditor) events() []Mod {
	auditor.mu.Lock()
	defer auditor.mu.Unlock()
	return append([]Mod(nil), auditor.recorded...)
}

func messagesContain(messages []probeMessage, needle string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, needle) {
			return true
		}
	}
	return false
}

func realRecorder(
	t *testing.T,
	conversationID string,
) (*loop.CortexAdapter, *neomemory.Pager) {
	t.Helper()
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "resurrection-controller-" + conversationID
	cfg.ContextWindowTokens = 1_000_000
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := pager.Close(); err != nil {
			t.Errorf("close cortex pager: %v", err)
		}
	})
	if _, err := pager.SetMemoryConsent(
		t.Context(), true, "runtime controller test user",
	); err != nil {
		t.Fatal(err)
	}
	adapter, err := loop.NewCortexAdapter(pager, cfg, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	return adapter, pager
}

func execManager(t *testing.T) *neotools.Manager {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for real exec dispatch: %v", err)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve controller test source")
	}
	execBridge := filepath.Clean(filepath.Join(
		filepath.Dir(thisFile), "..", "..", "..", "..",
		"tools", "exec", "exec.mjs",
	))
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/controller-test",
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
			TurnID: turnID, ActorID: "controller-test",
			SessionID: "controller-session",
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
		ActorDID:       "did:matrix:controller-test",
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

type probeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type gatewayProbe struct {
	Messages []probeMessage `json:"messages"`
	Tools    []struct {
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
				"content": "Running the next evidence step.",
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
