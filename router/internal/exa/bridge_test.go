// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

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
)

type exaBridge struct {
	stdin  io.WriteCloser
	stdout *bufio.Reader
	nextID int
	t      *testing.T
}

func startExaBridge(t *testing.T, laneURL, token string) *exaBridge {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node is not installed")
	}
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "tools", "exa", "exa.mjs")
	command := exec.Command("node", path)
	command.Env = append(os.Environ(), "MATRIX_EXA_URL="+laneURL, "MATRIX_EXA_TOKEN="+token, "MATRIX_USER_ID=user-one")
	stdin, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = stdin.Close(); _ = command.Process.Kill(); _, _ = command.Process.Wait() })
	return &exaBridge{stdin: stdin, stdout: bufio.NewReader(stdout), t: t}
}

func (b *exaBridge) call(name string, args map[string]any) map[string]any {
	b.t.Helper()
	b.nextID++
	request, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": b.nextID, "method": "tools/call", "params": map[string]any{"name": name, "arguments": args}})
	if _, err := b.stdin.Write(append(request, '\n')); err != nil {
		b.t.Fatal(err)
	}
	done := make(chan string, 1)
	go func() { line, _ := b.stdout.ReadString('\n'); done <- line }()
	var line string
	select {
	case line = <-done:
	case <-time.After(10 * time.Second):
		b.t.Fatal("exa bridge timed out")
	}
	var response struct {
		Result struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(line), &response); err != nil {
		b.t.Fatalf("frame %q: %v", line, err)
	}
	if len(response.Result.Content) != 1 {
		b.t.Fatalf("frame=%s", line)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(response.Result.Content[0].Text), &payload); err != nil {
		b.t.Fatal(err)
	}
	return payload
}

func TestGeneralExaBridgeUsesRouterAndPreservesGroundingStatuses(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/search":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if _, ok := body["contents"].(map[string]any); !ok {
				t.Fatalf("search contents not nested: %#v", body)
			}
			_, _ = w.Write([]byte(`{"requestId":"search-1","searchType":"auto","results":[{"title":"Primary","url":"https://www.sec.gov/x","highlights":["Extractive fact"]}],"output":{"content":{"answer":"fact"},"grounding":[{"field":"answer","confidence":"high","citations":[{"url":"https://www.sec.gov/x","title":"Primary"}]}]},"costDollars":{"total":0.01}}`))
		case "/contents":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["highlights"] == nil || body["contents"] != nil {
				t.Fatalf("contents placement: %#v", body)
			}
			_, _ = w.Write([]byte(`{"requestId":"contents-1","results":[{"title":"Primary","url":"https://www.sec.gov/x","highlights":["Extractive fact"]}],"statuses":[{"id":"https://www.sec.gov/x","status":"success"},{"id":"https://example.com/no","status":"error","error":{"tag":"CRAWL_TIMEOUT"}}],"costDollars":{"total":0.003}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	service := NewService(ServiceConfig{Client: NewClient(ClientConfig{APIKey: "key", BaseURL: upstream.URL}), Store: NewMemoryStore()})
	handler := NewInternalHandler(service, nil)
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer lane-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		handler.ServeHTTP(w, r)
	}))
	t.Cleanup(router.Close)
	bridge := startExaBridge(t, router.URL+"/internal/exa", "lane-token")

	search := bridge.call("exa_search", map[string]any{"query": "fact"})
	if search["provider"] != "exa" || search["request_id"] != "search-1" || len(search["results"].([]any)) != 1 {
		t.Fatalf("search=%+v", search)
	}
	contents := bridge.call("exa_contents", map[string]any{"urls": []string{"https://www.sec.gov/x", "https://example.com/no"}})
	if contents["partial"] != true || len(contents["statuses"].([]any)) != 2 {
		t.Fatalf("contents=%+v", contents)
	}
}

func TestExaBridgeResearchLifecycleReturnsTerminalGrounding(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent/runs":
			_, _ = w.Write([]byte(`{"id":"agent_run_1","status":"queued"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/agent/runs/agent_run_1":
			_, _ = w.Write([]byte(`{"id":"agent_run_1","status":"completed","output":{"text":"Answer","grounding":[{"field":"text","confidence":"high","citations":[{"url":"https://example.com/source","title":"Source"}]}]},"costDollars":{"total":0.1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.Close)
	service := NewService(ServiceConfig{Client: NewClient(ClientConfig{APIKey: "key", BaseURL: upstream.URL}), Store: NewMemoryStore()})
	handler := NewInternalHandler(service, nil)
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r) }))
	t.Cleanup(router.Close)
	bridge := startExaBridge(t, router.URL+"/internal/exa", "lane-token")
	started := bridge.call("exa_research_start", map[string]any{"query": "Research this", "subject": "topic"})
	if run, _ := started["run"].(map[string]any); run["status"] != "queued" {
		t.Fatalf("started=%+v", started)
	}
	completed := bridge.call("exa_research_get", map[string]any{"run_id": "agent_run_1"})
	if completed["answer"] != "Answer" || len(completed["results"].([]any)) != 1 || !strings.Contains(completed["synthesis_note"].(string), "terminal grounding") {
		t.Fatalf("completed=%+v", completed)
	}
}
