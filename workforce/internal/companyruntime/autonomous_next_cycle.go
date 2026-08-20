package companyruntime

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"centra/workforce/internal/autonomouscompany"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/learning"
	"centra/workforce/internal/portfolio"
)

type AutonomousNextCycleExecutor struct {
	coordinator *Coordinator
}

func NewAutonomousNextCycleExecutor(
	coordinator *Coordinator,
) (*AutonomousNextCycleExecutor, error) {
	if coordinator == nil || coordinator.cycles == nil {
		return nil, fmt.Errorf("company runtime: autonomous next-cycle dispatcher is required")
	}
	return &AutonomousNextCycleExecutor{coordinator: coordinator}, nil
}

func (executor *AutonomousNextCycleExecutor) DispatchNextCycle(
	ctx context.Context,
	snapshot autonomouscompany.NextCycleSnapshot,
) (autonomouscompany.NextCycleUpdate, error) {
	if err := executor.validateSnapshot(snapshot); err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	if binding, err := executor.dispatchBinding(ctx, snapshot); err == nil {
		return autonomouscompany.NewNextCycleUpdate(
			snapshot,
			autonomouscompany.NextCycleRunning,
			[]autonomouscompany.EvidenceBinding{binding},
			nil,
			binding.ObservedAt,
		), nil
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return autonomouscompany.NextCycleUpdate{}, err
	}

	configuration, err := executor.coordinator.store.LoadCurrent(ctx)
	if err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	plan, err := executor.companyCyclePlan(snapshot, configuration)
	if err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	if err := executor.ensureCycleRun(ctx, plan); err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	if _, err := executor.coordinator.DispatchCycle(ctx, plan); err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	binding, err := executor.dispatchBinding(ctx, snapshot)
	if err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	return autonomouscompany.NewNextCycleUpdate(
		snapshot,
		autonomouscompany.NextCycleRunning,
		[]autonomouscompany.EvidenceBinding{binding},
		nil,
		binding.ObservedAt,
	), nil
}

func (executor *AutonomousNextCycleExecutor) ReconcileNextCycle(
	ctx context.Context,
	snapshot autonomouscompany.NextCycleSnapshot,
) (autonomouscompany.NextCycleUpdate, error) {
	if err := executor.validateSnapshot(snapshot); err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	if err := executor.coordinator.cycles.Reconcile(ctx); err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	dispatch, state, updatedAt, err := executor.cycleState(ctx, snapshot)
	if err != nil {
		return autonomouscompany.NextCycleUpdate{}, err
	}
	switch state {
	case "dispatched":
		return autonomouscompany.NewNextCycleUpdate(
			snapshot,
			autonomouscompany.NextCycleRunning,
			[]autonomouscompany.EvidenceBinding{dispatch},
			nil,
			dispatch.ObservedAt,
		), nil
	case "failed":
		return autonomouscompany.NewNextCycleUpdate(
			snapshot,
			autonomouscompany.NextCycleFailed,
			[]autonomouscompany.EvidenceBinding{dispatch},
			[]string{"company_cycle_failed"},
			updatedAt,
		), nil
	case "completed":
		completion, err := executor.completionBinding(ctx, snapshot)
		if errors.Is(err, pgx.ErrNoRows) {
			return autonomouscompany.NewNextCycleUpdate(
				snapshot,
				autonomouscompany.NextCycleUncertain,
				[]autonomouscompany.EvidenceBinding{dispatch},
				[]string{"completion_receipt_missing"},
				updatedAt,
			), nil
		}
		if err != nil {
			return autonomouscompany.NextCycleUpdate{}, err
		}
		evidence := []autonomouscompany.EvidenceBinding{completion, dispatch}
		slices.SortFunc(evidence, compareAutonomousEvidence)
		return autonomouscompany.NewNextCycleUpdate(
			snapshot,
			autonomouscompany.NextCyclePassed,
			evidence,
			nil,
			completion.ObservedAt,
		), nil
	default:
		return autonomouscompany.NextCycleUpdate{}, fmt.Errorf(
			"company runtime: autonomous next-cycle state %q is not reconcilable",
			state,
		)
	}
}

