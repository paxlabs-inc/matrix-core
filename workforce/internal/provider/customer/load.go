package customer

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"matrix/workforce/internal/contracts"
)

func (store *Store) loadActive(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	hash contracts.ContentHash,
) (loadedConnection, error) {
	if token("organization id", string(organizationID)) != nil ||
		token("connection id", connectionID) != nil {
		return loadedConnection{}, fmt.Errorf("customer adapter: connection identity is invalid")
	}
	var headVersion uint64
	var headHash, state, externalState string
	var effectiveAt, expiresAt, externalEffective, externalExpires time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT head.version,head.canonical_hash,head.state,head.effective_at,head.expires_at,
		       external_head.state,external_head.effective_at,external_head.expires_at
		FROM workforce_customer_connection_heads head
		JOIN workforce_customer_connections record
		  ON record.tenant_id=head.tenant_id AND record.organization_id=head.organization_id
		 AND record.connection_id=head.connection_id AND record.version=head.version
		JOIN workforce_external_connection_heads external_head
		  ON external_head.tenant_id=record.tenant_id
		 AND external_head.organization_id=record.organization_id
		 AND external_head.connection_id=record.external_connection_id
		 AND external_head.version=record.external_connection_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2 AND head.connection_id=$3
	`, store.tenantID, organizationID, connectionID).Scan(
		&headVersion, &headHash, &state, &effectiveAt, &expiresAt,
		&externalState, &externalEffective, &externalExpires,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedConnection{}, fmt.Errorf("%w: connection is unavailable", ErrDenied)
	}
	if err != nil {
		return loadedConnection{}, fmt.Errorf("customer adapter: load connection head: %w", err)
	}
	if version != 0 && headVersion != version || hash.Digest != "" &&
		(hash.Algorithm != "sha256" || headHash != hash.Digest) {
		return loadedConnection{}, fmt.Errorf("%w: connection is not the current exact version", ErrDenied)
	}
	now, err := store.currentTime()
	if err != nil {
		return loadedConnection{}, err
	}
	if state != "active" || externalState != "active" || effectiveAt.After(now) ||
		!expiresAt.After(now) || externalEffective.After(now) || !externalExpires.After(now) {
		return loadedConnection{}, fmt.Errorf("%w: connection is outside active validity", ErrDenied)
	}
	loaded, err := store.loadConnectionVersion(ctx, organizationID, connectionID, headVersion)
	if err != nil {
		return loadedConnection{}, err
	}
	if loaded.hash.Digest != headHash {
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
		return loadedConnection{}, fmt.Errorf("customer adapter: connection identity is invalid")
	}
	var hash string
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT canonical_hash,sealed_record
		FROM workforce_customer_connections
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3 AND version=$4
	`, store.tenantID, organizationID, connectionID, version).Scan(&hash, &sealed)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedConnection{}, fmt.Errorf("%w: connection version is unavailable", ErrDenied)
	}
	if err != nil {
		return loadedConnection{}, fmt.Errorf("customer adapter: load connection version: %w", err)
	}
	connection, err := store.openConnection(organizationID, connectionID, version, hash, sealed)
	if err != nil {
		return loadedConnection{}, err
	}
	return loadedConnection{
		connection: connection,
		hash:       contracts.ContentHash{Algorithm: "sha256", Digest: hash},
	}, nil
}

func (store *Store) openConnection(
	organizationID contracts.OrganizationID,
	connectionID string,
	version uint64,
	hash string,
	sealed []byte,
) (Connection, error) {
	opened, err := store.vault.OpenRecord(
		store.connectionAD(organizationID, connectionID, version), sealed,
	)
	if err != nil {
		return Connection{}, fmt.Errorf("%w: open connection", ErrIntegrity)
	}
	var connection Connection
	if err := decodeStrict(opened, &connection); err != nil ||
		VerifyConnection(connection, store.founderKey, store.publicKey) != nil ||
		connection.OrganizationID != organizationID || connection.ID != connectionID ||
		connection.Version != version || digest(opened) != hash {
		return Connection{}, ErrIntegrity
	}
	return connection, nil
}

