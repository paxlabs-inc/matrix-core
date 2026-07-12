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
	"sync"
	"testing"

	"matrix/codegraph/selfmodel"
	cxself "matrix/cortex/self"
	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/tools"
)

func replayCapability(t *testing.T) *CapabilitySurface {
	t.Helper()
	graph := t.TempDir()
	artifact := selfmodel.Artifact{
		Summary: "Neo is the conversational agent served by the Matrix daemon.",
		Merkle:  "arena-replay-artifact",
		Scope:   []string{"matrix/neo"},
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal self-model artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graph, "self-model.json"), append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write self-model artifact: %v", err)
	}
	cfg := config.Default()
	cfg.CortexRoot = t.TempDir()
	cfg.CortexActor = "epistemic-arena-replay"
	cfg.SelfModelGraph = graph
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("open real cortex pager: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	facts := cxself.SurfaceFacts{
		API: []string{
			"POST /chat — send a user message; the reply streams over SSE GET /events",
			"GET /events — SSE event stream carrying replies and tool steps",
		},
		Is: []string{
			"You are the conversational agent behind the Matrix daemon HTTP front; clients send POST /chat and read SSE GET /events.",
		},
		IsNot: []string{
			"You have NO OpenAI-compatible endpoint: /v1/chat/completions, /v1/completions, and /v1/models are NOT served.",
		},
	}
	if _, err := pager.WriteSurfaceFacts(context.Background(), facts); err != nil {
		t.Fatalf("persist real surface facts: %v", err)
	}
	model, err := pager.SelfModel(context.Background())
	if err != nil {
		t.Fatalf("resolve real self-model: %v", err)
	}
	if model.Structural.Surface == nil {
		t.Fatal("real self-model lost its surface facet")
	}
	return &CapabilitySurface{
		API:               append([]string(nil), model.Structural.Surface.API...),
		Is:                append([]string(nil), model.Structural.Surface.Is...),
		IsNot:             append([]string(nil), model.Structural.Surface.IsNot...),
		StructuralSummary: model.Structural.Summary,
	}
}

