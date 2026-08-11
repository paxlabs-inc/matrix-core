// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	neoconfig "matrix/neo/internal/config"
	neomemory "matrix/neo/internal/memory"
)

func TestKnowledgeRoutesUseTypedNeocortexState(t *testing.T) {
	cfg := neoconfig.Default()
	cfg.DataRoot, cfg.NeocortexActor = t.TempDir(), "knowledge-route-test"
	pager, err := neomemory.Open(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()
	handler := (&Server{engine: &Engine{pager: pager}}).Handler()

	topicResponse := httptest.NewRecorder()
	handler.ServeHTTP(topicResponse, httptest.NewRequest(http.MethodPost, "/knowledge/topics", strings.NewReader(`{"name":"Architecture"}`)))
	if topicResponse.Code != http.StatusCreated {
		t.Fatalf("topic: %d %s", topicResponse.Code, topicResponse.Body.String())
	}
	var topic neomemory.KnowledgeTopic
	if err := json.Unmarshal(topicResponse.Body.Bytes(), &topic); err != nil {
		t.Fatal(err)
	}

	importResponse := httptest.NewRecorder()
	request := `{"topic_id":"` + topic.ID + `","title":"Neo boundaries","content":"Capabilities attach outside the canonical runtime loop.","source_kind":"file","source_title":"Boundary note","entities":[{"name":"Neo","kind":"system"},{"name":"Runtime loop","kind":"component"}],"relationships":[{"from":"Neo","to":"Runtime loop","kind":"protects"}]}`
	handler.ServeHTTP(importResponse, httptest.NewRequest(http.MethodPost, "/knowledge/import", strings.NewReader(request)))
	if importResponse.Code != http.StatusCreated {
		t.Fatalf("import: %d %s", importResponse.Code, importResponse.Body.String())
	}
	var document neomemory.KnowledgeDocument
	if err := json.Unmarshal(importResponse.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}

	searchResponse := httptest.NewRecorder()
	handler.ServeHTTP(searchResponse, httptest.NewRequest(http.MethodPost, "/knowledge/search", strings.NewReader(`{"query":"canonical runtime"}`)))
	if searchResponse.Code != http.StatusOK || !strings.Contains(searchResponse.Body.String(), document.ID) || !strings.Contains(searchResponse.Body.String(), "Boundary note") {
		t.Fatalf("search: %d %s", searchResponse.Code, searchResponse.Body.String())
	}

	archiveResponse := httptest.NewRecorder()
	handler.ServeHTTP(archiveResponse, httptest.NewRequest(http.MethodPatch, "/knowledge/documents/"+document.ID, strings.NewReader(`{"archived":true,"retention_days":7}`)))
	if archiveResponse.Code != http.StatusOK || !strings.Contains(archiveResponse.Body.String(), `"version":2`) {
		t.Fatalf("archive: %d %s", archiveResponse.Code, archiveResponse.Body.String())
	}

	exportResponse := httptest.NewRecorder()
	handler.ServeHTTP(exportResponse, httptest.NewRequest(http.MethodGet, "/knowledge/export", nil))
	if exportResponse.Code != http.StatusOK || !strings.Contains(exportResponse.Body.String(), document.ID) {
		t.Fatalf("export: %d %s", exportResponse.Code, exportResponse.Body.String())
	}
}
