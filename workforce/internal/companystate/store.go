package companystate

import (
	"context"
	"crypto/ed25519"
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
)

var (
	ErrConflict               = errors.New("company state: conflict")
	ErrIntegrity              = errors.New("company state: integrity failure")
	ErrUnauthorized           = errors.New("company state: unauthorized")
	ErrSchemaMismatch         = errors.New("company state: schema mismatch")
	ErrReconciliationRequired = errors.New("company state: reconciliation required")
	ErrExpired                = errors.New("company state: expired")
)

type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	organizationID contracts.OrganizationID
	now            func() time.Time
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
		return nil, fmt.Errorf("company state: PostgreSQL, Vault, tenant, organization, and time source are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("company state: Vault user does not match tenant")
	}
	if err := validateID("organization_id", string(organizationID)); err != nil {
		return nil, err
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		organizationID: organizationID, now: now,
	}, nil
}

func (store *Store) InitializeEmpty(ctx context.Context) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("company state: begin empty initialization: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockOrganization(ctx, tx); err != nil {
		return err
	}
	var executableOrganization bool
	if err := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_organization_v2_projection
			WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
		)
	`, store.tenantID, store.organizationID).Scan(&executableOrganization); err != nil {
		return fmt.Errorf("company state: inspect executable organization: %w", err)
	}
	if !executableOrganization {
		return ErrUnauthorized
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_company_state_schema (
			tenant_id,organization_id,active_version,state,staged_manifest_id,activated_at,updated_at
		) VALUES ($1,$2,$3,'active',NULL,$4,$4)
		ON CONFLICT DO NOTHING
	`, store.tenantID, store.organizationID, StoreSchemaVersion, now)
	if err != nil {
		return fmt.Errorf("company state: initialize empty schema: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("company state: commit empty initialization: %w", err)
	}
	return nil
}

