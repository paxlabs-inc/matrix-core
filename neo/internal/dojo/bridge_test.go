// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package dojo

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// shellFixture is a Railway GraphQL httptest stand-in that EMULATES the sandbox
// shell for the carrier's exec-curl scripts: it serves a canned bytebotd
// response through the real execCurlTransport (write-to-file curl, wc -c length,
// tail|head|base64 chunk reads) and a canned a11y payload through the
// docker-exec desktop wrapper. The real NewRailway client + real Manager +
// real execCurlTransport run against it end to end — no carrier logic is faked.
type shellFixture struct {
	bytebotBody       []byte // canned bytebotd response body
	bytebotCode       int    // canned bytebotd HTTP status
	a11yOut           string // canned a11y dumper stdout
	execOutputCap     int    // Railway's sandboxExec stdout cap; 0 means unlimited
	chunkDelay        time.Duration
	mu                sync.Mutex
	chunksInFlight    int
	maxChunksInFlight int
	truncatedReads    int
}

var (
	chunkRe = regexp.MustCompile(`tail -c \+(\d+) \S+ \| head -c (\d+)`)
)

func (f *shellFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		if !strings.Contains(body.Query, "sandboxExec") {
			t.Errorf("unexpected op: %s", body.Query)
			_, _ = io.WriteString(w, `{"errors":[{"message":"unexpected"}]}`)
			return
		}
		cmd, _ := body.Variables["command"].(string)
		if chunkRe.MatchString(cmd) {
			f.mu.Lock()
			f.chunksInFlight++
			if f.chunksInFlight > f.maxChunksInFlight {
				f.maxChunksInFlight = f.chunksInFlight
			}
			f.mu.Unlock()
			defer func() {
				f.mu.Lock()
				f.chunksInFlight--
				f.mu.Unlock()
			}()
			time.Sleep(f.chunkDelay)
		}
		stdout := f.run(cmd)
		truncated := false
		if f.execOutputCap > 0 && len(stdout) > f.execOutputCap {
			stdout = stdout[:f.execOutputCap]
			truncated = true
			f.mu.Lock()
			f.truncatedReads++
			f.mu.Unlock()
		}
		out, _ := json.Marshal(stdout)
		fmt.Fprintf(w, `{"data":{"sandboxExec":{"stdout":%s,"stderr":"","exitCode":0,"timedOut":false,"truncated":%t}}}`, out, truncated)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// run emulates the sandbox shell for exactly the commands the carrier issues.
func (f *shellFixture) run(cmd string) string {
	switch {
	case strings.Contains(cmd, "docker exec -u user"):
		// The a11y desktop-exec wrapper.
		return f.a11yOut
	case chunkRe.MatchString(cmd):
		// Chunk read: tail -c +OFF path | head -c N | base64 -w0.
		m := chunkRe.FindStringSubmatch(cmd)
		off, _ := strconv.Atoi(m[1])
		n, _ := strconv.Atoi(m[2])
		start := off - 1
		if start < 0 {
			start = 0
		}
		end := start + n
		if start > len(f.bytebotBody) {
			start = len(f.bytebotBody)
		}
		if end > len(f.bytebotBody) {
			end = len(f.bytebotBody)
		}
		return base64.StdEncoding.EncodeToString(f.bytebotBody[start:end])
	case strings.Contains(cmd, "curl") && strings.Contains(cmd, "wc -c"):
		// The Call script: echoes the HTTP code then the response length.
		code := f.bytebotCode
		if code == 0 {
			code = 201
		}
		return fmt.Sprintf("%d\n%d", code, len(f.bytebotBody))
	}
	return ""
}

func (f *shellFixture) maxConcurrentChunks() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.maxChunksInFlight
}

func (f *shellFixture) truncations() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.truncatedReads
}

