// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package work

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"matrix/cortex"
	"matrix/neo/internal/runtime/loop"
	"matrix/neo/internal/runtime/turnstate"
)

type Service struct {
	mu         sync.Mutex
	store      RunStore
	verifier   CitationVerifier
	metadata   ToolMetadata
	executor   Executor
	reconciler EffectReconciler
	now        func() time.Time
	running    map[uuid.UUID]attemptControl
}

type attemptControl struct {
	cancel   context.CancelFunc
	steering chan SteeringRecord
}

func NewService(
	store RunStore,
	verifier CitationVerifier,
	metadata ToolMetadata,
	executor Executor,
	reconciler EffectReconciler,
) (*Service, error) {
	if store == nil || verifier == nil || metadata == nil || executor == nil {
		return nil, fmt.Errorf(
			"runtime work: store, evidence verifier, manifest metadata, and executor are required",
		)
	}
	service := &Service{
		store: store, verifier: verifier, metadata: metadata,
		executor: executor, reconciler: reconciler,
		now: time.Now, running: make(map[uuid.UUID]attemptControl),
	}
	if _, err := service.ReconcileRestart(context.Background()); err != nil {
		return nil, err
	}
	return service, nil
}

func (service *Service) Start(
	ctx context.Context,
	actorID string,
	input StartInput,
) (SupervisorRun, error) {
	actorID = strings.TrimSpace(actorID)
	input.ConversationID = strings.TrimSpace(input.ConversationID)
	input.Plan.ID = strings.TrimSpace(input.Plan.ID)
	if actorID == "" || input.ConversationID == "" ||
		input.Plan.ID == "" || !input.Plan.Accepted {
		return SupervisorRun{}, fmt.Errorf(
			"%w: an accepted parent plan, actor, and conversation are required",
			ErrAuthorityDenied,
		)
	}
	if len(input.Tasks) == 0 || len(input.Tasks) > maxTasks {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: one or more bounded tasks are required",
		)
	}
	budget, err := normalizeSupervisorBudget(input.Plan.Budget)
	if err != nil {
		return SupervisorRun{}, err
	}
	input.Plan.Budget = budget
	if input.Plan.Scope.ExternalEffects && service.reconciler == nil {
		return SupervisorRun{}, fmt.Errorf(
			"%w: external effects require durable reconciliation",
			ErrAuthorityDenied,
		)
	}
	available := make(map[string]struct{})
	for _, definition := range service.executor.Surface(ctx) {
		available[definition.Name] = struct{}{}
	}
	planTools := trimUnique(input.Plan.Tools)
	if len(planTools) == 0 {
		return SupervisorRun{}, fmt.Errorf(
			"%w: accepted plan has no granted tool subset",
			ErrAuthorityDenied,
		)
	}
	for _, name := range planTools {
		if _, ok := available[name]; !ok {
			return SupervisorRun{}, fmt.Errorf(
				"%w: plan tool %q is not manifest-bound",
				ErrAuthorityDenied, name,
			)
		}
	}
	now := service.now().UTC()
	run := SupervisorRun{
		ID: uuid.New(), ActorID: actorID,
		ConversationID: input.ConversationID,
		ParentPlanID:   input.Plan.ID, Status: RunQueued,
		Budget: budget, CreatedAt: now, UpdatedAt: now,
	}
	packets, projection, err := service.compilePackets(
		run, input.Plan, input.Tasks, available,
	)
	if err != nil {
		return SupervisorRun{}, err
	}
	run.Projection = projection
	for _, packet := range packets {
		status := TaskPending
		if len(packet.DependsOn) == 0 {
			status = TaskReady
		}
		run.Tasks = append(run.Tasks, SpecialistTask{
			Packet: packet, Status: status, UpdatedAt: now,
		})
	}
	sort.SliceStable(run.Tasks, func(left int, right int) bool {
		return run.Tasks[left].Packet.ID < run.Tasks[right].Packet.ID
	})

	service.mu.Lock()
	defer service.mu.Unlock()
	records, err := service.store.ListSupervisorRuns(ctx, actorID)
	if err != nil {
		return SupervisorRun{}, err
	}
	for _, record := range records {
		var existing SupervisorRun
		if json.Unmarshal(record.State, &existing) == nil &&
			existing.ConversationID == run.ConversationID &&
			!terminalRun(existing.Status) {
			return SupervisorRun{}, fmt.Errorf(
				"runtime work: conversation already has an active supervisor",
			)
		}
	}
	if err := service.saveRun(ctx, &run); err != nil {
		return SupervisorRun{}, err
	}
	if err := service.scheduleLocked(
		context.WithoutCancel(ctx), &run,
	); err != nil {
		return SupervisorRun{}, err
	}
	return cloneRun(run), nil
}

func (service *Service) Get(
	ctx context.Context,
	actorID string,
	runID uuid.UUID,
) (SupervisorRun, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	run, err := service.loadRun(ctx, actorID, runID)
	if err != nil {
		return SupervisorRun{}, err
	}
	return cloneRun(run), nil
}

