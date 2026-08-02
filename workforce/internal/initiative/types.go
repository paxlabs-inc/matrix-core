// Package initiative compiles funded company initiatives into deterministic,
// bounded Work Order graphs without granting models control-plane authority.
package initiative

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/companystate"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/portfolio"
	"matrix/workforce/internal/workorder"
)

const (
	InitiativeSchemaVersion = "workforce.initiative.v1"
	BlueprintSchemaVersion  = "workforce.initiative-blueprint.v1"
	PlanSchemaVersion       = "workforce.initiative-plan.v1"
	MutationSchemaVersion   = "workforce.initiative-plan-mutation.v1"
)

type CapitalAllocation struct {
	ID                string `json:"capital_allocation_id"`
	Currency          string `json:"currency"`
	CapitalMicrounits uint64 `json:"capital_microunits"`
	RiskMicrounits    uint64 `json:"risk_microunits"`
}

func (value CapitalAllocation) Validate() error {
	if !validToken(value.ID) || !validToken(value.Currency) || value.CapitalMicrounits == 0 {
		return fmt.Errorf("initiative: capital allocation is invalid")
	}
	return nil
}

type CapabilityRequirement struct {
	ID           string                   `json:"capability_id"`
	Department   contracts.DepartmentKind `json:"department"`
	SkillID      contracts.SkillID        `json:"skill_id"`
	Operations   []string                 `json:"operations"`
	MinimumSeats uint16                   `json:"minimum_seats"`
	Independent  bool                     `json:"independent"`
}

func (value CapabilityRequirement) Validate() error {
	if !validToken(value.ID) || !value.Department.Valid() || value.SkillID == "" ||
		value.MinimumSeats == 0 || value.MinimumSeats > 32 ||
		!validSet(value.Operations, 1, 64, 128) {
		return fmt.Errorf("initiative: capability requirement is invalid")
	}
	return nil
}

type CapabilityPlan struct {
	ID           string                  `json:"capability_plan_id"`
	Version      uint64                  `json:"version"`
	Requirements []CapabilityRequirement `json:"requirements"`
	Hash         contracts.ContentHash   `json:"hash"`
}

func (value CapabilityPlan) Validate() error {
	if !validToken(value.ID) || value.Version == 0 || len(value.Requirements) == 0 ||
		len(value.Requirements) > 128 || value.Hash.Validate() != nil {
		return fmt.Errorf("initiative: capability plan is invalid")
	}
	previous := ""
	for index := range value.Requirements {
		if value.Requirements[index].Validate() != nil || value.Requirements[index].ID <= previous {
			return fmt.Errorf("initiative: capability requirements must be sorted and unique")
		}
		previous = value.Requirements[index].ID
	}
	return nil
}

type BusinessGate struct {
	ID                   string                         `json:"gate_id"`
	MetricID             string                         `json:"metric_id"`
	Comparator           string                         `json:"comparator"`
	ThresholdMicrounits  int64                          `json:"threshold_microunits"`
	MinimumObservations  uint32                         `json:"minimum_observations"`
	MaximumFreshness     time.Duration                  `json:"maximum_freshness"`
	AuthoritativeSources []companystate.RecordReference `json:"authoritative_sources"`
	SuccessCriterion     string                         `json:"success_criterion"`
}

func (value BusinessGate) Validate() error {
	if !validToken(value.ID) || !validToken(value.MetricID) ||
		(value.Comparator != "gte" && value.Comparator != "lte" && value.Comparator != "eq") ||
		value.MinimumObservations == 0 || value.MaximumFreshness <= 0 ||
		value.MaximumFreshness > 365*24*time.Hour ||
		strings.TrimSpace(value.SuccessCriterion) == "" || len(value.SuccessCriterion) > 1024 ||
		len(value.AuthoritativeSources) == 0 || len(value.AuthoritativeSources) > 256 {
		return fmt.Errorf("initiative: business outcome gate is invalid")
	}
	previous := ""
	for index := range value.AuthoritativeSources {
		if value.AuthoritativeSources[index].Validate() != nil ||
			value.AuthoritativeSources[index].ID <= previous {
			return fmt.Errorf("initiative: business gate sources must be sorted and unique")
		}
		previous = value.AuthoritativeSources[index].ID
	}
	return nil
}

