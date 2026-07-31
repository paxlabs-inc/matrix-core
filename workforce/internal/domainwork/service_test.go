package domainwork

import (
	"strings"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/skills"
)

func TestInputRejectsExpiredEvidenceCrossMandateAndFinalLegalAdvice(t *testing.T) {
	now := time.Now().UTC()
	evidence := domainEvidence(now)
	base := Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: "organization:domain",
		Department:     contracts.DepartmentMarketing,
		SeatID:         "seat:marketing",
		IntentID:       "intent:campaign",
		SkillID:        skills.ContentOperationsSkill,
		Objective:      "Draft bounded campaign content",
		Evidence:       []ExpiringEvidence{evidence},
		SourceDigest:   domainHash("source"),
		Draft: Draft{
			Summary: "Evidence-bound draft",
			Campaign: &CampaignDraft{
				Audience:           "Existing consenting customers",
				Channels:           []string{"owned-web"},
				Content:            "Current product update",
				PerformanceMetrics: []string{"qualified engagement"},
			},
		},
	}
	if err := base.validateAt(now); err != nil {
		t.Fatal(err)
	}
	expired := base
	expired.Evidence = append([]ExpiringEvidence(nil), base.Evidence...)
	expired.Evidence[0].ExpiresAt = now
	if err := expired.validateAt(now); err == nil {
		t.Fatal("expired channel evidence was accepted")
	}
	crossMandate := base
	crossMandate.SkillID = skills.ContractAnalysisSkill
	if err := crossMandate.validateAt(now); err == nil {
		t.Fatal("Marketing acquired a Legal skill")
	}
	selfCorrection := base
	selfCorrection.CorrectionOf = &selfCorrection.SourceDigest
	if err := selfCorrection.validateAt(now); err == nil {
		t.Fatal("correction targeted current source")
	}
	legal := base
	legal.Department = contracts.DepartmentLegal
	legal.SkillID = skills.ContractAnalysisSkill
	legal.Draft = Draft{
		Summary: "Contract issue analysis",
		Legal: &LegalDraft{
			Jurisdictions: []string{"DE"},
			Issues:        []string{"termination notice"},
			Analysis:      "The clause may require qualified review",
			Disclaimer:    "This is analysis, not final legal advice.",
		},
	}
	if err := legal.validateAt(now); err == nil {
		t.Fatal("legal output without human signoff was accepted")
	}
	legal.Draft.Legal.HumanSignoff = true
	if err := legal.validateAt(now); err != nil {
		t.Fatal(err)
	}
}

func domainEvidence(now time.Time) ExpiringEvidence {
	return ExpiringEvidence{
		Reference: contracts.EvidenceRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:domain",
			Hash:          domainHash("evidence"),
			Kind:          "channel_observation",
			ObservedAt:    now.Add(-time.Minute),
		},
		ExpiresAt: now.Add(time.Hour),
	}
}

func domainHash(value string) contracts.ContentHash {
	character := "a"
	if value == "evidence" {
		character = "b"
	}
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: strings.Repeat(character, 64),
	}
}
