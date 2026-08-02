package companylifecycle

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

// EffectStatus is the durable reconciliation state of an external effect.
type EffectStatus string

const (
	// EffectPrepared means dispatch may have happened and replay is forbidden until reconciliation.
	EffectPrepared EffectStatus = "PREPARED"
	// EffectCommitted means an exact provider outcome is durably recorded and must not be replayed.
	EffectCommitted EffectStatus = "COMMITTED"
	// EffectConsumed means a committed outcome is bound to one lifecycle transition.
	EffectConsumed EffectStatus = "CONSUMED"
)

// Valid reports whether the effect status is recognized.
func (value EffectStatus) Valid() bool {
	switch value {
	case EffectPrepared, EffectCommitted, EffectConsumed:
		return true
	default:
		return false
	}
}

// EffectRequest durably claims an external idempotency identity before dispatch.
type EffectRequest struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        EffectID                 `json:"effect_id"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	InitiativeID              InitiativeID             `json:"initiative_id"`
	ExpectedLifecycleVersion  uint64                   `json:"expected_lifecycle_version"`
	ExternalSystem            string                   `json:"external_system"`
	Operation                 string                   `json:"operation"`
	RequestHash               contracts.ContentHash    `json:"request_hash"`
	ExternalIdempotencyKey    string                   `json:"external_idempotency_key"`
	AuthorizationReceiptID    DecisionReceiptID        `json:"authorization_receipt_id"`
	AuthorizationDecisionHash contracts.ContentHash    `json:"authorization_decision_hash"`
	PreparedBySeatID          contracts.SeatID         `json:"prepared_by_seat_id"`
	LeaseID                   contracts.LeaseID        `json:"lease_id"`
	Fence                     contracts.FenceToken     `json:"fence"`
	LeaseExpiresAt            time.Time                `json:"lease_expires_at"`
}

// Validate enforces an exact external request and prior lifecycle authorization binding.
func (value EffectRequest) Validate() error {
	if value.SchemaVersion != EffectSchemaVersion ||
		validateToken("effect id", string(value.ID)) != nil ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		value.ExpectedLifecycleVersion == 0 ||
		validateToken("external system", value.ExternalSystem) != nil ||
		validateToken("external operation", value.Operation) != nil ||
		validateIdempotencyKey(value.ExternalIdempotencyKey) != nil ||
		validateToken("authorization receipt id", string(value.AuthorizationReceiptID)) != nil ||
		validateToken("preparing seat id", string(value.PreparedBySeatID)) != nil ||
		validateToken("lease id", string(value.LeaseID)) != nil ||
		!validUTC(value.LeaseExpiresAt) {
		return fmt.Errorf("company lifecycle: effect request is invalid")
	}
	if err := value.Fence.Validate(); err != nil {
		return err
	}
	if err := value.RequestHash.Validate(); err != nil {
		return err
	}
	return value.AuthorizationDecisionHash.Validate()
}

// EffectCommit records the exact outcome of one externally idempotent operation.
type EffectCommit struct {
	SchemaVersion       string                   `json:"schema_version"`
	EffectID            EffectID                 `json:"effect_id"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	InitiativeID        InitiativeID             `json:"initiative_id"`
	RequestHash         contracts.ContentHash    `json:"request_hash"`
	OutcomeHash         contracts.ContentHash    `json:"outcome_hash"`
	ExternalReceiptID   string                   `json:"external_receipt_id"`
	ExternalReceiptHash contracts.ContentHash    `json:"external_receipt_hash"`
	ExternalCommittedAt time.Time                `json:"external_committed_at"`
	IdempotencyKey      string                   `json:"idempotency_key"`
}

// Validate enforces an exact content-addressed external outcome.
func (value EffectCommit) Validate() error {
	if value.SchemaVersion != EffectSchemaVersion ||
		validateToken("effect id", string(value.EffectID)) != nil ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		validateToken("external receipt id", value.ExternalReceiptID) != nil ||
		validateIdempotencyKey(value.IdempotencyKey) != nil ||
		!validUTC(value.ExternalCommittedAt) {
		return fmt.Errorf("company lifecycle: effect commit is invalid")
	}
	for _, hash := range []contracts.ContentHash{
		value.RequestHash, value.OutcomeHash, value.ExternalReceiptHash,
	} {
		if err := hash.Validate(); err != nil {
			return err
		}
	}
	return nil
}

