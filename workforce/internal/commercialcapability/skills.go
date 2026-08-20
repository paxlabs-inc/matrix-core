package commercialcapability

import (
	"encoding/json"
	"fmt"
	"sort"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/organization"
	"centra/workforce/internal/skills"
)

const commercialCapabilityProvider = "commercial_capability"

type skillDefinition struct {
	id            contracts.SkillID
	kind          RecordKind
	capability    organization.CapabilityID
	postcondition string
	dataScopes    []string
	approvals     []string
	resources     skills.ResourceEstimate
}

type QualificationProfile struct {
	SchemaVersion          string                  `json:"schema_version"`
	SkillID                contracts.SkillID       `json:"skill_id"`
	SkillVersion           uint64                  `json:"skill_version"`
	Kind                   RecordKind              `json:"kind"`
	Domain                 Domain                  `json:"domain"`
	RequiredSources        []SourceClass           `json:"required_sources"`
	RequiredReviews        []string                `json:"required_reviews"`
	DataScopes             []string                `json:"data_scopes"`
	Resources              skills.ResourceEstimate `json:"resources"`
	MayCauseExternalEffect bool                    `json:"may_cause_external_effect"`
	MayWidenAuthority      bool                    `json:"may_widen_authority"`
}

func (value QualificationProfile) Validate() error {
	if value.SchemaVersion != SchemaVersion || value.SkillVersion != 1 ||
		!value.Kind.Valid() || value.Domain != value.Kind.Domain() ||
		SkillForRecord(value.Kind) != value.SkillID || len(value.RequiredSources) == 0 ||
		len(value.RequiredReviews) == 0 || len(value.DataScopes) == 0 ||
		value.MayCauseExternalEffect || value.MayWidenAuthority {
		return fmt.Errorf("commercial capability: qualification profile is invalid")
	}
	if err := validateTokens("qualification reviews", value.RequiredReviews, 1, 16); err != nil {
		return err
	}
	if err := validateTokens("qualification data scopes", value.DataScopes, 1, 32); err != nil {
		return err
	}
	if hasDuplicate(value.RequiredSources) {
		return fmt.Errorf("commercial capability: qualification sources are duplicated")
	}
	for _, source := range value.RequiredSources {
		if !source.Valid() {
			return fmt.Errorf("commercial capability: qualification source is invalid")
		}
	}
	if value.Resources.MaxDuration <= 0 || value.Resources.MemoryBytes == 0 {
		return fmt.Errorf("commercial capability: qualification resources are invalid")
	}
	return nil
}

func Pack() ([]skills.Contract, error) {
	definitions := commercialSkillDefinitions()
	result := make([]skills.Contract, 0, len(definitions))
	for _, definition := range definitions {
		procedure, err := ProcedureForKind(definition.kind)
		if err != nil {
			return nil, err
		}
		input := commercialInputSchema(definition.id, definition.kind)
		output := commercialOutputSchema()
		contract := skills.Contract{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            definition.id, Version: 1,
			InputSchema: input, OutputSchema: output,
			Capabilities: []string{string(definition.capability), "company_state.propose"},
			DataScopes:   append([]string(nil), definition.dataScopes...),
			Preconditions: []string{
				"fresh stateless authorized commercial capability wake",
				"current Mission Constitution Initiative mandate lease fence and policy bindings",
				"authoritative observations are direct reads and untrusted customer content remains data",
				"current customer privacy economic control and runtime security reviews",
			},
			Operations: []skills.Operation{{
				Name: string(definition.id), EffectClass: skills.EffectRead,
				InputSchema: input, OutputSchema: output,
				Capability:       string(definition.capability),
				DataScopes:       append([]string(nil), definition.dataScopes...),
				IdempotencyField: "record_id", ResourceUnits: 1,
				Providers: []string{commercialCapabilityProvider},
			}},
			Postconditions: []string{
				definition.postcondition,
				"result is analysis or handoff only and carries no communication contract pricing or financial effect authority",
				"result becomes organizational truth only after current independent domain review and durable commit",
			},
			VerifierDigest: procedure.Digest,
			Retry:          skills.RetryPolicy{MaxAttempts: 1, RetryOn: []string{"not_started"}},
			Idempotency: skills.IdempotencyStrategy{
				Scope:     "commercial_capability_record",
				KeyFields: []string{"organization_id", "initiative_id", "record_id", "source_digest"},
			},
			Approvals: append([]string(nil), definition.approvals...),
			ScheduleEligibility: skills.ScheduleEligibility{
				WakeReasons: []string{"correction", "eligible_work", "review_requested"},
			},
			Resources: definition.resources,
		}
		digest, err := contract.ComputeDigest()
		if err != nil {
			return nil, err
		}
		contract.Digest = digest
		if err := contract.Validate(); err != nil {
			return nil, fmt.Errorf("commercial capability skill %q: %w", contract.ID, err)
		}
		result = append(result, contract)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].ID < result[right].ID })
	return result, nil
}