func (service *Service) List(
	ctx context.Context,
	actorID string,
) ([]SupervisorRun, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	records, err := service.store.ListSupervisorRuns(
		ctx, strings.TrimSpace(actorID),
	)
	if err != nil {
		return nil, err
	}
	runs := make([]SupervisorRun, 0, len(records))
	for _, record := range records {
		var run SupervisorRun
		if err := json.Unmarshal(record.State, &run); err != nil {
			return nil, fmt.Errorf(
				"runtime work: decode supervisor run: %w", err,
			)
		}
		if err := validateRun(run); err != nil {
			return nil, err
		}
		runs = append(runs, cloneRun(run))
	}
	sort.SliceStable(runs, func(left int, right int) bool {
		return runs[left].CreatedAt.After(runs[right].CreatedAt)
	})
	return runs, nil
}

func (service *Service) Steer(
	ctx context.Context,
	actorID string,
	runID uuid.UUID,
	instruction string,
) (SupervisorRun, error) {
	instruction = boundedText(instruction)
	if instruction == "" {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: bounded steering instruction is required",
		)
	}
	service.mu.Lock()
	defer service.mu.Unlock()
	run, err := service.loadRun(ctx, strings.TrimSpace(actorID), runID)
	if err != nil {
		return SupervisorRun{}, err
	}
	if terminalRun(run.Status) {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: active supervisor run not found",
		)
	}
	if len(run.Steering) >= maxSteeringRecords {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: steering record limit reached",
		)
	}
	record := SteeringRecord{
		At: service.now().UTC(), Instruction: instruction,
	}
	run.Steering = append(run.Steering, record)
	run.UpdatedAt = record.At
	run.Revision++
	if err := service.saveRun(ctx, &run); err != nil {
		return SupervisorRun{}, err
	}
	for _, task := range run.Tasks {
		if task.Status != TaskRunning || len(task.Attempts) == 0 {
			continue
		}
		attemptID := task.Attempts[len(task.Attempts)-1].ID
		control, ok := service.running[attemptID]
		if !ok {
			continue
		}
		select {
		case control.steering <- record:
		default:
			return SupervisorRun{}, fmt.Errorf(
				"runtime work: live steering inbox is full",
			)
		}
	}
	return cloneRun(run), nil
}

func (service *Service) Cancel(
	ctx context.Context,
	actorID string,
	runID uuid.UUID,
) (SupervisorRun, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	run, err := service.loadRun(ctx, strings.TrimSpace(actorID), runID)
	if err != nil {
		return SupervisorRun{}, err
	}
	if terminalRun(run.Status) {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: active supervisor run not found",
		)
	}
	now := service.now().UTC()
	var cancellations []context.CancelFunc
	for index := range run.Tasks {
		task := &run.Tasks[index]
		if task.Status == TaskRunning && len(task.Attempts) > 0 {
			attempt := &task.Attempts[len(task.Attempts)-1]
			if control, ok := service.running[attempt.ID]; ok {
				cancellations = append(cancellations, control.cancel)
				delete(service.running, attempt.ID)
			}
			attempt.Status = TaskCancelled
			attempt.Error = "cancelled by parent supervisor"
			attempt.FinishedAt = &now
		}
		if task.Status != TaskCompleted {
			task.Status = TaskCancelled
			task.BlockingReason = ""
			task.UpdatedAt = now
		}
	}
	run.Status = RunCancelled
	run.Leases = nil
	run.Projection = run.Usage
	run.UpdatedAt = now
	run.FinishedAt = &now
	run.Revision++
	if err := service.saveRun(ctx, &run); err != nil {
		return SupervisorRun{}, err
	}
	for _, cancel := range cancellations {
		cancel()
	}
	return cloneRun(run), nil
}

func (service *Service) ReconcileRestart(
	ctx context.Context,
) ([]SupervisorRun, error) {
	service.mu.Lock()
	defer service.mu.Unlock()
	records, err := service.store.ListAllSupervisorRuns(ctx)
	if err != nil {
		return nil, err
	}
	var recovered []SupervisorRun
	for _, record := range records {
		var run SupervisorRun
		if err := json.Unmarshal(record.State, &run); err != nil {
			return nil, fmt.Errorf(
				"runtime work: decode supervisor run: %w", err,
			)
		}
		if err := validateRun(run); err != nil {
			return nil, err
		}
		if terminalRun(run.Status) {
			continue
		}
		changed, err := service.reconcileRunRestart(ctx, &run)
		if err != nil {
			return nil, err
		}
		if run.Status != RunOutcomeUnknown && run.Status != RunPaused {
			if err := service.scheduleLocked(
				context.WithoutCancel(ctx), &run,
			); err != nil {
				return nil, err
			}
		} else if changed {
			if err := service.saveRun(ctx, &run); err != nil {
				return nil, err
			}
		}
		if !changed {
			continue
		}
		recovered = append(recovered, cloneRun(run))
	}
	return recovered, nil
}

