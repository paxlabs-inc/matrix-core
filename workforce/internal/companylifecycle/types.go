package companylifecycle

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"centra/workforce/internal/contracts"
)

const (
	// InitiativeSchemaVersion is the canonical lifecycle initiative schema.
	InitiativeSchemaVersion = "workforce.company-initiative.v1"
	// TransitionSchemaVersion is the canonical lifecycle transition schema.
	TransitionSchemaVersion = "workforce.company-lifecycle-transition.v1"
	// CheckpointSchemaVersion is the canonical lifecycle checkpoint schema.
	CheckpointSchemaVersion = "workforce.company-lifecycle-checkpoint.v1"
	// ReceiptSchemaVersion is the canonical lifecycle decision-receipt schema.
	ReceiptSchemaVersion = "workforce.company-lifecycle-receipt.v1"
	// GateGrantSchemaVersion is the canonical lifecycle gate-verification schema.
	GateGrantSchemaVersion = "workforce.company-lifecycle-gate-grant.v1"
	// EffectSchemaVersion is the canonical lifecycle external-effect schema.
	EffectSchemaVersion = "workforce.company-lifecycle-effect.v1"
)

// InitiativeID identifies one durable company initiative.
type InitiativeID string

// OpportunityID identifies the immutable opportunity that originated an initiative.
type OpportunityID string

// PortfolioID identifies the portfolio scope that owns an initiative.
type PortfolioID string

// CompanyStateID identifies a versioned, content-addressed company-state head.
type CompanyStateID string

// TransitionID identifies one immutable lifecycle transition request.
type TransitionID string

// DecisionReceiptID identifies one immutable signed lifecycle decision receipt.
type DecisionReceiptID string

// EffectID identifies one crash-recoverable external effect.
type EffectID string

// State is one closed company lifecycle state.
type State string

const (
	// StateDiscover collects candidate demand and opportunity evidence.
	StateDiscover State = "DISCOVER"
	// StateScreen applies market, legal, constitutional, and strategic screens.
	StateScreen State = "SCREEN"
	// StateValidate runs preregistered demand and economic experiments.
	StateValidate State = "VALIDATE"
	// StateDecide produces a closed GO or NO_GO decision.
	StateDecide State = "DECIDE"
	// StateFund allocates exact initiative capital.
	StateFund State = "FUND"
	// StateDesign binds the problem, requirements, journey, and design evidence.
	StateDesign State = "DESIGN"
	// StateBuild creates the implementation and operational artifacts.
	StateBuild State = "BUILD"
	// StateVerify independently verifies product and launch obligations.
	StateVerify State = "VERIFY"
	// StateLaunch performs the authorized launch.
	StateLaunch State = "LAUNCH"
	// StateAcquire operates real distribution and acquisition channels.
	StateAcquire State = "ACQUIRE"
	// StateMonetize operates sales, pricing, contracts, and transactions.
	StateMonetize State = "MONETIZE"
	// StateOperate runs the launched product and customer operation.
	StateOperate State = "OPERATE"
	// StateMeasure evaluates verified business outcomes and thresholds.
	StateMeasure State = "MEASURE"
	// StateScale is the closed decision to increase a validated operation.
	StateScale State = "SCALE"
	// StatePivot is the closed decision to replace material hypotheses.
	StatePivot State = "PIVOT"
	// StateMaintain is the closed decision to hold the current operating posture.
	StateMaintain State = "MAINTAIN"
	// StateTerminate is the irreversible terminal initiative state.
	StateTerminate State = "TERMINATE"
	// StatePaused is a fail-closed resumable state with an exact resume target.
	StatePaused State = "PAUSED"
)

var allStates = []State{
	StateDiscover, StateScreen, StateValidate, StateDecide, StateFund,
	StateDesign, StateBuild, StateVerify, StateLaunch, StateAcquire,
	StateMonetize, StateOperate, StateMeasure, StateScale, StatePivot,
	StateMaintain, StateTerminate, StatePaused,
}

// AllStates returns the canonical closed lifecycle state set.
func AllStates() []State {
	return append([]State(nil), allStates...)
}

// Valid reports whether the state is executable by this lifecycle version.
func (value State) Valid() bool {
	return slices.Contains(allStates, value)
}

// Decision is the closed semantic reason for a lifecycle state transition.
type Decision string

