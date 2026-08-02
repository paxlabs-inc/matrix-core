package companyruntime

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/portfolio"
)

type correctionSnapshotPayload struct {
	SchemaVersion               string                   `json:"schema_version"`
	OrganizationID              contracts.OrganizationID `json:"organization_id"`
	SnapshotID                  string                   `json:"snapshot_id"`
	Version                     uint64                   `json:"version"`
	UnresolvedMaterialCount     uint32                   `json:"unresolved_material_count"`
	UnresolvedContaminatedCount uint32                   `json:"unresolved_contaminated_count"`
	CheckedAt                   time.Time                `json:"checked_at"`
}

func (value correctionSnapshotPayload) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 || value.OrganizationID == "" ||
		!validToken(value.SnapshotID) || value.Version == 0 || !validUTC(value.CheckedAt) {
		return fmt.Errorf("company runtime: correction snapshot is invalid")
	}
	return nil
}

type gateVerificationReceipt struct {
	SchemaVersion      string                         `json:"schema_version"`
	TransitionID       companylifecycle.TransitionID  `json:"transition_id"`
	RequestHash        contracts.ContentHash          `json:"request_hash"`
	AuthorizationHash  contracts.ContentHash          `json:"authorization_hash"`
	PolicyDecisionHash contracts.ContentHash          `json:"policy_decision_hash"`
	Limits             companylifecycle.CapitalLimits `json:"limits"`
	VerifiedAt         time.Time                      `json:"verified_at"`
}

func (value gateVerificationReceipt) Validate() error {
	if value.SchemaVersion != "workforce.lifecycle-gate-verification-receipt.v1" ||
		!validToken(string(value.TransitionID)) || !validUTC(value.VerifiedAt) {
		return fmt.Errorf("company runtime: gate verification receipt is invalid")
	}
	for _, hash := range []contracts.ContentHash{
		value.RequestHash, value.AuthorizationHash, value.PolicyDecisionHash,
	} {
		if err := hash.Validate(); err != nil {
			return err
		}
	}
	return value.Limits.Validate()
}

func (store *Store) SnapshotCorrections(ctx context.Context) (companylifecycle.CorrectionBinding, error) {
	now, err := store.currentTime()
	if err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lockKey := store.tenantID + "|" + string(store.organizationID) + "|correction-snapshot"
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lockKey); err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	material, contaminated, err := store.correctionCountsTx(ctx, tx)
	if err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	var version uint64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(MAX(version),0)+1
		FROM workforce_company_correction_snapshots
		WHERE tenant_id=$1 AND organization_id=$2 AND snapshot_id=$3
	`, store.tenantID, store.organizationID, "corrections:"+string(store.organizationID)).Scan(&version); err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	payload := correctionSnapshotPayload{
		SchemaVersion: contracts.SchemaVersionV1, OrganizationID: store.organizationID,
		SnapshotID: "corrections:" + string(store.organizationID), Version: version,
		UnresolvedMaterialCount: material, UnresolvedContaminatedCount: contaminated,
		CheckedAt: now,
	}
	hash, err := contracts.HashCanonical(payload)
	if err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_correction_snapshots (
			tenant_id,organization_id,snapshot_id,version,unresolved_material_count,
			unresolved_contaminated_count,content_hash,checked_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, store.tenantID, store.organizationID, payload.SnapshotID, payload.Version,
		payload.UnresolvedMaterialCount, payload.UnresolvedContaminatedCount,
		hash.Digest, payload.CheckedAt); err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return companylifecycle.CorrectionBinding{}, err
	}
	return companylifecycle.CorrectionBinding{
		SchemaVersion: contracts.SchemaVersionV1, OrganizationID: store.organizationID,
		SnapshotID: payload.SnapshotID, Version: payload.Version, Hash: hash,
		CheckedAt:                   payload.CheckedAt,
		UnresolvedMaterialCount:     payload.UnresolvedMaterialCount,
		UnresolvedContaminatedCount: payload.UnresolvedContaminatedCount,
	}, nil
}

func (store *Store) BindCompanyState(
	ctx context.Context,
	recordID string,
) (companylifecycle.CompanyStateBinding, error) {
	if !validToken(recordID) {
		return companylifecycle.CompanyStateBinding{}, fmt.Errorf("company runtime: company-state record id is invalid")
	}
	var version uint64
	var hash string
	var observedAt time.Time
	var state, truth string
	var expiresAt *time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT record.version,record.content_hash,record.observed_at,head.state,
		       record.truth_status,record.expires_at
		FROM workforce_company_state_heads head
		JOIN workforce_company_state_records record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.record_id=head.record_id AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.record_id=$3
	`, store.tenantID, store.organizationID, recordID).Scan(
		&version, &hash, &observedAt, &state, &truth, &expiresAt,
	)
	now, nowErr := store.currentTime()
	if err != nil || nowErr != nil || state != "active" || truth == "proposal" ||
		expiresAt != nil && !expiresAt.After(now) {
		return companylifecycle.CompanyStateBinding{}, fmt.Errorf("company runtime: company-state head is not current verified truth")
	}
	return companylifecycle.CompanyStateBinding{
		SchemaVersion: contracts.SchemaVersionV1, OrganizationID: store.organizationID,
		ID: companylifecycle.CompanyStateID(recordID), Version: version,
		Hash: contracts.ContentHash{Algorithm: "sha256", Digest: hash}, ObservedAt: observedAt,
	}, nil
}

