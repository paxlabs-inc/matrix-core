// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	mcllm "matrix/mcl/llm"
	"matrix/neo/internal/capabilityhub"
	"matrix/neo/internal/config"
	"matrix/neo/internal/improvement"
	"matrix/neo/internal/llm"
	"matrix/neo/internal/memory"
	"matrix/neo/internal/task"
)

type recordingImprovementChronos struct {
	mu          sync.Mutex
	calls       int
	lastRetry   int
	observation improvement.Observation
}

func (controller *recordingImprovementChronos) Set(_ context.Context, observation improvement.Observation, retry int) (string, error) {
	controller.mu.Lock()
	defer controller.mu.Unlock()
	controller.calls++
	controller.lastRetry = retry
	controller.observation = observation
	return "chronos-observation-alarm", nil
}

func newImprovementEngine(t *testing.T, modelURL string) (*Engine, *memory.Pager) {
	t.Helper()
	cfg := config.Default()
	cfg.DataRoot = t.TempDir()
	cfg.NeocortexActor = "neo-improvement-test"
	cfg.ActorDID = "did:matrix:improvement-test"
	cfg.ImprovementEnabled = true
	cfg.ImprovementIdleDelayMinutes = 1
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
	e := NewEngine(EngineOptions{
		Config:          cfg,
		Main:            client,
		Cheap:           client,
		SubMain:         client,
		Pager:           pager,
		ConversationDir: t.TempDir(),
		CapabilityDir:   t.TempDir(),
		ImprovementDir:  t.TempDir(),
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(func() {
		e.Close()
		_ = pager.Close()
	})
	if e.improvementErr != nil || e.improvement == nil {
		t.Fatalf("improvement store: %v", e.improvementErr)
	}
	return e, pager
}

func improvementModelServer(t *testing.T, response string, bodies *[][]byte) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		*bodies = append(*bodies, body)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":" + strconvQuote(response) + "},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	t.Cleanup(server.Close)
	return server
}

func strconvQuote(value string) string {
	raw, _ := json.Marshal(value)
	return string(raw)
}

func scheduleProposal(t *testing.T, store *improvement.Store, draft improvement.Draft) improvement.Proposal {
	t.Helper()
	observation, _, err := store.Schedule("conversation-review", "run-review", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Begin(observation.Key); err != nil {
		t.Fatal(err)
	}
	proposals, err := store.Finish(observation.Key, []improvement.Draft{draft})
	if err != nil || len(proposals) != 1 {
		t.Fatalf("finish proposal: count=%d err=%v", len(proposals), err)
	}
	return proposals[0]
}

func proposalEvidence() []improvement.Evidence {
	return []improvement.Evidence{{ConversationID: "conversation-review", RunID: "run-review", Role: "user", Quote: "Use Frankfurt, not Virginia."}}
}

func postImprovementAction(t *testing.T, handler http.Handler, proposalID, action, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/improvement/proposals/"+proposalID+"/"+action, strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func TestImprovementWakeDefersWhileConversationActive(t *testing.T) {
	e, _ := newImprovementEngine(t, "")
	chronos := &recordingImprovementChronos{}
	e.improvementAlarms = chronos
	observation, _, err := e.improvement.Schedule("conversation-active", "run-complete", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.runs["run-active"] = &run{id: "run-active"}
	e.mu.Unlock()
	wake, _ := json.Marshal(improvementWake{Key: observation.Key})
	if !e.MaybeHandleImprovementWake(context.Background(), improvementWakeMarker+" "+string(wake)) {
		t.Fatal("observer wake was not intercepted")
	}
	got, ok := e.improvement.Observation(observation.Key)
	if !ok || got.Status != improvement.ObservationScheduled || got.Attempts != 0 {
		t.Fatalf("active turn must defer observation without starting it: %+v", got)
	}
	if chronos.calls != 1 || chronos.lastRetry != 1 || got.AlarmID != "chronos-observation-alarm" {
		t.Fatalf("Chronos deferral not durably recorded: calls=%d retry=%d observation=%+v", chronos.calls, chronos.lastRetry, got)
	}
}

func TestImprovementCompletionObserverSchedulesOnlyAfterDone(t *testing.T) {
	e, _ := newImprovementEngine(t, "")
	chronos := &recordingImprovementChronos{}
	e.improvementAlarms = chronos
	observe, arm := e.improvementCompletionObserver("conversation-complete")
	observe(task.StatusDone)
	arm("run-complete", true)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		observation, ok := e.improvement.Observation(improvement.ObservationKey("conversation-complete", "run-complete"))
		if ok && observation.AlarmID == "chronos-observation-alarm" {
			if observation.Status != improvement.ObservationScheduled || chronos.calls != 1 || chronos.lastRetry != 0 {
				t.Fatalf("unexpected completed-run schedule: observation=%+v calls=%d retry=%d", observation, chronos.calls, chronos.lastRetry)
			}
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("completed run did not schedule a one-shot observation")
}

func TestImprovementObserverNoopHasNoToolsAndNoDirectWrites(t *testing.T) {
	var bodies [][]byte
	model := improvementModelServer(t, `{"proposals":[]}`, &bodies)
	e, pager := newImprovementEngine(t, model.URL)
	if _, err := pager.RememberFact(context.Background(), "The deployment region is Virginia."); err != nil {
		t.Fatal(err)
	}
	before, _, err := pager.Timeline(memory.TimelineQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	e.conv.AppendUser("conversation-noop", "run-noop", "Summarize this completed task.")
	e.conv.AppendAssistant("conversation-noop", "run-noop", "The task is complete.")
	observation, _, err := e.improvement.Schedule("conversation-noop", "run-noop", time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	e.runImprovementObservation(observation.Key)
	after, _, err := pager.Timeline(memory.TimelineQuery{Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := e.improvement.Observation(observation.Key)
	if got.Status != improvement.ObservationNoop || len(e.improvement.List("")) != 0 || len(after) != len(before) {
		t.Fatalf("no-op observer mutated durable owners: observation=%+v proposals=%d memories=%d->%d", got, len(e.improvement.List("")), len(before), len(after))
	}
	capabilities, err := e.capabilities.List(context.Background(), capabilityhub.Query{})
	if err != nil || len(capabilities) != 0 {
		t.Fatalf("observer wrote Capability Hub directly: count=%d err=%v", len(capabilities), err)
	}
	if len(bodies) != 1 || bytes.Contains(bodies[0], []byte(`"tools"`)) || !bytes.Contains(bodies[0], []byte("read-only quality observer")) {
		t.Fatalf("observer request must have an isolated no-tool charter: %s", string(bytes.Join(bodies, nil)))
	}
}

func TestImprovementMemoryApprovalAndRollbackUseNeocortexOwner(t *testing.T) {
	e, pager := newImprovementEngine(t, "")
	targetURI, err := pager.RememberFact(context.Background(), "Preferred deployment region is Virginia.")
	if err != nil {
		t.Fatal(err)
	}
	proposal := scheduleProposal(t, e.improvement, improvement.Draft{
		Kind: improvement.KindMemory, Summary: "Correct the preferred deployment region", Rationale: "The user explicitly corrected the region.", Confidence: 1,
		Evidence: proposalEvidence(),
		Payload:  improvement.Payload{Memory: &memory.MutationItem{Operation: memory.MutationSupersede, Target: &memory.MutationTarget{URI: targetURI}, Value: &memory.MutationValue{Type: "Fact", Content: "Preferred deployment region is Frankfurt."}, Reason: "explicit correction"}},
	})
	handler := (&Server{engine: e}).Handler()
	denied := postImprovementAction(t, handler, proposal.ID, "approve", `{"verification":"short"}`)
	if denied.Code != http.StatusBadRequest {
		t.Fatalf("unverified approval status=%d body=%s", denied.Code, denied.Body.String())
	}
	approved := postImprovementAction(t, handler, proposal.ID, "approve", `{"verification":"confirmed against the exact quoted user correction"}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("approved proposal status=%d body=%s", approved.Code, approved.Body.String())
	}
	applied, ok := e.improvement.Get(proposal.ID)
	if !ok || applied.Status != improvement.StatusApplied || applied.Version != 3 || applied.AppliedRef == "" || applied.RollbackRef == "" {
		t.Fatalf("proposal was not versioned and applied: %+v", applied)
	}
	current, err := e.memoryValueForURI(applied.AppliedRef)
	if err != nil || current.Content != "Preferred deployment region is Frankfurt." {
		t.Fatalf("Neocortex owner did not apply supersession: value=%+v err=%v", current, err)
	}
	rolled := postImprovementAction(t, handler, proposal.ID, "rollback", `{"reason":"owner requested rollback"}`)
	if rolled.Code != http.StatusOK {
		t.Fatalf("rollback status=%d body=%s", rolled.Code, rolled.Body.String())
	}
	rolledBack, _ := e.improvement.Get(proposal.ID)
	if rolledBack.Status != improvement.StatusRolledBack || rolledBack.Version != 4 {
		t.Fatalf("rollback lifecycle=%+v", rolledBack)
	}
	latest := rolledBack.History[len(rolledBack.History)-1]
	if latest.Detail != "owner requested rollback" {
		t.Fatalf("rollback reason not audited: %+v", latest)
	}
}

func TestImprovementGovernedChangeCreatesOnlyQuarantinedCandidateAndRollsBack(t *testing.T) {
	e, _ := newImprovementEngine(t, "")
	manifest, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "brainstorming", "SKILL.mtx"))
	if err != nil {
		t.Fatal(err)
	}
	prose, err := os.ReadFile(filepath.Join("..", "..", "..", "skills", "brainstorming", "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	proposal := scheduleProposal(t, e.improvement, improvement.Draft{
		Kind: improvement.KindSkill, Summary: "Review a bounded brainstorming capability", Rationale: "A governed change must enter quarantine.", Confidence: 0.9,
		Evidence: proposalEvidence(),
		Payload:  improvement.Payload{Capability: &improvement.CapabilityPayload{Manifest: string(manifest), Prose: string(prose), Provenance: "verified-improvement/test"}},
	})
	handler := (&Server{engine: e}).Handler()
	approved := postImprovementAction(t, handler, proposal.ID, "approve", `{"verification":"reviewed real package provenance and manifest"}`)
	if approved.Code != http.StatusOK {
		t.Fatalf("governed approve status=%d body=%s", approved.Code, approved.Body.String())
	}
	items, err := e.capabilities.List(context.Background(), capabilityhub.Query{})
	if err != nil || len(items) != 1 || items[0].State != capabilityhub.StateQuarantine || items[0].ActivatedAt != nil {
		t.Fatalf("governed proposal gained authority outside candidate lifecycle: items=%+v err=%v", items, err)
	}
	rolled := postImprovementAction(t, handler, proposal.ID, "rollback", `{"reason":"package review withdrawn"}`)
	if rolled.Code != http.StatusOK {
		t.Fatalf("governed rollback status=%d body=%s", rolled.Code, rolled.Body.String())
	}
	version, err := e.capabilities.Get(context.Background(), items[0].Slug, items[0].Version)
	if err != nil || version.State != capabilityhub.StateUninstalled {
		t.Fatalf("candidate rollback did not uninstall exact version: capability=%+v err=%v", version, err)
	}
}
