// Package domainwork executes bounded Marketing and Legal knowledge-work
// contracts. It can consume owner approval for publication readiness but has
// no effect gateway or publication credentials.
package domainwork

import (
	"context"
	"fmt"
	"strings"
	"time"

	"centra/workforce/internal/approval"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/skills"
)

const maxDomainPayloadBytes = 1 << 20

type ExpiringEvidence struct {
	Reference contracts.EvidenceRef `json:"reference"`
	ExpiresAt time.Time             `json:"expires_at"`
}

type CampaignDraft struct {
	Audience           string   `json:"audience"`
	Channels           []string `json:"channels"`
	Content            string   `json:"content"`
	PerformanceMetrics []string `json:"performance_metrics"`
}

type PublicationAuthorization struct {
	BatchID        contracts.ApprovalID `json:"batch_id"`
	CostMicrounits uint64               `json:"cost_microunits"`
	IdempotencyKey string               `json:"idempotency_key"`
}

type LegalDraft struct {
	Jurisdictions []string `json:"jurisdictions"`
	Issues        []string `json:"issues"`
	Analysis      string   `json:"analysis"`
	Disclaimer    string   `json:"disclaimer"`
	HumanSignoff  bool     `json:"human_signoff"`
}

type Draft struct {
	Summary              string                    `json:"summary"`
	Campaign             *CampaignDraft            `json:"campaign"`
	Publication          *PublicationAuthorization `json:"publication"`
	Legal                *LegalDraft               `json:"legal"`
	UnresolvedRisks      []string                  `json:"unresolved_risks"`
	PerformanceReceiptID string                    `json:"performance_receipt_id"`
}

