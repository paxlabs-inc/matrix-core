// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package proxy

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"matrix/router/internal/fly"
	"matrix/router/internal/provision"
	"matrix/router/internal/railway"
	"matrix/router/internal/workforceauth"
)

func TestSubjectRoundTrip(t *testing.T) {
	ctx := WithSubject(context.Background(), "alice-uuid")
	if got := Subject(ctx); got != "alice-uuid" {
		t.Fatalf("Subject: got %q", got)
	}
	if got := Subject(context.Background()); got != "" {
		t.Fatalf("default Subject: got %q want empty", got)
	}
}

func TestBuildUpstreamURLIPv6Bracketed(t *testing.T) {
	ep := provision.Endpoint{Host: "fdaa:75:8960::abcd"}
	r := &http.Request{URL: &url.URL{Path: "/messages", RawQuery: "k=v"}}
	u, err := buildUpstreamURL(ep, "8080", r)
	if err != nil {
		t.Fatalf("buildUpstreamURL: %v", err)
	}
	want := "http://[fdaa:75:8960::abcd]:8080/messages?k=v"
	if u.String() != want {
		t.Fatalf("upstream: got %q want %q", u.String(), want)
	}
}

func TestBuildUpstreamURLIPv4(t *testing.T) {
	ep := provision.Endpoint{Host: "10.0.0.5"}
	r := &http.Request{URL: &url.URL{Path: "/healthz"}}
	u, err := buildUpstreamURL(ep, "8080", r)
	if err != nil {
		t.Fatalf("buildUpstreamURL: %v", err)
	}
	if u.Host != "10.0.0.5:8080" {
		t.Fatalf("host: got %q", u.Host)
	}
	if u.Path != "/healthz" {
		t.Fatalf("path: got %q", u.Path)
	}
}

func TestBuildUpstreamURLHostname(t *testing.T) {
	// Railway-style private hostname endpoints pass through unbracketed.
	ep := provision.Endpoint{Host: "matrix-alice.railway.internal"}
	r := &http.Request{URL: &url.URL{Path: "/events"}}
	u, err := buildUpstreamURL(ep, "8080", r)
	if err != nil {
		t.Fatalf("buildUpstreamURL: %v", err)
	}
	if u.Host != "matrix-alice.railway.internal:8080" {
		t.Fatalf("host: got %q", u.Host)
	}
}

func TestBuildUpstreamURLEndpointPortOverride(t *testing.T) {
	ep := provision.Endpoint{Host: "matrix-alice.railway.internal", Port: "9000"}
	r := &http.Request{URL: &url.URL{Path: "/events"}}
	u, err := buildUpstreamURL(ep, "8080", r)
	if err != nil {
		t.Fatalf("buildUpstreamURL: %v", err)
	}
	if u.Host != "matrix-alice.railway.internal:9000" {
		t.Fatalf("host: got %q", u.Host)
	}
}

func TestBuildUpstreamURLEmptyHostErrors(t *testing.T) {
	r := &http.Request{URL: &url.URL{Path: "/events"}}
	if _, err := buildUpstreamURL(provision.Endpoint{}, "8080", r); err == nil {
		t.Fatalf("expected error for empty endpoint host")
	}
}

func TestBuildUpstreamURLNoQuery(t *testing.T) {
	ep := provision.Endpoint{Host: "fdaa::1"}
	r := &http.Request{URL: &url.URL{Path: "/intents/abc"}}
	u, err := buildUpstreamURL(ep, "8080", r)
	if err != nil {
		t.Fatalf("buildUpstreamURL: %v", err)
	}
	if u.RawQuery != "" {
		t.Fatalf("RawQuery should be empty, got %q", u.RawQuery)
	}
}

func TestWaitDaemonReadyReturnsWhenListening(t *testing.T) {
	// Even a non-200 response proves the HTTP server accepts connections,
	// which is all the readiness probe requires.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	h := &Handler{
		DaemonPort:    port,
		ReadyTimeout:  time.Second,
		ProbeInterval: 10 * time.Millisecond,
		Logf:          func(string, ...interface{}) {},
	}
	env := &provision.Env{ID: "m-1", Endpoint: provision.Endpoint{Host: host}}
	if err := h.waitDaemonReady(context.Background(), env); err != nil {
		t.Fatalf("waitDaemonReady: %v", err)
	}
}

func TestWaitDaemonReadyTimesOutWhenUnreachable(t *testing.T) {
	// Reserve a loopback port then release it so connections are refused,
	// simulating the daemon not yet listening immediately post-wake.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	_, port, _ := net.SplitHostPort(l.Addr().String())
	_ = l.Close()
	h := &Handler{
		DaemonPort:    port,
		ReadyTimeout:  200 * time.Millisecond,
		ProbeInterval: 20 * time.Millisecond,
		Logf:          func(string, ...interface{}) {},
	}
	env := &provision.Env{ID: "m-1", Endpoint: provision.Endpoint{Host: "127.0.0.1"}}
	start := time.Now()
	if err := h.waitDaemonReady(context.Background(), env); err == nil {
		t.Fatalf("expected readiness timeout, got nil")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("returned too early (%s); should have polled until the deadline", elapsed)
	}
}

