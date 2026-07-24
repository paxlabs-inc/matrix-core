// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// DOJO wave 3 — the client desktop surface + control lease at the HTTP seam,
// on the real Engine handlers and the real dojo.Manager. These legs never
// reach the sandbox transport (lease and lifecycle rejections gate first), so
// they run ungated; the transport-touching legs live in the NEO_DOJO_CONTRACT
// docker test.

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"matrix/neo/internal/dojo"
)

func wave3Manager(t *testing.T) *dojo.Manager {
	t.Helper()
	m := dojo.New(noRailway{}, nil, dojo.Config{})
	if _, err := m.RegisterSessionForTest("local-sb"); err != nil {
		t.Fatalf("register session: %v", err)
	}
	return m
}

func postJSON(t *testing.T, s *Server, handler string, body string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest("POST", handler, strings.NewReader(body))
	w := httptest.NewRecorder()
	switch handler {
	case "/dojo/takeover":
		s.handleDojoTakeover(w, r)
	case "/dojo/handback":
		s.handleDojoHandback(w, r)
	case "/dojo/input":
		s.handleDojoInput(w, r)
	default:
		t.Fatalf("unknown handler %s", handler)
	}
	return w
}

// TestDojoLeaseRoutes walks the lease over the real HTTP handlers: takeover
// 200 → the agent proxy answers the typed takeover_active → input-without-
// lease refused after handback 200 → the agent's mutating action answers the
// typed reobserve_required (req 4.2/4.3 at the wire).
func TestDojoLeaseRoutes(t *testing.T) {
	e := &Engine{dojo: wave3Manager(t), dojoBridgeToken: "secret"}
	s := &Server{engine: e}

	// Take control.
	w := postJSON(t, s, "/dojo/takeover", `{"conversation_id":"contract"}`)
	if w.Code != 200 {
		t.Fatalf("takeover = %d: %s", w.Code, w.Body.String())
	}
	var tk struct {
		Session dojo.Session `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &tk); err != nil || tk.Session.State != dojo.StateTakeover {
		t.Fatalf("takeover body = %s (%v)", w.Body.String(), err)
	}

	// The agent's bridge proxy is refused with the typed lease result.
	r := httptest.NewRequest("POST", "/dojo/computer-use", strings.NewReader(`{"action":"click_mouse","coordinates":{"x":1,"y":1},"clickCount":1}`))
	r.Header.Set("Authorization", "Bearer secret")
	pw := httptest.NewRecorder()
	s.handleDojoComputerUse(pw, r)
	if pw.Code != 409 || !strings.Contains(pw.Body.String(), "takeover_active") {
		t.Fatalf("agent call during takeover = %d: %s", pw.Code, pw.Body.String())
	}

	// Hand back.
	w = postJSON(t, s, "/dojo/handback", `{"conversation_id":"contract"}`)
	if w.Code != 200 {
		t.Fatalf("handback = %d: %s", w.Code, w.Body.String())
	}

	// Input passthrough refused once the lease is gone.
	w = postJSON(t, s, "/dojo/input", `{"conversation_id":"contract","request":{"action":"click_mouse","coordinates":{"x":1,"y":1},"clickCount":1}}`)
	if w.Code != 409 || !strings.Contains(w.Body.String(), "not_in_takeover") {
		t.Fatalf("input after handback = %d: %s", w.Code, w.Body.String())
	}

	// The agent must re-observe before mutating.
	r = httptest.NewRequest("POST", "/dojo/computer-use", strings.NewReader(`{"action":"type_text","text":"hi"}`))
	r.Header.Set("Authorization", "Bearer secret")
	pw = httptest.NewRecorder()
	s.handleDojoComputerUse(pw, r)
	if pw.Code != 409 || !strings.Contains(pw.Body.String(), "reobserve_required") {
		t.Fatalf("mutating call after handback = %d: %s", pw.Code, pw.Body.String())
	}
}

// TestDojoSessionAndFrameRoutes covers the panel's pull side without a
// transport: session state for a live conversation, null for an unknown one,
// and the frame's typed no-desktop result.
func TestDojoSessionAndFrameRoutes(t *testing.T) {
	e := &Engine{dojo: wave3Manager(t)}
	s := &Server{engine: e}

	w := httptest.NewRecorder()
	s.handleDojoSession(w, httptest.NewRequest("GET", "/dojo/session?conversation=contract", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"active"`) {
		t.Fatalf("session = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleDojoSession(w, httptest.NewRequest("GET", "/dojo/session?conversation=other", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"session":null`) {
		t.Fatalf("session(other) = %d: %s", w.Code, w.Body.String())
	}

	w = httptest.NewRecorder()
	s.handleDojoFrame(w, httptest.NewRequest("GET", "/dojo/frame?conversation=other", nil))
	if w.Code != 404 {
		t.Fatalf("frame(other) = %d: %s", w.Code, w.Body.String())
	}

	// Not configured → typed 503 on every dojo client route.
	bare := &Server{engine: &Engine{}}
	w = httptest.NewRecorder()
	bare.handleDojoSession(w, httptest.NewRequest("GET", "/dojo/session?conversation=x", nil))
	if w.Code != 503 {
		t.Fatalf("unconfigured session = %d", w.Code)
	}
}
