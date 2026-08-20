package portfolio

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/companystate"
	"centra/workforce/internal/contracts"
)

// Candidate binds one opportunity to its current independent assessment.
type Candidate struct {
	Opportunity Opportunity
	Assessment  Assessment
}

// RankedCandidate is one deterministic portfolio comparison result.
type RankedCandidate struct {
	OpportunityID OpportunityID
	ScoreBPS      uint16
	Decision      DecisionKind
	Reason        string
}

// RankCandidates compares current opportunities, applies resource consumption
// in score order, and uses opportunity identity as the stable final tie-breaker.
func RankCandidates(
	candidates []Candidate,
	procedure DecisionProcedure,
	context EvaluationContext,
	now time.Time,
) ([]RankedCandidate, error) {
	if len(candidates) == 0 || len(candidates) > 256 {
		return nil, fmt.Errorf("portfolio: candidate set must contain 1 to 256 opportunities")
	}
	type scored struct {
		candidate Candidate
		score     uint16
	}
	values := make([]scored, 0, len(candidates))
	seen := make(map[OpportunityID]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, exists := seen[candidate.Opportunity.ID]; exists {
			return nil, fmt.Errorf("portfolio: candidate opportunity is duplicated")
		}
		seen[candidate.Opportunity.ID] = struct{}{}
		if err := candidate.Opportunity.Validate(); err != nil {
			return nil, err
		}
		if err := candidate.Assessment.Validate(); err != nil {
			return nil, err
		}
		values = append(values, scored{
			candidate: candidate,
			score:     weightedScore(candidate.Assessment.Scores, procedure.Weights),
		})
	}
	slices.SortFunc(values, func(left, right scored) int {
		if left.score != right.score {
			return int(right.score) - int(left.score)
		}
		return strings.Compare(string(left.candidate.Opportunity.ID), string(right.candidate.Opportunity.ID))
	})
	result := make([]RankedCandidate, 0, len(values))
	current := context
	for _, value := range values {
		receipt, err := Evaluate(value.candidate.Opportunity, value.candidate.Assessment, procedure, current, now)
		if err != nil {
			return nil, err
		}
		result = append(result, RankedCandidate{
			OpportunityID: value.candidate.Opportunity.ID,
			ScoreBPS:      receipt.ScoreBPS,
			Decision:      receipt.Decision,
			Reason:        receipt.Reason,
		})
		if receipt.Decision == DecisionGO {
			current.ActiveInitiatives++
			current.AllocatedCapitalMicrounits += receipt.CapitalImpactMicrounits
			current.AllocatedRiskMicrounits += receipt.RiskImpactMicrounits
		}
	}
	return result, nil
}

// Evaluate applies a versioned procedure to current evidence and resource state.
// It has no model, storage, clock, or external-effect dependency.
func Evaluate(
	opportunity Opportunity,
	assessment Assessment,
	procedure DecisionProcedure,
	context EvaluationContext,
	now time.Time,
) (DecisionReceipt, error) {
	if err := opportunity.Validate(); err != nil {
		return DecisionReceipt{}, err
	}
	if err := assessment.Validate(); err != nil {
		return DecisionReceipt{}, err
	}
	if err := procedure.Validate(); err != nil {
		return DecisionReceipt{}, err
	}
	if !validUTC(now) || opportunity.OrganizationID != procedure.OrganizationID ||
		opportunity.ExpiresAt.Before(now) || assessment.AssessedAt.After(now) ||
		procedure.EffectiveAt.After(now) || procedure.ExpiresAt != nil && !procedure.ExpiresAt.After(now) {
		return DecisionReceipt{}, fmt.Errorf("portfolio: opportunity, assessment, procedure, or clock is not current")
	}
	score := weightedScore(assessment.Scores, procedure.Weights)
	decision, reason := selectDecision(opportunity, assessment, procedure, context, score)
	thresholds := []string{
		fmt.Sprintf("go_score_bps:%d", procedure.GOThresholdBPS),
		fmt.Sprintf("legal_safety_bps:%d", procedure.MinimumLegalSafetyBPS),
		fmt.Sprintf("no_go_score_bps:%d", procedure.NO_GOThresholdBPS),
		fmt.Sprintf("security_safety_bps:%d", procedure.MinimumSecuritySafetyBPS),
		fmt.Sprintf("validate_score_bps:%d", procedure.ValidateThresholdBPS),
	}
	slices.Sort(thresholds)
	receipt := DecisionReceipt{
		SchemaVersion:    DecisionSchemaVersion,
		OrganizationID:   opportunity.OrganizationID,
		OpportunityID:    opportunity.ID,
		ProcedureID:      procedure.ID,
		ProcedureVersion: procedure.Version,
		Decision:         decision,
		ScoreBPS:         score,
		Alternatives: []Alternative{{
			OpportunityID: opportunity.ID, ScoreBPS: score, Disposition: decision,
		}},
		Evidence:             append([]companystate.RecordReference(nil), assessment.Evidence...),
		Thresholds:           thresholds,
		Dissent:              append([]string(nil), assessment.Dissent...),
		AuthorityClauses:     append([]string(nil), procedure.AuthorityClauses...),
		RiskImpactMicrounits: assessment.RiskMicrounits,
		UnresolvedRisks:      append([]string(nil), assessment.UnresolvedRisks...),
		Reason:               reason,
		CreatedAt:            now,
		NextReviewAt:         nextReview(decision, opportunity, now),
		Signature:            contracts.Signature{},
	}
	if decision == DecisionGO {
		initiativeID := InitiativeID("initiative:" + string(opportunity.ID))
		receipt.InitiativeID = &initiativeID
		receipt.CapitalImpactMicrounits = opportunity.EstimatedCapitalMicrounits
	}
	return receipt, nil
}