func (store *Store) loadCustomerCurrent(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	customerID string,
) (loadedCustomer, error) {
	var version uint64
	var hash, state string
	var effectiveAt, expiresAt time.Time
	err := store.pool.QueryRow(ctx, `
		SELECT version,canonical_hash,state,effective_at,expires_at
		FROM workforce_customer_scope_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND customer_id=$3
	`, store.tenantID, organizationID, customerID).Scan(
		&version, &hash, &state, &effectiveAt, &expiresAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedCustomer{}, fmt.Errorf("%w: customer scope is unavailable", ErrDenied)
	}
	if err != nil {
		return loadedCustomer{}, fmt.Errorf("customer adapter: load customer head: %w", err)
	}
	now, err := store.currentTime()
	if err != nil {
		return loadedCustomer{}, err
	}
	if state != string(CustomerActive) || effectiveAt.After(now) || !expiresAt.After(now) {
		return loadedCustomer{}, fmt.Errorf("%w: customer scope is not active", ErrDenied)
	}
	loaded, err := store.loadCustomerVersion(ctx, organizationID, customerID, version)
	if err != nil {
		return loadedCustomer{}, err
	}
	if loaded.hash.Digest != hash {
		return loadedCustomer{}, ErrIntegrity
	}
	return loaded, nil
}

