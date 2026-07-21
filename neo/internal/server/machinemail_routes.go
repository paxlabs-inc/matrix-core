package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"matrix/neo/internal/machinemailsettings"
)

func (s *Server) handleMachineMail(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil || s.engine.machineMail == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "MachineMail is not configured on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.engine.machineMail.Status())
	case http.MethodPut:
		var request struct {
			APIKey string `json:"api_key"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		status, err := s.engine.machineMail.Configure(r.Context(), request.APIKey)
		if err != nil {
			code := http.StatusBadGateway
			switch {
			case errors.Is(err, machinemailsettings.ErrEncryptionRequired):
				code = http.StatusServiceUnavailable
			case !machineMailKeyPattern.MatchString(request.APIKey):
				code = http.StatusBadRequest
			}
			writeJSON(w, code, map[string]interface{}{"error": err.Error(), "status": status})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := s.engine.machineMail.Disconnect(r.Context()); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.engine.machineMail.Status())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}
