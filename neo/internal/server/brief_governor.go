// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/briefsettings"
	"matrix/neo/internal/tools"
)

// The ORACLE morning-brief governor (task 5.2) — the fail-closed opt-in +
// MORNING_BRIEF Chronos-alarm bookkeeping seam. It is the brief-side sibling of
// automatrixGovernor and shares its posture:
//
//   - a durable per-user settings sidecar (briefsettings) — the authoritative
//     schedule state (enabled, tz, local time, days, length, sections, paused,
//     the single alarm id, destination conversation, last-delivered date) the
//     wake handler re-reads live on every fire (req 14.3);
//   - a BriefAlarmController — the Chronos alarm_set/alarm_cancel lifecycle:
//     enabling creates exactly ONE recurring MORNING_BRIEF alarm at the user's
//     local time (req 14.2), disabling cancels it, a settings change reschedules
//     it (cancel + recreate with the new cron), and pause keeps the alarm but
//     the wake handler skips.
//
// The governor holds no signing key and performs no on-chain action — it flips a
// boolean and asks Chronos (which it owns an alarm in) to create/cancel that one
// alarm. Enable is FAIL-CLOSED: the opt-in only flips on once the alarm exists.

// BriefAlarmSpec is the shape of the single recurring MORNING_BRIEF alarm the
// governor asks the controller to create. Unlike the AUTOMATRIX spec this
// carries an explicit Timezone + wall-clock CronExpr, since the brief fires at
// the user's local delivery time (DST-correct via Chronos cron+tz).
type BriefAlarmSpec struct {
	ConversationID string
	CronExpr       string
	Timezone       string
	WakeMessage    string
	IdempotencyKey string
	Label          string
}

// BriefAlarmController is the Chronos alarm_set/alarm_cancel boundary for the
// brief. Set returns the created alarm's id so the governor can later cancel
// exactly its own alarm.
type BriefAlarmController interface {
	Set(ctx context.Context, spec BriefAlarmSpec) (alarmID string, err error)
	Cancel(ctx context.Context, alarmID string) error
}

// briefGovernor is the production morning-brief governor.
type briefGovernor struct {
	settings *briefsettings.Store
	alarms   BriefAlarmController
	convID   string
	idemKey  string
	label    string
}

// briefWakeConvID is the default conversation Chronos delivers MORNING_BRIEF
// wakes into when the user has not pinned a destination. The wake is intercepted
// by the engine before conversational dispatch; the brief itself is delivered
// into the configured destination conversation.
const briefWakeConvID = "morning-brief-wake"

// briefAlarmIdempotencyKey is the stable dedup key for the single per-agent
// MORNING_BRIEF alarm, so re-enabling never creates a second (req 14.2).
const briefAlarmIdempotencyKey = "neo-morning-brief"

// newBriefGovernor builds the production governor from the durable settings
// sidecar and the Chronos alarm controller.
func newBriefGovernor(settings *briefsettings.Store, alarms BriefAlarmController) *briefGovernor {
	return &briefGovernor{
		settings: settings,
		alarms:   alarms,
		convID:   briefWakeConvID,
		idemKey:  briefAlarmIdempotencyKey,
		label:    "Neo morning brief",
	}
}

// Enabled returns the authoritative per-user opt-in, re-read live (req 14.3).
func (g *briefGovernor) Enabled() bool { return g.settings.Enabled() }

// Active reports whether the brief should actually fire: enabled AND not paused.
func (g *briefGovernor) Active() bool {
	return g.settings.Enabled() && !g.settings.Paused()
}

// SettingsView is the read model for GET /brief/settings.
func (g *briefGovernor) SettingsView() briefsettings.State { return g.settings.View() }

// Settings exposes the underlying store to the wake handler (task 5.4).
func (g *briefGovernor) Settings() *briefsettings.Store { return g.settings }

