package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	PortfolioPlanningSkill contracts.SkillID = "portfolio-planning"
	PortfolioAnalysisSkill contracts.SkillID = "portfolio-analysis"
	EvidenceReviewSkill    contracts.SkillID = "evidence-review"
	ExperimentDesignSkill  contracts.SkillID = "experiment-design"
	ResearchAnalysisSkill  contracts.SkillID = "research-analysis"
	TypedHandoffSkill      contracts.SkillID = "typed-handoff"
)

var executiveSkillIDs = []contracts.SkillID{
	EvidenceReviewSkill, PortfolioAnalysisSkill, PortfolioPlanningSkill, TypedHandoffSkill,
}

var researchSkillIDs = []contracts.SkillID{
	EvidenceReviewSkill, ExperimentDesignSkill, ResearchAnalysisSkill, TypedHandoffSkill,
}

// ExecutiveResearchPack returns the deduplicated executable knowledge-work
// contracts shared by Executive and Research and Development.
func ExecutiveResearchPack() ([]Contract, error) {
	definitions := []struct {
		id            contracts.SkillID
		capability    string
		postcondition string
	}{
		{PortfolioPlanningSkill, "proposal.portfolio_plan", "priorities remain recommendations without approval authority"},
		{PortfolioAnalysisSkill, "proposal.portfolio_analysis", "portfolio conclusions cite current typed evidence"},
		{EvidenceReviewSkill, "proposal.evidence_review", "evidence gaps and expiry remain explicit"},
		{ExperimentDesignSkill, "proposal.experiment_design", "hypothesis method metrics and stop conditions are bounded"},
		{ResearchAnalysisSkill, "proposal.research_analysis", "research findings bind source evidence and uncertainty"},
		{TypedHandoffSkill, "mail.draft", "handoff binds sender recipient intent artifacts evidence action and expiry"},
	}
	result := make([]Contract, 0, len(definitions))
	for _, definition := range definitions {
		operation := Operation{
			Name:             string(definition.id),
			EffectClass:      EffectRead,
			InputSchema:      knowledgeInputSchema(definition.id),
			OutputSchema:     knowledgeOutputSchema(),
			Capability:       definition.capability,
			DataScopes:       []string{"department.records", "department.evidence"},
			IdempotencyField: "intent_id",
			ResourceUnits:    1,
			Providers:        []string{"knowledge"},
		}
		contract := Contract{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            definition.id,
			Version:       1,
			InputSchema:   knowledgeInputSchema(definition.id),
			OutputSchema:  knowledgeOutputSchema(),
			Capabilities:  []string{definition.capability, "organizational.records.read"},
			DataScopes:    []string{"department.records", "department.evidence"},
			Preconditions: []string{
				"fresh stateless Lead or Executor wake",
				"current mandate and typed evidence are present",
			},
			Operations:     []Operation{operation},
			Postconditions: []string{definition.postcondition},
			VerifierDigest: knowledgeVerifierDigest(definition.id),
			Retry: RetryPolicy{
				MaxAttempts: 1, RetryOn: []string{"not_started"},
			},
			Idempotency: IdempotencyStrategy{
				Scope: "department_intent",
				KeyFields: []string{
					"organization_id", "department", "seat_id", "intent_id",
					"source_digest",
				},
			},
			ScheduleEligibility: ScheduleEligibility{
				WakeReasons: []string{
					"eligible_work", "review_requested", "correction",
				},
			},
			Resources: ResourceEstimate{
				MaxDuration: 20 * time.Minute, ModelCalls: 4,
				EffectCalls: 0, CostMicros: 2_000_000, MemoryBytes: 256 << 20,
			},
		}
		digest, err := contract.ComputeDigest()
		if err != nil {
			return nil, err
		}
		contract.Digest = digest
		if err := contract.Validate(); err != nil {
			return nil, fmt.Errorf("knowledge skill %q: %w", contract.ID, err)
		}
		result = append(result, contract)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})
	return result, nil
}

// ExecutiveSkillIDs returns the exact IDs allowed by seeded Executive mandates.
func ExecutiveSkillIDs() []contracts.SkillID {
	return append([]contracts.SkillID(nil), executiveSkillIDs...)
}

// ResearchSkillIDs returns the exact IDs allowed by seeded R&D mandates.
func ResearchSkillIDs() []contracts.SkillID {
	return append([]contracts.SkillID(nil), researchSkillIDs...)
}

func knowledgeInputSchema(skillID contracts.SkillID) json.RawMessage {
	required := []string{
		"schema_version", "grant", "organization_id", "department", "seat_id",
		"intent_id", "skill_id", "objective", "constraints", "evidence", "source_digest",
		"draft",
	}
	requiredJSON, _ := json.Marshal(required)
	return json.RawMessage(fmt.Sprintf(`{
		"type":"object",
		"required":%s,
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"grant":{"type":"object"},
			"organization_id":{"type":"string","minLength":1},
				"department":{"type":"string","enum":["executive","research_and_development","legal"]},
			"seat_id":{"type":"string","minLength":1},
			"intent_id":{"type":"string","minLength":1},
			"skill_id":{"type":"string","enum":[%q]},
			"objective":{"type":"string","minLength":1},
			"constraints":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},
			"evidence":{"type":"array","minItems":1,"items":{"type":"object"}},
			"source_digest":{"type":"object"},
			"correction_of":{"type":["object","null"]},
			"draft":{"type":"object"}
		},
		"additionalProperties":false
	}`, requiredJSON, skillID))
}

func knowledgeOutputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"required":["schema_version","outcome","artifact","evidence","requires_human"],
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"outcome":{"type":"string","enum":["proposed","requires_human"]},
			"artifact":{"type":"object"},
			"evidence":{"type":"array","minItems":1,"items":{"type":"object"}},
			"handoff":{"type":["object","null"]},
			"requires_human":{"type":"boolean"}
		},
		"additionalProperties":false
	}`)
}

func knowledgeVerifierDigest(id contracts.SkillID) contracts.ContentHash {
	sum := sha256.Sum256([]byte("workforce.knowledge.verifier.v1\x00" + string(id)))
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}
