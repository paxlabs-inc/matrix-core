// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package notify

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestNtfyRoundTrip drives the REAL ntfy backend against an httptest server and
// asserts the POST path (/${topic}), the Title/Click headers, and the body.
func TestNtfyRoundTrip(t *testing.T) {
	var (
		gotPath  string
		gotTitle string
		gotClick string
		gotBody  string
		gotMeth  string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMeth = r.Method
		gotPath = r.URL.Path
		gotTitle = r.Header.Get("Title")
		gotClick = r.Header.Get("Click")
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{
		NtfyServer: srv.URL,
		NtfyTopic:  "my-topic",
		HTTPClient: srv.Client(),
	})
	if _, ok := n.(*ntfyBackend); !ok {
		t.Fatalf("expected ntfy backend, got %T", n)
	}

	note := Notification{Title: "Done", Body: "Drafted your update", URL: "https://app/conv/1"}
	if err := n.Notify(context.Background(), note); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	if gotMeth != http.MethodPost {
		t.Errorf("method = %q, want POST", gotMeth)
	}
	if gotPath != "/my-topic" {
		t.Errorf("path = %q, want /my-topic", gotPath)
	}
	if gotTitle != "Done" {
		t.Errorf("Title header = %q, want Done", gotTitle)
	}
	if gotClick != "https://app/conv/1" {
		t.Errorf("Click header = %q, want the URL", gotClick)
	}
	if gotBody != "Drafted your update" {
		t.Errorf("body = %q, want the result text", gotBody)
	}
}

// TestNtfyTopicDerivedFromDID verifies the topic is derived from the agent DID
// when not set explicitly, and that the derived topic (not the DID) is on the
// request path.
func TestNtfyTopicDerivedFromDID(t *testing.T) {
	const did = "did:matrix:alice"
	want := DeriveTopic(did)
	if want == "" || !strings.HasPrefix(want, "neo-") {
		t.Fatalf("DeriveTopic(%q) = %q, want non-empty neo- prefixed", did, want)
	}

	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{NtfyServer: srv.URL, AgentDID: did, HTTPClient: srv.Client()})
	if err := n.Notify(context.Background(), Notification{Body: "hi"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotPath != "/"+want {
		t.Errorf("path = %q, want /%s", gotPath, want)
	}
	if strings.Contains(gotPath, did) {
		t.Errorf("path %q leaks the DID", gotPath)
	}
}

// TestDeriveTopicDeterministicAndUnguessable checks determinism (same DID ->
// same topic), distinctness (different DID -> different topic), and that a blank
// DID yields no topic.
func TestDeriveTopicDeterministicAndUnguessable(t *testing.T) {
	a1 := DeriveTopic("did:matrix:alice")
	a2 := DeriveTopic("did:matrix:alice")
	b := DeriveTopic("did:matrix:bob")
	if a1 != a2 {
		t.Errorf("derivation not deterministic: %q != %q", a1, a2)
	}
	if a1 == b {
		t.Errorf("distinct DIDs collided on topic %q", a1)
	}
	if DeriveTopic("") != "" || DeriveTopic("   ") != "" {
		t.Errorf("blank DID must yield an empty topic")
	}
}

// TestNtfyHonestFailure verifies a non-2xx external send surfaces an error
// (honest failure) rather than silently reporting delivered.
func TestNtfyHonestFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	n := New(Config{NtfyServer: srv.URL, NtfyTopic: "t", HTTPClient: srv.Client()})
	if err := n.Notify(context.Background(), Notification{Body: "x"}); err == nil {
		t.Fatalf("expected an error on a 500 response, got nil")
	}
}

// TestAppriseHTTPRoundTrip drives the REAL Apprise backend's HTTP path against
// an httptest server (the apprise binary is NOT required).
func TestAppriseHTTPRoundTrip(t *testing.T) {
	var gotBody, gotTitle, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT = r.Header.Get("Content-Type")
		b, _ := io.ReadAll(r.Body)
		var payload struct {
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		_ = json.Unmarshal(b, &payload)
		gotTitle = payload.Title
		gotBody = payload.Body
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	n := New(Config{AppriseURL: srv.URL, HTTPClient: srv.Client()})
	if _, ok := n.(*appriseBackend); !ok {
		t.Fatalf("expected apprise backend, got %T", n)
	}
	if err := n.Notify(context.Background(), Notification{Title: "T", Body: "B"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if gotCT != "application/json" {
		t.Errorf("content-type = %q, want application/json", gotCT)
	}
	if gotTitle != "T" || gotBody != "B" {
		t.Errorf("payload = {%q,%q}, want {T,B}", gotTitle, gotBody)
	}
}

// TestFanOutBothBackends verifies that with both ntfy and Apprise configured the
// Notifier fans out to each (multiBackend).
func TestFanOutBothBackends(t *testing.T) {
	var mu sync.Mutex
	hits := map[string]int{}
	mk := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			hits[name]++
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		}))
	}
	ntfySrv := mk("ntfy")
	defer ntfySrv.Close()
	appSrv := mk("apprise")
	defer appSrv.Close()

	n := New(Config{
		NtfyServer: ntfySrv.URL,
		NtfyTopic:  "t",
		AppriseURL: appSrv.URL,
		HTTPClient: &http.Client{Timeout: 5 * time.Second},
	})
	if _, ok := n.(multiBackend); !ok {
		t.Fatalf("expected multi backend, got %T", n)
	}
	if err := n.Notify(context.Background(), Notification{Body: "x"}); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits["ntfy"] != 1 || hits["apprise"] != 1 {
		t.Errorf("fan-out hits = %v, want each backend hit once", hits)
	}
}

// TestNoopWhenUnconfigured verifies the fallback: with no DID, no topic, and no
// Apprise URL there is no external backend (a real result is never lost because
// the durable in-app record happens upstream).
func TestNoopWhenUnconfigured(t *testing.T) {
	n := New(Config{})
	if _, ok := n.(noopBackend); !ok {
		t.Fatalf("expected noop backend, got %T", n)
	}
	if err := n.Notify(context.Background(), Notification{Body: "x"}); err != nil {
		t.Errorf("noop Notify must not error, got %v", err)
	}
}

// TestDeliverNonBlocking verifies Deliver never blocks and never panics on a
// failing backend: it fires the send on a background goroutine and logs honestly.
func TestDeliverNonBlocking(t *testing.T) {
	done := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(done)
		w.WriteHeader(http.StatusInternalServerError) // honest failure path
	}))
	defer srv.Close()

	n := New(Config{NtfyServer: srv.URL, NtfyTopic: "t", HTTPClient: srv.Client()})
	Deliver(n, Notification{Body: "x"}) // returns immediately
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatalf("Deliver did not fire the background send")
	}

	// A nil notifier is a safe no-op.
	Deliver(nil, Notification{Body: "x"})
}
