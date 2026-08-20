package controlapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/autonomouscompany"
	"centra/workforce/internal/businessoutcome"
	"centra/workforce/internal/commercialexecution"
	"centra/workforce/internal/companyrecovery"
	"centra/workforce/internal/companyruntime"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/founderprojection"
	"centra/workforce/internal/learning"
	"centra/workforce/internal/mission"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/productexecution"
	"centra/workforce/internal/provider/financial"
	"centra/workforce/internal/securityqualification"
	"centra/workforce/internal/skills"
	"centra/workforce/scheduler"
)

var (
	ErrUnauthorized = errors.New("controlapi: unauthorized")
	ErrConflict     = errors.New("controlapi: stale or conflicting mutation")
	ErrNotActivated = errors.New("controlapi: organization is not activated")
)

type OwnerKey struct {
	KeyID     string
	PublicKey ed25519.PublicKey
}

type Service struct {
	pool                     *pgxpool.Pool
	authenticator            Authenticator
	ownerKeys                map[string]OwnerKey
	now                      func() time.Time
	broker                   *broker
	vault                    *vault.UserVault
	runtimeKeyID             string
	runtimePublic            ed25519.PublicKey
	companyIssuerKeyID       string
	companyIssuerPublic      ed25519.PublicKey
	runtimeModelProvider     string
	runtimeModelID           string
	scheduler                *scheduler.Store
	companyRuntime           *companyruntime.Coordinator
	productExecutionMu       sync.RWMutex
	productExecution         *productexecution.Store
	securityStore            *securityqualification.Store
	securityKeyID            string
	securityKey              ed25519.PrivateKey
	learningStore            *learning.Store
	learningEngine           *learning.Engine
	operatingStoresMu        sync.RWMutex
	financialStore           *financial.Store
	businessOutcomeStore     *businessoutcome.Store
	commercialExecutionStore *commercialexecution.Store
	commercialCoordinator    *commercialexecution.Coordinator
	companyRecovery          *companyrecovery.Store
	founderProjection        *founderprojection.Store
	autonomousCompanyStore   *autonomouscompany.Store
	autonomousCoordinator    *autonomouscompany.Coordinator
	runtimeReloadMu          sync.RWMutex
	runtimeReload            func()
}

func (service *Service) AttachRuntimeReload(reload func()) error {
	if reload == nil {
		return fmt.Errorf("controlapi: runtime reload signal is required")
	}
	service.runtimeReloadMu.Lock()
	service.runtimeReload = reload
	service.runtimeReloadMu.Unlock()
	return nil
}

func (service *Service) requestRuntimeReload() {
	service.runtimeReloadMu.RLock()
	reload := service.runtimeReload
	service.runtimeReloadMu.RUnlock()
	if reload != nil {
		reload()
	}
}

func (service *Service) AttachFinancial(store *financial.Store) error {
	if store == nil {
		return fmt.Errorf("controlapi: financial store is required")
	}
	service.operatingStoresMu.Lock()
	service.financialStore = store
	service.operatingStoresMu.Unlock()
	return nil
}

func (service *Service) AttachBusinessOutcomes(store *businessoutcome.Store) error {
	if store == nil {
		return fmt.Errorf("controlapi: business outcome store is required")
	}
	service.operatingStoresMu.Lock()
	service.businessOutcomeStore = store
	service.operatingStoresMu.Unlock()
	return nil
}

func (service *Service) AttachCommercialExecution(
	store *commercialexecution.Store,
	coordinator *commercialexecution.Coordinator,
) error {
	if store == nil || coordinator == nil {
		return fmt.Errorf("controlapi: commercial execution runtime is required")
	}
	service.operatingStoresMu.Lock()
	service.commercialExecutionStore = store
	service.commercialCoordinator = coordinator
	service.operatingStoresMu.Unlock()
	return nil
}

func (service *Service) AttachCompanyRecovery(store *companyrecovery.Store) error {
	if store == nil {
		return fmt.Errorf("controlapi: company recovery runtime is required")
	}
	service.operatingStoresMu.Lock()
	service.companyRecovery = store
	service.operatingStoresMu.Unlock()
	return nil
}

func (service *Service) AttachFounderProjection(store *founderprojection.Store) error {
	if store == nil {
		return fmt.Errorf("controlapi: founder projection store is required")
	}
	service.operatingStoresMu.Lock()
	service.founderProjection = store
	service.operatingStoresMu.Unlock()
	return nil
}

func (service *Service) AttachAutonomousCompany(
	store *autonomouscompany.Store,
	coordinator *autonomouscompany.Coordinator,
) error {
	if store == nil || coordinator == nil {
		return fmt.Errorf("controlapi: autonomous company runtime is required")
	}
	service.operatingStoresMu.Lock()
	service.autonomousCompanyStore = store
	service.autonomousCoordinator = coordinator
	service.operatingStoresMu.Unlock()
	return nil
}

func (service *Service) AttachProductExecution(store *productexecution.Store) error {
	if store == nil {
		return fmt.Errorf("controlapi: product execution store is required")
	}
	service.productExecutionMu.Lock()
	service.productExecution = store
	service.productExecutionMu.Unlock()
	return nil
}

func (service *Service) productExecutionStore() (*productexecution.Store, error) {
	service.productExecutionMu.RLock()
	store := service.productExecution
	service.productExecutionMu.RUnlock()
	if store == nil {
		return nil, fmt.Errorf("controlapi: product execution is unavailable")
	}
	return store, nil
}

func (service *Service) AttachSecurityQualification(
	store *securityqualification.Store,
	keyID string,
	key ed25519.PrivateKey,
) error {
	if store == nil || strings.TrimSpace(keyID) == "" || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("controlapi: security qualification runtime is invalid")
	}
	service.securityStore = store
	service.securityKeyID = keyID
	service.securityKey = append(ed25519.PrivateKey(nil), key...)
	return nil
}

func (service *Service) AttachLearning(store *learning.Store, engine *learning.Engine) error {
	if store == nil || engine == nil {
		return fmt.Errorf("controlapi: verified learning runtime is invalid")
	}
	service.learningStore = store
	service.learningEngine = engine
	return nil
}

func (service *Service) AttachCompanyRuntime(runtime *companyruntime.Coordinator) error {
	if runtime == nil {
		return fmt.Errorf("controlapi: company runtime is required")
	}
	service.companyRuntime = runtime
	return nil
}

// AttachCompanyIssuerAuthority binds activation previews to the dedicated
// deterministic company-controller issuer. It is never an owner authority.
func (service *Service) AttachCompanyIssuerAuthority(
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	if strings.TrimSpace(keyID) == "" || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("controlapi: company issuer authority is invalid")
	}
	if keyID == service.runtimeKeyID {
		return fmt.Errorf("controlapi: company issuer must be distinct from runtime authority")
	}
	service.companyIssuerKeyID = keyID
	service.companyIssuerPublic = append(ed25519.PublicKey(nil), publicKey...)
	return nil
}

func (service *Service) AttachScheduler(store *scheduler.Store) error {
	if store == nil {
		return fmt.Errorf("controlapi: scheduler is required")
	}
	service.scheduler = store
	return nil
}

// AttachRuntimeAuthority binds activation previews to the kernel's durable
// lease-signing key. Only the public key enters the owner-signed seed.
func (service *Service) AttachRuntimeAuthority(
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	if strings.TrimSpace(keyID) == "" ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("controlapi: runtime signing authority is invalid")
	}
	if keyID == service.companyIssuerKeyID {
		return fmt.Errorf("controlapi: runtime authority must be distinct from company issuer")
	}
	service.runtimeKeyID = keyID
	service.runtimePublic = append(ed25519.PublicKey(nil), publicKey...)
	return nil
}

// AttachRuntimeModel binds owner-signed Work Orders to the exact provider and
// model this daemon can execute.
func (service *Service) AttachRuntimeModel(provider, modelID string) error {
	provider = strings.TrimSpace(provider)
	modelID = strings.TrimSpace(modelID)
	if provider == "" || modelID == "" ||
		len(provider) > 128 || len(modelID) > 128 {
		return fmt.Errorf("controlapi: runtime model binding is invalid")
	}
	service.runtimeModelProvider = provider
	service.runtimeModelID = modelID
	return nil
}

func (service *Service) RuntimeOwnerRoot(
	ctx context.Context,
	principal Principal,
) (policy.OwnerRoot, error) {
	_, keyID, err := service.currentCompanyAuthority(ctx, principal)
	if errors.Is(err, ErrNotActivated) {
		var ownerID string
		err = service.pool.QueryRow(ctx, `
		SELECT record.owner_id,record.key_id
		FROM workforce_authority_heads head
		JOIN workforce_authority_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind
		 AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.authority_kind='organization'
		  AND head.authority_id=$2
		`, principal.TenantID, principal.OrganizationID).Scan(&ownerID, &keyID)
		if errors.Is(err, pgx.ErrNoRows) {
			return policy.OwnerRoot{}, ErrNotActivated
		}
		if err != nil {
			return policy.OwnerRoot{}, err
		}
		if contracts.OwnerID(ownerID) != principal.OwnerID {
			return policy.OwnerRoot{}, ErrUnauthorized
		}
	} else if err != nil {
		return policy.OwnerRoot{}, err
	}
	key, err := service.commandKey(ctx, principal, keyID)
	if err != nil {
		return policy.OwnerRoot{}, ErrUnauthorized
	}
	root := policy.OwnerRoot{
		TenantID: principal.TenantID, OrganizationID: principal.OrganizationID,
		OwnerID: principal.OwnerID, KeyID: key.KeyID,
		PublicKey: append(ed25519.PublicKey(nil), key.PublicKey...),
	}
	if err := root.Validate(); err != nil {
		return policy.OwnerRoot{}, err
	}
	return root, nil
}

