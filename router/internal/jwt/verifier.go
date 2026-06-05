// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package jwt verifies Supabase-issued access tokens.
//
// Supabase is mid-migration between two JWT signing schemes:
//
//  1. Legacy HS256 with a shared "JWT secret" (the project's
//     SUPABASE_JWT_SECRET). All tokens issued today are HS256.
//
//  2. Asymmetric ES256 / RS256 / EdDSA using per-project key pairs;
//     public keys are published at
//     ${SUPABASE_URL}/auth/v1/.well-known/jwks.json. As Supabase
//     rotates projects to asymmetric, tokens with these algs start
//     appearing alongside the HS256 ones.
//
// The Verifier accepts either: it dispatches on the JWT header's
// "alg" field, looks up the right key, and verifies. Tokens are
// rejected if the configured backend for that alg is missing
// (e.g. an ES256 token reaches a router that has no SUPABASE_URL).
//
// External deps: zero. crypto/hmac + crypto/sha256 + crypto/ecdsa +
// crypto/rsa + crypto/ed25519 + math/big + encoding/base64 +
// encoding/json. The trust surface stays Go stdlib.
package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"strings"
	"time"
)

// Sentinel errors. Callers should errors.Is to distinguish "this token
// was never going to verify" (Malformed/SignatureMismatch/AlgorithmRejected)
// from "this token was once valid but isn't now"
// (Expired/NotYetValid).
var (
	ErrMalformed         = errors.New("jwt: malformed token")
	ErrAlgorithmRejected = errors.New("jwt: alg unsupported or rejected")
	ErrSignatureMismatch = errors.New("jwt: signature mismatch")
	ErrExpired           = errors.New("jwt: expired")
	ErrNotYetValid       = errors.New("jwt: not yet valid (nbf in future)")
	ErrMissingSubject    = errors.New("jwt: sub claim missing")
	ErrUnknownKey        = errors.New("jwt: kid not found in JWKS")
	ErrNoBackend         = errors.New("jwt: no verification backend for alg")
)

// Claims captures the subset of Supabase JWT claims matrix-router
// needs. We accept extra fields (json.Unmarshal ignores them); rate
// limits / role come from our own DB row, NOT the JWT.
type Claims struct {
	Subject   string `json:"sub"`
	Email     string `json:"email,omitempty"`
	Audience  string `json:"aud,omitempty"`
	Issuer    string `json:"iss,omitempty"`
	Role      string `json:"role,omitempty"`
	IssuedAt  int64  `json:"iat,omitempty"`
	NotBefore int64  `json:"nbf,omitempty"`
	Expiry    int64  `json:"exp,omitempty"`
}

// header is the parsed JOSE header. We inspect alg + kid + typ.
type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	Kid string `json:"kid,omitempty"`
}

// Verifier holds the configured verification backends. At least one of
// LegacySecret / JWKSURL must be non-empty for any token to verify.
//
// Concurrency: Verify is safe to call from many goroutines. The JWKS
// cache uses a sync.RWMutex internally (see jwks.go).
type Verifier struct {
	// LegacySecret is the Supabase project's "JWT secret" used for
	// HS256 tokens. Empty disables HS256 verification.
	legacySecret []byte

	// expectedIssuer, when non-empty, is checked against the iss claim.
	// For Supabase the canonical issuer is
	// "<SUPABASE_URL>/auth/v1". Empty disables issuer enforcement.
	expectedIssuer string

	// JWKS state lives in jwks.go; nil disables JWKS verification.
	jwks *jwksCache

	// hc is the HTTP client used for JWKS fetches. Configurable for tests.
	hc *http.Client
}

