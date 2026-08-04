// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"matrix/neo/internal/conversation"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/notify"
	"matrix/neo/internal/task"
)

// Autonomous execution of one Automatrix opportunity (task 4.1, req 5.1–5.5).
//
// This is the AutomatrixRunner the wake handler (task 3.3) hands a picked
// opportunity to. The wake handler owns the pick + the per-day-cap/reschedule
// bookkeeping and advances the per-day counter only on a clean (nil-error)
// runner return — i.e. only when a proactive task was genuinely STARTED. So the
// runner's job is the in-flight hand-off + dispatch:
//
//  1. mark the opportunity scheduled -> in_progress (the durable, atomic
//     in-flight marker — a restart never double-runs or loses it, req 2.2/5.1);
//  2. resume into the opportunity's origin_conversation_id so Neo regains the
//     thread transcript + Neocortex memory (context fidelity, req 5.2), building
//     the working objective from the summary + rationale;
//  3. dispatch a SUPERVISED run on the RESTRICTED (no-money) tool surface
//     (req 3.2/5.3), DECOUPLED from the wake request (context.Background +
//     the normal task wall-clock) and durable across restart (the Task
//     Durability Rule — see ResumeInProgressAutomatrix);
//  4. hold the run to the SAME completion gate as any state-touching turn
//     (inside agent.Chat) and, on the terminal, settle the opportunity:
//     a genuine pass -> done; a partial/fail -> back to pending with attempts++
//     (bounded; dismissed once the ceiling is hit), surfacing NOTHING (req 5.5).
//
// The dispatch is launched on a background goroutine so the runner returns
// promptly (the per-day counter advances on a clean START, not on completion),
// and the run's lifecycle is independent of the wake HTTP request that
// triggered it.

// automatrixMaxAttempts bounds how many times an autonomous opportunity is
// retried across wakes before it is DISMISSED rather than re-queued (req 5.5:
// "eventually dismiss it"). A partial/failed attempt re-pends with attempts++;
// once attempts reach this ceiling the opportunity is dropped from the queue so
// a persistently-unachievable item cannot churn forever.
const automatrixMaxAttempts = 3

// RunAutomatrixOpportunity is the AutomatrixRunner (wired via
// SetAutomatrixRunner). It performs the in-flight status hand-off synchronously
// (so a failure to even mark the opportunity in_progress is reported back to
// the wake handler, which then does NOT advance the per-day counter) and then
// dispatches the supervised restricted-surface run on a background goroutine.
// A nil return means the proactive task was cleanly started.
func (e *Engine) RunAutomatrixOpportunity(ctx context.Context, opp memory.OpportunitySpec) error {
	if e.pager == nil {
		return fmt.Errorf("neo/automatrix: no pager wired; cannot run opportunity")
	}
	uri := strings.TrimSpace(opp.URI)
	if uri == "" {
		// Without a durable URI the lifecycle can't be tracked (we could neither
		// mark done nor re-pend on failure) — refuse rather than run blind.
		return fmt.Errorf("neo/automatrix: opportunity has no URI; cannot track its lifecycle")
	}

	// In-flight hand-off: scheduled -> in_progress, each persisted atomically.
	// If the transition can't be recorded, roll back to pending (best-effort)
	// and report the failure so the per-day counter does not advance and a
	// later wake retries.
	if err := e.pager.SetOpportunityStatus(ctx, uri, memory.OpportunityScheduled); err != nil {
		return fmt.Errorf("neo/automatrix: mark scheduled: %w", err)
	}
	if err := e.pager.SetOpportunityStatus(ctx, uri, memory.OpportunityInProgress); err != nil {
		_ = e.pager.SetOpportunityStatus(ctx, uri, memory.OpportunityPending)
		return fmt.Errorf("neo/automatrix: mark in_progress: %w", err)
	}

	e.dispatchAutomatrixRun(opp)
	return nil
}

