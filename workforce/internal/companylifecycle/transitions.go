package companylifecycle

import (
	"errors"
	"fmt"
	"math"
	"time"

	"matrix/workforce/internal/contracts"
)

var (
	// ErrConflict reports optimistic-version, identity, or idempotency conflict.
	ErrConflict = errors.New("company lifecycle: conflict")
	// ErrInvalidTransition reports an edge outside the closed lifecycle graph.
	ErrInvalidTransition = errors.New("company lifecycle: invalid transition")
	// ErrUnauthorized reports missing, expired, or revoked transition authority.
	ErrUnauthorized = errors.New("company lifecycle: unauthorized")
	// ErrEvidence reports a missing, stale, invalid, or unverified gate input.
	ErrEvidence = errors.New("company lifecycle: evidence gate failed")
	// ErrContaminated reports unresolved material correction contamination.
	ErrContaminated = errors.New("company lifecycle: evidence is contaminated")
	// ErrBudget reports an exact capital or transition-budget violation.
	ErrBudget = errors.New("company lifecycle: capital boundary exceeded")
	// ErrTerminal reports an attempted transition from TERMINATE.
	ErrTerminal = errors.New("company lifecycle: initiative is terminal")
	// ErrIntegrity reports a durable hash, seal, or signature mismatch.
	ErrIntegrity = errors.New("company lifecycle: integrity failure")
	// ErrEffectAmbiguous reports an effect that must be reconciled before use.
	ErrEffectAmbiguous = errors.New("company lifecycle: external effect is ambiguous")
)

func allowedTransition(from, to State, decision Decision, resume State) bool {
	if from == StateTerminate {
		return false
	}
	if decision == DecisionPause {
		return from != StatePaused && to == StatePaused
	}
	if from == StatePaused {
		if decision == DecisionResume {
			return to == resume && resume.Valid() && resume != StatePaused && resume != StateTerminate
		}
		return decision == DecisionTerminate && to == StateTerminate
	}
	if decision == DecisionTerminate {
		return to == StateTerminate
	}
	switch from {
	case StateDiscover:
		return to == StateScreen && decision == DecisionAdvance
	case StateScreen:
		return to == StateValidate && decision == DecisionAdvance
	case StateValidate:
		return to == StateDecide && decision == DecisionAdvance
	case StateDecide:
		return (to == StateFund && decision == DecisionGo) ||
			(to == StateTerminate && decision == DecisionNoGo)
	case StateFund:
		return to == StateDesign && decision == DecisionAdvance
	case StateDesign:
		return to == StateBuild && decision == DecisionAdvance
	case StateBuild:
		return to == StateVerify && decision == DecisionAdvance
	case StateVerify:
		return to == StateLaunch && decision == DecisionAdvance
	case StateLaunch:
		return to == StateAcquire && decision == DecisionAdvance
	case StateAcquire:
		return to == StateMonetize && decision == DecisionAdvance
	case StateMonetize:
		return to == StateOperate && decision == DecisionAdvance
	case StateOperate:
		return to == StateMeasure && decision == DecisionAdvance
	case StateMeasure:
		return (to == StateScale && decision == DecisionScale) ||
			(to == StatePivot && decision == DecisionPivot) ||
			(to == StateMaintain && decision == DecisionMaintain) ||
			(to == StateTerminate && decision == DecisionTerminate)
	case StateScale, StatePivot, StateMaintain:
		return to == StateDiscover && decision == DecisionAdvance
	default:
		return false
	}
}

// TransitionAllowed reports whether an edge and decision are part of the
// closed lifecycle graph. resume is consulted only for a PAUSED checkpoint.
func TransitionAllowed(from, to State, decision Decision, resume State) bool {
	if from == "" {
		return to == StateDiscover && decision == DecisionInitialize
	}
	return allowedTransition(from, to, decision, resume)
}

