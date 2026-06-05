// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_shell_routes.go — Forge PTY shell over WebSocket.
//
// One route:
//
//	WS /shell/exec[?cwd=&shell=]
//
// Auth: bearer via either (a) Authorization: Bearer header (curl,
// non-browser clients), OR (b) Sec-WebSocket-Protocol negotiation —
// the client offers ["matrix-shell.v1", "auth.bearer.<token>"], the
// daemon validates the second protocol and accepts the handshake with
// "matrix-shell.v1" as the chosen subprotocol. Browsers can't set
// arbitrary headers on `new WebSocket()` so the subprotocol is the
// load-bearing path for the SPA.
//
// Posture:
//
//   • Spawned PTY runs `bash -i` (or $SHELL when overridden via env
//     MATRIX_FORGE_SHELL) inside cwd. The cwd defaults to the policy
//     AllowRoot[0] (i.e. /root/matrix); the SPA may pin a subdir via
//     ?cwd=, but the resolved path MUST sit under an AllowRoot.
//   • Stdin frames (binary OR text) are written byte-for-byte to the
//     PTY master. Text frames may carry control JSON like
//     {"type":"resize","cols":N,"rows":N} or {"type":"signal","name":
//     "INT"}; the daemon parses and acts. Anything that doesn't parse
//     as a recognised control message is treated as raw stdin (so a
//     legacy client sending JSON-as-stdin still works).
//   • Stdout/stderr are emitted as binary frames (xterm.js writes raw
//     bytes via term.write()). PTY runs in cooked mode by default;
//     bash will TTY-handle line discipline.
//   • Idle timeout: if no input arrives for IdleTimeout (default 30
//     min), the daemon closes the WS + SIGKILLs the child.
//   • Disconnect: SIGTERM the child on graceful close, SIGKILL on
//     hard abort. Final frame {"type":"exit","code":N} sent before
//     close when feasible.
//   • Single-flight is NOT enforced — terminals are concurrent by
//     design (a user might want a build running in one tab and `git
//     status` in another). Each session is isolated.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
	"github.com/gorilla/websocket"
)

// shellDebug prints internal lifecycle messages to stderr when the
// MATRIX_FORGE_SHELL_DEBUG env var is set. Cheap path so production
// builds stay silent without code changes.
func shellDebug(format string, args ...interface{}) {
	if os.Getenv("MATRIX_FORGE_SHELL_DEBUG") == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "[forge-shell] "+format+"\n", args...)
}

// ForgeShellConfig drives the WS /shell/exec route.
type ForgeShellConfig struct {
	// AllowRoots: cwd MUST resolve under one of these. Reuses the
	// ForgeFSPolicy.AllowRoots posture so a shell session can't start
	// outside the agent's reach.
	AllowRoots []string

	// Shell is the binary to exec inside the PTY. Empty falls back to
	// $MATRIX_FORGE_SHELL env, then $SHELL, then /bin/bash.
	Shell string

	// IdleTimeout closes the session when no input arrives for this
	// duration. Default 30 min — long enough for builds + tests, short
	// enough that a forgotten tab doesn't hold a PTY forever.
	IdleTimeout time.Duration

	// MaxFrameBytes caps how much data the daemon will accept per
	// inbound WS frame; defends against a malicious client streaming
	// gigabytes of input. Default 64 KiB (one frame ≈ pasted shell
	// command, far larger than any realistic key sequence).
	MaxFrameBytes int64

	// MaxOutputFrameBytes is the chunk size used when forwarding PTY
	// output to the client. Smaller = lower latency, larger = fewer
	// frames; 4 KiB is a sweet spot for xterm rendering.
	MaxOutputFrameBytes int

	// HandshakeTimeout caps the WS upgrade window. Browsers can drag
	// here when the proxy is slow; 10s is generous.
	HandshakeTimeout time.Duration
}

// DefaultForgeShellConfig returns the Phase 3 self-maintenance posture.
func DefaultForgeShellConfig() *ForgeShellConfig {
	return &ForgeShellConfig{
		AllowRoots:          []string{"/root/matrix"},
		Shell:               "",
		IdleTimeout:         30 * time.Minute,
		MaxFrameBytes:       64 * 1024,
		MaxOutputFrameBytes: 4 * 1024,
		HandshakeTimeout:    10 * time.Second,
	}
}

