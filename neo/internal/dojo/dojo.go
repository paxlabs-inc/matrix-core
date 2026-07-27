// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package dojo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// State is one node of the dojo session state machine (DOJO req 1.3).
type State string

const (
	StateProvisioning State = "provisioning"
	StateReady        State = "ready"
	StateActive       State = "active"
	StateTakeover     State = "takeover"
	StateShipping     State = "shipping"
	StateFailed       State = "failed"
	StateDestroyed    State = "destroyed"
)

// transitions is the closed edge set of the state machine. Destroyed is
// terminal; ready/active/takeover may go straight to destroyed ONLY on the
// explicit-discard path (req 5.4) — normal teardown passes through shipping.
var transitions = map[State]map[State]bool{
	StateProvisioning: {StateReady: true, StateFailed: true, StateDestroyed: true},
	StateReady:        {StateActive: true, StateShipping: true, StateDestroyed: true},
	StateActive:       {StateTakeover: true, StateShipping: true, StateDestroyed: true},
	StateTakeover:     {StateActive: true, StateShipping: true, StateDestroyed: true},
	StateShipping:     {StateDestroyed: true},
	StateFailed:       {},
	StateDestroyed:    {},
}

// Event type names carried onto the conversation topic + durable trace
// (mirrored in the daemon trace whitelist; the client folds dojo.* in wave 3).
const (
	EvtProvisioning = "dojo.provisioning"
	EvtReady        = "dojo.ready"
	EvtActive       = "dojo.active"
	EvtTakeover     = "dojo.takeover"
	EvtHandback     = "dojo.handback"
	EvtShipping     = "dojo.shipping"
	EvtDestroyed    = "dojo.destroyed"
	EvtFailed       = "dojo.failed"
)

// Typed session errors (req 1.3: provisioning failure or timeout degrades to
// a typed error that never hangs the turn).
var (
	ErrNotConfigured     = errors.New("dojo: not configured in this environment")
	ErrNotFound          = errors.New("dojo: session not found")
	ErrDestroyed         = errors.New("dojo: session destroyed")
	ErrProvision         = errors.New("dojo: provision failed")
	ErrProvisionTimeout  = errors.New("dojo: provision timed out")
	ErrInvalidTransition = errors.New("dojo: invalid state transition")
	ErrImageNotPinned    = errors.New("dojo: desktop image must be digest-pinned (@sha256:...), never a floating tag")
	// ErrBooting is returned to a desktop caller while the session is still
	// provisioning — the computer is turning on (req 4.1: a boot, never an
	// error-hang).
	ErrBooting = errors.New("dojo: the desktop is still booting")
	// ErrTakeoverActive rejects an AGENT action while the human holds the
	// control lease (req 4.2) — the typed result the planner reads as "the
	// human is driving; wait for handback".
	ErrTakeoverActive = errors.New("dojo: takeover_active — the user is driving the desktop; wait for them to hand control back")
	// ErrReobserveRequired rejects an agent action after a handback until the
	// agent has re-observed (req 4.3) — the human may have changed anything, so
	// acting on the pre-takeover world model is forbidden.
	ErrReobserveRequired = errors.New("dojo: reobserve_required — the user drove the desktop and may have changed anything; take a fresh look (desktop_a11y, desktop_look, or a screenshot) before acting")
	// ErrNoTakeover rejects a human input passthrough when the lease is not
	// held.
	ErrNoTakeover = errors.New("dojo: the control lease is not held — take control before sending input")
)

// DefaultImage is the digest-pinned bytebot-desktop appliance (req 2.4).
// Resolved from ghcr.io/bytebot-ai/bytebot-desktop:edge on 2026-07-24;
// upstream is archived, so upgrades are explicit re-pins of this constant (or
// NEO_DOJO_IMAGE), never floating tags.
const DefaultImage = "ghcr.io/bytebot-ai/bytebot-desktop@sha256:e974e7ac93c8f755e98b8a437e5497a63175c7096b51a348f5f1d9320598a1c9"

