package executive

import (
	"encoding/base64"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/mission"
)

const (
	// DelegationPolicySchemaVersion is the founder-signed Executive policy schema.
	DelegationPolicySchemaVersion = "workforce.executive-delegation-policy.v1"
	// CompiledAuthoritySchemaVersion is the deterministic compiled authority schema.
	CompiledAuthoritySchemaVersion = "workforce.executive-compiled-authority.v1"
	// DecisionRequestSchemaVersion is the signed Executive request schema.
	DecisionRequestSchemaVersion = "workforce.executive-decision-request.v1"
	// ReviewSchemaVersion is the independent review schema.
	ReviewSchemaVersion = "workforce.executive-independent-review.v1"
	// DecisionSchemaVersion is the controller-signed Executive decision schema.
	DecisionSchemaVersion = "workforce.executive-decision.v1"
	// FounderRequestSchemaVersion is the typed founder escalation schema.
	FounderRequestSchemaVersion = "workforce.founder-decision-request.v1"
	// RevocationSchemaVersion is the founder-signed policy revocation schema.
	RevocationSchemaVersion = "workforce.executive-policy-revocation.v1"
	// IncidentSchemaVersion is the material decision-incident schema.
	IncidentSchemaVersion = "workforce.executive-decision-incident.v1"
	// ConsumptionSchemaVersion is the one-use authorization consumption schema.
	ConsumptionSchemaVersion = "workforce.executive-decision-consumption.v1"
)

const maxMicrounits = uint64(math.MaxInt64)

// PolicyID identifies one immutable Executive delegation version chain.
type PolicyID string

// RequestID identifies one immutable Executive decision request.
type RequestID string

// ReviewID identifies one immutable independent review.
type ReviewID string

// DecisionID identifies one immutable controller-signed decision.
type DecisionID string

// FounderRequestID identifies one immutable founder escalation.
type FounderRequestID string

// IncidentID identifies one immutable authority or review incident.
type IncidentID string

// ConsumptionID identifies one exact downstream use of an authorization.
type ConsumptionID string

// Action is the closed set of ordinary company decisions an Executive may
// exercise when an exact compiled clause permits it.
type Action string

const (
	ActionRejectOpportunity        Action = "reject_opportunity"
	ActionAuthorizeExperiment      Action = "authorize_experiment"
	ActionPrioritizeInitiative     Action = "prioritize_initiative"
	ActionAllocateDelegatedCapital Action = "allocate_delegated_capital"
	ActionSelectProduct            Action = "select_product"
	ActionAuthorizePricingTest     Action = "authorize_pricing_test"
	ActionSequenceLaunch           Action = "sequence_launch"
	ActionReallocateResources      Action = "reallocate_resources"
	ActionScale                    Action = "scale"
	ActionPivot                    Action = "pivot"
	ActionMaintain                 Action = "maintain"
	ActionPause                    Action = "pause"
	ActionTerminate                Action = "terminate"
	ActionEmergencyPause           Action = "emergency_pause"
)

var allActions = []Action{
	ActionAllocateDelegatedCapital,
	ActionAuthorizeExperiment,
	ActionAuthorizePricingTest,
	ActionEmergencyPause,
	ActionMaintain,
	ActionPause,
	ActionPivot,
	ActionPrioritizeInitiative,
	ActionReallocateResources,
	ActionRejectOpportunity,
	ActionScale,
	ActionSelectProduct,
	ActionSequenceLaunch,
	ActionTerminate,
}

// AllActions returns the canonical closed Executive action set.
func AllActions() []Action { return append([]Action(nil), allActions...) }

// Valid reports whether the action has executable semantics.
func (value Action) Valid() bool { return slices.Contains(allActions, value) }

// family returns the server-owned aggregation family used to detect requests
// that relabel one material decision as several superficially different ones.
func (value Action) family() string {
	switch value {
	case ActionAuthorizeExperiment, ActionAllocateDelegatedCapital,
		ActionAuthorizePricingTest, ActionSequenceLaunch,
		ActionReallocateResources, ActionScale, ActionPivot:
		return "capital_and_exposure"
	case ActionPrioritizeInitiative, ActionSelectProduct, ActionMaintain:
		return "portfolio_and_product"
	case ActionPause, ActionEmergencyPause, ActionTerminate:
		return "safety_and_terminal"
	case ActionRejectOpportunity:
		return "opportunity_disposition"
	default:
		return "invalid"
	}
}

// ReviewKind is one mandatory independent decision-review discipline.
type ReviewKind string

const (
	ReviewEconomic  ReviewKind = "economic"
	ReviewEvidence  ReviewKind = "evidence"
	ReviewFinancial ReviewKind = "financial"
	ReviewLegal     ReviewKind = "legal"
	ReviewSecurity  ReviewKind = "security"
)

var allReviewKinds = []ReviewKind{
	ReviewEconomic, ReviewEvidence, ReviewFinancial, ReviewLegal, ReviewSecurity,
}

// AllReviewKinds returns the canonical review discipline set.
func AllReviewKinds() []ReviewKind {
	return append([]ReviewKind(nil), allReviewKinds...)
}

// Valid reports whether the review discipline is closed and executable.
func (value ReviewKind) Valid() bool { return slices.Contains(allReviewKinds, value) }

// ReviewOutcome is the closed result of one independent review.
type ReviewOutcome string

const (
	ReviewApprove       ReviewOutcome = "approve"
	ReviewReject        ReviewOutcome = "reject"
	ReviewRequiresHuman ReviewOutcome = "requires_human"
)

// Valid reports whether the review outcome has executable semantics.
func (value ReviewOutcome) Valid() bool {
	switch value {
	case ReviewApprove, ReviewReject, ReviewRequiresHuman:
		return true
	default:
		return false
	}
}

