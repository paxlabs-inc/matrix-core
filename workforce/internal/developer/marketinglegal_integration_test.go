package developer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"testing"
	"time"

	"matrix/vault"

	"matrix/workforce/internal/approval"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/domainwork"
	"matrix/workforce/internal/skills"
)

func TestIntegration_MarketingPublicationAndLegalReviewUseApprovalFreshWakesAndCorrection(t *testing.T) {
	ctx := context.Background()
	tenant := "tenant:marketing-legal"
	organizationID := contracts.OrganizationID("organization:marketing-legal")
	ownerID := contracts.OwnerID("owner:marketing-legal")
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
		ownerID, "key:marketing-owner", ownerPublic, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := domainwork.New(developerNow, approvalStore)
	if err != nil {
		t.Fatal(err)
	}
	knowledgePack, err := skills.ExecutiveResearchPack()
	if err != nil {
		t.Fatal(err)
	}
	domainPack, err := skills.MarketingLegalPack()
	if err != nil {
		t.Fatal(err)
	}
	pack := append(knowledgePack, domainPack...)
	runner := buildKnowledgeSeatRunner(t)
	evidence := domainIntegrationEvidence(now)

	marketingAddress := contracts.SeatAddress{
		OrganizationID: organizationID,
		DepartmentID:   "department:marketing",
		SeatID:         "seat:marketing-executor",
	}
	marketingPacket := knowledgeLoopPacket(
		t, organizationID, marketingAddress, contracts.DepartmentMarketing,
		contracts.SeatExecutor, "wake:marketing-publication", "lease:marketing-publication",
		"intent:marketing-publication",
		knowledgeSkillRef(t, pack, skills.PublicationGatesSkill),
		nil, nil, []contracts.EvidenceRef{evidence.Reference}, now,
	)
	marketingWake, err := runKnowledgeSeat(t, ctx, runner, marketingPacket)
	if err != nil {
		t.Fatal(err)
	}
	input := domainwork.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentMarketing,
		SeatID:         marketingAddress.SeatID,
		IntentID:       marketingPacket.Intent.ID,
		SkillID:        skills.PublicationGatesSkill,
		Objective:      "Approve current evidence-bound owned-channel content",
		Evidence:       []domainwork.ExpiringEvidence{evidence},
		SourceDigest:   marketingWake.PacketDigest,
		Draft: domainwork.Draft{
			Summary: "Owned-channel update ready for owner publication review",
			Campaign: &domainwork.CampaignDraft{
				Audience:           "Existing consenting customers",
				Channels:           []string{"owned-web"},
				Content:            "A factual current product update",
				PerformanceMetrics: []string{"qualified engagement"},
			},
			Publication: &domainwork.PublicationAuthorization{
				BatchID:        "approval:missing",
				CostMicrounits: 1,
				IdempotencyKey: "consume:publication",
			},
			PerformanceReceiptID: "performance:owned-web",
		},
	}
	if _, err := service.Execute(ctx, input); err == nil {
		t.Fatal("public action passed without current owner approval")
	}

	batch := approval.BatchApproval{
		SchemaVersion:              contracts.SchemaVersionV1,
		BatchID:                    "approval:marketing-publication",
		TenantID:                   tenant,
		OrganizationID:             organizationID,
		IntentIDs:                  []contracts.IntentID{marketingPacket.Intent.ID},
		AggregateCeilingMicrounits: 1,
		ExpiresAt:                  now.Add(30 * time.Minute),
		OwnerID:                    ownerID,
	}
	if err := approval.SignBatch(&batch, "key:marketing-owner", ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := approvalStore.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	input.Draft.Publication.BatchID = batch.BatchID
	published, err := service.Execute(ctx, input)
	if err != nil {
		t.Fatal(err)
	}
	if published.Outcome != "approved_for_publication" || published.RequiresHuman {
		t.Fatalf("publication gate result = %#v", published)
	}
	replayed, err := service.Execute(ctx, input)
	if err != nil || replayed.Artifact.Hash != published.Artifact.Hash {
		t.Fatalf("idempotent approval replay = %#v, %v", replayed, err)
	}

	legalAddress := contracts.SeatAddress{
		OrganizationID: organizationID,
		DepartmentID:   "department:legal",
		SeatID:         "seat:legal-lead",
	}
	legalPacket := knowledgeLoopPacket(
		t, organizationID, legalAddress, contracts.DepartmentLegal,
		contracts.SeatLead, "wake:legal-review", "lease:legal-review",
		"intent:legal-review",
		knowledgeSkillRef(t, pack, skills.JurisdictionCheckSkill),
		nil, []contracts.ArtifactRef{published.Artifact},
		[]contracts.EvidenceRef{evidence.Reference}, now,
	)
	legalWake, err := runKnowledgeSeat(t, ctx, runner, legalPacket)
	if err != nil {
		t.Fatal(err)
	}
	legal, err := service.Execute(ctx, domainwork.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentLegal,
		SeatID:         legalAddress.SeatID,
		IntentID:       legalPacket.Intent.ID,
		SkillID:        skills.JurisdictionCheckSkill,
		Objective:      "Spot jurisdiction-specific issues in the publication proposal",
		Evidence:       []domainwork.ExpiringEvidence{evidence},
		SourceDigest:   published.Artifact.Hash,
		Draft: domainwork.Draft{
			Summary: "Qualified human review is required before relying on this analysis",
			Legal: &domainwork.LegalDraft{
				Jurisdictions: []string{"DE"},
				Issues:        []string{"consumer disclosure wording"},
				Analysis:      "The proposed wording may require jurisdiction-specific review",
				Disclaimer:    "This model analysis is not final legal advice.",
				HumanSignoff:  true,
			},
			UnresolvedRisks: []string{"Qualified counsel must confirm the final wording"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if legal.Outcome != "requires_human" || !legal.RequiresHuman {
		t.Fatalf("legal result = %#v", legal)
	}

	correctionOf := published.Artifact.Hash
	corrected := input
	corrected.IntentID = "intent:marketing-correction"
	corrected.SourceDigest = legal.Artifact.Hash
	corrected.CorrectionOf = &correctionOf
	corrected.SkillID = skills.ContentOperationsSkill
	corrected.Draft.Publication = nil
	corrected.Draft.Summary = "Corrected non-public content proposal pending renewed approval"
	corrected.Draft.Campaign.Content = "A corrected factual product update"
	corrected.Draft.PerformanceReceiptID = "performance:owned-web:corrected"
	correctedResult, err := service.Execute(ctx, corrected)
	if err != nil {
		t.Fatal(err)
	}
	if correctedResult.Artifact.Hash == published.Artifact.Hash ||
		marketingWake.WakeID == legalWake.WakeID {
		t.Fatalf(
			"correction/fresh wakes published=%s corrected=%s marketing=%s legal=%s",
			published.Artifact.Hash.Digest, correctedResult.Artifact.Hash.Digest,
			marketingWake.WakeID, legalWake.WakeID,
		)
	}
}

func domainIntegrationEvidence(now time.Time) domainwork.ExpiringEvidence {
	return domainwork.ExpiringEvidence{
		Reference: contracts.EvidenceRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:owned-channel",
			Hash:          developerHash("owned-channel-evidence"),
			Kind:          "channel_observation",
			ObservedAt:    now.Add(-time.Minute),
		},
		ExpiresAt: now.Add(time.Hour),
	}
}