// Options controls Verifier construction.
type Options struct {
	// LegacySecret is SUPABASE_JWT_SECRET (raw bytes). Optional once
	// Supabase finishes the asymmetric migration; required during the
	// overlap window because all HS256-issued tokens that haven't
	// expired yet need this to verify.
	LegacySecret []byte

	// SupabaseURL is the project URL, e.g.
	// "https://zezsqawedbikldiedlse.supabase.co". The JWKS endpoint
	// and expected issuer are derived from it. Empty disables the
	// asymmetric path.
	SupabaseURL string

	// HTTPClient overrides the default for JWKS fetches; nil =
	// http.DefaultClient with a 10s timeout.
	HTTPClient *http.Client

	// RefreshThrottle bounds how often the verifier will re-fetch the
	// JWKS in response to an unknown-kid hit. Zero = 30s default.
	RefreshThrottle time.Duration
}

// New constructs a Verifier. At least one of opts.LegacySecret or
// opts.SupabaseURL must be set, else error.
func New(opts Options) (*Verifier, error) {
	if len(opts.LegacySecret) == 0 && opts.SupabaseURL == "" {
		return nil, errors.New("jwt: at least one of LegacySecret or SupabaseURL is required")
	}
	hc := opts.HTTPClient
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Second}
	}
	v := &Verifier{
		legacySecret: append([]byte(nil), opts.LegacySecret...),
		hc:           hc,
	}
	if opts.SupabaseURL != "" {
		base := strings.TrimRight(opts.SupabaseURL, "/")
		v.jwks = newJWKSCache(base+"/auth/v1/.well-known/jwks.json", hc, opts.RefreshThrottle)
		v.expectedIssuer = base + "/auth/v1"
	}
	return v, nil
}

// PrimeJWKS does an eager fetch so first-request latency doesn't pay
// the network round-trip. Errors are returned but should be logged
// and ignored by callers — the Verifier will lazy-fetch on first
// asymmetric token hit if priming failed (e.g. transient DNS).
func (v *Verifier) PrimeJWKS(ctx context.Context) error {
	if v.jwks == nil {
		return nil
	}
	return v.jwks.refresh(ctx)
}

// Verify parses raw, dispatches on the alg header, runs signature +
// time checks, and returns the parsed Claims on success.
//
// Issuer enforcement: if the verifier was constructed with a non-empty
// SupabaseURL, the iss claim must match "<SupabaseURL>/auth/v1".
// Tokens forwarded from a different Supabase project will be rejected
// even if the algorithm is supported.
func (v *Verifier) Verify(ctx context.Context, raw string, now time.Time) (*Claims, error) {
	if raw == "" {
		return nil, ErrMalformed
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, fmt.Errorf("%w: want 3 segments, got %d", ErrMalformed, len(parts))
	}
	hdrBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("%w: header decode: %v", ErrMalformed, err)
	}
	var hdr header
	if err := json.Unmarshal(hdrBytes, &hdr); err != nil {
		return nil, fmt.Errorf("%w: header parse: %v", ErrMalformed, err)
	}
	if hdr.Typ != "" && hdr.Typ != "JWT" {
		return nil, fmt.Errorf("%w: typ=%q", ErrMalformed, hdr.Typ)
	}

	signingInput := []byte(raw[:len(parts[0])+1+len(parts[1])])
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("%w: sig decode: %v", ErrMalformed, err)
	}

	switch hdr.Alg {
	case "HS256":
		if len(v.legacySecret) == 0 {
			return nil, fmt.Errorf("%w: HS256 received but LegacySecret unset", ErrNoBackend)
		}
		mac := hmac.New(sha256.New, v.legacySecret)
		mac.Write(signingInput)
		if !hmac.Equal(mac.Sum(nil), sig) {
			return nil, ErrSignatureMismatch
		}
	case "RS256", "ES256", "EdDSA":
		if v.jwks == nil {
			return nil, fmt.Errorf("%w: %s received but SupabaseURL unset", ErrNoBackend, hdr.Alg)
		}
		if hdr.Kid == "" {
			return nil, fmt.Errorf("%w: %s requires kid header", ErrMalformed, hdr.Alg)
		}
		key, err := v.jwks.lookup(ctx, hdr.Kid)
		if err != nil {
			return nil, err
		}
		if err := verifyAsymmetric(hdr.Alg, key, signingInput, sig); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: alg=%q", ErrAlgorithmRejected, hdr.Alg)
	}

	// Decode payload AFTER signature verification.
	payloadBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("%w: payload decode: %v", ErrMalformed, err)
	}
	var c Claims
	if err := json.Unmarshal(payloadBytes, &c); err != nil {
		return nil, fmt.Errorf("%w: payload parse: %v", ErrMalformed, err)
	}
	if c.Subject == "" {
		return nil, ErrMissingSubject
	}
	if c.Expiry > 0 && now.Unix() >= c.Expiry {
		return nil, ErrExpired
	}
	if c.NotBefore > 0 && now.Unix() < c.NotBefore {
		return nil, ErrNotYetValid
	}
	if v.expectedIssuer != "" && c.Issuer != "" && c.Issuer != v.expectedIssuer {
		return nil, fmt.Errorf("%w: iss=%q want %q", ErrSignatureMismatch, c.Issuer, v.expectedIssuer)
	}
	return &c, nil
}