// OperationClass binds the decision to the exact downstream proposal class.
// Any founder-reserved class can be requested, but can never be authorized by
// an Executive clause.
type OperationClass string

const (
	OperationBoundedCompanyDecision OperationClass = "bounded_company_decision"
	OperationMissionChange          OperationClass = "mission_change"
	OperationConstitutionChange     OperationClass = "constitution_change"
	OperationCapitalIncrease        OperationClass = "aggregate_capital_increase"
	OperationDebtOrLeverage         OperationClass = "debt_or_leverage"
	OperationMaterialTransfer       OperationClass = "material_transfer"
	OperationRestrictedJurisdiction OperationClass = "restricted_jurisdiction"
	OperationCustodyOrWithdrawal    OperationClass = "custody_or_withdrawal"
	OperationCorporateAction        OperationClass = "irreversible_corporate_action"
	OperationControlRelaxation      OperationClass = "control_relaxation"
)

// Valid reports whether the operation class has a closed authority meaning.
func (value OperationClass) Valid() bool {
	switch value {
	case OperationBoundedCompanyDecision, OperationMissionChange,
		OperationConstitutionChange, OperationCapitalIncrease,
		OperationDebtOrLeverage, OperationMaterialTransfer,
		OperationRestrictedJurisdiction, OperationCustodyOrWithdrawal,
		OperationCorporateAction, OperationControlRelaxation:
		return true
	default:
		return false
	}
}

// ReservedKind maps a founder-reserved operation class to the exact Mission
// reservation that owns it.
func (value OperationClass) ReservedKind() (mission.ReservedDecisionKind, bool) {
	switch value {
	case OperationMissionChange:
		return mission.ReservedMissionChange, true
	case OperationConstitutionChange:
		return mission.ReservedConstitutionChange, true
	case OperationCapitalIncrease:
		return mission.ReservedCapitalIncrease, true
	case OperationDebtOrLeverage:
		return mission.ReservedDebtOrLeverage, true
	case OperationMaterialTransfer:
		return mission.ReservedMaterialTransfer, true
	case OperationRestrictedJurisdiction:
		return mission.ReservedRestrictedRegion, true
	case OperationCustodyOrWithdrawal:
		return mission.ReservedCustodyOrWithdrawal, true
	case OperationCorporateAction:
		return mission.ReservedCorporateAction, true
	case OperationControlRelaxation:
		return mission.ReservedControlRelaxation, true
	default:
		return "", false
	}
}

// OperationBinding content-addresses the exact downstream operation so an
// authorized ordinary decision cannot be replayed for a different effect.
type OperationBinding struct {
	Class OperationClass        `json:"class"`
	Hash  contracts.ContentHash `json:"hash"`
}

// Validate rejects an untyped or unbound operation.
func (value OperationBinding) Validate() error {
	if !value.Class.Valid() {
		return fmt.Errorf("executive: operation class %q is invalid", value.Class)
	}
	if err := value.Hash.Validate(); err != nil {
		return fmt.Errorf("executive: operation hash: %w", err)
	}
	return nil
}

// SeatAuthorityBinding is the founder-signed binding between one durable seat,
// its mandate, and the Ed25519 key allowed to sign decision material. The key
// grants no authority beyond the containing policy and compiled clause.
type SeatAuthorityBinding struct {
	SeatID           contracts.SeatID         `json:"seat_id"`
	SeatVersion      uint64                   `json:"seat_version"`
	SeatHash         contracts.ContentHash    `json:"seat_hash"`
	DepartmentKind   contracts.DepartmentKind `json:"department_kind"`
	Role             contracts.SeatRole       `json:"role"`
	MandateID        contracts.MandateID      `json:"mandate_id"`
	MandateVersion   uint64                   `json:"mandate_version"`
	MandateHash      contracts.ContentHash    `json:"mandate_hash"`
	SigningKeyID     string                   `json:"signing_key_id"`
	SigningPublicKey string                   `json:"signing_public_key"`
	ReviewKinds      []ReviewKind             `json:"review_kinds"`
}

