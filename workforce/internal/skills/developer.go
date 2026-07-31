package skills

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	DeveloperPlanSkill          contracts.SkillID = "developer.plan"
	DeveloperImplementSkill     contracts.SkillID = "developer.implement"
	DeveloperVerifySkill        contracts.SkillID = "developer.verify"
	DeveloperReviewHandoffSkill contracts.SkillID = "developer.review_handoff"
	DeveloperBrainUpdateSkill   contracts.SkillID = "developer.project_brain_update"
)

// DeveloperPack returns the complete versioned Lead and Executor skill pack.
func DeveloperPack() ([]Contract, error) {
	definitions := []struct {
		id             contracts.SkillID
		operations     []Operation
		capabilities   []string
		postconditions []string
		approvals      []string
		probe          *ProbeContract
	}{
		{
			id:           DeveloperPlanSkill,
			capabilities: []string{"codegraph.read", "project_brain.read"},
			operations: []Operation{
				developerOperation("plan_change", EffectRead, "codegraph.read", ""),
			},
			postconditions: []string{
				"plan binds current source, task, files, symbols, blast radius, and verification",
			},
		},
		{
			id: DeveloperImplementSkill,
			capabilities: []string{
				"codegraph.read", "source.read", "source.write.scoped",
			},
			operations: []Operation{
				developerOperation("inspect_source", EffectRead, "source.read", ""),
				developerOperation(
					"apply_scoped_change", EffectReversible,
					"source.write.scoped", "restore_source_snapshot",
				),
				developerOperation(
					"restore_source_snapshot", EffectReversible,
					"source.write.scoped", "apply_scoped_change",
				),
			},
			postconditions: []string{
				"only fenced files and symbols changed",
				"source hashes and affected tests are recorded",
			},
			probe: developerProbe("inspect_source"),
		},
		{
			id:           DeveloperVerifySkill,
			capabilities: []string{"codegraph.read", "verification.run"},
			operations: []Operation{
				developerOperation("run_verification", EffectRead, "verification.run", ""),
			},
			postconditions: []string{
				"real affected tests and declared quality gates produce hashed evidence",
			},
		},
		{
			id:           DeveloperReviewHandoffSkill,
			capabilities: []string{"source.read", "handoff.write"},
			operations: []Operation{
				developerOperation("inspect_handoff", EffectRead, "source.read", ""),
				developerOperation(
					"publish_review_handoff", EffectIrreversible, "handoff.write", "",
				),
			},
			postconditions: []string{
				"handoff binds intent, source, blast radius, evidence, and unresolved risk",
			},
			approvals: []string{"independent_developer_review"},
			probe:     developerProbe("inspect_handoff"),
		},
		{
			id:           DeveloperBrainUpdateSkill,
			capabilities: []string{"project_brain.read", "project_brain.propose"},
			operations: []Operation{
				developerOperation(
					"inspect_project_brain", EffectRead, "project_brain.read", "",
				),
				developerOperation(
					"propose_verified_record", EffectIrreversible,
					"project_brain.propose", "",
				),
			},
			postconditions: []string{
				"only typed source-grounded independently verified records become truth",
			},
			approvals: []string{"independent_engineering_verification"},
			probe:     developerProbe("inspect_project_brain"),
		},
	}
	result := make([]Contract, 0, len(definitions))
	for _, definition := range definitions {
		contract := Contract{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            definition.id, Version: 1,
			InputSchema:  developerInputSchema(""),
			OutputSchema: developerOutputSchema(),
			Capabilities: definition.capabilities,
			DataScopes: []string{
				"developer.project", "developer.workspace", "developer.task",
				"developer.file", "developer.symbol",
			},
			Preconditions: []string{
				"fresh stateless Developer Lead or Executor wake",
				"active fenced Developer change-scope lease",
				"current source and CodeGraph generation",
			},
			Operations:     definition.operations,
			Postconditions: definition.postconditions,
			VerifierDigest: developerVerifierDigest(definition.id),
			Probe:          definition.probe,
			Retry: RetryPolicy{
				MaxAttempts: 1,
				RetryOn:     []string{"not_started"},
			},
			Idempotency: IdempotencyStrategy{
				Scope: "developer_task",
				KeyFields: []string{
					"organization_id", "project_id", "workspace_id", "task_id",
					"source_root",
				},
			},
			Approvals: definition.approvals,
			ScheduleEligibility: ScheduleEligibility{
				WakeReasons: []string{"eligible_work", "review_requested", "correction"},
			},
			Resources: ResourceEstimate{
				MaxDuration: 30 * time.Minute, ModelCalls: 8,
				EffectCalls: uint16(len(definition.operations)),
				CostMicros:  5_000_000, MemoryBytes: 512 << 20,
			},
		}
		digest, err := contract.ComputeDigest()
		if err != nil {
			return nil, err
		}
		contract.Digest = digest
		if err := contract.Validate(); err != nil {
			return nil, fmt.Errorf("developer skill %q: %w", contract.ID, err)
		}
		result = append(result, contract)
	}
	return result, nil
}