func (store *Store) AuthorizeGate(
	ctx context.Context,
	transitionID companylifecycle.TransitionID,
	initiativeID companylifecycle.InitiativeID,
	requestHash contracts.ContentHash,
	decision portfolio.DecisionReceipt,
	clauseID string,
) (GateAuthorization, error) {
	if !validToken(string(transitionID)) || !validToken(string(initiativeID)) ||
		requestHash.Validate() != nil || !validToken(clauseID) {
		return GateAuthorization{}, fmt.Errorf("company runtime: gate authorization request is invalid")
	}
	configuration, err := store.LoadCurrent(ctx)
	if err != nil {
		return GateAuthorization{}, err
	}
	if err := portfolio.VerifyDecision(decision, store.controllerKeyID, store.controllerPublic); err != nil ||
		decision.Decision != portfolio.DecisionGO || decision.InitiativeID == nil ||
		string(*decision.InitiativeID) != string(initiativeID) ||
		decision.ProcedureID != configuration.Procedure.ID ||
		decision.ProcedureVersion != configuration.Procedure.Version ||
		!slicesContains(decision.AuthorityClauses, clauseID) {
		return GateAuthorization{}, fmt.Errorf("company runtime: portfolio decision does not authorize this lifecycle gate")
	}
	decisionHash, err := contracts.HashCanonical(&decision)
	if err != nil {
		return GateAuthorization{}, err
	}
	current, err := store.mission.LoadCurrent(ctx)
	if err != nil {
		return GateAuthorization{}, err
	}
	authority := current.Authority
	maximumAllocation := configuration.Procedure.MaximumCapitalMicrounits
	if authority.Capital.SpendCeilingMicrounits < maximumAllocation {
		maximumAllocation = authority.Capital.SpendCeilingMicrounits
	}
	limits := companylifecycle.CapitalLimits{
		SchemaVersion: contracts.SchemaVersionV1, Currency: authority.Capital.Currency,
		CapitalEnvelopeVersion:           authority.Capital.Version,
		CapitalEnvelopeHash:              configuration.CapitalEnvelopeHash,
		MaxResultingAllocationMicrounits: maximumAllocation,
		MaxResultingSpendMicrounits:      authority.Capital.SpendCeilingMicrounits,
		MaxResultingExposureMicrounits:   authority.Capital.ExposureCeilingMicrounits,
		MaxTransitionBudgetMicrounits:    authority.IssuerPolicy.MaxWorkOrderMicrounits,
	}
	if err := limits.Validate(); err != nil {
		return GateAuthorization{}, err
	}
	now, err := store.currentTime()
	if err != nil {
		return GateAuthorization{}, err
	}
	expiresAt := decision.NextReviewAt
	if configuration.ExpiresAt.Before(expiresAt) {
		expiresAt = configuration.ExpiresAt
	}
	if authority.IssuerPolicy.ExpiresAt.Before(expiresAt) {
		expiresAt = authority.IssuerPolicy.ExpiresAt
	}
	value := GateAuthorization{
		SchemaVersion: GateAuthorizationSchemaVersion, TransitionID: transitionID,
		OrganizationID: store.organizationID, InitiativeID: initiativeID,
		RequestHash: requestHash, PolicyDecisionID: string(decision.ID),
		PolicyDecisionHash: decisionHash, AuthorityClauseID: clauseID,
		Limits: limits, AuthorizedAt: now, ExpiresAt: expiresAt,
	}
	if err := signGateAuthorization(&value, store.controllerKeyID, store.controllerPrivate); err != nil {
		return GateAuthorization{}, err
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return GateAuthorization{}, err
	}
	canonicalHash := digest(canonical)
	limitsHash, err := contracts.HashCanonical(limits)
	if err != nil {
		return GateAuthorization{}, err
	}
	sealed, err := store.vault.SealRecord(store.gateAD(string(transitionID)), canonical)
	if err != nil {
		return GateAuthorization{}, err
	}
	result, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_lifecycle_gate_authorizations (
			tenant_id,organization_id,transition_id,initiative_id,request_hash,
			policy_decision_id,policy_decision_hash,authority_clause_id,capital_limits_hash,
			canonical_hash,sealed_authorization,signature_key_id,authorized_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
		ON CONFLICT (tenant_id,organization_id,transition_id) DO NOTHING
	`, store.tenantID, store.organizationID, value.TransitionID, value.InitiativeID,
		value.RequestHash.Digest, value.PolicyDecisionID, value.PolicyDecisionHash.Digest,
		value.AuthorityClauseID, limitsHash.Digest, canonicalHash, sealed,
		value.Signature.KeyID, value.AuthorizedAt, value.ExpiresAt)
	if err != nil {
		return GateAuthorization{}, err
	}
	if result.RowsAffected() == 0 {
		existing, loadErr := store.loadGateAuthorization(ctx, store.pool, transitionID)
		if loadErr != nil || existing.RequestHash != requestHash ||
			existing.PolicyDecisionHash != decisionHash || existing.AuthorityClauseID != clauseID {
			return GateAuthorization{}, fmt.Errorf("company runtime: conflicting lifecycle authorization replay")
		}
		return existing, nil
	}
	return value, nil
}

func (store *Store) VerifyLifecycleGate(
	ctx context.Context,
	tx pgx.Tx,
	request companylifecycle.GateVerificationRequest,
) (companylifecycle.GateVerificationGrant, error) {
	if tx == nil || request.OrganizationID != store.organizationID || !validUTC(request.VerifiedAt) {
		return companylifecycle.GateVerificationGrant{}, fmt.Errorf("company runtime: lifecycle verification scope is invalid")
	}
	configuration, err := store.loadCurrentTx(ctx, tx, request.VerifiedAt)
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	if err := store.verifyMissionRootsTx(ctx, tx, configuration, request.Authority); err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	if err := store.verifyMandateTx(ctx, tx, request.Authority, request.VerifiedAt); err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	if err := store.verifyCompanyStateTx(ctx, tx, request.CompanyState, request.VerifiedAt); err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	if err := store.verifyCorrectionTx(ctx, tx, request.Correction); err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	if err := store.verifyEvidenceTx(ctx, tx, request.Evidence, request.VerifiedAt); err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	authorization, authorizationHash, err := store.loadGateAuthorizationTx(ctx, tx, request.TransitionID)
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	if authorization.OrganizationID != request.OrganizationID ||
		authorization.InitiativeID != request.InitiativeID ||
		authorization.RequestHash != request.RequestHash ||
		authorization.AuthorityClauseID != request.Authority.ClauseID ||
		authorization.AuthorizedAt.After(request.VerifiedAt) ||
		!authorization.ExpiresAt.After(request.VerifiedAt) {
		return companylifecycle.GateVerificationGrant{}, fmt.Errorf("company runtime: lifecycle authorization scope drifted")
	}
	if request.CapitalImpact.CapitalEnvelopeVersion != authorization.Limits.CapitalEnvelopeVersion ||
		request.CapitalImpact.CapitalEnvelopeHash != authorization.Limits.CapitalEnvelopeHash ||
		request.CapitalImpact.Currency != authorization.Limits.Currency ||
		request.CapitalImpact.TransitionBudgetMicrounits > authorization.Limits.MaxTransitionBudgetMicrounits {
		return companylifecycle.GateVerificationGrant{}, fmt.Errorf("company runtime: lifecycle capital is outside current limits")
	}
	var allocated int64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(SUM(capital_microunits),0)::BIGINT
		FROM workforce_portfolio_allocations
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
	`, store.tenantID, store.organizationID).Scan(&allocated); err != nil || allocated < 0 ||
		uint64(allocated) > authorization.Limits.MaxResultingAllocationMicrounits {
		return companylifecycle.GateVerificationGrant{}, fmt.Errorf("company runtime: aggregate capital allocation exceeds authority")
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_lifecycle_gate_consumptions (
			tenant_id,organization_id,transition_id,request_hash,consumed_at
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id,organization_id,transition_id) DO NOTHING
	`, store.tenantID, store.organizationID, request.TransitionID,
		request.RequestHash.Digest, request.VerifiedAt); err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	authorityHash, err := contracts.HashCanonical(request.Authority)
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	companyHash, err := contracts.HashCanonical(request.CompanyState)
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	correctionHash, err := contracts.HashCanonical(request.Correction)
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	evidenceHash, err := companylifecycle.EvidenceSetHash(request.Evidence)
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	impactHash, err := contracts.HashCanonical(request.CapitalImpact)
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	receiptHash, err := contracts.HashCanonical(gateVerificationReceipt{
		SchemaVersion: "workforce.lifecycle-gate-verification-receipt.v1",
		TransitionID:  request.TransitionID, RequestHash: request.RequestHash,
		AuthorizationHash:  authorizationHash,
		PolicyDecisionHash: authorization.PolicyDecisionHash,
		Limits:             authorization.Limits, VerifiedAt: request.VerifiedAt,
	})
	if err != nil {
		return companylifecycle.GateVerificationGrant{}, err
	}
	expiresAt := authorization.ExpiresAt
	if request.Authority.ExpiresAt.Before(expiresAt) {
		expiresAt = request.Authority.ExpiresAt
	}
	if configuration.ExpiresAt.Before(expiresAt) {
		expiresAt = configuration.ExpiresAt
	}
	return companylifecycle.GateVerificationGrant{
		SchemaVersion: companylifecycle.GateGrantSchemaVersion,
		TransitionID:  request.TransitionID, OrganizationID: request.OrganizationID,
		InitiativeID: request.InitiativeID, AuthorityBindingHash: authorityHash,
		CompanyStateHash: companyHash, CorrectionBindingHash: correctionHash,
		EvidenceSetHash: evidenceHash, CapitalImpactHash: impactHash,
		PolicyDecisionID:   authorization.PolicyDecisionID,
		PolicyDecisionHash: authorization.PolicyDecisionHash,
		AuthorityClauseID:  authorization.AuthorityClauseID,
		VerifierID:         "workforced.company-runtime", VerificationReceiptHash: receiptHash,
		Limits: authorization.Limits, VerifiedAt: request.VerifiedAt, ExpiresAt: expiresAt,
	}, nil
}

func (store *Store) loadCurrentTx(
	ctx context.Context,
	tx pgx.Tx,
	at time.Time,
) (StartConfiguration, error) {
	var id, state, expectedHash string
	var version uint64
	var sealed []byte
	if err := tx.QueryRow(ctx, `
		SELECT head.config_id,head.version,head.state,config.canonical_hash,config.sealed_config
		FROM workforce_company_runtime_heads head
		JOIN workforce_company_runtime_configs config
		  ON config.tenant_id=head.tenant_id AND config.organization_id=head.organization_id
		 AND config.config_id=head.config_id AND config.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 FOR SHARE
	`, store.tenantID, store.organizationID).Scan(&id, &version, &state, &expectedHash, &sealed); err != nil {
		return StartConfiguration{}, err
	}
	canonical, err := store.vault.OpenRecord(store.configAD(id, version), sealed)
	if err != nil || digest(canonical) != expectedHash || state != "active" {
		return StartConfiguration{}, fmt.Errorf("company runtime: current configuration integrity failure")
	}
	value, err := contracts.DecodeCanonical[StartConfiguration, *StartConfiguration](canonical)
	if err != nil || VerifyStartConfiguration(value, store.founderKeyID, store.founderPublicKey) != nil ||
		at.Before(value.EffectiveAt) || !at.Before(value.ExpiresAt) {
		return StartConfiguration{}, fmt.Errorf("company runtime: current configuration is not executable")
	}
	return value, nil
}

func (store *Store) loadGateAuthorization(
	ctx context.Context,
	querier interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	transitionID companylifecycle.TransitionID,
) (GateAuthorization, error) {
	value, _, err := store.loadGateAuthorizationWithQuerier(ctx, querier, transitionID)
	return value, err
}

func (store *Store) loadGateAuthorizationTx(
	ctx context.Context,
	tx pgx.Tx,
	transitionID companylifecycle.TransitionID,
) (GateAuthorization, contracts.ContentHash, error) {
	return store.loadGateAuthorizationWithQuerier(ctx, tx, transitionID)
}

func (store *Store) loadGateAuthorizationWithQuerier(
	ctx context.Context,
	querier interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
	transitionID companylifecycle.TransitionID,
) (GateAuthorization, contracts.ContentHash, error) {
	var expectedHash string
	var sealed []byte
	if err := querier.QueryRow(ctx, `
		SELECT canonical_hash,sealed_authorization
		FROM workforce_lifecycle_gate_authorizations
		WHERE tenant_id=$1 AND organization_id=$2 AND transition_id=$3
	`, store.tenantID, store.organizationID, transitionID).Scan(&expectedHash, &sealed); err != nil {
		return GateAuthorization{}, contracts.ContentHash{}, err
	}
	canonical, err := store.vault.OpenRecord(store.gateAD(string(transitionID)), sealed)
	if err != nil || digest(canonical) != expectedHash {
		return GateAuthorization{}, contracts.ContentHash{}, fmt.Errorf("company runtime: gate authorization integrity failure")
	}
	value, err := contracts.DecodeCanonical[GateAuthorization, *GateAuthorization](canonical)
	if err != nil || value.TransitionID != transitionID ||
		verifyGateAuthorization(value, store.controllerKeyID, store.controllerPublic) != nil {
		return GateAuthorization{}, contracts.ContentHash{}, fmt.Errorf("company runtime: gate authorization authentication failure")
	}
	return value, contracts.ContentHash{Algorithm: "sha256", Digest: expectedHash}, nil
}

func (store *Store) verifyMissionRootsTx(
	ctx context.Context,
	tx pgx.Tx,
	configuration StartConfiguration,
	binding companylifecycle.AuthorityBinding,
) error {
	if binding.OrganizationID != store.organizationID ||
		binding.MissionVersion != configuration.MissionVersion || binding.MissionHash != configuration.MissionHash ||
		binding.ConstitutionVersion != configuration.ConstitutionVersion || binding.ConstitutionHash != configuration.ConstitutionHash ||
		binding.CapitalEnvelopeVersion != configuration.CapitalEnvelopeVersion || binding.CapitalEnvelopeHash != configuration.CapitalEnvelopeHash ||
		binding.IssuerPolicyVersion != configuration.IssuerPolicyVersion || binding.IssuerPolicyHash != configuration.IssuerPolicyHash {
		return fmt.Errorf("company runtime: lifecycle authority does not match current founder roots")
	}
	var state string
	var issuerRevokedAt *time.Time
	var missionVersion, constitutionVersion, capitalVersion, issuerVersion uint64
	if err := tx.QueryRow(ctx, `
		SELECT state,issuer_revoked_at,mission_version,constitution_version,
		       capital_envelope_version,issuer_policy_version
		FROM workforce_organization_v2_projection
		WHERE tenant_id=$1 AND organization_id=$2 FOR SHARE
	`, store.tenantID, store.organizationID).Scan(&state, &issuerRevokedAt, &missionVersion,
		&constitutionVersion, &capitalVersion, &issuerVersion); err != nil || state != "active" ||
		issuerRevokedAt != nil || missionVersion != binding.MissionVersion ||
		constitutionVersion != binding.ConstitutionVersion || capitalVersion != binding.CapitalEnvelopeVersion ||
		issuerVersion != binding.IssuerPolicyVersion {
		return fmt.Errorf("company runtime: founder authority projection is not executable")
	}
	expected := map[string]struct {
		version uint64
		hash    string
	}{
		"founder_mission":       {binding.MissionVersion, binding.MissionHash.Digest},
		"company_constitution":  {binding.ConstitutionVersion, binding.ConstitutionHash.Digest},
		"capital_envelope":      {binding.CapitalEnvelopeVersion, binding.CapitalEnvelopeHash.Digest},
		"company_issuer_policy": {binding.IssuerPolicyVersion, binding.IssuerPolicyHash.Digest},
	}
	rows, err := tx.Query(ctx, `
		SELECT head.authority_kind,head.latest_version,record.canonical_hash,
		       EXISTS (
		           SELECT 1 FROM workforce_company_authority_revocations revocation
		           WHERE revocation.tenant_id=record.tenant_id
		             AND revocation.organization_id=record.organization_id
		             AND revocation.authority_kind=record.authority_kind
		             AND revocation.authority_id=record.authority_id
		             AND revocation.version=record.version
		       )
		FROM workforce_company_authority_heads head
		JOIN workforce_company_authority_records record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.authority_kind = ANY($3)
		FOR SHARE OF record
	`, store.tenantID, store.organizationID,
		[]string{"founder_mission", "company_constitution", "capital_envelope", "company_issuer_policy"})
	if err != nil {
		return err
	}
	defer rows.Close()
	seen := 0
	for rows.Next() {
		var kind, hash string
		var version uint64
		var revoked bool
		if err := rows.Scan(&kind, &version, &hash, &revoked); err != nil {
			return err
		}
		root, exists := expected[kind]
		if !exists || revoked || root.version != version || root.hash != hash {
			return fmt.Errorf("company runtime: founder authority root drifted or was revoked")
		}
		seen++
	}
	if err := rows.Err(); err != nil || seen != len(expected) {
		return fmt.Errorf("company runtime: founder authority root set is incomplete")
	}
	return nil
}

func (store *Store) verifyMandateTx(
	ctx context.Context,
	tx pgx.Tx,
	binding companylifecycle.AuthorityBinding,
	at time.Time,
) error {
	var mandateID, hash string
	var mandateVersion uint64
	var active, revoked bool
	var effectiveAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT seat.mandate_id,seat.mandate_version,seat.active,
		       record.canonical_hash,record.effective_at,
		       EXISTS (
		           SELECT 1 FROM workforce_authority_revocations revocation
		           WHERE revocation.tenant_id=record.tenant_id
		             AND revocation.organization_id=record.organization_id
		             AND revocation.authority_kind='mandate'
		             AND revocation.authority_id=record.authority_id
		             AND revocation.version=record.version
		       )
		FROM workforce_organization_seats seat
		JOIN workforce_authority_heads head
		  ON head.tenant_id=seat.tenant_id AND head.organization_id=seat.organization_id
		 AND head.authority_kind='mandate' AND head.authority_id=seat.mandate_id
		JOIN workforce_authority_records record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE seat.tenant_id=$1 AND seat.organization_id=$2 AND seat.seat_id=$3
		FOR SHARE OF seat,record
	`, store.tenantID, store.organizationID, binding.RequestedBySeatID).Scan(
		&mandateID, &mandateVersion, &active, &hash, &effectiveAt, &revoked,
	)
	if err != nil || !active || revoked || effectiveAt.After(at) ||
		contracts.MandateID(mandateID) != binding.MandateID || mandateVersion != binding.MandateVersion ||
		hash != binding.MandateHash.Digest {
		return fmt.Errorf("company runtime: requesting seat mandate is not current")
	}
	return nil
}

