// Package operationswork executes bounded Accounting and Back Office skills.
// It produces typed proposals and can consume payment approval, but it owns no
// payment rail, filing credential, vendor credential, or effect gateway.
package operationswork

import (
	"context"
	"fmt"
	"strings"
	"time"

	"centra/workforce/internal/approval"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/skills"
)

type ExpiringEvidence struct {
	Reference contracts.EvidenceRef `json:"reference"`
	ExpiresAt time.Time             `json:"expires_at"`
}

type LedgerEntry struct {
	Account     string `json:"account"`
	Counterpart string `json:"counterpart"`
	AmountMinor int64  `json:"amount_minor"`
	Currency    string `json:"currency"`
}

type ReconciliationObservation struct {
	ExternalObservationID string `json:"external_observation_id"`
	ExpectedMinor         int64  `json:"expected_minor"`
	ObservedMinor         int64  `json:"observed_minor"`
	Currency              string `json:"currency"`
	Disposition           string `json:"disposition"`
}

type PaymentProposal struct {
	Counterparty string `json:"counterparty"`
	AmountMinor  uint64 `json:"amount_minor"`
	Currency     string `json:"currency"`
	Purpose      string `json:"purpose"`
}

type PaymentAuthorization struct {
	BatchID        contracts.ApprovalID `json:"batch_id"`
	CostMicrounits uint64               `json:"cost_microunits"`
	IdempotencyKey string               `json:"idempotency_key"`
}

type AccountingDraft struct {
	Entries        []LedgerEntry              `json:"entries"`
	Reconciliation *ReconciliationObservation `json:"reconciliation"`
	Report         string                     `json:"report"`
	ClosePeriod    string                     `json:"close_period"`
	Payment        *PaymentProposal           `json:"payment"`
	Authorization  *PaymentAuthorization      `json:"authorization"`
}

type AdministrativeHandoff struct {
	RecipientSeatID contracts.SeatID `json:"recipient_seat_id"`
	RequiredAction  string           `json:"required_action"`
	ExpiresAt       time.Time        `json:"expires_at"`
}

type BackOfficeDraft struct {
	Records      []string               `json:"records"`
	ScheduledFor *time.Time             `json:"scheduled_for"`
	Vendor       string                 `json:"vendor"`
	Process      string                 `json:"process"`
	SLAAt        *time.Time             `json:"sla_at"`
	Handoff      *AdministrativeHandoff `json:"handoff"`
}

type Draft struct {
	Summary          string           `json:"summary"`
	Accounting       *AccountingDraft `json:"accounting"`
	BackOffice       *BackOfficeDraft `json:"back_office"`
	CompletionChecks []string         `json:"completion_checks"`
	UnresolvedRisks  []string         `json:"unresolved_risks"`
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
		return nil, fmt.Errorf("operationswork: UTC time source and approval store are required")
	}
	return &Service{now: now, approvals: approvals}, nil
}

func (service *Service) Execute(ctx context.Context, input Input) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	now := service.now()
	if now.IsZero() || now.Location() != time.UTC {
		return Result{}, fmt.Errorf("operationswork: time source must return UTC")
	}
	if err := input.validateAt(now); err != nil {
		return Result{}, err
	}
	outcome := "proposed"
	requiresHuman := false
	if input.SkillID == skills.PaymentProposalSkill {
		authorization := input.Draft.Accounting.Authorization
		if authorization == nil {
			outcome = "requires_human"
			requiresHuman = true
		} else {
			if err := service.approvals.ConsumeBatch(
				ctx, authorization.BatchID, input.IntentID,
				authorization.CostMicrounits, authorization.IdempotencyKey,
			); err != nil {
				return Result{}, fmt.Errorf("operationswork: payment approval: %w", err)
			}
			outcome = "approved_for_payment_dispatch"
		}
	}
	payload, err := contracts.EncodeCanonical(&input)
	if err != nil {
		return Result{}, err
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
				"artifact:operations:" + digest.Digest[:32],
			),
			Hash:      digest,
			MediaType: "application/vnd.matrix.operations-work+json",
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
		input.OrganizationID == "" || input.SeatID == "" || input.IntentID == "" ||
		strings.TrimSpace(input.Objective) == "" || len(input.Objective) > 4096 ||
		!operationsSkillAllowed(input.Department, input.SkillID) ||
		len(input.Evidence) == 0 || len(input.Evidence) > 256 {
		return fmt.Errorf("operationswork: input is outside mandate or bounds")
	}
	seen := make(map[contracts.EvidenceID]bool, len(input.Evidence))
	for _, item := range input.Evidence {
		if err := item.Reference.Validate(); err != nil {
			return err
		}
		if item.ExpiresAt.IsZero() || item.ExpiresAt.Location() != time.UTC ||
			!item.ExpiresAt.After(item.Reference.ObservedAt) ||
			!now.IsZero() && !item.ExpiresAt.After(now) || seen[item.Reference.ID] {
			return fmt.Errorf("operationswork: evidence is expired, invalid, or duplicated")
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
			return fmt.Errorf("operationswork: correction must target a prior artifact")
		}
	}
	return input.Draft.validate(input.Department, input.SkillID, now)
}