// bytebotdPort is the appliance daemon's REST port inside the sandbox.
const bytebotdPort = 9990

// Defaults for the env-configured caps (req 1.4).
const (
	defaultIdleTimeout      = 10 * time.Minute
	defaultMaxLifetime      = 2 * time.Hour
	defaultProvisionTimeout = 10 * time.Minute
	defaultMaxActive        = 1
	defaultPollGap          = 2 * time.Second
	defaultProbeGap         = 3 * time.Second
	execBootTimeoutSec      = 480
	execProbeTimeoutSec     = 15
)

// Emit publishes a dojo.* event onto a conversation's live topic + durable
// trace (engine.publishDojo).
type Emit func(convID, typ string, fields map[string]interface{})

// ShipFunc is the teardown ship seam (req 5.1): it ships work products and the
// browser profile home BEFORE destroy. Wave 4 provides the production shipper;
// nil means nothing is declared to ship yet and the shipping stage records
// that honestly. A ship error never blocks destruction — it is surfaced on the
// destroyed event as ship_error.
type ShipFunc func(ctx context.Context, s Session) error

// Config configures a Manager. All caps are env-configured at the serve seam.
type Config struct {
	// Image is the digest-pinned appliance ref (default DefaultImage).
	Image string
	// IdleTimeout destroys a session untouched for this long (default 10m).
	IdleTimeout time.Duration
	// MaxLifetime destroys a session this old regardless of activity,
	// bounding worst-case spend (default 2h).
	MaxLifetime time.Duration
	// ProvisionTimeout bounds sandbox create + appliance boot + readiness
	// probe end to end (default 10m; the first boot pulls ~2 GiB from ghcr).
	ProvisionTimeout time.Duration
	// MaxActive caps concurrent sessions per daemon; provisioning past it
	// tears down the least-recently-touched session first (default 1).
	MaxActive int
	// Ship is the teardown ship seam (wave 4). Nil = nothing to ship yet.
	Ship ShipFunc
	// Logf receives diagnostics; nil discards.
	Logf func(string, ...interface{})

	// pollGap/probeGap pace the provisioning polls; zero means defaults. Kept
	// unexported-style (set only by tests in-package) so prod pacing is fixed.
	pollGap  time.Duration
	probeGap time.Duration
}

// Session is the client-visible snapshot of one dojo session.
type Session struct {
	ID        string    `json:"id"`
	ConvID    string    `json:"conversation_id"`
	State     State     `json:"state"`
	SandboxID string    `json:"sandbox_id"`
	Image     string    `json:"image"`
	CreatedAt time.Time `json:"created_at"`
	TouchedAt time.Time `json:"touched_at"`
	Reason    string    `json:"reason,omitempty"`
}

// session is the Manager-owned mutable record behind a Session snapshot.
type session struct {
	id        string
	convID    string
	state     State
	sandboxID string
	image     string
	createdAt time.Time
	touchedAt time.Time
	reason    string
	// needsReobserve is set on handback (req 4.3): the agent must land a fresh
	// observation before its next mutating action, because the human may have
	// changed anything.
	needsReobserve bool
}

func (s *session) snapshot() Session {
	return Session{
		ID: s.id, ConvID: s.convID, State: s.state, SandboxID: s.sandboxID,
		Image: s.image, CreatedAt: s.createdAt, TouchedAt: s.touchedAt, Reason: s.reason,
	}
}

// Manager owns the dojo sessions for one daemon (one user). Safe for
// concurrent use.
type Manager struct {
	rw   RailwayClient
	emit Emit
	cfg  Config

	// transport carries desktop actuation (bytebotd /computer-use + raw exec)
	// into the live session's sandbox. Defaults to the exec-curl carrier over
	// rw; a test can swap it for a direct-container carrier via SetTransport.
	transport BytebotdTransport

	// now is the clock; injectable so reaper/lifetime tests are deterministic.
	now func() time.Time

	mu       sync.Mutex
	sessions map[string]*session // keyed by session id
	byConv   map[string]string   // conversation id -> live session id
}