const (
	// DecisionInitialize creates the first DISCOVER checkpoint.
	DecisionInitialize Decision = "INITIALIZE"
	// DecisionAdvance follows the deterministic forward lifecycle edge.
	DecisionAdvance Decision = "ADVANCE"
	// DecisionGo authorizes exact capital allocation after validation.
	DecisionGo Decision = "GO"
	// DecisionNoGo terminates an initiative rejected at DECIDE.
	DecisionNoGo Decision = "NO_GO"
	// DecisionScale selects the SCALE portfolio alternative.
	DecisionScale Decision = "SCALE"
	// DecisionPivot selects the PIVOT portfolio alternative.
	DecisionPivot Decision = "PIVOT"
	// DecisionMaintain selects the MAINTAIN portfolio alternative.
	DecisionMaintain Decision = "MAINTAIN"
	// DecisionTerminate selects a safe terminal alternative.
	DecisionTerminate Decision = "TERMINATE"
	// DecisionPause records a fail-closed pause and exact resume state.
	DecisionPause Decision = "PAUSE"
	// DecisionResume resumes only the state captured by the pause checkpoint.
	DecisionResume Decision = "RESUME"
)

// Valid reports whether the decision has closed lifecycle semantics.
func (value Decision) Valid() bool {
	switch value {
	case DecisionInitialize, DecisionAdvance, DecisionGo, DecisionNoGo,
		DecisionScale, DecisionPivot, DecisionMaintain, DecisionTerminate,
		DecisionPause, DecisionResume:
		return true
	default:
		return false
	}
}

// EvidenceKind is one closed lifecycle gate input class.
type EvidenceKind string

const (
	EvidenceOpportunity           EvidenceKind = "opportunity"
	EvidenceDemandSignal          EvidenceKind = "demand_signal"
	EvidenceTargetCustomer        EvidenceKind = "target_customer"
	EvidenceMarketAnalysis        EvidenceKind = "market_analysis"
	EvidenceCompetitorAnalysis    EvidenceKind = "competitor_analysis"
	EvidenceLegalScreening        EvidenceKind = "legal_screening"
	EvidenceConstitutionScreening EvidenceKind = "constitutional_screening"
	EvidenceHypothesis            EvidenceKind = "hypothesis"
	EvidenceExperiment            EvidenceKind = "experiment"
	EvidenceEconomicModel         EvidenceKind = "economic_model"
	EvidenceRiskReview            EvidenceKind = "risk_review"
	EvidenceGoNoGoDecision        EvidenceKind = "go_no_go_decision"
	EvidenceCapitalAllocation     EvidenceKind = "capital_allocation"
	EvidenceCustomerProblem       EvidenceKind = "customer_problem"
	EvidenceRequirements          EvidenceKind = "requirements"
	EvidenceUserJourney           EvidenceKind = "user_journey"
	EvidenceImplementationPlan    EvidenceKind = "implementation_plan"
	EvidenceSourceState           EvidenceKind = "source_state"
	EvidenceDeploymentState       EvidenceKind = "deployment_state"
	EvidenceQuality               EvidenceKind = "quality"
	EvidenceSecurity              EvidenceKind = "security"
	EvidenceOperationsReadiness   EvidenceKind = "operations_readiness"
	EvidenceClaims                EvidenceKind = "claims"
	EvidenceLegal                 EvidenceKind = "legal"
	EvidencePricing               EvidenceKind = "pricing"
	EvidenceLaunchReadiness       EvidenceKind = "launch_readiness"
	EvidenceDistribution          EvidenceKind = "distribution"
	EvidenceSales                 EvidenceKind = "sales"
	EvidenceCustomer              EvidenceKind = "customer"
	EvidenceTransaction           EvidenceKind = "transaction"
	EvidenceRevenue               EvidenceKind = "revenue"
	EvidenceCost                  EvidenceKind = "cost"
	EvidenceProductUsage          EvidenceKind = "product_usage"
	EvidenceSupport               EvidenceKind = "support"
	EvidenceRetention             EvidenceKind = "retention"
	EvidenceRiskObservation       EvidenceKind = "risk_observation"
	EvidenceVerifiedOutcome       EvidenceKind = "verified_outcome"
	EvidenceThresholdEvaluation   EvidenceKind = "threshold_evaluation"
	EvidencePortfolioDecision     EvidenceKind = "portfolio_decision"
	EvidenceIndependentReview     EvidenceKind = "independent_review"
	EvidencePauseCondition        EvidenceKind = "pause_condition"
	EvidenceResumeCondition       EvidenceKind = "resume_condition"
	EvidenceTerminationDecision   EvidenceKind = "termination_decision"
	EvidenceNextCycleDecision     EvidenceKind = "next_cycle_decision"
)

