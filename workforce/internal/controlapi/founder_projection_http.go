package controlapi

import (
	"errors"
	"net/http"

	"centra/workforce/internal/founderprojection"
)

func (service *Service) handleFounderProjectionCapture(
	writer http.ResponseWriter,
	request *http.Request,
) {
	draft, ok := decodeSecurityCommand[founderprojection.CaptureDraft](writer, request)
	if !ok {
		return
	}
	receipt, err := service.CaptureFounderProjection(
		request.Context(), principalFrom(request), draft,
	)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrUnauthorized), errors.Is(err, founderprojection.ErrUnauthorized):
			status = http.StatusForbidden
		case errors.Is(err, founderprojection.ErrConflict):
			status = http.StatusConflict
		case errors.Is(err, founderprojection.ErrNotFound):
			status = http.StatusNotFound
		}
		writeError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, receipt)
}
