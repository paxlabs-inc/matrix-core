// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

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
	"time"

	mcllm "matrix/mcl/llm"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

// NEO-WORKBENCH task 5.2 — Property NEO-WORKBENCH (req 9.1, 9.3).
//
// A REAL Neo run writes a file and runs a command in a REAL project workspace
// through the REAL stack: scripted model boundary (the only stub — it is the
// external LLM) → real agent loop → real tools.Manager → the real fs MCP
// server (@modelcontextprotocol/server-filesystem) + the real exec MCP server
// (tools/exec/exec.mjs) → real engine surfacing → real broker/SSE events →
// real durable trace → real workspace HTTP endpoints. Asserts:
//   - artifact-card rows transition through REAL statuses (tool.step
//     running→done for the write AND the command),
//   - the live-typed tool.delta stream converges BYTE-IDENTICAL with the
//     file the fs server put on disk,
//   - the daemon diff endpoint reflects the change (real git diff),
//   - reopen (GET /conversations/{id}/trace) rebuilds final statuses with NO
//     ephemeral deltas.
// The preview half of the property (req 9.2) is proven against a real
// serving target in neo/internal/preview (TestLifecycleAgainstRealServingTarget)
// plus the durable replay in TestPreviewEventsReplayAcrossReopen.

// workbenchManifest declares the REAL fs + exec MCP servers scoped to root.
func workbenchManifest(t *testing.T, root, stateDir string) string {
	t.Helper()
	raw, err := os.ReadFile("../../../agents/neo.json")
	if err != nil {
		t.Fatalf("read neo.json: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse neo.json: %v", err)
	}
	var keep []any
	for _, s := range m["servers"].([]any) {
		e := s.(map[string]any)
		switch e["alias"] {
		case "fs":
			e["args"] = []any{"-y", "@modelcontextprotocol/server-filesystem", root}
			keep = append(keep, e)
		case "exec":
			e["env"] = []any{"MATRIX_EXEC_STATE_DIR=" + stateDir, "MATRIX_DATA_DIR=" + stateDir}
			keep = append(keep, e)
		}
	}
	m["servers"] = keep
	out, _ := json.Marshal(m)
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, out, 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// workbenchModelServer scripts the model: (1) a CHUNKED fs__write_file call
// (real incremental generation → live typing), (2) an exec__shell command in
// the project, (3) a bare final answer.
func workbenchModelServer(t *testing.T, argsWrite, argsShell string, calls *int, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	frame := func(w http.ResponseWriter, delta map[string]any, finish string) {
		payload := map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta}}}
		if finish != "" {
			payload = map[string]any{"choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}}
		}
		b, _ := json.Marshal(payload)
		fmt.Fprintf(w, "data: %s\n", b)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		idx := *calls
		*calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		switch idx {
		case 0:
			frame(w, map[string]any{"role": "assistant", "content": "Writing the page now."}, "")
			head := map[string]any{"index": 0, "id": "call_write", "type": "function",
				"function": map[string]any{"name": "fs__write_file", "arguments": ""}}
			frame(w, map[string]any{"tool_calls": []any{head}}, "")
			const chunk = 48
			for i := 0; i < len(argsWrite); i += chunk {
				end := i + chunk
				if end > len(argsWrite) {
					end = len(argsWrite)
				}
				tc := map[string]any{"index": 0, "function": map[string]any{"arguments": argsWrite[i:end]}}
				frame(w, map[string]any{"tool_calls": []any{tc}}, "")
			}
			frame(w, map[string]any{}, "tool_calls")
			fmt.Fprint(w, "data: [DONE]\n")
		case 1:
			tc := map[string]any{"index": 0, "id": "call_shell", "type": "function",
				"function": map[string]any{"name": "exec__shell", "arguments": argsShell}}
			frame(w, map[string]any{"role": "assistant", "tool_calls": []any{tc}}, "")
			frame(w, map[string]any{}, "tool_calls")
			fmt.Fprint(w, "data: [DONE]\n")
		default:
			frame(w, map[string]any{"role": "assistant", "content": "The page is written and verified."}, "stop")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestWorkbenchEndToEnd_RealRunRealWorkspaceRealTrace(t *testing.T) {
	if _, err := exec.LookPath("npx"); err != nil {
		t.Skip("npx not available")
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}

	// A REAL project: a git repo under the workspace root.
	wsRoot := t.TempDir()
	proj := filepath.Join(wsRoot, "app")
	if err := os.MkdirAll(proj, 0o755); err != nil {
		t.Fatal(err)
	}
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = proj
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(proj, "README.md"), []byte("# app\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "seed")

	// The content Neo will type + the command it will run. The src/ dir
	// pre-exists (the fs server's write_file does not create parents).
	if err := os.MkdirAll(filepath.Join(proj, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(proj, "src", "index.html")
	content := "<!doctype html>\n<h1>workbench e2e</h1>\n" +
		strings.Repeat("<p>real streamed line</p>\n", 30)
	pb, _ := json.Marshal(target)
	cb, _ := json.Marshal(content)
	argsWrite := `{"path":` + string(pb) + `,"content":` + string(cb) + `}`
	shellArgs, _ := json.Marshal(map[string]string{
		"command": "wc -c < src/index.html",
		"cwd":     proj,
	})

	var calls int
	var mu sync.Mutex
	model := workbenchModelServer(t, argsWrite, string(shellArgs), &calls, &mu)
	client, err := llm.New(mcllm.Config{
		Model:       "accounts/fireworks/models/gpt-oss-120b",
		Provider:    mcllm.ProviderFireworks,
		ProviderSet: true,
		GatewayURL:  model.URL,
	})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}

	tm, err := tools.Spawn(context.Background(), tools.Options{
		ManifestPath: workbenchManifest(t, wsRoot, t.TempDir()),
		StderrSink:   os.Stderr,
	})
	if err != nil {
		t.Fatalf("tools.Spawn: %v", err)
	}
	defer tm.Close()
	bound := map[string]bool{}
	for _, n := range tm.NaturalToolNames() {
		bound[n] = true
	}
	if !bound["fs__write_file"] || !bound["exec__shell"] {
		t.Skipf("required real tools not bound (have %v, warnings %v)", tm.NaturalToolNames(), tm.Warnings())
	}

	cfg := config.Default()
	cfg.CassandraEnabled = false
	cfg.TaskMaxRespawns = 0
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Main:            client,
		Tools:           tm,
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		WorkspaceDir:    wsRoot,
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	srv := newLiveRunTestServer(t, e)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	// Dispatch the REAL run exactly like POST /chat does.
	const convID = "conv_workbench_e2e"
	sess := e.sessions.get(convID)
	runID, fresh := sess.submit("build the landing page")
	if !fresh {
		t.Fatal("expected a fresh run")
	}
	e.conv.AppendUser(convID, runID, "build the landing page")

	events := collectUntilClosed(t, e, runID, 120*time.Second)
	e.trace.Flush()

	// --- 1. REAL per-action status transitions (the artifact-card source). ---
	type transition struct{ sawRunning, sawDone, ok bool }
	steps := map[string]*transition{} // keyed by step id
	var deltas []Event
	for _, ev := range events {
		switch ev.Type {
		case "tool.step":
			id, _ := ev.Fields["id"].(string)
			tr := steps[id]
			if tr == nil {
				tr = &transition{}
				steps[id] = tr
			}
			if run, _ := ev.Fields["running"].(bool); run {
				tr.sawRunning = true
			} else {
				tr.sawDone = true
				tr.ok, _ = ev.Fields["ok"].(bool)
			}
		case "tool.delta":
			deltas = append(deltas, ev)
		}
	}
	wtr := steps["call_write"]
	if wtr == nil || !wtr.sawRunning || !wtr.sawDone || !wtr.ok {
		t.Fatalf("write step transitions = %+v, want running→done ok", wtr)
	}
	str := steps["call_shell"]
	if str == nil || !str.sawRunning || !str.sawDone || !str.ok {
		t.Fatalf("shell step transitions = %+v, want running→done ok", str)
	}

	// --- 2. The REAL file landed on disk and the live-typed stream converges
	//        byte-identical with it. ---
	onDisk, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("the real fs server did not write %s: %v", target, err)
	}
	if string(onDisk) != content {
		t.Fatalf("disk bytes diverge from the scripted content")
	}
	if len(deltas) < 2 {
		t.Fatalf("want progressive live-typing deltas, got %d", len(deltas))
	}
	// Broker events carry native Go values (int offsets), unlike the JSON
	// round-trip a browser sees — coerce both.
	asInt := func(v interface{}) int {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
		return -1
	}
	sort.Slice(deltas, func(i, j int) bool {
		return asInt(deltas[i].Fields["offset"]) < asInt(deltas[j].Fields["offset"])
	})
	var typed strings.Builder
	for _, ev := range deltas {
		if p, _ := ev.Fields["path"].(string); p != target {
			t.Fatalf("delta path = %q, want %q", p, target)
		}
		if off := asInt(ev.Fields["offset"]); off != typed.Len() {
			t.Fatalf("delta offset gap at %d (have %d)", off, typed.Len())
		}
		d, _ := ev.Fields["delta"].(string)
		typed.WriteString(d)
	}
	if typed.String() != string(onDisk) {
		t.Fatalf("live-typed stream (%d bytes) != disk bytes (%d)", typed.Len(), len(onDisk))
	}

	// --- 3. The daemon diff endpoint reflects the change (real git). ---
	hc := &http.Client{Timeout: 10 * time.Second}
	diffBody := mustGetJSON(t, hc, ts.URL+"/workspace/diff?project=app")
	if diffBody["git"] != true {
		t.Fatalf("diff body = %v", diffBody)
	}
	ub, _ := json.Marshal(diffBody["untracked"])
	if !strings.Contains(string(ub), "src/index.html") {
		t.Fatalf("diff endpoint does not reflect the new file: %s", ub)
	}

	// --- 4. Reopen rebuilds cards from the durable trace: final statuses,
	//        both actions, NO ephemeral deltas. ---
	traceBody := mustGetJSON(t, hc, ts.URL+"/conversations/"+convID+"/trace")
	rawEvents, _ := traceBody["events"].([]interface{})
	if len(rawEvents) == 0 {
		t.Fatal("empty durable trace on reopen")
	}
	final := map[string]bool{} // id → running (last seen)
	sawDelta := false
	sawEditor, sawTerminal := false, false
	for _, re := range rawEvents {
		ev := re.(map[string]interface{})
		switch ev["type"] {
		case "tool.delta":
			sawDelta = true
		case "tool.step":
			f, _ := ev["fields"].(map[string]interface{})
			id, _ := f["id"].(string)
			run, _ := f["running"].(bool)
			final[id] = run
			switch f["surface"] {
			case "editor":
				sawEditor = true
			case "terminal":
				sawTerminal = true
			}
		}
	}
	if sawDelta {
		t.Fatal("ephemeral tool.delta leaked into the durable trace")
	}
	if !sawEditor || !sawTerminal {
		t.Fatalf("reopen missing surfaces (editor=%v terminal=%v)", sawEditor, sawTerminal)
	}
	if final["call_write"] || final["call_shell"] {
		t.Fatal("reopen must land on FINAL (not running) statuses")
	}

	// --- 5. The editable-editor read now serves the Neo-written file with a
	//        version hash (the buffer the user would edit next). ---
	fileBody := mustGetJSON(t, hc, ts.URL+"/workspace/file?project=app&path=src/index.html")
	if fileBody["content"] != content || fileBody["hash"] == "" {
		t.Fatal("workspace read does not serve the written file + hash")
	}
}