// shellControlMessage is the wire shape for inbound control frames.
// Stdin can also arrive as raw binary frames (preferred path).
type shellControlMessage struct {
	Type string `json:"type"`
	Cols uint16 `json:"cols,omitempty"`
	Rows uint16 `json:"rows,omitempty"`
	Name string `json:"name,omitempty"` // signal name when type==signal
	Data string `json:"data,omitempty"` // raw stdin when type==stdin
}

// shellExitMessage is the final outbound frame on graceful close.
type shellExitMessage struct {
	Type   string `json:"type"`
	Code   int    `json:"code"`
	Signal string `json:"signal,omitempty"`
	Reason string `json:"reason,omitempty"`
}

var shellUpgrader = websocket.Upgrader{
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
	// Local-only daemon. Vite proxy will be the immediate client; on
	// loopback there's no realistic origin spoofing surface. Tightening
	// to specific origins is a Phase 4 (Tauri) concern.
	CheckOrigin:  func(r *http.Request) bool { return true },
	Subprotocols: []string{"matrix-shell.v1"},
}

// handleForgeShellExec upgrades the connection to WS, validates auth
// (header OR subprotocol), spawns a PTY, and shuttles bytes between the
// PTY master and the WS client until idle / EOF / client disconnect.
//
// Routes 404 when shellCfg is nil so non-Forge daemons don't expose
// the surface.
func (d *daemonState) handleForgeShellExec(w http.ResponseWriter, r *http.Request) {
	if d.shellCfg == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "forge shell route disabled; restart daemon with -forge-mode",
		})
		return
	}
	// Auth: prefer Authorization header (CLI/curl path); fall back to
	// Sec-WebSocket-Protocol bearer.<token> entry (browser path, where
	// custom headers can't be set on `new WebSocket()`).
	if !d.authorizeShellRequest(w, r) {
		return
	}

	cfg := d.shellCfg
	cwd := r.URL.Query().Get("cwd")
	resolvedCwd, err := resolveShellCwd(cfg, cwd)
	if err != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
		return
	}

	upgrader := shellUpgrader
	upgrader.HandshakeTimeout = cfg.HandshakeTimeout
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrade already wrote the response; nothing further to send.
		return
	}
	defer conn.Close()

	conn.SetReadLimit(cfg.MaxFrameBytes)

	// Spawn the PTY-backed shell.
	shell := chooseShell(cfg)
	cmd := exec.Command(shell, "-i") //nolint:gosec — shell is operator-controlled (env / config), not user input
	cmd.Dir = resolvedCwd
	cmd.Env = shellEnv(resolvedCwd)
	// pty.Start sets Setsid+Setctty internally; setting Setsid here too
	// would conflict on Linux (multiple Setsid is fine but the pty
	// helper assumes it owns the SysProcAttr). Skip our own value.

	shellDebug("upgrading: shell=%s cwd=%s", shell, resolvedCwd)
	ptmx, err := pty.Start(cmd)
	if err != nil {
		shellDebug("pty.Start failed: %v", err)
		_ = writeShellControl(conn, shellExitMessage{
			Type:   "error",
			Code:   1,
			Reason: fmt.Sprintf("pty.Start: %v", err),
		})
		return
	}
	shellDebug("pty spawned: pid=%d", cmd.Process.Pid)
	defer func() {
		_ = ptmx.Close()
		// Make sure the child isn't lingering after the WS closes.
		if cmd.Process != nil {
			_ = syscallKillProcessGroup(cmd.Process.Pid, syscall.SIGKILL)
			_, _ = cmd.Process.Wait()
		}
	}()

	// Initial resize: sensible default (80x24) until the SPA sends
	// the first {type:"resize"} message. xterm-addon-fit fires resize
	// on attach so this is overwritten within ms.
	_ = pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80})

	// Idle deadline: bumped on every inbound frame.
	idle := newIdleTimer(cfg.IdleTimeout)
	defer idle.stop()

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	var closeOnce sync.Once
	closeWith := func(reason string, code int) {
		closeOnce.Do(func() {
			_ = writeShellControl(conn, shellExitMessage{
				Type:   "exit",
				Code:   code,
				Reason: reason,
			})
			_ = conn.WriteControl(websocket.CloseMessage,
				websocket.FormatCloseMessage(websocket.CloseNormalClosure, reason),
				time.Now().Add(time.Second))
			cancel()
		})
	}

	// PTY → WS pump.
	go func() {
		buf := make([]byte, cfg.MaxOutputFrameBytes)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					closeWith("client_write_failed: "+werr.Error(), 1)
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || strings.Contains(err.Error(), "input/output error") {
					// PTY closed (child exited).
					code := exitCode(cmd)
					closeWith("child_exited", code)
				} else {
					closeWith("pty_read: "+err.Error(), 1)
				}
				return
			}
		}
	}()

	// Idle timer goroutine.
	go func() {
		select {
		case <-ctx.Done():
			return
		case <-idle.fired():
			closeWith("idle_timeout", 1)
		}
	}()

	// WS → PTY pump (this goroutine).
	for {
		mt, payload, err := conn.ReadMessage()
		if err != nil {
			closeWith("client_read: "+err.Error(), 0)
			return
		}
		idle.bump()
		switch mt {
		case websocket.BinaryMessage:
			if _, werr := ptmx.Write(payload); werr != nil {
				closeWith("pty_write: "+werr.Error(), 1)
				break
			}
		case websocket.TextMessage:
			// Try control parse; fall back to raw stdin so legacy
			// clients can send keystrokes as text frames.
			var ctl shellControlMessage
			if jerr := json.Unmarshal(payload, &ctl); jerr == nil && ctl.Type != "" {
				switch ctl.Type {
				case "stdin":
					if ctl.Data != "" {
						_, _ = ptmx.Write([]byte(ctl.Data))
					}
				case "resize":
					_ = pty.Setsize(ptmx, &pty.Winsize{Cols: ctl.Cols, Rows: ctl.Rows})
				case "signal":
					forwardSignal(cmd, ctl.Name)
				default:
					// Unknown type — treat as stdin (defensive).
					_, _ = ptmx.Write(payload)
				}
			} else {
				_, _ = ptmx.Write(payload)
			}
		case websocket.CloseMessage:
			closeWith("client_close", 0)
			return
		default:
			// Ping / pong are handled internally by gorilla/websocket
			// when SetPingHandler isn't overridden; ignore other types.
		}
		select {
		case <-ctx.Done():
			return
		default:
		}
	}
}