func (draft Draft) validate(
	department contracts.DepartmentKind,
	skillID contracts.SkillID,
	now time.Time,
) error {
	if strings.TrimSpace(draft.Summary) == "" || len(draft.Summary) > 8192 ||
		len(draft.CompletionChecks) == 0 || len(draft.CompletionChecks) > 64 ||
		len(draft.UnresolvedRisks) > 64 {
		return fmt.Errorf("operationswork: draft is outside bounds")
	}
	for _, values := range [][]string{draft.CompletionChecks, draft.UnresolvedRisks} {
		for _, value := range values {
			if strings.TrimSpace(value) == "" || len(value) > 2048 {
				return fmt.Errorf("operationswork: draft clause is invalid")
			}
		}
	}
	if department == contracts.DepartmentAccounting {
		if draft.Accounting == nil || draft.BackOffice != nil {
			return fmt.Errorf("operationswork: Accounting requires an accounting draft only")
		}
		return draft.Accounting.validate(skillID)
	}
	if draft.BackOffice == nil || draft.Accounting != nil {
		return fmt.Errorf("operationswork: Back Office requires an administrative draft only")
	}
	return draft.BackOffice.validate(now)
}

func (draft AccountingDraft) validate(skillID contracts.SkillID) error {
	for _, entry := range draft.Entries {
		if strings.TrimSpace(entry.Account) == "" ||
			strings.TrimSpace(entry.Counterpart) == "" ||
			entry.AmountMinor == 0 || strings.TrimSpace(entry.Currency) == "" {
			return fmt.Errorf("operationswork: ledger entry is invalid")
		}
	}
	if skillID == skills.ReconciliationSkill {
		if draft.Reconciliation == nil ||
			strings.TrimSpace(draft.Reconciliation.ExternalObservationID) == "" ||
			strings.TrimSpace(draft.Reconciliation.Currency) == "" ||
			strings.TrimSpace(draft.Reconciliation.Disposition) == "" {
			return fmt.Errorf("operationswork: reconciliation requires an out-of-band observation")
		}
	} else if draft.Reconciliation != nil {
		return fmt.Errorf("operationswork: only reconciliation may emit an observation")
	}
	if skillID == skills.PaymentProposalSkill {
		if draft.Payment == nil || strings.TrimSpace(draft.Payment.Counterparty) == "" ||
			draft.Payment.AmountMinor == 0 ||
			strings.TrimSpace(draft.Payment.Currency) == "" ||
			strings.TrimSpace(draft.Payment.Purpose) == "" {
			return fmt.Errorf("operationswork: payment proposal is incomplete")
		}
		if draft.Authorization != nil &&
			(draft.Authorization.BatchID == "" ||
				draft.Authorization.CostMicrounits == 0 ||
				strings.TrimSpace(draft.Authorization.IdempotencyKey) == "") {
			return fmt.Errorf("operationswork: payment authorization is incomplete")
		}
	} else if draft.Payment != nil || draft.Authorization != nil {
		return fmt.Errorf("operationswork: only payment-proposal may carry payment state")
	}
	if len(draft.Entries) == 0 && strings.TrimSpace(draft.Report) == "" &&
		strings.TrimSpace(draft.ClosePeriod) == "" &&
		draft.Reconciliation == nil && draft.Payment == nil {
		return fmt.Errorf("operationswork: accounting draft has no work product")
	}
	return nil
}

func (draft BackOfficeDraft) validate(now time.Time) error {
	if len(draft.Records) == 0 && draft.ScheduledFor == nil &&
		strings.TrimSpace(draft.Vendor) == "" &&
		strings.TrimSpace(draft.Process) == "" && draft.SLAAt == nil &&
		draft.Handoff == nil {
		return fmt.Errorf("operationswork: Back Office draft has no work product")
	}
	for _, record := range draft.Records {
		if strings.TrimSpace(record) == "" || len(record) > 2048 {
			return fmt.Errorf("operationswork: administrative record is invalid")
		}
	}
	for _, timestamp := range []*time.Time{draft.ScheduledFor, draft.SLAAt} {
		if timestamp != nil &&
			(timestamp.IsZero() || timestamp.Location() != time.UTC ||
				!now.IsZero() && !timestamp.After(now)) {
			return fmt.Errorf("operationswork: administrative deadline is expired or invalid")
		}
	}
	if draft.Handoff != nil {
		if draft.Handoff.RecipientSeatID == "" ||
			strings.TrimSpace(draft.Handoff.RequiredAction) == "" ||
			draft.Handoff.ExpiresAt.IsZero() ||
			draft.Handoff.ExpiresAt.Location() != time.UTC ||
			!now.IsZero() && !draft.Handoff.ExpiresAt.After(now) {
			return fmt.Errorf("operationswork: administrative handoff is invalid")
		}
	}
	return nil
}

func operationsSkillAllowed(
	department contracts.DepartmentKind,
	skillID contracts.SkillID,
) bool {
	var allowed []contracts.SkillID
	switch department {
	case contracts.DepartmentAccounting:
		allowed = skills.AccountingSkillIDs()
	case contracts.DepartmentBackOffice:
		allowed = skills.BackOfficeSkillIDs()
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
