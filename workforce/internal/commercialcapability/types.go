package commercialcapability

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

const (
	SchemaVersion           = "workforce.commercial-capability.v1"
	CheckpointSchemaVersion = "workforce.commercial-checkpoint.v1"
	ReviewSchemaVersion     = "workforce.commercial-review.v1"
)

type RecordID string
type ChainID string
type ObservationID string
type MetricID string
type HandoffID string
type CheckpointID string
type WorkflowID string
type InitiativeID string
type CustomerRef string

type Domain string

const (
	DomainSales              Domain = "sales"
	DomainGrowth             Domain = "growth"
	DomainCustomerOperations Domain = "customer_operations"
	DomainPricing            Domain = "pricing"
	DomainFinance            Domain = "finance"
	DomainTreasury           Domain = "treasury"
)

func (value Domain) Valid() bool {
	switch value {
	case DomainSales, DomainGrowth, DomainCustomerOperations, DomainPricing,
		DomainFinance, DomainTreasury:
		return true
	default:
		return false
	}
}

type RecordKind string

const (
	RecordLead                    RecordKind = "lead"
	RecordQualification           RecordKind = "qualification"
	RecordOutreachPlan            RecordKind = "outreach_plan"
	RecordPipeline                RecordKind = "pipeline"
	RecordSalesConversation       RecordKind = "sales_conversation"
	RecordProposalHandoff         RecordKind = "proposal_handoff"
	RecordContractHandoff         RecordKind = "contract_handoff"
	RecordAcquisition             RecordKind = "acquisition"
	RecordGrowthExperiment        RecordKind = "growth_experiment"
	RecordGrowthAcquisition       RecordKind = "growth_acquisition"
	RecordGrowthRetention         RecordKind = "growth_retention"
	RecordGrowthEconomics         RecordKind = "growth_economics"
	RecordOnboarding              RecordKind = "onboarding"
	RecordSupportCase             RecordKind = "support_case"
	RecordIncidentCommunication   RecordKind = "incident_communication"
	RecordFeatureRequest          RecordKind = "feature_request"
	RecordCustomerHealth          RecordKind = "customer_health"
	RecordRetention               RecordKind = "retention"
	RecordChurn                   RecordKind = "churn"
	RecordSLAResolution           RecordKind = "sla_resolution"
	RecordPricing                 RecordKind = "pricing"
	RecordPackaging               RecordKind = "packaging"
	RecordUnitEconomics           RecordKind = "unit_economics"
	RecordCashPosition            RecordKind = "cash_position"
	RecordRunway                  RecordKind = "runway"
	RecordCapitalAllocation       RecordKind = "capital_allocation"
	RecordRevenueForecast         RecordKind = "revenue_forecast"
	RecordInitiativeProfitability RecordKind = "initiative_profitability"
)

func (value RecordKind) Valid() bool {
	switch value {
	case RecordLead, RecordQualification, RecordOutreachPlan, RecordPipeline,
		RecordSalesConversation, RecordProposalHandoff, RecordContractHandoff,
		RecordAcquisition, RecordGrowthExperiment, RecordGrowthAcquisition,
		RecordGrowthRetention, RecordGrowthEconomics, RecordOnboarding,
		RecordSupportCase, RecordIncidentCommunication, RecordFeatureRequest,
		RecordCustomerHealth, RecordRetention, RecordChurn, RecordSLAResolution,
		RecordPricing, RecordPackaging, RecordUnitEconomics, RecordCashPosition,
		RecordRunway, RecordCapitalAllocation, RecordRevenueForecast,
		RecordInitiativeProfitability:
		return true
	default:
		return false
	}
}

func (value RecordKind) Domain() Domain {
	switch value {
	case RecordLead, RecordQualification, RecordOutreachPlan, RecordPipeline,
		RecordSalesConversation, RecordProposalHandoff, RecordContractHandoff,
		RecordAcquisition:
		return DomainSales
	case RecordGrowthExperiment, RecordGrowthAcquisition, RecordGrowthRetention,
		RecordGrowthEconomics:
		return DomainGrowth
	case RecordOnboarding, RecordSupportCase, RecordIncidentCommunication,
		RecordFeatureRequest, RecordCustomerHealth, RecordRetention, RecordChurn,
		RecordSLAResolution:
		return DomainCustomerOperations
	case RecordPricing, RecordPackaging:
		return DomainPricing
	case RecordUnitEconomics, RecordRevenueForecast, RecordInitiativeProfitability:
		return DomainFinance
	case RecordCashPosition, RecordRunway, RecordCapitalAllocation:
		return DomainTreasury
	default:
		return ""
	}
}