func (store *Store) Append(ctx context.Context, record Record) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if err := record.Validate(); err != nil {
		return false, err
	}
	if record.Body.OrganizationID != store.organizationID || record.Body.EffectiveAt.After(now) {
		return false, ErrUnauthorized
	}
	if record.Body.ExpiresAt != nil && !record.Body.ExpiresAt.After(now) {
		return false, ErrExpired
	}
	canonical, err := contracts.EncodeCanonical(&record)
	if err != nil {
		return false, fmt.Errorf("company state: canonical record: %w", err)
	}
	canonicalHash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.recordAD(record.Body), canonical)
	if err != nil {
		return false, fmt.Errorf("company state: seal record: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("company state: begin append: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lockRecord(ctx, tx, record.Body.ID); err != nil {
		return false, err
	}
	if err := store.requireSchemaTx(ctx, tx, StoreSchemaVersion, "active"); err != nil {
		return false, err
	}
	publicKey, err := store.resolveAuthorKeyTx(ctx, tx, record, now)
	if err != nil {
		return false, err
	}
	if err := VerifyRecord(record, publicKey); err != nil {
		return false, ErrUnauthorized
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_company_state_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, record.Body.ID, record.Body.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != canonicalHash {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("company state: commit append replay: %w", err)
		}
		return true, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("company state: inspect append replay: %w", err)
	}
	if err := store.validateRevisionTx(ctx, tx, record); err != nil {
		return false, err
	}
	references := recordReferences(record.Body)
	contaminated, err := store.validateReferencesTx(ctx, tx, references)
	if err != nil {
		return false, err
	}
	if contaminated && materialDecisionKind(record.Body.Kind) {
		return false, ErrReconciliationRequired
	}
	initiativeID := initiativeValue(record.Body.Scope)
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_company_state_records (
			tenant_id,organization_id,record_id,version,kind,domain,initiative_id,
			author_seat_id,observation_kind,truth_status,observed_at,effective_at,
			expires_at,validity,classification,content_hash,canonical_hash,
			signature_key_id,sealed_record,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
	`, store.tenantID, store.organizationID, record.Body.ID, record.Body.Version,
		record.Body.Kind, record.Body.Domain, initiativeID, record.Body.AuthorSeatID,
		record.Body.Observation, record.Body.TruthStatus, record.Body.ObservedAt,
		record.Body.EffectiveAt, record.Body.ExpiresAt, record.Body.Validity,
		record.Body.Classification, record.ContentHash.Digest, canonicalHash,
		record.Signature.KeyID, sealed, now)
	if err != nil {
		return false, fmt.Errorf("company state: insert record: %w", err)
	}
	if err := store.insertDerivationsTx(ctx, tx, record, now); err != nil {
		return false, err
	}
	if err := store.insertAccessTx(ctx, tx, record, now); err != nil {
		return false, err
	}
	if err := store.advanceHeadTx(ctx, tx, record, now); err != nil {
		return false, err
	}
	if err := store.propagateExistingContaminationTx(ctx, tx, record, references, now); err != nil {
		return false, err
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("company state: commit append: %w", err)
	}
	return false, nil
}

type ReadRequest struct {
	RecordID      string
	Version       *uint64
	PrincipalKind PrincipalKind
	PrincipalID   string
	Purpose       string
	ConsentRef    *string
	Jurisdiction  *string
}

func (value ReadRequest) Validate() error {
	if err := validateID("read record_id", value.RecordID); err != nil {
		return err
	}
	if value.Version != nil && *value.Version == 0 {
		return fmt.Errorf("company state: read version must be positive")
	}
	if !value.PrincipalKind.Valid() {
		return fmt.Errorf("company state: invalid read principal")
	}
	if err := validateID("read principal_id", value.PrincipalID); err != nil {
		return err
	}
	if err := validateText("read purpose", value.Purpose, 512); err != nil {
		return err
	}
	if value.ConsentRef != nil {
		if err := validateID("read consent_ref", *value.ConsentRef); err != nil {
			return err
		}
	}
	if value.Jurisdiction != nil {
		if err := validateID("read jurisdiction", *value.Jurisdiction); err != nil {
			return err
		}
	}
	return nil
}

type Snapshot struct {
	Record                 Record
	HeadState              string
	Expired                bool
	MateriallyContaminated bool
}

func (snapshot Snapshot) DecisionSafe() bool {
	return snapshot.HeadState == "active" && !snapshot.Expired && !snapshot.MateriallyContaminated &&
		snapshot.Record.Body.TruthStatus != TruthProposal
}

func (store *Store) Read(ctx context.Context, request ReadRequest) (Snapshot, error) {
	now, err := store.currentTime()
	if err != nil {
		return Snapshot{}, err
	}
	if err := request.Validate(); err != nil {
		return Snapshot{}, err
	}
	version, headState, expiresAt, err := store.resolveReadVersion(ctx, request)
	if err != nil {
		_ = store.auditDenial(ctx, request, "record_not_found", now)
		return Snapshot{}, err
	}
	allowed, err := store.authorizeRead(ctx, request, version, now)
	if err != nil {
		return Snapshot{}, err
	}
	if !allowed {
		if auditErr := store.auditDenial(ctx, request, "scope_denied", now); auditErr != nil {
			return Snapshot{}, fmt.Errorf("%w: denial audit failed: %v", ErrUnauthorized, auditErr)
		}
		return Snapshot{}, ErrUnauthorized
	}
	record, err := store.loadVersion(ctx, request.RecordID, version)
	if err != nil {
		return Snapshot{}, err
	}
	contaminated, err := store.materiallyContaminated(ctx, recordReference(record))
	if err != nil {
		return Snapshot{}, err
	}
	return Snapshot{
		Record: record, HeadState: headState,
		Expired:                expiresAt != nil && !expiresAt.After(now),
		MateriallyContaminated: contaminated,
	}, nil
}

func (store *Store) ReadForDecision(ctx context.Context, request ReadRequest) (Snapshot, error) {
	snapshot, err := store.Read(ctx, request)
	if err != nil {
		return Snapshot{}, err
	}
	if snapshot.Expired {
		return Snapshot{}, ErrExpired
	}
	if !snapshot.DecisionSafe() {
		return Snapshot{}, ErrReconciliationRequired
	}
	return snapshot, nil
}

func (store *Store) materiallyContaminated(ctx context.Context, reference RecordReference) (bool, error) {
	var contaminated bool
	err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_company_state_contamination
			WHERE tenant_id=$1 AND organization_id=$2
			  AND affected_record_id=$3 AND affected_version=$4
			  AND state='open' AND materially_unsafe
		)
	`, store.tenantID, store.organizationID, reference.ID, reference.Version).Scan(&contaminated)
	if err != nil {
		return false, fmt.Errorf("company state: inspect contamination: %w", err)
	}
	return contaminated, nil
}

func (store *Store) DecisionSafety(ctx context.Context, references []RecordReference) error {
	if len(references) == 0 || len(references) > 4096 {
		return fmt.Errorf("company state: decision evidence must contain 1 to 4096 references")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	for index := range references {
		if err := references[index].Validate(); err != nil {
			return err
		}
		var state string
		var expiresAt *time.Time
		err := store.pool.QueryRow(ctx, `
			SELECT head.state,head.expires_at
			FROM workforce_company_state_heads head
			JOIN workforce_company_state_records record
			  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
			 AND record.record_id=head.record_id AND record.version=head.latest_version
			WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.record_id=$3
			  AND record.version=$4 AND record.content_hash=$5
		`, store.tenantID, store.organizationID, references[index].ID,
			references[index].Version, references[index].ContentHash.Digest).Scan(&state, &expiresAt)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrReconciliationRequired
		}
		if err != nil {
			return fmt.Errorf("company state: inspect decision evidence: %w", err)
		}
		if state != "active" || expiresAt != nil && !expiresAt.After(now) {
			return ErrReconciliationRequired
		}
		contaminated, err := store.materiallyContaminated(ctx, references[index])
		if err != nil {
			return err
		}
		if contaminated {
			return ErrReconciliationRequired
		}
	}
	return nil
}

func (store *Store) validateRevisionTx(ctx context.Context, tx pgx.Tx, record Record) error {
	var version uint64
	var contentHash, state string
	err := tx.QueryRow(ctx, `
		SELECT latest_version,latest_content_hash,state
		FROM workforce_company_state_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
		FOR UPDATE
	`, store.tenantID, store.organizationID, record.Body.ID).Scan(&version, &contentHash, &state)
	if record.Body.Version == 1 {
		if err == nil {
			return ErrConflict
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("company state: inspect initial head: %w", err)
		}
		return nil
	}
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrConflict
		}
		return fmt.Errorf("company state: lock current head: %w", err)
	}
	prior := record.Body.Revision.Prior
	if prior == nil || prior.Version != version || prior.ContentHash.Digest != contentHash || state == "retracted" {
		return ErrConflict
	}
	return nil
}

