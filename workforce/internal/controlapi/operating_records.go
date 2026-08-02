package controlapi

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"matrix/workforce/internal/commercialcapability"
	"matrix/workforce/internal/provider/customer"
	"matrix/workforce/internal/provider/external"
)

type ExternalConnectionRegistration struct {
	Connection external.Connection         `json:"connection"`
	Credential external.CredentialMaterial `json:"credential"`
}

func (service *Service) externalAuthorityStore(
	ctx context.Context,
	principal Principal,
) (*external.Store, error) {
	root, err := service.RuntimeOwnerRoot(ctx, principal)
	if err != nil {
		return nil, err
	}
	if service.vault == nil {
		return nil, fmt.Errorf("controlapi: operating-scope Vault is unavailable")
	}
	return external.NewStore(
		service.pool, service.vault, principal.TenantID,
		root.KeyID, root.PublicKey, service.now,
	)
}

func (service *Service) customerAuthorityStore(
	ctx context.Context,
	principal Principal,
) (*customer.Store, error) {
	root, err := service.RuntimeOwnerRoot(ctx, principal)
	if err != nil {
		return nil, err
	}
	if service.vault == nil || service.companyIssuerKeyID == "" ||
		len(service.companyIssuerPublic) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("controlapi: customer authority is unavailable")
	}
	return customer.NewStore(
		service.pool, service.vault, principal.TenantID,
		root.KeyID, root.PublicKey,
		map[string]ed25519.PublicKey{
			service.companyIssuerKeyID: service.companyIssuerPublic,
		},
		service.now,
	)
}

func (service *Service) commercialAuthorityStore(
	ctx context.Context,
	principal Principal,
) (*commercialcapability.Store, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, err
	}
	if service.vault == nil {
		return nil, fmt.Errorf("controlapi: commercial record Vault is unavailable")
	}
	return commercialcapability.NewStore(
		service.pool, service.vault, principal.TenantID,
		principal.OrganizationID, service.now,
	)
}

func (service *Service) RegisterExternalConnection(
	ctx context.Context,
	principal Principal,
	value ExternalConnectionRegistration,
) (external.Connection, error) {
	if value.Connection.OrganizationID != principal.OrganizationID {
		return external.Connection{}, ErrUnauthorized
	}
	store, err := service.externalAuthorityStore(ctx, principal)
	if err != nil {
		return external.Connection{}, err
	}
	if err := store.RegisterConnection(ctx, value.Connection, value.Credential); err != nil {
		return external.Connection{}, err
	}
	service.requestRuntimeReload()
	connection := value.Connection
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:operating-scope:%s:%d", connection.ID, connection.Version),
		OrganizationID: principal.OrganizationID, Type: "operating_scope.registered",
		ResourceKind: "operating-scope", ResourceID: connection.ID,
		ResourceVersion: connection.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"adapter_name": connection.AdapterName, "family": connection.Family,
			"provider": connection.Provider, "effective_at": connection.EffectiveAt,
			"expires_at": connection.ExpiresAt, "state": "registered",
		},
	})
	return connection, err
}

func (service *Service) RevokeExternalConnection(
	ctx context.Context,
	principal Principal,
	value external.ConnectionRevocation,
) (external.ConnectionRevocation, error) {
	if value.OrganizationID != principal.OrganizationID {
		return external.ConnectionRevocation{}, ErrUnauthorized
	}
	store, err := service.externalAuthorityStore(ctx, principal)
	if err != nil {
		return external.ConnectionRevocation{}, err
	}
	if err := store.RevokeConnection(ctx, value); err != nil {
		return external.ConnectionRevocation{}, err
	}
	service.requestRuntimeReload()
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:operating-scope-revoked:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "operating_scope.revoked",
		ResourceKind: "operating-scope", ResourceID: value.ConnectionID,
		ResourceVersion: value.Version, VerifiedCompletion: true,
		Fields: map[string]any{"reason_code": value.ReasonCode, "state": "revoked"},
	})
	return value, err
}

func (service *Service) RegisterCustomerConnection(
	ctx context.Context,
	principal Principal,
	value customer.Connection,
) (customer.Connection, error) {
	if value.OrganizationID != principal.OrganizationID {
		return customer.Connection{}, ErrUnauthorized
	}
	store, err := service.customerAuthorityStore(ctx, principal)
	if err != nil {
		return customer.Connection{}, err
	}
	if err := store.RegisterConnection(ctx, value); err != nil {
		return customer.Connection{}, err
	}
	service.requestRuntimeReload()
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:customer-connection:%s:%d", value.ID, value.Version),
		OrganizationID: principal.OrganizationID, Type: "customer.connection.registered",
		ResourceKind: "customer-connection", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"family": value.Family, "account_id": value.AccountID,
			"adapter_name": value.AdapterName, "state": "registered",
		},
	})
	return value, err
}

