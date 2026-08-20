package commercialcapability

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

type ConsentBinding struct {
	ConsentID    string                `json:"consent_id"`
	Status       ConsentStatus         `json:"status"`
	Purposes     []string              `json:"purposes"`
	Channels     []string              `json:"channels"`
	Jurisdiction string                `json:"jurisdiction"`
	GrantedAt    time.Time             `json:"granted_at"`
	WithdrawnAt  *time.Time            `json:"withdrawn_at"`
	ExpiresAt    time.Time             `json:"expires_at"`
	Authority    contracts.EvidenceRef `json:"authority"`
}

func (value ConsentBinding) Validate() error {
	if err := validateToken("consent_id", value.ConsentID); err != nil {
		return err
	}
	if !value.Status.Valid() {
		return fmt.Errorf("commercial capability: consent status is invalid")
	}
	if err := validateTokens("consent purposes", value.Purposes, 1, 16); err != nil {
		return err
	}
	if err := validateTokens("consent channels", value.Channels, 1, 16); err != nil {
		return err
	}
	if !sortedUnique(value.Purposes) || !sortedUnique(value.Channels) {
		return fmt.Errorf("commercial capability: consent purposes and channels must be sorted and unique")
	}
	if err := validateToken("consent jurisdiction", value.Jurisdiction); err != nil {
		return err
	}
	if !validUTC(value.GrantedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.GrantedAt) {
		return fmt.Errorf("commercial capability: consent validity interval is invalid")
	}
	if value.WithdrawnAt != nil {
		if !validUTC(*value.WithdrawnAt) || value.WithdrawnAt.Before(value.GrantedAt) ||
			value.WithdrawnAt.After(value.ExpiresAt) || value.Status != ConsentWithdrawn {
			return fmt.Errorf("commercial capability: consent withdrawal is invalid")
		}
	} else if value.Status == ConsentWithdrawn {
		return fmt.Errorf("commercial capability: withdrawn consent requires withdrawal time")
	}
	if err := value.Authority.Validate(); err != nil {
		return fmt.Errorf("commercial capability: consent authority: %w", err)
	}
	if value.Authority.Kind != string(ObservationConsent) ||
		value.Authority.ObservedAt.Before(value.GrantedAt) {
		return fmt.Errorf("commercial capability: consent authority does not bind the grant")
	}
	return nil
}

func (value ConsentBinding) Authorizes(at time.Time, purpose, channel, jurisdiction string) bool {
	if value.Validate() != nil || !validUTC(at) || value.Status != ConsentGranted ||
		at.Before(value.GrantedAt) || !at.Before(value.ExpiresAt) ||
		value.WithdrawnAt != nil || value.Jurisdiction != jurisdiction {
		return false
	}
	return slices.Contains(value.Purposes, purpose) && slices.Contains(value.Channels, channel)
}

type CustomerBoundary struct {
	OrganizationID     contracts.OrganizationID `json:"organization_id"`
	InitiativeID       InitiativeID             `json:"initiative_id"`
	DepartmentID       contracts.DepartmentID   `json:"department_id"`
	ProjectID          contracts.ProjectID      `json:"project_id"`
	SeatID             contracts.SeatID         `json:"seat_id"`
	CustomerRef        CustomerRef              `json:"customer_ref"`
	DataClassification string                   `json:"data_classification"`
	Purpose            string                   `json:"purpose"`
	Jurisdiction       string                   `json:"jurisdiction"`
	PolicyVersion      uint64                   `json:"policy_version"`
	PolicyHash         contracts.ContentHash    `json:"policy_hash"`
	BrandPolicyHash    contracts.ContentHash    `json:"brand_policy_hash"`
	ClaimsPolicyHash   contracts.ContentHash    `json:"claims_policy_hash"`
	AllowedChannels    []string                 `json:"allowed_channels"`
	DataScopes         []string                 `json:"data_scopes"`
	AllowedClaims      []contracts.ContentHash  `json:"allowed_claims"`
	MaxContactsPerWeek uint16                   `json:"max_contacts_per_week"`
	NextContactAt      time.Time                `json:"next_contact_at"`
	AccessExpiresAt    time.Time                `json:"access_expires_at"`
	RetentionUntil     time.Time                `json:"retention_until"`
	Consent            ConsentBinding           `json:"consent"`
}

