// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

func TestInteractionPostureClassificationAndFlagOff(t *testing.T) {
	cfg := config.Default()
	if got := ClassifyInteractionPosture(cfg, "hello there"); got != PostureExecution {
		t.Fatalf("flag-off posture = %q, want execution", got)
	}
	cfg.InteractionPosture = true
	cases := []struct {
		request string
		want    InteractionPosture
	}{
		{"hello, how has your day been?", PostureConversation},
		{"research the current implementation and compare the choices", PostureExploration},
		{"please implement the change and run the tests", PostureExecution},
		{"can you send the message after checking it?", PostureExecution},
		{"look up the status but do not change anything", PostureExecution},
	}
	for _, tc := range cases {
		if got := ClassifyInteractionPosture(cfg, tc.request); got != tc.want {
			t.Errorf("ClassifyInteractionPosture(%q) = %q, want %q", tc.request, got, tc.want)
		}
	}
	for _, name := range []string{"fs__write_file", "browser__click", "mail__send_message", "shell__exec"} {
		if !mutatingToolName(name) || readOnlyToolName(name) {
			t.Errorf("state-changing tool %q was not execution-only", name)
		}
	}
}

func TestConversationPostureKeepsCapabilitiesVisibleWithoutExecutionFrame(t *testing.T) {
	var calls atomic.Int32
	var advertised atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []json.RawMessage `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		calls.Add(1)
		advertised.Add(int32(len(request.Tools)))
		writeSSEAnswer(w, "I am here. What is on your mind?")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.InteractionPosture = true
	cfg.CassandraEnabled = true
	cfg.EpistemicPremises = true
	cfg.EpistemicPredictions = true
	client := revisionTestClient(t, model.URL)
	a := New(Options{Config: cfg, Main: client, Cheap: client})
	if err := a.Chat(context.Background(), "hello, how are you today?"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 || advertised.Load() == 0 {
		t.Fatalf("conversation calls/tools = %d/%d, want one call with the live capability surface", calls.Load(), advertised.Load())
	}
	if a.TurnPosture() != PostureConversation || a.turn.runLedger != nil || len(a.turn.verifiers.Verifiers) != 0 || a.turn.ledger != nil || len(a.turn.casRecord) != 0 {
		t.Fatalf("conversation carried execution frame: posture=%q ledger=%v verifiers=%+v premises=%+v cassandra=%+v", a.TurnPosture(), a.turn.runLedger, a.turn.verifiers, a.turn.ledger, a.turn.casRecord)
	}
}

func TestExplorationPostureRealReadOnlyTool(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	target := filepath.Join(root, "facts.txt")
	if err := os.WriteFile(target, []byte("grounded exploration"), 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: fsToolManifest(t, root), StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("real filesystem MCP unavailable: %v", warnings)
	}
	readName := ""
	for _, schema := range manager.Schemas() {
		if readOnlyToolName(schema.Function.Name) && strings.Contains(schema.Function.Name, "read") {
			readName = schema.Function.Name
			break
		}
	}
	if readName == "" {
		t.Fatal("filesystem MCP exposed no deterministic read-only schema")
	}
	args, _ := json.Marshal(map[string]string{"path": target, "expect": "the file content is returned"})
	call := fmt.Sprintf(`{"index":0,"id":"read-1","type":"function","function":{"name":%q,"arguments":%q}}`, readName, string(args))
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Tools []llm.Tool `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
		}
		foundWrite := false
		for _, schema := range request.Tools {
			foundWrite = foundWrite || schema.Function.Name == "fs__write_file"
		}
		if !foundWrite {
			t.Error("exploration prose hid the live write capability from the model")
		}
		if calls.Add(1) == 1 {
			writeSSEToolCall(w, call)
			return
		}
		writeSSEAnswer(w, "The file says grounded exploration.")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.InteractionPosture = true
	cfg.CassandraEnabled = true
	client := revisionTestClient(t, model.URL)
	a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: manager})
	if err := a.Chat(ctx, "read the facts file and tell me what it says"); err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 2 || a.TurnPosture() != PostureExploration || a.turn.runLedger != nil || len(a.turn.casRecord) != 0 {
		t.Fatalf("exploration state: calls=%d posture=%q ledger=%v cassandra=%+v", calls.Load(), a.TurnPosture(), a.turn.runLedger, a.turn.casRecord)
	}
}

func TestQueuedSideEffectPromotesBeforeRealDispatch(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	target := filepath.Join(root, "promoted.txt")
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: fsToolManifest(t, root), StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("real filesystem MCP unavailable: %v", warnings)
	}
	args, _ := json.Marshal(map[string]string{"path": target, "content": "promoted safely", "expect": "the file exists with the requested content"})
	call := fmt.Sprintf(`{"index":0,"id":"write-1","type":"function","function":{"name":"fs__write_file","arguments":%q}}`, string(args))
	var calls atomic.Int32
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			writeSSEToolCall(w, call)
			return
		}
		writeSSEAnswer(w, "The requested file was written.")
	}))
	t.Cleanup(model.Close)

	var inboxCalls atomic.Int32
	var dispatchSawExecution atomic.Bool
	cfg := config.Default()
	cfg.InteractionPosture = true
	cfg.SessionCurrentIntent = true
	cfg.CassandraEnabled = false
	client := revisionTestClient(t, model.URL)
	a := New(Options{
		Config: cfg, Main: client, Cheap: client, Tools: manager,
		Inbox: func() []string {
			if inboxCalls.Add(1) == 1 {
				return []string{"write the promoted.txt file with the requested content"}
			}
			return nil
		},
	})
	a.observer = func(event ToolEvent) {
		if event.Phase == ToolStart && a.TurnPosture() == PostureExecution && a.turn.runLedger != nil && a.turn.promoted {
			dispatchSawExecution.Store(true)
		}
	}
	if err := a.Chat(ctx, "I have another thought"); err != nil {
		t.Fatal(err)
	}
	content, err := os.ReadFile(target)
	if err != nil || string(content) != "promoted safely" {
		t.Fatalf("real promoted effect = %q, %v", content, err)
	}
	if !dispatchSawExecution.Load() {
		t.Fatal("state-changing dispatch began before execution posture and O1 ledger existed")
	}
}

func writeSSEAnswer(w http.ResponseWriter, answer string) {
	w.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(answer)
	fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%s},\"finish_reason\":\"stop\"}]}\n", payload)
	fmt.Fprint(w, "data: [DONE]\n")
}

func writeSSEToolCall(w http.ResponseWriter, call string) {
	w.Header().Set("Content-Type", "text/event-stream")
	fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", call)
	fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
	fmt.Fprint(w, "data: [DONE]\n")
}
