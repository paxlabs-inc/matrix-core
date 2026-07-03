// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package preview

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/cody/internal/sandbox"
	"matrix/cody/internal/workspace"
)

// railwayFixture is a recorded httptest stand-in for the Railway GraphQL
// sandbox API. It speaks the real GraphQL-over-HTTP protocol the sandbox.Client
// sends, so the client's marshal → HTTP → parse path is exercised for real
// (req 17.5: sandbox lifecycle against recorded httptest fixtures). Only the
// upstream responses are canned — the code under test is real.
type railwayFixture struct {
	mu           sync.Mutex
	created      int
	written      int
	destroyed    int
	execCommands [][]string
	failExecOnce bool // when set, the first exec returns exitCode!=0
	probeCode    string
}

func newRailwayFixture() *railwayFixture { return &railwayFixture{probeCode: "200"} }

func (f *railwayFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer railway-tok" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(body, &req)
		f.mu.Lock()
		defer f.mu.Unlock()
		switch {
		case strings.Contains(req.Query, "sandboxCreate"):
			f.created++
			writeData(w, `{"sandboxCreate":{"id":"sb-1","privateHost":"sb-1.railway.internal","status":"RUNNING"}}`)
		case strings.Contains(req.Query, "sandboxExec"):
			cmd := extractCommand(req.Variables)
			f.execCommands = append(f.execCommands, cmd)
			joined := strings.Join(cmd, " ")
			exit := 0
			stdout := ""
			if strings.Contains(joined, "http_code") {
				stdout = f.probeCode // health probe
			}
			if f.failExecOnce {
				f.failExecOnce = false
				exit = 1
				stdout = "boom"
			}
			writeData(w, `{"sandboxExec":{"stdout":`+jsonStr(stdout)+`,"stderr":"","exitCode":`+itoa(exit)+`,"durationMs":5}}`)
		case strings.Contains(req.Query, "sandboxFilesWrite"):
			f.written++
			writeData(w, `{"sandboxFilesWrite":{"written":1}}`)
		case strings.Contains(req.Query, "sandboxDestroy"):
			f.destroyed++
			writeData(w, `{"sandboxDestroy":true}`)
		default:
			http.Error(w, "unknown op", http.StatusBadRequest)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeData(w http.ResponseWriter, data string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"data":` + data + `}`))
}

func extractCommand(vars map[string]any) []string {
	input, _ := vars["input"].(map[string]any)
	raw, _ := input["command"].([]any)
	out := make([]string, 0, len(raw))
	for _, r := range raw {
		if s, ok := r.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }
func itoa(i int) string       { b, _ := json.Marshal(i); return string(b) }

// routerFixture records the router internal preview register/deregister door.
type routerFixture struct {
	mu           sync.Mutex
	registered   []map[string]string
	deregistered int
	gotToken     string
}

func (rf *routerFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/preview/register" {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		rf.mu.Lock()
		defer rf.mu.Unlock()
		rf.gotToken = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		switch r.Method {
		case http.MethodPost:
			var body map[string]string
			_ = json.NewDecoder(r.Body).Decode(&body)
			rf.registered = append(rf.registered, body)
		case http.MethodDelete:
			rf.deregistered++
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// recorder captures emitted preview.* events.
type recorder struct {
	mu     sync.Mutex
	events []recEvent
}
type recEvent struct {
	convID string
	typ    string
	fields map[string]interface{}
}

func (r *recorder) emit(convID, typ string, fields map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, recEvent{convID, typ, fields})
}
func (r *recorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.typ
	}
	return out
}
func (r *recorder) last(typ string) (recEvent, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := len(r.events) - 1; i >= 0; i-- {
		if r.events[i].typ == typ {
			return r.events[i], true
		}
	}
	return recEvent{}, false
}

// seedNodeProject writes a minimal Node project with a dev script so DetectStart
// fires on the real workspace scanner.
func seedNodeProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "package.json"),
		[]byte(`{"name":"demo","scripts":{"dev":"vite"},"dependencies":{"react":"^18"}}`), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "index.jsx"), []byte("export default ()=>null\n"), 0o644))
	// Prove the real scanner detects it as runnable.
	m, err := workspace.Scan(root)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if _, ok := DetectStart(m); !ok {
		t.Fatalf("DetectStart did not fire on seeded node project")
	}
	return root
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func newManager(t *testing.T, railway, router *httptest.Server, rec *recorder) *Manager {
	t.Helper()
	sb := sandbox.New(sandbox.Config{
		Token:         "railway-tok",
		ProjectID:     "proj-1",
		EnvironmentID: "env-1",
		Endpoint:      railway.URL,
	})
	return New(sb, rec.emit, Config{
		UserID:            "user-abc",
		RouterInternalURL: router.URL,
		RouterToken:       "preview-tok",
		TTL:               30 * time.Minute,
		Image:             "ubuntu:24.04",
	})
}

func TestProvisionReadyLifecycle(t *testing.T) {
	rw := newRailwayFixture()
	rwSrv := rw.server(t)
	rf := &routerFixture{}
	rfSrv := rf.server(t)
	rec := &recorder{}
	m := newManager(t, rwSrv, rfSrv, rec)

	root := seedNodeProject(t)
	info, err := m.Provision(context.Background(), Request{ConvID: "conv-1", Root: root})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// The proxied URL is site-relative and user-scoped.
	if info.URL != "/preview/user-abc/" {
		t.Fatalf("URL = %q, want /preview/user-abc/", info.URL)
	}
	// pending must precede ready.
	types := rec.types()
	if len(types) < 2 || types[0] != EvtPending || types[len(types)-1] != EvtReady {
		t.Fatalf("event sequence = %v, want pending…ready", types)
	}
	ready, _ := rec.last(EvtReady)
	if ready.fields["url"] != "/preview/user-abc/" {
		t.Fatalf("ready url = %v", ready.fields["url"])
	}
	// Sandbox lifecycle happened for real over the fixture.
	if rw.created != 1 || rw.written != 1 {
		t.Fatalf("created=%d written=%d, want 1/1", rw.created, rw.written)
	}
	// Router registration carried the right target + bearer.
	if len(rf.registered) != 1 {
		t.Fatalf("registrations = %d, want 1", len(rf.registered))
	}
	reg := rf.registered[0]
	if reg["user_id"] != "user-abc" || reg["host"] != "sb-1.railway.internal" || reg["port"] != "5173" {
		t.Fatalf("register body = %v", reg)
	}
	if rf.gotToken != "preview-tok" {
		t.Fatalf("router token = %q", rf.gotToken)
	}
	// A health probe ran (exec carrying http_code).
	probed := false
	for _, c := range rw.execCommands {
		if strings.Contains(strings.Join(c, " "), "http_code") {
			probed = true
		}
	}
	if !probed {
		t.Fatalf("no health probe exec observed: %v", rw.execCommands)
	}
}

func TestProvisionFailsWhenNoStartCommand(t *testing.T) {
	rw := newRailwayFixture()
	rwSrv := rw.server(t)
	rf := &routerFixture{}
	rfSrv := rf.server(t)
	rec := &recorder{}
	m := newManager(t, rwSrv, rfSrv, rec)

	// A bare text project — no dev/start script, no known entrypoint.
	root := t.TempDir()
	must(t, os.WriteFile(filepath.Join(root, "README.md"), []byte("# hi\n"), 0o644))

	_, err := m.Provision(context.Background(), Request{ConvID: "conv-x", Root: root})
	if err == nil {
		t.Fatal("expected Provision to fail with no start command")
	}
	if _, ok := rec.last(EvtFailed); !ok {
		t.Fatalf("expected preview.failed, got %v", rec.types())
	}
	if rw.created != 0 {
		t.Fatalf("no sandbox should be created on detection failure, got %d", rw.created)
	}
	if len(rf.registered) != 0 {
		t.Fatal("no registration should happen on failure")
	}
}

func TestProvisionUnreachableAppCleansUp(t *testing.T) {
	rw := newRailwayFixture()
	rw.probeCode = "000" // curl connection refused — never becomes reachable
	rwSrv := rw.server(t)
	rf := &routerFixture{}
	rfSrv := rf.server(t)
	rec := &recorder{}
	m := newManager(t, rwSrv, rfSrv, rec)
	// Shrink the probe budget via a cancellable context so the test is fast.
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()

	root := seedNodeProject(t)
	_, err := m.Provision(ctx, Request{ConvID: "conv-2", Root: root})
	if err == nil {
		t.Fatal("expected failure when app never becomes reachable")
	}
	if _, ok := rec.last(EvtFailed); !ok {
		t.Fatalf("expected preview.failed, got %v", rec.types())
	}
	// The sandbox was created then destroyed (no leak) and never registered.
	if rw.created != 1 || rw.destroyed != 1 {
		t.Fatalf("created=%d destroyed=%d, want 1/1 (cleanup)", rw.created, rw.destroyed)
	}
	if len(rf.registered) != 0 {
		t.Fatal("must not register an unreachable app")
	}
}

func TestReaperDestroysIdleAndEmitsExpired(t *testing.T) {
	rw := newRailwayFixture()
	rwSrv := rw.server(t)
	rf := &routerFixture{}
	rfSrv := rf.server(t)
	rec := &recorder{}
	m := newManager(t, rwSrv, rfSrv, rec)

	// Deterministic clock.
	base := time.Date(2026, 7, 3, 12, 0, 0, 0, time.UTC)
	m.now = func() time.Time { return base }

	root := seedNodeProject(t)
	if _, err := m.Provision(context.Background(), Request{ConvID: "conv-3", Root: root}); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	destroyedBefore := rw.destroyed

	// Advance past the TTL and reap.
	m.now = func() time.Time { return base.Add(31 * time.Minute) }
	m.reapOnce(context.Background())

	if rw.destroyed != destroyedBefore+1 {
		t.Fatalf("reaper did not destroy the idle sandbox (destroyed=%d)", rw.destroyed)
	}
	if rf.deregistered == 0 {
		t.Fatal("reaper did not deregister the preview target")
	}
	if _, ok := rec.last(EvtExpired); !ok {
		t.Fatalf("reaper did not emit preview.expired: %v", rec.types())
	}
	// A fresh reap with nothing idle is a no-op.
	before := rw.destroyed
	m.reapOnce(context.Background())
	if rw.destroyed != before {
		t.Fatal("second reap destroyed something it should not have")
	}
}

func TestRePreviewReplacesOldSandbox(t *testing.T) {
	rw := newRailwayFixture()
	rwSrv := rw.server(t)
	rf := &routerFixture{}
	rfSrv := rf.server(t)
	rec := &recorder{}
	m := newManager(t, rwSrv, rfSrv, rec)

	root := seedNodeProject(t)
	if _, err := m.Provision(context.Background(), Request{ConvID: "conv-4", Root: root}); err != nil {
		t.Fatalf("Provision 1: %v", err)
	}
	if _, err := m.Provision(context.Background(), Request{ConvID: "conv-4", Root: root}); err != nil {
		t.Fatalf("Provision 2: %v", err)
	}
	// Two creates, and the first sandbox was destroyed by the replacement.
	if rw.created != 2 {
		t.Fatalf("created=%d, want 2", rw.created)
	}
	if rw.destroyed < 1 {
		t.Fatal("re-preview did not destroy the previous sandbox")
	}
}

func TestDisabledManagerFailsClearly(t *testing.T) {
	rec := &recorder{}
	// No UserID / RouterInternalURL → disabled.
	m := New(sandbox.New(sandbox.Config{}), rec.emit, Config{})
	if m.Enabled() {
		t.Fatal("manager should be disabled without UserID/RouterInternalURL")
	}
	_, err := m.Provision(context.Background(), Request{ConvID: "c", Root: t.TempDir()})
	if err == nil {
		t.Fatal("disabled manager should fail Provision")
	}
	if _, ok := rec.last(EvtFailed); !ok {
		t.Fatal("disabled manager should emit preview.failed")
	}
}

func TestDetectStartTable(t *testing.T) {
	cases := []struct {
		name    string
		model   *workspace.Model
		wantOK  bool
		wantSrv string
		port    string
	}{
		{
			name:    "next dev",
			model:   &workspace.Model{Frameworks: []string{"next"}, BuildTargets: []string{"dev", "build"}, Files: []workspace.File{{Path: "package.json"}}},
			wantOK:  true,
			wantSrv: "npm run dev -- -H 0.0.0.0 -p 3000",
			port:    "3000",
		},
		{
			name:    "vite dev",
			model:   &workspace.Model{Frameworks: []string{"react", "vite"}, BuildTargets: []string{"dev"}, Files: []workspace.File{{Path: "package.json"}}},
			wantOK:  true,
			wantSrv: "npm run dev -- --host 0.0.0.0 --port 5173",
			port:    "5173",
		},
		{
			name:   "go service",
			model:  &workspace.Model{Languages: map[string]int{"go": 3}, EntryPoints: []string{"main.go"}},
			wantOK: true, wantSrv: "go run .", port: "8080",
		},
		{
			name:   "python fastapi",
			model:  &workspace.Model{Languages: map[string]int{"python": 2}, Frameworks: []string{"fastapi"}, Files: []workspace.File{{Path: "main.py"}}},
			wantOK: true, port: "8000",
		},
		{
			name:   "empty",
			model:  &workspace.Model{},
			wantOK: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, ok := DetectStart(tc.model)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if tc.wantSrv != "" && spec.Serve != tc.wantSrv {
				t.Fatalf("Serve = %q, want %q", spec.Serve, tc.wantSrv)
			}
			if tc.port != "" && spec.Port != tc.port {
				t.Fatalf("Port = %q, want %q", spec.Port, tc.port)
			}
		})
	}
}
