// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/automatrixsettings"
	"matrix/neo/internal/tools"
)

// The production AutomatrixGovernor (task 6.1) — the real implementation of the
// per-user opt-in + Chronos-alarm bookkeeping seam the wake handler (task 3.3)
// consumes on every wake. It is backed by:
//
//   - a durable per-user opt-in store (automatrixsettings) — the authoritative
//     `automatrix_enabled` setting (default false, req 7.1) the engine re-reads
//     live on every wake, so a toggle takes effect without a process restart
//     (req 7.5); it also holds the single alarm id (DID-scoped, req 8.1) and
//     the per-day proactive-task counter (req 4.7);
//   - an AutomatrixAlarmController — the Chronos alarm_set/alarm_cancel
//     lifecycle (req 4.1): enabling creates exactly ONE recurring AUTOMATRIX
//     alarm, disabling cancels it. The production controller issues these
//     through Neo's MCP tool surface (the Chronos alarm_set/alarm_cancel
//     tools); Chronos remains the durable timing backbone.
//
// The governor never holds a signing key and performs no on-chain action — it
// only flips a boolean and asks Chronos (which it owns an alarm in) to
// create/cancel that one alarm (frozen invariant i1, Chronos i5/i7, req 8.1).

// AutomatrixAlarmSpec is the shape of the single recurring AUTOMATRIX alarm the
// governor asks the controller to create on enable. The wake_message is fixed
// (the canonical AUTOMATRIX marker text, agent.AutomatrixWakeMessage); only the
// delivery conversation, cadence, and dedup key vary.
type AutomatrixAlarmSpec struct {
	// ConversationID is the conversation Chronos delivers the wake turn into
	// (required by Chronos). The proactive run itself resumes into each picked
	// opportunity's own origin conversation (task 4.1), independent of this.
	ConversationID string
	// CronExpr is the base recurring cadence (e.g. "@every 45m"); Neo
	// reschedules each fire with jitter so wakes feel random (req 4.3).
	CronExpr string
	// WakeMessage is the canonical AUTOMATRIX wake_message (carries the marker
	// the agent detects). Set by the governor; the controller passes it through.
	WakeMessage string
	// IdempotencyKey deduplicates the alarm so re-enabling never creates a
	// second one (a user enables Automatrix exactly once; req 4.1 "exactly ONE").
	IdempotencyKey string
	// Label is a human-readable label for the alarm.
	Label string
}

// AutomatrixAlarmController is the Chronos alarm_set/alarm_cancel boundary (the
// external Chronos system). The production implementation (mcpAlarmController)
// issues these through Neo's MCP tool surface; Chronos owns the durable timing
// state. Set returns the created alarm's id so the governor can later cancel
// exactly its own alarm (DID-scoped, req 8.1).
type AutomatrixAlarmController interface {
	Set(ctx context.Context, spec AutomatrixAlarmSpec) (alarmID string, err error)
	Cancel(ctx context.Context, alarmID string) error
}

// automatrixGovernor is the production AutomatrixGovernor. It satisfies the
// engine's AutomatrixGovernor seam and additionally exposes SetEnabled /
// SettingsView for the control-surface endpoints (task 6.1).
type automatrixGovernor struct {
	settings *automatrixsettings.Store
	alarms   AutomatrixAlarmController
	convID   string // the AUTOMATRIX alarm's delivery conversation
	cronExpr string // base wake cadence
	idemKey  string // stable dedup key (one alarm per agent)
	label    string
}

// automatrixWakeConvID is the default conversation Chronos delivers AUTOMATRIX
// wakes into. It is a stable, non-user-thread id (the wake is intercepted by
// the engine before any conversational dispatch; the real proactive run resumes
// into each opportunity's own origin conversation, task 4.1).
const automatrixWakeConvID = "automatrix-wake"

// automatrixAlarmIdempotencyKey is the stable dedup key for the single per-agent
// AUTOMATRIX alarm, so re-enabling (or a retried enable) never creates a second
// alarm (req 4.1 "exactly ONE").
const automatrixAlarmIdempotencyKey = "neo-automatrix-wake"

