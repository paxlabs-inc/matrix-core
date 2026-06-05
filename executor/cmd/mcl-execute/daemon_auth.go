// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_auth.go — auth middleware for the user-facing route surface
// (sess#27).
//
// Trust model (sess#27 Lock #B = "trust router headers + verify JWT
// signature when present"):
//
//   1. The router validates the Supabase JWT, strips the Authorization
//      header, and injects X-Matrix-User (the supabase user id) before
//      forwarding (router/internal/proxy/proxy.go:201-211). The daemon
//      receives the user id pre-authenticated by the router.
//
//   2. The daemon trusts X-Matrix-User but ALSO re-verifies the JWT
//      signature when the router is configured to forward the original
//      bearer in X-Matrix-JWT (defense in depth, near-zero CPU cost).
//      Forward-compatible: the router does not currently emit this
//      header, so the verify step is best-effort and gated on header
//      presence.
//
//   3. The legacy MATRIX_DAEMON_TOKEN bearer auth (CLI / scripts /
//      local dev) is preserved for the `/shutdown`, `/messages` (sync
//      CLI path), and pre-existing routes via daemonState.requireAuth.
//      Privileged admin-only routes (`/shutdown`) require the legacy
//      token regardless of router headers.
//
// Headers consumed:
//
//   X-Matrix-User   supabase user id (uuid string)
//                   injected by router/internal/proxy/proxy.go
//   X-Matrix-JWT    optional original bearer for HS256 re-verify
//                   (router would forward this in defense-in-depth mode)
//   Authorization   legacy "Bearer <MATRIX_DAEMON_TOKEN>" CLI auth
//
// Auth-disabled mode: when authToken == "" AND jwtSecret == nil (local
// dev), all requests pass with userID = X-Matrix-User if present, else
// the daemon's bound actor URI.

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// authCtxKey is the context key family for auth-derived state.
type authCtxKey int

const (
	// ctxKeyUserID stores the X-Matrix-User value (supabase uuid).
	ctxKeyUserID authCtxKey = iota + 100
	// ctxKeyAdmin marks the request as admin-authenticated (legacy
	// MATRIX_DAEMON_TOKEN bearer was presented). Admin requests bypass
	// per-user binding checks (e.g. allowed to call /shutdown).
	ctxKeyAdmin
)

// userIDFromContext retrieves the request's authenticated supabase
// user id, or "" when unauthenticated (auth-disabled mode).
func userIDFromContext(ctx context.Context) string {
	if v, ok := ctx.Value(ctxKeyUserID).(string); ok {
		return v
	}
	return ""
}

// isAdminFromContext reports whether the request was admin-auth'd.
func isAdminFromContext(ctx context.Context) bool {
	if v, ok := ctx.Value(ctxKeyAdmin).(bool); ok {
		return v
	}
	return false
}

// authPolicy controls per-route authentication strictness.
type authPolicy int

const (
	// authAny accepts EITHER router-header trust OR the legacy bearer
	// token. The default for user-facing routes (/messages, /events,
	// /intents/*, /memory/*, /skills/*, /tools/*).
	authAny authPolicy = iota
	// authAdmin requires the legacy MATRIX_DAEMON_TOKEN bearer (CLI/
	// admin only). Used for /shutdown and debug routes.
	authAdmin
	// authPublic disables auth entirely. Used for /healthz and
	// /version which are reachable for ops liveness probes.
	authPublic
)

