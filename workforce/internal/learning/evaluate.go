package learning

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"centra/workforce/internal/contracts"
)

type Engine struct {
	keyID string
	key   ed25519.PrivateKey
}

func NewEngine(keyID string, key ed25519.PrivateKey) (*Engine, error) {
	if token(keyID) != nil || len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("learning: runtime signing authority is required")
	}
	return &Engine{keyID: keyID, key: append(ed25519.PrivateKey(nil), key...)}, nil
}

func (engine *Engine) Evaluate(
	hypothesis Hypothesis,
	observations []Observation,
	contaminatedEvidence map[contracts.EvidenceID]bool,
	priorInconclusive uint16,
	now time.Time,
) (Evaluation, error) {
	if engine == nil || hypothesis.Validate() != nil || !utc(now) ||
		now.Before(hypothesis.ReviewAt) {
		return Evaluation{}, fmt.Errorf("learning: hypothesis is not due for evaluation")
	}
	hypothesisHash, err := contracts.HashCanonical(&hypothesis)
	if err != nil {
		return Evaluation{}, err
	}
	byMetric := make(map[string][]Observation, len(hypothesis.MetricThresholds))
	for _, observation := range observations {
		if observation.OrganizationID != hypothesis.OrganizationID ||
			observation.InitiativeID != hypothesis.InitiativeID ||
			observation.HypothesisID != hypothesis.ID ||
			observation.ValidateAt(now) != nil ||
			contaminatedEvidence[observation.Evidence.ID] ||
			observation.SourceID == hypothesis.ID {
			continue
		}
		byMetric[observation.MetricID] = append(byMetric[observation.MetricID], observation)
	}
	results := make([]MetricResult, 0, len(hypothesis.MetricThresholds))
	overall := ResultSucceeded
	for _, threshold := range hypothesis.MetricThresholds {
		values := byMetric[threshold.MetricID]
		sort.Slice(values, func(left, right int) bool {
			if values[left].ObservedAt.Equal(values[right].ObservedAt) {
				return values[left].ID < values[right].ID
			}
			return values[left].ObservedAt.After(values[right].ObservedAt)
		})
		metricResult := MetricResult{MetricID: threshold.MetricID, Result: ResultInconclusive}
		if len(values) > 0 {
			selected := values[0]
			metricResult.Value = selected.Value
			metricResult.ObservationID = selected.ID
			metricResult.EvidenceHash = selected.Evidence.Hash
			conflict := len(values) > 1 && values[1].ObservedAt == selected.ObservedAt &&
				values[1].Value != selected.Value
			missingDenominator := threshold.DenominatorMetric != "" &&
				(selected.Denominator == nil || *selected.Denominator <= 0)
			if conflict || missingDenominator {
				metricResult.Result = ResultRequiresHuman
			} else {
				metricResult.Result = compare(threshold, selected.Value)
			}
		}
		results = append(results, metricResult)
		switch metricResult.Result {
		case ResultRequiresHuman:
			overall = ResultRequiresHuman
		case ResultFailed:
			if overall != ResultRequiresHuman {
				overall = ResultFailed
			}
		case ResultInconclusive:
			if overall == ResultSucceeded {
				overall = ResultInconclusive
			}
		}
	}
	if overall == ResultInconclusive && (priorInconclusive >= 2 ||
		!now.Before(hypothesis.MaximumDurationAt)) {
		overall = ResultRequiresHuman
	}
	evaluation := Evaluation{
		SchemaVersion:  EvaluationSchemaVersion,
		ID:             derivedID("evaluation", hypothesis.ID, fmt.Sprint(now.Unix())),
		OrganizationID: hypothesis.OrganizationID, InitiativeID: hypothesis.InitiativeID,
		HypothesisID: hypothesis.ID, HypothesisHash: hypothesisHash,
		Results: results, Result: overall, EvaluatedAt: now,
	}
	if err := SignEvaluation(&evaluation, engine.keyID, engine.key); err != nil {
		return Evaluation{}, err
	}
	return evaluation, nil
}

func (engine *Engine) Conclude(
	hypothesis Hypothesis,
	evaluation Evaluation,
	review IndependentReview,
	action NextAction,
	superseded []string,
	nextReviewAt time.Time,
	now time.Time,
) (Conclusion, error) {
	if engine == nil || hypothesis.Validate() != nil || evaluation.Validate() != nil ||
		review.Validate() != nil || !utc(now) || review.AuditorSeatID == hypothesis.RegistrarSeatID ||
		evaluation.OrganizationID != hypothesis.OrganizationID ||
		evaluation.InitiativeID != hypothesis.InitiativeID ||
		evaluation.HypothesisID != hypothesis.ID || review.EvaluationID != evaluation.ID ||
		review.HypothesisID != hypothesis.ID || !review.ReviewedAt.After(evaluation.EvaluatedAt) {
		return Conclusion{}, fmt.Errorf("learning: conclusion lineage or segregation is invalid")
	}
	evaluationHash, err := contracts.HashCanonical(&evaluation)
	if err != nil || evaluationHash != review.EvaluationHash {
		return Conclusion{}, fmt.Errorf("learning: review does not bind the exact evaluation")
	}
	result := evaluation.Result
	if review.Decision == ReviewRequiresHuman || review.Decision == ReviewReject {
		result, action = ResultRequiresHuman, ActionHumanReview
	}
	if !permittedAction(result, action) {
		return Conclusion{}, fmt.Errorf("learning: next action is not permitted by the verified result")
	}
	conclusion := Conclusion{
		SchemaVersion:  ConclusionSchemaVersion,
		ID:             derivedID("conclusion", hypothesis.ID, fmt.Sprint(now.Unix())),
		OrganizationID: hypothesis.OrganizationID, InitiativeID: hypothesis.InitiativeID,
		HypothesisID: hypothesis.ID, EvaluationID: evaluation.ID, ReviewID: review.ID,
		Result: result, NextAction: action,
		SupersededRecordIDs: append([]string(nil), superseded...),
		PortfolioFeedbackID: derivedID("portfolio-feedback", hypothesis.ID, fmt.Sprint(now.Unix())),
		NextReviewAt:        nextReviewAt, CommittedAt: now,
	}
	if err := SignConclusion(&conclusion, engine.keyID, engine.key); err != nil {
		return Conclusion{}, err
	}
	return conclusion, nil
}

func compare(threshold MetricThreshold, value int64) Result {
	switch threshold.Comparator {
	case ComparatorAtLeast:
		if value >= threshold.SuccessValue {
			return ResultSucceeded
		}
		if value <= threshold.StopValue {
			return ResultFailed
		}
	case ComparatorAtMost:
		if value <= threshold.SuccessValue {
			return ResultSucceeded
		}
		if value >= threshold.StopValue {
			return ResultFailed
		}
	}
	return ResultInconclusive
}

func permittedAction(result Result, action NextAction) bool {
	switch result {
	case ResultSucceeded:
		return action == ActionScale || action == ActionMaintain || action == ActionDiscover
	case ResultFailed:
		return action == ActionPivot || action == ActionTerminate || action == ActionDiscover
	case ResultInconclusive:
		return action == ActionMaintain || action == ActionDiscover
	case ResultRequiresHuman:
		return action == ActionHumanReview
	default:
		return false
	}
}

func derivedID(kind string, values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return kind + ":" + hex.EncodeToString(hash.Sum(nil))
}