// UpdateSchedule persists the schedule fields and, when the brief is enabled and
// not paused, reschedules the alarm to the new local time/days (req 14.2).
func (g *briefGovernor) UpdateSchedule(ctx context.Context, sc briefsettings.Schedule) error {
	if err := g.settings.SetSchedule(sc); err != nil {
		return err
	}
	if g.settings.Enabled() && !g.settings.Paused() {
		return g.reschedule(ctx)
	}
	return nil
}

// SetEnabled is the control-surface toggle (req 11.3/14.2): enabling creates
// exactly ONE MORNING_BRIEF alarm (idempotent — a second enable with a live
// alarm is a no-op), disabling cancels it immediately. Enable is FAIL-CLOSED:
// the opt-in flips on only after the alarm is created.
func (g *briefGovernor) SetEnabled(ctx context.Context, enabled bool) error {
	if enabled {
		if g.settings.Enabled() && g.settings.AlarmID() != "" {
			return nil
		}
		alarmID, err := g.createAlarm(ctx)
		if err != nil {
			return fmt.Errorf("brief: create alarm: %w", err)
		}
		return g.settings.SetEnabled(true, alarmID)
	}
	// Disable: cancel this agent's own alarm, then persist the opt-in off.
	if alarmID := g.settings.AlarmID(); alarmID != "" {
		if err := g.alarms.Cancel(ctx, alarmID); err != nil {
			_ = g.settings.SetEnabled(false, alarmID)
			return fmt.Errorf("brief: cancel alarm: %w", err)
		}
	}
	return g.settings.SetEnabled(false, "")
}

// SetPaused pauses/resumes delivery without disabling. The alarm is retained;
// the wake handler skips while paused (req 14.2). Persisted so it takes effect
// on the next wake without a restart.
func (g *briefGovernor) SetPaused(ctx context.Context, paused bool) error {
	return g.settings.SetPaused(paused)
}

// reschedule cancels the current alarm and creates a fresh one from the current
// schedule. Fail-closed: if the recreate fails, the opt-in is turned off (an
// enabled setting with no alarm would never wake) and the error is surfaced.
func (g *briefGovernor) reschedule(ctx context.Context) error {
	if old := g.settings.AlarmID(); old != "" {
		if err := g.alarms.Cancel(ctx, old); err != nil {
			return fmt.Errorf("brief: cancel for reschedule: %w", err)
		}
	}
	alarmID, err := g.createAlarm(ctx)
	if err != nil {
		_ = g.settings.SetEnabled(false, "")
		return fmt.Errorf("brief: recreate alarm: %w", err)
	}
	return g.settings.SetAlarmID(alarmID)
}

// createAlarm builds the cron from the current schedule and asks the controller
// to create the single MORNING_BRIEF alarm.
func (g *briefGovernor) createAlarm(ctx context.Context) (string, error) {
	st := g.settings.View()
	cron, err := buildBriefCron(st.DeliveryTime, st.Days)
	if err != nil {
		return "", err
	}
	tz := strings.TrimSpace(st.Timezone)
	if tz == "" {
		tz = "UTC"
	}
	conv := strings.TrimSpace(st.ConversationID)
	if conv == "" {
		conv = g.convID
	}
	return g.alarms.Set(ctx, BriefAlarmSpec{
		ConversationID: conv,
		CronExpr:       cron,
		Timezone:       tz,
		WakeMessage:    agent.MorningBriefWakeMessage,
		IdempotencyKey: g.idemKey,
		Label:          g.label,
	})
}

// briefDefaultDeliveryTime mirrors chronos/internal/morningbrief.DefaultDeliveryTime.
const briefDefaultDeliveryTime = "07:00"