// Validate enforces exact seat, mandate, role, key, and review bindings.
func (value SeatAuthorityBinding) Validate() error {
	if validateToken("seat id", string(value.SeatID)) != nil || value.SeatVersion == 0 ||
		!value.DepartmentKind.Valid() || !value.Role.Valid() ||
		validateToken("mandate id", string(value.MandateID)) != nil ||
		value.MandateVersion == 0 || validateToken("signing key id", value.SigningKeyID) != nil {
		return fmt.Errorf("executive: seat authority identity is invalid")
	}
	if err := value.SeatHash.Validate(); err != nil {
		return fmt.Errorf("executive: seat authority seat hash: %w", err)
	}
	if err := value.MandateHash.Validate(); err != nil {
		return fmt.Errorf("executive: seat authority mandate hash: %w", err)
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(value.SigningPublicKey)
	if err != nil || len(publicKey) != 32 {
		return fmt.Errorf("executive: seat authority signing key is invalid")
	}
	if !sortedUniqueReviewKinds(value.ReviewKinds) {
		return fmt.Errorf("executive: seat authority review kinds must be sorted and unique")
	}
	if value.Role != contracts.SeatAuditor && len(value.ReviewKinds) != 0 {
		return fmt.Errorf("executive: only Auditor seats may hold review disciplines")
	}
	return nil
}

// DecisionClause is one exact bounded Executive authority clause. Per-request
// and rolling thresholds are inclusive; exceeding either threshold escalates
// rather than authorizing a restructured request.
type DecisionClause struct {
	ClauseID                     string                   `json:"clause_id"`
	Action                       Action                   `json:"action"`
	AllowedLifecycleStates       []companylifecycle.State `json:"allowed_lifecycle_states"`
	MaxRequestCapitalMicrounits  uint64                   `json:"max_request_capital_microunits"`
	MaxRollingCapitalMicrounits  uint64                   `json:"max_rolling_capital_microunits"`
	MaxRequestExposureMicrounits uint64                   `json:"max_request_exposure_microunits"`
	MaxRollingExposureMicrounits uint64                   `json:"max_rolling_exposure_microunits"`
	MaxResourceUnits             uint32                   `json:"max_resource_units"`
	MaxPriceChangeBPS            uint16                   `json:"max_price_change_bps"`
	MaxDurationSeconds           uint64                   `json:"max_duration_seconds"`
	AggregationWindowSeconds     uint64                   `json:"aggregation_window_seconds"`
	PermittedJurisdictions       []string                 `json:"permitted_jurisdictions"`
	PermittedCounterparties      []string                 `json:"permitted_counterparties"`
	RequiredReviews              []ReviewKind             `json:"required_reviews"`
	NextReviewWithinSeconds      uint64                   `json:"next_review_within_seconds"`
}

// Validate rejects open-ended, non-canonical, or self-approving clauses.
func (value DecisionClause) Validate() error {
	if validateToken("decision clause", value.ClauseID) != nil || !value.Action.Valid() ||
		len(value.AllowedLifecycleStates) == 0 ||
		value.MaxRequestCapitalMicrounits > maxMicrounits ||
		value.MaxRollingCapitalMicrounits > maxMicrounits ||
		value.MaxRequestExposureMicrounits > maxMicrounits ||
		value.MaxRollingExposureMicrounits > maxMicrounits ||
		value.MaxRequestCapitalMicrounits > value.MaxRollingCapitalMicrounits ||
		value.MaxRequestExposureMicrounits > value.MaxRollingExposureMicrounits ||
		value.MaxPriceChangeBPS > 10_000 || value.MaxDurationSeconds > 365*24*60*60 ||
		value.AggregationWindowSeconds < 60 || value.AggregationWindowSeconds > 365*24*60*60 ||
		value.NextReviewWithinSeconds < 60 || value.NextReviewWithinSeconds > 365*24*60*60 {
		return fmt.Errorf("executive: decision clause limits are invalid")
	}
	previousState := ""
	for _, state := range value.AllowedLifecycleStates {
		if !state.Valid() || string(state) <= previousState {
			return fmt.Errorf("executive: lifecycle states must be sorted, unique, and valid")
		}
		previousState = string(state)
	}
	if !sortedUniqueText(value.PermittedJurisdictions, 128) ||
		!sortedUniqueText(value.PermittedCounterparties, 128) ||
		!sortedUniqueReviewKinds(value.RequiredReviews) {
		return fmt.Errorf("executive: decision clause scopes or reviews are not canonical")
	}
	if value.Action == ActionEmergencyPause {
		if value.MaxRequestCapitalMicrounits != 0 || value.MaxRollingCapitalMicrounits != 0 ||
			value.MaxRequestExposureMicrounits != 0 || value.MaxRollingExposureMicrounits != 0 ||
			value.MaxResourceUnits != 0 || value.MaxPriceChangeBPS != 0 ||
			value.MaxDurationSeconds != 0 || len(value.RequiredReviews) != 0 {
			return fmt.Errorf("executive: emergency pause cannot carry spend, exposure, resources, price change, duration, or review delay")
		}
		return nil
	}
	if !slices.Equal(value.RequiredReviews, allReviewKinds) {
		return fmt.Errorf("executive: material decisions require economic, evidence, financial, legal, and security review")
	}
	return nil
}

// DelegationPolicy is the immutable founder-signed source compiled into
// executable Executive authority. It binds all relevant company roots, exact
// seat keys, exact reviewer scopes, and every ordinary decision clause.
type DelegationPolicy struct {
	SchemaVersion                string                   `json:"schema_version"`
	ID                           PolicyID                 `json:"executive_policy_id"`
	Version                      uint64                   `json:"version"`
	OrganizationID               contracts.OrganizationID `json:"organization_id"`
	MissionVersion               uint64                   `json:"mission_version"`
	ConstitutionVersion          uint64                   `json:"constitution_version"`
	CapitalEnvelopeVersion       uint64                   `json:"capital_envelope_version"`
	IssuerPolicyVersion          uint64                   `json:"issuer_policy_version"`
	DecisionMakers               []SeatAuthorityBinding   `json:"decision_makers"`
	Reviewers                    []SeatAuthorityBinding   `json:"reviewers"`
	Clauses                      []DecisionClause         `json:"clauses"`
	MaxRollingCapitalMicrounits  uint64                   `json:"max_rolling_capital_microunits"`
	MaxRollingExposureMicrounits uint64                   `json:"max_rolling_exposure_microunits"`
	AggregationWindowSeconds     uint64                   `json:"aggregation_window_seconds"`
	EffectiveAt                  time.Time                `json:"effective_at"`
	ExpiresAt                    time.Time                `json:"expires_at"`
	Signature                    contracts.Signature      `json:"signature"`
}

// Validate enforces a complete, canonical, expiring delegation policy.
func (value DelegationPolicy) Validate() error {
	if value.SchemaVersion != DelegationPolicySchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.ID != PolicyID("executive-policy:"+string(value.OrganizationID)) ||
		value.MissionVersion == 0 || value.ConstitutionVersion == 0 ||
		value.CapitalEnvelopeVersion == 0 || value.IssuerPolicyVersion == 0 ||
		len(value.DecisionMakers) != 2 || len(value.Reviewers) < len(allReviewKinds) ||
		len(value.Reviewers) > 32 || len(value.Clauses) != len(allActions) ||
		value.MaxRollingCapitalMicrounits > maxMicrounits ||
		value.MaxRollingExposureMicrounits > maxMicrounits ||
		value.AggregationWindowSeconds < 60 || value.AggregationWindowSeconds > 365*24*60*60 ||
		!validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) {
		return fmt.Errorf("executive: delegation policy identity, roots, bounds, or times are invalid")
	}
	if err := validateDecisionMakers(value.DecisionMakers); err != nil {
		return err
	}
	if err := validateReviewers(value.Reviewers, value.DecisionMakers); err != nil {
		return err
	}
	previousAction := ""
	for index := range value.Clauses {
		if err := value.Clauses[index].Validate(); err != nil {
			return fmt.Errorf("executive: clause %d: %w", index, err)
		}
		if string(value.Clauses[index].Action) <= previousAction ||
			value.Clauses[index].Action != allActions[index] {
			return fmt.Errorf("executive: clauses must contain every action in canonical order")
		}
		previousAction = string(value.Clauses[index].Action)
	}
	return value.Signature.Validate()
}

