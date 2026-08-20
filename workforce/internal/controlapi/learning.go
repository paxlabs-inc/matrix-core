package controlapi

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/learning"
)

type LearningEvaluationRequest struct {
	HypothesisID string `json:"hypothesis_id"`
}

type LearningConclusionRequest struct {
	HypothesisID        string              `json:"hypothesis_id"`
	EvaluationID        string              `json:"evaluation_id"`
	ReviewID            string              `json:"review_id"`
	NextAction          learning.NextAction `json:"next_action"`
	SupersededRecordIDs []string            `json:"superseded_record_ids"`
	NextReviewAt        time.Time           `json:"next_review_at"`
}

func (service *Service) verifiedLearning(
	ctx context.Context,
	principal Principal,
) (*learning.Store, *learning.Engine, error) {
	if _, _, err := service.currentCompanyAuthority(ctx, principal); err != nil {
		return nil, nil, err
	}
	if service.learningStore == nil || service.learningEngine == nil {
		return nil, nil, fmt.Errorf("controlapi: verified learning is unavailable")
	}
	return service.learningStore, service.learningEngine, nil
}

func (service *Service) RegisterLearningHypothesis(
	ctx context.Context, principal Principal, value learning.Hypothesis,
) (learning.Hypothesis, error) {
	store, _, err := service.verifiedLearning(ctx, principal)
	if err != nil {
		return learning.Hypothesis{}, err
	}
	if _, err := store.RegisterHypothesis(ctx, value); err != nil {
		return learning.Hypothesis{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:learning-hypothesis:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "learning.hypothesis.registered",
		ResourceKind: "learning-hypothesis", ResourceID: value.ID,
		ResourceVersion: value.Version, VerifiedCompletion: false,
		Fields: map[string]any{"initiative_id": value.InitiativeID, "review_at": value.ReviewAt, "state": "open"},
	})
	return value, err
}

func (service *Service) RecordLearningObservation(
	ctx context.Context, principal Principal, value learning.Observation,
) (learning.Observation, error) {
	store, _, err := service.verifiedLearning(ctx, principal)
	if err != nil {
		return learning.Observation{}, err
	}
	if _, err := store.RecordObservation(ctx, value); err != nil {
		return learning.Observation{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:learning-observation:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "learning.observation.committed",
		ResourceKind: "learning-observation", ResourceID: value.ID,
		ResourceVersion: 1, VerifiedCompletion: true,
		Fields: map[string]any{
			"hypothesis_id": value.HypothesisID, "metric_id": value.MetricID,
			"authority": value.Authority, "observed_at": value.ObservedAt,
		},
	})
	return value, err
}

func (service *Service) EvaluateLearning(
	ctx context.Context, principal Principal, request LearningEvaluationRequest,
) (learning.Evaluation, error) {
	if strings.TrimSpace(request.HypothesisID) == "" || len(request.HypothesisID) > 128 {
		return learning.Evaluation{}, fmt.Errorf("controlapi: learning evaluation request is invalid")
	}
	store, engine, err := service.verifiedLearning(ctx, principal)
	if err != nil {
		return learning.Evaluation{}, err
	}
	due, err := store.DueHypothesis(ctx, request.HypothesisID)
	if err != nil {
		return learning.Evaluation{}, err
	}
	observations, err := store.ListObservations(ctx, request.HypothesisID)
	if err != nil {
		return learning.Evaluation{}, err
	}
	contaminated, err := store.ContaminatedEvidence(ctx)
	if err != nil {
		return learning.Evaluation{}, err
	}
	now, err := service.currentTime()
	if err != nil {
		return learning.Evaluation{}, err
	}
	value, err := engine.Evaluate(
		due.Hypothesis, observations, contaminated, due.InconclusiveCount, now,
	)
	if err != nil {
		return learning.Evaluation{}, err
	}
	if _, err := store.CommitEvaluation(ctx, value); err != nil {
		return learning.Evaluation{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:learning-evaluation:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "learning.evaluation.committed",
		ResourceKind: "learning-evaluation", ResourceID: value.ID,
		ResourceVersion: 1, VerifiedCompletion: value.Result != learning.ResultRequiresHuman,
		Fields: map[string]any{"hypothesis_id": value.HypothesisID, "result": value.Result},
	})
	return value, err
}

func (service *Service) CommitLearningReview(
	ctx context.Context, principal Principal, value learning.IndependentReview,
) (learning.IndependentReview, error) {
	store, _, err := service.verifiedLearning(ctx, principal)
	if err != nil {
		return learning.IndependentReview{}, err
	}
	if _, err := store.CommitReview(ctx, value); err != nil {
		return learning.IndependentReview{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:learning-review:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "learning.review.committed",
		ResourceKind: "learning-review", ResourceID: value.ID,
		ResourceVersion: 1, VerifiedCompletion: value.Decision == learning.ReviewApprove,
		Fields: map[string]any{"hypothesis_id": value.HypothesisID, "decision": value.Decision, "auditor_seat_id": value.AuditorSeatID},
	})
	return value, err
}

func (service *Service) ConcludeLearning(
	ctx context.Context, principal Principal, request LearningConclusionRequest,
) (learning.Conclusion, error) {
	if strings.TrimSpace(request.HypothesisID) == "" ||
		strings.TrimSpace(request.EvaluationID) == "" || strings.TrimSpace(request.ReviewID) == "" ||
		!request.NextAction.Valid() || !slices.IsSorted(request.SupersededRecordIDs) ||
		request.NextReviewAt.IsZero() || request.NextReviewAt.Location() != time.UTC {
		return learning.Conclusion{}, fmt.Errorf("controlapi: learning conclusion request is invalid")
	}
	store, engine, err := service.verifiedLearning(ctx, principal)
	if err != nil {
		return learning.Conclusion{}, err
	}
	hypothesis, err := store.LoadHypothesis(ctx, request.HypothesisID)
	if err != nil {
		return learning.Conclusion{}, err
	}
	evaluation, err := store.LoadEvaluation(ctx, request.EvaluationID)
	if err != nil {
		return learning.Conclusion{}, err
	}
	review, err := store.LoadReview(ctx, request.ReviewID)
	if err != nil {
		return learning.Conclusion{}, err
	}
	now, err := service.currentTime()
	if err != nil {
		return learning.Conclusion{}, err
	}
	value, err := engine.Conclude(
		hypothesis, evaluation, review, request.NextAction,
		request.SupersededRecordIDs, request.NextReviewAt, now,
	)
	if err != nil {
		return learning.Conclusion{}, err
	}
	if _, err := store.CommitConclusion(ctx, value); err != nil {
		return learning.Conclusion{}, err
	}
	_, err = service.Publish(ctx, principal, LifecycleEvent{
		ID:             "event:learning-conclusion:" + value.ID,
		OrganizationID: principal.OrganizationID, Type: "learning.conclusion.committed",
		ResourceKind: "learning-conclusion", ResourceID: value.ID,
		ResourceVersion: 1, VerifiedCompletion: value.Result != learning.ResultRequiresHuman,
		Fields: map[string]any{
			"initiative_id": value.InitiativeID, "result": value.Result,
			"next_action": value.NextAction, "next_review_at": value.NextReviewAt,
		},
	})
	return value, err
}
