// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

// Real tests for the Automatrix control surface (task 6.1, req 7.1/7.3/7.5/8.1).
//
// Everything under test is REAL — no fakes/mocks of the code under test:
//   - the production AutomatrixGovernor over a REAL durable opt-in store,
//   - the REAL Neocortex pager (temp dir, hash embedder) for the queue,
//   - the REAL durable automatrixlog inbox,
//   - the REAL HTTP handlers driven through httptest against the real server.
//
// The ONLY substituted boundary is the external Chronos system: a recording
// AutomatrixAlarmController that tallies the alarm_set/alarm_cancel calls the
// REAL governor issues (the guidance for an external dependency — assert the
// call is issued through the real code path, do not stub the code under test).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"matrix/neo/internal/automatrixsettings"
	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

// recordingAlarmController is the external-Chronos boundary: it records the
// real governor's alarm_set / alarm_cancel calls and hands back a deterministic
// alarm id. It fabricates no governor decisions — it only tallies the calls the
// real code path makes.
type recordingAlarmController struct {
	setCalls         int
	cancelCalls      int
	lastSetSpec      AutomatrixAlarmSpec
	lastCancel       string
	nextID           string
	setErr           error
	cancelErr        error
	rescheduleCalls  int
	lastRescheduleID string
	lastRescheduleAt time.Time
}

func (c *recordingAlarmController) Set(_ context.Context, spec AutomatrixAlarmSpec) (string, error) {
	c.setCalls++
	c.lastSetSpec = spec
	if c.setErr != nil {
		return "", c.setErr
	}
	id := c.nextID
	if id == "" {
		id = "alarm-test-id"
	}
	return id, nil
}

func (c *recordingAlarmController) Cancel(_ context.Context, alarmID string) error {
	c.cancelCalls++
	c.lastCancel = alarmID
	return c.cancelErr
}

func (c *recordingAlarmController) Reschedule(_ context.Context, alarmID string, next time.Time) error {
	c.rescheduleCalls++
	c.lastRescheduleID = alarmID
	c.lastRescheduleAt = next
	return nil
}

// newControlTestEngine builds a real Engine with a real Neocortex pager + a real
// durable inbox, and installs the production governor backed by a recording
// alarm controller. Returns the engine, the controller, and the governor.
func newControlTestEngine(t *testing.T) (*Engine, *recordingAlarmController, *automatrixGovernor) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-automatrix-control-test"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Pager:           pager,
		ConversationDir: t.TempDir(),
		AutomatrixDir:   t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	ctl := &recordingAlarmController{nextID: "alarm-xyz"}
	gov := newAutomatrixGovernor(automatrixsettings.Open(t.TempDir()), ctl, cfg.AutomatrixBaseIntervalMinutes)
	e.SetAutomatrixGovernor(gov)
	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})
	return e, ctl, gov
}

func newControlTestServer(t *testing.T, e *Engine) *httptest.Server {
	t.Helper()
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// --- governor lifecycle (req 4.1 / 7.5 / 8.1) ---

// TestGovernorEnableCreatesExactlyOneAlarm proves enabling issues exactly ONE
// alarm_set and flips the live opt-in, and a redundant enable does NOT create a
// second alarm (req 4.1 "exactly ONE").
func TestGovernorEnableCreatesExactlyOneAlarm(t *testing.T) {
	_, ctl, gov := newControlTestEngine(t)
	ctx := context.Background()

	if gov.Enabled(ctx) {
		t.Fatal("opt-in must start OFF")
	}
	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !gov.Enabled(ctx) {
		t.Fatal("the engine must read the opt-in as ON after enable (live, no restart)")
	}
	if ctl.setCalls != 1 {
		t.Fatalf("enable must issue exactly one alarm_set, got %d", ctl.setCalls)
	}
	// The alarm carries the canonical AUTOMATRIX wake_message marker.
	if !bytes.Contains([]byte(ctl.lastSetSpec.WakeMessage), []byte("AUTOMATRIX")) {
		t.Errorf("the alarm wake_message must carry the AUTOMATRIX marker, got %q", ctl.lastSetSpec.WakeMessage)
	}

	// Redundant enable: still exactly one alarm.
	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("redundant SetEnabled(true): %v", err)
	}
	if ctl.setCalls != 1 {
		t.Fatalf("a redundant enable must not create a second alarm, got %d set calls", ctl.setCalls)
	}
}