// CompiledAuthority is the deterministic, non-self-amendable projection of a
// current founder policy and its signed company roots.
type CompiledAuthority struct {
	SchemaVersion                string                   `json:"schema_version"`
	ID                           string                   `json:"compiled_authority_id"`
	OrganizationID               contracts.OrganizationID `json:"organization_id"`
	PolicyID                     PolicyID                 `json:"policy_id"`
	PolicyVersion                uint64                   `json:"policy_version"`
	PolicyHash                   contracts.ContentHash    `json:"policy_hash"`
	MissionVersion               uint64                   `json:"mission_version"`
	MissionHash                  contracts.ContentHash    `json:"mission_hash"`
	ConstitutionVersion          uint64                   `json:"constitution_version"`
	ConstitutionHash             contracts.ContentHash    `json:"constitution_hash"`
	CapitalEnvelopeVersion       uint64                   `json:"capital_envelope_version"`
	CapitalEnvelopeHash          contracts.ContentHash    `json:"capital_envelope_hash"`
	IssuerPolicyVersion          uint64                   `json:"issuer_policy_version"`
	IssuerPolicyHash             contracts.ContentHash    `json:"issuer_policy_hash"`
	DecisionMakers               []SeatAuthorityBinding   `json:"decision_makers"`
	Reviewers                    []SeatAuthorityBinding   `json:"reviewers"`
	Clauses                      []DecisionClause         `json:"clauses"`
	MaxRollingCapitalMicrounits  uint64                   `json:"max_rolling_capital_microunits"`
	MaxRollingExposureMicrounits uint64                   `json:"max_rolling_exposure_microunits"`
	AggregationWindowSeconds     uint64                   `json:"aggregation_window_seconds"`
	EffectiveAt                  time.Time                `json:"effective_at"`
	ExpiresAt                    time.Time                `json:"expires_at"`
	CompiledAt                   time.Time                `json:"compiled_at"`
}

// Validate enforces complete signed-root lineage and canonical compiled clauses.
func (value CompiledAuthority) Validate() error {
	if value.SchemaVersion != CompiledAuthoritySchemaVersion || value.OrganizationID == "" ||
		value.ID != compiledAuthorityID(value.OrganizationID, value.PolicyVersion) ||
		value.PolicyID == "" || value.PolicyVersion == 0 || value.MissionVersion == 0 ||
		value.ConstitutionVersion == 0 || value.CapitalEnvelopeVersion == 0 ||
		value.IssuerPolicyVersion == 0 || len(value.Clauses) != len(allActions) ||
		value.MaxRollingCapitalMicrounits > maxMicrounits ||
		value.MaxRollingExposureMicrounits > maxMicrounits ||
		value.AggregationWindowSeconds < 60 || !validUTC(value.EffectiveAt) ||
		!validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.EffectiveAt) ||
		!validUTC(value.CompiledAt) || value.CompiledAt.Before(value.EffectiveAt) {
		return fmt.Errorf("executive: compiled authority identity, roots, or times are invalid")
	}
	for _, item := range []struct {
		name string
		hash contracts.ContentHash
	}{
		{"policy", value.PolicyHash}, {"mission", value.MissionHash},
		{"constitution", value.ConstitutionHash}, {"capital", value.CapitalEnvelopeHash},
		{"issuer policy", value.IssuerPolicyHash},
	} {
		if err := item.hash.Validate(); err != nil {
			return fmt.Errorf("executive: compiled %s hash: %w", item.name, err)
		}
	}
	if err := validateDecisionMakers(value.DecisionMakers); err != nil {
		return err
	}
	if err := validateReviewers(value.Reviewers, value.DecisionMakers); err != nil {
		return err
	}
	for index := range value.Clauses {
		if err := value.Clauses[index].Validate(); err != nil || value.Clauses[index].Action != allActions[index] {
			return fmt.Errorf("executive: compiled clause %d is invalid", index)
		}
	}
	return nil
}

