package external

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
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
	"matrix/workforce/internal/lease"
)

type Store struct {
	pool       *pgxpool.Pool
	vault      *vault.UserVault
	tenantID   string
	founderKey string
	publicKey  ed25519.PublicKey
	now        func() time.Time
}

type loadedConnection struct {
	connection Connection
	credential CredentialMaterial
	hash       contracts.ContentHash
}

type ConnectionView struct {
	Connection Connection            `json:"connection"`
	Hash       contracts.ContentHash `json:"hash"`
}

type attemptKind string

const (
	attemptDispatch   attemptKind = "dispatch"
	attemptProbe      attemptKind = "probe"
	attemptCompensate attemptKind = "compensate"
)

type attemptClaim struct {
	id             string
	organizationID contracts.OrganizationID
	connectionID   string
	version        uint64
	operation      string
	idempotencyKey string
	kind           attemptKind
	limits         ResourceLimits
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	founderKeyID string,
	founderPublicKey ed25519.PublicKey,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" ||
		token("founder key id", founderKeyID) != nil ||
		len(founderPublicKey) != ed25519.PublicKeySize || now == nil {
		return nil, fmt.Errorf("external adapter: store dependencies and founder authority are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("external adapter: Vault user does not match tenant")
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		founderKey: founderKeyID,
		publicKey:  append(ed25519.PublicKey(nil), founderPublicKey...),
		now:        now,
	}, nil
}

func (store *Store) RegisterConnection(
	ctx context.Context,
	connection Connection,
	credential CredentialMaterial,
) error {
	if err := VerifyConnection(connection, store.founderKey, store.publicKey); err != nil {
		return err
	}
	if err := credential.Validate(); err != nil || credential.ID != connection.CredentialID {
		return fmt.Errorf("external adapter: credential does not match signed connection")
	}
	credentialBinding, err := CredentialBindingHash(
		connection.ID, connection.Version, credential,
	)
	if err != nil || credentialBinding != connection.CredentialBinding {
		return fmt.Errorf("external adapter: credential material does not match founder binding")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if !connection.ExpiresAt.After(now) {
		return fmt.Errorf("%w: connection authority is expired", ErrDenied)
	}
	canonical, err := contracts.EncodeCanonical(&connection)
	if err != nil {
		return err
	}
	credentialCanonical, err := contracts.EncodeCanonical(&credential)
	if err != nil {
		return err
	}
	defer zeroBytes(credentialCanonical)
	digest := sha256.Sum256(canonical)
	hash := hex.EncodeToString(digest[:])
	sealed, err := store.vault.SealRecord(store.connectionAD(
		connection.OrganizationID, connection.ID, connection.Version,
	), canonical)
	if err != nil {
		return fmt.Errorf("external adapter: seal connection: %w", err)
	}
	sealedCredential, err := store.vault.SealRecord(store.credentialAD(
		connection.OrganizationID, connection.ID, connection.Version, credential.ID,
	), credentialCanonical)
	if err != nil {
		return fmt.Errorf("external adapter: seal credential: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("external adapter: begin connection registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.connectionLock(connection.OrganizationID, connection.ID)); err != nil {
		return fmt.Errorf("external adapter: lock connection registration: %w", err)
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_external_connections (
			tenant_id,organization_id,connection_id,version,adapter_name,family,
			protocol,provider,account_id,identity_id,credential_id,canonical_hash,
			sealed_record,effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT DO NOTHING
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
		connection.AdapterName, connection.Family, connection.Protocol, connection.Provider,
		connection.AccountID, connection.IdentityID, connection.CredentialID, hash,
		sealed, connection.EffectiveAt, connection.ExpiresAt, now)
	if err != nil {
		return fmt.Errorf("external adapter: persist connection: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingHash, credentialID string
		var existingCredential []byte
		err := tx.QueryRow(ctx, `
			SELECT c.canonical_hash,c.credential_id,k.sealed_credential
			FROM workforce_external_connections c
			JOIN workforce_external_credentials k
			  ON k.tenant_id=c.tenant_id AND k.organization_id=c.organization_id
			 AND k.connection_id=c.connection_id AND k.connection_version=c.version
			 AND k.credential_id=c.credential_id
			WHERE c.tenant_id=$1 AND c.organization_id=$2
			  AND c.connection_id=$3 AND c.version=$4
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version).Scan(&existingHash, &credentialID, &existingCredential)
		if err != nil || existingHash != hash || credentialID != credential.ID {
			return ErrConflict
		}
		opened, err := store.vault.OpenRecord(store.credentialAD(
			connection.OrganizationID, connection.ID, connection.Version, credential.ID,
		), existingCredential)
		if err != nil || !bytes.Equal(opened, credentialCanonical) {
			return ErrConflict
		}
	} else {
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_external_credentials (
				tenant_id,organization_id,connection_id,connection_version,
				credential_id,credential_kind,sealed_credential,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
		`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
			credential.ID, credential.Kind, sealedCredential, now); err != nil {
			return fmt.Errorf("external adapter: persist sealed credential: %w", err)
		}
	}
	state := "active"
	var currentVersion uint64
	var currentState string
	err = tx.QueryRow(ctx, `
		SELECT version,state FROM workforce_external_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		FOR UPDATE
	`, store.tenantID, connection.OrganizationID, connection.ID).Scan(
		&currentVersion, &currentState,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_external_connection_heads (
				tenant_id,organization_id,connection_id,version,canonical_hash,
				state,effective_at,expires_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
			hash, state, connection.EffectiveAt, connection.ExpiresAt, now)
	case err != nil:
		return fmt.Errorf("external adapter: load connection head: %w", err)
	case currentVersion > connection.Version:
		return ErrConflict
	case currentVersion == connection.Version:
		if currentState == "revoked" {
			return fmt.Errorf("%w: revoked connection cannot be reactivated", ErrDenied)
		}
	case currentVersion < connection.Version:
		_, err = tx.Exec(ctx, `
			UPDATE workforce_external_connection_heads
			SET version=$1,canonical_hash=$2,state=$3,effective_at=$4,
			    expires_at=$5,revoked_at=NULL,updated_at=$6
			WHERE tenant_id=$7 AND organization_id=$8 AND connection_id=$9
		`, connection.Version, hash, state, connection.EffectiveAt,
			connection.ExpiresAt, now, store.tenantID,
			connection.OrganizationID, connection.ID)
	}
	if err != nil {
		return fmt.Errorf("external adapter: persist connection head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("external adapter: commit connection registration: %w", err)
	}
	return nil
}

func (store *Store) RevokeConnection(
	ctx context.Context,
	revocation ConnectionRevocation,
) error {
	if err := VerifyConnectionRevocation(revocation, store.founderKey, store.publicKey); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if revocation.RevokedAt.After(now) {
		return fmt.Errorf("external adapter: revocation cannot be future-dated")
	}
	canonical, err := contracts.EncodeCanonical(&revocation)
	if err != nil {
		return err
	}
	hashValue := sha256.Sum256(canonical)
	hash := hex.EncodeToString(hashValue[:])
	sealed, err := store.vault.SealRecord(store.revocationAD(
		revocation.OrganizationID, revocation.ID,
	), canonical)
	if err != nil {
		return fmt.Errorf("external adapter: seal revocation: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("external adapter: begin revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var currentVersion uint64
	var state string
	err = tx.QueryRow(ctx, `
		SELECT version,state FROM workforce_external_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		FOR UPDATE
	`, store.tenantID, revocation.OrganizationID, revocation.ConnectionID).Scan(
		&currentVersion, &state,
	)
	if err != nil || currentVersion != revocation.Version {
		return ErrConflict
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_external_connection_revocations (
			tenant_id,organization_id,revocation_id,connection_id,
			connection_version,reason_code,canonical_hash,sealed_record,
			revoked_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING
	`, store.tenantID, revocation.OrganizationID, revocation.ID,
		revocation.ConnectionID, revocation.Version, revocation.ReasonCode,
		hash, sealed, revocation.RevokedAt, now)
	if err != nil {
		return fmt.Errorf("external adapter: persist revocation: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		err := tx.QueryRow(ctx, `
			SELECT canonical_hash
			FROM workforce_external_connection_revocations
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4
		`, store.tenantID, revocation.OrganizationID, revocation.ConnectionID,
			revocation.Version).Scan(&existingHash)
		if err != nil || existingHash != hash || state != "revoked" {
			return ErrConflict
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_external_connection_heads
		SET state='revoked',revoked_at=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND connection_id=$5
		  AND version=$6
	`, revocation.RevokedAt, now, store.tenantID, revocation.OrganizationID,
		revocation.ConnectionID, revocation.Version); err != nil {
		return fmt.Errorf("external adapter: revoke connection head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_external_inflight
		SET expires_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND connection_id=$4
		  AND connection_version=$5 AND expires_at>$1
	`, now, store.tenantID, revocation.OrganizationID,
		revocation.ConnectionID, revocation.Version); err != nil {
		return fmt.Errorf("external adapter: expire revoked connection slots: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("external adapter: commit revocation: %w", err)
	}
	return nil
}

func (store *Store) LoadConnection(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
) (ConnectionView, error) {
	if token("organization id", string(organizationID)) != nil ||
		token("connection id", connectionID) != nil {
		return ConnectionView{}, ErrDenied
	}
	var version uint64
	if err := store.pool.QueryRow(ctx, `
		SELECT version FROM workforce_external_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
	`, store.tenantID, organizationID, connectionID).Scan(&version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ConnectionView{}, ErrDenied
		}
		return ConnectionView{}, fmt.Errorf("%w: load connection head: %v", ErrUnavailable, err)
	}
	loaded, err := store.loadActive(ctx, organizationID, connectionID, version)
	if err != nil {
		return ConnectionView{}, err
	}
	defer wipeCredential(&loaded.credential)
	return ConnectionView{Connection: loaded.connection, Hash: loaded.hash}, nil
}

func (store *Store) ListActiveConnections(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) ([]ConnectionView, error) {
	if store == nil || token("organization id", string(organizationID)) != nil {
		return nil, ErrDenied
	}
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT connection_id,version
		FROM workforce_external_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
		  AND effective_at <= $3 AND expires_at > $3
		ORDER BY connection_id
		LIMIT 257
	`, store.tenantID, organizationID, now)
	if err != nil {
		return nil, fmt.Errorf("%w: list active connections: %v", ErrUnavailable, err)
	}
	type connectionHead struct {
		id      string
		version uint64
	}
	heads := make([]connectionHead, 0, 16)
	for rows.Next() {
		var head connectionHead
		if err := rows.Scan(&head.id, &head.version); err != nil {
			rows.Close()
			return nil, fmt.Errorf("%w: scan active connection: %v", ErrUnavailable, err)
		}
		heads = append(heads, head)
	}
	rowsErr := rows.Err()
	rows.Close()
	if rowsErr != nil {
		return nil, fmt.Errorf("%w: list active connections: %v", ErrUnavailable, rowsErr)
	}
	if len(heads) > 256 {
		return nil, fmt.Errorf("%w: active adapter registry exceeds limit", ErrCapacity)
	}
	views := make([]ConnectionView, 0, len(heads))
	for _, head := range heads {
		loaded, err := store.loadActive(ctx, organizationID, head.id, head.version)
		if err != nil {
			return nil, err
		}
		views = append(views, ConnectionView{
			Connection: loaded.connection,
			Hash:       loaded.hash,
		})
		wipeCredential(&loaded.credential)
	}
	return views, nil
}

func BuildEnvelope(
	view ConnectionView,
	grant lease.Grant,
	operationName string,
	idempotencyKey string,
	request BoundRequest,
	now time.Time,
) (Envelope, []byte, error) {
	connectionHash, hashErr := CanonicalHash(&view.Connection)
	if hashErr != nil || connectionHash != view.Hash ||
		view.Connection.Validate() != nil || view.Hash.Validate() != nil ||
		grant.Request.Validate() != nil ||
		grant.Fence.Validate() != nil || grant.State != lease.StateActive ||
		grant.OrganizationID != view.Connection.OrganizationID ||
		!grant.ExpiresAt.After(now) || token("idempotency key", idempotencyKey) != nil {
		return Envelope{}, nil, ErrDenied
	}
	operation, found := view.Connection.Operation(operationName)
	if !found {
		return Envelope{}, nil, ErrDenied
	}
	if err := validateBoundRequest(
		view.Connection, operation, request, idempotencyKey, now,
	); err != nil {
		return Envelope{}, nil, err
	}
	if request.ExpiresAt.After(grant.ExpiresAt) ||
		request.ExpiresAt.After(view.Connection.ExpiresAt) {
		return Envelope{}, nil, ErrDenied
	}
	envelope := Envelope{
		SchemaVersion: SchemaVersion, Grant: grant,
		ConnectionID:      view.Connection.ID,
		ConnectionVersion: view.Connection.Version,
		ConnectionHash:    view.Hash,
		Request:           request,
	}
	encoded, err := contracts.EncodeCanonical(&envelope)
	if err != nil {
		return Envelope{}, nil, err
	}
	return envelope, encoded, nil
}

func (store *Store) loadActive(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
) (loadedConnection, error) {
	if token("organization id", string(organizationID)) != nil ||
		token("connection id", connectionID) != nil || version == 0 {
		return loadedConnection{}, ErrDenied
	}
	now, err := store.currentTime()
	if err != nil {
		return loadedConnection{}, err
	}
	var canonicalHash, state, credentialID string
	var sealed, sealedCredential []byte
	var effectiveAt, expiresAt time.Time
	err = store.pool.QueryRow(ctx, `
		SELECT c.canonical_hash,c.sealed_record,c.credential_id,
		       h.state,h.effective_at,h.expires_at,k.sealed_credential
		FROM workforce_external_connection_heads h
		JOIN workforce_external_connections c
		  ON c.tenant_id=h.tenant_id AND c.organization_id=h.organization_id
		 AND c.connection_id=h.connection_id AND c.version=h.version
		JOIN workforce_external_credentials k
		  ON k.tenant_id=c.tenant_id AND k.organization_id=c.organization_id
		 AND k.connection_id=c.connection_id AND k.connection_version=c.version
		 AND k.credential_id=c.credential_id
		WHERE h.tenant_id=$1 AND h.organization_id=$2
		  AND h.connection_id=$3 AND h.version=$4
	`, store.tenantID, organizationID, connectionID, version).Scan(
		&canonicalHash, &sealed, &credentialID, &state,
		&effectiveAt, &expiresAt, &sealedCredential,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedConnection{}, ErrDenied
	}
	if err != nil {
		return loadedConnection{}, fmt.Errorf("%w: load connection: %v", ErrUnavailable, err)
	}
	if state != "active" || effectiveAt.After(now) || !expiresAt.After(now) {
		return loadedConnection{}, ErrDenied
	}
	opened, err := store.vault.OpenRecord(store.connectionAD(
		organizationID, connectionID, version,
	), sealed)
	if err != nil {
		return loadedConnection{}, ErrIntegrity
	}
	digest := sha256.Sum256(opened)
	if hex.EncodeToString(digest[:]) != canonicalHash {
		return loadedConnection{}, ErrIntegrity
	}
	connection, err := contracts.DecodeCanonical[Connection, *Connection](opened)
	if err != nil || VerifyConnection(connection, store.founderKey, store.publicKey) != nil ||
		connection.OrganizationID != organizationID || connection.ID != connectionID ||
		connection.Version != version || connection.CredentialID != credentialID {
		return loadedConnection{}, ErrIntegrity
	}
	credentialBytes, err := store.vault.OpenRecord(store.credentialAD(
		organizationID, connectionID, version, credentialID,
	), sealedCredential)
	if err != nil {
		return loadedConnection{}, ErrIntegrity
	}
	defer zeroBytes(credentialBytes)
	credential, err := contracts.DecodeCanonical[CredentialMaterial, *CredentialMaterial](credentialBytes)
	if err != nil || credential.Validate() != nil || credential.ID != credentialID {
		return loadedConnection{}, ErrIntegrity
	}
	credentialBinding, err := CredentialBindingHash(connection.ID, connection.Version, credential)
	if err != nil || credentialBinding != connection.CredentialBinding {
		wipeCredential(&credential)
		return loadedConnection{}, ErrIntegrity
	}
	credentialCopy := credential.Clone()
	wipeCredential(&credential)
	return loadedConnection{
		connection: connection, credential: credentialCopy,
		hash: contracts.ContentHash{Algorithm: "sha256", Digest: canonicalHash},
	}, nil
}

func (store *Store) claimAttempt(
	ctx context.Context,
	connection Connection,
	operation OperationPolicy,
	idempotencyKey string,
	requestHash contracts.ContentHash,
	kind attemptKind,
) (attemptClaim, error) {
	if token("idempotency key", idempotencyKey) != nil ||
		requestHash.Validate() != nil ||
		(kind != attemptDispatch && kind != attemptProbe && kind != attemptCompensate) {
		return attemptClaim{}, ErrDenied
	}
	now, err := store.currentTime()
	if err != nil {
		return attemptClaim{}, err
	}
	attemptID, err := randomToken()
	if err != nil {
		return attemptClaim{}, fmt.Errorf("%w: mint attempt identity", ErrUnavailable)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return attemptClaim{}, fmt.Errorf("%w: begin attempt claim: %v", ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	operationLock := store.connectionLock(connection.OrganizationID, connection.ID) +
		"/operation/" + operation.Name
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, operationLock); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: lock operation circuit: %v", ErrUnavailable, err)
	}
	identityLock := operationLock + "/" + idempotencyKey
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`, identityLock); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: lock attempt: %v", ErrUnavailable, err)
	}
	var headVersion uint64
	var headState string
	var effectiveAt, expiresAt time.Time
	if err := tx.QueryRow(ctx, `
		SELECT version,state,effective_at,expires_at
		FROM workforce_external_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		FOR UPDATE
	`, store.tenantID, connection.OrganizationID, connection.ID).Scan(
		&headVersion, &headState, &effectiveAt, &expiresAt,
	); err != nil || headVersion != connection.Version || headState != "active" ||
		effectiveAt.After(now) || !expiresAt.After(now) {
		return attemptClaim{}, ErrDenied
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM workforce_external_inflight
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND expires_at <= $5
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, now); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: expire operation slots: %v", ErrUnavailable, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_external_operation_circuits (
			tenant_id,organization_id,connection_id,connection_version,
			operation,state,window_started_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,'closed',$6,$6)
		ON CONFLICT DO NOTHING
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, operation.Name, now); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: initialize operation circuit: %v", ErrUnavailable, err)
	}
	var circuitState string
	var retryAt *time.Time
	if err := tx.QueryRow(ctx, `
		SELECT state,retry_at
		FROM workforce_external_operation_circuits
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5
		FOR UPDATE
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, operation.Name).Scan(&circuitState, &retryAt); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: load operation circuit: %v", ErrUnavailable, err)
	}
	if circuitState == "open" {
		if retryAt == nil || now.Before(*retryAt) {
			return attemptClaim{}, ErrCircuitOpen
		}
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_external_operation_circuits
			SET state='half_open',success_count=0,retry_at=NULL,
			    updated_at=$1,version=version+1
			WHERE tenant_id=$2 AND organization_id=$3 AND connection_id=$4
			  AND connection_version=$5 AND operation=$6
		`, now, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, operation.Name); err != nil {
			return attemptClaim{}, fmt.Errorf("%w: enter operation half-open: %v", ErrUnavailable, err)
		}
		circuitState = "half_open"
	}
	if circuitState == "half_open" {
		if operation.EffectClass == "irreversible" {
			return attemptClaim{}, ErrCircuitOpen
		}
		var trials int64
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_external_inflight
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND operation=$5 AND expires_at>$6
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, operation.Name, now).Scan(&trials); err != nil {
			return attemptClaim{}, fmt.Errorf("%w: count half-open operation trials: %v", ErrUnavailable, err)
		}
		if trials >= int64(connection.Limits.HalfOpenLimit) {
			return attemptClaim{}, ErrCircuitOpen
		}
	}
	if kind == attemptDispatch || kind == attemptCompensate {
		command, err := tx.Exec(ctx, `
			INSERT INTO workforce_external_identities (
				tenant_id,organization_id,connection_id,connection_version,
				operation,idempotency_key,request_hash,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
			ON CONFLICT DO NOTHING
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, operation.Name, idempotencyKey,
			requestHash.Digest, now)
		if err != nil {
			return attemptClaim{}, fmt.Errorf("%w: persist idempotency identity: %v", ErrUnavailable, err)
		}
		if command.RowsAffected() == 0 {
			var existingHash string
			err := tx.QueryRow(ctx, `
				SELECT request_hash FROM workforce_external_identities
				WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
				  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
			`, store.tenantID, connection.OrganizationID, connection.ID,
				connection.Version, operation.Name, idempotencyKey).Scan(&existingHash)
			if err != nil || existingHash != requestHash.Digest {
				return attemptClaim{}, ErrConflict
			}
			return attemptClaim{}, ErrConflict
		}
	} else {
		var existingHash string
		err := tx.QueryRow(ctx, `
			SELECT request_hash FROM workforce_external_identities
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, operation.Name, idempotencyKey).Scan(&existingHash)
		if err != nil || existingHash != requestHash.Digest {
			return attemptClaim{}, ErrConflict
		}
	}
	windowStart := now.Add(-connection.Limits.RetryWindow)
	var attempts int64
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_external_operation_attempts
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5 AND idempotency_key=$6
		  AND started_at >= $7
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, operation.Name, idempotencyKey, windowStart).Scan(&attempts); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: count retry budget: %v", ErrUnavailable, err)
	}
	if attempts >= int64(connection.Limits.MaxAttempts) {
		return attemptClaim{}, ErrRetryExhausted
	}
	var inFlight int64
	if err := tx.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_external_inflight
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND expires_at>$5
	`, store.tenantID, connection.OrganizationID, connection.ID,
		connection.Version, now).Scan(&inFlight); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: count operation slots: %v", ErrUnavailable, err)
	}
	if inFlight >= int64(connection.Limits.MaxConcurrent) {
		return attemptClaim{}, ErrCapacity
	}
	if (kind == attemptDispatch || kind == attemptCompensate) && operation.Action.Mutates() &&
		!operation.ProbeAuthoritative {
		var open int64
		if err := tx.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_external_drift_exposures
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
			  AND connection_version=$4 AND operation=$5 AND state='open'
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, operation.Name).Scan(&open); err != nil {
			return attemptClaim{}, fmt.Errorf("%w: count drift ceiling: %v", ErrUnavailable, err)
		}
		if open >= int64(connection.Limits.DriftBlindMutations) {
			return attemptClaim{}, fmt.Errorf("%w: drift-blind autonomy ceiling reached", ErrDenied)
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO workforce_external_drift_exposures (
				tenant_id,organization_id,connection_id,connection_version,
				operation,idempotency_key,state,created_at
			) VALUES ($1,$2,$3,$4,$5,$6,'open',$7)
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, operation.Name, idempotencyKey, now); err != nil {
			return attemptClaim{}, fmt.Errorf("%w: reserve drift ceiling: %v", ErrUnavailable, err)
		}
	}
	expires := now.Add(connection.Limits.TotalTimeout)
	if expires.After(connection.ExpiresAt) {
		expires = connection.ExpiresAt
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_external_operation_attempts (
			tenant_id,organization_id,attempt_id,connection_id,connection_version,
			operation,idempotency_key,attempt_kind,request_hash,state,
			started_at,expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'in_flight',$10,$11)
	`, store.tenantID, connection.OrganizationID, attemptID, connection.ID,
		connection.Version, operation.Name, idempotencyKey, kind,
		requestHash.Digest, now, expires); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: persist attempt: %v", ErrUnavailable, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_external_inflight (
			tenant_id,organization_id,attempt_id,connection_id,
			connection_version,operation,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)
	`, store.tenantID, connection.OrganizationID, attemptID, connection.ID,
		connection.Version, operation.Name, expires, now); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: reserve operation slot: %v", ErrUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return attemptClaim{}, fmt.Errorf("%w: commit attempt claim: %v", ErrUnavailable, err)
	}
	return attemptClaim{
		id: attemptID, organizationID: connection.OrganizationID,
		connectionID: connection.ID, version: connection.Version,
		operation: operation.Name, idempotencyKey: idempotencyKey, kind: kind,
		limits: connection.Limits,
	}, nil
}

func (store *Store) completeAttempt(
	ctx context.Context,
	claim attemptClaim,
	state string,
	safeCode string,
	externalID string,
	observationHash string,
) error {
	if state != "completed" && state != "failed" && state != "ambiguous" {
		return fmt.Errorf("external adapter: invalid attempt terminal state")
	}
	if len(safeCode) > 128 || len(externalID) > 512 ||
		(observationHash != "" && len(observationHash) != 64) {
		return fmt.Errorf("external adapter: invalid bounded attempt result")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return fmt.Errorf("%w: begin attempt completion: %v", ErrUnavailable, err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if err := store.completeCircuit(ctx, tx, claim, state == "completed", safeCode, now); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		UPDATE workforce_external_operation_attempts
		SET state=$1,safe_code=NULLIF($2,''),external_id=NULLIF($3,''),
		    observation_hash=NULLIF($4,''),finished_at=$5
		WHERE tenant_id=$6 AND organization_id=$7 AND attempt_id=$8
		  AND state='in_flight'
	`, state, safeCode, externalID, observationHash, now,
		store.tenantID, claim.organizationID, claim.id)
	if err != nil || command.RowsAffected() != 1 {
		return fmt.Errorf("%w: complete operation attempt", ErrUnavailable)
	}
	if _, err := tx.Exec(ctx, `
		DELETE FROM workforce_external_inflight
		WHERE tenant_id=$1 AND organization_id=$2 AND attempt_id=$3
	`, store.tenantID, claim.organizationID, claim.id); err != nil {
		return fmt.Errorf("%w: release operation slot: %v", ErrUnavailable, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("%w: commit attempt completion: %v", ErrUnavailable, err)
	}
	return nil
}

func (store *Store) completeCircuit(
	ctx context.Context,
	tx pgx.Tx,
	claim attemptClaim,
	success bool,
	safeCode string,
	now time.Time,
) error {
	var state string
	var failures, successes int64
	var windowStarted time.Time
	err := tx.QueryRow(ctx, `
		SELECT state,failure_count,success_count,window_started_at
		FROM workforce_external_operation_circuits
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND operation=$5
		FOR UPDATE
	`, store.tenantID, claim.organizationID, claim.connectionID,
		claim.version, claim.operation).Scan(
		&state, &failures, &successes, &windowStarted,
	)
	if err != nil {
		return fmt.Errorf("%w: load operation circuit completion: %v", ErrUnavailable, err)
	}
	if success {
		if state == "open" {
			return nil
		}
		if state == "half_open" {
			successes++
			if successes >= int64(claim.limits.SuccessThreshold) {
				state = "closed"
				failures = 0
				successes = 0
				windowStarted = now
			}
		} else if state == "closed" {
			failures = 0
			successes = 0
			windowStarted = now
		}
		_, err = tx.Exec(ctx, `
			UPDATE workforce_external_operation_circuits
			SET state=$1,failure_count=$2,success_count=$3,
			    window_started_at=$4,retry_at=NULL,last_safe_code=NULL,
			    updated_at=$5,version=version+1
			WHERE tenant_id=$6 AND organization_id=$7 AND connection_id=$8
			  AND connection_version=$9 AND operation=$10
		`, state, failures, successes, windowStarted, now, store.tenantID,
			claim.organizationID, claim.connectionID, claim.version, claim.operation)
	} else {
		if safeCode == "" {
			safeCode = "external_operation_failed"
		}
		if state == "half_open" || state == "open" {
			state = "open"
			failures++
		} else {
			if now.Sub(windowStarted) > claim.limits.CircuitWindow {
				failures = 0
				windowStarted = now
			}
			failures++
			if failures >= int64(claim.limits.FailureThreshold) {
				state = "open"
			}
		}
		var retryAt any
		if state == "open" {
			retryAt = now.Add(claim.limits.CircuitOpenDuration)
		}
		_, err = tx.Exec(ctx, `
			UPDATE workforce_external_operation_circuits
			SET state=$1,failure_count=$2,success_count=0,
			    window_started_at=$3,retry_at=$4,last_safe_code=$5,
			    updated_at=$6,version=version+1
			WHERE tenant_id=$7 AND organization_id=$8 AND connection_id=$9
			  AND connection_version=$10 AND operation=$11
		`, state, failures, windowStarted, retryAt, safeCode, now,
			store.tenantID, claim.organizationID, claim.connectionID,
			claim.version, claim.operation)
	}
	if err != nil {
		return fmt.Errorf("%w: update operation circuit: %v", ErrUnavailable, err)
	}
	return nil
}

func (store *Store) recordObservation(
	ctx context.Context,
	observation Observation,
) (contracts.ContentHash, error) {
	if err := observation.Validate(); err != nil {
		return contracts.ContentHash{}, err
	}
	canonical, err := contracts.EncodeCanonical(&observation)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	digest := sha256.Sum256(canonical)
	hash := contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(digest[:])}
	observationID, err := randomToken()
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("%w: mint observation identity", ErrUnavailable)
	}
	sealed, err := store.vault.SealRecord(store.observationAD(
		observation.OrganizationID, observation.ConnectionID, observationID,
	), canonical)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("external adapter: seal observation: %w", err)
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO workforce_external_observations (
			tenant_id,organization_id,observation_id,connection_id,
			connection_version,operation,external_id,idempotency_key,
			external_state,authority,canonical_hash,sealed_record,
			provider_observed_at,captured_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14)
	`, store.tenantID, observation.OrganizationID, observationID,
		observation.ConnectionID, observation.ConnectionVersion,
		observation.Operation, observation.ExternalID, observation.IdempotencyKey,
		observation.State, observation.Authority, hash.Digest, sealed,
		observation.ProviderObservedAt, observation.CapturedAt)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("%w: persist observation: %v", ErrUnavailable, err)
	}
	return hash, nil
}

func (store *Store) resolveDrift(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	operation string,
	idempotencyKey string,
	state string,
) error {
	if state != "reconciled" && state != "compensated" {
		return fmt.Errorf("external adapter: drift resolution state is invalid")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	command, err := store.pool.Exec(ctx, `
		UPDATE workforce_external_drift_exposures
		SET state=$1,resolved_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND connection_id=$5
		  AND connection_version=$6 AND operation=$7 AND idempotency_key=$8
		  AND state='open'
	`, state, now, store.tenantID, organizationID, connectionID,
		version, operation, idempotencyKey)
	if err != nil {
		return fmt.Errorf("%w: resolve drift exposure: %v", ErrUnavailable, err)
	}
	if command.RowsAffected() > 1 {
		return ErrIntegrity
	}
	return nil
}

func (store *Store) recordIncident(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	operation string,
	idempotencyKey string,
	kind string,
	safeCode string,
) error {
	if token("incident kind", kind) != nil || token("safe code", safeCode) != nil {
		return fmt.Errorf("external adapter: incident identity is invalid")
	}
	id, err := randomToken()
	if err != nil {
		return fmt.Errorf("%w: mint incident identity", ErrUnavailable)
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	_, err = store.pool.Exec(ctx, `
		INSERT INTO workforce_external_incidents (
			tenant_id,organization_id,incident_id,connection_id,
			connection_version,operation,idempotency_key,kind,safe_code,
			state,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,'open',$10)
	`, store.tenantID, organizationID, id, connectionID, version,
		operation, idempotencyKey, kind, safeCode, now)
	if err != nil {
		return fmt.Errorf("%w: persist external incident: %v", ErrUnavailable, err)
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("%w: time source must return UTC", ErrUnavailable)
	}
	return now, nil
}

func (store *Store) connectionLock(
	organizationID contracts.OrganizationID,
	connectionID string,
) string {
	return store.tenantID + "/" + string(organizationID) + "/external/" + connectionID
}

func (store *Store) connectionAD(
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.external.connection",
		Stream: fmt.Sprintf("%s/%s/%d", organizationID, connectionID, version),
		Schema: SchemaVersion,
	}
}

func (store *Store) credentialAD(
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	credentialID string,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.external.credential",
		Stream: fmt.Sprintf("%s/%s/%d/%s", organizationID, connectionID, version, credentialID),
		Schema: SchemaVersion,
	}
}

func (store *Store) revocationAD(
	organizationID contracts.OrganizationID,
	revocationID string,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.external.revocation",
		Stream: string(organizationID) + "/" + revocationID,
		Schema: SchemaVersion,
	}
}

func (store *Store) observationAD(
	organizationID contracts.OrganizationID,
	connectionID string,
	observationID string,
) vault.AD {
	return vault.AD{
		User: store.tenantID, Store: "workforce.external.observation",
		Stream: string(organizationID) + "/" + connectionID + "/" + observationID,
		Schema: SchemaVersion,
	}
}

func randomToken() (string, error) {
	value := make([]byte, 16)
	if _, err := rand.Read(value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func zeroBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