func managerOverShell(t *testing.T, f *shellFixture) *Manager {
	t.Helper()
	srv := f.server(t)
	rw := NewRailway(RailwayConfig{Token: "tok", EnvironmentID: "env-1", Endpoint: srv.URL})
	m := New(rw, nil, Config{pollGap: time.Millisecond, probeGap: time.Millisecond})
	// Register a live session by hand (Provision is covered elsewhere) so the
	// carrier has a sandbox to resolve.
	m.mu.Lock()
	s := &session{id: "dojo-x", convID: "c1", state: StateActive, sandboxID: "sb-1", image: DefaultImage, createdAt: m.now(), touchedAt: m.now()}
	m.sessions[s.id] = s
	m.byConv[s.convID] = s.id
	m.mu.Unlock()
	return m
}

// TestCallBytebotdSmall drives a small bytebotd response through the real
// exec-curl carrier (single chunk).
func TestCallBytebotdSmall(t *testing.T) {
	f := &shellFixture{bytebotBody: []byte(`{"x":640,"y":480}`), bytebotCode: 201}
	m := managerOverShell(t, f)
	resp, err := m.CallBytebotd(context.Background(), []byte(`{"action":"cursor_position"}`))
	if err != nil {
		t.Fatalf("CallBytebotd: %v", err)
	}
	if string(resp) != `{"x":640,"y":480}` {
		t.Fatalf("resp = %q", resp)
	}
}

// TestCallBytebotdLargeChunked proves the truncation-safe chunked reassembly:
// a response bigger than execChunkBytes (a screenshot) is reconstructed
// byte-exact across multiple exec reads.
func TestCallBytebotdLargeChunked(t *testing.T) {
	// Build a payload ~2.5 chunks long with a recognisable pattern.
	big := make([]byte, execChunkBytes*2+12345)
	for i := range big {
		big[i] = byte('A' + (i % 26))
	}
	payload := append([]byte(`{"image":"`), big...)
	payload = append(payload, []byte(`"}`)...)
	f := &shellFixture{
		bytebotBody: payload, bytebotCode: 201, execOutputCap: 40_000,
		chunkDelay: 5 * time.Millisecond,
	}
	m := managerOverShell(t, f)
	resp, err := m.CallBytebotd(context.Background(), []byte(`{"action":"screenshot"}`))
	if err != nil {
		t.Fatalf("CallBytebotd: %v", err)
	}
	if len(resp) != len(payload) {
		t.Fatalf("reassembled %d bytes, want %d", len(resp), len(payload))
	}
	if string(resp) != string(payload) {
		t.Fatal("reassembled bytes differ from source")
	}
	if f.truncations() == 0 {
		t.Fatal("test did not exercise adaptive truncated-range recovery")
	}
	if got := f.maxConcurrentChunks(); got < 2 || got > execReadWorkers {
		t.Fatalf("concurrent chunk reads = %d, want 2..%d", got, execReadWorkers)
	}
}

func TestCallBytebotdAdaptiveMultiLevelTruncation(t *testing.T) {
	payload := make([]byte, execChunkBytes)
	for i := range payload {
		payload[i] = byte('a' + (i % 26))
	}
	f := &shellFixture{bytebotBody: payload, bytebotCode: 201, execOutputCap: 10_000}
	m := managerOverShell(t, f)

	resp, err := m.CallBytebotd(context.Background(), []byte(`{"action":"screenshot"}`))
	if err != nil {
		t.Fatalf("CallBytebotd: %v", err)
	}
	if !bytes.Equal(resp, payload) {
		t.Fatalf("adaptive reassembly recovered %d of %d bytes", len(resp), len(payload))
	}
	if f.truncations() < 3 {
		t.Fatalf("truncated reads = %d, want multi-level splitting", f.truncations())
	}
}

