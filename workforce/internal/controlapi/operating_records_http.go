package controlapi

import (
	"errors"
	"net/http"

	"centra/workforce/internal/commercialcapability"
	"centra/workforce/internal/provider/customer"
	"centra/workforce/internal/provider/external"
)

func writeOperatingRecordError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized),
		errors.Is(err, external.ErrDenied),
		errors.Is(err, customer.ErrDenied),
		errors.Is(err, commercialcapability.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, external.ErrConflict),
		errors.Is(err, customer.ErrConflict),
		errors.Is(err, commercialcapability.ErrConflict),
		errors.Is(err, commercialcapability.ErrExpired):
		status = http.StatusConflict
	case errors.Is(err, commercialcapability.ErrNotFound):
		status = http.StatusNotFound
	case errors.Is(err, external.ErrUnavailable), errors.Is(err, customer.ErrUnavailable):
		status = http.StatusServiceUnavailable
	}
	writeError(writer, status, err.Error())
}

func (service *Service) handleExternalConnection(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[ExternalConnectionRegistration](writer, request)
	if !ok {
		return
	}
	result, err := service.RegisterExternalConnection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeOperatingRecordError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleExternalConnectionRevocation(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[external.ConnectionRevocation](writer, request)
	if !ok {
		return
	}
	result, err := service.RevokeExternalConnection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeOperatingRecordError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleCustomerConnection(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[customer.Connection](writer, request)
	if !ok {
		return
	}
	result, err := service.RegisterCustomerConnection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeOperatingRecordError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleCustomerConnectionRevocation(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[customer.ConnectionRevocation](writer, request)
	if !ok {
		return
	}
	result, err := service.RevokeCustomerConnection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeOperatingRecordError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleCustomerScope(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[customer.CustomerScope](writer, request)
	if !ok {
		return
	}
	result, err := service.RegisterCustomerScope(request.Context(), principalFrom(request), value)
	if err != nil {
		writeOperatingRecordError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleCustomerConsent(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[customer.ConsentRecord](writer, request)
	if !ok {
		return
	}
	result, err := service.RecordCustomerConsent(request.Context(), principalFrom(request), value)
	if err != nil {
		writeOperatingRecordError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleCommercialRecord(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[commercialcapability.VerifiedRecord](writer, request)
	if !ok {
		return
	}
	result, err := service.CommitCommercialRecord(request.Context(), principalFrom(request), value)
	if err != nil {
		writeOperatingRecordError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}
