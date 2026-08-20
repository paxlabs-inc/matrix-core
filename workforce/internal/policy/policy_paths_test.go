package policy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
)

func TestPublishRejectsInvalidContractsAndSignatures(t *testing.T) {
	ctx := context.Background()
	store, privateKey, seed, grant, _, scope := policyFixture(t)
	invalidOrganization := seed.Organization
	invalidOrganization.Version = 0
	if err := store.PublishOrganization(ctx, invalidOrganization, grant); err == nil {
		t.Fatal("published invalid organization")
	}
	tamperedOrganization := seed.Organization
	tamperedOrganization.Name = "tampered"
	if err := store.PublishOrganization(ctx, tamperedOrganization, grant); err == nil {
		t.Fatal("published organization with tampered signature")
	}
	invalidMandate := seed.Mandates[0]
	invalidMandate.Version = 0
	if err := store.PublishMandate(ctx, invalidMandate, grant); err == nil {
		t.Fatal("published invalid mandate")
	}
	tamperedMandate := seed.Mandates[0]
	tamperedMandate.AllowedSkills = append(
		append([]contracts.SkillID(nil), tamperedMandate.AllowedSkills...),
		"z-extra",
	)
	if err := store.PublishMandate(ctx, tamperedMandate, grant); err == nil {
		t.Fatal("published mandate with tampered signature")
	}
	invalidSeat := seed.Organization.Departments[0].Seats[0]
	invalidSeat.Version = 0
	if err := store.PublishSeat(ctx, invalidSeat, grant); err == nil {
		t.Fatal("published invalid seat")
	}
	tamperedSeat := seed.Organization.Departments[0].Seats[0]
	tamperedSeat.BindingVersion++
	if err := store.PublishSeat(ctx, tamperedSeat, grant); err == nil {
		t.Fatal("published seat with tampered signature")
	}
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	invalidPolicy := policy
	invalidPolicy.Version = 0
	if err := store.PublishPolicy(ctx, invalidPolicy, grant); err == nil {
		t.Fatal("published invalid policy")
	}
	future := policy
	future.EffectiveAt = policyNow().Add(time.Hour)
	if err := SignPolicy(&future, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign future policy: %v", err)
	}
	if err := store.PublishPolicy(ctx, future, grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("future authority error = %v, want ErrUnauthorized", err)
	}
	otherOrganization := policy
	otherOrganization.OrganizationID = "org-other"
	if err := SignPolicy(&otherOrganization, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign cross-organization policy: %v", err)
	}
	if err := store.PublishPolicy(ctx, otherOrganization, grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-organization error = %v, want ErrUnauthorized", err)
	}
}

func TestIntegration_LeaseRegistrationRejectsInvalidStaleAndDuplicateAuthority(t *testing.T) {
	ctx := context.Background()
	store, privateKey, seed, grant, _, scope := policyFixture(t)
	mandate := seed.Mandates[0]
	seat := seed.Organization.Departments[0].Seats[0]
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishMandate(ctx, mandate, grant); err != nil {
		t.Fatalf("publish mandate: %v", err)
	}
	if err := store.PublishSeat(ctx, seat, grant); err != nil {
		t.Fatalf("publish seat: %v", err)
	}
	if err := store.PublishPolicy(ctx, policy, grant); err != nil {
		t.Fatalf("publish policy: %v", err)
	}
	if err := store.PublishRuntimeAuthority(
		ctx, seed.RuntimeAuthority, grant,
	); err != nil {
		t.Fatalf("publish runtime authority: %v", err)
	}
	hash := canonicalHash(t, &policy)
	lease := validLease(scope, store.root.OrganizationID, seat, mandate, policy, hash)
	if err := SignWakeLease(&lease, store.root.KeyID, privateKey); err != nil {
		t.Fatal(err)
	}

	invalid := lease
	invalid.ID = ""
	if err := store.RegisterLease(ctx, invalid); err == nil {
		t.Fatal("registered invalid lease")
	}
	tampered := lease
	tampered.Reason = "tampered-after-signing"
	if err := store.RegisterLease(ctx, tampered); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("tampered signed lease error = %v, want ErrLeaseInvalid", err)
	}
	wrongOrganization := lease
	wrongOrganization.ID = contracts.LeaseID("wrong-org-" + scope)
	wrongOrganization.OrganizationID = "org-other"
	if err := store.RegisterLease(ctx, wrongOrganization); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("wrong organization lease error = %v", err)
	}
	expired := lease
	expired.ID = contracts.LeaseID("expired-" + scope)
	expired.IssuedAt = policyNow().Add(-time.Hour)
	expired.ExpiresAt = policyNow()
	if err := store.RegisterLease(ctx, expired); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("expired lease error = %v", err)
	}
	staleMandate := lease
	staleMandate.ID = contracts.LeaseID("stale-mandate-" + scope)
	staleMandate.MandateVersion = 2
	if err := store.RegisterLease(ctx, staleMandate); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("stale mandate lease error = %v", err)
	}
	stalePolicy := lease
	stalePolicy.ID = contracts.LeaseID("stale-policy-" + scope)
	stalePolicy.Policies = append([]contracts.PolicyRef(nil), lease.Policies...)
	stalePolicy.Policies[0].Hash = hashForPolicyTest("wrong-policy-hash")
	if err := store.RegisterLease(ctx, stalePolicy); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("stale policy lease error = %v", err)
	}
	if err := store.RegisterLease(ctx, lease); err != nil {
		t.Fatalf("register valid lease: %v", err)
	}
	if err := store.RegisterLease(ctx, lease); err != nil {
		t.Fatalf("repeat identical lease: %v", err)
	}
	conflict := lease
	conflict.Reason = "different-reason"
	if err := SignWakeLease(&conflict, store.root.KeyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLease(ctx, conflict); !errors.Is(err, ErrStale) {
		t.Fatalf("lease identity conflict = %v, want ErrStale", err)
	}
	if err := store.AuthorizeLease(ctx, "missing-lease"); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("missing lease authorization = %v, want ErrLeaseInvalid", err)
	}
	store.now = func() time.Time { return policyNow().Add(2 * time.Hour) }
	if err := store.AuthorizeLease(ctx, lease.ID); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("expired registered lease error = %v, want ErrLeaseInvalid", err)
	}
}

