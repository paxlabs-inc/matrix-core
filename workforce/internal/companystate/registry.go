package companystate

import (
	"fmt"
	"slices"
)

type FieldSpec struct {
	Name     string
	Kind     ValueKind
	Required bool
}

type RecordSchema struct {
	Kind   RecordKind
	Domain Domain
	Fields []FieldSpec
}

func SchemaFor(kind RecordKind) (RecordSchema, error) {
	domain, err := DomainFor(kind)
	if err != nil {
		return RecordSchema{}, err
	}
	fields, err := fieldsFor(kind)
	if err != nil {
		return RecordSchema{}, err
	}
	slices.SortFunc(fields, func(left, right FieldSpec) int {
		if left.Name < right.Name {
			return -1
		}
		if left.Name > right.Name {
			return 1
		}
		return 0
	})
	return RecordSchema{Kind: kind, Domain: domain, Fields: fields}, nil
}

func Registry() []RecordSchema {
	kinds := AllRecordKinds()
	result := make([]RecordSchema, 0, len(kinds))
	for _, kind := range kinds {
		schema, _ := SchemaFor(kind)
		result = append(result, schema)
	}
	return result
}

func validateAttributes(kind RecordKind, attributes []Attribute) error {
	schema, err := SchemaFor(kind)
	if err != nil {
		return err
	}
	if len(attributes) == 0 || len(attributes) > len(schema.Fields) {
		return fmt.Errorf("company state: %s attributes must contain 1 to %d values", kind, len(schema.Fields))
	}
	allowed := make(map[string]FieldSpec, len(schema.Fields))
	seen := make(map[string]bool, len(attributes))
	for _, field := range schema.Fields {
		allowed[field.Name] = field
	}
	for index := range attributes {
		if err := attributes[index].Validate(); err != nil {
			return fmt.Errorf("company state: attribute %d: %w", index, err)
		}
		if index > 0 && attributes[index-1].Name >= attributes[index].Name {
			return fmt.Errorf("company state: attributes must be sorted and unique by name")
		}
		field, found := allowed[attributes[index].Name]
		if !found {
			return fmt.Errorf("company state: attribute %q is not defined for %s", attributes[index].Name, kind)
		}
		if field.Kind != attributes[index].Kind {
			return fmt.Errorf("company state: attribute %q must have type %s", field.Name, field.Kind)
		}
		seen[field.Name] = true
	}
	for _, field := range schema.Fields {
		if field.Required && !seen[field.Name] {
			return fmt.Errorf("company state: required %s attribute %q is missing", kind, field.Name)
		}
	}
	return nil
}

func required(name string, kind ValueKind) FieldSpec {
	return FieldSpec{Name: name, Kind: kind, Required: true}
}

func optional(name string, kind ValueKind) FieldSpec {
	return FieldSpec{Name: name, Kind: kind, Required: false}
}

