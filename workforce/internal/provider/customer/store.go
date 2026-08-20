package customer

import (
	"bytes"
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
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

type Store struct {
	pool       *pgxpool.Pool
	vault      *vault.UserVault
	tenantID   string
	founderKey string
	publicKey  ed25519.PublicKey
	issuerKeys map[string]ed25519.PublicKey
	now        func() time.Time
}

type loadedConnection struct {
	connection Connection
	hash       contracts.ContentHash
}

type loadedCustomer struct {
	customer CustomerScope
	hash     contracts.ContentHash
}

type loadedConsent struct {
	consent ConsentRecord
	hash    contracts.ContentHash
}

func NewStore(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	founderKeyID string,
	founderPublicKey ed25519.PublicKey,
	issuerPublicKeys map[string]ed25519.PublicKey,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || userVault == nil || tenantID == "" ||
		token("founder key id", founderKeyID) != nil ||
		len(founderPublicKey) != ed25519.PublicKeySize || now == nil {
		return nil, fmt.Errorf("customer adapter: store dependencies and founder authority are required")
	}
	if userVault.User() != tenantID {
		return nil, fmt.Errorf("customer adapter: Vault user does not match tenant")
	}
	keys := make(map[string]ed25519.PublicKey, len(issuerPublicKeys)+1)
	keys[founderKeyID] = append(ed25519.PublicKey(nil), founderPublicKey...)
	for keyID, publicKey := range issuerPublicKeys {
		if token("issuer key id", keyID) != nil || len(publicKey) != ed25519.PublicKeySize {
			return nil, fmt.Errorf("customer adapter: issuer keyring is invalid")
		}
		keys[keyID] = append(ed25519.PublicKey(nil), publicKey...)
	}
	return &Store{
		pool: pool, vault: userVault, tenantID: tenantID,
		founderKey: founderKeyID,
		publicKey:  append(ed25519.PublicKey(nil), founderPublicKey...),
		issuerKeys: keys, now: now,
	}, nil
}

func (store *Store) RegisterConnection(ctx context.Context, connection Connection) error {
	if err := VerifyConnection(connection, store.founderKey, store.publicKey); err != nil {
		return err
	}
	for _, keyID := range append(
		append([]string(nil), connection.Authority.CustomerIssuerKeyIDs...),
		connection.Authority.ConsentIssuerKeyIDs...,
	) {
		if _, ok := store.issuerKeys[keyID]; !ok {
			return fmt.Errorf("%w: connection references an unavailable issuer key", ErrDenied)
		}
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if !connection.ExpiresAt.After(now) {
		return fmt.Errorf("%w: connection authority is expired", ErrDenied)
	}
	canonical, hash, sealed, err := store.seal(
		store.connectionAD(connection.OrganizationID, connection.ID, connection.Version),
		&connection,
	)
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("customer adapter: begin connection registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lock(connection.OrganizationID, "connection", connection.ID)); err != nil {
		return fmt.Errorf("customer adapter: lock connection registration: %w", err)
	}
	var externalState, externalAdapter, externalAccount, externalIdentity string
	var externalEffective, externalExpires time.Time
	err = tx.QueryRow(ctx, `
		SELECT head.state,record.adapter_name,record.account_id,record.identity_id,
		       head.effective_at,head.expires_at
		FROM workforce_external_connections record
		JOIN workforce_external_connection_heads head
		  ON head.tenant_id=record.tenant_id
		 AND head.organization_id=record.organization_id
		 AND head.connection_id=record.connection_id
		 AND head.version=record.version
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND record.connection_id=$3 AND record.version=$4
		  AND record.canonical_hash=$5
		FOR SHARE OF record,head
	`, store.tenantID, connection.OrganizationID, connection.ExternalConnectionID,
		connection.ExternalConnectionVersion, connection.ExternalConnectionHash.Digest).Scan(
		&externalState, &externalAdapter, &externalAccount, &externalIdentity,
		&externalEffective, &externalExpires,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("%w: exact external connection is unavailable", ErrDenied)
	}
	if err != nil {
		return fmt.Errorf("customer adapter: load external connection: %w", err)
	}
	if externalState != "active" && externalState != "scheduled" ||
		externalAdapter != connection.ExternalAdapterName ||
		externalAccount != connection.AccountID || externalIdentity != connection.IdentityID ||
		connection.EffectiveAt.Before(externalEffective.UTC()) ||
		connection.ExpiresAt.After(externalExpires.UTC()) {
		return fmt.Errorf("%w: external connection authority does not cover customer connection", ErrDenied)
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_customer_connections (
			tenant_id,organization_id,connection_id,version,adapter_name,
			external_adapter_name,external_connection_id,external_connection_version,
			family,account_id,identity_id,canonical_hash,sealed_record,
			effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16)
		ON CONFLICT DO NOTHING
	`, store.tenantID, connection.OrganizationID, connection.ID, connection.Version,
		connection.AdapterName, connection.ExternalAdapterName,
		connection.ExternalConnectionID, connection.ExternalConnectionVersion,
		connection.Family, connection.AccountID, connection.IdentityID, hash.Digest,
		sealed, connection.EffectiveAt, connection.ExpiresAt, now)
	if err != nil {
		return fmt.Errorf("customer adapter: persist connection: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		var existingSealed []byte
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash,sealed_record
			FROM workforce_customer_connections
			WHERE tenant_id=$1 AND organization_id=$2
			  AND connection_id=$3 AND version=$4
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version).Scan(&existingHash, &existingSealed); err != nil ||
			existingHash != hash.Digest || !store.matches(
			store.connectionAD(connection.OrganizationID, connection.ID, connection.Version),
			existingSealed, canonical,
		) {
			return ErrConflict
		}
	}
	state := "active"
	var currentVersion uint64
	var currentHash, currentState string
	err = tx.QueryRow(ctx, `
		SELECT version,canonical_hash,state
		FROM workforce_customer_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		FOR UPDATE
	`, store.tenantID, connection.OrganizationID, connection.ID).Scan(
		&currentVersion, &currentHash, &currentState,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_customer_connection_heads (
				tenant_id,organization_id,connection_id,version,canonical_hash,state,
				effective_at,expires_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		`, store.tenantID, connection.OrganizationID, connection.ID,
			connection.Version, hash.Digest, state,
			connection.EffectiveAt, connection.ExpiresAt, now)
	case err != nil:
		return fmt.Errorf("customer adapter: load connection head: %w", err)
	case currentVersion > connection.Version:
		return ErrConflict
	case currentVersion == connection.Version:
		if currentHash != hash.Digest || currentState == "revoked" {
			return ErrConflict
		}
	case currentVersion < connection.Version:
		_, err = tx.Exec(ctx, `
			UPDATE workforce_customer_connection_heads
			SET version=$1,canonical_hash=$2,state=$3,effective_at=$4,
			    expires_at=$5,revoked_at=NULL,updated_at=$6
			WHERE tenant_id=$7 AND organization_id=$8 AND connection_id=$9
		`, connection.Version, hash.Digest, state, connection.EffectiveAt,
			connection.ExpiresAt, now, store.tenantID,
			connection.OrganizationID, connection.ID)
	}
	if err != nil {
		return fmt.Errorf("customer adapter: persist connection head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("customer adapter: commit connection registration: %w", err)
	}
	return nil
}

func (store *Store) RevokeConnection(ctx context.Context, revocation ConnectionRevocation) error {
	if err := VerifyConnectionRevocation(revocation, store.founderKey, store.publicKey); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	if revocation.RevokedAt.After(now) {
		return fmt.Errorf("customer adapter: revocation cannot be future-dated")
	}
	canonical, hash, sealed, err := store.seal(
		store.revocationAD(revocation.OrganizationID, revocation.ID), &revocation,
	)
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("customer adapter: begin revocation: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lock(revocation.OrganizationID, "connection", revocation.ConnectionID)); err != nil {
		return err
	}
	var headVersion uint64
	var headState string
	if err := tx.QueryRow(ctx, `
		SELECT version,state FROM workforce_customer_connection_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		FOR UPDATE
	`, store.tenantID, revocation.OrganizationID, revocation.ConnectionID).Scan(
		&headVersion, &headState,
	); err != nil {
		return fmt.Errorf("customer adapter: load connection head for revocation: %w", err)
	}
	if headVersion != revocation.Version {
		return ErrConflict
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_customer_connection_revocations (
			tenant_id,organization_id,revocation_id,connection_id,connection_version,
			reason_code,canonical_hash,sealed_record,revoked_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10)
		ON CONFLICT DO NOTHING
	`, store.tenantID, revocation.OrganizationID, revocation.ID,
		revocation.ConnectionID, revocation.Version, revocation.ReasonCode,
		hash.Digest, sealed, revocation.RevokedAt, now)
	if err != nil {
		return fmt.Errorf("customer adapter: persist revocation: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		var existingSealed []byte
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash,sealed_record
			FROM workforce_customer_connection_revocations
			WHERE tenant_id=$1 AND organization_id=$2 AND revocation_id=$3
		`, store.tenantID, revocation.OrganizationID, revocation.ID).Scan(
			&existingHash, &existingSealed,
		); err != nil || existingHash != hash.Digest || !store.matches(
			store.revocationAD(revocation.OrganizationID, revocation.ID), existingSealed, canonical,
		) {
			return ErrConflict
		}
	}
	if headState != "revoked" {
		if _, err := tx.Exec(ctx, `
			UPDATE workforce_customer_connection_heads
			SET state='revoked',revoked_at=$1,updated_at=$2
			WHERE tenant_id=$3 AND organization_id=$4 AND connection_id=$5 AND version=$6
		`, revocation.RevokedAt, now, store.tenantID,
			revocation.OrganizationID, revocation.ConnectionID, revocation.Version); err != nil {
			return fmt.Errorf("customer adapter: revoke connection head: %w", err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("customer adapter: commit revocation: %w", err)
	}
	return nil
}

func (store *Store) RegisterCustomer(ctx context.Context, customer CustomerScope) error {
	connection, err := store.loadConnectionVersion(ctx, customer.OrganizationID,
		customer.ConnectionID, customer.ConnectionVersion)
	if err != nil {
		return err
	}
	key, ok := store.issuerKeys[customer.Signature.KeyID]
	if !ok || !contains(connection.connection.Authority.CustomerIssuerKeyIDs, customer.Signature.KeyID) {
		return fmt.Errorf("%w: customer issuer is not authorized", ErrDenied)
	}
	if err := VerifyCustomerScope(customer, customer.Signature.KeyID, key); err != nil {
		return err
	}
	if err := validateCustomerGovernance(connection.connection, customer); err != nil {
		return err
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	canonical, hash, sealed, err := store.seal(
		store.customerAD(customer.OrganizationID, customer.ID, customer.Version), &customer,
	)
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("customer adapter: begin customer registration: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lock(customer.OrganizationID, "customer", customer.ID)); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_customer_scopes (
			tenant_id,organization_id,customer_id,version,connection_id,
			connection_version,recipient_hash,state,canonical_hash,sealed_record,
			effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT DO NOTHING
	`, store.tenantID, customer.OrganizationID, customer.ID, customer.Version,
		customer.ConnectionID, customer.ConnectionVersion, customer.DestinationHash.Digest,
		customer.State, hash.Digest, sealed, customer.EffectiveAt, customer.ExpiresAt, now)
	if err != nil {
		return fmt.Errorf("customer adapter: persist customer scope: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		var existingSealed []byte
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash,sealed_record FROM workforce_customer_scopes
			WHERE tenant_id=$1 AND organization_id=$2 AND customer_id=$3 AND version=$4
		`, store.tenantID, customer.OrganizationID, customer.ID, customer.Version).Scan(
			&existingHash, &existingSealed,
		); err != nil || existingHash != hash.Digest || !store.matches(
			store.customerAD(customer.OrganizationID, customer.ID, customer.Version),
			existingSealed, canonical,
		) {
			return ErrConflict
		}
	}
	var currentVersion uint64
	var currentHash string
	err = tx.QueryRow(ctx, `
		SELECT version,canonical_hash FROM workforce_customer_scope_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND customer_id=$3 FOR UPDATE
	`, store.tenantID, customer.OrganizationID, customer.ID).Scan(&currentVersion, &currentHash)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_customer_scope_heads (
				tenant_id,organization_id,customer_id,version,canonical_hash,
				connection_id,connection_version,recipient_hash,state,
				effective_at,expires_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
		`, store.tenantID, customer.OrganizationID, customer.ID, customer.Version,
			hash.Digest, customer.ConnectionID, customer.ConnectionVersion,
			customer.DestinationHash.Digest, customer.State,
			customer.EffectiveAt, customer.ExpiresAt, now)
	case err != nil:
		return fmt.Errorf("customer adapter: load customer head: %w", err)
	case currentVersion > customer.Version:
		return ErrConflict
	case currentVersion == customer.Version:
		if currentHash != hash.Digest {
			return ErrConflict
		}
	case currentVersion < customer.Version:
		_, err = tx.Exec(ctx, `
			UPDATE workforce_customer_scope_heads
			SET version=$1,canonical_hash=$2,connection_id=$3,connection_version=$4,
			    recipient_hash=$5,state=$6,effective_at=$7,expires_at=$8,updated_at=$9
			WHERE tenant_id=$10 AND organization_id=$11 AND customer_id=$12
		`, customer.Version, hash.Digest, customer.ConnectionID,
			customer.ConnectionVersion, customer.DestinationHash.Digest,
			customer.State, customer.EffectiveAt, customer.ExpiresAt, now,
			store.tenantID, customer.OrganizationID, customer.ID)
	}
	if err != nil {
		return fmt.Errorf("customer adapter: persist customer head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("customer adapter: commit customer registration: %w", err)
	}
	return nil
}

func (store *Store) RecordConsent(ctx context.Context, consent ConsentRecord) error {
	connection, err := store.loadConnectionVersion(ctx, consent.OrganizationID,
		consent.ConnectionID, consent.ConnectionVersion)
	if err != nil {
		return err
	}
	key, ok := store.issuerKeys[consent.Signature.KeyID]
	if !ok || !contains(connection.connection.Authority.ConsentIssuerKeyIDs, consent.Signature.KeyID) {
		return fmt.Errorf("%w: consent issuer is not authorized", ErrDenied)
	}
	if err := VerifyConsentRecord(consent, consent.Signature.KeyID, key); err != nil {
		return err
	}
	customer, err := store.loadCustomerVersion(ctx, consent.OrganizationID,
		consent.CustomerID, consent.CustomerVersion)
	if err != nil {
		return err
	}
	if consent.State == ConsentGranted {
		activeConnection, err := store.loadActive(
			ctx, consent.OrganizationID, consent.ConnectionID,
			consent.ConnectionVersion, connection.hash,
		)
		if err != nil || activeConnection.hash != connection.hash {
			return fmt.Errorf("%w: consent grant requires the active exact connection", ErrDenied)
		}
		currentCustomer, err := store.loadCustomerCurrent(
			ctx, consent.OrganizationID, consent.CustomerID,
		)
		if err != nil || currentCustomer.hash != customer.hash {
			return fmt.Errorf("%w: consent grant requires the active exact customer scope", ErrDenied)
		}
	}
	if customer.customer.ConnectionID != consent.ConnectionID ||
		customer.customer.ConnectionVersion != consent.ConnectionVersion ||
		customer.customer.RecipientRef != consent.RecipientRef ||
		customer.customer.DestinationHash != consent.DestinationHash ||
		!contains(customer.customer.Channels, consent.Channel) ||
		!contains(customer.customer.Purposes, consent.Purpose) ||
		!contains(customer.customer.Jurisdictions, consent.Jurisdiction) ||
		!contains(connection.connection.Governance.PrivacyPolicyRefs, consent.PrivacyPolicyRef) ||
		!contains(connection.connection.Governance.Channels, consent.Channel) ||
		!contains(connection.connection.Governance.Purposes, consent.Purpose) ||
		!contains(connection.connection.Governance.Jurisdictions, consent.Jurisdiction) {
		return fmt.Errorf("%w: consent exceeds customer or connection scope", ErrDenied)
	}
	now, err := store.currentTime()
	if err != nil {
		return err
	}
	canonical, hash, sealed, err := store.seal(
		store.consentAD(consent.OrganizationID, consent.ID, consent.Version), &consent,
	)
	if err != nil {
		return err
	}
	tx, err := store.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("customer adapter: begin consent record: %w", err)
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1,0))`,
		store.lock(consent.OrganizationID, "consent", consent.ID)); err != nil {
		return err
	}
	command, err := tx.Exec(ctx, `
		INSERT INTO workforce_customer_consents (
			tenant_id,organization_id,consent_id,version,connection_id,
			connection_version,customer_id,customer_version,recipient_hash,
			channel,purpose,jurisdiction,basis,state,canonical_hash,sealed_record,
			effective_at,expires_at,created_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18,$19)
		ON CONFLICT DO NOTHING
	`, store.tenantID, consent.OrganizationID, consent.ID, consent.Version,
		consent.ConnectionID, consent.ConnectionVersion, consent.CustomerID,
		consent.CustomerVersion, consent.DestinationHash.Digest, consent.Channel,
		consent.Purpose, consent.Jurisdiction, consent.Basis, consent.State,
		hash.Digest, sealed, consent.EffectiveAt, consent.ExpiresAt, now)
	if err != nil {
		return fmt.Errorf("customer adapter: persist consent: %w", err)
	}
	if command.RowsAffected() == 0 {
		var existingHash string
		var existingSealed []byte
		if err := tx.QueryRow(ctx, `
			SELECT canonical_hash,sealed_record FROM workforce_customer_consents
			WHERE tenant_id=$1 AND organization_id=$2 AND consent_id=$3 AND version=$4
		`, store.tenantID, consent.OrganizationID, consent.ID, consent.Version).Scan(
			&existingHash, &existingSealed,
		); err != nil || existingHash != hash.Digest || !store.matches(
			store.consentAD(consent.OrganizationID, consent.ID, consent.Version),
			existingSealed, canonical,
		) {
			return ErrConflict
		}
	}
	var currentVersion uint64
	var currentHash string
	var currentState ConsentState
	err = tx.QueryRow(ctx, `
		SELECT version,canonical_hash,state FROM workforce_customer_consent_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND consent_id=$3 FOR UPDATE
	`, store.tenantID, consent.OrganizationID, consent.ID).Scan(
		&currentVersion, &currentHash, &currentState,
	)
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		_, err = tx.Exec(ctx, `
			INSERT INTO workforce_customer_consent_heads (
				tenant_id,organization_id,consent_id,version,canonical_hash,
				connection_id,connection_version,customer_id,customer_version,
				recipient_hash,channel,purpose,jurisdiction,basis,state,
				effective_at,expires_at,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)
		`, store.tenantID, consent.OrganizationID, consent.ID, consent.Version,
			hash.Digest, consent.ConnectionID, consent.ConnectionVersion,
			consent.CustomerID, consent.CustomerVersion, consent.DestinationHash.Digest,
			consent.Channel, consent.Purpose, consent.Jurisdiction, consent.Basis,
			consent.State, consent.EffectiveAt, consent.ExpiresAt, now)
	case err != nil:
		return fmt.Errorf("customer adapter: load consent head: %w", err)
	case currentVersion > consent.Version:
		return ErrConflict
	case currentVersion == consent.Version:
		if currentHash != hash.Digest {
			return ErrConflict
		}
	case currentVersion < consent.Version:
		if currentState == ConsentWithdrawn && consent.State == ConsentGranted {
			return fmt.Errorf("%w: withdrawn consent cannot be silently reactivated", ErrUnsubscribed)
		}
		_, err = tx.Exec(ctx, `
			UPDATE workforce_customer_consent_heads
			SET version=$1,canonical_hash=$2,connection_id=$3,connection_version=$4,
			    customer_id=$5,customer_version=$6,recipient_hash=$7,channel=$8,
			    purpose=$9,jurisdiction=$10,basis=$11,state=$12,
			    effective_at=$13,expires_at=$14,updated_at=$15
			WHERE tenant_id=$16 AND organization_id=$17 AND consent_id=$18
		`, consent.Version, hash.Digest, consent.ConnectionID, consent.ConnectionVersion,
			consent.CustomerID, consent.CustomerVersion, consent.DestinationHash.Digest,
			consent.Channel, consent.Purpose, consent.Jurisdiction, consent.Basis,
			consent.State, consent.EffectiveAt, consent.ExpiresAt, now,
			store.tenantID, consent.OrganizationID, consent.ID)
	}
	if err != nil {
		return fmt.Errorf("customer adapter: persist consent head: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("customer adapter: commit consent: %w", err)
	}
	return nil
}

func (store *Store) CurrentConnection(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
) (ConnectionView, error) {
	loaded, err := store.loadActive(ctx, organizationID, connectionID, 0, contracts.ContentHash{})
	if err != nil {
		return ConnectionView{}, err
	}
	return ConnectionView{Connection: loaded.connection, Hash: loaded.hash}, nil
}

func (store *Store) CurrentCustomer(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	customerID string,
) (CustomerView, error) {
	loaded, err := store.loadCustomerCurrent(ctx, organizationID, customerID)
	if err != nil {
		return CustomerView{}, err
	}
	return CustomerView{Customer: loaded.customer, Hash: loaded.hash}, nil
}

func (store *Store) CurrentConsent(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	consentID string,
) (ConsentView, error) {
	loaded, err := store.loadConsentCurrent(ctx, organizationID, consentID)
	if err != nil {
		return ConsentView{}, err
	}
	return ConsentView{Consent: loaded.consent, Hash: loaded.hash}, nil
}

func (store *Store) ListActiveConnections(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) ([]ConnectionView, error) {
	now, err := store.currentTime()
	if err != nil {
		return nil, err
	}
	rows, err := store.pool.Query(ctx, `
		SELECT record.connection_id,record.version,record.canonical_hash,record.sealed_record
		FROM workforce_customer_connections record
		JOIN workforce_customer_connection_heads head
		  ON head.tenant_id=record.tenant_id AND head.organization_id=record.organization_id
		 AND head.connection_id=record.connection_id AND head.version=record.version
		WHERE record.tenant_id=$1 AND record.organization_id=$2
		  AND head.state='active' AND head.effective_at<=$3 AND head.expires_at>$3
		ORDER BY record.adapter_name,record.connection_id
	`, store.tenantID, organizationID, now)
	if err != nil {
		return nil, fmt.Errorf("customer adapter: list connections: %w", err)
	}
	defer rows.Close()
	views := make([]ConnectionView, 0)
	for rows.Next() {
		var id, hash string
		var version uint64
		var sealed []byte
		if err := rows.Scan(&id, &version, &hash, &sealed); err != nil {
			return nil, err
		}
		connection, err := store.openConnection(organizationID, id, version, hash, sealed)
		if err != nil {
			return nil, err
		}
		views = append(views, ConnectionView{
			Connection: connection,
			Hash:       contracts.ContentHash{Algorithm: "sha256", Digest: hash},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return views, nil
}

func validateCustomerGovernance(connection Connection, customer CustomerScope) error {
	if customer.OrganizationID != connection.OrganizationID || customer.ConnectionID != connection.ID ||
		customer.ConnectionVersion != connection.Version || customer.EffectiveAt.Before(connection.EffectiveAt) ||
		customer.ExpiresAt.After(connection.ExpiresAt) {
		return fmt.Errorf("%w: customer scope is outside connection authority", ErrDenied)
	}
	for _, value := range customer.Channels {
		if !contains(connection.Governance.Channels, value) {
			return fmt.Errorf("%w: customer channel is outside connection authority", ErrDenied)
		}
	}
	for _, value := range customer.Audiences {
		if !contains(connection.Governance.Audiences, value) {
			return fmt.Errorf("%w: customer audience is outside connection authority", ErrDenied)
		}
	}
	for _, value := range customer.Purposes {
		if !contains(connection.Governance.Purposes, value) {
			return fmt.Errorf("%w: customer purpose is outside connection authority", ErrDenied)
		}
	}
	for _, value := range customer.Jurisdictions {
		if !contains(connection.Governance.Jurisdictions, value) {
			return fmt.Errorf("%w: customer jurisdiction is outside connection authority", ErrDenied)
		}
	}
	for _, value := range customer.DataClassifications {
		if !containsClassification(connection.Governance.DataClassifications, value) {
			return fmt.Errorf("%w: customer data scope is outside connection authority", ErrDenied)
		}
	}
	for _, value := range customer.ContractRefs {
		if !contains(connection.Governance.ContractTemplateRefs, value) {
			return fmt.Errorf("%w: customer contract is outside connection authority", ErrDenied)
		}
	}
	for _, value := range customer.SupportQueues {
		if !contains(connection.Governance.SupportQueues, value) {
			return fmt.Errorf("%w: customer support queue is outside connection authority", ErrDenied)
		}
	}
	return nil
}

func (store *Store) currentTime() (time.Time, error) {
	if store == nil || store.now == nil {
		return time.Time{}, ErrUnavailable
	}
	now := store.now()
	if !validUTC(now) {
		return time.Time{}, fmt.Errorf("customer adapter: time source must return UTC")
	}
	return now, nil
}

func (store *Store) seal(ad vault.AD, value contracts.Validatable) ([]byte, contracts.ContentHash, []byte, error) {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return nil, contracts.ContentHash{}, nil, err
	}
	sum := sha256.Sum256(canonical)
	hash := contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
	sealed, err := store.vault.SealRecord(ad, canonical)
	if err != nil {
		return nil, contracts.ContentHash{}, nil, fmt.Errorf("customer adapter: seal record: %w", err)
	}
	return canonical, hash, sealed, nil
}

func (store *Store) matches(ad vault.AD, sealed, expected []byte) bool {
	opened, err := store.vault.OpenRecord(ad, sealed)
	return err == nil && bytes.Equal(opened, expected)
}

func (store *Store) lock(organizationID contracts.OrganizationID, kind, id string) string {
	return store.tenantID + "|" + string(organizationID) + "|customer|" + kind + "|" + id
}

func (store *Store) connectionAD(organizationID contracts.OrganizationID, id string, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.customer.connection",
		Stream: fmt.Sprintf("%s/%s/%d", organizationID, id, version), Schema: SchemaVersion}
}

func (store *Store) revocationAD(organizationID contracts.OrganizationID, id string) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.customer.revocation",
		Stream: string(organizationID) + "/" + id, Schema: SchemaVersion}
}

func (store *Store) customerAD(organizationID contracts.OrganizationID, id string, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.customer.scope",
		Stream: fmt.Sprintf("%s/%s/%d", organizationID, id, version), Schema: SchemaVersion}
}

func (store *Store) consentAD(organizationID contracts.OrganizationID, id string, version uint64) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.customer.consent",
		Stream: fmt.Sprintf("%s/%s/%d", organizationID, id, version), Schema: SchemaVersion}
}

func (store *Store) observationAD(organizationID contracts.OrganizationID, id string) vault.AD {
	return vault.AD{User: store.tenantID, Store: "workforce.customer.observation",
		Stream: string(organizationID) + "/" + id, Schema: SchemaVersion}
}