// verifyAsymmetric runs the algorithm-specific verify step. key is the
// crypto/* PublicKey loaded from JWKS (kty=RSA → *rsa.PublicKey;
// kty=EC → *ecdsa.PublicKey; kty=OKP+crv=Ed25519 → ed25519.PublicKey).
func verifyAsymmetric(alg string, key any, signingInput, sig []byte) error {
	switch alg {
	case "RS256":
		pub, ok := key.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: RS256 needs RSA key, got %T", ErrAlgorithmRejected, key)
		}
		h := sha256.Sum256(signingInput)
		if err := rsa.VerifyPKCS1v15(pub, crypto.SHA256, h[:], sig); err != nil {
			return fmt.Errorf("%w: %v", ErrSignatureMismatch, err)
		}
		return nil
	case "ES256":
		pub, ok := key.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("%w: ES256 needs ECDSA key, got %T", ErrAlgorithmRejected, key)
		}
		// ES256 sig is r||s, each 32 bytes (P-256), big-endian.
		if len(sig) != 64 {
			return fmt.Errorf("%w: ES256 sig length %d != 64", ErrMalformed, len(sig))
		}
		r := new(big.Int).SetBytes(sig[:32])
		s := new(big.Int).SetBytes(sig[32:])
		h := sha256.Sum256(signingInput)
		if !ecdsa.Verify(pub, h[:], r, s) {
			return ErrSignatureMismatch
		}
		return nil
	case "EdDSA":
		pub, ok := key.(ed25519.PublicKey)
		if !ok {
			return fmt.Errorf("%w: EdDSA needs Ed25519 key, got %T", ErrAlgorithmRejected, key)
		}
		if !ed25519.Verify(pub, signingInput, sig) {
			return ErrSignatureMismatch
		}
		return nil
	default:
		return fmt.Errorf("%w: %s not handled by verifyAsymmetric", ErrAlgorithmRejected, alg)
	}
}

// SignHS256 produces a Supabase-shape HS256 JWT for testing. Only used
// in the test suite; never referenced by production code paths.
func SignHS256(c *Claims, secret []byte) (string, error) {
	hdr := header{Alg: "HS256", Typ: "JWT"}
	return signWithHeader(&hdr, c, func(input []byte) ([]byte, error) {
		mac := hmac.New(sha256.New, secret)
		mac.Write(input)
		return mac.Sum(nil), nil
	})
}

// signWithHeader is the shared marshaling routine for the test
// helpers. signFn closes over whatever key material is needed.
func signWithHeader(hdr *header, c *Claims, signFn func([]byte) ([]byte, error)) (string, error) {
	hdrJSON, err := json.Marshal(hdr)
	if err != nil {
		return "", err
	}
	payJSON, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	hp := base64.RawURLEncoding.EncodeToString(hdrJSON)
	pp := base64.RawURLEncoding.EncodeToString(payJSON)
	signingInput := hp + "." + pp
	sig, err := signFn([]byte(signingInput))
	if err != nil {
		return "", err
	}
	sp := base64.RawURLEncoding.EncodeToString(sig)
	return signingInput + "." + sp, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