type ObservationKind string

const (
	ObservationLeadSource          ObservationKind = "lead_source"
	ObservationConsent             ObservationKind = "consent"
	ObservationCRMState            ObservationKind = "crm_state"
	ObservationConversation        ObservationKind = "conversation"
	ObservationProposal            ObservationKind = "proposal"
	ObservationContract            ObservationKind = "contract"
	ObservationConversion          ObservationKind = "conversion"
	ObservationProductUsage        ObservationKind = "product_usage"
	ObservationSupport             ObservationKind = "support"
	ObservationSLA                 ObservationKind = "sla"
	ObservationCustomerHealth      ObservationKind = "customer_health"
	ObservationRetention           ObservationKind = "retention"
	ObservationChurn               ObservationKind = "churn"
	ObservationPrice               ObservationKind = "price"
	ObservationCost                ObservationKind = "cost"
	ObservationRevenue             ObservationKind = "revenue"
	ObservationCash                ObservationKind = "cash"
	ObservationLiability           ObservationKind = "liability"
	ObservationCapital             ObservationKind = "capital"
	ObservationForecastActual      ObservationKind = "forecast_actual"
	ObservationInitiativeEconomics ObservationKind = "initiative_economics"
)

func (value ObservationKind) Valid() bool {
	switch value {
	case ObservationLeadSource, ObservationConsent, ObservationCRMState,
		ObservationConversation, ObservationProposal, ObservationContract,
		ObservationConversion, ObservationProductUsage, ObservationSupport,
		ObservationSLA, ObservationCustomerHealth, ObservationRetention,
		ObservationChurn, ObservationPrice, ObservationCost, ObservationRevenue,
		ObservationCash, ObservationLiability, ObservationCapital,
		ObservationForecastActual, ObservationInitiativeEconomics:
		return true
	default:
		return false
	}
}

func (value ObservationKind) Economic() bool {
	switch value {
	case ObservationPrice, ObservationCost, ObservationRevenue, ObservationCash,
		ObservationLiability, ObservationCapital, ObservationForecastActual,
		ObservationInitiativeEconomics:
		return true
	default:
		return false
	}
}

type SourceClass string

const (
	SourceConsentRegistry    SourceClass = "consent_registry"
	SourceCRM                SourceClass = "crm"
	SourceContractRepository SourceClass = "contract_repository"
	SourceSupportSystem      SourceClass = "support_system"
	SourceProductAnalytics   SourceClass = "product_analytics"
	SourceBillingLedger      SourceClass = "billing_ledger"
	SourceAccountingLedger   SourceClass = "accounting_ledger"
	SourceBankLedger         SourceClass = "bank_ledger"
	SourceProviderAPI        SourceClass = "provider_api"
)

func (value SourceClass) Valid() bool {
	switch value {
	case SourceConsentRegistry, SourceCRM, SourceContractRepository,
		SourceSupportSystem, SourceProductAnalytics, SourceBillingLedger,
		SourceAccountingLedger, SourceBankLedger, SourceProviderAPI:
		return true
	default:
		return false
	}
}

func (value SourceClass) FinancialAuthority() bool {
	switch value {
	case SourceBillingLedger, SourceAccountingLedger, SourceBankLedger:
		return true
	default:
		return false
	}
}

type OutcomeKind string

const (
	OutcomeActivity          OutcomeKind = "activity"
	OutcomeOutput            OutcomeKind = "output"
	OutcomeCustomer          OutcomeKind = "customer_outcome"
	OutcomeCommercial        OutcomeKind = "commercial_outcome"
	OutcomeEconomic          OutcomeKind = "economic_outcome"
	OutcomeRisk              OutcomeKind = "risk_outcome"
	OutcomeStrategicLearning OutcomeKind = "strategic_learning"
)