// TestCallBytebotdNoSession fails closed with a typed error when no desktop is
// live (never hangs the turn).
func TestCallBytebotdNoSession(t *testing.T) {
	f := &shellFixture{}
	srv := f.server(t)
	rw := NewRailway(RailwayConfig{Token: "tok", EnvironmentID: "env-1", Endpoint: srv.URL})
	m := New(rw, nil, Config{})
	_, err := m.CallBytebotd(context.Background(), []byte(`{"action":"cursor_position"}`))
	if err == nil || !strings.Contains(err.Error(), "no live desktop") {
		t.Fatalf("want ErrNoSession, got %v", err)
	}
}

// TestCallBytebotdErrorStatus surfaces a bytebotd 4xx as ErrBytebotd with the
// body preserved.
func TestCallBytebotdErrorStatus(t *testing.T) {
	f := &shellFixture{bytebotBody: []byte(`{"error":"Bad Request"}`), bytebotCode: 400}
	m := managerOverShell(t, f)
	resp, err := m.CallBytebotd(context.Background(), []byte(`{"action":"press_keys","keys":["ctrl"]}`))
	if err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("want ErrBytebotd, got %v", err)
	}
	if !strings.Contains(string(resp), "Bad Request") {
		t.Fatalf("body not preserved: %q", resp)
	}
}

// TestA11yDumpOverCarrier proves the AT-SPI dump rides the desktop-exec wrapper
// and parses.
func TestA11yDumpOverCarrier(t *testing.T) {
	f := &shellFixture{a11yOut: `{"apps":["xfce4-panel"],"nodes":[{"app":"xfce4-panel","label":"Applications","role":"push button","x":40,"y":945,"w":80,"h":24}]}`}
	m := managerOverShell(t, f)
	tree, err := m.A11yDump(context.Background())
	if err != nil {
		t.Fatalf("A11yDump: %v", err)
	}
	if len(tree.Nodes) != 1 || tree.Nodes[0].Label != "Applications" {
		t.Fatalf("tree = %+v", tree)
	}
}

// managerOverShellState is managerOverShell with a chosen session state and an
// event recorder, for the lifecycle-sensitive carrier tests.
func managerOverShellState(t *testing.T, f *shellFixture, st State) (*Manager, *eventRecorder) {
	t.Helper()
	srv := f.server(t)
	rw := NewRailway(RailwayConfig{Token: "tok", EnvironmentID: "env-1", Endpoint: srv.URL})
	rec := &eventRecorder{}
	m := New(rw, rec.emit, Config{pollGap: time.Millisecond, probeGap: time.Millisecond})
	m.mu.Lock()
	s := &session{id: "dojo-x", convID: "c1", state: st, sandboxID: "sb-1", image: DefaultImage, createdAt: m.now(), touchedAt: m.now()}
	m.sessions[s.id] = s
	m.byConv[s.convID] = s.id
	m.mu.Unlock()
	return m, rec
}

// testPNG returns a real encoded PNG (the Frame contract returns raw PNG
// bytes the server re-encodes).
func testPNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 4, 3))
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	return buf.Bytes()
}

// TestDesktopCallsWhileBootingReturnTyped: a provisioning session answers every
// desktop caller with ErrBooting — never a hang, never a transport error into a
// half-built sandbox.
func TestDesktopCallsWhileBootingReturnTyped(t *testing.T) {
	f := &shellFixture{}
	m, _ := managerOverShellState(t, f, StateProvisioning)
	if _, err := m.CallBytebotd(context.Background(), []byte(`{"action":"cursor_position"}`)); !errors.Is(err, ErrBooting) {
		t.Fatalf("CallBytebotd = %v, want ErrBooting", err)
	}
	if _, err := m.A11yDump(context.Background()); !errors.Is(err, ErrBooting) {
		t.Fatalf("A11yDump = %v, want ErrBooting", err)
	}
	if _, err := m.Frame(context.Background(), "c1"); !errors.Is(err, ErrBooting) {
		t.Fatalf("Frame = %v, want ErrBooting", err)
	}
}

