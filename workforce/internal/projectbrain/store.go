package projectbrain

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

// Store owns immutable, Vault-sealed Project Brain records for one tenant.
type Store struct {
	pool           *pgxpool.Pool
	vault          *vault.UserVault
	tenantID       string
	authorityKeyID string
	authorityKey   ed25519.PublicKey
	seatAuthority  SeatAuthority
	graph          *CodeGraph
	now            func() time.Time
}

type SeatAuthority interface {
	LoadCurrentSeat(context.Context, contracts.SeatID) (contracts.Seat, error)
}

// New constructs a fail-closed tenant-scoped Project Brain store.
func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	authorityKeyID string,
	authorityKey ed25519.PublicKey,
	seatAuthority SeatAuthority,
	graph *CodeGraph,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	authorityKeyID = strings.TrimSpace(authorityKeyID)
	switch {
	case pool == nil:
		return nil, fmt.Errorf("project brain PostgreSQL pool is required")
	case userVault == nil:
		return nil, fmt.Errorf("project brain encrypting Vault is required")
	case tenantID == "":
		return nil, fmt.Errorf("project brain tenant_id is required")
	case userVault.User() != tenantID:
		return nil, fmt.Errorf("project brain Vault user does not match tenant")
	case authorityKeyID == "" || len(authorityKey) != ed25519.PublicKeySize:
		return nil, fmt.Errorf("project brain kernel authority is required")
	case seatAuthority == nil:
		return nil, fmt.Errorf("project brain current seat authority is required")
	case graph == nil:
		return nil, fmt.Errorf("project brain CodeGraph adapter is required")
	case now == nil:
		return nil, fmt.Errorf("project brain time source is required")
	default:
		return &Store{
			pool: pool, vault: userVault, tenantID: tenantID,
			authorityKeyID: authorityKeyID,
			authorityKey:   append(ed25519.PublicKey(nil), authorityKey...),
			seatAuthority:  seatAuthority,
			graph:          graph, now: now,
		}, nil
	}
}

