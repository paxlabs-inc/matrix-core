package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
)

const maxWorkforceWakeBytes = 256 << 10

type workforceWakeIdentity struct {
	SchemaVersion string `json:"schema_version"`
	TenantID      string `json:"tenant_id"`
}

// WorkforceWakeHandler authenticates only routing identity. The complete
// typed envelope is preserved byte-for-byte for workforced, which performs
// canonical validation, authority checks, deduplication, and persistence.
func (h *Handler) WorkforceWakeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, body, ok := workforceWakeSubject(w, r)
		if !ok {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ServeHTTP(w, r.WithContext(WithSubject(r.Context(), userID)))
	})
}

func (h *CentralProxy) WorkforceWakeHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, body, ok := workforceWakeSubject(w, r)
		if !ok {
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.ForwardUser(w, r, userID)
	})
}

func workforceWakeSubject(w http.ResponseWriter, r *http.Request) (string, []byte, bool) {
	if r.Method != http.MethodPost || r.URL.Path != "/internal/workforce/wake" {
		http.NotFound(w, r)
		return "", nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxWorkforceWakeBytes))
	if err != nil {
		http.Error(w, "invalid workforce wake body", http.StatusBadRequest)
		return "", nil, false
	}
	var identity workforceWakeIdentity
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&identity); err != nil ||
		identity.SchemaVersion != "workforce.wake.v1" ||
		strings.TrimSpace(identity.TenantID) == "" || len(identity.TenantID) > 128 {
		http.Error(w, "valid workforce wake identity required", http.StatusBadRequest)
		return "", nil, false
	}
	return strings.TrimSpace(identity.TenantID), body, true
}
