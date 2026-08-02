package controlapi

import (
	"errors"
	"net/http"

	"matrix/workforce/internal/learning"
)

func writeLearningError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, learning.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, learning.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, learning.ErrNotFound):
		status = http.StatusNotFound
	}
	writeError(writer, status, err.Error())
}

func (service *Service) handleLearningHypothesis(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeSecurityCommand[learning.Hypothesis](w, r)
	if !ok {
		return
	}
	result, err := service.RegisterLearningHypothesis(r.Context(), principalFrom(r), v)
	if err != nil {
		writeLearningError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (service *Service) handleLearningObservation(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeSecurityCommand[learning.Observation](w, r)
	if !ok {
		return
	}
	result, err := service.RecordLearningObservation(r.Context(), principalFrom(r), v)
	if err != nil {
		writeLearningError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (service *Service) handleLearningEvaluation(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeSecurityCommand[LearningEvaluationRequest](w, r)
	if !ok {
		return
	}
	result, err := service.EvaluateLearning(r.Context(), principalFrom(r), v)
	if err != nil {
		writeLearningError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (service *Service) handleLearningReview(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeSecurityCommand[learning.IndependentReview](w, r)
	if !ok {
		return
	}
	result, err := service.CommitLearningReview(r.Context(), principalFrom(r), v)
	if err != nil {
		writeLearningError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (service *Service) handleLearningConclusion(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeSecurityCommand[LearningConclusionRequest](w, r)
	if !ok {
		return
	}
	result, err := service.ConcludeLearning(r.Context(), principalFrom(r), v)
	if err != nil {
		writeLearningError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}