// TestFrameIsPassive: the live-view frame returns the real PNG through the real
// carrier WITHOUT bumping the idle clock or activating the session — watching
// never keeps a sandbox alive.
func TestFrameIsPassive(t *testing.T) {
	shot := testPNG(t)
	wrapped := append([]byte(`{"image":"`), []byte(base64.StdEncoding.EncodeToString(shot))...)
	wrapped = append(wrapped, []byte(`"}`)...)
	f := &shellFixture{bytebotBody: wrapped, bytebotCode: 201}
	m, rec := managerOverShellState(t, f, StateReady)

	base := time.Now()
	m.now = func() time.Time { return base.Add(30 * time.Minute) }
	got, err := m.Frame(context.Background(), "c1")
	if err != nil {
		t.Fatalf("Frame: %v", err)
	}
	if string(got) != string(shot) {
		t.Fatalf("frame bytes differ: %d vs %d", len(got), len(shot))
	}
	snap, ok := m.SessionForConv("c1")
	if !ok {
		t.Fatal("session vanished")
	}
	if snap.State != StateReady {
		t.Fatalf("Frame changed state to %s — a passive view must not activate", snap.State)
	}
	if snap.TouchedAt.After(base.Add(time.Minute)) {
		t.Fatalf("Frame bumped the idle clock to %v — watching must not keep the sandbox alive", snap.TouchedAt)
	}
	if len(rec.types()) != 0 {
		t.Fatalf("Frame emitted events: %v", rec.types())
	}
}

// TestFrameNoSession fails closed with a typed error.
func TestFrameNoSession(t *testing.T) {
	f := &shellFixture{}
	srv := f.server(t)
	rw := NewRailway(RailwayConfig{Token: "tok", EnvironmentID: "env-1", Endpoint: srv.URL})
	m := New(rw, nil, Config{})
	if _, err := m.Frame(context.Background(), "nope"); !errors.Is(err, ErrNoSession) {
		t.Fatalf("Frame = %v, want ErrNoSession", err)
	}
}

// TestFirstAgentActionActivatesReadySession: the agent's first desktop action
// moves ready -> active and emits dojo.active (the client's "Neo is driving").
func TestFirstAgentActionActivatesReadySession(t *testing.T) {
	f := &shellFixture{bytebotBody: []byte(`{"x":1,"y":2}`), bytebotCode: 201}
	m, rec := managerOverShellState(t, f, StateReady)
	if _, err := m.CallBytebotd(context.Background(), []byte(`{"action":"cursor_position"}`)); err != nil {
		t.Fatalf("CallBytebotd: %v", err)
	}
	snap, _ := m.SessionForConv("c1")
	if snap.State != StateActive {
		t.Fatalf("state = %s, want active", snap.State)
	}
	types := rec.types()
	if len(types) != 1 || types[0] != EvtActive {
		t.Fatalf("events = %v, want [dojo.active]", types)
	}
}

