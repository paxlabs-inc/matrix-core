// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package dojo

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
)

// railwayFixture is a recorded httptest stand-in for the LIVE Railway sandbox
// GraphQL API (schemas round-tripped against the real endpoint 2026-07-24).
// The real NewRailway client runs its full HTTP/GraphQL/parse path against it
// — it is not a stub of the client, and the Manager under test is the real
// Manager.
type railwayFixture struct {
	mu sync.Mutex
	// ops records every GraphQL operation name in arrival order.
	ops []string
	// destroyed records sandbox ids passed to SandboxDestroy.
	destroyed []string
	// created counts sandboxCreate calls (ids minted as sb-<n>).
	created int

	// status returned by the sandbox query; "" means RUNNING.
	queryStatus string
	// createStatus is the status returned by sandboxCreate; "" means RUNNING
	// (so provisioning skips the poll loop by default).
	createStatus string
	// failCreate makes sandboxCreate answer a GraphQL error.
	failCreate bool
	// probeCode is the stdout of the curl readiness probe; "" means "201".
	probeCode string
	// bootExit is the exit code of the docker boot exec.
	bootExit int
}

func (f *railwayFixture) record(op string) {
	f.mu.Lock()
	f.ops = append(f.ops, op)
	f.mu.Unlock()
}

func (f *railwayFixture) opCount(op string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, o := range f.ops {
		if o == op {
			n++
		}
	}
	return n
}

func (f *railwayFixture) server(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(raw, &body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(body.Query, "sandboxCreate"):
			f.record("create")
			f.mu.Lock()
			f.created++
			id := fmt.Sprintf("sb-%d", f.created)
			status := f.createStatus
			fail := f.failCreate
			f.mu.Unlock()
			if fail {
				_, _ = io.WriteString(w, `{"errors":[{"message":"environment sandbox quota exceeded"}]}`)
				return
			}
			if status == "" {
				status = SandboxRunning
			}
			fmt.Fprintf(w, `{"data":{"sandboxCreate":{"id":%q,"status":%q,"region":"us-west2","createdAt":"2026-07-24T00:00:00Z"}}}`, id, status)
		case strings.Contains(body.Query, "sandboxExec"):
			f.record("exec")
			cmd, _ := body.Variables["command"].(string)
			if strings.Contains(cmd, "docker pull") {
				f.mu.Lock()
				exit := f.bootExit
				f.mu.Unlock()
				fmt.Fprintf(w, `{"data":{"sandboxExec":{"stdout":"booted","stderr":"","exitCode":%d,"timedOut":false,"truncated":false}}}`, exit)
				return
			}
			f.mu.Lock()
			code := f.probeCode
			f.mu.Unlock()
			if code == "" {
				code = "201"
			}
			fmt.Fprintf(w, `{"data":{"sandboxExec":{"stdout":%q,"stderr":"","exitCode":0,"timedOut":false,"truncated":false}}}`, code)
		case strings.Contains(body.Query, "sandboxDestroy"):
			f.record("destroy")
			id, _ := body.Variables["id"].(string)
			f.mu.Lock()
			f.destroyed = append(f.destroyed, id)
			f.mu.Unlock()
			fmt.Fprintf(w, `{"data":{"sandboxDestroy":{"id":%q,"status":"DESTROYING"}}}`, id)
		case strings.Contains(body.Query, "query Sandbox"):
			f.record("get")
			id, _ := body.Variables["id"].(string)
			f.mu.Lock()
			status := f.queryStatus
			f.mu.Unlock()
			if status == "" {
				status = SandboxRunning
			}
			fmt.Fprintf(w, `{"data":{"sandbox":{"id":%q,"status":%q,"region":"us-west2","createdAt":"2026-07-24T00:00:00Z"}}}`, id, status)
		default:
			t.Errorf("fixture got unexpected query: %s", body.Query)
			_, _ = io.WriteString(w, `{"errors":[{"message":"unexpected operation"}]}`)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// eventRecorder captures emitted dojo.* events in order.
type eventRecorder struct {
	mu     sync.Mutex
	events []recordedEvent
}

type recordedEvent struct {
	convID string
	typ    string
	fields map[string]interface{}
}

func (r *eventRecorder) emit(convID, typ string, fields map[string]interface{}) {
	r.mu.Lock()
	r.events = append(r.events, recordedEvent{convID, typ, fields})
	r.mu.Unlock()
}

func (r *eventRecorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.typ
	}
	return out
}

func (r *eventRecorder) last() recordedEvent {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.events) == 0 {
		return recordedEvent{}
	}
	return r.events[len(r.events)-1]
}