// DecisionRequest is an Executive-signed request over exact lifecycle,
// evidence, procedure, policy, operation, capital, and next-review state.
type DecisionRequest struct {
	SchemaVersion            string                        `json:"schema_version"`
	ID                       RequestID                     `json:"request_id"`
	OrganizationID           contracts.OrganizationID      `json:"organization_id"`
	InitiativeID             companylifecycle.InitiativeID `json:"initiative_id"`
	OpportunityID            string                        `json:"opportunity_id"`
	Action                   Action                        `json:"action"`
	LifecycleState           companylifecycle.State        `json:"lifecycle_state"`
	TargetID                 string                        `json:"target_id"`
	SeriesID                 string                        `json:"series_id"`
	RequesterSeatID          contracts.SeatID              `json:"requester_seat_id"`
	RequesterMandateID       contracts.MandateID           `json:"requester_mandate_id"`
	RequesterMandateVersion  uint64                        `json:"requester_mandate_version"`
	PolicyID                 PolicyID                      `json:"policy_id"`
	PolicyVersion            uint64                        `json:"policy_version"`
	PolicyHash               contracts.ContentHash         `json:"policy_hash"`
	ClauseID                 string                        `json:"clause_id"`
	DecisionProcedureID      string                        `json:"decision_procedure_id"`
	DecisionProcedureVersion uint64                        `json:"decision_procedure_version"`
	DecisionProcedureHash    contracts.ContentHash         `json:"decision_procedure_hash"`
	Operation                OperationBinding              `json:"operation"`
	Evidence                 []contracts.RecordRef         `json:"evidence"`
	EvidenceFreshUntil       time.Time                     `json:"evidence_fresh_until"`
	ConflictedSeatIDs        []contracts.SeatID            `json:"conflicted_seat_ids"`
	CapitalMicrounits        uint64                        `json:"capital_microunits"`
	ExposureMicrounits       uint64                        `json:"exposure_microunits"`
	ResourceUnits            uint32                        `json:"resource_units"`
	PriceChangeBPS           uint16                        `json:"price_change_bps"`
	DurationSeconds          uint64                        `json:"duration_seconds"`
	Jurisdiction             string                        `json:"jurisdiction"`
	Counterparty             string                        `json:"counterparty"`
	Reason                   string                        `json:"reason"`
	CreatedAt                time.Time                     `json:"created_at"`
	ExpiresAt                time.Time                     `json:"expires_at"`
	NextReviewAt             time.Time                     `json:"next_review_at"`
	Signature                contracts.Signature           `json:"signature"`
}

// Validate rejects incomplete, stale-by-construction, unbound, or
// non-canonical decision requests.
func (value DecisionRequest) Validate() error {
	if value.SchemaVersion != DecisionRequestSchemaVersion ||
		validateToken("request id", string(value.ID)) != nil || value.OrganizationID == "" ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		validateToken("opportunity id", value.OpportunityID) != nil || !value.Action.Valid() ||
		!value.LifecycleState.Valid() || validateToken("target id", value.TargetID) != nil ||
		validateToken("series id", value.SeriesID) != nil ||
		validateToken("requester seat id", string(value.RequesterSeatID)) != nil ||
		validateToken("requester mandate id", string(value.RequesterMandateID)) != nil ||
		value.RequesterMandateVersion == 0 || value.PolicyID == "" || value.PolicyVersion == 0 ||
		validateToken("clause id", value.ClauseID) != nil ||
		validateToken("decision procedure id", value.DecisionProcedureID) != nil ||
		value.DecisionProcedureVersion == 0 || len(value.Evidence) == 0 || len(value.Evidence) > 256 ||
		value.CapitalMicrounits > maxMicrounits || value.ExposureMicrounits > maxMicrounits ||
		value.PriceChangeBPS > 10_000 || value.DurationSeconds > 365*24*60*60 ||
		!validOptionalToken(value.Jurisdiction) || !validOptionalToken(value.Counterparty) ||
		validateText("request reason", value.Reason, 4096) != nil ||
		!validUTC(value.CreatedAt) || !validUTC(value.ExpiresAt) ||
		!validUTC(value.EvidenceFreshUntil) || !validUTC(value.NextReviewAt) ||
		!value.ExpiresAt.After(value.CreatedAt) || !value.EvidenceFreshUntil.After(value.CreatedAt) ||
		!value.NextReviewAt.After(value.CreatedAt) || value.NextReviewAt.After(value.ExpiresAt) {
		return fmt.Errorf("executive: decision request identity, bindings, limits, or times are invalid")
	}
	if err := value.PolicyHash.Validate(); err != nil {
		return fmt.Errorf("executive: request policy hash: %w", err)
	}
	if err := value.DecisionProcedureHash.Validate(); err != nil {
		return fmt.Errorf("executive: request procedure hash: %w", err)
	}
	if err := value.Operation.Validate(); err != nil {
		return err
	}
	previousRecord := ""
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("executive: request evidence %d: %w", index, err)
		}
		if string(value.Evidence[index].ID) <= previousRecord {
			return fmt.Errorf("executive: request evidence must be sorted and unique")
		}
		previousRecord = string(value.Evidence[index].ID)
	}
	if len(value.ConflictedSeatIDs) == 0 || len(value.ConflictedSeatIDs) > 64 {
		return fmt.Errorf("executive: decision request requires an explicit conflict set")
	}
	previousSeat := ""
	requesterPresent := false
	for _, seatID := range value.ConflictedSeatIDs {
		if validateToken("conflicted seat id", string(seatID)) != nil || string(seatID) <= previousSeat {
			return fmt.Errorf("executive: conflict set must be sorted and unique")
		}
		requesterPresent = requesterPresent || seatID == value.RequesterSeatID
		previousSeat = string(seatID)
	}
	if !requesterPresent {
		return fmt.Errorf("executive: conflict set must include the decision maker")
	}
	if value.Action == ActionEmergencyPause &&
		(value.CapitalMicrounits != 0 || value.ExposureMicrounits != 0 ||
			value.ResourceUnits != 0 || value.PriceChangeBPS != 0 || value.DurationSeconds != 0) {
		return fmt.Errorf("executive: emergency pause cannot carry material authority")
	}
	return value.Signature.Validate()
}