// newAutomatrixGovernor builds the production governor from the durable opt-in
// store, the Chronos alarm controller, and the base wake cadence (minutes).
func newAutomatrixGovernor(settings *automatrixsettings.Store, alarms AutomatrixAlarmController, baseIntervalMinutes int) *automatrixGovernor {
	mins := baseIntervalMinutes
	if mins <= 0 {
		mins = 45 // mirrors the [automatrix] base_interval_minutes default
	}
	return &automatrixGovernor{
		settings: settings,
		alarms:   alarms,
		convID:   automatrixWakeConvID,
		cronExpr: fmt.Sprintf("@every %dm", mins),
		idemKey:  automatrixAlarmIdempotencyKey,
		label:    "Neo Automatrix idle-wake",
	}
}

// Enabled returns the authoritative per-user opt-in, re-read live (req 7.1).
func (g *automatrixGovernor) Enabled(context.Context) bool {
	return g.settings.Enabled()
}

// TasksToday returns the proactive-task count for the current UTC day (req 4.7).
func (g *automatrixGovernor) TasksToday(context.Context) int {
	return g.settings.TasksToday()
}

// RecordTaskStarted advances the per-day counter after a proactive task is
// dispatched (req 4.7).
func (g *automatrixGovernor) RecordTaskStarted(context.Context) error {
	return g.settings.RecordTaskStarted()
}

// CancelAlarm is the wake-side opt-out path (req 4.4): cancel this agent's own
// AUTOMATRIX alarm and persist the opt-in as off. Best-effort on the external
// cancel — if Chronos rejects it the opt-in still goes off (so the engine runs
// no further work) and the alarm id is retained for a later retry.
func (g *automatrixGovernor) CancelAlarm(ctx context.Context) error {
	alarmID := g.settings.AlarmID()
	if alarmID != "" {
		if err := g.alarms.Cancel(ctx, alarmID); err != nil {
			// Keep the opt-in off (the user/engine intent) but retain the id so
			// a later cancel can retry rather than orphaning the alarm.
			_ = g.settings.SetEnabled(false, alarmID)
			return err
		}
	}
	return g.settings.SetEnabled(false, "")
}

// Reschedule is the per-fire jittered next-fire hook (req 4.3). The recurring
// Chronos cron alarm is the durable timing backbone and fires on its base
// cadence regardless; updating a specific next-fire instant requires a Chronos
// alarm-mutation capability the current MCP surface does not expose, so this is
// a deliberate no-op here — the jitter/skip DECISION still runs in the engine
// (it just does not yet rewrite the DB's next-fire). Honest partial, tracked
// for a follow-up once Chronos exposes an alarm-update tool.
func (g *automatrixGovernor) Reschedule(context.Context, time.Time) error {
	return nil
}

// SetEnabled is the control-surface toggle (task 6.1, req 7.5): enabling creates
// exactly ONE recurring AUTOMATRIX alarm (idempotent — a second enable is a
// no-op while one already exists), disabling cancels it. The opt-in flag is
// persisted so the change takes effect on the next wake without a process
// restart.
func (g *automatrixGovernor) SetEnabled(ctx context.Context, enabled bool) error {
	if enabled {
		// Exactly one alarm: if already enabled with a live alarm, do nothing.
		if g.settings.Enabled() && g.settings.AlarmID() != "" {
			return nil
		}
		alarmID, err := g.alarms.Set(ctx, AutomatrixAlarmSpec{
			ConversationID: g.convID,
			CronExpr:       g.cronExpr,
			WakeMessage:    agent.AutomatrixWakeMessage,
			IdempotencyKey: g.idemKey,
			Label:          g.label,
		})
		if err != nil {
			// Fail closed: do NOT flip the opt-in on if the alarm could not be
			// created — an "enabled" setting with no alarm would never wake.
			return fmt.Errorf("automatrix: create alarm: %w", err)
		}
		return g.settings.SetEnabled(true, alarmID)
	}
	// Disable: cancel this agent's own alarm, then persist the opt-in off.
	alarmID := g.settings.AlarmID()
	if alarmID != "" {
		if err := g.alarms.Cancel(ctx, alarmID); err != nil {
			_ = g.settings.SetEnabled(false, alarmID)
			return fmt.Errorf("automatrix: cancel alarm: %w", err)
		}
	}
	return g.settings.SetEnabled(false, "")
}