// Commit verifies both independent signatures and atomically appends immutable truth.
func (store *Store) Commit(
	ctx context.Context,
	record EngineeringRecord,
	grant CapabilityGrant,
) (bool, error) {
	now, err := store.currentTime()
	if err != nil {
		return false, err
	}
	if err := record.Validate(); err != nil {
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, "invalid_record", now, err,
		)
	}
	if err := store.verifyGrant(ctx, grant, CapabilityWrite, now); err != nil {
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, denialReason(err), now, err,
		)
	}
	if grant.RecordID == nil || *grant.RecordID != record.Proposal.ID ||
		grant.OrganizationID != record.Proposal.OrganizationID ||
		grant.ProjectID != record.Proposal.ProjectID ||
		grant.WorkspaceID != record.Proposal.WorkspaceID ||
		grant.Author == nil || grant.Verifier == nil ||
		grant.Author.SeatID != record.Proposal.AuthorSeatID ||
		grant.Verifier.SeatID != record.Verification.VerifierSeatID ||
		grant.Author.KeyID != record.Proposal.Signature.KeyID ||
		grant.Verifier.KeyID != record.Verification.Signature.KeyID {
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, "scope_mismatch", now, ErrUnauthorized,
		)
	}
	authorKey, err := grant.Author.publicKey()
	if err != nil {
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, "invalid_author_key", now, err,
		)
	}
	verifierKey, err := grant.Verifier.publicKey()
	if err != nil {
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, "invalid_verifier_key", now, err,
		)
	}
	if err := verifyRecordSignatures(record, authorKey, verifierKey); err != nil {
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, "invalid_signature", now, err,
		)
	}
	currentSource, err := store.graph.Capture(ctx, grant.WorkspaceRoot, grant.Filter)
	if err != nil {
		return false, err
	}
	sameSource, err := equalGraphSnapshot(record.Proposal.Source, currentSource)
	if err != nil {
		return false, err
	}
	if !sameSource {
		err := fmt.Errorf("%w: project brain proposal source is not current", ErrIntegrity)
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, "source_mismatch", now, err,
		)
	}
	if record.Verification.VerifiedAt.After(now) || record.Proposal.CreatedAt.After(record.Verification.VerifiedAt) {
		err := fmt.Errorf("%w: project brain record chronology is invalid", ErrIntegrity)
		return false, store.auditedError(
			ctx, grant, CapabilityWrite, "invalid_chronology", now, err,
		)
	}
	canonical, err := contracts.EncodeCanonical(&record)
	if err != nil {
		return false, fmt.Errorf("project brain canonical record: %w", err)
	}
	canonicalHash := digestBytes(canonical)
	sealed, err := store.vault.SealRecord(store.recordAD(record.Proposal), canonical)
	if err != nil {
		return false, fmt.Errorf("project brain seal record: %w", err)
	}
	// The scope advisory lock below serializes concurrent commits. Read
	// committed is required for that to work: under a fixed snapshot the
	// duplicate check after the lock would still read the state from before the
	// winning writer committed, and an identical concurrent commit would raise a
	// duplicate key instead of deduplicating.
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return false, fmt.Errorf("project brain begin commit: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	proposal := record.Proposal
	lock := strings.Join([]string{
		store.tenantID, string(proposal.OrganizationID), string(proposal.ProjectID),
		string(proposal.WorkspaceID),
	}, "|")
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lock); err != nil {
		return false, fmt.Errorf("project brain lock scope: %w", err)
	}
	existingHash, found, err := existingRecordHash(ctx, tx, store.tenantID, proposal)
	if err != nil {
		return false, err
	}
	if found {
		if existingHash != canonicalHash.Digest {
			return false, ErrConflict
		}
		if err := tx.Commit(ctx); err != nil {
			return false, fmt.Errorf("project brain commit deduplication: %w", err)
		}
		return true, nil
	}
	if err := validateChain(ctx, tx, store.tenantID, proposal); err != nil {
		return false, err
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO workforce_project_brain_records (
			tenant_id, organization_id, project_id, workspace_id, record_id,
			kind, version, author_seat_id, verifier_seat_id, source_root,
			graph_generation, fresh, supersedes, corrects, canonical_hash,
			sealed_record, created_at, verified_at
		) VALUES (
			$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18
		)
	`, store.tenantID, proposal.OrganizationID, proposal.ProjectID, proposal.WorkspaceID,
		proposal.ID, proposal.Kind, proposal.Version, proposal.AuthorSeatID,
		record.Verification.VerifierSeatID, proposal.Source.RootDigest.Digest,
		proposal.Source.Generation, proposal.Source.Fresh, optionalRecordID(proposal.Supersedes),
		optionalRecordID(proposal.Corrects), canonicalHash.Digest, sealed,
		proposal.CreatedAt, record.Verification.VerifiedAt)
	if err != nil {
		return false, fmt.Errorf("project brain insert record: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, fmt.Errorf("project brain commit record: %w", err)
	}
	return false, nil
}

// View opens current verified records only for the exact expiring project capability.
func (store *Store) View(
	ctx context.Context,
	grant CapabilityGrant,
) (View, error) {
	now, err := store.currentTime()
	if err != nil {
		return View{}, err
	}
	if err := store.verifyGrant(ctx, grant, CapabilityRead, now); err != nil {
		if auditErr := store.auditDenial(
			ctx, grant, CapabilityRead, denialReason(err), now,
		); auditErr != nil {
			return View{}, fmt.Errorf("%w: denial audit failed: %v", err, auditErr)
		}
		return View{}, err
	}
	source, err := store.graph.Capture(ctx, grant.WorkspaceRoot, grant.Filter)
	if err != nil {
		return View{}, err
	}
	after := ""
	if grant.AfterRecordID != nil {
		after = string(*grant.AfterRecordID)
	}
	rows, err := store.pool.Query(ctx, `
		SELECT record_id, kind, canonical_hash, sealed_record
		FROM workforce_project_brain_records current_record
		WHERE tenant_id=$1 AND organization_id=$2 AND project_id=$3 AND workspace_id=$4
		  AND record_id > $5
		  AND NOT EXISTS (
			SELECT 1 FROM workforce_project_brain_records replacement
			WHERE replacement.tenant_id=current_record.tenant_id
			  AND replacement.organization_id=current_record.organization_id
			  AND replacement.project_id=current_record.project_id
			  AND replacement.workspace_id=current_record.workspace_id
			  AND (
				replacement.supersedes=current_record.record_id OR
				replacement.corrects=current_record.record_id
			  )
		  )
		ORDER BY record_id
		LIMIT $6
	`, store.tenantID, grant.OrganizationID, grant.ProjectID, grant.WorkspaceID,
		after, grant.MaxRecords+1)
	if err != nil {
		return View{}, fmt.Errorf("project brain query view: %w", err)
	}
	defer rows.Close()
	records := make([]EngineeringRecord, 0, grant.MaxRecords)
	staleRecordIDs := make([]RecordID, 0)
	var nextCursor *RecordID
	var scanned uint32
	var lastScanned RecordID
	for rows.Next() {
		var recordID, kind, canonicalHash string
		var sealed []byte
		if err := rows.Scan(&recordID, &kind, &canonicalHash, &sealed); err != nil {
			return View{}, fmt.Errorf("project brain scan view: %w", err)
		}
		if scanned == grant.MaxRecords {
			cursor := lastScanned
			nextCursor = &cursor
			break
		}
		scanned++
		lastScanned = RecordID(recordID)
		proposal := Proposal{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            RecordID(recordID), OrganizationID: grant.OrganizationID,
			ProjectID: grant.ProjectID, WorkspaceID: grant.WorkspaceID,
			Kind: Kind(kind),
		}
		canonical, err := store.vault.OpenRecord(store.recordAD(proposal), sealed)
		if err != nil {
			cause := fmt.Errorf("%w: Vault authentication", ErrIntegrity)
			return View{}, store.auditedError(
				ctx, grant, CapabilityRead, "vault_authentication", now, cause,
			)
		}
		if digestBytes(canonical).Digest != canonicalHash {
			cause := fmt.Errorf("%w: canonical hash mismatch", ErrIntegrity)
			return View{}, store.auditedError(
				ctx, grant, CapabilityRead, "canonical_hash_mismatch", now, cause,
			)
		}
		record, err := contracts.DecodeCanonical[EngineeringRecord, *EngineeringRecord](canonical)
		if err != nil {
			cause := fmt.Errorf("%w: canonical decode", ErrIntegrity)
			return View{}, store.auditedError(
				ctx, grant, CapabilityRead, "canonical_decode", now, cause,
			)
		}
		if record.Proposal.OrganizationID != grant.OrganizationID ||
			record.Proposal.ProjectID != grant.ProjectID ||
			record.Proposal.WorkspaceID != grant.WorkspaceID {
			cause := fmt.Errorf("%w: sealed scope mismatch", ErrIntegrity)
			return View{}, store.auditedError(
				ctx, grant, CapabilityRead, "sealed_scope_mismatch", now, cause,
			)
		}
		if record.ContentExpired(now) {
			continue
		}
		if !recordMatchesSource(record, source) {
			staleRecordIDs = append(staleRecordIDs, record.Proposal.ID)
			continue
		}
		records = append(records, record)
	}
	if err := rows.Err(); err != nil {
		return View{}, fmt.Errorf("project brain iterate view: %w", err)
	}
	view := View{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: grant.OrganizationID, ProjectID: grant.ProjectID,
		WorkspaceID: grant.WorkspaceID, Source: source, Records: records,
		StaleRecordIDs: staleRecordIDs, NextCursor: nextCursor,
		Digest: digestBytes(nil), ExpiresAt: grant.ExpiresAt,
	}
	payload := viewDigestPayload{View: view}
	encoded, err := contracts.EncodeCanonical(&payload)
	if err != nil {
		return View{}, fmt.Errorf("project brain encode view: %w", err)
	}
	view.Digest = digestBytes(encoded)
	if err := view.Validate(); err != nil {
		return View{}, err
	}
	return view, nil
}

// AuthorizeCapability verifies one kernel-signed capability against this
// store's tenant, authority key, operation, and current UTC time. Callers use
// this before a separate owned boundary, such as Developer source authority,
// relies on the grant's project/workspace/root binding.
func (store *Store) AuthorizeCapability(
	ctx context.Context,
	grant CapabilityGrant,
	operation CapabilityOperation,
) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if err := store.verifyGrant(ctx, grant, operation, now); err != nil {
		return store.auditedError(
			ctx, grant, operation, denialReason(err), now, err,
		)
	}
	return nil
}

// ContentExpired reports whether this record is outside its evidence validity window.
func (record EngineeringRecord) ContentExpired(now time.Time) bool {
	return record.Proposal.Content.ExpiresAt != nil &&
		!record.Proposal.Content.ExpiresAt.After(now)
}

func existingRecordHash(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	proposal Proposal,
) (string, bool, error) {
	var hash string
	err := tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_project_brain_records
		WHERE tenant_id=$1 AND organization_id=$2 AND project_id=$3
		  AND workspace_id=$4 AND record_id=$5
	`, tenantID, proposal.OrganizationID, proposal.ProjectID,
		proposal.WorkspaceID, proposal.ID).Scan(&hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("project brain inspect record identity: %w", err)
	}
	return hash, true, nil
}