// Review is one fresh, signed, independently scoped assessment of an exact
// decision request.
type Review struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             ReviewID                 `json:"review_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	RequestID      RequestID                `json:"request_id"`
	Kind           ReviewKind               `json:"kind"`
	Outcome        ReviewOutcome            `json:"outcome"`
	ReviewerSeatID contracts.SeatID         `json:"reviewer_seat_id"`
	Evidence       []contracts.RecordRef    `json:"evidence"`
	Findings       []string                 `json:"findings"`
	Dissent        []string                 `json:"dissent"`
	ReviewedAt     time.Time                `json:"reviewed_at"`
	ExpiresAt      time.Time                `json:"expires_at"`
	Signature      contracts.Signature      `json:"signature"`
}

// Validate enforces exact request scope, evidence, and bounded review lifetime.
func (value Review) Validate() error {
	if value.SchemaVersion != ReviewSchemaVersion || validateToken("review id", string(value.ID)) != nil ||
		value.OrganizationID == "" || validateToken("review request id", string(value.RequestID)) != nil ||
		!value.Kind.Valid() || !value.Outcome.Valid() ||
		validateToken("reviewer seat id", string(value.ReviewerSeatID)) != nil ||
		len(value.Evidence) == 0 || len(value.Evidence) > 256 ||
		len(value.Findings) == 0 ||
		!sortedUniqueText(value.Findings, 2048) || !sortedUniqueText(value.Dissent, 2048) ||
		!validUTC(value.ReviewedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.ReviewedAt) {
		return fmt.Errorf("executive: independent review is invalid")
	}
	previous := ""
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("executive: review evidence %d: %w", index, err)
		}
		if string(value.Evidence[index].ID) <= previous {
			return fmt.Errorf("executive: review evidence must be sorted and unique")
		}
		previous = string(value.Evidence[index].ID)
	}
	return value.Signature.Validate()
}

// DecisionOutcome is the closed machine result of Executive evaluation.
type DecisionOutcome string

const (
	DecisionAuthorized      DecisionOutcome = "authorized"
	DecisionDenied          DecisionOutcome = "denied"
	DecisionFounderRequired DecisionOutcome = "founder_required"
	DecisionEmergencyPaused DecisionOutcome = "emergency_paused"
)

// Valid reports whether the decision outcome is closed.
func (value DecisionOutcome) Valid() bool {
	switch value {
	case DecisionAuthorized, DecisionDenied, DecisionFounderRequired, DecisionEmergencyPaused:
		return true
	default:
		return false
	}
}

// ReviewBinding content-addresses one exact independent review used by a decision.
type ReviewBinding struct {
	ID      ReviewID              `json:"review_id"`
	Kind    ReviewKind            `json:"kind"`
	Outcome ReviewOutcome         `json:"outcome"`
	SeatID  contracts.SeatID      `json:"seat_id"`
	Hash    contracts.ContentHash `json:"hash"`
}

// Validate rejects incomplete or unaddressed review lineage.
func (value ReviewBinding) Validate() error {
	if validateToken("review binding id", string(value.ID)) != nil || !value.Kind.Valid() ||
		!value.Outcome.Valid() || validateToken("review binding seat", string(value.SeatID)) != nil {
		return fmt.Errorf("executive: review binding is invalid")
	}
	return value.Hash.Validate()
}

// Decision is the immutable controller-signed result of evaluating one request.
type Decision struct {
	SchemaVersion             string                        `json:"schema_version"`
	ID                        DecisionID                    `json:"decision_id"`
	OrganizationID            contracts.OrganizationID      `json:"organization_id"`
	RequestID                 RequestID                     `json:"request_id"`
	RequestHash               contracts.ContentHash         `json:"request_hash"`
	InitiativeID              companylifecycle.InitiativeID `json:"initiative_id"`
	Action                    Action                        `json:"action"`
	LifecycleState            companylifecycle.State        `json:"lifecycle_state"`
	Operation                 OperationBinding              `json:"operation"`
	Outcome                   DecisionOutcome               `json:"outcome"`
	ReasonCode                string                        `json:"reason_code"`
	Reason                    string                        `json:"reason"`
	PolicyID                  PolicyID                      `json:"policy_id"`
	PolicyVersion             uint64                        `json:"policy_version"`
	PolicyHash                contracts.ContentHash         `json:"policy_hash"`
	ClauseID                  string                        `json:"clause_id"`
	Reviews                   []ReviewBinding               `json:"reviews"`
	CapitalMicrounits         uint64                        `json:"capital_microunits"`
	ExposureMicrounits        uint64                        `json:"exposure_microunits"`
	RollingCapitalMicrounits  uint64                        `json:"rolling_capital_microunits"`
	RollingExposureMicrounits uint64                        `json:"rolling_exposure_microunits"`
	FounderRequestID          *FounderRequestID             `json:"founder_request_id"`
	IncidentID                *IncidentID                   `json:"incident_id"`
	CreatedAt                 time.Time                     `json:"created_at"`
	AuthorizedUntil           time.Time                     `json:"authorized_until"`
	NextReviewAt              time.Time                     `json:"next_review_at"`
	Signature                 contracts.Signature           `json:"signature"`
}

// Validate enforces exact authority, review, escalation, and expiry lineage.
func (value Decision) Validate() error {
	if value.SchemaVersion != DecisionSchemaVersion || validateToken("decision id", string(value.ID)) != nil ||
		value.OrganizationID == "" || validateToken("decision request id", string(value.RequestID)) != nil ||
		validateToken("decision initiative id", string(value.InitiativeID)) != nil || !value.Action.Valid() ||
		!value.LifecycleState.Valid() || !value.Outcome.Valid() ||
		validateToken("decision reason code", value.ReasonCode) != nil ||
		validateText("decision reason", value.Reason, 4096) != nil || value.PolicyID == "" ||
		value.PolicyVersion == 0 || validateToken("decision clause id", value.ClauseID) != nil ||
		value.CapitalMicrounits > maxMicrounits || value.ExposureMicrounits > maxMicrounits ||
		value.RollingCapitalMicrounits > maxMicrounits || value.RollingExposureMicrounits > maxMicrounits ||
		!validUTC(value.CreatedAt) || !validUTC(value.AuthorizedUntil) ||
		!validUTC(value.NextReviewAt) || !value.AuthorizedUntil.After(value.CreatedAt) ||
		!value.NextReviewAt.After(value.CreatedAt) || value.NextReviewAt.After(value.AuthorizedUntil) {
		return fmt.Errorf("executive: decision is invalid")
	}
	if err := value.RequestHash.Validate(); err != nil {
		return err
	}
	if err := value.PolicyHash.Validate(); err != nil {
		return err
	}
	if err := value.Operation.Validate(); err != nil {
		return err
	}
	previous := ""
	for index := range value.Reviews {
		if err := value.Reviews[index].Validate(); err != nil {
			return fmt.Errorf("executive: decision review %d: %w", index, err)
		}
		key := string(value.Reviews[index].Kind)
		if key <= previous {
			return fmt.Errorf("executive: decision reviews must be sorted and unique by kind")
		}
		previous = key
	}
	if value.Outcome == DecisionFounderRequired && value.FounderRequestID == nil {
		return fmt.Errorf("executive: founder-required decision lacks founder request")
	}
	if value.Outcome != DecisionFounderRequired && value.FounderRequestID != nil {
		return fmt.Errorf("executive: non-escalated decision cannot bind a founder request")
	}
	if value.Outcome == DecisionAuthorized && len(value.Reviews) != len(allReviewKinds) {
		return fmt.Errorf("executive: authorized material decision requires all independent reviews")
	}
	if value.Outcome == DecisionEmergencyPaused && len(value.Reviews) != 0 {
		return fmt.Errorf("executive: emergency pause cannot wait on review")
	}
	return value.Signature.Validate()
}

// FounderDecisionRequest is the immutable typed fail-closed result when an
// operation is reserved or falls outside exact delegated thresholds.
type FounderDecisionRequest struct {
	SchemaVersion      string                        `json:"schema_version"`
	ID                 FounderRequestID              `json:"founder_request_id"`
	OrganizationID     contracts.OrganizationID      `json:"organization_id"`
	RequestID          RequestID                     `json:"request_id"`
	RequestHash        contracts.ContentHash         `json:"request_hash"`
	InitiativeID       companylifecycle.InitiativeID `json:"initiative_id"`
	Action             Action                        `json:"action"`
	Operation          OperationBinding              `json:"operation"`
	ReservedKind       mission.ReservedDecisionKind  `json:"reserved_kind"`
	PolicyID           PolicyID                      `json:"policy_id"`
	PolicyVersion      uint64                        `json:"policy_version"`
	ClauseID           string                        `json:"clause_id"`
	Reasons            []string                      `json:"reasons"`
	Evidence           []contracts.RecordRef         `json:"evidence"`
	CapitalMicrounits  uint64                        `json:"capital_microunits"`
	ExposureMicrounits uint64                        `json:"exposure_microunits"`
	CreatedAt          time.Time                     `json:"created_at"`
	ExpiresAt          time.Time                     `json:"expires_at"`
	Signature          contracts.Signature           `json:"signature"`
}

// Validate enforces exact reserved authority, request lineage, and expiry.
func (value FounderDecisionRequest) Validate() error {
	if value.SchemaVersion != FounderRequestSchemaVersion ||
		validateToken("founder request id", string(value.ID)) != nil || value.OrganizationID == "" ||
		validateToken("founder original request id", string(value.RequestID)) != nil ||
		validateToken("founder request initiative", string(value.InitiativeID)) != nil ||
		!value.Action.Valid() || !value.ReservedKind.Valid() || value.PolicyID == "" ||
		value.PolicyVersion == 0 || validateToken("founder request clause", value.ClauseID) != nil ||
		!sortedUniqueText(value.Reasons, 2048) || len(value.Reasons) == 0 ||
		len(value.Evidence) == 0 || len(value.Evidence) > 256 ||
		value.CapitalMicrounits > maxMicrounits || value.ExposureMicrounits > maxMicrounits ||
		!validUTC(value.CreatedAt) || !validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.CreatedAt) {
		return fmt.Errorf("executive: founder decision request is invalid")
	}
	if err := value.RequestHash.Validate(); err != nil {
		return err
	}
	if err := value.Operation.Validate(); err != nil {
		return err
	}
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return fmt.Errorf("executive: founder request evidence %d: %w", index, err)
		}
	}
	return value.Signature.Validate()
}

// IncidentKind classifies fail-closed authority and independent-review failures.
type IncidentKind string

const (
	IncidentMaterialDisagreement IncidentKind = "material_disagreement"
	IncidentSelfApproval         IncidentKind = "self_approval"
	IncidentAuthorityEvasion     IncidentKind = "authority_evasion"
	IncidentStaleEvidence        IncidentKind = "stale_evidence"
)

// Valid reports whether the incident kind is closed.
func (value IncidentKind) Valid() bool {
	switch value {
	case IncidentMaterialDisagreement, IncidentSelfApproval,
		IncidentAuthorityEvasion, IncidentStaleEvidence:
		return true
	default:
		return false
	}
}

// DecisionIncident is an immutable founder-visible material decision incident.
type DecisionIncident struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             IncidentID               `json:"incident_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	RequestID      RequestID                `json:"request_id"`
	Kind           IncidentKind             `json:"kind"`
	ReviewIDs      []ReviewID               `json:"review_ids"`
	Reasons        []string                 `json:"reasons"`
	CreatedAt      time.Time                `json:"created_at"`
	Signature      contracts.Signature      `json:"signature"`
}