// authorizeShellRequest validates either Authorization: Bearer or the
// Sec-WebSocket-Protocol "auth.bearer.<token>" entry. Returns true iff
// the client is authorised; otherwise writes a 401 + returns false.
//
// The subprotocol path is the LOAD-BEARING one for the browser SPA
// because `new WebSocket()` doesn't accept custom headers. Browsers
// pass the protocols array; the daemon's upgrader echoes back the
// chosen subprotocol ("matrix-shell.v1") on the handshake, so the
// auth marker doesn't need to ride the chosen protocol — just the
// offered list.
func (d *daemonState) authorizeShellRequest(w http.ResponseWriter, r *http.Request) bool {
	if d.authToken == "" {
		return true
	}
	if got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")); got != "" {
		if got == d.authToken {
			return true
		}
	}
	for _, hv := range r.Header.Values("Sec-WebSocket-Protocol") {
		for _, item := range strings.Split(hv, ",") {
			item = strings.TrimSpace(item)
			if strings.HasPrefix(item, "auth.bearer.") {
				if strings.TrimPrefix(item, "auth.bearer.") == d.authToken {
					return true
				}
			}
		}
	}
	writeJSON(w, http.StatusUnauthorized, map[string]string{
		"error": "unauthorised: bearer token required (Authorization header or auth.bearer.<token> subprotocol)",
	})
	return false
}

// resolveShellCwd cleans + validates the cwd argument. Empty falls back
// to AllowRoots[0]. Returns an error when the resolved path is not
// under any AllowRoot.
func resolveShellCwd(cfg *ForgeShellConfig, cwd string) (string, error) {
	if cwd == "" {
		if len(cfg.AllowRoots) == 0 {
			return "", errors.New("no AllowRoots configured")
		}
		return cfg.AllowRoots[0], nil
	}
	clean, err := NormalizePath(cwd)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(clean)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("resolve cwd: %w", err)
		}
		resolved = clean
	}
	for _, root := range cfg.AllowRoots {
		if isUnderPrefix(resolved, root) {
			return resolved, nil
		}
	}
	return "", fmt.Errorf("%w: cwd %q not under AllowRoots", ErrPathOutsideAllowlist, resolved)
}

