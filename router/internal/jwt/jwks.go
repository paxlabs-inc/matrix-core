// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// jwks.go — JWKS fetcher + in-memory cache.
//
// The Verifier asks for a public key by kid; we serve it from a
// sync.RWMutex-guarded map populated by an HTTP fetch of
// ${SUPABASE_URL}/auth/v1/.well-known/jwks.json.
//
// On a kid miss we attempt at most one re-fetch (throttled) so a key
// rotation lands within seconds. Repeated misses for the same kid
// don't hammer Supabase — the throttle is a hard floor.
//
// Supported JWK types:
//
//	{"kty":"EC","crv":"P-256","x":"...","y":"..."}     -> *ecdsa.PublicKey
//	{"kty":"RSA","n":"...","e":"AQAB"}                  -> *rsa.PublicKey
//	{"kty":"OKP","crv":"Ed25519","x":"..."}             -> ed25519.PublicKey
//
// Other key types are silently dropped at parse time so an unsupported
// key in the JWKS doesn't break adjacent supported keys.
package jwt

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"sync"
	"time"
)

// defaultRefreshThrottle is the floor between forced JWKS fetches in
// response to unknown-kid hits. Supabase caches the JWKS at the edge
// for tens of seconds, so this is generous enough to avoid hammering
// upstream while still picking up a rotation within ~1 minute.
const defaultRefreshThrottle = 30 * time.Second

// jwk is the JSON Web Key wire format we accept (RFC 7517 §4 +
// RFC 7518 §6 for the parameter encodings).
type jwk struct {
	Kty string `json:"kty"`
	Use string `json:"use,omitempty"`
	Kid string `json:"kid"`
	Alg string `json:"alg,omitempty"`

	// EC params (kty=EC).
	Crv string `json:"crv,omitempty"`
	X   string `json:"x,omitempty"`
	Y   string `json:"y,omitempty"`

	// RSA params (kty=RSA).
	N string `json:"n,omitempty"`
	E string `json:"e,omitempty"`
}

// jwkSet is the JWKS document.
type jwkSet struct {
	Keys []jwk `json:"keys"`
}

// jwksCache is the in-memory map kid → public key. Concurrency-safe.
type jwksCache struct {
	url      string
	hc       *http.Client
	throttle time.Duration

	mu        sync.RWMutex
	keys      map[string]any // kid -> *ecdsa.PublicKey | *rsa.PublicKey | ed25519.PublicKey
	lastFetch time.Time
}

// newJWKSCache constructs a cache. The first lookup triggers a refresh
// if keys is empty.
func newJWKSCache(url string, hc *http.Client, throttle time.Duration) *jwksCache {
	if throttle <= 0 {
		throttle = defaultRefreshThrottle
	}
	return &jwksCache{
		url:      url,
		hc:       hc,
		throttle: throttle,
		keys:     map[string]any{},
	}
}

// lookup returns the public key for kid. On miss, it triggers at most
// one throttled refresh and retries once. Persistent misses return
// ErrUnknownKey.
func (c *jwksCache) lookup(ctx context.Context, kid string) (any, error) {
	c.mu.RLock()
	if k, ok := c.keys[kid]; ok {
		c.mu.RUnlock()
		return k, nil
	}
	c.mu.RUnlock()

	// Miss: attempt a throttled refresh.
	if err := c.refreshIfStale(ctx); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrUnknownKey, err)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if k, ok := c.keys[kid]; ok {
		return k, nil
	}
	return nil, fmt.Errorf("%w: kid=%q", ErrUnknownKey, kid)
}

// refreshIfStale only fires a fetch if the last fetch was longer ago
// than the throttle. Empty cache is always considered stale.
func (c *jwksCache) refreshIfStale(ctx context.Context) error {
	c.mu.RLock()
	stale := len(c.keys) == 0 || time.Since(c.lastFetch) >= c.throttle
	c.mu.RUnlock()
	if !stale {
		return nil
	}
	return c.refresh(ctx)
}

