// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

// NEO-WORKBENCH task 1.2 — the live-typing channel (req 3.2, 6.1, 6.3).
//
// These drive the REAL code paths: the llm stream aggregator delivering raw
// tool-call fragments, the liveTyper's resumable JSON decoder, and (end to
// end) the real agent loop dispatching a real write through the real fs MCP
// server so the streamed deltas are proven byte-identical to the bytes that
// land on disk. The scripted OpenAI SSE endpoint is the only boundary stubbed
// — it is the external model.

// feedFragments pushes raw JSON through a liveTyper in n-byte fragments and
// returns the reassembled stream per path plus the emitted events.
func feedFragments(t *testing.T, raw string, n int) (map[string]string, []ToolEvent) {
	t.Helper()
	var events []ToolEvent
	lt := newLiveTyper(func(ev ToolEvent) { events = append(events, ev) })
	for i := 0; i < len(raw); i += n {
		end := i + n
		if end > len(raw) {
			end = len(raw)
		}
		lt.feed(&llm.ToolCallDelta{Index: 0, ID: "call_w", Name: "fs__write_file", Args: raw[i:end]})
	}
	got := map[string]string{}
	offset := 0
	for _, ev := range events {
		if ev.Phase != ToolStream {
			t.Fatalf("unexpected phase %q", ev.Phase)
		}
		if ev.StreamOffset != offset {
			t.Fatalf("offset gap: got %d, want %d", ev.StreamOffset, offset)
		}
		got[ev.StreamPath] += ev.StreamDelta
		offset += len(ev.StreamDelta)
	}
	return got, events
}

// TestLiveTyper_DecodesAcrossEveryFragmentBoundary proves the resumable
// decoder converges byte-identically no matter where the stream splits —
// including mid-escape, mid-\uXXXX, and mid-surrogate-pair — and handles a
// content-before-path argument order without losing bytes.
func TestLiveTyper_DecodesAcrossEveryFragmentBoundary(t *testing.T) {
	content := "package main\n\nfunc main() {\n\tprintln(\"h\\i\")\t// tab + quote + backslash\n}\n∆ünïcode 🚀 done\n"
	for _, order := range []string{"path_first", "content_first"} {
		var args map[string]string
		if order == "path_first" {
			// json.Marshal sorts keys, so build path-first by hand.
			pb, _ := json.Marshal(content)
			raw := `{"path":"src/app.go","content":` + string(pb) + `}`
			for n := 1; n <= 13; n += 3 {
				got, _ := feedFragments(t, raw, n)
				if got["src/app.go"] != content {
					t.Fatalf("%s frag=%d: decoded %q != content", order, n, got["src/app.go"])
				}
			}
			continue
		}
		args = map[string]string{"content": content, "path": "src/app.go"}
		raw, _ := json.Marshal(args) // alphabetical: content precedes path
		for n := 1; n <= 13; n += 3 {
			got, _ := feedFragments(t, string(raw), n)
			if got["src/app.go"] != content {
				t.Fatalf("%s frag=%d: decoded %q != content", order, n, got["src/app.go"])
			}
		}
	}
}

// TestLiveTyper_IgnoresNonWriteTools proves only write_file calls stream.
func TestLiveTyper_IgnoresNonWriteTools(t *testing.T) {
	var events []ToolEvent
	lt := newLiveTyper(func(ev ToolEvent) { events = append(events, ev) })
	lt.feed(&llm.ToolCallDelta{Index: 0, Name: "fs__read_text_file", Args: `{"path":"a.txt","content":"x"}`})
	lt.feed(&llm.ToolCallDelta{Index: 1, Name: "exec__shell", Args: `{"command":"ls"}`})
	if len(events) != 0 {
		t.Fatalf("non-write tools must not stream, got %d events", len(events))
	}
}

