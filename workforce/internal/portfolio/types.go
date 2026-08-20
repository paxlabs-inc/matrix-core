// Package portfolio owns evidence-backed opportunity intake and deterministic
// portfolio decisions inside founder-signed company authority.
package portfolio

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/companystate"
	"centra/workforce/internal/contracts"
)

const (
	// OpportunitySchemaVersion is the canonical opportunity wire schema.
	OpportunitySchemaVersion = "workforce.opportunity.v1"
	// DecisionSchemaVersion is the canonical portfolio decision wire schema.
	DecisionSchemaVersion = "workforce.portfolio-decision.v1"
	// ProcedureSchemaVersion is the canonical scoring procedure schema.
	ProcedureSchemaVersion = "workforce.portfolio-procedure.v1"
)

// OpportunityID identifies one immutable versioned opportunity chain.
type OpportunityID string

// DecisionID identifies one immutable portfolio decision.
type DecisionID string

// InitiativeID identifies the funded company initiative created by a GO decision.
type InitiativeID string

// SourceKind is the closed authority class that introduced an opportunity.
type SourceKind string

const (
	SourceFounder  SourceKind = "founder"
	SourceResearch SourceKind = "research"
	SourceCustomer SourceKind = "customer"
	SourceMarket   SourceKind = "market"
	SourceProduct  SourceKind = "product"
	SourceSales    SourceKind = "sales"
	SourceFinance  SourceKind = "financial"
	SourceLearning SourceKind = "learning"
)

// Valid reports whether the source has a closed company meaning.
func (value SourceKind) Valid() bool {
	switch value {
	case SourceFounder, SourceResearch, SourceCustomer, SourceMarket,
		SourceProduct, SourceSales, SourceFinance, SourceLearning:
		return true
	default:
		return false
	}
}

// Opportunity is an immutable evidence-backed candidate for company work.
type Opportunity struct {
	SchemaVersion              string                         `json:"schema_version"`
	ID                         OpportunityID                  `json:"opportunity_id"`
	Version                    uint64                         `json:"version"`
	OrganizationID             contracts.OrganizationID       `json:"organization_id"`
	CanonicalIdentity          string                         `json:"canonical_identity"`
	SourceKind                 SourceKind                     `json:"source_kind"`
	AuthorSeatID               contracts.SeatID               `json:"author_seat_id"`
	TargetCustomerRecordID     contracts.RecordID             `json:"target_customer_record_id"`
	ProblemStatement           string                         `json:"problem_statement"`
	ProposedValue              string                         `json:"proposed_value"`
	EstimatedCapitalMicrounits uint64                         `json:"estimated_capital_microunits"`
	TimeToEvidenceDays         uint32                         `json:"time_to_evidence_days"`
	SourceRecords              []companystate.RecordReference `json:"source_records"`
	Evidence                   []contracts.EvidenceRef        `json:"evidence"`
	SubmittedAt                time.Time                      `json:"submitted_at"`
	ExpiresAt                  time.Time                      `json:"expires_at"`
	Signature                  contracts.Signature            `json:"signature"`
}

// Validate rejects unscoped, unbounded, unsupported, stale, or non-canonical intake.
func (value Opportunity) Validate() error {
	if value.SchemaVersion != OpportunitySchemaVersion || value.ID == "" ||
		value.Version == 0 || value.OrganizationID == "" || value.AuthorSeatID == "" ||
		!value.SourceKind.Valid() || value.TargetCustomerRecordID == "" {
		return fmt.Errorf("portfolio: opportunity identity or source is invalid")
	}
	if err := validateToken("canonical identity", value.CanonicalIdentity); err != nil {
		return err
	}
	if err := validateText("problem statement", value.ProblemStatement, 4096); err != nil {
		return err
	}
	if err := validateText("proposed value", value.ProposedValue, 4096); err != nil {
		return err
	}
	if value.EstimatedCapitalMicrounits == 0 || value.TimeToEvidenceDays == 0 ||
		value.TimeToEvidenceDays > 365 || len(value.SourceRecords) == 0 ||
		len(value.SourceRecords) > 256 || len(value.Evidence) == 0 || len(value.Evidence) > 256 {
		return fmt.Errorf("portfolio: opportunity evidence, capital, or time is outside bounds")
	}
	previousRecord := ""
	targetCustomerFound := false
	for index := range value.SourceRecords {
		if err := value.SourceRecords[index].Validate(); err != nil {
			return fmt.Errorf("portfolio: source record %d: %w", index, err)
		}
		key := string(value.SourceRecords[index].ID)
		if key <= previousRecord {
			return fmt.Errorf("portfolio: source records must be sorted and unique")
		}
		previousRecord = key
		if key == string(value.TargetCustomerRecordID) {
			targetCustomerFound = true
		}
	}
	if !targetCustomerFound {
		return fmt.Errorf("portfolio: target customer must be included in source records")
	}
	previousEvidence := ""
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("portfolio: evidence %d: %w", index, err)
		}
		key := string(value.Evidence[index].ID)
		if key <= previousEvidence {
			return fmt.Errorf("portfolio: evidence must be sorted and unique")
		}
		previousEvidence = key
	}
	if !validUTC(value.SubmittedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.SubmittedAt) {
		return fmt.Errorf("portfolio: opportunity times are invalid")
	}
	return value.Signature.Validate()
}