// PreviewFounderKeyRotation returns the exact next company-authority version
// under a new key plus the dual-proof record that authorizes the root change.
func (service *Service) PreviewFounderKeyRotation(
	ctx context.Context,
	principal Principal,
	request FounderKeyRotationPreviewRequest,
) (FounderKeyRotationPreview, error) {
	oldKey, err := service.commandKey(ctx, principal, request.OldKeyID)
	if err != nil {
		return FounderKeyRotationPreview{}, ErrUnauthorized
	}
	newPublicKey, err := decodePublicKey(request.NewPublicKey)
	if err != nil || request.NewKeyID == request.OldKeyID {
		return FounderKeyRotationPreview{}, fmt.Errorf("controlapi: new founder key is invalid")
	}
	now, err := service.currentTime()
	if err != nil {
		return FounderKeyRotationPreview{}, err
	}
	if request.ExpectedVersion == 0 || request.EffectiveAt.IsZero() ||
		request.EffectiveAt.Location() != time.UTC ||
		request.EffectiveAt.After(now.Add(5*time.Minute)) ||
		request.EffectiveAt.Before(now.Add(-15*time.Minute)) {
		return FounderKeyRotationPreview{}, fmt.Errorf("controlapi: founder rotation version or time is invalid")
	}
	currentVersion, currentKeyID, err := service.currentCompanyAuthority(ctx, principal)
	if err != nil {
		return FounderKeyRotationPreview{}, err
	}
	if currentVersion != request.ExpectedVersion || currentKeyID != oldKey.KeyID {
		return FounderKeyRotationPreview{}, ErrConflict
	}
	authority, err := mission.BuildAuthorityDraft(
		principal.OrganizationID, principal.OwnerID, request.EffectiveAt,
		request.NewKeyID, service.companyIssuerKeyID, service.companyIssuerPublic,
		request.ExpectedVersion+1, request.Authority,
	)
	if err != nil {
		return FounderKeyRotationPreview{}, err
	}
	rotation := FounderKeyRotation{
		SchemaVersion: SchemaVersion, OrganizationID: principal.OrganizationID,
		OwnerID: principal.OwnerID, ExpectedVersion: request.ExpectedVersion,
		EffectiveAt: request.EffectiveAt, OldKeyID: request.OldKeyID,
		NewKeyID:     request.NewKeyID,
		NewPublicKey: base64.RawURLEncoding.EncodeToString(newPublicKey),
		OldSignature: signaturePlaceholder(request.OldKeyID),
		NewSignature: signaturePlaceholder(request.NewKeyID),
	}
	return FounderKeyRotationPreview{
		SchemaVersion: SchemaVersion, Rotation: rotation, Authority: authority,
	}, nil
}

