// Package mission owns the founder-signed business authority that bounds an
// autonomous Workforce organization.
package mission

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

const (
	// MissionSchemaVersion is the canonical Founder Mission schema.
	MissionSchemaVersion = "workforce.founder-mission.v1"
	// ConstitutionSchemaVersion is the canonical Company Constitution schema.
	ConstitutionSchemaVersion = "workforce.company-constitution.v1"
	// CapitalSchemaVersion is the canonical capital-envelope schema.
	CapitalSchemaVersion = "workforce.capital-envelope.v1"
	// IssuerPolicySchemaVersion is the canonical company issuer-policy schema.
	IssuerPolicySchemaVersion = "workforce.company-issuer-policy.v1"
	// OrganizationV2SchemaVersion is the canonical capability-era organization schema.
	OrganizationV2SchemaVersion = "workforce.organization.v2"
)

// RiskTolerance is the closed founder-selected operating risk class.
type RiskTolerance string

const (
	RiskToleranceLow      RiskTolerance = "low"
	RiskToleranceModerate RiskTolerance = "moderate"
	RiskToleranceHigh     RiskTolerance = "high"
)

// Valid reports whether the risk class is executable.
func (value RiskTolerance) Valid() bool {
	switch value {
	case RiskToleranceLow, RiskToleranceModerate, RiskToleranceHigh:
		return true
	default:
		return false
	}
}

// AutonomyLevel is the maximum decision autonomy granted by the founder.
type AutonomyLevel string

const (
	AutonomySupervised     AutonomyLevel = "supervised"
	AutonomyReviewRequired AutonomyLevel = "review_required"
	AutonomyBoundedAuto    AutonomyLevel = "bounded_auto"
)

// Valid reports whether the autonomy class is executable.
func (value AutonomyLevel) Valid() bool {
	switch value {
	case AutonomySupervised, AutonomyReviewRequired, AutonomyBoundedAuto:
		return true
	default:
		return false
	}
}

// OperatingScopeKind identifies an owner-connected external authority domain.
// A scope is a reference to a connection, never a credential container.
type OperatingScopeKind string

const (
	OperatingScopeProject   OperatingScopeKind = "project"
	OperatingScopeBrowser   OperatingScopeKind = "browser"
	OperatingScopeChannel   OperatingScopeKind = "channel"
	OperatingScopeCustomer  OperatingScopeKind = "customer"
	OperatingScopeFinancial OperatingScopeKind = "financial"
)

// Valid reports whether the scope kind has a closed authority meaning.
func (value OperatingScopeKind) Valid() bool {
	switch value {
	case OperatingScopeProject, OperatingScopeBrowser, OperatingScopeChannel,
		OperatingScopeCustomer, OperatingScopeFinancial:
		return true
	default:
		return false
	}
}

// OperatingScope is one founder-authorized connection boundary. ScopeID names
// an already connected resource and MUST NOT contain a secret or credential.
type OperatingScope struct {
	Kind                OperatingScopeKind `json:"kind"`
	ScopeID             string             `json:"scope_id"`
	Purpose             string             `json:"purpose"`
	AllowedActions      []string           `json:"allowed_actions"`
	DataClassifications []string           `json:"data_classifications"`
	Jurisdictions       []string           `json:"jurisdictions"`
	ExpiresAt           *time.Time         `json:"expires_at"`
}

// Validate rejects open-ended, unclassified, expired, or non-canonical scopes.
func (value OperatingScope) Validate(effectiveAt time.Time) error {
	if !value.Kind.Valid() {
		return fmt.Errorf("mission: operating scope kind %q is invalid", value.Kind)
	}
	if err := validateToken("operating scope id", value.ScopeID); err != nil {
		return err
	}
	if err := validateText("operating scope purpose", value.Purpose, 1024); err != nil {
		return err
	}
	for name, entries := range map[string][]string{
		"operating scope allowed actions":      value.AllowedActions,
		"operating scope data classifications": value.DataClassifications,
		"operating scope jurisdictions":        value.Jurisdictions,
	} {
		if err := validateStringSet(name, entries, 1, 32, 255); err != nil {
			return err
		}
	}
	if value.ExpiresAt != nil && (!validUTC(*value.ExpiresAt) || !value.ExpiresAt.After(effectiveAt)) {
		return fmt.Errorf("mission: operating scope expiry is invalid")
	}
	return nil
}

