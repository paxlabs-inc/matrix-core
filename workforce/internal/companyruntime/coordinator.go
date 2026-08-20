package companyruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"centra/workforce/internal/companylifecycle"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/initiative"
	"centra/workforce/internal/mission"
	"centra/workforce/internal/portfolio"
	"centra/workforce/internal/workorder"
)

type Coordinator struct {
	pool      *pgxpool.Pool
	store     *Store
	mission   *mission.Store
	portfolio *portfolio.Store
	lifecycle *companylifecycle.Store
	compiler  *initiative.Compiler
	plans     *initiative.Store
	dispatch  *initiative.Dispatcher
	cycles    *CycleDispatcher
	now       func() time.Time
}

func (coordinator *Coordinator) CompanyAuthority(
	ctx context.Context,
) (workorder.CompanyAuthority, error) {
	now := coordinator.now()
	current, err := coordinator.mission.LoadCurrent(ctx)
	if err != nil || !validUTC(now) || !current.Executable(now) {
		return workorder.CompanyAuthority{}, fmt.Errorf("company runtime: company authority is not executable")
	}
	authority := workorder.CompanyAuthority{
		Policy:                        current.Authority.IssuerPolicy,
		FounderKeyID:                  coordinator.store.founderKeyID,
		FounderPublicKey:              append([]byte(nil), coordinator.store.founderPublicKey...),
		CurrentMissionVersion:         current.Authority.Mission.Version,
		CurrentConstitutionVersion:    current.Authority.Constitution.Version,
		CurrentCapitalEnvelopeVersion: current.Authority.Capital.Version,
		At:                            now,
	}
	if err := authority.Validate(coordinator.store.organizationID); err != nil {
		return workorder.CompanyAuthority{}, err
	}
	return authority, nil
}

func (coordinator *Coordinator) AttachCycleDispatcher(dispatcher *CycleDispatcher) error {
	if coordinator == nil || dispatcher == nil {
		return fmt.Errorf("company runtime: cycle dispatcher is required")
	}
	coordinator.cycles = dispatcher
	return nil
}

func (coordinator *Coordinator) DispatchCycle(
	ctx context.Context,
	plan portfolio.CyclePlan,
) (CycleDispatchResult, error) {
	if coordinator.cycles == nil {
		return CycleDispatchResult{}, fmt.Errorf("company runtime: cycle dispatcher is not attached")
	}
	configuration, err := coordinator.store.LoadCurrent(ctx)
	if err != nil {
		return CycleDispatchResult{}, err
	}
	return coordinator.cycles.Dispatch(ctx, plan, configuration)
}

func NewCoordinator(
	pool *pgxpool.Pool,
	store *Store,
	missionStore *mission.Store,
	portfolioStore *portfolio.Store,
	lifecycleStore *companylifecycle.Store,
	compiler *initiative.Compiler,
	planStore *initiative.Store,
	now func() time.Time,
) (*Coordinator, error) {
	if pool == nil || store == nil || missionStore == nil || portfolioStore == nil ||
		lifecycleStore == nil || compiler == nil || planStore == nil || now == nil {
		return nil, fmt.Errorf("company runtime: coordinator dependencies are required")
	}
	return &Coordinator{
		pool: pool, store: store, mission: missionStore, portfolio: portfolioStore,
		lifecycle: lifecycleStore, compiler: compiler, plans: planStore,
		now: now,
	}, nil
}

func NewCoordinatorWithDispatcher(
	pool *pgxpool.Pool,
	store *Store,
	missionStore *mission.Store,
	portfolioStore *portfolio.Store,
	lifecycleStore *companylifecycle.Store,
	compiler *initiative.Compiler,
	planStore *initiative.Store,
	dispatcher *initiative.Dispatcher,
	now func() time.Time,
) (*Coordinator, error) {
	coordinator, err := NewCoordinator(
		pool, store, missionStore, portfolioStore, lifecycleStore,
		compiler, planStore, now,
	)
	if err != nil {
		return nil, err
	}
	if dispatcher == nil {
		return nil, fmt.Errorf("company runtime: company dispatcher is required")
	}
	coordinator.dispatch = dispatcher
	return coordinator, nil
}

