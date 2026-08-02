package autonomouscompany

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/securityqualification"
)

var (
	ErrConflict            = errors.New("autonomous company: immutable conflict")
	ErrNotFound            = errors.New("autonomous company: record not found")
	ErrUnauthorized        = errors.New("autonomous company: unauthorized")
	ErrEvidence            = errors.New("autonomous company: authoritative evidence failed")
	ErrUnsupportedEvidence = errors.New("autonomous company: evidence kind has no authoritative owner")
	ErrIntegrity           = errors.New("autonomous company: integrity failure")
)

type EvidenceVerifier interface {
	VerifyAutonomousCompanyEvidence(
		context.Context,
		pgx.Tx,
		EvidenceBinding,
		time.Time,
	) error
	VerifyAutonomousCompanyProcess(
		context.Context,
		pgx.Tx,
		ProcessIdentity,
		string,
		time.Time,
	) error
}

type SecurityQualificationSource interface {
	CurrentQualification(context.Context) (securityqualification.Qualification, error)
}

type RecoveryEvidence struct {
	Qualification EvidenceBinding `json:"qualification"`
	CleanRestore  EvidenceBinding `json:"clean_restore"`
}

func (value RecoveryEvidence) ValidateAt(at time.Time) error {
	if value.Qualification.Validate() != nil || value.CleanRestore.Validate() != nil ||
		value.Qualification.Kind != EvidenceRecoveryQualification ||
		value.CleanRestore.Kind != EvidenceCleanRestoreReceipt ||
		value.Qualification.SourceState != "qualified" ||
		value.CleanRestore.SourceState != "ready" ||
		value.Qualification.OrganizationID != value.CleanRestore.OrganizationID ||
		value.Qualification.InitiativeID != value.CleanRestore.InitiativeID ||
		!value.Qualification.currentAt(at) || !value.CleanRestore.currentAt(at) {
		return fmt.Errorf("autonomous company: recovery qualification evidence is not current")
	}
	return nil
}

type RecoveryEvidenceSource interface {
	CurrentRecoveryEvidence(context.Context, string, time.Time) (RecoveryEvidence, error)
}

type FounderProjectionSource interface {
	CurrentFounderProjection(context.Context, string, time.Time) (EvidenceBinding, error)
}

type Store struct {
	pool         *pgxpool.Pool
	vault        *vault.UserVault
	tenantID     string
	organization contracts.OrganizationID
	keyID        string
	privateKey   ed25519.PrivateKey
	publicKey    ed25519.PublicKey
	verifier     EvidenceVerifier
	security     SecurityQualificationSource
	recovery     RecoveryEvidenceSource
	projection   FounderProjectionSource
	now          func() time.Time
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	organizationID contracts.OrganizationID,
	keyID string,
	privateKey ed25519.PrivateKey,
	verifier EvidenceVerifier,
	security SecurityQualificationSource,
	recovery RecoveryEvidenceSource,
	projection FounderProjectionSource,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" || organizationID == "" ||
		token(keyID) != nil || len(privateKey) != ed25519.PrivateKeySize || verifier == nil ||
		security == nil || recovery == nil || projection == nil || now == nil ||
		userVault.User() != tenantID {
		return nil, fmt.Errorf("autonomous company: durable Store dependencies are required")
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID, organization: organizationID,
		keyID: keyID, privateKey: append(ed25519.PrivateKey(nil), privateKey...),
		publicKey: append(ed25519.PublicKey(nil), publicKey...), verifier: verifier,
		security: security, recovery: recovery, projection: projection, now: now,
	}, nil
}