type Initiative struct {
	SchemaVersion          string                   `json:"schema_version"`
	ID                     portfolio.InitiativeID   `json:"initiative_id"`
	Version                uint64                   `json:"version"`
	OrganizationID         contracts.OrganizationID `json:"organization_id"`
	MissionID              string                   `json:"mission_id"`
	MissionVersion         uint64                   `json:"mission_version"`
	ConstitutionID         string                   `json:"constitution_id"`
	ConstitutionVersion    uint64                   `json:"constitution_version"`
	CapitalEnvelopeVersion uint64                   `json:"capital_envelope_version"`
	IssuerPolicyVersion    uint64                   `json:"issuer_policy_version"`
	PortfolioDecisionID    portfolio.DecisionID     `json:"portfolio_decision_id"`
	Allocation             CapitalAllocation        `json:"allocation"`
	CapabilityPlan         CapabilityPlan           `json:"capability_plan"`
	Objective              string                   `json:"objective"`
	ExecutionCriteria      []string                 `json:"execution_criteria"`
	BusinessCriteria       []string                 `json:"business_criteria"`
	BusinessGates          []BusinessGate           `json:"business_gates"`
	Deadline               time.Time                `json:"deadline"`
	CreatedAt              time.Time                `json:"created_at"`
	Signature              contracts.Signature      `json:"signature"`
}

func (value Initiative) Validate() error {
	if value.SchemaVersion != InitiativeSchemaVersion || !validToken(string(value.ID)) || value.Version == 0 ||
		value.OrganizationID == "" || value.MissionID != "mission:"+string(value.OrganizationID) ||
		value.MissionVersion == 0 ||
		value.ConstitutionID != "constitution:"+string(value.OrganizationID) ||
		value.ConstitutionVersion == 0 || value.CapitalEnvelopeVersion == 0 ||
		value.IssuerPolicyVersion == 0 || !validToken(string(value.PortfolioDecisionID)) ||
		strings.TrimSpace(value.Objective) == "" || len(value.Objective) > 4096 ||
		!validUTC(value.CreatedAt) || !validUTC(value.Deadline) ||
		!value.Deadline.After(value.CreatedAt) {
		return fmt.Errorf("initiative: identity, objective, or time is invalid")
	}
	if value.Allocation.Validate() != nil || value.CapabilityPlan.Validate() != nil ||
		!validSet(value.ExecutionCriteria, 1, 64, 2048) ||
		!validSet(value.BusinessCriteria, 1, 64, 2048) || len(value.BusinessGates) == 0 ||
		len(value.BusinessGates) > 128 {
		return fmt.Errorf("initiative: bounds, capabilities, or success criteria are invalid")
	}
	previous := ""
	for index := range value.BusinessGates {
		if value.BusinessGates[index].Validate() != nil || value.BusinessGates[index].ID <= previous {
			return fmt.Errorf("initiative: business gates must be sorted and unique")
		}
		previous = value.BusinessGates[index].ID
	}
	return value.Signature.Validate()
}

type NodeKind string

const (
	NodeWorkOrder       NodeKind = "work_order"
	NodeIntent          NodeKind = "intent"
	NodeDecisionGate    NodeKind = "decision_gate"
	NodeEvidenceGate    NodeKind = "evidence_gate"
	NodeApprovalGate    NodeKind = "approval_gate"
	NodeEffectGate      NodeKind = "effect_gate"
	NodeOutcomeGate     NodeKind = "outcome_gate"
	NodeBranch          NodeKind = "branch"
	NodeTerminalSuccess NodeKind = "terminal_success"
	NodeTerminalFailure NodeKind = "terminal_failure"
	NodeTerminalCancel  NodeKind = "terminal_cancelled"
)

func (value NodeKind) Valid() bool {
	switch value {
	case NodeWorkOrder, NodeIntent, NodeDecisionGate, NodeEvidenceGate,
		NodeApprovalGate, NodeEffectGate, NodeOutcomeGate, NodeBranch,
		NodeTerminalSuccess, NodeTerminalFailure, NodeTerminalCancel:
		return true
	default:
		return false
	}
}

type GateOutcome string

const (
	OutcomeSatisfied GateOutcome = "satisfied"
	OutcomeFailed    GateOutcome = "failed"
	OutcomeExpired   GateOutcome = "expired"
)

func (value GateOutcome) Valid() bool {
	return value == OutcomeSatisfied || value == OutcomeFailed || value == OutcomeExpired
}

type GateSpec struct {
	PredicateID      string                         `json:"predicate_id"`
	Evidence         []companystate.RecordReference `json:"evidence"`
	AuthorityClauses []string                       `json:"authority_clauses"`
	ExpiresAt        time.Time                      `json:"expires_at"`
}

func (value GateSpec) Validate() error {
	if !validToken(value.PredicateID) || !validSet(value.AuthorityClauses, 1, 64, 512) ||
		!validUTC(value.ExpiresAt) || len(value.Evidence) == 0 || len(value.Evidence) > 256 {
		return fmt.Errorf("initiative: gate specification is invalid")
	}
	previous := ""
	for index := range value.Evidence {
		if value.Evidence[index].Validate() != nil || value.Evidence[index].ID <= previous {
			return fmt.Errorf("initiative: gate evidence must be sorted and unique")
		}
		previous = value.Evidence[index].ID
	}
	return nil
}

