// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package proxy

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"matrix/router/internal/fly"
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
	m := &fly.Machine{ID: "m-1", PrivateIP: "fdaa:75:8960::abcd"}
	r := &http.Request{URL: &url.URL{Path: "/messages", RawQuery: "k=v"}}
	u, err := buildUpstreamURL(m, "8080", r)
	if err != nil {
		t.Fatalf("buildUpstreamURL: %v", err)
	}
	want := "http://[fdaa:75:8960::abcd]:8080/messages?k=v"
	if u.String() != want {
		t.Fatalf("upstream: got %q want %q", u.String(), want)
	}
}

func TestBuildUpstreamURLIPv4(t *testing.T) {
	m := &fly.Machine{ID: "m-1", PrivateIP: "10.0.0.5"}
	r := &http.Request{URL: &url.URL{Path: "/healthz"}}
	u, err := buildUpstreamURL(m, "8080", r)
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

func TestBuildUpstreamURLFallsBackToInternalDNS(t *testing.T) {
	m := &fly.Machine{ID: "m-abc"} // no PrivateIP
	r := &http.Request{URL: &url.URL{Path: "/events"}}
	u, err := buildUpstreamURL(m, "8080", r)
	if err != nil {
		t.Fatalf("buildUpstreamURL: %v", err)
	}
	if !strings.Contains(u.Host, "m-abc.vm.matrix-daemon.internal") {
		t.Fatalf("expected fly internal DNS host, got %q", u.Host)
	}
}

func TestBuildUpstreamURLNoQuery(t *testing.T) {
	m := &fly.Machine{ID: "m-1", PrivateIP: "fdaa::1"}
	r := &http.Request{URL: &url.URL{Path: "/intents/abc"}}
	u, err := buildUpstreamURL(m, "8080", r)
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
	host, port, err := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatalf("split host port: %v", err)
	}
	h := &Handler{
		DaemonPort:    port,
		ReadyTimeout:  time.Second,
		ProbeInterval: 10 * time.Millisecond,
		Logf:          func(string, ...interface{}) {},
	}
	m := &fly.Machine{ID: "m-1", PrivateIP: host}
	if err := h.waitDaemonReady(context.Background(), m); err != nil {
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
	m := &fly.Machine{ID: "m-1", PrivateIP: "127.0.0.1"}
	start := time.Now()
	if err := h.waitDaemonReady(context.Background(), m); err == nil {
		t.Fatalf("expected readiness timeout, got nil")
	}
	if elapsed := time.Since(start); elapsed < 150*time.Millisecond {
		t.Fatalf("returned too early (%s); should have polled until the deadline", elapsed)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
