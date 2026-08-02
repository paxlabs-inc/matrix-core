package controlapi

import (
	"errors"
	"net/http"

	"matrix/workforce/internal/businessoutcome"
)

func writeBusinessOutcomeError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, businessoutcome.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, businessoutcome.ErrConflict),
		errors.Is(err, businessoutcome.ErrStale),
		errors.Is(err, businessoutcome.ErrReconciliationRequired),
		errors.Is(err, businessoutcome.ErrContaminated):
		status = http.StatusConflict
	case errors.Is(err, businessoutcome.ErrNotFound):
		status = http.StatusNotFound
	}
	writeError(writer, status, err.Error())
}

func (service *Service) handleBusinessMetric(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[businessoutcome.MetricDefinition](writer, request)
	if !ok {
		return
	}
	result, err := service.RegisterBusinessMetric(request.Context(), principalFrom(request), value)
	if err != nil {
		writeBusinessOutcomeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleBusinessObservation(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[businessoutcome.Observation](writer, request)
	if !ok {
		return
	}
	result, err := service.CommitBusinessObservation(request.Context(), principalFrom(request), value)
	if err != nil {
		writeBusinessOutcomeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleBusinessOutcome(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[businessoutcome.VerifiedOutcome](writer, request)
	if !ok {
		return
	}
	result, err := service.CommitBusinessOutcome(request.Context(), principalFrom(request), value)
	if err != nil {
		writeBusinessOutcomeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleBusinessGate(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[businessoutcome.GateRequirement](writer, request)
	if !ok {
		return
	}
	result, err := service.EvaluateBusinessGate(request.Context(), principalFrom(request), value)
	if err != nil {
		writeBusinessOutcomeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleBusinessLineage(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[businessoutcome.LineageEdge](writer, request)
	if !ok {
		return
	}
	result, err := service.CommitBusinessLineage(request.Context(), principalFrom(request), value)
	if err != nil {
		writeBusinessOutcomeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleBusinessCorrection(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[businessoutcome.Correction](writer, request)
	if !ok {
		return
	}
	result, err := service.ApplyBusinessCorrection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeBusinessOutcomeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleBusinessCorrectionResolution(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[businessoutcome.CorrectionResolution](writer, request)
	if !ok {
		return
	}
	result, err := service.ResolveBusinessCorrection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeBusinessOutcomeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}
