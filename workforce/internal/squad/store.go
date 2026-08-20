package squad

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/organization"
)

var (
	ErrConflict     = errors.New("squad: immutable conflict")
	ErrNotFound     = errors.New("squad: record not found")
	ErrIntegrity    = errors.New("squad: record integrity failure")
	ErrUnauthorized = errors.New("squad: unauthorized")
	ErrSquadLimit   = errors.New("squad: active squad limit reached")
)

const MaximumAssignmentDuration = 24 * time.Hour

type ControllerAuthority struct {
	TenantID       string
	OrganizationID contracts.OrganizationID
	KeyID          string
	PrivateKey     ed25519.PrivateKey
	EffectiveAt    time.Time
	ExpiresAt      time.Time
	HistoricalKeys []ControllerVerificationKey
}

type ControllerVerificationKey struct {
	KeyID       string
	PublicKey   ed25519.PublicKey
	EffectiveAt time.Time
	ExpiresAt   time.Time
}

func (value ControllerVerificationKey) Validate() error {
	if value.KeyID == "" || len(value.PublicKey) != ed25519.PublicKeySize ||
		!validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("squad: controller verification key is invalid")
	}
	return nil
}

func (value ControllerAuthority) Validate() error {
	if strings.TrimSpace(value.TenantID) == "" || value.OrganizationID == "" ||
		value.KeyID == "" || len(value.PrivateKey) != ed25519.PrivateKeySize ||
		!validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("squad: controller authority is incomplete")
	}
	seen := map[string]bool{value.KeyID: true}
	for _, key := range value.HistoricalKeys {
		if err := key.Validate(); err != nil {
			return err
		}
		if seen[key.KeyID] {
			return fmt.Errorf("squad: controller verification key is duplicated")
		}
		seen[key.KeyID] = true
	}
	return nil
}

type Store struct {
	pool              *pgxpool.Pool
	vault             *vault.UserVault
	organizationStore *organization.Store
	authority         ControllerAuthority
	publicKey         ed25519.PublicKey
	limits            Limits
	now               func() time.Time
}

type Limits struct {
	MaxActiveSquads       uint16
	MaxAssignmentMembers  uint16
	MaxAssignmentDuration time.Duration
}

func DefaultLimits() Limits {
	return Limits{
		MaxActiveSquads:       DefaultActiveSquads,
		MaxAssignmentMembers:  MaximumCandidates,
		MaxAssignmentDuration: MaximumAssignmentDuration,
	}
}

func (value Limits) Validate() error {
	if value.MaxActiveSquads == 0 || value.MaxActiveSquads > MaximumActiveSquads ||
		value.MaxAssignmentMembers < 2 || value.MaxAssignmentMembers > MaximumCandidates ||
		value.MaxAssignmentDuration <= 0 || value.MaxAssignmentDuration > MaximumAssignmentDuration {
		return fmt.Errorf("squad: configured limits exceed the engineering ceilings")
	}
	return nil
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	organizationStore *organization.Store,
	authority ControllerAuthority,
	now func() time.Time,
) (*Store, error) {
	return NewStoreWithLimits(
		pool, userVault, organizationStore, authority, DefaultLimits(), now,
	)
}

