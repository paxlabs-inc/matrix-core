package autonomouscompany

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"time"

	"centra/workforce/internal/companylifecycle"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/learning"
)

const (
	NextCyclePlanSchemaVersion  = "workforce.autonomous-company-next-cycle-plan.v1"
	NextCycleEventSchemaVersion = "workforce.autonomous-company-next-cycle-event.v1"
)

type OperationKind string

const (
	OperationLifecycleTransition OperationKind = "lifecycle_transition"
	OperationCompanyDiscovery    OperationKind = "company_runtime_discovery"
	OperationFounderRequired     OperationKind = "founder_required"
)

type CycleOperation struct {
	Sequence      uint16                    `json:"sequence"`
	Kind          OperationKind             `json:"kind"`
	FromState     companylifecycle.State    `json:"from_state"`
	ToState       companylifecycle.State    `json:"to_state"`
	Decision      companylifecycle.Decision `json:"decision"`
	RuntimeAction learning.NextAction       `json:"runtime_action"`
}

func (value CycleOperation) Validate() error {
	if value.Sequence == 0 {
		return fmt.Errorf("autonomous company: next-cycle operation sequence is invalid")
	}
	switch value.Kind {
	case OperationLifecycleTransition:
		if !companylifecycle.TransitionAllowed(
			value.FromState, value.ToState, value.Decision, "",
		) || value.RuntimeAction != "" {
			return fmt.Errorf("autonomous company: lifecycle operation is invalid")
		}
	case OperationCompanyDiscovery:
		if value.FromState != "" || value.ToState != "" || value.Decision != "" ||
			value.RuntimeAction != learning.ActionDiscover {
			return fmt.Errorf("autonomous company: discovery operation is invalid")
		}
	case OperationFounderRequired:
		if value.FromState != "" || value.ToState != "" || value.Decision != "" ||
			value.RuntimeAction != learning.ActionHumanReview {
			return fmt.Errorf("autonomous company: founder-required operation is invalid")
		}
	default:
		return fmt.Errorf("autonomous company: next-cycle operation kind is invalid")
	}
	return nil
}