func (store *Store) loadCustomerVersion(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	customerID string,
	version uint64,
) (loadedCustomer, error) {
	if token("organization id", string(organizationID)) != nil ||
		token("customer id", customerID) != nil || version == 0 {
		return loadedCustomer{}, fmt.Errorf("customer adapter: customer identity is invalid")
	}
	var connectionID, hash string
	var connectionVersion uint64
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT connection_id,connection_version,canonical_hash,sealed_record
		FROM workforce_customer_scopes
		WHERE tenant_id=$1 AND organization_id=$2 AND customer_id=$3 AND version=$4
	`, store.tenantID, organizationID, customerID, version).Scan(
		&connectionID, &connectionVersion, &hash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedCustomer{}, fmt.Errorf("%w: customer version is unavailable", ErrDenied)
	}
	if err != nil {
		return loadedCustomer{}, fmt.Errorf("customer adapter: load customer version: %w", err)
	}
	opened, err := store.vault.OpenRecord(
		store.customerAD(organizationID, customerID, version), sealed,
	)
	if err != nil {
		return loadedCustomer{}, fmt.Errorf("%w: open customer scope", ErrIntegrity)
	}
	var customer CustomerScope
	if err := decodeStrict(opened, &customer); err != nil ||
		customer.OrganizationID != organizationID || customer.ID != customerID ||
		customer.Version != version || customer.ConnectionID != connectionID ||
		customer.ConnectionVersion != connectionVersion || digest(opened) != hash {
		return loadedCustomer{}, ErrIntegrity
	}
	connection, err := store.loadConnectionVersion(ctx, organizationID, connectionID, connectionVersion)
	if err != nil {
		return loadedCustomer{}, err
	}
	key, ok := store.issuerKeys[customer.Signature.KeyID]
	if !ok || !contains(connection.connection.Authority.CustomerIssuerKeyIDs, customer.Signature.KeyID) ||
		VerifyCustomerScope(customer, customer.Signature.KeyID, key) != nil ||
		validateCustomerGovernance(connection.connection, customer) != nil {
		return loadedCustomer{}, ErrIntegrity
	}
	return loadedCustomer{
		customer: customer,
		hash:     contracts.ContentHash{Algorithm: "sha256", Digest: hash},
	}, nil
}

func (store *Store) loadConsentCurrent(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	consentID string,
) (loadedConsent, error) {
	var version uint64
	var hash string
	err := store.pool.QueryRow(ctx, `
		SELECT version,canonical_hash FROM workforce_customer_consent_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND consent_id=$3
	`, store.tenantID, organizationID, consentID).Scan(&version, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedConsent{}, fmt.Errorf("%w: consent is unavailable", ErrConsent)
	}
	if err != nil {
		return loadedConsent{}, fmt.Errorf("customer adapter: load consent head: %w", err)
	}
	loaded, err := store.loadConsentVersion(ctx, organizationID, consentID, version)
	if err != nil {
		return loadedConsent{}, err
	}
	if loaded.hash.Digest != hash {
		return loadedConsent{}, ErrIntegrity
	}
	return loaded, nil
}

func (store *Store) loadConsentVersion(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	consentID string,
	version uint64,
) (loadedConsent, error) {
	if token("organization id", string(organizationID)) != nil ||
		token("consent id", consentID) != nil || version == 0 {
		return loadedConsent{}, fmt.Errorf("customer adapter: consent identity is invalid")
	}
	var connectionID, customerID, hash string
	var connectionVersion, customerVersion uint64
	var sealed []byte
	err := store.pool.QueryRow(ctx, `
		SELECT connection_id,connection_version,customer_id,customer_version,
		       canonical_hash,sealed_record
		FROM workforce_customer_consents
		WHERE tenant_id=$1 AND organization_id=$2 AND consent_id=$3 AND version=$4
	`, store.tenantID, organizationID, consentID, version).Scan(
		&connectionID, &connectionVersion, &customerID, &customerVersion,
		&hash, &sealed,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return loadedConsent{}, fmt.Errorf("%w: consent version is unavailable", ErrConsent)
	}
	if err != nil {
		return loadedConsent{}, fmt.Errorf("customer adapter: load consent version: %w", err)
	}
	opened, err := store.vault.OpenRecord(
		store.consentAD(organizationID, consentID, version), sealed,
	)
	if err != nil {
		return loadedConsent{}, fmt.Errorf("%w: open consent", ErrIntegrity)
	}
	var consent ConsentRecord
	if err := decodeStrict(opened, &consent); err != nil ||
		consent.OrganizationID != organizationID || consent.ID != consentID ||
		consent.Version != version || consent.ConnectionID != connectionID ||
		consent.ConnectionVersion != connectionVersion || consent.CustomerID != customerID ||
		consent.CustomerVersion != customerVersion || digest(opened) != hash {
		return loadedConsent{}, ErrIntegrity
	}
	connection, err := store.loadConnectionVersion(ctx, organizationID, connectionID, connectionVersion)
	if err != nil {
		return loadedConsent{}, err
	}
	key, ok := store.issuerKeys[consent.Signature.KeyID]
	if !ok || !contains(connection.connection.Authority.ConsentIssuerKeyIDs, consent.Signature.KeyID) ||
		VerifyConsentRecord(consent, consent.Signature.KeyID, key) != nil {
		return loadedConsent{}, ErrIntegrity
	}
	return loadedConsent{
		consent: consent,
		hash:    contracts.ContentHash{Algorithm: "sha256", Digest: hash},
	}, nil
}

func digest(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func (store *Store) latestConsentForScope(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	connectionID, customerID, recipientHash, channel, purpose string,
	now time.Time,
) (string, uint64, ConsentState, error) {
	var consentID string
	var version uint64
	var state ConsentState
	err := store.pool.QueryRow(ctx, `
		SELECT consent_id,version,state
		FROM workforce_customer_consent_heads
		WHERE tenant_id=$1 AND organization_id=$2 AND connection_id=$3
		  AND customer_id=$4 AND recipient_hash=$5 AND channel=$6 AND purpose=$7
		  AND effective_at<=$8
		ORDER BY effective_at DESC,updated_at DESC,version DESC,consent_id DESC
		LIMIT 1
	`, store.tenantID, organizationID, connectionID, customerID,
		recipientHash, channel, purpose, now).Scan(&consentID, &version, &state)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", 0, "", ErrConsent
	}
	if err != nil {
		return "", 0, "", fmt.Errorf("customer adapter: load scoped consent head: %w", err)
	}
	return consentID, version, state, nil
}