// TestGovernorDisableCancelsAlarm proves disabling cancels exactly the agent's
// own alarm and flips the opt-in OFF (req 4.1 / 8.1).
func TestGovernorDisableCancelsAlarm(t *testing.T) {
	_, ctl, gov := newControlTestEngine(t)
	ctx := context.Background()

	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	createdID := gov.SettingsView().AlarmID
	if createdID == "" {
		t.Fatal("enable must record the created alarm id")
	}
	if err := gov.SetEnabled(ctx, false); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if gov.Enabled(ctx) {
		t.Fatal("opt-in must be OFF after disable")
	}
	if ctl.cancelCalls != 1 {
		t.Fatalf("disable must issue exactly one alarm_cancel, got %d", ctl.cancelCalls)
	}
	if ctl.lastCancel != createdID {
		t.Errorf("disable must cancel the agent's OWN alarm id %q, got %q", createdID, ctl.lastCancel)
	}
	if gov.SettingsView().AlarmID != "" {
		t.Error("disable must clear the stored alarm id")
	}
}

// TestGovernorEnableFailsClosedOnAlarmError proves that if the alarm cannot be
// created the opt-in is NOT flipped on (an enabled flag with no alarm would
// never wake — fail closed).
func TestGovernorEnableFailsClosedOnAlarmError(t *testing.T) {
	_, ctl, gov := newControlTestEngine(t)
	ctl.setErr = context.DeadlineExceeded
	if err := gov.SetEnabled(context.Background(), true); err == nil {
		t.Fatal("a failed alarm_set must surface an error")
	}
	if gov.Enabled(context.Background()) {
		t.Fatal("a failed alarm create must leave the opt-in OFF (fail closed)")
	}
}

// TestGovernorRecordsPerDayCounter proves the governor's per-day counter seam
// is wired to the durable store (req 4.7).
func TestGovernorRecordsPerDayCounter(t *testing.T) {
	_, _, gov := newControlTestEngine(t)
	ctx := context.Background()
	if gov.TasksToday(ctx) != 0 {
		t.Fatal("tasks today must start at 0")
	}
	if err := gov.RecordTaskStarted(ctx); err != nil {
		t.Fatalf("RecordTaskStarted: %v", err)
	}
	if gov.TasksToday(ctx) != 1 {
		t.Fatalf("tasks today = %d, want 1", gov.TasksToday(ctx))
	}
}

func TestGovernorPersistsJitteredReschedule(t *testing.T) {
	_, controller, governor := newControlTestEngine(t)
	ctx := context.Background()
	if err := governor.SetEnabled(ctx, true); err != nil {
		t.Fatal(err)
	}
	next := time.Now().UTC().Add(37 * time.Minute).Truncate(time.Microsecond)
	if err := governor.Reschedule(ctx, next); err != nil {
		t.Fatal(err)
	}
	if controller.rescheduleCalls != 1 || controller.lastRescheduleID != governor.SettingsView().AlarmID || !controller.lastRescheduleAt.Equal(next) {
		t.Fatalf("reschedule calls=%d id=%q at=%s", controller.rescheduleCalls, controller.lastRescheduleID, controller.lastRescheduleAt)
	}
}

func TestGovernorReconcilesEnabledAlarmWithoutDuplication(t *testing.T) {
	directory := t.TempDir()
	settings := automatrixsettings.Open(directory)
	if err := settings.SetEnabled(true, "shared-alarm-old"); err != nil {
		t.Fatal(err)
	}
	controller := &recordingAlarmController{nextID: "local-alarm-current"}
	governor := newAutomatrixGovernor(settings, controller, 45)
	if err := governor.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if controller.setCalls != 1 || governor.SettingsView().AlarmID != "local-alarm-current" {
		t.Fatalf("set calls=%d state=%+v", controller.setCalls, governor.SettingsView())
	}
}

// --- endpoints over httptest (req 7.1/7.3/7.5/7.4) ---

