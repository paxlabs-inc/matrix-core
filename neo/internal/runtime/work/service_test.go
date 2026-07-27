// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package work

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"matrix/cortex"
	cortexstore "matrix/cortex/store"
	executortool "matrix/executor/tool"
	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/protocol"
	"matrix/neo/internal/runtime/provider"
	"matrix/neo/internal/runtime/turnstate"
	neotools "matrix/neo/internal/tools"
	"matrix/vault"
)

type criterionResolver string

func (resolver criterionResolver) SubgoalFor(
	protocol.NormalizedToolCall,
) string {
	return string(resolver)
}

type workHarness struct {
	service  *Service
	store    *turnstate.Store
	manager  *neotools.Manager
	cortex   *cortex.Cortex
	metadata ToolMetadata
	executor Executor
	effects  EffectReconciler
}

func TestSupervisorRealLoopEnforcesAuthorityProjectionAndEvidence(
	t *testing.T,
) {
	var (
		mu       sync.Mutex
		taskStep = make(map[string]int)
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			taskID := requestTaskID(decoded)
			mu.Lock()
			taskStep[taskID]++
			step := taskStep[taskID]
			mu.Unlock()
			if step == 1 {
				writeWorkTool(
					writer, "call-"+taskID, "exec__shell",
					map[string]interface{}{
						"command": "printf " + taskID,
						"cwd":     t.TempDir(),
						"expect":  "prints " + taskID,
					},
				)
				return
			}
			writeWorkText(writer, "Verified "+taskID+" with current evidence.")
		},
	))
	t.Cleanup(gateway.Close)
	harness := openWorkHarness(t, gateway.URL)
	plan := workPlan("plan-evidence", 1)
	task := workTask("criterion-live", "criterion-live")

	denied := task
	denied.Scope.ExternalEffects = false
	_, err := harness.service.Start(
		t.Context(), "actor-evidence",
		StartInput{
			ConversationID: "conv-denied",
			Plan:           plan,
			Tasks:          []TaskSpec{denied},
		},
	)
	if !errors.Is(err, ErrAuthorityDenied) {
		t.Fatalf("ungranted shell authority = %v", err)
	}

	overBudget := task
	overBudget.ProjectedUsage.Tokens = plan.Budget.MaxTokens + 1
	_, err = harness.service.Start(
		t.Context(), "actor-evidence",
		StartInput{
			ConversationID: "conv-over-budget",
			Plan:           plan,
			Tasks:          []TaskSpec{overBudget},
		},
	)
	if !errors.Is(err, ErrBudgetProjection) {
		t.Fatalf("unfinishable projection = %v", err)
	}
	perSpecialistPlan := workPlan("plan-specialist-budget", 2)
	firstProjection := workTask("criterion-first-budget", "criterion-first-budget")
	firstProjection.ProjectedUsage.Tokens = 1_500
	secondProjection := workTask("criterion-second-budget", "criterion-second-budget")
	_, err = harness.service.Start(
		t.Context(), "actor-evidence",
		StartInput{
			ConversationID: "conv-specialist-budget",
			Plan:           perSpecialistPlan,
			Tasks: []TaskSpec{
				firstProjection, secondProjection,
			},
		},
	)
	if !errors.Is(err, ErrBudgetProjection) {
		t.Fatalf("specialist projection widened parent overlay: %v", err)
	}

	withEnvironment := task
	withEnvironment.Scope.EnvironmentKeys = []string{"MATRIX_VAULT_KEY"}
	_, err = harness.service.Start(
		t.Context(), "actor-evidence",
		StartInput{
			ConversationID: "conv-environment",
			Plan:           plan,
			Tasks:          []TaskSpec{withEnvironment},
		},
	)
	if !errors.Is(err, ErrAuthorityDenied) {
		t.Fatalf("specialist inherited an environment key: %v", err)
	}

	run, err := harness.service.Start(
		t.Context(), "actor-evidence",
		StartInput{
			ConversationID: "conv-evidence",
			Plan:           plan,
			Tasks:          []TaskSpec{task},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForWorkRun(
		t, harness.service, "actor-evidence", run.ID,
		func(current SupervisorRun) bool {
			return current.Status == RunCompleted
		},
	)
	if len(completed.Tasks) != 1 ||
		completed.Tasks[0].Status != TaskCompleted ||
		len(completed.Tasks[0].Attempts) != 1 ||
		len(completed.Tasks[0].Attempts[0].Evidence) != 1 {
		t.Fatalf("verified supervisor completion = %+v", completed)
	}
	reference := completed.Tasks[0].Attempts[0].Evidence[0]
	payload, err := harness.cortex.VerifyToolEventCitation(reference.Citation)
	if err != nil {
		t.Fatal(err)
	}
	if reference.Criterion != "criterion-live" ||
		payload.SubgoalID != "criterion-live" ||
		payload.MatchVerdict != cortex.ToolMatchMatched ||
		payload.Error != "" {
		t.Fatalf("completion evidence = %+v payload=%+v", reference, payload)
	}
	parent, err := loop.NewToolManagerAdapter(
		harness.manager,
		&loop.DurableEffectJournal{Store: harness.store},
	)
	if err != nil {
		t.Fatal(err)
	}
	scoped, err := NewScopedToolManager(
		parent, completed.Tasks[0].Packet,
		ManifestToolMetadata{Manager: harness.manager},
	)
	if err != nil {
		t.Fatal(err)
	}
	_, err = scoped.Execute(
		t.Context(),
		protocol.NormalizedToolCall{
			ID: "outside-packet", Name: "exec__service_list",
			Arguments: json.RawMessage(`{"expect":"lists services"}`),
		},
		"outside-packet-key",
	)
	if !errors.Is(err, ErrAuthorityDenied) {
		t.Fatalf("unadvertised packet tool executed: %v", err)
	}
	mutated := completed.Tasks[0].Packet
	mutated.Title = "mutated after dispatch"
	if _, err := NewScopedToolManager(
		parent, mutated,
		ManifestToolMetadata{Manager: harness.manager},
	); err == nil || !strings.Contains(err.Error(), "immutable task packet") {
		t.Fatalf("mutated task packet accepted: %v", err)
	}
}

func TestSupervisorSerializesConflictingLeasesAcrossRealSubloops(
	t *testing.T,
) {
	releaseFirst := make(chan struct{})
	var releaseFirstOnce sync.Once
	release := func() {
		releaseFirstOnce.Do(func() {
			close(releaseFirst)
		})
	}
	t.Cleanup(release)
	firstStarted := make(chan struct{}, 1)
	var (
		mu       sync.Mutex
		taskStep = make(map[string]int)
		started  atomic.Int32
	)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			taskID := requestTaskID(decoded)
			mu.Lock()
			taskStep[taskID]++
			step := taskStep[taskID]
			mu.Unlock()
			if step == 1 {
				started.Add(1)
				if taskID == "criterion-a" {
					select {
					case firstStarted <- struct{}{}:
					default:
					}
					<-releaseFirst
				}
				writeWorkTool(
					writer, "call-"+taskID, "exec__shell",
					map[string]interface{}{
						"command": "printf " + taskID,
						"cwd":     t.TempDir(),
						"expect":  "prints " + taskID,
					},
				)
				return
			}
			writeWorkText(writer, "Verified "+taskID+".")
		},
	))
	t.Cleanup(gateway.Close)
	harness := openWorkHarness(t, gateway.URL)
	plan := workPlan("plan-leases", 2)
	first := workTask("criterion-a", "criterion-a")
	second := workTask("criterion-b", "criterion-b")
	run, err := harness.service.Start(
		t.Context(), "actor-leases",
		StartInput{
			ConversationID: "conv-leases", Plan: plan,
			Tasks: []TaskSpec{first, second},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstStarted:
	case <-time.After(3 * time.Second):
		t.Fatal("first real specialist did not start")
	}
	live := waitForWorkRun(
		t, harness.service, "actor-leases", run.ID,
		func(current SupervisorRun) bool {
			return current.Tasks[0].Status == TaskRunning
		},
	)
	release()
	if started.Load() != 1 ||
		live.Tasks[0].Status != TaskRunning ||
		live.Tasks[1].Status != TaskReady ||
		len(live.Leases) == 0 {
		t.Fatalf("conflicting scope was not serialized: %+v", live)
	}
	completed := waitForWorkRun(
		t, harness.service, "actor-leases", run.ID,
		func(current SupervisorRun) bool {
			return current.Status == RunCompleted
		},
	)
	if started.Load() != 2 || len(completed.Leases) != 0 {
		t.Fatalf("serialized completion = %+v started=%d",
			completed, started.Load())
	}
}

