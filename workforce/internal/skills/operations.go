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
	BookkeepingSkill            contracts.SkillID = "bookkeeping"
	CloseWorkflowSkill          contracts.SkillID = "close-workflow"
	PaymentProposalSkill        contracts.SkillID = "payment-proposal"
	ReconciliationSkill         contracts.SkillID = "reconciliation"
	ReportingSkill              contracts.SkillID = "reporting"
	AdministrativeWorkflowSkill contracts.SkillID = "administrative-workflow"
	RecordsSkill                contracts.SkillID = "records"
	SchedulingSkill             contracts.SkillID = "scheduling"
	SLATrackingSkill            contracts.SkillID = "sla-tracking"
	VendorCoordinationSkill     contracts.SkillID = "vendor-coordination"
)

var accountingSkillIDs = []contracts.SkillID{
	BookkeepingSkill, CloseWorkflowSkill, PaymentProposalSkill,
	ReconciliationSkill, ReportingSkill,
}

var backOfficeSkillIDs = []contracts.SkillID{
	AdministrativeWorkflowSkill, RecordsSkill, SchedulingSkill,
	SLATrackingSkill, VendorCoordinationSkill,
}

func OperationsPack() ([]Contract, error) {
	definitions := []struct {
		id         contracts.SkillID
		department contracts.DepartmentKind
		capability string
		approval   []string
	}{
		{BookkeepingSkill, contracts.DepartmentAccounting, "proposal.bookkeeping", nil},
		{CloseWorkflowSkill, contracts.DepartmentAccounting, "proposal.close_workflow", nil},
		{PaymentProposalSkill, contracts.DepartmentAccounting, "payment.gate", []string{"human_payment_approval"}},
		{ReconciliationSkill, contracts.DepartmentAccounting, "proposal.reconciliation", nil},
		{ReportingSkill, contracts.DepartmentAccounting, "proposal.reporting", nil},
		{AdministrativeWorkflowSkill, contracts.DepartmentBackOffice, "proposal.administrative_workflow", nil},
		{RecordsSkill, contracts.DepartmentBackOffice, "proposal.records", nil},
		{SchedulingSkill, contracts.DepartmentBackOffice, "proposal.scheduling", nil},
		{SLATrackingSkill, contracts.DepartmentBackOffice, "proposal.sla_tracking", nil},
		{VendorCoordinationSkill, contracts.DepartmentBackOffice, "proposal.vendor_coordination", nil},
	}
	result := make([]Contract, 0, len(definitions))
	for _, definition := range definitions {
		input := operationsInputSchema(definition.id, definition.department)
		output := operationsOutputSchema()
		contract := Contract{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            definition.id, Version: 1,
			InputSchema: input, OutputSchema: output,
			Capabilities: []string{definition.capability, "organizational.records.read"},
			DataScopes:   []string{"department.records", "department.evidence"},
			Preconditions: []string{
				"fresh stateless Lead or Executor wake",
				"current mandate and unexpired typed evidence are present",
			},
			Operations: []Operation{{
				Name: string(definition.id), EffectClass: EffectRead,
				InputSchema: input, OutputSchema: output,
				Capability:       definition.capability,
				DataScopes:       []string{"department.records", "department.evidence"},
				IdempotencyField: "intent_id", ResourceUnits: 1,
				Providers: []string{"operations"},
			}},
			Postconditions: []string{
				"the result remains evidence-backed and performs no external mutation",
			},
			VerifierDigest: operationsVerifierDigest(definition.id),
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
			Approvals: definition.approval,
			ScheduleEligibility: ScheduleEligibility{
				WakeReasons: []string{
					"eligible_work", "scheduled", "review_requested", "correction",
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
			return nil, fmt.Errorf("operations skill %q: %w", contract.ID, err)
		}
		result = append(result, contract)
	}
	sort.Slice(result, func(left, right int) bool {
		return result[left].ID < result[right].ID
	})
	return result, nil
}

func AccountingSkillIDs() []contracts.SkillID {
	return append([]contracts.SkillID(nil), accountingSkillIDs...)
}

func BackOfficeSkillIDs() []contracts.SkillID {
	return append([]contracts.SkillID(nil), backOfficeSkillIDs...)
}

func operationsInputSchema(
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

func operationsOutputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"required":["schema_version","outcome","artifact","evidence","requires_human"],
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"outcome":{"type":"string","enum":["proposed","requires_human","approved_for_payment_dispatch"]},
			"artifact":{"type":"object"},
			"evidence":{"type":"array","minItems":1,"items":{"type":"object"}},
			"requires_human":{"type":"boolean"}
		},
		"additionalProperties":false
	}`)
}

func operationsVerifierDigest(id contracts.SkillID) contracts.ContentHash {
	sum := sha256.Sum256([]byte("workforce.operations.verifier.v1\x00" + string(id)))
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}
