package controlapi

import (
	"context"
	"fmt"

	"centra/workforce/internal/provider/financial"
)

func (service *Service) authorizedFinancialStore(
	ctx context.Context,
	principal Principal,
) (*financial.Store, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, err
	}
	service.operatingStoresMu.RLock()
	store := service.financialStore
	service.operatingStoresMu.RUnlock()
	if store == nil {
		return nil, fmt.Errorf("controlapi: financial authority is unavailable")
	}
	return store, nil
}

func (service *Service) RegisterFinancialConnection(
	ctx context.Context,
	principal Principal,
	value financial.Connection,
) (financial.Connection, error) {
	store, err := service.authorizedFinancialStore(ctx, principal)
	if err != nil {
		return financial.Connection{}, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return financial.Connection{}, ErrUnauthorized
	}
	if err := store.RegisterConnection(ctx, value); err != nil {
		return financial.Connection{}, err
	}
	service.requestRuntimeReload()
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:financial-connection:%s:%d", value.ID, value.Version),
		OrganizationID: principal.OrganizationID, Type: "financial.connection.registered",
		ResourceKind: "financial-connection", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"family": value.Family, "account_id": value.AccountID,
			"adapter_name": value.AdapterName, "state": "registered",
		},
	})
	return value, err
}

func (service *Service) RevokeFinancialConnection(
	ctx context.Context,
	principal Principal,
	value financial.ConnectionRevocation,
) (financial.ConnectionRevocation, error) {
	store, err := service.authorizedFinancialStore(ctx, principal)
	if err != nil {
		return financial.ConnectionRevocation{}, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return financial.ConnectionRevocation{}, ErrUnauthorized
	}
	if err := store.RevokeConnection(ctx, value); err != nil {
		return financial.ConnectionRevocation{}, err
	}
	service.requestRuntimeReload()
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:financial-connection-revoked:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "financial.connection.revoked",
		ResourceKind: "financial-connection", ResourceID: value.ConnectionID,
		ResourceVersion: value.Version, VerifiedCompletion: true,
		Fields: map[string]any{"reason_code": value.ReasonCode, "state": "revoked"},
	})
	return value, err
}

func (service *Service) RegisterFinancialValuation(
	ctx context.Context,
	principal Principal,
	value financial.ValuationSnapshot,
) (financial.ValuationSnapshot, error) {
	store, err := service.authorizedFinancialStore(ctx, principal)
	if err != nil {
		return financial.ValuationSnapshot{}, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return financial.ValuationSnapshot{}, ErrUnauthorized
	}
	if err := store.RegisterValuation(ctx, value); err != nil {
		return financial.ValuationSnapshot{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:financial-valuation:%s:%d", value.ID, value.Version),
		OrganizationID: principal.OrganizationID, Type: "financial.valuation.registered",
		ResourceKind: "financial-valuation", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: true,
		Fields: map[string]any{
			"connection_id": value.ConnectionID, "observed_at": value.ObservedAt,
			"expires_at": value.ExpiresAt, "state": "observed",
		},
	})
	return value, err
}

func (service *Service) RegisterFinancialRisk(
	ctx context.Context,
	principal Principal,
	value financial.RiskSnapshot,
) (financial.RiskSnapshot, error) {
	store, err := service.authorizedFinancialStore(ctx, principal)
	if err != nil {
		return financial.RiskSnapshot{}, err
	}
	if value.OrganizationID != principal.OrganizationID {
		return financial.RiskSnapshot{}, ErrUnauthorized
	}
	if err := store.RegisterRiskSnapshot(ctx, value); err != nil {
		return financial.RiskSnapshot{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             fmt.Sprintf("event:financial-risk:%s:%d", value.ID, value.Version),
		OrganizationID: principal.OrganizationID, Type: "financial.risk.registered",
		ResourceKind: "financial-risk", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: true,
		Fields: map[string]any{
			"connection_id": value.ConnectionID, "observed_at": value.ObservedAt,
			"expires_at": value.ExpiresAt, "state": "observed",
		},
	})
	return value, err
}
