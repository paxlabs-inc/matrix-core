// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package finance

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	grounded "matrix/router/internal/exa"
)

// These drive the REAL tools/finance MCP bridge as a real node subprocess
// against the REAL internal finance handler. Nothing is stubbed: the bridge
// speaks its actual wire protocol, the handler runs its actual service, and the
// upstream is a real HTTP server serving documented vendor bodies.

func bridgePath(t *testing.T) string {
	t.Helper()
	// router/internal/finance -> repo root
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	path := filepath.Join(root, "tools", "finance", "finance.mjs")
	if _, err := os.Stat(path); err != nil {
		t.Skipf("finance bridge not present at %s", path)
	}
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	return path
}

// mcpBridge is a live subprocess speaking newline-delimited JSON-RPC.
type mcpBridge struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout *bufio.Reader
	t      *testing.T
	nextID int
}

func startBridge(t *testing.T, laneURL, token string) *mcpBridge {
	t.Helper()
	path := bridgePath(t)
	cmd := exec.Command("node", path)
	cmd.Env = append(os.Environ(),
		"MATRIX_FINANCE_URL="+laneURL,
		"MATRIX_FINANCE_TOKEN="+token,
		"MATRIX_USER_ID=user-under-test",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bridge: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return &mcpBridge{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), t: t}
}

