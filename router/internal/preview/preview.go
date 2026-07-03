// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package preview serves per-user application previews over the private
// network.
//
// A user's codyd (running inside their VM) registers a preview target —
// the private host:port its dev server binds — with the router via the
// INTERNAL listener (POST /internal/preview/register), and deregisters it
// on teardown (DELETE). The router keeps those targets in an in-memory
// Registry keyed by the supabase user id; codyd owns the lifecycle.
//
// Previews are exposed on the PUBLIC listener under /preview/{userID}/...,
// JWT-authenticated by the surrounding mw.JWT middleware. The load-bearing
// authorization check lives here: the {userID} path segment MUST equal the
// verified token subject, so previews are NEVER world-readable and one user
// can never reach another's target. Requests are reverse-proxied to the
// registered private target with the /preview/{userID} prefix stripped; no
// public sandbox domain is required.
package preview

import (
	"encoding/json"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"matrix/router/internal/proxy"
)

// previewCookie MUST match mw.PreviewCookie. It is duplicated (not imported)
// because mw imports this package, so importing mw here would cycle.
const previewCookie = "mx_pv"

// Target is a registered preview backend: the private host:port a user's
// preview server binds to, plus when it was registered.
type Target struct {
	Host         string
	Port         string
	RegisteredAt time.Time
}

// Registry is a thread-safe map of supabase user id → preview Target.
// codyd owns the lifecycle (register on start, deregister on teardown);
// the router just holds the current binding.
type Registry struct {
	mu      sync.RWMutex
	targets map[string]Target
}

// NewRegistry returns an empty, ready-to-use Registry.
func NewRegistry() *Registry {
	return &Registry{targets: make(map[string]Target)}
}

// Register sets (or replaces) the preview Target for userID.
func (r *Registry) Register(userID string, t Target) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.targets == nil {
		r.targets = make(map[string]Target)
	}
	r.targets[userID] = t
}

// Deregister removes any preview Target for userID (no-op if absent).
func (r *Registry) Deregister(userID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.targets, userID)
}

// Get returns the preview Target for userID and whether one is registered.
func (r *Registry) Get(userID string) (Target, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	t, ok := r.targets[userID]
	return t, ok
}

// Handler is the PUBLIC /preview/ mount. It authorizes the request against
// the JWT subject (stashed by mw.JWT via proxy.WithSubject), then
// reverse-proxies to the user's registered private target.
type Handler struct {
	Reg  *Registry
	Logf func(string, ...any)
}

func (h *Handler) logf(format string, args ...any) {
	if h.Logf != nil {
		h.Logf(format, args...)
	}
}