func fieldsFor(kind RecordKind) ([]FieldSpec, error) {
	switch kind {
	case RecordMarket:
		return []FieldSpec{
			required("name", ValueText), required("geographies", ValueTextSet),
			optional("description", ValueText), optional("estimated_size", ValueMoneyMinor),
			optional("growth_rate", ValueBasisPoints),
		}, nil
	case RecordCustomerSegment:
		return []FieldSpec{
			required("name", ValueText), required("problem", ValueText),
			required("qualification_criteria", ValueTextSet),
			optional("market", ValueRecordReference),
		}, nil
	case RecordCustomer:
		return []FieldSpec{
			required("name", ValueText), required("status", ValueIdentifier),
			required("jurisdiction", ValueIdentifier),
			optional("segment", ValueRecordReference),
		}, nil
	case RecordLead:
		return []FieldSpec{
			required("stage", ValueIdentifier), required("source", ValueText),
			optional("customer", ValueRecordReference),
			optional("segment", ValueRecordReference),
		}, nil
	case RecordOpportunity:
		return []FieldSpec{
			required("stage", ValueIdentifier), required("estimated_value", ValueMoneyMinor),
			required("probability", ValueBasisPoints), optional("customer", ValueRecordReference),
			optional("expected_close_at", ValueTimestamp),
		}, nil
	case RecordDemandSignal:
		return []FieldSpec{
			required("signal", ValueText), required("strength", ValueBasisPoints),
			required("segment", ValueRecordReference), optional("frequency", ValueInteger),
		}, nil
	case RecordCompetitor:
		return []FieldSpec{
			required("name", ValueText), required("positioning", ValueText),
			optional("products", ValueTextSet), optional("market", ValueRecordReference),
		}, nil
	case RecordProduct:
		return []FieldSpec{
			required("name", ValueText), required("problem", ValueText),
			required("status", ValueIdentifier), required("target_segment", ValueRecordReference),
		}, nil
	case RecordProductVersion:
		return []FieldSpec{
			required("product", ValueRecordReference), required("version_label", ValueIdentifier),
			required("release_state", ValueIdentifier), optional("released_at", ValueTimestamp),
		}, nil
	case RecordValueProposition:
		return []FieldSpec{
			required("product", ValueRecordReference), required("segment", ValueRecordReference),
			required("claim", ValueText), required("evidence_threshold", ValueBasisPoints),
		}, nil
	case RecordBusinessModel:
		return []FieldSpec{
			required("model_type", ValueIdentifier), required("revenue_mechanisms", ValueTextSet),
			required("unit_economics", ValueText), optional("product", ValueRecordReference),
		}, nil
	case RecordPricePackage:
		return []FieldSpec{
			required("product", ValueRecordReference), required("name", ValueText),
			required("amount", ValueMoneyMinor), required("billing_period", ValueIdentifier),
		}, nil
	case RecordHypothesis:
		return []FieldSpec{
			required("statement", ValueText), required("falsification_condition", ValueText),
			required("subject", ValueRecordReference),
		}, nil
	case RecordExperiment:
		return []FieldSpec{
			required("hypothesis", ValueRecordReference), required("method", ValueText),
			required("success_threshold", ValueBasisPoints), required("state", ValueIdentifier),
			optional("sample_size", ValueInteger),
		}, nil
	case RecordInitiative:
		return []FieldSpec{
			required("name", ValueText), required("objective", ValueText),
			required("lifecycle_state", ValueIdentifier), required("allocated_budget", ValueMoneyMinor),
		}, nil
	case RecordPortfolioDecision:
		return []FieldSpec{
			required("initiative", ValueRecordReference), required("decision", ValueIdentifier),
			required("rationale", ValueText), required("capital_allocation", ValueMoneyMinor),
		}, nil
	case RecordCampaign:
		return []FieldSpec{
			required("name", ValueText), required("segment", ValueRecordReference),
			required("channel", ValueIdentifier), required("budget", ValueMoneyMinor),
			optional("state", ValueIdentifier),
		}, nil
	case RecordSalesPipeline:
		return []FieldSpec{
			required("name", ValueText), required("stages", ValueTextSet),
			required("open_value", ValueMoneyMinor), optional("opportunity_count", ValueInteger),
		}, nil
	case RecordContract:
		return []FieldSpec{
			required("counterparty_id", ValueIdentifier), required("status", ValueIdentifier),
			required("effective_at", ValueTimestamp), required("consideration", ValueMoneyMinor),
			optional("expires_at", ValueTimestamp),
		}, nil
	case RecordSubscription:
		return []FieldSpec{
			required("customer", ValueRecordReference), required("price_package", ValueRecordReference),
			required("status", ValueIdentifier), optional("renewal_at", ValueTimestamp),
		}, nil
	case RecordPurchase:
		return []FieldSpec{
			required("supplier_id", ValueIdentifier), required("amount", ValueMoneyMinor),
			required("purchased_at", ValueTimestamp), optional("contract", ValueRecordReference),
		}, nil
	case RecordRevenue:
		return []FieldSpec{
			required("amount", ValueMoneyMinor), required("recognized_at", ValueTimestamp),
			optional("customer", ValueRecordReference), optional("contract", ValueRecordReference),
		}, nil
	case RecordExpense:
		return []FieldSpec{
			required("supplier_id", ValueIdentifier), required("amount", ValueMoneyMinor),
			required("incurred_at", ValueTimestamp), optional("purchase", ValueRecordReference),
		}, nil
	case RecordAsset:
		return []FieldSpec{
			required("asset_type", ValueIdentifier), required("carrying_value", ValueMoneyMinor),
			required("acquired_at", ValueTimestamp), optional("custodian_id", ValueIdentifier),
		}, nil
	case RecordLiability:
		return []FieldSpec{
			required("counterparty_id", ValueIdentifier), required("balance", ValueMoneyMinor),
			required("due_at", ValueTimestamp), optional("contract", ValueRecordReference),
		}, nil
	case RecordCashPosition:
		return []FieldSpec{
			required("account_id", ValueIdentifier), required("balance", ValueMoneyMinor),
			required("as_of", ValueTimestamp), required("reconciled", ValueBoolean),
		}, nil
	case RecordRunway:
		return []FieldSpec{
			required("cash_position", ValueRecordReference), required("monthly_burn", ValueMoneyMinor),
			required("months_remaining", ValueMicros), required("as_of", ValueTimestamp),
		}, nil
	case RecordConversionMetric:
		return []FieldSpec{
			required("name", ValueIdentifier), required("numerator", ValueInteger),
			required("denominator", ValueInteger), required("rate", ValueBasisPoints),
			required("window_start", ValueTimestamp), required("window_end", ValueTimestamp),
		}, nil
	case RecordRetentionMetric:
		return []FieldSpec{
			required("cohort", ValueIdentifier), required("rate", ValueBasisPoints),
			required("measured_at", ValueTimestamp), required("period", ValueIdentifier),
		}, nil
	case RecordSupportIssue:
		return []FieldSpec{
			required("customer", ValueRecordReference), required("severity", ValueIdentifier),
			required("status", ValueIdentifier), required("summary", ValueText),
		}, nil
	case RecordOperationalIncident:
		return []FieldSpec{
			required("severity", ValueIdentifier), required("status", ValueIdentifier),
			required("summary", ValueText), required("started_at", ValueTimestamp),
			optional("resolved_at", ValueTimestamp),
		}, nil
	case RecordStrategicReview:
		return []FieldSpec{
			required("period_start", ValueTimestamp), required("period_end", ValueTimestamp),
			required("decision_summary", ValueText), required("outcome_status", ValueIdentifier),
			optional("next_actions", ValueTextSet),
		}, nil
	default:
		return nil, fmt.Errorf("company state: no schema registered for %q", kind)
	}
}
