package controlapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (service *Service) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/workforce/events", service.handleEvents)
	mux.HandleFunc("GET /v1/workforce/events/stream", service.handleEventStream)
	mux.HandleFunc("GET /v1/workforce/session", service.handleSession)
	mux.HandleFunc("POST /v1/workforce/control-keys", service.handleControlKey)
	mux.HandleFunc("POST /v1/workforce/activation/preview", service.handleActivationPreview)
	mux.HandleFunc("POST /v1/workforce/activation", service.handleActivation)
	mux.HandleFunc("POST /v1/workforce/work-orders", service.handleWorkOrder)
	mux.HandleFunc("POST /v1/workforce/commands", service.handleCommand)
	mux.HandleFunc("GET /v1/workforce/{resource}", service.handleResource)
	return service.authenticate(mux)
}

func (service *Service) handleWorkOrder(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 256<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var order WorkOrder
	if err := decoder.Decode(&order); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid work order")
		return
	}
	result, err := service.CreateWorkOrder(
		request.Context(), principalFrom(request), order,
	)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrUnauthorized):
			status = http.StatusForbidden
		case errors.Is(err, ErrConflict):
			status = http.StatusConflict
		}
		writeError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleActivationPreview(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 8<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var previewRequest ActivationPreviewRequest
	if err := decoder.Decode(&previewRequest); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid activation preview")
		return
	}
	preview, err := service.PreviewActivation(
		request.Context(), principalFrom(request), previewRequest,
	)
	if err != nil {
		status := http.StatusBadRequest
		if errors.Is(err, ErrUnauthorized) {
			status = http.StatusForbidden
		}
		writeError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, preview)
}

func (service *Service) handleActivation(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 2<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var bundle ActivationBundle
	if err := decoder.Decode(&bundle); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid organization activation")
		return
	}
	result, err := service.ActivateOrganization(
		request.Context(), principalFrom(request), bundle,
	)
	if err != nil {
		status := http.StatusBadRequest
		switch {
		case errors.Is(err, ErrUnauthorized):
			status = http.StatusForbidden
		case errors.Is(err, ErrConflict):
			status = http.StatusConflict
		}
		writeError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusCreated, result)
}

func (service *Service) handleSession(writer http.ResponseWriter, request *http.Request) {
	principal := principalFrom(request)
	writeJSON(writer, http.StatusOK, map[string]any{
		"schema_version":  SchemaVersion,
		"organization_id": principal.OrganizationID,
		"owner_id":        principal.OwnerID,
		"model_provider":  service.runtimeModelProvider,
		"model_id":        service.runtimeModelID,
	})
}

func (service *Service) handleControlKey(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 4<<10)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var registration ControlKeyRegistration
	if err := decoder.Decode(&registration); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid control key")
		return
	}
	err := service.RegisterControlKey(
		request.Context(), principalFrom(request), registration,
	)
	if err != nil {
		if errors.Is(err, ErrConflict) {
			writeError(writer, http.StatusConflict, "control key conflict")
			return
		}
		writeError(writer, http.StatusBadRequest, "invalid control key")
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"schema_version": SchemaVersion, "key_id": registration.KeyID,
	})
}

func (service *Service) authenticate(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		principal, err := service.authenticator.Authenticate(request)
		if err != nil {
			writeError(writer, http.StatusUnauthorized, "unauthorized")
			return
		}
		request = request.WithContext(withPrincipal(request.Context(), principal))
		next.ServeHTTP(writer, request)
	})
}

func (service *Service) handleResource(writer http.ResponseWriter, request *http.Request) {
	resource := request.PathValue("resource")
	limit, err := queryInt(request, "limit", 50)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	page, err := service.List(
		request.Context(), principalFrom(request),
		resource, request.URL.Query().Get("cursor"), limit,
	)
	if err != nil {
		status := http.StatusInternalServerError
		if strings.Contains(err.Error(), "unknown resource") {
			status = http.StatusNotFound
		} else if strings.Contains(err.Error(), "cursor") ||
			strings.Contains(err.Error(), "limit") {
			status = http.StatusBadRequest
		}
		writeError(writer, status, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (service *Service) handleEvents(writer http.ResponseWriter, request *http.Request) {
	after, err := eventCursor(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	limit, err := queryInt(request, "limit", 100)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	page, err := service.Events(
		request.Context(), principalFrom(request), after, limit,
	)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(writer, http.StatusOK, page)
}

func (service *Service) handleEventStream(writer http.ResponseWriter, request *http.Request) {
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeError(writer, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	after, err := eventCursor(request)
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error())
		return
	}
	principal := principalFrom(request)
	subscription, cancel := service.broker.subscribe(topic(principal))
	defer cancel()
	replay, err := service.Events(request.Context(), principal, after, 500)
	if err != nil {
		writeError(writer, http.StatusInternalServerError, err.Error())
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Accel-Buffering", "no")
	last := after
	for _, event := range replay.Events {
		if event.Cursor > last {
			writeSSE(writer, event)
			last = event.Cursor
		}
	}
	flusher.Flush()
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()
	for {
		select {
		case <-request.Context().Done():
			return
		case event, open := <-subscription.events:
			if !open {
				if subscription.dropped.Load() {
					fmt.Fprintf(writer, "event: resync_required\nid: %d\ndata: {\"after\":%d}\n\n", last, last)
					flusher.Flush()
				}
				return
			}
			if event.Cursor <= last {
				continue
			}
			writeSSE(writer, event)
			flusher.Flush()
			last = event.Cursor
		case <-heartbeat.C:
			fmt.Fprint(writer, ": heartbeat\n\n")
			flusher.Flush()
		}
	}
}

func (service *Service) handleCommand(writer http.ResponseWriter, request *http.Request) {
	request.Body = http.MaxBytesReader(writer, request.Body, 1<<20)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	var command SignedCommand
	if err := decoder.Decode(&command); err != nil {
		writeError(writer, http.StatusBadRequest, "invalid command")
		return
	}
	result, err := service.ApplyCommand(
		request.Context(), principalFrom(request), command,
	)
	if err != nil {
		switch {
		case errors.Is(err, ErrUnauthorized):
			writeError(writer, http.StatusForbidden, "command authorization failed")
		case errors.Is(err, ErrConflict):
			writeError(writer, http.StatusConflict, "stale or conflicting mutation")
		default:
			writeError(writer, http.StatusBadRequest, err.Error())
		}
		return
	}
	writer.Header().Set("ETag", strconv.Quote(strconv.FormatUint(result.Version, 10)))
	writeJSON(writer, http.StatusCreated, result)
}

func writeSSE(writer http.ResponseWriter, event LifecycleEvent) {
	payload, _ := json.Marshal(event)
	fmt.Fprintf(writer, "id: %d\nevent: %s\ndata: %s\n\n", event.Cursor, event.Type, payload)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeError(writer http.ResponseWriter, status int, message string) {
	writeJSON(writer, status, map[string]any{
		"schema_version": SchemaVersion,
		"error":          message,
	})
}

func queryInt(request *http.Request, name string, fallback int) (int, error) {
	value := request.URL.Query().Get(name)
	if value == "" {
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func eventCursor(request *http.Request) (uint64, error) {
	value := request.URL.Query().Get("after")
	if value == "" {
		value = request.Header.Get("Last-Event-ID")
	}
	if value == "" {
		return 0, nil
	}
	cursor, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("event cursor is invalid")
	}
	return cursor, nil
}
