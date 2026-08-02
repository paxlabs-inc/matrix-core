package controlapi

import (
	"errors"
	"net/http"

	"matrix/workforce/internal/provider/financial"
)

func writeFinancialError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, financial.ErrDenied):
		status = http.StatusForbidden
	case errors.Is(err, financial.ErrConflict):
		status = http.StatusConflict
	case errors.Is(err, financial.ErrUnavailable):
		status = http.StatusServiceUnavailable
	}
	writeError(writer, status, err.Error())
}

func (service *Service) handleFinancialConnection(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[financial.Connection](writer, request)
	if !ok {
		return
	}
	result, err := service.RegisterFinancialConnection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeFinancialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleFinancialConnectionRevocation(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[financial.ConnectionRevocation](writer, request)
	if !ok {
		return
	}
	result, err := service.RevokeFinancialConnection(request.Context(), principalFrom(request), value)
	if err != nil {
		writeFinancialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleFinancialValuation(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[financial.ValuationSnapshot](writer, request)
	if !ok {
		return
	}
	result, err := service.RegisterFinancialValuation(request.Context(), principalFrom(request), value)
	if err != nil {
		writeFinancialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleFinancialRisk(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeSecurityCommand[financial.RiskSnapshot](writer, request)
	if !ok {
		return
	}
	result, err := service.RegisterFinancialRisk(request.Context(), principalFrom(request), value)
	if err != nil {
		writeFinancialError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}