// New builds a Manager over the Railway wire client and an event emitter.
func New(rw RailwayClient, emit Emit, cfg Config) *Manager {
	if cfg.Image == "" {
		cfg.Image = DefaultImage
	}
	if cfg.IdleTimeout <= 0 {
		cfg.IdleTimeout = defaultIdleTimeout
	}
	if cfg.MaxLifetime <= 0 {
		cfg.MaxLifetime = defaultMaxLifetime
	}
	if cfg.ProvisionTimeout <= 0 {
		cfg.ProvisionTimeout = defaultProvisionTimeout
	}
	if cfg.MaxActive <= 0 {
		cfg.MaxActive = defaultMaxActive
	}
	if cfg.pollGap <= 0 {
		cfg.pollGap = defaultPollGap
	}
	if cfg.probeGap <= 0 {
		cfg.probeGap = defaultProbeGap
	}
	if emit == nil {
		emit = func(string, string, map[string]interface{}) {}
	}
	return &Manager{
		rw:        rw,
		emit:      emit,
		cfg:       cfg,
		transport: &execCurlTransport{rw: rw},
		now:       time.Now,
		sessions:  map[string]*session{},
		byConv:    map[string]string{},
	}
}

// SetTransport swaps the desktop actuation carrier. Production keeps the
// default exec-curl carrier over the Railway client; a docker contract test
// injects a direct-container carrier so the SAME Manager drives real bytebotd
// over a local HTTP/exec path. nil leaves the default in place.
func (m *Manager) SetTransport(t BytebotdTransport) {
	if m != nil && t != nil {
		m.transport = t
	}
}

func (m *Manager) logf(format string, args ...interface{}) {
	if m.cfg.Logf != nil {
		m.cfg.Logf(format, args...)
	}
}

// Enabled reports whether the Manager can provision.
func (m *Manager) Enabled() bool {
	return m != nil && m.rw != nil
}

// Request describes the dojo session to provision.
type Request struct {
	ConvID string
}

// Provision brings up the dojo session for a conversation, or reattaches to
// the conversation's live session if one exists (sessions are reattachable
// across turns and reconnects, req 1.2). It is synchronous and bounded by
// ProvisionTimeout — it returns a typed error rather than hanging the turn.
func (m *Manager) Provision(ctx context.Context, req Request) (Session, error) {
	s, live, err := m.ensureRecord(req.ConvID)
	if err != nil {
		return Session{}, err
	}
	if !live {
		if err := m.settleProvision(ctx, s); err != nil {
			return Session{}, err
		}
	}
	m.mu.Lock()
	snap := s.snapshot()
	m.mu.Unlock()
	return snap, nil
}

// EnsureSession returns the conversation's live session, registering a fresh
// one and provisioning it in the BACKGROUND when none exists — the wave-3
// boot trigger: the caller returns a typed "booting" result immediately while
// the client watches the dojo.* boot events (req 4.1). The background boot is
// bounded by ProvisionTimeout exactly like the synchronous path.
func (m *Manager) EnsureSession(convID string) (Session, error) {
	s, live, err := m.ensureRecord(convID)
	if err != nil {
		return Session{}, err
	}
	if !live {
		go func() {
			if err := m.settleProvision(context.Background(), s); err != nil {
				m.logf("dojo: background provision %s: %v", s.id, err)
			}
		}()
	}
	m.mu.Lock()
	snap := s.snapshot()
	m.mu.Unlock()
	return snap, nil
}