// ReservedDecisionKind identifies authority that the company controller can
// never acquire from a Work Order or Executive decision.
type ReservedDecisionKind string

const (
	ReservedMissionChange       ReservedDecisionKind = "mission_change"
	ReservedConstitutionChange  ReservedDecisionKind = "constitution_change"
	ReservedCapitalIncrease     ReservedDecisionKind = "aggregate_capital_increase"
	ReservedDebtOrLeverage      ReservedDecisionKind = "debt_or_leverage"
	ReservedMaterialTransfer    ReservedDecisionKind = "material_transfer"
	ReservedRestrictedRegion    ReservedDecisionKind = "restricted_jurisdiction"
	ReservedCustodyOrWithdrawal ReservedDecisionKind = "custody_or_withdrawal"
	ReservedCorporateAction     ReservedDecisionKind = "irreversible_corporate_action"
	ReservedControlRelaxation   ReservedDecisionKind = "control_relaxation"
)

// Valid reports whether the reserved decision has a closed meaning.
func (value ReservedDecisionKind) Valid() bool {
	switch value {
	case ReservedMissionChange, ReservedConstitutionChange,
		ReservedCapitalIncrease, ReservedDebtOrLeverage,
		ReservedMaterialTransfer, ReservedRestrictedRegion,
		ReservedCustodyOrWithdrawal, ReservedCorporateAction,
		ReservedControlRelaxation:
		return true
	default:
		return false
	}
}

// ReservedDecision binds a founder-only decision to a stable clause and an
// exact escalation condition.
type ReservedDecision struct {
	ClauseID    string               `json:"clause_id"`
	Kind        ReservedDecisionKind `json:"kind"`
	Description string               `json:"description"`
	Escalation  string               `json:"escalation"`
}

// Validate rejects an incomplete or open-ended reserved decision.
func (value ReservedDecision) Validate() error {
	if err := validateToken("reserved decision clause", value.ClauseID); err != nil {
		return err
	}
	if !value.Kind.Valid() {
		return fmt.Errorf("mission: invalid reserved decision %q", value.Kind)
	}
	if err := validateText("reserved decision description", value.Description, 512); err != nil {
		return err
	}
	return validateText("reserved decision escalation", value.Escalation, 512)
}

// FounderMission is the immutable, versioned statement of company purpose.
// Its canonical identity is organization_id plus version.
type FounderMission struct {
	SchemaVersion            string                   `json:"schema_version"`
	ID                       string                   `json:"mission_id"`
	Version                  uint64                   `json:"version"`
	OrganizationID           contracts.OrganizationID `json:"organization_id"`
	OwnerID                  contracts.OwnerID        `json:"owner_id"`
	Purpose                  string                   `json:"purpose"`
	PermittedBusinessDomains []string                 `json:"permitted_business_domains"`
	StrategicPrinciples      []string                 `json:"strategic_principles"`
	TargetOutcomes           []string                 `json:"target_outcomes"`
	SuccessConditions        []string                 `json:"success_conditions"`
	FailureConditions        []string                 `json:"failure_conditions"`
	EffectiveAt              time.Time                `json:"effective_at"`
	Signature                contracts.Signature      `json:"signature"`
}

// Validate enforces a bounded mission with explicit success and failure.
func (value FounderMission) Validate() error {
	if value.SchemaVersion != MissionSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.OwnerID == "" ||
		value.ID != "mission:"+string(value.OrganizationID) {
		return fmt.Errorf("mission: founder mission identity is invalid")
	}
	if err := validateText("mission purpose", value.Purpose, 4096); err != nil {
		return err
	}
	for name, entries := range map[string][]string{
		"permitted business domains": value.PermittedBusinessDomains,
		"strategic principles":       value.StrategicPrinciples,
		"target outcomes":            value.TargetOutcomes,
		"success conditions":         value.SuccessConditions,
		"failure conditions":         value.FailureConditions,
	} {
		if err := validateStringSet(name, entries, 1, 32, 1024); err != nil {
			return err
		}
	}
	if !validUTC(value.EffectiveAt) {
		return fmt.Errorf("mission: effective_at must be non-zero UTC")
	}
	return value.Signature.Validate()
}

