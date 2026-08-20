// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package automatrix implements the Chronos AUTOMATRIX convention: a recurring
// alarm whose wake_message tells Neo it has idle time to review its pending
// non-financial opportunities, complete one end-to-end, and notify the user
// with the result — replying with the AUTOMATRIX_IDLE sentinel when nothing is
// worth doing right now.
//
// Chronos already supports durable recurring alarms with wake_message +
// payload + conversation_id; this package provides the CONVENTION — the
// canonical wake_message format and a BuildAlarm helper that assembles a
// correctly-shaped AUTOMATRIX alarm. It is a sibling of internal/heartbeat.
package automatrix

import (
	"fmt"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/chronos/internal/schedule"
	"github.com/Sidiora-Labs/centra-llm-agents/chronos/pkg/types"
)

// WakeMessage is the canonical wake_message a Chronos AUTOMATRIX alarm
// delivers. It instructs the agent to review its pending non-financial
// opportunities, complete one if worthwhile, and notify the user — or reply
// with the AUTOMATRIX_IDLE sentinel when nothing is worth doing right now.
//
// This MUST stay in sync with the agent-side AutomatrixWakeMessage
// (agents/neo/internal/agent/automatrix.go) — they share the AUTOMATRIX marker that
// the agent uses to detect an AUTOMATRIX wake.
const WakeMessage = "AUTOMATRIX: you have idle time. Review your pending non-financial opportunities and, if a worthwhile one exists, pick ONE and complete it end-to-end, then notify the user with the result. If nothing is worth doing right now, reply with exactly AUTOMATRIX_IDLE."

// DefaultCronExpr is the default recurring interval for an AUTOMATRIX alarm
// when none is specified. @every 45m is the base idle-wake cadence; Neo
// reschedules each subsequent fire with randomized jitter so wakes feel random
// rather than clockwork.
const DefaultCronExpr = "@every 45m"

// Spec configures an AUTOMATRIX alarm.
type Spec struct {
	// ConversationID is the conversation the wake resumes into (required — an
	// AUTOMATRIX wake without a conversation to resume is meaningless).
	ConversationID string
	// CronExpr is the recurring schedule. Defaults to DefaultCronExpr when empty.
	CronExpr string
	// Timezone is the IANA timezone for the cron expression (default UTC).
	Timezone string
	// Label is an optional human-readable label for the alarm.
	Label string
	// IdempotencyKey deduplicates the alarm (recommended for AUTOMATRIX alarms —
	// a user enables Automatrix exactly once).
	IdempotencyKey string
	// MaxFailures is the wake-delivery retry ceiling (0 = server default).
	MaxFailures int
}

// BuildAlarm assembles a Chronos AUTOMATRIX alarm (a cron recurring alarm with
// the canonical AUTOMATRIX wake_message). The alarm is active and ready to be
// stored via store.CreateAlarm. The caller must set OwnerDID + UserID before
// persisting (the server does this from the authenticated principal).
func BuildAlarm(spec Spec, now time.Time) (types.Alarm, error) {
	if spec.ConversationID == "" {
		return types.Alarm{}, fmt.Errorf("automatrix: conversation_id is required")
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
		return types.Alarm{}, fmt.Errorf("automatrix: resolve next fire: %w", err)
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
