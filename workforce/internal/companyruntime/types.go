package companyruntime

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/companylifecycle"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/initiative"
	"matrix/workforce/internal/portfolio"
	"matrix/workforce/internal/productcapability"
	"matrix/workforce/internal/projectbrain"
	"matrix/workforce/internal/squad"
)

const (
	StartConfigurationSchemaVersion = "workforce.company-runtime-config.v1"
	GateAuthorizationSchemaVersion  = "workforce.lifecycle-gate-authorization.v1"
)

type CadenceSchedule struct {
	Kind            portfolio.CadenceKind `json:"kind"`
	IntervalSeconds uint64                `json:"interval_seconds"`
	FirstDueAt      time.Time             `json:"first_due_at"`
}

type StartDraft struct {
	Version                  uint64                  `json:"version"`
	EffectiveAt              time.Time               `json:"effective_at"`
	ExpiresAt                time.Time               `json:"expires_at"`
	Weights                  portfolio.FactorWeights `json:"weights"`
	GOThresholdBPS           uint16                  `json:"go_threshold_bps"`
	ValidateThresholdBPS     uint16                  `json:"validate_threshold_bps"`
	NoGOThresholdBPS         uint16                  `json:"no_go_threshold_bps"`
	MinimumLegalSafetyBPS    uint16                  `json:"minimum_legal_safety_bps"`
	MinimumSecuritySafetyBPS uint16                  `json:"minimum_security_safety_bps"`
	MaximumActiveInitiatives uint16                  `json:"maximum_active_initiatives"`
	MaximumNoEvidenceCycles  uint16                  `json:"maximum_no_evidence_cycles"`
	MaximumCapitalMicrounits uint64                  `json:"maximum_capital_microunits"`
	MaximumRiskMicrounits    uint64                  `json:"maximum_risk_microunits"`
	AuthorityClauses         []string                `json:"authority_clauses"`
	Cadences                 []CadenceSchedule       `json:"cadences"`
}

type StartConfiguration struct {
	SchemaVersion          string                      `json:"schema_version"`
	ID                     string                      `json:"config_id"`
	Version                uint64                      `json:"version"`
	OrganizationID         contracts.OrganizationID    `json:"organization_id"`
	MissionVersion         uint64                      `json:"mission_version"`
	MissionHash            contracts.ContentHash       `json:"mission_hash"`
	ConstitutionVersion    uint64                      `json:"constitution_version"`
	ConstitutionHash       contracts.ContentHash       `json:"constitution_hash"`
	CapitalEnvelopeVersion uint64                      `json:"capital_envelope_version"`
	CapitalEnvelopeHash    contracts.ContentHash       `json:"capital_envelope_hash"`
	IssuerPolicyVersion    uint64                      `json:"issuer_policy_version"`
	IssuerPolicyHash       contracts.ContentHash       `json:"issuer_policy_hash"`
	Procedure              portfolio.DecisionProcedure `json:"decision_procedure"`
	Cadences               []portfolio.Cadence         `json:"cadences"`
	EffectiveAt            time.Time                   `json:"effective_at"`
	ExpiresAt              time.Time                   `json:"expires_at"`
	Signature              contracts.Signature         `json:"signature"`
}

func (value StartConfiguration) Validate() error {
	if value.SchemaVersion != StartConfigurationSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.ID != "company-runtime:"+string(value.OrganizationID) ||
		value.MissionVersion == 0 || value.ConstitutionVersion == 0 ||
		value.CapitalEnvelopeVersion == 0 || value.IssuerPolicyVersion == 0 ||
		!validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) || len(value.Cadences) != 7 {
		return fmt.Errorf("company runtime: start configuration identity, roots, time, or cadence set is invalid")
	}
	for _, hash := range []contracts.ContentHash{
		value.MissionHash, value.ConstitutionHash,
		value.CapitalEnvelopeHash, value.IssuerPolicyHash,
	} {
		if err := hash.Validate(); err != nil {
			return fmt.Errorf("company runtime: start authority hash: %w", err)
		}
	}
	if err := value.Procedure.Validate(); err != nil ||
		value.Procedure.OrganizationID != value.OrganizationID ||
		value.Procedure.Version != value.Version ||
		value.Procedure.EffectiveAt != value.EffectiveAt ||
		value.Procedure.ExpiresAt == nil || *value.Procedure.ExpiresAt != value.ExpiresAt {
		return fmt.Errorf("company runtime: decision procedure is not bound to the start configuration")
	}
	previous := ""
	for index := range value.Cadences {
		cadence := value.Cadences[index]
		if err := cadence.Validate(); err != nil || cadence.OrganizationID != value.OrganizationID ||
			cadence.Version != value.Version || cadence.EffectiveAt != value.EffectiveAt ||
			cadence.ExpiresAt == nil || *cadence.ExpiresAt != value.ExpiresAt ||
			string(cadence.Kind) <= previous {
			return fmt.Errorf("company runtime: cadence %d is invalid or non-canonical", index)
		}
		previous = string(cadence.Kind)
	}
	return value.Signature.Validate()
}