func requiredEvidence(from, to State, decision Decision) []EvidenceKind {
	if decision == DecisionPause {
		return []EvidenceKind{EvidencePauseCondition}
	}
	if decision == DecisionResume {
		return []EvidenceKind{EvidenceIndependentReview, EvidenceResumeCondition}
	}
	if to == StateTerminate {
		if decision == DecisionNoGo {
			return []EvidenceKind{EvidenceGoNoGoDecision, EvidenceIndependentReview, EvidenceRiskReview}
		}
		if from == StateMeasure {
			return []EvidenceKind{
				EvidenceIndependentReview, EvidencePortfolioDecision,
				EvidenceTerminationDecision, EvidenceThresholdEvaluation,
				EvidenceVerifiedOutcome,
			}
		}
		return []EvidenceKind{EvidenceIndependentReview, EvidenceTerminationDecision}
	}
	switch {
	case from == "" && to == StateDiscover:
		return []EvidenceKind{EvidenceOpportunity}
	case from == StateDiscover && to == StateScreen:
		return []EvidenceKind{EvidenceDemandSignal, EvidenceTargetCustomer}
	case from == StateScreen && to == StateValidate:
		return []EvidenceKind{
			EvidenceCompetitorAnalysis, EvidenceConstitutionScreening,
			EvidenceLegalScreening, EvidenceMarketAnalysis,
		}
	case from == StateValidate && to == StateDecide:
		return []EvidenceKind{
			EvidenceEconomicModel, EvidenceExperiment, EvidenceHypothesis,
			EvidenceIndependentReview, EvidenceRiskReview,
		}
	case from == StateDecide && to == StateFund:
		return []EvidenceKind{EvidenceCapitalAllocation, EvidenceGoNoGoDecision, EvidenceIndependentReview}
	case from == StateFund && to == StateDesign:
		return []EvidenceKind{EvidenceCustomerProblem, EvidenceRequirements, EvidenceUserJourney}
	case from == StateDesign && to == StateBuild:
		return []EvidenceKind{EvidenceImplementationPlan}
	case from == StateBuild && to == StateVerify:
		return []EvidenceKind{EvidenceDeploymentState, EvidenceSourceState}
	case from == StateVerify && to == StateLaunch:
		return []EvidenceKind{
			EvidenceClaims, EvidenceIndependentReview, EvidenceLaunchReadiness,
			EvidenceLegal, EvidenceOperationsReadiness, EvidencePricing,
			EvidenceQuality, EvidenceSecurity,
		}
	case from == StateLaunch && to == StateAcquire:
		return []EvidenceKind{EvidenceDistribution}
	case from == StateAcquire && to == StateMonetize:
		return []EvidenceKind{EvidenceCustomer, EvidenceSales, EvidenceTransaction}
	case from == StateMonetize && to == StateOperate:
		return []EvidenceKind{EvidenceCost, EvidenceProductUsage, EvidenceRevenue, EvidenceSupport}
	case from == StateOperate && to == StateMeasure:
		return []EvidenceKind{
			EvidenceRetention, EvidenceRiskObservation, EvidenceVerifiedOutcome,
		}
	case from == StateMeasure:
		return []EvidenceKind{
			EvidenceIndependentReview, EvidencePortfolioDecision,
			EvidenceThresholdEvaluation, EvidenceVerifiedOutcome,
		}
	case (from == StateScale || from == StatePivot || from == StateMaintain) && to == StateDiscover:
		return []EvidenceKind{EvidenceNextCycleDecision}
	default:
		return nil
	}
}

// RequiredEvidenceFor returns the complete gate evidence kinds for one closed
// lifecycle edge. An empty result means the edge is not executable.

func RequiredEvidenceFor(from, to State, decision Decision, resume State) []EvidenceKind {
	if !TransitionAllowed(from, to, decision, resume) {
		return nil
	}
	return append([]EvidenceKind(nil), requiredEvidence(from, to, decision)...)
}