func (service *Service) reconcileRunRestart(
	ctx context.Context,
	run *SupervisorRun,
) (bool, error) {
	now := service.now().UTC()
	reconciliation := Reconciliation{At: now}
	changed := false
	for index := range run.Tasks {
		task := &run.Tasks[index]
		if task.Status != TaskRunning || len(task.Attempts) == 0 {
			continue
		}
		attempt := &task.Attempts[len(task.Attempts)-1]
		if _, local := service.running[attempt.ID]; local {
			continue
		}
		uncertain, _, err := service.reconcileEffects(
			ctx, attempt.ExternalEffects,
		)
		if err != nil {
			return false, fmt.Errorf(
				"runtime work: reconcile restart task %s: %w",
				task.Packet.ID, err,
			)
		}
		attempt.FinishedAt = &now
		run.Leases = releaseTaskLeases(run.Leases, task.Packet.ID)
		if uncertain {
			attempt.Status = TaskOutcomeUnknown
			attempt.Error = "external effect outcome requires reconciliation"
			task.Status = TaskOutcomeUnknown
			task.BlockingReason = attempt.Error
			run.Status = RunOutcomeUnknown
			reconciliation.UncertainTasks++
		} else {
			attempt.Status = TaskRetrying
			attempt.ResumeCheckpoint = true
			attempt.Error =
				"supervisor restarted before terminal worker evidence"
			task.Status = TaskReady
			task.Progress = 0
			task.BlockingReason = ""
			reconciliation.RecoveredTasks++
		}
		task.UpdatedAt = now
		changed = true
	}
	if !changed {
		return false, nil
	}
	if reconciliation.UncertainTasks == 0 {
		run.Status = RunQueued
	}
	run.Reconciliations = append(run.Reconciliations, reconciliation)
	run.UpdatedAt = now
	run.Revision++
	return true, nil
}

func (service *Service) compilePackets(
	run SupervisorRun,
	plan ParentPlan,
	specs []TaskSpec,
	available map[string]struct{},
) ([]TaskPacket, BudgetUsage, error) {
	known := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		id := strings.TrimSpace(spec.ID)
		if id == "" || len(id) > 128 ||
			invalidText(spec.Title) || invalidText(spec.Prompt) ||
			len(spec.Criteria) == 0 || len(spec.Criteria) > maxCriteria {
			return nil, BudgetUsage{}, fmt.Errorf(
				"runtime work: task identity, prompt, and criteria are required",
			)
		}
		if _, duplicate := known[id]; duplicate {
			return nil, BudgetUsage{}, fmt.Errorf(
				"runtime work: duplicate task ID %q", id,
			)
		}
		known[id] = struct{}{}
	}
	for _, spec := range specs {
		for _, dependency := range trimUnique(spec.DependsOn) {
			if dependency == strings.TrimSpace(spec.ID) {
				return nil, BudgetUsage{}, fmt.Errorf(
					"runtime work: task cannot depend on itself",
				)
			}
			if _, ok := known[dependency]; !ok {
				return nil, BudgetUsage{}, fmt.Errorf(
					"runtime work: unknown dependency %q", dependency,
				)
			}
		}
	}
	if err := validateDependencyGraph(specs); err != nil {
		return nil, BudgetUsage{}, err
	}

	planToolSet := make(map[string]struct{}, len(plan.Tools))
	for _, name := range trimUnique(plan.Tools) {
		planToolSet[name] = struct{}{}
	}
	base := specialistBudgetFromParent(plan.Budget, len(specs))
	var projection BudgetUsage
	packets := make([]TaskPacket, 0, len(specs))
	for _, spec := range specs {
		scope := normalizeScope(spec.Scope)
		if len(scope.EnvironmentKeys) != 0 {
			return nil, BudgetUsage{}, fmt.Errorf(
				"%w: specialist task %q cannot inherit environment keys",
				ErrAuthorityDenied, spec.ID,
			)
		}
		if !scopeSubset(scope, plan.Scope) {
			return nil, BudgetUsage{}, fmt.Errorf(
				"%w: task %q widens its parent scope",
				ErrAuthorityDenied, spec.ID,
			)
		}
		taskTools := trimUnique(spec.Tools)
		if len(taskTools) == 0 {
			return nil, BudgetUsage{}, fmt.Errorf(
				"%w: task %q has no tool subset",
				ErrAuthorityDenied, spec.ID,
			)
		}
		classes := make(map[string]string, len(taskTools))
		for _, name := range taskTools {
			if name == "spawn_subagents" {
				return nil, BudgetUsage{}, fmt.Errorf(
					"%w: specialist delegation cannot recurse",
					ErrAuthorityDenied,
				)
			}
			if _, ok := planToolSet[name]; !ok {
				return nil, BudgetUsage{}, fmt.Errorf(
					"%w: task %q widens tool %q",
					ErrAuthorityDenied, spec.ID, name,
				)
			}
			if _, ok := available[name]; !ok {
				return nil, BudgetUsage{}, fmt.Errorf(
					"%w: task tool %q is not manifest-bound",
					ErrAuthorityDenied, name,
				)
			}
			class, ok := service.metadata.SideEffectClass(name)
			if !ok {
				return nil, BudgetUsage{}, fmt.Errorf(
					"%w: task tool %q has no manifest effect class",
					ErrAuthorityDenied, name,
				)
			}
			if err := authorizeClass(scope, class); err != nil {
				return nil, BudgetUsage{}, fmt.Errorf(
					"%w: task %q tool %q: %v",
					ErrAuthorityDenied, spec.ID, name, err,
				)
			}
			classes[name] = class
		}
		budget := overlaySpecialistBudget(base, spec.Budget)
		estimate := spec.ProjectedUsage
		if !usageObserved(estimate) {
			estimate = usageFromBudget(budget)
		}
		if specialistBudgetExceeded(estimate, budget) {
			return nil, BudgetUsage{}, fmt.Errorf(
				"%w: task %q estimate exceeds its specialist budget",
				ErrBudgetProjection, spec.ID,
			)
		}
		projection = addUsage(projection, estimate)
		packet := TaskPacket{
			ID:           strings.TrimSpace(spec.ID),
			SupervisorID: run.ID, ActorID: run.ActorID,
			ConversationID: run.ConversationID,
			ParentPlanID:   run.ParentPlanID,
			Title:          boundedText(spec.Title), Prompt: boundedText(spec.Prompt),
			Criteria:   trimUnique(spec.Criteria),
			DependsOn:  trimUnique(spec.DependsOn),
			Specialist: spec.Specialist,
			Scope:      scope, Tools: taskTools, ToolClasses: classes,
			Budget: budget, ProjectedUsage: estimate,
		}
		if packet.Specialist == "" {
			packet.Specialist = inferSpecialist(spec)
		}
		if !validSpecialist(packet.Specialist) {
			return nil, BudgetUsage{}, fmt.Errorf(
				"runtime work: invalid specialist %q", packet.Specialist,
			)
		}
		raw, err := json.Marshal(packet)
		if err != nil {
			return nil, BudgetUsage{}, err
		}
		packet.Digest = digestPacket(raw)
		packets = append(packets, packet)
	}
	if budgetExceeded(projection, plan.Budget) {
		return nil, BudgetUsage{}, fmt.Errorf(
			"%w: accepted plan projects past the parent budget",
			ErrBudgetProjection,
		)
	}
	return packets, projection, nil
}