func TestIntegration_EveryMaterialAuthorityKindInvalidatesAffectedLease(t *testing.T) {
	for _, kind := range []Kind{KindOrganization, KindMandate, KindSeat} {
		t.Run(string(kind), func(t *testing.T) {
			ctx := context.Background()
			store, privateKey, seed, grant, _, scope := policyFixture(t)
			mandate := seed.Mandates[0]
			seat := seed.Organization.Departments[0].Seats[0]
			policy := signedPolicy(t, privateKey, store.root, scope, 1)
			if err := store.PublishOrganization(ctx, seed.Organization, grant); err != nil {
				t.Fatalf("publish organization: %v", err)
			}
			if err := store.PublishRuntimeAuthority(
				ctx, seed.RuntimeAuthority, grant,
			); err != nil {
				t.Fatalf("publish runtime authority: %v", err)
			}
			if err := store.PublishMandate(ctx, mandate, grant); err != nil {
				t.Fatalf("publish mandate: %v", err)
			}
			if err := store.PublishSeat(ctx, seat, grant); err != nil {
				t.Fatalf("publish seat: %v", err)
			}
			if err := store.PublishPolicy(ctx, policy, grant); err != nil {
				t.Fatalf("publish policy: %v", err)
			}
			lease := validLease(
				scope,
				store.root.OrganizationID,
				seat,
				mandate,
				policy,
				canonicalHash(t, &policy),
			)
			if err := SignWakeLease(&lease, store.root.KeyID, privateKey); err != nil {
				t.Fatal(err)
			}
			if err := store.RegisterLease(ctx, lease); err != nil {
				t.Fatalf("register lease: %v", err)
			}
			switch kind {
			case KindOrganization:
				next := seed.Organization
				next.Version = 2
				next.Name += " v2"
				if err := SignOrganization(&next, store.root.KeyID, privateKey); err != nil {
					t.Fatalf("sign organization v2: %v", err)
				}
				if err := store.PublishOrganization(ctx, next, grant); err != nil {
					t.Fatalf("publish organization v2: %v", err)
				}
			case KindMandate:
				next := mandate
				next.Version = 2
				next.AllowedSkills = append(
					append([]contracts.SkillID(nil), next.AllowedSkills...),
					"z-material-skill",
				)
				if err := SignMandate(&next, store.root.KeyID, privateKey); err != nil {
					t.Fatalf("sign mandate v2: %v", err)
				}
				if err := store.PublishMandate(ctx, next, grant); err != nil {
					t.Fatalf("publish mandate v2: %v", err)
				}
			case KindSeat:
				next := seat
				next.Version = 2
				next.BindingVersion = 2
				if err := SignSeat(&next, store.root.KeyID, privateKey); err != nil {
					t.Fatalf("sign seat v2: %v", err)
				}
				if err := store.PublishSeat(ctx, next, grant); err != nil {
					t.Fatalf("publish seat v2: %v", err)
				}
			}
			if err := store.AuthorizeLease(ctx, lease.ID); !errors.Is(err, ErrLeaseInvalid) {
				t.Fatalf("%s change authorization error = %v", kind, err)
			}
		})
	}
}

func TestIntegration_RevocationRejectsInvalidAuthority(t *testing.T) {
	ctx := context.Background()
	store, privateKey, _, writeGrant, revokeGrant, scope := policyFixture(t)
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishPolicy(ctx, policy, writeGrant); err != nil {
		t.Fatalf("publish policy: %v", err)
	}
	valid := Revocation{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      store.root.TenantID, OrganizationID: store.root.OrganizationID,
		Kind: KindPolicy, AuthorityID: string(policy.ID), Version: policy.Version,
		OwnerID: store.root.OwnerID, Reason: "withdrawn", RevokedAt: policyNow(),
	}
	if err := SignRevocation(&valid, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	if err := store.Revoke(ctx, valid, writeGrant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("write-only grant revoked authority: %v", err)
	}
	invalid := valid
	invalid.Reason = ""
	if err := store.Revoke(ctx, invalid, revokeGrant); err == nil {
		t.Fatal("accepted invalid revocation")
	}
	wrongTenant := valid
	wrongTenant.TenantID = "tenant-other"
	if err := SignRevocation(&wrongTenant, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign cross-tenant revocation: %v", err)
	}
	if err := store.Revoke(ctx, wrongTenant, revokeGrant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-tenant revocation error = %v", err)
	}
	wrongSignature := valid
	_, otherKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate wrong revocation key: %v", err)
	}
	if err := SignRevocation(&wrongSignature, store.root.KeyID, otherKey); err != nil {
		t.Fatalf("sign wrong-key revocation: %v", err)
	}
	if err := store.Revoke(ctx, wrongSignature, revokeGrant); err == nil {
		t.Fatal("accepted revocation signed by another key")
	}
	missing := valid
	missing.AuthorityID = "missing-policy"
	if err := SignRevocation(&missing, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign missing-target revocation: %v", err)
	}
	if err := store.Revoke(ctx, missing, revokeGrant); err == nil {
		t.Fatal("revoked nonexistent authority")
	}
}

func hashForPolicyTest(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{
		Algorithm: "sha256",
		Digest:    hex.EncodeToString(sum[:]),
	}
}
