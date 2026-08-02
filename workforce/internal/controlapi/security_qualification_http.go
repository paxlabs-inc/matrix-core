package controlapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"matrix/workforce/internal/securityqualification"
)

func decodeSecurityCommand[T any](writer http.ResponseWriter, request *http.Request) (T, bool) {
	var value T
	request.Body = http.MaxBytesReader(writer, request.Body, 4<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid security qualification command")
		return value, false
	}
	return value, true
}

func writeSecurityQualificationError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, securityqualification.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, securityqualification.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, securityqualification.ErrNotFound):
		status = http.StatusNotFound
	}
	writeError(writer, status, err.Error())
}

func (service *Service) handleSecurityThreatModel(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[securityqualification.ThreatModel](writer, request)
	if !ok {
		return
	}
	result, err := service.CommitSecurityThreatModel(request.Context(), principalFrom(request), value)
	if err != nil {
		writeSecurityQualificationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleSecurityReview(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[securityqualification.BoundaryReview](writer, request)
	if !ok {
		return
	}
	result, err := service.CommitSecurityReview(request.Context(), principalFrom(request), value)
	if err != nil {
		writeSecurityQualificationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleSecurityQualification(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[SecurityQualificationRequest](writer, request)
	if !ok {
		return
	}
	result, err := service.QualifySecurity(request.Context(), principalFrom(request), value)
	if err != nil {
		writeSecurityQualificationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}