func (service *Service) scheduleLocked(
	ctx context.Context,
	run *SupervisorRun,
) error {
	if terminalRun(run.Status) || run.Status == RunPaused ||
		run.Status == RunOutcomeUnknown {
		return nil
	}
	active := 0
	var reserved BudgetUsage
	for _, task := range run.Tasks {
		if task.Status == TaskRunning {
			active++
			reserved = addUsage(reserved, task.Packet.ProjectedUsage)
		}
	}
	type launch struct {
		packet    TaskPacket
		attemptID uuid.UUID
		resumed   bool
		previous  Attempt
	}
	var launches []launch
	for index := range run.Tasks {
		if active >= run.Budget.MaxParallel ||
			budgetExceeded(run.Usage, run.Budget) {
			break
		}
		task := &run.Tasks[index]
		if task.Status != TaskReady ||
			!dependenciesComplete(run.Tasks, task.Packet) {
			continue
		}
		projected := addUsage(
			addUsage(run.Usage, reserved),
			task.Packet.ProjectedUsage,
		)
		if budgetExceeded(projected, run.Budget) {
			task.Status = TaskBlocked
			task.BlockingReason =
				"projected attempt cannot finish inside the remaining budget"
			run.Status = RunPaused
			break
		}
		leases, ok := acquireTaskLeases(
			run.Leases, task.Packet, service.now().UTC(),
		)
		if !ok {
			continue
		}
		run.Leases = leases
		now := service.now().UTC()
		item := launch{
			packet: task.Packet, attemptID: uuid.New(),
		}
		if len(task.Attempts) > 0 {
			last := &task.Attempts[len(task.Attempts)-1]
			if last.Status == TaskRetrying && last.ResumeCheckpoint {
				item.attemptID = last.ID
				item.resumed = true
				item.previous = *last
				last.Status = TaskRunning
				last.FinishedAt = nil
				last.Error = ""
			}
		}
		if !item.resumed {
			task.Attempts = append(task.Attempts, Attempt{
				ID: item.attemptID, Number: len(task.Attempts) + 1,
				Status: TaskRunning, StartedAt: now,
			})
		}
		task.Status, task.Progress, task.UpdatedAt = TaskRunning, 5, now
		run.Status, run.UpdatedAt = RunWorking, now
		launches = append(launches, item)
		reserved = addUsage(reserved, task.Packet.ProjectedUsage)
		active++
	}
	if active == 0 && run.Status == RunQueued {
		run.Status = RunWaiting
	}
	run.Revision++
	if err := service.saveRun(ctx, run); err != nil {
		for _, item := range launches {
			run.Leases = releaseTaskLeases(run.Leases, item.packet.ID)
			if task := findTask(run, item.packet.ID); task != nil {
				task.Status = TaskReady
				if item.resumed {
					task.Attempts[len(task.Attempts)-1] = item.previous
				} else {
					task.Attempts = task.Attempts[:len(task.Attempts)-1]
				}
			}
		}
		return err
	}
	for _, item := range launches {
		workerCtx, cancel := context.WithTimeout(
			context.Background(),
			time.Duration(item.packet.Budget.MaxWallSeconds)*time.Second,
		)
		steering := make(chan SteeringRecord, maxSteeringRecords)
		for _, record := range run.Steering {
			steering <- record
		}
		service.running[item.attemptID] = attemptControl{
			cancel: cancel, steering: steering,
		}
		go func(
			packet TaskPacket,
			attemptID uuid.UUID,
			guidance <-chan SteeringRecord,
		) {
			result, executeErr := service.executor.Execute(
				workerCtx, packet, attemptID, guidance,
			)
			cancel()
			service.finishAttempt(
				packet.ActorID, packet.SupervisorID, packet.ID,
				attemptID, result, executeErr,
			)
		}(item.packet, item.attemptID, steering)
	}
	return nil
}

