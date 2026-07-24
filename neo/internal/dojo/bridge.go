// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package dojo

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// bridge.go — the desktop actuation carrier (DOJO wave 2, req 2). bytebotd
// lives inside the Railway sandbox on localhost:9990 with NO privateHost and no
// public port (wave-1 finding), so it is reachable ONLY by running a command
// inside the sandbox. The carrier therefore rides sandboxExec-curl: every
// bytebotd /computer-use request is issued by a curl executed inside the
// sandbox, and every raw shell primitive (the AT-SPI dump) is a plain
// sandboxExec. The transport is a seam so a docker contract test can drive the
// SAME carrier logic against a real bytebot-desktop container over a direct
// HTTP/exec path — the bytebotd wire under test is real either way.

// Typed carrier errors (req 2.1/2.3: a desktop call with no live session never
// hangs — it degrades to a typed result the planner reads).
var (
	// ErrNoSession is returned when no live dojo session exists to drive. The
	// caller surfaces it as a "no desktop is running" tool result.
	ErrNoSession = errors.New("dojo: no live desktop session")
	// ErrBytebotd wraps a non-2xx bytebotd response.
	ErrBytebotd = errors.New("dojo: bytebotd error")
	// ErrTransport wraps a sandbox-exec/transport failure reaching bytebotd.
	ErrTransport = errors.New("dojo: desktop transport error")
)

// BytebotdTransport carries a request to a session's appliance. The production
// implementation (execCurlTransport) rides sandboxExec-curl into the sandbox;
// a docker contract test supplies a direct-HTTP/exec transport against a real
// local container. Both hit REAL bytebotd — the seam swaps only the carrier.
type BytebotdTransport interface {
	// Call POSTs a bytebotd /computer-use request body to the session's
	// appliance and returns the response body and the bytebotd HTTP status.
	Call(ctx context.Context, s Session, body []byte) (resp []byte, status int, err error)
	// ExecDesktop runs a shell command INSIDE the appliance desktop as the
	// session user, with DISPLAY and the XFCE session's a11y D-Bus exported —
	// the AT-SPI dumper's carrier. It returns stdout and the command exit code.
	ExecDesktop(ctx context.Context, s Session, command string, timeoutSec int) (stdout string, exit int, err error)
}

// applianceContainer is the name the boot script gives the bytebot-desktop
// container inside the sandbox (docker run --name dojo …). The AT-SPI dumper
// runs inside it as the desktop user.
const applianceContainer = "dojo"

// a11yTimeoutSec bounds the AT-SPI walk.
const a11yTimeoutSec = 30

// execMaxOutput bounds a single exec's captured output before the carrier
// treats it as potentially truncated and reads the remainder in chunks. Railway
// caps sandboxExec stdout; a 1280×960 PNG screenshot base64s to ~3.5 MB, well
// over any single-exec cap, so the screenshot path always chunk-reads.
const execChunkBytes = 512 * 1024

// bytebotdCurlTimeoutSec bounds the in-sandbox curl itself; the exec wrapping
// it gets a slightly larger budget so the exec never times out before curl
// reports its own error.
const (
	bytebotdCurlTimeoutSec = 30
	bytebotdExecTimeoutSec = 45
)