func developerOperation(
	name string,
	class EffectClass,
	capability, compensation string,
) Operation {
	return Operation{
		Name: name, EffectClass: class,
		InputSchema: developerInputSchema(name), OutputSchema: developerOutputSchema(),
		Capability: capability,
		DataScopes: []string{
			"developer.project", "developer.workspace", "developer.task",
			"developer.file", "developer.symbol",
		},
		IdempotencyField: "task_id", Compensation: compensation, ResourceUnits: 1,
		Providers:      []string{"developer"},
		CostMicrounits: irreversibleCost(class),
	}
}

// irreversibleCost is the owner-signed price of one resource unit. Only an
// irreversible operation spends against an approved ceiling.
func irreversibleCost(class EffectClass) uint64 {
	if class == EffectIrreversible {
		return 1
	}
	return 0
}

func developerProbe(operation string) *ProbeContract {
	return &ProbeContract{
		Operation: operation, OutputSchema: developerOutputSchema(),
		Authority: "current_source_and_ledger", Timeout: 2 * time.Minute,
		ReadOnly: true, Authoritative: true,
		VerifierDigest: developerVerifierDigest(
			contracts.SkillID("probe." + operation),
		),
		UnavailableMeans: ProbeUnknown,
	}
}

func developerInputSchema(operation string) json.RawMessage {
	required := []string{"schema_version", "grant"}
	switch operation {
	case "apply_scoped_change", "restore_source_snapshot":
		required = append(required, "changes")
	case "run_verification":
		required = append(required, "verification")
	case "inspect_handoff", "inspect_project_brain":
		required = append(required, "brain_grant")
	case "publish_review_handoff", "propose_verified_record":
		required = append(required, "brain_grant", "record")
	}
	requiredJSON, _ := json.Marshal(required)
	return json.RawMessage(fmt.Sprintf(`{
		"type":"object",
		"required":%s,
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"grant":{
				"type":"object",
				"required":["lease","scope"],
				"properties":{
					"lease":{
						"type":"object",
						"required":[
							"ID","WakeID","OrganizationID","SeatID","NodeID",
							"MandateID","MandateVersion","Policies","IssuedAt",
							"ExpiresAt","Fence","State"
						],
						"properties":{
							"ID":{"type":"string","minLength":1},
							"WakeID":{"type":"string","minLength":1},
							"OrganizationID":{"type":"string","minLength":1},
							"SeatID":{"type":"string","minLength":1},
							"NodeID":{"type":"string","minLength":1},
							"MandateID":{"type":"string","minLength":1},
							"MandateVersion":{"type":"integer"},
							"Policies":{"type":"array","minItems":1,"items":{"type":"object"}},
							"IssuedAt":{"type":"string","minLength":1},
							"ExpiresAt":{"type":"string","minLength":1},
							"Fence":{"type":"integer"},
							"State":{"type":"string","enum":["active"]},
							"RenewedAt":{"type":["string","null"]}
						},
						"additionalProperties":false
					},
					"scope":{
						"type":"object",
						"required":[
							"schema_version","project_id","workspace_id","task_node_id",
							"workspace_root","source","files","symbols","blast_radius",
							"affected_tests","capability","resolved_at"
						],
						"properties":{
							"schema_version":{"type":"string","enum":["workforce.v1"]},
							"project_id":{"type":"string","minLength":1},
							"workspace_id":{"type":"string","minLength":1},
							"task_node_id":{"type":"string","minLength":1},
							"workspace_root":{"type":"string","minLength":1},
							"source":{
								"type":"object",
								"required":[
									"schema_version","root_digest","graph_digest","generation",
									"indexed_at","captured_at","fresh","pending_files","files",
									"node_count","edge_count"
								],
								"properties":{
									"schema_version":{"type":"string","enum":["workforce.v1"]},
									"root_digest":{"type":"object"},
									"graph_digest":{"type":"object"},
									"generation":{"type":"integer"},
									"indexed_at":{"type":"string","minLength":1},
									"captured_at":{"type":"string","minLength":1},
									"fresh":{"type":"boolean"},
									"pending_files":{"type":["array","null"],"items":{"type":"string"}},
									"files":{"type":"array","minItems":1,"items":{"type":"object"}},
									"node_count":{"type":"integer"},
									"edge_count":{"type":"integer"}
								},
								"additionalProperties":false
							},
							"files":{"type":"array","minItems":1,"items":{"type":"object"}},
							"symbols":{"type":"array","minItems":1,"items":{"type":"string","minLength":1}},
							"blast_radius":{"type":"array","minItems":1,"items":{"type":"object"}},
							"affected_tests":{"type":["array","null"],"items":{"type":"string"}},
							"coordination_plan_id":{"type":["string","null"]},
							"coordination":{"type":["object","null"]},
							"coordination_grant":{"type":["object","null"]},
							"capability":{
								"type":"object",
								"required":[
									"schema_version","grant_id","tenant_id","organization_id",
									"project_id","workspace_id","workspace_root","filter",
									"operation","requester_seat_id","requester_seat_version",
									"requester_seat_did","requester_binding_id",
									"requester_binding_version","purpose","issued_at",
									"expires_at","signature"
								],
								"properties":{
									"schema_version":{"type":"string","enum":["workforce.v1"]},
									"grant_id":{"type":"string","minLength":1},
									"tenant_id":{"type":"string","minLength":1},
									"organization_id":{"type":"string","minLength":1},
									"project_id":{"type":"string","minLength":1},
									"workspace_id":{"type":"string","minLength":1},
									"workspace_root":{"type":"string","minLength":1},
									"filter":{"type":"string"},
									"operation":{"type":"string","enum":["change_scope"]},
									"requester_seat_id":{"type":"string","minLength":1},
									"requester_seat_version":{"type":"integer","minimum":1},
									"requester_seat_did":{"type":"string","minLength":1},
									"requester_binding_id":{"type":"string","minLength":1},
									"requester_binding_version":{"type":"integer","minimum":1},
									"record_id":{"type":["string","null"]},
									"author":{"type":["object","null"]},
									"verifier":{"type":["object","null"]},
									"purpose":{"type":"string","minLength":1},
									"after_record_id":{"type":["string","null"]},
									"max_records":{"type":"integer"},
									"issued_at":{"type":"string","minLength":1},
									"expires_at":{"type":"string","minLength":1},
									"signature":{"type":"object"}
								},
								"additionalProperties":false
							},
							"resolved_at":{"type":"string","minLength":1}
						},
						"additionalProperties":false
					}
				},
				"additionalProperties":false
			},
			"changes":{
				"type":"array","minItems":1,"maxItems":64,
				"items":{
					"type":"object",
					"required":["path","before_hash","content"],
					"properties":{
						"path":{"type":"string","minLength":1},
						"before_hash":{"type":"object"},
						"content":{"type":"string","minLength":1}
					},
					"additionalProperties":false
				}
			},
			"verification":{"type":"string","minLength":1},
			"brain_grant":{"type":["object","null"]},
			"probe_grant":{"type":["object","null"]},
			"record":{"type":["object","null"]}
		},
		"additionalProperties":false
	}`, requiredJSON))
}

func developerOutputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"required":["outcome","evidence_hash"],
			"properties":{
				"outcome":{"enum":["completed","rejected","partial","ambiguous"]},
				"evidence_hash":{"type":"string","pattern":"^[a-f0-9]{64}$"},
				"observation":{}
			},
		"additionalProperties":false
	}`)
}

func developerVerifierDigest(id contracts.SkillID) contracts.ContentHash {
	sum := sha256.Sum256([]byte("workforce.developer.verifier.v1\x00" + string(id)))
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}