// WaitReady waits up to wait for the conversation's session to leave
// provisioning, returning the latest snapshot either way — the caller reads
// its state. It never provisions; pair it with EnsureSession.
func (m *Manager) WaitReady(ctx context.Context, convID string, wait time.Duration) (Session, error) {
	deadline := m.now().Add(wait)
	for {
		snap, ok := m.SessionResultForConv(convID)
		if !ok {
			return Session{}, ErrNotFound
		}
		if snap.State != StateProvisioning {
			return snap, nil
		}
		if m.now().After(deadline) {
			return snap, nil
		}
		select {
		case <-ctx.Done():
			return snap, ctx.Err()
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// ensureRecord returns the conversation's live session (live=true, idle clock
// bumped) or registers and returns a fresh provisioning-state record
// (live=false) with the provisioning event emitted. The double-checked insert
// makes concurrent ensures converge on one session per conversation.
func (m *Manager) ensureRecord(convID string) (*session, bool, error) {
	if !m.Enabled() {
		return nil, false, ErrNotConfigured
	}
	if !strings.Contains(m.cfg.Image, "@sha256:") {
		return nil, false, ErrImageNotPinned
	}
	m.mu.Lock()
	if id, ok := m.byConv[convID]; ok {
		if s := m.sessions[id]; s != nil && s.state != StateDestroyed && s.state != StateFailed {
			s.touchedAt = m.now()
			m.mu.Unlock()
			return s, true, nil
		}
		delete(m.byConv, convID)
		delete(m.sessions, id)
	}
	m.mu.Unlock()

	m.evictForCap(context.Background())

	s := &session{
		id:        newSessionID(),
		convID:    convID,
		state:     StateProvisioning,
		image:     m.cfg.Image,
		createdAt: m.now(),
		touchedAt: m.now(),
	}
	m.mu.Lock()
	if id, ok := m.byConv[convID]; ok {
		if prev := m.sessions[id]; prev != nil && prev.state != StateDestroyed && prev.state != StateFailed {
			prev.touchedAt = m.now()
			m.mu.Unlock()
			return prev, true, nil
		}
		delete(m.sessions, id)
	}
	m.sessions[s.id] = s
	m.byConv[convID] = s.id
	m.mu.Unlock()
	m.emitState(s, EvtProvisioning, nil)
	return s, false, nil
}

// settleProvision runs the bounded provision for a freshly registered record
// and settles it to ready, or records a typed failure after cleanup.
func (m *Manager) settleProvision(ctx context.Context, s *session) error {
	pctx, cancel := context.WithTimeout(ctx, m.cfg.ProvisionTimeout)
	defer cancel()
	if err := m.provision(pctx, s); err != nil {
		terr := err
		if pctx.Err() != nil && ctx.Err() == nil {
			terr = fmt.Errorf("%w after %s: %v", ErrProvisionTimeout, m.cfg.ProvisionTimeout, err)
		} else if !errors.Is(err, ErrProvisionTimeout) {
			terr = fmt.Errorf("%w: %v", ErrProvision, err)
		}
		// Never leak a half-provisioned sandbox.
		m.mu.Lock()
		sandboxID := s.sandboxID
		m.mu.Unlock()
		if sandboxID != "" {
			if derr := m.rw.DestroySandbox(context.Background(), sandboxID); derr != nil {
				m.logf("dojo: cleanup destroy %s: %v", sandboxID, derr)
			} else {
				m.mu.Lock()
				s.sandboxID = ""
				m.mu.Unlock()
			}
		}
		m.mu.Lock()
		s.state = StateFailed
		s.reason = boundedProvisionReason(terr)
		snap := s.snapshot()
		m.mu.Unlock()
		m.emit(s.convID, EvtFailed, m.eventFields(snap, map[string]interface{}{"reason": snap.Reason}))
		return terr
	}
	return m.transition(s, StateReady, EvtReady, nil)
}

// provision creates the sandbox, boots the pinned appliance inside it via
// docker, and probes bytebotd until it answers. Every step is bounded by ctx.
func (m *Manager) provision(ctx context.Context, s *session) error {
	idleMin := int((m.cfg.IdleTimeout + time.Minute - 1) / time.Minute)
	sb, err := m.rw.CreateSandbox(ctx, CreateOpts{
		IdleTimeoutMinutes: idleMin,
		Network:            NetworkIsolated,
	})
	if err != nil {
		return fmt.Errorf("create sandbox: %w", err)
	}
	m.mu.Lock()
	s.sandboxID = sb.ID
	m.mu.Unlock()

	for sb.Status != SandboxRunning {
		if sb.Status == SandboxFailed || sb.Status == SandboxDestroying || sb.Status == SandboxDestroyed {
			return fmt.Errorf("sandbox %s entered %s while provisioning", sb.ID, sb.Status)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(m.cfg.pollGap):
		}
		if sb, err = m.rw.GetSandbox(ctx, s.sandboxID); err != nil {
			return fmt.Errorf("poll sandbox: %w", err)
		}
	}

	boot := fmt.Sprintf(
		"docker pull -q %s && (docker rm -f dojo >/dev/null 2>&1 || true) && docker run -d --name dojo -p %d:%d %s",
		s.image, bytebotdPort, bytebotdPort, s.image)
	res, err := m.rw.ExecSandbox(ctx, s.sandboxID, boot, execBootTimeoutSec)
	if err != nil {
		return fmt.Errorf("boot appliance: %w", err)
	}
	if res.ExitCode != 0 || res.TimedOut {
		return fmt.Errorf("boot appliance: exit=%d timedOut=%v: %s", res.ExitCode, res.TimedOut, strings.TrimSpace(res.Stderr))
	}

	probe := fmt.Sprintf(
		`curl -s -o /dev/null -w '%%{http_code}' --max-time 10 -X POST http://localhost:%d/computer-use -H 'Content-Type: application/json' -d '{"action":"cursor_position"}'`,
		bytebotdPort)
	for {
		res, err := m.rw.ExecSandbox(ctx, s.sandboxID, probe, execProbeTimeoutSec)
		if err == nil {
			code := strings.TrimSpace(res.Stdout)
			if len(code) == 3 && code[0] == '2' {
				return nil
			}
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("bytebotd did not answer on port %d: %w", bytebotdPort, ctx.Err())
		case <-time.After(m.cfg.probeGap):
		}
	}
}

// Get returns the snapshot of a session by id.
func (m *Manager) Get(id string) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[id]
	if s == nil {
		return Session{}, ErrNotFound
	}
	return s.snapshot(), nil
}

// SessionForConv returns the live session for a conversation, if any.
func (m *Manager) SessionForConv(convID string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byConv[convID]
	if !ok {
		return Session{}, false
	}
	s := m.sessions[id]
	if s == nil || s.state == StateDestroyed || s.state == StateFailed {
		return Session{}, false
	}
	return s.snapshot(), true
}

// SessionResultForConv returns a conversation's latest non-destroyed result,
// including a failed boot so clients can render its bounded reason. A retry
// still replaces the failed record through ensureRecord.
func (m *Manager) SessionResultForConv(convID string) (Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byConv[convID]
	if !ok {
		return Session{}, false
	}
	s := m.sessions[id]
	if s == nil || s.state == StateDestroyed {
		return Session{}, false
	}
	return s.snapshot(), true
}

// SessionForConvOrLive resolves a conversation's session, falling back to THE
// live session when the conversation has none bound. With MaxActive=1 (the
// default) the user has exactly one computer — every conversation's panel
// shows and controls it, whichever conversation booted it. With a larger cap
// the fallback is disabled and bindings stay strictly per-conversation.
func (m *Manager) SessionForConvOrLive(convID string) (Session, bool) {
	if s, ok := m.SessionForConv(convID); ok {
		return s, true
	}
	if m == nil || m.cfg.MaxActive != 1 {
		return Session{}, false
	}
	s, err := m.liveSession()
	if err != nil {
		return Session{}, false
	}
	return s, true
}

// Reattach verifies a session's sandbox is still alive, bumps its idle clock,
// and returns it. A dead sandbox is torn down honestly and reported as
// ErrDestroyed — the caller re-provisions rather than acting on a corpse.
func (m *Manager) Reattach(ctx context.Context, id string) (Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return Session{}, ErrNotFound
	}
	if s.state == StateDestroyed || s.state == StateFailed {
		m.mu.Unlock()
		return Session{}, ErrDestroyed
	}
	sandboxID := s.sandboxID
	m.mu.Unlock()

	sb, err := m.rw.GetSandbox(ctx, sandboxID)
	if err != nil || sb.Status != SandboxRunning {
		reason := "sandbox died"
		if err != nil {
			reason = fmt.Sprintf("sandbox unreachable: %v", err)
		} else if sb.Status != "" {
			reason = "sandbox " + strings.ToLower(sb.Status)
		}
		m.emitState(s, EvtFailed, map[string]interface{}{"reason": reason})
		m.finish(s, "sandbox_dead", "")
		return Session{}, fmt.Errorf("%w: %s", ErrDestroyed, reason)
	}
	m.mu.Lock()
	s.touchedAt = m.now()
	snap := s.snapshot()
	m.mu.Unlock()
	return snap, nil
}

// Touch bumps a session's idle clock.
func (m *Manager) Touch(id string) {
	m.mu.Lock()
	if s := m.sessions[id]; s != nil {
		s.touchedAt = m.now()
	}
	m.mu.Unlock()
}

// Activate marks the agent as driving (ready -> active). Activating an
// already-active session just bumps the idle clock. Activating during a
// takeover is refused — the human holds the controls.
func (m *Manager) Activate(id string) (Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return Session{}, ErrNotFound
	}
	switch s.state {
	case StateActive:
		s.touchedAt = m.now()
		snap := s.snapshot()
		m.mu.Unlock()
		return snap, nil
	case StateReady:
		s.state = StateActive
		s.touchedAt = m.now()
		snap := s.snapshot()
		m.mu.Unlock()
		m.emit(s.convID, EvtActive, m.eventFields(snap, nil))
		return snap, nil
	case StateDestroyed:
		m.mu.Unlock()
		return Session{}, ErrDestroyed
	default:
		st := s.state
		m.mu.Unlock()
		return Session{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, st, StateActive)
	}
}