func newTestManager(t *testing.T, f *railwayFixture, cfg Config) (*Manager, *eventRecorder) {
	t.Helper()
	srv := f.server(t)
	rw := NewRailway(RailwayConfig{Token: "test-token", EnvironmentID: "env-1", Endpoint: srv.URL})
	rec := &eventRecorder{}
	if cfg.pollGap == 0 {
		cfg.pollGap = 5 * time.Millisecond
	}
	if cfg.probeGap == 0 {
		cfg.probeGap = 5 * time.Millisecond
	}
	return New(rw, rec.emit, cfg), rec
}

func TestProvisionLifecycleEmitsTypedStates(t *testing.T) {
	f := &railwayFixture{}
	m, rec := newTestManager(t, f, Config{})

	s, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if s.State != StateReady {
		t.Fatalf("state = %s, want ready", s.State)
	}
	if s.SandboxID != "sb-1" {
		t.Fatalf("sandbox id = %q", s.SandboxID)
	}
	if s.Image != DefaultImage {
		t.Fatalf("image = %q, want the pinned default", s.Image)
	}
	if !strings.Contains(DefaultImage, "@sha256:") {
		t.Fatalf("DefaultImage is not digest-pinned: %s", DefaultImage)
	}
	got := rec.types()
	want := []string{EvtProvisioning, EvtReady}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if f.opCount("create") != 1 {
		t.Fatalf("create calls = %d", f.opCount("create"))
	}
	if f.opCount("exec") < 2 {
		t.Fatalf("exec calls = %d, want boot + probe", f.opCount("exec"))
	}
}

func TestProvisionReattachesLiveSessionForConversation(t *testing.T) {
	f := &railwayFixture{}
	m, _ := newTestManager(t, f, Config{MaxActive: 2})

	a, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	b, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("re-Provision: %v", err)
	}
	if a.ID != b.ID || b.SandboxID != a.SandboxID {
		t.Fatalf("second Provision minted a new session: %v vs %v", a.ID, b.ID)
	}
	if f.opCount("create") != 1 {
		t.Fatalf("create calls = %d, want 1 (reattach, not re-provision)", f.opCount("create"))
	}
}

func TestReattachByIDVerifiesSandboxAndTouches(t *testing.T) {
	f := &railwayFixture{}
	m, _ := newTestManager(t, f, Config{})
	s, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	base := time.Now()
	m.now = func() time.Time { return base.Add(5 * time.Minute) }
	got, err := m.Reattach(context.Background(), s.ID)
	if err != nil {
		t.Fatalf("Reattach: %v", err)
	}
	if got.ID != s.ID {
		t.Fatalf("reattached wrong session %q", got.ID)
	}
	if !got.TouchedAt.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("Reattach did not touch: %v", got.TouchedAt)
	}
	if _, err := m.Reattach(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Reattach(missing) = %v, want ErrNotFound", err)
	}
}

func TestReattachDeadSandboxFailsHonestly(t *testing.T) {
	f := &railwayFixture{}
	m, rec := newTestManager(t, f, Config{})
	s, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	f.mu.Lock()
	f.queryStatus = SandboxDestroyed
	f.mu.Unlock()

	if _, err := m.Reattach(context.Background(), s.ID); !errors.Is(err, ErrDestroyed) {
		t.Fatalf("Reattach dead = %v, want ErrDestroyed", err)
	}
	types := rec.types()
	if types[len(types)-2] != EvtFailed || types[len(types)-1] != EvtDestroyed {
		t.Fatalf("events = %v, want ...failed,destroyed", types)
	}
	if _, ok := m.SessionForConv("conv-1"); ok {
		t.Fatal("dead session still indexed for conversation")
	}
}

