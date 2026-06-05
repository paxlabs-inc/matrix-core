// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package proxy is the wake-then-reverse-proxy handler for matrix-router.
//
// Per request:
//
//  1. Resolve the user → DB lookup (fly_machine_id, fly_region, state)
//  2. If state != active: 503 (provisioning) / 451 (suspended) / 410 (deleted)
//  3. Wake the Machine (fly EnsureStarted) inside a wake-deadline context
//  4. Reverse-proxy to http://[<6PN>]:8080 (or fly internal DNS)
//  5. For SSE responses, set FlushInterval so each "data: " chunk hits
//     the client without server-side buffering
//
// The proxy is one of two pieces of cortex-adjacent code that talks
// across the WG mesh — the other is the snapshot package on the
// daemon side. Both rely on Fly's 6PN routing through wg0 + DNS
// resolver at fdaa:75:8960::3.
package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"time"

	"matrix/router/internal/db"
	"matrix/router/internal/fly"
)

// Provisioner triggers out-of-band provisioning of a user's Fly Machine
// on their first authenticated request. Implemented by *admin.Handler
// and wired in main; the proxy stays decoupled from the admin package
// via this interface.
type Provisioner interface {
	StartProvision(userID, email string)
}

// Handler builds an http.Handler that routes authenticated requests
// to the backing Fly Machine for the JWT subject.
//
// The JWT verification + extraction of the supabase user id is handled
// upstream by middleware (mw package); this handler reads the user id
// from request context using the SubjectKey. If the context value is
// missing, the handler returns 500 (programmer error: middleware
// misordered).
type Handler struct {
	DB            *db.DB
	Fly           *fly.Client
	DaemonPort    string        // backend listen port (e.g. "8080")
	WakeTimeout   time.Duration // Fly wake deadline
	ProbeInterval time.Duration // poll cadence inside EnsureStarted + readiness probe
	ReadyTimeout  time.Duration // deadline for the daemon HTTP server to accept connections post-wake
	Logf          func(format string, args ...interface{})

	// Provision, when non-nil, auto-provisions a Machine for an
	// authenticated user with no row yet (first-request onboarding).
	// Nil preserves the legacy 404 "not provisioned" response.
	Provision Provisioner

	// once holds the assembled httputil.ReverseProxy so per-request work
	// is just URL rewrite + Director.
	once *httputil.ReverseProxy
}

// SubjectKey is the type used to stash the verified Supabase subject
// (UUID string) in request context. The JWT middleware populates this
// before delegating to the proxy.
type ctxKey int

const (
	subjectKey ctxKey = iota
	emailKey
)

// WithSubject returns a derived context carrying the supabase user id.
func WithSubject(ctx context.Context, sub string) context.Context {
	return context.WithValue(ctx, subjectKey, sub)
}

// Subject extracts the supabase user id stashed by middleware. Returns
// "" when not set.
func Subject(ctx context.Context) string {
	if v, ok := ctx.Value(subjectKey).(string); ok {
		return v
	}
	return ""
}

// WithEmail returns a derived context carrying the verified Supabase
// email claim. May be empty when the IdP doesn't populate one.
func WithEmail(ctx context.Context, email string) context.Context {
	return context.WithValue(ctx, emailKey, email)
}

// Email extracts the verified Supabase email claim, "" when absent.
func Email(ctx context.Context) string {
	if v, ok := ctx.Value(emailKey).(string); ok {
		return v
	}
	return ""
}

