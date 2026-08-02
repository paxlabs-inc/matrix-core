package commercialcapability

import (
	"encoding/json"
	"fmt"
	"slices"

	"matrix/workforce/internal/contracts"
)

func ProcedureForRecord(body RecordBody) (ReviewProcedure, error) {
	if !body.Kind.Valid() || body.Kind.Domain() != body.Domain {
		return ReviewProcedure{}, fmt.Errorf("commercial capability: cannot derive review procedure for invalid record")
	}
	return ProcedureForKind(body.Kind)
}

func ProcedureForKind(kind RecordKind) (ReviewProcedure, error) {
	domain := kind.Domain()
	if !domain.Valid() {
		return ReviewProcedure{}, fmt.Errorf("commercial capability: cannot derive review procedure for invalid kind")
	}
	checks := []string{
		"authority_boundary_exact",
		"canonical_record_signature",
		"evidence_freshness",
		"independent_source_reconciliation",
		"outcome_derivation",
		"pre_registered_thresholds",
		"source_is_authoritative_not_narrative",
	}
	sources := requiredSources(kind)
	slices.Sort(checks)
	slices.Sort(sources)
	procedure := ReviewProcedure{
		SchemaVersion:   ReviewSchemaVersion,
		ID:              "review.commercial." + string(kind),
		Version:         1,
		Domain:          domain,
		Checks:          checks,
		RequiredSources: sources,
	}
	digest, err := reviewProcedureDigest(procedure)
	if err != nil {
		return ReviewProcedure{}, err
	}
	procedure.Digest = digest
	if err := procedure.Validate(); err != nil {
		return ReviewProcedure{}, err
	}
	return procedure, nil
}

func requiredSources(kind RecordKind) []SourceClass {
	switch kind {
	case RecordLead, RecordQualification, RecordOutreachPlan, RecordPipeline,
		RecordSalesConversation, RecordProposalHandoff:
		return []SourceClass{SourceConsentRegistry, SourceCRM}
	case RecordContractHandoff:
		return []SourceClass{SourceConsentRegistry, SourceCRM, SourceContractRepository}
	case RecordAcquisition:
		return []SourceClass{SourceBillingLedger, SourceConsentRegistry, SourceCRM}
	case RecordGrowthExperiment, RecordGrowthAcquisition, RecordGrowthRetention,
		RecordGrowthEconomics:
		return []SourceClass{SourceBillingLedger, SourceProductAnalytics}
	case RecordOnboarding, RecordFeatureRequest:
		return []SourceClass{SourceProductAnalytics, SourceSupportSystem}
	case RecordSupportCase, RecordIncidentCommunication, RecordSLAResolution:
		return []SourceClass{SourceSupportSystem}
	case RecordCustomerHealth, RecordRetention, RecordChurn:
		return []SourceClass{SourceBillingLedger, SourceProductAnalytics, SourceSupportSystem}
	case RecordPricing, RecordPackaging:
		return []SourceClass{SourceBillingLedger, SourceContractRepository}
	case RecordUnitEconomics, RecordRevenueForecast, RecordInitiativeProfitability:
		return []SourceClass{SourceAccountingLedger, SourceBillingLedger}
	case RecordCashPosition, RecordRunway, RecordCapitalAllocation:
		return []SourceClass{SourceAccountingLedger, SourceBankLedger}
	default:
		return nil
	}
}

func reviewProcedureDigest(value ReviewProcedure) (contracts.ContentHash, error) {
	copyValue := value
	copyValue.Digest = contracts.ContentHash{}
	encoded, err := json.Marshal(copyValue)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("commercial capability: encode review procedure: %w", err)
	}
	return digestBytes(encoded), nil
}

func validateRequiredSources(body RecordBody, procedure ReviewProcedure) error {
	observed := make(map[SourceClass]bool, len(body.Observations)*2)
	for _, observation := range body.Observations {
		observed[observation.Primary.Class] = true
		if observation.Reconciliation != nil {
			observed[observation.Reconciliation.Class] = true
		}
	}
	if body.Customer != nil {
		observed[SourceConsentRegistry] = true
	}
	for _, required := range procedure.RequiredSources {
		if !observed[required] {
			return fmt.Errorf("commercial capability: required authoritative source %q is absent", required)
		}
	}
	return nil
}
