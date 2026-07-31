package developer

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/projectbrain"
)

var (
	// ErrConflict reports an overlapping active Developer change scope.
	ErrConflict = errors.New("developer change scope conflicts with active work")
	// ErrDependency reports a task whose durable work-graph prerequisites are not ready.
	ErrDependency = errors.New("developer task dependency state is not executable")
	// ErrSourceDrift reports a scope whose source changed after acquisition.
	ErrSourceDrift = errors.New("developer change scope source is no longer current")
)

const maxDeveloperChangeBytes = 64 << 20

// Authority is the only Developer change-scope acquisition and authorization path.
type Authority struct {
	pool     *pgxpool.Pool
	leases   *lease.Store
	graph    *projectbrain.CodeGraph
	brain    *projectbrain.Store
	tenantID string
	now      func() time.Time
}

// NewAuthority constructs a fail-closed Developer authority.
func NewAuthority(
	pool *pgxpool.Pool,
	leases *lease.Store,
	graph *projectbrain.CodeGraph,
	brain *projectbrain.Store,
	tenantID string,
	now func() time.Time,
) (*Authority, error) {
	if pool == nil || leases == nil || graph == nil || brain == nil ||
		strings.TrimSpace(tenantID) == "" || now == nil {
		return nil, fmt.Errorf("developer authority requires database, lease, CodeGraph, Project Brain, tenant, and clock")
	}
	return &Authority{
		pool: pool, leases: leases, graph: graph, brain: brain,
		tenantID: strings.TrimSpace(tenantID), now: now,
	}, nil
}

// Acquire resolves live CodeGraph state, acquires a fenced wake lease, and then
// atomically claims the non-conflicting Developer resource set.
func (authority *Authority) Acquire(
	ctx context.Context,
	request lease.Request,
	scope ScopeRequest,
) (Grant, error) {
	if err := scope.Validate(); err != nil {
		return Grant{}, err
	}
	if request.NodeID != scope.TaskNodeID {
		return Grant{}, fmt.Errorf("developer scope task does not match lease graph node")
	}
	if request.OrganizationID != scope.Capability.OrganizationID ||
		request.SeatID != scope.Capability.RequesterSeatID {
		return Grant{}, fmt.Errorf("developer scope capability does not match lease authority")
	}
	now, err := authority.currentTime()
	if err != nil {
		return Grant{}, err
	}
	if err := authority.brain.AuthorizeCapability(
		ctx, scope.Capability, projectbrain.CapabilityChangeScope,
	); err != nil {
		return Grant{}, err
	}
	canonicalRoot, err := canonicalWorkspaceRoot(scope.WorkspaceRoot)
	if err != nil {
		return Grant{}, err
	}
	if canonicalRoot != scope.WorkspaceRoot {
		return Grant{}, fmt.Errorf("developer scope capability root is not canonical")
	}
	resolved, err := authority.resolve(ctx, scope, now)
	if err != nil {
		return Grant{}, err
	}
	leaseGrant, err := authority.leases.Acquire(ctx, request)
	if err != nil {
		return Grant{}, err
	}
	if err := authority.claim(ctx, leaseGrant, resolved, now); err != nil {
		cancelErr := authority.leases.Cancel(
			context.WithoutCancel(ctx), request.OrganizationID,
			request.ID, leaseGrant.Fence, "developer change-scope acquisition failed",
		)
		return Grant{}, errors.Join(err, cancelErr)
	}
	return Grant{Lease: leaseGrant, Scope: resolved}, nil
}

func (authority *Authority) Bind(
	ctx context.Context,
	leaseGrant lease.Grant,
	scope ScopeRequest,
) (Grant, error) {
	if err := scope.Validate(); err != nil {
		return Grant{}, err
	}
	if leaseGrant.NodeID != scope.TaskNodeID ||
		leaseGrant.OrganizationID != scope.Capability.OrganizationID ||
		leaseGrant.SeatID != scope.Capability.RequesterSeatID {
		return Grant{}, fmt.Errorf(
			"developer scope does not match existing lease authority",
		)
	}
	now, err := authority.currentTime()
	if err != nil {
		return Grant{}, err
	}
	current, err := authority.leases.Authorize(
		ctx, leaseGrant.OrganizationID, leaseGrant.ID, leaseGrant.Fence,
	)
	if err != nil {
		return Grant{}, err
	}
	if current.WakeID != leaseGrant.WakeID ||
		current.SeatID != leaseGrant.SeatID ||
		current.NodeID != leaseGrant.NodeID ||
		current.MandateID != leaseGrant.MandateID ||
		current.MandateVersion != leaseGrant.MandateVersion {
		return Grant{}, lease.ErrStaleFence
	}
	existing, found, err := authority.openBoundScope(
		ctx, current, scope,
	)
	if err != nil {
		return Grant{}, err
	}
	if found {
		grant := Grant{Lease: current, Scope: existing}
		if err := authority.Authorize(ctx, grant); err != nil {
			return Grant{}, err
		}
		return grant, nil
	}
	if err := authority.brain.AuthorizeCapability(
		ctx, scope.Capability, projectbrain.CapabilityChangeScope,
	); err != nil {
		return Grant{}, err
	}
	canonicalRoot, err := canonicalWorkspaceRoot(scope.WorkspaceRoot)
	if err != nil {
		return Grant{}, err
	}
	if canonicalRoot != scope.WorkspaceRoot {
		return Grant{}, fmt.Errorf(
			"developer scope capability root is not canonical",
		)
	}
	resolved, err := authority.resolve(ctx, scope, now)
	if err != nil {
		return Grant{}, err
	}
	if err := authority.claim(ctx, current, resolved, now); err != nil {
		return Grant{}, err
	}
	return Grant{Lease: current, Scope: resolved}, nil
}