// Takeover hands the control lease to the human (active -> takeover, req 4.2).
// While held, every AGENT desktop call is rejected with ErrTakeoverActive; the
// human's input passes through via TakeoverInput.
func (m *Manager) Takeover(id string) (Session, error) {
	return m.step(id, StateActive, StateTakeover, EvtTakeover)
}

// Handback returns control to the agent (takeover -> active) and arms the
// re-observation contract (req 4.3): the agent's next mutating action is
// rejected until a fresh observation lands — it never acts on a pre-takeover
// world model. The flag is set under the SAME lock as the transition so no
// agent call can slip between them.
func (m *Manager) Handback(id string) (Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return Session{}, ErrNotFound
	}
	if s.state == StateDestroyed || s.state == StateFailed {
		m.mu.Unlock()
		return Session{}, ErrDestroyed
	}
	if s.state != StateTakeover {
		st := s.state
		m.mu.Unlock()
		return Session{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, st, StateActive)
	}
	s.state = StateActive
	s.needsReobserve = true
	s.touchedAt = m.now()
	snap := s.snapshot()
	m.mu.Unlock()
	m.emit(s.convID, EvtHandback, m.eventFields(snap, nil))
	return snap, nil
}

// TakeoverConv hands the control lease to the human for a conversation's live
// session. Grabbing a booted-but-idle desktop (ready) first marks it active so
// the machine's edges stay closed.
func (m *Manager) TakeoverConv(convID string) (Session, error) {
	snap, ok := m.SessionForConvOrLive(convID)
	if !ok {
		return Session{}, ErrNoSession
	}
	if snap.State == StateProvisioning {
		return Session{}, ErrBooting
	}
	if snap.State == StateReady {
		if activated, err := m.Activate(snap.ID); err == nil {
			snap = activated
		}
	}
	return m.Takeover(snap.ID)
}

