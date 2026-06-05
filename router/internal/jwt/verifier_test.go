// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package jwt

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// ---------- HS256 (legacy secret) ----------

func TestHS256RoundTrip(t *testing.T) {
	secret := []byte("legacy-supabase-secret")
	v, err := New(Options{LegacySecret: secret})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tok, err := SignHS256(&Claims{
		Subject: "alice", Expiry: now.Unix() + 60,
	}, secret)
	if err != nil {
		t.Fatalf("Sign: %v", err)
	}
	c, err := v.Verify(context.Background(), tok, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "alice" {
		t.Fatalf("subject: %q", c.Subject)
	}
}

func TestHS256BadSignature(t *testing.T) {
	v, _ := New(Options{LegacySecret: []byte("right")})
	tok, _ := SignHS256(&Claims{Subject: "x", Expiry: time.Now().Unix() + 60}, []byte("wrong"))
	_, err := v.Verify(context.Background(), tok, time.Now())
	if !errors.Is(err, ErrSignatureMismatch) {
		t.Fatalf("want ErrSignatureMismatch, got %v", err)
	}
}

func TestHS256NoBackend(t *testing.T) {
	// Verifier has only JWKS configured — HS256 token must be rejected.
	v, _ := New(Options{SupabaseURL: "http://example.invalid"})
	tok, _ := SignHS256(&Claims{Subject: "x", Expiry: time.Now().Unix() + 60}, []byte("k"))
	_, err := v.Verify(context.Background(), tok, time.Now())
	if !errors.Is(err, ErrNoBackend) {
		t.Fatalf("want ErrNoBackend, got %v", err)
	}
}

// ---------- ES256 (asymmetric via JWKS) ----------

func TestES256RoundTripWithJWKS(t *testing.T) {
	priv, kid, jwks := mustGenerateECJWKS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(jwks)
	}))
	defer srv.Close()

	v, err := New(Options{SupabaseURL: srv.URL})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := v.PrimeJWKS(context.Background()); err != nil {
		t.Fatalf("PrimeJWKS: %v", err)
	}

	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tok := mustSignES256(t, priv, kid, &Claims{
		Subject: "bob",
		Issuer:  strings.TrimRight(srv.URL, "/") + "/auth/v1",
		Expiry:  now.Unix() + 60,
	})
	c, err := v.Verify(context.Background(), tok, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "bob" {
		t.Fatalf("subject: %q", c.Subject)
	}
}

func TestES256BadIssuerRejected(t *testing.T) {
	priv, kid, jwks := mustGenerateECJWKS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwks)
	}))
	defer srv.Close()

	v, _ := New(Options{SupabaseURL: srv.URL})
	_ = v.PrimeJWKS(context.Background())
	now := time.Now()
	tok := mustSignES256(t, priv, kid, &Claims{
		Subject: "bob",
		Issuer:  "https://other-supabase.co/auth/v1",
		Expiry:  now.Unix() + 60,
	})
	_, err := v.Verify(context.Background(), tok, now)
	if err == nil || !strings.Contains(err.Error(), "iss=") {
		t.Fatalf("expected iss mismatch error, got %v", err)
	}
}

func TestES256UnknownKidTriggersOneRefresh(t *testing.T) {
	priv1, kid1, jwks1 := mustGenerateECJWKS(t)
	priv2, kid2, jwks2 := mustGenerateECJWKS(t)
	_ = priv1

	var fetches atomic.Int32
	var serveSecond atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fetches.Add(1)
		if serveSecond.Load() {
			_, _ = w.Write(jwks2)
		} else {
			_, _ = w.Write(jwks1)
		}
	}))
	defer srv.Close()

	v, _ := New(Options{SupabaseURL: srv.URL, RefreshThrottle: 1 * time.Millisecond})
	if err := v.PrimeJWKS(context.Background()); err != nil {
		t.Fatalf("prime: %v", err)
	}
	if fetches.Load() != 1 {
		t.Fatalf("after prime, fetches=%d", fetches.Load())
	}
	// Sign with key2 — kid not yet in cache; verifier should refresh.
	serveSecond.Store(true)
	time.Sleep(2 * time.Millisecond) // exceed throttle
	now := time.Now()
	tok := mustSignES256(t, priv2, kid2, &Claims{
		Subject: "carol",
		Issuer:  strings.TrimRight(srv.URL, "/") + "/auth/v1",
		Expiry:  now.Unix() + 60,
	})
	c, err := v.Verify(context.Background(), tok, now)
	if err != nil {
		t.Fatalf("Verify: %v (fetches=%d)", err, fetches.Load())
	}
	if c.Subject != "carol" {
		t.Fatalf("subject: %q", c.Subject)
	}
	// Should have refreshed exactly once on the kid miss.
	if fetches.Load() != 2 {
		t.Fatalf("fetches: got %d want 2", fetches.Load())
	}
	_ = kid1
}