func TestSupervisorRetriesARealTransientLegWithoutLosingTheRun(
	t *testing.T,
) {
	var nonCanary atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			current := nonCanary.Add(1)
			if current <= 3 {
				http.Error(writer, "transient upstream failure", http.StatusBadGateway)
				return
			}
			taskID := requestTaskID(decoded)
			if current == 4 {
				writeWorkTool(
					writer, "call-"+taskID, "exec__shell",
					map[string]interface{}{
						"command": "printf " + taskID,
						"cwd":     t.TempDir(),
						"expect":  "prints " + taskID,
					},
				)
				return
			}
			writeWorkText(writer, "Recovered and verified "+taskID+".")
		},
	))
	t.Cleanup(gateway.Close)
	harness := openWorkHarness(t, gateway.URL)
	plan := workPlan("plan-transient", 1)
	plan.Budget.MaxRetries = 1
	task := workTask("criterion-recovered", "criterion-recovered")
	task.Budget.MaxRetries = 1
	run, err := harness.service.Start(
		t.Context(), "actor-transient",
		StartInput{
			ConversationID: "conv-transient", Plan: plan,
			Tasks: []TaskSpec{task},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	completed := waitForWorkRun(
		t, harness.service, "actor-transient", run.ID,
		func(current SupervisorRun) bool {
			return current.Status == RunCompleted
		},
	)
	if len(completed.Tasks[0].Attempts) != 2 ||
		completed.Tasks[0].Attempts[0].Status != TaskRetrying ||
		completed.Tasks[0].Attempts[1].Status != TaskCompleted ||
		completed.Usage.Retries != 1 {
		t.Fatalf("transient recovery = %+v", completed)
	}
}

func TestSupervisorRejectsNarrativeCompletionWithoutAnchoredEvidence(
	t *testing.T,
) {
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			writeWorkText(writer, "Everything is complete.")
		},
	))
	t.Cleanup(gateway.Close)
	harness := openWorkHarness(t, gateway.URL)
	run, err := harness.service.Start(
		t.Context(), "actor-narrative",
		StartInput{
			ConversationID: "conv-narrative",
			Plan:           workPlan("plan-narrative", 1),
			Tasks: []TaskSpec{
				workTask("criterion-unproven", "criterion-unproven"),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	blocked := waitForWorkRun(
		t, harness.service, "actor-narrative", run.ID,
		func(current SupervisorRun) bool {
			return current.Status == RunBlocked
		},
	)
	if blocked.Tasks[0].Status != TaskWaitingEvidence ||
		blocked.Tasks[0].Progress >= 100 ||
		!strings.Contains(
			blocked.Tasks[0].BlockingReason, ErrEvidenceRequired.Error(),
		) {
		t.Fatalf("narrative crossed evidence gate: %+v", blocked)
	}
}

func TestSupervisorSteerChangesTheNextLiveLoopAttempt(t *testing.T) {
	firstRequest := make(chan struct{})
	releaseFirst := make(chan struct{})
	var (
		releaseOnce sync.Once
		requests    atomic.Int32
		steerSeen   atomic.Bool
	)
	release := func() {
		releaseOnce.Do(func() {
			close(releaseFirst)
		})
	}
	t.Cleanup(release)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			switch requests.Add(1) {
			case 1:
				close(firstRequest)
				<-releaseFirst
				writeWorkTool(
					writer, "steer-evidence", "exec__shell",
					map[string]interface{}{
						"command": "printf steered",
						"cwd":     t.TempDir(),
						"expect":  "prints steered",
					},
				)
			default:
				for _, message := range decoded.Messages {
					if strings.Contains(
						message.Content,
						"Report the steered verification explicitly.",
					) {
						steerSeen.Store(true)
					}
				}
				writeWorkText(
					writer,
					"The steered verification is complete from current evidence.",
				)
			}
		},
	))
	t.Cleanup(gateway.Close)
	harness := openWorkHarness(t, gateway.URL)
	run, err := harness.service.Start(
		t.Context(), "actor-steer",
		StartInput{
			ConversationID: "conv-steer",
			Plan:           workPlan("plan-steer", 1),
			Tasks: []TaskSpec{
				workTask("criterion-steer", "criterion-steer"),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-firstRequest:
	case <-time.After(5 * time.Second):
		t.Fatal("live specialist did not enter its provider attempt")
	}
	steered, err := harness.service.Steer(
		t.Context(), "actor-steer", run.ID,
		"Report the steered verification explicitly.",
	)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if len(steered.Steering) != 1 {
		t.Fatalf("steering was not persisted: %+v", steered)
	}
	completed := waitForWorkRun(
		t, harness.service, "actor-steer", run.ID,
		func(current SupervisorRun) bool {
			return current.Status == RunCompleted
		},
	)
	if !steerSeen.Load() ||
		!strings.Contains(
			completed.Tasks[0].Attempts[0].Summary,
			"steered verification",
		) {
		t.Fatalf("live steering did not alter the next attempt: %+v", completed)
	}
}

func TestSupervisorCancelIsDurable(t *testing.T) {
	requestStarted := make(chan struct{})
	requestStopped := make(chan struct{})
	var startedOnce sync.Once
	var stoppedOnce sync.Once
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			startedOnce.Do(func() {
				close(requestStarted)
			})
			select {
			case <-request.Context().Done():
			case <-time.After(8 * time.Second):
			}
			stoppedOnce.Do(func() {
				close(requestStopped)
			})
		},
	))
	t.Cleanup(gateway.Close)
	harness := openWorkHarness(t, gateway.URL)
	run, err := harness.service.Start(
		t.Context(), "actor-cancel",
		StartInput{
			ConversationID: "conv-cancel",
			Plan:           workPlan("plan-cancel", 1),
			Tasks: []TaskSpec{
				workTask("criterion-cancel", "criterion-cancel"),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("live specialist did not start")
	}
	cancelled, err := harness.service.Cancel(
		t.Context(), "actor-cancel", run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.Status != RunCancelled ||
		cancelled.FinishedAt == nil ||
		cancelled.Tasks[0].Status != TaskCancelled ||
		cancelled.Tasks[0].Attempts[0].Status != TaskCancelled {
		t.Fatalf("cancelled run = %+v", cancelled)
	}
	select {
	case <-requestStopped:
	case <-time.After(time.Second):
		t.Fatal("durable cancellation did not stop the live worker")
	}
	record, err := harness.store.LoadSupervisorRun(
		t.Context(), run.ID.String(),
	)
	if err != nil {
		t.Fatal(err)
	}
	var durable SupervisorRun
	if err := json.Unmarshal(record.State, &durable); err != nil {
		t.Fatal(err)
	}
	if durable.Status != RunCancelled ||
		durable.Tasks[0].Attempts[0].Status != TaskCancelled {
		t.Fatalf("durable cancellation = %+v", durable)
	}
	recovered, err := harness.service.ReconcileRestart(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 0 {
		t.Fatalf("boot reconciliation revived cancellation: %+v", recovered)
	}
}

func TestSupervisorBootStopsOnUncertainPersistedEffect(t *testing.T) {
	releaseRequest := make(chan struct{})
	requestStarted := make(chan struct{})
	var (
		releaseOnce sync.Once
		startedOnce sync.Once
	)
	release := func() {
		releaseOnce.Do(func() {
			close(releaseRequest)
		})
	}
	t.Cleanup(release)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			startedOnce.Do(func() {
				close(requestStarted)
			})
			<-releaseRequest
			writeWorkText(writer, "This worker should not settle the restarted run.")
		},
	))
	t.Cleanup(gateway.Close)
	harness := openWorkHarness(t, gateway.URL)
	run, err := harness.service.Start(
		t.Context(), "actor-uncertain",
		StartInput{
			ConversationID: "conv-uncertain",
			Plan:           workPlan("plan-uncertain", 1),
			Tasks: []TaskSpec{
				workTask("criterion-uncertain", "criterion-uncertain"),
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-requestStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("live specialist did not start")
	}
	durable, err := harness.service.Get(
		t.Context(), "actor-uncertain", run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	effectKey := "persisted-uncertain-effect"
	if err := harness.store.BeginEffect(
		t.Context(), effectKey, "exec__shell",
		json.RawMessage(`{"command":"true"}`), false,
	); err != nil {
		t.Fatal(err)
	}
	durable.Tasks[0].Attempts[0].ExternalEffects = []ExternalEffect{{
		ToolName:       "exec__shell",
		IdempotencyKey: effectKey,
		State:          "started",
	}}
	raw, err := json.Marshal(durable)
	if err != nil {
		t.Fatal(err)
	}
	if err := harness.store.SaveSupervisorRun(
		t.Context(),
		turnstate.SupervisorRecord{
			RunID: durable.ID.String(), ActorID: durable.ActorID,
			Status: string(durable.Status), State: raw,
		},
	); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewService(
		harness.store, harness.cortex, harness.metadata,
		harness.executor, harness.effects,
	)
	if err != nil {
		t.Fatal(err)
	}
	release()
	unknown, err := restarted.Get(
		t.Context(), "actor-uncertain", run.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if unknown.Status != RunOutcomeUnknown ||
		unknown.Tasks[0].Status != TaskOutcomeUnknown ||
		len(unknown.Reconciliations) != 1 ||
		unknown.Reconciliations[0].UncertainTasks != 1 {
		t.Fatalf("uncertain restart crossed reconciliation gate: %+v", unknown)
	}
}

func TestSupervisorKill9MidAttemptBootReconciliationContinuesRun(
	t *testing.T,
) {
	if os.Getenv("RESURRECTION_WORK_KILL_HELPER") == "1" {
		runWorkKillHelper(t)
		return
	}
	root := t.TempDir()
	runFile := filepath.Join(root, "run-id")
	effectFile := filepath.Join(root, "restart-effect")
	blocked := make(chan struct{})
	var (
		blockedOnce sync.Once
		phase       atomic.Int32
		childStep   atomic.Int32
	)
	phase.Store(1)
	gateway := httptest.NewServer(http.HandlerFunc(
		func(writer http.ResponseWriter, request *http.Request) {
			decoded := decodeWorkGatewayRequest(t, request)
			if answerWorkCanary(writer, decoded) {
				return
			}
			if phase.Load() == 1 {
				if childStep.Add(1) == 1 {
					writeWorkTool(
						writer, "restart-evidence", "exec__shell",
						map[string]interface{}{
							"command": "printf restart-marker | tee -a " +
								effectFile,
							"cwd":    root,
							"expect": "prints restart-marker",
						},
					)
					return
				}
				blockedOnce.Do(func() {
					close(blocked)
				})
				<-request.Context().Done()
				return
			}
			writeWorkText(
				writer,
				"The restarted specialist completed from current evidence.",
			)
		},
	))
	t.Cleanup(gateway.Close)
	command := exec.Command(
		os.Args[0],
		"-test.run=TestSupervisorKill9MidAttemptBootReconciliationContinuesRun",
	)
	command.Env = append(
		os.Environ(),
		"RESURRECTION_WORK_KILL_HELPER=1",
		"RESURRECTION_WORK_KILL_ROOT="+root,
		"RESURRECTION_WORK_KILL_GATEWAY="+gateway.URL,
		"RESURRECTION_WORK_KILL_RUN_FILE="+runFile,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.Process != nil {
			_ = command.Process.Kill()
		}
	})
	select {
	case <-blocked:
	case <-time.After(30 * time.Second):
		t.Fatal("child did not reach the live provider attempt")
	}
	var runID uuid.UUID
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(runFile)
		if err == nil {
			runID, err = uuid.Parse(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatal(err)
			}
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if runID == uuid.Nil {
		t.Fatal("child did not persist its supervisor run ID")
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()
	if raw, err := os.ReadFile(effectFile); err != nil ||
		string(raw) != "restart-marker" {
		t.Fatalf("pre-restart effect = %q err=%v", raw, err)
	}
	phase.Store(2)

	harness := openWorkHarnessAt(t, gateway.URL, root)
	reconciled, err := harness.service.Get(
		t.Context(), "actor-restart", runID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(reconciled.Reconciliations) != 1 ||
		reconciled.Reconciliations[0].RecoveredTasks != 1 {
		t.Fatalf("boot reconciliation = %+v", reconciled)
	}
	completed := waitForWorkRun(
		t, harness.service, "actor-restart", runID,
		func(current SupervisorRun) bool {
			return current.Status == RunCompleted ||
				current.Status == RunBlocked
		},
	)
	if completed.Status != RunCompleted {
		var verifyErr error
		if len(completed.Tasks[0].Attempts[0].Evidence) > 0 {
			reference := completed.Tasks[0].Attempts[0].Evidence[0]
			_, verifyErr = harness.cortex.VerifyToolEventCitation(
				reference.Citation,
			)
		}
		t.Fatalf(
			"restarted evidence did not verify: run=%+v verify=%v",
			completed, verifyErr,
		)
	}
	if len(completed.Tasks[0].Attempts) != 1 ||
		completed.Tasks[0].Attempts[0].Status != TaskCompleted ||
		completed.Tasks[0].Attempts[0].Number != 1 {
		t.Fatalf("restarted supervisor run = %+v", completed)
	}
	if raw, err := os.ReadFile(effectFile); err != nil ||
		string(raw) != "restart-marker" {
		t.Fatalf("restart replayed completed effect = %q err=%v", raw, err)
	}
}

func runWorkKillHelper(t *testing.T) {
	root := strings.TrimSpace(os.Getenv("RESURRECTION_WORK_KILL_ROOT"))
	gatewayURL := strings.TrimSpace(
		os.Getenv("RESURRECTION_WORK_KILL_GATEWAY"),
	)
	runFile := strings.TrimSpace(
		os.Getenv("RESURRECTION_WORK_KILL_RUN_FILE"),
	)
	if root == "" || gatewayURL == "" || runFile == "" {
		os.Exit(2)
	}
	harness := openWorkHarnessAt(t, gatewayURL, root)
	run, err := harness.service.Start(
		context.Background(), "actor-restart",
		StartInput{
			ConversationID: "conv-restart",
			Plan:           workPlan("plan-restart", 1),
			Tasks: []TaskSpec{
				workTask("criterion-restart", "criterion-restart"),
			},
		},
	)
	if err != nil {
		os.Exit(3)
	}
	if err := os.WriteFile(
		runFile, []byte(run.ID.String()), 0o600,
	); err != nil {
		os.Exit(4)
	}
	select {}
}

func openWorkHarness(t *testing.T, gatewayURL string) workHarness {
	t.Helper()
	return openWorkHarnessAt(t, gatewayURL, t.TempDir())
}

func openWorkHarnessAt(
	t *testing.T,
	gatewayURL string,
	root string,
) workHarness {
	t.Helper()
	t.Setenv("MATRIX_GATEWAY_TOKEN", "runtime-work-test-token")
	session, err := vault.Boot(t.Context(), vault.Config{
		Required: true, DataDir: root,
		UserDID: "did:matrix:runtime-work-test",
		KEKHex: hex.EncodeToString(
			bytes.Repeat([]byte{0x69}, 32),
		),
	})
	if err != nil {
		t.Fatal(err)
	}
	turns, err := turnstate.Open(
		t.Context(), filepath.Join(root, "turnstate.db"), session,
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(
			context.Background(), 10*time.Second,
		)
		defer cancel()
		if err := turns.Close(ctx); err != nil {
			t.Errorf("close turnstate: %v", err)
		}
	})
	journalStore, err := cortexstore.Open(
		filepath.Join(root, "cortex"), "runtime-work-test", nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	journalStore.SetVault(session, "did:matrix:runtime-work-test")
	t.Cleanup(func() { _ = journalStore.Close() })
	cx := cortex.New(journalStore)
	manager := realWorkExecManager(t)
	effects := &loop.DurableEffectJournal{Store: turns}
	adapter, err := loop.NewToolManagerAdapter(manager, effects)
	if err != nil {
		t.Fatal(err)
	}
	metadata := ManifestToolMetadata{Manager: manager}
	executor := &LoopExecutor{
		Parent: adapter, Metadata: metadata, Store: turns,
		NewLoop: func(
			ctx context.Context,
			packet TaskPacket,
			attemptID uuid.UUID,
			scoped ToolManager,
			guidance loop.GuidanceSource,
		) (*loop.Loop, error) {
			generator, err := newWorkMiMoGenerator(gatewayURL)
			if err != nil {
				return nil, err
			}
			return loop.New(
				generator, scoped, turns,
				loop.Config{
					TurnID:         attemptID.String(),
					ConversationID: packet.ConversationID,
					Model:          "mimo-v2",
					MaxToolCalls:   packet.Budget.MaxToolCalls,
					MaxTurnTokens:  int(packet.Budget.MaxTokens),
					IdleTimeout:    10 * time.Second,
				},
				loop.Dependencies{
					EvidenceJournal: &loop.CortexToolJournal{
						Cortex: cx, CreatedBy: packet.ActorID,
					},
					Subgoals: criterionResolver(packet.Criteria[0]),
					Guidance: guidance,
				},
			)
		},
	}
	service, err := NewService(
		turns, cx, metadata, executor, effects,
	)
	if err != nil {
		t.Fatal(err)
	}
	return workHarness{
		service: service, store: turns, manager: manager, cortex: cx,
		metadata: metadata, executor: executor, effects: effects,
	}
}

func realWorkExecManager(t *testing.T) *neotools.Manager {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Fatalf("node is required for real exec dispatch: %v", err)
	}
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve runtime work test source")
	}
	execBridge := filepath.Clean(filepath.Join(
		filepath.Dir(thisFile), "..", "..", "..", "..",
		"tools", "exec", "exec.mjs",
	))
	manifestPath := filepath.Join(t.TempDir(), "agent.json")
	manifest := executortool.AgentManifest{
		SchemaVersion: 1,
		Agent:         "matrix://agent/resurrection-work-test",
		Servers: []executortool.ServerEntry{{
			Alias: "exec", Transport: "stdio",
			Command: "node", Args: []string{execBridge},
			PackageDigest: "sha256:" + strings.Repeat("c", 64),
			Version:       "0.1.0",
			Tools: []executortool.ToolEntry{
				{Name: "shell", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10_000},
				{Name: "service_start", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10_000},
				{Name: "service_list", SideEffectClass: executortool.SideEffectRead, TimeoutMs: 10_000},
				{Name: "service_logs", SideEffectClass: executortool.SideEffectRead, TimeoutMs: 10_000},
				{Name: "service_stop", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10_000},
				{Name: "service_restart", SideEffectClass: executortool.SideEffectShell, TimeoutMs: 10_000},
			},
		}},
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := neotools.Spawn(
		t.Context(),
		neotools.Options{
			ManifestPath: manifestPath,
			SpawnTimeout: 20 * time.Second,
			StderrSink:   io.Discard,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if warnings := manager.Warnings(); len(warnings) != 0 {
		_ = manager.Close()
		t.Fatalf("real exec bridge warnings: %v", warnings)
	}
	t.Cleanup(func() { _ = manager.Close() })
	return manager
}

func newWorkMiMoGenerator(
	gatewayURL string,
) (*provider.MiMoGenerator, error) {
	adapter := &provider.MiMoAdapter{}
	client, err := provider.New(adapter, provider.Config{
		GatewayURL:     gatewayURL,
		BearerEnv:      "MATRIX_GATEWAY_TOKEN",
		ActorDID:       "did:matrix:runtime-work-test",
		MaxAttempts:    3,
		BackoffInitial: time.Millisecond,
		BackoffMax:     2 * time.Millisecond,
		IdleTimeout:    5 * time.Second,
	})
	if err != nil {
		return nil, err
	}
	return provider.NewMiMoGenerator(client, adapter)
}

func workPlan(id string, parallel int) ParentPlan {
	return ParentPlan{
		ID: id, Accepted: true,
		Scope: AuthorityScope{
			ReadFiles:       []string{"workspace"},
			WriteFiles:      []string{"workspace"},
			Services:        []string{"workspace-process"},
			ExternalEffects: true,
		},
		Tools: []string{"exec__shell"},
		Budget: SupervisorBudget{
			SpecialistBudget: SpecialistBudget{
				MaxTokens:       2_000,
				MaxToolCalls:    20,
				MaxWallSeconds:  60,
				MaxProcesses:    4,
				MaxStorageBytes: 1 << 20,
				MaxNetworkBytes: 1 << 20,
				MaxRetries:      1,
			},
			MaxParallel: parallel,
		},
	}
}

func workTask(id string, criterion string) TaskSpec {
	return TaskSpec{
		ID: id, Title: "Verify " + criterion,
		Prompt:   "Run the real verification for " + criterion + ".",
		Criteria: []string{criterion},
		Scope: AuthorityScope{
			ReadFiles:       []string{"workspace"},
			WriteFiles:      []string{"workspace"},
			Services:        []string{"workspace-process"},
			ExternalEffects: true,
		},
		Tools: []string{"exec__shell"},
		ProjectedUsage: BudgetUsage{
			Tokens: 100, ToolCalls: 1, WallSeconds: 5,
			Processes: 1, ModelCostKnown: true,
			ProviderSpendKnown: true,
		},
	}
}

func waitForWorkRun(
	t *testing.T,
	service *Service,
	actor string,
	runID uuid.UUID,
	predicate func(SupervisorRun) bool,
) SupervisorRun {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		run, err := service.Get(t.Context(), actor, runID)
		if err != nil {
			t.Fatal(err)
		}
		if predicate(run) {
			return run
		}
		time.Sleep(5 * time.Millisecond)
	}
	run, _ := service.Get(t.Context(), actor, runID)
	t.Fatalf("supervisor condition timed out: %+v", run)
	return SupervisorRun{}
}

type workGatewayRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
	Tools []struct {
		Function struct {
			Name string `json:"name"`
		} `json:"function"`
	} `json:"tools"`
}

func decodeWorkGatewayRequest(
	t *testing.T,
	request *http.Request,
) workGatewayRequest {
	t.Helper()
	body, err := io.ReadAll(request.Body)
	if err != nil {
		t.Fatal(err)
	}
	var decoded workGatewayRequest
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatal(err)
	}
	return decoded
}

func answerWorkCanary(
	writer http.ResponseWriter,
	request workGatewayRequest,
) bool {
	canary := false
	for _, tool := range request.Tools {
		if tool.Function.Name == "matrix_runtime_capability_echo" {
			canary = true
			break
		}
	}
	if !canary {
		return false
	}
	latest := request.Messages[len(request.Messages)-1].Content
	if strings.Contains(latest, "Reply with READY") {
		writeWorkText(writer, "READY")
		return true
	}
	writeWorkTool(
		writer, "capability-call", "matrix_runtime_capability_echo",
		map[string]interface{}{
			"value": "READY", "expect": "returns READY",
		},
	)
	return true
}

func requestTaskID(request workGatewayRequest) string {
	for _, message := range request.Messages {
		for _, criterion := range []string{
			"criterion-live", "criterion-a", "criterion-b",
			"criterion-recovered", "criterion-unproven",
		} {
			if strings.Contains(message.Content, criterion) {
				return criterion
			}
		}
	}
	return "unknown"
}

func writeWorkText(writer http.ResponseWriter, content string) {
	writer.Header().Set("Content-Type", "text/event-stream")
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index":         0,
			"delta":         map[string]interface{}{"content": content},
			"finish_reason": "stop",
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 4, "completion_tokens": 3,
			"total_tokens": 7,
		},
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}

func writeWorkTool(
	writer http.ResponseWriter,
	id string,
	name string,
	arguments map[string]interface{},
) {
	writer.Header().Set("Content-Type", "text/event-stream")
	rawArguments, _ := json.Marshal(arguments)
	payload, _ := json.Marshal(map[string]interface{}{
		"model": "mimo-v2",
		"choices": []interface{}{map[string]interface{}{
			"index": 0,
			"delta": map[string]interface{}{
				"tool_calls": []interface{}{map[string]interface{}{
					"index": 0, "id": id, "type": "function",
					"function": map[string]interface{}{
						"name": name, "arguments": string(rawArguments),
					},
				}},
			},
			"finish_reason": "tool_calls",
		}},
		"usage": map[string]interface{}{
			"prompt_tokens": 4, "completion_tokens": 3,
			"total_tokens": 7,
		},
	})
	fmt.Fprintf(writer, "data: %s\n\n", payload)
	fmt.Fprint(writer, "data: [DONE]\n\n")
}