// EffectRecovery is one non-consumed effect returned after restart. PREPARED
// requires reconciliation; COMMITTED is final and must only be consumed.
type EffectRecovery struct {
	Status  EffectStatus
	Request EffectRequest
	Commit  *EffectCommit
}

// RecoveryState is the verified lifecycle checkpoint and every effect that may
// not be redispatched after a crash.
type RecoveryState struct {
	Checkpoint    Checkpoint
	LatestReceipt DecisionReceipt
	Effects       []EffectRecovery
}

type effectConsumption struct {
	SchemaVersion  string                   `json:"schema_version"`
	EffectID       EffectID                 `json:"effect_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   InitiativeID             `json:"initiative_id"`
	TransitionID   TransitionID             `json:"transition_id"`
	ConsumedAt     time.Time                `json:"consumed_at"`
}

func (value effectConsumption) Validate() error {
	if value.SchemaVersion != EffectSchemaVersion ||
		validateToken("effect id", string(value.EffectID)) != nil ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		validateToken("transition id", string(value.TransitionID)) != nil ||
		!validUTC(value.ConsumedAt) {
		return fmt.Errorf("company lifecycle: effect consumption is invalid")
	}
	return nil
}

// PrepareEffect durably records an external idempotency claim before dispatch.
// A crash after this call returns PREPARED from Recover and therefore requires
// provider reconciliation instead of blind replay.
func (store *Store) PrepareEffect(ctx context.Context, request EffectRequest) (EffectRecovery, error) {
	if err := request.Validate(); err != nil {
		return EffectRecovery{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return EffectRecovery{}, err
	}
	if !request.LeaseExpiresAt.After(now) {
		return EffectRecovery{}, ErrUnauthorized
	}
	prepareHash, err := contracts.HashCanonical(request)
	if err != nil {
		return EffectRecovery{}, err
	}
	canonical, err := contracts.EncodeCanonical(request)
	if err != nil {
		return EffectRecovery{}, err
	}
	sealed, err := store.vault.SealRecord(
		store.effectEventAD(request.OrganizationID, request.InitiativeID, request.ID, 1),
		canonical,
	)
	if err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: seal effect preparation: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: begin effect preparation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var existingID string
	err = tx.QueryRow(ctx, `
		SELECT effect_id
		FROM workforce_lifecycle_effect_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND (effect_id=$4 OR external_idempotency_key=$5)
		FOR UPDATE
	`, store.tenantID, request.OrganizationID, request.InitiativeID,
		request.ID, request.ExternalIdempotencyKey).Scan(&existingID)
	if err == nil {
		existing, loadErr := store.loadEffectRecovery(ctx, tx, request.OrganizationID, request.InitiativeID, EffectID(existingID))
		if loadErr != nil {
			return EffectRecovery{}, loadErr
		}
		existingHash, hashErr := contracts.HashCanonical(existing.Request)
		if hashErr != nil || existingHash != prepareHash {
			return EffectRecovery{}, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return EffectRecovery{}, err
		}
		return existing, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: inspect effect claim: %w", err)
	}
	checkpoint, _, _, err := store.loadCheckpointTx(
		ctx, tx, request.OrganizationID, request.InitiativeID, true,
	)
	if err != nil {
		return EffectRecovery{}, err
	}
	if checkpoint.State == StateTerminate {
		return EffectRecovery{}, ErrTerminal
	}
	if checkpoint.Version != request.ExpectedLifecycleVersion ||
		checkpoint.LastReceiptID != request.AuthorizationReceiptID ||
		checkpoint.Authority.RequestedBySeatID != request.PreparedBySeatID {
		return EffectRecovery{}, ErrConflict
	}
	receipt, err := store.loadReceiptRow(
		ctx, tx, request.OrganizationID, request.InitiativeID,
		request.AuthorizationReceiptID,
	)
	if err != nil {
		return EffectRecovery{}, err
	}
	if receipt.Verification.PolicyDecisionHash != request.AuthorizationDecisionHash ||
		!receipt.Verification.ExpiresAt.After(now) ||
		!checkpoint.Authority.ExpiresAt.After(now) {
		return EffectRecovery{}, ErrUnauthorized
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_effect_heads (
			tenant_id,organization_id,initiative_id,effect_id,status,
			expected_lifecycle_version,lease_id,fence,lease_expires_at,
			external_idempotency_key,external_request_hash,prepare_hash,sealed_prepare,
			commit_hash,sealed_commit,consumed_by_transition_id,updated_at
		) VALUES ($1,$2,$3,$4,'PREPARED',$5,$6,$7,$8,$9,$10,$11,$12,NULL,NULL,NULL,$13)
	`, store.tenantID, request.OrganizationID, request.InitiativeID, request.ID,
		request.ExpectedLifecycleVersion, request.LeaseID, request.Fence,
		request.LeaseExpiresAt, request.ExternalIdempotencyKey,
		request.RequestHash.Digest, prepareHash.Digest, sealed, now); err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: insert effect head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_effect_events (
			tenant_id,organization_id,initiative_id,effect_id,sequence,event_kind,
			event_hash,sealed_event,created_at
		) VALUES ($1,$2,$3,$4,1,'PREPARED',$5,$6,$7)
	`, store.tenantID, request.OrganizationID, request.InitiativeID,
		request.ID, prepareHash.Digest, sealed, now); err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: insert effect preparation event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: commit effect preparation: %w", err)
	}
	return EffectRecovery{Status: EffectPrepared, Request: request}, nil
}

// CommitEffect durably records a provider-confirmed external outcome. Repeated
// identical commits deduplicate; a different outcome for the same effect conflicts.
func (store *Store) CommitEffect(ctx context.Context, commit EffectCommit) (EffectRecovery, error) {
	if err := commit.Validate(); err != nil {
		return EffectRecovery{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return EffectRecovery{}, err
	}
	if commit.ExternalCommittedAt.After(now) {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: effect commit is future-dated")
	}
	commitHash, err := contracts.HashCanonical(commit)
	if err != nil {
		return EffectRecovery{}, err
	}
	canonical, err := contracts.EncodeCanonical(commit)
	if err != nil {
		return EffectRecovery{}, err
	}
	sealed, err := store.vault.SealRecord(
		store.effectEventAD(commit.OrganizationID, commit.InitiativeID, commit.EffectID, 2),
		canonical,
	)
	if err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: seal effect commit: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: begin effect commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var lockedID string
	if err := tx.QueryRow(ctx, `
		SELECT effect_id
		FROM workforce_lifecycle_effect_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 AND effect_id=$4
		FOR UPDATE
	`, store.tenantID, commit.OrganizationID, commit.InitiativeID, commit.EffectID).Scan(&lockedID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return EffectRecovery{}, ErrConflict
		}
		return EffectRecovery{}, fmt.Errorf("company lifecycle: lock effect commit: %w", err)
	}
	existing, err := store.loadEffectRecovery(ctx, tx, commit.OrganizationID, commit.InitiativeID, commit.EffectID)
	if err != nil {
		return EffectRecovery{}, err
	}
	if existing.Request.RequestHash != commit.RequestHash {
		return EffectRecovery{}, ErrConflict
	}
	if existing.Status == EffectCommitted || existing.Status == EffectConsumed {
		if existing.Commit == nil {
			return EffectRecovery{}, ErrIntegrity
		}
		existingHash, hashErr := contracts.HashCanonical(*existing.Commit)
		if hashErr != nil || existingHash != commitHash {
			return EffectRecovery{}, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return EffectRecovery{}, err
		}
		return existing, nil
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_lifecycle_effect_heads
		SET status='COMMITTED',commit_hash=$1,sealed_commit=$2,
			external_receipt_id=$3,external_receipt_hash=$4,updated_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND initiative_id=$8
		  AND effect_id=$9 AND status='PREPARED'
	`, commitHash.Digest, sealed, commit.ExternalReceiptID,
		commit.ExternalReceiptHash.Digest, now, store.tenantID, commit.OrganizationID,
		commit.InitiativeID, commit.EffectID)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return EffectRecovery{}, fmt.Errorf("company lifecycle: commit effect head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_effect_events (
			tenant_id,organization_id,initiative_id,effect_id,sequence,event_kind,
			event_hash,sealed_event,created_at
		) VALUES ($1,$2,$3,$4,2,'COMMITTED',$5,$6,$7)
	`, store.tenantID, commit.OrganizationID, commit.InitiativeID,
		commit.EffectID, commitHash.Digest, sealed, now); err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: insert effect commit event: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: finish effect commit: %w", err)
	}
	return EffectRecovery{Status: EffectCommitted, Request: existing.Request, Commit: &commit}, nil
}

// Recover loads the verified checkpoint and every PREPARED or COMMITTED effect.
// Callers must reconcile PREPARED effects and consume COMMITTED effects; neither
// state authorizes redispatch.
func (store *Store) Recover(ctx context.Context, organizationID contracts.OrganizationID, initiativeID InitiativeID) (RecoveryState, error) {
	checkpoint, err := store.Load(ctx, organizationID, initiativeID)
	if err != nil {
		return RecoveryState{}, err
	}
	latestReceipt, err := store.LoadReceipt(ctx, organizationID, initiativeID, checkpoint.LastReceiptID)
	if err != nil {
		return RecoveryState{}, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT effect_id
		FROM workforce_lifecycle_effect_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND status IN ('PREPARED','COMMITTED')
		ORDER BY effect_id
	`, store.tenantID, organizationID, initiativeID)
	if err != nil {
		return RecoveryState{}, fmt.Errorf("company lifecycle: load recoverable effects: %w", err)
	}
	effectIDs := make([]EffectID, 0)
	for rows.Next() {
		var effectID string
		if err := rows.Scan(&effectID); err != nil {
			rows.Close()
			return RecoveryState{}, err
		}
		effectIDs = append(effectIDs, EffectID(effectID))
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return RecoveryState{}, err
	}
	rows.Close()
	effects := make([]EffectRecovery, 0, len(effectIDs))
	for _, effectID := range effectIDs {
		recovery, err := store.loadEffectRecovery(
			ctx, store.pool, organizationID, initiativeID, effectID,
		)
		if err != nil {
			return RecoveryState{}, err
		}
		effects = append(effects, recovery)
	}
	return RecoveryState{Checkpoint: checkpoint, LatestReceipt: latestReceipt, Effects: effects}, nil
}

