package productexecution

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/companylifecycle"
	"centra/workforce/internal/companyruntime"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/initiative"
	"centra/workforce/internal/lineage"
	"centra/workforce/internal/mission"
	"centra/workforce/internal/productcapability"
	"centra/workforce/internal/squad"
	"centra/workforce/scheduler"
)

var (
	ErrConflict          = errors.New("product execution: durable state conflict")
	ErrInvalidPhase      = errors.New("product execution: operation is invalid in the current phase")
	ErrUnauthorized      = errors.New("product execution: authority or scope is invalid")
	ErrCorrectionBlocked = errors.New("product execution: unresolved correction blocks progress")
	ErrAmbiguousEffect   = errors.New("product execution: deployment effect requires reconciliation")
	ErrIntegrity         = errors.New("product execution: durable integrity failure")
)

// Store owns the durable saga that turns one funded Initiative into a launched
// product. All external credentials stay inside the injected effect Gateway.
type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	mission        *mission.Store
	squads         *squad.Store
	products       *productcapability.Store
	plans          *initiative.Store
	lifecycle      *companylifecycle.Store
	runtime        *companyruntime.Store
	scheduler      *scheduler.Store
	receipts       *lineage.Store
	effects        *effect.Gateway
	tenantID       string
	organizationID contracts.OrganizationID
	now            func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	missionStore *mission.Store,
	squadStore *squad.Store,
	productStore *productcapability.Store,
	planStore *initiative.Store,
	lifecycleStore *companylifecycle.Store,
	runtimeStore *companyruntime.Store,
	schedulerStore *scheduler.Store,
	receiptStore *lineage.Store,
	effectGateway *effect.Gateway,
	tenantID string,
	organizationID contracts.OrganizationID,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || missionStore == nil || squadStore == nil ||
		productStore == nil || planStore == nil || lifecycleStore == nil || runtimeStore == nil ||
		schedulerStore == nil || receiptStore == nil || effectGateway == nil ||
		validateToken("tenant id", tenantID) != nil || organizationID == "" || now == nil {
		return nil, fmt.Errorf("product execution: all durable stores and authorities are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("product execution: Vault tenant mismatch")
	}
	return &Store{
		pool: pool, vault: userVault, mission: missionStore, squads: squadStore,
		products: productStore, plans: planStore, lifecycle: lifecycleStore,
		runtime: runtimeStore, scheduler: schedulerStore, receipts: receiptStore,
		effects: effectGateway, tenantID: tenantID, organizationID: organizationID,
		now: now,
	}, nil
}

func (store *Store) Load(ctx context.Context, id ExecutionID) (View, error) {
	if validateToken("execution id", string(id)) != nil || len(id) > 64 {
		return View{}, ErrConflict
	}
	return store.loadView(ctx, store.pool, id)
}

