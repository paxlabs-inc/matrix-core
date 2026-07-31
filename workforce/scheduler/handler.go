package scheduler

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const maxWakeBodyBytes = 256 << 10

func Handler(store *Store, token string) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodPost || request.URL.Path != "/internal/workforce/wake" {
			http.NotFound(writer, request)
			return
		}
		if token != "" && subtle.ConstantTimeCompare(
			[]byte(bearer(request)), []byte(token),
		) != 1 {
			writeResponse(writer, http.StatusUnauthorized, map[string]any{
				"ok": false, "error": "unauthorized",
			})
			return
		}
		body, err := io.ReadAll(io.LimitReader(request.Body, maxWakeBodyBytes+1))
		if err != nil || len(body) > maxWakeBodyBytes {
			writeResponse(writer, http.StatusRequestEntityTooLarge, map[string]any{
				"ok": false, "error": "invalid_body",
			})
			return
		}
		var wake WakeEnvelope
		if err := json.Unmarshal(body, &wake); err != nil {
			writeResponse(writer, http.StatusBadRequest, map[string]any{
				"ok": false, "error": "invalid_json",
			})
			return
		}
		result, err := store.Enqueue(request.Context(), wake)
		if err != nil {
			status := http.StatusBadRequest
			if errors.Is(err, ErrUnauthorized) {
				status = http.StatusForbidden
			} else if errors.Is(err, ErrConflict) {
				status = http.StatusConflict
			} else if errors.Is(err, ErrUncertain) {
				status = http.StatusServiceUnavailable
			}
			writeResponse(writer, status, map[string]any{
				"ok": false, "error": err.Error(),
			})
			return
		}
		writeResponse(writer, http.StatusAccepted, map[string]any{
			"ok": true, "wake_id": result.WakeID,
			"deduplicated": result.Deduplicated, "coalesced": result.Coalesced,
		})
	})
}

func bearer(request *http.Request) string {
	value := strings.TrimSpace(request.Header.Get("Authorization"))
	if !strings.HasPrefix(value, "Bearer ") {
		return ""
	}
	return strings.TrimSpace(strings.TrimPrefix(value, "Bearer "))
}

func writeResponse(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
