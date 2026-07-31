package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestWorkforceWakeHandlerRoutesTenantAndPreservesEnvelope(t *testing.T) {
	body := `{"schema_version":"workforce.wake.v1","tenant_id":"user-one","wake_id":"wake-one"}`
	var gotSubject, gotBody string
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotSubject = Subject(r.Context())
		encoded, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		gotBody = string(encoded)
		w.WriteHeader(http.StatusAccepted)
	})
	handler := workforceWakeTestHandler(next)
	request := httptest.NewRequest(http.MethodPost, "/internal/workforce/wake", strings.NewReader(body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || gotSubject != "user-one" || gotBody != body {
		t.Fatalf("code=%d subject=%q body=%q", response.Code, gotSubject, gotBody)
	}
}

func TestWorkforceWakeHandlerRejectsMissingIdentity(t *testing.T) {
	handler := workforceWakeTestHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("invalid wake reached next handler")
	}))
	request := httptest.NewRequest(http.MethodPost, "/internal/workforce/wake", strings.NewReader(`{"schema_version":"workforce.wake.v1"}`))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("code=%d", response.Code)
	}
}

func workforceWakeTestHandler(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID, body, ok := workforceWakeSubject(w, r)
		if !ok {
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(body)))
		next.ServeHTTP(w, r.WithContext(WithSubject(context.Background(), userID)))
	})
}