type StartResult struct {
	Configuration StartConfiguration `json:"configuration"`
	Deduplicated  bool               `json:"deduplicated"`
	ActivatedAt   time.Time          `json:"activated_at"`
}

type GateAuthorization struct {
	SchemaVersion      string                         `json:"schema_version"`
	TransitionID       companylifecycle.TransitionID  `json:"transition_id"`
	OrganizationID     contracts.OrganizationID       `json:"organization_id"`
	InitiativeID       companylifecycle.InitiativeID  `json:"initiative_id"`
	RequestHash        contracts.ContentHash          `json:"request_hash"`
	PolicyDecisionID   string                         `json:"policy_decision_id"`
	PolicyDecisionHash contracts.ContentHash          `json:"policy_decision_hash"`
	AuthorityClauseID  string                         `json:"authority_clause_id"`
	Limits             companylifecycle.CapitalLimits `json:"limits"`
	AuthorizedAt       time.Time                      `json:"authorized_at"`
	ExpiresAt          time.Time                      `json:"expires_at"`
	Signature          contracts.Signature            `json:"signature"`
}

func (value GateAuthorization) Validate() error {
	if value.SchemaVersion != GateAuthorizationSchemaVersion ||
		!validToken(string(value.TransitionID)) || value.OrganizationID == "" ||
		!validToken(string(value.InitiativeID)) || !validToken(value.PolicyDecisionID) ||
		!validToken(value.AuthorityClauseID) || !validUTC(value.AuthorizedAt) ||
		!validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.AuthorizedAt) {
		return fmt.Errorf("company runtime: lifecycle gate authorization is invalid")
	}
	if err := value.RequestHash.Validate(); err != nil {
		return err
	}
	if err := value.PolicyDecisionHash.Validate(); err != nil {
		return err
	}
	if err := value.Limits.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type InitiativeDraft struct {
	Version           uint64                    `json:"version"`
	AllocationID      string                    `json:"capital_allocation_id"`
	Currency          string                    `json:"currency"`
	CapabilityPlan    initiative.CapabilityPlan `json:"capability_plan"`
	Objective         string                    `json:"objective"`
	ExecutionCriteria []string                  `json:"execution_criteria"`
	BusinessCriteria  []string                  `json:"business_criteria"`
	BusinessGates     []initiative.BusinessGate `json:"business_gates"`
	Deadline          time.Time                 `json:"deadline"`
	Blueprint         initiative.Blueprint      `json:"blueprint"`
}

type FundingRequest struct {
	FundingID              string                               `json:"funding_id"`
	DecisionID             portfolio.DecisionID                 `json:"decision_id"`
	OpportunityID          portfolio.OpportunityID              `json:"opportunity_id"`
	PortfolioID            companylifecycle.PortfolioID         `json:"portfolio_id"`
	RequestedBySeatID      contracts.SeatID                     `json:"requested_by_seat_id"`
	Assessment             portfolio.Assessment                 `json:"assessment"`
	Alternatives           []portfolio.Alternative              `json:"alternatives"`
	CompanyState           companylifecycle.CompanyStateBinding `json:"company_state"`
	Correction             companylifecycle.CorrectionBinding   `json:"correction"`
	Evidence               []companylifecycle.EvidenceBinding   `json:"evidence"`
	CapitalImpact          companylifecycle.CapitalImpact       `json:"capital_impact"`
	Initiative             InitiativeDraft                      `json:"initiative"`
	DecisionIdempotencyKey string                               `json:"decision_idempotency_key"`
	ProductExecution       *ProductExecutionPlan                `json:"product_execution"`
}

type ProductStagePlan struct {
	Stage      string `json:"stage"`
	PlanNodeID string `json:"plan_node_id"`
	NeedID     string `json:"need_id"`
}