var allEvidenceKinds = []EvidenceKind{
	EvidenceOpportunity, EvidenceDemandSignal, EvidenceTargetCustomer,
	EvidenceMarketAnalysis, EvidenceCompetitorAnalysis, EvidenceLegalScreening,
	EvidenceConstitutionScreening, EvidenceHypothesis, EvidenceExperiment,
	EvidenceEconomicModel, EvidenceRiskReview, EvidenceGoNoGoDecision,
	EvidenceCapitalAllocation, EvidenceCustomerProblem, EvidenceRequirements,
	EvidenceUserJourney, EvidenceImplementationPlan, EvidenceSourceState,
	EvidenceDeploymentState, EvidenceQuality, EvidenceSecurity,
	EvidenceOperationsReadiness, EvidenceClaims, EvidenceLegal, EvidencePricing,
	EvidenceLaunchReadiness, EvidenceDistribution, EvidenceSales, EvidenceCustomer,
	EvidenceTransaction, EvidenceRevenue, EvidenceCost, EvidenceProductUsage,
	EvidenceSupport, EvidenceRetention, EvidenceRiskObservation,
	EvidenceVerifiedOutcome, EvidenceThresholdEvaluation, EvidencePortfolioDecision,
	EvidenceIndependentReview, EvidencePauseCondition, EvidenceResumeCondition,
	EvidenceTerminationDecision, EvidenceNextCycleDecision,
}

// AllEvidenceKinds returns the closed lifecycle evidence kind set.
func AllEvidenceKinds() []EvidenceKind {
	return append([]EvidenceKind(nil), allEvidenceKinds...)
}

// Valid reports whether the evidence kind is recognized by lifecycle v1.
func (value EvidenceKind) Valid() bool {
	return slices.Contains(allEvidenceKinds, value)
}

// Initiative is the immutable typed identity and origin binding for one lifecycle.
type Initiative struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             InitiativeID             `json:"initiative_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	OpportunityID  OpportunityID            `json:"opportunity_id"`
	PortfolioID    PortfolioID              `json:"portfolio_id"`
	CreatedBySeat  contracts.SeatID         `json:"created_by_seat_id"`
	OriginHash     contracts.ContentHash    `json:"origin_hash"`
	CreatedAt      time.Time                `json:"created_at"`
}

// Validate rejects incomplete or non-canonical initiative identities.
func (value Initiative) Validate() error {
	if value.SchemaVersion != InitiativeSchemaVersion ||
		validateToken("initiative id", string(value.ID)) != nil ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("opportunity id", string(value.OpportunityID)) != nil ||
		validateToken("portfolio id", string(value.PortfolioID)) != nil ||
		validateToken("creator seat id", string(value.CreatedBySeat)) != nil {
		return fmt.Errorf("company lifecycle: initiative identity is invalid")
	}
	if err := value.OriginHash.Validate(); err != nil {
		return fmt.Errorf("company lifecycle: initiative origin hash: %w", err)
	}
	if !validUTC(value.CreatedAt) {
		return fmt.Errorf("company lifecycle: initiative created_at must be UTC")
	}
	return nil
}

// CompanyStateBinding binds a transition to one immutable company-state head
// without importing the company-state implementation package.
type CompanyStateBinding struct {
	SchemaVersion  string                   `json:"schema_version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	ID             CompanyStateID           `json:"company_state_id"`
	Version        uint64                   `json:"version"`
	Hash           contracts.ContentHash    `json:"hash"`
	ObservedAt     time.Time                `json:"observed_at"`
}

// Validate enforces a content-addressed, versioned company-state binding.
func (value CompanyStateBinding) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 || value.Version == 0 ||
		validateToken("company state organization id", string(value.OrganizationID)) != nil ||
		validateToken("company state id", string(value.ID)) != nil {
		return fmt.Errorf("company lifecycle: company-state binding is invalid")
	}
	if err := value.Hash.Validate(); err != nil {
		return fmt.Errorf("company lifecycle: company-state hash: %w", err)
	}
	if !validUTC(value.ObservedAt) {
		return fmt.Errorf("company lifecycle: company-state observed_at must be UTC")
	}
	return nil
}

// CorrectionBinding binds the exact correction closure inspected for a gate.
type CorrectionBinding struct {
	SchemaVersion               string                   `json:"schema_version"`
	OrganizationID              contracts.OrganizationID `json:"organization_id"`
	SnapshotID                  string                   `json:"snapshot_id"`
	Version                     uint64                   `json:"version"`
	Hash                        contracts.ContentHash    `json:"hash"`
	CheckedAt                   time.Time                `json:"checked_at"`
	UnresolvedMaterialCount     uint32                   `json:"unresolved_material_count"`
	UnresolvedContaminatedCount uint32                   `json:"unresolved_contaminated_count"`
}

// Validate enforces an exact, content-addressed correction snapshot.
func (value CorrectionBinding) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 || value.Version == 0 ||
		validateToken("correction organization id", string(value.OrganizationID)) != nil ||
		validateToken("correction snapshot id", value.SnapshotID) != nil {
		return fmt.Errorf("company lifecycle: correction binding is invalid")
	}
	if err := value.Hash.Validate(); err != nil {
		return fmt.Errorf("company lifecycle: correction hash: %w", err)
	}
	if !validUTC(value.CheckedAt) {
		return fmt.Errorf("company lifecycle: correction checked_at must be UTC")
	}
	return nil
}

