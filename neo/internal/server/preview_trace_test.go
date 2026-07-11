// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// NEO-WORKBENCH task 4.1 (req 7.2): preview.* events are durable — a reopened
// conversation replays them from the trace so the pane rebuilds the LAST
// honest state (here: ready then expired ⇒ the pane must land on expired).
func TestPreviewEventsReplayAcrossReopen(t *testing.T) {
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		TraceDir:        t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	srv, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	const convID = "conv_preview"
	const intentID = "neo_preview_run"
	r := &run{id: intentID, convID: convID}
	e.registerRun(r)
	e.broker.ensure(intentID)
	e.conv.AppendUser(convID, intentID, "preview the app")

	// The real controller emission path (publishPreview → broker → trace tap).
	e.publishPreview(convID, "preview.pending", map[string]interface{}{"state": "pending"})
	e.publishPreview(convID, "preview.ready", map[string]interface{}{
		"state": "ready", "url": "/preview/user-abc/",
	})
	e.publishPreview(convID, "preview.expired", map[string]interface{}{"state": "expired"})
	e.trace.Flush()
	e.broker.closeRun(intentID)
	e.unregisterRun(intentID)

	req := httptest.NewRequest(http.MethodGet, "/conversations/"+convID+"/trace", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("trace = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, typ := range []string{"preview.pending", "preview.ready", "preview.expired"} {
		if !strings.Contains(body, typ) {
			t.Fatalf("replayed trace missing %s: %s", typ, body)
		}
	}
	if !strings.Contains(body, "/preview/user-abc/") {
		t.Fatal("replayed trace missing the ready URL")
	}
	// Order preserved: ready precedes expired, so the client fold lands on
	// the honest final state.
	if strings.Index(body, "preview.ready") > strings.Index(body, "preview.expired") {
		t.Fatal("replayed preview events out of order")
	}
}

// TestWorkspacePreviewRouteUnconfigured proves the route answers an honest
// 503 when no sandbox client is configured.
func TestWorkspacePreviewRouteUnconfigured(t *testing.T) {
	e := NewEngine(EngineOptions{
		WorkspaceDir: t.TempDir(),
		BackendURL:   "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	srv, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/workspace/preview",
		strings.NewReader(`{"conversation_id":"c1"}`))
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("unconfigured preview = %d, want 503", rec.Code)
	}
}