func validateChain(
	ctx context.Context,
	tx pgx.Tx,
	tenantID string,
	proposal Proposal,
) error {
	parent := proposal.Supersedes
	if parent == nil {
		parent = proposal.Corrects
	}
	if parent == nil {
		if proposal.Version != 1 {
			return fmt.Errorf("project brain initial record version must be 1")
		}
		return nil
	}
	var version uint64
	var kind string
	err := tx.QueryRow(ctx, `
		SELECT version, kind FROM workforce_project_brain_records
		WHERE tenant_id=$1 AND organization_id=$2 AND project_id=$3
		  AND workspace_id=$4 AND record_id=$5
	`, tenantID, proposal.OrganizationID, proposal.ProjectID,
		proposal.WorkspaceID, *parent).Scan(&version, &kind)
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("project brain inspect supersession parent: %w", err)
	}
	if proposal.Version != version+1 {
		return fmt.Errorf("project brain version does not follow superseded record")
	}
	if proposal.Supersedes != nil && string(proposal.Kind) != kind {
		return fmt.Errorf("project brain supersession cannot change record kind")
	}
	var successor string
	err = tx.QueryRow(ctx, `
		SELECT record_id FROM workforce_project_brain_records
		WHERE tenant_id=$1 AND organization_id=$2 AND project_id=$3
		  AND workspace_id=$4 AND replacement_parent=$5
	`, tenantID, proposal.OrganizationID, proposal.ProjectID,
		proposal.WorkspaceID, *parent).Scan(&successor)
	if err == nil {
		return fmt.Errorf("%w: project brain record already has successor %q", ErrConflict, successor)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("project brain inspect supersession successor: %w", err)
	}
	return nil
}