// AuthorityBinding identifies every current authority version and the exact
// clause claimed by a transition. The GateVerifier must prove it is current.
type AuthorityBinding struct {
	SchemaVersion          string                   `json:"schema_version"`
	OrganizationID         contracts.OrganizationID `json:"organization_id"`
	MissionVersion         uint64                   `json:"mission_version"`
	MissionHash            contracts.ContentHash    `json:"mission_hash"`
	ConstitutionVersion    uint64                   `json:"constitution_version"`
	ConstitutionHash       contracts.ContentHash    `json:"constitution_hash"`
	CapitalEnvelopeVersion uint64                   `json:"capital_envelope_version"`
	CapitalEnvelopeHash    contracts.ContentHash    `json:"capital_envelope_hash"`
	IssuerPolicyVersion    uint64                   `json:"issuer_policy_version"`
	IssuerPolicyHash       contracts.ContentHash    `json:"issuer_policy_hash"`
	MandateID              contracts.MandateID      `json:"mandate_id"`
	MandateVersion         uint64                   `json:"mandate_version"`
	MandateHash            contracts.ContentHash    `json:"mandate_hash"`
	RequestedBySeatID      contracts.SeatID         `json:"requested_by_seat_id"`
	ClauseID               string                   `json:"authority_clause_id"`
	ExpiresAt              time.Time                `json:"expires_at"`
}

// Validate enforces complete version, hash, seat, mandate, clause, and expiry bindings.
func (value AuthorityBinding) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 ||
		validateToken("authority organization id", string(value.OrganizationID)) != nil ||
		value.MissionVersion == 0 || value.ConstitutionVersion == 0 ||
		value.CapitalEnvelopeVersion == 0 || value.IssuerPolicyVersion == 0 ||
		value.MandateVersion == 0 ||
		validateToken("mandate id", string(value.MandateID)) != nil ||
		validateToken("requesting seat id", string(value.RequestedBySeatID)) != nil ||
		validateToken("authority clause id", value.ClauseID) != nil {
		return fmt.Errorf("company lifecycle: authority binding is invalid")
	}
	for _, item := range []struct {
		name string
		hash contracts.ContentHash
	}{
		{"mission", value.MissionHash}, {"constitution", value.ConstitutionHash},
		{"capital envelope", value.CapitalEnvelopeHash},
		{"issuer policy", value.IssuerPolicyHash}, {"mandate", value.MandateHash},
	} {
		if err := item.hash.Validate(); err != nil {
			return fmt.Errorf("company lifecycle: %s hash: %w", item.name, err)
		}
	}
	if !validUTC(value.ExpiresAt) {
		return fmt.Errorf("company lifecycle: authority expiry must be UTC")
	}
	return nil
}

// EvidenceBinding binds one fresh, active, content-addressed evidence record.
type EvidenceBinding struct {
	SchemaVersion          string                 `json:"schema_version"`
	ID                     contracts.EvidenceID   `json:"evidence_id"`
	Kind                   EvidenceKind           `json:"kind"`
	SourceRecordID         string                 `json:"source_record_id"`
	SourceRecordVersion    uint64                 `json:"source_record_version"`
	SourceRecordHash       contracts.ContentHash  `json:"source_record_hash"`
	EvidenceHash           contracts.ContentHash  `json:"evidence_hash"`
	Validity               contracts.Validity     `json:"validity"`
	ObservedAt             time.Time              `json:"observed_at"`
	EffectiveAt            time.Time              `json:"effective_at"`
	FreshUntil             time.Time              `json:"fresh_until"`
	Contaminated           bool                   `json:"contaminated"`
	IndependentVerdictID   *contracts.VerdictID   `json:"independent_verdict_id"`
	IndependentVerdictHash *contracts.ContentHash `json:"independent_verdict_hash"`
}

// Validate enforces typed provenance, freshness bounds, and paired verdict fields.
func (value EvidenceBinding) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 ||
		validateToken("evidence id", string(value.ID)) != nil ||
		!value.Kind.Valid() || value.SourceRecordVersion == 0 ||
		validateToken("evidence source record id", value.SourceRecordID) != nil ||
		!value.Validity.Valid() {
		return fmt.Errorf("company lifecycle: evidence binding is invalid")
	}
	if err := value.SourceRecordHash.Validate(); err != nil {
		return fmt.Errorf("company lifecycle: evidence source hash: %w", err)
	}
	if err := value.EvidenceHash.Validate(); err != nil {
		return fmt.Errorf("company lifecycle: evidence hash: %w", err)
	}
	if !validUTC(value.ObservedAt) || !validUTC(value.EffectiveAt) ||
		!validUTC(value.FreshUntil) || value.EffectiveAt.Before(value.ObservedAt) ||
		!value.FreshUntil.After(value.EffectiveAt) {
		return fmt.Errorf("company lifecycle: evidence times are invalid")
	}
	if (value.IndependentVerdictID == nil) != (value.IndependentVerdictHash == nil) {
		return fmt.Errorf("company lifecycle: independent verdict binding is incomplete")
	}
	if value.IndependentVerdictID != nil {
		if validateToken("independent verdict id", string(*value.IndependentVerdictID)) != nil {
			return fmt.Errorf("company lifecycle: independent verdict id is invalid")
		}
		if err := value.IndependentVerdictHash.Validate(); err != nil {
			return fmt.Errorf("company lifecycle: independent verdict hash: %w", err)
		}
	}
	return nil
}

