package policy

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/ledger"
)

const policyPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var policyPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startPolicyPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "policy integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	pool, err := waitForPolicyPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "policy integration setup:", err)
		os.Exit(1)
	}
	policyPool = pool
	if err := ledger.ApplyMigrations(ctx, policyPool, policyNow()); err != nil {
		pool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "policy integration migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	pool.Close()
	cleanup()
	os.Exit(code)
}

func TestSeedContainsSevenEnabledRealDepartmentsAndTwentyOneSignedSeats(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate owner key: %v", err)
	}
	_ = publicKey
	seed, err := BuildSeed(
		"org-seed",
		"owner-seed",
		"Seed Organization",
		policyNow(),
		"owner-key",
		privateKey,
	)
	if err != nil {
		t.Fatalf("build seed: %v", err)
	}
	if len(seed.Organization.Departments) != 7 || len(seed.Mandates) != 21 {
		t.Fatalf(
			"seed has %d departments and %d mandates, want 7 and 21",
			len(seed.Organization.Departments),
			len(seed.Mandates),
		)
	}
	seen := make(map[contracts.DepartmentKind]bool)
	for _, department := range seed.Organization.Departments {
		if !department.Enabled || len(department.Seats) != 3 {
			t.Fatalf("department %#v is disabled or lacks three seats", department.Kind)
		}
		if len(departmentSkills[department.Kind]) == 0 {
			t.Fatalf("department %s has no real skill pack", department.Kind)
		}
		seen[department.Kind] = true
		for _, seat := range department.Seats {
			if seat.Version != 1 || seat.EffectiveAt != policyNow() ||
				seat.Signature.KeyID != "owner-key" {
				t.Fatalf("seat is not a signed effective version: %#v", seat)
			}
		}
	}
	for _, kind := range contracts.AllDepartmentKinds() {
		if !seen[kind] {
			t.Fatalf("seed omitted department %s", kind)
		}
	}
}