// ServeHTTP implements http.Handler for the public /preview/ mount.
//
// Path shape: /preview/{userID}/<rest>. {userID} MUST equal the verified
// token subject (403 otherwise) — this is the whole point: previews are
// never cross-user readable.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sub := proxy.Subject(r.Context())
	if sub == "" {
		// mw.JWT is expected to run before us; a missing subject means the
		// middleware was misordered (programmer error), not a client fault.
		http.Error(w, "internal: subject missing", http.StatusInternalServerError)
		return
	}

	userID, rest, ok := parsePreviewPath(r.URL.Path)
	if !ok {
		http.Error(w, "preview: path must be /preview/{userID}/...", http.StatusNotFound)
		return
	}
	if userID != sub {
		// Cross-user access denied. Load-bearing auth check.
		h.logf("preview: subject %s denied access to %s", sub, userID)
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}

	t, found := h.Reg.Get(userID)
	if !found {
		http.Error(w, "no preview registered", http.StatusNotFound)
		return
	}

	// The iframe loads the app root with ?access_token=<jwt> (an iframe cannot
	// set an Authorization header). Persist that token as a path-scoped cookie
	// so the app's own same-origin subresource requests (JS/CSS/HMR) — which
	// carry no token — still authenticate. SameSite=None+Secure is required
	// because the initial navigation is cross-site (client origin → API origin).
	if qt := strings.TrimSpace(r.URL.Query().Get("access_token")); qt != "" {
		http.SetCookie(w, &http.Cookie{
			Name:     previewCookie,
			Value:    qt,
			Path:     "/preview/" + userID,
			HttpOnly: true,
			Secure:   true,
			SameSite: http.SameSiteNoneMode,
			MaxAge:   3600,
		})
	}

	target := &url.URL{Scheme: "http", Host: hostPort(t.Host, t.Port)}
	rp := &httputil.ReverseProxy{
		// FlushInterval = -1 flushes after every Write so preview servers
		// that stream (SSE, HMR update channels) reach the client promptly
		// without server-side buffering.
		FlushInterval: -1,
		Director: func(req *http.Request) {
			req.URL.Scheme = target.Scheme
			req.URL.Host = target.Host
			req.Host = target.Host
			// Strip the /preview/{userID} prefix; forward the remainder.
			req.URL.Path = rest
			// RawQuery is preserved from the inbound request as-is.
			req.Header.Set("X-Matrix-User", sub)
			// Standard X-Forwarded-Proto hygiene; previews sit behind the
			// public TLS front door.
			if req.TLS != nil {
				req.Header.Set("X-Forwarded-Proto", "https")
			} else {
				req.Header.Set("X-Forwarded-Proto", "http")
			}
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			h.logf("preview proxy error: %s %s: %v", r.Method, r.URL.Path, err)
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
	}
	rp.ServeHTTP(w, r)
}

// parsePreviewPath splits "/preview/{userID}/<rest>" into the userID and
// the leading-slash rest path ("/" when empty). Reports ok=false when the
// path isn't under /preview/ or carries no userID segment.
func parsePreviewPath(p string) (userID, rest string, ok bool) {
	const prefix = "/preview/"
	if !strings.HasPrefix(p, prefix) {
		return "", "", false
	}
	tail := p[len(prefix):]
	if tail == "" {
		return "", "", false
	}
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		userID = tail[:i]
		rest = tail[i:] // keeps the leading slash
	} else {
		userID = tail
		rest = "/"
	}
	if userID == "" {
		return "", "", false
	}
	return userID, rest, true
}

// hostPort joins host and port, bracketing IPv6 literals per RFC 3986
// §3.2.2 so the URL authority parses correctly.
func hostPort(host, port string) string {
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = "[" + host + "]"
	}
	if port == "" {
		return host
	}
	return host + ":" + port
}

// registerRequest is the JSON body for the internal register/deregister
// endpoint.
type registerRequest struct {
	UserID string `json:"user_id"`
	Host   string `json:"host"`
	Port   string `json:"port"`
}

// RegisterHandler returns the INTERNAL listener handler for preview target
// lifecycle. It is mounted behind the internal listener's token middleware
// in main.go, so it performs no auth of its own.
//
//	POST   /internal/preview/register  {"user_id","host","port"}  → register
//	DELETE /internal/preview/register  ?user_id=... | {"user_id"}  → deregister
func RegisterHandler(reg *Registry) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			var body registerRequest
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				http.Error(w, "invalid JSON body", http.StatusBadRequest)
				return
			}
			if body.UserID == "" || body.Host == "" {
				http.Error(w, "user_id and host are required", http.StatusBadRequest)
				return
			}
			reg.Register(body.UserID, Target{
				Host:         body.Host,
				Port:         body.Port,
				RegisteredAt: time.Now().UTC(),
			})
			writeOK(w)

		case http.MethodDelete:
			userID := r.URL.Query().Get("user_id")
			if userID == "" {
				// Fall back to a JSON body carrying {"user_id"}.
				var body registerRequest
				if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
					userID = body.UserID
				}
			}
			if userID == "" {
				http.Error(w, "user_id is required", http.StatusBadRequest)
				return
			}
			reg.Deregister(userID)
			writeOK(w)

		default:
			w.Header().Set("Allow", "POST, DELETE")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	})
}

func writeOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"ok":true}`))
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