type BranchCase struct {
	Outcome   GateOutcome `json:"outcome"`
	Successor string      `json:"successor_node_id"`
}

type BranchSpec struct {
	GateNodeID string       `json:"gate_node_id"`
	Cases      []BranchCase `json:"cases"`
}

func (value BranchSpec) Validate() error {
	if !validToken(value.GateNodeID) || len(value.Cases) < 2 || len(value.Cases) > 3 {
		return fmt.Errorf("initiative: branch requires two or three outcomes")
	}
	previous := GateOutcome("")
	successors := make(map[string]bool, len(value.Cases))
	for _, branch := range value.Cases {
		if !branch.Outcome.Valid() || !validToken(branch.Successor) || branch.Outcome <= previous {
			return fmt.Errorf("initiative: branch cases must be sorted and unique")
		}
		if successors[branch.Successor] {
			return fmt.Errorf("initiative: branch outcomes require distinct successors")
		}
		successors[branch.Successor] = true
		previous = branch.Outcome
	}
	return nil
}

type TerminalSpec struct {
	Disposition              string `json:"disposition"`
	RequiresBusinessOutcomes bool   `json:"requires_business_outcomes"`
}

func (value TerminalSpec) Validate(kind NodeKind) error {
	expected := map[NodeKind]string{
		NodeTerminalSuccess: "success", NodeTerminalFailure: "failure", NodeTerminalCancel: "cancelled",
	}[kind]
	if expected == "" || value.Disposition != expected ||
		(kind == NodeTerminalSuccess) != value.RequiresBusinessOutcomes {
		return fmt.Errorf("initiative: terminal alternative is invalid")
	}
	return nil
}

type WorkOrderSpec struct {
	Order             workorder.CompanyOrder `json:"order"`
	Class             string                 `json:"class"`
	CapitalMicrounits uint64                 `json:"capital_microunits"`
	RiskMicrounits    uint64                 `json:"risk_microunits"`
	EffectIdentities  []string               `json:"effect_identities"`
}

type NodeTemplate struct {
	ID        string         `json:"node_id"`
	Kind      NodeKind       `json:"kind"`
	Title     string         `json:"title"`
	WorkOrder *WorkOrderSpec `json:"work_order"`
	Gate      *GateSpec      `json:"gate"`
	Branch    *BranchSpec    `json:"branch"`
	Terminal  *TerminalSpec  `json:"terminal"`
}

func (value NodeTemplate) Validate() error {
	if !validToken(value.ID) || !value.Kind.Valid() || strings.TrimSpace(value.Title) == "" ||
		len(value.Title) > 512 {
		return fmt.Errorf("initiative: node identity is invalid")
	}
	set := 0
	if value.WorkOrder != nil {
		set++
	}
	if value.Gate != nil {
		set++
	}
	if value.Branch != nil {
		set++
	}
	if value.Terminal != nil {
		set++
	}
	switch value.Kind {
	case NodeWorkOrder:
		if set != 1 || value.WorkOrder == nil || !validToken(value.WorkOrder.Class) ||
			value.WorkOrder.CapitalMicrounits == 0 ||
			!validSet(value.WorkOrder.EffectIdentities, 1, 64, 128) {
			return fmt.Errorf("initiative: Work Order node is invalid")
		}
	case NodeDecisionGate, NodeEvidenceGate, NodeApprovalGate, NodeEffectGate, NodeOutcomeGate:
		if set != 1 || value.Gate == nil || value.Gate.Validate() != nil {
			return fmt.Errorf("initiative: typed gate node is invalid")
		}
	case NodeBranch:
		if set != 1 || value.Branch == nil || value.Branch.Validate() != nil {
			return fmt.Errorf("initiative: branch node is invalid")
		}
	case NodeTerminalSuccess, NodeTerminalFailure, NodeTerminalCancel:
		if set != 1 || value.Terminal == nil || value.Terminal.Validate(value.Kind) != nil {
			return fmt.Errorf("initiative: terminal node is invalid")
		}
	case NodeIntent:
		if set != 0 {
			return fmt.Errorf("initiative: intent node cannot carry another node payload")
		}
	}
	return nil
}

type SuccessorSchedule struct {
	NotBefore     time.Time `json:"not_before"`
	Deadline      time.Time `json:"deadline"`
	PriorityDelta int32     `json:"priority_delta"`
}

func (value SuccessorSchedule) Validate() error {
	if !validUTC(value.NotBefore) || !validUTC(value.Deadline) ||
		value.Deadline.Before(value.NotBefore) || value.PriorityDelta < -1000 ||
		value.PriorityDelta > 1000 {
		return fmt.Errorf("initiative: successor schedule is invalid")
	}
	return nil
}

