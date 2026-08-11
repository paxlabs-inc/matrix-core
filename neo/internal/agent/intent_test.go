// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"matrix/neo/internal/config"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/tools"
)

func TestSessionCurrentIntentTracksRealTurnsAndLifecycles(t *testing.T) {
	var (
		bodyMu sync.Mutex
		bodies []string
	)
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		bodyMu.Lock()
		bodies = append(bodies, string(raw))
		bodyMu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"Understood."},"finish_reason":"stop"}]}`+"\n")
		fmt.Fprint(w, "data: [DONE]\n")
	}))
	t.Cleanup(model.Close)

	cfg := config.Default()
	cfg.SessionCurrentIntent = true
	cfg.CassandraEnabled = false
	cfg.EpistemicPremises = false
	cfg.EpistemicPredictions = false
	a := New(Options{Config: cfg, Main: windowLawClient(t, model.URL), Tools: &tools.Manager{}, ConvID: "conv-current-intent"})

	if err := a.Chat(context.Background(), "prepare the quarterly report"); err != nil {
		t.Fatal(err)
	}
	if got := a.TurnObjective(); got != "prepare the quarterly report" {
		t.Fatalf("first turn objective = %q", got)
	}
	if err := a.Chat(context.Background(), "what film should I watch tonight?"); err != nil {
		t.Fatal(err)
	}
	if got := a.TurnObjective(); got != "what film should I watch tonight?" {
		t.Fatalf("latest turn objective = %q", got)
	}
	if a.activeGoal != a.TurnObjective() {
		t.Fatalf("transitional activeGoal = %q, turn objective = %q", a.activeGoal, a.TurnObjective())
	}
	bodyMu.Lock()
	secondBody := bodies[len(bodies)-1]
	bodyMu.Unlock()
	secondWindow := decodeWireMessages(t, []byte(secondBody))
	secondTail := secondWindow[len(secondWindow)-1].Content
	if !strings.Contains(secondTail, "Current user objective (authoritative; newer than any historical task): what film should I watch tonight?") || strings.Contains(secondTail, "Standing objective for this conversation") {
		t.Fatalf("second-turn intent projection resurrected the first task:\n%s", secondTail)
	}

	if err := a.Chat(context.Background(), "Use PostgreSQL for the local cache."); err != nil {
		t.Fatal(err)
	}
	if err := a.Chat(context.Background(), "Correction: use SQLite for the local cache, not PostgreSQL."); err != nil {
		t.Fatal(err)
	}
	if got := a.TurnObjective(); got != "Correction: use SQLite for the local cache, not PostgreSQL." {
		t.Fatalf("correction did not become authoritative: %q", got)
	}
	for index := 0; index < 6; index++ {
		if err := a.Chat(context.Background(), fmt.Sprintf("Historical task %d is complete.", index)); err != nil {
			t.Fatal(err)
		}
	}
	a.cmTrimWorking()
	if err := a.Chat(context.Background(), "Tell me a short joke about databases."); err != nil {
		t.Fatal(err)
	}
	bodyMu.Lock()
	postTrimBody := bodies[len(bodies)-1]
	bodyMu.Unlock()
	postTrimWindow := decodeWireMessages(t, []byte(postTrimBody))
	postTrimTail := postTrimWindow[len(postTrimWindow)-1].Content
	if !strings.Contains(postTrimTail, "Current user objective (authoritative; newer than any historical task): Tell me a short joke about databases.") {
		t.Fatalf("post-trim current intent missing:\n%s", postTrimTail)
	}
	graph := a.ensureGraph()
	if graph.Goal != "Tell me a short joke about databases." || a.goalWantsAction() {
		t.Fatalf("current-intent consumers disagree: graph=%q action=%v", graph.Goal, a.goalWantsAction())
	}
	a.recordDeath(DeathReasonStepBudget, "test capture")
	if death, ok := a.LastDeath(); !ok || death.Objective != "Tell me a short joke about databases." {
		t.Fatalf("death objective = %+v, %v", death, ok)
	}

	goalID := a.RegisterPersistentGoal("monitor the release until it settles")
	goal, ok := a.CurrentPersistentGoal()
	if !ok || goal.ID != goalID || goal.Objective != "monitor the release until it settles" {
		t.Fatalf("persistent goal = %+v, %v", goal, ok)
	}
	newGoalID, ok := a.SupersedePersistentGoal(goalID, "monitor only the canary")
	if !ok || newGoalID == goalID {
		t.Fatalf("supersede persistent goal = %q, %v", newGoalID, ok)
	}
	if goal, ok = a.CurrentPersistentGoal(); !ok || goal.Objective != "monitor only the canary" {
		t.Fatalf("superseded persistent goal = %+v, %v", goal, ok)
	}
	if !a.AbandonPersistentGoal(newGoalID) {
		t.Fatal("abandon persistent goal failed")
	}
	if _, ok := a.CurrentPersistentGoal(); ok {
		t.Fatal("abandoned persistent goal remained live")
	}
	completedGoalID := a.RegisterPersistentGoal("finish the migration")
	if !a.CompletePersistentGoal(completedGoalID) {
		t.Fatal("complete persistent goal failed")
	}

	firstLoop := a.RegisterOpenLoop("confirm the launch window")
	secondLoop := a.RegisterOpenLoop("collect the final receipt")
	if loops := a.OpenLoops(); len(loops) != 2 || loops[0].ID != firstLoop || loops[1].ID != secondLoop {
		t.Fatalf("open loops = %+v", loops)
	}
	replacementLoop, ok := a.SupersedeOpenLoop(firstLoop, "confirm the canary window")
	if !ok || replacementLoop == firstLoop {
		t.Fatalf("supersede open loop = %q, %v", replacementLoop, ok)
	}
	if !a.CompleteOpenLoop(secondLoop) || !a.AbandonOpenLoop(replacementLoop) || len(a.OpenLoops()) != 0 {
		t.Fatalf("open loop lifecycle did not clear: %+v", a.OpenLoops())
	}

	a.Reset()
	if a.TurnObjective() != "" {
		t.Fatalf("reset turn objective = %q", a.TurnObjective())
	}
	if _, ok := a.CurrentPersistentGoal(); ok || len(a.OpenLoops()) != 0 {
		t.Fatal("reset retained session intent state")
	}
}

func TestSessionCurrentIntentOffKeepsLegacySeedGoal(t *testing.T) {
	cfg := config.Default()
	if cfg.SessionCurrentIntent {
		t.Fatal("flag default changed")
	}
	a := New(Options{Config: cfg})
	a.Seed([]llm.Message{llm.UserMessage("first task"), llm.AssistantMessage("working")}, "first task")
	if a.activeGoal != "first task" || a.TurnObjective() != "first task" {
		t.Fatalf("legacy seed goal drifted: active=%q turn=%q", a.activeGoal, a.TurnObjective())
	}
	if id := a.RegisterPersistentGoal("should be inert"); id != "" {
		t.Fatalf("flag-off persistent goal registered: %q", id)
	}
}
