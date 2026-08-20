package operationswork

import (
	"strings"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/skills"
)

func TestInputEnforcesReconciliationPaymentAndAdministrativeExpiry(t *testing.T) {
	now := time.Now().UTC()
	base := Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: "organization:operations",
		Department:     contracts.DepartmentAccounting,
		SeatID:         "seat:accounting",
		IntentID:       "intent:reconcile",
		SkillID:        skills.ReconciliationSkill,
		Objective:      "Reconcile current external observation",
		Evidence:       []ExpiringEvidence{operationsEvidence(now)},
		SourceDigest:   operationsHash("a"),
		Draft: Draft{
			Summary: "Out-of-band observation is reconciled",
			Accounting: &AccountingDraft{
				Reconciliation: &ReconciliationObservation{
					ExternalObservationID: "bank-observation:1",
					ExpectedMinor:         1000, ObservedMinor: 1000,
					Currency: "USD", Disposition: "matched",
				},
			},
			CompletionChecks: []string{"observation identity is retained"},
		},
	}
	if err := base.validateAt(now); err != nil {
		t.Fatal(err)
	}
	missingObservation := base
	missingObservation.Draft.Accounting = &AccountingDraft{}
	if err := missingObservation.validateAt(now); err == nil {
		t.Fatal("reconciliation without external observation was accepted")
	}
	payment := base
	payment.SkillID = skills.PaymentProposalSkill
	payment.Draft.Accounting = &AccountingDraft{
		Payment: &PaymentProposal{
			Counterparty: "Vendor", AmountMinor: 1000,
			Currency: "USD", Purpose: "Approved invoice",
		},
	}
	if err := payment.validateAt(now); err != nil {
		t.Fatal(err)
	}
	admin := base
	admin.Department = contracts.DepartmentBackOffice
	admin.SkillID = skills.SchedulingSkill
	expired := now
	admin.Draft = Draft{
		Summary: "Administrative schedule",
		BackOffice: &BackOfficeDraft{
			Records: []string{"vendor request"}, ScheduledFor: &expired,
		},
		CompletionChecks: []string{"schedule is future-dated"},
	}
	if err := admin.validateAt(now); err == nil {
		t.Fatal("expired administrative schedule was accepted")
	}
}

func operationsEvidence(now time.Time) ExpiringEvidence {
	return ExpiringEvidence{
		Reference: contracts.EvidenceRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:operations",
			Hash:          operationsHash("b"),
			Kind:          "external_observation",
			ObservedAt:    now.Add(-time.Minute),
		},
		ExpiresAt: now.Add(time.Hour),
	}
}

func operationsHash(character string) contracts.ContentHash {
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: strings.Repeat(character, 64),
	}
}
