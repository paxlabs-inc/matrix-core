// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

// Package morningbrief implements the Chronos MORNING_BRIEF convention: a
// recurring alarm that wakes Neo at the user's chosen LOCAL time on their chosen
// days to compose a short, source-backed personalized brief and deliver it.
//
// It is a sibling of internal/automatrix — same posture (a canonical
// wake_message carrying a marker the agent detects, plus a BuildAlarm helper
// that assembles a correctly-shaped Chronos alarm) — but where AUTOMATRIX is an
// @every idle cadence, MORNING_BRIEF is a wall-clock cron ("at HH:MM on selected
// weekdays") evaluated in the user's IANA timezone, so delivery is DST-correct
// (ORACLE req 14.2).
package morningbrief

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/chronos/internal/schedule"
	"github.com/Sidiora-Labs/centra-llm-agents/chronos/pkg/types"
)

// WakeMessage is the canonical wake_message a Chronos MORNING_BRIEF alarm
// delivers. It carries the MORNING_BRIEF marker the agent uses to detect a brief
// wake (which routes to the restricted, supervised brief runner rather than a
// normal conversational dispatch). It MUST stay in sync with the agent-side
// marker constant.
const WakeMessage = "MORNING_BRIEF: it is time to compose the user's personalized brief. Gather fresh, source-backed items across their stated interests and deliver a short brief. If personalization or the brief is disabled, do nothing."

// Marker is the token every MORNING_BRIEF wake_message carries so the agent can
// detect the wake.
const Marker = "MORNING_BRIEF"

// DefaultDeliveryTime is the local delivery time used when none is specified.
const DefaultDeliveryTime = "07:00"

// Spec configures a MORNING_BRIEF alarm.
type Spec struct {
	// ConversationID is the conversation the wake resumes into (required).
	ConversationID string
	// DeliveryTime is the local delivery time as "HH:MM" (24h). Defaults to
	// DefaultDeliveryTime when empty.
	DeliveryTime string
	// Days restricts delivery to these weekdays. Empty = every day.
	Days []time.Weekday
	// Timezone is the IANA timezone the delivery time is evaluated in
	// (default UTC). DST correctness comes from cron+timezone evaluation.
	Timezone string
	// Label is an optional human-readable label for the alarm.
	Label string
	// IdempotencyKey deduplicates the alarm (a user enables the brief once).
	IdempotencyKey string
	// MaxFailures is the wake-delivery retry ceiling (0 = server default).
	MaxFailures int
}

// CronExpr renders the standard 5-field cron expression ("M H * * DOW") for a
// local delivery time and weekday set. An empty/whole day set yields "*" for
// the day-of-week field (every day). Weekdays follow cron's Sunday=0..Saturday=6.
func CronExpr(deliveryTime string, days []time.Weekday) (string, error) {
	dt := strings.TrimSpace(deliveryTime)
	if dt == "" {
		dt = DefaultDeliveryTime
	}
	var hour, min int
	if _, err := fmt.Sscanf(dt, "%d:%d", &hour, &min); err != nil {
		return "", fmt.Errorf("morningbrief: invalid delivery time %q (want HH:MM): %w", deliveryTime, err)
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 {
		return "", fmt.Errorf("morningbrief: delivery time %q out of range", deliveryTime)
	}
	return fmt.Sprintf("%d %d * * %s", min, hour, dowField(days)), nil
}

// dowField renders the day-of-week cron field: "*" for an empty/full set, else
// a sorted, deduped comma list of 0-6.
func dowField(days []time.Weekday) string {
	if len(days) == 0 {
		return "*"
	}
	seen := make(map[int]struct{}, len(days))
	nums := make([]int, 0, len(days))
	for _, d := range days {
		n := int(d) % 7
		if n < 0 {
			n += 7
		}
		if _, dup := seen[n]; dup {
			continue
		}
		seen[n] = struct{}{}
		nums = append(nums, n)
	}
	if len(nums) >= 7 {
		return "*"
	}
	sort.Ints(nums)
	parts := make([]string, len(nums))
	for i, n := range nums {
		parts[i] = fmt.Sprintf("%d", n)
	}
	return strings.Join(parts, ",")
}

// BuildAlarm assembles a Chronos MORNING_BRIEF alarm (a cron recurring alarm at
// the user's local delivery time on the selected days, carrying the canonical
// MORNING_BRIEF wake_message). The caller sets OwnerDID + UserID before
// persisting (the server does this from the authenticated principal).
func BuildAlarm(spec Spec, now time.Time) (types.Alarm, error) {
	if spec.ConversationID == "" {
		return types.Alarm{}, fmt.Errorf("morningbrief: conversation_id is required")
	}
	cronExpr, err := CronExpr(spec.DeliveryTime, spec.Days)
	if err != nil {
		return types.Alarm{}, err
	}
	tz := spec.Timezone
	if tz == "" {
		tz = "UTC"
	}
	next, err := schedule.NextCron(cronExpr, tz, now)
	if err != nil {
		return types.Alarm{}, fmt.Errorf("morningbrief: resolve next fire: %w", err)
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