// FactorScores contains deterministic basis-point assessments. Uncertainty is
// a penalty; every other factor is positive evidence.
type FactorScores struct {
	MissionFitBPS        uint16 `json:"mission_fit_bps"`
	DemandStrengthBPS    uint16 `json:"demand_strength_bps"`
	ExpectedValueBPS     uint16 `json:"expected_value_bps"`
	UnitEconomicsBPS     uint16 `json:"unit_economics_bps"`
	TimeToEvidenceBPS    uint16 `json:"time_to_evidence_bps"`
	CapitalEfficiencyBPS uint16 `json:"capital_efficiency_bps"`
	OpportunityCostBPS   uint16 `json:"opportunity_cost_bps"`
	LegalSafetyBPS       uint16 `json:"legal_safety_bps"`
	SecuritySafetyBPS    uint16 `json:"security_safety_bps"`
	OperatingCapacityBPS uint16 `json:"operating_capacity_bps"`
	UncertaintyBPS       uint16 `json:"uncertainty_bps"`
}

// Validate enforces exact basis-point bounds.
func (value FactorScores) Validate() error {
	for _, item := range []struct {
		name  string
		score uint16
	}{
		{"mission fit", value.MissionFitBPS}, {"demand strength", value.DemandStrengthBPS},
		{"expected value", value.ExpectedValueBPS}, {"unit economics", value.UnitEconomicsBPS},
		{"time to evidence", value.TimeToEvidenceBPS}, {"capital efficiency", value.CapitalEfficiencyBPS},
		{"opportunity cost", value.OpportunityCostBPS}, {"legal safety", value.LegalSafetyBPS},
		{"security safety", value.SecuritySafetyBPS}, {"operating capacity", value.OperatingCapacityBPS},
		{"uncertainty", value.UncertaintyBPS},
	} {
		if item.score > 10_000 {
			return fmt.Errorf("portfolio: %s score exceeds 10000 basis points", item.name)
		}
	}
	return nil
}

// FactorWeights defines the exact versioned decision procedure weights.
type FactorWeights struct {
	MissionFit        uint16 `json:"mission_fit"`
	DemandStrength    uint16 `json:"demand_strength"`
	ExpectedValue     uint16 `json:"expected_value"`
	UnitEconomics     uint16 `json:"unit_economics"`
	TimeToEvidence    uint16 `json:"time_to_evidence"`
	CapitalEfficiency uint16 `json:"capital_efficiency"`
	OpportunityCost   uint16 `json:"opportunity_cost"`
	LegalSafety       uint16 `json:"legal_safety"`
	SecuritySafety    uint16 `json:"security_safety"`
	OperatingCapacity uint16 `json:"operating_capacity"`
	Certainty         uint16 `json:"certainty"`
}

// Validate requires weights to total exactly 10000 basis points.
func (value FactorWeights) Validate() error {
	total := uint32(value.MissionFit) + uint32(value.DemandStrength) +
		uint32(value.ExpectedValue) + uint32(value.UnitEconomics) +
		uint32(value.TimeToEvidence) + uint32(value.CapitalEfficiency) +
		uint32(value.OpportunityCost) + uint32(value.LegalSafety) +
		uint32(value.SecuritySafety) + uint32(value.OperatingCapacity) +
		uint32(value.Certainty)
	if total != 10_000 {
		return fmt.Errorf("portfolio: factor weights must total 10000 basis points")
	}
	return nil
}

