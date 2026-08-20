package controlapi

import (
	"context"
	"fmt"

	"centra/workforce/internal/commercialcapability"
)

func (service *Service) listCommercialRecords(
	ctx context.Context,
	principal Principal,
	cursor string,
	limit int,
) (ResourcePage, error) {
	offset, err := decodePageCursor("commercial-records", cursor)
	if err != nil {
		return ResourcePage{}, err
	}
	if service.vault == nil {
		return ResourcePage{}, fmt.Errorf("controlapi: commercial record Vault is unavailable")
	}
	store, err := commercialcapability.NewStore(
		service.pool,
		service.vault,
		principal.TenantID,
		principal.OrganizationID,
		service.now,
	)
	if err != nil {
		return ResourcePage{}, err
	}
	values, more, err := store.ListCurrent(ctx, nil, offset, limit)
	if err != nil {
		return ResourcePage{}, err
	}
	now, err := service.currentTime()
	if err != nil {
		return ResourcePage{}, err
	}
	page := ResourcePage{
		SchemaVersion: SchemaVersion,
		Resource:      "commercial-records",
		Items:         make([]ResourceItem, 0, len(values)),
	}
	for _, value := range values {
		body := value.Record.Body
		review := value.Review
		uncertainty := make([]string, 0, 2)
		if !body.FreshUntil.After(now) {
			uncertainty = append(uncertainty, "record_evidence_expired")
		}
		if !review.ExpiresAt.After(now) {
			uncertainty = append(uncertainty, "independent_review_expired")
		}
		page.Items = append(page.Items, ResourceItem{
			ID:        string(body.ID),
			Version:   body.Version,
			UpdatedAt: review.VerifiedAt,
			Fields: map[string]any{
				"chain_id":          body.ChainID,
				"initiative_id":     body.InitiativeID,
				"department_id":     body.DepartmentID,
				"project_id":        body.ProjectID,
				"workspace_id":      body.WorkspaceID,
				"author_seat_id":    body.AuthorSeatID,
				"domain":            body.Domain,
				"kind":              body.Kind,
				"skill_id":          body.SkillID,
				"skill_version":     body.SkillVersion,
				"material":          body.Material,
				"supersedes":        body.Supersedes,
				"authority":         body.Authority,
				"customer":          body.Customer,
				"economic":          body.Economic,
				"hypotheses":        body.Hypotheses,
				"metrics":           body.Metrics,
				"observations":      body.Observations,
				"outcome":           body.Outcome,
				"handoffs":          body.Handoffs,
				"effective_at":      body.EffectiveAt,
				"fresh_until":       body.FreshUntil,
				"fresh":             len(uncertainty) == 0,
				"uncertainty":       uncertainty,
				"review_id":         review.ID,
				"review_outcome":    review.Outcome,
				"reviewer_seat_id":  review.VerifierSeatID,
				"procedure_id":      review.ProcedureID,
				"procedure_version": review.ProcedureVersion,
				"record_hash":       review.RecordHash,
				"review_findings":   review.Findings,
				"reviewed_at":       review.VerifiedAt,
				"review_expires_at": review.ExpiresAt,
			},
		})
	}
	if more {
		page.NextCursor = encodePageCursor("commercial-records", offset+uint64(len(values)))
	}
	return page, nil
}