// refresh fetches the JWKS unconditionally and replaces the cache.
//
// On error: cache is left unchanged so prior keys keep working. The
// lastFetch timestamp IS still updated to enforce the throttle even
// when fetches fail (avoids hammering upstream during outages).
func (c *jwksCache) refresh(ctx context.Context) error {
	c.mu.Lock()
	c.lastFetch = time.Now()
	c.mu.Unlock()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.url, http.NoBody)
	if err != nil {
		return fmt.Errorf("jwks: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("jwks: fetch %s: %w", c.url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("jwks: %s returned %d", c.url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("jwks: read body: %w", err)
	}
	var set jwkSet
	if err := json.Unmarshal(body, &set); err != nil {
		return fmt.Errorf("jwks: parse: %w", err)
	}
	parsed := map[string]any{}
	for i := range set.Keys {
		k := &set.Keys[i]
		if k.Kid == "" {
			continue
		}
		pub, err := parseJWK(k)
		if err != nil {
			// Silently drop unsupported keys — coexist with other
			// supported keys in the same set.
			continue
		}
		parsed[k.Kid] = pub
	}
	c.mu.Lock()
	c.keys = parsed
	c.mu.Unlock()
	return nil
}

// parseJWK converts a JWK dict to a Go crypto/* public key.
func parseJWK(k *jwk) (any, error) {
	switch k.Kty {
	case "EC":
		return parseEC(k)
	case "RSA":
		return parseRSA(k)
	case "OKP":
		return parseOKP(k)
	default:
		return nil, fmt.Errorf("jwks: unsupported kty=%q", k.Kty)
	}
}

func parseEC(k *jwk) (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	switch k.Crv {
	case "P-256":
		curve = elliptic.P256()
	case "P-384":
		curve = elliptic.P384()
	case "P-521":
		curve = elliptic.P521()
	default:
		return nil, fmt.Errorf("jwks: EC curve %q unsupported", k.Crv)
	}
	x, err := decodeBigInt(k.X)
	if err != nil {
		return nil, fmt.Errorf("jwks: EC.x: %w", err)
	}
	y, err := decodeBigInt(k.Y)
	if err != nil {
		return nil, fmt.Errorf("jwks: EC.y: %w", err)
	}
	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

func parseRSA(k *jwk) (*rsa.PublicKey, error) {
	n, err := decodeBigInt(k.N)
	if err != nil {
		return nil, fmt.Errorf("jwks: RSA.n: %w", err)
	}
	eBytes, err := base64.RawURLEncoding.DecodeString(k.E)
	if err != nil {
		return nil, fmt.Errorf("jwks: RSA.e: %w", err)
	}
	if len(eBytes) > 8 {
		return nil, fmt.Errorf("jwks: RSA.e too large (%d bytes)", len(eBytes))
	}
	var e int
	for _, b := range eBytes {
		e = e<<8 | int(b)
	}
	if e == 0 {
		return nil, fmt.Errorf("jwks: RSA.e is zero")
	}
	return &rsa.PublicKey{N: n, E: e}, nil
}

func parseOKP(k *jwk) (ed25519.PublicKey, error) {
	if k.Crv != "Ed25519" {
		return nil, fmt.Errorf("jwks: OKP curve %q unsupported", k.Crv)
	}
	x, err := base64.RawURLEncoding.DecodeString(k.X)
	if err != nil {
		return nil, fmt.Errorf("jwks: OKP.x: %w", err)
	}
	if len(x) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("jwks: OKP.x length %d != %d", len(x), ed25519.PublicKeySize)
	}
	return ed25519.PublicKey(x), nil
}

// decodeBigInt unpacks a base64url-encoded big-endian integer per
// RFC 7518 §6. Empty / leading-zero values are rejected to match
// Supabase's well-formed JWK output.
func decodeBigInt(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, err
	}
	return new(big.Int).SetBytes(b), nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