func verifyGateInputs(
	now time.Time,
	transitionID TransitionID,
	organizationID contracts.OrganizationID,
	initiativeID InitiativeID,
	authority AuthorityBinding,
	companyState CompanyStateBinding,
	correction CorrectionBinding,
	evidence []EvidenceBinding,
	impact CapitalImpact,
	required []EvidenceKind,
	grant GateVerificationGrant,
) error {
	if authority.OrganizationID != organizationID ||
		companyState.OrganizationID != organizationID ||
		correction.OrganizationID != organizationID {
		return fmt.Errorf("%w: gate binding belongs to another organization", ErrUnauthorized)
	}
	if !authority.ExpiresAt.After(now) {
		return fmt.Errorf("%w: authority expired", ErrUnauthorized)
	}
	if companyState.ObservedAt.After(now) || correction.CheckedAt.After(now) {
		return fmt.Errorf("%w: future gate snapshot", ErrEvidence)
	}
	if correction.UnresolvedMaterialCount != 0 || correction.UnresolvedContaminatedCount != 0 {
		return ErrContaminated
	}
	latestInput := companyState.ObservedAt
	if correction.CheckedAt.After(latestInput) {
		latestInput = correction.CheckedAt
	}
	present := make(map[EvidenceKind]EvidenceBinding, len(evidence))
	for _, item := range evidence {
		if item.Validity != contracts.ValidityActive || item.Contaminated {
			return fmt.Errorf("%w: %s is not active and clean", ErrContaminated, item.ID)
		}
		if !item.FreshUntil.After(now) || item.ObservedAt.After(now) || item.EffectiveAt.After(now) {
			return fmt.Errorf("%w: %s is stale or future-dated", ErrEvidence, item.ID)
		}
		if item.EffectiveAt.After(latestInput) {
			latestInput = item.EffectiveAt
		}
		if _, exists := present[item.Kind]; !exists {
			present[item.Kind] = item
		}
	}
	for _, kind := range required {
		item, exists := present[kind]
		if !exists {
			return fmt.Errorf("%w: required %s is missing", ErrEvidence, kind)
		}
		if kind == EvidenceIndependentReview && item.IndependentVerdictID == nil {
			return fmt.Errorf("%w: independent review lacks a verdict", ErrEvidence)
		}
	}
	if err := grant.Validate(); err != nil {
		return fmt.Errorf("%w: %v", ErrUnauthorized, err)
	}
	if grant.TransitionID != transitionID || grant.OrganizationID != organizationID ||
		grant.InitiativeID != initiativeID || grant.AuthorityClauseID != authority.ClauseID ||
		grant.VerifiedAt.After(now) || grant.VerifiedAt.Before(latestInput) ||
		!grant.ExpiresAt.After(now) || grant.ExpiresAt.After(authority.ExpiresAt) {
		return fmt.Errorf("%w: verification grant scope or time is invalid", ErrUnauthorized)
	}
	authorityHash, err := contracts.HashCanonical(authority)
	if err != nil {
		return err
	}
	companyHash, err := contracts.HashCanonical(companyState)
	if err != nil {
		return err
	}
	correctionHash, err := contracts.HashCanonical(correction)
	if err != nil {
		return err
	}
	evidenceHash, err := hashEvidenceSet(evidence)
	if err != nil {
		return err
	}
	impactHash, err := contracts.HashCanonical(impact)
	if err != nil {
		return err
	}
	if grant.AuthorityBindingHash != authorityHash || grant.CompanyStateHash != companyHash ||
		grant.CorrectionBindingHash != correctionHash || grant.EvidenceSetHash != evidenceHash ||
		grant.CapitalImpactHash != impactHash {
		return fmt.Errorf("%w: verification grant does not bind the request", ErrUnauthorized)
	}
	if grant.Limits.Currency != impact.Currency ||
		grant.Limits.CapitalEnvelopeVersion != impact.CapitalEnvelopeVersion ||
		grant.Limits.CapitalEnvelopeHash != impact.CapitalEnvelopeHash ||
		impact.CapitalEnvelopeVersion != authority.CapitalEnvelopeVersion ||
		impact.CapitalEnvelopeHash != authority.CapitalEnvelopeHash {
		return fmt.Errorf("%w: capital authority binding drift", ErrBudget)
	}
	return nil
}

