package productcapability

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/skills"
)

type skillDefinition struct {
	id            contracts.SkillID
	kind          ArtifactKind
	capability    string
	postcondition string
	resources     skills.ResourceEstimate
	dataScopes    []string
	approvals     []string
}

// Pack returns the complete Product, Design, Developer, Deployment,
// Reliability, and Business Analytics executable contract set. Existing
// Developer contracts are reused by exact digest rather than reinterpreted.
func Pack() ([]skills.Contract, error) {
	definitions := capabilityDefinitions()
	result := make([]skills.Contract, 0, len(definitions)+8)
	for _, definition := range definitions {
		procedure, err := NewVerifierProcedure(
			"verify."+string(definition.id), definition.kind,
			verifierChecks(definition.kind),
		)
		if err != nil {
			return nil, err
		}
		operation := skills.Operation{
			Name:             string(definition.id),
			EffectClass:      skills.EffectRead,
			InputSchema:      capabilityInputSchema(definition.id, definition.kind),
			OutputSchema:     capabilityOutputSchema(),
			Capability:       definition.capability,
			DataScopes:       append([]string(nil), definition.dataScopes...),
			IdempotencyField: "record_id",
			ResourceUnits:    1,
			Providers:        []string{"product_capability"},
		}
		contract := skills.Contract{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            definition.id, Version: 1,
			InputSchema:  capabilityInputSchema(definition.id, definition.kind),
			OutputSchema: capabilityOutputSchema(),
			Capabilities: []string{definition.capability, "company_state.propose"},
			DataScopes:   append([]string(nil), definition.dataScopes...),
			Preconditions: []string{
				"fresh stateless authorized Lead or Executor wake",
				"current Mission lifecycle Initiative evidence lease fence and source bindings",
				"current domain and runtime security review",
			},
			Operations: []skills.Operation{operation},
			Postconditions: []string{
				definition.postcondition,
				"output remains a proposal until independent verification and durable commit",
			},
			VerifierDigest: procedure.Digest,
			Retry: skills.RetryPolicy{
				MaxAttempts: 1, RetryOn: []string{"not_started"},
			},
			Idempotency: skills.IdempotencyStrategy{
				Scope: "initiative_capability_record",
				KeyFields: []string{
					"organization_id", "initiative_id", "record_id", "source_digest",
				},
			},
			Approvals: append([]string(nil), definition.approvals...),
			ScheduleEligibility: skills.ScheduleEligibility{
				WakeReasons: []string{"eligible_work", "review_requested", "correction", "incident"},
			},
			Resources: definition.resources,
		}
		digest, err := contract.ComputeDigest()
		if err != nil {
			return nil, err
		}
		contract.Digest = digest
		if err := contract.Validate(); err != nil {
			return nil, fmt.Errorf("product capability skill %q: %w", contract.ID, err)
		}
		result = append(result, contract)
	}
	deployment, err := deploymentContract()
	if err != nil {
		return nil, err
	}
	result = append(result, deployment)
	developerPack, err := skills.DeveloperPack()
	if err != nil {
		return nil, err
	}
	result = append(result, developerPack...)
	byID := make(map[contracts.SkillID]skills.Contract, len(result))
	for _, contract := range result {
		if prior, exists := byID[contract.ID]; exists && prior.Digest != contract.Digest {
			return nil, fmt.Errorf("product capability: conflicting skill %q", contract.ID)
		}
		byID[contract.ID] = contract
	}
	result = result[:0]
	for _, contract := range byID {
		result = append(result, contract)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}

// SkillIDs returns the exact version-one capability identifiers without
// exposing mutable package state.
func SkillIDs() []contracts.SkillID {
	definitions := capabilityDefinitions()
	result := make([]contracts.SkillID, 0, len(definitions)+6)
	for _, definition := range definitions {
		result = append(result, definition.id)
	}
	result = append(result,
		"deployment.execute",
		skills.DeveloperPlanSkill,
		skills.DeveloperImplementSkill,
		skills.DeveloperVerifySkill,
		skills.DeveloperReviewHandoffSkill,
		skills.DeveloperBrainUpdateSkill,
	)
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

// EstimateResources returns exact sequential duration, call, and cost totals
// and peak memory for a bounded capability plan. Unknown or duplicate skills
// fail closed rather than acquiring an assumed estimate.
func EstimateResources(ids []contracts.SkillID) (skills.ResourceEstimate, error) {
	if len(ids) == 0 || len(ids) > 64 {
		return skills.ResourceEstimate{}, fmt.Errorf("product capability: resource plan is outside bounds")
	}
	pack, err := Pack()
	if err != nil {
		return skills.ResourceEstimate{}, err
	}
	byID := make(map[contracts.SkillID]skills.ResourceEstimate, len(pack))
	for _, contract := range pack {
		byID[contract.ID] = contract.Resources
	}
	seen := make(map[contracts.SkillID]bool, len(ids))
	var result skills.ResourceEstimate
	maxUint16 := uint32(^uint16(0))
	maxUint64 := ^uint64(0)
	for _, id := range ids {
		resource, ok := byID[id]
		if !ok || seen[id] {
			return skills.ResourceEstimate{}, fmt.Errorf("product capability: resource skill is unknown or duplicated")
		}
		seen[id] = true
		if result.MaxDuration > 2*time.Hour-resource.MaxDuration ||
			uint32(result.ModelCalls)+uint32(resource.ModelCalls) > maxUint16 ||
			uint32(result.EffectCalls)+uint32(resource.EffectCalls) > maxUint16 ||
			result.CostMicros > maxUint64-resource.CostMicros {
			return skills.ResourceEstimate{}, fmt.Errorf("product capability: resource estimate overflow")
		}
		result.MaxDuration += resource.MaxDuration
		result.ModelCalls += resource.ModelCalls
		result.EffectCalls += resource.EffectCalls
		result.CostMicros += resource.CostMicros
		if resource.MemoryBytes > result.MemoryBytes {
			result.MemoryBytes = resource.MemoryBytes
		}
	}
	return result, nil
}

func capabilityDefinitions() []skillDefinition {
	return []skillDefinition{
		definition("product.customer_problem", ArtifactCustomerProblem, "product.customer_problem", "customer problem binds target customer demand evidence uncertainty and expiry", 12*time.Minute, 4, 0, 384<<20, 1_200_000, "product.records", "customer.evidence"),
		definition("product.value_proposition", ArtifactValueProposition, "product.value_proposition", "value proposition binds the verified problem alternatives benefit and falsifier", 10*time.Minute, 3, 0, 320<<20, 900_000, "product.records", "customer.evidence"),
		definition("product.requirements", ArtifactRequirements, "product.requirements", "requirements bind acceptance business gates constraints and traceable evidence", 18*time.Minute, 5, 0, 512<<20, 1_800_000, "product.records", "project.requirements"),
		definition("product.roadmap", ArtifactRoadmap, "product.roadmap", "roadmap binds priorities dependencies evidence windows resources and stop conditions", 15*time.Minute, 4, 0, 384<<20, 1_500_000, "product.records", "portfolio.records"),
		definition("product.prioritize", ArtifactPriorityDecision, "product.prioritize", "prioritization records alternatives thresholds dissent and opportunity cost", 10*time.Minute, 3, 0, 320<<20, 1_000_000, "product.records", "portfolio.records"),
		definition("product.user_research", ArtifactUserResearch, "product.user_research", "research binds consented sources method sample limitations and contradictory findings", 20*time.Minute, 5, 0, 512<<20, 2_200_000, "product.records", "customer.evidence"),
		definition("product.analytics", ArtifactProductAnalytics, "product.analytics", "analytics binds exact metric definitions denominator attribution freshness and uncertainty", 12*time.Minute, 3, 0, 320<<20, 1_100_000, "product.records", "analytics.records"),
		definition("product.customer_outcome_acceptance", ArtifactCustomerOutcomeAcceptance, "product.customer_outcome_acceptance", "customer outcome acceptance binds preregistered authoritative thresholds and stop conditions", 10*time.Minute, 3, 0, 320<<20, 1_000_000, "product.records", "customer.evidence"),
		definition("design.user_journey", ArtifactUserJourney, "design.user_journey", "journey binds actors context states failure paths accessibility and evidence", 15*time.Minute, 4, 0, 384<<20, 1_500_000, "design.records", "customer.evidence"),
		definition("design.interaction_model", ArtifactInteractionModel, "design.interaction_model", "interaction model binds states actions system feedback errors and recovery", 18*time.Minute, 5, 0, 512<<20, 1_900_000, "design.records", "project.requirements"),
		definition("design.prototype", ArtifactPrototype, "design.prototype", "prototype is content addressed and binds scope fidelity assumptions and test purpose", 20*time.Minute, 5, 0, 640<<20, 2_100_000, "design.records", "project.artifacts"),
		definition("design.usability", ArtifactUsabilityEvidence, "design.usability", "usability evidence binds method participants tasks observations failures and uncertainty", 20*time.Minute, 5, 0, 512<<20, 2_200_000, "design.records", "customer.evidence"),
		definition("design.accessibility", ArtifactAccessibilityEvidence, "design.accessibility", "accessibility evidence binds applicable criteria assistive paths findings and remediation", 15*time.Minute, 3, 0, 384<<20, 1_400_000, "design.records", "project.verification"),
		definition("design.system_decision", ArtifactDesignSystemDecision, "design.system_decision", "design-system decisions bind current primitives constraints compatibility and evidence", 10*time.Minute, 3, 0, 320<<20, 1_000_000, "design.records", "project.source"),
		definition("design.build_handoff", ArtifactDesignHandoff, "design.build_handoff", "design handoff binds journeys interactions prototype accessibility requirements and acceptance", 12*time.Minute, 3, 0, 320<<20, 1_200_000, "design.records", "project.requirements"),
		definition("developer.implementation_plan", ArtifactImplementationPlan, "developer.implementation_plan", "implementation plan binds current Project Brain source scope blast radius leases and gates", 15*time.Minute, 4, 0, 512<<20, 1_600_000, "project.brain", "project.source"),
		definition("developer.release_preparation", ArtifactReleasePlan, "developer.release_preparation", "release preparation binds exact build deployment rollback observation and incident procedures", 12*time.Minute, 3, 0, 384<<20, 1_300_000, "project.brain", "deployment.records"),
		definition("deployment.release_plan", ArtifactReleasePlan, "deployment.release_plan", "release plan binds environment source build rollout health rollback and authority", 12*time.Minute, 3, 0, 384<<20, 1_300_000, "deployment.records", "project.source"),
		definition("deployment.health", ArtifactHealthEvidence, "deployment.health", "health evidence binds authoritative service observations window thresholds and ambiguity", 8*time.Minute, 1, 1, 256<<20, 600_000, "deployment.records", "telemetry.observations"),
		definition("reliability.incident", ArtifactIncidentEvidence, "reliability.incident", "incident record binds detection impact response ambiguity recovery and escalation", 10*time.Minute, 2, 0, 320<<20, 900_000, "reliability.records", "telemetry.observations"),
		definition("reliability.capacity", ArtifactCapacityEvidence, "reliability.capacity", "capacity evidence binds production demand resources limits headroom and expiry", 10*time.Minute, 2, 1, 320<<20, 800_000, "reliability.records", "telemetry.observations"),
		definition("reliability.observability", ArtifactObservabilityEvidence, "reliability.observability", "observability evidence binds metric trace log identity coverage retention and access", 10*time.Minute, 2, 1, 320<<20, 800_000, "reliability.records", "telemetry.observations"),
		definition("reliability.verify", ArtifactReliabilityEvidence, "reliability.verify", "reliability verification binds SLO health capacity incident rollback and independent evidence", 12*time.Minute, 2, 1, 320<<20, 900_000, "reliability.records", "telemetry.observations"),
		definition("analytics.metric_definition", ArtifactProductAnalytics, "analytics.metric_definition", "metric definition binds identity source denominator attribution freshness uncertainty and reconciliation", 10*time.Minute, 2, 0, 256<<20, 700_000, "analytics.records", "company.state"),
		definition("analytics.observation", ArtifactTelemetryEvidence, "analytics.observation", "observation binds the exact metric version source time denominator and uncertainty", 8*time.Minute, 1, 1, 256<<20, 500_000, "analytics.records", "telemetry.observations"),
		definition("analytics.reconcile", ArtifactTelemetryEvidence, "analytics.reconcile", "reconciliation records conflicts gaps freshness and authoritative disposition", 10*time.Minute, 2, 1, 320<<20, 700_000, "analytics.records", "telemetry.observations"),
		definition("analytics.compare", ArtifactProductAnalytics, "analytics.compare", "comparison rejects incompatible metric versions denominators attribution or sources", 8*time.Minute, 2, 0, 256<<20, 600_000, "analytics.records", "company.state"),
	}
}

func definition(
	id contracts.SkillID,
	kind ArtifactKind,
	capability, postcondition string,
	duration time.Duration,
	modelCalls, effectCalls uint16,
	memoryBytes, costMicros uint64,
	dataScopes ...string,
) skillDefinition {
	return skillDefinition{
		id: id, kind: kind, capability: capability, postcondition: postcondition,
		resources: skills.ResourceEstimate{
			MaxDuration: duration, ModelCalls: modelCalls, EffectCalls: effectCalls,
			CostMicros: costMicros, MemoryBytes: memoryBytes,
		},
		dataScopes: dataScopes,
		approvals:  []string{"independent_domain_review", "runtime_security_review"},
	}
}

func deploymentContract() (skills.Contract, error) {
	procedure, err := NewVerifierProcedure(
		"verify.deployment.execute", ArtifactDeploymentState,
		[]string{"content_addressed", "evidence_present", "fresh", "scope_bound", "source_fresh", "independent_review_required"},
	)
	if err != nil {
		return skills.Contract{}, err
	}
	schema := deploymentInputSchema()
	output := capabilityOutputSchema()
	operations := []skills.Operation{
		{
			Name: "observe_deployment", EffectClass: skills.EffectRead,
			InputSchema: schema, OutputSchema: output,
			Capability:       "deployment.observe",
			DataScopes:       []string{"deployment.records", "telemetry.observations"},
			IdempotencyField: "idempotency_key", ResourceUnits: 1,
			Providers: []string{"deployment"},
		},
		{
			Name: "deploy_release", EffectClass: skills.EffectReversible,
			InputSchema: schema, OutputSchema: output,
			Capability:       "deployment.execute",
			DataScopes:       []string{"deployment.records", "project.source"},
			IdempotencyField: "idempotency_key", Compensation: "rollback_release",
			ResourceUnits: 1, Providers: []string{"deployment"},
		},
		{
			Name: "rollback_release", EffectClass: skills.EffectReversible,
			InputSchema: schema, OutputSchema: output,
			Capability:       "deployment.rollback",
			DataScopes:       []string{"deployment.records", "project.source"},
			IdempotencyField: "idempotency_key", Compensation: "deploy_release",
			ResourceUnits: 1, Providers: []string{"deployment"},
		},
	}
	contract := skills.Contract{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "deployment.execute", Version: 1,
		InputSchema: schema, OutputSchema: output,
		Capabilities: []string{"deployment.execute", "deployment.rollback", "deployment.observe"},
		DataScopes:   []string{"deployment.records", "project.source", "telemetry.observations"},
		Preconditions: []string{
			"fresh stateless authorized Deployment Lead or Executor wake",
			"current source build tests release authority lease fence and rollback target",
			"single fenced deployment effect adapter owns credentials",
		},
		Operations: operations,
		Postconditions: []string{
			"deployment and rollback remain externally observed states with ambiguity preserved",
			"code completion and request acceptance never become launch evidence",
		},
		VerifierDigest: procedure.Digest,
		Probe: &skills.ProbeContract{
			Operation: "observe_deployment", OutputSchema: output,
			Authority: "deployment_provider_observation", Timeout: 2 * time.Minute,
			ReadOnly: true, Authoritative: true,
			VerifierDigest: procedure.Digest, UnavailableMeans: skills.ProbeUnknown,
		},
		Retry: skills.RetryPolicy{
			MaxAttempts: 2, Backoff: 5 * time.Second,
			RetryOn: []string{"not_started", "provider_unavailable"},
		},
		Idempotency: skills.IdempotencyStrategy{
			Scope: "deployment_environment_release",
			KeyFields: []string{
				"organization_id", "initiative_id", "environment", "release_id", "source_digest",
			},
			ProviderID: true,
		},
		Approvals: []string{"deployment_authority", "independent_release_review", "runtime_security_review"},
		ScheduleEligibility: skills.ScheduleEligibility{
			WakeReasons: []string{"eligible_work", "review_requested", "incident", "correction"},
		},
		Resources: skills.ResourceEstimate{
			MaxDuration: 20 * time.Minute, ModelCalls: 1, EffectCalls: 3,
			CostMicros: 1_500_000, MemoryBytes: 384 << 20,
		},
	}
	digest, err := contract.ComputeDigest()
	if err != nil {
		return skills.Contract{}, err
	}
	contract.Digest = digest
	return contract, contract.Validate()
}

func verifierChecks(kind ArtifactKind) []string {
	checks := []string{
		"content_addressed", "evidence_present", "fresh", "scope_bound",
		"independent_review_required",
	}
	if engineeringArtifact(kind) {
		checks = append(checks, "source_fresh")
	}
	return checks
}

func capabilityInputSchema(id contracts.SkillID, kind ArtifactKind) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{
		"type":"object",
		"required":[
			"schema_version","grant","organization_id","initiative_id",
			"project_id","workspace_id","seat_id","intent_id","skill_id","idempotency_key",
			"objective","record_id","kind","summary","evidence","data_scopes",
			"source","observed_at","effective_at","fresh_until"
		],
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"grant":{"type":"object"},
			"organization_id":{"type":"string","minLength":1},
			"initiative_id":{"type":"string","minLength":1},
			"project_id":{"type":"string","minLength":1},
			"workspace_id":{"type":"string","minLength":1},
			"seat_id":{"type":"string","minLength":1},
			"intent_id":{"type":"string","minLength":1},
			"skill_id":{"type":"string","enum":[%q]},
			"idempotency_key":{"type":"string","minLength":1},
			"objective":{"type":"string","minLength":1,"maxLength":4096},
			"record_id":{"type":"string","minLength":1},
			"kind":{"type":"string","enum":[%q]},
			"summary":{"type":"string","minLength":1,"maxLength":4096},
			"evidence":{"type":"array","minItems":1,"maxItems":128,"items":{"type":"object"}},
			"data_scopes":{"type":"array","minItems":1,"maxItems":32,"items":{"type":"string"}},
			"source":{"type":["object","null"]},
			"observed_at":{"type":"string","minLength":1},
			"effective_at":{"type":"string","minLength":1},
			"fresh_until":{"type":"string","minLength":1}
		},
		"additionalProperties":false
	}`, id, kind))
}

func deploymentInputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"required":[
			"schema_version","grant","organization_id","initiative_id",
			"environment","release_id","source_digest","build_evidence",
			"test_evidence","rollback_target","health_procedure",
			"idempotency_key","deadline"
		],
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.v1"]},
			"grant":{"type":"object"},
			"organization_id":{"type":"string","minLength":1},
			"initiative_id":{"type":"string","minLength":1},
			"environment":{"type":"string","minLength":1},
			"release_id":{"type":"string","minLength":1},
			"source_digest":{"type":"object"},
			"build_evidence":{"type":"object"},
			"test_evidence":{"type":"object"},
			"rollback_target":{"type":"string","minLength":1},
			"health_procedure":{"type":"string","minLength":1},
			"idempotency_key":{"type":"string","minLength":1},
			"deadline":{"type":"string","minLength":1}
		},
		"additionalProperties":false
	}`)
}

func capabilityOutputSchema() json.RawMessage {
	return json.RawMessage(`{
		"type":"object",
		"required":["schema_version","outcome","artifact","evidence","requires_human"],
		"properties":{
			"schema_version":{"type":"string","enum":["workforce.product-capability.v1"]},
			"outcome":{"type":"string","enum":["proposed","observed","ambiguous","requires_human"]},
			"artifact":{"type":"object"},
			"evidence":{"type":"array","minItems":1,"items":{"type":"object"}},
			"requires_human":{"type":"boolean"}
		},
		"additionalProperties":false
	}`)
}
