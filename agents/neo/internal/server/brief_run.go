// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"centra/agents/neo/internal/notify"
	"centra/agents/neo/internal/task"
)

// The ORACLE morning-brief runner (task 5.4, req 15.1/15.2).
//
// A fired-and-eligible MORNING_BRIEF wake (see brief_wake.go) hands off to this
// runner, which drives a supervised, restart-durable brief run on the TIGHT
// read-only tool surface and delivers the composed brief out of band. It is the
// brief-side sibling of RunAutomatrixOpportunity, with two deliberate
// differences: the run surface is the tighter brief allowlist (agent.
// NewMorningBrief; req 15.1), and durability is anchored in the task ledger with
// a dedicated brief-resume path the normal reaper EXCLUDES (a brief must never
// resume on the money surface, req 15.2).

// BriefRunner dispatches the supervised, restricted-surface morning-brief run
// for a fired MORNING_BRIEF wake. convID is the destination conversation; the
// localDate (YYYY-MM-DD in the user's timezone) is the single-delivery-per-date
// key. A nil return means the brief run was cleanly started (it completes on a
// background goroutine, decoupled from the wake request). A nil runner leaves
// the wake handler doing nothing (fail closed).
type BriefRunner func(ctx context.Context, convID, localDate string) error

// SetBriefRunner wires the supervised restricted-surface brief run dispatch
// (task 5.4). Safe to leave unset: the wake handler fails closed without it.
func (e *Engine) SetBriefRunner(r BriefRunner) { e.briefRunner = r }

// briefTaskWall is the fallback wall-clock ceiling for a brief run when the
// config does not set TaskMaxWall — the same ceiling a normal supervised task
// gets, so a brief is never granted more budget than an interactive turn.
const briefTaskWall = 20 * time.Minute

// RunMorningBrief is the production BriefRunner. It records the brief durably in
// the task ledger (Kind=brief, so the normal boot reaper never resumes it on the
// money surface) and dispatches the supervised run on a background goroutine.
func (e *Engine) RunMorningBrief(_ context.Context, convID, localDate string) error {
	convID = strings.TrimSpace(convID)
	if convID == "" {
		return fmt.Errorf("neo/brief: no destination conversation; cannot run brief")
	}
	e.dispatchBriefRun(convID, localDate)
	return nil
}

// briefRunID is the deterministic supervising run/CAS id for a brief on a given
// local date. Encoding the date in the id lets the boot-resume path recover the
// single-delivery-per-date key from the durable ledger record alone, and makes a
// re-dispatch for the same date idempotent (Begin CAS on the same id).
func briefRunID(localDate string) string {
	d := strings.TrimSpace(localDate)
	if d == "" {
		d = "unscheduled"
	}
	return "brief-" + d
}

// dispatchBriefRun drives the supervised brief run for a destination
// conversation on a background goroutine, decoupled from the wake request, and
// delivers the composed brief on a genuine completion. Shared by the live wake
// path and the restart-resume sweep.
func (e *Engine) dispatchBriefRun(convID, localDate string) {
	runID := briefRunID(localDate)
	noRepeatRecs, noRepeatURLs := e.briefHistory.RecentDelivered(briefNoRepeatWindow)
	objective := buildBriefObjective(e.briefSettingsView(), e.briefProfileBlock(), noRepeatRecs, noRepeatURLs)

	// Record the brief durably BEFORE the run starts so a restart/suspend mid-
	// compose resumes it (on the restricted brief surface via RunningKind, never
	// the money surface). Kind=brief steers the boot reaper.
	e.tasks.BeginKind(convID, runID, objective, task.KindBrief)

	e.briefInflight.Add(1)
	e.briefWG.Add(1)
	go func() {
		defer e.briefWG.Done()
		defer e.briefInflight.Add(-1)
		wall := e.cfg.TaskMaxWall
		if wall <= 0 {
			wall = briefTaskWall
		}
		rctx, cancel := context.WithTimeout(context.Background(), wall)
		defer cancel()

		sess := e.newBriefSession(convID)
		status := sess.superviseBriefTask(rctx, objective)
		e.tasks.Finish(convID, runID, status)
		if status == task.StatusDone {
			e.deliverBrief(convID, runID, localDate, sess.automatrixResult())
		}
		// A non-done terminal (ceiling / deterministic blocker) surfaces
		// NOTHING (req 15.2) and does NOT mark the date delivered, so a later
		// wake (or a boot resume) retries the brief for the same local date.
	}()
}

