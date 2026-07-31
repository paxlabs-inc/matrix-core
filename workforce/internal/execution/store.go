package execution

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

var (
	ErrConflict          = errors.New("execution: checkpoint conflict")
	ErrTerminal          = errors.New("execution: wake is terminal")
	ErrInvalidTransition = errors.New("execution: invalid transition")
)

type Store struct {
	pool     *pgxpool.Pool
	vault    *vault.UserVault
	tenantID string
	now      func() time.Time
}

func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || strings.TrimSpace(tenantID) == "" || now == nil {
		return nil, fmt.Errorf("execution: pool, Vault, tenant, and time source are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("execution: Vault user does not match tenant")
	}
	return &Store{pool: pool, vault: userVault, tenantID: tenantID, now: now}, nil
}

func (store *Store) Start(ctx context.Context, packet contracts.WorkPacket) (State, error) {
	if err := packet.Validate(); err != nil {
		return State{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return State{}, err
	}
	if !packet.Lease.ExpiresAt.After(now) {
		return State{}, fmt.Errorf("%w: lease already expired", ErrTerminal)
	}
	canonical, err := contracts.EncodeCanonical(&packet)
	if err != nil {
		return State{}, err
	}
	sum := sha256.Sum256(canonical)
	state := State{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: packet.Lease.OrganizationID,
		WakeID:         packet.Lease.WakeID, LeaseID: packet.Lease.ID,
		SeatID: packet.Seat.ID, IntentID: packet.Intent.ID,
		Stage: StageLease, Version: 1, Budget: packet.Lease.Budget,
		StartedAt: now, LeaseExpiresAt: packet.Lease.ExpiresAt,
		PacketDigest: contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		},
		UpdatedAt: now,
	}
	hash, sealed, err := store.seal(state)
	if err != nil {
		return State{}, err
	}
	command, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_wake_checkpoints (
			tenant_id,organization_id,wake_id,lease_id,stage,version,
			disposition,state_hash,sealed_state,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,NULL,$7,$8,$9)
		ON CONFLICT DO NOTHING
	`, store.tenantID, state.OrganizationID, state.WakeID, state.LeaseID,
		state.Stage, state.Version, hash, sealed, now)
	if err != nil {
		return State{}, fmt.Errorf("execution: start checkpoint: %w", err)
	}
	if command.RowsAffected() == 0 {
		current, loadErr := store.Load(ctx, state.OrganizationID, state.WakeID)
		if loadErr != nil {
			return State{}, loadErr
		}
		if current.PacketDigest != state.PacketDigest || current.LeaseID != state.LeaseID {
			return State{}, ErrConflict
		}
		return current, nil
	}
	return state, nil
}

func (store *Store) Load(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
) (State, error) {
	var sealed []byte
	var expectedHash string
	err := store.pool.QueryRow(ctx, `
		SELECT sealed_state,state_hash
		FROM workforce_wake_checkpoints
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, store.tenantID, organizationID, wakeID).Scan(&sealed, &expectedHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return State{}, ErrConflict
	}
	if err != nil {
		return State{}, fmt.Errorf("execution: load checkpoint: %w", err)
	}
	return store.open(organizationID, wakeID, sealed, expectedHash)
}

func (store *Store) Resume(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
	idempotencyKey string,
) (State, error) {
	state, err := store.Load(ctx, organizationID, wakeID)
	if err != nil || state.Stage != StageExecute || state.PendingEffectID == "" {
		return state, err
	}
	return store.Advance(ctx, AdvanceRequest{
		OrganizationID: organizationID, WakeID: wakeID,
		ExpectedVersion: state.Version, Decision: DecisionEffectAmbiguous,
		IdempotencyKey: idempotencyKey, EffectID: state.PendingEffectID,
		ReasonCode: "crash_after_dispatch",
	})
}

