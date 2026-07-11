// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package preview implements the Neo workbench's "preview is a deliverable" lifecycle: on
// plan completion (or an explicit user request) it provisions a Railway sandbox
// on the PRIVATE network, deploys the project's current state into it, starts
// the detected app, health-probes it, and registers the sandbox's private
// host:port with the router so the /preview/{user} proxy (router/internal/
// preview, task 3.2) can reverse-proxy to it under the user's JWT. A TTL reaper
// destroys idle sandboxes. No public sandbox domain is ever required.
//
// The Manager emits preview.pending/ready/failed/expired events through an
// Emit callback the engine supplies (engine.publishToConversation), so the
// client rebuilds the preview pane live and from the durable trace.
package preview

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/sandbox"
)

// State is the lifecycle state carried on every preview.* event.
type State string

const (
	StatePending State = "pending"
	StateReady   State = "ready"
	StateFailed  State = "failed"
	StateExpired State = "expired"
)

// Event type names (mirrored in the daemon trace whitelist and the client
// reducer's preview.* fold).
const (
	EvtPending = "preview.pending"
	EvtReady   = "preview.ready"
	EvtFailed  = "preview.failed"
	EvtExpired = "preview.expired"
)

// Default limits for deploying a project's state into a sandbox.
const (
	defaultTTL = 30 * time.Minute
	// defaultImage carries a Node runtime because most previews are web
	// apps (npm install + dev server). It has apt on top of Debian bookworm so a
	// preview can bootstrap python/go on demand. Override with NEO_PREVIEW_IMAGE
	// for a stack whose runtime it lacks.
	defaultImage     = "node:22-bookworm"
	maxDeployBytes   = 48 << 20 // 48 MiB total payload
	maxFileBytes     = 4 << 20  // 4 MiB per file
	maxDeployFiles   = 6000
	healthProbeTries = 40
	healthProbeGap   = 3 * time.Second
)

// Emit publishes a preview.* event onto a conversation's live topic + durable
// trace. It is engine.publishToConversation; a no-op when the run has no topic.
type Emit func(convID, typ string, fields map[string]interface{})

// Config configures a Manager. UserID and RouterInternalURL are required for a
// working preview; when either is empty the Manager is disabled and Provision
// returns a clear error (the engine only builds a Manager when configured).
type Config struct {
	// UserID is the supabase user id — the key the router /preview proxy uses
	// and the value the daemon registers its target under. MUST match the JWT subject.
	UserID string
	// RouterInternalURL is the base of the router's internal listener, e.g.
	// http://matrix-router.railway.internal:8088 . the daemon registers/deregisters
	// its preview target at {RouterInternalURL}/internal/preview/register.
	RouterInternalURL string
	// RouterToken is ROUTER_PREVIEW_TOKEN, sent as a bearer to the internal door.
	RouterToken string
	// PublicBase is the client-facing base for the proxied preview URL. Empty
	// yields a site-relative "/preview/{user}/" (correct behind the router).
	PublicBase string
	// TTL is the idle lifetime before the reaper destroys a sandbox (default 30m).
	TTL time.Duration
	// MaxActive caps this user's CONCURRENT sandboxes (req 7.4). Provisioning
	// past the cap evicts the least-recently-touched preview first (destroyed +
	// preview.expired), so the bound always holds. Default 2.
	MaxActive int
	// Image is the sandbox base image (default ubuntu:24.04).
	Image string
	// Logf receives diagnostics; nil discards.
	Logf func(string, ...interface{})
}

// Manager owns the live previews for one daemon (one user). It is safe for
// concurrent use.
type Manager struct {
	sb   sandbox.Client
	emit Emit
	cfg  Config
	hc   *http.Client

	// now is the clock; injectable so the reaper is deterministic under test.
	now func() time.Time

	mu     sync.Mutex
	active map[string]*live // keyed by conversation id
}

// live is one provisioned preview.
type live struct {
	convID    string
	sandboxID string
	host      string
	port      string
	url       string
	touchedAt time.Time
}

// Info is the result of a successful Provision.
type Info struct {
	URL       string
	Host      string
	Port      string
	SandboxID string
}