// HandbackConv returns control to the agent for a conversation's live session.
func (m *Manager) HandbackConv(convID string) (Session, error) {
	snap, ok := m.SessionForConvOrLive(convID)
	if !ok {
		return Session{}, ErrNoSession
	}
	return m.Handback(snap.ID)
}

func (m *Manager) step(id string, from, to State, evt string) (Session, error) {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return Session{}, ErrNotFound
	}
	if s.state == StateDestroyed || s.state == StateFailed {
		m.mu.Unlock()
		return Session{}, ErrDestroyed
	}
	if s.state != from {
		st := s.state
		m.mu.Unlock()
		return Session{}, fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, st, to)
	}
	s.state = to
	s.touchedAt = m.now()
	snap := s.snapshot()
	m.mu.Unlock()
	m.emit(s.convID, evt, m.eventFields(snap, nil))
	return snap, nil
}

// Teardown runs the full teardown pipeline for a session: shipping (the ship
// seam runs, its failure reported honestly, never silently dropped) then
// destroy. Idempotent — tearing down a session already shipping or destroyed
// is a no-op (req 1.3).
func (m *Manager) Teardown(ctx context.Context, id, reason string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return nil
	}
	if s.state == StateShipping || s.state == StateDestroyed || s.state == StateFailed {
		m.mu.Unlock()
		return nil
	}
	s.state = StateShipping
	snap := s.snapshot()
	m.mu.Unlock()
	m.emit(s.convID, EvtShipping, m.eventFields(snap, map[string]interface{}{"reason": reason}))

	shipErr := ""
	if m.cfg.Ship != nil {
		if err := m.cfg.Ship(ctx, snap); err != nil {
			shipErr = err.Error()
			m.logf("dojo: ship %s: %v", s.id, err)
		}
	}
	if s.sandboxID != "" {
		if err := m.rw.DestroySandbox(ctx, s.sandboxID); err != nil {
			m.logf("dojo: destroy %s: %v", s.sandboxID, err)
		}
	}
	m.finish(s, reason, shipErr)
	return nil
}

