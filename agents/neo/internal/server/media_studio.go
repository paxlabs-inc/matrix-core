package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strings"

	"centra/agents/neo/internal/mediastudio"
)

const maxStudioRequestBytes = 64 << 10

func (s *Server) handleMediaStudio(w http.ResponseWriter, r *http.Request) {
	if s.engine.mediaStudioErr != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "The private media library could not be opened.",
		})
		return
	}
	service := s.engine.mediaStudio
	if service == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{
			"error": "Media Studio is unavailable on this machine.",
		})
		return
	}
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/studio/media"), "/")
	switch {
	case path == "status":
		if r.Method != http.MethodGet {
			studioMethodNotAllowed(w, "GET")
			return
		}
		writeJSON(w, http.StatusOK, service.Status())
	case path == "jobs":
		s.handleMediaStudioJobs(w, r, service)
	case strings.HasPrefix(path, "jobs/"):
		s.handleMediaStudioJob(w, r, service, strings.TrimPrefix(path, "jobs/"))
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Studio resource not found."})
	}
}

func (s *Server) handleMediaStudioJobs(
	w http.ResponseWriter,
	r *http.Request,
	service *mediastudio.Service,
) {
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, service.List())
	case http.MethodPost:
		r.Body = http.MaxBytesReader(w, r.Body, maxStudioRequestBytes)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var request mediastudio.Request
		if err := decoder.Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "The Studio request is invalid or too large.",
			})
			return
		}
		if err := ensureStudioJSONEnd(decoder); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{
				"error": "The Studio request must contain one object.",
			})
			return
		}
		idempotencyKey := strings.TrimSpace(r.Header.Get("X-Matrix-Idempotency-Key"))
		if idempotencyKey == "" {
			idempotencyKey = strings.TrimSpace(r.Header.Get("Idempotency-Key"))
		}
		job, err := service.Create(idempotencyKey, request)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusAccepted, job)
	default:
		studioMethodNotAllowed(w, "GET, POST")
	}
}

func (s *Server) handleMediaStudioJob(
	w http.ResponseWriter,
	r *http.Request,
	service *mediastudio.Service,
	id string,
) {
	if id == "" || strings.Contains(id, "/") {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "Studio job not found."})
		return
	}
	switch r.Method {
	case http.MethodGet:
		job, ok := service.Get(id)
		if !ok {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Studio job not found."})
			return
		}
		writeJSON(w, http.StatusOK, job)
	case http.MethodDelete:
		err := service.Delete(id)
		if errors.Is(err, os.ErrNotExist) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "Studio job not found."})
			return
		}
		if err != nil {
			writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		studioMethodNotAllowed(w, "GET, DELETE")
	}
}

func ensureStudioJSONEnd(decoder *json.Decoder) error {
	var extra any
	err := decoder.Decode(&extra)
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err == nil {
		return errors.New("extra JSON value")
	}
	return err
}

func studioMethodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "Method not allowed."})
}
