package controlapi

import (
	"encoding/json"
	"errors"
	"net/http"

	"centra/workforce/internal/productexecution"
)

func decodeProductExecution[T any](writer http.ResponseWriter, request *http.Request) (T, bool) {
	var value T
	request.Body = http.MaxBytesReader(writer, request.Body, 4<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid product execution command")
		return value, false
	}
	return value, true
}

func writeProductExecutionError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, productexecution.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, productexecution.ErrConflict), errors.Is(err, productexecution.ErrInvalidPhase),
		errors.Is(err, productexecution.ErrCorrectionBlocked), errors.Is(err, productexecution.ErrAmbiguousEffect):
		status = http.StatusConflict
	}
	writeError(writer, status, err.Error())
}

func (service *Service) handleProductExecutionStart(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.StartRequest](w, r)
	if !ok {
		return
	}
	result, err := service.StartProductExecution(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, result)
}

func (service *Service) handleProductExecutionProduct(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.ReceiptRequest](w, r)
	if !ok {
		return
	}
	result, err := service.CompleteProductExecutionProduct(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionDesign(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.CompleteDesignRequest](w, r)
	if !ok {
		return
	}
	result, err := service.CompleteProductExecutionDesign(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionBuild(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.CompleteStageRequest](w, r)
	if !ok {
		return
	}
	result, err := service.CompleteProductExecutionBuild(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionVerification(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.CompleteStageRequest](w, r)
	if !ok {
		return
	}
	result, err := service.CompleteProductExecutionVerification(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionDeploymentPreparation(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.ReceiptRequest](w, r)
	if !ok {
		return
	}
	result, err := service.CompleteProductExecutionDeploymentPreparation(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionDeploy(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.DeploymentRequest](w, r)
	if !ok {
		return
	}
	result, err := service.DeployProductExecution(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionReconcileDeployment(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.DeploymentRequest](w, r)
	if !ok {
		return
	}
	result, err := service.ReconcileProductExecutionDeployment(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionLaunch(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.CompleteLaunchRequest](w, r)
	if !ok {
		return
	}
	result, err := service.CompleteProductExecutionLaunch(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func (service *Service) handleProductExecutionRecover(w http.ResponseWriter, r *http.Request) {
	v, ok := decodeProductExecution[productexecution.ResumeRequest](w, r)
	if !ok {
		return
	}
	result, err := service.RecoverProductExecution(r.Context(), principalFrom(r), v)
	if err != nil {
		writeProductExecutionError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, result)
}