// dispatchAutomatrixRun drives the supervised restricted-surface run for an
// already-in_progress opportunity on a background goroutine, decoupled from any
// request context, and settles the opportunity on the terminal. Shared by the
// live runner path and the restart-resume sweep.
func (e *Engine) dispatchAutomatrixRun(opp memory.OpportunitySpec) {
	convID := strings.TrimSpace(opp.OriginConversationID)
	if convID == "" {
		// No origin thread (e.g. a captured opportunity without a conversation):
		// run in a fresh synthetic conversation rather than refuse the work.
		convID = synthConvID(opp.Summary)
	}
	objective := buildAutomatrixObjective(opp)

	e.automatrixInflight.Add(1)
	e.automatrixWG.Add(1)
	go func() {
		defer e.automatrixWG.Done()
		defer e.automatrixInflight.Add(-1)
		// Decoupled from the wake request (req 5.1): a fresh wall-clock-bounded
		// context on context.Background, the SAME ceiling a normal supervised
		// task gets. Closing the app / dropping the wake never ends it.
		wall := e.cfg.TaskMaxWall
		if wall <= 0 {
			wall = 20 * time.Minute
		}
		rctx, cancel := context.WithTimeout(context.Background(), wall)
		defer cancel()

		// A dedicated, UNREGISTERED session bound to the origin conversation: it
		// seeds from that thread's durable history (context fidelity, req 5.2)
		// on the restricted surface, but is NOT in the session registry, so it
		// neither collides with the user's live session for that conversation
		// nor leaks proactive terminals onto it.
		sess := e.newAutomatrixSession(convID)
		status := sess.superviseAutomatrixTask(rctx, objective)
		e.settleAutomatrixOpportunity(context.Background(), opp, convID, status, sess.automatrixResult())
	}()
}

// settleAutomatrixOpportunity records the run's terminal outcome on the
// opportunity (req 5.5). A genuine completion-gate pass marks it done AND
// announces the result out of band (task 5.3); any other terminal (an honest
// partial at the ceiling, a deterministic blocker) leaves it re-pended with
// attempts++ (bounded; dismissed at the ceiling) for a later wake to retry —
// and surfaces NOTHING to the user (req 5.5/8.3: a non-completed task is never
// announced). convID is the conversation the run executed in (origin thread, or
// a synthetic one); resultSummary is the agent's captured final answer.
func (e *Engine) settleAutomatrixOpportunity(ctx context.Context, opp memory.OpportunitySpec, convID string, status task.Status, resultSummary string) {
	uri := strings.TrimSpace(opp.URI)
	if uri == "" || e.pager == nil {
		return
	}
	if status == task.StatusDone {
		if err := e.pager.SetOpportunityStatus(ctx, uri, memory.OpportunityDone); err != nil {
			fmt.Fprintf(os.Stderr, "neo/automatrix: mark done %s: %v\n", uri, err)
		}
		// ONLY a genuine, gate-passed completion announces (req 8.3: never a
		// fabricated or non-completed surprise). The durable record is written
		// first (canonical, never lost) then the ping fires best-effort.
		e.announceAutomatrixCompletion(convID, opp.Summary, resultSummary)
		return
	}
	if _, err := e.pager.FailOpportunityAttempt(ctx, uri, automatrixMaxAttempts); err != nil {
		fmt.Fprintf(os.Stderr, "neo/automatrix: record failed attempt %s: %v\n", uri, err)
	}
}

// announceAutomatrixCompletion is the completion -> durable record + notify seam
// (task 5.3, req 6.1/6.4/6.5/8.3). It runs ONLY on a genuine, gate-passed
// autonomous completion. The durable in-app record is written FIRST so the
// result is the canonical, replayable surface even if no client is connected and
// regardless of any external-send outcome (req 6.1); the out-of-app ntfy/Apprise
// ping is then fired BEST-EFFORT + non-blocking via notify.Deliver (req 6.4), so
// a failed external send neither blocks the run nor disturbs the durable record
// (honest failure — a ping we couldn't send is never reported as delivered). All
// copy is RESULT-not-protocol — no Chronos/alarm/marker/Neocortex jargon (req 6.5,
// the consumer rule): the title is the helpful thing Neo took on and the body is
// the result it produced (both plain English, sourced from the opportunity
// summary + the agent's own final answer). No secrets are recorded or sent — the
// record shape is whitelist-only and the body is the model's user-facing answer.
func (e *Engine) announceAutomatrixCompletion(convID, opportunitySummary, resultSummary string) {
	result := conciseAutomatrixResult(resultSummary)
	if result == "" {
		// No produced answer text to show — fall back to the opportunity itself
		// so the record/ping still reads as a finished, helpful thing rather
		// than a blank (never a protocol token).
		result = strings.TrimSpace(opportunitySummary)
	}

	// Durable in-app record FIRST (canonical surface; survives no-client and a
	// failed external send).
	e.emitAutomatrixComplete(convID, opportunitySummary, result)

	// Out-of-app ping: best-effort, non-blocking, honest-failure (notify.Deliver
	// fires on a background goroutine and never returns an error). A nil/noop
	// notifier is a safe no-op — the durable record above is enough.
	title := strings.TrimSpace(opportunitySummary)
	if title == "" {
		title = "Neo finished something for you"
	}
	notify.Deliver(e.automatrixNotifier, notify.Notification{
		Title: title,
		Body:  result,
	})
}