func (store *Store) CommitProperty(
	ctx context.Context,
	draft PropertyDraft,
) (PropertySnapshot, bool, error) {
	if draft.Validate() != nil || draft.OrganizationID != store.organization {
		return PropertySnapshot{}, false, fmt.Errorf("autonomous company: property draft is invalid")
	}
	now, err := store.currentTime()
	if err != nil {
		return PropertySnapshot{}, false, err
	}
	if draft.EvaluatedAt.After(now) || draft.StartedAt.After(now) ||
		(draft.State == StatePassed && !draft.FreshUntil.After(now)) {
		return PropertySnapshot{}, false, fmt.Errorf("autonomous company: property draft is not current")
	}
	requestHash, err := contracts.HashCanonical(&draft)
	if err != nil {
		return PropertySnapshot{}, false, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return PropertySnapshot{}, false, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.tenantID+"|"+string(store.organization)+"|autonomous-property|"+draft.ID); err != nil {
		return PropertySnapshot{}, false, err
	}
	var replayID string
	var replayVersion uint64
	var replayHash string
	err = tx.QueryRow(ctx, `
		SELECT property_id,version,request_hash
		FROM workforce_autonomous_company_property_records
		WHERE tenant_id=$1 AND organization_id=$2 AND idempotency_key=$3
	`, store.tenantID, store.organization, draft.IdempotencyKey).Scan(
		&replayID, &replayVersion, &replayHash,
	)
	if err == nil {
		if replayID != draft.ID || replayHash != requestHash.Digest {
			return PropertySnapshot{}, false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return PropertySnapshot{}, false, err
		}
		value, err := store.LoadProperty(ctx, replayID, replayVersion)
		return value, true, err
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return PropertySnapshot{}, false, err
	}
	var currentID string
	var currentVersion uint64
	err = tx.QueryRow(ctx, `
		SELECT property_id,version
		FROM workforce_autonomous_company_property_heads
		WHERE tenant_id=$1 AND organization_id=$2
		  AND property_kind=$3 AND initiative_id=$4
		FOR UPDATE
	`, store.tenantID, store.organization, draft.Kind, draft.InitiativeID).Scan(
		&currentID, &currentVersion,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		currentVersion = 0
	case err != nil:
		return PropertySnapshot{}, false, err
	case currentID != draft.ID:
		return PropertySnapshot{}, false, ErrConflict
	}
	if err := store.verifyEvidenceSet(ctx, tx, draft, now); err != nil {
		return PropertySnapshot{}, false, err
	}
	record := PropertyRecord{
		SchemaVersion: PropertySchemaVersion,
		ID:            draft.ID, Version: currentVersion + 1, Kind: draft.Kind,
		OrganizationID: draft.OrganizationID, InitiativeID: draft.InitiativeID,
		State: draft.State, Evidence: append([]EvidenceBinding(nil), draft.Evidence...),
		Processes:   append([]ProcessIdentity(nil), draft.Processes...),
		Lineage:     append([]LineageNode(nil), draft.Lineage...),
		ReasonCodes: append([]string(nil), draft.ReasonCodes...),
		StartedAt:   draft.StartedAt, EvaluatedAt: draft.EvaluatedAt,
		FreshUntil: draft.FreshUntil, CompletedAt: cloneTime(draft.CompletedAt),
		IdempotencyKey: draft.IdempotencyKey,
	}
	if err := signPropertyRecord(&record, store.keyID, store.privateKey); err != nil {
		return PropertySnapshot{}, false, err
	}
	canonical, err := contracts.EncodeCanonical(&record)
	if err != nil {
		return PropertySnapshot{}, false, err
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.propertyAD(record.ID, record.Version), canonical)
	if err != nil {
		return PropertySnapshot{}, false, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_autonomous_company_property_records (
			tenant_id,organization_id,property_id,version,property_kind,initiative_id,
			state,request_hash,canonical_hash,sealed_record,key_id,idempotency_key,
			started_at,evaluated_at,fresh_until,completed_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)
	`, store.tenantID, store.organization, record.ID, record.Version, record.Kind,
		record.InitiativeID, record.State, requestHash.Digest, canonicalHash.Digest,
		sealed, store.keyID, record.IdempotencyKey, record.StartedAt, record.EvaluatedAt,
		record.FreshUntil, record.CompletedAt, now); err != nil {
		return PropertySnapshot{}, false, err
	}
	if err := store.insertPropertyBindings(ctx, tx, record); err != nil {
		return PropertySnapshot{}, false, err
	}
	if currentVersion == 0 {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_autonomous_company_property_heads (
				tenant_id,organization_id,property_id,property_kind,initiative_id,
				version,state,canonical_hash,evaluated_at,fresh_until,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		`, store.tenantID, store.organization, record.ID, record.Kind, record.InitiativeID,
			record.Version, record.State, canonicalHash.Digest, record.EvaluatedAt,
			record.FreshUntil, now)
	} else {
		command, updateErr := tx.Exec(ctx, `
			UPDATE workforce_autonomous_company_property_heads
			SET version=$1,state=$2,canonical_hash=$3,evaluated_at=$4,
			    fresh_until=$5,updated_at=$6
			WHERE tenant_id=$7 AND organization_id=$8 AND property_id=$9 AND version=$10
		`, record.Version, record.State, canonicalHash.Digest, record.EvaluatedAt,
			record.FreshUntil, now, store.tenantID, store.organization, record.ID,
			currentVersion)
		if updateErr != nil {
			err = updateErr
		} else if command.RowsAffected() != 1 {
			err = ErrConflict
		}
	}
	if err != nil {
		return PropertySnapshot{}, false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return PropertySnapshot{}, false, err
	}
	return PropertySnapshot{Record: record, CanonicalHash: canonicalHash}, false, nil
}