func (service *Service) RevokeCustomerConnection(
	ctx context.Context,
	principal Principal,
	value customer.ConnectionRevocation,
) (customer.ConnectionRevocation, error) {
	if value.OrganizationID != principal.OrganizationID {
		return customer.ConnectionRevocation{}, ErrUnauthorized
	}
	store, err := service.customerAuthorityStore(ctx, principal)
	if err != nil {
		return customer.ConnectionRevocation{}, err
	}
	if err := store.RevokeConnection(ctx, value); err != nil {
		return customer.ConnectionRevocation{}, err
	}
	service.requestRuntimeReload()
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:customer-connection-revoked:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "customer.connection.revoked",
		ResourceKind: "customer-connection", ResourceID: value.ConnectionID,
		ResourceVersion: value.Version, VerifiedCompletion: true,
		Fields: map[string]any{"reason_code": value.ReasonCode, "state": "revoked"},
	})
	return value, err
}

func (service *Service) RegisterCustomerScope(
	ctx context.Context,
	principal Principal,
	value customer.CustomerScope,
) (customer.CustomerScope, error) {
	if value.OrganizationID != principal.OrganizationID {
		return customer.CustomerScope{}, ErrUnauthorized
	}
	store, err := service.customerAuthorityStore(ctx, principal)
	if err != nil {
		return customer.CustomerScope{}, err
	}
	if err := store.RegisterCustomer(ctx, value); err != nil {
		return customer.CustomerScope{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:customer:%s:%d", value.ID, value.Version),
		OrganizationID: principal.OrganizationID, Type: "customer.scope.registered",
		ResourceKind: "customer", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"connection_id": value.ConnectionID, "customer_state": value.State,
			"effective_at": value.EffectiveAt, "expires_at": value.ExpiresAt,
		},
	})
	return value, err
}

func (service *Service) RecordCustomerConsent(
	ctx context.Context,
	principal Principal,
	value customer.ConsentRecord,
) (customer.ConsentRecord, error) {
	if value.OrganizationID != principal.OrganizationID {
		return customer.ConsentRecord{}, ErrUnauthorized
	}
	store, err := service.customerAuthorityStore(ctx, principal)
	if err != nil {
		return customer.ConsentRecord{}, err
	}
	if err := store.RecordConsent(ctx, value); err != nil {
		return customer.ConsentRecord{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:customer-consent:%s:%d", value.ID, value.Version),
		OrganizationID: principal.OrganizationID, Type: "customer.consent.recorded",
		ResourceKind: "customer-consent", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: true,
		Fields: map[string]any{
			"customer_id": value.CustomerID, "channel": value.Channel,
			"purpose": value.Purpose, "consent_state": value.State,
			"effective_at": value.EffectiveAt, "expires_at": value.ExpiresAt,
		},
	})
	return value, err
}

func (service *Service) CommitCommercialRecord(
	ctx context.Context,
	principal Principal,
	value commercialcapability.VerifiedRecord,
) (commercialcapability.VerifiedRecord, error) {
	if value.Record.Body.OrganizationID != principal.OrganizationID {
		return commercialcapability.VerifiedRecord{}, ErrUnauthorized
	}
	store, err := service.commercialAuthorityStore(ctx, principal)
	if err != nil {
		return commercialcapability.VerifiedRecord{}, err
	}
	if _, err := store.Commit(ctx, value); err != nil {
		return commercialcapability.VerifiedRecord{}, err
	}
	body := value.Record.Body
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:commercial-record:" + string(body.ID),
		OrganizationID: principal.OrganizationID, Type: "commercial.record.verified",
		ResourceKind: "commercial-record", ResourceID: string(body.ID),
		ResourceVersion: body.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"initiative_id": body.InitiativeID, "domain": body.Domain,
			"kind": body.Kind, "outcome_kind": body.Outcome.Kind,
			"outcome_uncertainty_bps": body.Outcome.UncertaintyBPS,
			"review_outcome":          value.Review.Outcome, "fresh_until": body.FreshUntil,
			"state": "verified_record",
		},
	})
	return value, err
}
