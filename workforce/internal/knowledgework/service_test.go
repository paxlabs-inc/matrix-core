package knowledgework

import (
	"context"
	"fmt"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/skills"
)

func TestServiceBuildsEvidenceBoundExperimentAndTypedHandoff(t *testing.T) {
	now := time.Now().UTC()
	service, err := New(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	evidence := knowledgeEvidence(now)
	experiment, err := service.Execute(context.Background(), Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: "organization:knowledge",
		Department:     contracts.DepartmentResearch,
		SeatID:         "seat:research-executor", IntentID: "intent:experiment",
		SkillID:      skills.ExperimentDesignSkill,
		Objective:    "Test whether a bounded onboarding change improves activation",
		Constraints:  []string{"No production publication or spending"},
		Evidence:     []contracts.EvidenceRef{evidence},
		SourceDigest: knowledgeHash("source"),
		Draft: Draft{
			Summary: "A bounded offline experiment with an explicit stop condition",
			Findings: []Finding{{
				Statement:   "The current activation baseline is measured",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			Experiment: &ExperimentDesign{
				Hypothesis:      "The change increases activation",
				Method:          "Run an offline replay against consented historical events",
				SuccessMetrics:  []string{"activation delta"},
				StopConditions:  []string{"data quality falls below threshold"},
				MaximumDuration: "P7D", RequiresHumanRun: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if experiment.Outcome != "requires_human" ||
		experiment.Artifact.Hash == (contracts.ContentHash{}) ||
		len(experiment.Payload) == 0 {
		t.Fatalf("experiment result = %#v", experiment)
	}

	handoff, err := service.Execute(context.Background(), Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: "organization:knowledge",
		Department:     contracts.DepartmentExecutive,
		SeatID:         "seat:executive-lead", IntentID: "intent:handoff",
		SkillID:      skills.TypedHandoffSkill,
		Objective:    "Send the evidence review to Research and Development",
		Constraints:  []string{"Handoff grants no approval or effect authority"},
		Evidence:     []contracts.EvidenceRef{evidence},
		SourceDigest: experiment.Artifact.Hash,
		Draft: Draft{
			Summary: "Evidence-bound R&D handoff",
			Findings: []Finding{{
				Statement:   "Experiment design requires independent evidence review",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			Handoff: &HandoffDraft{
				RecipientDepartment: contracts.DepartmentResearch,
				RecipientSeatID:     "seat:research-lead",
				Subject:             "Review bounded experiment",
				RequiredAction:      "Return typed evidence review",
				TimeoutAction:       contracts.TimeoutEscalate,
				ExpiresAt:           now.Add(time.Hour),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if handoff.Handoff == nil || handoff.RequiresHuman {
		t.Fatalf("handoff result = %#v", handoff)
	}
}

func TestServiceRejectsCrossMandateUngroundedAndSelfTargetedCorrections(t *testing.T) {
	now := time.Now().UTC()
	service, err := New(func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	evidence := knowledgeEvidence(now)
	base := Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: "organization:knowledge",
		Department:     contracts.DepartmentExecutive,
		SeatID:         "seat:executive-lead", IntentID: "intent:analysis",
		SkillID:      skills.PortfolioAnalysisSkill,
		Objective:    "Compare current portfolio evidence",
		Constraints:  []string{"No approval authority"},
		Evidence:     []contracts.EvidenceRef{evidence},
		SourceDigest: knowledgeHash("source"),
		Draft: Draft{
			Summary: "Grounded analysis",
			Findings: []Finding{{
				Statement:   "Current evidence supports bounded investigation",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
		},
	}
	crossMandate := base
	crossMandate.SkillID = skills.ExperimentDesignSkill
	if _, err := service.Execute(context.Background(), crossMandate); err == nil {
		t.Fatal("Executive acquired R&D experiment-design authority")
	}
	ungrounded := base
	ungrounded.Draft.Findings[0].EvidenceIDs = []contracts.EvidenceID{"missing"}
	if _, err := service.Execute(context.Background(), ungrounded); err == nil {
		t.Fatal("ungrounded finding was accepted")
	}
	selfCorrection := base
	selfCorrection.CorrectionOf = &selfCorrection.SourceDigest
	if _, err := service.Execute(context.Background(), selfCorrection); err == nil {
		t.Fatal("correction targeted its current source instead of a prior artifact")
	}
}

func knowledgeEvidence(now time.Time) contracts.EvidenceRef {
	return contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "evidence:knowledge", Hash: knowledgeHash("evidence"),
		Kind: "analysis", ObservedAt: now,
	}
}

func knowledgeHash(value string) contracts.ContentHash {
	hash, err := contracts.HashCanonical(&knowledgeHashValue{Value: value})
	if err != nil {
		panic(err)
	}
	return hash
}

type knowledgeHashValue struct {
	Value string `json:"value"`
}

func (value knowledgeHashValue) Validate() error {
	if value.Value == "" {
		return fmt.Errorf("value is required")
	}
	return nil
}