type NextCyclePlan struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  string                   `json:"plan_id"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	ConclusionID        string                   `json:"conclusion_id"`
	ConclusionHash      contracts.ContentHash    `json:"conclusion_hash"`
	HypothesisID        string                   `json:"hypothesis_id"`
	InitiativeID        string                   `json:"initiative_id"`
	SelectedAction      learning.NextAction      `json:"selected_action"`
	PortfolioFeedbackID string                   `json:"portfolio_feedback_id"`
	DueAt               time.Time                `json:"due_at"`
	ClaimedAt           time.Time                `json:"claimed_at"`
	Operations          []CycleOperation         `json:"operations"`
	ContentHash         contracts.ContentHash    `json:"content_hash"`
	Signature           contracts.Signature      `json:"signature"`
}

func (value NextCyclePlan) Validate() error {
	if value.SchemaVersion != NextCyclePlanSchemaVersion || token(value.ID) != nil ||
		token(string(value.OrganizationID)) != nil || token(value.ConclusionID) != nil ||
		token(value.HypothesisID) != nil || token(value.InitiativeID) != nil ||
		!value.SelectedAction.Valid() || token(value.PortfolioFeedbackID) != nil ||
		!utc(value.DueAt) || !utc(value.ClaimedAt) || value.ClaimedAt.Before(value.DueAt) ||
		len(value.Operations) == 0 || len(value.Operations) > 2 {
		return fmt.Errorf("autonomous company: next-cycle plan is invalid")
	}
	if err := value.ConclusionHash.Validate(); err != nil {
		return fmt.Errorf("autonomous company: next-cycle conclusion hash: %w", err)
	}
	for index, operation := range value.Operations {
		if operation.Sequence != uint16(index+1) || operation.Validate() != nil {
			return fmt.Errorf("autonomous company: next-cycle operations are invalid")
		}
	}
	if !slices.Equal(value.Operations, operationsFor(value.SelectedAction)) {
		return fmt.Errorf("autonomous company: next-cycle action mapping is not exact")
	}
	if err := value.ContentHash.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type NextCycleState string

const (
	NextCyclePlanned   NextCycleState = "planned"
	NextCycleRunning   NextCycleState = "running"
	NextCycleBlocked   NextCycleState = "blocked"
	NextCyclePassed    NextCycleState = "passed"
	NextCycleFailed    NextCycleState = "failed"
	NextCycleUncertain NextCycleState = "uncertain"
)

func (value NextCycleState) Valid() bool {
	switch value {
	case NextCyclePlanned, NextCycleRunning, NextCycleBlocked,
		NextCyclePassed, NextCycleFailed, NextCycleUncertain:
		return true
	default:
		return false
	}
}

type NextCycleUpdate struct {
	PlanID      string                `json:"plan_id"`
	PlanHash    contracts.ContentHash `json:"plan_hash"`
	State       NextCycleState        `json:"state"`
	Evidence    []EvidenceBinding     `json:"evidence"`
	ReasonCodes []string              `json:"reason_codes"`
	OccurredAt  time.Time             `json:"occurred_at"`
}

func (value NextCycleUpdate) Validate() error {
	if token(value.PlanID) != nil || value.PlanHash.Validate() != nil ||
		!value.State.Valid() || value.State == NextCyclePlanned || !utc(value.OccurredAt) ||
		len(value.Evidence) > 64 || !sortedTokens(value.ReasonCodes) {
		return fmt.Errorf("autonomous company: next-cycle update is invalid")
	}
	if (value.State == NextCycleBlocked || value.State == NextCycleFailed ||
		value.State == NextCycleUncertain) && len(value.ReasonCodes) == 0 {
		return fmt.Errorf("autonomous company: next-cycle non-success state requires a reason")
	}
	if len(value.Evidence) == 0 &&
		!(value.State == NextCycleBlocked && slices.Equal(value.ReasonCodes, []string{"founder_required"})) {
		return fmt.Errorf("autonomous company: next-cycle update lacks authoritative evidence")
	}
	previous := ""
	for _, binding := range value.Evidence {
		if binding.Validate() != nil {
			return fmt.Errorf("autonomous company: next-cycle evidence is invalid")
		}
		key := string(binding.Kind) + "\x00" + binding.RecordID + "\x00" +
			fmt.Sprintf("%020d", binding.RecordVersion)
		if key <= previous {
			return fmt.Errorf("autonomous company: next-cycle evidence must be sorted and unique")
		}
		previous = key
	}
	if value.State == NextCyclePassed && !hasEvidence(value.Evidence, EvidenceNextCycleCompletionReceipt) {
		return fmt.Errorf("autonomous company: completed next cycle lacks its completion receipt")
	}
	if value.State == NextCyclePassed && !hasEvidence(value.Evidence, EvidenceNextCycleDispatchReceipt) {
		return fmt.Errorf("autonomous company: completed next cycle lacks its dispatch receipt")
	}
	if value.State == NextCycleRunning && !hasEvidence(value.Evidence, EvidenceNextCycleDispatchReceipt) {
		return fmt.Errorf("autonomous company: running next cycle lacks its dispatch receipt")
	}
	return nil
}

type NextCycleEvent struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"event_id"`
	Sequence       uint64                   `json:"sequence"`
	PlanID         string                   `json:"plan_id"`
	PlanHash       contracts.ContentHash    `json:"plan_hash"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	InitiativeID   string                   `json:"initiative_id"`
	State          NextCycleState           `json:"state"`
	Evidence       []EvidenceBinding        `json:"evidence"`
	ReasonCodes    []string                 `json:"reason_codes"`
	OccurredAt     time.Time                `json:"occurred_at"`
	ContentHash    contracts.ContentHash    `json:"content_hash"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value NextCycleEvent) Validate() error {
	update := NextCycleUpdate{
		PlanID: value.PlanID, PlanHash: value.PlanHash, State: value.State,
		Evidence: value.Evidence, ReasonCodes: value.ReasonCodes, OccurredAt: value.OccurredAt,
	}
	if value.SchemaVersion != NextCycleEventSchemaVersion || token(value.ID) != nil ||
		value.Sequence == 0 || token(string(value.OrganizationID)) != nil ||
		token(value.InitiativeID) != nil || update.Validate() != nil {
		return fmt.Errorf("autonomous company: next-cycle event is invalid")
	}
	if err := value.ContentHash.Validate(); err != nil {
		return err
	}
	return value.Signature.Validate()
}

