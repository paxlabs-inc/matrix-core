// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/sessionjournal"
	"matrix/neo/internal/tools"
)

func TestExactProjectionRealMultiStepSidecarFrozenAndJournaled(t *testing.T) {
	ctx := context.Background()
	const conversationID = "conv-exact-projection"
	journal := openRealAgentJournal(t)
	workdir := t.TempDir()
	manager, err := tools.Spawn(ctx, tools.Options{ManifestPath: fsToolManifest(t, workdir), StderrSink: io.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close() })
	if warnings := manager.Warnings(); len(warnings) != 0 {
		t.Fatalf("real filesystem MCP unavailable: %v", warnings)
	}
	target := filepath.Join(workdir, "projection.txt")
	arguments, err := json.Marshal(map[string]string{"path": target, "content": "frozen projection"})
	if err != nil {
		t.Fatal(err)
	}
	call := fmt.Sprintf(`{"index":0,"id":"projection-call","type":"function","function":{"name":"fs__write_file","arguments":%q}}`, string(arguments))

	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), raw...))
		step := len(bodies)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if step == 1 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", call)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
		} else {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
		}
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.SessionCurrentIntent = true
	cfg.SessionExactProjection = true
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	a := New(Options{Config: cfg, Main: windowLawClient(t, model.URL), Tools: manager, ConvID: conversationID, Journal: journal})
	request := "write the projection file"
	if err := a.Chat(ctx, request); err != nil {
		t.Fatal(err)
	}
	if content, err := os.ReadFile(target); err != nil || string(content) != "frozen projection" {
		t.Fatalf("filesystem effect = %q, %v", content, err)
	}

	mu.Lock()
	captured := append([][]byte(nil), bodies...)
	mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("model request count = %d, want 2", len(captured))
	}
	var sidecars []string
	for index, body := range captured {
		messages := decodeWireMessages(t, body)
		newestUser := ""
		for i := len(messages) - 1; i >= 0; i-- {
			if messages[i].Role == "user" {
				newestUser = messages[i].Content
				break
			}
		}
		if newestUser != request {
			t.Fatalf("step %d newest user = %q, want real request", index, newestUser)
		}
		last := messages[len(messages)-1]
		if last.Role != "system" || !strings.Contains(last.Content, "<session_context>") {
			t.Fatalf("step %d context did not use system sidecar: %+v", index, last)
		}
		sidecars = append(sidecars, last.Content)
	}
	if sidecars[0] != sidecars[1] {
		t.Fatal("context projection changed within the multi-step turn")
	}

	replay, err := journal.Replay(ctx, conversationID)
	if err != nil {
		t.Fatal(err)
	}
	var requestFrames [][]byte
	for _, frame := range replay.ProviderFrames {
		if frame.Kind == sessionjournal.KindProviderReplay {
			requestFrames = append(requestFrames, frame.Bytes)
		}
	}
	if len(requestFrames) != len(captured) {
		t.Fatalf("journaled requests = %d, want %d", len(requestFrames), len(captured))
	}
	for index := range captured {
		if !bytes.Equal(requestFrames[index], captured[index]) {
			t.Fatalf("journaled provider request %d is not byte-exact", index)
		}
	}
}

func TestExactProjectionRendersCortexTranscriptAfterRealTrim(t *testing.T) {
	ctx := context.Background()
	var (
		mu     sync.Mutex
		bodies [][]byte
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, append([]byte(nil), raw...))
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"acknowledged"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.SessionCurrentIntent = true
	cfg.SessionExactProjection = true
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "exact-projection-trim"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	a := New(Options{Config: cfg, Main: windowLawClient(t, model.URL), Tools: &tools.Manager{}, Pager: pager, ConvID: "conv-exact-trim"})
	for index := 0; index < 8; index++ {
		if err := a.Chat(ctx, fmt.Sprintf("historical exact message %d", index)); err != nil {
			t.Fatal(err)
		}
	}
	a.cmTrimWorking()
	if err := a.Chat(ctx, "current request after the forced trim"); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	lastBody := append([]byte(nil), bodies[len(bodies)-1]...)
	mu.Unlock()
	messages := decodeWireMessages(t, lastBody)
	sidecar := messages[len(messages)-1]
	if sidecar.Role != "system" || !strings.Contains(sidecar.Content, "Exact Neocortex transcript slice recovered") || !strings.Contains(sidecar.Content, "historical exact message 0") {
		t.Fatalf("trimmed exact transcript was not projected:\n%s", sidecar.Content)
	}
	newestUser := ""
	for index := len(messages) - 1; index >= 0; index-- {
		if messages[index].Role == "user" {
			newestUser = messages[index].Content
			break
		}
	}
	if newestUser != "current request after the forced trim" {
		t.Fatalf("newest user after trim = %q", newestUser)
	}
}