func (service *Service) finishAttempt(
	actorID string,
	runID uuid.UUID,
	taskID string,
	attemptID uuid.UUID,
	result WorkerResult,
	executeErr error,
) {
	service.mu.Lock()
	defer service.mu.Unlock()
	delete(service.running, attemptID)
	ctx := context.Background()
	run, err := service.loadRun(ctx, actorID, runID)
	if err != nil {
		return
	}
	task := findTask(&run, taskID)
	if task == nil || len(task.Attempts) == 0 {
		return
	}
	attempt := &task.Attempts[len(task.Attempts)-1]
	if attempt.ID != attemptID || attempt.Status != TaskRunning {
		return
	}
	now := service.now().UTC()
	attempt.FinishedAt = &now
	attempt.WorkerID = strings.TrimSpace(result.WorkerID)
	attempt.Summary = boundedText(result.Summary)
	attempt.Usage = result.Usage
	attempt.Evidence = append([]EvidenceRef(nil), result.Evidence...)
	attempt.Findings = append([]Finding(nil), result.Findings...)
	attempt.ExternalEffects = append(
		[]ExternalEffect(nil), result.ExternalEffects...,
	)
	attempt.ResumeCheckpoint = false
	task.Progress = clampProgress(result.Progress)
	run.Usage = addUsage(run.Usage, result.Usage)
	run.Leases = releaseTaskLeases(run.Leases, taskID)

	uncertain, retrySafe, reconcileErr := service.reconcileEffects(
		ctx, attempt.ExternalEffects,
	)
	if reconcileErr != nil && executeErr == nil {
		executeErr = reconcileErr
	}
	taskBudgetExceeded := specialistBudgetExceeded(
		accumulatedTaskUsage(*task), task.Packet.Budget,
	)
	accountingMissing := result.Status == TaskCompleted &&
		(task.Packet.Budget.MaxModelMicrocents > 0 &&
			!result.Usage.ModelCostKnown ||
			task.Packet.Budget.MaxProviderMicrocents > 0 &&
				!result.Usage.ProviderSpendKnown)
	verified := false
	if !taskBudgetExceeded && !accountingMissing && executeErr == nil &&
		result.Status == TaskCompleted && !uncertain {
		verified, err = service.hasVerifiedEvidence(task.Packet, result)
		if err != nil {
			executeErr = err
		}
	}
	switch {
	case taskBudgetExceeded:
		attempt.Status = TaskBlocked
		attempt.Error = "specialist budget exhausted before verified completion"
		task.Status = TaskBlocked
		task.BlockingReason = attempt.Error
		run.Status = RunPaused
	case accountingMissing:
		attempt.Status = TaskBlocked
		attempt.Error = "authoritative model or provider spend is unavailable"
		task.Status = TaskBlocked
		task.BlockingReason = attempt.Error
		run.Status = RunPaused
	case uncertain:
		attempt.Status = TaskOutcomeUnknown
		attempt.Error = "external effect outcome requires reconciliation"
		task.Status = TaskOutcomeUnknown
		task.BlockingReason = attempt.Error
		run.Status = RunOutcomeUnknown
	case executeErr != nil &&
		accumulatedTaskUsage(*task).Retries <
			task.Packet.Budget.MaxRetries &&
		(retrySafe || len(result.ExternalEffects) == 0) &&
		!budgetExceeded(run.Usage, run.Budget):
		attempt.Status = TaskRetrying
		attempt.Error = boundedText(executeErr.Error())
		task.Status, task.Progress, task.BlockingReason =
			TaskReady, 0, ""
		attempt.Usage.Retries++
		run.Usage.Retries++
	case executeErr != nil:
		attempt.Status = TaskBlocked
		attempt.Error = boundedText(executeErr.Error())
		task.Status, task.BlockingReason =
			TaskBlocked, attempt.Error
	case result.Status == TaskCompleted && verified:
		attempt.Status = TaskCompleted
		task.Status, task.Progress, task.BlockingReason =
			TaskCompleted, 100, ""
	case result.Status == TaskCompleted:
		attempt.Status = TaskWaitingEvidence
		attempt.Error = ErrEvidenceRequired.Error()
		task.Status = TaskWaitingEvidence
		task.BlockingReason = attempt.Error
		task.Progress = min(task.Progress, 99)
	default:
		attempt.Status = TaskWaitingEvidence
		attempt.Error = "specialist result lacks terminal evidence"
		task.Status = TaskWaitingEvidence
		task.BlockingReason = attempt.Error
		task.Progress = min(task.Progress, 99)
	}
	task.UpdatedAt, run.UpdatedAt = now, now
	refreshReady(&run)
	finalizeRun(&run, now)
	run.Revision++
	if err := service.saveRun(ctx, &run); err != nil {
		return
	}
	if !terminalRun(run.Status) && run.Status != RunPaused &&
		run.Status != RunOutcomeUnknown {
		_ = service.scheduleLocked(ctx, &run)
	}
}

func (service *Service) hasVerifiedEvidence(
	packet TaskPacket,
	result WorkerResult,
) (bool, error) {
	if len(packet.Criteria) == 0 || len(result.Evidence) == 0 {
		return false, nil
	}
	covered := make(map[string]struct{}, len(packet.Criteria))
	allowed := make(map[string]struct{}, len(packet.Criteria))
	for _, criterion := range packet.Criteria {
		allowed[criterion] = struct{}{}
	}
	for _, reference := range result.Evidence {
		if _, ok := allowed[reference.Criterion]; !ok {
			continue
		}
		payload, err := service.verifier.VerifyToolEventCitation(
			reference.Citation,
		)
		if err != nil {
			return false, fmt.Errorf(
				"%w: %v", ErrEvidenceRequired, err,
			)
		}
		if payload.SubgoalID != reference.Criterion ||
			payload.Error != "" ||
			payload.MatchVerdict != cortex.ToolMatchMatched {
			continue
		}
		covered[reference.Criterion] = struct{}{}
	}
	for _, criterion := range packet.Criteria {
		if _, ok := covered[criterion]; !ok {
			return false, nil
		}
	}
	return true, nil
}