// deliverBrief persists a genuinely-composed brief out of band and only then
// notifies (req 15.2: durable conversation turn + inbox record BEFORE any live
// notification). The delivery ORDER is load-bearing: the durable surfaces are
// the canonical record (they survive no-client and a failed external send); the
// last-delivered date is marked after they are written (so a crash before
// persistence retries rather than silently skipping the day); the out-of-app
// ping fires last, best-effort. All copy is result-not-protocol.
func (e *Engine) deliverBrief(convID, runID, localDate, briefText string) {
	text := strings.TrimSpace(briefText)
	if text == "" {
		// The run completed but captured no answer text — treat as not
		// delivered so a later wake retries rather than marking an empty brief.
		fmt.Fprintln(os.Stderr, "neo/brief: completed run produced no brief text; not marking delivered")
		return
	}

	// 0. Deterministic content-policy post-check (task 5.5, req 16/17.1): the
	//    source-link/recommendation budgets and the no-repeat property against
	//    the durable history. A violating brief is NOT delivered and the date
	//    is NOT marked, so the next wake retries with a fresh run.
	recentRecs, recentURLs := e.briefHistory.RecentDelivered(briefNoRepeatWindow)
	if v := briefPolicyViolation(text, recentRecs, recentURLs); v != "" {
		fmt.Fprintf(os.Stderr, "neo/brief: composed brief failed the content policy (%s); not delivering\n", v)
		return
	}

	// 1. Durable conversation turn (the brief lands in the destination thread).
	e.conv.AppendAssistant(convID, runID, text)

	// 2. Durable in-app inbox record (the "your brief is ready" surface).
	e.emitAutomatrixComplete(convID, briefInboxTitle, conciseAutomatrixResult(text))

	// 2b. Durable recommendation history (req 17.1): record the delivered
	//     recommendation identifiers + news source URLs so the composer's
	//     no-repeat set covers this brief from the next run onward.
	if err := e.briefHistory.AppendDelivery(localDate, convID, extractBriefRecs(text), extractBriefURLs(text)); err != nil {
		fmt.Fprintf(os.Stderr, "neo/brief: record history: %v\n", err)
	}

	// 3. Mark the local date delivered so the handler never delivers twice for
	//    one local date (req 14.3). Written only after the brief is durable.
	if e.briefGov != nil {
		if err := e.briefGov.Settings().MarkDelivered(localDate); err != nil {
			fmt.Fprintf(os.Stderr, "neo/brief: mark delivered %s: %v\n", localDate, err)
		}
	}

	// 4. Out-of-app ping LAST: best-effort, non-blocking, honest-failure.
	notify.Deliver(e.automatrixNotifier, notify.Notification{
		Title: briefInboxTitle,
		Body:  conciseAutomatrixResult(text),
	})
}

// briefInboxTitle is the result-not-protocol title for the brief's inbox record
// and out-of-app ping — no Chronos/alarm/marker jargon (the consumer rule).
const briefInboxTitle = "Your morning brief"

// newBriefSession mints an ephemeral session for a morning-brief run on the
// TIGHT read-only brief surface (agent.NewMorningBrief via brief=true), bound to
// convID so it resumes that thread's durable history. It is intentionally NOT
// registered in the session registry: a brief is a side task, not the
// conversation's live turn.
func (e *Engine) newBriefSession(convID string) *session {
	s := &session{
		id:     convID,
		engine: e,
		brief:  true,
		gates:  map[string]chan gateAnswer{},
		asks:   map[string]*askWaiter{},
	}
	s.rebuildAgent()
	return s
}