func startBridgeWithRouterEnv(t *testing.T, routerURL, token string) *mcpBridge {
	t.Helper()
	path := bridgePath(t)
	cmd := exec.Command("node", path)
	cmd.Env = append(os.Environ(),
		"ROUTER_INTERNAL_URL="+routerURL,
		"ROUTER_FINANCE_TOKEN="+token,
		"MATRIX_FINANCE_URL=",
		"MATRIX_FINANCE_TOKEN=",
		"MATRIX_USER_ID=user-under-test",
	)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("stdin: %v", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatalf("stdout: %v", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start bridge: %v", err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return &mcpBridge{cmd: cmd, stdin: stdin, stdout: bufio.NewReader(stdout), t: t}
}

// callTool performs one tools/call and returns the decoded result payload the
// bridge put in its text content — exactly what the model would read.
func (b *mcpBridge) callTool(name string, args map[string]any) map[string]any {
	b.t.Helper()
	b.nextID++
	req := map[string]any{
		"jsonrpc": "2.0", "id": b.nextID, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	}
	line, _ := json.Marshal(req)
	if _, err := b.stdin.Write(append(line, '\n')); err != nil {
		b.t.Fatalf("write: %v", err)
	}

	done := make(chan string, 1)
	go func() {
		out, err := b.stdout.ReadString('\n')
		if err != nil {
			done <- ""
			return
		}
		done <- out
	}()
	var raw string
	select {
	case raw = <-done:
	case <-time.After(30 * time.Second):
		b.t.Fatal("bridge did not answer in time")
	}
	if strings.TrimSpace(raw) == "" {
		b.t.Fatal("bridge closed its stream")
	}

	var envelope struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
			IsError bool `json:"isError"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		b.t.Fatalf("decode bridge frame %q: %v", raw, err)
	}
	if envelope.Error != nil {
		b.t.Fatalf("bridge rpc error: %s", envelope.Error.Message)
	}
	if len(envelope.Result.Content) == 0 {
		b.t.Fatalf("bridge returned no content: %s", raw)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(envelope.Result.Content[0].Text), &payload); err != nil {
		b.t.Fatalf("decode tool payload %q: %v", envelope.Result.Content[0].Text, err)
	}
	return payload
}

// laneServer mounts the REAL internal handler behind a real bearer check, as
// the router's internal listener does.
func laneServer(t *testing.T, svc *Service, token string) *httptest.Server {
	t.Helper()
	h := NewInternalHandler(svc, nil)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+token {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		h.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBridgeReadsMarketDataThroughTheRouterLane(t *testing.T) {
	svc, up, _, _ := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	lane := laneServer(t, svc, "lane-token")
	bridge := startBridge(t, lane.URL+"/internal/finance", "lane-token")

	payload := bridge.callTool("market_quote", map[string]any{"symbol": "aapl"})
	quote, ok := payload["quote"].(map[string]any)
	if !ok {
		t.Fatalf("no quote in payload: %+v", payload)
	}
	if quote["symbol"] != "AAPL" {
		t.Fatalf("symbol = %v", quote["symbol"])
	}
	if quote["price"] != 232.8 {
		t.Fatalf("price = %v, want 232.8", quote["price"])
	}
	if quote["source"] != string(ProviderFMP) {
		t.Fatalf("source = %v, want the answering provider named", quote["source"])
	}
	if _, hasAsOf := quote["as_of"]; !hasAsOf {
		t.Fatalf("no as-of stamp reached the model: %+v", quote)
	}
	if up.Hits() != 1 {
		t.Fatalf("upstream hits = %d, want 1", up.Hits())
	}
}

func TestBridgeReadsRouterLevelFinanceConfiguration(t *testing.T) {
	svc, _, _, _ := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	lane := laneServer(t, svc, "router-token")
	bridge := startBridgeWithRouterEnv(t, lane.URL, "router-token")

	payload := bridge.callTool("market_quote", map[string]any{"symbol": "AAPL"})
	quote, ok := payload["quote"].(map[string]any)
	if !ok || quote["symbol"] != "AAPL" || quote["price"] != 232.8 {
		t.Fatalf("router-level finance wiring failed: %+v", payload)
	}
}

// The whole point of routing the agent through the router: the agent's read and
// the browser's read share one cache entry and one vendor call.
func TestBridgeAndBrowserShareOneCacheEntry(t *testing.T) {
	svc, up, _, _ := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	lane := laneServer(t, svc, "lane-token")
	bridge := startBridge(t, lane.URL+"/internal/finance", "lane-token")

	// The browser reads first, through the public JWT-authed handler.
	public := NewHandler(svc, nil)
	if w := get(t, public, "/finance/quote?symbol=AAPL", "user-under-test"); w.Code != http.StatusOK {
		t.Fatalf("browser read failed: %d %s", w.Code, w.Body.String())
	}
	// Then the agent reads the same symbol through the bridge.
	payload := bridge.callTool("market_quote", map[string]any{"symbol": "AAPL"})
	if _, ok := payload["quote"]; !ok {
		t.Fatalf("bridge read failed: %+v", payload)
	}

	if up.Hits() != 1 {
		t.Fatalf("upstream hits = %d — the agent and the browser did not share the cache", up.Hits())
	}
	stats := svc.Stats()
	if stats.Requests != 2 || stats.CacheHits != 1 {
		t.Fatalf("meter did not record both consumers: %+v", stats)
	}
}

// A model must get a readable summary, not four hundred bars of JSON.
func TestBridgeSeriesIsSummarisedForAModel(t *testing.T) {
	var bars strings.Builder
	bars.WriteString("[")
	for i := 0; i < 300; i++ {
		if i > 0 {
			bars.WriteString(",")
		}
		// Newest-first, as the vendor sends.
		day := 300 - i
		bars.WriteString(`{"symbol":"X","date":"2026-06-05","open":10,"high":12,"low":9,"close":`)
		bars.WriteString(strings.TrimSpace(itoaFloat(float64(day))))
		bars.WriteString(`,"volume":100}`)
	}
	bars.WriteString("]")

	svc, _, _, _ := newTestService(t, map[string]string{"historical-price-eod/full": bars.String()}, nil)
	lane := laneServer(t, svc, "lane-token")
	bridge := startBridge(t, lane.URL+"/internal/finance", "lane-token")

	payload := bridge.callTool("market_series", map[string]any{"symbol": "X", "range": "1Y"})
	series, ok := payload["series"].(map[string]any)
	if !ok {
		t.Fatalf("no series in payload: %+v", payload)
	}
	points, ok := series["points"].([]any)
	if !ok {
		t.Fatalf("no points: %+v", series)
	}
	if len(points) > 45 {
		t.Fatalf("points = %d — the series was dumped, not summarized", len(points))
	}
	if series["bars"] == nil {
		t.Fatal("the bar count was lost")
	}
	for _, key := range []string{"open", "close", "high", "low", "change_percent"} {
		if _, has := series[key]; !has {
			t.Fatalf("series summary is missing %q: %+v", key, series)
		}
	}
}

// A vendor refusal must reach the model as a stated limitation, not as an empty
// structure it will read as "no news exists".
func TestBridgeReportsRefusalsHonestly(t *testing.T) {
	svc, up, _, _ := newTestService(t, map[string]string{"quote": docQuoteAAPL}, nil)
	up.Set("quote", `{"Error Message":"Limit Reached"}`, 0)
	lane := laneServer(t, svc, "lane-token")
	bridge := startBridge(t, lane.URL+"/internal/finance", "lane-token")

	payload := bridge.callTool("market_quote", map[string]any{"symbol": "AAPL"})
	if payload["ok"] != false {
		t.Fatalf("a refused call did not report failure: %+v", payload)
	}
	msg, _ := payload["error"].(string)
	if !strings.Contains(strings.ToLower(msg), "rate limiting") {
		t.Fatalf("error = %q, want the plain-language throttle line", msg)
	}
	if payload["kind"] != string(FailureThrottled) {
		t.Fatalf("kind = %v, want throttled", payload["kind"])
	}
	if _, leaked := payload["quote"]; leaked {
		t.Fatal("a refused call still produced a quote object")
	}
}

// Without the lane wiring the bridge still runs and says what is missing — it
// never falls back to calling a vendor directly.
func TestBridgeWithoutLaneWiringDegradesHonestly(t *testing.T) {
	bridge := startBridge(t, "", "")
	payload := bridge.callTool("market_quote", map[string]any{"symbol": "AAPL"})
	if payload["ok"] != false {
		t.Fatalf("unconfigured bridge did not report failure: %+v", payload)
	}
	msg, _ := payload["error"].(string)
	if !strings.Contains(msg, "not configured") {
		t.Fatalf("error = %q", msg)
	}
}

func TestBridgeRunsGroundedFinanceResearchThroughTheRouterLane(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent/runs":
			_, _ = w.Write([]byte(`{"id":"agent_run_finance","status":"queued"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/agent/runs/agent_run_finance":
			_, _ = w.Write([]byte(`{"id":"agent_run_finance","status":"completed","output":{"structured":{"ticker":"AAPL","key_debates":[],"kpis_to_watch":[]},"text":"Grounded brief","grounding":[{"field":"ticker","confidence":"high","citations":[{"url":"https://www.sec.gov/aapl","title":"Filing"}]}]},"costDollars":{"total":0.1}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/search":
			_, _ = w.Write([]byte(`{"requestId":"verify-1","searchType":"deep","results":[],"output":{"content":{"revenue":{"value":10,"unit":"USD","state":"verified","evidence":[]}},"grounding":[{"field":"revenue","confidence":"high","citations":[{"url":"https://www.sec.gov/aapl","title":"Filing"}]}]},"costDollars":{"total":0.02}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	service := grounded.NewService(grounded.ServiceConfig{
		Client: grounded.NewClient(grounded.ClientConfig{APIKey: "exa-key", BaseURL: upstream.URL}),
		Store:  grounded.NewMemoryStore(),
	})
	handler := grounded.NewFinanceHandler(service, true, nil, nil)
	lane := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer lane-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(lane.Close)
	bridge := startBridge(t, lane.URL+"/internal/finance", "lane-token")

	started := bridge.callTool("market_research_start", map[string]any{"symbol": "AAPL", "kind": "equity_brief"})
	research, ok := started["research"].(map[string]any)
	if !ok || research["id"] != "agent_run_finance" || research["status"] != "queued" {
		t.Fatalf("started=%+v", started)
	}
	completed := bridge.callTool("market_research_get", map[string]any{"run_id": "agent_run_finance"})
	research, ok = completed["research"].(map[string]any)
	output, _ := research["output"].(map[string]any)
	if !ok || research["status"] != "completed" || output["synthesis_note"] == nil {
		t.Fatalf("completed=%+v", completed)
	}
	grounding, _ := output["grounding"].([]any)
	if len(grounding) != 1 {
		t.Fatalf("grounding=%+v", grounding)
	}
	verified := bridge.callTool("market_verify_facts", map[string]any{"symbol": "AAPL", "fields": []string{"revenue"}})
	if verified["verification"] == nil || verified["synthesis_note"] == nil {
		t.Fatalf("verification=%+v", verified)
	}
}

func itoaFloat(v float64) string {
	b, _ := json.Marshal(v)
	return string(b)
}