func (coordinator *Coordinator) DispatchReady(
	ctx context.Context,
	limit uint16,
) ([]initiative.DispatchResult, error) {
	if limit == 0 || limit > 1000 {
		return nil, fmt.Errorf("company runtime: dispatch limit must be 1 to 1000")
	}
	if coordinator.dispatch == nil {
		return nil, fmt.Errorf("company runtime: company dispatcher is not attached")
	}
	if _, err := coordinator.store.LoadCurrent(ctx); err != nil {
		return nil, err
	}
	now := coordinator.now()
	current, err := coordinator.mission.LoadCurrent(ctx)
	if err != nil || !validUTC(now) || !current.Executable(now) {
		return nil, fmt.Errorf("company runtime: company authority is not executable")
	}
	companyAuthority := workorder.CompanyAuthority{
		Policy:                        current.Authority.IssuerPolicy,
		FounderKeyID:                  coordinator.store.founderKeyID,
		FounderPublicKey:              coordinator.store.founderPublicKey,
		CurrentMissionVersion:         current.Authority.Mission.Version,
		CurrentConstitutionVersion:    current.Authority.Constitution.Version,
		CurrentCapitalEnvelopeVersion: current.Authority.Capital.Version,
		At:                            now,
	}
	rows, err := coordinator.pool.Query(ctx, `
		SELECT initiative_id
		FROM workforce_company_initiative_plan_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
		ORDER BY updated_at,initiative_id
		LIMIT $3
	`, coordinator.store.tenantID, coordinator.store.organizationID, limit)
	if err != nil {
		return nil, fmt.Errorf("company runtime: list dispatchable plans: %w", err)
	}
	var initiativeIDs []string
	for rows.Next() {
		var initiativeID string
		if err := rows.Scan(&initiativeID); err != nil {
			rows.Close()
			return nil, err
		}
		initiativeIDs = append(initiativeIDs, initiativeID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	remaining := limit
	results := make([]initiative.DispatchResult, 0)
	for _, initiativeID := range initiativeIDs {
		currentPlan, err := coordinator.plans.LoadCurrent(ctx, initiativeID, companyAuthority)
		if err != nil {
			return nil, err
		}
		dispatched, err := coordinator.dispatch.DispatchReady(
			ctx, currentPlan.Plan, companyAuthority, remaining,
		)
		if err != nil {
			return nil, err
		}
		results = append(results, dispatched...)
		if len(dispatched) >= int(remaining) {
			break
		}
		remaining -= uint16(len(dispatched))
	}
	return results, nil
}

func (coordinator *Coordinator) Store() *Store {
	return coordinator.store
}

func (coordinator *Coordinator) RunDue(ctx context.Context, limit uint16) ([]portfolio.CyclePlan, error) {
	if coordinator.cycles != nil {
		if err := coordinator.cycles.Reconcile(ctx); err != nil {
			return nil, err
		}
	}
	controller, _, err := coordinator.store.Controller(ctx)
	if err != nil {
		return nil, err
	}
	plans, err := controller.ClaimCyclePlans(ctx, limit)
	if err != nil {
		return nil, err
	}
	now := coordinator.now()
	if !validUTC(now) {
		return nil, fmt.Errorf("company runtime: time source must return UTC")
	}
	for _, plan := range plans {
		departments := make([]string, len(plan.Departments))
		for index := range plan.Departments {
			departments[index] = string(plan.Departments[index])
		}
		if _, err := coordinator.pool.Exec(ctx, `
			INSERT INTO workforce_company_cycle_runs (
				tenant_id,organization_id,cycle_id,cadence_kind,due_at,next_at,
				departments,required_capabilities,independent_audit,state,created_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'planned',$10,$10)
			ON CONFLICT (tenant_id,organization_id,cycle_id) DO NOTHING
		`, coordinator.store.tenantID, coordinator.store.organizationID, plan.ID, plan.Kind,
			plan.DueAt, plan.NextAt, departments, plan.RequiredCapabilities,
			plan.IndependentAudit, now); err != nil {
			return nil, fmt.Errorf("company runtime: persist cadence cycle: %w", err)
		}
	}
	_, _ = coordinator.portfolio.DetectStarvation(ctx, 14*24*time.Hour)
	rows, err := coordinator.pool.Query(ctx, `
		SELECT cycle_id,cadence_kind,due_at,next_at,departments,
		       required_capabilities,independent_audit
		FROM workforce_company_cycle_runs
		WHERE tenant_id=$1 AND organization_id=$2 AND state='planned'
		ORDER BY due_at,cycle_id LIMIT $3
	`, coordinator.store.tenantID, coordinator.store.organizationID, limit)
	if err != nil {
		return nil, fmt.Errorf("company runtime: load planned cycle recovery: %w", err)
	}
	recovered := make([]portfolio.CyclePlan, 0)
	for rows.Next() {
		var plan portfolio.CyclePlan
		var departments []string
		if err := rows.Scan(
			&plan.ID, &plan.Kind, &plan.DueAt, &plan.NextAt, &departments,
			&plan.RequiredCapabilities, &plan.IndependentAudit,
		); err != nil {
			rows.Close()
			return nil, err
		}
		plan.SchemaVersion = "workforce.company-cycle.v1"
		plan.OrganizationID = coordinator.store.organizationID
		plan.Departments = make([]contracts.DepartmentKind, len(departments))
		for index := range departments {
			plan.Departments[index] = contracts.DepartmentKind(departments[index])
		}
		if err := plan.Validate(); err != nil {
			rows.Close()
			return nil, fmt.Errorf("company runtime: invalid recovered cycle: %w", err)
		}
		recovered = append(recovered, plan)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return recovered, nil
}

func (coordinator *Coordinator) FundInitiative(
	ctx context.Context,
	request FundingRequest,
) (FundingResult, error) {
	if err := request.Validate(); err != nil {
		return FundingResult{}, err
	}
	controller, configuration, err := coordinator.store.Controller(ctx)
	if err != nil {
		return FundingResult{}, err
	}
	decision, decisionDeduplicated, err := controller.Decide(
		ctx, request.DecisionID, request.OpportunityID, request.Assessment,
		request.Alternatives, request.DecisionIdempotencyKey,
	)
	if err != nil {
		return FundingResult{}, err
	}
	if decision.Decision != portfolio.DecisionGO || decision.InitiativeID == nil {
		return FundingResult{
			Decision: decision, State: string(decision.Decision), Deduplicated: decisionDeduplicated,
		}, nil
	}
	if request.CapitalImpact.AllocateMicrounits != decision.CapitalImpactMicrounits ||
		request.CapitalImpact.CapitalEnvelopeVersion != configuration.CapitalEnvelopeVersion ||
		request.CapitalImpact.CapitalEnvelopeHash != configuration.CapitalEnvelopeHash {
		return FundingResult{}, fmt.Errorf("company runtime: funded capital does not match the GO decision")
	}
	now := coordinator.now()
	if !validUTC(now) || request.Initiative.Deadline.Before(now) {
		return FundingResult{}, fmt.Errorf("company runtime: initiative deadline is not current")
	}
	current, err := coordinator.mission.LoadCurrent(ctx)
	if err != nil || !current.Executable(now) {
		return FundingResult{}, fmt.Errorf("company runtime: company authority is not executable")
	}
	clauseID := decision.AuthorityClauses[0]
	authorityBinding, err := coordinator.authorityBinding(
		ctx, current.Authority, configuration, request.RequestedBySeatID, clauseID,
	)
	if err != nil {
		return FundingResult{}, err
	}
	decisionHash, err := contracts.HashCanonical(&decision)
	if err != nil {
		return FundingResult{}, err
	}
	lifecycleID := companylifecycle.InitiativeID(*decision.InitiativeID)
	if lifecycleID != companylifecycle.InitiativeID("initiative:"+string(request.OpportunityID)) {
		return FundingResult{}, fmt.Errorf("company runtime: portfolio initiative identity is not canonical")
	}
	if _, err := coordinator.pool.Exec(ctx, `
		INSERT INTO workforce_company_funding_runs (
			tenant_id,organization_id,funding_id,opportunity_id,initiative_id,
			decision_id,decision_hash,lifecycle_version,plan_id,plan_version,
			state,error_code,created_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,0,NULL,NULL,'decided',NULL,$8,$8)
		ON CONFLICT (tenant_id,organization_id,funding_id) DO NOTHING
	`, coordinator.store.tenantID, coordinator.store.organizationID, request.FundingID,
		request.OpportunityID, lifecycleID, decision.ID, decisionHash.Digest, now); err != nil {
		return FundingResult{}, fmt.Errorf("company runtime: persist funding decision: %w", err)
	}
	var storedOpportunity, storedInitiative, storedDecision, storedHash string
	if err := coordinator.pool.QueryRow(ctx, `
		SELECT opportunity_id,initiative_id,decision_id,decision_hash
		FROM workforce_company_funding_runs
		WHERE tenant_id=$1 AND organization_id=$2 AND funding_id=$3
	`, coordinator.store.tenantID, coordinator.store.organizationID, request.FundingID).Scan(
		&storedOpportunity, &storedInitiative, &storedDecision, &storedHash,
	); err != nil {
		return FundingResult{}, fmt.Errorf("company runtime: inspect funding replay: %w", err)
	}
	if storedOpportunity != string(request.OpportunityID) || storedInitiative != string(lifecycleID) ||
		storedDecision != string(decision.ID) || storedHash != decisionHash.Digest {
		return FundingResult{}, fmt.Errorf("company runtime: funding identity conflicts with committed state")
	}
	zeroImpact := request.CapitalImpact
	zeroImpact.AllocateMicrounits = 0
	zeroImpact.ReleaseMicrounits = 0
	zeroImpact.SpendMicrounits = 0
	zeroImpact.AllocatedSpendMicrounits = 0
	zeroImpact.ExposureIncreaseMicrounits = 0
	zeroImpact.ExposureReleaseMicrounits = 0
	zeroImpact.RecognizedRevenueMicrounits = 0
	create := companylifecycle.CreateRequest{
		SchemaVersion: companylifecycle.TransitionSchemaVersion,
		TransitionID:  transitionID(request.FundingID, "discover"),
		ReceiptID:     receiptID(request.FundingID, "discover"),
		Initiative: companylifecycle.Initiative{
			SchemaVersion: companylifecycle.InitiativeSchemaVersion,
			ID:            lifecycleID, OrganizationID: coordinator.store.organizationID,
			OpportunityID: companylifecycle.OpportunityID(request.OpportunityID),
			PortfolioID:   request.PortfolioID, CreatedBySeat: request.RequestedBySeatID,
			OriginHash: decisionHash, CreatedAt: now,
		},
		Authority: authorityBinding, CompanyState: request.CompanyState,
		Correction: request.Correction, Evidence: request.Evidence,
		CapitalImpact: zeroImpact, IdempotencyKey: idempotencyKey(request.FundingID, "discover"),
	}
	createHash, err := contracts.HashCanonical(create)
	if err != nil {
		return FundingResult{}, err
	}
	if _, err := coordinator.store.AuthorizeGate(
		ctx, create.TransitionID, lifecycleID, createHash, decision, clauseID,
	); err != nil {
		return FundingResult{}, err
	}
	created, err := coordinator.lifecycle.CreateInitiative(ctx, create)
	if err != nil {
		return FundingResult{}, err
	}
	if _, err := coordinator.pool.Exec(ctx, `
		UPDATE workforce_company_funding_runs
		SET lifecycle_version=$1,state='lifecycle_started',updated_at=$2,error_code=NULL
		WHERE tenant_id=$3 AND organization_id=$4 AND funding_id=$5
	`, created.Checkpoint.Version, now, coordinator.store.tenantID,
		coordinator.store.organizationID, request.FundingID); err != nil {
		return FundingResult{}, err
	}
	checkpoint := created.Checkpoint
	steps := []struct {
		name     string
		to       companylifecycle.State
		decision companylifecycle.Decision
		impact   companylifecycle.CapitalImpact
	}{
		{"screen", companylifecycle.StateScreen, companylifecycle.DecisionAdvance, zeroImpact},
		{"validate", companylifecycle.StateValidate, companylifecycle.DecisionAdvance, zeroImpact},
		{"decide", companylifecycle.StateDecide, companylifecycle.DecisionAdvance, zeroImpact},
		{"fund", companylifecycle.StateFund, companylifecycle.DecisionGo, request.CapitalImpact},
	}
	for _, step := range steps {
		transition := companylifecycle.TransitionRequest{
			SchemaVersion:  companylifecycle.TransitionSchemaVersion,
			TransitionID:   transitionID(request.FundingID, step.name),
			ReceiptID:      receiptID(request.FundingID, step.name),
			OrganizationID: coordinator.store.organizationID, InitiativeID: lifecycleID,
			ExpectedVersion: checkpoint.Version, FromState: checkpoint.State,
			ToState: step.to, Decision: step.decision, Authority: authorityBinding,
			CompanyState: request.CompanyState, Correction: request.Correction,
			Evidence: request.Evidence, CapitalImpact: step.impact,
			IdempotencyKey: idempotencyKey(request.FundingID, step.name),
		}
		requestHash, hashErr := contracts.HashCanonical(transition)
		if hashErr != nil {
			return FundingResult{}, hashErr
		}
		if _, err := coordinator.store.AuthorizeGate(
			ctx, transition.TransitionID, lifecycleID, requestHash, decision, clauseID,
		); err != nil {
			return FundingResult{}, err
		}
		result, err := coordinator.lifecycle.Transition(ctx, transition)
		if err != nil {
			return FundingResult{}, err
		}
		checkpoint = result.Checkpoint
		if _, err := coordinator.pool.Exec(ctx, `
			UPDATE workforce_company_funding_runs SET lifecycle_version=$1,
				state=CASE WHEN $2='FUND' THEN 'funded' ELSE state END,
				updated_at=$3,error_code=NULL
			WHERE tenant_id=$4 AND organization_id=$5 AND funding_id=$6
		`, checkpoint.Version, checkpoint.State, now, coordinator.store.tenantID,
			coordinator.store.organizationID, request.FundingID); err != nil {
			return FundingResult{}, err
		}
	}
	companyAuthority := workorder.CompanyAuthority{
		Policy:                        current.Authority.IssuerPolicy,
		FounderKeyID:                  coordinator.store.founderKeyID,
		FounderPublicKey:              coordinator.store.founderPublicKey,
		CurrentMissionVersion:         current.Authority.Mission.Version,
		CurrentConstitutionVersion:    current.Authority.Constitution.Version,
		CurrentCapitalEnvelopeVersion: current.Authority.Capital.Version,
		At:                            now,
	}
	initiativeValue := initiative.Initiative{
		SchemaVersion: initiative.InitiativeSchemaVersion,
		ID:            *decision.InitiativeID, Version: request.Initiative.Version,
		OrganizationID: coordinator.store.organizationID,
		MissionID:      current.Authority.Mission.ID, MissionVersion: current.Authority.Mission.Version,
		ConstitutionID:         current.Authority.Constitution.ID,
		ConstitutionVersion:    current.Authority.Constitution.Version,
		CapitalEnvelopeVersion: current.Authority.Capital.Version,
		IssuerPolicyVersion:    current.Authority.IssuerPolicy.Version,
		PortfolioDecisionID:    decision.ID,
		Allocation: initiative.CapitalAllocation{
			ID: request.Initiative.AllocationID, Currency: request.Initiative.Currency,
			CapitalMicrounits: decision.CapitalImpactMicrounits,
			RiskMicrounits:    decision.RiskImpactMicrounits,
		},
		CapabilityPlan:    request.Initiative.CapabilityPlan,
		Objective:         request.Initiative.Objective,
		ExecutionCriteria: request.Initiative.ExecutionCriteria,
		BusinessCriteria:  request.Initiative.BusinessCriteria,
		BusinessGates:     request.Initiative.BusinessGates,
		Deadline:          request.Initiative.Deadline, CreatedAt: now,
	}
	if err := initiative.SignInitiative(
		&initiativeValue, companyAuthority, coordinator.store.controllerPrivate,
	); err != nil {
		return FundingResult{}, err
	}
	plan, err := coordinator.compiler.Compile(initiative.CompileInput{
		Authority: current.Authority, Decision: decision,
		DecisionKeyID:     coordinator.store.controllerKeyID,
		DecisionPublicKey: coordinator.store.controllerPublic,
		Initiative:        initiativeValue, Blueprint: request.Initiative.Blueprint, CompiledAt: now,
	})
	if err != nil {
		return FundingResult{}, err
	}
	committed, err := coordinator.plans.Commit(ctx, plan, companyAuthority)
	if err != nil {
		return FundingResult{}, err
	}
	if _, err := coordinator.pool.Exec(ctx, `
		UPDATE workforce_company_funding_runs
		SET plan_id=$1,plan_version=$2,state='plan_committed',updated_at=$3,error_code=NULL
		WHERE tenant_id=$4 AND organization_id=$5 AND funding_id=$6
	`, committed.PlanID, committed.PlanVersion, now, coordinator.store.tenantID,
		coordinator.store.organizationID, request.FundingID); err != nil {
		return FundingResult{}, err
	}
	return FundingResult{
		Decision: decision, Checkpoint: checkpoint, Plan: plan, State: "plan_committed",
		Deduplicated: decisionDeduplicated || created.Deduplicated || committed.Deduplicated,
	}, nil
}

func (coordinator *Coordinator) authorityBinding(
	ctx context.Context,
	authority mission.ActivationAuthority,
	configuration StartConfiguration,
	seatID contracts.SeatID,
	clauseID string,
) (companylifecycle.AuthorityBinding, error) {
	var mandateID string
	var mandateVersion uint64
	var mandateHash string
	var active bool
	if err := coordinator.pool.QueryRow(ctx, `
		SELECT seat.mandate_id,seat.mandate_version,record.canonical_hash,seat.active
		FROM workforce_organization_seats seat
		JOIN workforce_authority_records record
		  ON record.tenant_id=seat.tenant_id AND record.organization_id=seat.organization_id
		 AND record.authority_kind='mandate' AND record.authority_id=seat.mandate_id
		 AND record.version=seat.mandate_version
		WHERE seat.tenant_id=$1 AND seat.organization_id=$2 AND seat.seat_id=$3
	`, coordinator.store.tenantID, coordinator.store.organizationID, seatID).Scan(
		&mandateID, &mandateVersion, &mandateHash, &active,
	); err != nil || !active {
		return companylifecycle.AuthorityBinding{}, fmt.Errorf("company runtime: requesting seat mandate is unavailable")
	}
	expiresAt := configuration.ExpiresAt
	if authority.IssuerPolicy.ExpiresAt.Before(expiresAt) {
		expiresAt = authority.IssuerPolicy.ExpiresAt
	}
	return companylifecycle.AuthorityBinding{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: coordinator.store.organizationID,
		MissionVersion: configuration.MissionVersion, MissionHash: configuration.MissionHash,
		ConstitutionVersion:    configuration.ConstitutionVersion,
		ConstitutionHash:       configuration.ConstitutionHash,
		CapitalEnvelopeVersion: configuration.CapitalEnvelopeVersion,
		CapitalEnvelopeHash:    configuration.CapitalEnvelopeHash,
		IssuerPolicyVersion:    configuration.IssuerPolicyVersion,
		IssuerPolicyHash:       configuration.IssuerPolicyHash,
		MandateID:              contracts.MandateID(mandateID), MandateVersion: mandateVersion,
		MandateHash:       contracts.ContentHash{Algorithm: "sha256", Digest: mandateHash},
		RequestedBySeatID: seatID, ClauseID: clauseID, ExpiresAt: expiresAt,
	}, nil
}

func transitionID(fundingID, stage string) companylifecycle.TransitionID {
	return companylifecycle.TransitionID("transition:" + fundingID + ":" + stage)
}

func receiptID(fundingID, stage string) companylifecycle.DecisionReceiptID {
	return companylifecycle.DecisionReceiptID("receipt:" + fundingID + ":" + stage)
}

func idempotencyKey(fundingID, stage string) string {
	return "company-funding:" + fundingID + ":" + stage
}