// requireAuthPolicy gates a request per the supplied policy. Returns
// (ctx, true) on success; on failure writes the 401/403 and returns
// (nil, false). Callers MUST return after a false result.
//
// On success, ctx carries ctxKeyUserID + ctxKeyAdmin so route handlers
// can attribute the call to a user without re-parsing headers.
func (d *daemonState) requireAuthPolicy(w http.ResponseWriter, r *http.Request, policy authPolicy) (context.Context, bool) {
	switch policy {
	case authPublic:
		return r.Context(), true
	case authAdmin:
		if d.authToken == "" {
			// Auth-disabled local dev: still allow admin routes (the
			// operator has explicitly opted out by leaving the token
			// blank).
			return r.Context(), true
		}
		got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
		if got == "" || got != d.authToken {
			writeJSON(w, http.StatusUnauthorized, map[string]string{
				"error": "admin auth required (MATRIX_DAEMON_TOKEN)",
			})
			return nil, false
		}
		ctx := context.WithValue(r.Context(), ctxKeyAdmin, true)
		return ctx, true
	}

	// authAny path: prefer router-header trust, fall back to legacy
	// bearer.
	uid := strings.TrimSpace(r.Header.Get("X-Matrix-User"))
	if uid != "" {
		// Router has already validated the JWT. Optional defense-in-
		// depth: re-verify the signature when X-Matrix-JWT is forwarded.
		if d.jwtSecret != nil && len(d.jwtSecret) > 0 {
			if jwtRaw := r.Header.Get("X-Matrix-JWT"); jwtRaw != "" {
				if err := verifyHS256(jwtRaw, d.jwtSecret, time.Now()); err != nil {
					writeJSON(w, http.StatusUnauthorized, map[string]string{
						"error": "jwt verify: " + err.Error(),
					})
					return nil, false
				}
			}
		}
		// Single-machine-per-user invariant: the X-Matrix-User MUST
		// match this daemon's bound actor when boundUserID is set.
		// Mismatch means the router routed the wrong machine — this
		// is a deployment bug, not user error; reject with 403 so the
		// frontend knows it's not a transient retry case.
		if d.boundUserID != "" && uid != d.boundUserID {
			writeJSON(w, http.StatusForbidden, map[string]string{
				"error": fmt.Sprintf("user mismatch: this daemon serves %q, request asks for %q", d.boundUserID, uid),
			})
			return nil, false
		}
		ctx := context.WithValue(r.Context(), ctxKeyUserID, uid)
		return ctx, true
	}

	// Legacy bearer fallback (CLI / scripts / local dev).
	if d.authToken == "" {
		// Auth fully disabled — allow with empty userID.
		return r.Context(), true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if got == "" || got != d.authToken {
		writeJSON(w, http.StatusUnauthorized, map[string]string{
			"error": "auth required: present X-Matrix-User (router-forwarded) or Authorization Bearer (admin)",
		})
		return nil, false
	}
	ctx := context.WithValue(r.Context(), ctxKeyAdmin, true)
	return ctx, true
}

// verifyHS256 checks a Supabase HS256 JWT signature + expiry against
// the supplied secret. Stdlib-only (no external dep) so the executor
// module doesn't need to pull in router/internal/jwt across a module
// boundary.
//
// Validates:
//   - 3-segment dot-separated JWS shape
//   - alg=HS256 (symmetric)
//   - hmac(sha256, secret) signature
//   - exp claim (must be in the future)
//   - nbf claim (must be in the past, when set)
//
// Does NOT enforce iss/aud/sub presence — that's the router's job at
// the trust boundary; the daemon's verify is purely "this token wasn't
// tampered with".
func verifyHS256(raw string, secret []byte, now time.Time) error {
	if raw == "" {
		return fmt.Errorf("empty token")
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return fmt.Errorf("malformed: want 3 segments got %d", len(parts))
	}
	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return fmt.Errorf("header decode: %v", err)
	}
	var hdr struct {
		Alg string `json:"alg"`
		Typ string `json:"typ"`
	}
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return fmt.Errorf("header parse: %v", err)
	}
	if hdr.Alg != "HS256" {
		return fmt.Errorf("alg=%q (only HS256 supported in daemon-side verify)", hdr.Alg)
	}
	if hdr.Typ != "" && hdr.Typ != "JWT" {
		return fmt.Errorf("typ=%q", hdr.Typ)
	}

	signingInput := []byte(parts[0] + "." + parts[1])
	gotSig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return fmt.Errorf("sig decode: %v", err)
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(signingInput)
	if !hmac.Equal(mac.Sum(nil), gotSig) {
		return fmt.Errorf("signature mismatch")
	}

	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fmt.Errorf("payload decode: %v", err)
	}
	var claims struct {
		Exp int64  `json:"exp"`
		Nbf int64  `json:"nbf"`
		Sub string `json:"sub"`
	}
	if err := json.Unmarshal(payloadBytes, &claims); err != nil {
		return fmt.Errorf("payload parse: %v", err)
	}
	if claims.Exp > 0 && now.Unix() >= claims.Exp {
		return fmt.Errorf("expired (exp=%d)", claims.Exp)
	}
	if claims.Nbf > 0 && now.Unix() < claims.Nbf {
		return fmt.Errorf("not yet valid (nbf=%d)", claims.Nbf)
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