func (executor *AutonomousNextCycleExecutor) validateSnapshot(
	snapshot autonomouscompany.NextCycleSnapshot,
) error {
	if executor == nil || executor.coordinator == nil || executor.coordinator.cycles == nil ||
		snapshot.Plan.Validate() != nil || snapshot.CanonicalHash.Validate() != nil ||
		snapshot.Plan.ContentHash != snapshot.CanonicalHash ||
		snapshot.Plan.OrganizationID != executor.coordinator.store.organizationID ||
		snapshot.Plan.SelectedAction == learning.ActionHumanReview {
		return fmt.Errorf("company runtime: autonomous next-cycle snapshot is invalid")
	}
	return nil
}

func (executor *AutonomousNextCycleExecutor) companyCyclePlan(
	snapshot autonomouscompany.NextCycleSnapshot,
	configuration StartConfiguration,
) (portfolio.CyclePlan, error) {
	kind := portfolio.CadenceLearning
	if snapshot.Plan.SelectedAction == learning.ActionDiscover {
		kind = portfolio.CadenceDiscovery
	}
	departments, capabilities := autonomousCycleCoverage(kind)
	capabilities = append(
		capabilities,
		"autonomous.next-cycle."+strings.ToLower(string(snapshot.Plan.SelectedAction)),
	)
	slices.Sort(capabilities)
	capabilities = slices.Compact(capabilities)
	plan := portfolio.CyclePlan{
		SchemaVersion:        "workforce.company-cycle.v1",
		ID:                   snapshot.Plan.ID,
		OrganizationID:       snapshot.Plan.OrganizationID,
		Kind:                 kind,
		Departments:          departments,
		RequiredCapabilities: capabilities,
		IndependentAudit:     true,
		DueAt:                snapshot.Plan.ClaimedAt,
		NextAt:               configuration.ExpiresAt,
	}
	if err := plan.Validate(); err != nil {
		return portfolio.CyclePlan{}, err
	}
	return plan, nil
}

