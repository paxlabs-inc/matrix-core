// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package proxy is the wake-then-reverse-proxy handler for matrix-router.
//
// Per request:
//
//  1. Resolve the user → DB lookup (machine/service id, region, state)
//  2. If state != active: 503 (provisioning) / 451 (suspended) / 410 (deleted)
//  3. Wake the environment (provision.Provisioner.Wake) inside a
//     wake-deadline context — an explicit start+poll on Fly, a no-op on
//     wake-on-request providers (Railway), where the forwarded request
//     itself is the wake signal
//  4. Reverse-proxy to http://<endpoint host>:<port> (IPv6 hosts bracketed)
//  5. For SSE responses, set FlushInterval so each "data: " chunk hits
//     the client without server-side buffering
//
// The proxy is one of two pieces of cortex-adjacent code that talks
// across the private network — the other is the snapshot package on
// the daemon side.
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
	"matrix/router/internal/provision"
	"matrix/router/internal/workforceauth"
)

// Provisioner triggers out-of-band provisioning of a user's environment
// on their first authenticated request. Implemented by *admin.Handler.
type Provisioner interface {
	StartProvision(userID, email string)
}

// Handler builds an http.Handler that routes authenticated requests
// to the backing environment for the JWT subject.
//
// The JWT verification + extraction of the supabase user id is handled
// upstream by middleware (mw package); this handler reads the user id
// from request context using the SubjectKey. If the context value is
// missing, the handler returns 500 (programmer error: middleware
// misordered).
type Handler struct {
	DB             *db.DB
	Prov           provision.Provisioner
	ShardProviders interface {
		Provider(string) (provision.Provisioner, bool)
	}
	DaemonPort    string // backend listen port (e.g. "8080")
	CodyPort      string // codyd listen port (e.g. "8090"); "" disables /cody routing
	WorkforcePort string // workforced listen port (e.g. "8091"); empty disables Workforce routing
	Workforce     *workforceauth.Deriver
	WakeTimeout   time.Duration // environment wake deadline
	ProbeInterval time.Duration // poll cadence inside Wake + readiness probe
	ReadyTimeout  time.Duration // deadline for the daemon HTTP server to accept connections post-wake
	Logf          func(format string, args ...interface{})

	// Provision, when non-nil, auto-provisions an environment for an
	// authenticated user with no row yet.
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
func New(d *db.DB, p provision.Provisioner, daemonPort string, wakeTimeout, probeInterval time.Duration, logf func(string, ...interface{})) *Handler {
	if logf == nil {
		logf = func(string, ...interface{}) {}
	}
	h := &Handler{
		DB:            d,
		Prov:          p,
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
		ModifyResponse: func(resp *http.Response) error {
			stripCORSResponseHeaders(resp.Header)
			return nil
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

func stripCORSResponseHeaders(h http.Header) {
	for _, name := range []string{
		"Access-Control-Allow-Origin",
		"Access-Control-Allow-Headers",
		"Access-Control-Allow-Methods",
		"Access-Control-Allow-Credentials",
		"Access-Control-Expose-Headers",
		"Access-Control-Max-Age",
	} {
		h.Del(name)
	}
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
				// Public first-run gate: provisioning starts only after the
				// user has recorded the required disclosure acknowledgement
				// and an explicit training-data choice.
				approved, rErr := h.DB.HasCompletedFirstRunApprovals(
					r.Context(),
					sub,
					db.PublicLaunchDisclosureVersion,
				)
				if rErr != nil {
					h.Logf("first-run approval check %s: %v", sub, rErr)
					http.Error(w, "internal error", http.StatusInternalServerError)
					return
				}
				if !approved {
					http.Error(w, "first-run approvals required", http.StatusPreconditionFailed)
					return
				}
				// First authenticated request from a new user: kick off
				// provisioning out-of-band and ask the client to retry
				// while the environment comes up.
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
	if user.EnvID == "" {
		http.Error(w, "user has no machine attached", http.StatusServiceUnavailable)
		return
	}

	prov := h.Prov
	if user.Provider == "railway" && h.ShardProviders != nil {
		var ok bool
		prov, ok = h.ShardProviders.Provider(user.RailwayShardID)
		if !ok {
			http.Error(w, "assigned shard unavailable", http.StatusServiceUnavailable)
			return
		}
	}
	h.forwardWith(w, r, sub, provision.Ref{UserID: user.ID, EnvID: user.EnvID, VolumeID: user.VolumeID}, prov)
}

// forward is the wake-then-proxy core: wake the environment, wait for
// the in-machine daemon to accept connections, then reverse-proxy the
// request to it. Split from ServeHTTP so the wake path is exercisable
// end-to-end without a Postgres row behind it.
func (h *Handler) forward(w http.ResponseWriter, r *http.Request, sub string, ref provision.Ref) {
	h.forwardWith(w, r, sub, ref, h.Prov)
}

func (h *Handler) forwardWith(w http.ResponseWriter, r *http.Request, sub string, ref provision.Ref, prov provision.Provisioner) {
	workforceRoute := classifyWorkforceRoute(r.URL.Path)
	if workforceRoute != workforceRouteNone && (h.Workforce == nil || h.WorkforcePort == "") {
		http.Error(w, "workforce unavailable", http.StatusServiceUnavailable)
		return
	}
	// Wake the environment (idempotent if already running). We give the
	// wake step its own deadline so a stuck provider API call doesn't
	// pollute the proxy timeout for the body. On wake-on-request
	// providers Wake returns immediately and the readiness probe below
	// absorbs the whole cold boot.
	wakeCtx, cancel := context.WithTimeout(r.Context(), h.WakeTimeout)
	env, err := prov.Wake(wakeCtx, ref)
	cancel()
	if err != nil {
		switch {
		case errors.Is(err, provision.ErrNotFound):
			http.Error(w, "machine vanished; admin re-provision required", http.StatusGone)
		case errors.Is(err, provision.ErrUnauthorized):
			h.Logf("provider token rejected (refresh provider API token)")
			http.Error(w, "router misconfigured (provider token)", http.StatusInternalServerError)
		case errors.Is(err, context.DeadlineExceeded):
			http.Error(w, "machine wake timed out", http.StatusGatewayTimeout)
		default:
			h.Logf("wake %s: %v", ref.EnvID, err)
			http.Error(w, "machine not ready", http.StatusBadGateway)
		}
		return
	}

	// A running instance only means the VM/container is up — the daemon
	// still runs its entrypoint (snapshot pull from MinIO, git init)
	// before it binds the daemon port. Reverse-proxying into that gap
	// connection-refuses and 502s the first request after every cold
	// wake. Wait for the in-machine HTTP server to accept a connection.
	if err := h.waitDaemonReadyWith(r.Context(), env, prov); err != nil {
		h.Logf("daemon readiness %s: %v", ref.EnvID, err)
		w.Header().Set("Retry-After", "3")
		http.Error(w, "daemon waking; retry shortly", http.StatusServiceUnavailable)
		return
	}
	if workforceRoute != workforceRouteNone {
		ownerToken, tokenErr := h.Workforce.OwnerToken(sub)
		if tokenErr != nil {
			h.Logf("workforce readiness credential %s: %v", sub, tokenErr)
			http.Error(w, "workforce routing error", http.StatusInternalServerError)
			return
		}
		if err := h.waitWorkforceReady(r.Context(), env, ownerToken); err != nil {
			h.Logf("workforce readiness %s: %v", ref.EnvID, err)
			w.Header().Set("Retry-After", "3")
			http.Error(w, "workforce waking; retry shortly", http.StatusServiceUnavailable)
			return
		}
	}

	// Cody engine (codyd) is a co-located sibling on its own port (:8090),
	// reached over the private network — NOT proxied through the Neo front.
	// Route the /cody/* prefix to codyd and strip it so codyd sees its own
	// routes (/projects, /chat, /events, /workspace/*, /conversations/*,
	// /intents/*), which otherwise collide with Neo's identical paths on :8080.
	targetPort := h.DaemonPort
	if workforceRoute != workforceRouteNone {
		targetPort = h.WorkforcePort
	}
	if p := r.URL.Path; h.CodyPort != "" && (p == "/cody" || strings.HasPrefix(p, "/cody/")) {
		targetPort = h.CodyPort
		r.URL.Path = strings.TrimPrefix(p, "/cody")
		if r.URL.Path == "" {
			r.URL.Path = "/"
		}
	}

	// Build the upstream URL from the provider-neutral endpoint.
	upstream, err := buildUpstreamURL(env.Endpoint, targetPort, r)
	if err != nil {
		h.Logf("upstream url: %v", err)
		http.Error(w, "router config error", http.StatusInternalServerError)
		return
	}

	// Rewrite request URL to the upstream — keep path + query exactly.
	rew := r.Clone(r.Context())
	rew.URL = upstream
	rew.Host = upstream.Host
	// Preserve the verified user bearer token for the daemon's established
	// JWT contract. ShardIngress restores it only after authenticating the
	// central router and confirming the user's shard assignment.
	// Re-inject the per-user daemon token from DB if the proxy
	// has been configured with one. v1 takes a global token via
	// env (TODO: per-user tokens); the env-var pass is upstream
	// in main.go.
	// Pass user identity downstream so the daemon can attribute
	// requests in its own logs without re-verifying the JWT.
	rew.Header.Set("X-Matrix-User", sub)
	if workforceRoute != workforceRouteNone {
		var token string
		if workforceRoute == workforceRouteWake {
			token, err = h.Workforce.WakeToken(sub)
		} else {
			token, err = h.Workforce.OwnerToken(sub)
		}
		if err != nil {
			h.Logf("workforce credential %s: %v", sub, err)
			http.Error(w, "workforce routing error", http.StatusInternalServerError)
			return
		}
		rew.Header.Set("Authorization", "Bearer "+token)
	}

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

type workforceRouteKind uint8

const (
	workforceRouteNone workforceRouteKind = iota
	workforceRouteOwner
	workforceRouteWake
)

func classifyWorkforceRoute(path string) workforceRouteKind {
	if path == "/internal/workforce/wake" {
		return workforceRouteWake
	}
	if path == "/v1/workforce" || strings.HasPrefix(path, "/v1/workforce/") {
		return workforceRouteOwner
	}
	return workforceRouteNone
}

// buildUpstreamURL composes http://<host>:<port><path>?<query> from the
// provider-neutral endpoint. IPv6 hosts are bracketed per RFC 3986
// §3.2.2 so the URL parser sees them as authority not path; hostname
// endpoints (<service>.railway.internal) pass through untouched. An
// endpoint port, when set by the provider, overrides the router's
// daemon port.
func buildUpstreamURL(ep provision.Endpoint, port string, r *http.Request) (*url.URL, error) {
	if ep.Host == "" {
		return nil, errors.New("proxy: endpoint has no host")
	}
	host := ep.Host
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if ep.Port != "" {
		port = ep.Port
	}
	return urlFromHostPort(host, port, r)
}

// defaultReadyTimeout bounds how long the proxy waits for a freshly
// woken daemon's HTTP server to start accepting connections before
// giving up with a retryable 503. Used when Handler.ReadyTimeout is 0.
const defaultReadyTimeout = 30 * time.Second

// waitDaemonReady polls the daemon's /healthz on the woken environment
// until its in-machine HTTP server accepts a connection, or the
// readiness deadline elapses. ANY HTTP response (200/401/404/503)
// proves the server is listening, so the probe is auth- and
// route-agnostic; only transport errors (connection refused/reset,
// dial timeout) count as "not ready yet". An already-warm daemon
// answers the first probe in a single round-trip, so the warm path
// adds negligible latency.
//
// Wake-on-request nuance: while such a platform (Railway Serverless)
// revives a slept service, its edge can answer 502 Bad Gateway before
// anything of ours is listening. That 502 is the platform talking,
// not the daemon, so on wake-on-request providers it counts as "not
// ready yet" and the probe keeps polling within the budget.
// Explicit-start providers (Fly) keep the any-response semantics
// unchanged.
func (h *Handler) waitDaemonReady(ctx context.Context, env *provision.Env) error {
	return h.waitDaemonReadyWith(ctx, env, h.Prov)
}

func (h *Handler) waitDaemonReadyWith(ctx context.Context, env *provision.Env, prov provision.Provisioner) error {
	ready := h.ReadyTimeout
	if ready <= 0 {
		ready = defaultReadyTimeout
	}
	interval := h.ProbeInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	probeURL, err := healthzURL(env, h.DaemonPort)
	if err != nil {
		return err
	}

	readyCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	// Per-probe ceiling so a hung SYN can't consume the whole budget;
	// readyCtx bounds the total wait.
	client := &http.Client{Timeout: 3 * time.Second}

	wakeOnRequest := prov != nil && prov.WakeOnRequest()

	var lastErr error
	for {
		req, err := http.NewRequestWithContext(readyCtx, http.MethodGet, probeURL, http.NoBody)
		if err != nil {
			return err
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if !wakeOnRequest || resp.StatusCode != http.StatusBadGateway {
				return nil
			}
			lastErr = fmt.Errorf("edge answered 502 while the service revives")
		} else {
			lastErr = err
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("daemon %s not ready within %s: %w", env.ID, ready, lastErr)
		case <-time.After(interval):
		}
	}
}

func (h *Handler) waitWorkforceReady(ctx context.Context, env *provision.Env, ownerToken string) error {
	ready := h.ReadyTimeout
	if ready <= 0 {
		ready = defaultReadyTimeout
	}
	interval := h.ProbeInterval
	if interval <= 0 {
		interval = 250 * time.Millisecond
	}
	probe := &http.Request{URL: &url.URL{Path: "/v1/workforce/session"}}
	probeURL, err := buildUpstreamURL(env.Endpoint, h.WorkforcePort, probe)
	if err != nil {
		return err
	}
	readyCtx, cancel := context.WithTimeout(ctx, ready)
	defer cancel()
	client := &http.Client{Timeout: 3 * time.Second}
	var lastErr error
	for {
		req, err := http.NewRequestWithContext(readyCtx, http.MethodGet, probeURL.String(), http.NoBody)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("workforced answered %d", resp.StatusCode)
		} else {
			lastErr = err
		}
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("workforced %s not ready within %s: %w", env.ID, ready, lastErr)
		case <-time.After(interval):
		}
	}
}

// healthzURL composes the daemon's /healthz probe URL, reusing the
// same host-resolution rules (bracketed IPv6, provider port override)
// as buildUpstreamURL.
func healthzURL(env *provision.Env, port string) (string, error) {
	probe := &http.Request{URL: &url.URL{Path: "/healthz"}}
	u, err := buildUpstreamURL(env.Endpoint, port, probe)
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
