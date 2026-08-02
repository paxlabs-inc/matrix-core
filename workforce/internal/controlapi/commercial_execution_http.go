package controlapi

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"matrix/workforce/internal/commercialexecution"
)

func (service *Service) handleCommercialExecutionStart(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCommercialRequest[commercialexecution.Plan](writer, request, "commercial execution plan")
	if !ok {
		return
	}
	result, err := service.StartCommercialExecution(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	status := http.StatusOK
	if result.Changed {
		status = http.StatusCreated
	}
	writeJSON(writer, status, result)
}

func (service *Service) handleCommercialExecutionEvidence(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCommercialRequest[commercialexecution.Evidence](writer, request, "commercial execution evidence")
	if !ok {
		return
	}
	result, err := service.RecordCommercialEvidence(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleCommercialExecutionCorrection(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCommercialRequest[commercialexecution.Correction](writer, request, "commercial execution correction")
	if !ok {
		return
	}
	result, err := service.CorrectCommercialExecution(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleCommercialExecutionRecovery(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCommercialRequest[commercialexecution.Recovery](writer, request, "commercial execution recovery")
	if !ok {
		return
	}
	result, err := service.RecoverCommercialExecution(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleCommercialExecution(writer http.ResponseWriter, request *http.Request) {
	id := commercialexecution.ExecutionID(strings.TrimSpace(request.PathValue("execution_id")))
	result, err := service.CommercialExecution(request.Context(), principalFrom(request), id)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleCommercialMeasurement(writer http.ResponseWriter, request *http.Request) {
	id := commercialexecution.ExecutionID(strings.TrimSpace(request.PathValue("execution_id")))
	result, err := service.CommercialMeasurement(request.Context(), principalFrom(request), id)
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleCommercialIncidents(writer http.ResponseWriter, request *http.Request) {
	result, err := service.CommercialIncidents(request.Context(), principalFrom(request))
	if err != nil {
		writeCommercialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func decodeCommercialRequest[T any](
	writer http.ResponseWriter,
	request *http.Request,
	label string,
) (T, bool) {
	var value T
	request.Body = http.MaxBytesReader(writer, request.Body, 4<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid "+label)
		return value, false
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeError(writer, http.StatusBadRequest, "invalid "+label)
		return value, false
	}
	return value, true
}

func writeCommercialError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, commercialexecution.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, ErrConflict), errors.Is(err, commercialexecution.ErrConflict),
		errors.Is(err, commercialexecution.ErrOutOfOrder), errors.Is(err, commercialexecution.ErrPending),
		errors.Is(err, commercialexecution.ErrReconciling), errors.Is(err, commercialexecution.ErrFailed):
		status = http.StatusConflict
	case errors.Is(err, commercialexecution.ErrIntegrity):
		status = http.StatusInternalServerError
	}
	writeError(writer, status, err.Error())
}
