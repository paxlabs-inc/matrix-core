package portfolio

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/companystate"
	"centra/workforce/internal/contracts"
)

var (
	// ErrConflict reports reuse of an immutable identity with different content.
	ErrConflict = errors.New("portfolio: immutable conflict")
	// ErrNotFound intentionally combines absent and out-of-scope portfolio state.
	ErrNotFound = errors.New("portfolio: record not found")
	// ErrIntegrity reports Vault authentication or canonical hash failure.
	ErrIntegrity = errors.New("portfolio: record integrity failure")
	// ErrUnauthorized reports authority, policy, scope, or resource denial.
	ErrUnauthorized = errors.New("portfolio: unauthorized")
)

// Store owns one tenant and organization's opportunity, portfolio, and cadence state.
type Store struct {
	pool              *pgxpool.Pool
	vault             *vault.UserVault
	tenantID          string
	organizationID    contracts.OrganizationID
	controllerKeyID   string
	controllerPrivate ed25519.PrivateKey
	controllerPublic  ed25519.PublicKey
	now               func() time.Time
}

// NewStore constructs a Vault-separated portfolio store. The caller owns the pool.
func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	controllerKeyID string,
	controllerPrivate ed25519.PrivateKey,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || strings.TrimSpace(tenantID) == "" ||
		organizationID == "" || controllerKeyID == "" ||
		len(controllerPrivate) != ed25519.PrivateKeySize || now == nil {
		return nil, fmt.Errorf("portfolio: store dependencies and authority are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("portfolio: Vault tenant does not match store tenant")
	}
	privateCopy := append(ed25519.PrivateKey(nil), controllerPrivate...)
	publicKey := controllerPrivate.Public().(ed25519.PublicKey)
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID, organizationID: organizationID,
		controllerKeyID: controllerKeyID, controllerPrivate: privateCopy,
		controllerPublic: append(ed25519.PublicKey(nil), publicKey...), now: now,
	}, nil
}

// SubmitResult describes immutable intake or canonical deduplication.
type SubmitResult struct {
	OpportunityID OpportunityID
	Version       uint64
	Deduplicated  bool
}

