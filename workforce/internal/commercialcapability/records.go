package commercialcapability

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

type AuthoritativeSource struct {
	Class         SourceClass           `json:"class"`
	Provider      string                `json:"provider"`
	Account       string                `json:"account"`
	ObjectRef     string                `json:"object_ref"`
	Endpoint      string                `json:"endpoint"`
	ReadOnly      bool                  `json:"read_only"`
	Direct        bool                  `json:"direct"`
	Authoritative bool                  `json:"authoritative"`
	Evidence      contracts.EvidenceRef `json:"evidence"`
}

func (value AuthoritativeSource) Validate() error {
	if !value.Class.Valid() {
		return fmt.Errorf("commercial capability: authoritative source class is invalid")
	}
	for _, token := range []struct{ name, value string }{
		{"source provider", value.Provider}, {"source account", value.Account},
		{"source object", value.ObjectRef}, {"source endpoint", value.Endpoint},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if !value.ReadOnly || !value.Direct || !value.Authoritative {
		return fmt.Errorf("commercial capability: source must be a direct authoritative read")
	}
	if err := value.Evidence.Validate(); err != nil {
		return fmt.Errorf("commercial capability: source evidence: %w", err)
	}
	return nil
}

func (value AuthoritativeSource) SameAuthority(other AuthoritativeSource) bool {
	return value.Class == other.Class && value.Provider == other.Provider &&
		value.Account == other.Account && value.ObjectRef == other.ObjectRef
}

type ObservedValue struct {
	ValueMicros int64                 `json:"value_micros"`
	Scale       uint8                 `json:"scale"`
	Unit        string                `json:"unit"`
	Currency    string                `json:"currency"`
	Denominator uint64                `json:"denominator"`
	ValueHash   contracts.ContentHash `json:"value_hash"`
}

func (value ObservedValue) Validate() error {
	if value.Scale > 18 || value.Denominator == 0 {
		return fmt.Errorf("commercial capability: observed value scale or denominator is invalid")
	}
	if err := validateToken("observed unit", value.Unit); err != nil {
		return err
	}
	if err := validateToken("observed currency", value.Currency); err != nil {
		return err
	}
	if value.Currency != "none" && (value.Currency != strings.ToUpper(value.Currency) || len(value.Currency) > 12) {
		return fmt.Errorf("commercial capability: observed currency must be none or uppercase")
	}
	if err := value.ValueHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: observed value hash: %w", err)
	}
	return nil
}

type AuthoritativeObservation struct {
	ID                  ObservationID           `json:"observation_id"`
	Kind                ObservationKind         `json:"kind"`
	SubjectRef          string                  `json:"subject_ref"`
	Primary             AuthoritativeSource     `json:"primary"`
	Reconciliation      *AuthoritativeSource    `json:"reconciliation"`
	Value               ObservedValue           `json:"value"`
	MetricIDs           []MetricID              `json:"metric_ids"`
	ObservedAt          time.Time               `json:"observed_at"`
	FreshUntil          time.Time               `json:"fresh_until"`
	UncertaintyBPS      uint16                  `json:"uncertainty_bps"`
	KnownGaps           []string                `json:"known_gaps"`
	ConflictingEvidence []contracts.ContentHash `json:"conflicting_evidence"`
}

func (value AuthoritativeObservation) Validate() error {
	if err := validateToken("observation_id", string(value.ID)); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("commercial capability: observation kind is invalid")
	}
	if err := validateToken("observation subject", value.SubjectRef); err != nil {
		return err
	}
	if err := value.Primary.Validate(); err != nil {
		return err
	}
	if value.Primary.Evidence.Kind != string(value.Kind) {
		return fmt.Errorf("commercial capability: primary evidence kind does not match the observation")
	}
	if err := value.Value.Validate(); err != nil {
		return err
	}
	if len(value.MetricIDs) > 32 || hasDuplicate(value.MetricIDs) {
		return fmt.Errorf("commercial capability: observation metric bindings are invalid")
	}
	for _, id := range value.MetricIDs {
		if err := validateToken("observation metric_id", string(id)); err != nil {
			return err
		}
	}
	if !validUTC(value.ObservedAt) || !validUTC(value.FreshUntil) ||
		!value.FreshUntil.After(value.ObservedAt) ||
		value.Primary.Evidence.ObservedAt != value.ObservedAt || value.UncertaintyBPS > 10_000 {
		return fmt.Errorf("commercial capability: observation time or uncertainty is invalid")
	}
	if err := validateTokens("observation gaps", value.KnownGaps, 0, 32); err != nil {
		return err
	}
	if !sortedUnique(value.KnownGaps) || len(value.ConflictingEvidence) > 32 {
		return fmt.Errorf("commercial capability: observation gaps or conflicts are invalid")
	}
	for _, hash := range value.ConflictingEvidence {
		if err := hash.Validate(); err != nil {
			return fmt.Errorf("commercial capability: conflicting evidence hash: %w", err)
		}
	}
	if err := validateObservationAuthority(value); err != nil {
		return err
	}
	if value.Reconciliation != nil {
		if err := value.Reconciliation.Validate(); err != nil {
			return err
		}
		if value.Primary.SameAuthority(*value.Reconciliation) ||
			value.Primary.Evidence.Hash == value.Reconciliation.Evidence.Hash ||
			value.Reconciliation.Evidence.ObservedAt.Before(value.ObservedAt) ||
			value.Reconciliation.Evidence.Kind != string(value.Kind) {
			return fmt.Errorf("commercial capability: reconciliation must be an independent authoritative observation")
		}
	}
	if value.Kind.Economic() {
		if value.Reconciliation == nil ||
			value.Primary.Class == value.Reconciliation.Class ||
			(!value.Primary.Class.FinancialAuthority() && !value.Reconciliation.Class.FinancialAuthority()) ||
			value.Value.Currency == "none" {
			return fmt.Errorf("commercial capability: economic observation requires independent financial reconciliation")
		}
	}
	return nil
}

