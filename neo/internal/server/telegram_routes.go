// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"matrix/neo/internal/telegramsettings"
)

func (s *Server) handleTelegram(w http.ResponseWriter, r *http.Request) {
	if s.engine == nil || s.engine.telegram == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "Telegram is not configured on this Neo daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, s.engine.telegram.Status())
	case http.MethodPut:
		var request struct {
			BotToken string `json:"bot_token"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&request); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
			return
		}
		status, err := s.engine.telegram.Configure(r.Context(), request.BotToken)
		if err != nil {
			code := http.StatusBadGateway
			switch {
			case errors.Is(err, telegramsettings.ErrEncryptionRequired):
				code = http.StatusServiceUnavailable
			case !telegramTokenPattern.MatchString(request.BotToken):
				code = http.StatusBadRequest
			}
			writeJSON(w, code, map[string]interface{}{"error": err.Error(), "status": status})
			return
		}
		writeJSON(w, http.StatusOK, status)
	case http.MethodDelete:
		if err := s.engine.telegram.Disconnect(); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, s.engine.telegram.Status())
	default:
		w.Header().Set("Allow", "GET, PUT, DELETE")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
	}
}