// Validate enforces typed, canonical, signed incident evidence.
func (value DecisionIncident) Validate() error {
	if value.SchemaVersion != IncidentSchemaVersion || validateToken("incident id", string(value.ID)) != nil ||
		value.OrganizationID == "" || validateToken("incident request id", string(value.RequestID)) != nil ||
		!value.Kind.Valid() || !sortedUniqueReviewIDs(value.ReviewIDs) ||
		!sortedUniqueText(value.Reasons, 2048) || len(value.Reasons) == 0 || !validUTC(value.CreatedAt) {
		return fmt.Errorf("executive: decision incident is invalid")
	}
	return value.Signature.Validate()
}

// PolicyRevocation is an immutable founder-signed revocation of one exact
// delegation policy version.
type PolicyRevocation struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"revocation_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	PolicyID       PolicyID                 `json:"policy_id"`
	PolicyVersion  uint64                   `json:"policy_version"`
	Reason         string                   `json:"reason"`
	RevokedAt      time.Time                `json:"revoked_at"`
	Signature      contracts.Signature      `json:"signature"`
}

// Validate enforces exact policy identity, reason, and UTC revocation time.
func (value PolicyRevocation) Validate() error {
	if value.SchemaVersion != RevocationSchemaVersion || validateToken("revocation id", value.ID) != nil ||
		value.OrganizationID == "" || value.PolicyID == "" || value.PolicyVersion == 0 ||
		validateText("revocation reason", value.Reason, 2048) != nil || !validUTC(value.RevokedAt) {
		return fmt.Errorf("executive: policy revocation is invalid")
	}
	return value.Signature.Validate()
}

