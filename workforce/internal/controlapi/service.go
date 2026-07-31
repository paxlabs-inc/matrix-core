package controlapi

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
	"matrix/workforce/scheduler"
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
	pool                 *pgxpool.Pool
	authenticator        Authenticator
	ownerKeys            map[string]OwnerKey
	now                  func() time.Time
	broker               *broker
	vault                *vault.UserVault
	runtimeKeyID         string
	runtimePublic        ed25519.PublicKey
	runtimeModelProvider string
	runtimeModelID       string
	scheduler            *scheduler.Store
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
	var ownerID, keyID string
	err := service.pool.QueryRow(ctx, `
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
	`, principal.TenantID, principal.OrganizationID).Scan(
		&ownerID, &keyID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return policy.OwnerRoot{}, ErrNotActivated
	}
	if err != nil {
		return policy.OwnerRoot{}, err
	}
	if contracts.OwnerID(ownerID) != principal.OwnerID {
		return policy.OwnerRoot{}, ErrUnauthorized
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
	seed, err := policy.BuildSeedDraft(
		principal.OrganizationID, principal.OwnerID, request.Name,
		request.EffectiveAt, request.KeyID,
		service.runtimeKeyID, service.runtimePublic,
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
		SchemaVersion: SchemaVersion, Seed: seed,
		SkillContracts: signedContracts,
	}, nil
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
		Fields: map[string]any{"departments": 7, "seats": 21},
	}
	published, err := store.PublishSeedWithCommit(
		ctx, seed,
		func(ctx context.Context, tx pgx.Tx, now time.Time) error {
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
		ResourceVersion: version, Fields: map[string]any{"action": command.Action},
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
		SELECT owner_id,public_key FROM workforce_owner_control_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND key_id=$3 AND revoked_at IS NULL
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
	if configured, exists := service.ownerKeys[principal.TenantID]; exists &&
		configured.KeyID == keyID {
		return configured, nil
	}
	var publicKey []byte
	var ownerID contracts.OwnerID
	err := service.pool.QueryRow(ctx, `
		SELECT owner_id,public_key FROM workforce_owner_control_keys
		WHERE tenant_id=$1 AND organization_id=$2 AND key_id=$3 AND revoked_at IS NULL
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
	for key, value := range fields {
		if strings.EqualFold(key, "status") {
			if text, ok := value.(string); ok && strings.Contains(strings.ToLower(text), "complete") {
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
