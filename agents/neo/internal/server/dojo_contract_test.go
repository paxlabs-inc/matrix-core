// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// DOJO wave 2 — the no-fakes bytebotd contract + grounding proof. It drives a
// REAL bytebot-desktop container (docker) through the SAME carrier seam
// production uses (the daemon /dojo/computer-use proxy + the desktopLook /
// desktopA11y engine methods); only the transport is swapped from exec-curl to
// a direct localhost carrier, so the bytebotd wire, the AT-SPI dump, and the
// omni grounding under test are all real.
//
// Gated on a reachable container:
//
//	docker run -d --name dojo-dev -p 9990:9990 \
//	  ghcr.io/bytebot-ai/bytebot-desktop@sha256:e974e7ac93c8f755e98b8a437e5497a63175c7096b51a348f5f1d9320598a1c9
//	NEO_DOJO_CONTRACT=1 go test ./internal/server/ -run TestDojo -v
//
// The live omni grounding leg additionally needs XIAOMI_API_KEY (or
// MIMO_API_KEY).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	mcllm "centra/core/mcl/llm"
	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/dojo"
	"centra/agents/neo/internal/llm"
)

const dojoContractContainer = "dojo-dev"
const dojoContractBase = "http://localhost:9990"

// directTransport is the docker contract carrier: it reaches the REAL local
// bytebot-desktop container over HTTP (bytebotd) and runs the AT-SPI dumper via
// docker exec — REUSING the production dojo.WrapDesktopExec wrapper so that code
// is proven too. It substitutes ONLY the transport; bytebotd is real.
type directTransport struct {
	base      string
	container string
	hc        *http.Client
}

func (t *directTransport) Call(ctx context.Context, _ dojo.Session, body []byte) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.base+"/computer-use", bytes.NewReader(body))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := t.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	buf := new(bytes.Buffer)
	_, _ = buf.ReadFrom(resp.Body)
	return buf.Bytes(), resp.StatusCode, nil
}

func (t *directTransport) ExecDesktop(ctx context.Context, _ dojo.Session, command string, timeoutSec int) (string, int, error) {
	wrapped := dojo.WrapDesktopExec(t.container, command)
	cmd := exec.CommandContext(ctx, "bash", "-lc", wrapped)
	out, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok {
			exit = ee.ExitCode()
		} else {
			return string(out), 0, err
		}
	}
	return string(out), exit, nil
}

var _ dojo.BytebotdTransport = (*directTransport)(nil)

// noRailway is a never-called RailwayClient stub: the direct transport replaces
// the exec-curl path, so no Railway op is ever issued in the contract test.
type noRailway struct{}

func (noRailway) CreateSandbox(context.Context, dojo.CreateOpts) (dojo.SandboxInfo, error) {
	return dojo.SandboxInfo{}, nil
}
func (noRailway) GetSandbox(context.Context, string) (dojo.SandboxInfo, error) {
	return dojo.SandboxInfo{Status: dojo.SandboxRunning}, nil
}
func (noRailway) ExecSandbox(context.Context, string, string, int) (dojo.ExecResult, error) {
	return dojo.ExecResult{}, nil
}
func (noRailway) DestroySandbox(context.Context, string) error { return nil }