func (store *Store) verifyCompanyStateTx(
	ctx context.Context,
	tx pgx.Tx,
	binding companylifecycle.CompanyStateBinding,
	at time.Time,
) error {
	var version uint64
	var contentHash, headState, validity, truth string
	var observedAt, effectiveAt time.Time
	var expiresAt *time.Time
	err := tx.QueryRow(ctx, `
		SELECT record.version,record.content_hash,head.state,record.validity,
		       record.truth_status,record.observed_at,record.effective_at,record.expires_at
		FROM workforce_company_state_heads head
		JOIN workforce_company_state_records record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.record_id=head.record_id AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.record_id=$3
		FOR SHARE OF record
	`, store.tenantID, store.organizationID, binding.ID).Scan(
		&version, &contentHash, &headState, &validity, &truth,
		&observedAt, &effectiveAt, &expiresAt,
	)
	if err != nil || version != binding.Version || contentHash != binding.Hash.Digest ||
		headState != "active" || validity != "active" || truth == "proposal" ||
		observedAt != binding.ObservedAt || observedAt.After(at) || effectiveAt.After(at) ||
		expiresAt != nil && !expiresAt.After(at) {
		return fmt.Errorf("company runtime: company-state binding is stale, proposed, or inactive")
	}
	var contaminated bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_company_state_contamination
			WHERE tenant_id=$1 AND organization_id=$2 AND affected_record_id=$3
			  AND affected_version=$4 AND state='open'
		)
	`, store.tenantID, store.organizationID, binding.ID, binding.Version).Scan(&contaminated); err != nil || contaminated {
		return fmt.Errorf("company runtime: company-state binding is contaminated")
	}
	return nil
}

func (store *Store) verifyCorrectionTx(
	ctx context.Context,
	tx pgx.Tx,
	binding companylifecycle.CorrectionBinding,
) error {
	var material, contaminated int64
	var hash string
	var checkedAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT unresolved_material_count,unresolved_contaminated_count,content_hash,checked_at
		FROM workforce_company_correction_snapshots
		WHERE tenant_id=$1 AND organization_id=$2 AND snapshot_id=$3 AND version=$4
		FOR SHARE
	`, store.tenantID, store.organizationID, binding.SnapshotID, binding.Version).Scan(
		&material, &contaminated, &hash, &checkedAt,
	)
	if err != nil || material < 0 || contaminated < 0 || uint64(material) > uint64(^uint32(0)) ||
		uint64(contaminated) > uint64(^uint32(0)) || uint32(material) != binding.UnresolvedMaterialCount ||
		uint32(contaminated) != binding.UnresolvedContaminatedCount || hash != binding.Hash.Digest ||
		checkedAt != binding.CheckedAt {
		return fmt.Errorf("company runtime: correction snapshot binding is invalid")
	}
	currentMaterial, currentContaminated, err := store.correctionCountsTx(ctx, tx)
	if err != nil || currentMaterial != binding.UnresolvedMaterialCount ||
		currentContaminated != binding.UnresolvedContaminatedCount || currentMaterial != 0 ||
		currentContaminated != 0 {
		return fmt.Errorf("company runtime: unresolved corrections block the lifecycle gate")
	}
	return nil
}