// CapitalImpact records the exact bounded monetary impact of one transition.
// All amounts are integer microunits in the bound capital-envelope currency.
type CapitalImpact struct {
	SchemaVersion               string                `json:"schema_version"`
	Currency                    string                `json:"currency"`
	CapitalEnvelopeVersion      uint64                `json:"capital_envelope_version"`
	CapitalEnvelopeHash         contracts.ContentHash `json:"capital_envelope_hash"`
	TransitionBudgetMicrounits  uint64                `json:"transition_budget_microunits"`
	AllocateMicrounits          uint64                `json:"allocate_microunits"`
	ReleaseMicrounits           uint64                `json:"release_microunits"`
	SpendMicrounits             uint64                `json:"spend_microunits"`
	AllocatedSpendMicrounits    uint64                `json:"allocated_spend_microunits"`
	ExposureIncreaseMicrounits  uint64                `json:"exposure_increase_microunits"`
	ExposureReleaseMicrounits   uint64                `json:"exposure_release_microunits"`
	RecognizedRevenueMicrounits uint64                `json:"recognized_revenue_microunits"`
}

// Validate enforces explicit currency, envelope, and transition budget bounds.
func (value CapitalImpact) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 ||
		validateToken("capital currency", value.Currency) != nil ||
		value.CapitalEnvelopeVersion == 0 || value.TransitionBudgetMicrounits == 0 ||
		value.AllocatedSpendMicrounits > value.SpendMicrounits ||
		value.SpendMicrounits > value.TransitionBudgetMicrounits {
		return fmt.Errorf("company lifecycle: capital impact is invalid")
	}
	if err := value.CapitalEnvelopeHash.Validate(); err != nil {
		return fmt.Errorf("company lifecycle: capital envelope hash: %w", err)
	}
	return nil
}

// CapitalLimits are the current, transactionally verified authority ceilings.
type CapitalLimits struct {
	SchemaVersion                    string                `json:"schema_version"`
	Currency                         string                `json:"currency"`
	CapitalEnvelopeVersion           uint64                `json:"capital_envelope_version"`
	CapitalEnvelopeHash              contracts.ContentHash `json:"capital_envelope_hash"`
	MaxResultingAllocationMicrounits uint64                `json:"max_resulting_allocation_microunits"`
	MaxResultingSpendMicrounits      uint64                `json:"max_resulting_spend_microunits"`
	MaxResultingExposureMicrounits   uint64                `json:"max_resulting_exposure_microunits"`
	MaxTransitionBudgetMicrounits    uint64                `json:"max_transition_budget_microunits"`
}

// Validate rejects missing or internally inconsistent current capital limits.
func (value CapitalLimits) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 ||
		validateToken("capital limit currency", value.Currency) != nil ||
		value.CapitalEnvelopeVersion == 0 ||
		value.MaxResultingAllocationMicrounits == 0 ||
		value.MaxResultingSpendMicrounits == 0 ||
		value.MaxResultingExposureMicrounits == 0 ||
		value.MaxTransitionBudgetMicrounits == 0 {
		return fmt.Errorf("company lifecycle: capital limits are invalid")
	}
	return value.CapitalEnvelopeHash.Validate()
}

// CapitalSnapshot is the exact cumulative initiative capital checkpoint.
type CapitalSnapshot struct {
	SchemaVersion                  string                `json:"schema_version"`
	Currency                       string                `json:"currency"`
	CapitalEnvelopeVersion         uint64                `json:"capital_envelope_version"`
	CapitalEnvelopeHash            contracts.ContentHash `json:"capital_envelope_hash"`
	AllocatedMicrounits            uint64                `json:"allocated_microunits"`
	ConsumedAllocationMicrounits   uint64                `json:"consumed_allocation_microunits"`
	SpentMicrounits                uint64                `json:"spent_microunits"`
	ExposureMicrounits             uint64                `json:"exposure_microunits"`
	RecognizedRevenueMicrounits    uint64                `json:"recognized_revenue_microunits"`
	LastTransitionBudgetMicrounits uint64                `json:"last_transition_budget_microunits"`
}