// New builds a Manager over a sandbox client and an event emitter.
func New(sb sandbox.Client, emit Emit, cfg Config) *Manager {
	if cfg.TTL <= 0 {
		cfg.TTL = defaultTTL
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = 2
	}
	if cfg.Image == "" {
		cfg.Image = defaultImage
	}
	if emit == nil {
		emit = func(string, string, map[string]interface{}) {}
	}
	return &Manager{
		sb:     sb,
		emit:   emit,
		cfg:    cfg,
		hc:     &http.Client{Timeout: 30 * time.Second},
		now:    time.Now,
		active: map[string]*live{},
	}
}

func (m *Manager) logf(format string, args ...interface{}) {
	if m.cfg.Logf != nil {
		m.cfg.Logf(format, args...)
	}
}

// Enabled reports whether the Manager can actually provision (configured).
func (m *Manager) Enabled() bool {
	return m != nil && m.sb != nil && m.cfg.UserID != "" && m.cfg.RouterInternalURL != ""
}

// Request describes what to preview.
type Request struct {
	ConvID string // the conversation whose topic carries the preview.* events
	Root   string // the project's workspace root to deploy
}

// Provision brings up (or replaces) the preview for a conversation. It emits
// preview.pending immediately, then preview.ready with the proxied URL on
// success or preview.failed with an honest reason on any failure. It is
// synchronous; the engine calls it from a goroutine on the re-preview route (or
// a future auto-preview seam).
func (m *Manager) Provision(ctx context.Context, req Request) (Info, error) {
	if !m.Enabled() {
		err := fmt.Errorf("preview is not configured in this environment")
		m.emit(req.ConvID, EvtFailed, map[string]interface{}{"state": string(StateFailed), "reason": err.Error()})
		return Info{}, err
	}
	m.emit(req.ConvID, EvtPending, map[string]interface{}{"state": string(StatePending)})

	// Replace any existing preview for this conversation (re-preview is a single
	// action): tear the old sandbox down first so we never leak.
	m.teardownConv(context.Background(), req.ConvID, false)

	// Per-user concurrent-sandbox cap (req 7.4): make room by evicting the
	// least-recently-touched previews before provisioning a new one.
	m.evictForCap(context.Background())

	info, err := m.provision(ctx, req)
	if err != nil {
		m.logf("preview: provision %s: %v", req.ConvID, err)
		m.emit(req.ConvID, EvtFailed, map[string]interface{}{"state": string(StateFailed), "reason": userReason(err)})
		return Info{}, err
	}
	m.emit(req.ConvID, EvtReady, map[string]interface{}{
		"state": string(StateReady), "url": info.URL, "port": info.Port,
	})
	return info, nil
}

func (m *Manager) provision(ctx context.Context, req Request) (Info, error) {
	spec, ok := DetectStart(req.Root)
	if !ok {
		return Info{}, fmt.Errorf("could not detect how to start this project (no dev/start script or known entrypoint)")
	}
	files, err := collectFiles(req.Root)
	if err != nil {
		return Info{}, fmt.Errorf("collect project files: %w", err)
	}
	if len(files) == 0 {
		return Info{}, fmt.Errorf("project is empty — nothing to preview yet")
	}

	sb, err := m.sb.Create(ctx, sandbox.CreateOpts{
		Image:   m.cfg.Image,
		Network: sandbox.NetworkPrivate,
		Env:     spec.Env,
	})
	if err != nil {
		return Info{}, fmt.Errorf("create sandbox: %w", err)
	}
	// From here on a failure must destroy the sandbox so we don't leak Railway
	// services.
	cleanup := func() { _ = m.sb.Destroy(context.Background(), sb.ID) }

	if err := m.sb.WriteFiles(ctx, sb.ID, files); err != nil {
		cleanup()
		return Info{}, fmt.Errorf("deploy files: %w", err)
	}
	if spec.Install != "" {
		if _, err := m.sb.Exec(ctx, sb.ID, shell(spec.Install)); err != nil {
			cleanup()
			return Info{}, fmt.Errorf("install: %w", err)
		}
	}
	// Start the app detached so Exec returns immediately; the dev server keeps
	// running inside the sandbox. Output is captured for diagnostics.
	start := fmt.Sprintf("nohup %s > /tmp/neo-preview.log 2>&1 & echo started", spec.Serve)
	if _, err := m.sb.Exec(ctx, sb.ID, shell(start)); err != nil {
		cleanup()
		return Info{}, fmt.Errorf("start app: %w", err)
	}
	if err := m.healthProbe(ctx, sb.ID, spec.Port); err != nil {
		cleanup()
		return Info{}, err
	}
	if err := m.register(ctx, sb.PrivateHost, spec.Port); err != nil {
		cleanup()
		return Info{}, fmt.Errorf("register preview target: %w", err)
	}

	url := m.previewURL()
	m.mu.Lock()
	m.active[req.ConvID] = &live{
		convID:    req.ConvID,
		sandboxID: sb.ID,
		host:      sb.PrivateHost,
		port:      spec.Port,
		url:       url,
		touchedAt: m.now(),
	}
	m.mu.Unlock()

	return Info{URL: url, Host: sb.PrivateHost, Port: spec.Port, SandboxID: sb.ID}, nil
}