// CompanyConstitution is the immutable, versioned operating boundary for the
// company controller, Executive seats, and every external effect.
type CompanyConstitution struct {
	SchemaVersion           string                   `json:"schema_version"`
	ID                      string                   `json:"constitution_id"`
	Version                 uint64                   `json:"version"`
	OrganizationID          contracts.OrganizationID `json:"organization_id"`
	OwnerID                 contracts.OwnerID        `json:"owner_id"`
	LegalProhibitions       []string                 `json:"legal_prohibitions"`
	EthicalProhibitions     []string                 `json:"ethical_prohibitions"`
	PermittedJurisdictions  []string                 `json:"permitted_jurisdictions"`
	DataBoundaries          []string                 `json:"data_boundaries"`
	PermittedCounterparties []string                 `json:"permitted_counterparties"`
	OperatingScopes         []OperatingScope         `json:"operating_scopes"`
	RiskTolerance           RiskTolerance            `json:"risk_tolerance"`
	Autonomy                AutonomyLevel            `json:"autonomy"`
	ReservedDecisions       []ReservedDecision       `json:"reserved_decisions"`
	EscalationConditions    []string                 `json:"escalation_conditions"`
	PauseConditions         []string                 `json:"pause_conditions"`
	ShutdownConditions      []string                 `json:"shutdown_conditions"`
	EffectiveAt             time.Time                `json:"effective_at"`
	Signature               contracts.Signature      `json:"signature"`
}

// Validate enforces explicit prohibitions, jurisdictions, data boundaries,
// founder reservations, and safe-stop conditions.
func (value CompanyConstitution) Validate() error {
	if value.SchemaVersion != ConstitutionSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.OwnerID == "" ||
		value.ID != "constitution:"+string(value.OrganizationID) {
		return fmt.Errorf("mission: company constitution identity is invalid")
	}
	if !value.RiskTolerance.Valid() || !value.Autonomy.Valid() {
		return fmt.Errorf("mission: constitution risk or autonomy is invalid")
	}
	for name, entries := range map[string][]string{
		"legal prohibitions":       value.LegalProhibitions,
		"ethical prohibitions":     value.EthicalProhibitions,
		"permitted jurisdictions":  value.PermittedJurisdictions,
		"data boundaries":          value.DataBoundaries,
		"permitted counterparties": value.PermittedCounterparties,
		"escalation conditions":    value.EscalationConditions,
		"pause conditions":         value.PauseConditions,
		"shutdown conditions":      value.ShutdownConditions,
	} {
		if err := validateStringSet(name, entries, 1, 64, 1024); err != nil {
			return err
		}
	}
	if len(value.ReservedDecisions) != 9 {
		return fmt.Errorf("mission: constitution requires all nine reserved decisions")
	}
	kinds := make([]string, len(value.ReservedDecisions))
	for index := range value.ReservedDecisions {
		if err := value.ReservedDecisions[index].Validate(); err != nil {
			return fmt.Errorf("mission: reserved decision %d: %w", index, err)
		}
		kinds[index] = string(value.ReservedDecisions[index].Kind)
	}
	if !slices.IsSorted(kinds) || adjacentDuplicate(kinds) {
		return fmt.Errorf("mission: reserved decisions must be sorted and unique")
	}
	if len(value.OperatingScopes) == 0 || len(value.OperatingScopes) > 32 {
		return fmt.Errorf("mission: constitution requires 1 to 32 operating scopes")
	}
	previousScope := ""
	for index := range value.OperatingScopes {
		if err := value.OperatingScopes[index].Validate(value.EffectiveAt); err != nil {
			return fmt.Errorf("mission: operating scope %d: %w", index, err)
		}
		scopeKey := string(value.OperatingScopes[index].Kind) + "\x00" + value.OperatingScopes[index].ScopeID
		if scopeKey <= previousScope {
			return fmt.Errorf("mission: operating scopes must be sorted and unique")
		}
		previousScope = scopeKey
	}
	if !validUTC(value.EffectiveAt) {
		return fmt.Errorf("mission: constitution effective_at must be non-zero UTC")
	}
	return value.Signature.Validate()
}

