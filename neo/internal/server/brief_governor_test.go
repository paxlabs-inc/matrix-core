// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"matrix/neo/internal/briefsettings"
	"matrix/neo/internal/config"
	"matrix/neo/internal/memory"
)

// recordingBriefAlarmController is the external-Chronos boundary for the brief:
// it records the real governor's alarm_set/alarm_cancel calls and hands back a
// deterministic id. It fabricates no governor decisions — it only tallies the
// calls the real code path makes (same posture as recordingAlarmController).
type recordingBriefAlarmController struct {
	setCalls    int
	cancelCalls int
	lastSetSpec BriefAlarmSpec
	lastCancel  string
	nextID      string
	setErr      error
	cancelErr   error
}

func (c *recordingBriefAlarmController) Set(_ context.Context, spec BriefAlarmSpec) (string, error) {
	c.setCalls++
	c.lastSetSpec = spec
	if c.setErr != nil {
		return "", c.setErr
	}
	id := c.nextID
	if id == "" {
		id = "brief-alarm-1"
	}
	return id, nil
}

func (c *recordingBriefAlarmController) Cancel(_ context.Context, alarmID string) error {
	c.cancelCalls++
	c.lastCancel = alarmID
	return c.cancelErr
}

func newBriefTestGovernor(t *testing.T) (*briefGovernor, *recordingBriefAlarmController) {
	t.Helper()
	ctl := &recordingBriefAlarmController{nextID: "brief-alarm-1"}
	gov := newBriefGovernor(briefsettings.Open(t.TempDir()), ctl)
	// A default schedule so createAlarm has a valid cron.
	if err := gov.settings.SetSchedule(briefsettings.Schedule{
		Timezone: "UTC", DeliveryTime: "07:00", ConversationID: "brief-conv",
	}); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	return gov, ctl
}

// TestBriefEnableCreatesExactlyOneAlarm — enable issues one alarm_set carrying
// the MORNING_BRIEF marker + a wall-clock cron; a redundant enable is a no-op.
func TestBriefEnableCreatesExactlyOneAlarm(t *testing.T) {
	gov, ctl := newBriefTestGovernor(t)
	ctx := context.Background()

	if gov.Enabled() {
		t.Fatal("brief must start OFF")
	}
	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !gov.Enabled() {
		t.Fatal("brief must read ON after enable (live)")
	}
	if ctl.setCalls != 1 {
		t.Fatalf("enable must issue exactly one alarm_set, got %d", ctl.setCalls)
	}
	if !bytes.Contains([]byte(ctl.lastSetSpec.WakeMessage), []byte("MORNING_BRIEF")) {
		t.Errorf("wake_message must carry the MORNING_BRIEF marker, got %q", ctl.lastSetSpec.WakeMessage)
	}
	if ctl.lastSetSpec.CronExpr != "0 7 * * *" {
		t.Errorf("cron = %q, want '0 7 * * *'", ctl.lastSetSpec.CronExpr)
	}
	if ctl.lastSetSpec.Timezone != "UTC" {
		t.Errorf("tz = %q, want UTC", ctl.lastSetSpec.Timezone)
	}

	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("redundant enable: %v", err)
	}
	if ctl.setCalls != 1 {
		t.Fatalf("redundant enable must not create a second alarm, got %d", ctl.setCalls)
	}
}

// TestBriefDisableCancelsOwnAlarm — disable cancels exactly the agent's own
// alarm and flips OFF (req 11.3).
func TestBriefDisableCancelsOwnAlarm(t *testing.T) {
	gov, ctl := newBriefTestGovernor(t)
	ctx := context.Background()
	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	createdID := gov.SettingsView().AlarmID
	if createdID == "" {
		t.Fatal("enable must record the alarm id")
	}
	if err := gov.SetEnabled(ctx, false); err != nil {
		t.Fatalf("disable: %v", err)
	}
	if gov.Enabled() {
		t.Fatal("must be OFF after disable")
	}
	if ctl.cancelCalls != 1 || ctl.lastCancel != createdID {
		t.Fatalf("disable must cancel own alarm %q, got calls=%d last=%q", createdID, ctl.cancelCalls, ctl.lastCancel)
	}
	if gov.SettingsView().AlarmID != "" {
		t.Error("disable must clear the stored alarm id")
	}
}

// TestBriefEnableFailClosed — if the alarm cannot be created, the opt-in does
// NOT flip on (an enabled setting with no alarm would never wake).
func TestBriefEnableFailClosed(t *testing.T) {
	gov, ctl := newBriefTestGovernor(t)
	ctl.setErr = errors.New("chronos down")
	if err := gov.SetEnabled(context.Background(), true); err == nil {
		t.Fatal("enable must fail when the alarm cannot be created")
	}
	if gov.Enabled() {
		t.Fatal("opt-in must stay OFF when the alarm was not created (fail closed)")
	}
	if gov.SettingsView().AlarmID != "" {
		t.Error("no alarm id should be stored on a failed enable")
	}
}