type Edge struct {
	Prerequisite string            `json:"prerequisite"`
	Successor    string            `json:"successor"`
	When         *GateOutcome      `json:"when"`
	Schedule     SuccessorSchedule `json:"schedule"`
}

func (value Edge) Validate() error {
	if !validToken(value.Prerequisite) || !validToken(value.Successor) ||
		value.Prerequisite == value.Successor || value.Schedule.Validate() != nil ||
		value.When != nil && !value.When.Valid() {
		return fmt.Errorf("initiative: graph edge is invalid")
	}
	return nil
}

type Blueprint struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"blueprint_id"`
	Version        uint64                   `json:"version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   portfolio.InitiativeID   `json:"initiative_id"`
	Nodes          []NodeTemplate           `json:"nodes"`
	Edges          []Edge                   `json:"edges"`
}

func (value Blueprint) Validate() error {
	if value.SchemaVersion != BlueprintSchemaVersion || !validToken(value.ID) || value.Version == 0 ||
		value.OrganizationID == "" || !validToken(string(value.InitiativeID)) || len(value.Nodes) < 11 ||
		len(value.Nodes) > 1024 || len(value.Edges) == 0 || len(value.Edges) > 4096 {
		return fmt.Errorf("initiative: blueprint identity or size is invalid")
	}
	previous := ""
	kinds := make(map[NodeKind]bool)
	for index := range value.Nodes {
		if value.Nodes[index].Validate() != nil || value.Nodes[index].ID <= previous {
			return fmt.Errorf("initiative: blueprint nodes must be sorted and unique")
		}
		previous = value.Nodes[index].ID
		kinds[value.Nodes[index].Kind] = true
	}
	for _, required := range []NodeKind{
		NodeWorkOrder, NodeIntent, NodeDecisionGate, NodeEvidenceGate,
		NodeApprovalGate, NodeEffectGate, NodeOutcomeGate, NodeBranch,
		NodeTerminalSuccess, NodeTerminalFailure, NodeTerminalCancel,
	} {
		if !kinds[required] {
			return fmt.Errorf("initiative: blueprint requires node kind %s", required)
		}
	}
	return nil
}

type AuthorityBinding struct {
	MissionVersion         uint64 `json:"mission_version"`
	ConstitutionVersion    uint64 `json:"constitution_version"`
	CapitalEnvelopeVersion uint64 `json:"capital_envelope_version"`
	IssuerPolicyVersion    uint64 `json:"issuer_policy_version"`
	PortfolioDecisionID    string `json:"portfolio_decision_id"`
	CapitalAllocationID    string `json:"capital_allocation_id"`
	CapabilityPlanID       string `json:"capability_plan_id"`
}

type NodeState string

const (
	StatePending     NodeState = "pending"
	StatePreserved   NodeState = "preserved"
	StateInvalidated NodeState = "invalidated"
	StateCancelled   NodeState = "cancelled"
)

type CompiledNode struct {
	Template          NodeTemplate            `json:"template"`
	Order             *workorder.CompanyOrder `json:"company_order"`
	Digest            contracts.ContentHash   `json:"digest"`
	State             NodeState               `json:"state"`
	ReceiptReferences []contracts.ReceiptID   `json:"receipt_references"`
}

type Plan struct {
	SchemaVersion     string                   `json:"schema_version"`
	ID                string                   `json:"plan_id"`
	Version           uint64                   `json:"version"`
	OrganizationID    contracts.OrganizationID `json:"organization_id"`
	InitiativeID      portfolio.InitiativeID   `json:"initiative_id"`
	InitiativeVersion uint64                   `json:"initiative_version"`
	BlueprintID       string                   `json:"blueprint_id"`
	BlueprintVersion  uint64                   `json:"blueprint_version"`
	Authority         AuthorityBinding         `json:"authority"`
	Nodes             []CompiledNode           `json:"nodes"`
	Edges             []Edge                   `json:"edges"`
	TopologicalOrder  []string                 `json:"topological_order"`
	CapitalMicrounits uint64                   `json:"capital_microunits"`
	RiskMicrounits    uint64                   `json:"risk_microunits"`
	CompiledAt        time.Time                `json:"compiled_at"`
	Hash              contracts.ContentHash    `json:"hash"`
	Signature         contracts.Signature      `json:"signature"`
}

func validSet(values []string, minimum, maximum, maxBytes int) bool {
	if len(values) < minimum || len(values) > maximum || !slices.IsSorted(values) {
		return false
	}
	for index, value := range values {
		if strings.TrimSpace(value) == "" || len(value) > maxBytes || index > 0 && value == values[index-1] {
			return false
		}
	}
	return true
}

func validToken(value string) bool {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
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
