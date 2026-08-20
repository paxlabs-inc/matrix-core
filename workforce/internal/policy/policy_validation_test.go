package policy

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
)

func TestPolicyKindsRootsConstructionAndClockValidation(t *testing.T) {
	for _, kind := range []Kind{
		KindOrganization, KindMandate, KindSeat, KindPolicy,
	} {
		if !kind.Valid() {
			t.Fatalf("valid authority kind %q rejected", kind)
		}
	}
	if Kind("unknown").Valid() {
		t.Fatal("unknown authority kind accepted")
	}
	store, _, _, _, _, _ := policyFixture(t)
	validRoot := store.root
	for _, mutate := range []func(*OwnerRoot){
		func(value *OwnerRoot) { value.TenantID = "" },
		func(value *OwnerRoot) { value.OrganizationID = "" },
		func(value *OwnerRoot) { value.OwnerID = "" },
		func(value *OwnerRoot) { value.KeyID = "" },
		func(value *OwnerRoot) { value.PublicKey = nil },
	} {
		invalid := validRoot
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("owner root accepted invalid value %#v", invalid)
		}
	}
	if err := validRoot.Validate(); err != nil {
		t.Fatalf("valid owner root: %v", err)
	}
	if _, err := New(nil, store.vault, validRoot, policyNow); err == nil {
		t.Fatal("New accepted nil pool")
	}
	if _, err := New(policyPool, nil, validRoot, policyNow); err == nil {
		t.Fatal("New accepted nil Vault")
	}
	if _, err := New(policyPool, store.vault, validRoot, nil); err == nil {
		t.Fatal("New accepted nil clock")
	}
	invalidRoot := validRoot
	invalidRoot.PublicKey = nil
	if _, err := New(policyPool, store.vault, invalidRoot, policyNow); err == nil {
		t.Fatal("New accepted invalid owner root")
	}
	wrongTenant := validRoot
	wrongTenant.TenantID = "tenant-other"
	if _, err := New(policyPool, store.vault, wrongTenant, policyNow); err == nil {
		t.Fatal("New accepted mismatched Vault tenant")
	}
	for _, clock := range []func() time.Time{
		func() time.Time { return time.Time{} },
		func() time.Time { return policyNow().In(time.FixedZone("test", 3600)) },
	} {
		store.now = clock
		if _, err := store.currentTime(); err == nil {
			t.Fatal("currentTime accepted invalid clock")
		}
	}
	store.now = policyNow
	if _, err := store.currentTime(); err != nil {
		t.Fatalf("currentTime rejected UTC clock: %v", err)
	}
}

func TestOwnerGrantAndRevocationValidation(t *testing.T) {
	store, privateKey, _, grant, _, scope := policyFixture(t)
	if err := grant.Validate(); err != nil {
		t.Fatalf("valid owner grant: %v", err)
	}
	for _, mutate := range []func(*OwnerGrant){
		func(value *OwnerGrant) { value.SchemaVersion = "" },
		func(value *OwnerGrant) { value.TenantID = "" },
		func(value *OwnerGrant) { value.OrganizationID = "" },
		func(value *OwnerGrant) { value.OwnerID = "" },
		func(value *OwnerGrant) { value.KeyID = "" },
		func(value *OwnerGrant) { value.Scope = "" },
		func(value *OwnerGrant) { value.IssuedAt = time.Time{} },
		func(value *OwnerGrant) { value.ExpiresAt = value.IssuedAt },
		func(value *OwnerGrant) { value.Signature = contracts.Signature{} },
	} {
		invalid := grant
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("owner grant accepted invalid value %#v", invalid)
		}
	}
	revocation := Revocation{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      store.root.TenantID, OrganizationID: store.root.OrganizationID,
		Kind: KindPolicy, AuthorityID: "policy-" + scope, Version: 1,
		OwnerID: store.root.OwnerID, Reason: "withdrawn", RevokedAt: policyNow(),
	}
	if err := SignRevocation(&revocation, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	if err := revocation.Validate(); err != nil {
		t.Fatalf("valid revocation: %v", err)
	}
	for _, mutate := range []func(*Revocation){
		func(value *Revocation) { value.SchemaVersion = "" },
		func(value *Revocation) { value.TenantID = "" },
		func(value *Revocation) { value.OrganizationID = "" },
		func(value *Revocation) { value.Kind = "unknown" },
		func(value *Revocation) { value.AuthorityID = "" },
		func(value *Revocation) { value.Version = 0 },
		func(value *Revocation) { value.OwnerID = "" },
		func(value *Revocation) { value.KeyID = "" },
		func(value *Revocation) { value.Reason = "" },
		func(value *Revocation) { value.RevokedAt = time.Time{} },
		func(value *Revocation) { value.Signature = contracts.Signature{} },
	} {
		invalid := revocation
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("revocation accepted invalid value %#v", invalid)
		}
	}
}