// TestControlLeaseProperty is the wave-3 lease property on real components
// (req 4.2/4.3/6.2), driven through the REAL Manager + REAL exec-curl carrier:
// (1) while the human holds the lease every agent desktop call is rejected
// with the typed takeover_active result and the human's input passes through;
// (2) handback is an explicit delivered event; (3) after handback the agent's
// mutating actions are rejected until a fresh observation lands — it never
// acts on the pre-takeover world model.
func TestControlLeaseProperty(t *testing.T) {
	f := &shellFixture{bytebotBody: []byte(`{"ok":true}`), bytebotCode: 201}
	m, rec := managerOverShellState(t, f, StateActive)
	ctx := context.Background()

	// Take the lease.
	if _, err := m.TakeoverConv("c1"); err != nil {
		t.Fatalf("TakeoverConv: %v", err)
	}
	if types := rec.types(); len(types) != 1 || types[0] != EvtTakeover {
		t.Fatalf("events after takeover = %v", types)
	}

	// (1) Agent calls rejected while held — actuation, grounding, and a11y.
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"click_mouse","coordinates":{"x":1,"y":1},"clickCount":1}`)); !errors.Is(err, ErrTakeoverActive) {
		t.Fatalf("agent click during takeover = %v, want ErrTakeoverActive", err)
	}
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"screenshot"}`)); !errors.Is(err, ErrTakeoverActive) {
		t.Fatalf("agent screenshot during takeover = %v, want ErrTakeoverActive", err)
	}
	if _, err := m.A11yDump(ctx); !errors.Is(err, ErrTakeoverActive) {
		t.Fatalf("agent a11y during takeover = %v, want ErrTakeoverActive", err)
	}
	// The HUMAN's input passes through the real carrier while held…
	if _, err := m.TakeoverInput(ctx, "c1", []byte(`{"action":"click_mouse","coordinates":{"x":5,"y":5},"clickCount":1}`)); err != nil {
		t.Fatalf("TakeoverInput during takeover: %v", err)
	}
	// …and the passive live view still works (the human watches themselves).
	shot := testPNG(t)
	wrapped := append([]byte(`{"image":"`), []byte(base64.StdEncoding.EncodeToString(shot))...)
	wrapped = append(wrapped, []byte(`"}`)...)
	f.bytebotBody = wrapped
	if _, err := m.Frame(ctx, "c1"); err != nil {
		t.Fatalf("Frame during takeover: %v", err)
	}
	f.bytebotBody = []byte(`{"ok":true}`)

	// (2) Handback is an explicit delivered event.
	if _, err := m.HandbackConv("c1"); err != nil {
		t.Fatalf("HandbackConv: %v", err)
	}
	if types := rec.types(); len(types) != 2 || types[1] != EvtHandback {
		t.Fatalf("events after handback = %v, want takeover, handback", types)
	}

	// Input passthrough dies with the lease.
	if _, err := m.TakeoverInput(ctx, "c1", []byte(`{"action":"click_mouse","coordinates":{"x":5,"y":5},"clickCount":1}`)); !errors.Is(err, ErrNoTakeover) {
		t.Fatalf("TakeoverInput after handback = %v, want ErrNoTakeover", err)
	}

	// (3) Re-observation forced: a mutating action is rejected until a fresh
	// observation lands; the observation clears the gate.
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"click_mouse","coordinates":{"x":1,"y":1},"clickCount":1}`)); !errors.Is(err, ErrReobserveRequired) {
		t.Fatalf("post-handback click = %v, want ErrReobserveRequired", err)
	}
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"type_text","text":"hi"}`)); !errors.Is(err, ErrReobserveRequired) {
		t.Fatalf("post-handback type = %v, want ErrReobserveRequired", err)
	}
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"screenshot"}`)); err != nil {
		t.Fatalf("post-handback screenshot (the re-observation): %v", err)
	}
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"click_mouse","coordinates":{"x":1,"y":1},"clickCount":1}`)); err != nil {
		t.Fatalf("click after re-observation: %v", err)
	}
}