// New returns a Handler with all required deps wired. Logf is optional.
func New(d *db.DB, f *fly.Client, daemonPort string, wakeTimeout, probeInterval time.Duration, logf func(string, ...interface{})) *Handler {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	h := &Handler{
		DB:            d,
		Fly:           f,
		DaemonPort:    daemonPort,
		WakeTimeout:   wakeTimeout,
		ProbeInterval: probeInterval,
		Logf:          logf,
	}
	rp := &httputil.ReverseProxy{
		Director: func(*http.Request) {
			// Director is a no-op; we rewrite the request fully in
			// ServeHTTP before the proxy reaches the wire.
		},
		// FlushInterval = -1 forces an immediate flush after every Write
		// so SSE chunks reach the client without buffering. JSON bodies
		// also flush immediately, which is harmless (small bodies).
		FlushInterval: -1,
		Transport: &http.Transport{
			// MaxIdleConns + IdleConnTimeout are sized for many short
			// JSON request bursts; SSE hijacks the conn for the
			// duration so they don't pollute the pool.
			MaxIdleConns:        128,
			MaxIdleConnsPerHost: 32,
			IdleConnTimeout:     90 * time.Second,
			DialContext: (&net.Dialer{
				Timeout:   10 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.Logf("proxy error: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	h.once = rp
	return h
}

// ServeHTTP implements http.Handler.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := Subject(r.Context())
	if sub == "" {
		http.Error(w, "internal: subject missing", http.StatusInternalServerError)
		return
	}

	user, err := h.DB.LookupForRoute(r.Context(), sub)
	if err != nil {
		switch {
		case errors.Is(err, db.ErrUserNotFound):
			if h.Provision != nil {
				// First authenticated request from a new user: kick off
				// provisioning out-of-band and ask the client to retry
				// while the Machine comes up.
				h.Provision.StartProvision(sub, Email(r.Context()))
				http.Error(w, "user provisioning; retry shortly", http.StatusServiceUnavailable)
			} else {
				http.Error(w, "user not provisioned (POST /admin/users to create)", http.StatusNotFound)
			}
		default:
			h.Logf("db lookup: %v", err)
			http.Error(w, "db lookup error", http.StatusInternalServerError)
		}
		return
	}
	switch user.State {
	case db.StateActive:
		// continue
	case db.StateProvisioning:
		http.Error(w, "user provisioning; retry shortly", http.StatusServiceUnavailable)
		return
	case db.StateSuspended:
		http.Error(w, "user suspended", http.StatusUnavailableForLegalReasons)
		return
	case db.StateDeleted:
		http.Error(w, "user deleted", http.StatusGone)
		return
	default:
		http.Error(w, "user in unexpected state: "+user.State, http.StatusInternalServerError)
		return
	}
	if user.FlyMachineID == "" {
		http.Error(w, "user has no machine attached", http.StatusServiceUnavailable)
		return
	}

	// Wake the Machine (idempotent if already started). We give the
	// wake step its own deadline so a stuck Fly API call doesn't
	// pollute the proxy timeout for the body.
	wakeCtx, cancel := context.WithTimeout(r.Context(), h.WakeTimeout)
	machine, err := h.Fly.EnsureStarted(wakeCtx, user.FlyMachineID, h.ProbeInterval)
	cancel()
	if err != nil {
		switch {
		case errors.Is(err, fly.ErrMachineNotFound):
			http.Error(w, "machine vanished from fly; admin re-provision required", http.StatusGone)
		case errors.Is(err, fly.ErrUnauthorized):
			h.Logf("fly token rejected (refresh FLY_API_TOKEN)")
			http.Error(w, "router misconfigured (fly token)", http.StatusInternalServerError)
		case errors.Is(err, context.DeadlineExceeded):
			http.Error(w, "machine wake timed out", http.StatusGatewayTimeout)
		default:
			h.Logf("ensure started %s: %v", user.FlyMachineID, err)
			http.Error(w, "machine not ready", http.StatusBadGateway)
		}
		return
	}

	// Fly state=started only means the Firecracker VM is up — the daemon
	// still runs its entrypoint (snapshot pull from MinIO, git init)
	// before it binds the daemon port. Reverse-proxying into that gap
	// connection-refuses and 502s the first request after every cold
	// wake. Wait for the in-machine HTTP server to accept a connection.
	if err := h.waitDaemonReady(r.Context(), machine); err != nil {
		h.Logf("daemon readiness %s: %v", user.FlyMachineID, err)
		w.Header().Set("Retry-After", "3")
		http.Error(w, "daemon waking; retry shortly", http.StatusServiceUnavailable)
		return
	}

	// Build the upstream URL. Prefer the explicit private IPv6 from
	// Fly so we don't depend on DNS resolution within the request
	// path. Fly's internal DNS resolves <machine_id>.vm.<app>.internal,
	// but private_ip is canonical and avoids the resolver hop.
	upstream, err := buildUpstreamURL(machine, h.DaemonPort, r)
	if err != nil {
		h.Logf("upstream url: %v", err)
		http.Error(w, "router config error", http.StatusInternalServerError)
		return
	}

	// Rewrite request URL to the upstream — keep path + query exactly.
	rew := r.Clone(r.Context())
	rew.URL = upstream
	rew.Host = upstream.Host
	// Strip auth header before forwarding so the daemon can apply its
	// own bearer-token check via MATRIX_DAEMON_TOKEN if configured.
	// The daemon's authToken (if set) MUST already be present as
	// X-Matrix-Daemon-Token from the admin/provisioning step.
	rew.Header.Del("Authorization")
	// Re-inject the per-user daemon token from DB if the proxy
	// has been configured with one. v1 takes a global token via
	// env (TODO: per-user tokens); the env-var pass is upstream
	// in main.go.
	// Pass user identity downstream so the daemon can attribute
	// requests in its own logs without re-verifying the JWT.
	rew.Header.Set("X-Matrix-User", sub)

	// X-Forwarded-* hygiene
	if cli, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		prior := r.Header.Get("X-Forwarded-For")
		if prior != "" {
			rew.Header.Set("X-Forwarded-For", prior+", "+cli)
		} else {
			rew.Header.Set("X-Forwarded-For", cli)
		}
	}
	if r.TLS != nil {
		rew.Header.Set("X-Forwarded-Proto", "https")
	} else {
		rew.Header.Set("X-Forwarded-Proto", "http")
	}

	h.once.ServeHTTP(w, rew)
}

// buildUpstreamURL composes http://[<6pn>]:<port><path>?<query>.
// IPv6 hosts are bracketed per RFC 3986 §3.2.2 so the URL parser sees
// them as authority not path.
func buildUpstreamURL(m *fly.Machine, port string, r *http.Request) (*url.URL, error) {
	if m == nil || m.PrivateIP == "" {
		// Fall back to internal DNS form. This requires the box to
		// have wg0 up + 6PN DNS resolver reachable (verified live in
		// matrix.kvx sess#26 step 3). Format documented at
		// https://fly.io/docs/networking/private-networking/.
		host := fmt.Sprintf("%s.vm.%s.internal", m.ID, "matrix-daemon")
		return urlFromHostPort(host, port, r)
	}
	host := m.PrivateIP
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return urlFromHostPort(host, port, r)
}

// defaultReadyTimeout bounds how long the proxy waits for a freshly
// woken daemon's HTTP server to start accepting connections before
// giving up with a retryable 503. Used when Handler.ReadyTimeout is 0.
const defaultReadyTimeout = 30 * time.Second

// waitDaemonReady polls the daemon's /healthz on the woken machine
// until its in-machine HTTP server accepts a connection, or the
// readiness deadline elapses. ANY HTTP response (200/401/404/503)
// proves the server is listening, so the probe is auth- and
// route-agnostic; only transport errors (connection refused/reset,
// dial timeout) count as "not ready yet". An already-warm daemon
// answers the first probe in a single round-trip, so the warm path
// adds negligible latency.
func (h *Handler) waitDaemonReady(ctx context.Context, machine *fly.Machine) error {
	ready := h.ReadyTimeout
	if ready <= 0 {
		ready = defaultReadyTimeout
	}
	interval := h.ProbeInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	probeURL, err := healthzURL(machine, h.DaemonPort)
	if err != nil {
		return err
	}

	readyCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	// Per-probe ceiling so a hung SYN can't consume the whole budget;
	// readyCtx bounds the total wait.
	client := &http.Client{Timeout: 3 * time.Second}

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(readyCtx, http.MethodGet, probeURL, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			return nil
		}
		lastErr = err
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("daemon %s not ready within %s: %w", machine.ID, ready, lastErr)
		case <-time.After(interval):
		}
	}
}

// healthzURL composes the daemon's /healthz probe URL, reusing the
// same host-resolution rules (bracketed private IPv6 or fly internal
// DNS fallback) as buildUpstreamURL.
func healthzURL(m *fly.Machine, port string) (string, error) {
	probe := &http.Request{URL: &url.URL{Path: "/healthz"}}
	u, err := buildUpstreamURL(m, port, probe)
	if err != nil {
		return "", err
	}
	return u.String(), nil
}

func urlFromHostPort(host, port string, r *http.Request) (*url.URL, error) {
	raw := "http://" + host
	if port != "" {
		raw += ":" + port
	}
	if r.URL.Path != "" {
		raw += r.URL.Path
	}
	if r.URL.RawQuery != "" {
		raw += "?" + r.URL.RawQuery
	}
	u, err := url.Parse(raw)
	if err != nil {
		return nil, fmt.Errorf("proxy: parse upstream %q: %w", raw, err)
	}
	return u, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