func TestSigningRejectsInvalidKeysAndDetectsProfileMismatch(t *testing.T) {
	store, privateKey, seed, grant, _, scope := policyFixture(t)
	organization := seed.Organization
	if err := SignOrganization(&organization, store.root.KeyID, nil); err == nil {
		t.Fatal("SignOrganization accepted invalid private key")
	}
	var signature contracts.Signature
	if err := signValue(
		&signature,
		store.root.KeyID,
		privateKey,
		nil,
		errors.New("encoding failed"),
	); err == nil {
		t.Fatal("signValue ignored canonical encoding failure")
	}
	if err := verifyValue(
		store.root.PublicKey,
		store.root.KeyID,
		grant.Signature,
		nil,
		errors.New("encoding failed"),
	); err == nil {
		t.Fatal("verifyValue ignored canonical encoding failure")
	}
	mandate := seed.Mandates[0]
	expiresAt := policyNow().Add(time.Hour)
	mandate.ExpiresAt = &expiresAt
	if err := SignMandate(&mandate, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign expiring mandate: %v", err)
	}
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	policy.ExpiresAt = &expiresAt
	if err := SignPolicy(&policy, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign expiring policy: %v", err)
	}
	if err := verifyOrganization(
		store.root.PublicKey,
		store.root.KeyID,
		seed.Organization,
	); err != nil {
		t.Fatalf("verify organization: %v", err)
	}
	if err := verifyMandate(store.root.PublicKey, store.root.KeyID, mandate); err != nil {
		t.Fatalf("verify mandate: %v", err)
	}
	if err := verifySeat(
		store.root.PublicKey,
		store.root.KeyID,
		seed.Organization.Departments[0].Seats[0],
	); err != nil {
		t.Fatalf("verify seat: %v", err)
	}
	if err := verifyGrant(store.root.PublicKey, store.root.KeyID, grant); err != nil {
		t.Fatalf("verify grant: %v", err)
	}
	changedKeyID := grant
	changedKeyID.Signature.KeyID = "key-other"
	if err := verifyGrant(store.root.PublicKey, store.root.KeyID, changedKeyID); err == nil {
		t.Fatal("verified signature under a different key ID")
	}
	changedAlgorithm := grant
	changedAlgorithm.Signature.Algorithm = "other"
	if err := verifyGrant(store.root.PublicKey, store.root.KeyID, changedAlgorithm); err == nil {
		t.Fatal("verified signature under another algorithm")
	}
	changedSignature := grant
	changedSignature.Signature.Value = base64.RawURLEncoding.EncodeToString(
		make([]byte, ed25519.SignatureSize),
	)
	if err := verifyGrant(store.root.PublicKey, store.root.KeyID, changedSignature); err == nil {
		t.Fatal("verified an invalid owner signature")
	}
}

func TestBuildSeedRejectsInvalidSigningAndTopologyInputs(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if _, err := BuildSeed(
		"org", "owner", "name", policyNow(), "key", nil,
	); err == nil {
		t.Fatal("BuildSeed accepted invalid signing key")
	}
	if _, err := BuildSeed(
		"org", "owner", "name", time.Time{}, "key", privateKey,
	); err == nil {
		t.Fatal("BuildSeed accepted invalid effective time")
	}
}

func TestVerifyGrantRejectsScopeFutureAndTamper(t *testing.T) {
	store, privateKey, _, grant, _, _ := policyFixture(t)
	if _, err := store.verifyGrant(grant, grant.Scope); err != nil {
		t.Fatalf("verify valid grant: %v", err)
	}
	tests := []OwnerGrant{
		func() OwnerGrant {
			value := grant
			value.Scope = "other"
			_ = SignOwnerGrant(&value, store.root.KeyID, privateKey)
			return value
		}(),
		func() OwnerGrant {
			value := grant
			value.IssuedAt = policyNow().Add(time.Minute)
			value.ExpiresAt = policyNow().Add(time.Hour)
			_ = SignOwnerGrant(&value, store.root.KeyID, privateKey)
			return value
		}(),
		func() OwnerGrant {
			value := grant
			value.Signature.Value = base64.RawURLEncoding.EncodeToString(
				make([]byte, ed25519.SignatureSize),
			)
			return value
		}(),
	}
	for _, invalid := range tests {
		if _, err := store.verifyGrant(invalid, grant.Scope); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("invalid grant error = %v, want ErrUnauthorized", err)
		}
	}
}
