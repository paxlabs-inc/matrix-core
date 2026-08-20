package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"centra/workforce/internal/contracts"
)

const (
	CampaignResearchSkill   contracts.SkillID = "campaign-research"
	CampaignOperationsSkill contracts.SkillID = "campaign-operations"
	ChannelEvidenceSkill    contracts.SkillID = "channel-evidence"
	ContentOperationsSkill  contracts.SkillID = "content-operations"
	PublicationGatesSkill   contracts.SkillID = "publication-gates"
	ComplianceWorkflowSkill contracts.SkillID = "compliance-workflow"
	ContractAnalysisSkill   contracts.SkillID = "contract-analysis"
	IssueSpottingSkill      contracts.SkillID = "issue-spotting"
	JurisdictionCheckSkill  contracts.SkillID = "jurisdiction-check"
)

var marketingSkillIDs = []contracts.SkillID{
	CampaignOperationsSkill, CampaignResearchSkill, ChannelEvidenceSkill,
	ContentOperationsSkill, PublicationGatesSkill,
}

var legalSkillIDs = []contracts.SkillID{
	ComplianceWorkflowSkill, ContractAnalysisSkill, EvidenceReviewSkill,
	IssueSpottingSkill, JurisdictionCheckSkill,
}

func MarketingLegalPack() ([]Contract, error) {
	definitions := []struct {
		id            contracts.SkillID
		department    contracts.DepartmentKind
		capability    string
		postcondition string
		approvals     []string
	}{
		{CampaignResearchSkill, contracts.DepartmentMarketing, "proposal.campaign_research", "research remains bound to current channel evidence", nil},
		{CampaignOperationsSkill, contracts.DepartmentMarketing, "proposal.campaign_operation", "campaign state remains a proposal until separately authorized", nil},
		{ChannelEvidenceSkill, contracts.DepartmentMarketing, "proposal.channel_evidence", "performance observations retain source and expiry", nil},
		{ContentOperationsSkill, contracts.DepartmentMarketing, "proposal.content_operation", "content is reviewable and not publicly distributed", nil},
		{PublicationGatesSkill, contracts.DepartmentMarketing, "publication.gate", "a current owner approval is consumed before publication readiness", []string{"human_publication_approval"}},
		{ComplianceWorkflowSkill, contracts.DepartmentLegal, "proposal.compliance_workflow", "legal workflow remains non-final and requires human signoff", nil},
		{ContractAnalysisSkill, contracts.DepartmentLegal, "proposal.contract_analysis", "contract analysis remains non-final and requires human signoff", nil},
		{IssueSpottingSkill, contracts.DepartmentLegal, "proposal.issue_spotting", "issues cite current evidence and explicit jurisdiction", nil},
		{JurisdictionCheckSkill, contracts.DepartmentLegal, "proposal.jurisdiction_check", "jurisdiction uncertainty escalates to qualified human review", nil},
	}
	result := make([]Contract, 0, len(definitions))
	for _, definition := range definitions {
		inputSchema := marketingLegalInputSchema(definition.id, definition.department)
		outputSchema := marketingLegalOutputSchema()
		operation := Operation{
			Name: string(definition.id), EffectClass: EffectRead,
			InputSchema: inputSchema, OutputSchema: outputSchema,
			Capability:       definition.capability,
			DataScopes:       []string{"department.records", "department.evidence"},
			IdempotencyField: "intent_id", ResourceUnits: 1,
			Providers: []string{"marketing_legal"},
		}
		contract := Contract{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            definition.id, Version: 1,
			InputSchema: inputSchema, OutputSchema: outputSchema,
			Capabilities: []string{definition.capability, "organizational.records.read"},
			DataScopes:   []string{"department.records", "department.evidence"},
			Preconditions: []string{
				"fresh stateless Lead or Executor wake",
				"current mandate and unexpired typed evidence are present",
			},
			Operations:     []Operation{operation},
			Postconditions: []string{definition.postcondition},
			VerifierDigest: marketingLegalVerifierDigest(definition.id),
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
			Approvals: definition.approvals,
			ScheduleEligibility: ScheduleEligibility{
				WakeReasons: []string{"eligible_work", "review_requested", "correction"},
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
			return nil, fmt.Errorf("marketing/legal skill %q: %w", contract.ID, err)
		}
		result = append(result, contract)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})
	return result, nil
}

func MarketingSkillIDs() []contracts.SkillID {
	return append([]contracts.SkillID(nil), marketingSkillIDs...)
}

func LegalSkillIDs() []contracts.SkillID {
	return append([]contracts.SkillID(nil), legalSkillIDs...)
}

func marketingLegalInputSchema(
	skillID contracts.SkillID,
	department contracts.DepartmentKind,
) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type":"object",
		"required":["schema_version","grant","organization_id","department","seat_id","intent_id","skill_id","objective","evidence","source_digest","draft"],
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"grant":{"type":"object"},
			"organization_id":{"type":"string","minLength":1},
			"department":{"type":"string","enum":[%q]},
			"seat_id":{"type":"string","minLength":1},
			"intent_id":{"type":"string","minLength":1},
			"skill_id":{"type":"string","enum":[%q]},
			"objective":{"type":"string","minLength":1},
			"evidence":{"type":"array","minItems":1,"items":{"type":"object"}},
			"source_digest":{"type":"object"},
			"correction_of":{"type":["object","null"]},
			"draft":{"type":"object"}
		},
		"additionalProperties":false
	}`, department, skillID))
}

func marketingLegalOutputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"required":["schema_version","outcome","artifact","evidence","requires_human"],
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"outcome":{"type":"string","enum":["proposed","approved_for_publication","requires_human"]},
			"artifact":{"type":"object"},
			"evidence":{"type":"array","minItems":1,"items":{"type":"object"}},
			"requires_human":{"type":"boolean"}
		},
		"additionalProperties":false
	}`)
}

func marketingLegalVerifierDigest(id contracts.SkillID) contracts.ContentHash {
	sum := sha256.Sum256([]byte("workforce.marketing-legal.verifier.v1\x00" + string(id)))
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}