type ProductExecutionPlan struct {
	ExecutionID          string                      `json:"execution_id"`
	ProjectID            contracts.ProjectID         `json:"project_id"`
	WorkspaceID          contracts.WorkspaceID       `json:"workspace_id"`
	SquadRequirement     squad.Requirement           `json:"squad_requirement"`
	Stages               []ProductStagePlan          `json:"stages"`
	HandoffID            productcapability.HandoffID `json:"handoff_id"`
	BaselineSource       projectbrain.GraphSnapshot  `json:"baseline_source"`
	BrainViewDigest      contracts.ContentHash       `json:"project_brain_view_digest"`
	CompanyStateRecordID string                      `json:"company_state_record_id"`
	IdempotencyKey       string                      `json:"idempotency_key"`
}

func (value ProductExecutionPlan) Validate(
	organizationID contracts.OrganizationID,
	initiativeID string,
) error {
	if !validToken(value.ExecutionID) || len(value.ExecutionID) > 64 ||
		!validToken(string(value.ProjectID)) || !validToken(string(value.WorkspaceID)) ||
		!validToken(string(value.HandoffID)) || !validToken(value.CompanyStateRecordID) ||
		!validToken(value.IdempotencyKey) || value.SquadRequirement.Validate() != nil ||
		value.SquadRequirement.OrganizationID != organizationID ||
		string(value.SquadRequirement.InitiativeID) != initiativeID ||
		value.SquadRequirement.LifecycleStage != "DESIGN" ||
		value.BaselineSource.Validate() != nil || !value.BaselineSource.Fresh ||
		value.BrainViewDigest.Validate() != nil {
		return fmt.Errorf("company runtime: product execution plan is invalid")
	}
	expected := []string{"product", "design", "build", "verification", "deployment", "telemetry"}
	if len(value.Stages) != len(expected) {
		return fmt.Errorf("company runtime: product execution requires every ordered stage")
	}
	seenNodes := make(map[string]bool, len(value.Stages))
	seenNeeds := make(map[string]bool, len(value.Stages))
	for index, stage := range value.Stages {
		if stage.Stage != expected[index] || !validToken(stage.PlanNodeID) ||
			!validToken(stage.NeedID) || seenNodes[stage.PlanNodeID] || seenNeeds[stage.NeedID] ||
			!slices.Contains(value.SquadRequirement.GraphScopes, stage.PlanNodeID) {
			return fmt.Errorf("company runtime: product execution stages are incomplete or conflicting")
		}
		seenNodes[stage.PlanNodeID] = true
		seenNeeds[stage.NeedID] = true
	}
	return nil
}

type FundingResult struct {
	Decision     portfolio.DecisionReceipt   `json:"decision"`
	Checkpoint   companylifecycle.Checkpoint `json:"checkpoint"`
	Plan         initiative.Plan             `json:"plan"`
	State        string                      `json:"state"`
	Deduplicated bool                        `json:"deduplicated"`
}

func (value FundingRequest) Validate() error {
	if !validToken(value.FundingID) || len(value.FundingID) > 64 || !validToken(string(value.DecisionID)) ||
		!validToken(string(value.OpportunityID)) || !validToken(string(value.PortfolioID)) ||
		!validToken(string(value.RequestedBySeatID)) ||
		!validToken(value.DecisionIdempotencyKey) || value.Initiative.Version != 1 ||
		!validToken(value.Initiative.AllocationID) || !validToken(value.Initiative.Currency) ||
		!validUTC(value.Initiative.Deadline) {
		return fmt.Errorf("company runtime: funding request identity is invalid")
	}
	if err := value.Assessment.Validate(); err != nil {
		return err
	}
	if err := value.CompanyState.Validate(); err != nil {
		return err
	}
	if err := value.Correction.Validate(); err != nil {
		return err
	}
	if err := value.CapitalImpact.Validate(); err != nil {
		return err
	}
	if len(value.Evidence) == 0 || len(value.Evidence) > 256 {
		return fmt.Errorf("company runtime: funding evidence is invalid")
	}
	for index := range value.Evidence {
		if err := value.Evidence[index].Validate(); err != nil {
			return err
		}
	}
	if value.ProductExecution != nil {
		initiativeID := "initiative:" + string(value.OpportunityID)
		if err := value.ProductExecution.Validate(value.CompanyState.OrganizationID, initiativeID); err != nil {
			return err
		}
	}
	return nil
}

func canonicalCadences(values []portfolio.Cadence) bool {
	kinds := make([]string, len(values))
	for index := range values {
		kinds[index] = string(values[index].Kind)
	}
	return slices.IsSorted(kinds) && !hasDuplicate(kinds)
}

func hasDuplicate(values []string) bool {
	for index := 1; index < len(values); index++ {
		if values[index] == values[index-1] {
			return true
		}
	}
	return false
}

func validToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 255 {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character) {
			continue
		}
		return false
	}
	return true
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