func TestActivateTakeoverHandbackTransitions(t *testing.T) {
	f := &railwayFixture{}
	m, rec := newTestManager(t, f, Config{})
	s, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	// Takeover before the agent drives is an invalid edge.
	if _, err := m.Takeover(s.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Takeover from ready = %v, want ErrInvalidTransition", err)
	}
	if _, err := m.Handback(s.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Handback from ready = %v, want ErrInvalidTransition", err)
	}

	a, err := m.Activate(s.ID)
	if err != nil || a.State != StateActive {
		t.Fatalf("Activate = %v %v", a.State, err)
	}
	// Idempotent while active.
	if a2, err := m.Activate(s.ID); err != nil || a2.State != StateActive {
		t.Fatalf("re-Activate = %v %v", a2.State, err)
	}

	tk, err := m.Takeover(s.ID)
	if err != nil || tk.State != StateTakeover {
		t.Fatalf("Takeover = %v %v", tk.State, err)
	}
	// The agent cannot re-activate while the human holds the controls.
	if _, err := m.Activate(s.ID); !errors.Is(err, ErrInvalidTransition) {
		t.Fatalf("Activate during takeover = %v, want ErrInvalidTransition", err)
	}
	hb, err := m.Handback(s.ID)
	if err != nil || hb.State != StateActive {
		t.Fatalf("Handback = %v %v", hb.State, err)
	}

	got := rec.types()
	want := []string{EvtProvisioning, EvtReady, EvtActive, EvtTakeover, EvtHandback}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
}

func TestTeardownShipsThenDestroysIdempotently(t *testing.T) {
	f := &railwayFixture{}
	var shipped []string
	var mu sync.Mutex
	m, rec := newTestManager(t, f, Config{
		Ship: func(ctx context.Context, s Session) error {
			mu.Lock()
			shipped = append(shipped, s.SandboxID)
			mu.Unlock()
			return nil
		},
	})
	s, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}

	if err := m.Teardown(context.Background(), s.ID, "task_complete"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	// Idempotent: repeat teardown and discard are no-ops.
	if err := m.Teardown(context.Background(), s.ID, "task_complete"); err != nil {
		t.Fatalf("repeat Teardown: %v", err)
	}
	if err := m.Discard(context.Background(), s.ID); err != nil {
		t.Fatalf("Discard after destroy: %v", err)
	}

	mu.Lock()
	nShipped := len(shipped)
	mu.Unlock()
	if nShipped != 1 || shipped[0] != s.SandboxID {
		t.Fatalf("ship ran %d times (%v), want once for %s", nShipped, shipped, s.SandboxID)
	}
	if f.opCount("destroy") != 1 {
		t.Fatalf("destroy calls = %d, want exactly 1", f.opCount("destroy"))
	}
	got := rec.types()
	want := []string{EvtProvisioning, EvtReady, EvtShipping, EvtDestroyed}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", got, want)
	}
	if st, _ := m.Get(s.ID); st.State != StateDestroyed {
		t.Fatalf("state = %s, want destroyed", st.State)
	}
}

func TestShipFailureIsReportedNeverSilent(t *testing.T) {
	f := &railwayFixture{}
	m, rec := newTestManager(t, f, Config{
		Ship: func(ctx context.Context, s Session) error {
			return errors.New("volume unreachable")
		},
	})
	s, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := m.Teardown(context.Background(), s.ID, "idle_timeout"); err != nil {
		t.Fatalf("Teardown: %v", err)
	}
	last := rec.last()
	if last.typ != EvtDestroyed {
		t.Fatalf("last event = %s", last.typ)
	}
	if se, _ := last.fields["ship_error"].(string); !strings.Contains(se, "volume unreachable") {
		t.Fatalf("ship_error not surfaced on destroyed event: %v", last.fields)
	}
	// The sandbox is still destroyed — a failed ship never blocks teardown.
	if f.opCount("destroy") != 1 {
		t.Fatalf("destroy calls = %d", f.opCount("destroy"))
	}
}

