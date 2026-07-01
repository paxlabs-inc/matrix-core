// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package automatrix

import (
	"testing"
	"time"
)

// TestAutomatrixAlarmIsCronRecurring verifies that the AUTOMATRIX helper builds
// a CRON (recurring) alarm — the AUTOMATRIX convention is a periodic idle wake,
// not a one-shot. The alarm must carry the canonical wake_message, a
// conversation_id, and an active status.
func TestAutomatrixAlarmIsCronRecurring(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	spec := Spec{
		ConversationID: "conv-123",
		CronExpr:       "@every 45m",
	}
	alarm, err := BuildAlarm(spec, now)
	if err != nil {
		t.Fatalf("BuildAlarm: %v", err)
	}

	if alarm.Kind != "cron" {
		t.Errorf("automatrix alarm must be kind=cron (recurring), got %q", alarm.Kind)
	}
	if alarm.CronExpr != "@every 45m" {
		t.Errorf("cron expr must be carried, got %q", alarm.CronExpr)
	}
	if alarm.ConversationID != "conv-123" {
		t.Errorf("conversation_id must be carried, got %q", alarm.ConversationID)
	}
	if alarm.WakeMessage != WakeMessage {
		t.Errorf("wake_message must be the canonical AUTOMATRIX wake, got %q", alarm.WakeMessage)
	}
	if alarm.Status != "active" {
		t.Errorf("alarm must be active, got %q", alarm.Status)
	}
	if alarm.NextFireAt.IsZero() {
		t.Error("next_fire_at must be resolved (non-zero)")
	}
	if !alarm.NextFireAt.After(now) {
		t.Error("next_fire_at must be in the future")
	}
}

// TestAutomatrixAlarmDefaultInterval verifies the default interval: when no
// cron expression is provided, the base idle-wake cadence (@every 45m) is used.
func TestAutomatrixAlarmDefaultInterval(t *testing.T) {
	now := time.Date(2026, 6, 30, 12, 0, 0, 0, time.UTC)
	spec := Spec{
		ConversationID: "conv-456",
	}
	alarm, err := BuildAlarm(spec, now)
	if err != nil {
		t.Fatalf("BuildAlarm: %v", err)
	}
	if alarm.CronExpr != DefaultCronExpr {
		t.Errorf("default cron must be %q, got %q", DefaultCronExpr, alarm.CronExpr)
	}
}

// TestAutomatrixAlarmRejectsEmptyConversationID verifies the precondition: an
// AUTOMATRIX wake without a conversation to resume into is meaningless.
func TestAutomatrixAlarmRejectsEmptyConversationID(t *testing.T) {
	spec := Spec{CronExpr: "@every 45m"}
	if _, err := BuildAlarm(spec, time.Now()); err == nil {
		t.Error("empty conversation_id must reject")
	}
}

// TestAutomatrixWakeMessageCarriesMarkerAndIdleSentinel verifies the canonical
// wake_message carries the AUTOMATRIX marker (so the agent can detect the wake)
// and instructs the AUTOMATRIX_IDLE sentinel reply when nothing is worth doing.
// These tokens MUST stay in sync with the Neo-side constants.
func TestAutomatrixWakeMessageCarriesMarkerAndIdleSentinel(t *testing.T) {
	if !contains(WakeMessage, "AUTOMATRIX") {
		t.Errorf("wake_message must carry the AUTOMATRIX marker, got %q", WakeMessage)
	}
	if !contains(WakeMessage, "AUTOMATRIX_IDLE") {
		t.Errorf("wake_message must instruct the AUTOMATRIX_IDLE sentinel, got %q", WakeMessage)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