func (store *Store) verifyGrant(
	ctx context.Context,
	grant CapabilityGrant,
	operation CapabilityOperation,
	now time.Time,
) error {
	if err := grant.Validate(); err != nil {
		return ErrUnauthorized
	}
	if grant.TenantID != store.tenantID ||
		grant.Operation != operation || grant.Signature.KeyID != store.authorityKeyID ||
		grant.IssuedAt.After(now) || !grant.ExpiresAt.After(now) {
		return ErrUnauthorized
	}
	requester, err := store.seatAuthority.LoadCurrentSeat(
		ctx, grant.RequesterSeatID,
	)
	if err != nil || !grantMatchesSeat(grant, requester) {
		return ErrUnauthorized
	}
	if grant.Author != nil {
		author, loadErr := store.seatAuthority.LoadCurrentSeat(
			ctx, grant.Author.SeatID,
		)
		if loadErr != nil || !bindingMatchesSeat(*grant.Author, author) {
			return ErrUnauthorized
		}
	}
	if grant.Verifier != nil {
		verifier, loadErr := store.seatAuthority.LoadCurrentSeat(
			ctx, grant.Verifier.SeatID,
		)
		if loadErr != nil || !bindingMatchesSeat(*grant.Verifier, verifier) {
			return ErrUnauthorized
		}
	}
	payload, err := grantSigningBytes(grant)
	if err != nil {
		return ErrUnauthorized
	}
	signature, err := base64.RawURLEncoding.DecodeString(grant.Signature.Value)
	if err != nil || !ed25519.Verify(store.authorityKey, payload, signature) {
		return ErrUnauthorized
	}
	return nil
}