func (value OutcomeKind) Valid() bool {
	switch value {
	case OutcomeActivity, OutcomeOutput, OutcomeCustomer, OutcomeCommercial,
		OutcomeEconomic, OutcomeRisk, OutcomeStrategicLearning:
		return true
	default:
		return false
	}
}

type ConsentStatus string

const (
	ConsentGranted   ConsentStatus = "granted"
	ConsentWithdrawn ConsentStatus = "withdrawn"
	ConsentExpired   ConsentStatus = "expired"
	ConsentUnknown   ConsentStatus = "unknown"
)

func (value ConsentStatus) Valid() bool {
	switch value {
	case ConsentGranted, ConsentWithdrawn, ConsentExpired, ConsentUnknown:
		return true
	default:
		return false
	}
}

type HandoffKind string

const (
	HandoffGrowthToSales      HandoffKind = "growth_to_sales"
	HandoffSalesToCustomerOps HandoffKind = "sales_to_customer_operations"
	HandoffSalesToContract    HandoffKind = "sales_to_contract_review"
	HandoffCustomerToProduct  HandoffKind = "customer_operations_to_product"
	HandoffPricingToSales     HandoffKind = "pricing_to_sales"
	HandoffFinanceToExecutive HandoffKind = "finance_to_executive"
	HandoffTreasuryToFinance  HandoffKind = "treasury_to_finance"
)

func (value HandoffKind) Valid() bool {
	switch value {
	case HandoffGrowthToSales, HandoffSalesToCustomerOps, HandoffSalesToContract,
		HandoffCustomerToProduct, HandoffPricingToSales, HandoffFinanceToExecutive,
		HandoffTreasuryToFinance:
		return true
	default:
		return false
	}
}

func (value HandoffKind) Domains() (Domain, Domain) {
	switch value {
	case HandoffGrowthToSales:
		return DomainGrowth, DomainSales
	case HandoffSalesToCustomerOps:
		return DomainSales, DomainCustomerOperations
	case HandoffSalesToContract:
		return DomainSales, "legal_compliance"
	case HandoffCustomerToProduct:
		return DomainCustomerOperations, "product"
	case HandoffPricingToSales:
		return DomainPricing, DomainSales
	case HandoffFinanceToExecutive:
		return DomainFinance, "executive"
	case HandoffTreasuryToFinance:
		return DomainTreasury, DomainFinance
	default:
		return "", ""
	}
}

var founderReservedActions = []string{
	"capital_commitment",
	"contract_acceptance",
	"custody_transfer",
	"debt_or_leverage",
	"funds_movement",
	"new_financial_venue",
	"price_publication",
	"unrestricted_trading",
	"withdrawal",
}

func FounderReservedActions() []string {
	return append([]string(nil), founderReservedActions...)
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("commercial capability: %s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' || char == '/' {
			continue
		}
		return fmt.Errorf("commercial capability: %s contains an invalid character", name)
	}
	return nil
}

func validateTokens(name string, values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("commercial capability: %s must contain %d to %d entries", name, minimum, maximum)
	}
	seen := make(map[string]bool, len(values))
	for _, value := range values {
		if err := validateToken(name, value); err != nil {
			return err
		}
		if seen[value] {
			return fmt.Errorf("commercial capability: %s contains duplicates", name)
		}
		seen[value] = true
	}
	return nil
}

func validateTexts(name string, values []string, minimum, maximum, maxBytes int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("commercial capability: %s must contain %d to %d entries", name, minimum, maximum)
	}
	for _, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > maxBytes {
			return fmt.Errorf("commercial capability: %s contains an invalid entry", name)
		}
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func sortedUnique[T ~string](values []T) bool {
	return slices.IsSorted(values) && !hasDuplicate(values)
}

func hasDuplicate[T comparable](values []T) bool {
	seen := make(map[T]bool, len(values))
	for _, value := range values {
		if seen[value] {
			return true
		}
		seen[value] = true
	}
	return false
}

func contains[T comparable](values []T, wanted T) bool {
	return slices.Contains(values, wanted)
}