// Submit verifies, seals, deduplicates, versions, and persists an opportunity.
func (store *Store) Submit(
	ctx context.Context,
	value Opportunity,
	authorPublicKey ed25519.PublicKey,
	idempotencyKey string,
) (SubmitResult, error) {
	if value.OrganizationID != store.organizationID || idempotencyKey == "" {
		return SubmitResult{}, ErrUnauthorized
	}
	if err := VerifyOpportunity(value, value.Signature.KeyID, authorPublicKey); err != nil {
		return SubmitResult{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return SubmitResult{}, err
	}
	if value.SubmittedAt.After(now) || !value.ExpiresAt.After(now) {
		return SubmitResult{}, ErrUnauthorized
	}
	canonical, hash, sealed, err := store.prepareOpportunity(value)
	if err != nil {
		return SubmitResult{}, err
	}
	_ = canonical
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return SubmitResult{}, fmt.Errorf("portfolio: begin opportunity intake: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|opportunity|" + value.CanonicalIdentity
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return SubmitResult{}, fmt.Errorf("portfolio: lock opportunity: %w", err)
	}
	if result, existingHash, found, err := store.findOpportunityIdempotency(ctx, tx, idempotencyKey); err != nil {
		return SubmitResult{}, err
	} else if found {
		if result.OpportunityID != value.ID || result.Version != value.Version || existingHash != hash {
			return SubmitResult{}, ErrConflict
		}
		result.Deduplicated = true
		return result, tx.Commit(ctx)
	}
	if err := store.validateCurrentCompanyStateRefsTx(ctx, tx, value.SourceRecords, now); err != nil {
		return SubmitResult{}, err
	}
	var targetKind string
	if err := tx.QueryRow(ctx, `
		SELECT kind FROM workforce_company_state_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
	`, store.tenantID, store.organizationID, value.TargetCustomerRecordID).Scan(&targetKind); err != nil || targetKind != "customer" {
		return SubmitResult{}, ErrUnauthorized
	}
	var existingID string
	var existingVersion uint64
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT opportunity_id,current_version,canonical_hash
		FROM workforce_opportunity_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND canonical_identity=$3
	`, store.tenantID, store.organizationID, value.CanonicalIdentity).Scan(
		&existingID, &existingVersion, &existingHash,
	)
	if err == nil {
		if OpportunityID(existingID) != value.ID {
			return SubmitResult{OpportunityID: OpportunityID(existingID), Version: existingVersion, Deduplicated: true}, tx.Commit(ctx)
		}
		if value.Version == existingVersion && existingHash == hash {
			return SubmitResult{OpportunityID: value.ID, Version: value.Version, Deduplicated: true}, tx.Commit(ctx)
		}
		if value.Version != existingVersion+1 {
			return SubmitResult{}, ErrConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return SubmitResult{}, fmt.Errorf("portfolio: inspect opportunity head: %w", err)
	} else if value.Version != 1 {
		return SubmitResult{}, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_opportunities (
			tenant_id,organization_id,opportunity_id,version,canonical_identity,
			source_kind,author_seat_id,submitted_at,expires_at,canonical_hash,
			sealed_record,idempotency_key,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
	`, store.tenantID, store.organizationID, value.ID, value.Version,
		value.CanonicalIdentity, value.SourceKind, value.AuthorSeatID, value.SubmittedAt,
		value.ExpiresAt, hash, sealed, idempotencyKey, now); err != nil {
		return SubmitResult{}, fmt.Errorf("portfolio: insert opportunity: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_opportunity_heads (
			tenant_id,organization_id,canonical_identity,opportunity_id,current_version,
			canonical_hash,state,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,'submitted',$7)
		ON CONFLICT (tenant_id,organization_id,canonical_identity) DO UPDATE SET
			current_version=EXCLUDED.current_version,canonical_hash=EXCLUDED.canonical_hash,
			state='submitted',updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.CanonicalIdentity, value.ID,
		value.Version, hash, now); err != nil {
		return SubmitResult{}, fmt.Errorf("portfolio: advance opportunity head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return SubmitResult{}, fmt.Errorf("portfolio: commit opportunity: %w", err)
	}
	return SubmitResult{OpportunityID: value.ID, Version: value.Version}, nil
}

// DecideRequest binds current signed policy and independent evidence to one decision.
type DecideRequest struct {
	ID                 DecisionID
	OpportunityID      OpportunityID
	Assessment         Assessment
	Procedure          DecisionProcedure
	ProcedurePublicKey ed25519.PublicKey
	Alternatives       []Alternative
	IdempotencyKey     string
}

// Decide evaluates and atomically commits a signed portfolio decision and allocation.
func (store *Store) Decide(ctx context.Context, request DecideRequest) (DecisionReceipt, bool, error) {
	if request.ID == "" || request.OpportunityID == "" || request.IdempotencyKey == "" ||
		request.Procedure.OrganizationID != store.organizationID {
		return DecisionReceipt{}, false, ErrUnauthorized
	}
	if err := VerifyProcedure(
		request.Procedure, request.Procedure.Signature.KeyID, request.ProcedurePublicKey,
	); err != nil {
		return DecisionReceipt{}, false, err
	}
	now, err := store.currentTime()
	if err != nil {
		return DecisionReceipt{}, false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return DecisionReceipt{}, false, fmt.Errorf("portfolio: begin decision: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|decision|" + string(request.OpportunityID)
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return DecisionReceipt{}, false, fmt.Errorf("portfolio: lock decision: %w", err)
	}
	if existing, found, err := store.findDecisionIdempotency(ctx, tx, request.IdempotencyKey); err != nil {
		return DecisionReceipt{}, false, err
	} else if found {
		if existing.ID != request.ID || existing.OpportunityID != request.OpportunityID {
			return DecisionReceipt{}, false, ErrConflict
		}
		return existing, true, tx.Commit(ctx)
	}
	opportunity, err := store.loadOpportunityTx(ctx, tx, request.OpportunityID)
	if err != nil {
		return DecisionReceipt{}, false, err
	}
	if err := store.validateCurrentCompanyStateRefsTx(ctx, tx, opportunity.SourceRecords, now); err != nil {
		return DecisionReceipt{}, false, err
	}
	if err := store.validateCurrentCompanyStateRefsTx(ctx, tx, request.Assessment.Evidence, now); err != nil {
		return DecisionReceipt{}, false, err
	}
	evaluationContext, err := store.evaluationContext(ctx, tx, request.Assessment, opportunity.SourceRecords)
	if err != nil {
		return DecisionReceipt{}, false, err
	}
	receipt, err := Evaluate(opportunity, request.Assessment, request.Procedure, evaluationContext, now)
	if err != nil {
		return DecisionReceipt{}, false, err
	}
	receipt.ID = request.ID
	if len(request.Alternatives) > 0 {
		receipt.Alternatives = append(receipt.Alternatives, request.Alternatives...)
		slices.SortFunc(receipt.Alternatives, func(left, right Alternative) int {
			return strings.Compare(string(left.OpportunityID), string(right.OpportunityID))
		})
		for index := 1; index < len(receipt.Alternatives); index++ {
			if receipt.Alternatives[index].OpportunityID == receipt.Alternatives[index-1].OpportunityID {
				return DecisionReceipt{}, false, ErrConflict
			}
		}
	}
	if err := signDecision(&receipt, store.controllerKeyID, store.controllerPrivate); err != nil {
		return DecisionReceipt{}, false, err
	}
	if err := receipt.Validate(); err != nil {
		return DecisionReceipt{}, false, err
	}
	canonical, err := contracts.EncodeCanonical(&receipt)
	if err != nil {
		return DecisionReceipt{}, false, err
	}
	sum := sha256.Sum256(canonical)
	hash := hex.EncodeToString(sum[:])
	sealed, err := store.vault.SealRecord(store.decisionAD(receipt.ID), canonical)
	if err != nil {
		return DecisionReceipt{}, false, fmt.Errorf("portfolio: seal decision: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_portfolio_decisions (
			tenant_id,organization_id,decision_id,opportunity_id,procedure_id,
			procedure_version,decision,score_bps,capital_impact_microunits,
			risk_impact_microunits,next_review_at,canonical_hash,sealed_record,
			idempotency_key,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`, store.tenantID, store.organizationID, receipt.ID, receipt.OpportunityID,
		receipt.ProcedureID, receipt.ProcedureVersion, receipt.Decision, receipt.ScoreBPS,
		receipt.CapitalImpactMicrounits, receipt.RiskImpactMicrounits,
		receipt.NextReviewAt, hash, sealed, request.IdempotencyKey, now); err != nil {
		return DecisionReceipt{}, false, fmt.Errorf("portfolio: insert decision: %w", err)
	}
	state := opportunityState(receipt.Decision)
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_opportunity_heads SET state=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND opportunity_id=$5
	`, state, now, store.tenantID, store.organizationID, receipt.OpportunityID); err != nil {
		return DecisionReceipt{}, false, fmt.Errorf("portfolio: update opportunity state: %w", err)
	}
	if receipt.Decision == DecisionGO {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_portfolio_allocations (
				tenant_id,organization_id,initiative_id,opportunity_id,decision_id,
				capital_microunits,risk_microunits,state,created_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,'active',$8,$8)
		`, store.tenantID, store.organizationID, *receipt.InitiativeID,
			receipt.OpportunityID, receipt.ID, receipt.CapitalImpactMicrounits,
			receipt.RiskImpactMicrounits, now); err != nil {
			return DecisionReceipt{}, false, fmt.Errorf("portfolio: allocate initiative: %w", err)
		}
	}
	if kind := decisionIncidentKind(receipt); kind != "" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_portfolio_incidents (
				tenant_id,organization_id,incident_id,kind,opportunity_id,
				detail,state,created_at,resolved_at
			) VALUES ($1,$2,$3,$4,$5,$6,'open',$7,NULL)
			ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID,
			"incident:portfolio:"+string(receipt.ID), kind, receipt.OpportunityID,
			receipt.Reason, now); err != nil {
			return DecisionReceipt{}, false, fmt.Errorf("portfolio: insert decision incident: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return DecisionReceipt{}, false, fmt.Errorf("portfolio: commit decision: %w", err)
	}
	return receipt, false, nil
}

// LoadDecision authenticates and returns one immutable controller-signed decision.
func (store *Store) LoadDecision(ctx context.Context, id DecisionID) (DecisionReceipt, error) {
	var hash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_portfolio_decisions
		WHERE tenant_id=$1 AND organization_id=$2 AND decision_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionReceipt{}, ErrNotFound
	}
	if err != nil {
		return DecisionReceipt{}, fmt.Errorf("portfolio: load decision: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.decisionAD(id), sealed)
	if err != nil || digest(canonical) != hash {
		return DecisionReceipt{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[DecisionReceipt, *DecisionReceipt](canonical)
	if err != nil {
		return DecisionReceipt{}, ErrIntegrity
	}
	payload, payloadErr := decisionSigningBytes(value, store.controllerKeyID)
	if verifySignature(value.Signature, store.controllerKeyID, store.controllerPublic, payload, payloadErr) != nil {
		return DecisionReceipt{}, ErrIntegrity
	}
	return value, nil
}

// RegisterCadence seals and installs a founder-signed recurring controller cadence.
func (store *Store) RegisterCadence(
	ctx context.Context,
	value Cadence,
	founderPublicKey ed25519.PublicKey,
) error {
	if value.OrganizationID != store.organizationID {
		return ErrUnauthorized
	}
	if err := VerifyCadence(value, value.Signature.KeyID, founderPublicKey); err != nil {
		return err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.cadenceAD(value.ID, value.Version), canonical)
	if err != nil {
		return fmt.Errorf("portfolio: seal cadence: %w", err)
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("portfolio: begin cadence registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|cadence|" + value.ID
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return fmt.Errorf("portfolio: lock cadence: %w", err)
	}
	var currentVersion uint64
	var currentHash string
	err = tx.QueryRow(ctx, `
		SELECT head.version,record.canonical_hash
		FROM workforce_company_cadences head
		JOIN workforce_company_cadence_records record ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id AND record.cadence_id=head.cadence_id
		 AND record.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.cadence_id=$3
	`, store.tenantID, store.organizationID, value.ID).Scan(&currentVersion, &currentHash)
	if err == nil {
		if value.Version == currentVersion && hash == currentHash {
			return tx.Commit(ctx)
		}
		if value.Version != currentVersion+1 {
			return ErrConflict
		}
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("portfolio: inspect cadence: %w", err)
	} else if value.Version != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_cadence_records (
			tenant_id,organization_id,cadence_id,version,kind,canonical_hash,
			sealed_record,effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
	`, store.tenantID, store.organizationID, value.ID, value.Version, value.Kind,
		hash, sealed, value.EffectiveAt, value.ExpiresAt, now); err != nil {
		return fmt.Errorf("portfolio: insert cadence record: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_cadences (
			tenant_id,organization_id,cadence_id,version,kind,interval_seconds,
			next_due_at,effective_at,expires_at,state,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'active',$10)
		ON CONFLICT (tenant_id,organization_id,cadence_id) DO UPDATE SET
			version=EXCLUDED.version,kind=EXCLUDED.kind,interval_seconds=EXCLUDED.interval_seconds,
			next_due_at=EXCLUDED.next_due_at,effective_at=EXCLUDED.effective_at,
			expires_at=EXCLUDED.expires_at,state='active',updated_at=EXCLUDED.updated_at
	`, store.tenantID, store.organizationID, value.ID, value.Version, value.Kind,
		value.IntervalSeconds, value.FirstDueAt, value.EffectiveAt, value.ExpiresAt, now); err != nil {
		return fmt.Errorf("portfolio: advance cadence: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("portfolio: commit cadence registration: %w", err)
	}
	return nil
}

// DueCadence is one atomically advanced recurring controller cycle.
type DueCadence struct {
	ID     string
	Kind   CadenceKind
	DueAt  time.Time
	NextAt time.Time
}

// DetectStarvation records founder-visible incidents for opportunities that
// have remained unresolved beyond the signed operating review window.
func (store *Store) DetectStarvation(ctx context.Context, maximumAge time.Duration) (int64, error) {
	if maximumAge < time.Hour || maximumAge > 365*24*time.Hour {
		return 0, fmt.Errorf("portfolio: starvation window is outside bounds")
	}
	now, err := store.currentTime()
	if err != nil {
		return 0, err
	}
	result, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_portfolio_incidents (
			tenant_id,organization_id,incident_id,kind,opportunity_id,
			detail,state,created_at,resolved_at
		)
		SELECT tenant_id,organization_id,
		       'incident:starvation:' || opportunity_id,'starvation',opportunity_id,
		       'Opportunity exceeded its deterministic review window','open',$1,NULL
		FROM workforce_opportunity_heads
		WHERE tenant_id=$2 AND organization_id=$3
		  AND state IN ('submitted','validating','deferred') AND updated_at<$4
		ON CONFLICT DO NOTHING
	`, now, store.tenantID, store.organizationID, now.Add(-maximumAge))
	if err != nil {
		return 0, fmt.Errorf("portfolio: detect starvation: %w", err)
	}
	return result.RowsAffected(), nil
}

// ClaimDueCadences advances and returns due work without accumulating backlog storms.
func (store *Store) ClaimDueCadences(ctx context.Context, limit uint16) ([]DueCadence, error) {
	if limit == 0 || limit > 64 {
		return nil, fmt.Errorf("portfolio: cadence claim limit must be 1 to 64")
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return nil, fmt.Errorf("portfolio: begin cadence claim: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	rows, err := tx.Query(ctx, `
		SELECT cadence_id,kind,next_due_at,interval_seconds
		FROM workforce_company_cadences
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
		  AND effective_at<=$3 AND (expires_at IS NULL OR expires_at>$3) AND next_due_at<=$3
		ORDER BY next_due_at,cadence_id LIMIT $4 FOR UPDATE SKIP LOCKED
	`, store.tenantID, store.organizationID, now, limit)
	if err != nil {
		return nil, fmt.Errorf("portfolio: query due cadences: %w", err)
	}
	defer rows.Close()
	var result []DueCadence
	for rows.Next() {
		var item DueCadence
		var seconds uint64
		if err := rows.Scan(&item.ID, &item.Kind, &item.DueAt, &seconds); err != nil {
			return nil, fmt.Errorf("portfolio: scan due cadence: %w", err)
		}
		interval := time.Duration(seconds) * time.Second
		item.NextAt = item.DueAt
		for !item.NextAt.After(now) {
			item.NextAt = item.NextAt.Add(interval)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_company_cadences SET next_due_at=$1,updated_at=$2
			WHERE tenant_id=$3 AND organization_id=$4 AND cadence_id=$5
		`, item.NextAt, now, store.tenantID, store.organizationID, item.ID); err != nil {
			return nil, fmt.Errorf("portfolio: advance cadence: %w", err)
		}
		result = append(result, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("portfolio: iterate due cadences: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, fmt.Errorf("portfolio: commit cadence claim: %w", err)
	}
	return result, nil
}

func (store *Store) evaluationContext(
	ctx context.Context,
	tx pgx.Tx,
	assessment Assessment,
	opportunityEvidence []companystate.RecordReference,
) (EvaluationContext, error) {
	var result EvaluationContext
	var activeInitiatives, allocatedCapital, allocatedRisk int64
	err := tx.QueryRow(ctx, `
		SELECT COUNT(*),COALESCE(SUM(capital_microunits),0),COALESCE(SUM(risk_microunits),0)
		FROM workforce_portfolio_allocations
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
	`, store.tenantID, store.organizationID).Scan(
		&activeInitiatives, &allocatedCapital, &allocatedRisk,
	)
	if err != nil {
		return EvaluationContext{}, fmt.Errorf("portfolio: inspect allocations: %w", err)
	}
	if activeInitiatives < 0 || activeInitiatives > 65_535 || allocatedCapital < 0 || allocatedRisk < 0 {
		return EvaluationContext{}, fmt.Errorf("portfolio: persisted allocation totals are outside supported bounds")
	}
	result.ActiveInitiatives = uint16(activeInitiatives)
	result.AllocatedCapitalMicrounits = uint64(allocatedCapital)
	result.AllocatedRiskMicrounits = uint64(allocatedRisk)
	evidenceIDs := make([]string, 0, len(assessment.Evidence)+len(opportunityEvidence))
	evidenceVersions := make([]int64, 0, len(assessment.Evidence)+len(opportunityEvidence))
	seenEvidence := make(map[string]struct{}, len(assessment.Evidence)+len(opportunityEvidence))
	for _, item := range append(append([]companystate.RecordReference(nil), opportunityEvidence...), assessment.Evidence...) {
		key := fmt.Sprintf("%s/%d", item.ID, item.Version)
		if _, exists := seenEvidence[key]; exists {
			continue
		}
		seenEvidence[key] = struct{}{}
		evidenceIDs = append(evidenceIDs, string(item.ID))
		evidenceVersions = append(evidenceVersions, int64(item.Version))
	}
	err = tx.QueryRow(ctx, `
		SELECT EXISTS (
		  SELECT 1
		  FROM workforce_company_state_contamination contamination
		  JOIN UNNEST($3::text[],$4::bigint[]) wanted(record_id,version)
		    ON wanted.record_id=contamination.affected_record_id
		   AND wanted.version=contamination.affected_version
		  WHERE contamination.tenant_id=$1 AND contamination.organization_id=$2
		    AND contamination.state='open' AND contamination.materially_unsafe
		)
	`, store.tenantID, store.organizationID, evidenceIDs, evidenceVersions).Scan(&result.Contaminated)
	if err != nil {
		return EvaluationContext{}, fmt.Errorf("portfolio: inspect contamination: %w", err)
	}
	var consecutiveNoEvidence int64
	err = tx.QueryRow(ctx, `
		WITH recent AS (
		  SELECT decision,ROW_NUMBER() OVER (ORDER BY created_at DESC,decision_id DESC) AS ordinal
		  FROM workforce_portfolio_decisions
		  WHERE tenant_id=$1 AND organization_id=$2
		  ORDER BY created_at DESC,decision_id DESC LIMIT 32
		), first_evidenced AS (
		  SELECT COALESCE(MIN(ordinal),33) AS ordinal FROM recent WHERE decision<>'defer'
		)
		SELECT COUNT(*) FROM recent,first_evidenced
		WHERE recent.decision='defer' AND recent.ordinal<first_evidenced.ordinal
	`, store.tenantID, store.organizationID).Scan(&consecutiveNoEvidence)
	if err != nil {
		return EvaluationContext{}, fmt.Errorf("portfolio: inspect no-evidence cycles: %w", err)
	}
	if consecutiveNoEvidence < 0 || consecutiveNoEvidence > 65_535 {
		return EvaluationContext{}, fmt.Errorf("portfolio: no-evidence cycle count is outside supported bounds")
	}
	result.ConsecutiveNoEvidenceCycles = uint16(consecutiveNoEvidence)
	return result, nil
}

func (store *Store) validateCurrentCompanyStateRefsTx(
	ctx context.Context,
	tx pgx.Tx,
	references []companystate.RecordReference,
	now time.Time,
) error {
	if len(references) == 0 || len(references) > 512 {
		return ErrUnauthorized
	}
	for index := range references {
		if err := references[index].Validate(); err != nil {
			return ErrUnauthorized
		}
		var state, truthStatus string
		var expiresAt *time.Time
		err := tx.QueryRow(ctx, `
			SELECT head.state,record.truth_status,record.expires_at
			FROM workforce_company_state_heads head
			JOIN workforce_company_state_records record
			  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
			 AND record.record_id=head.record_id AND record.version=head.latest_version
			WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.record_id=$3
			  AND record.version=$4 AND record.content_hash=$5
		`, store.tenantID, store.organizationID, references[index].ID,
			references[index].Version, references[index].ContentHash.Digest).Scan(
			&state, &truthStatus, &expiresAt,
		)
		if err != nil || state != "active" || truthStatus == "proposal" ||
			expiresAt != nil && !expiresAt.After(now) {
			return ErrUnauthorized
		}
	}
	return nil
}

func (store *Store) loadOpportunityTx(
	ctx context.Context,
	tx pgx.Tx,
	id OpportunityID,
) (Opportunity, error) {
	var version uint64
	var hash string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT record.version,record.canonical_hash,record.sealed_record
		FROM workforce_opportunity_heads head
		JOIN workforce_opportunities record ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.opportunity_id=head.opportunity_id AND record.version=head.current_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.opportunity_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&version, &hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Opportunity{}, ErrNotFound
	}
	if err != nil {
		return Opportunity{}, fmt.Errorf("portfolio: load opportunity: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.opportunityAD(id, version), sealed)
	if err != nil || digest(canonical) != hash {
		return Opportunity{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[Opportunity, *Opportunity](canonical)
	if err != nil {
		return Opportunity{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) findOpportunityIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	key string,
) (SubmitResult, string, bool, error) {
	var result SubmitResult
	var hash string
	err := tx.QueryRow(ctx, `
		SELECT opportunity_id,version,canonical_hash FROM workforce_opportunities
		WHERE tenant_id=$1 AND organization_id=$2 AND idempotency_key=$3
	`, store.tenantID, store.organizationID, key).Scan(
		&result.OpportunityID, &result.Version, &hash,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SubmitResult{}, "", false, nil
	}
	if err != nil {
		return SubmitResult{}, "", false, fmt.Errorf("portfolio: inspect opportunity idempotency: %w", err)
	}
	return result, hash, true, nil
}

func (store *Store) findDecisionIdempotency(
	ctx context.Context,
	tx pgx.Tx,
	key string,
) (DecisionReceipt, bool, error) {
	var id DecisionID
	err := tx.QueryRow(ctx, `
		SELECT decision_id FROM workforce_portfolio_decisions
		WHERE tenant_id=$1 AND organization_id=$2 AND idempotency_key=$3
	`, store.tenantID, store.organizationID, key).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return DecisionReceipt{}, false, nil
	}
	if err != nil {
		return DecisionReceipt{}, false, fmt.Errorf("portfolio: inspect decision idempotency: %w", err)
	}
	value, err := store.loadDecisionTx(ctx, tx, id)
	return value, err == nil, err
}

func (store *Store) loadDecisionTx(
	ctx context.Context,
	tx pgx.Tx,
	id DecisionID,
) (DecisionReceipt, error) {
	var hash string
	var sealed []byte
	if err := tx.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_portfolio_decisions
		WHERE tenant_id=$1 AND organization_id=$2 AND decision_id=$3
	`, store.tenantID, store.organizationID, id).Scan(&hash, &sealed); err != nil {
		return DecisionReceipt{}, err
	}
	canonical, err := store.vault.OpenRecord(store.decisionAD(id), sealed)
	if err != nil || digest(canonical) != hash {
		return DecisionReceipt{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[DecisionReceipt, *DecisionReceipt](canonical)
	if err != nil {
		return DecisionReceipt{}, ErrIntegrity
	}
	return value, nil
}

func (store *Store) prepareOpportunity(value Opportunity) ([]byte, string, []byte, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return nil, "", nil, err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.opportunityAD(value.ID, value.Version), canonical)
	if err != nil {
		return nil, "", nil, fmt.Errorf("portfolio: seal opportunity: %w", err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("portfolio: time source must return UTC")
	}
	return now, nil
}

func (store *Store) opportunityAD(id OpportunityID, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.portfolio.opportunity",
		Stream: string(store.organizationID) + "/" + string(id),
		Schema: fmt.Sprintf("%s.v%d", OpportunitySchemaVersion, version)}
}

func (store *Store) decisionAD(id DecisionID) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.portfolio.decision",
		Stream: string(store.organizationID) + "/" + string(id), Schema: DecisionSchemaVersion}
}

func (store *Store) cadenceAD(id string, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.portfolio.cadence",
		Stream: string(store.organizationID) + "/" + id,
		Schema: fmt.Sprintf("workforce.company-cadence.v1.v%d", version)}
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func opportunityState(decision DecisionKind) string {
	switch decision {
	case DecisionGO:
		return "funded"
	case DecisionNO_GO, DecisionReject, DecisionTerminate:
		return "closed"
	case DecisionValidate:
		return "validating"
	case DecisionPause, DecisionEscalate:
		return "paused"
	default:
		return "deferred"
	}
}

func decisionIncidentKind(receipt DecisionReceipt) string {
	switch {
	case strings.Contains(receipt.Reason, "capital"):
		return "capital_limit"
	case strings.Contains(receipt.Reason, "risk"):
		return "risk_limit"
	case strings.Contains(receipt.Reason, "initiative capacity"):
		return "initiative_limit"
	case strings.Contains(receipt.Reason, "Repeated no-evidence"):
		return "no_evidence"
	case strings.Contains(receipt.Reason, "operating capacity"):
		return "resource_capture"
	default:
		return ""
	}
}
