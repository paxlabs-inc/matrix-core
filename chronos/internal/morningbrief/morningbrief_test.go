// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package morningbrief

import (
	"strings"
	"testing"
	"time"
)

// TestBriefAlarmIsCronAtLocalTime verifies the alarm is a cron recurring alarm
// carrying the canonical wake_message, conversation, tz, and an active status.
func TestBriefAlarmIsCronAtLocalTime(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	alarm, err := BuildAlarm(Spec{
		ConversationID: "brief-wake",
		DeliveryTime:   "07:30",
		Timezone:       "America/New_York",
	}, now)
	if err != nil {
		t.Fatalf("BuildAlarm: %v", err)
	}
	if alarm.Kind != "cron" {
		t.Errorf("kind = %q, want cron", alarm.Kind)
	}
	if alarm.CronExpr != "30 7 * * *" {
		t.Errorf("cron = %q, want '30 7 * * *'", alarm.CronExpr)
	}
	if alarm.Timezone != "America/New_York" {
		t.Errorf("tz = %q", alarm.Timezone)
	}
	if alarm.WakeMessage != WakeMessage {
		t.Errorf("wake_message not canonical")
	}
	if alarm.Status != "active" {
		t.Errorf("status = %q, want active", alarm.Status)
	}
	if alarm.NextFireAt.IsZero() || !alarm.NextFireAt.After(now) {
		t.Errorf("next_fire_at not resolved in the future: %v", alarm.NextFireAt)
	}
}

// TestCronExprDaysSubset verifies weekday selection renders a sorted DOW list.
func TestCronExprDaysSubset(t *testing.T) {
	expr, err := CronExpr("06:05", []time.Weekday{time.Friday, time.Monday, time.Monday})
	if err != nil {
		t.Fatalf("CronExpr: %v", err)
	}
	if expr != "5 6 * * 1,5" {
		t.Errorf("cron = %q, want '5 6 * * 1,5'", expr)
	}
}

// TestCronExprAllDays collapses a full weekday set to "*".
func TestCronExprAllDays(t *testing.T) {
	all := []time.Weekday{time.Sunday, time.Monday, time.Tuesday, time.Wednesday, time.Thursday, time.Friday, time.Saturday}
	expr, err := CronExpr("08:00", all)
	if err != nil {
		t.Fatalf("CronExpr: %v", err)
	}
	if expr != "0 8 * * *" {
		t.Errorf("cron = %q, want '0 8 * * *'", expr)
	}
}

// TestCronExprDefaultTime uses DefaultDeliveryTime when empty.
func TestCronExprDefaultTime(t *testing.T) {
	expr, err := CronExpr("", nil)
	if err != nil {
		t.Fatalf("CronExpr: %v", err)
	}
	if expr != "0 7 * * *" {
		t.Errorf("cron = %q, want '0 7 * * *'", expr)
	}
}

// TestCronExprRejectsBadTime rejects malformed / out-of-range times.
func TestCronExprRejectsBadTime(t *testing.T) {
	for _, bad := range []string{"25:00", "07:61", "abc", "7"} {
		if _, err := CronExpr(bad, nil); err == nil {
			t.Errorf("CronExpr(%q) = nil error, want reject", bad)
		}
	}
}

// TestBuildAlarmRejectsEmptyConversation guards the precondition.
func TestBuildAlarmRejectsEmptyConversation(t *testing.T) {
	if _, err := BuildAlarm(Spec{DeliveryTime: "07:00"}, time.Now()); err == nil {
		t.Error("empty conversation_id must reject")
	}
}

// TestDSTCorrectness: the local delivery time stays fixed across a DST boundary
// (the next fire is computed in the IANA tz, not a fixed UTC offset).
func TestDSTCorrectness(t *testing.T) {
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skipf("tzdata unavailable: %v", err)
	}
	// Just before US spring-forward (2026-03-08 02:00 EST->EDT).
	now := time.Date(2026, 3, 7, 12, 0, 0, 0, loc)
	alarm, err := BuildAlarm(Spec{ConversationID: "c", DeliveryTime: "07:00", Timezone: "America/New_York"}, now)
	if err != nil {
		t.Fatalf("BuildAlarm: %v", err)
	}
	got := alarm.NextFireAt.In(loc)
	if got.Hour() != 7 || got.Minute() != 0 {
		t.Errorf("next fire local time = %02d:%02d, want 07:00 (DST-correct)", got.Hour(), got.Minute())
	}
	if !strings.Contains(alarm.CronExpr, "7") {
		t.Errorf("cron = %q", alarm.CronExpr)
	}
}