// RotateFounderKey atomically installs the new founder key, advances every
// company-authority chain under it, revokes the old key, invalidates leases,
// pauses initiation, and records the dual-signed immutable proof.
func (service *Service) RotateFounderKey(
	ctx context.Context,
	principal Principal,
	bundle FounderKeyRotationBundle,
) (ActivationResult, error) {
	rotation := bundle.Rotation
	if service.vault == nil || service.vault.User() != principal.TenantID ||
		rotation.OrganizationID != principal.OrganizationID ||
		rotation.OwnerID != principal.OwnerID {
		return ActivationResult{}, ErrUnauthorized
	}
	oldKey, err := service.commandKey(ctx, principal, rotation.OldKeyID)
	if err != nil {
		return ActivationResult{}, ErrUnauthorized
	}
	newPublicKey, err := decodePublicKey(rotation.NewPublicKey)
	if err != nil || verifyFounderKeyRotation(rotation, oldKey.PublicKey, newPublicKey) != nil {
		return ActivationResult{}, ErrUnauthorized
	}
	if bundle.Authority.Mission.Signature.KeyID != rotation.NewKeyID {
		return ActivationResult{}, ErrConflict
	}
	store, err := mission.NewStore(
		service.pool, service.vault, principal.TenantID, principal.OrganizationID,
		principal.OwnerID, rotation.NewKeyID, newPublicKey, service.now,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	prepared, err := store.PrepareActivation(bundle.Authority)
	if err != nil {
		return ActivationResult{}, err
	}
	now, err := service.currentTime()
	if err != nil || rotation.EffectiveAt.After(now) ||
		bundle.Authority.Mission.EffectiveAt != rotation.EffectiveAt {
		return ActivationResult{}, ErrConflict
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ActivationResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		principal.TenantID+"|"+string(principal.OrganizationID)+"|founder-key"); err != nil {
		return ActivationResult{}, err
	}
	var currentVersion uint64
	var currentKeyID string
	if err := tx.QueryRow(ctx, `
		SELECT MIN(head.latest_version),MIN(record.key_id)
		FROM workforce_company_authority_heads head
		JOIN workforce_company_authority_records record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		HAVING COUNT(*)=5 AND MIN(head.latest_version)=MAX(head.latest_version)
		   AND MIN(record.key_id)=MAX(record.key_id)
	`, principal.TenantID, principal.OrganizationID).Scan(&currentVersion, &currentKeyID); err != nil ||
		currentVersion != rotation.ExpectedVersion || currentKeyID != rotation.OldKeyID {
		return ActivationResult{}, ErrConflict
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_owner_control_keys (
			tenant_id,organization_id,owner_id,key_id,public_key,registered_at
		) VALUES ($1,$2,$3,$4,$5,$6) ON CONFLICT DO NOTHING
	`, principal.TenantID, principal.OrganizationID, principal.OwnerID,
		rotation.NewKeyID, []byte(newPublicKey), now)
	if err != nil {
		return ActivationResult{}, err
	}
	if command.RowsAffected() == 0 {
		var existing []byte
		var revoked bool
		err := tx.QueryRow(ctx, `
			SELECT key.public_key,EXISTS(
			  SELECT 1 FROM workforce_owner_control_key_revocations rev
			  WHERE rev.tenant_id=key.tenant_id AND rev.organization_id=key.organization_id
			    AND rev.key_id=key.key_id
			) FROM workforce_owner_control_keys key
			WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.key_id=$3
		`, principal.TenantID, principal.OrganizationID, rotation.NewKeyID).Scan(&existing, &revoked)
		if err != nil || revoked || !ed25519.PublicKey(existing).Equal(newPublicKey) {
			return ActivationResult{}, ErrConflict
		}
	}
	if err := store.CommitVersionTx(ctx, tx, prepared, rotation.ExpectedVersion, now); err != nil {
		if errors.Is(err, mission.ErrConflict) {
			return ActivationResult{}, ErrConflict
		}
		return ActivationResult{}, err
	}
	canonical, err := founderKeyRotationSigningBytes(rotation)
	if err != nil {
		return ActivationResult{}, err
	}
	sum := sha256.Sum256(canonical)
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_founder_key_rotations (
			tenant_id,organization_id,owner_id,expected_version,old_key_id,new_key_id,
			new_public_key,canonical_hash,old_signature,new_signature,effective_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, principal.TenantID, principal.OrganizationID, principal.OwnerID,
		rotation.ExpectedVersion, rotation.OldKeyID, rotation.NewKeyID,
		[]byte(newPublicKey), hex.EncodeToString(sum[:]), rotation.OldSignature.Value,
		rotation.NewSignature.Value, rotation.EffectiveAt, now); err != nil {
		return ActivationResult{}, err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_owner_control_key_revocations (
			tenant_id,organization_id,owner_id,key_id,rotation_version,reason,revoked_at
		) VALUES ($1,$2,$3,$4,$5,'founder key rotation',$6)
	`, principal.TenantID, principal.OrganizationID, principal.OwnerID,
		rotation.OldKeyID, rotation.ExpectedVersion+1, now); err != nil {
		return ActivationResult{}, err
	}
	event := LifecycleEvent{
		SchemaVersion:  SchemaVersion,
		ID:             fmt.Sprintf("event:founder-key:%s:%d", principal.OrganizationID, rotation.ExpectedVersion+1),
		OrganizationID: principal.OrganizationID, Type: "founder_key.rotated",
		ResourceKind: "company_authority", ResourceID: string(principal.OrganizationID),
		ResourceVersion: rotation.ExpectedVersion + 1,
		Fields: map[string]any{"old_key_id": rotation.OldKeyID, "new_key_id": rotation.NewKeyID,
			"company_state": "paused", "organization_version": bundle.Authority.Organization.Version}, CreatedAt: now,
	}
	fields, err := json.Marshal(event.Fields)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO workforce_lifecycle_events (
			tenant_id,organization_id,event_id,event_type,resource_kind,resource_id,
			resource_version,verified_completion,fields,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,FALSE,$8,$9) RETURNING cursor
	`, principal.TenantID, principal.OrganizationID, event.ID, event.Type,
		event.ResourceKind, event.ResourceID, event.ResourceVersion, fields, now).Scan(&event.Cursor); err != nil {
		return ActivationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ActivationResult{}, err
	}
	service.broker.publish(topic(principal), event)
	return ActivationResult{SchemaVersion: SchemaVersion,
		OrganizationID: string(principal.OrganizationID), Departments: 7, Seats: 21,
		MissionVersion:      bundle.Authority.Mission.Version,
		ConstitutionVersion: bundle.Authority.Constitution.Version,
		OrganizationVersion: bundle.Authority.Organization.Version,
		OrganizationSchema:  "workforce.organization.v2", EventCursor: event.Cursor}, nil
}

func New(
	pool *pgxpool.Pool,
	authenticator Authenticator,
	ownerKeys map[string]OwnerKey,
	now func() time.Time,
	subscriberCapacity int,
) (*Service, error) {
	if pool == nil || authenticator == nil || len(ownerKeys) == 0 || now == nil ||
		subscriberCapacity <= 0 {
		return nil, fmt.Errorf("controlapi: store, auth, owner keys, time, and capacity are required")
	}
	cloned := make(map[string]OwnerKey, len(ownerKeys))
	for tenant, key := range ownerKeys {
		if tenant == "" || key.KeyID == "" || len(key.PublicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("controlapi: owner key binding is invalid")
		}
		key.PublicKey = append(ed25519.PublicKey(nil), key.PublicKey...)
		cloned[tenant] = key
	}
	return &Service{
		pool: pool, authenticator: authenticator, ownerKeys: cloned,
		now: now, broker: newBroker(subscriberCapacity),
	}, nil
}

// AttachVault enables activation and other sealed owner-write paths. The
// control plane remains read-only for these paths until an encrypting tenant
// Vault has been attached.
func (service *Service) AttachVault(userVault *vault.UserVault) error {
	if userVault == nil {
		return fmt.Errorf("controlapi: encrypting Vault is required")
	}
	service.vault = userVault
	return nil
}

// PreviewActivation returns the exact canonical seven-department seed with
// zero signatures for local owner signing.
func (service *Service) PreviewActivation(
	ctx context.Context,
	principal Principal,
	request ActivationPreviewRequest,
) (ActivationPreview, error) {
	key, err := service.commandKey(ctx, principal, request.KeyID)
	if err != nil || key.KeyID != request.KeyID {
		return ActivationPreview{}, ErrUnauthorized
	}
	now, err := service.currentTime()
	if err != nil {
		return ActivationPreview{}, err
	}
	if request.EffectiveAt.IsZero() || request.EffectiveAt.Location() != time.UTC ||
		request.EffectiveAt.After(now.Add(5*time.Minute)) ||
		request.EffectiveAt.Before(now.Add(-15*time.Minute)) {
		return ActivationPreview{}, fmt.Errorf("controlapi: activation effective_at is outside the acceptance window")
	}
	if service.runtimeKeyID == "" ||
		len(service.runtimePublic) != ed25519.PublicKeySize {
		return ActivationPreview{}, fmt.Errorf(
			"controlapi: runtime signing authority is unavailable",
		)
	}
	if service.companyIssuerKeyID == "" ||
		len(service.companyIssuerPublic) != ed25519.PublicKeySize {
		return ActivationPreview{}, fmt.Errorf(
			"controlapi: company issuer authority is unavailable",
		)
	}
	seed, err := policy.BuildSeedDraft(
		principal.OrganizationID, principal.OwnerID, request.Name,
		request.EffectiveAt, request.KeyID,
		service.runtimeKeyID, service.runtimePublic,
	)
	if err != nil {
		return ActivationPreview{}, err
	}
	authority, err := mission.BuildActivationDraft(
		principal.OrganizationID, principal.OwnerID, request.EffectiveAt,
		request.KeyID, service.companyIssuerKeyID, service.companyIssuerPublic,
		request.Authority,
	)
	if err != nil {
		return ActivationPreview{}, err
	}
	contractsPack, err := skills.WorkforcePack()
	if err != nil {
		return ActivationPreview{}, err
	}
	placeholder := contracts.Signature{
		Algorithm: "ed25519", KeyID: request.KeyID,
		Value: base64.RawURLEncoding.EncodeToString(
			make([]byte, ed25519.SignatureSize),
		),
	}
	signedContracts := make([]skills.SignedContract, len(contractsPack))
	for index := range contractsPack {
		signedContracts[index] = skills.SignedContract{
			SchemaVersion:  contracts.SchemaVersionV1,
			OrganizationID: principal.OrganizationID,
			Contract:       contractsPack[index],
			EffectiveAt:    request.EffectiveAt,
			Signature:      placeholder,
		}
	}
	return ActivationPreview{
		SchemaVersion: SchemaVersion, Seed: seed, Authority: authority,
		SkillContracts: signedContracts,
	}, nil
}

// PreviewMigration returns exact owner-signable company authority while
// preserving the current v1 organization record and its original signature.
func (service *Service) PreviewMigration(
	ctx context.Context,
	principal Principal,
	request MigrationPreviewRequest,
) (MigrationPreview, error) {
	key, err := service.commandKey(ctx, principal, request.KeyID)
	if err != nil || key.KeyID != request.KeyID {
		return MigrationPreview{}, ErrUnauthorized
	}
	now, err := service.currentTime()
	if err != nil {
		return MigrationPreview{}, err
	}
	if request.EffectiveAt.IsZero() || request.EffectiveAt.Location() != time.UTC ||
		request.EffectiveAt.After(now.Add(5*time.Minute)) ||
		request.EffectiveAt.Before(now.Add(-15*time.Minute)) {
		return MigrationPreview{}, fmt.Errorf("controlapi: migration effective_at is outside the acceptance window")
	}
	if service.companyIssuerKeyID == "" ||
		len(service.companyIssuerPublic) != ed25519.PublicKeySize {
		return MigrationPreview{}, fmt.Errorf("controlapi: company issuer authority is unavailable")
	}
	var legacyVersion uint64
	var ownerID contracts.OwnerID
	var currentKeyID string
	err = service.pool.QueryRow(ctx, `
		SELECT record.version,record.owner_id,record.key_id
		FROM workforce_authority_heads head
		JOIN workforce_authority_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind
		 AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.authority_kind='organization' AND head.authority_id=$2
	`, principal.TenantID, principal.OrganizationID).Scan(
		&legacyVersion, &ownerID, &currentKeyID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return MigrationPreview{}, ErrNotActivated
	}
	if err != nil {
		return MigrationPreview{}, err
	}
	if ownerID != principal.OwnerID || currentKeyID != request.KeyID {
		return MigrationPreview{}, ErrUnauthorized
	}
	authority, err := mission.BuildActivationDraft(
		principal.OrganizationID, principal.OwnerID, request.EffectiveAt,
		request.KeyID, service.companyIssuerKeyID, service.companyIssuerPublic,
		request.Authority,
	)
	if err != nil {
		return MigrationPreview{}, err
	}
	impact := MigrationImpactPreview{
		TargetTemplateID: "organization-template:default-v1", TargetTemplateVersion: 1,
		NewAuthorityKinds:         []string{"capital_envelope", "company_constitution", "company_issuer_policy", "founder_mission", "organization_v2"},
		StartingMicrounits:        request.Authority.StartingMicrounits,
		SpendCeilingMicrounits:    request.Authority.SpendCeilingMicrounits,
		ExposureCeilingMicrounits: request.Authority.ExposureCeilingMicrounits,
		Compatibility:             "all v1 authority, topology, work, mail, receipts, leases, and signatures remain byte-identical",
		Effect:                    "no external effect is executed and no current lease is widened or reissued",
		RollbackPoint:             "safe before signed commit; the commit is append-only and has no destructive downgrade",
		IrreversibleConsequences: []string{
			"five founder-signed company-authority records become durable history",
			"organization-v2 compatibility projection becomes active",
			"dedicated company issuer becomes usable only within its signed policy",
		},
	}
	err = service.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM workforce_organization_departments WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_organization_seats WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_work_nodes WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_mail_messages WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_execution_receipts WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_skill_versions WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_authority_leases WHERE tenant_id=$1 AND organization_id=$2 AND expires_at>$3)
	`, principal.TenantID, principal.OrganizationID, now).Scan(
		&impact.CurrentDepartments, &impact.CurrentSeats, &impact.CurrentWorkNodes,
		&impact.CurrentMailRecords, &impact.CurrentReceipts, &impact.CurrentSkillContracts,
		&impact.ActiveAuthorityLeases,
	)
	if err != nil {
		return MigrationPreview{}, err
	}
	return MigrationPreview{
		SchemaVersion: SchemaVersion, Authority: authority,
		LegacyOrganizationVersion: legacyVersion,
		Impact:                    impact,
	}, nil
}