func (store *Store) validateReferencesTx(ctx context.Context, tx pgx.Tx, references []RecordReference) (bool, error) {
	contaminated := false
	for index := range references {
		var count int
		err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_company_state_records
			WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
			  AND version=$4 AND content_hash=$5
		`, store.tenantID, store.organizationID, references[index].ID,
			references[index].Version, references[index].ContentHash.Digest).Scan(&count)
		if err != nil {
			return false, fmt.Errorf("company state: verify record reference: %w", err)
		}
		if count != 1 {
			return false, ErrIntegrity
		}
		var open bool
		err = tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM workforce_company_state_contamination
				WHERE tenant_id=$1 AND organization_id=$2
				  AND affected_record_id=$3 AND affected_version=$4
				  AND state='open' AND materially_unsafe
			)
		`, store.tenantID, store.organizationID, references[index].ID,
			references[index].Version).Scan(&open)
		if err != nil {
			return false, fmt.Errorf("company state: inspect referenced contamination: %w", err)
		}
		contaminated = contaminated || open
	}
	return contaminated, nil
}

func (store *Store) insertDerivationsTx(ctx context.Context, tx pgx.Tx, record Record, now time.Time) error {
	for index := range record.Body.Provenance {
		edge := record.Body.Provenance[index]
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_state_derivations (
				tenant_id,organization_id,source_record_id,source_version,
				consumer_record_id,consumer_version,relation,evidence_id,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, store.organizationID, edge.Source.ID, edge.Source.Version,
			record.Body.ID, record.Body.Version, edge.Relation, edge.EvidenceID, now)
		if err != nil {
			return fmt.Errorf("company state: insert derivation: %w", err)
		}
	}
	for index := range record.Body.Attributes {
		attribute := record.Body.Attributes[index]
		if attribute.Kind != ValueRecordReference || attribute.Reference == nil {
			continue
		}
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_state_derivations (
				tenant_id,organization_id,source_record_id,source_version,
				consumer_record_id,consumer_version,relation,evidence_id,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
			ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID, attribute.Reference.ID,
			attribute.Reference.Version, record.Body.ID, record.Body.Version,
			"typed_attribute:"+attribute.Name, record.Body.Evidence[0].ID, now)
		if err != nil {
			return fmt.Errorf("company state: insert typed derivation: %w", err)
		}
	}
	return nil
}