func TestDiscardSkipsShip(t *testing.T) {
	f := &railwayFixture{}
	shipCalls := 0
	m, rec := newTestManager(t, f, Config{
		Ship: func(ctx context.Context, s Session) error { shipCalls++; return nil },
	})
	s, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	if err := m.Discard(context.Background(), s.ID); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if shipCalls != 0 {
		t.Fatalf("ship ran on discard (%d calls)", shipCalls)
	}
	got := rec.types()
	want := []string{EvtProvisioning, EvtReady, EvtDestroyed}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v (no shipping stage on discard)", got, want)
	}
	if last := rec.last(); last.fields["reason"] != "discard" {
		t.Fatalf("destroyed reason = %v, want discard", last.fields["reason"])
	}
}

func TestProvisionFailureIsTypedAndLeaksNothing(t *testing.T) {
	f := &railwayFixture{failCreate: true}
	m, rec := newTestManager(t, f, Config{})

	_, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if !errors.Is(err, ErrProvision) {
		t.Fatalf("err = %v, want ErrProvision", err)
	}
	types := rec.types()
	want := []string{EvtProvisioning, EvtFailed}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("events = %v, want %v", types, want)
	}
	if _, ok := m.SessionForConv("conv-1"); ok {
		t.Fatal("failed session reported as live")
	}
	failed, ok := m.SessionResultForConv("conv-1")
	if !ok || failed.State != StateFailed || failed.Reason == "" {
		t.Fatalf("failed result = %+v ok=%v", failed, ok)
	}
}

func TestProvisionFailureReasonHidesInfrastructureLabels(t *testing.T) {
	reason := boundedProvisionReason(errors.New("dojo: Railway rejected RAILWAY_API_TOKEN"))
	if strings.Contains(strings.ToLower(reason), "railway") || strings.Contains(reason, "RAILWAY_API_TOKEN") {
		t.Fatalf("infrastructure label escaped: %q", reason)
	}
}

func TestProvisionBootFailureDestroysSandbox(t *testing.T) {
	f := &railwayFixture{bootExit: 125}
	m, _ := newTestManager(t, f, Config{})

	_, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if !errors.Is(err, ErrProvision) {
		t.Fatalf("err = %v, want ErrProvision", err)
	}
	if f.opCount("destroy") != 1 {
		t.Fatalf("destroy calls = %d, want 1 (no leaked sandbox)", f.opCount("destroy"))
	}
}

