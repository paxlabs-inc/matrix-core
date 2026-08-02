package initiative

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/portfolio"
	"matrix/workforce/internal/workorder"
)

var (
	ErrStoreConflict = errors.New("initiative: durable plan conflict")
	ErrStoreState    = errors.New("initiative: durable plan state mismatch")
)

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	now            func() time.Time
}

type CommitResult struct {
	PlanID       string
	PlanVersion  uint64
	WorkOrderIDs []string
	Deduplicated bool
}

type CurrentPlan struct {
	Plan  Plan
	State string
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" || organizationID == "" || now == nil {
		return nil, fmt.Errorf("initiative: plan store dependencies are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("initiative: plan store Vault tenant mismatch")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		organizationID: organizationID, now: now,
	}, nil
}

func (store *Store) LoadCurrent(
	ctx context.Context,
	initiativeID string,
	authority workorder.CompanyAuthority,
) (CurrentPlan, error) {
	if !validToken(initiativeID) {
		return CurrentPlan{}, ErrStoreState
	}
	var planID, state string
	var version uint64
	if err := store.pool.QueryRow(ctx, `
		SELECT plan_id,version,state FROM workforce_company_initiative_plan_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
	`, store.tenantID, store.organizationID, initiativeID).Scan(&planID, &version, &state); err != nil {
		return CurrentPlan{}, fmt.Errorf("initiative: load current plan head: %w", err)
	}
	var canonicalHash string
	var sealed []byte
	if err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_plan FROM workforce_company_initiative_plans
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND plan_id=$4 AND version=$5
	`, store.tenantID, store.organizationID, initiativeID, planID, version).Scan(
		&canonicalHash, &sealed,
	); err != nil {
		return CurrentPlan{}, fmt.Errorf("initiative: load sealed current plan: %w", err)
	}
	body := Plan{OrganizationID: store.organizationID, InitiativeID: portfolio.InitiativeID(initiativeID), Version: version}
	canonical, err := store.vault.OpenRecord(store.planAD(body), sealed)
	if err != nil || hashBytes(canonical) != canonicalHash {
		return CurrentPlan{}, fmt.Errorf("initiative: current plan integrity failure")
	}
	decoded, err := contracts.DecodeCanonical[canonicalValue[Plan], *canonicalValue[Plan]](canonical)
	if err != nil || decoded.Value.ID != planID || decoded.Value.Version != version ||
		decoded.Value.OrganizationID != store.organizationID ||
		string(decoded.Value.InitiativeID) != initiativeID {
		return CurrentPlan{}, fmt.Errorf("initiative: current plan canonical identity mismatch")
	}
	if err := VerifyPlan(decoded.Value, authority); err != nil {
		return CurrentPlan{}, err
	}
	return CurrentPlan{Plan: decoded.Value, State: state}, nil
}

func (store *Store) Commit(
	ctx context.Context,
	plan Plan,
	authority workorder.CompanyAuthority,
) (CommitResult, error) {
	if plan.Version != 1 || plan.OrganizationID != store.organizationID {
		return CommitResult{}, ErrStoreState
	}
	if err := VerifyPlan(plan, authority); err != nil {
		return CommitResult{}, err
	}
	now, err := store.currentTime()
	if err != nil || plan.CompiledAt.After(now) {
		return CommitResult{}, ErrStoreState
	}
	prepared, err := store.preparePlan(plan)
	if err != nil {
		return CommitResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CommitResult{}, fmt.Errorf("initiative: begin plan commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockInitiative(ctx, tx, string(plan.InitiativeID)); err != nil {
		return CommitResult{}, err
	}
	result, found, err := store.findPlanTx(ctx, tx, plan, prepared.canonicalHash)
	if err != nil {
		return CommitResult{}, err
	}
	if found {
		result.Deduplicated = true
		return result, tx.Commit(ctx)
	}
	var headCount int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_company_initiative_plan_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
	`, store.tenantID, store.organizationID, plan.InitiativeID).Scan(&headCount); err != nil {
		return CommitResult{}, fmt.Errorf("initiative: inspect initial plan head: %w", err)
	}
	if headCount != 0 {
		return CommitResult{}, ErrStoreConflict
	}
	if err := store.insertPlanTx(ctx, tx, plan, prepared, now); err != nil {
		return CommitResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_initiative_plan_heads (
			tenant_id,organization_id,initiative_id,plan_id,version,state,updated_at
		) VALUES ($1,$2,$3,$4,$5,'active',$6)
	`, store.tenantID, store.organizationID, plan.InitiativeID, plan.ID, plan.Version, now); err != nil {
		return CommitResult{}, fmt.Errorf("initiative: insert initial plan head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return CommitResult{}, fmt.Errorf("initiative: commit initial plan: %w", err)
	}
	return commitResult(plan, false), nil
}

func (store *Store) CommitMutation(
	ctx context.Context,
	next Plan,
	mutation Mutation,
	authority workorder.CompanyAuthority,
) (CommitResult, error) {
	if next.OrganizationID != store.organizationID || mutation.OrganizationID != store.organizationID ||
		mutation.InitiativeID != string(next.InitiativeID) || mutation.ToPlanID != next.ID ||
		mutation.ToPlanVersion != next.Version {
		return CommitResult{}, ErrStoreState
	}
	if err := VerifyPlan(next, authority); err != nil {
		return CommitResult{}, err
	}
	if err := VerifyMutation(mutation, authority); err != nil {
		return CommitResult{}, err
	}
	now, err := store.currentTime()
	if err != nil || next.CompiledAt.After(now) || mutation.MutatedAt.After(now) {
		return CommitResult{}, ErrStoreState
	}
	preparedPlan, err := store.preparePlan(next)
	if err != nil {
		return CommitResult{}, err
	}
	preparedMutation, err := store.prepareMutation(mutation)
	if err != nil {
		return CommitResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CommitResult{}, fmt.Errorf("initiative: begin plan mutation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockInitiative(ctx, tx, string(next.InitiativeID)); err != nil {
		return CommitResult{}, err
	}
	var existingMutationHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_company_plan_mutations
		WHERE tenant_id=$1 AND organization_id=$2 AND mutation_id=$3
	`, store.tenantID, store.organizationID, mutation.ID).Scan(&existingMutationHash)
	if err == nil {
		if existingMutationHash != preparedMutation.canonicalHash {
			return CommitResult{}, ErrStoreConflict
		}
		result, found, findErr := store.findPlanTx(ctx, tx, next, preparedPlan.canonicalHash)
		if findErr != nil || !found {
			return CommitResult{}, ErrStoreState
		}
		result.Deduplicated = true
		return result, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return CommitResult{}, fmt.Errorf("initiative: inspect mutation replay: %w", err)
	}
	var currentPlanID, currentState string
	var currentVersion uint64
	if err := tx.QueryRow(ctx, `
		SELECT plan_id,version,state FROM workforce_company_initiative_plan_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 FOR UPDATE
	`, store.tenantID, store.organizationID, next.InitiativeID).Scan(
		&currentPlanID, &currentVersion, &currentState,
	); err != nil {
		return CommitResult{}, fmt.Errorf("initiative: lock current plan head: %w", err)
	}
	if currentPlanID != mutation.FromPlanID || currentVersion != mutation.FromPlanVersion ||
		next.Version != currentVersion+1 || currentState == "cancelled" {
		return CommitResult{}, ErrStoreState
	}
	if err := store.insertPlanTx(ctx, tx, next, preparedPlan, now); err != nil {
		return CommitResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_plan_mutations (
			tenant_id,organization_id,initiative_id,mutation_id,kind,from_plan_version,
			to_plan_version,reason,authority_ref,canonical_hash,signature_key_id,
			sealed_mutation,mutated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	`, store.tenantID, store.organizationID, mutation.InitiativeID, mutation.ID,
		mutation.Kind, mutation.FromPlanVersion, mutation.ToPlanVersion, mutation.Reason,
		mutation.AuthorityRef, preparedMutation.canonicalHash, mutation.Signature.KeyID,
		preparedMutation.sealed, mutation.MutatedAt); err != nil {
		return CommitResult{}, fmt.Errorf("initiative: insert plan mutation: %w", err)
	}
	for _, invalidation := range mutation.Invalidations {
		recordIDs := make([]string, len(invalidation.Evidence))
		for index := range invalidation.Evidence {
			recordIDs[index] = invalidation.Evidence[index].ID
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_plan_invalidations (
				tenant_id,organization_id,mutation_id,node_id,correction_id,reason,
				authority_ref,evidence_record_ids,invalidated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, store.organizationID, mutation.ID, invalidation.NodeID,
			invalidation.CorrectionID, invalidation.Reason, invalidation.AuthorityRef,
			recordIDs, mutation.MutatedAt); err != nil {
			return CommitResult{}, fmt.Errorf("initiative: insert plan invalidation: %w", err)
		}
	}
	for _, preserved := range mutation.PreservedReceipts {
		for _, receiptID := range preserved.Receipts {
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_company_plan_preserved_receipts (
					tenant_id,organization_id,mutation_id,node_id,receipt_id,preserved_at
				) VALUES ($1,$2,$3,$4,$5,$6)
			`, store.tenantID, store.organizationID, mutation.ID, preserved.NodeID,
				receiptID, mutation.MutatedAt); err != nil {
				return CommitResult{}, fmt.Errorf("initiative: insert preserved receipt: %w", err)
			}
		}
	}
	nextState := "active"
	if mutation.Kind == MutationCancel {
		nextState = "cancelled"
	} else if mutation.Kind == MutationInvalidate {
		nextState = "contested"
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_company_initiative_plan_heads
		SET plan_id=$4,version=$5,state=$6,updated_at=$7
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND plan_id=$8 AND version=$9
	`, store.tenantID, store.organizationID, next.InitiativeID, next.ID, next.Version,
		nextState, now, mutation.FromPlanID, mutation.FromPlanVersion)
	if err != nil {
		return CommitResult{}, fmt.Errorf("initiative: advance plan head: %w", err)
	}
	if command.RowsAffected() != 1 {
		return CommitResult{}, ErrStoreState
	}
	if err := tx.Commit(ctx); err != nil {
		return CommitResult{}, fmt.Errorf("initiative: commit plan mutation: %w", err)
	}
	return commitResult(next, false), nil
}

type preparedRecord struct {
	canonicalHash string
	sealed        []byte
}

func (store *Store) preparePlan(plan Plan) (preparedRecord, error) {
	canonical, err := contracts.EncodeCanonical(&canonicalValue[Plan]{Value: plan})
	if err != nil {
		return preparedRecord{}, fmt.Errorf("initiative: canonical plan: %w", err)
	}
	sealed, err := store.vault.SealRecord(store.planAD(plan), canonical)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("initiative: seal plan: %w", err)
	}
	return preparedRecord{canonicalHash: hashBytes(canonical), sealed: sealed}, nil
}

func (store *Store) prepareMutation(mutation Mutation) (preparedRecord, error) {
	canonical, err := contracts.EncodeCanonical(&mutation)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("initiative: canonical mutation: %w", err)
	}
	sealed, err := store.vault.SealRecord(store.mutationAD(mutation), canonical)
	if err != nil {
		return preparedRecord{}, fmt.Errorf("initiative: seal mutation: %w", err)
	}
	return preparedRecord{canonicalHash: hashBytes(canonical), sealed: sealed}, nil
}

func (store *Store) insertPlanTx(
	ctx context.Context,
	tx pgx.Tx,
	plan Plan,
	prepared preparedRecord,
	now time.Time,
) error {
	_, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_initiative_plans (
			tenant_id,organization_id,initiative_id,plan_id,version,initiative_version,
			blueprint_id,blueprint_version,mission_version,constitution_version,
			capital_envelope_version,issuer_policy_version,portfolio_decision_id,
			capital_allocation_id,capability_plan_id,capital_microunits,risk_microunits,
			canonical_hash,signature_key_id,sealed_plan,compiled_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21)
	`, store.tenantID, store.organizationID, plan.InitiativeID, plan.ID, plan.Version,
		plan.InitiativeVersion, plan.BlueprintID, plan.BlueprintVersion,
		plan.Authority.MissionVersion, plan.Authority.ConstitutionVersion,
		plan.Authority.CapitalEnvelopeVersion, plan.Authority.IssuerPolicyVersion,
		plan.Authority.PortfolioDecisionID, plan.Authority.CapitalAllocationID,
		plan.Authority.CapabilityPlanID, plan.CapitalMicrounits, plan.RiskMicrounits,
		prepared.canonicalHash, plan.Signature.KeyID, prepared.sealed, plan.CompiledAt)
	if err != nil {
		return fmt.Errorf("initiative: insert plan: %w", err)
	}
	for _, node := range plan.Nodes {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_initiative_plan_nodes (
				tenant_id,organization_id,initiative_id,plan_version,node_id,node_kind,
				state,node_hash,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, store.organizationID, plan.InitiativeID, plan.Version,
			node.Template.ID, node.Template.Kind, node.State, node.Digest.Digest, now); err != nil {
			return fmt.Errorf("initiative: insert plan node: %w", err)
		}
	}
	for _, edge := range plan.Edges {
		var outcome any
		if edge.When != nil {
			outcome = *edge.When
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_initiative_plan_edges (
				tenant_id,organization_id,initiative_id,plan_version,prerequisite_node_id,
				successor_node_id,branch_outcome,not_before,deadline,priority_delta,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, store.tenantID, store.organizationID, plan.InitiativeID, plan.Version,
			edge.Prerequisite, edge.Successor, outcome, edge.Schedule.NotBefore,
			edge.Schedule.Deadline, edge.Schedule.PriorityDelta, now); err != nil {
			return fmt.Errorf("initiative: insert plan edge: %w", err)
		}
	}
	for _, node := range plan.Nodes {
		if node.Order != nil {
			if err := store.insertCompanyOrderTx(ctx, tx, plan, node, now); err != nil {
				return err
			}
		}
		if node.Template.Kind == NodeOutcomeGate && node.Template.Gate != nil {
			if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_company_outcome_gates (
					tenant_id,organization_id,initiative_id,gate_id,plan_version,node_id,
					predicate_id,state,expires_at,created_at
				) VALUES ($1,$2,$3,$4,$5,$6,$7,'open',$8,$9)
			`, store.tenantID, store.organizationID, plan.InitiativeID,
				node.Template.ID, plan.Version, node.Template.ID,
				node.Template.Gate.PredicateID, node.Template.Gate.ExpiresAt, now); err != nil {
				return fmt.Errorf("initiative: insert outcome gate: %w", err)
			}
		}
	}
	return nil
}

func (store *Store) insertCompanyOrderTx(
	ctx context.Context,
	tx pgx.Tx,
	plan Plan,
	node CompiledNode,
	now time.Time,
) error {
	order := *node.Order
	canonical, err := contracts.EncodeCanonical(&order)
	if err != nil {
		return fmt.Errorf("initiative: canonical company Work Order: %w", err)
	}
	sealed, err := store.vault.SealRecord(store.orderAD(order), canonical)
	if err != nil {
		return fmt.Errorf("initiative: seal company Work Order: %w", err)
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_company_work_orders (
			tenant_id,organization_id,work_order_id,initiative_id,plan_version,
			plan_node_id,controller_id,issuer_kind,issuer_key_id,issuer_policy_version,
			work_order_class,issue_identity,idempotency_key,capital_microunits,
			risk_microunits,canonical_hash,sealed_order,deadline,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
	`, store.tenantID, store.organizationID, order.ID, plan.InitiativeID, plan.Version,
		node.Template.ID, order.ControllerID, order.IssuerKind, order.Signature.KeyID,
		order.Binding.IssuerPolicyVersion, order.Binding.WorkOrderClass,
		order.Binding.IssueIdentity, order.IdempotencyKey, order.Binding.CapitalMicrounits,
		order.Binding.RiskMicrounits, hashBytes(canonical), sealed, order.Deadline, order.CreatedAt)
	if err != nil {
		return fmt.Errorf("initiative: insert company Work Order: %w", err)
	}
	binding := order.Binding
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_work_order_bindings (
			tenant_id,organization_id,work_order_id,mission_id,mission_version,
			constitution_id,constitution_version,initiative_id,portfolio_decision_id,
			capital_allocation_id,capital_envelope_version,issuer_policy_version,
			capability_plan_id,capability_plan_hash,initiative_plan_id,
			initiative_plan_version,initiative_execution_criteria,
			business_success_criteria,business_outcome_gate_ids,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
	`, store.tenantID, store.organizationID, order.ID, binding.MissionID,
		binding.MissionVersion, binding.ConstitutionID, binding.ConstitutionVersion,
		binding.InitiativeID, binding.PortfolioDecisionID, binding.CapitalAllocationID,
		binding.CapitalEnvelopeVersion, binding.IssuerPolicyVersion,
		binding.CapabilityPlanID, binding.CapabilityPlanHash.Digest,
		binding.InitiativePlanID, binding.InitiativePlanVersion,
		binding.InitiativeExecutionCriteria, binding.BusinessSuccessCriteria,
		binding.BusinessOutcomeGateIDs, now); err != nil {
		return fmt.Errorf("initiative: insert company Work Order binding: %w", err)
	}
	for _, identity := range binding.EffectIdentities {
		command, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_effect_identities (
				tenant_id,organization_id,initiative_id,effect_identity,
				first_plan_version,first_node_id,work_order_id,reserved_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID, plan.InitiativeID, identity,
			plan.Version, node.Template.ID, order.ID, now)
		if err != nil {
			return fmt.Errorf("initiative: reserve effect identity: %w", err)
		}
		if command.RowsAffected() == 0 {
			var existingInitiative string
			if err := tx.QueryRow(ctx, `
				SELECT initiative_id FROM workforce_company_effect_identities
				WHERE tenant_id=$1 AND organization_id=$2 AND effect_identity=$3
			`, store.tenantID, store.organizationID, identity).Scan(&existingInitiative); err != nil ||
				existingInitiative != string(plan.InitiativeID) {
				return ErrStoreConflict
			}
		}
	}
	return nil
}

func (store *Store) findPlanTx(
	ctx context.Context,
	tx pgx.Tx,
	plan Plan,
	canonicalHash string,
) (CommitResult, bool, error) {
	var existingPlanID, existingHash string
	err := tx.QueryRow(ctx, `
		SELECT plan_id,canonical_hash FROM workforce_company_initiative_plans
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, plan.InitiativeID, plan.Version).Scan(
		&existingPlanID, &existingHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return CommitResult{}, false, nil
	}
	if err != nil {
		return CommitResult{}, false, fmt.Errorf("initiative: inspect plan replay: %w", err)
	}
	if existingPlanID != plan.ID || existingHash != canonicalHash {
		return CommitResult{}, false, ErrStoreConflict
	}
	return commitResult(plan, true), true, nil
}

func commitResult(plan Plan, deduplicated bool) CommitResult {
	workOrderIDs := make([]string, 0)
	for _, node := range plan.Nodes {
		if node.Order != nil {
			workOrderIDs = append(workOrderIDs, node.Order.ID)
		}
	}
	return CommitResult{
		PlanID: plan.ID, PlanVersion: plan.Version,
		WorkOrderIDs: workOrderIDs, Deduplicated: deduplicated,
	}
}

func (store *Store) lockInitiative(ctx context.Context, tx pgx.Tx, initiativeID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.tenantID+"|"+string(store.organizationID)+"|initiative-plan|"+initiativeID)
	if err != nil {
		return fmt.Errorf("initiative: lock plan: %w", err)
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("initiative: plan store time source must be UTC")
	}
	return now, nil
}

func (store *Store) planAD(plan Plan) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-initiative-plan",
		Stream: fmt.Sprintf("%s/%s/%d", store.organizationID, plan.InitiativeID, plan.Version),
		Schema: PlanSchemaVersion,
	}
}

func (store *Store) mutationAD(mutation Mutation) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-plan-mutation",
		Stream: string(store.organizationID) + "/" + mutation.ID,
		Schema: MutationSchemaVersion,
	}
}

func (store *Store) orderAD(order workorder.CompanyOrder) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.company-work-order",
		Stream: string(store.organizationID) + "/" + order.ID,
		Schema: workorder.CompanyOrderSchemaVersion,
	}
}

func hashBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
