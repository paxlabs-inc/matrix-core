package controlapi

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/securityqualification"
)

type SecurityQualificationRequest struct {
	ThreatModelID   string   `json:"threat_model_id"`
	ReviewIDs       []string `json:"review_ids"`
	ValidForSeconds uint64   `json:"valid_for_seconds"`
}

func (value SecurityQualificationRequest) validate() error {
	if strings.TrimSpace(value.ThreatModelID) == "" || len(value.ThreatModelID) > 128 ||
		len(value.ReviewIDs) < 2 || len(value.ReviewIDs) > 64 ||
		!slices.IsSorted(value.ReviewIDs) || value.ValidForSeconds == 0 ||
		value.ValidForSeconds > uint64((90*24*time.Hour)/time.Second) {
		return fmt.Errorf("controlapi: security qualification request is invalid")
	}
	for index, id := range value.ReviewIDs {
		if strings.TrimSpace(id) == "" || len(id) > 128 ||
			index > 0 && id == value.ReviewIDs[index-1] {
			return fmt.Errorf("controlapi: security review identities are invalid")
		}
	}
	return nil
}

func (service *Service) securityQualificationStore(
	ctx context.Context,
	principal Principal,
) (*securityqualification.Store, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, err
	}
	if service.securityStore == nil || len(service.securityKey) == 0 {
		return nil, fmt.Errorf("controlapi: security qualification is unavailable")
	}
	return service.securityStore, nil
}

func (service *Service) CommitSecurityThreatModel(
	ctx context.Context,
	principal Principal,
	value securityqualification.ThreatModel,
) (securityqualification.ThreatModel, error) {
	store, err := service.securityQualificationStore(ctx, principal)
	if err != nil {
		return securityqualification.ThreatModel{}, err
	}
	if _, err := store.CommitThreatModel(ctx, value); err != nil {
		return securityqualification.ThreatModel{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:security-threat-model:" + value.ID + ":" + fmt.Sprint(value.Version),
		OrganizationID: principal.OrganizationID, Type: "security.threat_model.committed",
		ResourceKind: "security-threat-model", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: false,
		Fields: map[string]any{
			"hazard_count": len(value.Hazards), "expires_at": value.ExpiresAt,
			"state": "reviewing",
		},
	})
	return value, err
}

func (service *Service) CommitSecurityReview(
	ctx context.Context,
	principal Principal,
	value securityqualification.BoundaryReview,
) (securityqualification.BoundaryReview, error) {
	store, err := service.securityQualificationStore(ctx, principal)
	if err != nil {
		return securityqualification.BoundaryReview{}, err
	}
	if _, err := store.CommitReview(ctx, value); err != nil {
		return securityqualification.BoundaryReview{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:security-review:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "security.boundary_review.committed",
		ResourceKind: "security-review", ResourceID: value.ID,
		ResourceVersion: 1, VerifiedCompletion: value.Outcome == securityqualification.ReviewApproved,
		Fields: map[string]any{
			"threat_model_id": value.ThreatModelID, "outcome": value.Outcome,
			"boundaries": value.Boundaries, "reviewer_seat_id": value.ReviewerSeatID,
		},
	})
	return value, err
}

func (service *Service) QualifySecurity(
	ctx context.Context,
	principal Principal,
	request SecurityQualificationRequest,
) (securityqualification.Qualification, error) {
	if err := request.validate(); err != nil {
		return securityqualification.Qualification{}, err
	}
	store, err := service.securityQualificationStore(ctx, principal)
	if err != nil {
		return securityqualification.Qualification{}, err
	}
	model, err := store.LoadThreatModel(ctx, request.ThreatModelID)
	if err != nil {
		return securityqualification.Qualification{}, err
	}
	reviews := make([]securityqualification.BoundaryReview, len(request.ReviewIDs))
	for index, id := range request.ReviewIDs {
		reviews[index], err = store.LoadReview(ctx, id)
		if err != nil {
			return securityqualification.Qualification{}, err
		}
	}
	now, err := service.currentTime()
	if err != nil {
		return securityqualification.Qualification{}, err
	}
	value, err := securityqualification.Qualify(
		model, reviews, service.securityKeyID, service.securityKey, now,
		time.Duration(request.ValidForSeconds)*time.Second,
	)
	if err != nil {
		return securityqualification.Qualification{}, err
	}
	if _, err := store.CommitQualification(ctx, value); err != nil {
		return securityqualification.Qualification{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:security-qualified:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "security.qualification.committed",
		ResourceKind: "security-qualification", ResourceID: value.ID,
		ResourceVersion: 1, VerifiedCompletion: true,
		Fields: map[string]any{
			"threat_model_id":      value.ThreatModelID,
			"qualified_boundaries": value.QualifiedBoundaries,
			"review_count":         len(value.ReviewIDs), "expires_at": value.ExpiresAt,
		},
	})
	return value, err
}
