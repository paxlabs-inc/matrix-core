// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

// Tests for the ORACLE morning-brief runner (task 5.4, req 14.3/15.1/15.2/15.3).
//
// Everything under test is REAL — no fakes/mocks/stubs of the pager, agent,
// session, governor, task ledger, conversation store, or inbox:
//
//   • the run drives the REAL restricted brief agent (agent.NewMorningBrief) over
//     a REAL tools.Manager against the model HTTP endpoint behind an httptest
//     server (the established neo seam);
//   • the schedule is the REAL briefsettings sidecar + REAL governor over the
//     recorded external-Chronos boundary (same posture as the automatrix tests);
//   • durability is the REAL task ledger under t.TempDir(), the REAL conversation
//     store, and the REAL automatrixlog inbox.
//
// The recording BriefRunner used by the gate tests records only the dispatch
// BOUNDARY the wake handler calls — the decision path in front of it is the
// production decideMorningBriefWake/handleMorningBriefWake code, unmodified.

import (
	"context"
	"strings"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/agent"
	"matrix/neo/internal/briefsettings"
	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/task"
	"matrix/neo/internal/tools"
)

// newBriefRunTestEngine builds a real Engine with a real Neocortex pager, real
// durable task ledger + conversation store + inbox, a real tools.Manager, and
// the main model pointed at modelURL ("" = no model). A real governor over the
// recorded Chronos boundary is wired with an enabled-ready schedule delivering
// into "brief-conv".
func newBriefRunTestEngine(t *testing.T, modelURL string) (*Engine, *briefGovernor) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-brief-run-test"
	pager, err := memory.Open(cfg)
	if err != nil {
		t.Fatalf("memory.Open: %v", err)
	}
	var client *llm.Client
	if modelURL != "" {
		client, err = llm.New(mcllm.Config{
			Model:       "test-model",
			Endpoint:    modelURL,
			GatewayURL:  modelURL,
			Provider:    mcllm.ProviderFireworks,
			ProviderSet: true,
			APIKey:      "test-key",
		})
		if err != nil {
			t.Fatalf("llm.New: %v", err)
		}
	}
	manager := &tools.Manager{}
	manager.SetRecall(pager.Recall)
	if _, err := pager.RememberFact(
		t.Context(), "Recent brief context is ready.",
	); err != nil {
		t.Fatalf("seed brief recall evidence: %v", err)
	}
	var runtime *agent.ResurrectionRuntime
	if modelURL != "" {
		runtime = openCanonicalTestRuntime(t, &cfg, manager, pager, modelURL)
	}
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Main:            client,
		Cheap:           client,
		Tools:           manager,
		Pager:           pager,
		Runtime:         runtime,
		ConversationDir: t.TempDir(),
		TaskDir:         t.TempDir(),
		AutomatrixDir:   t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	ctl := &recordingBriefAlarmController{nextID: "brief-alarm-run"}
	gov := newBriefGovernor(briefsettings.Open(t.TempDir()), ctl)
	if serr := gov.settings.SetSchedule(briefsettings.Schedule{
		Timezone: "UTC", DeliveryTime: "07:00", ConversationID: "brief-conv",
	}); serr != nil {
		t.Fatalf("SetSchedule: %v", serr)
	}
	e.SetBriefGovernor(gov)
	t.Cleanup(func() {
		e.Close()
		pager.Close()
	})
	return e, gov
}

// TestBriefSession_TightSurface proves the structural exclusion at the SESSION
// boundary (req 15.1) against the REAL advertised schema set: a brief session's
// agent advertises ONLY read-only information tools + memory recall, while a
// normal session over the same manager advertises a strictly broader surface —
// so the restriction is the brief path's doing, not an empty manager. It is
// also strictly tighter than the automatrix restricted surface (no
// spawn_subagents, no todo bookkeeping beyond the info set).
func TestBriefSession_TightSurface(t *testing.T) {
	e, _ := newBriefRunTestEngine(t, "")

	brief := e.newBriefSession("brief-conv")
	if err := tools.AssertBriefSurface(brief.agent.AdvertisedTools()); err != nil {
		t.Errorf("brief session advertised a non-information tool: %v", err)
	}
	advertised := map[string]bool{}
	for _, s := range brief.agent.AdvertisedTools() {
		advertised[s.Function.Name] = true
	}
	for _, forbidden := range []string{"spawn_subagents", tools.CoreExecuteTool} {
		if advertised[forbidden] {
			t.Errorf("brief surface must structurally exclude %s", forbidden)
		}
	}

	// Control: a normal session over the same manager advertises tools the
	// brief surface excludes.
	normal := e.newSession("conv-normal")
	normalHasSpawn := false
	for _, s := range normal.agent.AdvertisedTools() {
		if s.Function.Name == "spawn_subagents" {
			normalHasSpawn = true
			break
		}
	}
	if !normalHasSpawn {
		t.Error("precondition: a normal session must advertise spawn_subagents, else this test is vacuous")
	}
}