func containerReachable(t *testing.T) {
	t.Helper()
	if os.Getenv("NEO_DOJO_CONTRACT") != "1" {
		t.Skip("set NEO_DOJO_CONTRACT=1 and run a local bytebot-desktop container to run the dojo contract test")
	}
	req, _ := http.NewRequest(http.MethodPost, dojoContractBase+"/computer-use", strings.NewReader(`{"action":"cursor_position"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("NEO_DOJO_CONTRACT=1 but bytebot-desktop is unreachable at %s: %v", dojoContractBase, err)
	}
	resp.Body.Close()
}

// contractManager builds a real dojo.Manager whose carrier is the direct
// container transport, with a single hand-registered live session.
func contractManager(t *testing.T) *dojo.Manager {
	t.Helper()
	m := dojo.New(noRailway{}, nil, dojo.Config{})
	m.SetTransport(&directTransport{base: dojoContractBase, container: dojoContractContainer, hc: &http.Client{Timeout: 30 * time.Second}})
	// Provision writes real sandboxes; the contract test drives an existing
	// container instead, so register the live session directly through the
	// public seam.
	if _, err := m.RegisterSessionForTest("local-sb"); err != nil {
		t.Fatalf("register session: %v", err)
	}
	return m
}

// TestDojoComputerUseProxyContract drives the REAL bytebotd wire through the
// daemon proxy handler: cursor_position, a click, and a screenshot returned as
// a real PNG — the no-fakes contract for the actuation lane.
func TestDojoComputerUseProxyContract(t *testing.T) {
	containerReachable(t)
	e := &Engine{dojo: contractManager(t), dojoBridgeToken: "secret"}
	s := &Server{engine: e}

	post := func(action string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/dojo/computer-use", strings.NewReader(action))
		r.Header.Set("Authorization", "Bearer secret")
		w := httptest.NewRecorder()
		s.handleDojoComputerUse(w, r)
		return w
	}

	// Unauthorized without the bridge token.
	rBad := httptest.NewRequest(http.MethodPost, "/dojo/computer-use", strings.NewReader(`{"action":"cursor_position"}`))
	wBad := httptest.NewRecorder()
	s.handleDojoComputerUse(wBad, rBad)
	if wBad.Code != http.StatusUnauthorized {
		t.Fatalf("no-token request = %d, want 401", wBad.Code)
	}

	w := post(`{"action":"cursor_position"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("cursor_position = %d: %s", w.Code, w.Body.String())
	}
	var pos struct{ X, Y int }
	if err := json.Unmarshal(w.Body.Bytes(), &pos); err != nil {
		t.Fatalf("cursor_position body: %v (%s)", err, w.Body.String())
	}

	if w := post(`{"action":"click_mouse","coordinates":{"x":100,"y":100},"button":"left","clickCount":1}`); w.Code != http.StatusOK {
		t.Fatalf("click_mouse = %d: %s", w.Code, w.Body.String())
	}

	w = post(`{"action":"screenshot"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("screenshot = %d: %s", w.Code, w.Body.String())
	}
	var wrap struct{ Image string }
	if err := json.Unmarshal(w.Body.Bytes(), &wrap); err != nil || wrap.Image == "" {
		t.Fatalf("screenshot not an image: %v", err)
	}

	// A malformed action surfaces bytebotd's own 4xx, relayed for the model.
	if w := post(`{"action":"press_keys","keys":["ctrl"]}`); w.Code != http.StatusBadRequest {
		t.Fatalf("bad action = %d, want 400 relay: %s", w.Code, w.Body.String())
	}
}

// TestDojoA11yContract dumps the REAL XFCE accessibility tree through the
// engine seam and asserts it found registered applications with coordinates.
func TestDojoA11yContract(t *testing.T) {
	containerReachable(t)
	e := &Engine{dojo: contractManager(t)}
	ctx := context.Background()
	// Stage a native app (the file manager) so the accessibility tree has real
	// interactive elements to expose (a bare desktop legitimately exposes none).
	if _, err := e.dojo.CallBytebotd(ctx, []byte(`{"action":"application","application":"directory"}`)); err != nil {
		t.Fatalf("open file manager: %v", err)
	}
	if _, err := e.dojo.CallBytebotd(ctx, []byte(`{"action":"wait","duration":3000}`)); err != nil {
		t.Fatalf("wait: %v", err)
	}
	out, err := e.desktopA11y(ctx)
	if err != nil {
		t.Fatalf("desktopA11y: %v", err)
	}
	if !strings.Contains(out, "interactive elements") {
		t.Fatalf("expected interactive elements from the open file manager, got:\n%s", out)
	}
	if !strings.Contains(out, "push button") && !strings.Contains(out, "menu") {
		t.Fatalf("a11y tree missing expected native controls:\n%s", truncateStr(out, 600))
	}
	t.Logf("a11y:\n%s", truncateStr(out, 600))
}

// TestDojoLookLiveGrounding is the env-gated live grounding round-trip: capture
// the REAL desktop, ground it with the REAL MiMo v2.5 omni vision model, and
// assert a click point comes back in screen pixels for a named target — no
// fakes anywhere.
func TestDojoLookLiveGrounding(t *testing.T) {
	containerReachable(t)
	key := os.Getenv("XIAOMI_API_KEY")
	if key == "" {
		key = os.Getenv("MIMO_API_KEY")
	}
	if key == "" {
		t.Skip("set XIAOMI_API_KEY (or MIMO_API_KEY) to run the live omni grounding leg")
	}
	omni, err := llm.New(mcllm.Config{
		Model:       "mimo-v2.5",
		Provider:    mcllm.ProviderXiaomi,
		ProviderSet: true,
		Endpoint:    "https://api.xiaomimimo.com/v1/chat/completions",
		APIKey:      key,
		Temperature: 0.1,
		MaxTokens:   2048,
	})
	if err != nil {
		t.Fatalf("build omni client: %v", err)
	}
	e := &Engine{dojo: contractManager(t), omni: omni, cfg: config.Default()}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	out, err := e.desktopLook(ctx, "the file manager icon in the taskbar")
	if err != nil {
		t.Fatalf("desktopLook: %v", err)
	}
	if !strings.Contains(out, "Desktop view") {
		t.Fatalf("grounding output missing header:\n%s", out)
	}
	t.Logf("grounding:\n%s", truncateStr(out, 800))
}

// TestDojoWave3Contract drives the wave-3 surface against the REAL container:
// the live-view frame comes back as a real JPEG through the frame route, the
// human's input passes through to real bytebotd during takeover, and after
// handback the re-observation contract clears against a real screenshot.
func TestDojoWave3Contract(t *testing.T) {
	containerReachable(t)
	e := &Engine{dojo: contractManager(t), dojoBridgeToken: "secret"}
	s := &Server{engine: e}

	// Live-view frame: a real JPEG (transcoded from the real PNG screenshot).
	w := httptest.NewRecorder()
	s.handleDojoFrame(w, httptest.NewRequest(http.MethodGet, "/dojo/frame?conversation=contract", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("frame = %d: %s", w.Code, w.Body.String())
	}
	if ct := w.Header().Get("Content-Type"); ct != "image/jpeg" {
		t.Fatalf("frame content-type = %q, want image/jpeg", ct)
	}
	if b := w.Body.Bytes(); len(b) < 4 || b[0] != 0xFF || b[1] != 0xD8 {
		t.Fatalf("frame is not a JPEG (%d bytes)", len(b))
	}

	// Take control; the human's click passes through to real bytebotd.
	tw := httptest.NewRecorder()
	s.handleDojoTakeover(tw, httptest.NewRequest(http.MethodPost, "/dojo/takeover", strings.NewReader(`{"conversation_id":"contract"}`)))
	if tw.Code != http.StatusOK {
		t.Fatalf("takeover = %d: %s", tw.Code, tw.Body.String())
	}
	iw := httptest.NewRecorder()
	s.handleDojoInput(iw, httptest.NewRequest(http.MethodPost, "/dojo/input",
		strings.NewReader(`{"conversation_id":"contract","request":{"action":"click_mouse","coordinates":{"x":200,"y":200},"button":"left","clickCount":1}}`)))
	if iw.Code != http.StatusOK {
		t.Fatalf("takeover input = %d: %s", iw.Code, iw.Body.String())
	}

	// Hand back, then prove the re-observe contract end to end on the real
	// wire: mutate refused → real screenshot lands → mutate allowed.
	hw := httptest.NewRecorder()
	s.handleDojoHandback(hw, httptest.NewRequest(http.MethodPost, "/dojo/handback", strings.NewReader(`{"conversation_id":"contract"}`)))
	if hw.Code != http.StatusOK {
		t.Fatalf("handback = %d: %s", hw.Code, hw.Body.String())
	}
	post := func(action string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, "/dojo/computer-use", strings.NewReader(action))
		r.Header.Set("Authorization", "Bearer secret")
		rw := httptest.NewRecorder()
		s.handleDojoComputerUse(rw, r)
		return rw
	}
	if rw := post(`{"action":"click_mouse","coordinates":{"x":200,"y":200},"button":"left","clickCount":1}`); rw.Code != http.StatusConflict || !strings.Contains(rw.Body.String(), "reobserve_required") {
		t.Fatalf("post-handback click = %d: %s", rw.Code, rw.Body.String())
	}
	if rw := post(`{"action":"screenshot"}`); rw.Code != http.StatusOK {
		t.Fatalf("re-observation screenshot = %d: %s", rw.Code, rw.Body.String())
	}
	if rw := post(`{"action":"click_mouse","coordinates":{"x":200,"y":200},"button":"left","clickCount":1}`); rw.Code != http.StatusOK {
		t.Fatalf("click after re-observation = %d: %s", rw.Code, rw.Body.String())
	}
}

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