func (store *Store) loadView(
	ctx context.Context,
	querier interface {
		Query(context.Context, string, ...any) (pgx.Rows, error)
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	id ExecutionID,
) (View, error) {
	view := View{SchemaVersion: SchemaVersion, ID: id, OrganizationID: store.organizationID}
	var planHash string
	var productRecord, engineeringRecord, deploymentEffect, launchTransition sql.NullString
	err := querier.QueryRow(ctx, `
		SELECT initiative_id,plan_id,plan_version,plan_hash,squad_assignment_id,
		       project_id,workspace_id,phase,version,product_record_id,
		       engineering_record_id,deployment_effect_id,launch_transition_id,
		       checkpoint_version,created_at,updated_at
		FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
	`, store.tenantID, store.organizationID, id).Scan(
		&view.InitiativeID, &view.PlanID, &view.PlanVersion, &planHash,
		&view.SquadAssignmentID, &view.ProjectID, &view.WorkspaceID, &view.Phase,
		&view.Version, &productRecord, &engineeringRecord, &deploymentEffect,
		&launchTransition, &view.CheckpointVersion, &view.CreatedAt, &view.UpdatedAt,
	)
	if err != nil {
		return View{}, fmt.Errorf("product execution: load head: %w", err)
	}
	view.PlanHash = contracts.ContentHash{Algorithm: "sha256", Digest: planHash}
	if productRecord.Valid {
		value := productcapability.RecordID(productRecord.String)
		view.ProductRecordID = &value
	}
	if engineeringRecord.Valid {
		value := productcapability.RecordID(engineeringRecord.String)
		view.EngineeringRecordID = &value
	}
	if deploymentEffect.Valid {
		value := companylifecycle.EffectID(deploymentEffect.String)
		view.DeploymentEffectID = &value
	}
	if launchTransition.Valid {
		value := companylifecycle.TransitionID(launchTransition.String)
		view.LaunchTransitionID = &value
	}
	if !view.Phase.Valid() || view.PlanHash.Validate() != nil || view.Version == 0 ||
		!validUTC(view.CreatedAt) || !validUTC(view.UpdatedAt) {
		return View{}, ErrIntegrity
	}

	rows, err := querier.Query(ctx, `
		SELECT stage,plan_node_id,work_order_id,need_id,seat_id,department_id,
		       seat_role,mandate_id,mandate_version,mandate_digest,goal_id,intent_id,wake_id
		FROM workforce_product_execution_stage_bindings
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		ORDER BY CASE stage
		  WHEN 'product' THEN 1 WHEN 'design' THEN 2 WHEN 'build' THEN 3
		  WHEN 'verification' THEN 4 WHEN 'deployment' THEN 5 ELSE 6 END
	`, store.tenantID, store.organizationID, id)
	if err != nil {
		return View{}, fmt.Errorf("product execution: load stage bindings: %w", err)
	}
	for rows.Next() {
		var binding StageBinding
		var digest string
		if err := rows.Scan(
			&binding.Stage, &binding.PlanNodeID, &binding.WorkOrderID, &binding.NeedID,
			&binding.SeatID, &binding.DepartmentID, &binding.Role, &binding.MandateID,
			&binding.MandateVersion, &digest, &binding.GoalID, &binding.IntentID,
			&binding.WakeID,
		); err != nil {
			rows.Close()
			return View{}, err
		}
		binding.MandateDigest = contracts.ContentHash{Algorithm: "sha256", Digest: digest}
		if err := binding.Validate(); err != nil {
			rows.Close()
			return View{}, ErrIntegrity
		}
		view.Stages = append(view.Stages, binding)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return View{}, err
	}
	rows.Close()
	if len(view.Stages) != len(orderedStages) {
		return View{}, ErrIntegrity
	}

	rows, err = querier.Query(ctx, `
		SELECT stage,receipt_id,receipt_hash,verdict_id,seat_id,intent_id,accepted_at
		FROM workforce_product_execution_stage_receipts
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		ORDER BY accepted_at,stage
	`, store.tenantID, store.organizationID, id)
	if err != nil {
		return View{}, err
	}
	for rows.Next() {
		var receipt StageReceipt
		var hash string
		if err := rows.Scan(&receipt.Stage, &receipt.ReceiptID, &hash, &receipt.VerdictID,
			&receipt.SeatID, &receipt.IntentID, &receipt.AcceptedAt); err != nil {
			rows.Close()
			return View{}, err
		}
		receipt.ReceiptHash = contracts.ContentHash{Algorithm: "sha256", Digest: hash}
		if err := receipt.Validate(); err != nil {
			rows.Close()
			return View{}, ErrIntegrity
		}
		view.Receipts = append(view.Receipts, receipt)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return View{}, err
	}
	rows.Close()

	rows, err = querier.Query(ctx, `
		SELECT effect_id,proposal_id,proposal_hash,operation,state,
		       COALESCE(external_id,''),evidence_hash,prepared_at,reconciled_at
		FROM workforce_product_execution_effects
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		ORDER BY prepared_at,effect_id
	`, store.tenantID, store.organizationID, id)
	if err != nil {
		return View{}, err
	}
	for rows.Next() {
		var item EffectView
		var proposalHash string
		var evidenceHash sql.NullString
		var reconciledAt sql.NullTime
		if err := rows.Scan(&item.EffectID, &item.ProposalID, &proposalHash,
			&item.Operation, &item.State, &item.ExternalID, &evidenceHash,
			&item.PreparedAt, &reconciledAt); err != nil {
			rows.Close()
			return View{}, err
		}
		item.ProposalHash = contracts.ContentHash{Algorithm: "sha256", Digest: proposalHash}
		if evidenceHash.Valid {
			value := contracts.ContentHash{Algorithm: "sha256", Digest: evidenceHash.String}
			item.EvidenceHash = &value
		}
		if reconciledAt.Valid {
			value := reconciledAt.Time.UTC()
			item.ReconciledAt = &value
		}
		view.Effects = append(view.Effects, item)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return View{}, err
	}
	rows.Close()
	return view, nil
}

func (store *Store) loadStart(ctx context.Context, id ExecutionID) (StartRecord, error) {
	var expectedHash string
	var sealed []byte
	if err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_start FROM workforce_product_executions
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&expectedHash, &sealed); err != nil {
		return StartRecord{}, err
	}
	opened, err := store.vault.OpenRecord(store.startAD(id), sealed)
	if err != nil {
		return StartRecord{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[StartRecord, *StartRecord](opened)
	if err != nil || value.Request.ID != id || value.Request.OrganizationID != store.organizationID ||
		value.Validate() != nil {
		return StartRecord{}, ErrIntegrity
	}
	hash, err := contracts.HashCanonical(value)
	if err != nil || hash.Digest != expectedHash {
		return StartRecord{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) appendEventTx(
	ctx context.Context,
	tx pgx.Tx,
	id ExecutionID,
	phase Phase,
	kind string,
	stage *Stage,
	sourceID string,
	idempotencyKey string,
	now time.Time,
) error {
	var existingPhase, existingKind string
	var existingStage, existingSource sql.NullString
	err := tx.QueryRow(ctx, `
		SELECT phase,event_kind,stage,source_id
		FROM workforce_product_execution_events
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
		  AND idempotency_key=$4
	`, store.tenantID, store.organizationID, id, idempotencyKey).Scan(
		&existingPhase, &existingKind, &existingStage, &existingSource,
	)
	if err == nil {
		stageMatches := stage == nil && !existingStage.Valid ||
			stage != nil && existingStage.Valid && existingStage.String == string(*stage)
		if existingPhase != string(phase) || existingKind != kind ||
			!stageMatches || existingSource.String != sourceID {
			return ErrConflict
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	var sequence uint64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(sequence),0)+1
		FROM workforce_product_execution_events
		WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&sequence); err != nil {
		return err
	}
	value := Event{
		SchemaVersion: EventSchemaVersion, ExecutionID: id, Sequence: sequence,
		Phase: phase, Kind: kind, Stage: stage, SourceID: sourceID,
		IdempotencyKey: idempotencyKey, CreatedAt: now,
	}
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	hash, err := contracts.HashCanonical(value)
	if err != nil {
		return err
	}
	sealed, err := store.vault.SealRecord(store.eventAD(id, sequence), canonical)
	if err != nil {
		return err
	}
	var stageValue any
	if stage != nil {
		stageValue = *stage
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_product_execution_events (
			tenant_id,organization_id,execution_id,sequence,phase,event_kind,stage,
			source_id,idempotency_key,content_hash,sealed_event,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),$9,$10,$11,$12)
		ON CONFLICT (tenant_id,organization_id,execution_id,idempotency_key) DO NOTHING
	`, store.tenantID, store.organizationID, id, sequence, phase, kind, stageValue,
		sourceID, idempotencyKey, hash.Digest, sealed, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		if err := tx.QueryRow(ctx, `
			SELECT content_hash FROM workforce_product_execution_events
			WHERE tenant_id=$1 AND organization_id=$2 AND execution_id=$3
			  AND idempotency_key=$4
		`, store.tenantID, store.organizationID, id, idempotencyKey).Scan(&existingHash); err != nil ||
			existingHash != hash.Digest {
			return ErrConflict
		}
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("product execution: time source must return UTC")
	}
	return now, nil
}

func (store *Store) startAD(id ExecutionID) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.product-execution.start",
		Stream: string(store.organizationID) + "/" + string(id), Schema: SchemaVersion,
	}
}

func (store *Store) eventAD(id ExecutionID, sequence uint64) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.product-execution.event",
		Stream: fmt.Sprintf("%s/%s/%d", store.organizationID, id, sequence),
		Schema: EventSchemaVersion,
	}
}

func nullableString[T ~string](value *T) any {
	if value == nil {
		return nil
	}
	return string(*value)
}