func (executor *AutonomousNextCycleExecutor) ensureCycleRun(
	ctx context.Context,
	plan portfolio.CyclePlan,
) error {
	now := executor.coordinator.now()
	if !validUTC(now) || !plan.NextAt.After(now) {
		return fmt.Errorf("company runtime: autonomous next-cycle authority expired")
	}
	departments := make([]string, len(plan.Departments))
	for index := range plan.Departments {
		departments[index] = string(plan.Departments[index])
	}
	command, err := executor.coordinator.pool.Exec(ctx, `
		INSERT INTO workforce_company_cycle_runs (
			tenant_id,organization_id,cycle_id,cadence_kind,due_at,next_at,
			departments,required_capabilities,independent_audit,state,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'planned',$10,$10)
		ON CONFLICT (tenant_id,organization_id,cycle_id) DO NOTHING
	`, executor.coordinator.store.tenantID, plan.OrganizationID, plan.ID, plan.Kind,
		plan.DueAt, plan.NextAt, departments, plan.RequiredCapabilities,
		plan.IndependentAudit, now)
	if err != nil {
		return fmt.Errorf("company runtime: persist autonomous next cycle: %w", err)
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var existing portfolio.CyclePlan
	var existingDepartments []string
	if err := executor.coordinator.pool.QueryRow(ctx, `
		SELECT cycle_id,cadence_kind,due_at,next_at,departments,
		       required_capabilities,independent_audit
		FROM workforce_company_cycle_runs
		WHERE tenant_id=$1 AND organization_id=$2 AND cycle_id=$3
	`, executor.coordinator.store.tenantID, plan.OrganizationID, plan.ID).Scan(
		&existing.ID, &existing.Kind, &existing.DueAt, &existing.NextAt,
		&existingDepartments, &existing.RequiredCapabilities, &existing.IndependentAudit,
	); err != nil {
		return err
	}
	existing.SchemaVersion = plan.SchemaVersion
	existing.OrganizationID = plan.OrganizationID
	existing.Departments = make([]contracts.DepartmentKind, len(existingDepartments))
	for index := range existingDepartments {
		existing.Departments[index] = contracts.DepartmentKind(existingDepartments[index])
	}
	if existing.Validate() != nil || existing.ID != plan.ID || existing.Kind != plan.Kind ||
		!existing.DueAt.Equal(plan.DueAt) || !existing.NextAt.Equal(plan.NextAt) ||
		!slices.Equal(existing.Departments, plan.Departments) ||
		!slices.Equal(existing.RequiredCapabilities, plan.RequiredCapabilities) ||
		existing.IndependentAudit != plan.IndependentAudit {
		return fmt.Errorf("company runtime: autonomous next-cycle dispatch conflict")
	}
	return nil
}

func (executor *AutonomousNextCycleExecutor) dispatchBinding(
	ctx context.Context,
	snapshot autonomouscompany.NextCycleSnapshot,
) (autonomouscompany.EvidenceBinding, error) {
	var canonicalHash string
	var authority string
	var state string
	var observedAt time.Time
	var freshUntil time.Time
	err := executor.coordinator.pool.QueryRow(ctx, `
		SELECT orders.canonical_hash,orders.controller_key_id,runs.state,
		       dispatch.dispatched_at,orders.deadline
		FROM workforce_company_cycle_runs runs
		JOIN workforce_company_cycle_orders orders
		  ON orders.tenant_id=runs.tenant_id
		 AND orders.organization_id=runs.organization_id
		 AND orders.cycle_id=runs.cycle_id
		JOIN workforce_company_cycle_dispatches dispatch
		  ON dispatch.tenant_id=runs.tenant_id
		 AND dispatch.organization_id=runs.organization_id
		 AND dispatch.cycle_id=runs.cycle_id
		WHERE runs.tenant_id=$1 AND runs.organization_id=$2 AND runs.cycle_id=$3
	`, executor.coordinator.store.tenantID, snapshot.Plan.OrganizationID,
		snapshot.Plan.ID).Scan(
		&canonicalHash, &authority, &state, &observedAt, &freshUntil,
	)
	if err != nil {
		return autonomouscompany.EvidenceBinding{}, err
	}
	if state != "dispatched" && state != "completed" && state != "failed" {
		return autonomouscompany.EvidenceBinding{}, fmt.Errorf(
			"company runtime: autonomous next cycle has no dispatch receipt",
		)
	}
	binding := autonomouscompany.EvidenceBinding{
		SchemaVersion:  autonomouscompany.EvidenceSchemaVersion,
		Kind:           autonomouscompany.EvidenceNextCycleDispatchReceipt,
		OrganizationID: snapshot.Plan.OrganizationID,
		InitiativeID:   snapshot.Plan.InitiativeID,
		RecordID:       snapshot.Plan.ID,
		RecordVersion:  1,
		RecordHash: contracts.ContentHash{
			Algorithm: "sha256",
			Digest:    canonicalHash,
		},
		Authority:      authority,
		SourceState:    "dispatched",
		Validity:       contracts.ValidityActive,
		Reconciliation: autonomouscompany.ReconciliationNotApplicable,
		ObservedAt:     observedAt,
		FreshUntil:     freshUntil,
	}
	if err := binding.Validate(); err != nil {
		return autonomouscompany.EvidenceBinding{}, err
	}
	return binding, nil
}

func (executor *AutonomousNextCycleExecutor) cycleState(
	ctx context.Context,
	snapshot autonomouscompany.NextCycleSnapshot,
) (autonomouscompany.EvidenceBinding, string, time.Time, error) {
	dispatch, err := executor.dispatchBinding(ctx, snapshot)
	if err != nil {
		return autonomouscompany.EvidenceBinding{}, "", time.Time{}, err
	}
	var state string
	var updatedAt time.Time
	if err := executor.coordinator.pool.QueryRow(ctx, `
		SELECT state,updated_at FROM workforce_company_cycle_runs
		WHERE tenant_id=$1 AND organization_id=$2 AND cycle_id=$3
	`, executor.coordinator.store.tenantID, snapshot.Plan.OrganizationID,
		snapshot.Plan.ID).Scan(&state, &updatedAt); err != nil {
		return autonomouscompany.EvidenceBinding{}, "", time.Time{}, err
	}
	return dispatch, state, updatedAt, nil
}

func (executor *AutonomousNextCycleExecutor) completionBinding(
	ctx context.Context,
	snapshot autonomouscompany.NextCycleSnapshot,
) (autonomouscompany.EvidenceBinding, error) {
	var receiptHash string
	var authority string
	var disposition string
	var observedAt time.Time
	var freshUntil time.Time
	err := executor.coordinator.pool.QueryRow(ctx, `
		SELECT receipt.content_hash,wake.seat_id,receipt.disposition,
		       receipt.created_at,orders.deadline
		FROM workforce_company_cycle_runs runs
		JOIN workforce_company_cycle_dispatches dispatch
		  ON dispatch.tenant_id=runs.tenant_id
		 AND dispatch.organization_id=runs.organization_id
		 AND dispatch.cycle_id=runs.cycle_id
		JOIN workforce_company_cycle_orders orders
		  ON orders.tenant_id=runs.tenant_id
		 AND orders.organization_id=runs.organization_id
		 AND orders.cycle_id=runs.cycle_id
		JOIN workforce_work_nodes goal
		  ON goal.tenant_id=dispatch.tenant_id
		 AND goal.organization_id=dispatch.organization_id
		 AND goal.node_id=dispatch.goal_id
		JOIN workforce_scheduled_wakes wake
		  ON wake.tenant_id=dispatch.tenant_id
		 AND wake.organization_id=dispatch.organization_id
		 AND wake.wake_id=dispatch.wake_id
		JOIN workforce_execution_receipts receipt
		  ON receipt.tenant_id=dispatch.tenant_id
		 AND receipt.organization_id=dispatch.organization_id
		 AND receipt.wake_id=dispatch.wake_id
		WHERE runs.tenant_id=$1 AND runs.organization_id=$2 AND runs.cycle_id=$3
		  AND runs.state='completed' AND goal.state='completed'
		  AND receipt.disposition='goal_completed'
	`, executor.coordinator.store.tenantID, snapshot.Plan.OrganizationID,
		snapshot.Plan.ID).Scan(
		&receiptHash, &authority, &disposition, &observedAt, &freshUntil,
	)
	if err != nil {
		return autonomouscompany.EvidenceBinding{}, err
	}
	binding := autonomouscompany.EvidenceBinding{
		SchemaVersion:  autonomouscompany.EvidenceSchemaVersion,
		Kind:           autonomouscompany.EvidenceNextCycleCompletionReceipt,
		OrganizationID: snapshot.Plan.OrganizationID,
		InitiativeID:   snapshot.Plan.InitiativeID,
		RecordID:       snapshot.Plan.ID,
		RecordVersion:  1,
		RecordHash: contracts.ContentHash{
			Algorithm: "sha256",
			Digest:    receiptHash,
		},
		Authority:      authority,
		SourceState:    disposition,
		Validity:       contracts.ValidityActive,
		Reconciliation: autonomouscompany.ReconciliationNotApplicable,
		ObservedAt:     observedAt,
		FreshUntil:     freshUntil,
	}
	if err := binding.Validate(); err != nil {
		return autonomouscompany.EvidenceBinding{}, err
	}
	return binding, nil
}

func compareAutonomousEvidence(left, right autonomouscompany.EvidenceBinding) int {
	leftKey := string(left.Kind) + "\x00" + left.RecordID + "\x00" +
		fmt.Sprintf("%020d", left.RecordVersion)
	rightKey := string(right.Kind) + "\x00" + right.RecordID + "\x00" +
		fmt.Sprintf("%020d", right.RecordVersion)
	return strings.Compare(leftKey, rightKey)
}

func autonomousCycleCoverage(
	kind portfolio.CadenceKind,
) ([]contracts.DepartmentKind, []string) {
	var departments []contracts.DepartmentKind
	var capabilities []string
	switch kind {
	case portfolio.CadenceDiscovery:
		departments = []contracts.DepartmentKind{
			contracts.DepartmentExecutive,
			contracts.DepartmentResearch,
		}
		capabilities = []string{
			"decision.portfolio",
			"market.research",
			"opportunity.intake",
		}
	case portfolio.CadenceLearning:
		departments = []contracts.DepartmentKind{
			contracts.DepartmentAccounting,
			contracts.DepartmentExecutive,
			contracts.DepartmentResearch,
		}
		capabilities = []string{
			"decision.portfolio",
			"learning.review",
			"measurement.review",
		}
	}
	return departments, capabilities
}

var _ autonomouscompany.NextCycleExecutor = (*AutonomousNextCycleExecutor)(nil)