// ---------- RS256 (alternate asymmetric path) ----------

func TestRS256RoundTrip(t *testing.T) {
	priv, kid, jwks := mustGenerateRSAJWKS(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwks)
	}))
	defer srv.Close()

	v, _ := New(Options{SupabaseURL: srv.URL})
	_ = v.PrimeJWKS(context.Background())
	now := time.Now()
	tok := mustSignRS256(t, priv, kid, &Claims{
		Subject: "dave",
		Issuer:  strings.TrimRight(srv.URL, "/") + "/auth/v1",
		Expiry:  now.Unix() + 60,
	})
	c, err := v.Verify(context.Background(), tok, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "dave" {
		t.Fatalf("sub: %q", c.Subject)
	}
}

// ---------- EdDSA ----------

func TestEdDSARoundTrip(t *testing.T) {
	pub, priv, kid, jwks := mustGenerateEd25519JWKS(t)
	_ = pub
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write(jwks)
	}))
	defer srv.Close()
	v, _ := New(Options{SupabaseURL: srv.URL})
	_ = v.PrimeJWKS(context.Background())
	now := time.Now()
	tok := mustSignEdDSA(t, priv, kid, &Claims{
		Subject: "eve",
		Issuer:  strings.TrimRight(srv.URL, "/") + "/auth/v1",
		Expiry:  now.Unix() + 60,
	})
	c, err := v.Verify(context.Background(), tok, now)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if c.Subject != "eve" {
		t.Fatalf("sub: %q", c.Subject)
	}
}

// ---------- expiry / nbf ----------

func TestExpired(t *testing.T) {
	v, _ := New(Options{LegacySecret: []byte("k")})
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tok, _ := SignHS256(&Claims{Subject: "x", Expiry: now.Unix() - 1}, []byte("k"))
	_, err := v.Verify(context.Background(), tok, now)
	if !errors.Is(err, ErrExpired) {
		t.Fatalf("want ErrExpired, got %v", err)
	}
}

func TestNotYetValid(t *testing.T) {
	v, _ := New(Options{LegacySecret: []byte("k")})
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tok, _ := SignHS256(&Claims{Subject: "x", NotBefore: now.Unix() + 60, Expiry: now.Unix() + 600}, []byte("k"))
	_, err := v.Verify(context.Background(), tok, now)
	if !errors.Is(err, ErrNotYetValid) {
		t.Fatalf("want ErrNotYetValid, got %v", err)
	}
}

func TestMalformed(t *testing.T) {
	v, _ := New(Options{LegacySecret: []byte("k")})
	for _, in := range []string{"", "abc", "a.b", "a.b.c.d"} {
		_, err := v.Verify(context.Background(), in, time.Now())
		if !errors.Is(err, ErrMalformed) {
			t.Fatalf("Verify(%q) want ErrMalformed got %v", in, err)
		}
	}
}