type Input struct {
	SchemaVersion  string                   `json:"schema_version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Department     contracts.DepartmentKind `json:"department"`
	SeatID         contracts.SeatID         `json:"seat_id"`
	IntentID       contracts.IntentID       `json:"intent_id"`
	SkillID        contracts.SkillID        `json:"skill_id"`
	Objective      string                   `json:"objective"`
	Evidence       []ExpiringEvidence       `json:"evidence"`
	SourceDigest   contracts.ContentHash    `json:"source_digest"`
	CorrectionOf   *contracts.ContentHash   `json:"correction_of"`
	Draft          Draft                    `json:"draft"`
}

type Result struct {
	SchemaVersion string                  `json:"schema_version"`
	Outcome       string                  `json:"outcome"`
	Artifact      contracts.ArtifactRef   `json:"artifact"`
	Evidence      []contracts.EvidenceRef `json:"evidence"`
	RequiresHuman bool                    `json:"requires_human"`
	Payload       []byte                  `json:"-"`
}

type Service struct {
	now       func() time.Time
	approvals *approval.Store
}

func New(now func() time.Time, approvals *approval.Store) (*Service, error) {
	if now == nil || approvals == nil {
		return nil, fmt.Errorf("domainwork: UTC time source and approval store are required")
	}
	return &Service{now: now, approvals: approvals}, nil
}

func (service *Service) Execute(ctx context.Context, input Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	now := service.now()
	if now.IsZero() || now.Location() != time.UTC {
		return Result{}, fmt.Errorf("domainwork: time source must return UTC")
	}
	if err := input.validateAt(now); err != nil {
		return Result{}, err
	}
	outcome := "proposed"
	requiresHuman := input.Department == contracts.DepartmentLegal
	if input.SkillID == skills.PublicationGatesSkill {
		authorization := input.Draft.Publication
		if err := service.approvals.ConsumeBatch(
			ctx, authorization.BatchID, input.IntentID,
			authorization.CostMicrounits, authorization.IdempotencyKey,
		); err != nil {
			return Result{}, fmt.Errorf("domainwork: publication approval: %w", err)
		}
		outcome = "approved_for_publication"
	}
	if requiresHuman {
		outcome = "requires_human"
	}
	payload, err := contracts.EncodeCanonical(&input)
	if err != nil {
		return Result{}, err
	}
	if len(payload) > maxDomainPayloadBytes {
		return Result{}, fmt.Errorf("domainwork: typed payload exceeds one MiB")
	}
	digest, err := contracts.HashCanonical(&input)
	if err != nil {
		return Result{}, err
	}
	evidence := make([]contracts.EvidenceRef, len(input.Evidence))
	for index := range input.Evidence {
		evidence[index] = input.Evidence[index].Reference
	}
	return Result{
		SchemaVersion: contracts.SchemaVersionV1,
		Outcome:       outcome,
		Artifact: contracts.ArtifactRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID: contracts.ArtifactID(
				"artifact:domain:" + digest.Digest[:32],
			),
			Hash:      digest,
			MediaType: "application/vnd.matrix.domain-work+json",
			SizeBytes: uint64(len(payload)),
		},
		Evidence: evidence, RequiresHuman: requiresHuman,
		Payload: append([]byte(nil), payload...),
	}, nil
}

func (input Input) Validate() error {
	return input.validateAt(time.Time{})
}

func (input Input) validateAt(now time.Time) error {
	if input.SchemaVersion != contracts.SchemaVersionV1 ||
		input.OrganizationID == "" || input.SeatID == "" || input.IntentID == "" {
		return fmt.Errorf("domainwork: input identity is incomplete")
	}
	if !domainSkillAllowed(input.Department, input.SkillID) {
		return fmt.Errorf("domainwork: skill is outside department mandate")
	}
	if strings.TrimSpace(input.Objective) == "" || len(input.Objective) > 4096 ||
		len(input.Evidence) == 0 || len(input.Evidence) > 256 {
		return fmt.Errorf("domainwork: objective or evidence is outside bounds")
	}
	seen := make(map[contracts.EvidenceID]bool, len(input.Evidence))
	for _, item := range input.Evidence {
		if err := item.Reference.Validate(); err != nil {
			return err
		}
		if item.ExpiresAt.IsZero() || item.ExpiresAt.Location() != time.UTC ||
			!item.ExpiresAt.After(item.Reference.ObservedAt) ||
			!now.IsZero() && !item.ExpiresAt.After(now) {
			return fmt.Errorf("domainwork: evidence is expired or invalid")
		}
		if seen[item.Reference.ID] {
			return fmt.Errorf("domainwork: evidence is duplicated")
		}
		seen[item.Reference.ID] = true
	}
	if err := input.SourceDigest.Validate(); err != nil {
		return err
	}
	if input.CorrectionOf != nil {
		if err := input.CorrectionOf.Validate(); err != nil {
			return err
		}
		if *input.CorrectionOf == input.SourceDigest {
			return fmt.Errorf("domainwork: correction must target a prior artifact")
		}
	}
	return input.Draft.validate(input.Department, input.SkillID)
}

func (draft Draft) validate(
	department contracts.DepartmentKind,
	skillID contracts.SkillID,
) error {
	if strings.TrimSpace(draft.Summary) == "" || len(draft.Summary) > 8192 ||
		len(draft.UnresolvedRisks) > 64 {
		return fmt.Errorf("domainwork: draft is outside bounds")
	}
	for _, risk := range draft.UnresolvedRisks {
		if strings.TrimSpace(risk) == "" || len(risk) > 2048 {
			return fmt.Errorf("domainwork: unresolved risk is invalid")
		}
	}
	if department == contracts.DepartmentMarketing {
		if draft.Legal != nil || draft.Campaign == nil {
			return fmt.Errorf("domainwork: Marketing requires a campaign draft only")
		}
		if err := draft.Campaign.validate(); err != nil {
			return err
		}
		if skillID == skills.PublicationGatesSkill {
			if draft.Publication == nil || draft.Publication.BatchID == "" ||
				draft.Publication.CostMicrounits == 0 ||
				strings.TrimSpace(draft.Publication.IdempotencyKey) == "" {
				return fmt.Errorf("domainwork: publication gate requires exact owner approval")
			}
		} else if draft.Publication != nil {
			return fmt.Errorf("domainwork: only publication-gates may consume approval")
		}
		return nil
	}
	if draft.Campaign != nil || draft.Publication != nil || draft.Legal == nil {
		return fmt.Errorf("domainwork: Legal requires a legal draft only")
	}
	return draft.Legal.validate()
}

func (campaign CampaignDraft) validate() error {
	if strings.TrimSpace(campaign.Audience) == "" ||
		len(campaign.Audience) > 2048 ||
		len(campaign.Channels) == 0 || len(campaign.Channels) > 32 ||
		strings.TrimSpace(campaign.Content) == "" ||
		len(campaign.Content) > 64<<10 ||
		len(campaign.PerformanceMetrics) == 0 ||
		len(campaign.PerformanceMetrics) > 32 {
		return fmt.Errorf("domainwork: campaign draft is incomplete")
	}
	for _, values := range [][]string{campaign.Channels, campaign.PerformanceMetrics} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 2048 {
				return fmt.Errorf("domainwork: campaign clause is invalid")
			}
		}
	}
	return nil
}

func (legal LegalDraft) validate() error {
	if len(legal.Jurisdictions) == 0 || len(legal.Jurisdictions) > 32 ||
		len(legal.Issues) == 0 || len(legal.Issues) > 64 ||
		strings.TrimSpace(legal.Analysis) == "" ||
		len(legal.Analysis) > 64<<10 ||
		strings.TrimSpace(legal.Disclaimer) == "" ||
		len(legal.Disclaimer) > 2048 || !legal.HumanSignoff {
		return fmt.Errorf("domainwork: legal draft must remain non-final and require human signoff")
	}
	for _, values := range [][]string{legal.Jurisdictions, legal.Issues} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 2048 {
				return fmt.Errorf("domainwork: legal clause is invalid")
			}
		}
	}
	return nil
}

func domainSkillAllowed(
	department contracts.DepartmentKind,
	skillID contracts.SkillID,
) bool {
	var allowed []contracts.SkillID
	switch department {
	case contracts.DepartmentMarketing:
		allowed = skills.MarketingSkillIDs()
	case contracts.DepartmentLegal:
		allowed = skills.LegalSkillIDs()
	default:
		return false
	}
	for _, candidate := range allowed {
		if candidate == skillID {
			return true
		}
	}
	return false
}
