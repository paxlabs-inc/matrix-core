// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"matrix/neo/internal/agent"
	"matrix/neo/internal/briefsettings"
)

// The ORACLE MORNING_BRIEF wake handler (task 5.4, req 14.3/15.3).
//
// A Chronos MORNING_BRIEF alarm fires at the user's chosen local time on their
// chosen days and is delivered as a POST /chat turn carrying the MORNING_BRIEF
// marker. This handler is the fail-closed decision gate in front of any brief
// run. It mirrors the AUTOMATRIX wake posture but the schedule is wall-clock
// (a specific local date/time), so the guards are date/day-based rather than a
// jittered cadence:
//
//  1. re-read the durable schedule on EVERY wake (req 14.3) through the governor
//     — a nil governor fails closed (do nothing);
//  2. do ZERO network retrieval when the brief is disabled or paused (req 15.3):
//     the run is never dispatched, so no search/fetch tool ever fires;
//  3. skip a non-selected weekday (the alarm cron already restricts days, but
//     the handler re-checks against the live settings so a same-day settings
//     edit is honored);
//  4. never deliver twice for one local date (req 14.3): the last-delivered date
//     is consulted before dispatch and set only after a durable delivery;
//  5. defer when a brief is already in flight (a duplicate fire never
//     double-dispatches the same day's brief).
//
// The wake produces no live conversational run of its own — the brief is
// composed out of band by the supervised runner and delivered as a durable
// conversation turn + inbox record (task 5.4 / req 15.2). Chronos remains the
// durable timing backbone; only the guard DECISIONS live here.

// briefWakeOutcome is the decision the wake gate reaches (used for the honest
// accepted-envelope reason + test assertions).
type briefWakeOutcome int

const (
	briefWakeNoGovernor briefWakeOutcome = iota // no opt-in source wired — fail closed
	briefWakeDisabled                           // opt-in off — zero network, do nothing (req 15.3)
	briefWakePaused                             // paused — zero network, do nothing (req 15.3)
	briefWakeOffDay                             // today is not a selected weekday — skip
	briefWakeAlready                            // already delivered for this local date — skip (req 14.3)
	briefWakeBusy                               // a brief is already in flight — defer
	briefWakeNoRunner                           // eligible, but no runner wired — nothing runs
	briefWakeDispatched                         // eligible — the supervised brief run was dispatched
)

// MaybeHandleMorningBriefWake intercepts a Chronos MORNING_BRIEF wake delivered
// over POST /chat. It returns true iff the message is a MORNING_BRIEF wake (so
// the caller must NOT dispatch it as a normal user turn): the wake produces no
// live conversational run of its own — the brief is composed out of band by the
// runner and delivered durably (task 5.4). A non-wake message returns false and
// is left for the normal /chat path.
func (e *Engine) MaybeHandleMorningBriefWake(ctx context.Context, convID, message string) bool {
	if !agent.IsMorningBriefWake(message) {
		return false
	}
	e.handleMorningBriefWake(ctx)
	return true
}

// handleMorningBriefWake runs the fail-closed wake gate and, on an eligible
// wake, dispatches the supervised restricted-surface brief run. Best-effort
// throughout — a governor/runner error is logged and never wedges the wake.
func (e *Engine) handleMorningBriefWake(ctx context.Context) briefWakeOutcome {
	out := e.decideMorningBriefWake(time.Now())
	if out != briefWakeDispatched {
		return out
	}
	st := e.briefGov.SettingsView()
	convID := briefStateForConv(st)
	localDate := briefLocalDate(st.Timezone, time.Now())
	if e.briefRunner == nil {
		fmt.Fprintln(os.Stderr, "neo/brief: wake eligible but no runner wired (task 5.4); skipping")
		return briefWakeNoRunner
	}
	if err := e.briefRunner(ctx, convID, localDate); err != nil {
		fmt.Fprintf(os.Stderr, "neo/brief: run brief: %v\n", err)
	}
	return briefWakeDispatched
}

// decideMorningBriefWake is the PURE fail-closed wake gate over the live
// schedule (re-read this wake) and the current time. It performs NO dispatch and
// NO network — it only classifies the wake, so a disabled/paused/off-day/
// already-delivered/busy wake provably never reaches the runner (req 15.3).
func (e *Engine) decideMorningBriefWake(now time.Time) briefWakeOutcome {
	gov := e.briefGov
	if gov == nil {
		// No opt-in source wired: a wake cannot verify the user opted in, so do
		// nothing — fail closed. A brief never runs without an authoritative
		// enabled schedule.
		return briefWakeNoGovernor
	}
	if !gov.Enabled() {
		return briefWakeDisabled
	}
	if gov.Settings().Paused() {
		return briefWakePaused
	}
	st := gov.SettingsView()
	if !briefDaySelected(st.Days, briefLocalWeekday(st.Timezone, now)) {
		return briefWakeOffDay
	}
	localDate := briefLocalDate(st.Timezone, now)
	if gov.Settings().AlreadyDelivered(localDate) {
		return briefWakeAlready
	}
	if e.briefInflight.Load() > 0 {
		// A brief is already underway (a duplicate fire, or a resumed run):
		// never double-dispatch the same local date's brief.
		return briefWakeBusy
	}
	return briefWakeDispatched
}

// briefLocalWeekday returns the current weekday (0=Sunday..6=Saturday) in the
// settings timezone. A bad/empty tz falls back to UTC.
func briefLocalWeekday(tz string, now time.Time) int {
	loc, err := time.LoadLocation(strings.TrimSpace(tz))
	if err != nil || strings.TrimSpace(tz) == "" {
		loc = time.UTC
	}
	return int(now.In(loc).Weekday())
}

// briefDaySelected reports whether weekday (0=Sun..6=Sat) is in the selected
// set. An empty set means every day (matching the alarm cron's "*").
func briefDaySelected(days []int, weekday int) bool {
	if len(days) == 0 {
		return true
	}
	for _, d := range days {
		n := d % 7
		if n < 0 {
			n += 7
		}
		if n == weekday {
			return true
		}
	}
	return false
}

// briefStateForConv resolves the destination conversation the brief is delivered
// into from the live schedule, defaulting to the wake conversation when unset.
func briefStateForConv(st briefsettings.State) string {
	if c := strings.TrimSpace(st.ConversationID); c != "" {
		return c
	}
	return briefWakeConvID
}
