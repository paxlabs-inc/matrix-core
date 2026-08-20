package developer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"centra/packages/vault"

	"centra/workforce/internal/approval"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/operationswork"
	"centra/workforce/internal/skills"
)

func TestIntegration_AccountingAndBackOfficeUseObservationsApprovalHandoffAndFreshWakes(t *testing.T) {
	ctx := context.Background()
	tenant := "tenant:operations"
	organizationID := contracts.OrganizationID("organization:operations")
	ownerID := contracts.OwnerID("owner:operations")
	now := developerNow()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approvalStore, err := approval.New(
		developerPool, session.UserVault(), tenant, organizationID,
		ownerID, "key:operations-owner", ownerPublic, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := operationswork.New(developerNow, approvalStore)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := skills.OperationsPack()
	if err != nil {
		t.Fatal(err)
	}
	runner := buildKnowledgeSeatRunner(t)
	evidence := operationsIntegrationEvidence(now)
	accountingAddress := contracts.SeatAddress{
		OrganizationID: organizationID,
		DepartmentID:   "department:accounting",
		SeatID:         "seat:accounting-executor",
	}
	reconcilePacket := knowledgeLoopPacket(
		t, organizationID, accountingAddress, contracts.DepartmentAccounting,
		contracts.SeatExecutor, "wake:accounting-reconcile", "lease:accounting-reconcile",
		"intent:accounting-reconcile",
		knowledgeSkillRef(t, pack, skills.ReconciliationSkill),
		nil, nil, []contracts.EvidenceRef{evidence.Reference}, now,
	)
	reconcileWake, err := runKnowledgeSeat(t, ctx, runner, reconcilePacket)
	if err != nil {
		t.Fatal(err)
	}
	reconciled, err := service.Execute(ctx, operationswork.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentAccounting,
		SeatID:         accountingAddress.SeatID,
		IntentID:       reconcilePacket.Intent.ID,
		SkillID:        skills.ReconciliationSkill,
		Objective:      "Reconcile the bank observation against the current payable",
		Evidence:       []operationswork.ExpiringEvidence{evidence},
		SourceDigest:   reconcileWake.PacketDigest,
		Draft: operationswork.Draft{
			Summary: "The out-of-band bank observation matches the expected payable",
			Accounting: &operationswork.AccountingDraft{
				Reconciliation: &operationswork.ReconciliationObservation{
					ExternalObservationID: "bank-observation:invoice-1",
					ExpectedMinor:         12500, ObservedMinor: 12500,
					Currency: "USD", Disposition: "matched",
				},
			},
			CompletionChecks: []string{"external observation identity is retained"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	paymentInput := operationswork.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentAccounting,
		SeatID:         accountingAddress.SeatID,
		IntentID:       "intent:accounting-payment",
		SkillID:        skills.PaymentProposalSkill,
		Objective:      "Propose payment for the reconciled payable",
		Evidence:       []operationswork.ExpiringEvidence{evidence},
		SourceDigest:   reconciled.Artifact.Hash,
		Draft: operationswork.Draft{
			Summary: "Payment proposal requires owner authority and moves no funds",
			Accounting: &operationswork.AccountingDraft{
				Payment: &operationswork.PaymentProposal{
					Counterparty: "Vendor One", AmountMinor: 12500,
					Currency: "USD", Purpose: "Reconciled invoice",
				},
			},
			CompletionChecks: []string{"proposal amount matches reconciliation"},
		},
	}
	pending, err := service.Execute(ctx, paymentInput)
	if err != nil {
		t.Fatal(err)
	}
	if pending.Outcome != "requires_human" || !pending.RequiresHuman {
		t.Fatalf("unapproved payment = %#v", pending)
	}
	batch := approval.BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       "approval:payment",
		TenantID:      tenant, OrganizationID: organizationID,
		IntentIDs:                  []contracts.IntentID{paymentInput.IntentID},
		AggregateCeilingMicrounits: 1,
		ExpiresAt:                  now.Add(30 * time.Minute),
		OwnerID:                    ownerID,
	}
	if err := approval.SignBatch(&batch, "key:operations-owner", ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := approvalStore.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	paymentInput.Draft.Accounting.Authorization = &operationswork.PaymentAuthorization{
		BatchID: batch.BatchID, CostMicrounits: 1,
		IdempotencyKey: "consume:payment",
	}
	approved, err := service.Execute(ctx, paymentInput)
	if err != nil {
		t.Fatal(err)
	}
	if approved.Outcome != "approved_for_payment_dispatch" ||
		approved.RequiresHuman || len(approved.Evidence) != 1 ||
		approved.Artifact.Hash == (contracts.ContentHash{}) {
		t.Fatalf("approved payment proposal = %#v", approved)
	}

	backOfficeAddress := contracts.SeatAddress{
		OrganizationID: organizationID,
		DepartmentID:   "department:back-office",
		SeatID:         "seat:back-office-lead",
	}
	backOfficePacket := knowledgeLoopPacket(
		t, organizationID, backOfficeAddress, contracts.DepartmentBackOffice,
		contracts.SeatLead, "wake:back-office-handoff", "lease:back-office-handoff",
		"intent:back-office-handoff",
		knowledgeSkillRef(t, pack, skills.AdministrativeWorkflowSkill),
		nil, []contracts.ArtifactRef{approved.Artifact},
		[]contracts.EvidenceRef{evidence.Reference}, now,
	)
	backOfficeWake, err := runKnowledgeSeat(t, ctx, runner, backOfficePacket)
	if err != nil {
		t.Fatal(err)
	}
	scheduled := now.Add(24 * time.Hour)
	sla := now.Add(2 * time.Hour)
	admin, err := service.Execute(ctx, operationswork.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentBackOffice,
		SeatID:         backOfficeAddress.SeatID,
		IntentID:       backOfficePacket.Intent.ID,
		SkillID:        skills.AdministrativeWorkflowSkill,
		Objective:      "Coordinate the approved proposal without dispatching payment",
		Evidence:       []operationswork.ExpiringEvidence{evidence},
		SourceDigest:   approved.Artifact.Hash,
		Draft: operationswork.Draft{
			Summary: "Administrative handoff tracks the next human-owned action",
			BackOffice: &operationswork.BackOfficeDraft{
				Records:      []string{"approved payment proposal"},
				ScheduledFor: &scheduled,
				Vendor:       "Vendor One",
				Process:      "human payment dispatch coordination",
				SLAAt:        &sla,
				Handoff: &operationswork.AdministrativeHandoff{
					RecipientSeatID: "seat:accounting-lead",
					RequiredAction:  "Confirm human-owned dispatch and return receipt",
					ExpiresAt:       now.Add(time.Hour),
				},
			},
			CompletionChecks: []string{"handoff retains expiry and SLA"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if admin.Outcome != "proposed" || len(admin.Payload) == 0 ||
		reconcileWake.WakeID == backOfficeWake.WakeID {
		t.Fatalf(
			"admin/fresh wakes result=%#v accounting=%s back-office=%s",
			admin, reconcileWake.WakeID, backOfficeWake.WakeID,
		)
	}
}

func operationsIntegrationEvidence(now time.Time) operationswork.ExpiringEvidence {
	return operationswork.ExpiringEvidence{
		Reference: contracts.EvidenceRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:bank-observation",
			Hash:          developerHash("bank-observation"),
			Kind:          "external_observation",
			ObservedAt:    now.Add(-time.Minute),
		},
		ExpiresAt: now.Add(time.Hour),
	}
}
