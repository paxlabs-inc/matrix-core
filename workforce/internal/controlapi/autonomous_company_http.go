package controlapi

import (
	"errors"
	"net/http"
	"strconv"
	"strings"

	"centra/workforce/internal/autonomouscompany"
)

type autonomousLimitRequest struct {
	Limit int `json:"limit"`
}

func (service *Service) handleAutonomousPropertyCommit(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[autonomouscompany.PropertyDraft](writer, request, "autonomous company property")
	if !ok {
		return
	}
	result, err := service.CommitAutonomousProperty(request.Context(), principalFrom(request), value)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, replayStatus(result.Replayed), result)
}

func (service *Service) handleAutonomousPropertyCurrent(writer http.ResponseWriter, request *http.Request) {
	kind := autonomouscompany.PropertyKind(strings.TrimSpace(request.URL.Query().Get("kind")))
	initiativeID := strings.TrimSpace(request.URL.Query().Get("initiative_id"))
	result, err := service.CurrentAutonomousProperty(
		request.Context(), principalFrom(request), kind, initiativeID,
	)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleAutonomousPropertyList(writer http.ResponseWriter, request *http.Request) {
	limit, ok := autonomousQueryLimit(writer, request, 100)
	if !ok {
		return
	}
	result, err := service.ListAutonomousProperties(request.Context(), principalFrom(request), limit)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleAutonomousRelease(writer http.ResponseWriter, request *http.Request) {
	initiativeID := strings.TrimSpace(request.URL.Query().Get("initiative_id"))
	result, err := service.CurrentAutonomousRelease(request.Context(), principalFrom(request), initiativeID)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleAutonomousNextCycleUpdate(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[autonomouscompany.NextCycleUpdate](writer, request, "autonomous next cycle update")
	if !ok {
		return
	}
	result, err := service.RecordAutonomousNextCycle(request.Context(), principalFrom(request), value)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, replayStatus(result.Replayed), result)
}

func (service *Service) handleAutonomousNextCycleActive(writer http.ResponseWriter, request *http.Request) {
	limit, ok := autonomousQueryLimit(writer, request, 100)
	if !ok {
		return
	}
	result, err := service.ListActiveAutonomousNextCycles(request.Context(), principalFrom(request), limit)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleAutonomousNextCycleRun(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[autonomousLimitRequest](writer, request, "autonomous next cycle run")
	if !ok {
		return
	}
	result, err := service.RunAutonomousNextCycles(request.Context(), principalFrom(request), value.Limit)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func (service *Service) handleAutonomousNextCycleReconcile(writer http.ResponseWriter, request *http.Request) {
	value, ok := decodeCompanyRecoveryRequest[autonomousLimitRequest](writer, request, "autonomous next cycle reconciliation")
	if !ok {
		return
	}
	result, err := service.ReconcileAutonomousNextCycles(request.Context(), principalFrom(request), value.Limit)
	if err != nil {
		writeAutonomousCompanyError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, result)
}

func autonomousQueryLimit(writer http.ResponseWriter, request *http.Request, fallback int) (int, bool) {
	raw := strings.TrimSpace(request.URL.Query().Get("limit"))
	if raw == "" {
		return fallback, true
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 || value > 200 {
		writeError(writer, http.StatusBadRequest, "invalid autonomous company limit")
		return 0, false
	}
	return value, true
}

func replayStatus(replayed bool) int {
	if replayed {
		return http.StatusOK
	}
	return http.StatusCreated
}

func writeAutonomousCompanyError(writer http.ResponseWriter, err error) {
	status := http.StatusBadRequest
	switch {
	case errors.Is(err, ErrUnauthorized), errors.Is(err, autonomouscompany.ErrUnauthorized):
		status = http.StatusForbidden
	case errors.Is(err, ErrConflict), errors.Is(err, autonomouscompany.ErrConflict),
		errors.Is(err, autonomouscompany.ErrNotFound), errors.Is(err, autonomouscompany.ErrEvidence):
		status = http.StatusConflict
	case errors.Is(err, autonomouscompany.ErrIntegrity):
		status = http.StatusInternalServerError
	}
	writeError(writer, status, err.Error())
}