func TestWaitDaemonReadyRetriesEdge502OnWakeOnRequest(t *testing.T) {
	// Slept-service revival: a wake-on-request platform's edge answers
	// 502 before anything of ours listens. The probe must keep polling
	// through those and succeed once the daemon genuinely answers —
	// so the FIRST forwarded request lands inside the wake budget.
	var mu sync.Mutex
	probes := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		probes++
		if probes < 4 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	h := &Handler{
		Prov:          &railway.Provisioner{}, // real wake-on-request provider
		DaemonPort:    port,
		ReadyTimeout:  2 * time.Second,
		ProbeInterval: 10 * time.Millisecond,
		Logf:          func(string, ...interface{}) {},
	}
	env := &provision.Env{ID: "svc-1", Endpoint: provision.Endpoint{Host: host}}
	if err := h.waitDaemonReady(context.Background(), env); err != nil {
		t.Fatalf("waitDaemonReady: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if probes < 4 {
		t.Fatalf("expected the probe to retry through the 502s, got %d probes", probes)
	}
}

func TestWaitDaemonReadyPersistent502FailsRetryablyOnWakeOnRequest(t *testing.T) {
	// A service that never comes up keeps 502ing: the probe must exhaust
	// the budget and error (the caller turns that into a retryable 503),
	// never forward the platform 502 to the user.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	h := &Handler{
		Prov:          &railway.Provisioner{},
		DaemonPort:    port,
		ReadyTimeout:  200 * time.Millisecond,
		ProbeInterval: 20 * time.Millisecond,
		Logf:          func(string, ...interface{}) {},
	}
	env := &provision.Env{ID: "svc-1", Endpoint: provision.Endpoint{Host: host}}
	if err := h.waitDaemonReady(context.Background(), env); err == nil {
		t.Fatalf("expected readiness error on persistent edge 502s, got nil")
	}
}

func TestWaitDaemonReady502IsReadyOnExplicitStartProvider(t *testing.T) {
	// Fly path byte-identical: on an explicit-start provider ANY HTTP
	// response — including 502 — proves the in-machine server listens.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer srv.Close()
	host, port, err := net.SplitHostPort(srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	h := &Handler{
		Prov:          &fly.Provisioner{}, // real explicit-start provider
		DaemonPort:    port,
		ReadyTimeout:  time.Second,
		ProbeInterval: 10 * time.Millisecond,
		Logf:          func(string, ...interface{}) {},
	}
	env := &provision.Env{ID: "m-1", Endpoint: provision.Endpoint{Host: host}}
	if err := h.waitDaemonReady(context.Background(), env); err != nil {
		t.Fatalf("waitDaemonReady must accept 502 as listening on explicit-start providers: %v", err)
	}
}

func TestWaitWorkforceReadyRequiresDerivedOwnerCredential(t *testing.T) {
	deriver, err := workforceauth.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, err := deriver.OwnerToken("user-one")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/workforce/session" ||
			r.Header.Get("Authorization") != "Bearer "+ownerToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"schema_version":"workforce.control.v1"}`))
	}))
	defer server.Close()
	host, port, err := net.SplitHostPort(server.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{
		WorkforcePort: port, Workforce: deriver,
		ReadyTimeout: time.Second, ProbeInterval: 10 * time.Millisecond,
	}
	env := &provision.Env{ID: "service-one", Endpoint: provision.Endpoint{Host: host}}
	if err := handler.waitWorkforceReady(context.Background(), env, ownerToken); err != nil {
		t.Fatal(err)
	}
}

func TestForwardWorkforceUsesRealProvisionerDedicatedPortAndDerivedCredential(t *testing.T) {
	deriver, err := workforceauth.New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	ownerToken, err := deriver.OwnerToken("user-one")
	if err != nil {
		t.Fatal(err)
	}
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer daemon.Close()
	daemonHost, daemonPort, err := net.SplitHostPort(daemon.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	workforced := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+ownerToken {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/workforce/session":
			_, _ = w.Write([]byte(`{"schema_version":"workforce.control.v1"}`))
		case "/v1/workforce/organization":
			_, _ = w.Write([]byte(`{"resource":"organization"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer workforced.Close()
	workforceHost, workforcePort, err := net.SplitHostPort(workforced.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	if workforceHost != daemonHost {
		t.Fatalf("test servers do not share a routable host: daemon=%s workforce=%s", daemonHost, workforceHost)
	}
	flyAPI := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(fly.Machine{
			ID: "machine-one", State: "started", PrivateIP: daemonHost,
		})
	}))
	defer flyAPI.Close()
	provider := &fly.Provisioner{
		Client: fly.New("provider-token", "matrix-daemon").WithEndpoint(flyAPI.URL),
	}
	handler := New(nil, provider, daemonPort, time.Second, 10*time.Millisecond, nil)
	handler.Workforce = deriver
	handler.WorkforcePort = workforcePort
	handler.ReadyTimeout = time.Second
	request := httptest.NewRequest(http.MethodGet, "/v1/workforce/organization", http.NoBody)
	request.Header.Set("Authorization", "Bearer browser-jwt")
	response := httptest.NewRecorder()
	handler.forward(response, request, "user-one", provision.Ref{
		UserID: "user-one", EnvID: "machine-one",
	})
	if response.Code != http.StatusOK || strings.TrimSpace(response.Body.String()) != `{"resource":"organization"}` {
		t.Fatalf("code=%d body=%q", response.Code, response.Body.String())
	}
}

// Copyright © 2026 Sidiora Labs. All rights reserved.
