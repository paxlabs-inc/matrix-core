package controlapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"matrix/workforce/internal/mission"
)

type authorityProjectionMeta struct {
	Hash      string
	UpdatedAt time.Time
}

func (service *Service) listFounderResource(
	ctx context.Context,
	principal Principal,
	resource, cursor string,
	limit int,
) (ResourcePage, error) {
	offset, err := decodePageCursor(resource, cursor)
	if err != nil {
		return ResourcePage{}, err
	}
	_, keyID, err := service.currentCompanyAuthority(ctx, principal)
	if errors.Is(err, ErrNotActivated) {
		return ResourcePage{
			SchemaVersion: SchemaVersion,
			Resource:      resource,
			Items:         []ResourceItem{},
		}, nil
	}
	if err != nil {
		return ResourcePage{}, err
	}
	if service.vault == nil {
		return ResourcePage{}, fmt.Errorf("controlapi: company authority Vault is unavailable")
	}
	key, err := service.commandKey(ctx, principal, keyID)
	if err != nil {
		return ResourcePage{}, ErrUnauthorized
	}
	store, err := mission.NewStore(
		service.pool,
		service.vault,
		principal.TenantID,
		principal.OrganizationID,
		principal.OwnerID,
		key.KeyID,
		key.PublicKey,
		service.now,
	)
	if err != nil {
		return ResourcePage{}, err
	}
	current, err := store.LoadCurrent(ctx)
	if err != nil {
		return ResourcePage{}, err
	}
	metadata, err := service.currentAuthorityMetadata(ctx, principal)
	if err != nil {
		return ResourcePage{}, err
	}
	page := ResourcePage{
		SchemaVersion: SchemaVersion,
		Resource:      resource,
		Items:         make([]ResourceItem, 0, limit),
	}
	appendItem := func(item ResourceItem) {
		if uint64(len(page.Items)) < uint64(limit) {
			page.Items = append(page.Items, item)
		}
	}
	var all []ResourceItem
	switch resource {
	case "mission":
		value := current.Authority.Mission
		meta := metadata["founder_mission"]
		all = []ResourceItem{{
			ID: value.ID, Version: value.Version, UpdatedAt: meta.UpdatedAt,
			Fields: map[string]any{
				"purpose":                    value.Purpose,
				"permitted_business_domains": value.PermittedBusinessDomains,
				"strategic_principles":       value.StrategicPrinciples,
				"target_outcomes":            value.TargetOutcomes,
				"success_conditions":         value.SuccessConditions,
				"failure_conditions":         value.FailureConditions,
				"effective_at":               value.EffectiveAt,
				"canonical_hash":             meta.Hash,
				"signing_key_id":             value.Signature.KeyID,
			},
		}}
	case "constitution":
		value := current.Authority.Constitution
		meta := metadata["company_constitution"]
		all = []ResourceItem{{
			ID: value.ID, Version: value.Version, UpdatedAt: meta.UpdatedAt,
			Fields: map[string]any{
				"legal_prohibitions":       value.LegalProhibitions,
				"ethical_prohibitions":     value.EthicalProhibitions,
				"permitted_jurisdictions":  value.PermittedJurisdictions,
				"data_boundaries":          value.DataBoundaries,
				"permitted_counterparties": value.PermittedCounterparties,
				"risk_tolerance":           value.RiskTolerance,
				"autonomy":                 value.Autonomy,
				"escalation_conditions":    value.EscalationConditions,
				"pause_conditions":         value.PauseConditions,
				"shutdown_conditions":      value.ShutdownConditions,
				"effective_at":             value.EffectiveAt,
				"canonical_hash":           meta.Hash,
				"signing_key_id":           value.Signature.KeyID,
			},
		}}
	case "capital":
		value := current.Authority.Capital
		meta := metadata["capital_envelope"]
		all = []ResourceItem{{
			ID: value.ID, Version: value.Version, UpdatedAt: meta.UpdatedAt,
			Fields: map[string]any{
				"currency":                    value.Currency,
				"starting_microunits":         value.StartingMicrounits,
				"spend_ceiling_microunits":    value.SpendCeilingMicrounits,
				"exposure_ceiling_microunits": value.ExposureCeilingMicrounits,
				"minimum_runway_days":         value.MinimumRunwayDays,
				"effective_at":                value.EffectiveAt,
				"canonical_hash":              meta.Hash,
				"signing_key_id":              value.Signature.KeyID,
			},
		}}
	case "company-issuer-policy":
		value := current.Authority.IssuerPolicy
		meta := metadata["company_issuer_policy"]
		all = []ResourceItem{{
			ID: value.ID, Version: value.Version, UpdatedAt: meta.UpdatedAt,
			Fields: map[string]any{
				"issuer_key_id":              value.IssuerKeyID,
				"mission_version":            value.MissionVersion,
				"constitution_version":       value.ConstitutionVersion,
				"capital_envelope_version":   value.CapitalEnvelopeVersion,
				"allowed_work_order_classes": value.AllowedWorkOrderClasses,
				"max_work_order_microunits":  value.MaxWorkOrderMicrounits,
				"effective_at":               value.EffectiveAt,
				"expires_at":                 value.ExpiresAt,
				"canonical_hash":             meta.Hash,
				"signing_key_id":             value.Signature.KeyID,
			},
		}}
	case "operating-scopes":
		value := current.Authority.Constitution
		for _, scope := range value.OperatingScopes {
			updatedAt := value.EffectiveAt
			if meta := metadata["company_constitution"]; !meta.UpdatedAt.IsZero() {
				updatedAt = meta.UpdatedAt
			}
			all = append(all, ResourceItem{
				ID:        string(scope.Kind) + ":" + scope.ScopeID,
				Version:   value.Version,
				UpdatedAt: updatedAt,
				Fields: map[string]any{
					"kind":                 scope.Kind,
					"scope_id":             scope.ScopeID,
					"purpose":              scope.Purpose,
					"allowed_actions":      scope.AllowedActions,
					"data_classifications": scope.DataClassifications,
					"jurisdictions":        scope.Jurisdictions,
					"expires_at":           scope.ExpiresAt,
				},
			})
		}
	case "reserved-decisions":
		value := current.Authority.Constitution
		for _, decision := range value.ReservedDecisions {
			updatedAt := value.EffectiveAt
			if meta := metadata["company_constitution"]; !meta.UpdatedAt.IsZero() {
				updatedAt = meta.UpdatedAt
			}
			all = append(all, ResourceItem{
				ID:        decision.ClauseID,
				Version:   value.Version,
				UpdatedAt: updatedAt,
				Fields: map[string]any{
					"kind":        decision.Kind,
					"description": decision.Description,
					"escalation":  decision.Escalation,
				},
			})
		}
	default:
		return ResourcePage{}, fmt.Errorf("controlapi: unknown resource")
	}
	if offset > uint64(len(all)) {
		return ResourcePage{}, fmt.Errorf("controlapi: page cursor is outside the resource")
	}
	end := offset + uint64(limit)
	if end > uint64(len(all)) {
		end = uint64(len(all))
	}
	for _, item := range all[offset:end] {
		appendItem(item)
	}
	if end < uint64(len(all)) {
		page.NextCursor = encodePageCursor(resource, end)
	}
	return page, nil
}

func (service *Service) currentAuthorityMetadata(
	ctx context.Context,
	principal Principal,
) (map[string]authorityProjectionMeta, error) {
	rows, err := service.pool.Query(ctx, `
		SELECT head.authority_kind,record.canonical_hash,head.updated_at
		FROM workforce_company_authority_heads head
		JOIN workforce_company_authority_records record
		  ON record.tenant_id=head.tenant_id
		 AND record.organization_id=head.organization_id
		 AND record.authority_kind=head.authority_kind
		 AND record.authority_id=head.authority_id
		 AND record.version=head.latest_version
		WHERE head.tenant_id=$1 AND head.organization_id=$2
	`, principal.TenantID, principal.OrganizationID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]authorityProjectionMeta, 5)
	for rows.Next() {
		var kind string
		var meta authorityProjectionMeta
		if err := rows.Scan(&kind, &meta.Hash, &meta.UpdatedAt); err != nil {
			return nil, err
		}
		result[kind] = meta
	}
	return result, rows.Err()
}
