// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package preview

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"centra/agents/neo/internal/sandbox"
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
	// Prove the real detector reads the manifest as runnable.
	if _, ok := DetectStart(root); !ok {
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

// TestConcurrentSandboxCapHolds proves the per-user cap (req 7.4): a third
// concurrent preview evicts the least-recently-touched one (destroy +
// preview.expired) so at most MaxActive sandboxes ever live.
func TestConcurrentSandboxCapHolds(t *testing.T) {
	rw := newRailwayFixture()
	rwSrv := rw.server(t)
	rf := &routerFixture{}
	rfSrv := rf.server(t)
	rec := &recorder{}
	sb := sandbox.New(sandbox.Config{
		Token: "railway-tok", ProjectID: "p", EnvironmentID: "e", Endpoint: rwSrv.URL,
	})
	m := New(sb, rec.emit, Config{
		UserID:            "user-abc",
		RouterInternalURL: rfSrv.URL,
		MaxActive:         2,
	})
	base := time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC)
	tick := 0
	m.now = func() time.Time { tick++; return base.Add(time.Duration(tick) * time.Second) }

	root := seedNodeProject(t)
	for _, conv := range []string{"conv-a", "conv-b", "conv-c"} {
		if _, err := m.Provision(context.Background(), Request{ConvID: conv, Root: root}); err != nil {
			t.Fatalf("Provision %s: %v", conv, err)
		}
	}
	m.mu.Lock()
	n := len(m.active)
	_, aAlive := m.active["conv-a"]
	m.mu.Unlock()
	if n != 2 {
		t.Fatalf("active = %d, want the cap (2)", n)
	}
	if aAlive {
		t.Fatal("the oldest preview must have been evicted for the cap")
	}
	if ev, ok := rec.last(EvtExpired); !ok || ev.convID != "conv-a" {
		t.Fatalf("eviction must emit preview.expired on the evicted conversation, got %v", rec.types())
	}
	if rw.destroyed < 1 {
		t.Fatal("eviction must destroy the sandbox for real")
	}
}

// localSandbox is a REAL second implementation of the sandbox.Client seam: it
// runs commands with a real shell in a real directory on this machine — so
// the lifecycle test drives an ACTUAL dev server and the health probe curls
// an ACTUAL HTTP listener (req 7.5: a real serving target, not a canned
// probe). Destroy kills the started server for real.
type localSandbox struct {
	t    *testing.T
	dir  string
	env  map[string]string
	port string
}

func (l *localSandbox) Create(ctx context.Context, opts sandbox.CreateOpts) (*sandbox.Sandbox, error) {
	l.env = opts.Env
	return &sandbox.Sandbox{ID: "local-1", PrivateHost: "127.0.0.1", Status: sandbox.StatusRunning}, nil
}

func (l *localSandbox) Exec(ctx context.Context, id string, cmd []string) (sandbox.ExecResult, error) {
	c := exec.CommandContext(ctx, cmd[0], cmd[1:]...)
	c.Dir = l.dir
	c.Env = os.Environ()
	for k, v := range l.env {
		c.Env = append(c.Env, k+"="+v)
	}
	out, err := c.CombinedOutput()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			return sandbox.ExecResult{}, err
		}
	}
	return sandbox.ExecResult{Stdout: string(out), Exit: exit}, nil
}

func (l *localSandbox) WriteFiles(ctx context.Context, id string, files []sandbox.File) error {
	for _, f := range files {
		p := filepath.Join(l.dir, filepath.FromSlash(f.Path))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(p, []byte(f.Content), 0o644); err != nil {
			return err
		}
	}
	return nil
}

func (l *localSandbox) Destroy(ctx context.Context, id string) error {
	// Kill the real dev server the start command backgrounded.
	_ = exec.Command("pkill", "-f", "http.server "+l.port).Run()
	return nil
}

