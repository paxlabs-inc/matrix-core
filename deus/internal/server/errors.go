package server

import (
	"encoding/json"
	"net/http"
)

// APIError is the uniform error envelope (docs/05-api.md §5.2).
type APIError struct {
	Error   string         `json:"error"`
	Message string         `json:"message"`
	Detail  map[string]any `json:"detail,omitempty"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string, detail map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(APIError{Error: code, Message: message, Detail: detail})
}