// lastAssistantText returns the most recent persisted assistant turn of a
// conversation — the final answer an approve-path run produced (its quiet
// sibling, the autonomous runner, captures the answer via automatrixReporter
// instead). Empty when the durable store is disabled or the thread has no
// assistant turn yet.
func (e *Engine) lastAssistantText(convID string) string {
	if !e.conv.Enabled() {
		return ""
	}
	turns := e.conv.Recent(convID, conversation.DefaultRecallTurns)
	for i := len(turns) - 1; i >= 0; i-- {
		if turns[i].Role == "assistant" {
			return turns[i].Text
		}
	}
	return ""
}

// conciseAutomatrixResult condenses the run's produced answer into a short,
// result-not-protocol summary for the completion record + ping: it takes the
// first paragraph, collapses whitespace, and caps the length. No protocol jargon
// is introduced — the source is the agent's own user-facing answer.
func conciseAutomatrixResult(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	// A multi-paragraph recap collapses to its lede (the headline result).
	if i := strings.Index(s, "\n\n"); i > 0 {
		s = strings.TrimSpace(s[:i])
	}
	s = strings.Join(strings.Fields(s), " ")
	const maxLen = 280
	if len(s) > maxLen {
		s = strings.TrimSpace(s[:maxLen]) + "…"
	}
	return s
}

// newAutomatrixSession mints an ephemeral session for an autonomous run on the
// RESTRICTED (no-money) tool surface, bound to convID so it resumes that
// thread's durable history (req 5.2). It is intentionally NOT registered in the
// session registry: a proactive run is a side task, not the conversation's live
// turn.
func (e *Engine) newAutomatrixSession(convID string) *session {
	s := &session{
		id:         convID,
		engine:     e,
		automatrix: true,
		gates:      map[string]chan gateAnswer{},
		asks:       map[string]*askWaiter{},
	}
	s.rebuildAgent()
	return s
}

// buildAutomatrixObjective composes the run's working objective from the
// opportunity's summary + rationale (req 5.2). It is result-not-protocol: plain
// task framing with no Chronos/alarm/marker jargon, so the run (and anything it
// produces) reads as ordinary helpful work.
func buildAutomatrixObjective(opp memory.OpportunitySpec) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(opp.Summary))
	if r := strings.TrimSpace(opp.Rationale); r != "" {
		b.WriteString("\n\nWhy this helps: ")
		b.WriteString(r)
	}
	b.WriteString("\n\nThis is proactive work you took on for the user during their downtime — they did not ask for it this turn and they are not present. Complete it end-to-end to a high standard using the relevant context from this conversation, and finish only when it genuinely meets the bar. Deliver the result as one self-contained written answer they will read later: no screen surfaces, no questions back, no trailing offers.")
	return b.String()
}

// ResumeInProgressAutomatrix re-dispatches every opportunity left IN_PROGRESS in
// Neocortex — proactive runs that were in flight when the daemon was restarted or
// suspended (the Task Durability Rule, req 5.1). Each resumes on the RESTRICTED
// surface through the same dispatch path (InProgressOpportunities only returns
// eligible-autonomous items, so a restart can never resume work on the money
// surface). Called once at boot, after the normal task reaper. Best-effort;
// returns the count resumed.
func (e *Engine) ResumeInProgressAutomatrix() int {
	if e.pager == nil {
		return 0
	}
	opps, err := e.pager.InProgressOpportunities(context.Background(), 0)
	if err != nil {
		fmt.Fprintf(os.Stderr, "neo/automatrix: resume in-progress: %v\n", err)
		return 0
	}
	for _, opp := range opps {
		e.dispatchAutomatrixRun(opp)
	}
	return len(opps)
}