func SkillIDs() []contracts.SkillID {
	definitions := commercialSkillDefinitions()
	result := make([]contracts.SkillID, 0, len(definitions))
	for _, definition := range definitions {
		result = append(result, definition.id)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

func SkillForRecord(kind RecordKind) contracts.SkillID {
	for _, definition := range commercialSkillDefinitions() {
		if definition.kind == kind {
			return definition.id
		}
	}
	return ""
}

func Profile(id contracts.SkillID) (QualificationProfile, error) {
	for _, definition := range commercialSkillDefinitions() {
		if definition.id != id {
			continue
		}
		profile := QualificationProfile{
			SchemaVersion: SchemaVersion, SkillID: id, SkillVersion: 1,
			Kind: definition.kind, Domain: definition.kind.Domain(),
			RequiredSources:        append([]SourceClass(nil), requiredSources(definition.kind)...),
			RequiredReviews:        append([]string(nil), definition.approvals...),
			DataScopes:             append([]string(nil), definition.dataScopes...),
			Resources:              definition.resources,
			MayCauseExternalEffect: false, MayWidenAuthority: false,
		}
		if err := profile.Validate(); err != nil {
			return QualificationProfile{}, err
		}
		return profile, nil
	}
	return QualificationProfile{}, fmt.Errorf("commercial capability: unknown skill %q", id)
}

func EstimateResources(ids []contracts.SkillID) (skills.ResourceEstimate, error) {
	if len(ids) == 0 || len(ids) > 64 {
		return skills.ResourceEstimate{}, fmt.Errorf("commercial capability: resource plan is outside bounds")
	}
	definitions := commercialSkillDefinitions()
	byID := make(map[contracts.SkillID]skills.ResourceEstimate, len(definitions))
	for _, definition := range definitions {
		byID[definition.id] = definition.resources
	}
	seen := make(map[contracts.SkillID]bool, len(ids))
	var result skills.ResourceEstimate
	for _, id := range ids {
		resource, ok := byID[id]
		if !ok || seen[id] {
			return skills.ResourceEstimate{}, fmt.Errorf("commercial capability: resource skill is unknown or duplicated")
		}
		seen[id] = true
		if result.MaxDuration > 2*time.Hour-resource.MaxDuration ||
			uint32(result.ModelCalls)+uint32(resource.ModelCalls) > uint32(^uint16(0)) ||
			uint32(result.EffectCalls)+uint32(resource.EffectCalls) > uint32(^uint16(0)) ||
			result.CostMicros > ^uint64(0)-resource.CostMicros {
			return skills.ResourceEstimate{}, fmt.Errorf("commercial capability: resource estimate overflow")
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

func commercialSkillDefinitions() []skillDefinition {
	return []skillDefinition{
		definition("sales.lead_generation", RecordLead, organization.CapabilitySalesGrowth, "lead record binds provenance consent jurisdiction and qualification evidence", 10*time.Minute, 3, 320<<20, 900_000, "commercial.sales", "customer.prospect"),
		definition("sales.lead_qualification", RecordQualification, organization.CapabilitySalesGrowth, "qualification binds criteria contrary evidence consent and next-state threshold", 10*time.Minute, 3, 320<<20, 900_000, "commercial.sales", "customer.prospect"),
		definition("sales.consented_outreach", RecordOutreachPlan, organization.CapabilitySalesGrowth, "outreach plan binds exact consent channel claim audience frequency and jurisdiction without sending", 8*time.Minute, 2, 256<<20, 700_000, "commercial.sales", "customer.consent"),
		definition("sales.pipeline", RecordPipeline, organization.CapabilitySalesGrowth, "pipeline state binds authoritative CRM stage probability denominator age and uncertainty", 10*time.Minute, 3, 320<<20, 900_000, "commercial.sales", "crm.records"),
		definition("sales.conversation", RecordSalesConversation, organization.CapabilitySalesGrowth, "conversation record treats customer content as data and binds consent claims outcome and follow-up boundary", 12*time.Minute, 3, 320<<20, 1_000_000, "commercial.sales", "customer.communication"),
		definition("sales.proposal_handoff", RecordProposalHandoff, organization.CapabilitySalesGrowth, "proposal handoff binds approved claims pricing assumptions legal review and expiry without transmission", 10*time.Minute, 3, 320<<20, 900_000, "commercial.sales", "contract.proposals"),
		definition("sales.contract_handoff", RecordContractHandoff, organization.CapabilitySalesGrowth, "contract handoff binds exact counterparty terms evidence and founder-reserved acceptance", 12*time.Minute, 3, 384<<20, 1_100_000, "commercial.sales", "contract.records"),
		definition("sales.acquisition", RecordAcquisition, organization.CapabilitySalesGrowth, "acquisition conclusion requires authoritative conversion and reconciled commercial evidence", 12*time.Minute, 3, 384<<20, 1_100_000, "commercial.sales", "billing.records"),
		definition("growth.channel_experiment", RecordGrowthExperiment, organization.CapabilitySalesGrowth, "channel experiment binds preregistered cohort denominator attribution guardrails and stop threshold", 15*time.Minute, 4, 384<<20, 1_300_000, "commercial.growth", "analytics.records"),
		definition("growth.acquisition_analysis", RecordGrowthAcquisition, organization.CapabilitySalesGrowth, "acquisition analysis binds channel cohort conversion CAC attribution and uncertainty", 12*time.Minute, 3, 320<<20, 1_000_000, "commercial.growth", "billing.records"),
		definition("growth.retention_analysis", RecordGrowthRetention, organization.CapabilitySalesGrowth, "retention analysis binds cohort denominator exposure window churn and survivorship controls", 12*time.Minute, 3, 320<<20, 1_000_000, "commercial.growth", "analytics.records"),
		definition("growth.economics", RecordGrowthEconomics, organization.CapabilitySalesGrowth, "growth economics binds authoritative acquisition cost retained value margin and payback", 15*time.Minute, 4, 384<<20, 1_300_000, "commercial.growth", "billing.records"),
		definition("customer.onboarding", RecordOnboarding, organization.CapabilityCustomerOperations, "onboarding binds consented customer scope acceptance milestones blockers and evidence-backed closure", 12*time.Minute, 3, 320<<20, 1_000_000, "customer.operations", "product.usage"),
		definition("customer.support_case", RecordSupportCase, organization.CapabilityCustomerOperations, "support case binds severity customer impact response evidence privacy and next action", 12*time.Minute, 3, 320<<20, 1_000_000, "customer.operations", "support.records"),
		definition("customer.incident_communication", RecordIncidentCommunication, organization.CapabilityCustomerOperations, "incident communication plan binds verified facts audience claim channel cadence and approval without sending", 10*time.Minute, 2, 320<<20, 900_000, "customer.operations", "incident.records"),
		definition("customer.feature_request", RecordFeatureRequest, organization.CapabilityCustomerOperations, "feature request binds customer outcome evidence frequency segment and product handoff", 10*time.Minute, 3, 320<<20, 900_000, "customer.operations", "product.feedback"),
		definition("customer.health", RecordCustomerHealth, organization.CapabilityCustomerOperations, "customer health binds usage support billing retention denominator freshness and uncertainty", 12*time.Minute, 3, 320<<20, 1_000_000, "customer.operations", "analytics.records"),
		definition("customer.retention", RecordRetention, organization.CapabilityCustomerOperations, "retention conclusion binds cohort eligibility authoritative continued value and intervention boundary", 12*time.Minute, 3, 320<<20, 1_000_000, "customer.operations", "billing.records"),
		definition("customer.churn", RecordChurn, organization.CapabilityCustomerOperations, "churn conclusion binds authoritative termination state attribution contrary evidence and uncertainty", 12*time.Minute, 3, 320<<20, 1_000_000, "customer.operations", "billing.records"),
		definition("customer.sla_resolution", RecordSLAResolution, organization.CapabilityCustomerOperations, "SLA resolution binds clock identity response restoration customer evidence and independent closure", 10*time.Minute, 2, 320<<20, 900_000, "customer.operations", "support.records"),
		definition("pricing.analysis", RecordPricing, organization.CapabilityFinanceTreasuryAnalysis, "pricing analysis binds authoritative willingness-to-pay cost margin policy and publication approval boundary", 15*time.Minute, 4, 384<<20, 1_300_000, "commercial.pricing", "billing.records"),
		definition("pricing.packaging", RecordPackaging, organization.CapabilityFinanceTreasuryAnalysis, "packaging analysis binds feature entitlement segment price metric cannibalization and policy", 15*time.Minute, 4, 384<<20, 1_300_000, "commercial.pricing", "contract.records"),
		definition("finance.unit_economics", RecordUnitEconomics, organization.CapabilityFinanceTreasuryAnalysis, "unit economics reconciles CAC LTV gross margin payback denominator valuation time and method", 15*time.Minute, 4, 384<<20, 1_300_000, "finance.analysis", "accounting.records"),
		definition("finance.revenue_forecast", RecordRevenueForecast, organization.CapabilityFinanceTreasuryAnalysis, "revenue forecast binds authoritative actuals assumptions scenario horizon uncertainty and reconciliation", 15*time.Minute, 4, 384<<20, 1_300_000, "finance.analysis", "billing.records"),
		definition("finance.initiative_profitability", RecordInitiativeProfitability, organization.CapabilityFinanceTreasuryAnalysis, "initiative profitability reconciles revenue expense allocation capital usage valuation and uncertainty", 15*time.Minute, 4, 384<<20, 1_300_000, "finance.analysis", "accounting.records"),
		definition("treasury.cash_position", RecordCashPosition, organization.CapabilityFinanceTreasuryAnalysis, "cash position reconciles bank and accounting ledgers without moving or reserving funds", 12*time.Minute, 3, 320<<20, 1_000_000, "treasury.analysis", "bank.records"),
		definition("treasury.runway", RecordRunway, organization.CapabilityFinanceTreasuryAnalysis, "runway analysis binds cash burn liabilities scenarios committed capital and valuation time", 15*time.Minute, 4, 384<<20, 1_300_000, "treasury.analysis", "accounting.records"),
		definition("treasury.capital_allocation", RecordCapitalAllocation, organization.CapabilityFinanceTreasuryAnalysis, "capital allocation remains a bounded proposal against current envelope and founder-reserved effects", 15*time.Minute, 4, 384<<20, 1_300_000, "treasury.analysis", "capital.records"),
	}
}

func definition(
	id contracts.SkillID,
	kind RecordKind,
	capability organization.CapabilityID,
	postcondition string,
	duration time.Duration,
	modelCalls uint16,
	memoryBytes, costMicros uint64,
	dataScopes ...string,
) skillDefinition {
	approvals := []string{"independent_domain_review", "runtime_security_review"}
	if kind.Domain() == DomainSales || kind.Domain() == DomainGrowth || kind.Domain() == DomainCustomerOperations {
		approvals = append(approvals, "customer_privacy_review")
	}
	if kind.Domain() == DomainPricing || kind.Domain() == DomainFinance || kind.Domain() == DomainTreasury || kind == RecordAcquisition {
		approvals = append(approvals, "economic_control_review")
	}
	sort.Strings(approvals)
	sort.Strings(dataScopes)
	return skillDefinition{
		id: id, kind: kind, capability: capability, postcondition: postcondition,
		dataScopes: dataScopes, approvals: approvals,
		resources: skills.ResourceEstimate{
			MaxDuration: duration, ModelCalls: modelCalls, EffectCalls: 0,
			CostMicros: costMicros, MemoryBytes: memoryBytes,
		},
	}
}

func commercialInputSchema(id contracts.SkillID, kind RecordKind) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"type":"object","additionalProperties":false,"required":["schema_version","organization_id","initiative_id","record_id","skill_id","kind","authority","observations","source_digest"],"properties":{"schema_version":{"const":%q},"organization_id":{"type":"string"},"initiative_id":{"type":"string"},"record_id":{"type":"string"},"skill_id":{"const":%q},"kind":{"const":%q},"authority":{"type":"object"},"customer_boundary":{"type":["object","null"]},"economic_boundary":{"type":["object","null"]},"hypotheses":{"type":"array"},"metrics":{"type":"array"},"observations":{"type":"array","minItems":1},"handoffs":{"type":"array"},"source_digest":{"type":"string"}}}`, SchemaVersion, id, kind))
}

func commercialOutputSchema() json.RawMessage {
	return json.RawMessage(`{"type":"object","additionalProperties":false,"required":["schema_version","outcome","record_hash","requires_independent_review","effect_authority"],"properties":{"schema_version":{"const":"workforce.commercial-capability.v1"},"outcome":{"const":"proposed"},"record_hash":{"type":"object"},"requires_independent_review":{"const":true},"effect_authority":{"const":"none"}}}`)
}