// Validate enforces exact envelope and allocation accounting invariants.
func (value CapitalSnapshot) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 ||
		validateToken("capital snapshot currency", value.Currency) != nil ||
		value.CapitalEnvelopeVersion == 0 ||
		value.ConsumedAllocationMicrounits > value.AllocatedMicrounits {
		return fmt.Errorf("company lifecycle: capital snapshot is invalid")
	}
	return value.CapitalEnvelopeHash.Validate()
}

// CreateRequest initializes an initiative at DISCOVER and records its first receipt.
type CreateRequest struct {
	SchemaVersion  string              `json:"schema_version"`
	TransitionID   TransitionID        `json:"transition_id"`
	ReceiptID      DecisionReceiptID   `json:"receipt_id"`
	Initiative     Initiative          `json:"initiative"`
	Authority      AuthorityBinding    `json:"authority"`
	CompanyState   CompanyStateBinding `json:"company_state"`
	Correction     CorrectionBinding   `json:"correction"`
	Evidence       []EvidenceBinding   `json:"evidence"`
	CapitalImpact  CapitalImpact       `json:"capital_impact"`
	IdempotencyKey string              `json:"idempotency_key"`
}

// Validate enforces a complete canonical lifecycle initialization request.
func (value CreateRequest) Validate() error {
	if value.SchemaVersion != TransitionSchemaVersion ||
		validateToken("transition id", string(value.TransitionID)) != nil ||
		validateToken("receipt id", string(value.ReceiptID)) != nil ||
		validateIdempotencyKey(value.IdempotencyKey) != nil {
		return fmt.Errorf("company lifecycle: create request identity is invalid")
	}
	if err := value.Initiative.Validate(); err != nil {
		return err
	}
	return validateGateInputs(value.Authority, value.CompanyState, value.Correction, value.Evidence, value.CapitalImpact)
}

// TransitionRequest is one versioned, optimistic lifecycle state transition.
type TransitionRequest struct {
	SchemaVersion   string                   `json:"schema_version"`
	TransitionID    TransitionID             `json:"transition_id"`
	ReceiptID       DecisionReceiptID        `json:"receipt_id"`
	OrganizationID  contracts.OrganizationID `json:"organization_id"`
	InitiativeID    InitiativeID             `json:"initiative_id"`
	ExpectedVersion uint64                   `json:"expected_version"`
	FromState       State                    `json:"from_state"`
	ToState         State                    `json:"to_state"`
	Decision        Decision                 `json:"decision"`
	Authority       AuthorityBinding         `json:"authority"`
	CompanyState    CompanyStateBinding      `json:"company_state"`
	Correction      CorrectionBinding        `json:"correction"`
	Evidence        []EvidenceBinding        `json:"evidence"`
	CapitalImpact   CapitalImpact            `json:"capital_impact"`
	EffectIDs       []EffectID               `json:"effect_ids"`
	IdempotencyKey  string                   `json:"idempotency_key"`
}

// Validate enforces the transition envelope independently of current durable state.
func (value TransitionRequest) Validate() error {
	if value.SchemaVersion != TransitionSchemaVersion ||
		validateToken("transition id", string(value.TransitionID)) != nil ||
		validateToken("receipt id", string(value.ReceiptID)) != nil ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		value.ExpectedVersion == 0 || !value.FromState.Valid() ||
		!value.ToState.Valid() || !value.Decision.Valid() ||
		validateIdempotencyKey(value.IdempotencyKey) != nil {
		return fmt.Errorf("company lifecycle: transition request is invalid")
	}
	if err := validateGateInputs(value.Authority, value.CompanyState, value.Correction, value.Evidence, value.CapitalImpact); err != nil {
		return err
	}
	return validateEffectIDs(value.EffectIDs)
}

// GateVerificationRequest is the exact snapshot passed to the transactional verifier.
type GateVerificationRequest struct {
	TransitionID    TransitionID
	OrganizationID  contracts.OrganizationID
	InitiativeID    InitiativeID
	FromState       State
	ToState         State
	Decision        Decision
	ExpectedVersion uint64
	RequestHash     contracts.ContentHash
	Authority       AuthorityBinding
	CompanyState    CompanyStateBinding
	Correction      CorrectionBinding
	Evidence        []EvidenceBinding
	CapitalImpact   CapitalImpact
	VerifiedAt      time.Time
}

// GateVerifier verifies policy authority, source evidence, corrections, company
// state, and available capital using the same serializable transaction that
// commits the lifecycle transition. It must fail closed on revocation or drift.
type GateVerifier interface {
	VerifyLifecycleGate(context.Context, pgx.Tx, GateVerificationRequest) (GateVerificationGrant, error)
}