// TestBriefScheduleEditReschedules — editing the time while enabled cancels the
// old alarm and creates a fresh one with the new cron (req 14.2).
func TestBriefScheduleEditReschedules(t *testing.T) {
	gov, ctl := newBriefTestGovernor(t)
	ctx := context.Background()
	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := gov.UpdateSchedule(ctx, briefsettings.Schedule{
		Timezone: "America/New_York", DeliveryTime: "09:15",
		Days: []int{1, 3, 5}, ConversationID: "brief-conv",
	}); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	if ctl.cancelCalls != 1 {
		t.Fatalf("reschedule must cancel the old alarm, got %d cancels", ctl.cancelCalls)
	}
	if ctl.setCalls != 2 {
		t.Fatalf("reschedule must create a fresh alarm, got %d set calls", ctl.setCalls)
	}
	if ctl.lastSetSpec.CronExpr != "15 9 * * 1,3,5" {
		t.Errorf("rescheduled cron = %q, want '15 9 * * 1,3,5'", ctl.lastSetSpec.CronExpr)
	}
	if ctl.lastSetSpec.Timezone != "America/New_York" {
		t.Errorf("rescheduled tz = %q", ctl.lastSetSpec.Timezone)
	}
}

// TestBriefScheduleEditWhileDisabledDoesNotAlarm — editing while disabled
// persists the schedule but issues no alarm calls.
func TestBriefScheduleEditWhileDisabledDoesNotAlarm(t *testing.T) {
	gov, ctl := newBriefTestGovernor(t)
	if err := gov.UpdateSchedule(context.Background(), briefsettings.Schedule{
		Timezone: "UTC", DeliveryTime: "06:00", ConversationID: "brief-conv",
	}); err != nil {
		t.Fatalf("UpdateSchedule: %v", err)
	}
	if ctl.setCalls != 0 || ctl.cancelCalls != 0 {
		t.Fatalf("no alarm calls while disabled, got set=%d cancel=%d", ctl.setCalls, ctl.cancelCalls)
	}
	if gov.SettingsView().DeliveryTime != "06:00" {
		t.Error("schedule must persist while disabled")
	}
}

// TestBriefPauseKeepsAlarm — pause skips without cancelling the alarm; Active()
// is false while paused (req 14.2).
func TestBriefPauseKeepsAlarm(t *testing.T) {
	gov, ctl := newBriefTestGovernor(t)
	ctx := context.Background()
	if err := gov.SetEnabled(ctx, true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := gov.SetPaused(ctx, true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	if ctl.cancelCalls != 0 {
		t.Errorf("pause must NOT cancel the alarm, got %d cancels", ctl.cancelCalls)
	}
	if gov.Active() {
		t.Error("Active() must be false while paused")
	}
	if !gov.Enabled() {
		t.Error("Enabled() must remain true while paused")
	}
}

// --- HTTP control surface (default off, toggle, edit) ---

func newBriefTestServer(t *testing.T) (*httptest.Server, *recordingBriefAlarmController) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-brief-control-test"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Pager:           pager,
		ConversationDir: t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	ctl := &recordingBriefAlarmController{nextID: "brief-alarm-http"}
	gov := newBriefGovernor(briefsettings.Open(t.TempDir()), ctl)
	e.SetBriefGovernor(gov)
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatalf("server.New: %v", err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(func() {
		ts.Close()
		e.Close()
		pager.Close()
	})
	return ts, ctl
}

func TestBriefRoutesDefaultOffThenEnable(t *testing.T) {
	ts, ctl := newBriefTestServer(t)

	// GET default off.
	resp, err := http.Get(ts.URL + "/brief/settings")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	var got briefSettingsResponse
	if derr := json.NewDecoder(resp.Body).Decode(&got); derr != nil {
		t.Fatalf("decode: %v", derr)
	}
	resp.Body.Close()
	if got.Enabled {
		t.Fatal("brief must default OFF")
	}

	// PUT schedule + enable in one call.
	body, _ := json.Marshal(briefSettingsRequest{
		Timezone:     ptr("UTC"),
		DeliveryTime: ptr("07:30"),
		Days:         ptrInts([]int{1, 2, 3, 4, 5}),
		Length:       ptr("standard"),
		Sections:     ptrStrings([]string{"news", "music"}),
		Enabled:      ptrBool(true),
	})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/brief/settings", bytes.NewReader(body))
	putResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	var after briefSettingsResponse
	if derr := json.NewDecoder(putResp.Body).Decode(&after); derr != nil {
		t.Fatalf("decode put: %v", derr)
	}
	putResp.Body.Close()
	if !after.Enabled || !after.AlarmLive {
		t.Fatalf("after enable: enabled=%v alarm_live=%v", after.Enabled, after.AlarmLive)
	}
	if after.DeliveryTime != "07:30" || after.Timezone != "UTC" {
		t.Errorf("schedule not persisted: %+v", after)
	}
	if ctl.setCalls != 1 {
		t.Errorf("enable must issue one alarm_set, got %d", ctl.setCalls)
	}
	if ctl.lastSetSpec.CronExpr != "30 7 * * 1,2,3,4,5" {
		t.Errorf("cron = %q", ctl.lastSetSpec.CronExpr)
	}
}

func TestBriefRoutesRejectBadVocab(t *testing.T) {
	ts, _ := newBriefTestServer(t)
	body, _ := json.Marshal(briefSettingsRequest{Length: ptr("epic")})
	req, _ := http.NewRequest(http.MethodPut, ts.URL+"/brief/settings", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func ptr(s string) *string            { return &s }
func ptrBool(b bool) *bool            { return &b }
func ptrInts(v []int) *[]int          { return &v }
func ptrStrings(v []string) *[]string { return &v }