// healthProbe execs curl inside the sandbox until the app answers or the budget
// is exhausted. A 2xx/3xx/4xx status means the server is up (4xx = routing, not
// dead); only connection refusal / timeout is "not yet".
func (m *Manager) healthProbe(ctx context.Context, sandboxID, port string) error {
	probe := shell(fmt.Sprintf(
		`code=$(curl -s -o /dev/null -w '%%{http_code}' --max-time 2 http://localhost:%s/ 2>/dev/null); echo "$code"`, port))
	for i := 0; i < healthProbeTries; i++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		res, err := m.sb.Exec(ctx, sandboxID, probe)
		if err == nil {
			code := strings.TrimSpace(res.Stdout)
			if len(code) == 3 && code[0] >= '2' && code[0] <= '4' {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(healthProbeGap):
		}
	}
	return fmt.Errorf("app did not become reachable on port %s within %s", port, time.Duration(healthProbeTries)*healthProbeGap)
}

// register / deregister talk to the router internal preview door.
func (m *Manager) register(ctx context.Context, host, port string) error {
	body, _ := json.Marshal(map[string]string{"user_id": m.cfg.UserID, "host": host, "port": port})
	return m.routerCall(ctx, http.MethodPost, body)
}

func (m *Manager) deregister(ctx context.Context) error {
	body, _ := json.Marshal(map[string]string{"user_id": m.cfg.UserID})
	return m.routerCall(ctx, http.MethodDelete, body)
}

func (m *Manager) routerCall(ctx context.Context, method string, body []byte) error {
	url := strings.TrimRight(m.cfg.RouterInternalURL, "/") + "/internal/preview/register"
	rq, err := http.NewRequestWithContext(ctx, method, url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	rq.Header.Set("Content-Type", "application/json")
	if m.cfg.RouterToken != "" {
		rq.Header.Set("Authorization", "Bearer "+m.cfg.RouterToken)
	}
	resp, err := m.hc.Do(rq)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("router %s /internal/preview/register: status %d", method, resp.StatusCode)
	}
	return nil
}

func (m *Manager) previewURL() string {
	if base := strings.TrimRight(m.cfg.PublicBase, "/"); base != "" {
		return base + "/preview/" + m.cfg.UserID + "/"
	}
	return "/preview/" + m.cfg.UserID + "/"
}

// Touch marks a preview as recently used, deferring the reaper.
func (m *Manager) Touch(convID string) {
	m.mu.Lock()
	if l := m.active[convID]; l != nil {
		l.touchedAt = m.now()
	}
	m.mu.Unlock()
}

// StartReaper runs the idle-preview reaper until ctx is cancelled. Idle
// previews (untouched for > TTL) are destroyed and emit preview.expired.
func (m *Manager) StartReaper(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.reapOnce(ctx)
		}
	}
}

func (m *Manager) reapOnce(ctx context.Context) {
	cutoff := m.now().Add(-m.cfg.TTL)
	var expired []*live
	m.mu.Lock()
	for id, l := range m.active {
		if l.touchedAt.Before(cutoff) {
			expired = append(expired, l)
			delete(m.active, id)
		}
	}
	m.mu.Unlock()
	for _, l := range expired {
		_ = m.sb.Destroy(ctx, l.sandboxID)
		_ = m.deregister(ctx)
		m.emit(l.convID, EvtExpired, map[string]interface{}{"state": string(StateExpired)})
		m.logf("preview: reaped idle sandbox %s (conv %s)", l.sandboxID, l.convID)
	}
}

