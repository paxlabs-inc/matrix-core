// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package auth

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"matrix/gateway/internal/types"
)

var agentCoreTestNow = time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)

func newAgentCoreAuth(t *testing.T, keyID string, key []byte, now time.Time) (*Authenticator, string) {
	t.Helper()
	issuer, err := NewAgentCoreIssuer(AgentCoreIssuerOptions{
		KeyID: keyID,
		Key:   key,
		Now:   func() time.Time { return agentCoreTestNow },
		Rand:  bytes.NewReader(bytes.Repeat([]byte{7}, 16)),
	})
	if err != nil {
		t.Fatalf("NewAgentCoreIssuer: %v", err)
	}
	token, _, err := issuer.Mint("did:matrix:user-1:cody", AgentCoreTokenTTL)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	authenticator, err := New(Options{
		Token:                     "legacy",
		AgentCoreVerificationKeys: map[string][]byte{keyID: key},
		Now:                       func() time.Time { return now },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return authenticator, token
}

func agentCoreRequest(token string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", http.NoBody)
	request.Header.Set(types.HeaderAuthorization, "Bearer "+token)
	request.Header.Set(types.HeaderActorDID, "did:matrix:user-1:cody")
	request.Header.Set(types.HeaderSlot, AgentCoreSlot)
	return request
}

func signedAgentCoreTestToken(t *testing.T, key []byte, claims AgentCoreClaims) string {
	t.Helper()
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signed := agentCoreTokenPrefix + "." + base64.RawURLEncoding.EncodeToString(payload)
	signature := signAgentCoreToken(key, signed)
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func newAuth(t *testing.T) *Authenticator {
	t.Helper()
	a, err := New(Options{Token: "shh"})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return a
}

func TestVerifyAcceptsValidBearer(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", http.NoBody)
	r.Header.Set(types.HeaderAuthorization, "Bearer shh")
	r.Header.Set(types.HeaderActorDID, "did:pax:abcdef")
	r.Header.Set(types.HeaderSlot, types.SlotExecutor)
	actor, err := a.Verify(r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if actor != "did:pax:abcdef" {
		t.Fatalf("actor mismatch: %q", actor)
	}
	principal, err := a.Authenticate(r)
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.Actor != actor || principal.Slot != types.SlotExecutor || principal.Scoped ||
		principal.Provider != "" || principal.Model != "" {
		t.Fatalf("legacy principal=%+v", principal)
	}
}

func TestVerifyRejectsBadToken(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", http.NoBody)
	r.Header.Set(types.HeaderAuthorization, "Bearer wrong")
	r.Header.Set(types.HeaderActorDID, "did:pax:abc")
	_, err := a.Verify(r)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestVerifyRejectsMissingActor(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", http.NoBody)
	r.Header.Set(types.HeaderAuthorization, "Bearer shh")
	_, err := a.Verify(r)
	if !errors.Is(err, ErrMissingActor) {
		t.Fatalf("expected ErrMissingActor, got %v", err)
	}
}

func TestVerifyRejectsMalformedActor(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", http.NoBody)
	r.Header.Set(types.HeaderAuthorization, "Bearer shh")
	r.Header.Set(types.HeaderActorDID, "not-a-did")
	_, err := a.Verify(r)
	if !errors.Is(err, ErrMalformedActor) {
		t.Fatalf("expected ErrMalformedActor, got %v", err)
	}
}

func TestEmptyTokenWithoutAllowFails(t *testing.T) {
	if _, err := New(Options{}); err == nil {
		t.Fatalf("expected error: empty token + AllowEmptyToken=false")
	}
}

func TestEmptyTokenWithAllowAcceptsEverything(t *testing.T) {
	a, err := New(Options{AllowEmptyToken: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r := httptest.NewRequest("POST", "/", http.NoBody)
	r.Header.Set(types.HeaderActorDID, "did:pax:1")
	actor, err := a.Verify(r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if actor != "did:pax:1" {
		t.Fatalf("actor: %q", actor)
	}
}

func TestVerifySignatureStubReturnsNil(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/", http.NoBody)
	if err := a.VerifySignature(r, "did:pax:1"); err != nil {
		t.Fatalf("stub should return nil; got %v", err)
	}
}

func TestAgentCoreTokenClaimsAndVerification(t *testing.T) {
	key := bytes.Repeat([]byte("k"), 32)
	authenticator, token := newAgentCoreAuth(t, "active-1", key, agentCoreTestNow)
	if !strings.HasPrefix(token, "mxg1.") || token == "legacy" {
		t.Fatalf("token shape=%q", token)
	}
	actor, err := authenticator.Verify(agentCoreRequest(token))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if actor != "did:matrix:user-1:cody" {
		t.Fatalf("actor=%q", actor)
	}
	principal, err := authenticator.Authenticate(agentCoreRequest(token))
	if err != nil {
		t.Fatalf("Authenticate: %v", err)
	}
	if principal.Actor != actor || principal.Slot != AgentCoreSlot || !principal.Scoped ||
		principal.Provider != AgentCoreProvider || principal.Model != AgentCoreModel {
		t.Fatalf("principal=%+v", principal)
	}

	parts := strings.Split(token, ".")
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	var claims AgentCoreClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	if claims.KeyID != "active-1" || claims.Audience != AgentCoreAudience ||
		claims.Scope != AgentCoreScope || claims.Slot != AgentCoreSlot ||
		claims.Provider != AgentCoreProvider || claims.Model != AgentCoreModel ||
		claims.ExpiresAt-claims.IssuedAt != int64(AgentCoreTokenTTL/time.Second) || claims.TokenID == "" {
		t.Fatalf("claims=%+v", claims)
	}
}

func TestAgentCoreTokenRejectsScopeSpoofAndBYO(t *testing.T) {
	key := bytes.Repeat([]byte("s"), 32)
	authenticator, token := newAgentCoreAuth(t, "scope", key, agentCoreTestNow)
	tests := []struct {
		name   string
		mutate func(*http.Request)
	}{
		{name: "actor", mutate: func(r *http.Request) { r.Header.Set(types.HeaderActorDID, "did:matrix:other:cody") }},
		{name: "slot", mutate: func(r *http.Request) { r.Header.Set(types.HeaderSlot, "neo") }},
		{name: "method", mutate: func(r *http.Request) { r.Method = http.MethodGet }},
		{name: "path", mutate: func(r *http.Request) { r.URL.Path = "/v1/embeddings" }},
		{name: "byo flag", mutate: func(r *http.Request) { r.Header.Set(types.HeaderBYOAPIKey, "true") }},
		{name: "byo key", mutate: func(r *http.Request) { r.Header.Set(types.HeaderUserAPIKey, "provider-secret") }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			request := agentCoreRequest(token)
			test.mutate(request)
			if _, err := authenticator.Verify(request); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("expected unauthorized, got %v", err)
			}
		})
	}
}

func TestAgentCoreTokenRejectsExpiryTamperAndMalformedWithoutLegacyFallback(t *testing.T) {
	key := bytes.Repeat([]byte("e"), 32)
	expiredAuth, token := newAgentCoreAuth(t, "expiry", key, agentCoreTestNow.Add(AgentCoreTokenTTL))
	if _, err := expiredAuth.Verify(agentCoreRequest(token)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired token: %v", err)
	}

	validAuth, token := newAgentCoreAuth(t, "tamper", key, agentCoreTestNow)
	parts := strings.Split(token, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(parts[1])
	payload[len(payload)-1] ^= 1
	tampered := parts[0] + "." + base64.RawURLEncoding.EncodeToString(payload) + "." + parts[2]
	if _, err := validAuth.Verify(agentCoreRequest(tampered)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered token: %v", err)
	}

	legacyShaped, err := New(Options{
		Token: "mxg1.bad.bad",
		AgentCoreVerificationKeys: map[string][]byte{
			"tamper": key,
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := legacyShaped.Verify(agentCoreRequest("mxg1.bad.bad")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("malformed mxg1 fell back to legacy: %v", err)
	}
	bareLegacyShaped, err := New(Options{Token: agentCoreTokenPrefix})
	if err != nil {
		t.Fatalf("New bare legacy-shaped token: %v", err)
	}
	if _, err := bareLegacyShaped.Verify(agentCoreRequest(agentCoreTokenPrefix)); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("bare malformed mxg1 fell back to legacy: %v", err)
	}
}

func TestAgentCoreTokenRejectsInvalidSignedClaims(t *testing.T) {
	key := bytes.Repeat([]byte("c"), 32)
	authenticator, err := New(Options{
		Token:                     "legacy",
		AgentCoreVerificationKeys: map[string][]byte{"claims": key},
		Now:                       func() time.Time { return agentCoreTestNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	valid := AgentCoreClaims{
		KeyID:     "claims",
		Audience:  AgentCoreAudience,
		Scope:     AgentCoreScope,
		ActorDID:  "did:matrix:user-1:cody",
		Slot:      AgentCoreSlot,
		Provider:  AgentCoreProvider,
		Model:     AgentCoreModel,
		IssuedAt:  agentCoreTestNow.Unix(),
		ExpiresAt: agentCoreTestNow.Add(AgentCoreTokenTTL).Unix(),
		TokenID:   base64.RawURLEncoding.EncodeToString(bytes.Repeat([]byte{3}, 16)),
	}
	tests := []struct {
		name   string
		mutate func(*AgentCoreClaims)
	}{
		{name: "audience", mutate: func(c *AgentCoreClaims) { c.Audience = "other" }},
		{name: "scope", mutate: func(c *AgentCoreClaims) { c.Scope = "embeddings" }},
		{name: "provider", mutate: func(c *AgentCoreClaims) { c.Provider = "xai" }},
		{name: "model", mutate: func(c *AgentCoreClaims) { c.Model = "grok-4" }},
		{name: "future issued at", mutate: func(c *AgentCoreClaims) { c.IssuedAt = agentCoreTestNow.Add(time.Minute).Unix() }},
		{name: "expires before issued", mutate: func(c *AgentCoreClaims) { c.ExpiresAt = c.IssuedAt }},
		{name: "overlong lifetime", mutate: func(c *AgentCoreClaims) {
			c.ExpiresAt = c.IssuedAt + int64((AgentCoreMaxTokenTTL+time.Second)/time.Second)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			claims := valid
			test.mutate(&claims)
			token := signedAgentCoreTestToken(t, key, claims)
			if _, err := authenticator.Verify(agentCoreRequest(token)); !errors.Is(err, ErrUnauthorized) {
				t.Fatalf("expected unauthorized, got %v", err)
			}
		})
	}
}

func TestAgentCoreVerificationKeyRotation(t *testing.T) {
	oldKey := bytes.Repeat([]byte("o"), 32)
	newKey := bytes.Repeat([]byte("n"), 32)
	oldIssuer, err := NewAgentCoreIssuer(AgentCoreIssuerOptions{
		KeyID: "old", Key: oldKey, Now: func() time.Time { return agentCoreTestNow },
	})
	if err != nil {
		t.Fatalf("old issuer: %v", err)
	}
	newIssuer, err := NewAgentCoreIssuer(AgentCoreIssuerOptions{
		KeyID: "new", Key: newKey, Now: func() time.Time { return agentCoreTestNow },
	})
	if err != nil {
		t.Fatalf("new issuer: %v", err)
	}
	oldToken, _, _ := oldIssuer.Mint("did:matrix:user-1:cody", AgentCoreTokenTTL)
	newToken, _, _ := newIssuer.Mint("did:matrix:user-1:cody", AgentCoreTokenTTL)
	authenticator, err := New(Options{
		Token: "legacy",
		AgentCoreVerificationKeys: map[string][]byte{
			"old": oldKey,
			"new": newKey,
		},
		Now: func() time.Time { return agentCoreTestNow },
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, current := range []string{oldToken, newToken} {
		if _, err := authenticator.Verify(agentCoreRequest(current)); err != nil {
			t.Fatalf("rotation token rejected: %v", err)
		}
	}
}

func TestAgentCoreIssuerEnforcesTTLAndKeyStrength(t *testing.T) {
	if _, err := NewAgentCoreIssuer(AgentCoreIssuerOptions{KeyID: "weak", Key: []byte("short")}); err == nil {
		t.Fatal("weak key accepted")
	}
	issuer, err := NewAgentCoreIssuer(AgentCoreIssuerOptions{
		KeyID: "active", Key: bytes.Repeat([]byte("a"), 32),
	})
	if err != nil {
		t.Fatalf("NewAgentCoreIssuer: %v", err)
	}
	if _, _, err := issuer.Mint("did:matrix:user-1:cody", AgentCoreMaxTokenTTL+time.Second); err == nil {
		t.Fatal("overlong TTL accepted")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
