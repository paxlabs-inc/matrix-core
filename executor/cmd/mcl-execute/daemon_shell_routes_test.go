// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// TestForgeShell_Disabled404 verifies the route 404s when shellCfg is nil.
func TestForgeShell_Disabled404(t *testing.T) {
	d := &daemonState{shellCfg: nil}
	req := httptest.NewRequest(http.MethodGet, "/shell/exec", nil)
	rec := httptest.NewRecorder()
	d.handleForgeShellExec(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestForgeShell_AuthRequired refuses unauthenticated requests when an
// auth token is configured.
func TestForgeShell_AuthRequired(t *testing.T) {
	d := &daemonState{
		shellCfg:  DefaultForgeShellConfig(),
		authToken: "secret",
	}
	req := httptest.NewRequest(http.MethodGet, "/shell/exec", nil)
	rec := httptest.NewRecorder()
	d.handleForgeShellExec(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", rec.Code)
	}
}

// TestForgeShell_SubprotocolAuth accepts the Sec-WebSocket-Protocol
// "auth.bearer.<token>" path that browsers must use because they
// can't set Authorization headers on `new WebSocket()`.
//
// Smoke-only: we don't complete the WS upgrade here (that requires a
// PTY-capable runtime). Just assert that authorizeShellRequest returns
// true with the subprotocol token set.
func TestForgeShell_SubprotocolAuth(t *testing.T) {
	d := &daemonState{
		shellCfg:  DefaultForgeShellConfig(),
		authToken: "abc123",
	}
	req := httptest.NewRequest(http.MethodGet, "/shell/exec", nil)
	req.Header.Set("Sec-WebSocket-Protocol", "matrix-shell.v1, auth.bearer.abc123")
	rec := httptest.NewRecorder()
	if !d.authorizeShellRequest(rec, req) {
		t.Errorf("subprotocol bearer must authenticate; got 401 (rec.Code=%d)", rec.Code)
	}
}

// TestForgeShell_HeaderAuth accepts the Authorization: Bearer path.
func TestForgeShell_HeaderAuth(t *testing.T) {
	d := &daemonState{
		shellCfg:  DefaultForgeShellConfig(),
		authToken: "abc123",
	}
	req := httptest.NewRequest(http.MethodGet, "/shell/exec", nil)
	req.Header.Set("Authorization", "Bearer abc123")
	rec := httptest.NewRecorder()
	if !d.authorizeShellRequest(rec, req) {
		t.Errorf("Authorization header must authenticate")
	}
}

// TestForgeShell_AuthDisabled bypasses auth when authToken is empty
// (local-dev posture).
func TestForgeShell_AuthDisabled(t *testing.T) {
	d := &daemonState{shellCfg: DefaultForgeShellConfig()} // authToken == ""
	req := httptest.NewRequest(http.MethodGet, "/shell/exec", nil)
	rec := httptest.NewRecorder()
	if !d.authorizeShellRequest(rec, req) {
		t.Errorf("empty authToken must accept all requests")
	}
}

// TestResolveShellCwd_Default falls back to AllowRoots[0].
func TestResolveShellCwd_Default(t *testing.T) {
	cfg := &ForgeShellConfig{AllowRoots: []string{"/tmp"}}
	cwd, err := resolveShellCwd(cfg, "")
	if err != nil {
		t.Fatalf("resolveShellCwd: %v", err)
	}
	if cwd != "/tmp" {
		t.Errorf("cwd = %q, want /tmp", cwd)
	}
}

// TestResolveShellCwd_Outside refuses cwd not under AllowRoots.
func TestResolveShellCwd_Outside(t *testing.T) {
	cfg := &ForgeShellConfig{AllowRoots: []string{"/tmp"}}
	_, err := resolveShellCwd(cfg, "/etc")
	if err == nil {
		t.Errorf("cwd outside AllowRoots must error")
	}
}

// TestResolveShellCwd_Subdir accepts a path under AllowRoots.
func TestResolveShellCwd_Subdir(t *testing.T) {
	cfg := &ForgeShellConfig{AllowRoots: []string{"/tmp"}}
	dir := t.TempDir() // under /tmp on linux
	cwd, err := resolveShellCwd(cfg, dir)
	if err != nil {
		// Some test runners use a non-/tmp temp dir; skip in that case.
		if !strings.HasPrefix(dir, "/tmp") {
			t.Skipf("temp dir %s not under /tmp; skipping", dir)
			return
		}
		t.Fatalf("resolveShellCwd: %v", err)
	}
	if cwd == "" {
		t.Errorf("cwd empty")
	}
}

// TestChooseShell_Precedence: cfg.Shell > MATRIX_FORGE_SHELL > SHELL > /bin/bash.
func TestChooseShell_Precedence(t *testing.T) {
	cfg := &ForgeShellConfig{Shell: "/usr/bin/zsh"}
	if got := chooseShell(cfg); got != "/usr/bin/zsh" {
		t.Errorf("explicit cfg.Shell = %q, want /usr/bin/zsh", got)
	}

	cfg2 := &ForgeShellConfig{}
	t.Setenv("MATRIX_FORGE_SHELL", "/usr/bin/dash")
	if got := chooseShell(cfg2); got != "/usr/bin/dash" {
		t.Errorf("MATRIX_FORGE_SHELL = %q, want /usr/bin/dash", got)
	}

	t.Setenv("MATRIX_FORGE_SHELL", "")
	t.Setenv("SHELL", "/usr/bin/fish")
	if got := chooseShell(cfg2); got != "/usr/bin/fish" {
		t.Errorf("SHELL = %q, want /usr/bin/fish", got)
	}

	t.Setenv("SHELL", "")
	t.Setenv("MATRIX_FORGE_SHELL", "")
	if got := chooseShell(cfg2); got != "/bin/bash" {
		t.Errorf("default = %q, want /bin/bash", got)
	}
}

// TestShellEnv_StripsToken redacts MATRIX_DAEMON_TOKEN from the child
// env so it doesn't land in shell history / process listings.
func TestShellEnv_StripsToken(t *testing.T) {
	t.Setenv("MATRIX_DAEMON_TOKEN", "secret-do-not-leak")
	t.Setenv("MATRIX_FOO", "yes")
	env := shellEnv("/tmp")
	for _, e := range env {
		if strings.HasPrefix(e, "MATRIX_DAEMON_TOKEN=") {
			t.Errorf("MATRIX_DAEMON_TOKEN must be stripped from child env; got %s", e)
		}
	}
	if !envHas(env, "MATRIX_FOO") {
		t.Errorf("MATRIX_FOO should have been propagated")
	}
	if !envHas(env, "PWD") || !envHas(env, "TERM") {
		t.Errorf("baseline PWD/TERM missing from child env: %v", env)
	}
}

// TestForgeShell_E2E spawns a real PTY, runs `echo MATRIX_PHASE3`,
// expects the output back, and verifies the exit frame on EOF.
//
// Skipped automatically if /bin/sh isn't available (extremely unlikely
// on linux but defensive against minimal containers).
func TestForgeShell_E2E(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("/bin/sh missing: %v", err)
	}
	d := &daemonState{
		shellCfg: &ForgeShellConfig{
			AllowRoots:          []string{os.TempDir()},
			Shell:               "/bin/sh",
			IdleTimeout:         10 * time.Second,
			MaxFrameBytes:       64 * 1024,
			MaxOutputFrameBytes: 4096,
			HandshakeTimeout:    5 * time.Second,
		},
	}

	srv := httptest.NewServer(http.HandlerFunc(d.handleForgeShellExec))
	defer srv.Close()

	wsURL, _ := url.Parse(srv.URL)
	wsURL.Scheme = "ws"
	wsURL.Path = "/shell/exec"

	dialer := websocket.DefaultDialer
	dialer.Subprotocols = []string{"matrix-shell.v1"}
	dialer.HandshakeTimeout = 5 * time.Second

	conn, _, err := dialer.Dial(wsURL.String(), nil)
	if err != nil {
		t.Fatalf("ws dial: %v", err)
	}
	defer conn.Close()

	// Read pump — runs until the connection is closed or the test
	// deadline (10s overall) is exceeded.
	var (
		mu       sync.Mutex
		output   bytes.Buffer
		exitMsg  shellExitMessage
		gotExit  bool
		readDone = make(chan struct{})
	)
	var readErr error
	go func() {
		defer close(readDone)
		for {
			_ = conn.SetReadDeadline(time.Now().Add(8 * time.Second))
			mt, payload, err := conn.ReadMessage()
			if err != nil {
				readErr = err
				return
			}
			mu.Lock()
			switch mt {
			case websocket.BinaryMessage:
				output.Write(payload)
			case websocket.TextMessage:
				_ = json.Unmarshal(payload, &exitMsg)
				if exitMsg.Type == "exit" {
					gotExit = true
				}
			}
			mu.Unlock()
		}
	}()

	// Give the upgrader a moment to fully spawn the PTY before we
	// flood it with stdin. PTY race: if we write before pty.Start
	// returns, the bytes go into the WS buffer and are then forwarded
	// once the spawn completes — works in practice but can race the
	// initial prompt on some kernels.
	time.Sleep(100 * time.Millisecond)

	if err := conn.WriteMessage(websocket.BinaryMessage,
		[]byte("echo MATRIX_PHASE3_OK\nexit 0\n")); err != nil {
		t.Fatalf("write stdin: %v", err)
	}

	// Wait for the OK string to appear OR the read goroutine to exit.
	deadline := time.After(8 * time.Second)
	for {
		mu.Lock()
		ok := strings.Contains(output.String(), "MATRIX_PHASE3_OK") && gotExit
		mu.Unlock()
		if ok {
			break
		}
		select {
		case <-readDone:
			// Connection closed; do final assertion below.
			goto check
		case <-deadline:
			goto check
		case <-time.After(50 * time.Millisecond):
		}
	}
check:
	mu.Lock()
	defer mu.Unlock()
	if !strings.Contains(output.String(), "MATRIX_PHASE3_OK") {
		t.Errorf("expected MATRIX_PHASE3_OK in output; got %q (readErr=%v subprotocol=%q)",
			output.String(), readErr, conn.Subprotocol())
	}
	if !gotExit {
		t.Errorf("expected exit frame; got none. output=%q readErr=%v",
			output.String(), readErr)
	}
}

// TestForgeShell_ResizeFrame parses a resize control message correctly.
// We don't actually verify the PTY winsize change here (would require
// peeking at the ptmx fd) — instead we verify the JSON parse path.
func TestForgeShell_ResizeFrame(t *testing.T) {
	body, _ := json.Marshal(shellControlMessage{Type: "resize", Cols: 120, Rows: 40})
	var got shellControlMessage
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Type != "resize" || got.Cols != 120 || got.Rows != 40 {
		t.Errorf("decoded = %+v", got)
	}
}

// TestForgeShell_IdleTimer fires when nothing bumps it.
func TestForgeShell_IdleTimer(t *testing.T) {
	idle := newIdleTimer(50 * time.Millisecond)
	defer idle.stop()

	select {
	case <-idle.fired():
		// expected
	case <-time.After(200 * time.Millisecond):
		t.Errorf("idle timer didn't fire within budget")
	}
}

// TestForgeShell_IdleTimer_Bump resets the timer.
func TestForgeShell_IdleTimer_Bump(t *testing.T) {
	idle := newIdleTimer(80 * time.Millisecond)
	defer idle.stop()

	// Bump before expiry.
	time.Sleep(40 * time.Millisecond)
	idle.bump()
	// Wait less than the FULL window past the bump.
	select {
	case <-idle.fired():
		t.Errorf("idle timer fired before full window after bump")
	case <-time.After(50 * time.Millisecond):
		// good — still within the post-bump window
	}
	// Now wait for it to actually fire.
	select {
	case <-idle.fired():
	case <-time.After(120 * time.Millisecond):
		t.Errorf("idle timer didn't fire after extended window")
	}
}

// silence unused imports if Go version optimisations remove them.
var _ = context.Background

// Copyright © 2026 Paxlabs Inc. All rights reserved.
