// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package heartbeat implements the P1-4 Chronos heartbeat convention: a
// recurring alarm whose wake_message tells Neo to review active goals and
// reply with the HEARTBEAT_OK sentinel when nothing needs attention.
//
// Chronos already supports durable recurring alarms with wake_message +
// payload + conversation_id; this package provides the CONVENTION — the
// canonical wake_message format and a BuildAlarm helper that assembles a
// correctly-shaped heartbeat alarm.
package heartbeat

import (
	"fmt"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/chronos/internal/schedule"
	"github.com/Sidiora-Labs/centra-llm-agents/chronos/pkg/types"
)

// WakeMessage is the canonical wake_message a Chronos heartbeat alarm
// delivers. It instructs the agent to review active goals/constraints and
// reply with the HEARTBEAT_OK sentinel when nothing needs attention, or
// surface real content when something does.
//
// This MUST stay in sync with the agent-side HeartbeatWakeMessage
// (neo/internal/agent/heartbeat.go) — they share the HEARTBEAT marker that
// the agent uses to detect a heartbeat wake.
const WakeMessage = "HEARTBEAT: review your active goals and constraints. If nothing needs attention, reply with exactly HEARTBEAT_OK. If something needs attention, surface it."

// DefaultCronExpr is the default recurring interval for a heartbeat alarm
// when none is specified. @every 5m is frequent enough for proactive review
// without being noisy.
const DefaultCronExpr = "@every 5m"

// Spec configures a heartbeat alarm.
type Spec struct {
	// ConversationID is the conversation the heartbeat reviews (required — a
	// heartbeat without a conversation to review is meaningless).
	ConversationID string
	// CronExpr is the recurring schedule. Defaults to DefaultCronExpr when empty.
	CronExpr string
	// Timezone is the IANA timezone for the cron expression (default UTC).
	Timezone string
	// Label is an optional human-readable label for the alarm.
	Label string
	// IdempotencyKey deduplicates the alarm (recommended for heartbeat alarms).
	IdempotencyKey string
	// MaxFailures is the wake-delivery retry ceiling (0 = server default).
	MaxFailures int
}

// BuildAlarm assembles a Chronos heartbeat alarm (a cron recurring alarm with
// the canonical heartbeat wake_message). The alarm is active and ready to be
// stored via store.CreateAlarm. The caller must set OwnerDID + UserID before
// persisting (the server does this from the authenticated principal).
func BuildAlarm(spec Spec, now time.Time) (types.Alarm, error) {
	if spec.ConversationID == "" {
		return types.Alarm{}, fmt.Errorf("heartbeat: conversation_id is required")
	}
	cronExpr := spec.CronExpr
	if cronExpr == "" {
		cronExpr = DefaultCronExpr
	}
	tz := spec.Timezone
	if tz == "" {
		tz = "UTC"
	}
	next, err := schedule.NextCron(cronExpr, tz, now)
	if err != nil {
		return types.Alarm{}, fmt.Errorf("heartbeat: resolve next fire: %w", err)
	}
	return types.Alarm{
		Kind:           types.KindCron,
		Label:          spec.Label,
		CronExpr:       cronExpr,
		Timezone:       tz,
		NextFireAt:     next,
		ConversationID: spec.ConversationID,
		WakeMessage:    WakeMessage,
		Status:         types.StatusActive,
		IdempotencyKey: spec.IdempotencyKey,
		MaxFailures:    spec.MaxFailures,
	}, nil
}