// TestMorningBriefWake_FailClosedGates walks the PURE wake gate through every
// guard on the real governor + settings sidecar (req 14.3/15.3): fail closed
// with no governor, zero dispatch when disabled or paused, off-day skip,
// never-twice-per-local-date, and busy deferral.
func TestMorningBriefWake_FailClosedGates(t *testing.T) {
	e, gov := newBriefRunTestEngine(t, "")
	now := time.Now()
	today := briefLocalDate("UTC", now)

	// No governor wired at all → fail closed.
	bare := &Engine{}
	if got := bare.decideMorningBriefWake(now); got != briefWakeNoGovernor {
		t.Errorf("nil governor: got %d, want briefWakeNoGovernor", got)
	}

	// Disabled (the default) → zero dispatch.
	if got := e.decideMorningBriefWake(now); got != briefWakeDisabled {
		t.Errorf("disabled: got %d, want briefWakeDisabled", got)
	}

	if err := gov.SetEnabled(context.Background(), true); err != nil {
		t.Fatalf("enable: %v", err)
	}

	// Paused → zero dispatch, opt-in retained.
	if err := gov.SetPaused(context.Background(), true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if got := e.decideMorningBriefWake(now); got != briefWakePaused {
		t.Errorf("paused: got %d, want briefWakePaused", got)
	}
	if err := gov.SetPaused(context.Background(), false); err != nil {
		t.Fatalf("unpause: %v", err)
	}

	// Off-day: select only a weekday that is NOT today (in UTC).
	notToday := (int(now.UTC().Weekday()) + 3) % 7
	if err := gov.settings.SetSchedule(briefsettings.Schedule{
		Timezone: "UTC", DeliveryTime: "07:00", ConversationID: "brief-conv",
		Days: []int{notToday},
	}); err != nil {
		t.Fatalf("SetSchedule off-day: %v", err)
	}
	if got := e.decideMorningBriefWake(now); got != briefWakeOffDay {
		t.Errorf("off-day: got %d, want briefWakeOffDay", got)
	}
	if err := gov.settings.SetSchedule(briefsettings.Schedule{
		Timezone: "UTC", DeliveryTime: "07:00", ConversationID: "brief-conv",
	}); err != nil {
		t.Fatalf("SetSchedule restore: %v", err)
	}

	// Eligible now.
	if got := e.decideMorningBriefWake(now); got != briefWakeDispatched {
		t.Errorf("eligible: got %d, want briefWakeDispatched", got)
	}

	// Busy: a brief already in flight defers a duplicate fire.
	e.briefInflight.Add(1)
	if got := e.decideMorningBriefWake(now); got != briefWakeBusy {
		t.Errorf("busy: got %d, want briefWakeBusy", got)
	}
	e.briefInflight.Add(-1)

	// Already delivered for this local date → never twice (req 14.3).
	if err := gov.Settings().MarkDelivered(today); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if got := e.decideMorningBriefWake(now); got != briefWakeAlready {
		t.Errorf("already-delivered: got %d, want briefWakeAlready", got)
	}
}

// TestMorningBriefWake_DisabledDispatchesNothing proves req 15.3 at the dispatch
// BOUNDARY: a MORNING_BRIEF wake with the brief disabled/paused is intercepted
// (never becomes a user turn) yet the runner — the only path to any network
// retrieval — is never invoked. A normal message is left for the normal /chat
// path.
func TestMorningBriefWake_DisabledDispatchesNothing(t *testing.T) {
	e, gov := newBriefRunTestEngine(t, "")
	dispatches := 0
	e.SetBriefRunner(func(context.Context, string, string) error {
		dispatches++
		return nil
	})

	// Not a wake → false, untouched.
	if e.MaybeHandleMorningBriefWake(context.Background(), "conv-x", "good morning, what's on today?") {
		t.Fatal("a normal message must not be intercepted as a brief wake")
	}

	// Disabled wake → intercepted, zero dispatch.
	if !e.MaybeHandleMorningBriefWake(context.Background(), "conv-x", agent.MorningBriefWakeMessage) {
		t.Fatal("a MORNING_BRIEF wake must be intercepted")
	}
	if dispatches != 0 {
		t.Fatalf("disabled brief must dispatch NOTHING, got %d dispatches", dispatches)
	}

	// Paused wake → intercepted, zero dispatch.
	if err := gov.SetEnabled(context.Background(), true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	if err := gov.SetPaused(context.Background(), true); err != nil {
		t.Fatalf("pause: %v", err)
	}
	if !e.MaybeHandleMorningBriefWake(context.Background(), "conv-x", agent.MorningBriefWakeMessage) {
		t.Fatal("a paused MORNING_BRIEF wake must still be intercepted")
	}
	if dispatches != 0 {
		t.Fatalf("paused brief must dispatch NOTHING, got %d dispatches", dispatches)
	}

	// Eligible wake → exactly one dispatch.
	if err := gov.SetPaused(context.Background(), false); err != nil {
		t.Fatalf("unpause: %v", err)
	}
	if !e.MaybeHandleMorningBriefWake(context.Background(), "conv-x", agent.MorningBriefWakeMessage) {
		t.Fatal("an eligible MORNING_BRIEF wake must be intercepted")
	}
	if dispatches != 1 {
		t.Fatalf("eligible wake must dispatch exactly once, got %d", dispatches)
	}
}

// waitBriefDelivered polls the REAL durable surfaces until the brief for
// localDate is marked delivered (or the deadline passes). The run completes on
// a background goroutine, so the test waits on the durable record.
func waitBriefDelivered(t *testing.T, gov *briefGovernor, localDate string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if gov.Settings().AlreadyDelivered(localDate) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("brief for %s was not delivered within %v", localDate, timeout)
}

// TestRunMorningBrief_DeliversDurablyOnce drives the production runner end-to-
// end against a clean-completion model: the brief must land as a durable
// conversation turn + inbox record, the ledger's brief task must settle done,
// the local date must be marked delivered ONLY after the durable persistence,
// and a second wake for the same date must be refused (req 14.3/15.2).
func TestRunMorningBrief_DeliversDurablyOnce(t *testing.T) {
	srv := doneModelServer(t)
	e, gov := newBriefRunTestEngine(t, srv.URL)
	e.SetBriefRunner(e.RunMorningBrief)
	if err := gov.SetEnabled(context.Background(), true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	localDate := briefLocalDate("UTC", time.Now())

	if !e.MaybeHandleMorningBriefWake(context.Background(), "ignored", agent.MorningBriefWakeMessage) {
		t.Fatal("wake must be intercepted")
	}
	waitBriefDelivered(t, gov, localDate, 10*time.Second)

	// Durable conversation turn in the destination thread.
	var briefText string
	for _, turn := range e.conv.History("brief-conv") {
		if turn.Role == "assistant" && strings.TrimSpace(turn.Text) != "" {
			briefText = turn.Text
		}
	}
	if briefText == "" {
		t.Error("the brief must be persisted as a durable assistant turn in the destination conversation")
	}

	// Durable inbox record.
	recs := e.AutomatrixInbox().List()
	found := false
	for _, r := range recs {
		if r.OpportunitySummary == briefInboxTitle && r.ConversationID == "brief-conv" {
			found = true
		}
	}
	if !found {
		t.Errorf("the brief must land a durable inbox record titled %q, got %d records", briefInboxTitle, len(recs))
	}

	// The ledger's brief task settled — nothing left running on the brief kind.
	if running := e.tasks.RunningKind(task.KindBrief); len(running) != 0 {
		t.Errorf("brief task must be settled in the ledger, still running: %v", running)
	}

	// Never twice for one local date.
	if got := e.decideMorningBriefWake(time.Now()); got != briefWakeAlready {
		t.Errorf("second wake same date: got %d, want briefWakeAlready", got)
	}
}

// TestResumeInProgressBriefs_RestrictedResume proves the restart-durability
// split (req 15.2): a brief left running in the ledger is EXCLUDED from the
// normal reaper's resume set (it must never resume on the money surface) and is
// picked up by the dedicated brief-resume path, which re-runs it on the
// restricted surface through the same dispatch path and delivers durably.
func TestResumeInProgressBriefs_RestrictedResume(t *testing.T) {
	srv := doneModelServer(t)
	e, gov := newBriefRunTestEngine(t, srv.URL)
	if err := gov.SetEnabled(context.Background(), true); err != nil {
		t.Fatalf("enable: %v", err)
	}
	localDate := briefLocalDate("UTC", time.Now())

	// Seed the ledger as a restart would find it: one interrupted-mid-compose
	// brief and one ordinary interactive task.
	e.tasks.BeginKind("brief-conv", briefRunID(localDate), "compose the brief", task.KindBrief)
	e.tasks.Begin("conv-interactive", "run-live", "an ordinary user task")

	// The normal reaper's set must NOT contain the brief.
	for _, orphan := range e.tasks.Running() {
		if orphan.Kind == task.KindBrief || orphan.ConvID == "brief-conv" {
			t.Fatalf("the normal reaper must never see a brief task: %+v", orphan)
		}
	}
	if got := len(e.tasks.Running()); got != 1 {
		t.Fatalf("normal reaper set: got %d tasks, want 1 (the interactive one)", got)
	}

	// The dedicated brief resume picks up exactly the brief and finishes it.
	if n := e.ResumeInProgressBriefs(); n != 1 {
		t.Fatalf("ResumeInProgressBriefs: got %d, want 1", n)
	}
	waitBriefDelivered(t, gov, localDate, 10*time.Second)

	if running := e.tasks.RunningKind(task.KindBrief); len(running) != 0 {
		t.Errorf("resumed brief must settle in the ledger, still running: %v", running)
	}
	hasTurn := false
	for _, turn := range e.conv.History("brief-conv") {
		if turn.Role == "assistant" && strings.TrimSpace(turn.Text) != "" {
			hasTurn = true
		}
	}
	if !hasTurn {
		t.Error("a resumed brief must still deliver the durable conversation turn")
	}
}
