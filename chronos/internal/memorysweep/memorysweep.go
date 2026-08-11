// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package memorysweep implements the Chronos CONTINUOUS-MEMORY-SWEEP
// convention: a recurring alarm whose wake_message triggers the cortex
// temporal-ladder cascade (cortex.Cortex.SweepNow / cortex.Cascade) to run
// eagerly on window-close (continuous-memory spec req.5.1).
//
// Chronos already supports durable recurring alarms with wake_message +
// payload + conversation_id; this package provides the CONVENTION — the
// canonical wake_message marker and a BuildAlarm helper that assembles a
// correctly-shaped sweep alarm. It is a sibling of internal/heartbeat and
// internal/automatrix.
//
// UNLIKE heartbeat/automatrix, this wake is NOT a conversational prompt for
// the model: it carries no request for the agent to reason or reply. The
// wake_message is a recognizable MARKER whose receiver — the process holding
// the actor's live *cortex.Cortex — SHALL, on detecting it, call
// cortex.Cortex.SweepNow directly and produce no chat turn. Chronos's role is
// limited to delivering the trigger on a schedule (via the existing
// dispatch.Worker -> wake.Waker machinery, exactly like every other alarm);
// it never holds a cortex handle, never signs anything, and never runs a
// plan/walk, so it gains NO new signing / cortex-write / plan-walk capability
// (req.5.3). The rollup write happens entirely inside cortex, on the derived
// lane (matrix/cortex/cascade.go), driven by this trigger.
package memorysweep

import (
	"fmt"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/chronos/internal/schedule"
	"github.com/Sidiora-Labs/centra-llm-agents/chronos/pkg/types"
)

// WakeMessage is the canonical wake_message a Chronos continuous-memory-sweep
// alarm delivers. It carries the CONTINUOUS_MEMORY_SWEEP marker so the
// receiver can distinguish it from a conversational wake (heartbeat /
// AUTOMATRIX) and from a normal user message, and route it directly to
// cortex.Cortex.SweepNow without invoking the model.
const WakeMessage = "CONTINUOUS_MEMORY_SWEEP: window-close trigger. Do not answer this — run the cortex temporal-ladder cascade for the actor's journal (cortex.SweepNow) and produce no reply."

// DefaultCronExpr is the default recurring interval for a continuous-memory
// sweep alarm when none is specified. @every 1h matches the finest ladder
// resolution (TierHour, matrix/cortex/rollup.go) so closed hour windows are
// picked up promptly; Cascade's own idempotence makes a tighter or looser
// cadence safe too (bounded work per sweep either way).
const DefaultCronExpr = "@every 1h"

// Spec configures a continuous-memory-sweep alarm.
type Spec struct {
	// ConversationID is the delivery target for the wake (required by the
	// underlying alarm machinery); the sweep itself is NOT scoped to this
	// conversation — cortex.SweepNow cascades the whole actor journal.
	ConversationID string
	// CronExpr is the recurring schedule. Defaults to DefaultCronExpr when empty.
	CronExpr string
	// Timezone is the IANA timezone for the cron expression (default UTC).
	Timezone string
	// Label is an optional human-readable label for the alarm.
	Label string
	// IdempotencyKey deduplicates the alarm (recommended — a sweep alarm is
	// enabled at most once per actor).
	IdempotencyKey string
	// MaxFailures is the wake-delivery retry ceiling (0 = server default).
	MaxFailures int
}

// BuildAlarm assembles a Chronos continuous-memory-sweep alarm (a cron
// recurring alarm with the canonical sweep wake_message). The alarm is
// active and ready to be stored via store.CreateAlarm. The caller must set
// OwnerDID + UserID before persisting (the server does this from the
// authenticated principal).
func BuildAlarm(spec Spec, now time.Time) (types.Alarm, error) {
	if spec.ConversationID == "" {
		return types.Alarm{}, fmt.Errorf("memorysweep: conversation_id is required")
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
		return types.Alarm{}, fmt.Errorf("memorysweep: resolve next fire: %w", err)
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
