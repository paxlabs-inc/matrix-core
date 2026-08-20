// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memorysweep

import (
	"strings"
	"testing"
	"time"
)

// TestMemorySweepAlarmIsCronRecurring verifies that the memorysweep helper
// builds a CRON (recurring) alarm — the sweep convention is a periodic
// window-close trigger, not a one-shot. The alarm must carry the canonical
// wake_message, a conversation_id, and an active status.
func TestMemorySweepAlarmIsCronRecurring(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	spec := Spec{
		ConversationID: "conv-sweep",
		CronExpr:       "@every 1h",
	}
	alarm, err := BuildAlarm(spec, now)
	if err != nil {
		t.Fatalf("BuildAlarm: %v", err)
	}

	if alarm.Kind != "cron" {
		t.Errorf("memorysweep alarm must be kind=cron (recurring), got %q", alarm.Kind)
	}
	if alarm.CronExpr != "@every 1h" {
		t.Errorf("cron expr must be carried, got %q", alarm.CronExpr)
	}
	if alarm.ConversationID != "conv-sweep" {
		t.Errorf("conversation_id must be carried, got %q", alarm.ConversationID)
	}
	if alarm.WakeMessage != WakeMessage {
		t.Errorf("wake_message must be the canonical memorysweep wake, got %q", alarm.WakeMessage)
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

// TestMemorySweepAlarmDefaultInterval verifies the default interval: when no
// cron expression is provided, the sane default (@every 1h) is used.
func TestMemorySweepAlarmDefaultInterval(t *testing.T) {
	now := time.Date(2026, 6, 23, 12, 0, 0, 0, time.UTC)
	spec := Spec{ConversationID: "conv-sweep-2"}
	alarm, err := BuildAlarm(spec, now)
	if err != nil {
		t.Fatalf("BuildAlarm: %v", err)
	}
	if alarm.CronExpr != DefaultCronExpr {
		t.Errorf("default cron must be %q, got %q", DefaultCronExpr, alarm.CronExpr)
	}
}

// TestMemorySweepAlarmRejectsEmptyConversationID verifies the precondition:
// the underlying alarm machinery requires a delivery target even though the
// sweep itself is not scoped to that conversation's content.
func TestMemorySweepAlarmRejectsEmptyConversationID(t *testing.T) {
	spec := Spec{CronExpr: "@every 1h"}
	if _, err := BuildAlarm(spec, time.Now()); err == nil {
		t.Error("empty conversation_id must reject")
	}
}

// TestMemorySweepWakeMessageIsNotConversational proves the sweep marker is
// distinct from the heartbeat/AUTOMATRIX conventions and is explicitly
// marked as not requiring a model reply — the receiver routes it directly
// to cortex.SweepNow instead of invoking the agent.
func TestMemorySweepWakeMessageIsNotConversational(t *testing.T) {
	if !strings.Contains(WakeMessage, "CONTINUOUS_MEMORY_SWEEP") {
		t.Errorf("WakeMessage must carry the CONTINUOUS_MEMORY_SWEEP marker, got %q", WakeMessage)
	}
	if strings.Contains(WakeMessage, "HEARTBEAT") || strings.Contains(WakeMessage, "AUTOMATRIX") {
		t.Errorf("WakeMessage must not collide with the heartbeat/AUTOMATRIX markers, got %q", WakeMessage)
	}
}