func NewStoreWithLimits(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	organizationStore *organization.Store,
	authority ControllerAuthority,
	limits Limits,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || organizationStore == nil || now == nil {
		return nil, fmt.Errorf("squad: PostgreSQL, Vault, organization store, and time source are required")
	}
	if err := authority.Validate(); err != nil {
		return nil, err
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if userVault.User() != authority.TenantID ||
		organizationStore.OrganizationID() != authority.OrganizationID {
		return nil, fmt.Errorf("squad: store tenant or organization boundary does not match")
	}
	privateKey := append(ed25519.PrivateKey(nil), authority.PrivateKey...)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	authority.PrivateKey = privateKey
	authority.HistoricalKeys = append([]ControllerVerificationKey(nil), authority.HistoricalKeys...)
	for index := range authority.HistoricalKeys {
		authority.HistoricalKeys[index].PublicKey = append(
			ed25519.PublicKey(nil), authority.HistoricalKeys[index].PublicKey...,
		)
	}
	return &Store{
		pool: pool, vault: userVault, organizationStore: organizationStore,
		authority: authority, publicKey: append(ed25519.PublicKey(nil), publicKey...),
		limits: limits, now: now,
	}, nil
}

func (store *Store) PublishSeatRuntimeState(
	ctx context.Context,
	value SeatRuntimeState,
) (bool, error) {
	if value.OrganizationID != store.authority.OrganizationID ||
		store.verifyRuntimeStateAuthority(value) != nil {
		return false, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if value.ObservedAt.After(now) || !value.ExpiresAt.After(now) {
		return false, ErrUnauthorized
	}
	template, err := store.organizationStore.LoadActiveTemplate(ctx)
	if err != nil {
		return false, err
	}
	mandate, found := templateMandate(template, value.SeatID)
	if !found || value.TemplateID != template.ID || value.TemplateVersion != template.Version {
		return false, ErrUnauthorized
	}
	mandateDigest, err := organization.SeatMandateDigest(mandate)
	if err != nil || mandateDigest != value.MandateDigest {
		return false, ErrUnauthorized
	}
	canonical, hash, sealed, err := store.prepareRuntimeState(value)
	if err != nil {
		return false, err
	}
	_ = canonical
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return false, fmt.Errorf("squad: begin runtime state publication: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lock(ctx, tx, "squad-assignment-plan"); err != nil {
		return false, err
	}
	if err := store.lock(ctx, tx, "runtime|"+string(value.SeatID)); err != nil {
		return false, err
	}
	if err := store.requireActiveTemplateTx(ctx, tx, template); err != nil {
		return false, err
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_squad_seat_runtime_states
		WHERE tenant_id=$1 AND organization_id=$2 AND seat_id=$3 AND version=$4
	`, store.authority.TenantID, store.authority.OrganizationID, value.SeatID, value.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return false, ErrConflict
		}
		return true, tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, fmt.Errorf("squad: inspect runtime state: %w", err)
	}
	var latestVersion uint64
	var latestObservedAt time.Time
	err = tx.QueryRow(ctx, `
		SELECT head.latest_version,state.observed_at
		FROM workforce_squad_seat_runtime_heads head
		JOIN workforce_squad_seat_runtime_states state
		  ON state.tenant_id=head.tenant_id AND state.organization_id=head.organization_id
		 AND state.seat_id=head.seat_id AND state.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.seat_id=$3
	`, store.authority.TenantID, store.authority.OrganizationID, value.SeatID).Scan(
		&latestVersion, &latestObservedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if value.Version != 1 {
			return false, ErrConflict
		}
	} else if err != nil {
		return false, fmt.Errorf("squad: inspect runtime state head: %w", err)
	} else if value.Version != latestVersion+1 || !value.ObservedAt.After(latestObservedAt) {
		return false, ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_squad_seat_runtime_states (
			tenant_id,organization_id,seat_id,version,runtime_state_id,
			template_id,template_version,mandate_digest,availability,
			canonical_hash,signature_key_id,sealed_state,observed_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15)
	`, store.authority.TenantID, store.authority.OrganizationID, value.SeatID,
		value.Version, value.ID, value.TemplateID, value.TemplateVersion,
		value.MandateDigest.Digest, value.Availability, hash, value.Signature.KeyID,
		sealed, value.ObservedAt, value.ExpiresAt, now); err != nil {
		return false, fmt.Errorf("squad: insert runtime state: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_squad_seat_runtime_heads (
			tenant_id,organization_id,seat_id,latest_version,updated_at
		) VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (tenant_id,organization_id,seat_id) DO UPDATE SET
			latest_version=EXCLUDED.latest_version,updated_at=EXCLUDED.updated_at
	`, store.authority.TenantID, store.authority.OrganizationID, value.SeatID,
		value.Version, now); err != nil {
		return false, fmt.Errorf("squad: advance runtime state head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("squad: commit runtime state: %w", err)
	}
	return false, nil
}

type AssignResult struct {
	Assignment   Assignment
	SearchNodes  uint64
	Deduplicated bool
}

type AssignmentState string

const (
	AssignmentActive  AssignmentState = "active"
	AssignmentExpired AssignmentState = "expired"
	AssignmentRevoked AssignmentState = "revoked"
)

func (store *Store) Assign(ctx context.Context, requirement Requirement) (AssignResult, error) {
	if err := requirement.Validate(); err != nil {
		return AssignResult{}, err
	}
	if requirement.OrganizationID != store.authority.OrganizationID {
		return AssignResult{}, ErrUnauthorized
	}
	now, err := store.currentTime()
	if err != nil {
		return AssignResult{}, err
	}
	if requirement.IssuedAt.After(now) ||
		requirement.ExpiresAt.Sub(requirement.IssuedAt) > store.limits.MaxAssignmentDuration ||
		requirement.MaximumMembers > store.limits.MaxAssignmentMembers ||
		requirement.IssuedAt.Before(store.authority.EffectiveAt) ||
		requirement.ExpiresAt.After(store.authority.ExpiresAt) {
		return AssignResult{}, ErrUnauthorized
	}
	template, err := store.organizationStore.LoadActiveTemplate(ctx)
	if err != nil {
		return AssignResult{}, err
	}
	templateDigest, err := organization.TemplateDigest(template)
	if err != nil || requirement.TemplateID != template.ID ||
		requirement.TemplateVersion != template.Version || requirement.TemplateDigest != templateDigest {
		return AssignResult{}, ErrUnauthorized
	}
	registry, err := store.organizationStore.LoadRegistry(ctx, requirement.IssuedAt)
	if err != nil {
		return AssignResult{}, err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return AssignResult{}, fmt.Errorf("squad: begin assignment: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.lock(ctx, tx, "squad-assignment-plan"); err != nil {
		return AssignResult{}, err
	}
	if err := store.requireActiveTemplateTx(ctx, tx, template); err != nil {
		return AssignResult{}, err
	}
	if existing, found, err := store.findAssignmentIdempotencyTx(
		ctx, tx, requirement.IdempotencyKey,
	); err != nil {
		return AssignResult{}, err
	} else if found {
		if existing.ID != requirement.ID {
			return AssignResult{}, ErrConflict
		}
		expectedRequirementDigest, err := contracts.HashCanonical(&requirement)
		if err != nil || existing.RequirementDigest != expectedRequirementDigest {
			return AssignResult{}, ErrConflict
		}
		return AssignResult{Assignment: existing, Deduplicated: true}, tx.Commit(ctx)
	}
	if !requirement.ExpiresAt.After(now) {
		return AssignResult{}, ErrUnauthorized
	}
	var activeCount int64
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_squad_assignments
		WHERE tenant_id=$1 AND organization_id=$2 AND expires_at>$3
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_squad_assignment_revocations revocation
			WHERE revocation.tenant_id=workforce_squad_assignments.tenant_id
			  AND revocation.organization_id=workforce_squad_assignments.organization_id
			  AND revocation.assignment_id=workforce_squad_assignments.assignment_id
		  )
	`, store.authority.TenantID, store.authority.OrganizationID, now).Scan(&activeCount); err != nil {
		return AssignResult{}, fmt.Errorf("squad: count active assignments: %w", err)
	}
	if activeCount >= int64(store.limits.MaxActiveSquads) {
		return AssignResult{}, ErrSquadLimit
	}
	candidates, err := store.loadCandidatesTx(ctx, tx, template, now)
	if err != nil {
		return AssignResult{}, err
	}
	selection, err := SelectSmallest(requirement, template, registry, candidates)
	if err != nil {
		return AssignResult{}, err
	}
	assignment, err := BuildSignedAssignment(
		requirement, template, selection, store.authority.KeyID, store.authority.PrivateKey,
	)
	if err != nil {
		return AssignResult{}, err
	}
	if err := VerifyAssignment(
		assignment, requirement, template, registry, candidates,
		store.authority.KeyID, store.publicKey,
	); err != nil {
		return AssignResult{}, err
	}
	canonical, hash, sealed, err := store.prepareAssignment(assignment)
	if err != nil {
		return AssignResult{}, err
	}
	_ = canonical
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_squad_assignments (
			tenant_id,organization_id,assignment_id,initiative_id,lifecycle_stage,
			template_id,template_version,template_digest,requirement_digest,
			graph_scopes,conflict_domains,member_count,authority_effect,
			receipt_schema_versions,idempotency_key,canonical_hash,signature_key_id,
			sealed_assignment,issued_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,'none',$13,$14,$15,$16,$17,$18,$19,$20)
	`, store.authority.TenantID, store.authority.OrganizationID, assignment.ID,
		assignment.InitiativeID, assignment.LifecycleStage, assignment.TemplateID,
		assignment.TemplateVersion, assignment.TemplateDigest.Digest,
		assignment.RequirementDigest.Digest, assignment.GraphScopes, assignment.ConflictDomains,
		len(assignment.Members), assignment.ReceiptSchemaVersions,
		requirement.IdempotencyKey, hash, assignment.Signature.KeyID, sealed,
		assignment.IssuedAt, assignment.ExpiresAt, now); err != nil {
		return AssignResult{}, fmt.Errorf("squad: insert assignment: %w", err)
	}
	for _, member := range assignment.Members {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_squad_assignment_members (
				tenant_id,organization_id,assignment_id,seat_id,department_id,seat_role,
				mandate_id,mandate_version,mandate_digest,binding_id,binding_version,
				independence_domain,need_ids,model_calls,tool_calls,effect_dispatches,
				memory_bytes,cost_minor,currency,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20)
		`, store.authority.TenantID, store.authority.OrganizationID, assignment.ID,
			member.SeatID, member.DepartmentID, member.Role, member.MandateID,
			member.MandateVersion, member.MandateDigest.Digest, member.ModelBinding.ID,
			member.ModelBinding.Version, member.IndependenceDomain, member.NeedIDs,
			member.AllocatedResources.ModelCalls, member.AllocatedResources.ToolCalls,
			member.AllocatedResources.EffectDispatches, member.AllocatedResources.MemoryBytes,
			member.AllocatedResources.CostMinor, member.AllocatedResources.Currency, now); err != nil {
			return AssignResult{}, fmt.Errorf("squad: insert assignment member: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return AssignResult{}, fmt.Errorf("squad: commit assignment: %w", err)
	}
	return AssignResult{Assignment: assignment, SearchNodes: selection.SearchNodes}, nil
}

func (store *Store) LoadAssignment(ctx context.Context, id AssignmentID) (Assignment, error) {
	return store.loadAssignmentQuery(ctx, store.pool, id)
}

func (store *Store) AssignmentState(
	ctx context.Context,
	id AssignmentID,
	at time.Time,
) (AssignmentState, error) {
	if !validUTC(at) {
		return "", fmt.Errorf("squad: assignment state time must be UTC")
	}
	var expiresAt time.Time
	var revokedAt *time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT assignment.expires_at,
		       (
			SELECT revocation.revoked_at FROM workforce_squad_assignment_revocations revocation
			WHERE revocation.tenant_id=assignment.tenant_id
			  AND revocation.organization_id=assignment.organization_id
			  AND revocation.assignment_id=assignment.assignment_id
		       )
		FROM workforce_squad_assignments assignment
		WHERE assignment.tenant_id=$1 AND assignment.organization_id=$2
		  AND assignment.assignment_id=$3
	`, store.authority.TenantID, store.authority.OrganizationID, id).Scan(&expiresAt, &revokedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("squad: load assignment state: %w", err)
	}
	if revokedAt != nil && !revokedAt.After(at) {
		return AssignmentRevoked, nil
	}
	if !expiresAt.After(at) {
		return AssignmentExpired, nil
	}
	return AssignmentActive, nil
}

type assignmentQuerier interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func (store *Store) loadAssignmentQuery(
	ctx context.Context,
	query assignmentQuerier,
	id AssignmentID,
) (Assignment, error) {
	var expectedHash string
	var sealed []byte
	err := query.QueryRow(ctx, `
		SELECT canonical_hash,sealed_assignment FROM workforce_squad_assignments
		WHERE tenant_id=$1 AND organization_id=$2 AND assignment_id=$3
	`, store.authority.TenantID, store.authority.OrganizationID, id).Scan(&expectedHash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assignment{}, ErrNotFound
	}
	if err != nil {
		return Assignment{}, fmt.Errorf("squad: load assignment: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.assignmentAD(id), sealed)
	if err != nil || digest(canonical) != expectedHash {
		return Assignment{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[Assignment, *Assignment](canonical)
	if err != nil || value.ID != id {
		return Assignment{}, ErrIntegrity
	}
	publicKey, err := store.controllerPublicKey(
		value.Signature.KeyID, value.IssuedAt, value.ExpiresAt,
	)
	if err != nil || VerifyAssignmentSignature(
		value, value.Signature.KeyID, publicKey,
	) != nil {
		return Assignment{}, ErrIntegrity
	}
	return copyAssignment(value), nil
}

func (store *Store) loadCandidatesTx(
	ctx context.Context,
	tx pgx.Tx,
	template organization.OrganizationTemplate,
	now time.Time,
) ([]Candidate, error) {
	candidates := make([]Candidate, 0, len(template.Departments)*3)
	for _, department := range template.Departments {
		for _, mandate := range department.Mandates {
			state, found, err := store.loadRuntimeStateTx(ctx, tx, mandate.SeatID)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			reserved := organization.ResourceVector{Currency: state.ResourceAvailable.Currency}
			activeScopes := make([]string, 0)
			rows, err := tx.Query(ctx, `
				SELECT member.model_calls,member.tool_calls,member.effect_dispatches,
				       member.memory_bytes,member.cost_minor,member.currency,
				       assignment.graph_scopes,assignment.conflict_domains
				FROM workforce_squad_assignment_members member
				JOIN workforce_squad_assignments assignment
				  ON assignment.tenant_id=member.tenant_id
				 AND assignment.organization_id=member.organization_id
				 AND assignment.assignment_id=member.assignment_id
				WHERE member.tenant_id=$1 AND member.organization_id=$2
				  AND member.seat_id=$3 AND assignment.expires_at>$4
				  AND NOT EXISTS (
					SELECT 1 FROM workforce_squad_assignment_revocations revocation
					WHERE revocation.tenant_id=assignment.tenant_id
					  AND revocation.organization_id=assignment.organization_id
					  AND revocation.assignment_id=assignment.assignment_id
				  )
				ORDER BY assignment.assignment_id
			`, store.authority.TenantID, store.authority.OrganizationID, mandate.SeatID, now)
			if err != nil {
				return nil, fmt.Errorf("squad: query active seat reservations: %w", err)
			}
			for rows.Next() {
				var item organization.ResourceVector
				var modelCalls, toolCalls, effectDispatches, memoryBytes int64
				var graphScopes, conflictDomains []string
				if err := rows.Scan(
					&modelCalls, &toolCalls, &effectDispatches,
					&memoryBytes, &item.CostMinor, &item.Currency,
					&graphScopes, &conflictDomains,
				); err != nil {
					rows.Close()
					return nil, fmt.Errorf("squad: scan active seat reservation: %w", err)
				}
				if modelCalls < 0 || modelCalls > int64(^uint32(0)) ||
					toolCalls < 0 || toolCalls > int64(^uint32(0)) ||
					effectDispatches < 0 || effectDispatches > int64(^uint16(0)) || memoryBytes < 0 {
					rows.Close()
					return nil, ErrIntegrity
				}
				item.ModelCalls = uint32(modelCalls)
				item.ToolCalls = uint32(toolCalls)
				item.EffectDispatches = uint16(effectDispatches)
				item.MemoryBytes = uint64(memoryBytes)
				reserved, err = reserved.Add(item)
				if err != nil {
					rows.Close()
					return nil, ErrIntegrity
				}
				activeScopes = append(activeScopes, graphScopes...)
				activeScopes = append(activeScopes, conflictDomains...)
			}
			if err := rows.Err(); err != nil {
				rows.Close()
				return nil, fmt.Errorf("squad: iterate active seat reservations: %w", err)
			}
			rows.Close()
			slices.Sort(activeScopes)
			activeScopes = slices.Compact(activeScopes)
			candidate := Candidate{
				DepartmentID: department.ID, Mandate: mandate, Runtime: state,
				ReservedResources: reserved, ActiveConflictScopes: activeScopes,
			}
			if err := candidate.Validate(); err != nil {
				return nil, err
			}
			candidates = append(candidates, candidate)
		}
	}
	return candidates, nil
}

func (store *Store) loadRuntimeStateTx(
	ctx context.Context,
	tx pgx.Tx,
	seatID contracts.SeatID,
) (SeatRuntimeState, bool, error) {
	var version uint64
	var expectedHash string
	var sealed []byte
	err := tx.QueryRow(ctx, `
		SELECT record.version,record.canonical_hash,record.sealed_state
		FROM workforce_squad_seat_runtime_heads head
		JOIN workforce_squad_seat_runtime_states record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.seat_id=head.seat_id AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.seat_id=$3
	`, store.authority.TenantID, store.authority.OrganizationID, seatID).Scan(
		&version, &expectedHash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return SeatRuntimeState{}, false, nil
	}
	if err != nil {
		return SeatRuntimeState{}, false, fmt.Errorf("squad: load runtime seat state: %w", err)
	}
	canonical, err := store.vault.OpenRecord(store.runtimeStateAD(seatID, version), sealed)
	if err != nil || digest(canonical) != expectedHash {
		return SeatRuntimeState{}, false, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[SeatRuntimeState, *SeatRuntimeState](canonical)
	if err != nil || value.SeatID != seatID || value.Version != version ||
		store.verifyRuntimeStateAuthority(value) != nil {
		return SeatRuntimeState{}, false, ErrIntegrity
	}
	return value, true, nil
}

func (store *Store) verifyRuntimeStateAuthority(value SeatRuntimeState) error {
	publicKey, err := store.controllerPublicKey(
		value.Signature.KeyID, value.ObservedAt, value.ExpiresAt,
	)
	if err != nil {
		return err
	}
	return VerifySeatRuntimeState(value, value.Signature.KeyID, publicKey)
}

func (store *Store) controllerPublicKey(
	keyID string,
	effectiveAt time.Time,
	expiresAt time.Time,
) (ed25519.PublicKey, error) {
	if keyID == store.authority.KeyID {
		if effectiveAt.Before(store.authority.EffectiveAt) || expiresAt.After(store.authority.ExpiresAt) {
			return nil, ErrUnauthorized
		}
		return append(ed25519.PublicKey(nil), store.publicKey...), nil
	}
	for _, key := range store.authority.HistoricalKeys {
		if key.KeyID == keyID {
			if effectiveAt.Before(key.EffectiveAt) || expiresAt.After(key.ExpiresAt) {
				return nil, ErrUnauthorized
			}
			return append(ed25519.PublicKey(nil), key.PublicKey...), nil
		}
	}
	return nil, ErrUnauthorized
}

func (store *Store) findAssignmentIdempotencyTx(
	ctx context.Context,
	tx pgx.Tx,
	idempotencyKey string,
) (Assignment, bool, error) {
	var id AssignmentID
	err := tx.QueryRow(ctx, `
		SELECT assignment_id FROM workforce_squad_assignments
		WHERE tenant_id=$1 AND organization_id=$2 AND idempotency_key=$3
	`, store.authority.TenantID, store.authority.OrganizationID, idempotencyKey).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return Assignment{}, false, nil
	}
	if err != nil {
		return Assignment{}, false, fmt.Errorf("squad: inspect assignment idempotency: %w", err)
	}
	value, err := store.loadAssignmentQuery(ctx, tx, id)
	return value, true, err
}

func (store *Store) requireActiveTemplateTx(
	ctx context.Context,
	tx pgx.Tx,
	template organization.OrganizationTemplate,
) error {
	var schemaVersion string
	var id organization.TemplateID
	var version uint64
	err := tx.QueryRow(ctx, `
		SELECT schema_version,template_id,template_version
		FROM workforce_active_organization_template
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.authority.TenantID, store.authority.OrganizationID).Scan(&schemaVersion, &id, &version)
	if err != nil || schemaVersion != organization.TemplateSchemaVersion ||
		id != template.ID || version != template.Version {
		return ErrUnauthorized
	}
	return nil
}

func templateMandate(
	template organization.OrganizationTemplate,
	seatID contracts.SeatID,
) (organization.SeatMandate, bool) {
	for _, department := range template.Departments {
		for _, mandate := range department.Mandates {
			if mandate.SeatID == seatID {
				return mandate, true
			}
		}
	}
	return organization.SeatMandate{}, false
}

func (store *Store) prepareRuntimeState(
	value SeatRuntimeState,
) ([]byte, string, []byte, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return nil, "", nil, err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.runtimeStateAD(value.SeatID, value.Version), canonical)
	if err != nil {
		return nil, "", nil, fmt.Errorf("squad: seal runtime state: %w", err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) prepareAssignment(
	value Assignment,
) ([]byte, string, []byte, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return nil, "", nil, err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.assignmentAD(value.ID), canonical)
	if err != nil {
		return nil, "", nil, fmt.Errorf("squad: seal assignment: %w", err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("squad: time source must return UTC")
	}
	return now, nil
}

func (store *Store) lock(ctx context.Context, tx pgx.Tx, identity string) error {
	_, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.authority.TenantID+"|"+string(store.authority.OrganizationID)+"|"+identity)
	if err != nil {
		return fmt.Errorf("squad: acquire transaction lock: %w", err)
	}
	return nil
}

func (store *Store) runtimeStateAD(seatID contracts.SeatID, version uint64) vault.AD {
	return vault.AD{
		User: store.authority.TenantID, Store: "workforce.squad.runtime-state",
		Stream: string(store.authority.OrganizationID) + "/" + string(seatID),
		Schema: RuntimeStateSchemaVersion + ".v" + strconv.FormatUint(version, 10),
	}
}

func (store *Store) assignmentAD(id AssignmentID) vault.AD {
	return vault.AD{
		User: store.authority.TenantID, Store: "workforce.squad.assignment",
		Stream: string(store.authority.OrganizationID) + "/" + string(id),
		Schema: AssignmentSchemaVersion,
	}
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func copyAssignment(value Assignment) Assignment {
	value.GraphScopes = append([]string(nil), value.GraphScopes...)
	value.ConflictDomains = append([]string(nil), value.ConflictDomains...)
	value.Members = copyMembers(value.Members)
	value.SatisfiedRuleIDs = append([]string(nil), value.SatisfiedRuleIDs...)
	value.ReceiptSchemaVersions = append([]string(nil), value.ReceiptSchemaVersions...)
	return value
}