// DecisionProcedure is the immutable deterministic portfolio selection policy.
type DecisionProcedure struct {
	SchemaVersion            string                   `json:"schema_version"`
	ID                       string                   `json:"procedure_id"`
	Version                  uint64                   `json:"version"`
	OrganizationID           contracts.OrganizationID `json:"organization_id"`
	Weights                  FactorWeights            `json:"weights"`
	GOThresholdBPS           uint16                   `json:"go_threshold_bps"`
	ValidateThresholdBPS     uint16                   `json:"validate_threshold_bps"`
	NO_GOThresholdBPS        uint16                   `json:"no_go_threshold_bps"`
	MinimumLegalSafetyBPS    uint16                   `json:"minimum_legal_safety_bps"`
	MinimumSecuritySafetyBPS uint16                   `json:"minimum_security_safety_bps"`
	MaximumActiveInitiatives uint16                   `json:"maximum_active_initiatives"`
	MaximumNoEvidenceCycles  uint16                   `json:"maximum_no_evidence_cycles"`
	MaximumCapitalMicrounits uint64                   `json:"maximum_capital_microunits"`
	MaximumRiskMicrounits    uint64                   `json:"maximum_risk_microunits"`
	EffectiveAt              time.Time                `json:"effective_at"`
	ExpiresAt                *time.Time               `json:"expires_at"`
	AuthorityClauses         []string                 `json:"authority_clauses"`
	Signature                contracts.Signature      `json:"signature"`
}

// Validate enforces monotonic thresholds, resource limits, and canonical clauses.
func (value DecisionProcedure) Validate() error {
	if value.SchemaVersion != ProcedureSchemaVersion || value.ID == "" ||
		value.Version == 0 || value.OrganizationID == "" {
		return fmt.Errorf("portfolio: procedure identity is invalid")
	}
	if err := value.Weights.Validate(); err != nil {
		return err
	}
	if value.GOThresholdBPS > 10_000 || value.ValidateThresholdBPS > value.GOThresholdBPS ||
		value.NO_GOThresholdBPS > value.ValidateThresholdBPS ||
		value.MinimumLegalSafetyBPS > 10_000 || value.MinimumSecuritySafetyBPS > 10_000 ||
		value.MaximumActiveInitiatives == 0 || value.MaximumActiveInitiatives > 32 ||
		value.MaximumNoEvidenceCycles == 0 || value.MaximumCapitalMicrounits == 0 ||
		value.MaximumRiskMicrounits == 0 || len(value.AuthorityClauses) == 0 ||
		len(value.AuthorityClauses) > 64 {
		return fmt.Errorf("portfolio: procedure thresholds or limits are invalid")
	}
	if !sortedUniqueText(value.AuthorityClauses) || !validUTC(value.EffectiveAt) ||
		value.ExpiresAt != nil && (!validUTC(*value.ExpiresAt) || !value.ExpiresAt.After(value.EffectiveAt)) {
		return fmt.Errorf("portfolio: procedure clauses or times are invalid")
	}
	return value.Signature.Validate()
}

// Assessment is a closed independent review of one opportunity.
type Assessment struct {
	Scores          FactorScores                   `json:"scores"`
	Evidence        []companystate.RecordReference `json:"evidence"`
	Dissent         []string                       `json:"dissent"`
	UnresolvedRisks []string                       `json:"unresolved_risks"`
	RiskMicrounits  uint64                         `json:"risk_microunits"`
	AssessedAt      time.Time                      `json:"assessed_at"`
}

// Validate requires current, canonical, independently referential assessment evidence.
func (value Assessment) Validate() error {
	if err := value.Scores.Validate(); err != nil {
		return err
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 256 ||
		len(value.Dissent) > 64 || len(value.UnresolvedRisks) > 64 ||
		!validUTC(value.AssessedAt) {
		return fmt.Errorf("portfolio: assessment evidence or time is invalid")
	}
	previous := ""
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return err
		}
		if string(value.Evidence[index].ID) <= previous {
			return fmt.Errorf("portfolio: assessment evidence must be sorted and unique")
		}
		previous = string(value.Evidence[index].ID)
	}
	if !sortedUniqueText(value.Dissent) || !sortedUniqueText(value.UnresolvedRisks) {
		return fmt.Errorf("portfolio: dissent and risk lists must be sorted and unique")
	}
	return nil
}

// DecisionKind is a closed company portfolio action.
type DecisionKind string