func (value CustomerBoundary) Validate() error {
	for _, token := range []struct{ name, value string }{
		{"organization_id", string(value.OrganizationID)}, {"initiative_id", string(value.InitiativeID)},
		{"department_id", string(value.DepartmentID)}, {"project_id", string(value.ProjectID)},
		{"seat_id", string(value.SeatID)},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if err := validateToken("customer_ref", string(value.CustomerRef)); err != nil {
		return err
	}
	switch value.DataClassification {
	case "prospect_confidential", "customer_confidential", "customer_personal":
	default:
		return fmt.Errorf("commercial capability: customer data classification is invalid")
	}
	if err := validateToken("customer purpose", value.Purpose); err != nil {
		return err
	}
	if err := validateToken("customer jurisdiction", value.Jurisdiction); err != nil {
		return err
	}
	if value.PolicyVersion == 0 {
		return fmt.Errorf("commercial capability: customer policy version must be positive")
	}
	for _, digest := range []struct {
		name string
		hash contracts.ContentHash
	}{{"policy", value.PolicyHash}, {"brand policy", value.BrandPolicyHash}, {"claims policy", value.ClaimsPolicyHash}} {
		if err := digest.hash.Validate(); err != nil {
			return fmt.Errorf("commercial capability: customer %s hash: %w", digest.name, err)
		}
	}
	if err := validateTokens("allowed channels", value.AllowedChannels, 1, 16); err != nil {
		return err
	}
	if err := validateTokens("customer data scopes", value.DataScopes, 1, 32); err != nil {
		return err
	}
	if !sortedUnique(value.AllowedChannels) || !sortedUnique(value.DataScopes) ||
		len(value.AllowedClaims) == 0 || len(value.AllowedClaims) > 64 {
		return fmt.Errorf("commercial capability: allowed channels or claims are outside bounds")
	}
	previousClaim := ""
	for _, hash := range value.AllowedClaims {
		if err := hash.Validate(); err != nil {
			return fmt.Errorf("commercial capability: allowed claim hash: %w", err)
		}
		if previousClaim != "" && hash.Digest <= previousClaim {
			return fmt.Errorf("commercial capability: allowed claim hashes must be sorted and unique")
		}
		previousClaim = hash.Digest
	}
	if value.MaxContactsPerWeek == 0 || value.MaxContactsPerWeek > 64 ||
		!validUTC(value.NextContactAt) || !validUTC(value.AccessExpiresAt) ||
		!validUTC(value.RetentionUntil) || !value.AccessExpiresAt.After(value.NextContactAt) ||
		!value.RetentionUntil.After(value.AccessExpiresAt) {
		return fmt.Errorf("commercial capability: customer contact or retention bound is invalid")
	}
	if err := value.Consent.Validate(); err != nil {
		return err
	}
	if value.Consent.Jurisdiction != value.Jurisdiction ||
		!slices.Equal(value.Consent.Channels, value.AllowedChannels) {
		return fmt.Errorf("commercial capability: consent does not match customer boundary")
	}
	return nil
}

func (value CustomerBoundary) AuthorizeContact(at time.Time, channel string) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if at.Before(value.NextContactAt) || !at.Before(value.AccessExpiresAt) ||
		!value.Consent.Authorizes(at, value.Purpose, channel, value.Jurisdiction) {
		return fmt.Errorf("commercial capability: customer contact is not consented in this exact scope")
	}
	return nil
}

func CustomerBoundaryHash(value CustomerBoundary) (contracts.ContentHash, error) {
	return contracts.HashCanonical(&value)
}

type AuthorityBoundary struct {
	AnalysisOnly          bool                  `json:"analysis_only"`
	MayContactCustomer    bool                  `json:"may_contact_customer"`
	MayMutateCRM          bool                  `json:"may_mutate_crm"`
	MayTransmitContract   bool                  `json:"may_transmit_contract"`
	MayPublishPrice       bool                  `json:"may_publish_price"`
	MayMoveFunds          bool                  `json:"may_move_funds"`
	MayAllocateCapital    bool                  `json:"may_allocate_capital"`
	MayTrade              bool                  `json:"may_trade"`
	EffectGatewayRequired bool                  `json:"effect_gateway_required"`
	FounderReserved       []string              `json:"founder_reserved"`
	ConstitutionHash      contracts.ContentHash `json:"constitution_hash"`
	IssuerPolicyHash      contracts.ContentHash `json:"issuer_policy_hash"`
}

func (value AuthorityBoundary) Validate() error {
	if !value.AnalysisOnly || !value.EffectGatewayRequired || value.MayContactCustomer ||
		value.MayMutateCRM || value.MayTransmitContract || value.MayPublishPrice ||
		value.MayMoveFunds || value.MayAllocateCapital || value.MayTrade {
		return fmt.Errorf("commercial capability: capability authority must remain analysis-only")
	}
	if !slices.Equal(value.FounderReserved, founderReservedActions) {
		return fmt.Errorf("commercial capability: founder-reserved actions are incomplete or reordered")
	}
	if err := value.ConstitutionHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: constitution hash: %w", err)
	}
	if err := value.IssuerPolicyHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: issuer policy hash: %w", err)
	}
	return nil
}