func validateObservationAuthority(value AuthoritativeObservation) error {
	switch value.Kind {
	case ObservationConsent:
		if value.Primary.Class != SourceConsentRegistry {
			return fmt.Errorf("commercial capability: consent observation must come from the consent registry")
		}
	case ObservationCRMState, ObservationLeadSource, ObservationConversation,
		ObservationProposal, ObservationConversion:
		if value.Primary.Class != SourceCRM && value.Primary.Class != SourceProviderAPI {
			return fmt.Errorf("commercial capability: sales observation source is not authoritative")
		}
	case ObservationContract:
		if value.Primary.Class != SourceContractRepository {
			return fmt.Errorf("commercial capability: contract observation must come from the contract repository")
		}
	case ObservationSupport, ObservationSLA, ObservationCustomerHealth:
		if value.Primary.Class != SourceSupportSystem && value.Primary.Class != SourceProductAnalytics {
			return fmt.Errorf("commercial capability: customer operation source is not authoritative")
		}
	case ObservationRetention, ObservationChurn, ObservationProductUsage:
		if value.Primary.Class != SourceProductAnalytics && value.Primary.Class != SourceBillingLedger {
			return fmt.Errorf("commercial capability: customer outcome source is not authoritative")
		}
	case ObservationPrice:
		if value.Primary.Class != SourceBillingLedger && value.Primary.Class != SourceContractRepository {
			return fmt.Errorf("commercial capability: price observation source is not authoritative")
		}
	}
	return nil
}

type Outcome struct {
	Kind           OutcomeKind           `json:"kind"`
	Summary        string                `json:"summary"`
	ObservationIDs []ObservationID       `json:"observation_ids"`
	MetricIDs      []MetricID            `json:"metric_ids"`
	DerivedAt      time.Time             `json:"derived_at"`
	UncertaintyBPS uint16                `json:"uncertainty_bps"`
	DerivationHash contracts.ContentHash `json:"derivation_hash"`
}

func (value Outcome) Validate() error {
	if !value.Kind.Valid() || strings.TrimSpace(value.Summary) == "" || len(value.Summary) > 4096 {
		return fmt.Errorf("commercial capability: outcome kind or summary is invalid")
	}
	if len(value.ObservationIDs) == 0 || len(value.ObservationIDs) > 128 ||
		len(value.MetricIDs) > 64 || hasDuplicate(value.ObservationIDs) ||
		hasDuplicate(value.MetricIDs) || value.UncertaintyBPS > 10_000 ||
		!validUTC(value.DerivedAt) {
		return fmt.Errorf("commercial capability: outcome derivation is invalid")
	}
	for _, id := range value.ObservationIDs {
		if err := validateToken("outcome observation_id", string(id)); err != nil {
			return err
		}
	}
	for _, id := range value.MetricIDs {
		if err := validateToken("outcome metric_id", string(id)); err != nil {
			return err
		}
	}
	if err := value.DerivationHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: outcome derivation hash: %w", err)
	}
	expected, err := outcomeDerivationHash(value)
	if err != nil {
		return err
	}
	if value.DerivationHash != expected {
		return fmt.Errorf("commercial capability: outcome derivation hash does not match its canonical fields")
	}
	return nil
}

func outcomeDerivationHash(value Outcome) (contracts.ContentHash, error) {
	copyValue := value
	copyValue.DerivationHash = contracts.ContentHash{}
	encoded, err := json.Marshal(copyValue)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("commercial capability: encode outcome derivation: %w", err)
	}
	return digestBytes(encoded), nil
}

func ComputeOutcomeDerivationHash(value Outcome) (contracts.ContentHash, error) {
	return outcomeDerivationHash(value)
}

type CrossFunctionalHandoff struct {
	SchemaVersion        string                 `json:"schema_version"`
	ID                   HandoffID              `json:"handoff_id"`
	Kind                 HandoffKind            `json:"kind"`
	FromDomain           string                 `json:"from_domain"`
	ToDomain             string                 `json:"to_domain"`
	InputRecordIDs       []RecordID             `json:"input_record_ids"`
	ObservationIDs       []ObservationID        `json:"observation_ids"`
	CustomerBoundaryHash *contracts.ContentHash `json:"customer_boundary_hash"`
	EconomicBoundaryHash *contracts.ContentHash `json:"economic_boundary_hash"`
	AllowedNextActions   []string               `json:"allowed_next_actions"`
	UnresolvedRisks      []string               `json:"unresolved_risks"`
	NoEffectAuthority    bool                   `json:"no_effect_authority"`
	CreatedAt            time.Time              `json:"created_at"`
	ExpiresAt            time.Time              `json:"expires_at"`
}

