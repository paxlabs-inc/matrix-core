// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
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

	"centra/agents/neo/internal/dojo"
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
	case "/dojo/boot":
		s.handleDojoBoot(w, r)
	case "/dojo/shutdown":
		s.handleDojoShutdown(w, r)
	default:
		t.Fatalf("unknown handler %s", handler)
	}
	return w
}

// TestDojoUserPowerRoutes proves the desktop's power is the USER's: boot
// reattaches (or starts) a session with no agent involved, and shutdown runs
// the ship-first teardown, idempotently.
func TestDojoUserPowerRoutes(t *testing.T) {
	e := &Engine{dojo: wave3Manager(t)}
	s := &Server{engine: e}

	// Boot with a live session = reattach; the user's power button never
	// depends on the agent having called anything.
	w := postJSON(t, s, "/dojo/boot", `{"conversation_id":"contract"}`)
	if w.Code != 200 {
		t.Fatalf("boot = %d: %s", w.Code, w.Body.String())
	}
	var br struct {
		Session dojo.Session `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &br); err != nil || br.Session.State != dojo.StateActive {
		t.Fatalf("boot body = %s (%v)", w.Body.String(), err)
	}

	// Shutdown tears down (ship-first pipeline) and reports off.
	w = postJSON(t, s, "/dojo/shutdown", `{"conversation_id":"contract"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"session":null`) {
		t.Fatalf("shutdown = %d: %s", w.Code, w.Body.String())
	}
	if _, ok := e.dojo.SessionForConv("contract"); ok {
		t.Fatal("session survived shutdown")
	}
	// Idempotent: a second shutdown is a calm no-op.
	w = postJSON(t, s, "/dojo/shutdown", `{"conversation_id":"contract"}`)
	if w.Code != 200 {
		t.Fatalf("second shutdown = %d: %s", w.Code, w.Body.String())
	}

	// Not configured → typed 503.
	bare := &Server{engine: &Engine{}}
	r := httptest.NewRequest("POST", "/dojo/boot", strings.NewReader(`{"conversation_id":"x"}`))
	bw := httptest.NewRecorder()
	bare.handleDojoBoot(bw, r)
	if bw.Code != 503 {
		t.Fatalf("unconfigured boot = %d", bw.Code)
	}
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
// transport: session state for a live conversation, THE live session surfaced
// to every other conversation (one computer, MaxActive=1), and the typed
// no-desktop results when nothing runs.
func TestDojoSessionAndFrameRoutes(t *testing.T) {
	e := &Engine{dojo: wave3Manager(t)}
	s := &Server{engine: e}

	w := httptest.NewRecorder()
	s.handleDojoSession(w, httptest.NewRequest("GET", "/dojo/session?conversation=contract", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"active"`) {
		t.Fatalf("session = %d: %s", w.Code, w.Body.String())
	}

	// The user has ONE computer: another conversation's panel sees it too.
	w = httptest.NewRecorder()
	s.handleDojoSession(w, httptest.NewRequest("GET", "/dojo/session?conversation=other", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"active"`) {
		t.Fatalf("session(other) = %d, want the live session: %s", w.Code, w.Body.String())
	}

	// With nothing live, session is null and the frame is typed 404.
	empty := &Server{engine: &Engine{dojo: dojo.New(noRailway{}, nil, dojo.Config{})}}
	w = httptest.NewRecorder()
	empty.handleDojoSession(w, httptest.NewRequest("GET", "/dojo/session?conversation=x", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"session":null`) {
		t.Fatalf("session(empty) = %d: %s", w.Code, w.Body.String())
	}
	w = httptest.NewRecorder()
	empty.handleDojoFrame(w, httptest.NewRequest("GET", "/dojo/frame?conversation=x", nil))
	if w.Code != 404 {
		t.Fatalf("frame(empty) = %d: %s", w.Code, w.Body.String())
	}

	// Not configured → typed 503 on every dojo client route.
	bare := &Server{engine: &Engine{}}
	w = httptest.NewRecorder()
	bare.handleDojoSession(w, httptest.NewRequest("GET", "/dojo/session?conversation=x", nil))
	if w.Code != 503 {
		t.Fatalf("unconfigured session = %d", w.Code)
	}
}

// TestDojoBootReattachesTheOneComputer: booting from a conversation that has
// no bound session while the user's computer is already running reattaches it
// — never evict-and-reboot, never a second sandbox.
func TestDojoBootReattachesTheOneComputer(t *testing.T) {
	e := &Engine{dojo: wave3Manager(t)}
	s := &Server{engine: e}
	w := postJSON(t, s, "/dojo/boot", `{"conversation_id":"a-fresh-chat"}`)
	if w.Code != 200 {
		t.Fatalf("boot = %d: %s", w.Code, w.Body.String())
	}
	var br struct {
		Session dojo.Session `json:"session"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &br); err != nil {
		t.Fatalf("boot body: %v", err)
	}
	if br.Session.State != dojo.StateActive || br.Session.ConvID != "contract" {
		t.Fatalf("boot did not reattach the live computer: %+v", br.Session)
	}
}