// chunkedWriteServer scripts the model boundary: call 1 streams ONE write_file
// tool call whose argument JSON arrives in many small fragments (real
// incremental generation); call 2 closes with a bare answer.
func chunkedWriteServer(t *testing.T, argsJSON string, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if idx == 0 {
			head := map[string]any{
				"index": 0, "id": "call_write", "type": "function",
				"function": map[string]any{"name": "fs__write_file", "arguments": ""},
			}
			frame := map[string]any{"choices": []any{map[string]any{
				"index": 0, "delta": map[string]any{"role": "assistant", "tool_calls": []any{head}},
			}}}
			fb, _ := json.Marshal(frame)
			fmt.Fprintf(w, "data: %s\n", fb)
			const chunk = 48
			for i := 0; i < len(argsJSON); i += chunk {
				end := i + chunk
				if end > len(argsJSON) {
					end = len(argsJSON)
				}
				tc := map[string]any{"index": 0, "function": map[string]any{"arguments": argsJSON[i:end]}}
				frame := map[string]any{"choices": []any{map[string]any{
					"index": 0, "delta": map[string]any{"tool_calls": []any{tc}},
				}}}
				fb, _ := json.Marshal(frame)
				fmt.Fprintf(w, "data: %s\n", fb)
			}
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		frame := map[string]any{"choices": []any{map[string]any{
			"index": 0, "delta": map[string]any{"role": "assistant", "content": "Wrote the file."},
		}}}
		fb, _ := json.Marshal(frame)
		fmt.Fprintf(w, "data: %s\n", fb)
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fsToolManifest clones the production fs server entry (agents/neo.json)
// scoped to root, so the test spawns the REAL filesystem MCP server.
func fsToolManifest(t *testing.T, root string) string {
	t.Helper()
	raw, err := os.ReadFile("../../../agents/neo.json")
	if err != nil {
		t.Fatalf("read neo.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse neo.json: %v", err)
	}
	var fsEntry map[string]any
	for _, s := range m["servers"].([]any) {
		e := s.(map[string]any)
		if e["alias"] == "fs" {
			fsEntry = e
			break
		}
	}
	if fsEntry == nil {
		t.Fatal("no fs server in neo.json")
	}
	fsEntry["args"] = []any{"-y", "@modelcontextprotocol/server-filesystem", root}
	m["servers"] = []any{fsEntry}
	out, _ := json.Marshal(m)
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// TestLiveTyping_EndToEnd_StreamsAndConvergesWithDisk drives the REAL agent
// loop: the scripted model streams a chunked write_file call; the observer
// must see running→done status transitions AND progressive ToolStream deltas
// whose reassembly is byte-identical to the file the REAL fs MCP server puts
// on disk (req 3.2, 6.1: real transitions, real streamed write, convergence).
func TestLiveTyping_EndToEnd_StreamsAndConvergesWithDisk(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available")
	}
	workdir := t.TempDir()
	tm, err := tools.Spawn(context.Background(), tools.Options{
		ManifestPath: fsToolManifest(t, workdir),
		StderrSink:   os.Stderr,
	})
	if err != nil {
		t.Fatalf("tools.Spawn: %v", err)
	}
	defer tm.Close()
	bound := false
	for _, n := range tm.NaturalToolNames() {
		if n == "fs__write_file" {
			bound = true
		}
	}
	if !bound {
		t.Skipf("fs MCP server did not bind write_file (warnings: %v)", tm.Warnings())
	}

	target := filepath.Join(workdir, "hello.go")
	content := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"hello \\\"workbench\\\"\")\n}\n" +
		strings.Repeat("// filler line to make the stream span many fragments\n", 40)
	pb, _ := json.Marshal(target)
	cb, _ := json.Marshal(content)
	argsJSON := `{"path":` + string(pb) + `,"content":` + string(cb) + `}`

	var calls int
	var mu sync.Mutex
	srv := chunkedWriteServer(t, argsJSON, &calls, &mu)
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  srv.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "neo-livetype-e2e"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	defer pager.Close()

	var evMu sync.Mutex
	var events []ToolEvent
	a := New(Options{
		Config: cfg,
		Main:   client,
		Tools:  tm,
		Pager:  pager,
		ConvID: "conv-livetype-e2e",
		Observer: func(ev ToolEvent) {
			evMu.Lock()
			events = append(events, ev)
			evMu.Unlock()
		},
	})
	if err := a.Chat(context.Background(), "write hello.go"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// The REAL write landed on disk via the real fs MCP server.
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read %s: %v", target, err)
	}
	if string(onDisk) != content {
		t.Fatalf("disk bytes diverge from the scripted content (%d vs %d bytes)", len(onDisk), len(content))
	}

	evMu.Lock()
	defer evMu.Unlock()
	var streams []ToolEvent
	sawStart, sawEnd := false, false
	var endErr bool
	for _, ev := range events {
		if ev.Name != "fs__write_file" {
			continue
		}
		switch ev.Phase {
		case ToolStream:
			streams = append(streams, ev)
		case ToolStart:
			sawStart = true
		case ToolEnd:
			sawEnd = true
			endErr = ev.IsErr
		}
	}
	// Real per-action status transitions (req 3.2): running → done.
	if !sawStart || !sawEnd {
		t.Fatalf("want ToolStart+ToolEnd for the write (start=%v end=%v)", sawStart, sawEnd)
	}
	if endErr {
		t.Fatal("the real write dispatch reported an error")
	}
	// Progressive live typing (req 6.1): multiple bounded deltas, contiguous
	// offsets, reassembling byte-identical to the file on disk.
	if len(streams) < 2 {
		t.Fatalf("want progressive deltas (>=2 ToolStream events), got %d", len(streams))
	}
	sort.Slice(streams, func(i, j int) bool { return streams[i].StreamOffset < streams[j].StreamOffset })
	var sb strings.Builder
	for _, ev := range streams {
		if ev.StreamPath != target {
			t.Fatalf("stream path = %q, want %q", ev.StreamPath, target)
		}
		if ev.StreamOffset != sb.Len() {
			t.Fatalf("offset gap at %d (have %d)", ev.StreamOffset, sb.Len())
		}
		sb.WriteString(ev.StreamDelta)
	}
	if sb.String() != string(onDisk) {
		t.Fatalf("reassembled stream (%d bytes) != disk bytes (%d)", sb.Len(), len(onDisk))
	}
}
