package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
)

// TestStageContracts_RealMultiStepTurn proves the staged loop's contracts on
// a real multi-step turn (MORPHEUS req.3.5, task 3.1): real httptest SSE
// model, a REAL spawned MCP fetch tool, genuine dispatch — and window
// assembly executed exactly ONCE per step through the one site
// (prepareWindow), with the assembled shape holding on every request the
// model received.
func TestStageContracts_RealMultiStepTurn(t *testing.T) {
	// The real endpoint the real fetch tool probes.
	var apiHits int
	var apiMu sync.Mutex
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		apiMu.Lock()
		apiHits++
		apiMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `[{"open":1,"close":2}]`)
	}))
	t.Cleanup(api.Close)

	probeArgs, _ := json.Marshal(map[string]string{"url": api.URL + "/data", "expect": "HTTP 200 with a JSON array"})
	probe := fmt.Sprintf(`{"index":0,"id":"p1","type":"function","function":{"name":"fetch__fetch","arguments":%q}}`, string(probeArgs))

	var (
		mu     sync.Mutex
		bodies []string
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		// Cheap-model comparator (prediction discipline): deterministic match.
		if strings.Contains(body, "stated expectation") {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"match"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		call := len(bodies)
		mu.Unlock()
		if call == 1 {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", probe)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"the data is fetched"},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	client := revisionTestClient(t, model.URL)
	cfg := config.Default()
	cfg.CassandraEnabled = false
	a := New(Options{Config: cfg, Main: client, Cheap: client, Tools: replayFetchManager(t)})

	if err := a.Chat(context.Background(), "fetch the data and report"); err != nil {
		t.Fatalf("Chat: %v", err)
	}

	mu.Lock()
	captured := append([]string(nil), bodies...)
	mu.Unlock()
	if len(captured) != 2 {
		t.Fatalf("the turn must be exactly two steps, got %d model calls", len(captured))
	}
	apiMu.Lock()
	hits := apiHits
	apiMu.Unlock()
	if hits != 1 {
		t.Fatalf("the real tool must have dispatched exactly once, endpoint hits = %d", hits)
	}

	// Window assembly executed exactly ONCE per step (req.3.2): the counter on
	// the one assembly site equals the number of steps.
	if a.windowAssemblies != 2 {
		t.Fatalf("window assembled %d times over 2 steps — assembly must run exactly once per step through the one site", a.windowAssemblies)
	}

	// The assembled shape holds on every request: byte-stable system prefix at
	// index 0, ONE trailing USER-role tail carrying exactly one budget stat.
	var prefix string
	for step, raw := range captured {
		msgs := decodeWireMessages(t, []byte(raw))
		if msgs[0].Role != "system" {
			t.Fatalf("step %d: window[0].role = %q, want system", step, msgs[0].Role)
		}
		if step == 0 {
			prefix = msgs[0].Content
		} else if msgs[0].Content != prefix {
			t.Fatalf("step %d: system prefix not byte-stable across steps", step)
		}
		tail := msgs[len(msgs)-1]
		if tail.Role != "user" {
			t.Fatalf("step %d: trailing message role = %q, want user", step, tail.Role)
		}
		if n := strings.Count(tail.Content, "[context:"); n != 1 {
			t.Fatalf("step %d: tail must carry exactly one budget stat, got %d", step, n)
		}
	}
	// Result assembly reached the transcript: step 2's window carries the
	// real tool result the fetch returned.
	if !strings.Contains(captured[1], "open") {
		t.Fatal("step 2's window must carry the dispatched tool's real result")
	}
}

// TestStageContracts_SingleAssemblySiteAudit pins the structural half of
// req.3.2: assembleWindowUserTail has exactly ONE production call site
// (prepareWindow). A second caller would reintroduce the multi-site window
// drift the reassembly removed.
func TestStageContracts_SingleAssemblySiteAudit(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	callSites := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		for _, line := range strings.Split(string(src), "\n") {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "//") || strings.HasPrefix(trimmed, "func assembleWindowUserTail(") {
				continue
			}
			if strings.Contains(trimmed, "assembleWindowUserTail(") {
				callSites++
			}
		}
	}
	if callSites != 1 {
		t.Fatalf("assembleWindowUserTail must have exactly ONE production call site (prepareWindow), found %d", callSites)
	}
}
