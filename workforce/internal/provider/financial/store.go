package financial

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
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

type Store struct {
	pool       *pgxpool.Pool
	vault      *vault.UserVault
	tenantID   string
	founderKey string
	founderPub ed25519.PublicKey
	issuerKeys map[string]ed25519.PublicKey
	now        func() time.Time
}

type loadedConnection struct {
	connection Connection
	hash       contracts.ContentHash
}

type loadedValuation struct {
	valuation ValuationSnapshot
	hash      contracts.ContentHash
}

type loadedRisk struct {
	id         string
	version    uint64
	state      RiskState
	hash       contracts.ContentHash
	observedAt time.Time
	expiresAt  time.Time
	sourceKind string
	sourceID   string
}

type attemptKind string

const (
	attemptDispatch attemptKind = "dispatch"
	attemptProbe    attemptKind = "probe"
)

type attemptClaim struct {
	id             string
	reservationID  string
	organizationID contracts.OrganizationID
	connectionID   string
	version        uint64
	operation      string
	idempotencyKey string
	requestHash    contracts.ContentHash
	kind           attemptKind
	policy         CapitalPolicy
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	founderKeyID string,
	founderPublicKey ed25519.PublicKey,
	issuerKeys map[string]ed25519.PublicKey,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" ||
		token("founder key id", founderKeyID) != nil ||
		len(founderPublicKey) != ed25519.PublicKeySize || len(issuerKeys) == 0 || now == nil {
		return nil, fmt.Errorf("financial adapter: store dependencies and founder authority are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("financial adapter: Vault user does not match tenant")
	}
	keys := make(map[string]ed25519.PublicKey, len(issuerKeys))
	for keyID, publicKey := range issuerKeys {
		if token("issuer key id", keyID) != nil || len(publicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("financial adapter: issuer key registry is invalid")
		}
		keys[keyID] = append(ed25519.PublicKey(nil), publicKey...)
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		founderKey: founderKeyID, founderPub: append(ed25519.PublicKey(nil), founderPublicKey...),
		issuerKeys: keys, now: now,
	}, nil
}

func (store *Store) RegisterConnection(ctx context.Context, connection Connection) error {
	if err := VerifyConnection(connection, store.founderKey, store.founderPub); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if !connection.ExpiresAt.After(now) {
		return fmt.Errorf("%w: connection authority is expired", ErrDenied)
	}
	if err := store.verifyExternalBinding(ctx, connection, now); err != nil {
		return err
	}
	canonical, err := contracts.EncodeCanonical(&connection)
	if err != nil {
		return err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.connectionAD(
		connection.OrganizationID, connection.ID, connection.Version,
	), canonical)
	if err != nil {
		return fmt.Errorf("financial adapter: seal connection: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("financial adapter: begin connection registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(connection.OrganizationID, connection.ID)); err != nil {
		return fmt.Errorf("financial adapter: lock connection registration: %w", err)
	}
	var existingHash string
	err = tx.QueryRow(ctx, `
		SELECT canonical_hash FROM workforce_financial_connections
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND version=$4
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version).Scan(&existingHash)
	if err == nil {
		if existingHash != hash {
			return ErrConflict
		}
		return tx.Commit(ctx)
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("financial adapter: inspect connection replay: %w", err)
	}
	var prior uint64
	err = tx.QueryRow(ctx, `
		SELECT version FROM workforce_financial_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 FOR UPDATE
	`, store.tenantID, connection.OrganizationID, connection.ID).Scan(&prior)
	if errors.Is(err, pgx.ErrNoRows) {
		if connection.Version != 1 {
			return ErrConflict
		}
	} else if err != nil {
		return fmt.Errorf("financial adapter: load connection head: %w", err)
	} else if connection.Version != prior+1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_connections (
			tenant_id,organization_id,connection_id,version,adapter_name,
			external_adapter_name,external_connection_id,external_connection_version,
			family,account_id,identity_id,base_currency,provider_contract_kind,
			network_id,chain_id,settlement_contract,contract_version,
			required_confirmations,canonical_hash,sealed_record,
			effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19,$20,$21,$22,$23)
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
		connection.AdapterName, connection.ExternalAdapterName, connection.ExternalConnectionID,
		connection.ExternalConnectionVersion, connection.Family, connection.AccountID,
		connection.IdentityID, connection.Capital.BaseCurrency, connection.ProviderContract.Kind,
		connection.ProviderContract.NetworkID, connection.ProviderContract.ChainID,
		connection.ProviderContract.SettlementContract, connection.ProviderContract.ContractVersion,
		connection.ProviderContract.RequiredConfirmations, hash, sealed,
		connection.EffectiveAt, connection.ExpiresAt, now); err != nil {
		return fmt.Errorf("financial adapter: persist connection: %w", err)
	}
	state := "scheduled"
	if !connection.EffectiveAt.After(now) {
		state = "active"
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_connection_heads (
			tenant_id,organization_id,connection_id,version,canonical_hash,state,
			effective_at,expires_at,revoked_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,NULL,$9)
		ON CONFLICT (tenant_id,organization_id,connection_id) DO UPDATE SET
			version=EXCLUDED.version,canonical_hash=EXCLUDED.canonical_hash,
			state=EXCLUDED.state,effective_at=EXCLUDED.effective_at,
			expires_at=EXCLUDED.expires_at,revoked_at=NULL,updated_at=EXCLUDED.updated_at
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
		hash, state, connection.EffectiveAt, connection.ExpiresAt, now); err != nil {
		return fmt.Errorf("financial adapter: advance connection head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("financial adapter: commit connection registration: %w", err)
	}
	return nil
}

func (store *Store) RevokeConnection(ctx context.Context, revocation ConnectionRevocation) error {
	if err := VerifyConnectionRevocation(revocation, store.founderKey, store.founderPub); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if revocation.RevokedAt.After(now) {
		return fmt.Errorf("%w: future revocation is not effective", ErrDenied)
	}
	canonical, err := contracts.EncodeCanonical(&revocation)
	if err != nil {
		return err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.revocationAD(
		revocation.OrganizationID, revocation.ID,
	), canonical)
	if err != nil {
		return fmt.Errorf("financial adapter: seal revocation: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("financial adapter: begin revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	var version uint64
	var state string
	if err := tx.QueryRow(ctx, `
		SELECT version,state FROM workforce_financial_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 FOR UPDATE
	`, store.tenantID, revocation.OrganizationID, revocation.ConnectionID).Scan(&version, &state); err != nil {
		return fmt.Errorf("financial adapter: load revocation target: %w", err)
	}
	if version != revocation.Version || state == "revoked" {
		return ErrConflict
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_connection_revocations (
			tenant_id,organization_id,revocation_id,connection_id,connection_version,
			reason_code,canonical_hash,sealed_record,revoked_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT DO NOTHING
	`, store.tenantID, revocation.OrganizationID, revocation.ID, revocation.ConnectionID,
		revocation.Version, revocation.ReasonCode, hash, sealed, revocation.RevokedAt, now)
	if err != nil {
		return fmt.Errorf("financial adapter: persist revocation: %w", err)
	}
	if command.RowsAffected() != 1 {
		return ErrConflict
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_financial_connection_heads
		SET state='revoked',revoked_at=$1,updated_at=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND connection_id=$5 AND version=$6
	`, revocation.RevokedAt, now, store.tenantID, revocation.OrganizationID,
		revocation.ConnectionID, revocation.Version); err != nil {
		return fmt.Errorf("financial adapter: revoke connection head: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		UPDATE workforce_financial_scope_freezes SET state='escalated',resolved_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND state='open'
	`, now, store.tenantID, revocation.OrganizationID); err != nil {
		return fmt.Errorf("financial adapter: preserve freezes during revocation: %w", err)
	}
	return tx.Commit(ctx)
}

func (store *Store) RegisterValuation(ctx context.Context, value ValuationSnapshot) error {
	connection, err := store.loadConnectionVersion(ctx, value.OrganizationID, value.ConnectionID, value.ConnectionVersion)
	if err != nil {
		return err
	}
	key, ok := store.issuerKeys[value.Signature.KeyID]
	if !ok || !contains(connection.connection.Authority.ValuationIssuerKeyIDs, value.Signature.KeyID) ||
		VerifyValuationSnapshot(value, value.Signature.KeyID, key) != nil ||
		value.BaseCurrency != connection.connection.Capital.BaseCurrency {
		return fmt.Errorf("%w: valuation issuer or connection binding mismatch", ErrDenied)
	}
	for _, price := range value.Prices {
		if !contains(connection.connection.Governance.Assets, price.Asset) {
			return fmt.Errorf("%w: valuation includes an unauthorized asset", ErrDenied)
		}
	}
	return store.persistValuation(ctx, value)
}

func (store *Store) persistValuation(ctx context.Context, value ValuationSnapshot) error {
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if value.ObservedAt.After(now.Add(5*time.Minute)) || !value.ExpiresAt.After(now) {
		return ErrStaleValuation
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return err
	}
	hash := digest(canonical)
	sealed, err := store.vault.SealRecord(store.valuationAD(
		value.OrganizationID, value.ConnectionID, value.ConnectionVersion,
		value.ID, value.Version,
	), canonical)
	if err != nil {
		return fmt.Errorf("financial adapter: seal valuation: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("financial adapter: begin valuation registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(value.OrganizationID, value.ConnectionID)+"|valuation"); err != nil {
		return err
	}
	var priorID, priorHash string
	var priorVersion uint64
	var priorObserved time.Time
	err = tx.QueryRow(ctx, `
		SELECT valuation_id,version,canonical_hash,observed_at
		FROM workforce_financial_valuation_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND connection_version=$4
		FOR UPDATE
	`, store.tenantID, value.OrganizationID, value.ConnectionID, value.ConnectionVersion).Scan(
		&priorID, &priorVersion, &priorHash, &priorObserved,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if value.Version != 1 {
			return ErrConflict
		}
	} else if err != nil {
		return fmt.Errorf("financial adapter: load valuation head: %w", err)
	} else {
		if priorID == value.ID && priorVersion == value.Version {
			if priorHash != hash {
				return ErrConflict
			}
			return tx.Commit(ctx)
		}
		if value.Version != priorVersion+1 || value.ObservedAt.Before(priorObserved) {
			return ErrConflict
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_valuations (
			tenant_id,organization_id,connection_id,connection_version,valuation_id,
			version,base_currency,canonical_hash,sealed_record,observed_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
	`, store.tenantID, value.OrganizationID, value.ConnectionID, value.ConnectionVersion,
		value.ID, value.Version, value.BaseCurrency, hash, sealed,
		value.ObservedAt, value.ExpiresAt, now); err != nil {
		return fmt.Errorf("financial adapter: persist valuation: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_valuation_heads (
			tenant_id,organization_id,connection_id,connection_version,valuation_id,
			version,canonical_hash,observed_at,expires_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT (tenant_id,organization_id,connection_id,connection_version)
		DO UPDATE SET valuation_id=EXCLUDED.valuation_id,version=EXCLUDED.version,
			canonical_hash=EXCLUDED.canonical_hash,observed_at=EXCLUDED.observed_at,
			expires_at=EXCLUDED.expires_at,updated_at=EXCLUDED.updated_at
	`, store.tenantID, value.OrganizationID, value.ConnectionID, value.ConnectionVersion,
		value.ID, value.Version, hash, value.ObservedAt, value.ExpiresAt, now); err != nil {
		return fmt.Errorf("financial adapter: advance valuation head: %w", err)
	}
	return tx.Commit(ctx)
}

func (store *Store) RegisterRiskSnapshot(ctx context.Context, value RiskSnapshot) error {
	connection, err := store.loadConnectionVersion(ctx, value.OrganizationID, value.ConnectionID, value.ConnectionVersion)
	if err != nil {
		return err
	}
	key, ok := store.issuerKeys[value.Signature.KeyID]
	if !ok || !contains(connection.connection.Authority.RiskStateIssuerKeyIDs, value.Signature.KeyID) ||
		VerifyRiskSnapshot(value, value.Signature.KeyID, key) != nil ||
		value.AccountID != connection.connection.AccountID || value.IdentityID != connection.connection.IdentityID ||
		value.State.BaseCurrency != connection.connection.Capital.BaseCurrency {
		return fmt.Errorf("%w: risk-state issuer or account binding mismatch", ErrDenied)
	}
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return err
	}
	return store.persistRisk(ctx, value.OrganizationID, value.ConnectionID, value.ConnectionVersion,
		value.ID, value.Version, "signed_snapshot", value.SourceRef, value.State,
		digest(canonical), canonical, value.ObservedAt, value.ExpiresAt)
}

func (store *Store) ListActiveConnections(ctx context.Context, organizationID contracts.OrganizationID) ([]ConnectionView, error) {
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT connection_id,version,canonical_hash
		FROM workforce_financial_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND state='active'
		  AND effective_at <= $3 AND expires_at > $3
		ORDER BY connection_id
	`, store.tenantID, organizationID, now)
	if err != nil {
		return nil, fmt.Errorf("financial adapter: list active connections: %w", err)
	}
	defer rows.Close()
	views := make([]ConnectionView, 0)
	for rows.Next() {
		var id, hash string
		var version uint64
		if err := rows.Scan(&id, &version, &hash); err != nil {
			return nil, fmt.Errorf("financial adapter: scan active connection: %w", err)
		}
		loaded, err := store.loadActive(ctx, organizationID, id, version,
			contracts.ContentHash{Algorithm: "sha256", Digest: hash})
		if err != nil {
			return nil, err
		}
		views = append(views, ConnectionView{Connection: loaded.connection, Hash: loaded.hash})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("financial adapter: iterate active connections: %w", err)
	}
	return views, nil
}

func (store *Store) loadActive(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	hash contracts.ContentHash,
) (loadedConnection, error) {
	var headVersion, externalVersion uint64
	var headHash, state, externalState, externalHash string
	var effectiveAt, expiresAt, externalEffective, externalExpires time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT head.version,head.canonical_hash,head.state,head.effective_at,head.expires_at,
		       external_head.version,external_head.canonical_hash,external_head.state,
		       external_head.effective_at,external_head.expires_at
		FROM workforce_financial_connection_heads head
		JOIN workforce_financial_connections record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.connection_id=head.connection_id AND record.version=head.version
		JOIN workforce_external_connection_heads external_head
		  ON external_head.tenant_id=record.tenant_id
		 AND external_head.organization_id=record.organization_id
		 AND external_head.connection_id=record.external_connection_id
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.connection_id=$3
	`, store.tenantID, organizationID, connectionID).Scan(
		&headVersion, &headHash, &state, &effectiveAt, &expiresAt,
		&externalVersion, &externalHash, &externalState, &externalEffective, &externalExpires,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedConnection{}, fmt.Errorf("%w: connection is unavailable", ErrDenied)
	}
	if err != nil {
		return loadedConnection{}, fmt.Errorf("financial adapter: load connection head: %w", err)
	}
	now, err := store.currentTime()
	if err != nil {
		return loadedConnection{}, err
	}
	if version != 0 && version != headVersion || hash.Digest != "" &&
		(hash.Algorithm != "sha256" || hash.Digest != headHash) || state != "active" ||
		externalState != "active" || effectiveAt.After(now) || !expiresAt.After(now) ||
		externalEffective.After(now) || !externalExpires.After(now) {
		return loadedConnection{}, fmt.Errorf("%w: connection is not current and active", ErrDenied)
	}
	loaded, err := store.loadConnectionVersion(ctx, organizationID, connectionID, headVersion)
	if err != nil {
		return loadedConnection{}, err
	}
	if loaded.hash.Digest != headHash || loaded.connection.ExternalConnectionVersion != externalVersion ||
		loaded.connection.ExternalConnectionHash.Digest != externalHash {
		return loadedConnection{}, ErrIntegrity
	}
	return loaded, nil
}

func (store *Store) loadConnectionVersion(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
) (loadedConnection, error) {
	if token("organization id", string(organizationID)) != nil ||
		token("connection id", connectionID) != nil || version == 0 {
		return loadedConnection{}, fmt.Errorf("financial adapter: connection identity is invalid")
	}
	var hash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_financial_connections
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND version=$4
	`, store.tenantID, organizationID, connectionID, version).Scan(&hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedConnection{}, fmt.Errorf("%w: connection version is unavailable", ErrDenied)
	}
	if err != nil {
		return loadedConnection{}, fmt.Errorf("financial adapter: load connection version: %w", err)
	}
	opened, err := store.vault.OpenRecord(store.connectionAD(organizationID, connectionID, version), sealed)
	if err != nil {
		return loadedConnection{}, fmt.Errorf("%w: open connection", ErrIntegrity)
	}
	connection, err := contracts.DecodeCanonical[Connection, *Connection](opened)
	if err != nil || VerifyConnection(connection, store.founderKey, store.founderPub) != nil ||
		connection.OrganizationID != organizationID || connection.ID != connectionID ||
		connection.Version != version || digest(opened) != hash {
		return loadedConnection{}, ErrIntegrity
	}
	return loadedConnection{connection: connection,
		hash: contracts.ContentHash{Algorithm: "sha256", Digest: hash}}, nil
}

func (store *Store) loadValuation(
	ctx context.Context,
	connection loadedConnection,
	id string,
	version uint64,
	hash contracts.ContentHash,
	current bool,
) (loadedValuation, error) {
	if current {
		var headID, headHash string
		var headVersion uint64
		var observedAt, expiresAt time.Time
		err := store.pool.QueryRow(ctx, `
			SELECT valuation_id,version,canonical_hash,observed_at,expires_at
			FROM workforce_financial_valuation_heads
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND connection_version=$4
		`, store.tenantID, connection.connection.OrganizationID, connection.connection.ID,
			connection.connection.Version).Scan(&headID, &headVersion, &headHash, &observedAt, &expiresAt)
		if err != nil {
			return loadedValuation{}, ErrStaleValuation
		}
		now, err := store.currentTime()
		if err != nil {
			return loadedValuation{}, err
		}
		if id != headID || version != headVersion || hash.Algorithm != "sha256" || hash.Digest != headHash ||
			!expiresAt.After(now) || now.Sub(observedAt) > connection.connection.Capital.MaxValuationAge {
			return loadedValuation{}, ErrStaleValuation
		}
	}
	var storedHash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record FROM workforce_financial_valuations
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND valuation_id=$5 AND version=$6
	`, store.tenantID, connection.connection.OrganizationID, connection.connection.ID,
		connection.connection.Version, id, version).Scan(&storedHash, &sealed)
	if err != nil || storedHash != hash.Digest {
		return loadedValuation{}, ErrStaleValuation
	}
	opened, err := store.vault.OpenRecord(store.valuationAD(
		connection.connection.OrganizationID, connection.connection.ID,
		connection.connection.Version, id, version,
	), sealed)
	if err != nil {
		return loadedValuation{}, ErrIntegrity
	}
	value, err := contracts.DecodeCanonical[ValuationSnapshot, *ValuationSnapshot](opened)
	key, ok := store.issuerKeys[value.Signature.KeyID]
	if err != nil || !ok || !contains(connection.connection.Authority.ValuationIssuerKeyIDs, value.Signature.KeyID) ||
		VerifyValuationSnapshot(value, value.Signature.KeyID, key) != nil || digest(opened) != storedHash {
		return loadedValuation{}, ErrIntegrity
	}
	return loadedValuation{valuation: value,
		hash: contracts.ContentHash{Algorithm: "sha256", Digest: storedHash}}, nil
}

func (store *Store) loadRisk(
	ctx context.Context,
	connection loadedConnection,
	id string,
	version uint64,
	hash contracts.ContentHash,
	current bool,
) (loadedRisk, error) {
	if current {
		var headID, headHash string
		var headVersion uint64
		var observedAt, expiresAt time.Time
		err := store.pool.QueryRow(ctx, `
			SELECT snapshot_id,version,canonical_hash,observed_at,expires_at
			FROM workforce_financial_risk_heads
			WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND connection_version=$4
		`, store.tenantID, connection.connection.OrganizationID, connection.connection.ID,
			connection.connection.Version).Scan(&headID, &headVersion, &headHash, &observedAt, &expiresAt)
		if err != nil {
			return loadedRisk{}, ErrStaleRisk
		}
		now, err := store.currentTime()
		if err != nil {
			return loadedRisk{}, err
		}
		if id != headID || version != headVersion || hash.Algorithm != "sha256" || hash.Digest != headHash ||
			!expiresAt.After(now) || now.Sub(observedAt) > connection.connection.Capital.MaxRiskStateAge {
			return loadedRisk{}, ErrStaleRisk
		}
	}
	var sourceKind, sourceID, storedHash string
	var sealed []byte
	var observedAt, expiresAt time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT source_kind,source_id,canonical_hash,sealed_record,observed_at,expires_at
		FROM workforce_financial_risk_snapshots
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND connection_version=$4 AND snapshot_id=$5 AND version=$6
	`, store.tenantID, connection.connection.OrganizationID, connection.connection.ID,
		connection.connection.Version, id, version).Scan(
		&sourceKind, &sourceID, &storedHash, &sealed, &observedAt, &expiresAt,
	)
	if err != nil || storedHash != hash.Digest {
		return loadedRisk{}, ErrStaleRisk
	}
	opened, err := store.vault.OpenRecord(store.riskAD(
		connection.connection.OrganizationID, connection.connection.ID,
		connection.connection.Version, id, version,
	), sealed)
	if err != nil {
		return loadedRisk{}, ErrIntegrity
	}
	var state RiskState
	if sourceKind == "signed_snapshot" {
		value, decodeErr := contracts.DecodeCanonical[RiskSnapshot, *RiskSnapshot](opened)
		key, ok := store.issuerKeys[value.Signature.KeyID]
		if decodeErr != nil || !ok || !contains(connection.connection.Authority.RiskStateIssuerKeyIDs, value.Signature.KeyID) ||
			VerifyRiskSnapshot(value, value.Signature.KeyID, key) != nil ||
			value.ID != id || value.Version != version || digest(opened) != storedHash {
			return loadedRisk{}, ErrIntegrity
		}
		state = value.State
	} else if sourceKind == "provider_observation" {
		value, decodeErr := contracts.DecodeCanonical[RiskState, *RiskState](opened)
		if decodeErr != nil || digest(opened) != storedHash {
			return loadedRisk{}, ErrIntegrity
		}
		state = value
	} else {
		return loadedRisk{}, ErrIntegrity
	}
	return loadedRisk{id: id, version: version, state: state,
		hash:       contracts.ContentHash{Algorithm: "sha256", Digest: storedHash},
		observedAt: observedAt.UTC(), expiresAt: expiresAt.UTC(),
		sourceKind: sourceKind, sourceID: sourceID}, nil
}

func (store *Store) persistRisk(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	connectionVersion uint64,
	id string,
	version uint64,
	sourceKind string,
	sourceID string,
	state RiskState,
	hash string,
	canonical []byte,
	observedAt time.Time,
	expiresAt time.Time,
) error {
	if state.Validate() != nil || (sourceKind != "signed_snapshot" && sourceKind != "provider_observation") ||
		token("risk source id", sourceID) != nil || !validUTC(observedAt) || !validUTC(expiresAt) ||
		!expiresAt.After(observedAt) {
		return fmt.Errorf("financial adapter: persisted risk state is invalid")
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if observedAt.After(now.Add(5*time.Minute)) || !expiresAt.After(now) {
		return ErrStaleRisk
	}
	sealed, err := store.vault.SealRecord(store.riskAD(
		organizationID, connectionID, connectionVersion, id, version,
	), canonical)
	if err != nil {
		return fmt.Errorf("financial adapter: seal risk state: %w", err)
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("financial adapter: begin risk registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lockKey(organizationID, connectionID)+"|risk"); err != nil {
		return err
	}
	var priorID, priorHash string
	var priorVersion uint64
	var priorObserved time.Time
	err = tx.QueryRow(ctx, `
		SELECT snapshot_id,version,canonical_hash,observed_at
		FROM workforce_financial_risk_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND connection_version=$4
		FOR UPDATE
	`, store.tenantID, organizationID, connectionID, connectionVersion).Scan(
		&priorID, &priorVersion, &priorHash, &priorObserved,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		if version != 1 {
			return ErrConflict
		}
	} else if err != nil {
		return fmt.Errorf("financial adapter: load risk head: %w", err)
	} else {
		if priorID == id && priorVersion == version {
			if priorHash != hash {
				return ErrConflict
			}
			return tx.Commit(ctx)
		}
		if version != priorVersion+1 || observedAt.Before(priorObserved) {
			return ErrConflict
		}
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_risk_snapshots (
			tenant_id,organization_id,connection_id,connection_version,snapshot_id,
			version,source_kind,source_id,canonical_hash,sealed_record,resource_version,
			total_capital_microunits,available_liquidity_microunits,
			gross_exposure_microunits,drawdown_microunits,runway_microunits,
			observed_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
	`, store.tenantID, organizationID, connectionID, connectionVersion, id, version,
		sourceKind, sourceID, hash, sealed, state.ResourceVersion,
		state.TotalCapitalMicrounits, state.AvailableLiquidityMicrounits,
		state.GrossExposureMicrounits, state.DrawdownMicrounits, state.RunwayMicrounits,
		observedAt, expiresAt, now); err != nil {
		return fmt.Errorf("financial adapter: persist risk state: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_financial_risk_heads (
			tenant_id,organization_id,connection_id,connection_version,snapshot_id,
			version,canonical_hash,resource_version,observed_at,expires_at,updated_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)
		ON CONFLICT (tenant_id,organization_id,connection_id,connection_version)
		DO UPDATE SET snapshot_id=EXCLUDED.snapshot_id,version=EXCLUDED.version,
			canonical_hash=EXCLUDED.canonical_hash,resource_version=EXCLUDED.resource_version,
			observed_at=EXCLUDED.observed_at,expires_at=EXCLUDED.expires_at,
			updated_at=EXCLUDED.updated_at
	`, store.tenantID, organizationID, connectionID, connectionVersion, id, version,
		hash, state.ResourceVersion, observedAt, expiresAt, now); err != nil {
		return fmt.Errorf("financial adapter: advance risk head: %w", err)
	}
	return tx.Commit(ctx)
}

func (store *Store) verifyExternalBinding(ctx context.Context, connection Connection, now time.Time) error {
	var adapterName, family, accountID, identityID, hash, state string
	var effectiveAt, expiresAt time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT record.adapter_name,record.family,record.account_id,record.identity_id,
		       record.canonical_hash,head.state,head.effective_at,head.expires_at
		FROM workforce_external_connections record
		JOIN workforce_external_connection_heads head
		  ON head.tenant_id=record.tenant_id AND head.organization_id=record.organization_id
		 AND head.connection_id=record.connection_id AND head.version=record.version
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND record.connection_id=$3 AND record.version=$4
	`, store.tenantID, connection.OrganizationID, connection.ExternalConnectionID,
		connection.ExternalConnectionVersion).Scan(
		&adapterName, &family, &accountID, &identityID, &hash, &state, &effectiveAt, &expiresAt,
	)
	if err != nil || adapterName != connection.ExternalAdapterName || family != "financial_transport" ||
		accountID != connection.AccountID || identityID != connection.IdentityID ||
		hash != connection.ExternalConnectionHash.Digest || state != "active" ||
		effectiveAt.After(now) || !expiresAt.After(now) {
		return fmt.Errorf("%w: external financial transport binding is not exact and active", ErrDenied)
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("financial adapter: time source must return UTC")
	}
	return now, nil
}

func (store *Store) connectionAD(organizationID contracts.OrganizationID, connectionID string, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.financial.connection",
		Stream: string(organizationID) + "/" + connectionID + "/" + fmt.Sprint(version), Schema: SchemaVersion}
}