func (value CrossFunctionalHandoff) Validate() error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("commercial capability: handoff schema is invalid")
	}
	if err := validateToken("handoff_id", string(value.ID)); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("commercial capability: handoff kind is invalid")
	}
	expectedFrom, expectedTo := value.Kind.Domains()
	if value.FromDomain != string(expectedFrom) || value.ToDomain != string(expectedTo) {
		return fmt.Errorf("commercial capability: handoff domain pair is invalid")
	}
	if len(value.InputRecordIDs) == 0 || len(value.InputRecordIDs) > 64 ||
		len(value.ObservationIDs) == 0 || len(value.ObservationIDs) > 128 ||
		hasDuplicate(value.InputRecordIDs) || hasDuplicate(value.ObservationIDs) {
		return fmt.Errorf("commercial capability: handoff inputs are outside bounds")
	}
	for _, id := range value.InputRecordIDs {
		if err := validateToken("handoff input record", string(id)); err != nil {
			return err
		}
	}
	for _, id := range value.ObservationIDs {
		if err := validateToken("handoff observation", string(id)); err != nil {
			return err
		}
	}
	if value.CustomerBoundaryHash != nil {
		if err := value.CustomerBoundaryHash.Validate(); err != nil {
			return fmt.Errorf("commercial capability: handoff customer boundary hash: %w", err)
		}
	}
	if value.EconomicBoundaryHash != nil {
		if err := value.EconomicBoundaryHash.Validate(); err != nil {
			return fmt.Errorf("commercial capability: handoff economic boundary hash: %w", err)
		}
	}
	if err := validateTokens("handoff allowed actions", value.AllowedNextActions, 1, 32); err != nil {
		return err
	}
	if !sortedUnique(value.AllowedNextActions) {
		return fmt.Errorf("commercial capability: handoff actions must be sorted and unique")
	}
	for _, action := range value.AllowedNextActions {
		if !handoffActionAllowed(value.Kind, action) {
			return fmt.Errorf("commercial capability: handoff action %q exceeds the receiving boundary", action)
		}
	}
	if err := validateTexts("handoff unresolved risks", value.UnresolvedRisks, 0, 32, 2048); err != nil {
		return err
	}
	if !value.NoEffectAuthority || !validUTC(value.CreatedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.CreatedAt) {
		return fmt.Errorf("commercial capability: handoff cannot carry effect authority and must be time bounded")
	}
	return nil
}

func handoffActionAllowed(kind HandoffKind, action string) bool {
	var allowed []string
	switch kind {
	case HandoffGrowthToSales:
		allowed = []string{"create_lead_proposal", "request_sales_review"}
	case HandoffSalesToCustomerOps:
		allowed = []string{"prepare_onboarding", "request_customer_operations_review"}
	case HandoffSalesToContract:
		allowed = []string{"request_contract_review"}
	case HandoffCustomerToProduct:
		allowed = []string{"request_product_review"}
	case HandoffPricingToSales:
		allowed = []string{"prepare_proposal", "request_pricing_review"}
	case HandoffFinanceToExecutive:
		allowed = []string{"request_executive_review"}
	case HandoffTreasuryToFinance:
		allowed = []string{"request_finance_review"}
	}
	return slices.Contains(allowed, action)
}

type RecordBody struct {
	SchemaVersion  string                     `json:"schema_version"`
	ID             RecordID                   `json:"record_id"`
	ChainID        ChainID                    `json:"chain_id"`
	Version        uint64                     `json:"version"`
	OrganizationID contracts.OrganizationID   `json:"organization_id"`
	InitiativeID   InitiativeID               `json:"initiative_id"`
	DepartmentID   contracts.DepartmentID     `json:"department_id"`
	ProjectID      contracts.ProjectID        `json:"project_id"`
	WorkspaceID    contracts.WorkspaceID      `json:"workspace_id"`
	AuthorSeatID   contracts.SeatID           `json:"author_seat_id"`
	Domain         Domain                     `json:"domain"`
	Kind           RecordKind                 `json:"kind"`
	SkillID        contracts.SkillID          `json:"skill_id"`
	SkillVersion   uint64                     `json:"skill_version"`
	Material       bool                       `json:"material"`
	Supersedes     *RecordID                  `json:"supersedes"`
	Authority      AuthorityBoundary          `json:"authority"`
	Customer       *CustomerBoundary          `json:"customer"`
	Economic       *EconomicBoundary          `json:"economic"`
	Hypotheses     []Hypothesis               `json:"hypotheses"`
	Metrics        []MetricDefinition         `json:"metrics"`
	Observations   []AuthoritativeObservation `json:"observations"`
	Outcome        Outcome                    `json:"outcome"`
	Handoffs       []CrossFunctionalHandoff   `json:"handoffs"`
	CreatedAt      time.Time                  `json:"created_at"`
	EffectiveAt    time.Time                  `json:"effective_at"`
	FreshUntil     time.Time                  `json:"fresh_until"`
}