// TestHandbackReobserveClearedByA11y: the deterministic middle rung (a landed
// a11y dump) also satisfies the re-observation contract.
func TestHandbackReobserveClearedByA11y(t *testing.T) {
	f := &shellFixture{
		bytebotBody: []byte(`{"ok":true}`),
		bytebotCode: 201,
		a11yOut:     `{"apps":["Thunar"],"nodes":[{"app":"Thunar","label":"File","role":"menu","x":10,"y":10,"w":40,"h":20}]}`,
	}
	m, _ := managerOverShellState(t, f, StateActive)
	ctx := context.Background()
	if _, err := m.TakeoverConv("c1"); err != nil {
		t.Fatalf("TakeoverConv: %v", err)
	}
	if _, err := m.HandbackConv("c1"); err != nil {
		t.Fatalf("HandbackConv: %v", err)
	}
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"paste_text","text":"x"}`)); !errors.Is(err, ErrReobserveRequired) {
		t.Fatalf("post-handback paste = %v, want ErrReobserveRequired", err)
	}
	if _, err := m.A11yDump(ctx); err != nil {
		t.Fatalf("A11yDump as re-observation: %v", err)
	}
	if _, err := m.CallBytebotd(ctx, []byte(`{"action":"paste_text","text":"x"}`)); err != nil {
		t.Fatalf("paste after a11y re-observation: %v", err)
	}
}

// TestTakeoverFromReadyActivatesFirst: grabbing a booted-but-idle desktop
// walks the closed machine edges (ready -> active -> takeover) with both
// events delivered.
func TestTakeoverFromReadyActivatesFirst(t *testing.T) {
	f := &shellFixture{}
	m, rec := managerOverShellState(t, f, StateReady)
	snap, err := m.TakeoverConv("c1")
	if err != nil {
		t.Fatalf("TakeoverConv from ready: %v", err)
	}
	if snap.State != StateTakeover {
		t.Fatalf("state = %s, want takeover", snap.State)
	}
	types := rec.types()
	if len(types) != 2 || types[0] != EvtActive || types[1] != EvtTakeover {
		t.Fatalf("events = %v, want [dojo.active dojo.takeover]", types)
	}
}

// TestLiveDojoBridge is the env-gated live round-trip against a REAL Railway
// sandbox: provision, drive real bytebotd (cursor_position + screenshot), dump
// the real a11y tree, then destroy — no fakes anywhere. It leak-checks nothing
// is left running.
func TestLiveDojoBridge(t *testing.T) {
	tok := os.Getenv("RAILWAY_API_TOKEN")
	envID := os.Getenv("RAILWAY_ENVIRONMENT_ID")
	if tok == "" || envID == "" {
		t.Skip("set RAILWAY_API_TOKEN + RAILWAY_ENVIRONMENT_ID to run the live dojo bridge round-trip")
	}
	rw := NewRailway(RailwayConfig{Token: tok, EnvironmentID: envID})
	m := New(rw, nil, Config{})
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Minute)
	defer cancel()

	s, err := m.Provision(ctx, Request{ConvID: "live-bridge"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	t.Cleanup(func() { _ = m.Teardown(context.Background(), s.ID, "test_done") })

	// cursor_position — the cheap readiness/actuation proof.
	resp, err := m.CallBytebotd(ctx, []byte(`{"action":"cursor_position"}`))
	if err != nil {
		t.Fatalf("cursor_position: %v", err)
	}
	var pos struct{ X, Y int }
	if err := json.Unmarshal(resp, &pos); err != nil {
		t.Fatalf("cursor_position not JSON: %v (%s)", err, resp)
	}

	// screenshot — proves the chunked large-response reassembly on real bytes.
	shot, err := m.CallBytebotd(ctx, []byte(`{"action":"screenshot"}`))
	if err != nil {
		t.Fatalf("screenshot: %v", err)
	}
	var wrap struct{ Image string }
	if err := json.Unmarshal(shot, &wrap); err != nil || wrap.Image == "" {
		t.Fatalf("screenshot not an image: %v", err)
	}
	png, err := base64.StdEncoding.DecodeString(wrap.Image)
	if err != nil {
		t.Fatalf("screenshot base64: %v", err)
	}
	if wpx, hpx, derr := PNGDimensions(png); derr != nil {
		t.Fatalf("screenshot not a PNG: %v", derr)
	} else {
		t.Logf("live screenshot %dx%d (%d bytes)", wpx, hpx, len(png))
	}

	// AT-SPI dump — proves the deterministic middle rung against the real XFCE.
	tree, err := m.A11yDump(ctx)
	if err != nil {
		t.Fatalf("A11yDump: %v", err)
	}
	t.Logf("live a11y: %d apps, %d interactive nodes", len(tree.Apps), len(tree.Nodes))
}
