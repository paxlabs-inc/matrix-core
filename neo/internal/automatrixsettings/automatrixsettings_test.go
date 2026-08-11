// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package automatrixsettings

// Real tests for the durable per-user Automatrix opt-in store (task 6.1, req
// 7.1/7.5/8.1). No fakes — every assertion runs against the real on-disk store.

import (
	"testing"
	"time"
)

// TestDefaultOptInIsOff proves the opt-in defaults to false (req 7.1: default
// OFF) on a fresh, never-written store.
func TestDefaultOptInIsOff(t *testing.T) {
	s := Open(t.TempDir())
	if s.Enabled() {
		t.Fatal("a fresh store must default the opt-in to OFF")
	}
	if s.AlarmID() != "" {
		t.Fatal("a fresh store must hold no alarm id")
	}
	if s.TasksToday() != 0 {
		t.Fatal("a fresh store must report zero tasks today")
	}
}

// TestEnableThenDisablePersistsAndReverses proves the opt-in is durable and
// reversible: enabling stores the flag + alarm id, disabling clears them, and a
// reopen of the SAME dir reflects the last write (req 7.5: a toggle survives
// without losing state).
func TestEnableThenDisablePersistsAndReverses(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if err := s.SetEnabled(true, "alarm-123"); err != nil {
		t.Fatalf("SetEnabled(true): %v", err)
	}
	if !s.Enabled() || s.AlarmID() != "alarm-123" {
		t.Fatalf("after enable: enabled=%v alarm=%q", s.Enabled(), s.AlarmID())
	}

	// Reopen: the durable file must carry the enabled state forward.
	reopened := Open(dir)
	if !reopened.Enabled() || reopened.AlarmID() != "alarm-123" {
		t.Fatalf("reopen lost state: enabled=%v alarm=%q", reopened.Enabled(), reopened.AlarmID())
	}

	if err := reopened.SetEnabled(false, ""); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	if reopened.Enabled() {
		t.Fatal("after disable the opt-in must be OFF")
	}
	if reopened.AlarmID() != "" {
		t.Fatal("disable must clear the alarm id")
	}
	// A third open sees the disabled state.
	if Open(dir).Enabled() {
		t.Fatal("disable must persist across reopen")
	}
}

// TestDisableClearsAlarmIDEvenIfPassed proves SetEnabled(false, ...) never
// retains an alarm id (the disabled invariant).
func TestDisableClearsAlarmIDEvenIfPassed(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.SetEnabled(false, "should-be-ignored"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if s.AlarmID() != "" {
		t.Fatalf("disabled store must hold no alarm id, got %q", s.AlarmID())
	}
}

// TestTaskCounterIncrementsAndPersists proves the per-day counter (req 4.7) is
// durable across reopen.
func TestTaskCounterIncrementsAndPersists(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	for i := 0; i < 3; i++ {
		if err := s.RecordTaskStarted(); err != nil {
			t.Fatalf("RecordTaskStarted: %v", err)
		}
	}
	if s.TasksToday() != 3 {
		t.Fatalf("tasks today = %d, want 3", s.TasksToday())
	}
	if Open(dir).TasksToday() != 3 {
		t.Fatalf("counter did not persist across reopen")
	}
}

// TestTaskCounterDailyReset proves a counter recorded on a previous UTC date is
// treated as zero today (the daily window roll, req 4.7).
func TestTaskCounterDailyReset(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	if err := s.RecordTaskStarted(); err != nil {
		t.Fatalf("RecordTaskStarted: %v", err)
	}
	// Force the stored window to a stale date and re-read: today's count is 0.
	s.mu.Lock()
	s.st.TasksDay = time.Now().UTC().AddDate(0, 0, -1).Format("2006-01-02")
	s.mu.Unlock()
	if s.TasksToday() != 0 {
		t.Fatalf("a stale-day counter must read as 0 today, got %d", s.TasksToday())
	}
	// Recording again rolls the window to today and starts fresh at 1.
	if err := s.RecordTaskStarted(); err != nil {
		t.Fatalf("RecordTaskStarted: %v", err)
	}
	if s.TasksToday() != 1 {
		t.Fatalf("after the daily roll the count must be 1, got %d", s.TasksToday())
	}
}

// TestInMemoryFallbackToggles proves an empty dir yields a working in-memory
// store (dev/CLI), so the opt-in still toggles without disk durability.
func TestInMemoryFallbackToggles(t *testing.T) {
	s := Open("")
	if s.Persistent() {
		t.Fatal("an empty dir must be non-persistent")
	}
	if err := s.SetEnabled(true, "x"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	if !s.Enabled() {
		t.Fatal("in-memory store must reflect the toggle")
	}
}

func TestTaskCounterUsesConfiguredUserTimezone(t *testing.T) {
	s := Open(t.TempDir())
	if err := s.SetTimezone("Pacific/Honolulu"); err != nil {
		t.Fatal(err)
	}
	if err := s.RecordTaskStarted(); err != nil {
		t.Fatal(err)
	}
	location, _ := time.LoadLocation("Pacific/Honolulu")
	state := s.View()
	if state.Timezone != "Pacific/Honolulu" || state.TasksDay != time.Now().In(location).Format("2006-01-02") {
		t.Fatalf("state=%+v", state)
	}
	if err := s.SetTimezone("Not/A_Zone"); err == nil {
		t.Fatal("invalid timezone was accepted")
	}
}