func TestAlgRejected(t *testing.T) {
	v, _ := New(Options{LegacySecret: []byte("k")})
	// alg=none token (cannot sign through SignHS256 — craft manually).
	hdr := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none","typ":"JWT"}`))
	pay := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"x"}`))
	tok := hdr + "." + pay + "."
	_, err := v.Verify(context.Background(), tok, time.Now())
	if err == nil || (!errors.Is(err, ErrAlgorithmRejected) && !errors.Is(err, ErrMalformed) && !errors.Is(err, ErrSignatureMismatch)) {
		t.Fatalf("alg=none must be rejected, got %v", err)
	}
}

func TestNewRequiresAtLeastOneBackend(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatal("expected error with no backends")
	}
}

// ---------- helpers ----------

func mustGenerateECJWKS(t *testing.T) (privKey *ecdsa.PrivateKey, keyID string, jwksJSON []byte) {
	t.Helper()
	priv, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("ecdsa.GenerateKey: %v", err)
	}
	kid := fmt.Sprintf("ec-%d", time.Now().UnixNano())
	x := pad32(priv.PublicKey.X.Bytes())
	y := pad32(priv.PublicKey.Y.Bytes())
	set := jwkSet{Keys: []jwk{{
		Kty: "EC", Crv: "P-256", Kid: kid, Alg: "ES256", Use: "sig",
		X: base64.RawURLEncoding.EncodeToString(x),
		Y: base64.RawURLEncoding.EncodeToString(y),
	}}}
	body, _ := json.Marshal(set)
	return priv, kid, body
}

func mustGenerateRSAJWKS(t *testing.T) (privKey *rsa.PrivateKey, keyID string, jwksJSON []byte) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa.GenerateKey: %v", err)
	}
	kid := fmt.Sprintf("rsa-%d", time.Now().UnixNano())
	n := priv.PublicKey.N.Bytes()
	e := big.NewInt(int64(priv.PublicKey.E)).Bytes()
	set := jwkSet{Keys: []jwk{{
		Kty: "RSA", Kid: kid, Alg: "RS256", Use: "sig",
		N: base64.RawURLEncoding.EncodeToString(n),
		E: base64.RawURLEncoding.EncodeToString(e),
	}}}
	body, _ := json.Marshal(set)
	return priv, kid, body
}

func mustGenerateEd25519JWKS(t *testing.T) (pubKey ed25519.PublicKey, privKey ed25519.PrivateKey, keyID string, jwksJSON []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey: %v", err)
	}
	kid := fmt.Sprintf("ed-%d", time.Now().UnixNano())
	set := jwkSet{Keys: []jwk{{
		Kty: "OKP", Crv: "Ed25519", Kid: kid, Alg: "EdDSA", Use: "sig",
		X: base64.RawURLEncoding.EncodeToString(pub),
	}}}
	body, _ := json.Marshal(set)
	return pub, priv, kid, body
}

func mustSignES256(t *testing.T, priv *ecdsa.PrivateKey, kid string, c *Claims) string {
	t.Helper()
	hdr := header{Alg: "ES256", Typ: "JWT", Kid: kid}
	tok, err := signWithHeader(&hdr, c, func(input []byte) ([]byte, error) {
		h := sha256.Sum256(input)
		r, s, err := ecdsa.Sign(rand.Reader, priv, h[:])
		if err != nil {
			return nil, err
		}
		out := make([]byte, 64)
		copy(out[32-len(r.Bytes()):32], r.Bytes())
		copy(out[64-len(s.Bytes()):64], s.Bytes())
		return out, nil
	})
	if err != nil {
		t.Fatalf("sign ES256: %v", err)
	}
	return tok
}

func mustSignRS256(t *testing.T, priv *rsa.PrivateKey, kid string, c *Claims) string {
	t.Helper()
	hdr := header{Alg: "RS256", Typ: "JWT", Kid: kid}
	tok, err := signWithHeader(&hdr, c, func(input []byte) ([]byte, error) {
		h := sha256.Sum256(input)
		return rsa.SignPKCS1v15(rand.Reader, priv, crypto.SHA256, h[:])
	})
	if err != nil {
		t.Fatalf("sign RS256: %v", err)
	}
	return tok
}

func mustSignEdDSA(t *testing.T, priv ed25519.PrivateKey, kid string, c *Claims) string {
	t.Helper()
	hdr := header{Alg: "EdDSA", Typ: "JWT", Kid: kid}
	tok, err := signWithHeader(&hdr, c, func(input []byte) ([]byte, error) {
		return ed25519.Sign(priv, input), nil
	})
	if err != nil {
		t.Fatalf("sign EdDSA: %v", err)
	}
	return tok
}

func pad32(b []byte) []byte {
	if len(b) == 32 {
		return b
	}
	out := make([]byte, 32)
	copy(out[32-len(b):], b)
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