// chooseShell picks the binary to exec inside the PTY.
func chooseShell(cfg *ForgeShellConfig) string {
	if cfg.Shell != "" {
		return cfg.Shell
	}
	if v := os.Getenv("MATRIX_FORGE_SHELL"); v != "" {
		return v
	}
	if v := os.Getenv("SHELL"); v != "" {
		return v
	}
	return "/bin/bash"
}

// shellEnv returns the env the PTY-backed shell inherits. We pass
// HOME, PATH, TERM, LANG, USER, plus MATRIX_* vars (operator-controlled
// daemon config) and PWD set to the resolved cwd. We strip
// MATRIX_DAEMON_TOKEN to avoid leaking it into shell history.
func shellEnv(cwd string) []string {
	keep := map[string]bool{
		"HOME": true, "USER": true, "LANG": true, "LC_ALL": true,
		"TERM": true, "PATH": true, "TZ": true,
	}
	out := []string{
		"PWD=" + cwd,
		"TERM=xterm-256color",
		"MATRIX_FORGE_SESSION=1",
	}
	for _, e := range os.Environ() {
		k := e
		if i := strings.IndexByte(e, '='); i >= 0 {
			k = e[:i]
		}
		if k == "MATRIX_DAEMON_TOKEN" {
			continue
		}
		if keep[k] || strings.HasPrefix(k, "MATRIX_") || strings.HasPrefix(k, "OPENCODE_") {
			out = append(out, e)
		}
	}
	if !envHas(out, "PATH") {
		out = append(out, "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	}
	if !envHas(out, "HOME") {
		out = append(out, "HOME=/root")
	}
	return out
}

func envHas(env []string, k string) bool {
	prefix := k + "="
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}

// idleTimer fires when no `bump()` has been called for `dur`.
type idleTimer struct {
	dur     time.Duration
	timer   *time.Timer
	firedCh chan struct{}
	once    sync.Once
}

func newIdleTimer(dur time.Duration) *idleTimer {
	t := &idleTimer{
		dur:     dur,
		firedCh: make(chan struct{}),
	}
	t.timer = time.AfterFunc(dur, func() {
		t.once.Do(func() { close(t.firedCh) })
	})
	return t
}

func (t *idleTimer) bump() {
	t.timer.Reset(t.dur)
}

func (t *idleTimer) fired() <-chan struct{} { return t.firedCh }

func (t *idleTimer) stop() {
	t.timer.Stop()
}

// writeShellControl marshals + sends a control message as a text frame.
func writeShellControl(conn *websocket.Conn, msg shellExitMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	return conn.WriteMessage(websocket.TextMessage, body)
}

// forwardSignal maps a string signal name to syscall.Signal and sends
// it to the child's process group. Recognised: INT, TERM, QUIT, HUP.
// Unknown names are silently ignored — no panic, no error frame —
// because the SPA might send vendor-specific names that future versions
// add support for.
func forwardSignal(cmd *exec.Cmd, name string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	var sig syscall.Signal
	switch strings.ToUpper(name) {
	case "INT":
		sig = syscall.SIGINT
	case "TERM":
		sig = syscall.SIGTERM
	case "QUIT":
		sig = syscall.SIGQUIT
	case "HUP":
		sig = syscall.SIGHUP
	case "KILL":
		sig = syscall.SIGKILL
	default:
		return
	}
	_ = syscallKillProcessGroup(cmd.Process.Pid, sig)
}

// syscallKillProcessGroup sends `sig` to the process group leader at
// `pid` (negated so kill(2) treats it as a pgid). Wrapped so test
// builds on Windows would compile; the daemon only ships on Linux but
// keeping the helper isolated is friendlier to future portability.
func syscallKillProcessGroup(pid int, sig syscall.Signal) error {
	return syscall.Kill(-pid, sig)
}

// exitCode returns the child's exit code if it has terminated, else 0.
func exitCode(cmd *exec.Cmd) int {
	if cmd == nil || cmd.ProcessState == nil {
		// Wait might not have run yet; try a non-blocking probe.
		if cmd != nil && cmd.Process != nil {
			ps, err := cmd.Process.Wait()
			if err == nil && ps != nil {
				return ps.ExitCode()
			}
		}
		return 0
	}
	return cmd.ProcessState.ExitCode()
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