// CapitalEnvelope is the exact founder-delegated company capital and exposure
// boundary. Monetary values are integer microunits.
type CapitalEnvelope struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        string                   `json:"capital_envelope_id"`
	Version                   uint64                   `json:"version"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	Currency                  string                   `json:"currency"`
	StartingMicrounits        uint64                   `json:"starting_microunits"`
	SpendCeilingMicrounits    uint64                   `json:"spend_ceiling_microunits"`
	ExposureCeilingMicrounits uint64                   `json:"exposure_ceiling_microunits"`
	MinimumRunwayDays         uint32                   `json:"minimum_runway_days"`
	EffectiveAt               time.Time                `json:"effective_at"`
	Signature                 contracts.Signature      `json:"signature"`
}

// Validate rejects zero, internally inconsistent, or binary-floating capital.
func (value CapitalEnvelope) Validate() error {
	if value.SchemaVersion != CapitalSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" ||
		value.ID != "capital:"+string(value.OrganizationID) {
		return fmt.Errorf("mission: capital envelope identity is invalid")
	}
	if err := validateToken("capital currency", value.Currency); err != nil {
		return err
	}
	if value.StartingMicrounits == 0 || value.SpendCeilingMicrounits == 0 ||
		value.ExposureCeilingMicrounits == 0 ||
		value.SpendCeilingMicrounits > value.StartingMicrounits ||
		value.ExposureCeilingMicrounits > value.StartingMicrounits ||
		value.MinimumRunwayDays == 0 || value.MinimumRunwayDays > 3650 {
		return fmt.Errorf("mission: capital envelope limits are invalid")
	}
	if !validUTC(value.EffectiveAt) {
		return fmt.Errorf("mission: capital effective_at must be non-zero UTC")
	}
	return value.Signature.Validate()
}

// CompanyIssuerPolicy delegates only bounded Work Order issuance to the
// deterministic company controller; it never delegates founder authority.
type CompanyIssuerPolicy struct {
	SchemaVersion           string                   `json:"schema_version"`
	ID                      string                   `json:"company_issuer_policy_id"`
	Version                 uint64                   `json:"version"`
	OrganizationID          contracts.OrganizationID `json:"organization_id"`
	IssuerKeyID             string                   `json:"issuer_key_id"`
	IssuerPublicKey         string                   `json:"issuer_public_key"`
	MissionVersion          uint64                   `json:"mission_version"`
	ConstitutionVersion     uint64                   `json:"constitution_version"`
	CapitalEnvelopeVersion  uint64                   `json:"capital_envelope_version"`
	AllowedWorkOrderClasses []string                 `json:"allowed_work_order_classes"`
	MaxWorkOrderMicrounits  uint64                   `json:"max_work_order_microunits"`
	EffectiveAt             time.Time                `json:"effective_at"`
	ExpiresAt               time.Time                `json:"expires_at"`
	Signature               contracts.Signature      `json:"signature"`
}

// Validate enforces a current, bounded controller delegation.
func (value CompanyIssuerPolicy) Validate() error {
	if value.SchemaVersion != IssuerPolicySchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" ||
		value.ID != "company-issuer-policy:"+string(value.OrganizationID) ||
		value.MissionVersion == 0 || value.ConstitutionVersion == 0 ||
		value.CapitalEnvelopeVersion == 0 || value.MaxWorkOrderMicrounits == 0 {
		return fmt.Errorf("mission: company issuer policy identity is invalid")
	}
	if err := validateToken("issuer key", value.IssuerKeyID); err != nil {
		return err
	}
	if err := validateToken("issuer public key", value.IssuerPublicKey); err != nil {
		return err
	}
	issuerPublicKey, err := base64.RawURLEncoding.DecodeString(value.IssuerPublicKey)
	if err != nil || len(issuerPublicKey) != 32 {
		return fmt.Errorf("mission: company issuer public key is invalid")
	}
	if err := validateStringSet("allowed Work Order classes", value.AllowedWorkOrderClasses, 1, 16, 128); err != nil {
		return err
	}
	if !validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("mission: company issuer policy times are invalid")
	}
	return value.Signature.Validate()
}

// OrganizationV2 is the immutable founder-signed projection contract that
// binds the executable organization template to the exact company authority.
type OrganizationV2 struct {
	SchemaVersion          string                   `json:"schema_version"`
	ID                     string                   `json:"organization_v2_id"`
	Version                uint64                   `json:"version"`
	OrganizationID         contracts.OrganizationID `json:"organization_id"`
	OwnerID                contracts.OwnerID        `json:"owner_id"`
	TemplateID             string                   `json:"template_id"`
	TemplateVersion        uint64                   `json:"template_version"`
	MissionVersion         uint64                   `json:"mission_version"`
	ConstitutionVersion    uint64                   `json:"constitution_version"`
	CapitalEnvelopeVersion uint64                   `json:"capital_envelope_version"`
	IssuerPolicyVersion    uint64                   `json:"issuer_policy_version"`
	EffectiveAt            time.Time                `json:"effective_at"`
	Signature              contracts.Signature      `json:"signature"`
}

// Validate enforces the default v1 topology projection without granting any
// capability or authority beyond the separately signed records it references.
func (value OrganizationV2) Validate() error {
	if value.SchemaVersion != OrganizationV2SchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.OwnerID == "" ||
		value.ID != "organization-v2:"+string(value.OrganizationID) ||
		value.TemplateID != "organization-template:default-v1" ||
		value.TemplateVersion != 1 || value.MissionVersion == 0 ||
		value.ConstitutionVersion == 0 || value.CapitalEnvelopeVersion == 0 ||
		value.IssuerPolicyVersion == 0 || !validUTC(value.EffectiveAt) {
		return fmt.Errorf("mission: organization-v2 authority is invalid")
	}
	return value.Signature.Validate()
}

// ActivationAuthority is the closed founder-signed authority set installed
// atomically with the compatible organization projection.
type ActivationAuthority struct {
	Mission      FounderMission      `json:"mission"`
	Constitution CompanyConstitution `json:"constitution"`
	Capital      CapitalEnvelope     `json:"capital_envelope"`
	IssuerPolicy CompanyIssuerPolicy `json:"company_issuer_policy"`
	Organization OrganizationV2      `json:"organization_v2"`
}

// Validate enforces cross-record identity, version, time, and capital bounds.
func (value ActivationAuthority) Validate() error {
	if err := value.Mission.Validate(); err != nil {
		return err
	}
	if err := value.Constitution.Validate(); err != nil {
		return err
	}
	if err := value.Capital.Validate(); err != nil {
		return err
	}
	if err := value.IssuerPolicy.Validate(); err != nil {
		return err
	}
	if err := value.Organization.Validate(); err != nil {
		return err
	}
	organizationID := value.Mission.OrganizationID
	if value.Constitution.OrganizationID != organizationID ||
		value.Capital.OrganizationID != organizationID ||
		value.IssuerPolicy.OrganizationID != organizationID ||
		value.Organization.OrganizationID != organizationID ||
		value.Constitution.OwnerID != value.Mission.OwnerID ||
		value.Organization.OwnerID != value.Mission.OwnerID ||
		value.Constitution.Version != value.Mission.Version ||
		value.Capital.Version != value.Mission.Version ||
		value.IssuerPolicy.Version != value.Mission.Version ||
		value.Organization.Version != value.Mission.Version ||
		value.Mission.EffectiveAt != value.Constitution.EffectiveAt ||
		value.Mission.EffectiveAt != value.Capital.EffectiveAt ||
		value.Mission.EffectiveAt != value.IssuerPolicy.EffectiveAt ||
		value.Mission.EffectiveAt != value.Organization.EffectiveAt ||
		value.IssuerPolicy.MissionVersion != value.Mission.Version ||
		value.IssuerPolicy.ConstitutionVersion != value.Constitution.Version ||
		value.IssuerPolicy.CapitalEnvelopeVersion != value.Capital.Version ||
		value.Organization.MissionVersion != value.Mission.Version ||
		value.Organization.ConstitutionVersion != value.Constitution.Version ||
		value.Organization.CapitalEnvelopeVersion != value.Capital.Version ||
		value.Organization.IssuerPolicyVersion != value.IssuerPolicy.Version ||
		value.IssuerPolicy.MaxWorkOrderMicrounits > value.Capital.SpendCeilingMicrounits {
		return fmt.Errorf("mission: activation authority bindings are inconsistent")
	}
	return nil
}

func validateText(name, value string, max int) error {
	if strings.TrimSpace(value) == "" || len(value) > max {
		return fmt.Errorf("mission: %s must contain 1 to %d bytes", name, max)
	}
	return nil
}

func validateToken(name, value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 255 || strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("mission: %s is invalid", name)
	}
	return nil
}

func validateStringSet(name string, values []string, minimum, maximum, maxBytes int) error {
	if len(values) < minimum || len(values) > maximum {
		return fmt.Errorf("mission: %s must contain %d to %d entries", name, minimum, maximum)
	}
	for index := range values {
		if err := validateText(name, values[index], maxBytes); err != nil {
			return err
		}
	}
	if !slices.IsSorted(values) || adjacentDuplicate(values) {
		return fmt.Errorf("mission: %s must be sorted and unique", name)
	}
	return nil
}

func adjacentDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