func TestProvisionTimeoutNeverHangsTheTurn(t *testing.T) {
	f := &railwayFixture{createStatus: SandboxCreating, queryStatus: SandboxCreating}
	m, rec := newTestManager(t, f, Config{ProvisionTimeout: 150 * time.Millisecond})

	start := time.Now()
	_, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	elapsed := time.Since(start)
	if !errors.Is(err, ErrProvisionTimeout) {
		t.Fatalf("err = %v, want ErrProvisionTimeout", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Provision blocked %s past its budget", elapsed)
	}
	if f.opCount("destroy") != 1 {
		t.Fatalf("destroy calls = %d, want 1 (half-provisioned sandbox reclaimed)", f.opCount("destroy"))
	}
	types := rec.types()
	if types[len(types)-1] != EvtFailed {
		t.Fatalf("events = %v, want a typed dojo.failed", types)
	}
}

func TestReaperEnforcesIdleAndHardLifetime(t *testing.T) {
	f := &railwayFixture{}
	m, rec := newTestManager(t, f, Config{IdleTimeout: 10 * time.Minute, MaxLifetime: time.Hour, MaxActive: 2})
	base := time.Now()
	m.now = func() time.Time { return base }

	idle, err := m.Provision(context.Background(), Request{ConvID: "conv-idle"})
	if err != nil {
		t.Fatalf("Provision idle: %v", err)
	}
	old, err := m.Provision(context.Background(), Request{ConvID: "conv-old"})
	if err != nil {
		t.Fatalf("Provision old: %v", err)
	}

	// 20 minutes on: conv-idle untouched (reaped for idle), conv-old touched.
	m.now = func() time.Time { return base.Add(20 * time.Minute) }
	m.Touch(old.ID)
	m.reapOnce(context.Background())
	if st, _ := m.Get(idle.ID); st.State != StateDestroyed {
		t.Fatalf("idle session state = %s, want destroyed", st.State)
	}
	if st, _ := m.Get(old.ID); st.State == StateDestroyed {
		t.Fatal("touched session reaped early")
	}

	// 61 minutes past create: hard lifetime fires even for a touched session.
	m.now = func() time.Time { return base.Add(61 * time.Minute) }
	m.Touch(old.ID)
	m.reapOnce(context.Background())
	if st, _ := m.Get(old.ID); st.State != StateDestroyed {
		t.Fatalf("over-lifetime session state = %s, want destroyed", st.State)
	}

	reasons := map[string]bool{}
	rec.mu.Lock()
	for _, e := range rec.events {
		if e.typ == EvtDestroyed {
			if r, _ := e.fields["reason"].(string); r != "" {
				reasons[r] = true
			}
		}
	}
	rec.mu.Unlock()
	if !reasons["idle_timeout"] || !reasons["max_lifetime"] {
		t.Fatalf("teardown reasons = %v, want idle_timeout and max_lifetime", reasons)
	}
}

func TestMaxActiveEvictsOldestThroughShipPath(t *testing.T) {
	f := &railwayFixture{}
	shipCalls := 0
	m, _ := newTestManager(t, f, Config{MaxActive: 1, Ship: func(ctx context.Context, s Session) error {
		shipCalls++
		return nil
	}})

	a, err := m.Provision(context.Background(), Request{ConvID: "conv-a"})
	if err != nil {
		t.Fatalf("Provision a: %v", err)
	}
	b, err := m.Provision(context.Background(), Request{ConvID: "conv-b"})
	if err != nil {
		t.Fatalf("Provision b: %v", err)
	}
	if st, _ := m.Get(a.ID); st.State != StateDestroyed {
		t.Fatalf("evicted session state = %s, want destroyed", st.State)
	}
	if st, _ := m.Get(b.ID); st.State != StateReady {
		t.Fatalf("new session state = %s, want ready", st.State)
	}
	if shipCalls != 1 {
		t.Fatalf("eviction bypassed the ship path (%d ship calls)", shipCalls)
	}
}

func TestFloatingTagImageRefused(t *testing.T) {
	f := &railwayFixture{}
	m, _ := newTestManager(t, f, Config{Image: "ghcr.io/bytebot-ai/bytebot-desktop:edge"})
	_, err := m.Provision(context.Background(), Request{ConvID: "conv-1"})
	if !errors.Is(err, ErrImageNotPinned) {
		t.Fatalf("err = %v, want ErrImageNotPinned", err)
	}
	if f.opCount("create") != 0 {
		t.Fatal("sandbox created from a floating tag")
	}
}

func TestNotConfiguredIsTyped(t *testing.T) {
	m := New(nil, nil, Config{})
	if _, err := m.Provision(context.Background(), Request{ConvID: "c"}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
}

// TestLiveDojoRoundTrip provisions a REAL Railway sandbox from the pinned
// bytebot-desktop digest, reattaches, and destroys it. Env-gated on the
// Railway credential plus an explicit opt-in (it spends real minutes of a
// real sandbox and pulls ~2 GiB inside it).
func TestLiveDojoRoundTrip(t *testing.T) {
	token := os.Getenv("RAILWAY_API_TOKEN")
	envID := os.Getenv("RAILWAY_ENVIRONMENT_ID")
	if token == "" || envID == "" || os.Getenv("NEO_DOJO_LIVE") == "" {
		t.Skip("set RAILWAY_API_TOKEN, RAILWAY_ENVIRONMENT_ID and NEO_DOJO_LIVE=1 for the live round-trip")
	}
	rw := NewRailway(RailwayConfig{Token: token, EnvironmentID: envID})
	rec := &eventRecorder{}
	m := New(rw, rec.emit, Config{ProvisionTimeout: 12 * time.Minute})

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	s, err := m.Provision(ctx, Request{ConvID: "live-conv"})
	if err != nil {
		t.Fatalf("live Provision: %v", err)
	}
	t.Logf("live session %s sandbox %s ready", s.ID, s.SandboxID)
	defer func() { _ = m.Discard(context.Background(), s.ID) }()

	if got, err := m.Reattach(ctx, s.ID); err != nil || got.SandboxID != s.SandboxID {
		t.Fatalf("live Reattach: %v %v", got, err)
	}
	if _, err := m.Activate(s.ID); err != nil {
		t.Fatalf("live Activate: %v", err)
	}

	// The appliance must actually answer: real bytebotd screenshot through
	// the real sandbox exec wire.
	res, err := rw.ExecSandbox(ctx, s.SandboxID,
		`curl -s -X POST http://localhost:9990/computer-use -H 'Content-Type: application/json' -d '{"action":"screenshot"}' | head -c 64`, 30)
	if err != nil {
		t.Fatalf("live screenshot exec: %v", err)
	}
	if !strings.Contains(res.Stdout, `"image"`) {
		t.Fatalf("live screenshot answer = %q, want an image payload", res.Stdout)
	}

	if err := m.Teardown(ctx, s.ID, "live_test_complete"); err != nil {
		t.Fatalf("live Teardown: %v", err)
	}
	// Destruction is eventually consistent on the live API (the first read
	// after sandboxDestroy can still answer RUNNING): poll briefly.
	deadline := time.Now().Add(90 * time.Second)
	for {
		sb, err := rw.GetSandbox(ctx, s.SandboxID)
		if err != nil || sb.Status != SandboxRunning {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("sandbox %s still RUNNING %s after teardown", s.SandboxID, 90*time.Second)
		}
		time.Sleep(3 * time.Second)
	}
	types := rec.types()
	if types[len(types)-1] != EvtDestroyed {
		t.Fatalf("events = %v, want terminal dojo.destroyed", types)
	}
}

// TestEnsureSessionBootsInBackground proves the wave-3 boot trigger: the first
// EnsureSession returns a provisioning-state snapshot immediately (the client
// renders the computer turning on) while the REAL provision path runs in the
// background and settles to ready with its events in order.
func TestEnsureSessionBootsInBackground(t *testing.T) {
	f := &railwayFixture{}
	m, rec := newTestManager(t, f, Config{})

	s, err := m.EnsureSession("conv-1")
	if err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	if s.State != StateProvisioning {
		t.Fatalf("immediate state = %s, want provisioning", s.State)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		snap, ok := m.SessionForConv("conv-1")
		if ok && snap.State == StateReady {
			if snap.ID != s.ID {
				t.Fatalf("settled session %q != ensured %q", snap.ID, s.ID)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session never settled to ready; last = %+v", snap)
		}
		time.Sleep(5 * time.Millisecond)
	}
	got := rec.types()
	if len(got) < 2 || got[0] != EvtProvisioning || got[len(got)-1] != EvtReady {
		t.Fatalf("events = %v, want provisioning … ready", got)
	}
	if f.opCount("create") != 1 {
		t.Fatalf("create calls = %d, want 1", f.opCount("create"))
	}

	// A second ensure reattaches the settled session — no second boot.
	again, err := m.EnsureSession("conv-1")
	if err != nil || again.ID != s.ID {
		t.Fatalf("re-EnsureSession = %+v, %v; want reattach of %s", again, err, s.ID)
	}
	if f.opCount("create") != 1 {
		t.Fatalf("create calls after reattach = %d, want 1", f.opCount("create"))
	}
}

// TestEnsureSessionConcurrentConvergesOnOneSession: concurrent ensures for one
// conversation mint exactly one session (the double-checked insert).
func TestEnsureSessionConcurrentConvergesOnOneSession(t *testing.T) {
	f := &railwayFixture{}
	m, _ := newTestManager(t, f, Config{MaxActive: 4})

	const n = 8
	ids := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := m.EnsureSession("conv-1")
			if err != nil {
				t.Errorf("EnsureSession: %v", err)
				ids <- ""
				return
			}
			ids <- s.ID
		}()
	}
	wg.Wait()
	close(ids)
	first := ""
	for id := range ids {
		if first == "" {
			first = id
		}
		if id != first {
			t.Fatalf("concurrent ensures minted different sessions: %q vs %q", first, id)
		}
	}
	if _, err := m.WaitReady(context.Background(), "conv-1", 5*time.Second); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if f.opCount("create") != 1 {
		t.Fatalf("create calls = %d, want exactly 1", f.opCount("create"))
	}
}

// TestEnsureSessionFailedBootDestroysAndReports: a background boot failure
// settles honestly, remains visible with a reason, and the next ensure retries.
func TestEnsureSessionFailedBootDestroysAndReports(t *testing.T) {
	f := &railwayFixture{failCreate: true}
	m, rec := newTestManager(t, f, Config{})

	if _, err := m.EnsureSession("conv-1"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var failed Session
	for {
		if current, ok := m.SessionResultForConv("conv-1"); ok && current.State == StateFailed {
			failed = current
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("failed boot never settled; events = %v", rec.types())
		}
		time.Sleep(5 * time.Millisecond)
	}
	types := rec.types()
	if len(types) != 2 || types[0] != EvtProvisioning || types[1] != EvtFailed {
		t.Fatalf("events = %v, want provisioning, failed", types)
	}
	if failed.Reason == "" {
		t.Fatal("failed boot reason was discarded")
	}

	f.mu.Lock()
	f.failCreate = false
	f.mu.Unlock()
	retry, err := m.EnsureSession("conv-1")
	if err != nil {
		t.Fatalf("retry EnsureSession: %v", err)
	}
	if retry.ID == failed.ID || retry.State != StateProvisioning {
		t.Fatalf("retry session = %+v, failed session = %+v", retry, failed)
	}
}

// TestSessionForConvOrLive: with MaxActive=1 (one computer) every conversation
// resolves THE live session; with a larger cap bindings stay per-conversation.
func TestSessionForConvOrLive(t *testing.T) {
	f := &railwayFixture{}
	m, _ := newTestManager(t, f, Config{})
	s, err := m.Provision(context.Background(), Request{ConvID: "conv-a"})
	if err != nil {
		t.Fatalf("Provision: %v", err)
	}
	got, ok := m.SessionForConvOrLive("some-other-conversation")
	if !ok || got.ID != s.ID {
		t.Fatalf("OrLive(other) = %+v %v, want the one computer %s", got, ok, s.ID)
	}

	multi, _ := newTestManager(t, f, Config{MaxActive: 2})
	if _, err := multi.Provision(context.Background(), Request{ConvID: "conv-a"}); err != nil {
		t.Fatalf("Provision multi: %v", err)
	}
	if _, ok := multi.SessionForConvOrLive("some-other-conversation"); ok {
		t.Fatal("OrLive fallback must be disabled when MaxActive > 1")
	}
}

// TestWaitReadyFollowsBoot: WaitReady blocks through the background boot and
// returns the ready snapshot.
func TestWaitReadyFollowsBoot(t *testing.T) {
	f := &railwayFixture{}
	m, _ := newTestManager(t, f, Config{})
	if _, err := m.EnsureSession("conv-1"); err != nil {
		t.Fatalf("EnsureSession: %v", err)
	}
	snap, err := m.WaitReady(context.Background(), "conv-1", 5*time.Second)
	if err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if snap.State != StateReady {
		t.Fatalf("WaitReady state = %s, want ready", snap.State)
	}
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