// TestSettingsEndpointToggleLifecycle drives the REAL HTTP PUT/GET against the
// real server: enabling creates the alarm, GET reflects it, disabling cancels.
func TestSettingsEndpointToggleLifecycle(t *testing.T) {
	e, ctl, _ := newControlTestEngine(t)
	ts := newControlTestServer(t, e)

	// GET default: OFF.
	var got settingsResponse
	doJSON(t, http.MethodGet, ts.URL+"/automatrix/settings", nil, http.StatusOK, &got)
	if got.Enabled {
		t.Fatal("default opt-in must be OFF")
	}

	// PUT enable -> 200, alarm created and live.
	doJSON(t, http.MethodPut, ts.URL+"/automatrix/settings", map[string]any{"enabled": true}, http.StatusOK, &got)
	if !got.Enabled || !got.AlarmLive {
		t.Fatalf("after enable: %+v", got)
	}
	if ctl.setCalls != 1 {
		t.Fatalf("enable endpoint must issue one alarm_set, got %d", ctl.setCalls)
	}

	// PUT disable -> 200, alarm cancelled.
	doJSON(t, http.MethodPut, ts.URL+"/automatrix/settings", map[string]any{"enabled": false}, http.StatusOK, &got)
	if got.Enabled || got.AlarmLive {
		t.Fatalf("after disable: %+v", got)
	}
	if ctl.cancelCalls != 1 {
		t.Fatalf("disable endpoint must issue one alarm_cancel, got %d", ctl.cancelCalls)
	}
}

// TestQueueEndpointSurfacesFinancialForApproval proves the management queue
// returns BOTH eligible and financial pending opportunities, flagging the
// financial ones (req 7.3) — distinct from the autonomous picker's filter.
func TestQueueEndpointSurfacesFinancialForApproval(t *testing.T) {
	e, _, _ := newControlTestEngine(t)
	ts := newControlTestServer(t, e)
	ctx := context.Background()

	if _, err := e.pager.RememberOpportunity(ctx, memory.OpportunitySpec{
		Summary: "Draft the quarterly update doc you mentioned", EligibleAutonomous: true, Confidence: 0.8,
	}); err != nil {
		t.Fatalf("RememberOpportunity eligible: %v", err)
	}
	if _, err := e.pager.RememberOpportunity(ctx, memory.OpportunitySpec{
		Summary: "Swap 100 PAX for USDC at the best rate", EligibleAutonomous: false, Confidence: 0.9,
	}); err != nil {
		t.Fatalf("RememberOpportunity financial: %v", err)
	}

	var resp struct {
		Items []queueItem `json:"items"`
	}
	doJSON(t, http.MethodGet, ts.URL+"/automatrix/queue", nil, http.StatusOK, &resp)
	if len(resp.Items) != 2 {
		t.Fatalf("queue must surface both pending items (eligible + financial), got %d", len(resp.Items))
	}
	var sawFinancial, sawEligible bool
	for _, it := range resp.Items {
		if it.Financial {
			sawFinancial = true
		} else {
			sawEligible = true
		}
		if it.URI == "" {
			t.Error("each queue item must carry its uri for dismiss/approve")
		}
	}
	if !sawFinancial || !sawEligible {
		t.Errorf("queue must distinguish financial vs eligible items; financial=%v eligible=%v", sawFinancial, sawEligible)
	}
}

// TestDismissEndpointRemovesFromQueue proves POST dismiss sets status=dismissed
// and the item leaves the pending queue (req 7.3).
func TestDismissEndpointRemovesFromQueue(t *testing.T) {
	e, _, _ := newControlTestEngine(t)
	ts := newControlTestServer(t, e)
	ctx := context.Background()

	uri, err := e.pager.RememberOpportunity(ctx, memory.OpportunitySpec{
		Summary: "Clean up the stale TODO comments", EligibleAutonomous: true, Confidence: 0.7,
	})
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}

	var dres map[string]any
	doJSON(t, http.MethodPost, ts.URL+"/automatrix/queue/dismiss", map[string]any{"uri": uri}, http.StatusOK, &dres)
	if dres["status"] != string(memory.OpportunityDismissed) {
		t.Fatalf("dismiss must report dismissed, got %v", dres["status"])
	}

	left, err := e.pager.QueuedOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("QueuedOpportunities: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("a dismissed item must leave the pending queue, got %d", len(left))
	}
}