// GateVerificationGrant is the immutable result of a current transactional gate check.
type GateVerificationGrant struct {
	SchemaVersion           string                   `json:"schema_version"`
	TransitionID            TransitionID             `json:"transition_id"`
	OrganizationID          contracts.OrganizationID `json:"organization_id"`
	InitiativeID            InitiativeID             `json:"initiative_id"`
	AuthorityBindingHash    contracts.ContentHash    `json:"authority_binding_hash"`
	CompanyStateHash        contracts.ContentHash    `json:"company_state_hash"`
	CorrectionBindingHash   contracts.ContentHash    `json:"correction_binding_hash"`
	EvidenceSetHash         contracts.ContentHash    `json:"evidence_set_hash"`
	CapitalImpactHash       contracts.ContentHash    `json:"capital_impact_hash"`
	PolicyDecisionID        string                   `json:"policy_decision_id"`
	PolicyDecisionHash      contracts.ContentHash    `json:"policy_decision_hash"`
	AuthorityClauseID       string                   `json:"authority_clause_id"`
	VerifierID              string                   `json:"verifier_id"`
	VerificationReceiptHash contracts.ContentHash    `json:"verification_receipt_hash"`
	Limits                  CapitalLimits            `json:"capital_limits"`
	VerifiedAt              time.Time                `json:"verified_at"`
	ExpiresAt               time.Time                `json:"expires_at"`
}

// Validate enforces a complete, bounded, content-addressed verification grant.
func (value GateVerificationGrant) Validate() error {
	if value.SchemaVersion != GateGrantSchemaVersion ||
		validateToken("transition id", string(value.TransitionID)) != nil ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		validateToken("policy decision id", value.PolicyDecisionID) != nil ||
		validateToken("authority clause id", value.AuthorityClauseID) != nil ||
		validateToken("gate verifier id", value.VerifierID) != nil {
		return fmt.Errorf("company lifecycle: gate verification grant is invalid")
	}
	for _, hash := range []contracts.ContentHash{
		value.AuthorityBindingHash, value.CompanyStateHash,
		value.CorrectionBindingHash, value.EvidenceSetHash,
		value.CapitalImpactHash, value.PolicyDecisionHash,
		value.VerificationReceiptHash,
	} {
		if err := hash.Validate(); err != nil {
			return fmt.Errorf("company lifecycle: gate verification hash: %w", err)
		}
	}
	if err := value.Limits.Validate(); err != nil {
		return err
	}
	if !validUTC(value.VerifiedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.VerifiedAt) {
		return fmt.Errorf("company lifecycle: gate verification times are invalid")
	}
	return nil
}

// Checkpoint is the durable current lifecycle head for one initiative.
type Checkpoint struct {
	SchemaVersion    string                   `json:"schema_version"`
	OrganizationID   contracts.OrganizationID `json:"organization_id"`
	InitiativeID     InitiativeID             `json:"initiative_id"`
	State            State                    `json:"state"`
	ResumeState      State                    `json:"resume_state"`
	Version          uint64                   `json:"version"`
	CompanyState     CompanyStateBinding      `json:"company_state"`
	Authority        AuthorityBinding         `json:"authority"`
	Capital          CapitalSnapshot          `json:"capital"`
	LastTransitionID TransitionID             `json:"last_transition_id"`
	LastReceiptID    DecisionReceiptID        `json:"last_receipt_id"`
	CreatedAt        time.Time                `json:"created_at"`
	UpdatedAt        time.Time                `json:"updated_at"`
	TerminatedAt     *time.Time               `json:"terminated_at"`
}

// Validate enforces the complete lifecycle, authority, capital, and terminal invariants.
func (value Checkpoint) Validate() error {
	if value.SchemaVersion != CheckpointSchemaVersion ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		!value.State.Valid() || value.Version == 0 ||
		validateToken("last transition id", string(value.LastTransitionID)) != nil ||
		validateToken("last receipt id", string(value.LastReceiptID)) != nil {
		return fmt.Errorf("company lifecycle: checkpoint identity is invalid")
	}
	if value.State == StatePaused {
		if !value.ResumeState.Valid() || value.ResumeState == StatePaused || value.ResumeState == StateTerminate {
			return fmt.Errorf("company lifecycle: paused checkpoint requires an active resume state")
		}
	} else if value.ResumeState != "" {
		return fmt.Errorf("company lifecycle: only a paused checkpoint may carry resume state")
	}
	if (value.State == StateTerminate) != (value.TerminatedAt != nil) {
		return fmt.Errorf("company lifecycle: terminal timestamp does not match state")
	}
	if value.TerminatedAt != nil && !validUTC(*value.TerminatedAt) {
		return fmt.Errorf("company lifecycle: terminated_at must be UTC")
	}
	if err := value.CompanyState.Validate(); err != nil {
		return err
	}
	if err := value.Authority.Validate(); err != nil {
		return err
	}
	if err := value.Capital.Validate(); err != nil {
		return err
	}
	if !validUTC(value.CreatedAt) || !validUTC(value.UpdatedAt) || value.UpdatedAt.Before(value.CreatedAt) {
		return fmt.Errorf("company lifecycle: checkpoint times are invalid")
	}
	return nil
}