func applyCapital(before CapitalSnapshot, impact CapitalImpact, limits CapitalLimits, destination State) (CapitalSnapshot, error) {
	if before.Currency != impact.Currency ||
		before.CapitalEnvelopeVersion != impact.CapitalEnvelopeVersion ||
		before.CapitalEnvelopeHash != impact.CapitalEnvelopeHash ||
		limits.Currency != impact.Currency ||
		limits.CapitalEnvelopeVersion != impact.CapitalEnvelopeVersion ||
		limits.CapitalEnvelopeHash != impact.CapitalEnvelopeHash ||
		impact.TransitionBudgetMicrounits > limits.MaxTransitionBudgetMicrounits {
		return CapitalSnapshot{}, ErrBudget
	}
	allocated, ok := checkedAdd(before.AllocatedMicrounits, impact.AllocateMicrounits)
	if !ok || before.ConsumedAllocationMicrounits > allocated {
		return CapitalSnapshot{}, ErrBudget
	}
	available := allocated - before.ConsumedAllocationMicrounits
	if impact.AllocatedSpendMicrounits > available {
		return CapitalSnapshot{}, ErrBudget
	}
	consumed, ok := checkedAdd(before.ConsumedAllocationMicrounits, impact.AllocatedSpendMicrounits)
	if !ok {
		return CapitalSnapshot{}, ErrBudget
	}
	remaining := allocated - consumed
	if impact.ReleaseMicrounits > remaining {
		return CapitalSnapshot{}, ErrBudget
	}
	allocated -= impact.ReleaseMicrounits
	spent, ok := checkedAdd(before.SpentMicrounits, impact.SpendMicrounits)
	if !ok {
		return CapitalSnapshot{}, ErrBudget
	}
	grossExposure, ok := checkedAdd(before.ExposureMicrounits, impact.ExposureIncreaseMicrounits)
	if !ok || impact.ExposureReleaseMicrounits > grossExposure {
		return CapitalSnapshot{}, ErrBudget
	}
	exposure := grossExposure - impact.ExposureReleaseMicrounits
	revenue, ok := checkedAdd(before.RecognizedRevenueMicrounits, impact.RecognizedRevenueMicrounits)
	if !ok {
		return CapitalSnapshot{}, ErrBudget
	}
	if destination == StateFund && impact.AllocateMicrounits == 0 {
		return CapitalSnapshot{}, fmt.Errorf("%w: FUND requires exact non-zero allocation", ErrBudget)
	}
	if destination == StateTerminate {
		if impact.AllocateMicrounits != 0 || impact.ReleaseMicrounits != remaining ||
			impact.ExposureReleaseMicrounits != grossExposure {
			return CapitalSnapshot{}, fmt.Errorf("%w: TERMINATE must release all unconsumed capital and exposure", ErrBudget)
		}
	}
	if allocated > limits.MaxResultingAllocationMicrounits ||
		spent > limits.MaxResultingSpendMicrounits ||
		exposure > limits.MaxResultingExposureMicrounits {
		return CapitalSnapshot{}, ErrBudget
	}
	return CapitalSnapshot{
		SchemaVersion:                  contracts.SchemaVersionV1,
		Currency:                       impact.Currency,
		CapitalEnvelopeVersion:         impact.CapitalEnvelopeVersion,
		CapitalEnvelopeHash:            impact.CapitalEnvelopeHash,
		AllocatedMicrounits:            allocated,
		ConsumedAllocationMicrounits:   consumed,
		SpentMicrounits:                spent,
		ExposureMicrounits:             exposure,
		RecognizedRevenueMicrounits:    revenue,
		LastTransitionBudgetMicrounits: impact.TransitionBudgetMicrounits,
	}, nil
}

func initialCapital(impact CapitalImpact) CapitalSnapshot {
	return CapitalSnapshot{
		SchemaVersion:          contracts.SchemaVersionV1,
		Currency:               impact.Currency,
		CapitalEnvelopeVersion: impact.CapitalEnvelopeVersion,
		CapitalEnvelopeHash:    impact.CapitalEnvelopeHash,
	}
}

func checkedAdd(left, right uint64) (uint64, bool) {
	if right > math.MaxUint64-left {
		return 0, false
	}
	return left + right, true
}

func hashEvidenceSet(evidence []EvidenceBinding) (contracts.ContentHash, error) {
	return contracts.HashCanonical(evidenceHashInput{
		SchemaVersion: contracts.SchemaVersionV1,
		Evidence:      evidence,
	})
}

// EvidenceSetHash returns the canonical digest a GateVerifier must bind into
// its GateVerificationGrant.
func EvidenceSetHash(evidence []EvidenceBinding) (contracts.ContentHash, error) {
	return hashEvidenceSet(evidence)
}

type evidenceHashInput struct {
	SchemaVersion string            `json:"schema_version"`
	Evidence      []EvidenceBinding `json:"evidence"`
}

func (value evidenceHashInput) Validate() error {
	if value.SchemaVersion != contracts.SchemaVersionV1 || len(value.Evidence) == 0 {
		return fmt.Errorf("company lifecycle: evidence set is invalid")
	}
	previous := ""
	for _, item := range value.Evidence {
		if err := item.Validate(); err != nil {
			return err
		}
		key := string(item.Kind) + "\x00" + string(item.ID)
		if key <= previous {
			return fmt.Errorf("company lifecycle: evidence set is not canonical")
		}
		previous = key
	}
	return nil
}