func (service *Service) reconcileEffects(
	ctx context.Context,
	effects []ExternalEffect,
) (uncertain bool, retrySafe bool, err error) {
	for index := range effects {
		effect := &effects[index]
		switch effect.State {
		case "completed":
			continue
		case "retry_safe":
			retrySafe = true
			continue
		}
		if service.reconciler == nil {
			return true, retrySafe, nil
		}
		reconciled, reconcileErr := service.reconciler.ReconcileEffect(
			ctx, effect.IdempotencyKey,
		)
		if reconcileErr != nil {
			return false, retrySafe, reconcileErr
		}
		switch reconciled.Status {
		case loop.ReconcileCompleted:
			effect.State = "completed"
		case loop.ReconcileRetrySafe:
			effect.State = "retry_safe"
			retrySafe = true
		default:
			effect.State = "outcome_unknown"
			uncertain = true
		}
	}
	return uncertain, retrySafe, nil
}

func (service *Service) saveRun(
	ctx context.Context,
	run *SupervisorRun,
) error {
	if err := validateRun(*run); err != nil {
		return err
	}
	raw, err := json.Marshal(run)
	if err != nil {
		return fmt.Errorf("runtime work: encode supervisor run: %w", err)
	}
	return service.store.SaveSupervisorRun(
		ctx,
		turnstate.SupervisorRecord{
			RunID: run.ID.String(), ActorID: run.ActorID,
			Status: string(run.Status), State: raw,
		},
	)
}

func (service *Service) loadRun(
	ctx context.Context,
	actorID string,
	runID uuid.UUID,
) (SupervisorRun, error) {
	if runID == uuid.Nil {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: supervisor run ID is required",
		)
	}
	record, err := service.store.LoadSupervisorRun(ctx, runID.String())
	if err != nil {
		return SupervisorRun{}, err
	}
	if strings.TrimSpace(actorID) != record.ActorID {
		return SupervisorRun{}, fmt.Errorf(
			"%w: supervisor actor mismatch", ErrAuthorityDenied,
		)
	}
	var run SupervisorRun
	if err := json.Unmarshal(record.State, &run); err != nil {
		return SupervisorRun{}, fmt.Errorf(
			"runtime work: decode supervisor run: %w", err,
		)
	}
	if err := validateRun(run); err != nil {
		return SupervisorRun{}, err
	}
	return run, nil
}

func normalizeSupervisorBudget(
	input SupervisorBudget,
) (SupervisorBudget, error) {
	if input.MaxParallel < 1 ||
		input.MaxParallel > MaxParallelSpecialists ||
		input.MaxTokens < 1 || input.MaxToolCalls < 1 ||
		input.MaxWallSeconds < 1 || input.MaxProcesses < 1 ||
		input.MaxModelMicrocents < 0 ||
		input.MaxProviderMicrocents < 0 ||
		input.MaxStorageBytes < 0 || input.MaxNetworkBytes < 0 ||
		input.MaxRetries < 0 || input.MaxRetries > 10 {
		return SupervisorBudget{}, fmt.Errorf(
			"runtime work: supervisor budget is outside bounds",
		)
	}
	return input, nil
}

func specialistBudgetFromParent(
	parent SupervisorBudget,
	taskCount int,
) SpecialistBudget {
	divisor := int64(max(taskCount, 1))
	return SpecialistBudget{
		MaxTokens:             max(parent.MaxTokens/divisor, 1),
		MaxModelMicrocents:    parent.MaxModelMicrocents / divisor,
		MaxProviderMicrocents: parent.MaxProviderMicrocents / divisor,
		MaxToolCalls:          max(parent.MaxToolCalls/taskCount, 1),
		MaxWallSeconds:        max(parent.MaxWallSeconds/taskCount, 1),
		MaxProcesses:          max(parent.MaxProcesses/taskCount, 1),
		MaxStorageBytes:       parent.MaxStorageBytes / divisor,
		MaxNetworkBytes:       parent.MaxNetworkBytes / divisor,
		MaxRetries:            parent.MaxRetries,
	}
}

func overlaySpecialistBudget(
	base SpecialistBudget,
	override SpecialistBudget,
) SpecialistBudget {
	if override.MaxTokens > 0 {
		base.MaxTokens = min(base.MaxTokens, override.MaxTokens)
	}
	if override.MaxModelMicrocents > 0 {
		base.MaxModelMicrocents = min(
			base.MaxModelMicrocents, override.MaxModelMicrocents,
		)
	}
	if override.MaxProviderMicrocents > 0 {
		base.MaxProviderMicrocents = min(
			base.MaxProviderMicrocents, override.MaxProviderMicrocents,
		)
	}
	if override.MaxToolCalls > 0 {
		base.MaxToolCalls = min(base.MaxToolCalls, override.MaxToolCalls)
	}
	if override.MaxWallSeconds > 0 {
		base.MaxWallSeconds = min(
			base.MaxWallSeconds, override.MaxWallSeconds,
		)
	}
	if override.MaxProcesses > 0 {
		base.MaxProcesses = min(base.MaxProcesses, override.MaxProcesses)
	}
	if override.MaxStorageBytes > 0 {
		base.MaxStorageBytes = min(
			base.MaxStorageBytes, override.MaxStorageBytes,
		)
	}
	if override.MaxNetworkBytes > 0 {
		base.MaxNetworkBytes = min(
			base.MaxNetworkBytes, override.MaxNetworkBytes,
		)
	}
	if override.MaxRetries > 0 {
		base.MaxRetries = min(base.MaxRetries, override.MaxRetries)
	}
	return base
}