func (store *Store) LoadProperty(
	ctx context.Context,
	id string,
	version uint64,
) (PropertySnapshot, error) {
	if token(id) != nil || version == 0 {
		return PropertySnapshot{}, ErrNotFound
	}
	var expected string
	var keyID string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,key_id,sealed_record
		FROM workforce_autonomous_company_property_records
		WHERE tenant_id=$1 AND organization_id=$2 AND property_id=$3 AND version=$4
	`, store.tenantID, store.organization, id, version).Scan(&expected, &keyID, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return PropertySnapshot{}, ErrNotFound
	}
	if err != nil {
		return PropertySnapshot{}, err
	}
	opened, err := store.vault.OpenRecord(store.propertyAD(id, version), sealed)
	if err != nil || digest(opened).Digest != expected {
		return PropertySnapshot{}, ErrIntegrity
	}
	record, err := contracts.DecodeCanonical[PropertyRecord, *PropertyRecord](opened)
	if err != nil || record.ID != id || record.Version != version ||
		record.OrganizationID != store.organization || keyID != store.keyID ||
		verifyPropertyRecord(record, store.keyID, store.publicKey) != nil {
		return PropertySnapshot{}, ErrIntegrity
	}
	return PropertySnapshot{
		Record:        record,
		CanonicalHash: contracts.ContentHash{Algorithm: "sha256", Digest: expected},
	}, nil
}

func (store *Store) CurrentProperty(
	ctx context.Context,
	kind PropertyKind,
	initiativeID string,
) (PropertySnapshot, error) {
	if !kind.Valid() || token(initiativeID) != nil {
		return PropertySnapshot{}, ErrNotFound
	}
	var id string
	var version uint64
	err := store.pool.QueryRow(ctx, `
		SELECT property_id,version
		FROM workforce_autonomous_company_property_heads
		WHERE tenant_id=$1 AND organization_id=$2
		  AND property_kind=$3 AND initiative_id=$4
	`, store.tenantID, store.organization, kind, initiativeID).Scan(&id, &version)
	if errors.Is(err, pgx.ErrNoRows) {
		return PropertySnapshot{}, ErrNotFound
	}
	if err != nil {
		return PropertySnapshot{}, err
	}
	return store.LoadProperty(ctx, id, version)
}

func (store *Store) ListCurrentProperties(
	ctx context.Context,
	limit int,
) ([]PropertySnapshot, error) {
	if limit <= 0 || limit > 200 {
		return nil, fmt.Errorf("autonomous company: property list limit is invalid")
	}
	rows, err := store.pool.Query(ctx, `
		SELECT property_id,version
		FROM workforce_autonomous_company_property_heads
		WHERE tenant_id=$1 AND organization_id=$2
		ORDER BY updated_at DESC,property_kind,initiative_id
		LIMIT $3
	`, store.tenantID, store.organization, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type propertyRef struct {
		id      string
		version uint64
	}
	var refs []propertyRef
	for rows.Next() {
		var ref propertyRef
		if err := rows.Scan(&ref.id, &ref.version); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]PropertySnapshot, 0, len(refs))
	for _, ref := range refs {
		value, err := store.LoadProperty(ctx, ref.id, ref.version)
		if err != nil {
			return nil, err
		}
		result = append(result, value)
	}
	return result, nil
}

func (store *Store) CurrentReleaseProperties(
	ctx context.Context,
	initiativeID string,
) (ReleasePropertySet, error) {
	if token(initiativeID) != nil {
		return ReleasePropertySet{}, ErrNotFound
	}
	var result ReleasePropertySet
	properties := []struct {
		kind   PropertyKind
		target *PropertySnapshot
	}{
		{kind: PropertyCompanyControl, target: &result.CompanyControl},
		{kind: PropertyProductExecution, target: &result.ProductExecution},
		{kind: PropertyCommercialExecution, target: &result.CommercialExecution},
		{kind: PropertyAutonomousCompany, target: &result.AutonomousCompany},
	}
	for _, property := range properties {
		value, err := store.CurrentProperty(ctx, property.kind, initiativeID)
		if err != nil {
			return ReleasePropertySet{}, err
		}
		*property.target = value
	}
	return result, nil
}

func (store *Store) CurrentPropertyEvidence(
	ctx context.Context,
	kind PropertyKind,
	initiativeID string,
) (EvidenceBinding, error) {
	evidenceKind := EvidenceCompanyControlProperty
	switch kind {
	case PropertyProductExecution:
		evidenceKind = EvidenceProductExecutionProperty
	case PropertyCommercialExecution:
		evidenceKind = EvidenceCommercialProperty
	case PropertyCompanyControl:
	default:
		return EvidenceBinding{}, ErrEvidence
	}
	snapshot, err := store.CurrentProperty(ctx, kind, initiativeID)
	if err != nil || snapshot.Record.State != StatePassed {
		return EvidenceBinding{}, ErrEvidence
	}
	return EvidenceBinding{
		SchemaVersion:  EvidenceSchemaVersion,
		Kind:           evidenceKind,
		OrganizationID: store.organization,
		InitiativeID:   initiativeID,
		RecordID:       snapshot.Record.ID,
		RecordVersion:  snapshot.Record.Version,
		RecordHash:     snapshot.CanonicalHash,
		Authority:      snapshot.Record.Signature.KeyID,
		SourceState:    string(snapshot.Record.State),
		Validity:       contracts.ValidityActive,
		Reconciliation: ReconciliationNotApplicable,
		ObservedAt:     snapshot.Record.EvaluatedAt,
		FreshUntil:     snapshot.Record.FreshUntil,
	}, nil
}

func (store *Store) verifyEvidenceSet(
	ctx context.Context,
	tx pgx.Tx,
	draft PropertyDraft,
	now time.Time,
) error {
	for _, binding := range draft.Evidence {
		if binding.OrganizationID != store.organization {
			return ErrUnauthorized
		}
		switch binding.Kind {
		case EvidenceCompanyControlProperty, EvidenceProductExecutionProperty,
			EvidenceCommercialProperty:
			if err := store.verifyPropertyBinding(ctx, tx, binding, now); err != nil {
				return err
			}
		case EvidenceCleanRestoreReceipt:
			if err := store.verifyCleanRestoreBinding(ctx, tx, binding, now); err != nil {
				return err
			}
		case EvidenceSecurityQualification,
			EvidenceRecoveryQualification,
			EvidenceFounderProjectionReceipt:
		default:
			if err := store.verifier.VerifyAutonomousCompanyEvidence(
				ctx, tx, binding, now,
			); err != nil {
				return fmt.Errorf("%w: %s: %v", ErrEvidence, binding.Kind, err)
			}
		}
	}
	for _, process := range draft.Processes {
		if process.OrganizationID != store.organization {
			return ErrUnauthorized
		}
		if err := store.verifier.VerifyAutonomousCompanyProcess(
			ctx, tx, process, draft.InitiativeID, now,
		); err != nil {
			return fmt.Errorf("%w: process %s: %v", ErrEvidence, process.ProcessID, err)
		}
	}
	if draft.Kind != PropertyAutonomousCompany || draft.State != StatePassed {
		return nil
	}
	if err := store.verifySecurityQualification(ctx, tx, draft, now); err != nil {
		return err
	}
	recovery, err := store.recovery.CurrentRecoveryEvidence(ctx, draft.InitiativeID, now)
	if err != nil || recovery.ValidateAt(now) != nil ||
		recovery.Qualification.OrganizationID != store.organization ||
		recovery.Qualification.InitiativeID != draft.InitiativeID ||
		!containsExactEvidence(draft.Evidence, recovery.Qualification) ||
		!containsExactEvidence(draft.Evidence, recovery.CleanRestore) {
		return fmt.Errorf("%w: current recovery qualification or clean restore is absent", ErrEvidence)
	}
	projection, err := store.projection.CurrentFounderProjection(ctx, draft.InitiativeID, now)
	if err != nil || projection.Validate() != nil ||
		projection.Kind != EvidenceFounderProjectionReceipt ||
		projection.OrganizationID != store.organization ||
		projection.InitiativeID != draft.InitiativeID || !projection.currentAt(now) ||
		!containsExactEvidence(draft.Evidence, projection) {
		return fmt.Errorf("%w: current founder projection receipt is absent", ErrEvidence)
	}
	return nil
}

func (store *Store) verifyCleanRestoreBinding(
	ctx context.Context,
	tx pgx.Tx,
	binding EvidenceBinding,
	now time.Time,
) error {
	var id string
	var hash string
	var keyID string
	var state string
	var mode string
	var reconciledAt time.Time
	err := tx.QueryRow(ctx, `
		SELECT restore.restore_id,restore.receipt_hash,restore.key_id,
		       head.state,restore.mode,restore.reconciled_at
		FROM workforce_recovery_restore_heads head
		JOIN workforce_recovery_restores restore
		  ON restore.tenant_id=head.tenant_id
		 AND restore.organization_id=head.organization_id
		 AND restore.restore_id=head.restore_id
		WHERE head.tenant_id=$1 AND head.organization_id=$2
	`, store.tenantID, store.organization).Scan(
		&id, &hash, &keyID, &state, &mode, &reconciledAt,
	)
	if err != nil || id != binding.RecordID || binding.RecordVersion != 1 ||
		hash != binding.RecordHash.Digest || keyID != binding.Authority ||
		state != "ready" || binding.SourceState != state || mode != "clean" ||
		!reconciledAt.Equal(binding.ObservedAt) || !binding.currentAt(now) {
		return fmt.Errorf("%w: current clean restore receipt changed or is not ready", ErrEvidence)
	}
	return nil
}

func (store *Store) verifyNextCycleBinding(
	ctx context.Context,
	tx pgx.Tx,
	binding EvidenceBinding,
	now time.Time,
) error {
	var sequence uint64
	var hash string
	var eventState string
	var occurredAt time.Time
	var headState string
	err := tx.QueryRow(ctx, `
		SELECT event.sequence,event.canonical_hash,event.state,event.occurred_at,head.state
		FROM workforce_autonomous_company_next_cycle_events event
		JOIN workforce_autonomous_company_next_cycle_heads head
		  ON head.tenant_id=event.tenant_id
		 AND head.organization_id=event.organization_id
		 AND head.plan_id=event.plan_id
		WHERE event.tenant_id=$1 AND event.organization_id=$2
		  AND event.event_id=$3 AND event.initiative_id=$4
	`, store.tenantID, store.organization, binding.RecordID, binding.InitiativeID).Scan(
		&sequence, &hash, &eventState, &occurredAt, &headState,
	)
	if err != nil || sequence != binding.RecordVersion || hash != binding.RecordHash.Digest ||
		eventState != binding.SourceState || binding.Authority != store.keyID ||
		!occurredAt.Equal(binding.ObservedAt) ||
		(headState != string(NextCycleRunning) && headState != string(NextCyclePassed)) ||
		!binding.currentAt(now) {
		return fmt.Errorf("%w: next-cycle dispatch does not bind an active or completed exact plan", ErrEvidence)
	}
	return nil
}

func (store *Store) verifyPropertyBinding(
	ctx context.Context,
	tx pgx.Tx,
	binding EvidenceBinding,
	now time.Time,
) error {
	kind := PropertyCompanyControl
	switch binding.Kind {
	case EvidenceProductExecutionProperty:
		kind = PropertyProductExecution
	case EvidenceCommercialProperty:
		kind = PropertyCommercialExecution
	}
	var id string
	var version uint64
	var state string
	var hash string
	var keyID string
	var evaluatedAt time.Time
	var freshUntil time.Time
	err := tx.QueryRow(ctx, `
		SELECT head.property_id,head.version,head.state,head.canonical_hash,
		       record.key_id,head.evaluated_at,head.fresh_until
		FROM workforce_autonomous_company_property_heads head
		JOIN workforce_autonomous_company_property_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.property_id=head.property_id
		 AND record.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.property_kind=$3 AND head.initiative_id=$4
	`, store.tenantID, store.organization, kind, binding.InitiativeID).Scan(
		&id, &version, &state, &hash, &keyID, &evaluatedAt, &freshUntil,
	)
	if err != nil || id != binding.RecordID || version != binding.RecordVersion ||
		state != string(StatePassed) || hash != binding.RecordHash.Digest ||
		binding.SourceState != string(StatePassed) || binding.Authority != keyID ||
		!evaluatedAt.Equal(binding.ObservedAt) || !freshUntil.Equal(binding.FreshUntil) ||
		!freshUntil.After(now) || !binding.currentAt(now) {
		return fmt.Errorf("%w: %s does not bind the current passed subproperty", ErrEvidence, binding.Kind)
	}
	return nil
}

func (store *Store) verifySecurityQualification(
	ctx context.Context,
	tx pgx.Tx,
	draft PropertyDraft,
	now time.Time,
) error {
	qualification, err := store.security.CurrentQualification(ctx)
	if err != nil || qualification.OrganizationID != store.organization ||
		!qualification.ExpiresAt.After(now) {
		return fmt.Errorf("%w: current Task 21 security qualification is absent", ErrEvidence)
	}
	canonicalHash, err := contracts.HashCanonical(&qualification)
	if err != nil {
		return err
	}
	var id string
	var version uint64
	var storedHash string
	err = tx.QueryRow(ctx, `
		SELECT head.qualification_id,head.version,record.canonical_hash
		FROM workforce_security_qualification_heads head
		JOIN workforce_security_qualification_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.record_id=head.qualification_id
		 AND record.record_kind='qualification'
		 AND record.version=head.version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.state='qualified' AND head.expires_at>$3
		ORDER BY head.updated_at DESC LIMIT 1
	`, store.tenantID, store.organization, now).Scan(&id, &version, &storedHash)
	if err != nil || id != qualification.ID || storedHash != canonicalHash.Digest {
		return fmt.Errorf("%w: current Task 21 security qualification changed", ErrEvidence)
	}
	binding, found := evidenceByKind(draft.Evidence, EvidenceSecurityQualification)
	if !found || binding.RecordID != id || binding.RecordVersion != version ||
		binding.RecordHash != canonicalHash || !binding.ObservedAt.Equal(qualification.QualifiedAt) ||
		!binding.FreshUntil.Equal(qualification.ExpiresAt) ||
		binding.SourceState != "qualified" || binding.Authority != qualification.Signature.KeyID ||
		!binding.currentAt(now) {
		return fmt.Errorf("%w: release does not bind the current Task 21 qualification", ErrEvidence)
	}
	return nil
}

func (store *Store) insertPropertyBindings(
	ctx context.Context,
	tx pgx.Tx,
	record PropertyRecord,
) error {
	for _, binding := range record.Evidence {
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_autonomous_company_property_evidence (
				tenant_id,organization_id,property_id,property_version,evidence_kind,
				initiative_id,record_id,record_version,record_hash,authority,
				source_state,validity,reconciliation,contaminated,observed_at,fresh_until
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		`, store.tenantID, store.organization, record.ID, record.Version, binding.Kind,
			binding.InitiativeID, binding.RecordID, binding.RecordVersion,
			binding.RecordHash.Digest, binding.Authority, binding.SourceState,
			binding.Validity, binding.Reconciliation, binding.Contaminated,
			binding.ObservedAt, binding.FreshUntil)
		if err != nil {
			return err
		}
	}
	for _, node := range record.Lineage {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_autonomous_company_property_lineage (
				tenant_id,organization_id,property_id,property_version,position,
				stage,record_id,record_version,record_hash,observed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, store.tenantID, store.organization, record.ID, record.Version,
			node.Position, node.Stage, node.RecordID, node.RecordVersion,
			node.RecordHash.Digest, node.ObservedAt); err != nil {
			return err
		}
	}
	for _, process := range record.Processes {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_autonomous_company_property_processes (
				tenant_id,organization_id,property_id,property_version,process_id,
				wake_id,seat_id,department_id,role,memoryless,fresh_process,
				evidence_id,evidence_version,evidence_hash,started_at,observed_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		`, store.tenantID, store.organization, record.ID, record.Version,
			process.ProcessID, process.WakeID, process.SeatID, process.DepartmentID,
			process.Role, process.Memoryless, process.FreshProcess, process.EvidenceID,
			process.EvidenceVersion, process.EvidenceHash.Digest,
			process.StartedAt, process.ObservedAt); err != nil {
			return err
		}
	}
	return nil
}

func evidenceByKind(values []EvidenceBinding, kind EvidenceKind) (EvidenceBinding, bool) {
	for _, value := range values {
		if value.Kind == kind {
			return value, true
		}
	}
	return EvidenceBinding{}, false
}

func containsExactEvidence(values []EvidenceBinding, expected EvidenceBinding) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}

func cloneTime(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !utc(now) {
		return time.Time{}, fmt.Errorf("autonomous company: time source must return UTC")
	}
	return now, nil
}

func (store *Store) propertyAD(id string, version uint64) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.autonomous-company.property",
		Stream: string(store.organization) + "/" + id + "/" + strconv.FormatUint(version, 10),
		Schema: PropertySchemaVersion,
	}
}