type EconomicBoundary struct {
	OrganizationID        contracts.OrganizationID `json:"organization_id"`
	InitiativeID          InitiativeID             `json:"initiative_id"`
	DepartmentID          contracts.DepartmentID   `json:"department_id"`
	SeatID                contracts.SeatID         `json:"seat_id"`
	DataClassification    string                   `json:"data_classification"`
	Purpose               string                   `json:"purpose"`
	Jurisdiction          string                   `json:"jurisdiction"`
	FinancialAccounts     []string                 `json:"financial_accounts"`
	AccessExpiresAt       time.Time                `json:"access_expires_at"`
	BaseCurrency          string                   `json:"base_currency"`
	ValuationAt           time.Time                `json:"valuation_at"`
	ValuationMethod       string                   `json:"valuation_method"`
	DataFreshUntil        time.Time                `json:"data_fresh_until"`
	AccountingPolicyHash  contracts.ContentHash    `json:"accounting_policy_hash"`
	PricingPolicyHash     contracts.ContentHash    `json:"pricing_policy_hash"`
	CapitalEnvelopeID     string                   `json:"capital_envelope_id"`
	CapitalEnvelopeHash   contracts.ContentHash    `json:"capital_envelope_hash"`
	MaxAnalysisMicrounits uint64                   `json:"max_analysis_microunits"`
	FounderEffectApproval bool                     `json:"founder_effect_approval"`
	AnalysisOnly          bool                     `json:"analysis_only"`
}

func (value EconomicBoundary) Validate() error {
	for _, token := range []struct{ name, value string }{
		{"organization_id", string(value.OrganizationID)}, {"initiative_id", string(value.InitiativeID)},
		{"department_id", string(value.DepartmentID)}, {"seat_id", string(value.SeatID)},
		{"economic data classification", value.DataClassification}, {"economic purpose", value.Purpose},
		{"economic jurisdiction", value.Jurisdiction},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if err := validateTokens("financial accounts", value.FinancialAccounts, 1, 32); err != nil {
		return err
	}
	if !sortedUnique(value.FinancialAccounts) || !validUTC(value.AccessExpiresAt) {
		return fmt.Errorf("commercial capability: economic account scope or expiry is invalid")
	}
	if err := validateToken("base currency", value.BaseCurrency); err != nil {
		return err
	}
	if value.BaseCurrency != strings.ToUpper(value.BaseCurrency) || len(value.BaseCurrency) > 12 {
		return fmt.Errorf("commercial capability: base currency must be uppercase and bounded")
	}
	if !validUTC(value.ValuationAt) || !validUTC(value.DataFreshUntil) ||
		!value.DataFreshUntil.After(value.ValuationAt) ||
		!value.AccessExpiresAt.After(value.ValuationAt) {
		return fmt.Errorf("commercial capability: economic valuation interval is invalid")
	}
	if err := validateToken("valuation method", value.ValuationMethod); err != nil {
		return err
	}
	if err := value.AccountingPolicyHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: accounting policy hash: %w", err)
	}
	if err := value.PricingPolicyHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: pricing policy hash: %w", err)
	}
	if err := validateToken("capital envelope id", value.CapitalEnvelopeID); err != nil {
		return err
	}
	if err := value.CapitalEnvelopeHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: capital envelope hash: %w", err)
	}
	if value.MaxAnalysisMicrounits == 0 || !value.FounderEffectApproval || !value.AnalysisOnly {
		return fmt.Errorf("commercial capability: economic boundary must preserve founder effect authority")
	}
	return nil
}

func EconomicBoundaryHash(value EconomicBoundary) (contracts.ContentHash, error) {
	return contracts.HashCanonical(&value)
}

type MetricDefinition struct {
	ID                    MetricID              `json:"metric_id"`
	Version               uint64                `json:"version"`
	Name                  string                `json:"name"`
	Unit                  string                `json:"unit"`
	Numerator             string                `json:"numerator"`
	Denominator           string                `json:"denominator"`
	Attribution           string                `json:"attribution"`
	SourceClass           SourceClass           `json:"source_class"`
	SourceProvider        string                `json:"source_provider"`
	Freshness             time.Duration         `json:"freshness"`
	MaximumUncertaintyBPS uint16                `json:"maximum_uncertainty_bps"`
	Guardrails            []string              `json:"guardrails"`
	Reconciliation        string                `json:"reconciliation"`
	DefinitionHash        contracts.ContentHash `json:"definition_hash"`
}