// CallBytebotd resolves the live session and issues one bytebotd /computer-use
// request through the carrier. A non-2xx status is returned as ErrBytebotd with
// the body preserved so the caller can surface bytebotd's own message.
func (m *Manager) CallBytebotd(ctx context.Context, body []byte) ([]byte, error) {
	s, err := m.liveSession()
	if err != nil {
		return nil, err
	}
	switch s.State {
	case StateProvisioning:
		return nil, ErrBooting
	case StateTakeover:
		// The human holds the control lease (req 4.2): every agent desktop
		// call is refused until handback.
		return nil, ErrTakeoverActive
	}
	action := actionOf(body)
	if err := m.gateReobserve(s.ID, action); err != nil {
		return nil, err
	}
	if m.transport == nil {
		return nil, fmt.Errorf("%w: no transport configured", ErrTransport)
	}
	if s.State == StateReady {
		// The agent's first action marks the session driven (dojo.active).
		if snap, aerr := m.Activate(s.ID); aerr == nil {
			s = snap
		}
	}
	m.Touch(s.ID)
	resp, status, err := m.transport.Call(ctx, s, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	if status < 200 || status >= 300 {
		return resp, fmt.Errorf("%w: HTTP %d: %s", ErrBytebotd, status, strings.TrimSpace(truncate(resp, 256)))
	}
	if action == "screenshot" {
		// A landed screenshot IS a fresh observation (desktop_look begins with
		// one) — it satisfies the post-handback re-observation contract.
		m.clearReobserve(s.ID)
	}
	return resp, nil
}

// mutatingActions are the bytebotd actions that CHANGE the desktop; these are
// what the post-handback re-observation gate blocks. Reads (screenshot,
// cursor_position, read_file) and wait stay allowed — only the observation
// itself clears the gate.
var mutatingActions = map[string]bool{
	"move_mouse": true, "trace_mouse": true, "click_mouse": true, "press_mouse": true,
	"drag_mouse": true, "scroll": true, "type_keys": true, "press_keys": true,
	"type_text": true, "paste_text": true, "application": true, "write_file": true,
}

func actionOf(body []byte) string {
	var w struct {
		Action string `json:"action"`
	}
	_ = json.Unmarshal(body, &w)
	return w.Action
}

// gateReobserve enforces req 4.3 on the agent path: after a handback, mutating
// actions are rejected until a fresh observation lands.
func (m *Manager) gateReobserve(id, action string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return ErrNotFound
	}
	if s.needsReobserve && mutatingActions[action] {
		return ErrReobserveRequired
	}
	return nil
}

func (m *Manager) clearReobserve(id string) {
	m.mu.Lock()
	if s := m.sessions[id]; s != nil {
		s.needsReobserve = false
	}
	m.mu.Unlock()
}

// TakeoverInput passes ONE human input action through to the desktop while the
// human holds the control lease (req 4.2). It is refused in every other state
// — the passthrough exists only under an explicit takeover, and holding the
// mouse never widens anyone's authority.
func (m *Manager) TakeoverInput(ctx context.Context, convID string, body []byte) ([]byte, error) {
	snap, ok := m.SessionForConv(convID)
	if !ok {
		return nil, ErrNoSession
	}
	if snap.State != StateTakeover {
		return nil, ErrNoTakeover
	}
	if m.transport == nil {
		return nil, fmt.Errorf("%w: no transport configured", ErrTransport)
	}
	m.Touch(snap.ID)
	resp, status, err := m.transport.Call(ctx, snap, body)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	if status < 200 || status >= 300 {
		return resp, fmt.Errorf("%w: HTTP %d: %s", ErrBytebotd, status, strings.TrimSpace(truncate(resp, 256)))
	}
	return resp, nil
}

// A11yDump resolves the live session and runs the AT-SPI accessibility dumper
// inside the appliance desktop, returning the parsed accessibility tree — the
// deterministic middle rung of the driving ladder (roles, labels, screen-pixel
// coordinates), no vision model involved.
func (m *Manager) A11yDump(ctx context.Context) (A11yTree, error) {
	s, err := m.liveSession()
	if err != nil {
		return A11yTree{}, err
	}
	switch s.State {
	case StateProvisioning:
		return A11yTree{}, ErrBooting
	case StateTakeover:
		return A11yTree{}, ErrTakeoverActive
	}
	if m.transport == nil {
		return A11yTree{}, fmt.Errorf("%w: no transport configured", ErrTransport)
	}
	if s.State == StateReady {
		if snap, aerr := m.Activate(s.ID); aerr == nil {
			s = snap
		}
	}
	m.Touch(s.ID)
	out, exit, err := m.transport.ExecDesktop(ctx, s, a11yPayloadCommand(), a11yTimeoutSec)
	if err != nil {
		return A11yTree{}, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	if exit != 0 {
		return A11yTree{}, fmt.Errorf("%w: a11y dumper exited %d: %s", ErrTransport, exit, strings.TrimSpace(truncate([]byte(out), 256)))
	}
	tree, err := ParseA11yTree(out)
	if err == nil {
		// A landed a11y dump is a fresh observation (req 4.3).
		m.clearReobserve(s.ID)
	}
	return tree, err
}

// Frame captures a live-view screenshot of the conversation's desktop for the
// client panel (req 4.1) and returns the raw PNG. It is a PASSIVE view: it
// never bumps the idle clock (an abandoned open tab must not keep a sandbox
// billed forever — actively driving does, watching does not), never activates
// the session, and works in every attached state including takeover (the human
// watches themselves drive).
func (m *Manager) Frame(ctx context.Context, convID string) ([]byte, error) {
	snap, ok := m.SessionForConv(convID)
	if !ok {
		return nil, ErrNoSession
	}
	switch snap.State {
	case StateProvisioning:
		return nil, ErrBooting
	case StateShipping:
		return nil, ErrNoSession
	}
	if m.transport == nil {
		return nil, fmt.Errorf("%w: no transport configured", ErrTransport)
	}
	resp, status, err := m.transport.Call(ctx, snap, []byte(`{"action":"screenshot"}`))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrTransport, err)
	}
	if status < 200 || status >= 300 {
		return nil, fmt.Errorf("%w: HTTP %d: %s", ErrBytebotd, status, strings.TrimSpace(truncate(resp, 256)))
	}
	var wrap struct {
		Image string `json:"image"`
	}
	if err := json.Unmarshal(resp, &wrap); err != nil || wrap.Image == "" {
		return nil, fmt.Errorf("%w: screenshot was not an image", ErrTransport)
	}
	png, err := base64.StdEncoding.DecodeString(wrap.Image)
	if err != nil {
		return nil, fmt.Errorf("%w: screenshot base64: %v", ErrTransport, err)
	}
	return png, nil
}