// evictForCap destroys least-recently-touched previews until a new one fits
// under MaxActive. Evicted previews emit preview.expired so their panes render
// the honest state.
func (m *Manager) evictForCap(ctx context.Context) {
	for {
		m.mu.Lock()
		if len(m.active) < m.cfg.MaxActive {
			m.mu.Unlock()
			return
		}
		var oldest *live
		for _, l := range m.active {
			if oldest == nil || l.touchedAt.Before(oldest.touchedAt) {
				oldest = l
			}
		}
		delete(m.active, oldest.convID)
		m.mu.Unlock()
		_ = m.sb.Destroy(ctx, oldest.sandboxID)
		_ = m.deregister(ctx)
		m.emit(oldest.convID, EvtExpired, map[string]interface{}{"state": string(StateExpired)})
		m.logf("preview: evicted %s (conv %s) for the sandbox cap", oldest.sandboxID, oldest.convID)
	}
}

// teardownConv destroys the sandbox for one conversation. When emitExpired is
// true it also emits preview.expired (used by explicit teardown, not re-preview
// replacement, which is silent).
func (m *Manager) teardownConv(ctx context.Context, convID string, emitExpired bool) {
	m.mu.Lock()
	l := m.active[convID]
	delete(m.active, convID)
	m.mu.Unlock()
	if l == nil {
		return
	}
	_ = m.sb.Destroy(ctx, l.sandboxID)
	_ = m.deregister(ctx)
	if emitExpired {
		m.emit(convID, EvtExpired, map[string]interface{}{"state": string(StateExpired)})
	}
}

// Teardown destroys the preview for a conversation and emits preview.expired.
func (m *Manager) Teardown(ctx context.Context, convID string) {
	m.teardownConv(ctx, convID, true)
}

// Close destroys every live preview (graceful daemon shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	all := make([]*live, 0, len(m.active))
	for _, l := range m.active {
		all = append(all, l)
	}
	m.active = map[string]*live{}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for _, l := range all {
		_ = m.sb.Destroy(ctx, l.sandboxID)
	}
	if len(all) > 0 {
		_ = m.deregister(ctx)
	}
}

// shell wraps a command line to run under sh -lc inside the sandbox.
func shell(cmdline string) []string {
	return []string{"sh", "-lc", cmdline}
}

// userReason maps an internal error to a consumer-facing sentence (result, not
// protocol). The full error is logged separately.
func userReason(err error) string {
	msg := err.Error()
	switch {
	case strings.Contains(msg, "could not detect"):
		return "I couldn't tell how to start this project yet — add a dev/start script and I'll preview it."
	case strings.Contains(msg, "did not become reachable"):
		return "The app started but didn't answer in time. It may still be booting — try preview again."
	case strings.Contains(msg, "empty"):
		return "There's nothing to preview yet."
	default:
		return "I couldn't bring the preview up. " + msg
	}
}

// collectFiles walks a project root into a bounded []sandbox.File payload,
// skipping VCS/build/dependency dirs (they're rebuilt in the sandbox), oversized
// files, and obvious binaries. Paths are workspace-relative with forward slashes.
func collectFiles(root string) ([]sandbox.File, error) {
	var out []sandbox.File
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDir[d.Name()] && path != root {
				return fs.SkipDir
			}
			return nil
		}
		if len(out) >= maxDeployFiles || total >= maxDeployBytes {
			return fs.SkipAll
		}
		info, err := d.Info()
		if err != nil {
			return nil // skip unreadable entries rather than abort the deploy
		}
		if info.Size() > maxFileBytes {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if isBinary(data) {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		out = append(out, sandbox.File{Path: filepath.ToSlash(rel), Content: string(data)})
		total += info.Size()
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

var skipDir = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true, ".turbo": true, ".neo": true,
}

// isBinary reports whether a leading NUL byte marks content as non-text; deploy
// skips binaries (they bloat the payload and aren't source).
func isBinary(data []byte) bool {
	n := len(data)
	if n > 8000 {
		n = 8000
	}
	return bytes.IndexByte(data[:n], 0) >= 0
}