func (value RecordBody) Validate() error {
	if value.SchemaVersion != SchemaVersion {
		return fmt.Errorf("commercial capability: record schema is invalid")
	}
	for _, token := range []struct{ name, value string }{
		{"record_id", string(value.ID)}, {"chain_id", string(value.ChainID)},
		{"organization_id", string(value.OrganizationID)}, {"initiative_id", string(value.InitiativeID)},
		{"department_id", string(value.DepartmentID)},
		{"project_id", string(value.ProjectID)}, {"workspace_id", string(value.WorkspaceID)},
		{"author_seat_id", string(value.AuthorSeatID)}, {"skill_id", string(value.SkillID)},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if value.Version == 0 || value.SkillVersion != 1 || !value.Domain.Valid() ||
		!value.Kind.Valid() || value.Kind.Domain() != value.Domain ||
		SkillForRecord(value.Kind) != value.SkillID {
		return fmt.Errorf("commercial capability: record kind, domain, skill, or version is invalid")
	}
	if value.Version == 1 && value.Supersedes != nil || value.Version > 1 && value.Supersedes == nil {
		return fmt.Errorf("commercial capability: record lineage is invalid")
	}
	if value.Supersedes != nil {
		if err := validateToken("superseded record", string(*value.Supersedes)); err != nil || *value.Supersedes == value.ID {
			return fmt.Errorf("commercial capability: superseded record is invalid")
		}
	}
	if err := value.Authority.Validate(); err != nil {
		return err
	}
	if err := validateRecordBoundaries(value); err != nil {
		return err
	}
	if err := validateRecordOutcome(value); err != nil {
		return err
	}
	if len(value.Hypotheses) > 32 || len(value.Metrics) > 64 ||
		len(value.Observations) == 0 || len(value.Observations) > 128 || len(value.Handoffs) > 32 {
		return fmt.Errorf("commercial capability: record evidence collections are outside bounds")
	}
	hypothesisIDs := make([]string, 0, len(value.Hypotheses))
	for _, hypothesis := range value.Hypotheses {
		if err := hypothesis.Validate(); err != nil {
			return err
		}
		if hypothesis.RegisteredAt.After(value.EffectiveAt) {
			return fmt.Errorf("commercial capability: hypothesis was not preregistered")
		}
		if hypothesis.ReviewAt.After(value.FreshUntil) {
			return fmt.Errorf("commercial capability: hypothesis review occurs after the record freshness window")
		}
		hypothesisIDs = append(hypothesisIDs, hypothesis.ID)
	}
	if hasDuplicate(hypothesisIDs) || (value.Material && len(value.Hypotheses) == 0) {
		return fmt.Errorf("commercial capability: material record requires unique preregistered hypotheses")
	}
	metricIDs := make([]MetricID, 0, len(value.Metrics))
	for _, metric := range value.Metrics {
		if err := metric.Validate(); err != nil {
			return err
		}
		metricIDs = append(metricIDs, metric.ID)
	}
	if hasDuplicate(metricIDs) {
		return fmt.Errorf("commercial capability: metric definitions are duplicated")
	}
	for _, hypothesis := range value.Hypotheses {
		for _, threshold := range hypothesis.Thresholds {
			index := slices.IndexFunc(value.Metrics, func(metric MetricDefinition) bool {
				return metric.ID == threshold.MetricID && metric.Version == threshold.MetricVersion
			})
			if index < 0 {
				return fmt.Errorf("commercial capability: hypothesis threshold lacks its exact metric definition")
			}
		}
	}
	observationIDs := make([]ObservationID, 0, len(value.Observations))
	for _, observation := range value.Observations {
		if err := observation.Validate(); err != nil {
			return err
		}
		if observation.ObservedAt.Before(value.CreatedAt) || observation.ObservedAt.After(value.EffectiveAt) ||
			observation.FreshUntil.Before(value.FreshUntil) ||
			(observation.Reconciliation != nil && observation.Reconciliation.Evidence.ObservedAt.After(value.EffectiveAt)) {
			return fmt.Errorf("commercial capability: observation chronology or freshness is invalid")
		}
		for _, metricID := range observation.MetricIDs {
			index := slices.IndexFunc(value.Metrics, func(metric MetricDefinition) bool { return metric.ID == metricID })
			if index < 0 {
				return fmt.Errorf("commercial capability: observation references an absent metric definition")
			}
			metric := value.Metrics[index]
			if metric.SourceProvider != observation.Primary.Provider ||
				metric.SourceClass != observation.Primary.Class || metric.Unit != observation.Value.Unit {
				return fmt.Errorf("commercial capability: observation does not match its metric source or unit")
			}
		}
		observationIDs = append(observationIDs, observation.ID)
	}
	if hasDuplicate(observationIDs) {
		return fmt.Errorf("commercial capability: observations are duplicated")
	}
	for _, hypothesis := range value.Hypotheses {
		for _, source := range hypothesis.EvidenceSources {
			present := source == SourceConsentRegistry && value.Customer != nil
			for _, observation := range value.Observations {
				if observation.Primary.Class == source ||
					(observation.Reconciliation != nil && observation.Reconciliation.Class == source) {
					present = true
					break
				}
			}
			if !present {
				return fmt.Errorf("commercial capability: hypothesis evidence source %q is absent", source)
			}
		}
	}
	for _, required := range requiredObservationKinds(value.Kind) {
		present := false
		for _, observation := range value.Observations {
			if observation.Kind == required {
				present = true
				break
			}
		}
		if !present {
			return fmt.Errorf("commercial capability: required authoritative observation %q is absent", required)
		}
	}
	if err := value.Outcome.Validate(); err != nil {
		return err
	}
	for _, id := range value.Outcome.ObservationIDs {
		if !contains(observationIDs, id) {
			return fmt.Errorf("commercial capability: outcome references an absent observation")
		}
	}
	for _, id := range value.Outcome.MetricIDs {
		if !contains(metricIDs, id) {
			return fmt.Errorf("commercial capability: outcome references an absent metric definition")
		}
		bound := false
		for _, observation := range value.Observations {
			if contains(value.Outcome.ObservationIDs, observation.ID) && contains(observation.MetricIDs, id) {
				bound = true
				break
			}
		}
		if !bound {
			return fmt.Errorf("commercial capability: outcome metric lacks an authoritative observation binding")
		}
	}
	if value.Outcome.DerivedAt.Before(value.EffectiveAt) || value.Outcome.DerivedAt.After(value.FreshUntil) {
		return fmt.Errorf("commercial capability: outcome derivation chronology is invalid")
	}
	handoffIDs := make([]HandoffID, 0, len(value.Handoffs))
	for _, handoff := range value.Handoffs {
		if err := handoff.Validate(); err != nil {
			return err
		}
		if handoff.FromDomain != string(value.Domain) || !contains(handoff.InputRecordIDs, value.ID) ||
			handoff.CreatedAt.Before(value.EffectiveAt) || handoff.ExpiresAt.After(value.FreshUntil) {
			return fmt.Errorf("commercial capability: handoff is not bound to this record")
		}
		for _, id := range handoff.ObservationIDs {
			if !contains(observationIDs, id) {
				return fmt.Errorf("commercial capability: handoff references an absent observation")
			}
		}
		if err := validateHandoffBoundaries(value, handoff); err != nil {
			return err
		}
		handoffIDs = append(handoffIDs, handoff.ID)
	}
	if hasDuplicate(handoffIDs) || !validUTC(value.CreatedAt) || !validUTC(value.EffectiveAt) ||
		!validUTC(value.FreshUntil) || value.EffectiveAt.Before(value.CreatedAt) ||
		!value.FreshUntil.After(value.EffectiveAt) {
		return fmt.Errorf("commercial capability: record time or handoff identity is invalid")
	}
	if required := requiredHandoff(value.Kind); required != "" {
		found := false
		for _, handoff := range value.Handoffs {
			if handoff.Kind == required {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("commercial capability: required cross-functional handoff %q is absent", required)
		}
	}
	return nil
}

func validateRecordBoundaries(value RecordBody) error {
	customerRequired := value.Domain == DomainSales || value.Domain == DomainCustomerOperations
	economicRequired := value.Domain == DomainPricing || value.Domain == DomainFinance ||
		value.Domain == DomainTreasury || value.Kind == RecordAcquisition
	for _, observation := range value.Observations {
		if observation.Kind.Economic() {
			economicRequired = true
		}
	}
	materialRequired := value.Kind == RecordAcquisition || value.Domain == DomainGrowth ||
		value.Domain == DomainPricing || value.Domain == DomainFinance || value.Domain == DomainTreasury
	if materialRequired && !value.Material {
		return fmt.Errorf("commercial capability: this record kind is intrinsically material")
	}
	if customerRequired {
		if value.Customer == nil {
			return fmt.Errorf("commercial capability: customer-scoped record requires an exact customer boundary")
		}
		if err := value.Customer.Validate(); err != nil {
			return err
		}
		if value.Customer.Consent.Status != ConsentGranted ||
			value.Customer.OrganizationID != value.OrganizationID ||
			value.Customer.InitiativeID != value.InitiativeID ||
			value.Customer.DepartmentID != value.DepartmentID ||
			value.Customer.ProjectID != value.ProjectID || value.Customer.SeatID != value.AuthorSeatID ||
			value.EffectiveAt.Before(value.Customer.Consent.GrantedAt) ||
			!value.Customer.AccessExpiresAt.After(value.FreshUntil) ||
			!value.Customer.Consent.ExpiresAt.After(value.FreshUntil) ||
			!value.Customer.RetentionUntil.After(value.FreshUntil) {
			return fmt.Errorf("commercial capability: customer authority is not current for the record freshness window")
		}
		profile, err := Profile(value.SkillID)
		if err != nil {
			return err
		}
		expectedScopes := append([]string(nil), profile.DataScopes...)
		actualScopes := append([]string(nil), value.Customer.DataScopes...)
		slices.Sort(expectedScopes)
		slices.Sort(actualScopes)
		if !slices.Equal(expectedScopes, actualScopes) {
			return fmt.Errorf("commercial capability: customer data scope does not match the signed skill")
		}
		for _, observation := range value.Observations {
			if observation.SubjectRef != string(value.Customer.CustomerRef) {
				return fmt.Errorf("commercial capability: customer observation crosses the bound customer scope")
			}
		}
		if contactRecord(value.Kind) {
			for _, channel := range value.Customer.AllowedChannels {
				if err := value.Customer.AuthorizeContact(value.EffectiveAt, channel); err != nil {
					return err
				}
			}
		}
	} else if value.Customer != nil {
		if err := value.Customer.Validate(); err != nil {
			return err
		}
	}
	if economicRequired {
		if value.Economic == nil {
			return fmt.Errorf("commercial capability: economic record requires an exact economic boundary")
		}
		if err := value.Economic.Validate(); err != nil {
			return err
		}
		if !value.Economic.DataFreshUntil.After(value.FreshUntil) {
			return fmt.Errorf("commercial capability: economic boundary expires before the record")
		}
		if value.Economic.OrganizationID != value.OrganizationID ||
			value.Economic.InitiativeID != value.InitiativeID ||
			value.Economic.DepartmentID != value.DepartmentID ||
			value.Economic.SeatID != value.AuthorSeatID ||
			!value.Economic.AccessExpiresAt.After(value.FreshUntil) {
			return fmt.Errorf("commercial capability: economic access scope does not match the record")
		}
	} else if value.Economic != nil {
		if err := value.Economic.Validate(); err != nil {
			return err
		}
	}
	if value.Economic != nil {
		if value.Economic.OrganizationID != value.OrganizationID ||
			value.Economic.InitiativeID != value.InitiativeID ||
			value.Economic.DepartmentID != value.DepartmentID ||
			value.Economic.SeatID != value.AuthorSeatID ||
			!value.Economic.AccessExpiresAt.After(value.FreshUntil) {
			return fmt.Errorf("commercial capability: economic access scope does not match the record")
		}
		for _, observation := range value.Observations {
			if !observation.Kind.Economic() {
				continue
			}
			if !contains(value.Economic.FinancialAccounts, observation.Primary.Account) ||
				(observation.Reconciliation != nil && !contains(value.Economic.FinancialAccounts, observation.Reconciliation.Account)) {
				return fmt.Errorf("commercial capability: economic observation crosses a financial account boundary")
			}
		}
	}
	if value.Outcome.Kind == OutcomeEconomic && value.Economic == nil {
		return fmt.Errorf("commercial capability: economic outcome lacks an economic boundary")
	}
	return nil
}

func validateHandoffBoundaries(body RecordBody, handoff CrossFunctionalHandoff) error {
	if body.Customer == nil {
		if handoff.CustomerBoundaryHash != nil {
			return fmt.Errorf("commercial capability: handoff adds an absent customer boundary")
		}
	} else {
		hash, err := CustomerBoundaryHash(*body.Customer)
		if err != nil || handoff.CustomerBoundaryHash == nil || *handoff.CustomerBoundaryHash != hash {
			return fmt.Errorf("commercial capability: handoff customer boundary is not exact")
		}
	}
	if body.Economic == nil {
		if handoff.EconomicBoundaryHash != nil {
			return fmt.Errorf("commercial capability: handoff adds an absent economic boundary")
		}
	} else {
		hash, err := EconomicBoundaryHash(*body.Economic)
		if err != nil || handoff.EconomicBoundaryHash == nil || *handoff.EconomicBoundaryHash != hash {
			return fmt.Errorf("commercial capability: handoff economic boundary is not exact")
		}
	}
	return nil
}

func contactRecord(kind RecordKind) bool {
	switch kind {
	case RecordOutreachPlan, RecordSalesConversation, RecordProposalHandoff,
		RecordContractHandoff, RecordAcquisition, RecordIncidentCommunication:
		return true
	default:
		return false
	}
}

func validateRecordOutcome(value RecordBody) error {
	metricRequired := value.Domain == DomainGrowth || value.Domain == DomainPricing ||
		value.Domain == DomainFinance || value.Domain == DomainTreasury
	if metricRequired && (len(value.Metrics) == 0 || len(value.Outcome.MetricIDs) == 0) {
		return fmt.Errorf("commercial capability: analytical outcome requires explicit metric definitions")
	}
	switch value.Domain {
	case DomainSales:
		if value.Kind == RecordAcquisition {
			if value.Outcome.Kind != OutcomeCommercial && value.Outcome.Kind != OutcomeEconomic {
				return fmt.Errorf("commercial capability: acquisition must be a commercial or economic outcome")
			}
		} else if value.Outcome.Kind != OutcomeActivity && value.Outcome.Kind != OutcomeOutput &&
			value.Outcome.Kind != OutcomeCommercial {
			return fmt.Errorf("commercial capability: sales outcome class is invalid")
		}
	case DomainGrowth:
		if value.Outcome.Kind != OutcomeCustomer && value.Outcome.Kind != OutcomeCommercial &&
			value.Outcome.Kind != OutcomeEconomic && value.Outcome.Kind != OutcomeStrategicLearning {
			return fmt.Errorf("commercial capability: growth outcome class is invalid")
		}
	case DomainCustomerOperations:
		if value.Outcome.Kind != OutcomeActivity && value.Outcome.Kind != OutcomeOutput &&
			value.Outcome.Kind != OutcomeCustomer && value.Outcome.Kind != OutcomeRisk {
			return fmt.Errorf("commercial capability: customer operation outcome class is invalid")
		}
	case DomainPricing:
		if value.Outcome.Kind != OutcomeOutput && value.Outcome.Kind != OutcomeEconomic {
			return fmt.Errorf("commercial capability: pricing outcome class is invalid")
		}
	case DomainFinance, DomainTreasury:
		if value.Outcome.Kind != OutcomeEconomic && value.Outcome.Kind != OutcomeRisk {
			return fmt.Errorf("commercial capability: finance or treasury outcome class is invalid")
		}
	}
	return nil
}

func requiredObservationKinds(kind RecordKind) []ObservationKind {
	switch kind {
	case RecordLead:
		return []ObservationKind{ObservationLeadSource}
	case RecordQualification, RecordPipeline:
		return []ObservationKind{ObservationCRMState}
	case RecordOutreachPlan:
		return []ObservationKind{ObservationConsent}
	case RecordSalesConversation:
		return []ObservationKind{ObservationConversation}
	case RecordProposalHandoff:
		return []ObservationKind{ObservationProposal}
	case RecordContractHandoff:
		return []ObservationKind{ObservationContract}
	case RecordAcquisition:
		return []ObservationKind{ObservationConversion, ObservationRevenue}
	case RecordGrowthExperiment:
		return []ObservationKind{ObservationCost, ObservationProductUsage}
	case RecordGrowthAcquisition:
		return []ObservationKind{ObservationConversion, ObservationCost}
	case RecordGrowthRetention:
		return []ObservationKind{ObservationRetention, ObservationRevenue}
	case RecordGrowthEconomics:
		return []ObservationKind{ObservationCost, ObservationRevenue}
	case RecordOnboarding:
		return []ObservationKind{ObservationProductUsage, ObservationSupport}
	case RecordSupportCase, RecordFeatureRequest, RecordIncidentCommunication:
		return []ObservationKind{ObservationSupport}
	case RecordCustomerHealth:
		return []ObservationKind{ObservationCustomerHealth, ObservationProductUsage}
	case RecordRetention:
		return []ObservationKind{ObservationRetention}
	case RecordChurn:
		return []ObservationKind{ObservationChurn}
	case RecordSLAResolution:
		return []ObservationKind{ObservationSLA}
	case RecordPricing, RecordPackaging:
		return []ObservationKind{ObservationCost, ObservationPrice}
	case RecordUnitEconomics:
		return []ObservationKind{ObservationCost, ObservationRevenue}
	case RecordCashPosition:
		return []ObservationKind{ObservationCash, ObservationLiability}
	case RecordRunway:
		return []ObservationKind{ObservationCash, ObservationCost, ObservationLiability}
	case RecordCapitalAllocation:
		return []ObservationKind{ObservationCapital, ObservationCash}
	case RecordRevenueForecast:
		return []ObservationKind{ObservationForecastActual, ObservationRevenue}
	case RecordInitiativeProfitability:
		return []ObservationKind{ObservationCost, ObservationInitiativeEconomics, ObservationRevenue}
	default:
		return nil
	}
}

func requiredHandoff(kind RecordKind) HandoffKind {
	switch kind {
	case RecordAcquisition:
		return HandoffSalesToCustomerOps
	case RecordProposalHandoff, RecordContractHandoff:
		return HandoffSalesToContract
	case RecordGrowthExperiment, RecordGrowthAcquisition, RecordGrowthRetention,
		RecordGrowthEconomics:
		return HandoffGrowthToSales
	case RecordFeatureRequest:
		return HandoffCustomerToProduct
	case RecordPricing, RecordPackaging:
		return HandoffPricingToSales
	case RecordUnitEconomics, RecordRevenueForecast, RecordInitiativeProfitability:
		return HandoffFinanceToExecutive
	case RecordCashPosition, RecordRunway, RecordCapitalAllocation:
		return HandoffTreasuryToFinance
	default:
		return ""
	}
}

type Record struct {
	Body      RecordBody          `json:"body"`
	Signature contracts.Signature `json:"signature"`
}

func (value Record) Validate() error {
	if err := value.Body.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type ReviewDisposition string

const (
	DispositionConfirmed    ReviewDisposition = "confirmed"
	DispositionContradicted ReviewDisposition = "contradicted"
	DispositionUnavailable  ReviewDisposition = "unavailable"
	DispositionStale        ReviewDisposition = "stale"
)

func (value ReviewDisposition) Valid() bool {
	switch value {
	case DispositionConfirmed, DispositionContradicted, DispositionUnavailable, DispositionStale:
		return true
	default:
		return false
	}
}

type ObservationReview struct {
	ObservationID              ObservationID          `json:"observation_id"`
	PrimaryEvidenceHash        contracts.ContentHash  `json:"primary_evidence_hash"`
	ReconciliationEvidenceHash *contracts.ContentHash `json:"reconciliation_evidence_hash"`
	Disposition                ReviewDisposition      `json:"disposition"`
	ReviewedAt                 time.Time              `json:"reviewed_at"`
}

func (value ObservationReview) Validate() error {
	if err := validateToken("review observation_id", string(value.ObservationID)); err != nil {
		return err
	}
	if err := value.PrimaryEvidenceHash.Validate(); err != nil {
		return fmt.Errorf("commercial capability: review primary hash: %w", err)
	}
	if value.ReconciliationEvidenceHash != nil {
		if err := value.ReconciliationEvidenceHash.Validate(); err != nil {
			return fmt.Errorf("commercial capability: review reconciliation hash: %w", err)
		}
	}
	if !value.Disposition.Valid() || !validUTC(value.ReviewedAt) {
		return fmt.Errorf("commercial capability: observation review disposition or time is invalid")
	}
	return nil
}

type ReviewProcedure struct {
	SchemaVersion   string                `json:"schema_version"`
	ID              string                `json:"procedure_id"`
	Version         uint64                `json:"version"`
	Domain          Domain                `json:"domain"`
	Checks          []string              `json:"checks"`
	RequiredSources []SourceClass         `json:"required_sources"`
	Digest          contracts.ContentHash `json:"digest"`
}

func (value ReviewProcedure) Validate() error {
	if value.SchemaVersion != ReviewSchemaVersion || value.Version != 1 || !value.Domain.Valid() {
		return fmt.Errorf("commercial capability: review procedure schema, version, or domain is invalid")
	}
	if err := validateToken("review procedure_id", value.ID); err != nil {
		return err
	}
	if err := validateTokens("review checks", value.Checks, 4, 32); err != nil {
		return err
	}
	if !sortedUnique(value.Checks) || len(value.RequiredSources) == 0 ||
		len(value.RequiredSources) > 16 || hasDuplicate(value.RequiredSources) {
		return fmt.Errorf("commercial capability: review procedure checks or sources are invalid")
	}
	for _, source := range value.RequiredSources {
		if !source.Valid() {
			return fmt.Errorf("commercial capability: review procedure source is invalid")
		}
	}
	expected, err := reviewProcedureDigest(value)
	if err != nil {
		return err
	}
	if value.Digest != expected {
		return fmt.Errorf("commercial capability: review procedure digest is invalid")
	}
	return nil
}

type IndependentReview struct {
	SchemaVersion        string                   `json:"schema_version"`
	ID                   contracts.VerdictID      `json:"verdict_id"`
	RecordID             RecordID                 `json:"record_id"`
	RecordHash           contracts.ContentHash    `json:"record_hash"`
	AuthorSeatID         contracts.SeatID         `json:"author_seat_id"`
	VerifierSeatID       contracts.SeatID         `json:"verifier_seat_id"`
	ProcedureID          string                   `json:"procedure_id"`
	ProcedureVersion     uint64                   `json:"procedure_version"`
	ProcedureDigest      contracts.ContentHash    `json:"procedure_digest"`
	ObservationReviews   []ObservationReview      `json:"observation_reviews"`
	CustomerBoundaryHash *contracts.ContentHash   `json:"customer_boundary_hash"`
	EconomicBoundaryHash *contracts.ContentHash   `json:"economic_boundary_hash"`
	Outcome              contracts.VerdictOutcome `json:"outcome"`
	Findings             []string                 `json:"findings"`
	VerifiedAt           time.Time                `json:"verified_at"`
	ExpiresAt            time.Time                `json:"expires_at"`
	Signature            contracts.Signature      `json:"signature"`
}

func (value IndependentReview) Validate() error {
	if value.SchemaVersion != ReviewSchemaVersion || value.ProcedureVersion != 1 ||
		!value.Outcome.Valid() {
		return fmt.Errorf("commercial capability: independent review schema or outcome is invalid")
	}
	for _, token := range []struct{ name, value string }{
		{"verdict_id", string(value.ID)}, {"record_id", string(value.RecordID)},
		{"author_seat_id", string(value.AuthorSeatID)}, {"verifier_seat_id", string(value.VerifierSeatID)},
		{"procedure_id", value.ProcedureID},
	} {
		if err := validateToken(token.name, token.value); err != nil {
			return err
		}
	}
	if value.AuthorSeatID == value.VerifierSeatID {
		return fmt.Errorf("commercial capability: author cannot independently review the record")
	}
	if err := value.RecordHash.Validate(); err != nil {
		return err
	}
	if err := value.ProcedureDigest.Validate(); err != nil {
		return err
	}
	if len(value.ObservationReviews) == 0 || len(value.ObservationReviews) > 128 {
		return fmt.Errorf("commercial capability: observation reviews are outside bounds")
	}
	ids := make([]ObservationID, 0, len(value.ObservationReviews))
	for _, review := range value.ObservationReviews {
		if err := review.Validate(); err != nil {
			return err
		}
		ids = append(ids, review.ObservationID)
	}
	if hasDuplicate(ids) {
		return fmt.Errorf("commercial capability: observation reviews are duplicated")
	}
	for _, hash := range []*contracts.ContentHash{value.CustomerBoundaryHash, value.EconomicBoundaryHash} {
		if hash != nil {
			if err := hash.Validate(); err != nil {
				return err
			}
		}
	}
	if err := validateTexts("review findings", value.Findings, 0, 64, 4096); err != nil {
		return err
	}
	if !validUTC(value.VerifiedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.VerifiedAt) {
		return fmt.Errorf("commercial capability: independent review validity interval is invalid")
	}
	return value.Signature.Validate()
}

type VerifiedRecord struct {
	Record Record            `json:"record"`
	Review IndependentReview `json:"review"`
}

func (value VerifiedRecord) Validate() error {
	return value.ValidateAt(value.Review.VerifiedAt)
}

func (value VerifiedRecord) ValidateAt(now time.Time) error {
	if err := value.Record.Validate(); err != nil {
		return err
	}
	if err := value.Review.Validate(); err != nil {
		return err
	}
	body := value.Record.Body
	if !validUTC(now) || !body.FreshUntil.After(now) || !value.Review.ExpiresAt.After(now) ||
		value.Review.Outcome != contracts.VerdictPass || value.Review.RecordID != body.ID ||
		value.Review.AuthorSeatID != body.AuthorSeatID || value.Review.VerifiedAt.Before(body.EffectiveAt) {
		return fmt.Errorf("commercial capability: record is stale or lacks a binding passing review")
	}
	hash, err := RecordHash(value.Record)
	if err != nil || hash != value.Review.RecordHash {
		return fmt.Errorf("commercial capability: independent review record hash is invalid")
	}
	procedure, err := ProcedureForRecord(body)
	if err != nil {
		return err
	}
	if value.Review.ProcedureID != procedure.ID || value.Review.ProcedureVersion != procedure.Version ||
		value.Review.ProcedureDigest != procedure.Digest {
		return fmt.Errorf("commercial capability: independent review procedure is not current")
	}
	if err := validateRequiredSources(body, procedure); err != nil {
		return err
	}
	if value.Review.ExpiresAt.After(body.FreshUntil) {
		return fmt.Errorf("commercial capability: independent review outlives the authoritative evidence")
	}
	if len(value.Review.ObservationReviews) != len(body.Observations) {
		return fmt.Errorf("commercial capability: independent review does not cover every observation")
	}
	for _, observation := range body.Observations {
		index := slices.IndexFunc(value.Review.ObservationReviews, func(review ObservationReview) bool {
			return review.ObservationID == observation.ID
		})
		if index < 0 {
			return fmt.Errorf("commercial capability: independent review is missing an observation")
		}
		review := value.Review.ObservationReviews[index]
		if review.Disposition != DispositionConfirmed ||
			review.PrimaryEvidenceHash != observation.Primary.Evidence.Hash ||
			review.ReviewedAt.Before(observation.ObservedAt) ||
			review.ReviewedAt.After(value.Review.VerifiedAt) ||
			len(observation.ConflictingEvidence) != 0 {
			return fmt.Errorf("commercial capability: independent review did not confirm authoritative evidence")
		}
		if observation.Reconciliation == nil && review.ReconciliationEvidenceHash != nil ||
			observation.Reconciliation != nil && (review.ReconciliationEvidenceHash == nil ||
				*review.ReconciliationEvidenceHash != observation.Reconciliation.Evidence.Hash) {
			return fmt.Errorf("commercial capability: reconciliation review binding is invalid")
		}
	}
	if body.Customer != nil {
		digest, err := CustomerBoundaryHash(*body.Customer)
		if err != nil || value.Review.CustomerBoundaryHash == nil || *value.Review.CustomerBoundaryHash != digest {
			return fmt.Errorf("commercial capability: customer boundary was not independently reviewed")
		}
	} else if value.Review.CustomerBoundaryHash != nil {
		return fmt.Errorf("commercial capability: review adds a customer boundary not present in the record")
	}
	if body.Economic != nil {
		digest, err := EconomicBoundaryHash(*body.Economic)
		if err != nil || value.Review.EconomicBoundaryHash == nil || *value.Review.EconomicBoundaryHash != digest {
			return fmt.Errorf("commercial capability: economic boundary was not independently reviewed")
		}
	} else if value.Review.EconomicBoundaryHash != nil {
		return fmt.Errorf("commercial capability: review adds an economic boundary not present in the record")
	}
	return nil
}

func RecordHash(value Record) (contracts.ContentHash, error) {
	if err := value.Body.Validate(); err != nil {
		return contracts.ContentHash{}, err
	}
	return contracts.HashCanonical(&value.Body)
}

func SignRecord(value *Record, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("commercial capability: record and Ed25519 author key are required")
	}
	if err := validateToken("author key id", keyID); err != nil {
		return err
	}
	payload, err := contracts.EncodeCanonical(&value.Body)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func SignReview(value *IndependentReview, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("commercial capability: review and Ed25519 verifier key are required")
	}
	if err := validateToken("review key id", keyID); err != nil {
		return err
	}
	value.Signature = signingPreimageSignature(keyID)
	payload, err := reviewSigningBytes(*value)
	if err != nil {
		return err
	}
	value.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return value.Validate()
}

func verifyRecord(value Record, publicKey ed25519.PublicKey) error {
	payload, err := contracts.EncodeCanonical(&value.Body)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("commercial capability: record signature is invalid")
	}
	return nil
}

func verifyReview(value IndependentReview, publicKey ed25519.PublicKey) error {
	payload, err := reviewSigningBytes(value)
	if err != nil {
		return err
	}
	signature, err := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if err != nil || len(publicKey) != ed25519.PublicKeySize ||
		!ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("commercial capability: review signature is invalid")
	}
	return nil
}

func reviewSigningBytes(value IndependentReview) ([]byte, error) {
	value.Signature = signingPreimageSignature(value.Signature.KeyID)
	return contracts.EncodeCanonical(&value)
}

func signingPreimageSignature(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func digestBytes(value []byte) contracts.ContentHash {
	sum := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}
