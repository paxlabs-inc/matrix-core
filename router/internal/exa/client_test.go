// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestSearchUsesNestedContentsAndGrounding(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Fatalf("authorization = %q", got)
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		contents, ok := body["contents"].(map[string]any)
		if !ok || contents["highlights"] != true {
			t.Fatalf("contents = %#v", body["contents"])
		}
		if _, leaked := body["highlights"]; leaked {
			t.Fatal("search highlights must not be top-level")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"requestId":"req-1","searchType":"auto","results":[{"title":"Filing","url":"https://www.sec.gov/a?utm_source=x","highlights":["Revenue was 10."]}],"output":{"content":{"answer":"10"},"grounding":[{"field":"answer","confidence":"high","citations":[{"url":"https://www.sec.gov/a?utm_source=x","title":"Filing"}]}]},"costDollars":{"total":0.007}}`))
	}))
	defer server.Close()

	client := NewClient(ClientConfig{APIKey: "secret", BaseURL: server.URL})
	out, err := client.Search(context.Background(), SearchRequest{Query: "revenue"})
	if err != nil {
		t.Fatal(err)
	}
	if out.RequestID != "req-1" || len(out.Output.Grounding) != 1 {
		t.Fatalf("response = %#v", out)
	}
	evidence := EvidenceFromGrounding(out.Output.Grounding, time.Unix(10, 0))
	if len(evidence) != 1 || evidence[0].URL != "https://www.sec.gov/a" || !evidence[0].Primary {
		t.Fatalf("evidence = %#v", evidence)
	}
}

func TestContentsKeepsPartialResultAndReportsEveryStatus(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["highlights"] != true {
			t.Fatalf("top-level highlights = %#v", body["highlights"])
		}
		if _, nested := body["contents"]; nested {
			t.Fatal("contents extraction options must be top-level")
		}
		_, _ = w.Write([]byte(`{"requestId":"req-2","results":[{"title":"Good","url":"https://example.com/good","highlights":["Evidence"]}],"statuses":[{"id":"https://example.com/good","status":"success"},{"id":"https://example.com/bad","status":"error","error":{"tag":"CRAWL_TIMEOUT","httpStatusCode":504}}],"costDollars":{"total":0.003}}`))
	}))
	defer server.Close()

	client := NewClient(ClientConfig{APIKey: "secret", BaseURL: server.URL})
	out, err := client.Contents(context.Background(), ContentsRequest{URLs: []string{"https://example.com/good", "https://example.com/bad"}})
	if KindOf(err) != FailurePartial || out == nil || len(out.Results) != 1 || len(out.Statuses) != 2 {
		t.Fatalf("out=%#v err=%v", out, err)
	}
}

func TestAgentLifecycleRequiresTerminalGrounding(t *testing.T) {
	var paths []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/agent/runs":
			_, _ = w.Write([]byte(`{"id":"agent_run_1","status":"queued"}`))
		case "/agent/runs/agent_run_1":
			_, _ = w.Write([]byte(`{"id":"agent_run_1","status":"completed","output":{"structured":{"answer":"x"},"grounding":[]}}`))
		case "/agent/runs/agent_run_1/cancel":
			_, _ = w.Write([]byte(`{"id":"agent_run_1","status":"cancelled"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := NewClient(ClientConfig{APIKey: "secret", BaseURL: server.URL})
	run, err := client.CreateRun(context.Background(), AgentRequest{Query: "research", Effort: "medium"})
	if err != nil || run.ID != "agent_run_1" {
		t.Fatalf("create=%#v err=%v", run, err)
	}
	run, err = client.GetRun(context.Background(), run.ID)
	if KindOf(err) != FailureUngrounded || run == nil {
		t.Fatalf("get=%#v err=%v", run, err)
	}
	if _, err = client.CancelRun(context.Background(), run.ID); err != nil {
		t.Fatal(err)
	}
	want := []string{"POST /agent/runs", "GET /agent/runs/agent_run_1", "POST /agent/runs/agent_run_1/cancel"}
	if !reflect.DeepEqual(paths, want) {
		t.Fatalf("paths=%v want=%v", paths, want)
	}
}

func TestTypedFailuresAndNoKeyLeak(t *testing.T) {
	client := NewClient(ClientConfig{})
	if _, err := client.Search(context.Background(), SearchRequest{Query: "x"}); KindOf(err) != FailureNotConfigured {
		t.Fatalf("missing key kind = %q (%v)", KindOf(err), err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "3")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":"slow down"}`))
	}))
	defer server.Close()
	client = NewClient(ClientConfig{APIKey: "super-secret", BaseURL: server.URL})
	_, err := client.Search(context.Background(), SearchRequest{Query: "x"})
	failure := FailureOf(err)
	if failure == nil || failure.Kind != FailureRateLimited || failure.RetryAfter != 3*time.Second {
		t.Fatalf("failure = %#v", failure)
	}
	if failure != nil && (failure.Detail == "super-secret" || failure.Error() == "super-secret") {
		t.Fatal("key leaked")
	}
}