// TestApproveEndpointDispatchesGatedRun proves approving a financial item
// dispatches it as a NORMAL user-initiated run (returns an intent id, persists
// the user turn) and marks the opportunity scheduled — never auto-run, never on
// the restricted surface. The target session is pre-activated so submit folds
// the message into the live run (no model call needed to prove the contract).
func TestApproveEndpointDispatchesGatedRun(t *testing.T) {
	e, _, _ := newControlTestEngine(t)
	ts := newControlTestServer(t, e)
	ctx := context.Background()

	const originConv = "conv-financial-origin"
	uri, err := e.pager.RememberOpportunity(ctx, memory.OpportunitySpec{
		Summary:              "Swap 100 PAX for USDC at the best rate",
		Rationale:            "user said they want to rebalance into USDC",
		EligibleAutonomous:   false,
		Confidence:           0.9,
		OriginConversationID: originConv,
	})
	if err != nil {
		t.Fatalf("RememberOpportunity: %v", err)
	}

	// Pre-activate the origin session so submit ENQUEUES into the live run
	// rather than dispatching a fresh drive (which would require a live model).
	sess := e.sessions.get(originConv)
	pre := &run{id: "pre-active-run", convID: originConv, sess: sess}
	sess.active = pre
	e.registerRun(pre)

	var ares map[string]any
	doJSON(t, http.MethodPost, ts.URL+"/automatrix/queue/approve", map[string]any{"uri": uri}, http.StatusAccepted, &ares)
	if ares["intent_id"] != "pre-active-run" {
		t.Fatalf("approve must dispatch through the normal session path, got intent_id=%v", ares["intent_id"])
	}
	if ares["conversation_id"] != originConv {
		t.Errorf("approve must resume into the origin conversation, got %v", ares["conversation_id"])
	}

	// The user turn was persisted (the normal gated dispatch contract).
	rec := e.conv.Get(originConv)
	if rec == nil || len(rec.Turns) == 0 {
		t.Fatal("approve must persist the user turn for the gated run")
	}

	// The opportunity left the pending management queue (now scheduled).
	left, err := e.pager.QueuedOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("QueuedOpportunities: %v", err)
	}
	if len(left) != 0 {
		t.Fatalf("an approved item must leave the pending queue, got %d", len(left))
	}
}

// TestInboxEndpointListsAndMarksRead proves the completion inbox endpoint serves
// the durable records + unread badge and marks one read (req 7.4).
func TestInboxEndpointListsAndMarksRead(t *testing.T) {
	e, _, _ := newControlTestEngine(t)
	ts := newControlTestServer(t, e)

	rec := e.emitAutomatrixComplete("conv_surprise",
		"Draft the quarterly update doc you mentioned",
		"Drafted a 1-page quarterly update in the thread")

	var resp struct {
		Items  []map[string]any `json:"items"`
		Unread int              `json:"unread"`
	}
	doJSON(t, http.MethodGet, ts.URL+"/automatrix/inbox", nil, http.StatusOK, &resp)
	if len(resp.Items) != 1 || resp.Unread != 1 {
		t.Fatalf("inbox must list 1 unread completion, got items=%d unread=%d", len(resp.Items), resp.Unread)
	}

	var mres map[string]any
	doJSON(t, http.MethodPost, ts.URL+"/automatrix/inbox/read", map[string]any{"id": rec.ID}, http.StatusOK, &mres)
	if mres["changed"] != true {
		t.Fatalf("marking an unread record read must report changed=true, got %v", mres["changed"])
	}
	if e.AutomatrixInbox().Unread() != 0 {
		t.Fatalf("unread badge must be 0 after marking read, got %d", e.AutomatrixInbox().Unread())
	}
}

// TestControlEndpointsUnavailableWithoutGovernor proves the settings endpoint
// fails cleanly (503) when no production governor is wired, rather than panics.
func TestControlEndpointsUnavailableWithoutGovernor(t *testing.T) {
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-automatrix-nogov"
	e := NewEngine(EngineOptions{Config: cfg, BackendURL: "http://127.0.0.1:1"})
	t.Cleanup(e.Close)
	ts := newControlTestServer(t, e)

	resp, err := http.Get(ts.URL + "/automatrix/settings")
	if err != nil {
		t.Fatalf("GET settings: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("settings without a governor must be 503, got %d", resp.StatusCode)
	}
}

// doJSON performs an HTTP request with an optional JSON body, asserts the
// status, and decodes the JSON response into out (when non-nil).
func doJSON(t *testing.T, method, url string, body any, wantStatus int, out any) {
	t.Helper()
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatalf("marshal body: %v", err)
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != wantStatus {
		t.Fatalf("%s %s status = %d, want %d", method, url, resp.StatusCode, wantStatus)
	}
	if out != nil {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatalf("decode response: %v", err)
		}
	}
}
