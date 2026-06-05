// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package auth verifies the gateway's bearer token + X-Matrix-Actor-DID
// header on every request. Future plan §5.15 line-item: ed25519 wallet
// signature verification across the request body — stubbed here behind
// VerifySignature, deliberately a no-op until the daemon-side wiring
// (MCL/llm canonical signing) lands.
//
// Concurrency: every exported function is pure or constructs a new
// authenticator; the *Authenticator value is itself immutable after
// construction and safe for concurrent use.
package auth

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"matrix/gateway/internal/types"
)

// Authenticator validates the gateway shared bearer token and the
// X-Matrix-Actor-DID actor header. Construct once per process via
// NewAuthenticator; safe for concurrent use.
type Authenticator struct {
	// token is the expected MATRIX_GATEWAY_TOKEN value. Compared with
	// constant-time equality to avoid leaking length-class via timing.
	token string

	// allowEmptyToken keeps local-dev posture viable: when true AND
	// token == "", every request is accepted. Production callers MUST
	// construct with allowEmptyToken=false (the default) so a misconfig
	// can never silently disable auth.
	allowEmptyToken bool
}

// Options controls Authenticator behaviour.
type Options struct {
	// Token is the shared secret expected in Authorization: Bearer ...
	// Empty + AllowEmptyToken=true → auth disabled (local-dev only).
	Token string

	// AllowEmptyToken explicitly opts into "no auth" when Token == "".
	// Defaults to false: an empty Token without this flag returns an
	// error from NewAuthenticator so misconfigured production deploys
	// fail fast instead of silently accepting traffic.
	AllowEmptyToken bool
}

// New constructs a new Authenticator from the supplied options.
func New(opts Options) (*Authenticator, error) {
	if opts.Token == "" && !opts.AllowEmptyToken {
		return nil, fmt.Errorf("gateway.auth: empty token but AllowEmptyToken=false")
	}
	return &Authenticator{
		token:           opts.Token,
		allowEmptyToken: opts.AllowEmptyToken,
	}, nil
}

// ErrMissingActor is returned when X-Matrix-Actor-DID is empty.
var ErrMissingActor = errors.New("gateway.auth: missing X-Matrix-Actor-DID header")

// ErrUnauthorized is returned when the bearer token is missing or
// fails constant-time comparison against the expected token.
var ErrUnauthorized = errors.New("gateway.auth: unauthorized")

// ErrMalformedActor is returned when the actor header is present but
// not a recognisable DID-shape ("did:" prefix, single colon-separated
// triple). The check is deliberately permissive — any string starting
// with "did:" passes; deeper validation is the wallet's job.
var ErrMalformedActor = errors.New("gateway.auth: malformed X-Matrix-Actor-DID")

// Verify checks the Authorization header against the configured token
// and the actor header for presence. Returns the actor DID on success.
//
// Returns one of: ErrUnauthorized, ErrMissingActor, ErrMalformedActor,
// or nil. Callers map these to HTTP 401 / 400 themselves so the
// package stays HTTP-framework agnostic.
func (a *Authenticator) Verify(r *http.Request) (actor string, err error) {
	if r == nil {
		return "", fmt.Errorf("gateway.auth: nil request")
	}
	if !a.checkBearer(r.Header.Get(types.HeaderAuthorization)) {
		return "", ErrUnauthorized
	}
	actor = strings.TrimSpace(r.Header.Get(types.HeaderActorDID))
	if actor == "" {
		return "", ErrMissingActor
	}
	if !looksLikeDID(actor) {
		return "", ErrMalformedActor
	}
	return actor, nil
}

// VerifySignature is a placeholder for future ed25519 wallet
// verification across the canonical request body. The plan §5.15
// reserves this surface; v1 returns nil unconditionally so wiring
// can land without forcing daemon-side signing in the same change.
//
// When implemented, it will:
//  1. Read Authorization-Signature: ed25519=<base58> from the request
//  2. Resolve the wallet pubkey for the actor DID (from a cache
//     populated by /wallet/keys, or via a future on-chain lookup)
//  3. Recompute canonical body bytes and verify the signature
//
// Until then, callers should still invoke it so the boundary check
// is wired in advance.
func (a *Authenticator) VerifySignature(_ *http.Request, _ string) error {
	// TODO(sess#32+): wire ed25519 verification when daemon-side
	// signing is added to MCL/llm. Until then, keep returning nil
	// so the gateway boundary already exercises this hook.
	return nil
}

func (a *Authenticator) checkBearer(header string) bool {
	if a.token == "" && a.allowEmptyToken {
		return true
	}
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return false
	}
	supplied := strings.TrimSpace(header[len(prefix):])
	if supplied == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(a.token)) == 1
}

// looksLikeDID is a deliberately permissive shape check. The DID Core
// spec allows arbitrary methods + identifiers; we only enforce the
// "did:<method>:<id>" triple structure. Heavy validation is wallet
// responsibility.
func looksLikeDID(s string) bool {
	if !strings.HasPrefix(s, "did:") {
		return false
	}
	parts := strings.SplitN(s, ":", 3)
	if len(parts) != 3 {
		return false
	}
	if parts[1] == "" || parts[2] == "" {
		return false
	}
	return true
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