func (store *Store) insertAccessTx(ctx context.Context, tx pgx.Tx, record Record, now time.Time) error {
	for index := range record.Body.Access {
		grant := record.Body.Access[index]
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_state_access (
				tenant_id,organization_id,record_id,record_version,principal_kind,
				principal_id,classification,purpose,consent_ref,jurisdictions,expires_at,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		`, store.tenantID, store.organizationID, record.Body.ID, record.Body.Version,
			grant.PrincipalKind, grant.PrincipalID, grant.Classification, grant.Purpose,
			grant.ConsentRef, grant.Jurisdictions, grant.ExpiresAt, now)
		if err != nil {
			return fmt.Errorf("company state: insert access edge: %w", err)
		}
	}
	return nil
}

func (store *Store) advanceHeadTx(ctx context.Context, tx pgx.Tx, record Record, now time.Time) error {
	state := "active"
	if record.Body.Validity == contracts.ValidityContested {
		state = "contested"
	}
	if record.Body.Revision.State == RevisionRetract {
		state = "retracted"
	}
	if record.Body.Version == 1 {
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_state_heads (
				tenant_id,organization_id,record_id,kind,domain,latest_version,
				latest_content_hash,state,expires_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		`, store.tenantID, store.organizationID, record.Body.ID, record.Body.Kind,
			record.Body.Domain, record.Body.Version, record.ContentHash.Digest, state,
			record.Body.ExpiresAt, now)
		if err != nil {
			return fmt.Errorf("company state: insert current head: %w", err)
		}
		return nil
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_company_state_heads
		SET latest_version=$4,latest_content_hash=$5,state=$6,expires_at=$7,updated_at=$8
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
		  AND latest_version=$9 AND latest_content_hash=$10
	`, store.tenantID, store.organizationID, record.Body.ID, record.Body.Version,
		record.ContentHash.Digest, state, record.Body.ExpiresAt, now,
		record.Body.Revision.Prior.Version, record.Body.Revision.Prior.ContentHash.Digest)
	if err != nil {
		return fmt.Errorf("company state: advance current head: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	return nil
}

func (store *Store) propagateExistingContaminationTx(
	ctx context.Context,
	tx pgx.Tx,
	record Record,
	references []RecordReference,
	now time.Time,
) error {
	for index := range references {
		_, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_state_contamination (
				tenant_id,organization_id,correction_id,affected_record_id,
				affected_version,derivation_depth,materially_unsafe,state,contaminated_at
			)
			SELECT tenant_id,organization_id,correction_id,$5,$6,
			       derivation_depth+1,materially_unsafe,'open',$7
			FROM workforce_company_state_contamination
			WHERE tenant_id=$1 AND organization_id=$2
			  AND affected_record_id=$3 AND affected_version=$4 AND state='open'
			ON CONFLICT DO NOTHING
		`, store.tenantID, store.organizationID, references[index].ID,
			references[index].Version, record.Body.ID, record.Body.Version, now)
		if err != nil {
			return fmt.Errorf("company state: propagate existing contamination: %w", err)
		}
	}
	return nil
}