func (store *Store) correctionCountsTx(
	ctx context.Context,
	querier interface {
		QueryRow(context.Context, string, ...any) pgx.Row
	},
) (uint32, uint32, error) {
	var material, contaminated int64
	if err := querier.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE materially_unsafe),COUNT(*)
		FROM workforce_company_state_contamination
		WHERE tenant_id=$1 AND organization_id=$2 AND state='open'
	`, store.tenantID, store.organizationID).Scan(&material, &contaminated); err != nil ||
		material < 0 || contaminated < 0 || uint64(material) > uint64(^uint32(0)) ||
		uint64(contaminated) > uint64(^uint32(0)) {
		return 0, 0, fmt.Errorf("company runtime: correction count is invalid")
	}
	return uint32(material), uint32(contaminated), nil
}

func (store *Store) verifyEvidenceTx(
	ctx context.Context,
	tx pgx.Tx,
	evidence []companylifecycle.EvidenceBinding,
	at time.Time,
) error {
	for _, item := range evidence {
		var version uint64
		var contentHash, headState, validity, truth string
		var observedAt, effectiveAt time.Time
		var expiresAt *time.Time
		err := tx.QueryRow(ctx, `
			SELECT record.version,record.content_hash,head.state,record.validity,
			       record.truth_status,record.observed_at,record.effective_at,record.expires_at
			FROM workforce_company_state_records record
			JOIN workforce_company_state_heads head
			  ON head.tenant_id=record.tenant_id AND head.organization_id=record.organization_id
			 AND head.record_id=record.record_id AND head.latest_version=record.version
			WHERE record.tenant_id=$1 AND record.organization_id=$2
			  AND record.record_id=$3 AND record.version=$4
			FOR SHARE OF record
		`, store.tenantID, store.organizationID, item.SourceRecordID,
			item.SourceRecordVersion).Scan(&version, &contentHash, &headState, &validity,
			&truth, &observedAt, &effectiveAt, &expiresAt)
		if err != nil || version != item.SourceRecordVersion || contentHash != item.SourceRecordHash.Digest ||
			headState != "active" || validity != "active" || truth == "proposal" ||
			observedAt != item.ObservedAt || effectiveAt != item.EffectiveAt ||
			expiresAt != nil && !expiresAt.After(at) || !item.FreshUntil.After(at) {
			return fmt.Errorf("company runtime: lifecycle evidence %s is not current authoritative truth", item.ID)
		}
		var evidenceHash, evidenceValidity string
		if err := tx.QueryRow(ctx, `
			SELECT content_hash_digest,validity FROM workforce_records
			WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
			FOR SHARE
		`, store.tenantID, store.organizationID, item.ID).Scan(&evidenceHash, &evidenceValidity); err != nil ||
			evidenceHash != item.EvidenceHash.Digest || evidenceValidity != "active" {
			return fmt.Errorf("company runtime: lifecycle evidence record %s is inactive or mismatched", item.ID)
		}
		var contaminated bool
		if err := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_company_state_contamination
				WHERE tenant_id=$1 AND organization_id=$2 AND affected_record_id=$3
				  AND affected_version=$4 AND state='open'
			)
		`, store.tenantID, store.organizationID, item.SourceRecordID,
			item.SourceRecordVersion).Scan(&contaminated); err != nil || contaminated {
			return fmt.Errorf("company runtime: lifecycle evidence %s is contaminated", item.ID)
		}
		if item.IndependentVerdictID != nil {
			var verdictHash string
			if err := tx.QueryRow(ctx, `
				SELECT verdict_hash FROM workforce_verdict_records
				WHERE tenant_id=$1 AND organization_id=$2 AND verdict_id=$3
				FOR SHARE
			`, store.tenantID, store.organizationID, *item.IndependentVerdictID).Scan(&verdictHash); err != nil ||
				verdictHash != item.IndependentVerdictHash.Digest {
				return fmt.Errorf("company runtime: independent verdict for %s is invalid", item.ID)
			}
		}
	}
	return nil
}

func slicesContains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

var _ companylifecycle.GateVerifier = (*Store)(nil)