// SettingsView is the read model for GET /automatrix/settings.
func (g *automatrixGovernor) SettingsView() automatrixsettings.State {
	return g.settings.View()
}

// --- production Chronos alarm controller (MCP alarm_set / alarm_cancel) ---

// mcpAlarmController issues the Chronos alarm lifecycle through Neo's MCP tool
// surface — the Chronos alarm_set / alarm_cancel tools (the design's "create
// the AUTOMATRIX alarm via the Chronos MCP tool"). Neo holds no signing key;
// the MCP server authenticates to Chronos on the agent's behalf (DID-scoped,
// req 8.1). This is the production boundary to the external Chronos system; it
// is intentionally thin — assemble args, dispatch, parse the returned id.
type mcpAlarmController struct {
	tools *tools.Manager
}

// newMCPAlarmController wraps the shared tool manager. A nil manager yields a
// controller whose calls error cleanly (the daemon logs and the opt-in fails
// closed rather than silently pretending an alarm exists).
func newMCPAlarmController(tm *tools.Manager) *mcpAlarmController {
	return &mcpAlarmController{tools: tm}
}

// alarmSetToolSuffix / alarmCancelToolSuffix match the Chronos MCP tool names
// regardless of the alias prefix Neo assigns (funcName is alias+name). Resolving
// by suffix keeps the controller robust to manifest aliasing.
const (
	alarmSetToolSuffix    = "alarm_set"
	alarmCancelToolSuffix = "alarm_cancel"
)

func (c *mcpAlarmController) Set(ctx context.Context, spec AutomatrixAlarmSpec) (string, error) {
	fn, ok := c.resolveTool(alarmSetToolSuffix)
	if !ok {
		return "", fmt.Errorf("automatrix: chronos %q tool not available", alarmSetToolSuffix)
	}
	args := map[string]interface{}{
		"kind":            "cron",
		"cron_expr":       spec.CronExpr,
		"timezone":        "UTC",
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
		return "", fmt.Errorf("automatrix: alarm_set failed: %s", strings.TrimSpace(out))
	}
	id := parseAlarmID(out)
	if id == "" {
		// The alarm was created (idempotency key guards against duplicates) but
		// the id could not be parsed; surface it so the daemon logs rather than
		// silently losing the cancel handle.
		return "", fmt.Errorf("automatrix: alarm_set returned no parseable id: %s", strings.TrimSpace(out))
	}
	return id, nil
}

func (c *mcpAlarmController) Cancel(ctx context.Context, alarmID string) error {
	fn, ok := c.resolveTool(alarmCancelToolSuffix)
	if !ok {
		return fmt.Errorf("automatrix: chronos %q tool not available", alarmCancelToolSuffix)
	}
	out, isErr, err := c.tools.Dispatch(ctx, fn, map[string]interface{}{
		"id":       alarmID,
		"alarm_id": alarmID, // tolerate either arg name
	})
	if err != nil {
		return err
	}
	if isErr {
		return fmt.Errorf("automatrix: alarm_cancel failed: %s", strings.TrimSpace(out))
	}
	return nil
}

// resolveTool finds the natural tool whose function name ends with suffix
// (alias-prefix tolerant). Returns false when the manager is nil or the tool is
// not advertised in this session.
func (c *mcpAlarmController) resolveTool(suffix string) (string, bool) {
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

// parseAlarmID extracts the created alarm's id from the alarm_set result. The
// Chronos CreateAlarmResponse is {"id":...,"next_fire_at":...,"status":...};
// MCP tool text is typically that JSON (possibly wrapped). Best-effort: parse
// the first JSON object carrying an "id".
func parseAlarmID(out string) string {
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	// Direct object.
	var obj map[string]json.RawMessage
	if json.Unmarshal([]byte(out), &obj) == nil {
		if id := stringField(obj, "id"); id != "" {
			return id
		}
		if id := stringField(obj, "alarm_id"); id != "" {
			return id
		}
	}
	return ""
}

func stringField(obj map[string]json.RawMessage, key string) string {
	raw, ok := obj[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	return ""
}

// logAutomatrixGovernorErr centralises the best-effort error logging the
// control surface uses (kept off the user-facing path).
func logAutomatrixGovernorErr(stage string, err error) {
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/automatrix: %s: %v\n", stage, err)
	}
}