func (store *Store) requireCommittedEffects(ctx context.Context, tx pgx.Tx, request TransitionRequest, lifecycleVersion uint64) error {
	for _, effectID := range request.EffectIDs {
		var status string
		var expectedVersion uint64
		var consumedBy *string
		err := tx.QueryRow(ctx, `
			SELECT status,expected_lifecycle_version,consumed_by_transition_id
			FROM workforce_lifecycle_effect_heads
			WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 AND effect_id=$4
			FOR UPDATE
		`, store.tenantID, request.OrganizationID, request.InitiativeID, effectID).Scan(
			&status, &expectedVersion, &consumedBy,
		)
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w: effect %s does not exist", ErrEffectAmbiguous, effectID)
		}
		if err != nil {
			return fmt.Errorf("company lifecycle: lock effect %s: %w", effectID, err)
		}
		if EffectStatus(status) == EffectPrepared {
			return fmt.Errorf("%w: effect %s requires reconciliation", ErrEffectAmbiguous, effectID)
		}
		if EffectStatus(status) != EffectCommitted || expectedVersion != lifecycleVersion || consumedBy != nil {
			return fmt.Errorf("%w: effect %s cannot be consumed", ErrConflict, effectID)
		}
	}
	return nil
}

func (store *Store) consumeEffects(ctx context.Context, tx pgx.Tx, request TransitionRequest, now time.Time) error {
	for _, effectID := range request.EffectIDs {
		consumption := effectConsumption{
			SchemaVersion:  EffectSchemaVersion,
			EffectID:       effectID,
			OrganizationID: request.OrganizationID,
			InitiativeID:   request.InitiativeID,
			TransitionID:   request.TransitionID,
			ConsumedAt:     now,
		}
		consumptionHash, err := contracts.HashCanonical(consumption)
		if err != nil {
			return err
		}
		canonical, err := contracts.EncodeCanonical(consumption)
		if err != nil {
			return err
		}
		sealed, err := store.vault.SealRecord(
			store.effectEventAD(request.OrganizationID, request.InitiativeID, effectID, 3),
			canonical,
		)
		if err != nil {
			return fmt.Errorf("company lifecycle: seal effect consumption: %w", err)
		}
		command, err := tx.Exec(ctx, `
			UPDATE workforce_lifecycle_effect_heads
			SET status='CONSUMED',consumed_by_transition_id=$1,updated_at=$2
			WHERE tenant_id=$3 AND organization_id=$4 AND initiative_id=$5
			  AND effect_id=$6 AND status='COMMITTED'
		`, request.TransitionID, now, store.tenantID, request.OrganizationID,
			request.InitiativeID, effectID)
		if err != nil || command.RowsAffected() != 1 {
			if err == nil {
				err = ErrConflict
			}
			return fmt.Errorf("company lifecycle: consume effect %s: %w", effectID, err)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_lifecycle_effect_events (
				tenant_id,organization_id,initiative_id,effect_id,sequence,event_kind,
				event_hash,sealed_event,created_at
			) VALUES ($1,$2,$3,$4,3,'CONSUMED',$5,$6,$7)
		`, store.tenantID, request.OrganizationID, request.InitiativeID,
			effectID, consumptionHash.Digest, sealed, now); err != nil {
			return fmt.Errorf("company lifecycle: insert effect consumption event: %w", err)
		}
	}
	return nil
}

func (store *Store) loadEffectRecovery(
	ctx context.Context,
	querier queryRower,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	effectID EffectID,
) (EffectRecovery, error) {
	var status string
	var prepareHash string
	var sealedPrepare []byte
	var leaseID string
	var fence uint64
	var leaseExpiresAt time.Time
	var commitHash *string
	var sealedCommit []byte
	var externalReceiptID *string
	var externalReceiptHash *string
	err := querier.QueryRow(ctx, `
		SELECT status,prepare_hash,sealed_prepare,lease_id,fence,lease_expires_at,
		       commit_hash,sealed_commit,
		       external_receipt_id,external_receipt_hash
		FROM workforce_lifecycle_effect_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 AND effect_id=$4
	`, store.tenantID, organizationID, initiativeID, effectID).Scan(
		&status, &prepareHash, &sealedPrepare, &leaseID, &fence, &leaseExpiresAt,
		&commitHash, &sealedCommit,
		&externalReceiptID, &externalReceiptHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return EffectRecovery{}, ErrConflict
	}
	if err != nil {
		return EffectRecovery{}, fmt.Errorf("company lifecycle: load effect: %w", err)
	}
	if !EffectStatus(status).Valid() {
		return EffectRecovery{}, ErrIntegrity
	}
	opened, err := store.vault.OpenRecord(
		store.effectEventAD(organizationID, initiativeID, effectID, 1), sealedPrepare,
	)
	if err != nil || contentHash(opened).Digest != prepareHash {
		return EffectRecovery{}, ErrIntegrity
	}
	request, err := contracts.DecodeCanonical[EffectRequest, *EffectRequest](opened)
	if err != nil || request.OrganizationID != organizationID ||
		request.InitiativeID != initiativeID || request.ID != effectID ||
		string(request.LeaseID) != leaseID || uint64(request.Fence) != fence ||
		request.LeaseExpiresAt != leaseExpiresAt {
		return EffectRecovery{}, ErrIntegrity
	}
	recovery := EffectRecovery{Status: EffectStatus(status), Request: request}
	if recovery.Status == EffectPrepared {
		if commitHash != nil || len(sealedCommit) != 0 ||
			externalReceiptID != nil || externalReceiptHash != nil {
			return EffectRecovery{}, ErrIntegrity
		}
		return recovery, nil
	}
	if commitHash == nil || len(sealedCommit) == 0 ||
		externalReceiptID == nil || externalReceiptHash == nil {
		return EffectRecovery{}, ErrIntegrity
	}
	opened, err = store.vault.OpenRecord(
		store.effectEventAD(organizationID, initiativeID, effectID, 2), sealedCommit,
	)
	if err != nil || contentHash(opened).Digest != *commitHash {
		return EffectRecovery{}, ErrIntegrity
	}
	commit, err := contracts.DecodeCanonical[EffectCommit, *EffectCommit](opened)
	if err != nil || commit.OrganizationID != organizationID ||
		commit.InitiativeID != initiativeID || commit.EffectID != effectID ||
		commit.RequestHash != request.RequestHash ||
		commit.ExternalReceiptID != *externalReceiptID ||
		commit.ExternalReceiptHash.Digest != *externalReceiptHash {
		return EffectRecovery{}, ErrIntegrity
	}
	recovery.Commit = &commit
	return recovery, nil
}

func (store *Store) effectEventAD(
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	effectID EffectID,
	sequence uint64,
) vault.AD {
	return vault.AD{
		User:  store.tenantID,
		Store: "workforce.company.lifecycle.effect",
		Stream: string(organizationID) + "/" + string(initiativeID) + "/" +
			string(effectID) + "/" + strconv.FormatUint(sequence, 10),
		Schema: EffectSchemaVersion,
	}
}