func TestIntegration_PublishSeedRejectsTamperThenAtomicallyCreatesCanonicalTopology(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, _, seed, _, _, _ := policyFixture(t)
	tampered := seed
	tampered.Mandates = append([]contracts.Mandate(nil), seed.Mandates...)
	tampered.Mandates[0].AllowedSkills = append(
		[]contracts.SkillID(nil), tampered.Mandates[0].AllowedSkills...,
	)
	tampered.Mandates[0].AllowedSkills[0] = "tampered-skill"
	if _, err := store.PublishSeed(ctx, tampered); err == nil {
		t.Fatal("tampered seed was accepted")
	}
	var authorityCount int
	if err := policyPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.root.TenantID, store.root.OrganizationID).Scan(&authorityCount); err != nil {
		t.Fatal(err)
	}
	if authorityCount != 0 {
		t.Fatalf("failed seed left %d partial authority records", authorityCount)
	}
	result, err := store.PublishSeed(ctx, seed)
	if err != nil || result.Deduplicated {
		t.Fatalf("publish seed result=%#v err=%v", result, err)
	}
	var departments, seats int
	if err := policyPool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM workforce_organization_departments
		   WHERE tenant_id=$1 AND organization_id=$2),
		  (SELECT COUNT(*) FROM workforce_organization_seats
		   WHERE tenant_id=$1 AND organization_id=$2)
	`, store.root.TenantID, store.root.OrganizationID).Scan(
		&departments, &seats,
	); err != nil {
		t.Fatal(err)
	}
	if departments != 7 || seats != 21 {
		t.Fatalf("projection = %d departments and %d seats", departments, seats)
	}
	duplicate, err := store.PublishSeed(ctx, seed)
	if err != nil || !duplicate.Deduplicated {
		t.Fatalf("idempotent seed result=%#v err=%v", duplicate, err)
	}
}

func TestIntegration_SignedAuthorityVersionLeaseInvalidationAndRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, privateKey, seed, writeGrant, revokeGrant, scope := policyFixture(t)
	if err := store.PublishOrganization(ctx, seed.Organization, writeGrant); err != nil {
		t.Fatalf("publish organization: %v", err)
	}
	if err := store.PublishRuntimeAuthority(
		ctx, seed.RuntimeAuthority, writeGrant,
	); err != nil {
		t.Fatalf("publish runtime authority: %v", err)
	}
	mandate := seed.Mandates[0]
	seat := seed.Organization.Departments[0].Seats[0]
	if err := store.PublishMandate(ctx, mandate, writeGrant); err != nil {
		t.Fatalf("publish mandate: %v", err)
	}
	if err := store.PublishSeat(ctx, seat, writeGrant); err != nil {
		t.Fatalf("publish seat: %v", err)
	}
	policy := signedPolicy(t, privateKey, store.root, scope, 1)
	if err := store.PublishPolicy(ctx, policy, writeGrant); err != nil {
		t.Fatalf("publish policy: %v", err)
	}
	loaded, err := store.LoadPolicy(ctx, policy.ID, policy.Version)
	if err != nil || loaded.ID != policy.ID || loaded.Signature != policy.Signature {
		t.Fatalf("load policy = %#v, %v", loaded, err)
	}
	var sealed []byte
	if err := policyPool.QueryRow(ctx, `
		SELECT sealed_record FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind='policy' AND authority_id=$3 AND version=1
	`, store.root.TenantID, store.root.OrganizationID, policy.ID).Scan(&sealed); err != nil {
		t.Fatalf("read sealed policy: %v", err)
	}
	if !vault.IsVault(sealed) || bytes.Contains(sealed, []byte(policy.Rules[0].Scope)) {
		t.Fatal("authority record is not sealed at rest")
	}

	policyHash := canonicalHash(t, &policy)
	lease := validLease(scope, store.root.OrganizationID, seat, mandate, policy, policyHash)
	if err := SignWakeLease(&lease, store.root.KeyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLease(ctx, lease); err != nil {
		t.Fatalf("register lease: %v", err)
	}
	if err := store.AuthorizeLease(ctx, lease.ID); err != nil {
		t.Fatalf("authorize current lease: %v", err)
	}
	if _, err := policyPool.Exec(ctx, `
		INSERT INTO workforce_runtime_leases (
			tenant_id,organization_id,lease_id,wake_id,seat_id,node_id,
			mandate_id,mandate_version,policy_binding_hash,fence,state,
			issued_at,expires_at
		) VALUES ($1,$2,'runtime:policy-change','wake:policy-change',$3,
			'node:policy-change',$4,$5,$6,1,'active',$7,$8)
	`, store.root.TenantID, store.root.OrganizationID, seat.ID,
		mandate.ID, mandate.Version, strings.Repeat("c", 64),
		policyNow(), policyNow().Add(time.Hour)); err != nil {
		t.Fatalf("insert runtime lease: %v", err)
	}
	if _, err := policyPool.Exec(ctx, `
		INSERT INTO workforce_runtime_lease_policies (
			tenant_id,organization_id,lease_id,policy_id,policy_version,policy_hash
		) VALUES ($1,$2,'runtime:policy-change',$3,$4,$5)
	`, store.root.TenantID, store.root.OrganizationID, policy.ID,
		policy.Version, policyHash.Digest); err != nil {
		t.Fatalf("bind runtime policy: %v", err)
	}
	policyV2 := policy
	policyV2.Version = 2
	policyV2.Rules = []contracts.PolicyRule{{
		ClauseID: "external-effects",
		Outcome:  "require_review",
		Scope:    "all external effects require independent human review",
	}}
	if err := SignPolicy(&policyV2, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign policy v2: %v", err)
	}
	if err := store.PublishPolicy(ctx, policyV2, writeGrant); err != nil {
		t.Fatalf("publish material policy change: %v", err)
	}
	if err := store.AuthorizeLease(ctx, lease.ID); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("material policy change lease error = %v, want ErrLeaseInvalid", err)
	}
	var invalidations int
	if err := policyPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_authority_lease_invalidations
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
	`, store.root.TenantID, store.root.OrganizationID, lease.ID).Scan(&invalidations); err != nil {
		t.Fatalf("count lease invalidations: %v", err)
	}
	if invalidations != 1 {
		t.Fatalf("lease invalidations = %d, want 1", invalidations)
	}
	var runtimeState, cancellationReason string
	if err := policyPool.QueryRow(ctx, `
		SELECT state,cancellation_reason FROM workforce_runtime_leases
		WHERE tenant_id=$1 AND organization_id=$2 AND lease_id='runtime:policy-change'
	`, store.root.TenantID, store.root.OrganizationID).Scan(
		&runtimeState, &cancellationReason,
	); err != nil {
		t.Fatalf("load runtime policy change: %v", err)
	}
	if runtimeState != "cancelled" || cancellationReason != "material authority change" {
		t.Fatalf("runtime policy change = %s/%q", runtimeState, cancellationReason)
	}
	var policyChangeReceipts int
	if err := policyPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_policy_change_receipts
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind='policy' AND authority_id=$3
	`, store.root.TenantID, store.root.OrganizationID, policy.ID).Scan(
		&policyChangeReceipts,
	); err != nil {
		t.Fatalf("count policy change receipts: %v", err)
	}
	if policyChangeReceipts != 2 {
		t.Fatalf("policy change receipts = %d, want authority and runtime", policyChangeReceipts)
	}

	revocation := Revocation{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      store.root.TenantID, OrganizationID: store.root.OrganizationID,
		Kind: KindPolicy, AuthorityID: string(policyV2.ID), Version: policyV2.Version,
		OwnerID: store.root.OwnerID, Reason: "owner withdrew policy",
		RevokedAt: policyNow(),
	}
	if err := SignRevocation(&revocation, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign revocation: %v", err)
	}
	if err := store.Revoke(ctx, revocation, revokeGrant); err != nil {
		t.Fatalf("revoke policy: %v", err)
	}
	if _, err := store.LoadPolicy(ctx, policyV2.ID, policyV2.Version); !errors.Is(err, ErrRevoked) {
		t.Fatalf("revoked policy load error = %v, want ErrRevoked", err)
	}
}

func TestIntegration_AuthorityFailsClosedForTamperStaleOwnerTenantAndExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	store, privateKey, seed, grant, _, scope := policyFixture(t)
	policy := signedPolicy(t, privateKey, store.root, scope, 1)

	tampered := policy
	tampered.Rules = append([]contracts.PolicyRule(nil), policy.Rules...)
	tampered.Rules[0].Scope = "tampered authority"
	if err := store.PublishPolicy(ctx, tampered, grant); err == nil {
		t.Fatal("published policy whose signed content was tampered")
	}
	_, wrongPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate wrong owner key: %v", err)
	}
	wrongSigner := policy
	if err := SignPolicy(&wrongSigner, store.root.KeyID, wrongPrivateKey); err != nil {
		t.Fatalf("sign with wrong key: %v", err)
	}
	if err := store.PublishPolicy(ctx, wrongSigner, grant); err == nil {
		t.Fatal("published policy signed by a non-owner key")
	}
	wrongOwner := seed.Organization
	wrongOwner.OwnerID = "owner-other"
	if err := SignOrganization(&wrongOwner, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign wrong-owner organization: %v", err)
	}
	if err := store.PublishOrganization(ctx, wrongOwner, grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong owner error = %v, want ErrUnauthorized", err)
	}
	wrongTenant := grant
	wrongTenant.TenantID = "tenant-other"
	if err := SignOwnerGrant(&wrongTenant, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign wrong-tenant grant: %v", err)
	}
	if err := store.PublishPolicy(ctx, policy, wrongTenant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong tenant error = %v, want ErrUnauthorized", err)
	}
	expired := grant
	expired.ExpiresAt = policyNow()
	if err := SignOwnerGrant(&expired, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign expired grant: %v", err)
	}
	if err := store.PublishPolicy(ctx, policy, expired); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired grant error = %v, want ErrUnauthorized", err)
	}

	if err := store.PublishPolicy(ctx, policy, grant); err != nil {
		t.Fatalf("publish valid policy: %v", err)
	}
	if err := store.PublishPolicy(ctx, policy, grant); !errors.Is(err, ErrStale) {
		t.Fatalf("duplicate stale version error = %v, want ErrStale", err)
	}
	versionThree := policy
	versionThree.Version = 3
	if err := SignPolicy(&versionThree, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign skipped version: %v", err)
	}
	if err := store.PublishPolicy(ctx, versionThree, grant); !errors.Is(err, ErrStale) {
		t.Fatalf("skipped version error = %v, want ErrStale", err)
	}

	if _, err := policyPool.Exec(
		ctx,
		`ALTER TABLE workforce_authority_records
		 DISABLE TRIGGER workforce_authority_records_immutable`,
	); err != nil {
		t.Fatalf("disable authority immutability for tamper simulation: %v", err)
	}
	defer func() {
		_, err := policyPool.Exec(context.Background(), `
			ALTER TABLE workforce_authority_records
			ENABLE TRIGGER workforce_authority_records_immutable
		`)
		if err != nil {
			t.Errorf("restore authority immutability trigger: %v", err)
		}
	}()
	if _, err := policyPool.Exec(ctx, `
		UPDATE workforce_authority_records SET canonical_hash=$6
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind=$3 AND authority_id=$4 AND version=$5
	`, store.root.TenantID, store.root.OrganizationID, KindPolicy,
		policy.ID, policy.Version, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("tamper stored authority hash: %v", err)
	}
	if _, err := store.LoadPolicy(ctx, policy.ID, policy.Version); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("stored tamper error = %v, want ErrIntegrity", err)
	}
	if _, err := policyPool.Exec(ctx, `
		UPDATE workforce_authority_records SET sealed_record=$6
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind=$3 AND authority_id=$4 AND version=$5
	`, store.root.TenantID, store.root.OrganizationID, KindPolicy,
		policy.ID, policy.Version, []byte("not-a-vault-record")); err != nil {
		t.Fatalf("tamper stored Vault record: %v", err)
	}
	if _, err := store.LoadPolicy(ctx, policy.ID, policy.Version); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("Vault tamper error = %v, want ErrIntegrity", err)
	}
	invalidCanonical := []byte(`{"invalid":"policy"}`)
	invalidHash := sha256.Sum256(invalidCanonical)
	invalidSealed, err := store.vault.SealRecord(
		store.authorityAD(KindPolicy, string(policy.ID), policy.Version),
		invalidCanonical,
	)
	if err != nil {
		t.Fatalf("seal invalid canonical policy: %v", err)
	}
	if _, err := policyPool.Exec(ctx, `
		UPDATE workforce_authority_records
		SET canonical_hash=$6, sealed_record=$7
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind=$3 AND authority_id=$4 AND version=$5
	`, store.root.TenantID, store.root.OrganizationID, KindPolicy,
		policy.ID, policy.Version, hex.EncodeToString(invalidHash[:]),
		invalidSealed); err != nil {
		t.Fatalf("store invalid canonical policy: %v", err)
	}
	if _, err := store.LoadPolicy(ctx, policy.ID, policy.Version); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("invalid canonical policy error = %v, want ErrIntegrity", err)
	}
	_, storedWrongPrivateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate stored wrong-signature key: %v", err)
	}
	wrongSigned := policy
	if err := SignPolicy(&wrongSigned, store.root.KeyID, storedWrongPrivateKey); err != nil {
		t.Fatalf("sign stored wrong-signature policy: %v", err)
	}
	wrongCanonical, err := contracts.EncodeCanonical(&wrongSigned)
	if err != nil {
		t.Fatalf("encode stored wrong-signature policy: %v", err)
	}
	wrongHash := sha256.Sum256(wrongCanonical)
	wrongSealed, err := store.vault.SealRecord(
		store.authorityAD(KindPolicy, string(policy.ID), policy.Version),
		wrongCanonical,
	)
	if err != nil {
		t.Fatalf("seal stored wrong-signature policy: %v", err)
	}
	if _, err := policyPool.Exec(ctx, `
		UPDATE workforce_authority_records
		SET canonical_hash=$6, sealed_record=$7
		WHERE tenant_id=$1 AND organization_id=$2
		  AND authority_kind=$3 AND authority_id=$4 AND version=$5
	`, store.root.TenantID, store.root.OrganizationID, KindPolicy,
		policy.ID, policy.Version, hex.EncodeToString(wrongHash[:]),
		wrongSealed); err != nil {
		t.Fatalf("store wrong-signature policy: %v", err)
	}
	if _, err := store.LoadPolicy(ctx, policy.ID, policy.Version); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("stored wrong-signature policy error = %v, want ErrIntegrity", err)
	}
}

func policyFixture(
	t *testing.T,
) (*Store, ed25519.PrivateKey, Seed, OwnerGrant, OwnerGrant, string) {
	t.Helper()
	sum := sha256.Sum256([]byte(t.Name()))
	scope := hex.EncodeToString(sum[:6])
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate owner key: %v", err)
	}
	tenantID := "tenant-" + scope
	organizationID := contracts.OrganizationID("org-" + scope)
	ownerID := contracts.OwnerID("owner-" + scope)
	keyID := "owner-key-" + scope
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenantID, KEKHex: kek,
	})
	if err != nil {
		t.Fatalf("boot Vault: %v", err)
	}
	root := OwnerRoot{
		TenantID: tenantID, OrganizationID: organizationID,
		OwnerID: ownerID, KeyID: keyID, PublicKey: publicKey,
	}
	store, err := New(policyPool, session.UserVault(), root, policyNow)
	if err != nil {
		t.Fatalf("construct policy store: %v", err)
	}
	seed, err := BuildSeed(
		organizationID,
		ownerID,
		"Organization "+scope,
		policyNow(),
		keyID,
		privateKey,
	)
	if err != nil {
		t.Fatalf("build signed seed: %v", err)
	}
	return store, privateKey, seed,
		signedGrant(t, privateKey, root, "authority:write"),
		signedGrant(t, privateKey, root, "authority:revoke"),
		scope
}

func signedGrant(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	root OwnerRoot,
	scope string,
) OwnerGrant {
	t.Helper()
	grant := OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      root.TenantID, OrganizationID: root.OrganizationID,
		OwnerID: root.OwnerID, Scope: scope,
		IssuedAt:  policyNow().Add(-time.Minute),
		ExpiresAt: policyNow().Add(time.Hour),
	}
	if err := SignOwnerGrant(&grant, root.KeyID, privateKey); err != nil {
		t.Fatalf("sign owner grant: %v", err)
	}
	return grant
}

func signedPolicy(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	root OwnerRoot,
	scope string,
	version uint64,
) contracts.Policy {
	t.Helper()
	policy := contracts.Policy{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.PolicyID("policy-" + scope), Version: version,
		OrganizationID: root.OrganizationID, Kind: "autonomy",
		EffectiveAt: policyNow(),
		Rules: []contracts.PolicyRule{{
			ClauseID: "bounded-effects", Outcome: "allow",
			Scope: "reversible internal work within the signed mandate",
		}},
	}
	if err := SignPolicy(&policy, root.KeyID, privateKey); err != nil {
		t.Fatalf("sign policy: %v", err)
	}
	return policy
}

func validLease(
	scope string,
	organizationID contracts.OrganizationID,
	seat contracts.Seat,
	mandate contracts.Mandate,
	policy contracts.Policy,
	policyHash contracts.ContentHash,
) contracts.WakeLease {
	signature := contracts.Signature{
		Algorithm: "ed25519", KeyID: "kernel-key",
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	hash := func(value string) contracts.ContentHash {
		sum := sha256.Sum256([]byte(value))
		return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
	}
	return contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.LeaseID("lease-" + scope), WakeID: contracts.WakeID("wake-" + scope),
		OrganizationID: organizationID, SeatID: seat.ID, SeatDID: seat.DID,
		Reason: "eligible_work", MandateID: mandate.ID, MandateVersion: mandate.Version,
		Policies: []contracts.PolicyRef{{
			ID: policy.ID, Version: policy.Version, Hash: policyHash,
		}},
		GraphScope: []contracts.IntentID{contracts.IntentID("intent-" + scope)},
		Model: contracts.ModelBinding{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.ModelBindingID("model-" + scope),
			Provider:      "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
			SamplingDigest: hash("sampling-" + scope),
		},
		MGS: contracts.MGSGenomeRef{
			Reference: "mgs-" + scope, Digest: hash("mgs-" + scope),
		},
		Runtime: contracts.RuntimeBinding{
			BuildDigest:             hash("build-" + scope),
			AuditorBuildDigest:      hash("auditor-build-" + scope),
			OperationRegistryDigest: hash("operations-" + scope),
		},
		SkillCatalogDigest: hash("skills-" + scope),
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64((30 * time.Minute) / time.Millisecond),
			MaxSteps:          20, MaxModelCalls: 10, MaxToolCalls: 40,
			MaxCostMinor: 1000, Currency: "USD", MaxOutputBytes: 1 << 20,
		},
		IssuedAt: policyNow(), ExpiresAt: policyNow().Add(time.Hour),
		Fence: 1, Signature: signature,
	}
}

func canonicalHash[T contracts.Validatable](t *testing.T, value T) contracts.ContentHash {
	t.Helper()
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		t.Fatalf("encode canonical value: %v", err)
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func startPolicyPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforce-policy-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(
		ctx,
		"docker", "run", "--rm", "-d", "--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", policyPostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(
		ctx, "docker", "port", containerID, "5432/tcp",
	).CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("inspect PostgreSQL port: %w: %s", err, portOutput)
	}
	_, port, found := strings.Cut(strings.TrimSpace(string(portOutput)), ":")
	if !found || port == "" {
		return containerID, "", fmt.Errorf("unexpected PostgreSQL port %q", portOutput)
	}
	return containerID,
		"postgres://postgres:workforce-test-password@127.0.0.1:" +
			port + "/workforce?sslmode=disable",
		nil
}

func waitForPolicyPostgres(
	ctx context.Context,
	databaseURL string,
) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if err := pool.Ping(ctx); err == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func policyNow() time.Time {
	return time.Date(2026, time.July, 30, 16, 0, 0, 0, time.UTC)
}