func (store *Store) resolveReadVersion(ctx context.Context, request ReadRequest) (uint64, string, *time.Time, error) {
	if request.Version != nil {
		var expiresAt *time.Time
		var state string
		err := store.pool.QueryRow(ctx, `
			SELECT record.expires_at,
			  CASE WHEN head.latest_version=record.version THEN head.state ELSE 'historical' END
			FROM workforce_company_state_records record
			JOIN workforce_company_state_heads head
			  ON head.tenant_id=record.tenant_id AND head.organization_id=record.organization_id
			 AND head.record_id=record.record_id
			WHERE record.tenant_id=$1 AND record.organization_id=$2
			  AND record.record_id=$3 AND record.version=$4
		`, store.tenantID, store.organizationID, request.RecordID, *request.Version).Scan(&expiresAt, &state)
		return *request.Version, state, expiresAt, err
	}
	var version uint64
	var state string
	var expiresAt *time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT latest_version,state,expires_at FROM workforce_company_state_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3
	`, store.tenantID, store.organizationID, request.RecordID).Scan(&version, &state, &expiresAt)
	return version, state, expiresAt, err
}

func (store *Store) authorizeRead(ctx context.Context, request ReadRequest, version uint64, now time.Time) (bool, error) {
	consent := ""
	if request.ConsentRef != nil {
		consent = *request.ConsentRef
	}
	jurisdiction := ""
	if request.Jurisdiction != nil {
		jurisdiction = *request.Jurisdiction
	}
	var allowed bool
	err := store.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM workforce_company_state_access
			WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND record_version=$4
			  AND principal_kind=$5 AND principal_id=$6 AND purpose=$7
			  AND (consent_ref IS NULL OR consent_ref=$8)
			  AND (cardinality(jurisdictions)=0 OR $9=ANY(jurisdictions))
			  AND (expires_at IS NULL OR expires_at>$10)
		)
	`, store.tenantID, store.organizationID, request.RecordID, version,
		request.PrincipalKind, request.PrincipalID, request.Purpose, consent,
		jurisdiction, now).Scan(&allowed)
	if err != nil {
		return false, fmt.Errorf("company state: authorize read: %w", err)
	}
	return allowed, nil
}

func (store *Store) loadVersion(ctx context.Context, recordID string, version uint64) (Record, error) {
	var kind RecordKind
	var domain Domain
	var keyID, expectedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT kind,domain,signature_key_id,canonical_hash,sealed_record
		FROM workforce_company_state_records
		WHERE tenant_id=$1 AND organization_id=$2 AND record_id=$3 AND version=$4
	`, store.tenantID, store.organizationID, recordID, version).Scan(
		&kind, &domain, &keyID, &expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return Record{}, ErrUnauthorized
	}
	if err != nil {
		return Record{}, fmt.Errorf("company state: load sealed record: %w", err)
	}
	body := RecordBody{ID: recordID, Version: version, Kind: kind, Domain: domain, OrganizationID: store.organizationID}
	canonical, err := store.vault.OpenRecord(store.recordAD(body), sealed)
	if err != nil || digest(canonical) != expectedHash {
		return Record{}, ErrIntegrity
	}
	record, err := contracts.DecodeCanonical[Record, *Record](canonical)
	if err != nil || record.Body.ID != recordID || record.Body.Version != version ||
		record.Body.OrganizationID != store.organizationID || record.Body.Kind != kind ||
		record.Body.Domain != domain || record.Signature.KeyID != keyID {
		return Record{}, ErrIntegrity
	}
	var publicKey []byte
	err = store.pool.QueryRow(ctx, `
		SELECT public_key FROM workforce_mail_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3 AND key_id=$4
		  AND effective_at<= $5
	`, store.tenantID, store.organizationID, record.Body.AuthorSeatID,
		record.Signature.KeyID, record.Body.EffectiveAt).Scan(&publicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize ||
		VerifyRecord(record, ed25519.PublicKey(publicKey)) != nil {
		return Record{}, ErrIntegrity
	}
	return record, nil
}

func (store *Store) resolveAuthorKeyTx(
	ctx context.Context,
	tx pgx.Tx,
	record Record,
	now time.Time,
) (ed25519.PublicKey, error) {
	if record.Body.EffectiveAt.After(now) {
		return nil, ErrUnauthorized
	}
	return store.resolveSeatKeyTx(ctx, tx, record.Body.AuthorSeatID, record.Signature.KeyID, record.Body.EffectiveAt)
}

func (store *Store) resolveSeatKeyTx(
	ctx context.Context,
	tx pgx.Tx,
	seatID contracts.SeatID,
	keyID string,
	now time.Time,
) (ed25519.PublicKey, error) {
	var publicKey []byte
	var activeSeat bool
	err := tx.QueryRow(ctx, `
		SELECT key.public_key,EXISTS (
			SELECT 1 FROM workforce_authority_heads head
			LEFT JOIN workforce_authority_revocations revoked
			  ON revoked.tenant_id=head.tenant_id AND revoked.organization_id=head.organization_id
			 AND revoked.authority_kind=head.authority_kind AND revoked.authority_id=head.authority_id
			 AND revoked.version=head.latest_version
			WHERE head.tenant_id=key.tenant_id AND head.organization_id=key.organization_id
			  AND head.authority_kind='seat' AND head.authority_id=key.seat_id
			  AND revoked.authority_id IS NULL
		)
		FROM workforce_mail_keys key
		WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.seat_id=$3
		  AND key.key_id=$4 AND key.effective_at<=$5 AND key.revoked_at IS NULL
		FOR SHARE OF key
	`, store.tenantID, store.organizationID, seatID, keyID, now).Scan(&publicKey, &activeSeat)
	if errors.Is(err, pgx.ErrNoRows) || !activeSeat || len(publicKey) != ed25519.PublicKeySize {
		return nil, ErrUnauthorized
	}
	if err != nil {
		return nil, fmt.Errorf("company state: resolve author key: %w", err)
	}
	return ed25519.PublicKey(publicKey), nil
}

func (store *Store) auditDenial(ctx context.Context, request ReadRequest, reason string, now time.Time) error {
	hash := sha256.Sum256([]byte(request.RecordID))
	_, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_company_state_access_denials (
			tenant_id,organization_id,requested_record_hash,principal_kind,
			principal_id,purpose,reason_code,denied_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, store.tenantID, store.organizationID, hex.EncodeToString(hash[:]),
		request.PrincipalKind, request.PrincipalID, request.Purpose, reason, now)
	if err != nil {
		return fmt.Errorf("company state: audit access denial: %w", err)
	}
	return nil
}

func (store *Store) requireSchemaTx(ctx context.Context, tx pgx.Tx, version, state string) error {
	var activeVersion, activeState string
	err := tx.QueryRow(ctx, `
		SELECT active_version,state FROM workforce_company_state_schema
		WHERE tenant_id=$1 AND organization_id=$2
		FOR SHARE
	`, store.tenantID, store.organizationID).Scan(&activeVersion, &activeState)
	if errors.Is(err, pgx.ErrNoRows) || activeVersion != version || activeState != state {
		return ErrSchemaMismatch
	}
	if err != nil {
		return fmt.Errorf("company state: read active schema: %w", err)
	}
	return nil
}

func (store *Store) lockOrganization(ctx context.Context, tx pgx.Tx) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		store.tenantID+"|"+string(store.organizationID)+"|company-state")
	if err != nil {
		return fmt.Errorf("company state: lock organization: %w", err)
	}
	return nil
}

func (store *Store) lockRecord(ctx context.Context, tx pgx.Tx, recordID string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`,
		store.tenantID+"|"+string(store.organizationID)+"|company-state|"+recordID)
	if err != nil {
		return fmt.Errorf("company state: lock record: %w", err)
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("company state: time source returned a non-UTC timestamp")
	}
	return now, nil
}