type NextCycleSnapshot struct {
	Plan          NextCyclePlan         `json:"plan"`
	CanonicalHash contracts.ContentHash `json:"canonical_hash"`
	State         NextCycleState        `json:"state"`
	LastEvent     *NextCycleEvent       `json:"last_event"`
}

func newNextCyclePlan(
	organizationID contracts.OrganizationID,
	value learning.NextCycle,
	conclusionHash contracts.ContentHash,
	claimedAt time.Time,
) NextCyclePlan {
	return NextCyclePlan{
		SchemaVersion:       NextCyclePlanSchemaVersion,
		ID:                  nextCyclePlanID(organizationID, value.ConclusionID),
		OrganizationID:      organizationID,
		ConclusionID:        value.ConclusionID,
		ConclusionHash:      conclusionHash,
		HypothesisID:        value.HypothesisID,
		InitiativeID:        value.InitiativeID,
		SelectedAction:      value.Action,
		PortfolioFeedbackID: value.PortfolioFeedbackID,
		DueAt:               value.DueAt,
		ClaimedAt:           claimedAt,
		Operations:          operationsFor(value.Action),
	}
}

func operationsFor(action learning.NextAction) []CycleOperation {
	transition := func(
		sequence uint16,
		from, to companylifecycle.State,
		decision companylifecycle.Decision,
	) CycleOperation {
		return CycleOperation{
			Sequence: sequence, Kind: OperationLifecycleTransition,
			FromState: from, ToState: to, Decision: decision,
		}
	}
	discovery := func(sequence uint16) CycleOperation {
		return CycleOperation{
			Sequence: sequence, Kind: OperationCompanyDiscovery,
			RuntimeAction: learning.ActionDiscover,
		}
	}
	switch action {
	case learning.ActionScale:
		return []CycleOperation{
			transition(1, companylifecycle.StateMeasure, companylifecycle.StateScale, companylifecycle.DecisionScale),
			transition(2, companylifecycle.StateScale, companylifecycle.StateDiscover, companylifecycle.DecisionAdvance),
		}
	case learning.ActionPivot:
		return []CycleOperation{
			transition(1, companylifecycle.StateMeasure, companylifecycle.StatePivot, companylifecycle.DecisionPivot),
			transition(2, companylifecycle.StatePivot, companylifecycle.StateDiscover, companylifecycle.DecisionAdvance),
		}
	case learning.ActionMaintain:
		return []CycleOperation{
			transition(1, companylifecycle.StateMeasure, companylifecycle.StateMaintain, companylifecycle.DecisionMaintain),
			transition(2, companylifecycle.StateMaintain, companylifecycle.StateDiscover, companylifecycle.DecisionAdvance),
		}
	case learning.ActionTerminate:
		return []CycleOperation{
			transition(1, companylifecycle.StateMeasure, companylifecycle.StateTerminate, companylifecycle.DecisionTerminate),
			discovery(2),
		}
	case learning.ActionDiscover:
		return []CycleOperation{discovery(1)}
	case learning.ActionHumanReview:
		return []CycleOperation{{
			Sequence: 1, Kind: OperationFounderRequired,
			RuntimeAction: learning.ActionHumanReview,
		}}
	default:
		return nil
	}
}

func nextCyclePlanID(organizationID contracts.OrganizationID, conclusionID string) string {
	hash := sha256.Sum256([]byte(string(organizationID) + "\x00" + conclusionID))
	return "next-cycle:" + hex.EncodeToString(hash[:])
}

func nextCycleEventID(update NextCycleUpdate) (string, error) {
	canonical, err := contracts.EncodeCanonical(&update)
	if err != nil {
		return "", err
	}
	hash := sha256.Sum256(canonical)
	return "next-cycle-event:" + hex.EncodeToString(hash[:]), nil
}

func hasEvidence(values []EvidenceBinding, kind EvidenceKind) bool {
	return slices.ContainsFunc(values, func(value EvidenceBinding) bool { return value.Kind == kind })
}
