// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package auth verifies the gateway's bearer token + X-Matrix-Actor-DID
// header on every request. Future plan §5.15 line-item: ed25519 wallet
// signature verification across the request body — stubbed here behind
// VerifySignature, deliberately a no-op until the daemon-side wiring
// (core/mcl/llm canonical signing) lands.
//
// Concurrency: every exported function is pure or constructs a new
// authenticator; the *Authenticator value is itself immutable after
// construction and safe for concurrent use.
package auth

import (
	"bytes"
	"crypto/hmac"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"centra/gateway/internal/types"
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
	agentCoreKeys   map[string][]byte
	now             func() time.Time
}

// Options controls Authenticator behavior.
type Options struct {
	// Token is the shared secret expected in Authorization: Bearer ...
	// Empty + AllowEmptyToken=true → auth disabled (local-dev only).
	Token string

	// AllowEmptyToken explicitly opts into "no auth" when Token == "".
	// Defaults to false: an empty Token without this flag returns an
	// error from NewAuthenticator so misconfigured production deploys
	// fail fast instead of silently accepting traffic.
	AllowEmptyToken bool

	// AgentCoreVerificationKeys maps token key ids to HMAC-SHA256 keys.
	// Include the active key and any prior keys still inside their maximum
	// one-hour token lifetime. Keys are cloned during construction.
	AgentCoreVerificationKeys map[string][]byte

	// Now is the verification clock. nil uses time.Now.
	Now func() time.Time
}

