package commercialexecution

import (
	"context"
	"fmt"
	"sort"

	"centra/workforce/internal/businessoutcome"
	"centra/workforce/internal/contracts"
)

// Coordinator binds the commercial execution's final measurement phase to the
// independently reviewed business-outcome ledger. It prepares evidence; it
// does not sign evidence or advance an execution on the caller's behalf.
type Coordinator struct {
	executions *Store
	outcomes   *businessoutcome.Store
}

func NewCoordinator(executions *Store, outcomes *businessoutcome.Store) (*Coordinator, error) {
	if executions == nil || outcomes == nil {
		return nil, fmt.Errorf("commercial execution: execution and outcome stores are required")
	}
	return &Coordinator{executions: executions, outcomes: outcomes}, nil
}

// EvaluateMeasurement reevaluates the preregistered gate and returns the exact
// durable records that a phase issuer must bind into signed completion
// evidence. Open or blocked gates never become successful execution evidence.
func (coordinator *Coordinator) EvaluateMeasurement(
	ctx context.Context,
	executionID ExecutionID,
) (MeasurementProof, error) {
	if coordinator == nil || coordinator.executions == nil || coordinator.outcomes == nil {
		return MeasurementProof{}, ErrUnauthorized
	}
	snapshot, err := coordinator.executions.Load(ctx, executionID)
	if err != nil {
		return MeasurementProof{}, err
	}
	if snapshot.CurrentPhase != PhaseMeasurement || snapshot.State == StateCompleted || snapshot.State == StateFailed {
		return MeasurementProof{}, ErrOutOfOrder
	}
	requirement := snapshot.Plan.Body.Scope.Gate
	decision, _, err := coordinator.outcomes.EvaluateGate(ctx, requirement)
	if err != nil {
		return MeasurementProof{}, err
	}
	partial := MeasurementProof{Requirement: requirement, Decision: decision}
	switch decision.State {
	case businessoutcome.GateOpen:
		return partial, ErrPending
	case businessoutcome.GateBlocked:
		return partial, ErrFailed
	case businessoutcome.GateSatisfied:
	default:
		return partial, ErrIntegrity
	}
	if !sameGateRequirement(decision.Requirement, requirement) {
		return MeasurementProof{}, ErrIntegrity
	}
	outcome, err := coordinator.outcomes.LoadOutcome(ctx, requirement.OutcomeID, true)
	if err != nil {
		return MeasurementProof{}, err
	}
	recordHash, err := businessoutcome.OutcomeRecordHash(outcome.Record)
	if err != nil || recordHash != decision.OutcomeHash || outcome.Record.Body.ID != requirement.OutcomeID ||
		outcome.Record.Body.OrganizationID != requirement.OrganizationID ||
		outcome.Record.Body.InitiativeID != requirement.InitiativeID ||
		outcome.Record.Body.Metric != requirement.Metric ||
		outcome.Record.Body.Kind != requirement.OutcomeKind ||
		!sameObservationBindings(outcome.Record.Body.Observations, decision.Observations) {
		return MeasurementProof{}, ErrIntegrity
	}
	metric, err := coordinator.outcomes.LoadMetric(ctx, requirement.Metric, true)
	if err != nil {
		return MeasurementProof{}, err
	}
	if metric.Reference() != requirement.Metric || metric.Body.OrganizationID != requirement.OrganizationID ||
		metric.Body.InitiativeID != requirement.InitiativeID || metric.Body.OutcomeKind != requirement.OutcomeKind {
		return MeasurementProof{}, ErrIntegrity
	}
	operation, err := measurementOperation(snapshot.Plan.Body.Scope)
	if err != nil {
		return MeasurementProof{}, err
	}
	sources := make([]SourceRef, 0, len(decision.Observations)+3)
	sources = append(sources, SourceRef{
		Role:       RoleMetricDefinition,
		Kind:       SourceBusinessMetric,
		RecordID:   string(requirement.Metric.ID),
		Version:    requirement.Metric.Version,
		Hash:       requirement.Metric.DefinitionHash,
		Operation:  operation,
		State:      SourceCompleted,
		Authority:  AuthorityInternalVerified,
		ObservedAt: metric.Body.RegisteredAt,
	})
	observations := make([]businessoutcome.Observation, 0, len(decision.Observations))
	for _, binding := range decision.Observations {
		observation, err := coordinator.outcomes.LoadObservation(ctx, binding.ID)
		if err != nil {
			return MeasurementProof{}, err
		}
		if observation.ContentHash != binding.Hash || observation.Body.OrganizationID != requirement.OrganizationID ||
			observation.Body.InitiativeID != requirement.InitiativeID || observation.Body.Metric != requirement.Metric ||
			observation.Body.OutcomeKind != requirement.OutcomeKind {
			return MeasurementProof{}, ErrIntegrity
		}
		state := SourceCompleted
		switch observation.Body.Status {
		case businessoutcome.MeasurementObserved:
		case businessoutcome.MeasurementReconciled:
			state = SourceReconciled
		default:
			return MeasurementProof{}, ErrPending
		}
		observations = append(observations, observation)
		sources = append(sources, SourceRef{
			Role:       RoleMetricObservation,
			Kind:       SourceBusinessObservation,
			RecordID:   string(binding.ID),
			Hash:       binding.Hash,
			Operation:  operation,
			State:      state,
			Authority:  AuthorityInternalVerified,
			ObservedAt: observation.Body.ObservedAt,
		})
	}
	sources = append(sources,
		SourceRef{
			Role:       RoleCommercialOutcome,
			Kind:       SourceBusinessOutcome,
			RecordID:   string(outcome.Record.Body.ID),
			Version:    outcome.Record.Body.Version,
			Hash:       recordHash,
			Operation:  operation,
			State:      SourceCompleted,
			Authority:  AuthorityIndependentOutcome,
			ObservedAt: outcome.Review.VerifiedAt,
		},
		SourceRef{
			Role:       RoleBusinessGate,
			Kind:       SourceBusinessGate,
			RecordID:   string(requirement.ID),
			Hash:       decision.DecisionHash,
			Operation:  operation,
			State:      SourceSatisfied,
			Authority:  AuthorityIndependentOutcome,
			ObservedAt: decision.EvaluatedAt,
		},
	)
	sort.Slice(sources, func(left, right int) bool { return sourceKey(sources[left]) < sourceKey(sources[right]) })
	proof := MeasurementProof{
		Requirement:  requirement,
		Decision:     decision,
		Outcome:      outcome,
		Observations: observations,
		Sources:      sources,
	}
	if proof.Validate() != nil {
		return MeasurementProof{}, ErrIntegrity
	}
	return proof, nil
}

func measurementOperation(scope Scope) (string, error) {
	for _, binding := range scope.Operations {
		if binding.Phase == PhaseMeasurement && len(binding.Operations) != 0 {
			return binding.Operations[0], nil
		}
	}
	return "", ErrIntegrity
}

func sameGateRequirement(left, right businessoutcome.GateRequirement) bool {
	leftHash, leftErr := contracts.HashCanonical(&left)
	rightHash, rightErr := contracts.HashCanonical(&right)
	return leftErr == nil && rightErr == nil && leftHash == rightHash
}

func sameObservationBindings(left, right []businessoutcome.ObservationBinding) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
