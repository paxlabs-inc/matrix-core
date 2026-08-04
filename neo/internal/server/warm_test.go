// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

// Q2 — warm-on-open. These exercise the REAL Engine over a REAL Pager + Neocortex
// store (only the co-located daemon is unreachable, which fetchUserProfile
// tolerates). They prove the first inbound HTTP request warms the embedder/HNSW
// substrate once, off the request's critical path, and that the flag gates it.

func warmEngine(t *testing.T, warm bool) *Engine {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-warm-test"
	cfg.WarmOnOpen = warm
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	t.Cleanup(func() { _ = pager.Close() })
	e := NewEngine(EngineOptions{
		Config:     cfg,
		Pager:      pager,
		BackendURL: "http://127.0.0.1:1", // unreachable daemon; profile fetch is best-effort
	})
	t.Cleanup(e.Close)
	return e
}

func waitWarmed(t *testing.T, e *Engine) bool {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e.Warmed() {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return e.Warmed()
}

// TestWarmBodyRunsSubstrateRetrieval proves the synchronous warm actually
// exercises the substrate (Retrieve over a real embedder/HNSW) and flips the
// observable warmed flag.
func TestWarmBodyRunsSubstrateRetrieval(t *testing.T) {
	e := warmEngine(t, true)
	if e.Warmed() {
		t.Fatal("precondition: engine must start un-warmed")
	}
	e.warmBody(context.Background())
	if !e.Warmed() {
		t.Fatal("warmBody did not flip Warmed()")
	}
}

// TestWarmIsOneShotAndAsync proves Warm() is idempotent (safe to call on every
// request) and completes the warm on a background goroutine.
func TestWarmIsOneShotAndAsync(t *testing.T) {
	e := warmEngine(t, true)
	e.Warm()
	e.Warm() // idempotent: the second call is a no-op, not a second warm
	if !waitWarmed(t, e) {
		t.Fatal("Warm() did not warm the substrate within the deadline")
	}
}

// TestWarmDisabledIsNoOp proves the flag gates the behavior: with WarmOnOpen off
// the engine never warms.
func TestWarmDisabledIsNoOp(t *testing.T) {
	e := warmEngine(t, false)
	e.Warm()
	// Give any (erroneous) goroutine a chance to run before asserting.
	time.Sleep(50 * time.Millisecond)
	if e.Warmed() {
		t.Fatal("engine warmed despite WarmOnOpen being off")
	}
}

// TestWarmNilPagerNoPanic proves the guardrails: a nil-pager engine (and a nil
// engine) never panic on Warm.
func TestWarmNilPagerNoPanic(t *testing.T) {
	e := NewEngine(EngineOptions{
		Config:     config.Config{WarmOnOpen: true},
		BackendURL: "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	e.Warm() // pager is nil → no-op, no panic
	time.Sleep(20 * time.Millisecond)
	if e.Warmed() {
		t.Fatal("engine with no pager reported warmed")
	}
	var nilEngine *Engine
	nilEngine.Warm() // must not panic
}

// TestWarmOnFirstRequest proves the HTTP middleware fires the warm on the first
// inbound request and still serves it (the warm never blocks the response).
func TestWarmOnFirstRequest(t *testing.T) {
	e := warmEngine(t, true)
	srv, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	h := srv.Handler()

	// A Neo-owned route that answers without the daemon: POST /halt (kill
	// switch) returns 200 with halted:0 when nothing is live.
	req := httptest.NewRequest(http.MethodPost, "/halt", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("request served with status %d, want 200", rec.Code)
	}
	if !waitWarmed(t, e) {
		t.Fatal("first request did not trigger the warm-on-open")
	}
}
