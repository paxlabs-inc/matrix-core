// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func uploadOne(t *testing.T, s *Server, filename, contentType, body string) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	h := make(map[string][]string)
	h["Content-Disposition"] = []string{`form-data; name="file"; filename="` + filename + `"`}
	h["Content-Type"] = []string{contentType}
	part, err := mw.CreatePart(h)
	if err != nil {
		t.Fatalf("CreatePart: %v", err)
	}
	if _, err := part.Write([]byte(body)); err != nil {
		t.Fatalf("part.Write: %v", err)
	}
	_ = mw.Close()
	req := httptest.NewRequest(http.MethodPost, "/upload", &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.handleUpload(rec, req)
	var out map[string]interface{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestUploadAcceptsDocumentsAndServesThemAsDownloads proves the general-file
// upload path end to end against the real handlers: a PDF and a dotted source
// file store with their extension intact, report kind "file", land on the
// media dir, and stream back via GET /media with the anti-XSS download
// headers; an image keeps rendering inline with none of those headers.
func TestUploadAcceptsDocumentsAndServesThemAsDownloads(t *testing.T) {
	dir := t.TempDir()
	s := &Server{engine: &Engine{mediaDir: dir}}

	// Document upload: extension preserved, kind=file.
	code, out := uploadOne(t, s, "Q2 report.PDF", "application/pdf", "%PDF-1.7 fake body")
	if code != http.StatusCreated {
		t.Fatalf("pdf upload = %d (%v), want 201", code, out)
	}
	if out["kind"] != "file" {
		t.Errorf("pdf kind = %v, want file", out["kind"])
	}
	url, _ := out["url"].(string)
	if !strings.HasPrefix(url, "/media/") || !strings.HasSuffix(url, ".pdf") {
		t.Fatalf("pdf url = %q, want /media/<id>.pdf", url)
	}
	name := strings.TrimPrefix(url, "/media/")
	if b, err := os.ReadFile(filepath.Join(dir, name)); err != nil || string(b) != "%PDF-1.7 fake body" {
		t.Fatalf("stored pdf bytes wrong: %v %q", err, b)
	}

	// Serving a document: download disposition + nosniff (stored-XSS guard).
	rec := httptest.NewRecorder()
	s.handleMedia(rec, httptest.NewRequest(http.MethodGet, url, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s = %d, want 200", url, rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("document Content-Disposition = %q, want attachment", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("document missing nosniff header")
	}

	// Unknown/odd extension falls back to .bin rather than rejecting.
	code, out = uploadOne(t, s, "weird.name.with.spaces and $stuff", "application/octet-stream", "bytes")
	if code != http.StatusCreated {
		t.Fatalf("odd upload = %d (%v), want 201", code, out)
	}
	if u, _ := out["url"].(string); !strings.HasSuffix(u, ".bin") {
		t.Errorf("odd upload url = %v, want .bin fallback", out["url"])
	}

	// Images keep the inline path: no attachment disposition.
	code, out = uploadOne(t, s, "pic.png", "image/png", "\x89PNG fake")
	if code != http.StatusCreated || out["kind"] != "image" {
		t.Fatalf("png upload = %d kind=%v, want 201/image", code, out["kind"])
	}
	rec = httptest.NewRecorder()
	s.handleMedia(rec, httptest.NewRequest(http.MethodGet, out["url"].(string), nil))
	if got := rec.Header().Get("Content-Disposition"); got != "" {
		t.Errorf("image Content-Disposition = %q, want inline (empty)", got)
	}
}

// TestUploadHTMLNeverRendersInline pins the stored-XSS guard specifically for
// markup: an uploaded .html file must come back as a download, never inline.
func TestUploadHTMLNeverRendersInline(t *testing.T) {
	dir := t.TempDir()
	s := &Server{engine: &Engine{mediaDir: dir}}
	code, out := uploadOne(t, s, "evil.html", "text/html", "<script>alert(1)</script>")
	if code != http.StatusCreated {
		t.Fatalf("html upload = %d (%v), want 201", code, out)
	}
	rec := httptest.NewRecorder()
	s.handleMedia(rec, httptest.NewRequest(http.MethodGet, out["url"].(string), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET html = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Disposition"); !strings.HasPrefix(got, "attachment") {
		t.Errorf("html Content-Disposition = %q, want attachment", got)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("html missing nosniff header")
	}
}