// briefSettingsView returns the live schedule for objective composition, or a
// zero State when no governor is wired.
func (e *Engine) briefSettingsView() briefSettingsState {
	if e.briefGov == nil {
		return briefSettingsState{}
	}
	st := e.briefGov.SettingsView()
	return briefSettingsState{Length: st.Length, Sections: st.Sections}
}

// briefSettingsState is the minimal, personalization-free slice of the schedule
// the objective builder needs (length + sections). The interests/goals profile
// (task 5.1/5.5) is injected by the composition policy in task 5.5.
type briefSettingsState struct {
	Length   string
	Sections []string
}

// buildBriefObjective composes the run's working objective — the instruction the
// restricted brief agent executes. It is result-not-protocol (plain task framing
// with no Chronos/alarm/marker jargon) and encodes the invariants task 5.4 owns
// (autonomous run, user away, ONE self-contained written brief, FRESH in-run
// retrieval, no questions back) plus the task 5.5 layers: the confirmed
// personalization profile and the content policy with the durable no-repeat set.
func buildBriefObjective(st briefSettingsState, profile string, noRepeatRecs, noRepeatURLs []string) string {
	var b strings.Builder
	b.WriteString("Compose the user's personalized morning brief. They are away and did not ask for it this turn — this is scheduled proactive work you deliver for them to read later.\n\n")
	b.WriteString("Gather fresh, source-backed items across their stated interests using ONLY read-only information tools (recent-news search, web search, read-only fetch) and your memory of them. Every current-events claim MUST come from a retrieval you run in THIS session — never from your own memory — and carry its source. Keep it short and skimmable.")
	if profile = strings.TrimSpace(profile); profile != "" {
		b.WriteString("\n\nTheir confirmed personalization profile (the ONLY personal context to build on):\n")
		b.WriteString(profile)
	}
	b.WriteString("\n\n")
	b.WriteString(briefPolicyRules(noRepeatRecs, noRepeatURLs))
	if secs := briefSectionList(st.Sections); secs != "" {
		b.WriteString("\nSections the user wants included: ")
		b.WriteString(secs)
		b.WriteString(".")
	}
	if length := strings.TrimSpace(st.Length); length != "" {
		b.WriteString("\n\nLength preference: ")
		b.WriteString(length)
		b.WriteString(".")
	}
	b.WriteString("\n\nYou cannot spend money, sign anything, touch the filesystem, deploy, or run code on this surface, and none of that is needed. Deliver the finished brief as one self-contained written answer: no screen surfaces, no questions back, no trailing offers.")
	return b.String()
}

// briefProfileBlock renders the confirmed personalization profile for the
// objective ("" when none is saved — the brief still runs, just unprofiled).
func (e *Engine) briefProfileBlock() string {
	if e.pager == nil {
		return ""
	}
	prof, _, ok, err := e.pager.PersonalizationProfile(context.Background())
	if err != nil || !ok {
		return ""
	}
	return prof.RenderForInterview()
}

// briefSectionList renders the selected sections as a readable comma list.
func briefSectionList(sections []string) string {
	out := make([]string, 0, len(sections))
	for _, s := range sections {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, s)
		}
	}
	return strings.Join(out, ", ")
}

// ResumeInProgressBriefs re-dispatches every brief left running in the durable
// ledger — briefs that were mid-compose when the daemon was restarted or
// suspended (the Task Durability Rule, req 15.2). Each resumes on the RESTRICTED
// brief surface through the same dispatch path (RunningKind returns ONLY
// KindBrief tasks, so a restart can never resume a brief on the money surface,
// and the normal reaper's Running() already excludes them). Called once at boot,
// after the normal task reaper. Best-effort; returns the count resumed.
func (e *Engine) ResumeInProgressBriefs() int {
	if e.tasks == nil || !e.tasks.Enabled() {
		return 0
	}
	briefs := e.tasks.RunningKind(task.KindBrief)
	for _, t := range briefs {
		// The single-delivery-per-date key was encoded into the run id at
		// dispatch, so it is recoverable from the ledger record alone.
		localDate := strings.TrimPrefix(t.RunID, "brief-")
		e.dispatchBriefRun(t.ConvID, localDate)
	}
	return len(briefs)
}
