// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package auth

import (
	"errors"
	"net/http/httptest"
	"testing"

	"matrix/gateway/internal/types"
)

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
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set(types.HeaderAuthorization, "Bearer shh")
	r.Header.Set(types.HeaderActorDID, "did:pax:abcdef")
	actor, err := a.Verify(r)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if actor != "did:pax:abcdef" {
		t.Fatalf("actor mismatch: %q", actor)
	}
}

func TestVerifyRejectsBadToken(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set(types.HeaderAuthorization, "Bearer wrong")
	r.Header.Set(types.HeaderActorDID, "did:pax:abc")
	_, err := a.Verify(r)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expected ErrUnauthorized, got %v", err)
	}
}

func TestVerifyRejectsMissingActor(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
	r.Header.Set(types.HeaderAuthorization, "Bearer shh")
	_, err := a.Verify(r)
	if !errors.Is(err, ErrMissingActor) {
		t.Fatalf("expected ErrMissingActor, got %v", err)
	}
}

func TestVerifyRejectsMalformedActor(t *testing.T) {
	a := newAuth(t)
	r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
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
	r := httptest.NewRequest("POST", "/", nil)
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
	r := httptest.NewRequest("POST", "/", nil)
	if err := a.VerifySignature(r, "did:pax:1"); err != nil {
		t.Fatalf("stub should return nil; got %v", err)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