// buildBriefCron renders the standard 5-field cron ("M H * * DOW") for a local
// delivery time (HH:MM) and weekday set (0=Sun..6=Sat; empty/full = every day).
// Mirrors chronos/internal/morningbrief.CronExpr so the neo-side alarm request
// and the chronos-side convention agree byte-for-byte.
func buildBriefCron(deliveryTime string, days []int) (string, error) {
	dt := strings.TrimSpace(deliveryTime)
	if dt == "" {
		dt = briefDefaultDeliveryTime
	}
	var hour, min int
	if _, err := fmt.Sscanf(dt, "%d:%d", &hour, &min); err != nil {
		return "", fmt.Errorf("brief: invalid delivery time %q (want HH:MM): %w", deliveryTime, err)
	}
	if hour < 0 || hour > 23 || min < 0 || min > 59 {
		return "", fmt.Errorf("brief: delivery time %q out of range", deliveryTime)
	}
	return fmt.Sprintf("%d %d * * %s", min, hour, briefDOWField(days)), nil
}

func briefDOWField(days []int) string {
	if len(days) == 0 {
		return "*"
	}
	seen := make(map[int]struct{}, len(days))
	nums := make([]int, 0, len(days))
	for _, d := range days {
		n := d % 7
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

// briefLocalDate returns the current date (YYYY-MM-DD) in the settings timezone,
// the key the wake handler uses for the single-delivery-per-local-date guard
// (req 14.3). A bad/empty tz falls back to UTC.
func briefLocalDate(tz string, now time.Time) string {
	loc, err := time.LoadLocation(strings.TrimSpace(tz))
	if err != nil || strings.TrimSpace(tz) == "" {
		loc = time.UTC
	}
	return now.In(loc).Format("2006-01-02")
}

// --- production Chronos alarm controller (MCP alarm_set / alarm_cancel) ---

// briefMCPAlarmController issues the Chronos alarm lifecycle through Neo's MCP
// tool surface (the shared alarm_set / alarm_cancel tools), passing the brief's
// explicit timezone + wall-clock cron. It reuses the alarm-id parsing and tool
// name suffixes shared with the automatrix controller.
type briefMCPAlarmController struct {
	tools *tools.Manager
}

func newBriefMCPAlarmController(tm *tools.Manager) *briefMCPAlarmController {
	return &briefMCPAlarmController{tools: tm}
}

func (c *briefMCPAlarmController) Set(ctx context.Context, spec BriefAlarmSpec) (string, error) {
	fn, ok := c.resolveTool(alarmSetToolSuffix)
	if !ok {
		return "", fmt.Errorf("brief: chronos %q tool not available", alarmSetToolSuffix)
	}
	args := map[string]interface{}{
		"kind":            "cron",
		"cron_expr":       spec.CronExpr,
		"timezone":        spec.Timezone,
		"conversation_id": spec.ConversationID,
		"wake_message":    spec.WakeMessage,
		"idempotency_key": spec.IdempotencyKey,
		"label":           spec.Label,
	}
	out, isErr, err := c.tools.Dispatch(ctx, fn, args)
	if err != nil {
		return "", err
	}
	if isErr {
		return "", fmt.Errorf("brief: alarm_set failed: %s", strings.TrimSpace(out))
	}
	id := parseAlarmID(out)
	if id == "" {
		return "", fmt.Errorf("brief: alarm_set returned no parseable id: %s", strings.TrimSpace(out))
	}
	return id, nil
}

func (c *briefMCPAlarmController) Cancel(ctx context.Context, alarmID string) error {
	fn, ok := c.resolveTool(alarmCancelToolSuffix)
	if !ok {
		return fmt.Errorf("brief: chronos %q tool not available", alarmCancelToolSuffix)
	}
	out, isErr, err := c.tools.Dispatch(ctx, fn, map[string]interface{}{
		"id":       alarmID,
		"alarm_id": alarmID,
	})
	if err != nil {
		return err
	}
	if isErr {
		return fmt.Errorf("brief: alarm_cancel failed: %s", strings.TrimSpace(out))
	}
	return nil
}

func (c *briefMCPAlarmController) resolveTool(suffix string) (string, bool) {
	if c.tools == nil {
		return "", false
	}
	for _, name := range c.tools.NaturalToolNames() {
		if name == suffix || strings.HasSuffix(name, suffix) {
			return name, true
		}
	}
	return "", false
}