func (store *Store) recordAD(body RecordBody) vault.AD {
	return vault.AD{
		User:  store.tenantID,
		Store: "workforce.company-state." + string(body.Domain),
		Stream: strings.Join([]string{
			string(store.organizationID), body.ID, fmt.Sprintf("%d", body.Version),
		}, "/"),
		Schema: RecordSchemaVersion,
	}
}

func recordReference(record Record) RecordReference {
	return RecordReference{ID: record.Body.ID, Version: record.Body.Version, ContentHash: record.ContentHash}
}

func recordReferences(body RecordBody) []RecordReference {
	byKey := make(map[string]RecordReference)
	for index := range body.Provenance {
		reference := body.Provenance[index].Source
		byKey[fmt.Sprintf("%s/%d", reference.ID, reference.Version)] = reference
	}
	for index := range body.Attributes {
		if body.Attributes[index].Kind == ValueRecordReference && body.Attributes[index].Reference != nil {
			reference := *body.Attributes[index].Reference
			byKey[fmt.Sprintf("%s/%d", reference.ID, reference.Version)] = reference
		}
	}
	result := make([]RecordReference, 0, len(byKey))
	for _, reference := range byKey {
		result = append(result, reference)
	}
	return result
}

func materialDecisionKind(kind RecordKind) bool {
	switch kind {
	case RecordPortfolioDecision, RecordContract, RecordPurchase, RecordCampaign,
		RecordBusinessModel, RecordPricePackage, RecordStrategicReview:
		return true
	default:
		return false
	}
}

func initiativeValue(scope InitiativeScope) any {
	if scope.InitiativeID == nil {
		return nil
	}
	return *scope.InitiativeID
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
