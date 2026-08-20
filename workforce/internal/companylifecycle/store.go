package companylifecycle

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

// Store owns durable initiative checkpoints, transition journals, decision
// receipts, and crash-recoverable effect state for one tenant.
type Store struct {
	pool       *pgxpool.Pool
	vault      *vault.UserVault
	tenantID   string
	keyID      string
	privateKey ed25519.PrivateKey
	publicKey  ed25519.PublicKey
	verifier   GateVerifier
	now        func() time.Time
}

// New constructs a fail-closed lifecycle store with transactional gate
// verification and a dedicated receipt-signing authority.
func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	keyID string,
	privateKey ed25519.PrivateKey,
	verifier GateVerifier,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || strings.TrimSpace(tenantID) == "" ||
		validateToken("receipt signing key id", keyID) != nil ||
		len(privateKey) != ed25519.PrivateKeySize || verifier == nil || now == nil {
		return nil, fmt.Errorf("company lifecycle: durable store, Vault, verifier, signing authority, and time source are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("company lifecycle: Vault user does not match tenant")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Store{
		pool:       pool,
		vault:      userVault,
		tenantID:   tenantID,
		keyID:      keyID,
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
		publicKey:  append(ed25519.PublicKey(nil), publicKey...),
		verifier:   verifier,
		now:        now,
	}, nil
}