func grantMatchesSeat(grant CapabilityGrant, seat contracts.Seat) bool {
	return seat.ID == grant.RequesterSeatID &&
		seat.Version == grant.RequesterSeatVersion &&
		seat.DID == grant.RequesterSeatDID &&
		seat.BindingID == grant.RequesterBindingID &&
		seat.BindingVersion == grant.RequesterBindingVersion &&
		seat.OrganizationID == grant.OrganizationID
}

func bindingMatchesSeat(binding SeatKeyBinding, seat contracts.Seat) bool {
	return seat.ID == binding.SeatID &&
		seat.Version == binding.SeatVersion &&
		seat.DID == binding.SeatDID &&
		seat.BindingID == binding.BindingID &&
		seat.BindingVersion == binding.BindingVersion
}

func (store *Store) auditDenial(
	ctx context.Context,
	grant CapabilityGrant,
	operation CapabilityOperation,
	reason string,
	now time.Time,
) error {
	value := func(candidate string) string {
		if validateToken("audit", candidate) == nil {
			return candidate
		}
		return "invalid"
	}
	_, err := store.pool.Exec(ctx, `
		INSERT INTO workforce_project_brain_access_denials (
			tenant_id, grant_id, organization_id, project_id, workspace_id,
			seat_id, operation, reason_code, denied_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
	`, store.tenantID, value(grant.ID), value(string(grant.OrganizationID)),
		value(string(grant.ProjectID)), value(string(grant.WorkspaceID)),
		value(string(grant.RequesterSeatID)), value(string(operation)), reason, now)
	if err != nil {
		return fmt.Errorf("project brain audit security event: %w", err)
	}
	return nil
}

func (store *Store) auditedError(
	ctx context.Context,
	grant CapabilityGrant,
	operation CapabilityOperation,
	reason string,
	now time.Time,
	cause error,
) error {
	if auditErr := store.auditDenial(ctx, grant, operation, reason, now); auditErr != nil {
		return fmt.Errorf("%w: security audit failed: %v", cause, auditErr)
	}
	return cause
}

func denialReason(err error) string {
	if errors.Is(err, ErrUnauthorized) {
		return "unauthorized"
	}
	return "invalid"
}

func equalGraphSnapshot(left, right GraphSnapshot) (bool, error) {
	left.CapturedAt = right.CapturedAt
	leftHash, err := contracts.HashCanonical(&left)
	if err != nil {
		return false, err
	}
	rightHash, err := contracts.HashCanonical(&right)
	if err != nil {
		return false, err
	}
	return leftHash == rightHash, nil
}

func recordMatchesSource(record EngineeringRecord, source GraphSnapshot) bool {
	current := make(map[string]contracts.ContentHash, len(source.Files))
	for _, file := range source.Files {
		current[file.Path] = file.Hash
	}
	for _, claim := range record.Proposal.Content.Claims {
		for _, file := range claim.Files {
			hash, found := current[file.Path]
			if !found || hash != file.Hash {
				return false
			}
		}
	}
	return true
}

type viewDigestPayload struct {
	View View `json:"view"`
}

func (payload viewDigestPayload) Validate() error {
	return payload.View.Validate()
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("project brain time source returned a non-UTC timestamp")
	}
	return now, nil
}

func (store *Store) recordAD(proposal Proposal) vault.AD {
	return vault.AD{
		User:  store.tenantID,
		Store: "workforce.projectbrain." + string(proposal.Kind),
		Stream: strings.Join([]string{
			string(proposal.OrganizationID), string(proposal.ProjectID),
			string(proposal.WorkspaceID), string(proposal.ID),
		}, "/"),
		Schema: contracts.SchemaVersionV1,
	}
}

func optionalRecordID(value *RecordID) any {
	if value == nil {
		return nil
	}
	return string(*value)
}