// MigrateOrganization atomically adds company authority and the v2 projection
// without rewriting any current v1 authority, receipt, or organizational row.
func (service *Service) MigrateOrganization(
	ctx context.Context,
	principal Principal,
	bundle MigrationBundle,
) (ActivationResult, error) {
	if service.vault == nil || service.vault.User() != principal.TenantID {
		return ActivationResult{}, fmt.Errorf("controlapi: sealed migration is unavailable")
	}
	key, err := service.commandKey(ctx, principal, bundle.Authority.Mission.Signature.KeyID)
	if err != nil {
		return ActivationResult{}, ErrUnauthorized
	}
	store, err := mission.NewStore(
		service.pool, service.vault, principal.TenantID,
		principal.OrganizationID, principal.OwnerID, key.KeyID,
		key.PublicKey, service.now,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	prepared, err := store.PrepareActivation(bundle.Authority)
	if err != nil {
		return ActivationResult{}, err
	}
	now, err := service.currentTime()
	if err != nil {
		return ActivationResult{}, err
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ActivationResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		principal.TenantID+"|"+string(principal.OrganizationID)+"|company-migration"); err != nil {
		return ActivationResult{}, err
	}
	var currentVersion uint64
	var ownerID contracts.OwnerID
	var currentKeyID string
	err = tx.QueryRow(ctx, `
		SELECT record.version,record.owner_id,record.key_id
		FROM workforce_authority_heads head
		JOIN workforce_authority_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind
		 AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		  AND head.authority_kind='organization' AND head.authority_id=$2
		FOR SHARE
	`, principal.TenantID, principal.OrganizationID).Scan(
		&currentVersion, &ownerID, &currentKeyID,
	)
	if err != nil || currentVersion != bundle.LegacyOrganizationVersion ||
		ownerID != principal.OwnerID || currentKeyID != key.KeyID {
		return ActivationResult{}, ErrConflict
	}
	deduplicated, err := store.CommitActivationTx(
		ctx, tx, prepared, currentVersion, now,
	)
	if err != nil {
		if errors.Is(err, mission.ErrConflict) {
			return ActivationResult{}, ErrConflict
		}
		return ActivationResult{}, err
	}
	event := LifecycleEvent{
		SchemaVersion:  SchemaVersion,
		ID:             "event:organization:migrated:" + string(principal.OrganizationID),
		OrganizationID: principal.OrganizationID,
		Type:           "organization.migrated", ResourceKind: "organization",
		ResourceID: string(principal.OrganizationID), ResourceVersion: 2,
		Fields: map[string]any{
			"legacy_organization_version": currentVersion,
			"organization_version":        bundle.Authority.Organization.Version,
			"mission_version":             bundle.Authority.Mission.Version,
			"constitution_version":        bundle.Authority.Constitution.Version,
			"organization_schema":         "workforce.organization.v2",
		},
		CreatedAt: now,
	}
	fields, err := json.Marshal(event.Fields)
	if err != nil {
		return ActivationResult{}, err
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO workforce_lifecycle_events (
			tenant_id,organization_id,event_id,event_type,resource_kind,
			resource_id,resource_version,verified_completion,fields,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,FALSE,$8,$9)
		ON CONFLICT (tenant_id,organization_id,event_id) DO NOTHING
		RETURNING cursor
	`, principal.TenantID, principal.OrganizationID, event.ID, event.Type,
		event.ResourceKind, event.ResourceID, event.ResourceVersion,
		fields, now).Scan(&event.Cursor)
	if errors.Is(err, pgx.ErrNoRows) && !deduplicated {
		return ActivationResult{}, ErrConflict
	}
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return ActivationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ActivationResult{}, err
	}
	if event.Cursor != 0 {
		service.broker.publish(topic(principal), event)
	}
	return ActivationResult{
		SchemaVersion:  SchemaVersion,
		OrganizationID: string(principal.OrganizationID), Departments: 7, Seats: 21,
		MissionVersion:      bundle.Authority.Mission.Version,
		ConstitutionVersion: bundle.Authority.Constitution.Version,
		OrganizationVersion: bundle.Authority.Organization.Version,
		OrganizationSchema:  "workforce.organization.v2",
		Deduplicated:        deduplicated, EventCursor: event.Cursor,
	}, nil
}

// PreviewAuthorityChange returns the exact next founder company-authority
// version and rejects stale or mixed current versions before local signing.
func (service *Service) PreviewAuthorityChange(
	ctx context.Context,
	principal Principal,
	request AuthorityChangePreviewRequest,
) (AuthorityChangePreview, error) {
	key, err := service.commandKey(ctx, principal, request.KeyID)
	if err != nil {
		return AuthorityChangePreview{}, ErrUnauthorized
	}
	now, err := service.currentTime()
	if err != nil {
		return AuthorityChangePreview{}, err
	}
	if request.ExpectedVersion == 0 || request.EffectiveAt.IsZero() ||
		request.EffectiveAt.Location() != time.UTC ||
		request.EffectiveAt.After(now.Add(5*time.Minute)) ||
		request.EffectiveAt.Before(now.Add(-15*time.Minute)) {
		return AuthorityChangePreview{}, fmt.Errorf("controlapi: authority change version or time is invalid")
	}
	currentVersion, currentKeyID, err := service.currentCompanyAuthority(ctx, principal)
	if err != nil {
		return AuthorityChangePreview{}, err
	}
	if currentVersion != request.ExpectedVersion || currentKeyID != key.KeyID {
		return AuthorityChangePreview{}, ErrConflict
	}
	authority, err := mission.BuildAuthorityDraft(
		principal.OrganizationID, principal.OwnerID, request.EffectiveAt,
		request.KeyID, service.companyIssuerKeyID, service.companyIssuerPublic,
		request.ExpectedVersion+1, request.Authority,
	)
	if err != nil {
		return AuthorityChangePreview{}, err
	}
	store, err := mission.NewStore(
		service.pool, service.vault, principal.TenantID, principal.OrganizationID,
		principal.OwnerID, key.KeyID, key.PublicKey, service.now,
	)
	if err != nil {
		return AuthorityChangePreview{}, err
	}
	changeImpact, err := store.AnalyzeChange(ctx, now)
	if err != nil {
		return AuthorityChangePreview{}, err
	}
	impact := AuthorityChangeImpactPreview{
		CurrentVersion: request.ExpectedVersion, ProposedVersion: request.ExpectedVersion + 1,
		AffectedAuthorityKinds: []string{
			"capital_envelope", "company_constitution", "company_issuer_policy",
			"founder_mission", "organization_v2",
		},
		ActiveAuthorityLeases:      changeImpact.ActiveAuthorityLeases,
		ActiveRuntimeLeases:        changeImpact.ActiveRuntimeLeases,
		QueuedWakes:                changeImpact.QueuedWakes,
		DispatchedWakes:            changeImpact.DispatchedWakes,
		UnsettledEffects:           changeImpact.UnsettledEffects,
		InvalidatedLeaseCount:      changeImpact.ActiveAuthorityLeases + changeImpact.ActiveRuntimeLeases,
		CompanyStateAfterCommit:    "paused",
		NewInitiationAfterCommit:   "blocked until a signed founder resume",
		ExternalEffectsAfterCommit: "existing observations and reconciliation continue; new effects are blocked",
	}
	return AuthorityChangePreview{
		SchemaVersion: SchemaVersion, ExpectedVersion: request.ExpectedVersion,
		Authority: authority, Impact: impact,
	}, nil
}

// ChangeAuthority atomically commits a complete material authority version,
// invalidates affected leases, pauses initiation, and emits a change event.
func (service *Service) ChangeAuthority(
	ctx context.Context,
	principal Principal,
	bundle AuthorityChangeBundle,
) (ActivationResult, error) {
	if service.vault == nil || service.vault.User() != principal.TenantID ||
		bundle.ExpectedVersion == 0 {
		return ActivationResult{}, ErrUnauthorized
	}
	key, err := service.commandKey(ctx, principal, bundle.Authority.Mission.Signature.KeyID)
	if err != nil {
		return ActivationResult{}, ErrUnauthorized
	}
	store, err := mission.NewStore(
		service.pool, service.vault, principal.TenantID,
		principal.OrganizationID, principal.OwnerID, key.KeyID,
		key.PublicKey, service.now,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	prepared, err := store.PrepareActivation(bundle.Authority)
	if err != nil {
		return ActivationResult{}, err
	}
	now, err := service.currentTime()
	if err != nil {
		return ActivationResult{}, err
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return ActivationResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		principal.TenantID+"|"+string(principal.OrganizationID)+"|company-authority"); err != nil {
		return ActivationResult{}, err
	}
	if err := store.CommitVersionTx(
		ctx, tx, prepared, bundle.ExpectedVersion, now,
	); err != nil {
		if errors.Is(err, mission.ErrConflict) {
			return ActivationResult{}, ErrConflict
		}
		return ActivationResult{}, err
	}
	event := LifecycleEvent{
		SchemaVersion:  SchemaVersion,
		ID:             fmt.Sprintf("event:company-authority:%s:%d", principal.OrganizationID, bundle.Authority.Mission.Version),
		OrganizationID: principal.OrganizationID,
		Type:           "company_authority.changed", ResourceKind: "company_authority",
		ResourceID:      string(principal.OrganizationID),
		ResourceVersion: bundle.Authority.Mission.Version,
		Fields: map[string]any{
			"organization_version":     bundle.Authority.Organization.Version,
			"mission_version":          bundle.Authority.Mission.Version,
			"constitution_version":     bundle.Authority.Constitution.Version,
			"capital_envelope_version": bundle.Authority.Capital.Version,
			"issuer_policy_version":    bundle.Authority.IssuerPolicy.Version,
			"company_state":            "paused",
		},
		CreatedAt: now,
	}
	fields, err := json.Marshal(event.Fields)
	if err != nil {
		return ActivationResult{}, err
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO workforce_lifecycle_events (
			tenant_id,organization_id,event_id,event_type,resource_kind,
			resource_id,resource_version,verified_completion,fields,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,FALSE,$8,$9)
		RETURNING cursor
	`, principal.TenantID, principal.OrganizationID, event.ID, event.Type,
		event.ResourceKind, event.ResourceID, event.ResourceVersion, fields, now,
	).Scan(&event.Cursor); err != nil {
		return ActivationResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return ActivationResult{}, err
	}
	service.broker.publish(topic(principal), event)
	return ActivationResult{
		SchemaVersion:  SchemaVersion,
		OrganizationID: string(principal.OrganizationID), Departments: 7, Seats: 21,
		MissionVersion:      bundle.Authority.Mission.Version,
		ConstitutionVersion: bundle.Authority.Constitution.Version,
		OrganizationVersion: bundle.Authority.Organization.Version,
		OrganizationSchema:  "workforce.organization.v2", EventCursor: event.Cursor,
	}, nil
}

func (service *Service) currentCompanyAuthority(
	ctx context.Context,
	principal Principal,
) (uint64, string, error) {
	rows, err := service.pool.Query(ctx, `
		SELECT head.latest_version,record.key_id
		FROM workforce_company_authority_heads head
		JOIN workforce_company_authority_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind
		 AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
		ORDER BY head.authority_kind
	`, principal.TenantID, principal.OrganizationID)
	if err != nil {
		return 0, "", err
	}
	defer rows.Close()
	var version uint64
	var keyID string
	count := 0
	for rows.Next() {
		var currentVersion uint64
		var currentKeyID string
		if err := rows.Scan(&currentVersion, &currentKeyID); err != nil {
			return 0, "", err
		}
		if count == 0 {
			version, keyID = currentVersion, currentKeyID
		} else if currentVersion != version || currentKeyID != keyID {
			return 0, "", mission.ErrIntegrity
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return 0, "", err
	}
	if count != 5 {
		return 0, "", ErrNotActivated
	}
	return version, keyID, nil
}

// ActivateOrganization verifies and atomically persists the first signed
// organization topology.
func (service *Service) ActivateOrganization(
	ctx context.Context,
	principal Principal,
	bundle ActivationBundle,
) (ActivationResult, error) {
	seed := bundle.Seed
	if service.vault == nil || service.vault.User() != principal.TenantID {
		return ActivationResult{}, fmt.Errorf("controlapi: sealed activation is unavailable")
	}
	key, err := service.commandKey(
		ctx, principal, seed.Organization.Signature.KeyID,
	)
	if err != nil {
		return ActivationResult{}, ErrUnauthorized
	}
	store, err := policy.New(
		service.pool, service.vault,
		policy.OwnerRoot{
			TenantID: principal.TenantID, OrganizationID: principal.OrganizationID,
			OwnerID: principal.OwnerID, KeyID: key.KeyID, PublicKey: key.PublicKey,
		},
		service.now,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	missionStore, err := mission.NewStore(
		service.pool, service.vault, principal.TenantID,
		principal.OrganizationID, principal.OwnerID, key.KeyID,
		key.PublicKey, service.now,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	preparedAuthority, err := missionStore.PrepareActivation(bundle.Authority)
	if err != nil {
		return ActivationResult{}, err
	}
	skillStore, err := skills.NewStore(
		service.pool, service.vault, principal.TenantID,
		principal.OrganizationID, key.KeyID, key.PublicKey, service.now,
	)
	if err != nil {
		return ActivationResult{}, err
	}
	expectedPack, err := skills.WorkforcePack()
	if err != nil {
		return ActivationResult{}, err
	}
	if len(bundle.SkillContracts) != len(expectedPack) {
		return ActivationResult{}, fmt.Errorf(
			"controlapi: activation skill contract set is incomplete",
		)
	}
	for index := range expectedPack {
		signed := bundle.SkillContracts[index]
		if signed.Contract.ID != expectedPack[index].ID ||
			signed.Contract.Digest != expectedPack[index].Digest ||
			signed.OrganizationID != principal.OrganizationID ||
			signed.EffectiveAt != seed.Organization.EffectiveAt {
			return ActivationResult{}, fmt.Errorf(
				"controlapi: activation skill contract set is not canonical",
			)
		}
	}
	event := LifecycleEvent{
		SchemaVersion:  SchemaVersion,
		ID:             "event:organization:activated:" + string(principal.OrganizationID),
		OrganizationID: principal.OrganizationID,
		Type:           "organization.activated", ResourceKind: "organization",
		ResourceID: string(principal.OrganizationID), ResourceVersion: 1,
		Fields: map[string]any{
			"departments":          7,
			"seats":                21,
			"organization_version": bundle.Authority.Organization.Version,
			"mission_version":      bundle.Authority.Mission.Version,
			"constitution_version": bundle.Authority.Constitution.Version,
			"organization_schema":  "workforce.organization.v2",
			"execution_state":      "activated",
		},
	}
	published, err := store.PublishSeedWithCommit(
		ctx, seed,
		func(ctx context.Context, tx pgx.Tx, now time.Time) error {
			authorityDeduplicated, err := missionStore.CommitActivationTx(
				ctx, tx, preparedAuthority, seed.Organization.Version, now,
			)
			if err != nil {
				return err
			}
			for index := range bundle.SkillContracts {
				if err := skillStore.PublishTx(
					ctx, tx, bundle.SkillContracts[index], now,
				); err != nil {
					return err
				}
			}
			event.CreatedAt = now
			if err := validateEvent(event); err != nil {
				return err
			}
			fields, err := json.Marshal(event.Fields)
			if err != nil {
				return err
			}
			err = tx.QueryRow(ctx, `
				INSERT INTO workforce_lifecycle_events (
					tenant_id,organization_id,event_id,event_type,resource_kind,
					resource_id,resource_version,verified_completion,fields,created_at
				) VALUES ($1,$2,$3,$4,$5,$6,1,FALSE,$7,$8)
				ON CONFLICT (tenant_id,organization_id,event_id) DO NOTHING
				RETURNING cursor
			`, principal.TenantID, principal.OrganizationID, event.ID, event.Type,
				event.ResourceKind, event.ResourceID, fields, now).Scan(&event.Cursor)
			if errors.Is(err, pgx.ErrNoRows) {
				if !authorityDeduplicated {
					return fmt.Errorf(
						"controlapi: activation event conflicts with new authority",
					)
				}
				return nil
			}
			return err
		},
	)
	if err != nil {
		if errors.Is(err, policy.ErrStale) {
			return ActivationResult{}, ErrConflict
		}
		return ActivationResult{}, err
	}
	result := ActivationResult{
		SchemaVersion:  SchemaVersion,
		OrganizationID: string(principal.OrganizationID),
		Departments:    7, Seats: 21, Deduplicated: published.Deduplicated,
		MissionVersion:      bundle.Authority.Mission.Version,
		ConstitutionVersion: bundle.Authority.Constitution.Version,
		OrganizationVersion: bundle.Authority.Organization.Version,
		OrganizationSchema:  "workforce.organization.v2",
	}
	if published.Deduplicated {
		return result, nil
	}
	result.EventCursor = event.Cursor
	service.broker.publish(topic(principal), event)
	return result, nil
}

func (service *Service) List(
	ctx context.Context,
	principal Principal,
	resource, cursor string,
	limit int,
) (ResourcePage, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		return ResourcePage{}, fmt.Errorf("controlapi: page limit exceeds 200")
	}
	if resource == "product-records" {
		return service.listProductRecords(ctx, principal, cursor, limit)
	}
	switch resource {
	case "mission", "constitution", "capital", "company-issuer-policy",
		"operating-scopes", "reserved-decisions":
		return service.listFounderResource(ctx, principal, resource, cursor, limit)
	case "commercial-records":
		return service.listCommercialRecords(ctx, principal, cursor, limit)
	}
	return listResource(ctx, service.pool, principal, resource, cursor, limit)
}

func (service *Service) Publish(
	ctx context.Context,
	principal Principal,
	event LifecycleEvent,
) (LifecycleEvent, error) {
	if principal.TenantID == "" || principal.OrganizationID == "" ||
		event.OrganizationID != principal.OrganizationID {
		return LifecycleEvent{}, ErrUnauthorized
	}
	now, err := service.currentTime()
	if err != nil {
		return LifecycleEvent{}, err
	}
	event.SchemaVersion = SchemaVersion
	event.CreatedAt = now
	if err := validateEvent(event); err != nil {
		return LifecycleEvent{}, err
	}
	fields, err := json.Marshal(event.Fields)
	if err != nil {
		return LifecycleEvent{}, err
	}
	err = service.pool.QueryRow(ctx, `
		INSERT INTO workforce_lifecycle_events (
			tenant_id,organization_id,event_id,event_type,resource_kind,
			resource_id,resource_version,verified_completion,receipt_id,fields,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULLIF($9,''),$10,$11)
		ON CONFLICT (tenant_id,organization_id,event_id) DO NOTHING
		RETURNING cursor
	`, principal.TenantID, principal.OrganizationID, event.ID, event.Type,
		event.ResourceKind, event.ResourceID, event.ResourceVersion,
		event.VerifiedCompletion, event.ReceiptID, fields, now).Scan(&event.Cursor)
	if errors.Is(err, pgx.ErrNoRows) {
		existing := LifecycleEvent{
			SchemaVersion:  SchemaVersion,
			OrganizationID: principal.OrganizationID,
		}
		var existingFields []byte
		err = service.pool.QueryRow(ctx, `
			SELECT cursor,event_type,resource_kind,resource_id,
			       resource_version,verified_completion,
			       COALESCE(receipt_id,''),fields,created_at
			FROM workforce_lifecycle_events
			WHERE tenant_id=$1 AND organization_id=$2 AND event_id=$3
		`, principal.TenantID, principal.OrganizationID, event.ID).Scan(
			&existing.Cursor, &existing.Type, &existing.ResourceKind,
			&existing.ResourceID, &existing.ResourceVersion,
			&existing.VerifiedCompletion, &existing.ReceiptID,
			&existingFields, &existing.CreatedAt,
		)
		if err != nil {
			return LifecycleEvent{}, err
		}
		existing.ID = event.ID
		expectedFields, expectedErr := canonicalJSON(fields)
		actualFields, actualErr := canonicalJSON(existingFields)
		if expectedErr != nil || actualErr != nil ||
			existing.Type != event.Type ||
			existing.ResourceKind != event.ResourceKind ||
			existing.ResourceID != event.ResourceID ||
			existing.ResourceVersion != event.ResourceVersion ||
			existing.VerifiedCompletion != event.VerifiedCompletion ||
			existing.ReceiptID != event.ReceiptID ||
			!bytes.Equal(expectedFields, actualFields) {
			return LifecycleEvent{}, ErrConflict
		}
		if err := json.Unmarshal(existingFields, &existing.Fields); err != nil {
			return LifecycleEvent{}, err
		}
		return existing, nil
	}
	if err != nil {
		return LifecycleEvent{}, err
	}
	service.broker.publish(topic(principal), event)
	return event, nil
}

func (service *Service) Events(
	ctx context.Context,
	principal Principal,
	after uint64,
	limit int,
) (EventPage, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 500 {
		return EventPage{}, fmt.Errorf("controlapi: event limit exceeds 500")
	}
	rows, err := service.pool.Query(ctx, `
		SELECT cursor,event_id,event_type,resource_kind,resource_id,
		       resource_version,verified_completion,COALESCE(receipt_id,''),
		       fields,created_at
		FROM workforce_lifecycle_events
		WHERE tenant_id=$1 AND organization_id=$2 AND cursor>$3
		ORDER BY cursor LIMIT $4
	`, principal.TenantID, principal.OrganizationID, after, limit)
	if err != nil {
		return EventPage{}, err
	}
	defer rows.Close()
	page := EventPage{SchemaVersion: SchemaVersion, Events: make([]LifecycleEvent, 0, limit)}
	page.NextCursor = after
	for rows.Next() {
		event := LifecycleEvent{
			SchemaVersion: SchemaVersion, OrganizationID: principal.OrganizationID,
		}
		var fields []byte
		if err := rows.Scan(
			&event.Cursor, &event.ID, &event.Type, &event.ResourceKind,
			&event.ResourceID, &event.ResourceVersion, &event.VerifiedCompletion,
			&event.ReceiptID, &fields, &event.CreatedAt,
		); err != nil {
			return EventPage{}, err
		}
		if err := json.Unmarshal(fields, &event.Fields); err != nil {
			return EventPage{}, err
		}
		page.Events = append(page.Events, event)
		page.NextCursor = event.Cursor
	}
	return page, rows.Err()
}

func (service *Service) ApplyCommand(
	ctx context.Context,
	principal Principal,
	command SignedCommand,
) (CommandResult, error) {
	if command.OrganizationID != principal.OrganizationID ||
		command.OwnerID != principal.OwnerID {
		return CommandResult{}, ErrUnauthorized
	}
	if err := validateOperationalCommand(command); err != nil {
		return CommandResult{}, err
	}
	key, err := service.commandKey(ctx, principal, command.Signature.KeyID)
	if err != nil {
		return CommandResult{}, ErrUnauthorized
	}
	if err := verifyCommand(command, key.KeyID, key.PublicKey); err != nil {
		return CommandResult{}, ErrUnauthorized
	}
	now, err := service.currentTime()
	if err != nil {
		return CommandResult{}, err
	}
	if command.EffectiveAt.After(now.Add(5*time.Minute)) ||
		command.EffectiveAt.Before(now.Add(-15*time.Minute)) {
		return CommandResult{}, fmt.Errorf("controlapi: command effective_at is outside the acceptance window")
	}
	changeHash, err := commandHash(command)
	if err != nil {
		return CommandResult{}, err
	}
	tx, err := service.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return CommandResult{}, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var current uint64
	err = tx.QueryRow(ctx, `
		SELECT version FROM workforce_control_versions
		WHERE tenant_id=$1 AND organization_id=$2 AND resource_kind=$3 AND resource_id=$4
		FOR UPDATE
	`, principal.TenantID, principal.OrganizationID,
		command.ResourceKind, command.ResourceID).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return CommandResult{}, err
	}
	if command.ExpectedVersion != current {
		return CommandResult{}, ErrConflict
	}
	version := current + 1
	change, err := canonicalJSON(command.Change)
	if err != nil {
		return CommandResult{}, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_owner_commands (
			tenant_id,organization_id,command_id,owner_id,action,resource_kind,
			resource_id,expected_version,resulting_version,change_hash,change,
			key_id,signature,effective_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`, principal.TenantID, principal.OrganizationID, command.ID, command.OwnerID,
		command.Action, command.ResourceKind, command.ResourceID,
		command.ExpectedVersion, version, changeHash, change, command.Signature.KeyID,
		command.Signature.Value, command.EffectiveAt, now)
	if err != nil {
		return CommandResult{}, ErrConflict
	}
	if err := service.applyCompanyCommandTx(
		ctx, tx, principal, command, key, now,
	); err != nil {
		return CommandResult{}, err
	}
	if current == 0 {
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_control_versions (
				tenant_id,organization_id,resource_kind,resource_id,version,command_id,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, principal.TenantID, principal.OrganizationID, command.ResourceKind,
			command.ResourceID, version, command.ID, now)
	} else {
		_, err = tx.Exec(ctx, `
			UPDATE workforce_control_versions SET version=$5,command_id=$6,updated_at=$7
			WHERE tenant_id=$1 AND organization_id=$2 AND resource_kind=$3
			  AND resource_id=$4 AND version=$8
		`, principal.TenantID, principal.OrganizationID, command.ResourceKind,
			command.ResourceID, version, command.ID, now, current)
	}
	if err != nil {
		return CommandResult{}, ErrConflict
	}
	event := LifecycleEvent{
		SchemaVersion: SchemaVersion, ID: "event:" + command.ID,
		OrganizationID: principal.OrganizationID, Type: "owner_command.accepted",
		ResourceKind: command.ResourceKind, ResourceID: command.ResourceID,
		ResourceVersion: version, Fields: map[string]any{
			"action": command.Action, "state": operationalCommandState(command.Action),
		},
		CreatedAt: now,
	}
	fields, _ := json.Marshal(event.Fields)
	err = tx.QueryRow(ctx, `
		INSERT INTO workforce_lifecycle_events (
			tenant_id,organization_id,event_id,event_type,resource_kind,
			resource_id,resource_version,verified_completion,fields,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,FALSE,$8,$9)
		RETURNING cursor
	`, principal.TenantID, principal.OrganizationID, event.ID, event.Type,
		event.ResourceKind, event.ResourceID, event.ResourceVersion, fields, now,
	).Scan(&event.Cursor)
	if err != nil {
		return CommandResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return CommandResult{}, err
	}
	service.broker.publish(topic(principal), event)
	return CommandResult{
		SchemaVersion: SchemaVersion, CommandID: command.ID,
		Version: version, EventCursor: event.Cursor,
	}, nil
}

type companyStateChange struct {
	Reason string `json:"reason"`
}

type issuerRevocationChange struct {
	AuthorityID string `json:"authority_id"`
	Version     uint64 `json:"version"`
	Reason      string `json:"reason"`
}

type valueChange struct {
	Value json.RawMessage `json:"value"`
}

type scheduleChange struct {
	Enabled         bool   `json:"enabled"`
	IntervalSeconds uint64 `json:"interval_seconds"`
	Reason          string `json:"reason"`
}

type budgetChange struct {
	MaxCostMinor  int64  `json:"max_cost_minor"`
	Currency      string `json:"currency"`
	MaxModelCalls uint32 `json:"max_model_calls"`
	MaxToolCalls  uint32 `json:"max_tool_calls"`
	MaxEffects    uint16 `json:"max_effects"`
	Reason        string `json:"reason"`
}

type capitalChange struct {
	MaxAmountMinor int64  `json:"max_amount_minor"`
	Currency       string `json:"currency"`
	Reason         string `json:"reason"`
}

type modelChange struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
	Reason   string `json:"reason"`
}

type toggleChange struct {
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

func validateOperationalCommand(command SignedCommand) error {
	expected := map[string]string{
		"set_policy": "policy", "set_mandate": "mandate", "set_autonomy": "autonomy",
		"set_schedule": "schedule", "set_budget": "budget", "set_capital": "capital",
		"set_model": "model", "set_capability": "capability", "set_channel": "channel",
		"set_counterparty": "counterparty", "set_jurisdiction": "jurisdiction",
		"cancel_work": "work", "force_wake": "seat", "approve_batch": "approval-batch",
		"pause_company": "company", "resume_company": "company", "emergency_stop": "company",
		"revoke_company_issuer": "company_issuer_policy",
		"pause_portfolio":       "portfolio", "resume_portfolio": "portfolio", "terminate_portfolio": "portfolio",
		"pause_initiative": "initiative", "resume_initiative": "initiative", "terminate_initiative": "initiative",
		"pause_department": "department", "resume_department": "department",
		"pause_squad": "squad", "resume_squad": "squad",
		"pause_seat": "seat", "resume_seat": "seat",
	}[command.Action]
	if expected == "" || command.ResourceKind != expected {
		return ErrUnauthorized
	}
	validReason := func(reason string) bool {
		reason = strings.TrimSpace(reason)
		return reason != "" && len(reason) <= 512
	}
	switch command.Action {
	case "set_policy", "set_mandate", "set_autonomy", "approve_batch":
		var change valueChange
		if decodeTypedChange(command.Change, &change) != nil || len(change.Value) == 0 || !json.Valid(change.Value) {
			return fmt.Errorf("controlapi: command value is invalid")
		}
	case "set_schedule":
		var change scheduleChange
		if decodeTypedChange(command.Change, &change) != nil || !validReason(change.Reason) ||
			change.Enabled && (change.IntervalSeconds < 300 || change.IntervalSeconds > 31536000) {
			return fmt.Errorf("controlapi: schedule control is invalid")
		}
	case "set_budget":
		var change budgetChange
		if decodeTypedChange(command.Change, &change) != nil || !validReason(change.Reason) ||
			change.MaxCostMinor < 0 || strings.TrimSpace(change.Currency) == "" ||
			change.MaxModelCalls == 0 && change.MaxToolCalls == 0 && change.MaxEffects == 0 && change.MaxCostMinor == 0 {
			return fmt.Errorf("controlapi: budget control is invalid")
		}
	case "set_capital":
		var change capitalChange
		if decodeTypedChange(command.Change, &change) != nil || !validReason(change.Reason) ||
			change.MaxAmountMinor < 0 || strings.TrimSpace(change.Currency) == "" {
			return fmt.Errorf("controlapi: capital control is invalid")
		}
	case "set_model":
		var change modelChange
		if decodeTypedChange(command.Change, &change) != nil || !validReason(change.Reason) ||
			strings.TrimSpace(change.Provider) == "" || strings.TrimSpace(change.ModelID) == "" {
			return fmt.Errorf("controlapi: model control is invalid")
		}
	case "set_capability", "set_channel", "set_counterparty", "set_jurisdiction":
		var change toggleChange
		if decodeTypedChange(command.Change, &change) != nil || !validReason(change.Reason) {
			return fmt.Errorf("controlapi: scope control is invalid")
		}
	case "revoke_company_issuer":
		var change issuerRevocationChange
		if decodeTypedChange(command.Change, &change) != nil || change.AuthorityID != command.ResourceID ||
			change.Version == 0 || !validReason(change.Reason) {
			return fmt.Errorf("controlapi: issuer revocation is invalid")
		}
	default:
		var change companyStateChange
		if decodeTypedChange(command.Change, &change) != nil || !validReason(change.Reason) {
			return fmt.Errorf("controlapi: state control is invalid")
		}
	}
	return nil
}

func operationalCommandState(action string) string {
	switch action {
	case "pause_company", "resume_company", "emergency_stop", "revoke_company_issuer",
		"pause_department", "resume_department", "pause_seat", "resume_seat", "cancel_work", "force_wake":
		return "applied"
	default:
		return "pending"
	}
}

func (service *Service) applyCompanyCommandTx(
	ctx context.Context,
	tx pgx.Tx,
	principal Principal,
	command SignedCommand,
	currentKey OwnerKey,
	now time.Time,
) error {
	switch command.Action {
	case "pause_company", "resume_company", "emergency_stop":
		if command.ResourceKind != "company" ||
			command.ResourceID != string(principal.OrganizationID) {
			return ErrUnauthorized
		}
		var change companyStateChange
		if err := decodeTypedChange(command.Change, &change); err != nil ||
			strings.TrimSpace(change.Reason) == "" || len(change.Reason) > 512 {
			return fmt.Errorf("controlapi: company state change is invalid")
		}
		state := "paused"
		if command.Action == "resume_company" {
			state = "active"
		}
		result, err := tx.Exec(ctx, `
			UPDATE workforce_organization_v2_projection
			SET state=$3,paused_at=CASE WHEN $3='paused' THEN $4::timestamptz ELSE NULL END
			WHERE tenant_id=$1 AND organization_id=$2
			  AND ($3='paused' OR issuer_revoked_at IS NULL)
		`, principal.TenantID, principal.OrganizationID, state, now)
		if err != nil {
			return fmt.Errorf("controlapi: change company state: %w", err)
		}
		if result.RowsAffected() != 1 {
			return ErrConflict
		}
		if state == "paused" {
			if err := cancelCompanyRuntimeLeases(ctx, tx, principal, change.Reason, now); err != nil {
				return err
			}
		}
		if command.Action == "emergency_stop" {
			if _, err := tx.Exec(ctx, `
				UPDATE workforce_scheduled_wakes
				SET state='failed',completed_at=$3,updated_at=$3,last_error='emergency_stop'
				WHERE tenant_id=$1 AND organization_id=$2 AND state IN ('queued','dispatched')
			`, principal.TenantID, principal.OrganizationID, now); err != nil {
				return fmt.Errorf("controlapi: stop scheduled wakes: %w", err)
			}
		}
	case "pause_department", "resume_department":
		var change companyStateChange
		if err := decodeTypedChange(command.Change, &change); err != nil {
			return err
		}
		enabled := command.Action == "resume_department"
		result, err := tx.Exec(ctx, `
			UPDATE workforce_organization_departments SET enabled=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND department_id=$4
		`, enabled, principal.TenantID, principal.OrganizationID, command.ResourceID)
		if err != nil || result.RowsAffected() != 1 {
			return ErrConflict
		}
		if !enabled {
			if _, err := tx.Exec(ctx, `
				UPDATE workforce_runtime_leases SET state='cancelled',cancellation_reason=$1
				WHERE tenant_id=$2 AND organization_id=$3 AND state='active'
				  AND seat_id IN (
				    SELECT seat_id FROM workforce_organization_seats
				    WHERE tenant_id=$2 AND organization_id=$3 AND department_id=$4
				  )
			`, change.Reason, principal.TenantID, principal.OrganizationID, command.ResourceID); err != nil {
				return err
			}
		}
	case "pause_seat", "resume_seat":
		var change companyStateChange
		if err := decodeTypedChange(command.Change, &change); err != nil {
			return err
		}
		active := command.Action == "resume_seat"
		result, err := tx.Exec(ctx, `
			UPDATE workforce_organization_seats SET active=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND seat_id=$4
		`, active, principal.TenantID, principal.OrganizationID, command.ResourceID)
		if err != nil || result.RowsAffected() != 1 {
			return ErrConflict
		}
		if !active {
			if _, err := tx.Exec(ctx, `
				UPDATE workforce_runtime_leases SET state='cancelled',cancellation_reason=$1
				WHERE tenant_id=$2 AND organization_id=$3 AND seat_id=$4 AND state='active'
			`, change.Reason, principal.TenantID, principal.OrganizationID, command.ResourceID); err != nil {
				return err
			}
		}
	case "force_wake":
		var change companyStateChange
		if err := decodeTypedChange(command.Change, &change); err != nil || service.scheduler == nil {
			return fmt.Errorf("controlapi: force-wake scheduler is unavailable")
		}
		if _, err := service.scheduler.ForceSeatTx(
			ctx, tx, string(principal.OrganizationID), command.ResourceID,
			command.ID, change.Reason, now,
		); err != nil {
			return err
		}
	case "cancel_work":
		var change companyStateChange
		if err := decodeTypedChange(command.Change, &change); err != nil {
			return err
		}
		result, err := tx.Exec(ctx, `
			UPDATE workforce_scheduled_wakes SET state='failed',completed_at=$1,
			    updated_at=$1,last_error=$2
			WHERE tenant_id=$3 AND organization_id=$4 AND state IN ('queued','dispatched')
			  AND (wake_id=$5 OR wake_id IN (
			    SELECT wake_id FROM workforce_company_work_order_dispatches
			    WHERE tenant_id=$3 AND organization_id=$4 AND work_order_id=$5
			  ))
		`, now, change.Reason, principal.TenantID, principal.OrganizationID, command.ResourceID)
		if err != nil || result.RowsAffected() == 0 {
			return ErrConflict
		}
	case "revoke_company_issuer":
		if command.ResourceKind != "company_issuer_policy" {
			return ErrUnauthorized
		}
		var change issuerRevocationChange
		if err := decodeTypedChange(command.Change, &change); err != nil ||
			change.AuthorityID != command.ResourceID || change.Version == 0 ||
			strings.TrimSpace(change.Reason) == "" || len(change.Reason) > 512 {
			return fmt.Errorf("controlapi: issuer revocation is invalid")
		}
		result, err := tx.Exec(ctx, `
			INSERT INTO workforce_company_authority_revocations (
				tenant_id,organization_id,authority_kind,authority_id,version,
				reason,key_id,signature,revoked_at
			) SELECT $1,$2,'company_issuer_policy',$3,$4,$5,$6,$7,$8
			WHERE EXISTS (
			  SELECT 1 FROM workforce_company_authority_heads
			  WHERE tenant_id=$1 AND organization_id=$2
			    AND authority_kind='company_issuer_policy' AND authority_id=$3
			    AND latest_version=$4
			)
		`, principal.TenantID, principal.OrganizationID, change.AuthorityID,
			change.Version, change.Reason, currentKey.KeyID,
			command.Signature.Value, now)
		if err != nil || result.RowsAffected() != 1 {
			return ErrConflict
		}
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_organization_v2_projection
			SET state='paused',paused_at=$3,issuer_revoked_at=$3
			WHERE tenant_id=$1 AND organization_id=$2
		`, principal.TenantID, principal.OrganizationID, now); err != nil {
			return err
		}
		if err := cancelCompanyRuntimeLeases(ctx, tx, principal, change.Reason, now); err != nil {
			return err
		}
	}
	return nil
}

func cancelCompanyRuntimeLeases(
	ctx context.Context,
	tx pgx.Tx,
	principal Principal,
	reason string,
	now time.Time,
) error {
	_, err := tx.Exec(ctx, `
		UPDATE workforce_runtime_leases
		SET state='cancelled',cancellation_reason=$3
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
		  AND expires_at>$4
	`, principal.TenantID, principal.OrganizationID, reason, now)
	if err != nil {
		return fmt.Errorf("controlapi: cancel runtime leases: %w", err)
	}
	return nil
}

func decodeTypedChange(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return fmt.Errorf("controlapi: trailing command change")
	}
	return nil
}

func (service *Service) RegisterControlKey(
	ctx context.Context,
	principal Principal,
	registration ControlKeyRegistration,
) error {
	if principal.TenantID == "" || principal.OrganizationID == "" ||
		principal.OwnerID == "" || registration.KeyID == "" {
		return ErrUnauthorized
	}
	publicKey, err := decodePublicKey(registration.PublicKey)
	if err != nil {
		return err
	}
	now, err := service.currentTime()
	if err != nil {
		return err
	}
	command, err := service.pool.Exec(ctx, `
		INSERT INTO workforce_owner_control_keys (
			tenant_id,organization_id,owner_id,key_id,public_key,registered_at
		) VALUES ($1,$2,$3,$4,$5,$6)
		ON CONFLICT DO NOTHING
	`, principal.TenantID, principal.OrganizationID, principal.OwnerID,
		registration.KeyID, []byte(publicKey), now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 1 {
		return nil
	}
	var existing []byte
	var ownerID contracts.OwnerID
	err = service.pool.QueryRow(ctx, `
		SELECT key.owner_id,key.public_key FROM workforce_owner_control_keys key
		WHERE key.tenant_id=$1 AND key.organization_id=$2 AND key.key_id=$3
		  AND NOT EXISTS (
		    SELECT 1 FROM workforce_owner_control_key_revocations rev
		    WHERE rev.tenant_id=key.tenant_id AND rev.organization_id=key.organization_id
		      AND rev.key_id=key.key_id
		  )
	`, principal.TenantID, principal.OrganizationID, registration.KeyID,
	).Scan(&ownerID, &existing)
	if err != nil || ownerID != principal.OwnerID ||
		!ed25519.PublicKey(existing).Equal(publicKey) {
		return ErrConflict
	}
	return nil
}

func (service *Service) commandKey(
	ctx context.Context,
	principal Principal,
	keyID string,
) (OwnerKey, error) {
	var revoked bool
	if err := service.pool.QueryRow(ctx, `
		SELECT EXISTS(
		  SELECT 1 FROM workforce_owner_control_key_revocations
		  WHERE tenant_id=$1 AND organization_id=$2 AND key_id=$3
		)
	`, principal.TenantID, principal.OrganizationID, keyID).Scan(&revoked); err != nil || revoked {
		return OwnerKey{}, ErrUnauthorized
	}
	if configured, exists := service.ownerKeys[principal.TenantID]; exists &&
		configured.KeyID == keyID {
		return configured, nil
	}
	var publicKey []byte
	var ownerID contracts.OwnerID
	err := service.pool.QueryRow(ctx, `
		SELECT owner_id,public_key FROM workforce_owner_control_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND key_id=$3
	`, principal.TenantID, principal.OrganizationID, keyID).Scan(&ownerID, &publicKey)
	if err != nil || ownerID != principal.OwnerID || len(publicKey) != ed25519.PublicKeySize {
		return OwnerKey{}, ErrUnauthorized
	}
	return OwnerKey{KeyID: keyID, PublicKey: ed25519.PublicKey(publicKey)}, nil
}

func (service *Service) currentTime() (time.Time, error) {
	now := service.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("controlapi: time source must return UTC")
	}
	return now, nil
}

func validateEvent(event LifecycleEvent) error {
	if event.ID == "" || event.OrganizationID == "" || event.Type == "" ||
		event.ResourceKind == "" || event.ResourceID == "" || event.Fields == nil ||
		event.CreatedAt.IsZero() || event.CreatedAt.Location() != time.UTC {
		return fmt.Errorf("controlapi: lifecycle event is incomplete")
	}
	lowerType := strings.ToLower(event.Type)
	if (strings.Contains(lowerType, "complete") ||
		containsCompletedStatus(event.Fields)) &&
		(!event.VerifiedCompletion || event.ReceiptID == "") {
		return fmt.Errorf("controlapi: completion events require a verified receipt")
	}
	if event.VerifiedCompletion && event.ReceiptID == "" {
		return fmt.Errorf("controlapi: verified completion requires receipt_id")
	}
	if unsafeFields(event.Fields) {
		return fmt.Errorf("controlapi: event fields contain prohibited content")
	}
	return nil
}

func unsafeFields(value any) bool {
	switch current := value.(type) {
	case map[string]any:
		for key, nested := range current {
			lower := strings.ToLower(key)
			for _, prohibited := range []string{
				"reasoning", "chain_of_thought", "prompt", "secret",
				"token", "credential", "password", "environment", "env_var",
				"api_key", "private_key", "access_key",
			} {
				if strings.Contains(lower, prohibited) {
					return true
				}
			}
			if unsafeFields(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range current {
			if unsafeFields(nested) {
				return true
			}
		}
	}
	return false
}

func containsCompletedStatus(fields map[string]any) bool {
	return containsCompletedStatusValue(fields)
}

func containsCompletedStatusValue(value any) bool {
	switch fields := value.(type) {
	case map[string]any:
		for key, nested := range fields {
			if strings.EqualFold(key, "status") {
				if text, ok := nested.(string); ok && strings.Contains(strings.ToLower(text), "complete") {
					return true
				}
			}
			if containsCompletedStatusValue(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range fields {
			if containsCompletedStatusValue(nested) {
				return true
			}
		}
	}
	return false
}

func canonicalJSON(value []byte) ([]byte, error) {
	var decoded any
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}

func topic(principal Principal) string {
	return principal.TenantID + "\x1f" + string(principal.OrganizationID)
}
