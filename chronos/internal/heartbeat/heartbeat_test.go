// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package heartbeat

import (
	"testing"
	"time"
)

// TestHeartbeatAlarmIsCronRecurring verifies that the heartbeat helper builds a
// CRON (recurring) alarm — the heartbeat convention is a periodic self-review,
// not a one-shot. The alarm must carry the canonical wake_message, a
// conversation_id, and an active status.
func TestHeartbeatAlarmIsCronRecurring(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	spec := Spec{
		ConversationID: "conv-123",
		CronExpr:       "@every 5m",
	}
	alarm, err := BuildAlarm(spec, now)
	if err != nil {
		t.Fatalf("BuildAlarm: %v", err)
	}

	if alarm.Kind != "cron" {
		t.Errorf("heartbeat alarm must be kind=cron (recurring), got %q", alarm.Kind)
	}
	if alarm.CronExpr != "@every 5m" {
		t.Errorf("cron expr must be carried, got %q", alarm.CronExpr)
	}
	if alarm.ConversationID != "conv-123" {
		t.Errorf("conversation_id must be carried, got %q", alarm.ConversationID)
	}
	if alarm.WakeMessage != WakeMessage {
		t.Errorf("wake_message must be the canonical heartbeat wake, got %q", alarm.WakeMessage)
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

// TestHeartbeatAlarmDefaultInterval verifies the default interval: when no cron
// expression is provided, a sane default (@every 5m) is used.
func TestHeartbeatAlarmDefaultInterval(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
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

// TestHeartbeatAlarmRejectsEmptyConversationID verifies the precondition: a
// heartbeat without a conversation to review is meaningless.
func TestHeartbeatAlarmRejectsEmptyConversationID(t *testing.T) {
	spec := Spec{CronExpr: "@every 5m"}
	if _, err := BuildAlarm(spec, time.Now()); err == nil {
		t.Error("empty conversation_id must reject")
	}
}