// New constructs a new Authenticator from the supplied options.
func New(opts Options) (*Authenticator, error) {
	if opts.Token == "" && !opts.AllowEmptyToken {
		return nil, fmt.Errorf("gateway.auth: empty token but AllowEmptyToken=false")
	}
	keys := make(map[string][]byte, len(opts.AgentCoreVerificationKeys))
	for keyID, key := range opts.AgentCoreVerificationKeys {
		if !validKeyID(keyID) {
			return nil, fmt.Errorf("gateway.auth: invalid AgentCore verification key id")
		}
		if len(key) < 32 {
			return nil, fmt.Errorf("gateway.auth: AgentCore verification key %q must be at least 32 bytes", keyID)
		}
		keys[keyID] = append([]byte(nil), key...)
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Authenticator{
		token:           opts.Token,
		allowEmptyToken: opts.AllowEmptyToken,
		agentCoreKeys:   keys,
		now:             now,
	}, nil
}

// ErrMissingActor is returned when X-Matrix-Actor-DID is empty.
var ErrMissingActor = errors.New("gateway.auth: missing X-Matrix-Actor-DID header")

// ErrUnauthorized is returned when the bearer token is missing or
// fails constant-time comparison against the expected token.
var ErrUnauthorized = errors.New("gateway.auth: unauthorized")

// ErrMalformedActor is returned when the actor header is present but
// not a recognizable DID-shape ("did:" prefix, single colon-separated
// triple). The check is deliberately permissive — any string starting
// with "did:" passes; deeper validation is the wallet's job.
var ErrMalformedActor = errors.New("gateway.auth: malformed X-Matrix-Actor-DID")

// Principal is the authenticated authority for a gateway request. Scoped
// principals carry signed provider and model constraints; legacy principals
// retain the shared-token behavior and leave those constraints empty.
type Principal struct {
	Actor    string
	Slot     string
	Scoped   bool
	Provider string
	Model    string
}

// Authenticate verifies the request and returns its authenticated authority.
func (a *Authenticator) Authenticate(r *http.Request) (Principal, error) {
	if r == nil {
		return Principal{}, fmt.Errorf("gateway.auth: nil request")
	}
	supplied, hasBearer := bearerToken(r.Header.Get(types.HeaderAuthorization))
	if supplied == agentCoreTokenPrefix || strings.HasPrefix(supplied, agentCoreTokenPrefix+".") {
		return a.verifyAgentCore(r, supplied)
	}
	if !a.checkLegacyBearer(supplied, hasBearer) {
		return Principal{}, ErrUnauthorized
	}
	actor := strings.TrimSpace(r.Header.Get(types.HeaderActorDID))
	if actor == "" {
		return Principal{}, ErrMissingActor
	}
	if !looksLikeDID(actor) {
		return Principal{}, ErrMalformedActor
	}
	return Principal{
		Actor: actor,
		Slot:  strings.TrimSpace(r.Header.Get(types.HeaderSlot)),
	}, nil
}

// Verify checks the Authorization header against the configured token
// and the actor header for presence. Returns the actor DID on success.
//
// Returns one of: ErrUnauthorized, ErrMissingActor, ErrMalformedActor,
// or nil. Callers map these to HTTP 401 / 400 themselves so the
// package stays HTTP-framework agnostic.
func (a *Authenticator) Verify(r *http.Request) (actor string, err error) {
	principal, err := a.Authenticate(r)
	if err != nil {
		return "", err
	}
	return principal.Actor, nil
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
	// signing is added to core/mcl/llm. Until then, keep returning nil
	// so the gateway boundary already exercises this hook.
	return nil
}

func (a *Authenticator) checkLegacyBearer(supplied string, hasBearer bool) bool {
	if a.token == "" && a.allowEmptyToken {
		return true
	}
	if !hasBearer {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(supplied), []byte(a.token)) == 1
}

func bearerToken(header string) (string, bool) {
	header = strings.TrimSpace(header)
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	supplied := strings.TrimSpace(header[len(prefix):])
	return supplied, supplied != ""
}

func (a *Authenticator) verifyAgentCore(r *http.Request, token string) (Principal, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[0] != agentCoreTokenPrefix {
		return Principal{}, ErrUnauthorized
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return Principal{}, ErrUnauthorized
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || len(signature) != 32 {
		return Principal{}, ErrUnauthorized
	}
	var claims AgentCoreClaims
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&claims); err != nil {
		return Principal{}, ErrUnauthorized
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return Principal{}, ErrUnauthorized
	}
	key, ok := a.agentCoreKeys[claims.KeyID]
	if !ok {
		return Principal{}, ErrUnauthorized
	}
	want := signAgentCoreToken(key, parts[0]+"."+parts[1])
	if !hmac.Equal(signature, want) {
		return Principal{}, ErrUnauthorized
	}
	jti, jtiErr := base64.RawURLEncoding.DecodeString(claims.TokenID)
	if claims.Audience != AgentCoreAudience || claims.Scope != AgentCoreScope ||
		claims.Slot != AgentCoreSlot || claims.Provider != AgentCoreProvider ||
		claims.Model != AgentCoreModel || !looksLikeDID(claims.ActorDID) ||
		jtiErr != nil || len(jti) != 16 {
		return Principal{}, ErrUnauthorized
	}
	now := a.now().UTC()
	issuedAt := time.Unix(claims.IssuedAt, 0)
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt ||
		expiresAt.Sub(issuedAt) > AgentCoreMaxTokenTTL ||
		issuedAt.After(now.Add(agentCoreClockSkew)) || !now.Before(expiresAt) {
		return Principal{}, ErrUnauthorized
	}
	if r.Method != http.MethodPost || r.URL == nil || r.URL.Path != "/v1/chat/completions" {
		return Principal{}, ErrUnauthorized
	}
	actor := strings.TrimSpace(r.Header.Get(types.HeaderActorDID))
	slot := strings.TrimSpace(r.Header.Get(types.HeaderSlot))
	if actor != claims.ActorDID || slot != claims.Slot {
		return Principal{}, ErrUnauthorized
	}
	byo := strings.TrimSpace(r.Header.Get(types.HeaderBYOAPIKey))
	if strings.EqualFold(byo, "true") || byo == "1" || strings.EqualFold(byo, "yes") ||
		strings.TrimSpace(r.Header.Get(types.HeaderUserAPIKey)) != "" {
		return Principal{}, ErrUnauthorized
	}
	return Principal{
		Actor:    claims.ActorDID,
		Slot:     claims.Slot,
		Scoped:   true,
		Provider: claims.Provider,
		Model:    claims.Model,
	}, nil
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

// Copyright © 2026 Sidiora Labs. All rights reserved.