func (store *Store) Advance(ctx context.Context, request AdvanceRequest) (State, error) {
	if err := validateAdvanceRequest(request); err != nil {
		return State{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return State{}, err
	}
	requestHash, err := hashRequest(request)
	if err != nil {
		return State{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return State{}, fmt.Errorf("execution: begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var sealed []byte
	var expectedHash string
	err = tx.QueryRow(ctx, `
		SELECT sealed_state,state_hash
		FROM workforce_wake_checkpoints
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
		FOR UPDATE
	`, store.tenantID, request.OrganizationID, request.WakeID).Scan(&sealed, &expectedHash)
	if err != nil {
		return State{}, fmt.Errorf("execution: lock checkpoint: %w", err)
	}
	state, err := store.open(request.OrganizationID, request.WakeID, sealed, expectedHash)
	if err != nil {
		return State{}, err
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT request_hash
		FROM workforce_wake_transitions
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
		  AND idempotency_key=$4
	`, store.tenantID, request.OrganizationID, request.WakeID,
		request.IdempotencyKey).Scan(&existingHash)
	if err == nil {
		if existingHash != requestHash {
			return State{}, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return State{}, err
		}
		return state, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return State{}, fmt.Errorf("execution: inspect idempotency: %w", err)
	}
	err = nil
	if state.Version != request.ExpectedVersion {
		return State{}, ErrConflict
	}
	if state.Stage == StageSleep {
		return State{}, ErrTerminal
	}
	next := state
	if now.Before(state.LeaseExpiresAt) {
		next, err = transition(state, request, now)
	} else {
		next = terminal(state, contracts.DispositionLeaseExpired, "lease_expired", now)
	}
	if err != nil {
		return State{}, err
	}
	next.Version++
	next.Steps++
	next.Usage.ModelCalls += request.Usage.ModelCalls
	next.Usage.ToolCalls += request.Usage.ToolCalls
	next.Usage.CostMinor += request.Usage.CostMinor
	if budgetExceeded(next, now) && next.Stage != StageSleep {
		next = terminal(next, contracts.DispositionBudgetExhausted, "budget_exhausted", now)
	}
	next.UpdatedAt = now
	if err := next.Validate(); err != nil {
		return State{}, err
	}
	nextHash, nextSealed, err := store.seal(next)
	if err != nil {
		return State{}, err
	}
	if state.Stage == StageCommit && next.Stage == StageYield {
		command, err := tx.Exec(ctx, `
			INSERT INTO workforce_wake_commits (
				tenant_id,organization_id,wake_id,receipt_id,effect_id,committed_at
			) VALUES ($1,$2,$3,$4,NULLIF($5,''),$6)
			ON CONFLICT DO NOTHING
		`, store.tenantID, state.OrganizationID, state.WakeID,
			next.ReceiptID, next.PendingEffectID, now)
		if err != nil {
			return State{}, fmt.Errorf("execution: commit marker: %w", err)
		}
		if command.RowsAffected() == 0 {
			var receiptID string
			var effectID *string
			if err := tx.QueryRow(ctx, `
				SELECT receipt_id,effect_id
				FROM workforce_wake_commits
				WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
			`, store.tenantID, state.OrganizationID, state.WakeID).Scan(
				&receiptID, &effectID,
			); err != nil || receiptID != string(next.ReceiptID) ||
				(effectID == nil) != (next.PendingEffectID == "") ||
				(effectID != nil && *effectID != next.PendingEffectID) {
				return State{}, ErrConflict
			}
		}
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_wake_checkpoints
		SET stage=$1,version=$2,disposition=NULLIF($3,''),state_hash=$4,
			sealed_state=$5,updated_at=$6
		WHERE tenant_id=$7 AND organization_id=$8 AND wake_id=$9 AND version=$10
	`, next.Stage, next.Version, next.Disposition, nextHash, nextSealed, now,
		store.tenantID, next.OrganizationID, next.WakeID, state.Version)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return State{}, fmt.Errorf("execution: persist checkpoint: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_wake_transitions (
			tenant_id,organization_id,wake_id,sequence,from_stage,to_stage,
			decision,idempotency_key,request_hash,state_hash,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
	`, store.tenantID, next.OrganizationID, next.WakeID, next.Version,
		state.Stage, next.Stage, request.Decision, request.IdempotencyKey,
		requestHash, nextHash, now); err != nil {
		return State{}, fmt.Errorf("execution: record transition: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return State{}, fmt.Errorf("execution: commit transition: %w", err)
	}
	return next, nil
}

func transition(state State, request AdvanceRequest, now time.Time) (State, error) {
	if disposition, terminalDecision := decisionDisposition(request.Decision); terminalDecision {
		if request.Decision == DecisionComplete && (state.Stage != StageYield || state.ReceiptID == "") {
			return State{}, fmt.Errorf("%w: completion requires committed receipt and yield", ErrInvalidTransition)
		}
		return terminal(state, disposition, request.ReasonCode, now), nil
	}
	switch {
	case state.Stage == StageExecute && request.Decision == DecisionDispatch:
		if state.PendingEffectID != "" || request.EffectID == "" {
			return State{}, ErrInvalidTransition
		}
		state.PendingEffectID = request.EffectID
		return state, nil
	case state.Stage == StageExecute && request.Decision == DecisionObserved:
		if state.PendingEffectID == "" || request.EffectID != state.PendingEffectID {
			return State{}, ErrInvalidTransition
		}
		state.Stage = StageObserve
		return state, nil
	case state.Stage == StageExecute && request.Decision == DecisionEffectAmbiguous:
		if state.PendingEffectID == "" || request.EffectID != state.PendingEffectID {
			return State{}, ErrInvalidTransition
		}
		state.ResumeStage = StageExecute
		state.Stage = StageReconcile
		return state, nil
	case state.Stage == StageReconcile && state.ResumeStage == StageExecute &&
		request.Decision == DecisionReconcileCompleted:
		if request.EffectID != state.PendingEffectID {
			return State{}, ErrInvalidTransition
		}
		state.ResumeStage = ""
		state.Stage = StageObserve
		return state, nil
	case state.Stage == StageReconcile && state.ResumeStage == StageExecute &&
		request.Decision == DecisionReconcileUnchanged:
		if request.EffectID != state.PendingEffectID {
			return State{}, ErrInvalidTransition
		}
		state.ResumeStage = ""
		state.PendingEffectID = ""
		state.Stage = StageExecute
		return state, nil
	case request.Decision != DecisionAdvance:
		return State{}, ErrInvalidTransition
	}
	if state.Stage == StageExecute {
		if state.PendingEffectID != "" {
			return State{}, ErrInvalidTransition
		}
		state.Stage = StageObserve
		return state, nil
	}
	if state.Stage == StageCommit {
		if request.ReceiptID == "" {
			return State{}, fmt.Errorf("%w: commit requires receipt identity", ErrInvalidTransition)
		}
		state.ReceiptID = request.ReceiptID
		state.Stage = StageYield
		return state, nil
	}
	if state.Stage == StageYield {
		if request.FinalDisposition != contracts.DispositionProgressed &&
			request.FinalDisposition != contracts.DispositionGoalCompleted {
			return State{}, fmt.Errorf("%w: yield requires progressed or completed disposition", ErrInvalidTransition)
		}
		return terminal(state, request.FinalDisposition, request.ReasonCode, now), nil
	}
	next, ok := nextStage(state.Stage)
	if !ok || state.Stage == StageReconcile && state.ResumeStage != "" {
		return State{}, ErrInvalidTransition
	}
	state.Stage = next
	return state, nil
}

func nextStage(stage Stage) (Stage, bool) {
	for index := range orderedStages {
		if orderedStages[index] == stage && index+1 < len(orderedStages) {
			return orderedStages[index+1], true
		}
	}
	return "", false
}

func decisionDisposition(decision Decision) (contracts.WakeDisposition, bool) {
	switch decision {
	case DecisionWaitDependency:
		return contracts.DispositionWaitingDependency, true
	case DecisionWaitApproval:
		return contracts.DispositionWaitingApproval, true
	case DecisionBlock:
		return contracts.DispositionBlocked, true
	case DecisionComplete:
		return contracts.DispositionGoalCompleted, true
	case DecisionExhaustBudget:
		return contracts.DispositionBudgetExhausted, true
	case DecisionExpireLease:
		return contracts.DispositionLeaseExpired, true
	case DecisionCancel:
		return contracts.DispositionCancelled, true
	case DecisionFail:
		return contracts.DispositionFailed, true
	default:
		return "", false
	}
}

func terminal(
	state State,
	disposition contracts.WakeDisposition,
	reason string,
	now time.Time,
) State {
	state.Stage = StageSleep
	state.ResumeStage = ""
	state.Disposition = disposition
	state.ReasonCode = reason
	state.UpdatedAt = now
	return state
}

func budgetExceeded(state State, now time.Time) bool {
	return state.Steps > state.Budget.MaxSteps ||
		state.Usage.ModelCalls > state.Budget.MaxModelCalls ||
		state.Usage.ToolCalls > state.Budget.MaxToolCalls ||
		state.Usage.CostMinor > state.Budget.MaxCostMinor ||
		now.Sub(state.StartedAt) > time.Duration(state.Budget.MaxDurationMillis)*time.Millisecond
}

func validateAdvanceRequest(request AdvanceRequest) error {
	if request.OrganizationID == "" || request.WakeID == "" ||
		request.ExpectedVersion == 0 || !request.Decision.Valid() ||
		strings.TrimSpace(request.IdempotencyKey) == "" ||
		len(request.IdempotencyKey) > 128 || request.Usage.CostMinor < 0 {
		return fmt.Errorf("execution: transition request is invalid")
	}
	return nil
}

func hashRequest(request AdvanceRequest) (string, error) {
	value := strings.Join([]string{
		string(request.OrganizationID), string(request.WakeID),
		strconv.FormatUint(request.ExpectedVersion, 10), string(request.Decision),
		request.IdempotencyKey,
		strconv.FormatUint(uint64(request.Usage.ModelCalls), 10),
		strconv.FormatUint(uint64(request.Usage.ToolCalls), 10),
		strconv.FormatInt(request.Usage.CostMinor, 10),
		request.EffectID, string(request.ReceiptID),
		string(request.FinalDisposition), request.ReasonCode,
	}, "\x1f")
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:]), nil
}

func (store *Store) seal(state State) (string, []byte, error) {
	canonical, err := contracts.EncodeCanonical(&state)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	sealed, err := store.vault.SealRecord(store.ad(state.OrganizationID, state.WakeID), canonical)
	if err != nil {
		return "", nil, fmt.Errorf("execution: seal checkpoint: %w", err)
	}
	return hash, sealed, nil
}

func (store *Store) open(
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
	sealed []byte,
	expectedHash string,
) (State, error) {
	canonical, err := store.vault.OpenRecord(store.ad(organizationID, wakeID), sealed)
	if err != nil {
		return State{}, fmt.Errorf("execution: open checkpoint: %w", err)
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != expectedHash {
		return State{}, fmt.Errorf("execution: checkpoint integrity failure")
	}
	state, err := contracts.DecodeCanonical[State, *State](canonical)
	if err != nil || state.Validate() != nil ||
		state.OrganizationID != organizationID || state.WakeID != wakeID {
		return State{}, fmt.Errorf("execution: invalid checkpoint")
	}
	return state, nil
}

func (store *Store) ad(
	organizationID contracts.OrganizationID,
	wakeID contracts.WakeID,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.execution.checkpoint",
		Stream: string(organizationID) + "/" + string(wakeID),
		Schema: contracts.SchemaVersionV1,
	}
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("execution: time source must return UTC")
	}
	return now, nil
}