// DecisionConsumption binds one authorized decision to one exact downstream
// operation and effect. It prevents a bounded decision from being replayed or
// substituted after authorization.
type DecisionConsumption struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             ConsumptionID            `json:"consumption_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	DecisionID     DecisionID               `json:"decision_id"`
	Operation      OperationBinding         `json:"operation"`
	EffectID       string                   `json:"effect_id"`
	ConsumedAt     time.Time                `json:"consumed_at"`
	Signature      contracts.Signature      `json:"signature"`
}

// Validate enforces exact one-use decision and effect lineage.
func (value DecisionConsumption) Validate() error {
	if value.SchemaVersion != ConsumptionSchemaVersion ||
		validateToken("consumption id", string(value.ID)) != nil || value.OrganizationID == "" ||
		validateToken("consumption decision id", string(value.DecisionID)) != nil ||
		validateToken("consumption effect id", value.EffectID) != nil || !validUTC(value.ConsumedAt) {
		return fmt.Errorf("executive: decision consumption is invalid")
	}
	if err := value.Operation.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

func validateDecisionMakers(values []SeatAuthorityBinding) error {
	if len(values) != 2 {
		return fmt.Errorf("executive: policy requires exactly one Executive Lead and Executor decision maker")
	}
	previous := ""
	roles := make(map[contracts.SeatRole]struct{}, 2)
	for index := range values {
		if err := values[index].Validate(); err != nil {
			return fmt.Errorf("executive: decision maker %d: %w", index, err)
		}
		if string(values[index].SeatID) <= previous ||
			values[index].DepartmentKind != contracts.DepartmentExecutive ||
			(values[index].Role != contracts.SeatLead && values[index].Role != contracts.SeatExecutor) ||
			len(values[index].ReviewKinds) != 0 {
			return fmt.Errorf("executive: decision makers must be sorted Executive Lead and Executor seats")
		}
		if _, exists := roles[values[index].Role]; exists {
			return fmt.Errorf("executive: duplicate Executive decision-maker role")
		}
		roles[values[index].Role] = struct{}{}
		previous = string(values[index].SeatID)
	}
	return nil
}

func validateReviewers(values, decisionMakers []SeatAuthorityBinding) error {
	if len(values) < len(allReviewKinds) || len(values) > 32 {
		return fmt.Errorf("executive: policy requires bounded independent reviewers")
	}
	decisionSeats := make(map[contracts.SeatID]struct{}, len(decisionMakers))
	for _, binding := range decisionMakers {
		decisionSeats[binding.SeatID] = struct{}{}
	}
	coverage := make(map[ReviewKind]bool, len(allReviewKinds))
	previous := ""
	for index := range values {
		if err := values[index].Validate(); err != nil {
			return fmt.Errorf("executive: reviewer %d: %w", index, err)
		}
		if string(values[index].SeatID) <= previous || values[index].Role != contracts.SeatAuditor ||
			len(values[index].ReviewKinds) == 0 {
			return fmt.Errorf("executive: reviewers must be sorted, unique Auditor seats")
		}
		if _, conflicted := decisionSeats[values[index].SeatID]; conflicted {
			return fmt.Errorf("executive: a decision maker cannot be a reviewer")
		}
		for _, kind := range values[index].ReviewKinds {
			coverage[kind] = true
		}
		previous = string(values[index].SeatID)
	}
	for _, kind := range allReviewKinds {
		if !coverage[kind] {
			return fmt.Errorf("executive: reviewer coverage lacks %s", kind)
		}
	}
	return nil
}

func sortedUniqueReviewKinds(values []ReviewKind) bool {
	previous := ""
	for _, value := range values {
		if !value.Valid() || string(value) <= previous {
			return false
		}
		previous = string(value)
	}
	return true
}

func sortedUniqueReviewIDs(values []ReviewID) bool {
	previous := ""
	for _, value := range values {
		if validateToken("review id", string(value)) != nil || string(value) <= previous {
			return false
		}
		previous = string(value)
	}
	return true
}

func sortedUniqueText(values []string, maximum int) bool {
	previous := ""
	for _, value := range values {
		if validateText("canonical text", value, maximum) != nil || value <= previous {
			return false
		}
		previous = value
	}
	return true
}

func validateText(name, value string, maximum int) error {
	if strings.TrimSpace(value) == "" || len(value) > maximum {
		return fmt.Errorf("executive: %s must contain 1 to %d bytes", name, maximum)
	}
	return nil
}

func validateToken(name, value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 128 {
		return fmt.Errorf("executive: %s must contain 1 to 128 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character) {
			continue
		}
		return fmt.Errorf("executive: %s contains an invalid character", name)
	}
	return nil
}

func validOptionalToken(value string) bool {
	return value == "" || validateToken("optional token", value) == nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func compiledAuthorityID(organizationID contracts.OrganizationID, version uint64) string {
	return fmt.Sprintf("compiled-executive:%s:%d", organizationID, version)
}