// Discard destroys a session WITHOUT shipping — only the explicit user
// discard path calls this (req 5.4). Idempotent.
func (m *Manager) Discard(ctx context.Context, id string) error {
	m.mu.Lock()
	s := m.sessions[id]
	if s == nil {
		m.mu.Unlock()
		return nil
	}
	if s.state == StateDestroyed || s.state == StateFailed {
		m.mu.Unlock()
		return nil
	}
	m.mu.Unlock()
	if s.sandboxID != "" {
		if err := m.rw.DestroySandbox(ctx, s.sandboxID); err != nil {
			m.logf("dojo: discard destroy %s: %v", s.sandboxID, err)
		}
	}
	m.finish(s, "discard", "")
	return nil
}

// finish moves a session to destroyed, unindexes it, and emits the terminal
// event. Safe to call from any state; repeat calls are no-ops.
func (m *Manager) finish(s *session, reason, shipErr string) {
	m.mu.Lock()
	if s.state == StateDestroyed {
		m.mu.Unlock()
		return
	}
	s.state = StateDestroyed
	if m.byConv[s.convID] == s.id {
		delete(m.byConv, s.convID)
	}
	snap := s.snapshot()
	m.mu.Unlock()
	fields := map[string]interface{}{"reason": reason}
	if shipErr != "" {
		fields["ship_error"] = shipErr
	}
	m.emit(s.convID, EvtDestroyed, m.eventFields(snap, fields))
}

