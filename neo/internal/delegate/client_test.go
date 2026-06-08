// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package delegate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func fastClient(base string, ap Approver) *Client {
	return New(Options{
		BaseURL:      base,
		Approver:     ap,
		PollInterval: time.Millisecond,
		MaxWait:      5 * time.Second,
		Timeout:      2 * time.Second,
	})
}

func TestRunHappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "i1"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "completed",
				"result": map[string]string{"answer": "all done"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := fastClient(srv.URL, nil).Run(context.Background(), "do x")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "all done" {
		t.Errorf("answer = %q, want 'all done'", out)
	}
}

func TestRunServicesApprovalGateInline(t *testing.T) {
	var mu sync.Mutex
	answered := false
	var askedQuestion string
	var askedOptions []string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		done := answered
		mu.Unlock()
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "i2"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []map[string]any{
				{"node_id": "n1", "question": "Approve spend of 5 PAX?", "options": []string{"yes", "no"}},
			}})
		case r.Method == http.MethodPost && strings.Contains(r.URL.Path, "/gates/") && strings.HasSuffix(r.URL.Path, "/answer"):
			mu.Lock()
			answered = true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			if done {
				_ = json.NewEncoder(w).Encode(map[string]any{"status": "completed", "result": map[string]string{"answer": "settled"}})
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "working"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	approver := func(ctx context.Context, nodeID, q string, opts []string) (bool, string) {
		mu.Lock()
		askedQuestion = q
		askedOptions = opts
		mu.Unlock()
		return true, ""
	}

	out, err := fastClient(srv.URL, approver).Run(context.Background(), "send 5 PAX")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if out != "settled" {
		t.Errorf("answer = %q, want 'settled'", out)
	}
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(askedQuestion, "Approve spend") {
		t.Errorf("approver was not asked the gate question, got %q", askedQuestion)
	}
	if len(askedOptions) != 2 {
		t.Errorf("approver should receive the gate options, got %v", askedOptions)
	}
}

func TestRunPropagatesPipelineFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/messages/async":
			_ = json.NewEncoder(w).Encode(map[string]string{"intent_id": "i3"})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/gates"):
			_ = json.NewEncoder(w).Encode(map[string]any{"pending": []any{}})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/messages/async/"):
			_ = json.NewEncoder(w).Encode(map[string]any{"status": "failed", "error": "compile blew up"})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	_, err := fastClient(srv.URL, nil).Run(context.Background(), "do x")
	if err == nil || !strings.Contains(err.Error(), "compile blew up") {
		t.Errorf("expected the pipeline error to propagate, got %v", err)
	}
}

func TestClarifyText(t *testing.T) {
	if got := clarifyText(json.RawMessage(`{"question":"which token?"}`)); got != "which token?" {
		t.Errorf("clarifyText = %q", got)
	}
	if got := clarifyText(json.RawMessage(`{"other":"x"}`)); got == "" {
		t.Error("clarifyText should fall back to the raw text")
	}
}

func TestTruncate(t *testing.T) {
	if truncate("short", 10) != "short" {
		t.Error("under-limit unchanged")
	}
	if got := truncate("abcdefghij", 4); !strings.HasSuffix(got, "…") {
		t.Errorf("truncate should add an ellipsis: %q", got)
	}
}