const (
	DecisionGO         DecisionKind = "go"
	DecisionNO_GO      DecisionKind = "no_go"
	DecisionValidate   DecisionKind = "validate"
	DecisionDefer      DecisionKind = "defer"
	DecisionReject     DecisionKind = "reject"
	DecisionPrioritize DecisionKind = "prioritize"
	DecisionPause      DecisionKind = "pause"
	DecisionResume     DecisionKind = "resume"
	DecisionReallocate DecisionKind = "reallocate"
	DecisionScale      DecisionKind = "scale"
	DecisionPivot      DecisionKind = "pivot"
	DecisionMaintain   DecisionKind = "maintain"
	DecisionTerminate  DecisionKind = "terminate"
	DecisionEscalate   DecisionKind = "escalate"
)

// Valid reports whether the action has an executable meaning.
func (value DecisionKind) Valid() bool {
	switch value {
	case DecisionGO, DecisionNO_GO, DecisionValidate, DecisionDefer, DecisionReject,
		DecisionPrioritize, DecisionPause, DecisionResume, DecisionReallocate,
		DecisionScale, DecisionPivot, DecisionMaintain, DecisionTerminate, DecisionEscalate:
		return true
	default:
		return false
	}
}

// Alternative records one evaluated option and its deterministic score.
type Alternative struct {
	OpportunityID OpportunityID `json:"opportunity_id"`
	ScoreBPS      uint16        `json:"score_bps"`
	Disposition   DecisionKind  `json:"disposition"`
}

// DecisionReceipt is the signed machine-verifiable and founder-readable result.
type DecisionReceipt struct {
	SchemaVersion           string                         `json:"schema_version"`
	ID                      DecisionID                     `json:"decision_id"`
	OrganizationID          contracts.OrganizationID       `json:"organization_id"`
	OpportunityID           OpportunityID                  `json:"opportunity_id"`
	InitiativeID            *InitiativeID                  `json:"initiative_id"`
	ProcedureID             string                         `json:"procedure_id"`
	ProcedureVersion        uint64                         `json:"procedure_version"`
	Decision                DecisionKind                   `json:"decision"`
	ScoreBPS                uint16                         `json:"score_bps"`
	Alternatives            []Alternative                  `json:"alternatives"`
	Evidence                []companystate.RecordReference `json:"evidence"`
	Thresholds              []string                       `json:"thresholds"`
	Dissent                 []string                       `json:"dissent"`
	AuthorityClauses        []string                       `json:"authority_clauses"`
	CapitalImpactMicrounits uint64                         `json:"capital_impact_microunits"`
	RiskImpactMicrounits    uint64                         `json:"risk_impact_microunits"`
	UnresolvedRisks         []string                       `json:"unresolved_risks"`
	Reason                  string                         `json:"reason"`
	NextReviewAt            time.Time                      `json:"next_review_at"`
	CreatedAt               time.Time                      `json:"created_at"`
	Signature               contracts.Signature            `json:"signature"`
}

// Validate rejects receipts without complete evidence, authority, alternatives, or review.
func (value DecisionReceipt) Validate() error {
	if value.SchemaVersion != DecisionSchemaVersion || value.ID == "" ||
		value.OrganizationID == "" || value.OpportunityID == "" ||
		value.ProcedureID == "" || value.ProcedureVersion == 0 ||
		!value.Decision.Valid() || value.ScoreBPS > 10_000 ||
		len(value.Alternatives) == 0 || len(value.Evidence) == 0 ||
		len(value.Thresholds) == 0 || len(value.AuthorityClauses) == 0 {
		return fmt.Errorf("portfolio: decision receipt is incomplete")
	}
	if value.Decision == DecisionGO && (value.InitiativeID == nil || value.CapitalImpactMicrounits == 0) {
		return fmt.Errorf("portfolio: GO requires an initiative and capital allocation")
	}
	requiresInitiative := value.Decision == DecisionPrioritize || value.Decision == DecisionPause ||
		value.Decision == DecisionResume || value.Decision == DecisionReallocate ||
		value.Decision == DecisionScale || value.Decision == DecisionPivot ||
		value.Decision == DecisionMaintain || value.Decision == DecisionTerminate
	if requiresInitiative && value.InitiativeID == nil {
		return fmt.Errorf("portfolio: initiative portfolio action requires initiative_id")
	}
	if value.Decision != DecisionGO && !requiresInitiative && value.InitiativeID != nil {
		return fmt.Errorf("portfolio: opportunity decision cannot target an existing initiative")
	}
	if err := validateText("decision reason", value.Reason, 4096); err != nil {
		return err
	}
	if !sortedUniqueText(value.Thresholds) || !sortedUniqueText(value.Dissent) ||
		!sortedUniqueText(value.AuthorityClauses) || !sortedUniqueText(value.UnresolvedRisks) ||
		!validUTC(value.NextReviewAt) || !validUTC(value.CreatedAt) ||
		!value.NextReviewAt.After(value.CreatedAt) {
		return fmt.Errorf("portfolio: decision lists or times are invalid")
	}
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return err
		}
	}
	for index := range value.Alternatives {
		if value.Alternatives[index].OpportunityID == "" ||
			value.Alternatives[index].ScoreBPS > 10_000 ||
			!value.Alternatives[index].Disposition.Valid() {
			return fmt.Errorf("portfolio: alternative %d is invalid", index)
		}
	}
	return value.Signature.Validate()
}