// transition applies a validated state-machine edge and emits its event.
func (m *Manager) transition(s *session, to State, evt string, fields map[string]interface{}) error {
	m.mu.Lock()
	if !transitions[s.state][to] {
		err := fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, s.state, to)
		m.mu.Unlock()
		return err
	}
	s.state = to
	s.touchedAt = m.now()
	snap := s.snapshot()
	m.mu.Unlock()
	m.emit(s.convID, evt, m.eventFields(snap, fields))
	return nil
}

func (m *Manager) emitState(s *session, evt string, fields map[string]interface{}) {
	m.mu.Lock()
	snap := s.snapshot()
	m.mu.Unlock()
	m.emit(s.convID, evt, m.eventFields(snap, fields))
}

func (m *Manager) eventFields(snap Session, extra map[string]interface{}) map[string]interface{} {
	f := map[string]interface{}{
		"session_id": snap.ID,
		"state":      string(snap.State),
	}
	if snap.SandboxID != "" {
		f["sandbox_id"] = snap.SandboxID
	}
	for k, v := range extra {
		f[k] = v
	}
	return f
}

func boundedProvisionReason(err error) string {
	message := strings.TrimSpace(err.Error())
	for _, replacement := range [][2]string{
		{"RAILWAY_API_TOKEN", "service credential"},
		{"Railway", "computer service"},
		{"railway", "computer service"},
	} {
		message = strings.ReplaceAll(message, replacement[0], replacement[1])
	}
	if len(message) > 300 {
		message = message[:300]
	}
	if message == "" {
		return "The computer service could not create the sandbox."
	}
	return message
}

// evictForCap tears down least-recently-touched sessions (through the ship
// path — eviction is not a discard) until a new one fits under MaxActive.
func (m *Manager) evictForCap(ctx context.Context) {
	for {
		m.mu.Lock()
		live := 0
		var oldest *session
		for _, s := range m.sessions {
			if s.state == StateDestroyed || s.state == StateShipping || s.state == StateFailed {
				continue
			}
			live++
			if oldest == nil || s.touchedAt.Before(oldest.touchedAt) {
				oldest = s
			}
		}
		m.mu.Unlock()
		if live < m.cfg.MaxActive || oldest == nil {
			return
		}
		_ = m.Teardown(ctx, oldest.id, "evicted_for_cap")
	}
}

// StartReaper enforces the idle-timeout and hard-lifetime caps until ctx ends.
func (m *Manager) StartReaper(ctx context.Context) {
	t := time.NewTicker(30 * time.Second)
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
	now := m.now()
	type victim struct {
		id     string
		reason string
	}
	var victims []victim
	m.mu.Lock()
	for _, s := range m.sessions {
		if s.state == StateDestroyed || s.state == StateShipping || s.state == StateProvisioning || s.state == StateFailed {
			continue
		}
		switch {
		case now.Sub(s.createdAt) > m.cfg.MaxLifetime:
			victims = append(victims, victim{s.id, "max_lifetime"})
		case now.Sub(s.touchedAt) > m.cfg.IdleTimeout:
			victims = append(victims, victim{s.id, "idle_timeout"})
		}
	}
	m.mu.Unlock()
	for _, v := range victims {
		m.logf("dojo: reaping session %s (%s)", v.id, v.reason)
		_ = m.Teardown(ctx, v.id, v.reason)
	}
}

// Close tears down every live session (graceful daemon shutdown).
func (m *Manager) Close() {
	m.mu.Lock()
	var ids []string
	for id, s := range m.sessions {
		if s.state != StateDestroyed && s.state != StateFailed {
			ids = append(ids, id)
		}
	}
	m.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for _, id := range ids {
		_ = m.Teardown(ctx, id, "shutdown")
	}
}

func newSessionID() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("dojo-%d", time.Now().UnixNano())
	}
	return "dojo-" + hex.EncodeToString(b[:])
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