// CreateInitiative atomically installs an immutable initiative, its DISCOVER
// checkpoint, initialization journal record, and signed decision receipt.
func (store *Store) CreateInitiative(ctx context.Context, request CreateRequest) (TransitionResult, error) {
	if err := request.Validate(); err != nil {
		return TransitionResult{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return TransitionResult{}, err
	}
	if request.Initiative.CreatedAt.After(now) ||
		request.Authority.OrganizationID != request.Initiative.OrganizationID ||
		request.CompanyState.OrganizationID != request.Initiative.OrganizationID ||
		request.Correction.OrganizationID != request.Initiative.OrganizationID {
		return TransitionResult{}, fmt.Errorf("company lifecycle: create request organization or time is invalid")
	}
	requestHash, err := contracts.HashCanonical(request)
	if err != nil {
		return TransitionResult{}, err
	}
	initiativeHash, err := contracts.HashCanonical(request.Initiative)
	if err != nil {
		return TransitionResult{}, err
	}
	initiativeCanonical, err := contracts.EncodeCanonical(request.Initiative)
	if err != nil {
		return TransitionResult{}, err
	}
	sealedInitiative, err := store.vault.SealRecord(
		store.initiativeAD(request.Initiative.OrganizationID, request.Initiative.ID),
		initiativeCanonical,
	)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("company lifecycle: seal initiative: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("company lifecycle: begin initialization: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if result, found, err := store.duplicateResult(
		ctx, tx, request.Initiative.OrganizationID, request.Initiative.ID,
		request.IdempotencyKey, requestHash,
	); err != nil || found {
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			return TransitionResult{}, err
		}
		result.Deduplicated = true
		return result, nil
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT initiative_hash
		FROM workforce_lifecycle_initiatives
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
	`, store.tenantID, request.Initiative.OrganizationID, request.Initiative.ID).Scan(&existingHash)
	if err == nil {
		return TransitionResult{}, ErrConflict
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return TransitionResult{}, fmt.Errorf("company lifecycle: inspect initiative: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_initiatives (
			tenant_id,organization_id,initiative_id,opportunity_id,portfolio_id,
			initiative_hash,sealed_initiative,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, store.tenantID, request.Initiative.OrganizationID, request.Initiative.ID,
		request.Initiative.OpportunityID, request.Initiative.PortfolioID,
		initiativeHash.Digest, sealedInitiative, request.Initiative.CreatedAt); err != nil {
		return TransitionResult{}, fmt.Errorf("company lifecycle: insert initiative: %w", err)
	}
	verificationRequest := GateVerificationRequest{
		TransitionID:    request.TransitionID,
		OrganizationID:  request.Initiative.OrganizationID,
		InitiativeID:    request.Initiative.ID,
		FromState:       "",
		ToState:         StateDiscover,
		Decision:        DecisionInitialize,
		ExpectedVersion: 0,
		RequestHash:     requestHash,
		Authority:       request.Authority,
		CompanyState:    request.CompanyState,
		Correction:      request.Correction,
		Evidence:        request.Evidence,
		CapitalImpact:   request.CapitalImpact,
		VerifiedAt:      now,
	}
	grant, err := store.verifier.VerifyLifecycleGate(ctx, tx, verificationRequest)
	if err != nil {
		return TransitionResult{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if err := verifyGateInputs(
		now, request.TransitionID, request.Initiative.OrganizationID,
		request.Initiative.ID, request.Authority, request.CompanyState,
		request.Correction, request.Evidence, request.CapitalImpact,
		requiredEvidence("", StateDiscover, DecisionInitialize), grant,
	); err != nil {
		return TransitionResult{}, err
	}
	before := initialCapital(request.CapitalImpact)
	after, err := applyCapital(before, request.CapitalImpact, grant.Limits, StateDiscover)
	if err != nil {
		return TransitionResult{}, err
	}
	checkpoint := Checkpoint{
		SchemaVersion:    CheckpointSchemaVersion,
		OrganizationID:   request.Initiative.OrganizationID,
		InitiativeID:     request.Initiative.ID,
		State:            StateDiscover,
		Version:          1,
		CompanyState:     request.CompanyState,
		Authority:        request.Authority,
		Capital:          after,
		LastTransitionID: request.TransitionID,
		LastReceiptID:    request.ReceiptID,
		CreatedAt:        now,
		UpdatedAt:        now,
	}
	result, sealedRequest, sealedCheckpoint, sealedReceipt, err := store.materializeTransition(
		request, requestHash, checkpoint, DecisionInitialize, "", grant,
		before, request.CapitalImpact, nil, now,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	if err := store.insertTransition(
		ctx, tx, request.Initiative.OrganizationID, request.Initiative.ID,
		request.TransitionID, request.ReceiptID, request.IdempotencyKey,
		requestHash, result, sealedRequest, sealedCheckpoint, sealedReceipt,
	); err != nil {
		return TransitionResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_heads (
			tenant_id,organization_id,initiative_id,state,resume_state,version,
			checkpoint_hash,sealed_checkpoint,last_transition_id,last_receipt_id,
			last_receipt_hash,updated_at,terminated_at
		) VALUES ($1,$2,$3,$4,NULL,$5,$6,$7,$8,$9,$10,$11,NULL)
	`, store.tenantID, checkpoint.OrganizationID, checkpoint.InitiativeID,
		checkpoint.State, checkpoint.Version, result.Receipt.CheckpointHash.Digest,
		sealedCheckpoint, checkpoint.LastTransitionID, checkpoint.LastReceiptID,
		result.Receipt.ContentHash.Digest, now); err != nil {
		return TransitionResult{}, fmt.Errorf("company lifecycle: insert checkpoint head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TransitionResult{}, fmt.Errorf("company lifecycle: commit initialization: %w", err)
	}
	return result, nil
}

// Transition atomically verifies and commits one optimistic lifecycle edge.
func (store *Store) Transition(ctx context.Context, request TransitionRequest) (TransitionResult, error) {
	if err := request.Validate(); err != nil {
		return TransitionResult{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return TransitionResult{}, err
	}
	requestHash, err := contracts.HashCanonical(request)
	if err != nil {
		return TransitionResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("company lifecycle: begin transition: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if result, found, err := store.duplicateResult(
		ctx, tx, request.OrganizationID, request.InitiativeID,
		request.IdempotencyKey, requestHash,
	); err != nil || found {
		if err == nil {
			err = tx.Commit(ctx)
		}
		if err != nil {
			return TransitionResult{}, err
		}
		result.Deduplicated = true
		return result, nil
	}
	current, currentHash, _, err := store.loadCheckpointTx(
		ctx, tx, request.OrganizationID, request.InitiativeID, true,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	if current.State == StateTerminate {
		return TransitionResult{}, ErrTerminal
	}
	if current.Version != request.ExpectedVersion || current.State != request.FromState {
		return TransitionResult{}, ErrConflict
	}
	if !allowedTransition(current.State, request.ToState, request.Decision, current.ResumeState) {
		return TransitionResult{}, ErrInvalidTransition
	}
	if request.Authority.OrganizationID != request.OrganizationID ||
		request.CompanyState.OrganizationID != request.OrganizationID ||
		request.Correction.OrganizationID != request.OrganizationID {
		return TransitionResult{}, fmt.Errorf("%w: cross-organization gate binding", ErrUnauthorized)
	}
	if err := store.requireCommittedEffects(ctx, tx, request, current.Version); err != nil {
		return TransitionResult{}, err
	}
	grant, err := store.verifier.VerifyLifecycleGate(ctx, tx, GateVerificationRequest{
		TransitionID:    request.TransitionID,
		OrganizationID:  request.OrganizationID,
		InitiativeID:    request.InitiativeID,
		FromState:       request.FromState,
		ToState:         request.ToState,
		Decision:        request.Decision,
		ExpectedVersion: request.ExpectedVersion,
		RequestHash:     requestHash,
		Authority:       request.Authority,
		CompanyState:    request.CompanyState,
		Correction:      request.Correction,
		Evidence:        request.Evidence,
		CapitalImpact:   request.CapitalImpact,
		VerifiedAt:      now,
	})
	if err != nil {
		return TransitionResult{}, fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if err := verifyGateInputs(
		now, request.TransitionID, request.OrganizationID, request.InitiativeID,
		request.Authority, request.CompanyState, request.Correction,
		request.Evidence, request.CapitalImpact,
		requiredEvidence(request.FromState, request.ToState, request.Decision), grant,
	); err != nil {
		return TransitionResult{}, err
	}
	after, err := applyCapital(current.Capital, request.CapitalImpact, grant.Limits, request.ToState)
	if err != nil {
		return TransitionResult{}, err
	}
	next := current
	next.State = request.ToState
	next.Version++
	next.CompanyState = request.CompanyState
	next.Authority = request.Authority
	next.Capital = after
	next.LastTransitionID = request.TransitionID
	next.LastReceiptID = request.ReceiptID
	next.UpdatedAt = now
	next.TerminatedAt = nil
	switch request.Decision {
	case DecisionPause:
		next.ResumeState = current.State
	case DecisionResume:
		next.ResumeState = ""
	default:
		next.ResumeState = ""
	}
	if request.ToState == StateTerminate {
		terminatedAt := now
		next.TerminatedAt = &terminatedAt
	}
	if err := next.Validate(); err != nil {
		return TransitionResult{}, err
	}
	result, sealedRequest, sealedCheckpoint, sealedReceipt, err := store.materializeTransition(
		request, requestHash, next, request.Decision, current.State, grant,
		current.Capital, request.CapitalImpact, request.EffectIDs, now,
	)
	if err != nil {
		return TransitionResult{}, err
	}
	if err := store.insertTransition(
		ctx, tx, request.OrganizationID, request.InitiativeID,
		request.TransitionID, request.ReceiptID, request.IdempotencyKey,
		requestHash, result, sealedRequest, sealedCheckpoint, sealedReceipt,
	); err != nil {
		return TransitionResult{}, err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_lifecycle_heads
		SET state=$1,resume_state=NULLIF($2,''),version=$3,checkpoint_hash=$4,
			sealed_checkpoint=$5,last_transition_id=$6,last_receipt_id=$7,
			last_receipt_hash=$8,updated_at=$9,terminated_at=$10
		WHERE tenant_id=$11 AND organization_id=$12 AND initiative_id=$13
		  AND version=$14 AND checkpoint_hash=$15
	`, next.State, next.ResumeState, next.Version,
		result.Receipt.CheckpointHash.Digest, sealedCheckpoint,
		next.LastTransitionID, next.LastReceiptID, result.Receipt.ContentHash.Digest,
		now, next.TerminatedAt, store.tenantID, request.OrganizationID,
		request.InitiativeID, current.Version, currentHash)
	if err != nil || command.RowsAffected() != 1 {
		if err == nil {
			err = ErrConflict
		}
		return TransitionResult{}, fmt.Errorf("company lifecycle: update checkpoint head: %w", err)
	}
	if err := store.consumeEffects(ctx, tx, request, now); err != nil {
		return TransitionResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return TransitionResult{}, fmt.Errorf("company lifecycle: commit transition: %w", err)
	}
	return result, nil
}

// Load returns the current verified checkpoint after process restart.
func (store *Store) Load(ctx context.Context, organizationID contracts.OrganizationID, initiativeID InitiativeID) (Checkpoint, error) {
	if validateToken("organization id", string(organizationID)) != nil ||
		validateToken("initiative id", string(initiativeID)) != nil {
		return Checkpoint{}, fmt.Errorf("company lifecycle: checkpoint identity is invalid")
	}
	checkpoint, checkpointHash, receiptHash, err := store.loadCheckpointRow(
		ctx, store.pool, organizationID, initiativeID, false,
	)
	if err != nil {
		return Checkpoint{}, err
	}
	receipt, err := store.LoadReceipt(ctx, organizationID, initiativeID, checkpoint.LastReceiptID)
	if err != nil || receipt.ContentHash.Digest != receiptHash ||
		receipt.CheckpointHash.Digest != checkpointHash {
		return Checkpoint{}, ErrIntegrity
	}
	return checkpoint, nil
}

// LoadInitiative opens and verifies the immutable typed initiative identity.
func (store *Store) LoadInitiative(ctx context.Context, organizationID contracts.OrganizationID, initiativeID InitiativeID) (Initiative, error) {
	if validateToken("organization id", string(organizationID)) != nil ||
		validateToken("initiative id", string(initiativeID)) != nil {
		return Initiative{}, fmt.Errorf("company lifecycle: initiative identity is invalid")
	}
	var expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT initiative_hash,sealed_initiative
		FROM workforce_lifecycle_initiatives
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
	`, store.tenantID, organizationID, initiativeID).Scan(&expectedHash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Initiative{}, ErrConflict
	}
	if err != nil {
		return Initiative{}, fmt.Errorf("company lifecycle: load initiative: %w", err)
	}
	opened, err := store.vault.OpenRecord(store.initiativeAD(organizationID, initiativeID), sealed)
	if err != nil || contentHash(opened).Digest != expectedHash {
		return Initiative{}, ErrIntegrity
	}
	initiative, err := contracts.DecodeCanonical[Initiative, *Initiative](opened)
	if err != nil || initiative.OrganizationID != organizationID || initiative.ID != initiativeID {
		return Initiative{}, ErrIntegrity
	}
	return initiative, nil
}

// LoadHistory returns up to one thousand verified immutable decision receipts
// strictly after afterVersion, ordered by resulting lifecycle version.
func (store *Store) LoadHistory(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	afterVersion uint64,
	limit uint32,
) ([]DecisionReceipt, error) {
	if validateToken("organization id", string(organizationID)) != nil ||
		validateToken("initiative id", string(initiativeID)) != nil ||
		limit == 0 || limit > 1000 {
		return nil, fmt.Errorf("company lifecycle: history request is invalid")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT receipt_id,receipt_hash,checkpoint_hash
		FROM workforce_lifecycle_transition_journal
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND sequence > $4
		ORDER BY sequence
		LIMIT $5
	`, store.tenantID, organizationID, initiativeID, afterVersion, limit)
	if err != nil {
		return nil, fmt.Errorf("company lifecycle: load history: %w", err)
	}
	type historyRef struct {
		receiptID      DecisionReceiptID
		receiptHash    string
		checkpointHash string
	}
	refs := make([]historyRef, 0, limit)
	for rows.Next() {
		var ref historyRef
		if err := rows.Scan(&ref.receiptID, &ref.receiptHash, &ref.checkpointHash); err != nil {
			rows.Close()
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	receipts := make([]DecisionReceipt, 0, len(refs))
	for _, ref := range refs {
		receipt, err := store.LoadReceipt(ctx, organizationID, initiativeID, ref.receiptID)
		if err != nil || receipt.ContentHash.Digest != ref.receiptHash ||
			receipt.CheckpointHash.Digest != ref.checkpointHash {
			return nil, ErrIntegrity
		}
		receipts = append(receipts, receipt)
	}
	return receipts, nil
}

// LoadReceipt opens and cryptographically verifies an immutable decision receipt.
func (store *Store) LoadReceipt(ctx context.Context, organizationID contracts.OrganizationID, initiativeID InitiativeID, receiptID DecisionReceiptID) (DecisionReceipt, error) {
	if validateToken("organization id", string(organizationID)) != nil ||
		validateToken("initiative id", string(initiativeID)) != nil ||
		validateToken("receipt id", string(receiptID)) != nil {
		return DecisionReceipt{}, fmt.Errorf("company lifecycle: receipt identity is invalid")
	}
	return store.loadReceiptRow(ctx, store.pool, organizationID, initiativeID, receiptID)
}

func (store *Store) materializeTransition(
	request contracts.Validatable,
	requestHash contracts.ContentHash,
	checkpoint Checkpoint,
	decision Decision,
	from State,
	grant GateVerificationGrant,
	capitalBefore CapitalSnapshot,
	impact CapitalImpact,
	effectIDs []EffectID,
	now time.Time,
) (TransitionResult, []byte, []byte, []byte, error) {
	requestCanonical, err := contracts.EncodeCanonical(request)
	if err != nil {
		return TransitionResult{}, nil, nil, nil, err
	}
	var transitionID TransitionID
	var receiptID DecisionReceiptID
	switch value := request.(type) {
	case CreateRequest:
		transitionID, receiptID = value.TransitionID, value.ReceiptID
	case TransitionRequest:
		transitionID, receiptID = value.TransitionID, value.ReceiptID
	default:
		return TransitionResult{}, nil, nil, nil, fmt.Errorf("company lifecycle: unsupported transition request")
	}
	checkpointHash, err := contracts.HashCanonical(checkpoint)
	if err != nil {
		return TransitionResult{}, nil, nil, nil, err
	}
	receipt := DecisionReceipt{
		SchemaVersion:    ReceiptSchemaVersion,
		ID:               receiptID,
		TransitionID:     transitionID,
		OrganizationID:   checkpoint.OrganizationID,
		InitiativeID:     checkpoint.InitiativeID,
		FromState:        from,
		ToState:          checkpoint.State,
		Decision:         decision,
		ExpectedVersion:  checkpoint.Version - 1,
		ResultingVersion: checkpoint.Version,
		RequestHash:      requestHash,
		Verification:     grant,
		CapitalBefore:    capitalBefore,
		CapitalImpact:    impact,
		CapitalAfter:     checkpoint.Capital,
		EffectIDs:        append([]EffectID(nil), effectIDs...),
		CheckpointHash:   checkpointHash,
		CreatedAt:        now,
	}
	if err := store.signReceipt(&receipt); err != nil {
		return TransitionResult{}, nil, nil, nil, err
	}
	checkpointCanonical, err := contracts.EncodeCanonical(checkpoint)
	if err != nil {
		return TransitionResult{}, nil, nil, nil, err
	}
	receiptCanonical, err := contracts.EncodeCanonical(receipt)
	if err != nil {
		return TransitionResult{}, nil, nil, nil, err
	}
	sealedRequest, err := store.vault.SealRecord(
		store.transitionAD(checkpoint.OrganizationID, checkpoint.InitiativeID, transitionID),
		requestCanonical,
	)
	if err != nil {
		return TransitionResult{}, nil, nil, nil, fmt.Errorf("company lifecycle: seal transition request: %w", err)
	}
	sealedCheckpoint, err := store.vault.SealRecord(
		store.checkpointAD(checkpoint.OrganizationID, checkpoint.InitiativeID, checkpoint.Version),
		checkpointCanonical,
	)
	if err != nil {
		return TransitionResult{}, nil, nil, nil, fmt.Errorf("company lifecycle: seal checkpoint: %w", err)
	}
	sealedReceipt, err := store.vault.SealRecord(
		store.receiptAD(checkpoint.OrganizationID, checkpoint.InitiativeID, receipt.ID),
		receiptCanonical,
	)
	if err != nil {
		return TransitionResult{}, nil, nil, nil, fmt.Errorf("company lifecycle: seal receipt: %w", err)
	}
	return TransitionResult{Checkpoint: checkpoint, Receipt: receipt}, sealedRequest, sealedCheckpoint, sealedReceipt, nil
}

func (store *Store) insertTransition(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	transitionID TransitionID,
	receiptID DecisionReceiptID,
	idempotencyKey string,
	requestHash contracts.ContentHash,
	result TransitionResult,
	sealedRequest []byte,
	sealedCheckpoint []byte,
	sealedReceipt []byte,
) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_decision_receipts (
			tenant_id,organization_id,initiative_id,receipt_id,transition_id,
			receipt_hash,sealed_receipt,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, store.tenantID, organizationID, initiativeID, receiptID, transitionID,
		result.Receipt.ContentHash.Digest, sealedReceipt, result.Receipt.CreatedAt); err != nil {
		return fmt.Errorf("company lifecycle: insert decision receipt: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_transition_journal (
			tenant_id,organization_id,initiative_id,sequence,transition_id,
			from_state,to_state,decision,idempotency_key,request_hash,sealed_request,
			checkpoint_hash,sealed_checkpoint,receipt_id,receipt_hash,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
	`, store.tenantID, organizationID, initiativeID, result.Checkpoint.Version,
		transitionID, result.Receipt.FromState, result.Receipt.ToState,
		result.Receipt.Decision, idempotencyKey, requestHash.Digest,
		sealedRequest, result.Receipt.CheckpointHash.Digest, sealedCheckpoint,
		receiptID, result.Receipt.ContentHash.Digest, result.Receipt.CreatedAt); err != nil {
		return fmt.Errorf("company lifecycle: insert transition journal: %w", err)
	}
	return nil
}

func (store *Store) duplicateResult(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	idempotencyKey string,
	requestHash contracts.ContentHash,
) (TransitionResult, bool, error) {
	var existingHash string
	var checkpointHash string
	var sealedCheckpoint []byte
	var receiptID string
	var receiptHash string
	var version uint64
	err := tx.QueryRow(ctx, `
		SELECT request_hash,checkpoint_hash,sealed_checkpoint,receipt_id,
		       receipt_hash,sequence
		FROM workforce_lifecycle_transition_journal
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3
		  AND idempotency_key=$4
	`, store.tenantID, organizationID, initiativeID, idempotencyKey).Scan(
		&existingHash, &checkpointHash, &sealedCheckpoint, &receiptID,
		&receiptHash, &version,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return TransitionResult{}, false, nil
	}
	if err != nil {
		return TransitionResult{}, false, fmt.Errorf("company lifecycle: inspect idempotency: %w", err)
	}
	if existingHash != requestHash.Digest {
		return TransitionResult{}, false, ErrConflict
	}
	checkpoint, err := store.openCheckpoint(
		organizationID, initiativeID, version, checkpointHash, sealedCheckpoint,
	)
	if err != nil {
		return TransitionResult{}, false, err
	}
	receipt, err := store.loadReceiptRow(
		ctx, tx, organizationID, initiativeID, DecisionReceiptID(receiptID),
	)
	if err != nil || receipt.ContentHash.Digest != receiptHash ||
		receipt.CheckpointHash.Digest != checkpointHash {
		return TransitionResult{}, false, ErrIntegrity
	}
	return TransitionResult{Checkpoint: checkpoint, Receipt: receipt}, true, nil
}

type queryRower interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (store *Store) loadCheckpointTx(
	ctx context.Context,
	tx pgx.Tx,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	forUpdate bool,
) (Checkpoint, string, string, error) {
	return store.loadCheckpointRow(ctx, tx, organizationID, initiativeID, forUpdate)
}

func (store *Store) loadCheckpointRow(
	ctx context.Context,
	querier queryRower,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	forUpdate bool,
) (Checkpoint, string, string, error) {
	query := `
		SELECT state,COALESCE(resume_state,''),version,checkpoint_hash,
		       sealed_checkpoint,last_transition_id,last_receipt_id,last_receipt_hash
		FROM workforce_lifecycle_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3`
	if forUpdate {
		query += " FOR UPDATE"
	}
	var state string
	var resume string
	var version uint64
	var checkpointHash string
	var sealed []byte
	var transitionID string
	var receiptID string
	var receiptHash string
	err := querier.QueryRow(ctx, query, store.tenantID, organizationID, initiativeID).Scan(
		&state, &resume, &version, &checkpointHash, &sealed,
		&transitionID, &receiptID, &receiptHash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Checkpoint{}, "", "", ErrConflict
	}
	if err != nil {
		return Checkpoint{}, "", "", fmt.Errorf("company lifecycle: load checkpoint: %w", err)
	}
	checkpoint, err := store.openCheckpoint(
		organizationID, initiativeID, version, checkpointHash, sealed,
	)
	if err != nil || checkpoint.State != State(state) || checkpoint.ResumeState != State(resume) ||
		checkpoint.LastTransitionID != TransitionID(transitionID) ||
		checkpoint.LastReceiptID != DecisionReceiptID(receiptID) {
		return Checkpoint{}, "", "", ErrIntegrity
	}
	return checkpoint, checkpointHash, receiptHash, nil
}

func (store *Store) openCheckpoint(
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	version uint64,
	expectedHash string,
	sealed []byte,
) (Checkpoint, error) {
	opened, err := store.vault.OpenRecord(
		store.checkpointAD(organizationID, initiativeID, version), sealed,
	)
	if err != nil || contentHash(opened).Digest != expectedHash {
		return Checkpoint{}, ErrIntegrity
	}
	checkpoint, err := contracts.DecodeCanonical[Checkpoint, *Checkpoint](opened)
	if err != nil || checkpoint.OrganizationID != organizationID ||
		checkpoint.InitiativeID != initiativeID || checkpoint.Version != version {
		return Checkpoint{}, ErrIntegrity
	}
	return checkpoint, nil
}

func (store *Store) loadReceiptRow(
	ctx context.Context,
	querier queryRower,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	receiptID DecisionReceiptID,
) (DecisionReceipt, error) {
	var expectedHash string
	var sealed []byte
	err := querier.QueryRow(ctx, `
		SELECT receipt_hash,sealed_receipt
		FROM workforce_lifecycle_decision_receipts
		WHERE tenant_id=$1 AND organization_id=$2 AND initiative_id=$3 AND receipt_id=$4
	`, store.tenantID, organizationID, initiativeID, receiptID).Scan(&expectedHash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionReceipt{}, ErrConflict
	}
	if err != nil {
		return DecisionReceipt{}, fmt.Errorf("company lifecycle: load receipt: %w", err)
	}
	opened, err := store.vault.OpenRecord(
		store.receiptAD(organizationID, initiativeID, receiptID), sealed,
	)
	if err != nil {
		return DecisionReceipt{}, ErrIntegrity
	}
	receipt, err := contracts.DecodeCanonical[DecisionReceipt, *DecisionReceipt](opened)
	if err != nil || receipt.OrganizationID != organizationID ||
		receipt.InitiativeID != initiativeID || receipt.ID != receiptID ||
		receipt.ContentHash.Digest != expectedHash || store.verifyReceipt(receipt) != nil {
		return DecisionReceipt{}, ErrIntegrity
	}
	return receipt, nil
}

func (store *Store) signReceipt(value *DecisionReceipt) error {
	value.ContentHash = contracts.ContentHash{}
	value.Signature = signatureMarker(store.keyID)
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	value.ContentHash = contentHash(payload)
	value.Signature = signatureMarker(store.keyID)
	payload, err = json.Marshal(value)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     store.keyID,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(store.privateKey, payload)),
	}
	return value.Validate()
}

func (store *Store) verifyReceipt(value DecisionReceipt) error {
	if err := value.Validate(); err != nil || value.Signature.KeyID != store.keyID ||
		value.Signature.Algorithm != "ed25519" {
		return ErrIntegrity
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return ErrIntegrity
	}
	signing := value
	signing.Signature = signatureMarker(store.keyID)
	payload, err := json.Marshal(signing)
	if err != nil || !ed25519.Verify(store.publicKey, payload, signature) {
		return ErrIntegrity
	}
	hashing := value
	hashing.ContentHash = contracts.ContentHash{}
	hashing.Signature = signatureMarker(store.keyID)
	payload, err = json.Marshal(hashing)
	if err != nil || contentHash(payload) != value.ContentHash {
		return ErrIntegrity
	}
	return nil
}

func signatureMarker(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func contentHash(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("company lifecycle: time source must return UTC")
	}
	return now, nil
}

func (store *Store) initiativeAD(organizationID contracts.OrganizationID, initiativeID InitiativeID) vault.AD {
	return vault.AD{
		User:   store.tenantID,
		Store:  "workforce.company.lifecycle.initiative",
		Stream: string(organizationID) + "/" + string(initiativeID),
		Schema: InitiativeSchemaVersion,
	}
}

func (store *Store) checkpointAD(organizationID contracts.OrganizationID, initiativeID InitiativeID, version uint64) vault.AD {
	return vault.AD{
		User:   store.tenantID,
		Store:  "workforce.company.lifecycle.checkpoint",
		Stream: string(organizationID) + "/" + string(initiativeID) + "/" + strconv.FormatUint(version, 10),
		Schema: CheckpointSchemaVersion,
	}
}

func (store *Store) transitionAD(organizationID contracts.OrganizationID, initiativeID InitiativeID, transitionID TransitionID) vault.AD {
	return vault.AD{
		User:   store.tenantID,
		Store:  "workforce.company.lifecycle.transition",
		Stream: string(organizationID) + "/" + string(initiativeID) + "/" + string(transitionID),
		Schema: TransitionSchemaVersion,
	}
}

func (store *Store) receiptAD(organizationID contracts.OrganizationID, initiativeID InitiativeID, receiptID DecisionReceiptID) vault.AD {
	return vault.AD{
		User:   store.tenantID,
		Store:  "workforce.company.lifecycle.receipt",
		Stream: string(organizationID) + "/" + string(initiativeID) + "/" + string(receiptID),
		Schema: ReceiptSchemaVersion,
	}
}