// DecisionReceipt is the immutable signed decision and lineage record for one transition.
type DecisionReceipt struct {
	SchemaVersion    string                   `json:"schema_version"`
	ID               DecisionReceiptID        `json:"receipt_id"`
	TransitionID     TransitionID             `json:"transition_id"`
	OrganizationID   contracts.OrganizationID `json:"organization_id"`
	InitiativeID     InitiativeID             `json:"initiative_id"`
	FromState        State                    `json:"from_state"`
	ToState          State                    `json:"to_state"`
	Decision         Decision                 `json:"decision"`
	ExpectedVersion  uint64                   `json:"expected_version"`
	ResultingVersion uint64                   `json:"resulting_version"`
	RequestHash      contracts.ContentHash    `json:"request_hash"`
	Verification     GateVerificationGrant    `json:"verification"`
	CapitalBefore    CapitalSnapshot          `json:"capital_before"`
	CapitalImpact    CapitalImpact            `json:"capital_impact"`
	CapitalAfter     CapitalSnapshot          `json:"capital_after"`
	EffectIDs        []EffectID               `json:"effect_ids"`
	CheckpointHash   contracts.ContentHash    `json:"checkpoint_hash"`
	ContentHash      contracts.ContentHash    `json:"content_hash"`
	CreatedAt        time.Time                `json:"created_at"`
	Signature        contracts.Signature      `json:"signature"`
}

// Validate enforces immutable decision, verification, capital, checkpoint, and signature bindings.
func (value DecisionReceipt) Validate() error {
	if value.SchemaVersion != ReceiptSchemaVersion ||
		validateToken("receipt id", string(value.ID)) != nil ||
		validateToken("transition id", string(value.TransitionID)) != nil ||
		validateToken("organization id", string(value.OrganizationID)) != nil ||
		validateToken("initiative id", string(value.InitiativeID)) != nil ||
		(value.FromState != "" && !value.FromState.Valid()) ||
		!value.ToState.Valid() || !value.Decision.Valid() ||
		value.ResultingVersion != value.ExpectedVersion+1 {
		return fmt.Errorf("company lifecycle: decision receipt identity is invalid")
	}
	if err := value.RequestHash.Validate(); err != nil {
		return err
	}
	if err := value.Verification.Validate(); err != nil {
		return err
	}
	if err := value.CapitalBefore.Validate(); err != nil {
		return err
	}
	if err := value.CapitalImpact.Validate(); err != nil {
		return err
	}
	if err := value.CapitalAfter.Validate(); err != nil {
		return err
	}
	if err := validateEffectIDs(value.EffectIDs); err != nil {
		return err
	}
	if err := value.CheckpointHash.Validate(); err != nil {
		return err
	}
	if err := value.ContentHash.Validate(); err != nil {
		return err
	}
	if !validUTC(value.CreatedAt) {
		return fmt.Errorf("company lifecycle: receipt created_at must be UTC")
	}
	return value.Signature.Validate()
}

// TransitionResult is the exact committed checkpoint and immutable receipt.
type TransitionResult struct {
	Checkpoint   Checkpoint
	Receipt      DecisionReceipt
	Deduplicated bool
}

func validateGateInputs(authority AuthorityBinding, companyState CompanyStateBinding, correction CorrectionBinding, evidence []EvidenceBinding, impact CapitalImpact) error {
	if err := authority.Validate(); err != nil {
		return err
	}
	if err := companyState.Validate(); err != nil {
		return err
	}
	if err := correction.Validate(); err != nil {
		return err
	}
	if len(evidence) == 0 || len(evidence) > 128 {
		return fmt.Errorf("company lifecycle: evidence set must contain 1 to 128 bindings")
	}
	previous := ""
	for index := range evidence {
		if err := evidence[index].Validate(); err != nil {
			return fmt.Errorf("company lifecycle: evidence %d: %w", index, err)
		}
		key := string(evidence[index].Kind) + "\x00" + string(evidence[index].ID)
		if key <= previous {
			return fmt.Errorf("company lifecycle: evidence must be sorted and unique")
		}
		previous = key
	}
	return impact.Validate()
}

func validateEffectIDs(values []EffectID) error {
	if len(values) > 64 {
		return fmt.Errorf("company lifecycle: transition exceeds 64 effects")
	}
	previous := ""
	for _, value := range values {
		if validateToken("effect id", string(value)) != nil || string(value) <= previous {
			return fmt.Errorf("company lifecycle: effect ids must be sorted and unique")
		}
		previous = string(value)
	}
	return nil
}

func validateIdempotencyKey(value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("company lifecycle: idempotency key is invalid")
	}
	return nil
}

func validateToken(name, value string) error {
	if strings.TrimSpace(value) == "" || len(value) > 128 || strings.ContainsAny(value, "\r\n\t") {
		return fmt.Errorf("company lifecycle: %s is invalid", name)
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
