package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"matrix/neo/internal/mediastudio"
)

func TestMediaStudioRoutesExposeRealUnconfiguredService(t *testing.T) {
	service, err := mediastudio.Open(
		context.Background(),
		mediastudio.Config{MediaDir: filepath.Join(t.TempDir(), "media")},
	)
	if err != nil {
		t.Fatalf("open media studio: %v", err)
	}
	defer service.Close()
	server := &Server{engine: &Engine{mediaStudio: service}}

	statusRequest := httptest.NewRequest(http.MethodGet, "/studio/media/status", nil)
	statusResponse := httptest.NewRecorder()
	server.handleMediaStudio(statusResponse, statusRequest)
	if statusResponse.Code != http.StatusOK {
		t.Fatalf("status code = %d body=%s", statusResponse.Code, statusResponse.Body.String())
	}
	var status mediastudio.StatusView
	if err := json.Unmarshal(statusResponse.Body.Bytes(), &status); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if status.Configured || status.Provider != "Centra Media" {
		t.Fatalf("status = %+v", status)
	}

	listRequest := httptest.NewRequest(http.MethodGet, "/studio/media/jobs", nil)
	listResponse := httptest.NewRecorder()
	server.handleMediaStudio(listResponse, listRequest)
	if listResponse.Code != http.StatusOK || strings.TrimSpace(listResponse.Body.String()) != "[]" {
		t.Fatalf("list code=%d body=%s", listResponse.Code, listResponse.Body.String())
	}

	createRequest := httptest.NewRequest(
		http.MethodPost,
		"/studio/media/jobs",
		strings.NewReader(`{"kind":"text-to-image","prompt":"A real request"}`),
	)
	createRequest.Header.Set("Content-Type", "application/json")
	createRequest.Header.Set("X-Matrix-Idempotency-Key", "studio-test-request")
	createResponse := httptest.NewRecorder()
	server.handleMediaStudio(createResponse, createRequest)
	if createResponse.Code != http.StatusBadRequest ||
		!strings.Contains(createResponse.Body.String(), "not configured") {
		t.Fatalf("create code=%d body=%s", createResponse.Code, createResponse.Body.String())
	}
}
