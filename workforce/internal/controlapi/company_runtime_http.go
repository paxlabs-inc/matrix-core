package controlapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"matrix/workforce/internal/companyruntime"
)

func (service *Service) handleCompanyStartPreview(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 64<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var draft companyruntime.StartDraft
	if err := decoder.Decode(&draft); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid company start preview")
		return
	}
	preview, err := service.PreviewCompanyStart(request.Context(), principalFrom(request), draft)
	if err != nil {
		writeCompanyRuntimeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, preview)
}

func (service *Service) handleCompanyStart(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 512<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var configuration companyruntime.StartConfiguration
	if err := decoder.Decode(&configuration); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid company start configuration")
		return
	}
	result, err := service.StartCompany(request.Context(), principalFrom(request), configuration)
	if err != nil {
		writeCompanyRuntimeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleCompanyRuntime(writer http.ResponseWriter, request *http.Request) {
	value, err := service.CurrentCompanyRuntime(request.Context(), principalFrom(request))
	if err != nil {
		writeCompanyRuntimeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, value)
}

func (service *Service) handleCompanyFunding(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 2<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var value companyruntime.FundingRequest
	if err := decoder.Decode(&value); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid company funding request")
		return
	}
	result, err := service.FundCompanyInitiative(request.Context(), principalFrom(request), value)
	if err != nil {
		writeCompanyRuntimeError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func writeCompanyRuntimeError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	if errors.Is(err, ErrUnauthorized) {
		status = http.StatusForbidden
	} else if errors.Is(err, ErrConflict) || errors.Is(err, ErrNotActivated) {
		status = http.StatusConflict
	}
	writeError(writer, status, err.Error())
}