// EvaluationContext is the current resource state used by deterministic selection.
type EvaluationContext struct {
	ActiveInitiatives           uint16
	AllocatedCapitalMicrounits  uint64
	AllocatedRiskMicrounits     uint64
	ConsecutiveNoEvidenceCycles uint16
	Contaminated                bool
}

// CadenceKind is a recurring deterministic company-controller review cycle.
type CadenceKind string

const (
	CadenceDiscovery  CadenceKind = "opportunity_discovery"
	CadencePortfolio  CadenceKind = "portfolio_review"
	CadenceCapital    CadenceKind = "capital_review"
	CadenceProduct    CadenceKind = "product_review"
	CadenceCommercial CadenceKind = "commercial_review"
	CadenceOperations CadenceKind = "operational_review"
	CadenceLearning   CadenceKind = "strategic_learning"
)

// Valid reports whether the cadence has a closed controller meaning.
func (value CadenceKind) Valid() bool {
	switch value {
	case CadenceDiscovery, CadencePortfolio, CadenceCapital, CadenceProduct,
		CadenceCommercial, CadenceOperations, CadenceLearning:
		return true
	default:
		return false
	}
}

// Cadence is immutable founder-authorized recurring controller work.
type Cadence struct {
	SchemaVersion   string                   `json:"schema_version"`
	ID              string                   `json:"cadence_id"`
	Version         uint64                   `json:"version"`
	OrganizationID  contracts.OrganizationID `json:"organization_id"`
	Kind            CadenceKind              `json:"kind"`
	IntervalSeconds uint64                   `json:"interval_seconds"`
	FirstDueAt      time.Time                `json:"first_due_at"`
	EffectiveAt     time.Time                `json:"effective_at"`
	ExpiresAt       *time.Time               `json:"expires_at"`
	Signature       contracts.Signature      `json:"signature"`
}

// Validate enforces bounded recurrence and explicit authority lifetime.
func (value Cadence) Validate() error {
	if value.SchemaVersion != "workforce.company-cadence.v1" || value.ID == "" ||
		value.Version == 0 || value.OrganizationID == "" || !value.Kind.Valid() ||
		value.IntervalSeconds < 300 || value.IntervalSeconds > 365*24*60*60 ||
		!validUTC(value.FirstDueAt) || !validUTC(value.EffectiveAt) ||
		value.FirstDueAt.Before(value.EffectiveAt) ||
		value.ExpiresAt != nil && (!validUTC(*value.ExpiresAt) || !value.ExpiresAt.After(value.FirstDueAt)) {
		return fmt.Errorf("portfolio: cadence is invalid")
	}
	return value.Signature.Validate()
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("portfolio: %s must contain 1 to 128 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character) {
			continue
		}
		return fmt.Errorf("portfolio: %s contains an invalid character", name)
	}
	return nil
}

func validateText(name, value string, maximum int) error {
	if strings.TrimSpace(value) == "" || len(value) > maximum {
		return fmt.Errorf("portfolio: %s must contain 1 to %d bytes", name, maximum)
	}
	return nil
}

func sortedUniqueText(values []string) bool {
	if len(values) == 0 {
		return true
	}
	if !slices.IsSorted(values) {
		return false
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > 2048 || index > 0 && value == values[index-1] {
			return false
		}
	}
	return true
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