// RegisterSessionForTest hand-registers a live (active) session bound to the
// given sandbox id, bypassing Provision — for docker contract tests that drive
// an existing local bytebot-desktop container through the carrier seam. It
// returns the session snapshot. Not used in production paths.
func (m *Manager) RegisterSessionForTest(sandboxID string) (Session, error) {
	if m == nil {
		return Session{}, ErrNoSession
	}
	s := &session{
		id:        newSessionID(),
		convID:    "contract",
		state:     StateActive,
		sandboxID: sandboxID,
		image:     m.cfg.Image,
		createdAt: m.now(),
		touchedAt: m.now(),
	}
	m.mu.Lock()
	m.sessions[s.id] = s
	m.byConv[s.convID] = s.id
	m.mu.Unlock()
	return s.snapshot(), nil
}

// liveSession returns the most-recently-touched non-destroyed session. DOJO's
// MaxActive defaults to 1, so "the user's desktop" is unambiguous; when more
// than one is somehow live the freshest wins (the one the user is watching).
func (m *Manager) liveSession() (Session, error) {
	if m == nil {
		return Session{}, ErrNoSession
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	var best *session
	for _, s := range m.sessions {
		if s.state == StateDestroyed || s.state == StateShipping {
			continue
		}
		if best == nil || s.touchedAt.After(best.touchedAt) {
			best = s
		}
	}
	if best == nil {
		return Session{}, ErrNoSession
	}
	return best.snapshot(), nil
}

// execCurlTransport is the production carrier: it reaches bytebotd by executing
// curl inside the sandbox and reads back potentially-large responses (a
// screenshot) in truncation-safe base64 chunks, since Railway caps a single
// exec's stdout.
type execCurlTransport struct {
	rw RailwayClient
}

var _ BytebotdTransport = (*execCurlTransport)(nil)

func (t *execCurlTransport) Call(ctx context.Context, s Session, body []byte) ([]byte, int, error) {
	if s.SandboxID == "" {
		return nil, 0, fmt.Errorf("session %s has no sandbox", s.ID)
	}
	// Write the request body to a file (avoids shell-escaping a large JSON
	// payload), POST it, capture bytebotd's body to a response file plus the
	// HTTP status to a tiny status file, then read both back. Base64 keeps the
	// JSON binary-safe across the exec stdout boundary.
	reqB64 := base64.StdEncoding.EncodeToString(body)
	script := fmt.Sprintf(
		`set -e; echo %s | base64 -d > /tmp/dojo-req.json; `+
			`code=$(curl -s -o /tmp/dojo-resp.bin -w '%%{http_code}' --max-time %d `+
			`-X POST http://localhost:%d/computer-use -H 'Content-Type: application/json' --data-binary @/tmp/dojo-req.json); `+
			`printf '%%s\n' "$code"; wc -c < /tmp/dojo-resp.bin`,
		reqB64, bytebotdCurlTimeoutSec, bytebotdPort)
	res, err := t.rw.ExecSandbox(ctx, s.SandboxID, script, bytebotdExecTimeoutSec)
	if err != nil {
		return nil, 0, err
	}
	if res.ExitCode != 0 || res.TimedOut {
		return nil, 0, fmt.Errorf("bytebotd curl exit=%d timedOut=%v: %s", res.ExitCode, res.TimedOut, strings.TrimSpace(res.Stderr))
	}
	lines := strings.Fields(strings.TrimSpace(res.Stdout))
	if len(lines) < 2 {
		return nil, 0, fmt.Errorf("bytebotd curl produced no status: %q", truncate([]byte(res.Stdout), 128))
	}
	status, err := strconv.Atoi(lines[0])
	if err != nil {
		return nil, 0, fmt.Errorf("bytebotd curl bad status %q", lines[0])
	}
	total, err := strconv.Atoi(lines[len(lines)-1])
	if err != nil {
		return nil, 0, fmt.Errorf("bytebotd curl bad length %q", lines[len(lines)-1])
	}
	resp, err := t.readFile(ctx, s.SandboxID, "/tmp/dojo-resp.bin", total)
	if err != nil {
		return nil, 0, err
	}
	return resp, status, nil
}

// readFile reads a sandbox file back in truncation-safe base64 chunks until the
// full byte count is recovered. Small responses (clicks, cursor reads) come
// back in one chunk; a screenshot spans several.
func (t *execCurlTransport) readFile(ctx context.Context, sandboxID, path string, total int) ([]byte, error) {
	out := make([]byte, 0, total)
	for off := 0; off < total; off += execChunkBytes {
		n := execChunkBytes
		if off+n > total {
			n = total - off
		}
		cmd := fmt.Sprintf("tail -c +%d %s | head -c %d | base64 -w0", off+1, path, n)
		res, err := t.rw.ExecSandbox(ctx, sandboxID, cmd, bytebotdExecTimeoutSec)
		if err != nil {
			return nil, err
		}
		if res.ExitCode != 0 {
			return nil, fmt.Errorf("read chunk exit=%d: %s", res.ExitCode, strings.TrimSpace(res.Stderr))
		}
		chunk, err := base64.StdEncoding.DecodeString(strings.TrimSpace(res.Stdout))
		if err != nil {
			return nil, fmt.Errorf("decode chunk at %d: %w", off, err)
		}
		out = append(out, chunk...)
		if len(chunk) == 0 {
			break
		}
	}
	if len(out) != total {
		return out, fmt.Errorf("%w: recovered %d of %d bytes from %s", ErrTransport, len(out), total, path)
	}
	return out, nil
}

func (t *execCurlTransport) ExecDesktop(ctx context.Context, s Session, command string, timeoutSec int) (string, int, error) {
	if s.SandboxID == "" {
		return "", 0, fmt.Errorf("session %s has no sandbox", s.ID)
	}
	if timeoutSec <= 0 {
		timeoutSec = bytebotdExecTimeoutSec
	}
	full := WrapDesktopExec(applianceContainer, command)
	res, err := t.rw.ExecSandbox(ctx, s.SandboxID, full, timeoutSec)
	if err != nil {
		return "", 0, err
	}
	if res.TimedOut {
		return res.Stdout, res.ExitCode, fmt.Errorf("command timed out after %ds", timeoutSec)
	}
	return res.Stdout, res.ExitCode, nil
}

// WrapDesktopExec builds the shell that runs command inside the named appliance
// container as the desktop user, with DISPLAY and the live XFCE session's
// a11y D-Bus address exported (discovered from xfce4-session's environ). It is
// exported so a docker contract test builds the identical wrapper for its own
// local container.
func WrapDesktopExec(container, command string) string {
	inner := `export DISPLAY=:0; ` +
		`export "$(cat /proc/$(pgrep -u user -x xfce4-session | head -1)/environ 2>/dev/null | tr '\0' '\n' | grep '^DBUS_SESSION_BUS_ADDRESS=')"; ` +
		command
	return fmt.Sprintf("docker exec -u user %s bash -lc %s", container, shellSingleQuote(inner))
}

// shellSingleQuote wraps s in single quotes, escaping embedded single quotes so
// it survives as one argv element.
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}