func weightedScore(scores FactorScores, weights FactorWeights) uint16 {
	certainty := uint16(10_000 - scores.UncertaintyBPS)
	weighted := uint64(scores.MissionFitBPS)*uint64(weights.MissionFit) +
		uint64(scores.DemandStrengthBPS)*uint64(weights.DemandStrength) +
		uint64(scores.ExpectedValueBPS)*uint64(weights.ExpectedValue) +
		uint64(scores.UnitEconomicsBPS)*uint64(weights.UnitEconomics) +
		uint64(scores.TimeToEvidenceBPS)*uint64(weights.TimeToEvidence) +
		uint64(scores.CapitalEfficiencyBPS)*uint64(weights.CapitalEfficiency) +
		uint64(scores.OpportunityCostBPS)*uint64(weights.OpportunityCost) +
		uint64(scores.LegalSafetyBPS)*uint64(weights.LegalSafety) +
		uint64(scores.SecuritySafetyBPS)*uint64(weights.SecuritySafety) +
		uint64(scores.OperatingCapacityBPS)*uint64(weights.OperatingCapacity) +
		uint64(certainty)*uint64(weights.Certainty)
	return uint16(weighted / 10_000)
}

func selectDecision(
	opportunity Opportunity,
	assessment Assessment,
	procedure DecisionProcedure,
	context EvaluationContext,
	score uint16,
) (DecisionKind, string) {
	switch {
	case context.Contaminated:
		return DecisionPause, "Material input is contaminated and requires reconciliation"
	case assessment.Scores.LegalSafetyBPS < procedure.MinimumLegalSafetyBPS:
		return DecisionReject, "Legal safety is below the signed minimum"
	case assessment.Scores.SecuritySafetyBPS < procedure.MinimumSecuritySafetyBPS:
		return DecisionReject, "Security safety is below the signed minimum"
	case assessment.RiskMicrounits > procedure.MaximumRiskMicrounits ||
		context.AllocatedRiskMicrounits > procedure.MaximumRiskMicrounits-assessment.RiskMicrounits:
		return DecisionEscalate, "Aggregate risk would exceed delegated authority"
	case opportunity.EstimatedCapitalMicrounits > procedure.MaximumCapitalMicrounits ||
		context.AllocatedCapitalMicrounits > procedure.MaximumCapitalMicrounits-opportunity.EstimatedCapitalMicrounits:
		return DecisionEscalate, "Aggregate capital would exceed delegated authority"
	case context.ActiveInitiatives >= procedure.MaximumActiveInitiatives:
		return DecisionDefer, "Active initiative capacity is exhausted"
	case context.ConsecutiveNoEvidenceCycles >= procedure.MaximumNoEvidenceCycles:
		return DecisionEscalate, "Repeated no-evidence work reached the signed limit"
	case score >= procedure.GOThresholdBPS:
		return DecisionGO, "Score and all signed safety and resource thresholds permit funding"
	case score >= procedure.ValidateThresholdBPS:
		return DecisionValidate, "Evidence warrants a bounded validation experiment"
	case score <= procedure.NO_GOThresholdBPS:
		return DecisionNO_GO, "Evidence does not meet the signed continuation threshold"
	default:
		return DecisionDefer, "Current evidence is insufficient for GO or NO_GO"
	}
}

func nextReview(decision DecisionKind, opportunity Opportunity, now time.Time) time.Time {
	days := opportunity.TimeToEvidenceDays
	if decision == DecisionNO_GO || decision == DecisionReject || decision == DecisionTerminate {
		days = 90
	}
	if decision == DecisionEscalate || decision == DecisionPause {
		days = 1
	}
	if days == 0 {
		days = 7
	}
	return now.Add(time.Duration(days) * 24 * time.Hour)
}
