// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package preview

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"centra/router/internal/proxy"
)

// withSubject drives the public Handler with a request whose context
// carries the given verified subject (as mw.JWT would have stashed it).
func serveWithSubject(h *Handler, sub, target string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, target, http.NoBody)
	req = req.WithContext(proxy.WithSubject(req.Context(), sub))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestHandler_ProxiesToRegisteredTarget(t *testing.T) {
	// Real backend that echoes the path (and query) it received.
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "path="+r.URL.Path+" query="+r.URL.RawQuery+" cookie="+r.Header.Get("Cookie")+" authorization="+r.Header.Get("Authorization"))
	}))
	defer backend.Close()

	u, err := url.Parse(backend.URL)
	if err != nil {
		t.Fatalf("parse backend url: %v", err)
	}

	reg := NewRegistry()
	reg.Register("alice", Target{Host: u.Hostname(), Port: u.Port()})

	h := &Handler{Reg: reg, Logf: func(string, ...any) {}}

	req := httptest.NewRequest(http.MethodGet, "/preview/alice/app/page?x=1&access_token=private-jwt", http.NoBody)
	req.Header.Set("Cookie", "app_session=allowed; mx_pv=private-jwt")
	req.Header.Set("Authorization", "Bearer private-jwt")
	req = req.WithContext(proxy.WithSubject(req.Context(), "alice"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "path=/app/page") {
		t.Errorf("prefix not stripped: body=%q, want path=/app/page", body)
	}
	if !strings.Contains(body, "query=x=1") {
		t.Errorf("query not preserved: body=%q", body)
	}
	if strings.Contains(body, "private-jwt") || strings.Contains(body, "access_token") {
		t.Errorf("preview credential reached the project server: body=%q", body)
	}
	if !strings.Contains(body, "cookie=app_session=allowed") {
		t.Errorf("project cookie was not preserved: body=%q", body)
	}
	if cookie := rec.Header().Get("Set-Cookie"); !strings.Contains(cookie, "mx_pv=private-jwt") || !strings.Contains(cookie, "Path=/preview/alice") {
		t.Errorf("preview cookie not established: %q", cookie)
	}
}

func TestHandler_CrossUserForbidden(t *testing.T) {
	reg := NewRegistry()
	reg.Register("alice", Target{Host: "127.0.0.1", Port: "1"})
	h := &Handler{Reg: reg}

	rec := serveWithSubject(h, "bob", "/preview/alice/secret")
	if rec.Code != http.StatusForbidden {
		t.Fatalf("cross-user status = %d, want 403", rec.Code)
	}
}

func TestHandler_UnregisteredNotFound(t *testing.T) {
	reg := NewRegistry()
	h := &Handler{Reg: reg}

	rec := serveWithSubject(h, "carol", "/preview/carol/index.html")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("unregistered status = %d, want 404", rec.Code)
	}
}

func TestHandler_MissingSubjectServerError(t *testing.T) {
	h := &Handler{Reg: NewRegistry()}
	req := httptest.NewRequest(http.MethodGet, "/preview/alice/x", http.NoBody)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("missing-subject status = %d, want 500", rec.Code)
	}
}

func TestRegisterHandler_RoundTrip(t *testing.T) {
	reg := NewRegistry()
	h := RegisterHandler(reg)

	// Register.
	body := strings.NewReader(`{"user_id":"alice","host":"10.0.0.5","port":"3000"}`)
	req := httptest.NewRequest(http.MethodPost, "/internal/preview/register", body)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("register status = %d, want 200; body=%q", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Errorf("register body = %q, want ok:true", rec.Body.String())
	}
	tgt, ok := reg.Get("alice")
	if !ok || tgt.Host != "10.0.0.5" || tgt.Port != "3000" {
		t.Fatalf("registry after register = %+v, ok=%v", tgt, ok)
	}
	if tgt.RegisteredAt.IsZero() {
		t.Errorf("RegisteredAt not set")
	}

	// Deregister via query param.
	req = httptest.NewRequest(http.MethodDelete, "/internal/preview/register?user_id=alice", http.NoBody)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deregister status = %d, want 200", rec.Code)
	}
	if _, ok := reg.Get("alice"); ok {
		t.Errorf("target still registered after deregister")
	}

	// Deregister via JSON body (round-trip the alternate shape too).
	reg.Register("dave", Target{Host: "10.0.0.9", Port: "8080"})
	req = httptest.NewRequest(http.MethodDelete, "/internal/preview/register", strings.NewReader(`{"user_id":"dave"}`))
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("deregister(body) status = %d, want 200", rec.Code)
	}
	if _, ok := reg.Get("dave"); ok {
		t.Errorf("dave still registered after body deregister")
	}
}

func TestRegisterHandler_BadRequests(t *testing.T) {
	h := RegisterHandler(NewRegistry())

	// Missing host.
	req := httptest.NewRequest(http.MethodPost, "/internal/preview/register", strings.NewReader(`{"user_id":"x"}`))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing-host status = %d, want 400", rec.Code)
	}

	// Unsupported method.
	req = httptest.NewRequest(http.MethodGet, "/internal/preview/register", http.NoBody)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET status = %d, want 405", rec.Code)
	}
}