func usageFromBudget(budget SpecialistBudget) BudgetUsage {
	return BudgetUsage{
		Tokens:             budget.MaxTokens,
		ModelMicrocents:    budget.MaxModelMicrocents,
		ModelCostKnown:     true,
		ProviderMicrocents: budget.MaxProviderMicrocents,
		ProviderSpendKnown: true,
		ToolCalls:          budget.MaxToolCalls,
		WallSeconds:        budget.MaxWallSeconds,
		Processes:          budget.MaxProcesses,
		StorageBytes:       budget.MaxStorageBytes,
		NetworkBytes:       budget.MaxNetworkBytes,
	}
}

func addUsage(left BudgetUsage, right BudgetUsage) BudgetUsage {
	result := BudgetUsage{
		Tokens:          left.Tokens + right.Tokens,
		ModelMicrocents: left.ModelMicrocents + right.ModelMicrocents,
		ProviderMicrocents: left.ProviderMicrocents +
			right.ProviderMicrocents,
		ToolCalls:    left.ToolCalls + right.ToolCalls,
		WallSeconds:  left.WallSeconds + right.WallSeconds,
		Processes:    left.Processes + right.Processes,
		StorageBytes: left.StorageBytes + right.StorageBytes,
		NetworkBytes: left.NetworkBytes + right.NetworkBytes,
		Retries:      left.Retries + right.Retries,
	}
	switch {
	case !usageObserved(left):
		result.ModelCostKnown = right.ModelCostKnown
		result.ProviderSpendKnown = right.ProviderSpendKnown
	case !usageObserved(right):
		result.ModelCostKnown = left.ModelCostKnown
		result.ProviderSpendKnown = left.ProviderSpendKnown
	default:
		result.ModelCostKnown = left.ModelCostKnown &&
			right.ModelCostKnown
		result.ProviderSpendKnown = left.ProviderSpendKnown &&
			right.ProviderSpendKnown
	}
	return result
}

func usageObserved(usage BudgetUsage) bool {
	return usage.Tokens != 0 || usage.ModelMicrocents != 0 ||
		usage.ModelCostKnown || usage.ProviderMicrocents != 0 ||
		usage.ProviderSpendKnown || usage.ToolCalls != 0 ||
		usage.WallSeconds != 0 || usage.Processes != 0 ||
		usage.StorageBytes != 0 || usage.NetworkBytes != 0 ||
		usage.Retries != 0
}

func specialistBudgetExceeded(
	usage BudgetUsage,
	budget SpecialistBudget,
) bool {
	return usage.Tokens > budget.MaxTokens ||
		usage.ModelMicrocents > budget.MaxModelMicrocents ||
		usage.ProviderMicrocents > budget.MaxProviderMicrocents ||
		usage.ToolCalls > budget.MaxToolCalls ||
		usage.WallSeconds > budget.MaxWallSeconds ||
		usage.Processes > budget.MaxProcesses ||
		usage.StorageBytes > budget.MaxStorageBytes ||
		usage.NetworkBytes > budget.MaxNetworkBytes ||
		usage.Retries > budget.MaxRetries
}

func accumulatedTaskUsage(task SpecialistTask) BudgetUsage {
	var usage BudgetUsage
	for _, attempt := range task.Attempts {
		usage = addUsage(usage, attempt.Usage)
	}
	usage.Retries = max(len(task.Attempts)-1, 0)
	return usage
}

func budgetExceeded(
	usage BudgetUsage,
	budget SupervisorBudget,
) bool {
	return specialistBudgetExceeded(usage, budget.SpecialistBudget)
}

func inferSpecialist(spec TaskSpec) Specialist {
	value := strings.ToLower(
		spec.ID + " " + spec.Title + " " +
			strings.Join(spec.Criteria, " "),
	)
	candidates := []struct {
		needles []string
		role    Specialist
	}{
		{[]string{"security", "auth", "secret"}, SpecialistSecurity},
		{[]string{"frontend", " ui", "browser", "accessib"}, SpecialistFrontend},
		{[]string{"database", " data", "migration", "schema"}, SpecialistData},
		{[]string{"performance", "latency", "benchmark"}, SpecialistPerformance},
		{[]string{"deploy", "operation", "release", "runtime"}, SpecialistOperations},
		{[]string{"test", "verify", "acceptance"}, SpecialistTest},
		{[]string{"review", "audit"}, SpecialistReview},
		{[]string{"research", "explore"}, SpecialistExploration},
		{[]string{"discover", "inspect", "inventory"}, SpecialistDiscovery},
	}
	for _, candidate := range candidates {
		for _, needle := range candidate.needles {
			if strings.Contains(value, needle) {
				return candidate.role
			}
		}
	}
	return SpecialistImplementation
}