func (value MetricDefinition) Validate() error {
	if err := validateToken("metric_id", string(value.ID)); err != nil {
		return err
	}
	if value.Version == 0 || strings.TrimSpace(value.Name) == "" || len(value.Name) > 256 {
		return fmt.Errorf("commercial capability: metric identity is invalid")
	}
	for _, token := range []struct{ name, value string }{
		{"metric unit", value.Unit}, {"metric numerator", value.Numerator},
		{"metric denominator", value.Denominator}, {"metric attribution", value.Attribution},
		{"metric source provider", value.SourceProvider}, {"metric reconciliation", value.Reconciliation},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if !value.SourceClass.Valid() || value.Freshness <= 0 || value.Freshness > 365*24*time.Hour ||
		value.MaximumUncertaintyBPS > 10_000 {
		return fmt.Errorf("commercial capability: metric source, freshness, or uncertainty is invalid")
	}
	if err := validateTokens("metric guardrails", value.Guardrails, 1, 32); err != nil {
		return err
	}
	if !sortedUnique(value.Guardrails) {
		return fmt.Errorf("commercial capability: metric guardrails must be sorted and unique")
	}
	if err := value.DefinitionHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: metric definition hash: %w", err)
	}
	expected, err := metricDefinitionHash(value)
	if err != nil {
		return err
	}
	if value.DefinitionHash != expected {
		return fmt.Errorf("commercial capability: metric definition hash does not match its canonical fields")
	}
	return nil
}

func (value MetricDefinition) ComparableWith(other MetricDefinition) bool {
	return value.ID == other.ID && value.Version == other.Version &&
		value.DefinitionHash == other.DefinitionHash && value.Unit == other.Unit &&
		value.Numerator == other.Numerator && value.Denominator == other.Denominator &&
		value.Attribution == other.Attribution && value.SourceClass == other.SourceClass &&
		value.SourceProvider == other.SourceProvider
}

func metricDefinitionHash(value MetricDefinition) (contracts.ContentHash, error) {
	copyValue := value
	copyValue.DefinitionHash = contracts.ContentHash{}
	encoded, err := json.Marshal(copyValue)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("commercial capability: encode metric definition: %w", err)
	}
	return digestBytes(encoded), nil
}

// ComputeMetricDefinitionHash returns the immutable digest callers place in a
// new metric definition before signing a commercial record.
func ComputeMetricDefinitionHash(value MetricDefinition) (contracts.ContentHash, error) {
	return metricDefinitionHash(value)
}

type MetricThreshold struct {
	MetricID       MetricID `json:"metric_id"`
	MetricVersion  uint64   `json:"metric_version"`
	BaselineMicros int64    `json:"baseline_micros"`
	SuccessMicros  int64    `json:"success_micros"`
	StopMicros     int64    `json:"stop_micros"`
}

func (value MetricThreshold) Validate() error {
	if err := validateToken("threshold metric_id", string(value.MetricID)); err != nil {
		return err
	}
	if value.MetricVersion == 0 || value.SuccessMicros == value.StopMicros {
		return fmt.Errorf("commercial capability: metric threshold is invalid")
	}
	return nil
}

type Hypothesis struct {
	ID              string            `json:"hypothesis_id"`
	Statement       string            `json:"statement"`
	Thresholds      []MetricThreshold `json:"thresholds"`
	EvidenceSources []SourceClass     `json:"evidence_sources"`
	RegisteredAt    time.Time         `json:"registered_at"`
	ReviewAt        time.Time         `json:"review_at"`
	MaximumDuration time.Duration     `json:"maximum_duration"`
}

func (value Hypothesis) Validate() error {
	if err := validateToken("hypothesis_id", value.ID); err != nil {
		return err
	}
	if strings.TrimSpace(value.Statement) == "" || len(value.Statement) > 4096 ||
		len(value.Thresholds) == 0 || len(value.Thresholds) > 32 ||
		len(value.EvidenceSources) == 0 || len(value.EvidenceSources) > 16 {
		return fmt.Errorf("commercial capability: hypothesis is incomplete")
	}
	metricIDs := make([]MetricID, 0, len(value.Thresholds))
	for _, threshold := range value.Thresholds {
		if err := threshold.Validate(); err != nil {
			return err
		}
		metricIDs = append(metricIDs, threshold.MetricID)
	}
	if hasDuplicate(metricIDs) || hasDuplicate(value.EvidenceSources) {
		return fmt.Errorf("commercial capability: hypothesis contains duplicate thresholds or sources")
	}
	for _, source := range value.EvidenceSources {
		if !source.Valid() {
			return fmt.Errorf("commercial capability: hypothesis evidence source is invalid")
		}
	}
	if !validUTC(value.RegisteredAt) || !validUTC(value.ReviewAt) ||
		!value.ReviewAt.After(value.RegisteredAt) || value.MaximumDuration <= 0 ||
		value.MaximumDuration > 5*365*24*time.Hour ||
		value.ReviewAt.Sub(value.RegisteredAt) > value.MaximumDuration {
		return fmt.Errorf("commercial capability: hypothesis review window is invalid")
	}
	return nil
}