func (store *Store) revocationAD(organizationID contracts.OrganizationID, id string) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.financial.revocation",
		Stream: string(organizationID) + "/" + id, Schema: SchemaVersion}
}

func (store *Store) valuationAD(organizationID contracts.OrganizationID, connectionID string, connectionVersion uint64, id string, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.financial.valuation",
		Stream: fmt.Sprintf("%s/%s/%d/%s/%d", organizationID, connectionID, connectionVersion, id, version), Schema: SchemaVersion}
}

func (store *Store) riskAD(organizationID contracts.OrganizationID, connectionID string, connectionVersion uint64, id string, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.financial.risk",
		Stream: fmt.Sprintf("%s/%s/%d/%s/%d", organizationID, connectionID, connectionVersion, id, version), Schema: SchemaVersion}
}

func (store *Store) observationAD(organizationID contracts.OrganizationID, id string) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.financial.observation",
		Stream: string(organizationID) + "/" + id, Schema: SchemaVersion}
}

func (store *Store) founderReservationAD(organizationID contracts.OrganizationID, id string) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.financial.founder_reservation",
		Stream: string(organizationID) + "/" + id, Schema: SchemaVersion}
}

func (store *Store) lockKey(organizationID contracts.OrganizationID, suffix string) string {
	return store.tenantID + "|" + string(organizationID) + "|financial|" + suffix
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func randomID(prefix string) (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return prefix + hex.EncodeToString(buffer), nil
}

func equalCanonical(left, right []byte) bool { return bytes.Equal(left, right) }