func validSpecialist(value Specialist) bool {
	switch value {
	case SpecialistDiscovery, SpecialistExploration,
		SpecialistImplementation, SpecialistTest, SpecialistSecurity,
		SpecialistData, SpecialistFrontend, SpecialistPerformance,
		SpecialistOperations, SpecialistReview:
		return true
	default:
		return false
	}
}

func validateDependencyGraph(specs []TaskSpec) error {
	dependencies := make(map[string][]string, len(specs))
	for _, spec := range specs {
		dependencies[strings.TrimSpace(spec.ID)] =
			trimUnique(spec.DependsOn)
	}
	visiting := make(map[string]bool, len(specs))
	visited := make(map[string]bool, len(specs))
	var visit func(string) error
	visit = func(id string) error {
		if visiting[id] {
			return fmt.Errorf("runtime work: dependency cycle at %q", id)
		}
		if visited[id] {
			return nil
		}
		visiting[id] = true
		for _, dependency := range dependencies[id] {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true
		return nil
	}
	for id := range dependencies {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}

func dependenciesComplete(
	tasks []SpecialistTask,
	packet TaskPacket,
) bool {
	for _, dependency := range packet.DependsOn {
		completed := false
		for _, task := range tasks {
			if task.Packet.ID == dependency &&
				task.Status == TaskCompleted {
				completed = true
				break
			}
		}
		if !completed {
			return false
		}
	}
	return true
}

func refreshReady(run *SupervisorRun) {
	for index := range run.Tasks {
		task := &run.Tasks[index]
		if task.Status == TaskPending &&
			dependenciesComplete(run.Tasks, task.Packet) {
			task.Status = TaskReady
		}
	}
}

func finalizeRun(run *SupervisorRun, now time.Time) {
	completed, active, blocked, uncertain := 0, 0, 0, 0
	var findings []Finding
	for _, task := range run.Tasks {
		switch task.Status {
		case TaskCompleted:
			completed++
		case TaskRunning, TaskReady, TaskRetrying, TaskPending:
			active++
		case TaskOutcomeUnknown:
			uncertain++
		case TaskBlocked, TaskWaitingEvidence:
			blocked++
		}
		for _, attempt := range task.Attempts {
			findings = append(findings, attempt.Findings...)
		}
	}
	sort.SliceStable(findings, func(left int, right int) bool {
		if findings[left].Kind != findings[right].Kind {
			return findings[left].Kind < findings[right].Kind
		}
		return findings[left].Summary < findings[right].Summary
	})
	run.Synthesis = findings
	switch {
	case uncertain > 0:
		run.Status = RunOutcomeUnknown
	case budgetExceeded(run.Usage, run.Budget):
		run.Status = RunPaused
	case completed == len(run.Tasks):
		run.Status, run.FinishedAt = RunCompleted, &now
	case active > 0:
		run.Status = RunWorking
	case blocked > 0:
		run.Status = RunBlocked
	default:
		run.Status = RunWaiting
	}
}

func acquireTaskLeases(
	current []ScopeLease,
	packet TaskPacket,
	now time.Time,
) ([]ScopeLease, bool) {
	var requested []ScopeLease
	add := func(resource string, mode string) {
		requested = append(requested, ScopeLease{
			ID: uuid.New(), TaskID: packet.ID,
			Resource: resource, Mode: mode,
			ExpiresAt: now.Add(
				time.Duration(packet.Budget.MaxWallSeconds) * time.Second,
			),
		})
	}
	for _, resource := range packet.Scope.ReadFiles {
		add("file:"+resource, "read")
	}
	for _, resource := range packet.Scope.WriteFiles {
		add("file:"+resource, "write")
	}
	for _, resource := range packet.Scope.Services {
		add("service:"+resource, "write")
	}
	for _, resource := range packet.Scope.NetworkHosts {
		add("network:"+resource, "write")
	}
	for _, wanted := range requested {
		for _, held := range current {
			if held.ExpiresAt.After(now) &&
				held.TaskID != packet.ID &&
				leaseResourcesConflict(held.Resource, wanted.Resource) &&
				(held.Mode == "write" || wanted.Mode == "write") {
				return current, false
			}
		}
	}
	return append(current, requested...), true
}

func leaseResourcesConflict(left string, right string) bool {
	if left == right {
		return true
	}
	if !strings.HasPrefix(left, "file:") ||
		!strings.HasPrefix(right, "file:") {
		return false
	}
	left = strings.Trim(strings.TrimPrefix(left, "file:"), "/")
	right = strings.Trim(strings.TrimPrefix(right, "file:"), "/")
	return strings.HasPrefix(left, right+"/") ||
		strings.HasPrefix(right, left+"/")
}

func releaseTaskLeases(
	leases []ScopeLease,
	taskID string,
) []ScopeLease {
	result := leases[:0]
	for _, lease := range leases {
		if lease.TaskID != taskID {
			result = append(result, lease)
		}
	}
	return result
}

func findTask(run *SupervisorRun, taskID string) *SpecialistTask {
	for index := range run.Tasks {
		if run.Tasks[index].Packet.ID == taskID {
			return &run.Tasks[index]
		}
	}
	return nil
}

func clampProgress(value int) int {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

func invalidText(value string) bool {
	value = strings.TrimSpace(value)
	return value == "" || len(value) > maxTextBytes
}

func digestPacket(raw []byte) [32]byte {
	return sha256.Sum256(raw)
}
