// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package mw bundles HTTP middleware: request logging, JWT auth,
// admin bearer auth, and the "rate-limit-shape" placeholders the
// rate_buckets table will eventually populate.
package mw

import (
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"matrix/router/internal/jwt"
	"matrix/router/internal/proxy"
)

// Logf is the log sink used by middleware. Cmd/main.go wires this to
// os.Stderr; callers can swap to a structured logger if desired.
type Logf func(format string, args ...interface{})

// JWT returns middleware that validates an Authorization: Bearer <jwt>
// header against the provided Verifier. On success, the verified
// Subject is stashed in request context via proxy.WithSubject.
//
// The Verifier internally dispatches on the JWT alg header:
// HS256 → legacy SUPABASE_JWT_SECRET; ES256/RS256/EdDSA → JWKS at
// SUPABASE_URL/auth/v1/.well-known/jwks.json. See router/internal/jwt.
//
// Failure mapping:
//
//	missing/empty header        -> 401 (unauthenticated)
//	malformed token             -> 401 (bad token)
//	expired                     -> 401 (refresh-needed)
//	bad signature               -> 401 (intentionally indistinguishable
//	                                    from "wrong key" to avoid
//	                                    leaking server config)
//
// Distinguishable error categories surface in the WWW-Authenticate
// header so a smart client can decide whether to refresh.
func JWT(v *jwt.Verifier, log Logf) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tok := bearerOf(r.Header.Get("Authorization"))
			if tok == "" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="matrix", error="missing_token"`)
				http.Error(w, "missing bearer token", http.StatusUnauthorized)
				return
			}
			claims, err := v.Verify(r.Context(), tok, time.Now())
			if err != nil {
				cat := "invalid_token"
				switch {
				case errors.Is(err, jwt.ErrExpired):
					cat = "expired_token"
				case errors.Is(err, jwt.ErrNotYetValid):
					cat = "not_yet_valid"
				}
				log("jwt reject: %v", err)
				w.Header().Set("WWW-Authenticate", `Bearer realm="matrix", error="`+cat+`"`)
				http.Error(w, "invalid token: "+cat, http.StatusUnauthorized)
				return
			}
			ctx := proxy.WithSubject(r.Context(), claims.Subject)
			ctx = proxy.WithEmail(ctx, claims.Email)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// Admin returns middleware that requires Authorization: Bearer
// <admin-token> matching token via constant-time compare. When token
// is empty the middleware returns 503 — operationally a misconfigured
// deploy SHOULD fail open-loud not silent-pass.
func Admin(token string, log Logf) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if token == "" {
				http.Error(w, "admin disabled (ROUTER_ADMIN_TOKEN unset)", http.StatusServiceUnavailable)
				return
			}
			supplied := bearerOf(r.Header.Get("Authorization"))
			if supplied == "" {
				w.Header().Set("WWW-Authenticate", `Bearer realm="matrix-admin"`)
				http.Error(w, "missing admin bearer token", http.StatusUnauthorized)
				return
			}
			if subtle.ConstantTimeCompare([]byte(supplied), []byte(token)) != 1 {
				log("admin reject: token mismatch")
				http.Error(w, "forbidden", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// AccessLog records method/path/status/latency for every request. The
// status capture uses a wrapped ResponseWriter; flushing is preserved
// so SSE responses still stream live.
func AccessLog(log Logf) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rw := &statusWriter{ResponseWriter: w, status: 200}
			next.ServeHTTP(rw, r)
			log("%s %s %d %s", r.Method, r.URL.Path, rw.status, time.Since(start))
		})
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusWriter) WriteHeader(code int) {
	if s.wrote {
		return
	}
	s.status = code
	s.wrote = true
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if !s.wrote {
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

// Flush forwards to the underlying ResponseWriter when it implements
// http.Flusher. Required for SSE.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack forwards to the underlying ResponseWriter when it implements
// http.Hijacker. Required for httputil.ReverseProxy WebSockets / long
// lived streams.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

// bearerOf extracts the token from "Bearer <tok>" with case-insensitive
// scheme match. Returns "" when the header is empty / wrong scheme.
func bearerOf(h string) string {
	if h == "" {
		return ""
	}
	const prefix = "Bearer "
	if len(h) <= len(prefix) {
		return ""
	}
	if !strings.EqualFold(h[:len(prefix)], prefix) {
		return ""
	}
	return strings.TrimSpace(h[len(prefix):])
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
