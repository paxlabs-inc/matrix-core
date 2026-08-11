// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package exa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestServiceCollapsesAndCachesIdenticalSearch(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		time.Sleep(20 * time.Millisecond)
		_, _ = w.Write([]byte(`{"requestId":"one","searchType":"auto","results":[{"title":"Primary","url":"https://www.sec.gov/filing","highlights":["Fact"]}],"costDollars":{"total":0.005}}`))
	}))
	defer upstream.Close()
	now := time.Date(2026, 7, 31, 12, 0, 0, 0, time.UTC)
	service := NewService(ServiceConfig{Client: NewClient(ClientConfig{APIKey: "key", BaseURL: upstream.URL}), Store: NewMemoryStore(), Now: func() time.Time { return now }})

	const workers = 8
	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			out, err := service.Search(context.Background(), "user-1", SearchRequest{Query: "fact"})
			if err == nil && (out.Data == nil || out.Data.RequestID != "one") {
				err = &Failure{Kind: FailureUpstream, Message: "unexpected response"}
			}
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("upstream calls=%d want=1", got)
	}
	out, err := service.Search(context.Background(), "user-1", SearchRequest{Query: "fact"})
	if err != nil || !out.Meta.CacheHit {
		t.Fatalf("cached=%#v err=%v", out, err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("cached upstream calls=%d", got)
	}
	stats := service.Stats()
	if stats.Requests != workers+1 || stats.CacheHits == 0 {
		t.Fatalf("stats=%#v", stats)
	}
}

func TestServiceDailySpendAndPartialContents(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(`{"requestId":"partial","results":[{"url":"https://example.com/ok","highlights":["ok"]}],"statuses":[{"id":"https://example.com/ok","status":"success"},{"id":"https://example.com/no","status":"error","error":{"tag":"CRAWL_TIMEOUT"}}],"costDollars":{"total":0.003}}`))
	}))
	defer upstream.Close()
	client := NewClient(ClientConfig{APIKey: "key", BaseURL: upstream.URL})
	limited := NewService(ServiceConfig{Client: client, Store: NewMemoryStore(), DailySpendLimit: .01})
	if _, err := limited.Search(context.Background(), "user", SearchRequest{Query: "blocked"}); KindOf(err) != FailureRateLimited {
		t.Fatalf("spend kind=%q err=%v", KindOf(err), err)
	}
	if calls.Load() != 0 {
		t.Fatal("spend refusal reached upstream")
	}

	service := NewService(ServiceConfig{Client: client, Store: NewMemoryStore()})
	out, err := service.Contents(context.Background(), "user", ContentsRequest{URLs: []string{"https://example.com/ok", "https://example.com/no"}})
	if err != nil || out.Error == nil || out.Error.Kind != FailurePartial || len(out.Data.Statuses) != 2 {
		t.Fatalf("partial=%#v err=%v", out, err)
	}
}

func TestInternalHandlersEnforceRunOwnershipAndFinanceWorkflow(t *testing.T) {
	var mu sync.Mutex
	status := "queued"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/agent/runs":
			var body AgentRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body.Effort != "medium" || body.OutputSchema == nil {
				t.Fatalf("agent body=%#v", body)
			}
			_, _ = w.Write([]byte(`{"id":"agent_run_finance","status":"queued"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/agent/runs/agent_run_finance":
			status = "completed"
			_, _ = w.Write([]byte(`{"id":"agent_run_finance","status":"completed","output":{"structured":{"ticker":"AAPL"},"grounding":[{"field":"ticker","confidence":"high","citations":[{"url":"https://www.sec.gov/aapl","title":"Filing"}]}]},"costDollars":{"total":0.1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()
	service := NewService(ServiceConfig{Client: NewClient(ClientConfig{APIKey: "key", BaseURL: upstream.URL}), Store: NewMemoryStore()})
	handler := NewFinanceHandler(service, true, nil, nil)

	startBody := `{"kind":"equity_brief","symbol":"aapl"}`
	request := httptest.NewRequest(http.MethodPost, "/internal/finance/research/start", strings.NewReader(startBody))
	request.Header.Set(SubjectHeader, "user-a")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("start status=%d body=%s", recorder.Code, recorder.Body.String())
	}

	other := httptest.NewRequest(http.MethodGet, "/internal/finance/research/agent_run_finance", http.NoBody)
	other.Header.Set(SubjectHeader, "user-b")
	otherRecorder := httptest.NewRecorder()
	handler.ServeHTTP(otherRecorder, other)
	if otherRecorder.Code != http.StatusNotFound {
		t.Fatalf("cross-user status=%d body=%s", otherRecorder.Code, otherRecorder.Body.String())
	}

	get := httptest.NewRequest(http.MethodGet, "/internal/finance/research/agent_run_finance", http.NoBody)
	get.Header.Set(SubjectHeader, "user-a")
	getRecorder := httptest.NewRecorder()
	handler.ServeHTTP(getRecorder, get)
	if getRecorder.Code != http.StatusOK || !strings.Contains(getRecorder.Body.String(), `"status":"completed"`) {
		t.Fatalf("get status=%d body=%s", getRecorder.Code, getRecorder.Body.String())
	}
	if status != "completed" {
		t.Fatalf("upstream status=%s", status)
	}
}

func TestFinanceNewsExtractionDeduplicatesCanonicalURLsAndPreservesPartialStatuses(t *testing.T) {
	var received ContentsRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/contents" {
			http.NotFound(w, r)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&received); err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"requestId":"news-1","results":[{"title":"Issuer update","url":"https://example.com/story","highlights":["Material fact"]}],"statuses":[{"id":"https://example.com/story","status":"success"},{"id":"https://example.com/failed","status":"error","error":{"tag":"CRAWL_TIMEOUT"}}],"costDollars":{"total":0.004}}`))
	}))
	t.Cleanup(upstream.Close)
	service := NewService(ServiceConfig{Client: NewClient(ClientConfig{APIKey: "key", BaseURL: upstream.URL}), Store: NewMemoryStore()})
	out, err := service.ExtractFinanceNews(context.Background(), "user-one", FinanceNewsRequest{
		Symbol: "aapl",
		URLs: []string{
			"https://example.com/story?utm_source=feed",
			"https://example.com/story",
			"https://example.com/failed",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(received.URLs) != 2 || received.URLs[0] != "https://example.com/story" {
		t.Fatalf("deduplicated URLs = %#v", received.URLs)
	}
	if out.Error == nil || out.Error.Kind != FailurePartial || len(out.Data.Statuses) != 2 {
		t.Fatalf("partial news evidence = %#v", out)
	}
	if _, ok := received.Highlights.(map[string]any); !ok {
		t.Fatalf("highlights must be a top-level Contents option: %#v", received.Highlights)
	}
}