func TestPropertyArenaReplay(t *testing.T) {
	const scaffold = `{"index":0,"id":"scaffold","type":"function","function":{"name":"write_file","arguments":"{\"path\":\"bridge.py\",\"content\":\"wired through the real chat surface\"}"}}`
	var (
		mu          sync.Mutex
		bodies      []string
		mainCalls   int
		dispatched  int
		badDispatch bool
		a           *Agent
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(body, "load-bearing FACTUAL PREMISES") {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"[]"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		mu.Lock()
		bodies = append(bodies, body)
		mainCalls++
		call := mainCalls
		mu.Unlock()
		switch call {
		case 1:
			plan := "Plan: I expose an API at POST /chat. I can wire the other agent directly through my own OpenAI-compatible API. I will scaffold that bridge now."
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q,\"tool_calls\":[%s]}}]}\n", plan, scaffold)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		case 2:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Revised plan: the OpenAI-compatible premise is false. The integration must use POST /chat and SSE GET /events."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		case 3:
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", scaffold)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"done"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(srv.Close)
	client, err := llm.New(mcllm.Config{Model: "accounts/fireworks/models/gpt-oss-120b", Provider: mcllm.ProviderFireworks, ProviderSet: true, GatewayURL: srv.URL})
	if err != nil {
		t.Fatalf("llm.New: %v", err)
	}
	cfg := config.Default()
	cfg.CassandraEnabled = false
	a = New(Options{
		Config:     cfg,
		Main:       client,
		Cheap:      client,
		Tools:      &tools.Manager{},
		Capability: replayCapability(t),
		Observer: func(ev ToolEvent) {
			if ev.Phase != ToolStart {
				return
			}
			dispatched++
			if a.ledger != nil && (len(a.ledger.ungroundedSelf()) != 0 || len(a.ledger.unrevisedRefuted()) != 0) {
				badDispatch = true
			}
		},
	})
	if err := a.Chat(context.Background(), "bridge Neo with the arena agent"); err != nil {
		t.Fatalf("arena replay: %v", err)
	}
	if dispatched != 1 {
		t.Fatalf("only the revised scaffold may dispatch; got %d dispatches", dispatched)
	}
	if badDispatch {
		t.Fatal("a scaffold dispatched while a cheaply-checkable self premise was unchecked or unrevised")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) < 3 {
		t.Fatalf("arena replay did not force revision before dispatch; model calls=%d", len(bodies))
	}
	if !strings.Contains(bodies[1], "cited: self-model") || !strings.Contains(bodies[1], "REFUTED") {
		t.Fatal("the resident self-model must cite the true /chat premise and refute the OpenAI premise before scaffold dispatch")
	}
	if strings.Contains(bodies[1], `"tools"`) {
		t.Fatal("the arena revision step must be tools-stripped")
	}
}

func replayFetchManager(t *testing.T) *tools.Manager {
	t.Helper()
	manifest := filepath.Join(t.TempDir(), "agent.json")
	doc := `{
  "schema_version": 1,
  "agent": "matrix://agent/epistemic-replay",
  "description": "epistemic replay",
  "allowed_side_effects": ["network"],
  "servers": [{
    "alias": "fetch",
    "transport": "stdio",
    "command": "uvx",
    "args": ["--offline", "mcp-server-fetch==2025.4.7"],
    "env": [],
    "package_digest": "sha256:1111111111111111111111111111111111111111111111111111111111111111",
    "version": "2025.4.7",
    "tools": [{"name":"fetch","description":"Fetch a URL and return its content","side_effect_class":"network","timeout_ms":30000}]
  }]
}`
	if err := os.WriteFile(manifest, []byte(doc), 0o644); err != nil {
		t.Fatalf("write fetch manifest: %v", err)
	}
	mgr, err := tools.Spawn(context.Background(), tools.Options{ManifestPath: manifest, StderrSink: io.Discard})
	if err != nil {
		t.Fatalf("spawn real fetch tool: %v", err)
	}
	if warnings := mgr.Warnings(); len(warnings) != 0 {
		_ = mgr.Close()
		t.Fatalf("real fetch tool unavailable: %v", warnings)
	}
	t.Cleanup(func() { _ = mgr.Close() })
	return mgr
}

func TestPropertyOHLCReplay(t *testing.T) {
	var (
		apiMu      sync.Mutex
		probePaths []string
	)
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/robots.txt" {
			apiMu.Lock()
			probePaths = append(probePaths, r.URL.Path)
			apiMu.Unlock()
		}
		http.NotFound(w, r)
	}))
	t.Cleanup(api.Close)
	probe := func(id, path string) string {
		args, _ := json.Marshal(map[string]string{"url": api.URL + path, "expect": "HTTP 200 with a JSON array of OHLC candles"})
		return fmt.Sprintf(`{"index":0,"id":%q,"type":"function","function":{"name":"fetch__fetch","arguments":%q}}`, id, string(args))
	}
	var (
		modelMu    sync.Mutex
		bodies     []string
		mainCall   int
		toolStarts int
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body := string(raw)
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(body, "load-bearing FACTUAL PREMISES") {
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"[]"},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
			return
		}
		modelMu.Lock()
		bodies = append(bodies, body)
		mainCall++
		call := mainCall
		modelMu.Unlock()
		emit := func(tc string) {
			fmt.Fprintf(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[%s]}}]}\n", tc)
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
		switch call {
		case 1:
			emit(probe("ohlc", "/ohlc"))
		case 2:
			emit(probe("candles", "/candles"))
		case 3:
			emit(probe("klines", "/klines"))
		case 4:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Three expected-success paths returned 404. Revised plan: stop guessing paths and obtain the API contract."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		default:
			fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"The endpoint is undocumented; I stopped after the bounded probes."},"finish_reason":"stop"}]}`+"\n")
			fmt.Fprint(w, "data: [DONE]\n")
		}
	}))
	t.Cleanup(model.Close)
	client := revisionTestClient(t, model.URL)
	cfg := config.Default()
	cfg.CassandraEnabled = false
	a := New(Options{
		Config: cfg,
		Main:   client,
		Cheap:  client,
		Tools:  replayFetchManager(t),
		Observer: func(ev ToolEvent) {
			if ev.Phase == ToolStart {
				toolStarts++
			}
		},
	})
	if err := a.Chat(context.Background(), "find the current OHLC candles on this undocumented API"); err != nil {
		t.Fatalf("OHLC replay: %v", err)
	}
	if toolStarts != 3 {
		t.Fatalf("OHLC probes must be bounded at three; dispatched=%d", toolStarts)
	}
	apiMu.Lock()
	paths := append([]string(nil), probePaths...)
	apiMu.Unlock()
	if len(paths) != 3 || paths[0] != "/ohlc" || paths[1] != "/candles" || paths[2] != "/klines" {
		t.Fatalf("real API saw unexpected probe sequence: %v", paths)
	}
	modelMu.Lock()
	defer modelMu.Unlock()
	if len(bodies) < 4 {
		t.Fatalf("expected three probes and one revision, got %d model calls", len(bodies))
	}
	for i := 1; i <= 3; i++ {
		if !strings.Contains(bodies[i], `\"expect\"`) {
			t.Fatalf("probe %d did not carry an expectation through the real dispatch transcript", i)
		}
	}
	if !strings.Contains(bodies[3], "3 consecutive prediction mismatches") || !strings.Contains(bodies[3], "REFUTED") {
		t.Fatal("the third mismatch must force revision with the mismatch meter and refuted hypothesis resident")
	}
	if strings.Contains(bodies[3], `"tools"`) {
		t.Fatal("the OHLC revision step must be tools-stripped")
	}
}