// TestLifecycleAgainstRealServingTarget drives the FULL controller lifecycle
// (pending → ready → expired) against a genuinely serving HTTP target: the
// detected dev command starts a real python http.server, the health probe
// curls it for real, and teardown kills it and emits preview.expired.
func TestLifecycleAgainstRealServingTarget(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not available")
	}
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl not available")
	}
	rf := &routerFixture{}
	rfSrv := rf.server(t)
	rec := &recorder{}

	// A real project whose dev script binds a real listener on $PORT.
	root := t.TempDir()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve preview port: %v", err)
	}
	port := strconv.Itoa(listener.Addr().(*net.TCPAddr).Port)
	if err := listener.Close(); err != nil {
		t.Fatalf("release preview port: %v", err)
	}
	must(t, os.WriteFile(filepath.Join(root, "package.json"),
		[]byte(fmt.Sprintf(`{"name":"demo","config":{"port":%q},"scripts":{"dev":"exec python3 -m http.server $PORT --bind 127.0.0.1"}}`, port)), 0o644))
	must(t, os.WriteFile(filepath.Join(root, "index.html"), []byte("<h1>real preview</h1>\n"), 0o644))

	ls := &localSandbox{t: t, dir: t.TempDir(), port: port}
	t.Cleanup(func() { _ = ls.Destroy(context.Background(), "local-1") })
	if _, err := exec.LookPath("npm"); err != nil {
		t.Skip("npm not available")
	}

	m := New(ls, rec.emit, Config{
		UserID:            "user-real",
		RouterInternalURL: rfSrv.URL,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	info, err := m.Provision(ctx, Request{ConvID: "conv-real", Root: root})
	if err != nil {
		t.Fatalf("Provision against the real server: %v", err)
	}
	if info.Port != port {
		t.Fatalf("port = %q, want %s", info.Port, port)
	}

	// The target REALLY serves: fetch the deployed file over real HTTP.
	previewURL := "http://127.0.0.1:" + port + "/index.html"
	resp, err := http.Get(previewURL)
	if err != nil {
		t.Fatalf("GET real target: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !strings.Contains(string(body), "real preview") {
		t.Fatalf("real target served %d %q", resp.StatusCode, body)
	}

	// pending → ready in order.
	types := rec.types()
	if types[0] != EvtPending || types[len(types)-1] != EvtReady {
		t.Fatalf("event sequence = %v", types)
	}

	// Teardown: the server dies for real and preview.expired is emitted.
	m.Teardown(context.Background(), "conv-real")
	if _, ok := rec.last(EvtExpired); !ok {
		t.Fatalf("teardown must emit preview.expired, got %v", rec.types())
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := http.Get(previewURL); err != nil {
			return // listener really gone
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatal("the real dev server is still serving after teardown")
}

// TestDetectStartTable pins the manifest-reading detector.
func TestDetectStartTable(t *testing.T) {
	write := func(t *testing.T, root, name, content string) {
		t.Helper()
		must(t, os.WriteFile(filepath.Join(root, name), []byte(content), 0o644))
	}
	t.Run("next dev", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "package.json", `{"scripts":{"dev":"next dev"},"dependencies":{"next":"15"}}`)
		spec, ok := DetectStart(root)
		if !ok || spec.Serve != "npm run dev -- -H 0.0.0.0 -p 3000" || spec.Port != "3000" {
			t.Fatalf("spec = %+v ok=%v", spec, ok)
		}
	})
	t.Run("vite dev", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "package.json", `{"scripts":{"dev":"vite"},"devDependencies":{"vite":"6"},"dependencies":{"react":"18"}}`)
		spec, ok := DetectStart(root)
		if !ok || spec.Serve != "npm run dev -- --host 0.0.0.0 --port 5173" || spec.Port != "5173" {
			t.Fatalf("spec = %+v ok=%v", spec, ok)
		}
	})
	t.Run("go service", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "go.mod", "module demo\n")
		spec, ok := DetectStart(root)
		if !ok || spec.Serve != "go run ." || spec.Port != "8080" {
			t.Fatalf("spec = %+v ok=%v", spec, ok)
		}
	})
	t.Run("python fastapi", func(t *testing.T) {
		root := t.TempDir()
		write(t, root, "main.py", "app = None\n")
		spec, ok := DetectStart(root)
		if !ok || spec.Port != "8000" || !strings.Contains(spec.Serve, "uvicorn") {
			t.Fatalf("spec = %+v ok=%v", spec, ok)
		}
	})
	t.Run("empty", func(t *testing.T) {
		if _, ok := DetectStart(t.TempDir()); ok {
			t.Fatal("empty project must not detect")
		}
	})
}