func (authority *Authority) openBoundScope(
	ctx context.Context,
	leaseGrant lease.Grant,
	request ScopeRequest,
) (ResolvedScope, bool, error) {
	var payload []byte
	err := authority.pool.QueryRow(ctx, `
		SELECT scope_payload
		FROM workforce_developer_change_scopes
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, authority.tenantID, leaseGrant.OrganizationID, leaseGrant.ID).Scan(
		&payload,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return ResolvedScope{}, false, nil
	}
	if err != nil {
		return ResolvedScope{}, false, err
	}
	existing, err := contracts.DecodeCanonical[
		ResolvedScope, *ResolvedScope,
	](payload)
	if err != nil {
		return ResolvedScope{}, false, err
	}
	if existing.ProjectID != request.ProjectID ||
		existing.WorkspaceID != request.WorkspaceID ||
		existing.TaskNodeID != request.TaskNodeID ||
		existing.WorkspaceRoot != request.WorkspaceRoot ||
		existing.Capability != request.Capability ||
		!sameStrings(graphFilePaths(existing.Files), request.Files) ||
		!sameStrings(existing.Symbols, request.Symbols) {
		return ResolvedScope{}, false, ErrConflict
	}
	return existing, true, nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	left = append([]string(nil), left...)
	right = append([]string(nil), right...)
	sort.Strings(left)
	sort.Strings(right)
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

// Authorize proves both the generic fence and exact Developer source scope are current.
func (authority *Authority) Authorize(
	ctx context.Context,
	grant Grant,
) error {
	now, err := authority.currentTime()
	if err != nil {
		return err
	}
	if _, err := authority.leases.Authorize(
		ctx, grant.Lease.OrganizationID, grant.Lease.ID, grant.Lease.Fence,
	); err != nil {
		return err
	}
	if grant.Scope.Coordination != nil {
		currentPlan, planErr := authority.resolveCoordination(
			ctx,
			ScopeRequest{
				SchemaVersion: grant.Scope.SchemaVersion,
				ProjectID:     grant.Scope.ProjectID, WorkspaceID: grant.Scope.WorkspaceID,
				TaskNodeID:         grant.Scope.TaskNodeID,
				WorkspaceRoot:      grant.Scope.WorkspaceRoot,
				Files:              graphFilePaths(grant.Scope.Files),
				Symbols:            grant.Scope.Symbols,
				CoordinationPlanID: grant.Scope.CoordinationPlanID,
				CoordinationGrant:  grant.Scope.CoordinationGrant,
				Capability:         grant.Scope.Capability,
			},
			grant.Scope.Source,
		)
		if planErr != nil || currentPlan == nil ||
			currentPlan.Digest != grant.Scope.Coordination.Digest {
			return ErrConflict
		}
	}
	if err := grant.Scope.Validate(); err != nil {
		return err
	}
	if grant.Lease.OrganizationID != grant.Scope.Capability.OrganizationID ||
		grant.Lease.SeatID != grant.Scope.Capability.RequesterSeatID ||
		grant.Lease.NodeID != grant.Scope.TaskNodeID {
		return lease.ErrStaleFence
	}
	if err := authority.brain.AuthorizeCapability(
		ctx, grant.Scope.Capability, projectbrain.CapabilityChangeScope,
	); err != nil {
		return err
	}
	expected, err := contracts.HashCanonical(&grant.Scope)
	if err != nil {
		return err
	}
	var stored string
	var payload []byte
	err = authority.pool.QueryRow(ctx, `
			SELECT scope_hash,scope_payload
			FROM workforce_developer_change_scopes scope
		JOIN workforce_runtime_leases runtime
		  ON runtime.tenant_id=scope.tenant_id
		 AND runtime.organization_id=scope.organization_id
		 AND runtime.lease_id=scope.lease_id
		WHERE scope.tenant_id=$1 AND scope.organization_id=$2 AND scope.lease_id=$3
		  AND runtime.state='active' AND runtime.expires_at>$4
			  AND scope.scope_payload IS NOT NULL
		`, authority.tenantID, grant.Lease.OrganizationID, grant.Lease.ID, now).Scan(&stored, &payload)
	if errors.Is(err, pgx.ErrNoRows) {
		return lease.ErrStaleFence
	}
	if err != nil {
		return err
	}
	if stored != expected.Digest {
		return ErrSourceDrift
	}
	canonical, err := contracts.EncodeCanonical(&grant.Scope)
	if err != nil || !strings.EqualFold(stored, expected.Digest) ||
		!bytes.Equal(payload, canonical) {
		return ErrSourceDrift
	}
	for _, file := range grant.Scope.Files {
		current, hashErr := hashScopedFile(grant.Scope.WorkspaceRoot, file.Path)
		if hashErr != nil {
			return ErrSourceDrift
		}
		expected, stateErr := authority.latestFileHash(
			ctx, grant.Lease.OrganizationID, grant.Lease.ID, file,
		)
		if stateErr != nil || current != expected {
			return ErrSourceDrift
		}
	}
	return authority.requireTaskReady(
		ctx, grant.Lease.OrganizationID, grant.Scope.TaskNodeID,
	)
}

// ApplyScopedChanges is the only source-mutation path owned by Developer
// authority. It rechecks the signed capability, live fence, durable exact
// scope, dependency state, and current scoped bytes immediately before
// atomically replacing regular files. Successful and failed attempts append
// immutable scope events.
func (authority *Authority) ApplyScopedChanges(
	ctx context.Context,
	grant Grant,
	operation string,
	changes []SourceChange,
) ([]ChangedFile, error) {
	if operation != "apply_scoped_change" &&
		operation != "restore_source_snapshot" {
		return nil, fmt.Errorf("developer source operation is not allowed")
	}
	if len(changes) == 0 || len(changes) > 64 {
		return nil, fmt.Errorf("developer source operation requires 1 to 64 changes")
	}
	if err := authority.Authorize(ctx, grant); err != nil {
		_ = authority.appendScopeEvent(
			context.WithoutCancel(ctx), grant, "denied", operation,
			contracts.ContentHash{}, "authorization_denied",
		)
		return nil, err
	}
	var total int
	allowed := make(map[string]contracts.ContentHash, len(grant.Scope.Files))
	for _, file := range grant.Scope.Files {
		allowed[file.Path] = file.Hash
	}
	seen := make(map[string]bool, len(changes))
	for _, change := range changes {
		if err := change.Validate(); err != nil {
			return nil, err
		}
		total += len(change.Content)
		original, exists := allowed[change.Path]
		if !exists || seen[change.Path] {
			return nil, fmt.Errorf("developer source change is outside the exact scope")
		}
		current, stateErr := authority.latestFileHash(
			ctx, grant.Lease.OrganizationID, grant.Lease.ID,
			projectbrain.GraphFile{Path: change.Path, Hash: original},
		)
		if stateErr != nil || current != change.BeforeHash {
			return nil, ErrSourceDrift
		}
		if operation == "restore_source_snapshot" &&
			hashBytes(change.Content) != original {
			return nil, fmt.Errorf("developer restore must reproduce the granted source snapshot")
		}
		seen[change.Path] = true
	}
	if total > maxDeveloperChangeBytes {
		return nil, fmt.Errorf("developer source operation exceeds byte budget")
	}
	if err := authority.appendScopeEvent(
		ctx, grant, "effect_started", operation,
		contracts.ContentHash{}, "",
	); err != nil {
		return nil, err
	}
	evidence, err := applySourceFiles(ctx, grant.Scope.WorkspaceRoot, changes)
	if err != nil {
		eventErr := authority.appendScopeEvent(
			context.WithoutCancel(ctx), grant, "effect_failed", operation,
			contracts.ContentHash{}, "source_commit_failed",
		)
		return nil, errors.Join(err, eventErr)
	}
	evidenceHash := hashChangedEvidence(evidence)
	if err := authority.appendCommittedScopeEvent(
		ctx, grant, operation, evidenceHash, evidence,
	); err != nil {
		return nil, err
	}
	return evidence, nil
}

func (authority *Authority) appendCommittedScopeEvent(
	ctx context.Context,
	grant Grant,
	operation string,
	evidenceHash contracts.ContentHash,
	files []ChangedFile,
) error {
	if err := evidenceHash.Validate(); err != nil {
		return err
	}
	now, err := authority.currentTime()
	if err != nil {
		return err
	}
	scopeHash, err := contracts.HashCanonical(&grant.Scope)
	if err != nil {
		return err
	}
	tx, err := authority.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var eventID int64
	if err := tx.QueryRow(ctx, `
		INSERT INTO workforce_developer_scope_events (
			tenant_id,organization_id,lease_id,event_kind,operation,
			scope_hash,evidence_hash,occurred_at
		) VALUES ($1,$2,$3,'effect_committed',$4,$5,$6,$7)
		RETURNING event_id
	`, authority.tenantID, grant.Lease.OrganizationID, grant.Lease.ID,
		operation, scopeHash.Digest, evidenceHash.Digest, now).Scan(&eventID); err != nil {
		return err
	}
	for _, file := range files {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_developer_file_events (
				event_id,tenant_id,organization_id,lease_id,file_path,
				before_hash,after_hash
			) VALUES ($1,$2,$3,$4,$5,$6,$7)
		`, eventID, authority.tenantID, grant.Lease.OrganizationID,
			grant.Lease.ID, file.Path, file.BeforeHash.Digest,
			file.AfterHash.Digest); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (authority *Authority) latestFileHash(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	leaseID contracts.LeaseID,
	file projectbrain.GraphFile,
) (contracts.ContentHash, error) {
	current := file.Hash
	var digest string
	err := authority.pool.QueryRow(ctx, `
		SELECT transition.after_hash
		FROM workforce_developer_file_events transition
		WHERE transition.tenant_id=$1 AND transition.organization_id=$2
		  AND transition.lease_id=$3 AND transition.file_path=$4
		ORDER BY transition.event_id DESC
		LIMIT 1
	`, authority.tenantID, organizationID, leaseID, file.Path).Scan(&digest)
	if errors.Is(err, pgx.ErrNoRows) {
		return current, nil
	}
	if err != nil {
		return contracts.ContentHash{}, err
	}
	current.Digest = digest
	return current, current.Validate()
}

func (authority *Authority) appendScopeEvent(
	ctx context.Context,
	grant Grant,
	eventKind, operation string,
	evidence contracts.ContentHash,
	reason string,
) error {
	now, err := authority.currentTime()
	if err != nil {
		return err
	}
	scopeHash, err := contracts.HashCanonical(&grant.Scope)
	if err != nil {
		return err
	}
	var evidenceDigest any
	if evidence.Digest != "" {
		if err := evidence.Validate(); err != nil {
			return err
		}
		evidenceDigest = evidence.Digest
	}
	var reasonCode any
	if reason != "" {
		reasonCode = reason
	}
	_, err = authority.pool.Exec(ctx, `
		INSERT INTO workforce_developer_scope_events (
			tenant_id,organization_id,lease_id,event_kind,operation,
			scope_hash,evidence_hash,reason_code,occurred_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, authority.tenantID, grant.Lease.OrganizationID, grant.Lease.ID,
		eventKind, operation, scopeHash.Digest, evidenceDigest, reasonCode, now)
	return err
}

func (authority *Authority) resolve(
	ctx context.Context,
	request ScopeRequest,
	now time.Time,
) (ResolvedScope, error) {
	blast := make([]projectbrain.ImpactNode, 0)
	for _, symbol := range request.Symbols {
		impact, err := authority.graph.Impact(ctx, request.WorkspaceRoot, symbol, 5)
		if err != nil {
			return ResolvedScope{}, err
		}
		blast = append(blast, impact.Affected...)
	}
	blast = uniqueImpact(blast)
	affected, err := authority.graph.TestsAffected(
		ctx, request.WorkspaceRoot, request.Files, 5,
	)
	if err != nil {
		return ResolvedScope{}, err
	}
	sourcePaths := append([]string(nil), request.Files...)
	for _, node := range blast {
		sourcePaths = append(sourcePaths, filepath.ToSlash(node.FilePath))
	}
	sourcePaths = append(sourcePaths, affected.AffectedTests...)
	source, err := authority.graph.CaptureFiles(ctx, request.WorkspaceRoot, sourcePaths)
	if err != nil {
		return ResolvedScope{}, err
	}
	if !source.Fresh {
		return ResolvedScope{}, fmt.Errorf(
			"%w: CodeGraph has pending source %v", ErrSourceDrift, source.PendingFiles,
		)
	}
	byPath := make(map[string]projectbrain.GraphFile, len(source.Files))
	for _, file := range source.Files {
		byPath[file.Path] = file
	}
	files := make([]projectbrain.GraphFile, 0, len(request.Files))
	for _, path := range request.Files {
		file, exists := byPath[path]
		if !exists || !file.Indexed {
			return ResolvedScope{}, fmt.Errorf("developer declared file %q is not current in CodeGraph", path)
		}
		files = append(files, file)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	coordination, err := authority.resolveCoordination(ctx, request, source)
	if err != nil {
		return ResolvedScope{}, err
	}
	resolvedAt, err := authority.currentTime()
	if err != nil {
		return ResolvedScope{}, err
	}
	resolved := ResolvedScope{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     request.ProjectID, WorkspaceID: request.WorkspaceID,
		TaskNodeID: request.TaskNodeID, Source: source, Files: files,
		Symbols: sortedUnique(request.Symbols), BlastRadius: blast,
		AffectedTests:      sortedUnique(affected.AffectedTests),
		CoordinationPlanID: request.CoordinationPlanID, ResolvedAt: resolvedAt,
		WorkspaceRoot: request.WorkspaceRoot, Capability: request.Capability,
		Coordination: coordination, CoordinationGrant: request.CoordinationGrant,
	}
	if coordination != nil && !coordinationCoversScope(*coordination, resolved) {
		return ResolvedScope{}, fmt.Errorf(
			"%w: coordination plan does not cover exact scope", ErrConflict,
		)
	}
	if err := resolved.Validate(); err != nil {
		return ResolvedScope{}, err
	}
	return resolved, nil
}

func (authority *Authority) claim(
	ctx context.Context,
	grant lease.Grant,
	scope ResolvedScope,
	now time.Time,
) error {
	tx, err := authority.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	lock := strings.Join([]string{
		authority.tenantID, string(grant.OrganizationID),
		string(scope.ProjectID), string(scope.WorkspaceID),
	}, "|")
	if _, err := tx.Exec(
		ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, lock,
	); err != nil {
		return err
	}
	if err := requireTaskReadyTx(
		ctx, tx, authority.tenantID, grant.OrganizationID, scope.TaskNodeID,
	); err != nil {
		return err
	}
	claims := scopeClaims(scope)
	for _, claim := range claims {
		var conflictingLease string
		var conflictingPayload []byte
		err := tx.QueryRow(ctx, `
				SELECT existing.lease_id,scope.scope_payload
				FROM workforce_developer_change_claims existing
			JOIN workforce_developer_change_scopes scope
			  ON scope.tenant_id=existing.tenant_id
			 AND scope.organization_id=existing.organization_id
			 AND scope.lease_id=existing.lease_id
			JOIN workforce_runtime_leases runtime
			  ON runtime.tenant_id=existing.tenant_id
			 AND runtime.organization_id=existing.organization_id
			 AND runtime.lease_id=existing.lease_id
			WHERE existing.tenant_id=$1 AND existing.organization_id=$2
				  AND existing.resource_key=$3
				  AND (existing.exclusive OR $4)
				  AND runtime.state='active' AND runtime.expires_at>$5
				LIMIT 1
			`, authority.tenantID, grant.OrganizationID, claim.resource,
			claim.exclusive, now).Scan(&conflictingLease, &conflictingPayload)
		if err == nil {
			existing, decodeErr := contracts.DecodeCanonical[
				ResolvedScope, *ResolvedScope,
			](conflictingPayload)
			if decodeErr != nil ||
				!coordinationAllows(claim, scope, existing) {
				return fmt.Errorf("%w: %s", ErrConflict, claim.kind)
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
	}
	scopeHash, err := contracts.HashCanonical(&scope)
	if err != nil {
		return err
	}
	scopePayload, err := contracts.EncodeCanonical(&scope)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
			INSERT INTO workforce_developer_change_scopes (
				tenant_id,organization_id,lease_id,project_id,workspace_id,
				task_node_id,source_root,graph_generation,fresh,
				coordination_plan_id,scope_hash,created_at,scope_payload
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		`, authority.tenantID, grant.OrganizationID, grant.ID, scope.ProjectID,
		scope.WorkspaceID, scope.TaskNodeID, scope.Source.RootDigest.Digest,
		scope.Source.Generation, scope.Source.Fresh, optionalPlan(scope.CoordinationPlanID),
		scopeHash.Digest, now, scopePayload)
	if err != nil {
		return err
	}
	for _, claim := range claims {
		if _, err := tx.Exec(ctx, `
				INSERT INTO workforce_developer_change_claims (
					tenant_id,organization_id,lease_id,claim_kind,claim_hash,exclusive,
					resource_key
				) VALUES ($1,$2,$3,$4,$5,$6,$7)
			`, authority.tenantID, grant.OrganizationID, grant.ID,
			claim.kind, claim.hash, claim.exclusive, claim.resource); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (authority *Authority) requireTaskReady(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	nodeID dependency.NodeID,
) error {
	tx, err := authority.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := requireTaskReadyTx(
		ctx, tx, authority.tenantID, organizationID, nodeID,
	); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func requireTaskReadyTx(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	organizationID contracts.OrganizationID,
	nodeID dependency.NodeID,
) error {
	var state string
	var contested bool
	err := tx.QueryRow(ctx, `
		SELECT state,contested FROM workforce_work_nodes
		WHERE tenant_id=$1 AND organization_id=$2 AND node_id=$3
	`, tenantID, organizationID, string(nodeID)).Scan(&state, &contested)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDependency
	}
	if err != nil {
		return err
	}
	if contested || state != "eligible" && state != "leased" {
		return ErrDependency
	}
	var blockers int
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM workforce_work_edges edge
		JOIN workforce_work_nodes prerequisite
		  ON prerequisite.tenant_id=edge.tenant_id
		 AND prerequisite.organization_id=edge.organization_id
		 AND prerequisite.node_id=edge.prerequisite_node_id
		WHERE edge.tenant_id=$1 AND edge.organization_id=$2
		  AND edge.dependent_node_id=$3 AND prerequisite.state<>'completed'
	`, tenantID, organizationID, string(nodeID)).Scan(&blockers); err != nil {
		return err
	}
	if blockers != 0 {
		return ErrDependency
	}
	return nil
}

func (authority *Authority) resolveCoordination(
	ctx context.Context,
	request ScopeRequest,
	source projectbrain.GraphSnapshot,
) (*CoordinationPlan, error) {
	if request.CoordinationPlanID == nil {
		return nil, nil
	}
	if request.CoordinationGrant == nil {
		return nil, fmt.Errorf("%w: coordination capability is missing", ErrConflict)
	}
	view, err := authority.brain.View(ctx, *request.CoordinationGrant)
	if err != nil {
		return nil, err
	}
	var selected *projectbrain.EngineeringRecord
	for index := range view.Records {
		record := &view.Records[index]
		if record.Proposal.ID == *request.CoordinationPlanID {
			selected = record
			break
		}
	}
	if selected == nil || selected.Proposal.Kind != projectbrain.KindPlan ||
		selected.Proposal.Source.Generation != source.Generation {
		return nil, fmt.Errorf(
			"%w: coordination plan is not current verified truth", ErrConflict,
		)
	}
	digest, err := contracts.HashCanonical(selected)
	if err != nil {
		return nil, err
	}
	plan := &CoordinationPlan{
		RecordID: selected.Proposal.ID, Digest: digest,
		ExpiresAt: selected.Proposal.Content.ExpiresAt,
	}
	taskPrefix := "coordinate_task:"
	seatPrefix := "coordinate_seat:"
	for _, claim := range selected.Proposal.Content.Claims {
		statement := strings.TrimSpace(claim.Statement)
		switch {
		case strings.HasPrefix(statement, taskPrefix):
			plan.Tasks = append(
				plan.Tasks,
				dependency.NodeID(strings.TrimPrefix(statement, taskPrefix)),
			)
		case strings.HasPrefix(statement, seatPrefix):
			plan.Seats = append(
				plan.Seats,
				contracts.SeatID(strings.TrimPrefix(statement, seatPrefix)),
			)
		}
		for _, file := range claim.Files {
			plan.Files = append(plan.Files, filepath.ToSlash(file.Path))
		}
	}
	plan.Tasks = uniqueNodeIDs(plan.Tasks)
	plan.Seats = uniqueSeatIDs(plan.Seats)
	plan.Files = sortedUnique(plan.Files)
	if err := plan.Validate(); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	if !coordinationSourceCurrent(*selected, source) {
		return nil, fmt.Errorf(
			"%w: coordination plan source is no longer current", ErrConflict,
		)
	}
	if !containsNode(plan.Tasks, request.TaskNodeID) ||
		!containsSeat(plan.Seats, request.Capability.RequesterSeatID) {
		return nil, fmt.Errorf(
			"%w: coordination plan does not authorize task and seat", ErrConflict,
		)
	}
	return plan, nil
}

func coordinationSourceCurrent(
	record projectbrain.EngineeringRecord,
	source projectbrain.GraphSnapshot,
) bool {
	claimed := make(map[string]contracts.ContentHash)
	for _, claim := range record.Proposal.Content.Claims {
		for _, file := range claim.Files {
			path := filepath.ToSlash(file.Path)
			if existing, exists := claimed[path]; exists && existing != file.Hash {
				return false
			}
			claimed[path] = file.Hash
		}
	}
	for _, file := range source.Files {
		if hash, exists := claimed[file.Path]; !exists || hash != file.Hash {
			return false
		}
	}
	return true
}

func coordinationCoversScope(plan CoordinationPlan, scope ResolvedScope) bool {
	files := make(map[string]bool, len(plan.Files))
	for _, path := range plan.Files {
		files[path] = true
	}
	for _, file := range scope.Files {
		if !files[file.Path] {
			return false
		}
	}
	for _, node := range scope.BlastRadius {
		if !files[filepath.ToSlash(node.FilePath)] {
			return false
		}
	}
	for _, test := range scope.AffectedTests {
		if !files[filepath.ToSlash(test)] {
			return false
		}
	}
	return true
}

func coordinationAllows(
	resource scopeClaim,
	current ResolvedScope,
	existing ResolvedScope,
) bool {
	if resource.kind != "file" && resource.kind != "symbol" ||
		current.Coordination == nil || existing.Coordination == nil ||
		current.Coordination.RecordID != existing.Coordination.RecordID ||
		current.Coordination.Digest != existing.Coordination.Digest {
		return false
	}
	plan := *current.Coordination
	return containsNode(plan.Tasks, current.TaskNodeID) &&
		containsNode(plan.Tasks, existing.TaskNodeID) &&
		containsSeat(plan.Seats, current.Capability.RequesterSeatID) &&
		containsSeat(plan.Seats, existing.Capability.RequesterSeatID)
}

func graphFilePaths(files []projectbrain.GraphFile) []string {
	result := make([]string, len(files))
	for index, file := range files {
		result[index] = file.Path
	}
	return result
}

func uniqueNodeIDs(values []dependency.NodeID) []dependency.NodeID {
	seen := make(map[dependency.NodeID]bool, len(values))
	result := make([]dependency.NodeID, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

func uniqueSeatIDs(values []contracts.SeatID) []contracts.SeatID {
	seen := make(map[contracts.SeatID]bool, len(values))
	result := make([]contracts.SeatID, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

func containsNode(values []dependency.NodeID, target dependency.NodeID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsSeat(values []contracts.SeatID, target contracts.SeatID) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

type scopeClaim struct {
	kind      string
	hash      string
	resource  string
	exclusive bool
}

func scopeClaims(scope ResolvedScope) []scopeClaim {
	project := string(scope.ProjectID)
	workspace := string(scope.WorkspaceID)
	values := []scopeClaim{
		claim("project", resourceKey("project", project), false),
		claim("workspace", resourceKey("workspace", project, workspace), false),
		claim("task", resourceKey("task", project, workspace, string(scope.TaskNodeID)), true),
	}
	for _, file := range scope.Files {
		values = append(values, claim("file", resourceKey("file", project, workspace, file.Path), true))
	}
	for _, symbol := range scope.Symbols {
		values = append(values, claim("symbol", resourceKey("symbol", project, workspace, symbol), true))
	}
	for _, node := range scope.BlastRadius {
		values = append(values, claim(
			"file", resourceKey("file", project, workspace, filepath.ToSlash(node.FilePath)), true,
		))
		values = append(values, claim(
			"symbol", resourceKey("symbol", project, workspace, node.Name),
			true,
		))
	}
	for _, test := range scope.AffectedTests {
		values = append(values, claim(
			"file", resourceKey("file", project, workspace, filepath.ToSlash(test)), true,
		))
	}
	values = uniqueClaims(values)
	sort.Slice(values, func(left, right int) bool {
		if values[left].kind != values[right].kind {
			return values[left].kind < values[right].kind
		}
		return values[left].hash < values[right].hash
	})
	return values
}

func resourceKey(kind string, values ...string) string {
	var value strings.Builder
	value.WriteString(strconv.Itoa(len(kind)))
	value.WriteByte(':')
	value.WriteString(kind)
	for _, part := range values {
		value.WriteString(strconv.Itoa(len(part)))
		value.WriteByte(':')
		value.WriteString(part)
	}
	return value.String()
}

func claim(kind, value string, exclusive bool) scopeClaim {
	sum := sha256.Sum256([]byte(value))
	return scopeClaim{
		kind: kind, hash: hex.EncodeToString(sum[:]), resource: value,
		exclusive: exclusive,
	}
}

func uniqueClaims(values []scopeClaim) []scopeClaim {
	byResource := make(map[string]scopeClaim, len(values))
	for _, value := range values {
		existing, found := byResource[value.resource]
		if !found || value.exclusive && !existing.exclusive {
			byResource[value.resource] = value
		}
	}
	result := make([]scopeClaim, 0, len(byResource))
	for _, value := range byResource {
		result = append(result, value)
	}
	return result
}

func uniqueImpact(values []projectbrain.ImpactNode) []projectbrain.ImpactNode {
	seen := make(map[string]projectbrain.ImpactNode, len(values))
	for _, value := range values {
		key := strings.Join([]string{
			value.Name, value.Kind, value.FilePath, fmt.Sprintf("%d", value.StartLine),
		}, "\x00")
		seen[key] = value
	}
	result := make([]projectbrain.ImpactNode, 0, len(seen))
	for _, value := range seen {
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].FilePath != result[right].FilePath {
			return result[left].FilePath < result[right].FilePath
		}
		if result[left].StartLine != result[right].StartLine {
			return result[left].StartLine < result[right].StartLine
		}
		return result[left].Name < result[right].Name
	})
	return result
}

func canonicalWorkspaceRoot(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", fmt.Errorf("developer workspace root must be absolute")
	}
	canonical, err := filepath.EvalSymlinks(filepath.Clean(root))
	if err != nil {
		return "", fmt.Errorf("developer resolve workspace root: %w", err)
	}
	info, err := os.Stat(canonical)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("developer workspace root is not a directory")
	}
	return canonical, nil
}

func hashScopedFile(root, relative string) (contracts.ContentHash, error) {
	if err := relativePath(relative); err != nil {
		return contracts.ContentHash{}, err
	}
	canonicalRoot, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	path := filepath.Join(canonicalRoot, filepath.FromSlash(relative))
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	within, err := filepath.Rel(canonicalRoot, resolved)
	if err != nil || within == ".." ||
		strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return contracts.ContentHash{}, fmt.Errorf("developer scoped file escapes workspace")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() {
		return contracts.ContentHash{}, fmt.Errorf("developer scoped source is not a regular file")
	}
	if info.Size() > 32<<20 {
		return contracts.ContentHash{}, fmt.Errorf("developer scoped source exceeds size limit")
	}
	file, err := os.Open(resolved)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	defer file.Close()
	sum := sha256.New()
	if _, err := io.Copy(sum, io.LimitReader(file, (32<<20)+1)); err != nil {
		return contracts.ContentHash{}, err
	}
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum.Sum(nil)),
	}, nil
}

type preparedSourceChange struct {
	change   SourceChange
	path     string
	tempPath string
	before   []byte
	mode     os.FileMode
	after    contracts.ContentHash
}

func applySourceFiles(
	ctx context.Context,
	root string,
	changes []SourceChange,
) ([]ChangedFile, error) {
	canonicalRoot, err := canonicalWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	prepared := make([]preparedSourceChange, 0, len(changes))
	for _, change := range changes {
		select {
		case <-ctx.Done():
			cleanupPrepared(prepared)
			return nil, ctx.Err()
		default:
		}
		path, info, err := scopedRegularPath(canonicalRoot, change.Path)
		if err != nil {
			cleanupPrepared(prepared)
			return nil, err
		}
		current, err := hashScopedFile(canonicalRoot, change.Path)
		if err != nil || current != change.BeforeHash {
			cleanupPrepared(prepared)
			return nil, ErrSourceDrift
		}
		before, err := os.ReadFile(path)
		if err != nil {
			cleanupPrepared(prepared)
			return nil, err
		}
		after := hashBytes(change.Content)
		if after == change.BeforeHash {
			cleanupPrepared(prepared)
			return nil, fmt.Errorf("developer source change does not change bytes")
		}
		temp, err := os.CreateTemp(filepath.Dir(path), ".workforce-change-*")
		if err != nil {
			cleanupPrepared(prepared)
			return nil, err
		}
		tempPath := temp.Name()
		err = temp.Chmod(info.Mode().Perm())
		if err == nil {
			_, err = temp.Write(change.Content)
		}
		if err == nil {
			err = temp.Sync()
		}
		closeErr := temp.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			_ = os.Remove(tempPath)
			cleanupPrepared(prepared)
			return nil, err
		}
		prepared = append(prepared, preparedSourceChange{
			change: change, path: path, tempPath: tempPath,
			before: before, mode: info.Mode().Perm(), after: after,
		})
	}
	committed := 0
	for index := range prepared {
		select {
		case <-ctx.Done():
			rollbackErr := rollbackSourceFiles(prepared[:committed])
			cleanupPrepared(prepared[index:])
			return nil, errors.Join(ctx.Err(), rollbackErr)
		default:
		}
		current, err := hashScopedFile(canonicalRoot, prepared[index].change.Path)
		if err != nil || current != prepared[index].change.BeforeHash {
			rollbackErr := rollbackSourceFiles(prepared[:committed])
			cleanupPrepared(prepared[index:])
			return nil, errors.Join(ErrSourceDrift, rollbackErr)
		}
		if err := os.Rename(prepared[index].tempPath, prepared[index].path); err != nil {
			rollbackErr := rollbackSourceFiles(prepared[:committed])
			cleanupPrepared(prepared[index:])
			return nil, errors.Join(err, rollbackErr)
		}
		prepared[index].tempPath = ""
		if err := syncDirectory(filepath.Dir(prepared[index].path)); err != nil {
			rollbackErr := rollbackSourceFiles(prepared[:index+1])
			cleanupPrepared(prepared[index+1:])
			return nil, errors.Join(err, rollbackErr)
		}
		committed++
	}
	evidence := make([]ChangedFile, 0, len(prepared))
	for _, item := range prepared {
		current, err := hashScopedFile(canonicalRoot, item.change.Path)
		if err != nil || current != item.after {
			return nil, errors.Join(ErrSourceDrift, err)
		}
		evidence = append(evidence, ChangedFile{
			Path: item.change.Path, BeforeHash: item.change.BeforeHash,
			AfterHash: item.after,
		})
	}
	return evidence, nil
}

func scopedRegularPath(root, relative string) (string, os.FileInfo, error) {
	if err := relativePath(relative); err != nil {
		return "", nil, err
	}
	path := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Lstat(path)
	if err != nil {
		return "", nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return "", nil, fmt.Errorf("developer scoped source is not a regular file")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", nil, err
	}
	within, err := filepath.Rel(root, resolved)
	if err != nil || within == ".." ||
		strings.HasPrefix(within, ".."+string(filepath.Separator)) {
		return "", nil, fmt.Errorf("developer scoped source escapes workspace")
	}
	return resolved, info, nil
}

func rollbackSourceFiles(committed []preparedSourceChange) error {
	var result error
	for index := len(committed) - 1; index >= 0; index-- {
		item := committed[index]
		temp, err := os.CreateTemp(filepath.Dir(item.path), ".workforce-rollback-*")
		if err == nil {
			if chmodErr := temp.Chmod(item.mode); chmodErr != nil {
				err = chmodErr
			}
		}
		if err == nil {
			_, err = temp.Write(item.before)
		}
		if err == nil {
			err = temp.Sync()
		}
		if temp != nil {
			err = errors.Join(err, temp.Close())
		}
		if err == nil {
			err = os.Rename(temp.Name(), item.path)
		}
		if temp != nil && err != nil {
			_ = os.Remove(temp.Name())
		}
		result = errors.Join(result, err)
	}
	return result
}

func cleanupPrepared(prepared []preparedSourceChange) {
	for _, item := range prepared {
		if item.tempPath != "" {
			_ = os.Remove(item.tempPath)
		}
	}
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	return errors.Join(syncErr, closeErr)
}

func hashBytes(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}

func hashChangedEvidence(values []ChangedFile) contracts.ContentHash {
	hasher := sha256.New()
	for _, value := range values {
		_, _ = io.WriteString(
			hasher,
			resourceKey(
				"changed", value.Path, value.BeforeHash.Digest, value.AfterHash.Digest,
			),
		)
	}
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(hasher.Sum(nil)),
	}
}

func optionalPlan(value *projectbrain.RecordID) any {
	if value == nil {
		return nil
	}
	return string(*value)
}

func (authority *Authority) currentTime() (time.Time, error) {
	now := authority.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("developer authority clock must return UTC")
	}
	return now, nil
}
