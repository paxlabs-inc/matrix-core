package policy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

func TestIntegrationPublishSeedWithCommitIsAtomicAndDeduplicated(t *testing.T) {
	ctx := context.Background()
	store, _, seed, _, _, _ := policyFixture(t)
	if _, err := store.PublishSeedWithCommit(ctx, seed, nil); err == nil {
		t.Fatal("published seed without required commit hook")
	}
	commitFailure := errors.New("commit rejected")
	if _, err := store.PublishSeedWithCommit(
		ctx,
		seed,
		func(context.Context, pgx.Tx, time.Time) error { return commitFailure },
	); !errors.Is(err, commitFailure) {
		t.Fatalf("commit failure = %v, want %v", err, commitFailure)
	}
	var count int
	if err := policyPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_authority_records
		WHERE tenant_id=$1 AND organization_id=$2
	`, store.root.TenantID, store.root.OrganizationID).Scan(&count); err != nil {
		t.Fatalf("count rolled-back authorities: %v", err)
	}
	if count != 0 {
		t.Fatalf("failed activation left %d authority records", count)
	}

	commits := 0
	commit := func(ctx context.Context, tx pgx.Tx, now time.Time) error {
		if now != policyNow() {
			t.Fatalf("commit time = %v, want %v", now, policyNow())
		}
		if _, err := tx.Exec(ctx, `SELECT 1`); err != nil {
			return err
		}
		commits++
		return nil
	}
	result, err := store.PublishSeedWithCommit(ctx, seed, commit)
	if err != nil || result.Deduplicated {
		t.Fatalf("initial publish = %#v, %v", result, err)
	}
	result, err = store.PublishSeedWithCommit(ctx, seed, commit)
	if err != nil || !result.Deduplicated {
		t.Fatalf("deduplicated publish = %#v, %v", result, err)
	}
	if commits != 2 {
		t.Fatalf("commit hook calls = %d, want 2", commits)
	}
}

func TestIntegrationLoadsAndAuthorizesExactWorkPacketAuthority(t *testing.T) {
	ctx := context.Background()
	store, privateKey, seed, _, _, scope := policyFixture(t)
	if _, err := store.PublishSeed(ctx, seed); err != nil {
		t.Fatalf("publish seed: %v", err)
	}
	seat := seed.Organization.Departments[0].Seats[0]
	mandate := seed.Mandates[0]
	policyValue := seed.Policies[0]

	refs, err := store.LoadCurrentPolicyRefs(ctx)
	if err != nil || len(refs) != 1 || refs[0].ID != policyValue.ID {
		t.Fatalf("current policy refs = %#v, %v", refs, err)
	}
	loadedMandate, err := store.LoadMandate(ctx, mandate.ID, mandate.Version)
	if err != nil || loadedMandate.ID != mandate.ID {
		t.Fatalf("loaded mandate = %#v, %v", loadedMandate, err)
	}
	loadedSeat, err := store.LoadSeat(ctx, seat.ID, seat.Version)
	if err != nil || loadedSeat.ID != seat.ID {
		t.Fatalf("loaded seat = %#v, %v", loadedSeat, err)
	}
	currentSeat, err := store.LoadCurrentSeat(ctx, seat.ID)
	if err != nil || currentSeat.Version != seat.Version {
		t.Fatalf("current seat = %#v, %v", currentSeat, err)
	}
	runtimeAuthority, err := store.LoadCurrentRuntimeAuthority(ctx, seed.RuntimeAuthority.KeyID)
	if err != nil || runtimeAuthority.ID != seed.RuntimeAuthority.ID {
		t.Fatalf("current runtime authority = %#v, %v", runtimeAuthority, err)
	}

	lease := validLease(
		scope,
		store.root.OrganizationID,
		seat,
		mandate,
		policyValue,
		canonicalHash(t, &policyValue),
	)
	if err := SignWakeLease(&lease, seed.RuntimeAuthority.KeyID, privateKey); err != nil {
		t.Fatalf("sign runtime lease: %v", err)
	}
	if err := VerifyWakeLeaseAuthority(
		lease, seed.RuntimeAuthority.KeyID, store.root.PublicKey,
	); err != nil {
		t.Fatalf("verify runtime lease: %v", err)
	}
	if err := VerifySeatAuthority(seat, store.root.KeyID, store.root.PublicKey); err != nil {
		t.Fatalf("verify seat authority: %v", err)
	}
	if err := VerifyMandateAuthority(mandate, store.root.KeyID, store.root.PublicKey); err != nil {
		t.Fatalf("verify mandate authority: %v", err)
	}
	if err := store.RegisterLease(ctx, lease); err != nil {
		t.Fatalf("register lease: %v", err)
	}
	loadedLease, err := store.LoadLease(ctx, lease.ID)
	if err != nil || loadedLease.ID != lease.ID {
		t.Fatalf("loaded lease = %#v, %v", loadedLease, err)
	}

	packet := contracts.WorkPacket{Lease: lease, Seat: seat, Mandate: mandate}
	if err := store.AuthorizeWorkPacket(ctx, packet); err != nil {
		t.Fatalf("authorize exact packet: %v", err)
	}
	clockFailure := *store
	clockFailure.now = func() time.Time { return time.Time{} }
	if err := clockFailure.AuthorizeWorkPacket(ctx, packet); err == nil {
		t.Fatal("authorized work packet under invalid clock")
	}
	otherSeat := seed.Organization.Departments[0].Seats[1]
	if err := store.AuthorizeWorkPacket(
		ctx,
		contracts.WorkPacket{Lease: lease, Seat: otherSeat, Mandate: mandate},
	); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("other signed seat error = %v, want ErrLeaseInvalid", err)
	}
	otherMandate := seed.Mandates[1]
	if err := store.AuthorizeWorkPacket(
		ctx,
		contracts.WorkPacket{Lease: lease, Seat: seat, Mandate: otherMandate},
	); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("other signed mandate error = %v, want ErrLeaseInvalid", err)
	}
	for name, mutate := range map[string]func(*contracts.WorkPacket){
		"organization":    func(value *contracts.WorkPacket) { value.Lease.OrganizationID = "other" },
		"lease signature": func(value *contracts.WorkPacket) { value.Lease.Reason = "tampered" },
		"seat signature":  func(value *contracts.WorkPacket) { value.Seat.BindingVersion++ },
		"mandate signature": func(value *contracts.WorkPacket) {
			value.Mandate.AllowedSkills = append(value.Mandate.AllowedSkills, "z-tampered")
		},
		"seat id":              func(value *contracts.WorkPacket) { value.Seat.ID = "other-seat" },
		"seat did":             func(value *contracts.WorkPacket) { value.Seat.DID = "did:matrix:other" },
		"seat org":             func(value *contracts.WorkPacket) { value.Seat.OrganizationID = "other" },
		"seat mandate":         func(value *contracts.WorkPacket) { value.Seat.MandateID = "other" },
		"seat mandate version": func(value *contracts.WorkPacket) { value.Seat.MandateVersion++ },
		"mandate id":           func(value *contracts.WorkPacket) { value.Mandate.ID = "other" },
		"mandate version":      func(value *contracts.WorkPacket) { value.Mandate.Version++ },
		"mandate org":          func(value *contracts.WorkPacket) { value.Mandate.OrganizationID = "other" },
		"mandate seat role":    func(value *contracts.WorkPacket) { value.Mandate.SeatRole = contracts.SeatExecutor },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := packet
			candidate.Mandate.AllowedSkills = append(
				[]contracts.SkillID(nil), packet.Mandate.AllowedSkills...,
			)
			mutate(&candidate)
			if err := store.AuthorizeWorkPacket(ctx, candidate); err == nil {
				t.Fatal("authorized mismatched packet")
			}
		})
	}

	if _, err := store.LoadMandate(ctx, "missing", 1); !errors.Is(err, ErrRevoked) {
		t.Fatalf("missing mandate = %v, want ErrRevoked", err)
	}
	if _, err := store.LoadSeat(ctx, "missing", 1); !errors.Is(err, ErrRevoked) {
		t.Fatalf("missing seat = %v, want ErrRevoked", err)
	}
	if _, err := store.LoadCurrentSeat(ctx, "missing"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("missing current seat = %v, want ErrRevoked", err)
	}
	if _, err := store.LoadLease(ctx, "missing"); !errors.Is(err, ErrLeaseInvalid) {
		t.Fatalf("missing lease = %v, want ErrLeaseInvalid", err)
	}
	if _, err := store.LoadCurrentRuntimeAuthority(ctx, "missing"); !errors.Is(err, ErrRevoked) {
		t.Fatalf("missing runtime authority = %v, want ErrRevoked", err)
	}
}

func TestIntegrationAuthorityLoadersRejectDurableCorruption(t *testing.T) {
	ctx := context.Background()
	for _, test := range []struct {
		name string
		kind Kind
		id   func(Seed) string
		load func(context.Context, *Store, Seed) error
		bad  func(Seed) []byte
	}{
		{
			name: "runtime canonical", kind: KindRuntime,
			id: func(seed Seed) string { return seed.RuntimeAuthority.ID },
			load: func(ctx context.Context, store *Store, seed Seed) error {
				_, err := store.LoadRuntimeAuthority(ctx, seed.RuntimeAuthority.ID, 1)
				return err
			},
			bad: func(Seed) []byte { return []byte(`{}`) },
		},
		{
			name: "runtime signature", kind: KindRuntime,
			id: func(seed Seed) string { return seed.RuntimeAuthority.ID },
			load: func(ctx context.Context, store *Store, seed Seed) error {
				_, err := store.LoadRuntimeAuthority(ctx, seed.RuntimeAuthority.ID, 1)
				return err
			},
			bad: func(seed Seed) []byte {
				value := seed.RuntimeAuthority
				value.Signature.Value = zeroPolicySignature()
				return canonicalPolicyTestValue(&value)
			},
		},
		{
			name: "mandate canonical", kind: KindMandate,
			id: func(seed Seed) string { return string(seed.Mandates[0].ID) },
			load: func(ctx context.Context, store *Store, seed Seed) error {
				_, err := store.LoadMandate(ctx, seed.Mandates[0].ID, 1)
				return err
			},
			bad: func(Seed) []byte { return []byte(`{}`) },
		},
		{
			name: "mandate signature", kind: KindMandate,
			id: func(seed Seed) string { return string(seed.Mandates[0].ID) },
			load: func(ctx context.Context, store *Store, seed Seed) error {
				_, err := store.LoadMandate(ctx, seed.Mandates[0].ID, 1)
				return err
			},
			bad: func(seed Seed) []byte {
				value := seed.Mandates[0]
				value.Signature.Value = zeroPolicySignature()
				return canonicalPolicyTestValue(&value)
			},
		},
		{
			name: "seat canonical", kind: KindSeat,
			id: func(seed Seed) string { return string(seed.Organization.Departments[0].Seats[0].ID) },
			load: func(ctx context.Context, store *Store, seed Seed) error {
				seat := seed.Organization.Departments[0].Seats[0]
				_, err := store.LoadSeat(ctx, seat.ID, 1)
				return err
			},
			bad: func(Seed) []byte { return []byte(`{}`) },
		},
		{
			name: "seat signature", kind: KindSeat,
			id: func(seed Seed) string { return string(seed.Organization.Departments[0].Seats[0].ID) },
			load: func(ctx context.Context, store *Store, seed Seed) error {
				seat := seed.Organization.Departments[0].Seats[0]
				_, err := store.LoadSeat(ctx, seat.ID, 1)
				return err
			},
			bad: func(seed Seed) []byte {
				value := seed.Organization.Departments[0].Seats[0]
				value.Signature.Value = zeroPolicySignature()
				return canonicalPolicyTestValue(&value)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			store, _, seed, _, _, _ := policyFixture(t)
			if _, err := store.PublishSeed(ctx, seed); err != nil {
				t.Fatal(err)
			}
			replaceAuthorityRecord(
				t, ctx, store, test.kind, test.id(seed), 1, test.bad(seed),
			)
			if err := test.load(ctx, store, seed); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("corrupt authority error = %v, want ErrIntegrity", err)
			}
		})
	}
}

func TestIntegrationLeaseLoaderRejectsVaultHashCanonicalAndClockCorruption(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"vault", "hash", "canonical", "clock"} {
		t.Run(mode, func(t *testing.T) {
			store, privateKey, seed, _, _, scope := policyFixture(t)
			if _, err := store.PublishSeed(ctx, seed); err != nil {
				t.Fatal(err)
			}
			seat := seed.Organization.Departments[0].Seats[0]
			mandate := seed.Mandates[0]
			policyValue := seed.Policies[0]
			lease := validLease(
				scope,
				store.root.OrganizationID,
				seat,
				mandate,
				policyValue,
				canonicalHash(t, &policyValue),
			)
			if err := SignWakeLease(&lease, seed.RuntimeAuthority.KeyID, privateKey); err != nil {
				t.Fatal(err)
			}
			if err := store.RegisterLease(ctx, lease); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "vault":
				updateLeaseStorage(t, ctx, store, lease.ID, []byte("not-a-vault"), nil)
			case "hash":
				wrong := "0"
				updateLeaseStorage(t, ctx, store, lease.ID, nil, &wrong)
			case "canonical":
				canonical := []byte(`{}`)
				sealed, err := store.vault.SealRecord(leaseAD(store, lease.ID), canonical)
				if err != nil {
					t.Fatal(err)
				}
				hash := hashPolicyTestBytes(canonical)
				updateLeaseStorage(t, ctx, store, lease.ID, sealed, &hash)
			case "clock":
				store.now = func() time.Time { return time.Time{} }
			}
			if _, err := store.LoadLease(ctx, lease.ID); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("%s lease error = %v, want ErrIntegrity", mode, err)
			}
		})
	}
}

func TestIntegrationLeaseRegistrationRejectsFreshlySignedStaleBindings(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"mandate", "seat", "policy"} {
		t.Run(mode, func(t *testing.T) {
			store, privateKey, seed, _, _, scope := policyFixture(t)
			if _, err := store.PublishSeed(ctx, seed); err != nil {
				t.Fatal(err)
			}
			seat := seed.Organization.Departments[0].Seats[0]
			mandate := seed.Mandates[0]
			policyValue := seed.Policies[0]
			lease := validLease(
				scope, store.root.OrganizationID, seat, mandate, policyValue,
				canonicalHash(t, &policyValue),
			)
			switch mode {
			case "mandate":
				lease.MandateVersion++
			case "seat":
				other := seed.Organization.Departments[0].Seats[1]
				lease.SeatID = other.ID
				lease.SeatDID = other.DID
			case "policy":
				lease.Policies[0].Version++
			}
			if err := SignWakeLease(&lease, seed.RuntimeAuthority.KeyID, privateKey); err != nil {
				t.Fatal(err)
			}
			if err := store.RegisterLease(ctx, lease); !errors.Is(err, ErrLeaseInvalid) {
				t.Fatalf("%s stale binding error = %v, want ErrLeaseInvalid", mode, err)
			}
		})
	}
}

func TestIntegrationAuthorizeLeaseRejectsDurablyStaleBindings(t *testing.T) {
	ctx := context.Background()
	for _, mode := range []string{"mandate", "policy"} {
		t.Run(mode, func(t *testing.T) {
			store, _, _, lease := publishAndRegisterPolicyLease(t, ctx)
			table := "workforce_authority_leases"
			if mode == "policy" {
				table = "workforce_authority_lease_policies"
			}
			if _, err := policyPool.Exec(ctx, `ALTER TABLE `+table+` DISABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_, _ = policyPool.Exec(context.Background(), `ALTER TABLE `+table+` ENABLE TRIGGER USER`)
			}()
			var err error
			if mode == "mandate" {
				_, err = policyPool.Exec(ctx, `
					UPDATE workforce_authority_leases SET mandate_version=999
					WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
				`, store.root.TenantID, store.root.OrganizationID, lease.ID)
			} else {
				_, err = policyPool.Exec(ctx, `
					UPDATE workforce_authority_lease_policies SET policy_version=999
					WHERE tenant_id=$1 AND organization_id=$2 AND lease_id=$3
				`, store.root.TenantID, store.root.OrganizationID, lease.ID)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := policyPool.Exec(ctx, `ALTER TABLE `+table+` ENABLE TRIGGER USER`); err != nil {
				t.Fatal(err)
			}
			if err := store.AuthorizeLease(ctx, lease.ID); !errors.Is(err, ErrLeaseInvalid) {
				t.Fatalf("%s durable stale error = %v, want ErrLeaseInvalid", mode, err)
			}
		})
	}
}

func publishAndRegisterPolicyLease(
	t *testing.T,
	ctx context.Context,
) (*Store, Seed, ed25519.PrivateKey, contracts.WakeLease) {
	t.Helper()
	store, privateKey, seed, _, _, scope := policyFixture(t)
	if _, err := store.PublishSeed(ctx, seed); err != nil {
		t.Fatal(err)
	}
	seat := seed.Organization.Departments[0].Seats[0]
	mandate := seed.Mandates[0]
	policyValue := seed.Policies[0]
	lease := validLease(
		scope, store.root.OrganizationID, seat, mandate, policyValue,
		canonicalHash(t, &policyValue),
	)
	if err := SignWakeLease(&lease, seed.RuntimeAuthority.KeyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := store.RegisterLease(ctx, lease); err != nil {
		t.Fatal(err)
	}
	return store, seed, privateKey, lease
}

func replaceAuthorityRecord(
	t *testing.T,
	ctx context.Context,
	store *Store,
	kind Kind,
	id string,
	version uint64,
	canonical []byte,
) {
	t.Helper()
	sealed, err := store.vault.SealRecord(store.authorityAD(kind, id, version), canonical)
	if err != nil {
		t.Fatalf("seal corrupt authority: %v", err)
	}
	if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_records DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = policyPool.Exec(context.Background(), `ALTER TABLE workforce_authority_records ENABLE TRIGGER USER`)
	}()
	if _, err := policyPool.Exec(ctx, `
		UPDATE workforce_authority_records
		SET canonical_hash=$1,sealed_record=$2
		WHERE tenant_id=$3 AND organization_id=$4
		  AND authority_kind=$5 AND authority_id=$6 AND version=$7
	`, hashPolicyTestBytes(canonical), sealed, store.root.TenantID,
		store.root.OrganizationID, kind, id, version); err != nil {
		t.Fatal(err)
	}
	if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_records ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func updateLeaseStorage(
	t *testing.T,
	ctx context.Context,
	store *Store,
	id contracts.LeaseID,
	sealed []byte,
	hash *string,
) {
	t.Helper()
	if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_leases DISABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, _ = policyPool.Exec(context.Background(), `ALTER TABLE workforce_authority_leases ENABLE TRIGGER USER`)
	}()
	if sealed != nil {
		if _, err := policyPool.Exec(ctx, `
			UPDATE workforce_authority_leases SET sealed_lease=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND lease_id=$4
		`, sealed, store.root.TenantID, store.root.OrganizationID, id); err != nil {
			t.Fatal(err)
		}
	}
	if hash != nil {
		if _, err := policyPool.Exec(ctx, `
			UPDATE workforce_authority_leases SET canonical_hash=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND lease_id=$4
		`, *hash, store.root.TenantID, store.root.OrganizationID, id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_leases ENABLE TRIGGER USER`); err != nil {
		t.Fatal(err)
	}
}

func leaseAD(store *Store, id contracts.LeaseID) vault.AD {
	return vault.AD{
		User: store.root.TenantID, Store: "workforce.authority.lease",
		Stream: string(store.root.OrganizationID) + "/" + string(id),
		Schema: contracts.SchemaVersionV1,
	}
}

func canonicalPolicyTestValue(value contracts.Validatable) []byte {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		panic(err)
	}
	return canonical
}

func zeroPolicySignature() string {
	return base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))
}

func hashPolicyTestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func TestLoadCurrentPolicyRefsRequiresAnEffectivePolicy(t *testing.T) {
	store, _, _, _, _, _ := policyFixture(t)
	refs, err := store.LoadCurrentPolicyRefs(context.Background())
	if !errors.Is(err, ErrUnauthorized) || refs != nil {
		t.Fatalf("empty policy refs = %#v, %v", refs, err)
	}
}

func TestPublishSeedRejectsTamperedAndNotYetEffectiveAuthority(t *testing.T) {
	tests := map[string]func(*testing.T, *Store, ed25519.PrivateKey, *Seed){
		"owner mismatch": func(t *testing.T, store *Store, key ed25519.PrivateKey, seed *Seed) {
			seed.Organization.OwnerID = "owner-other"
			if err := SignOrganization(&seed.Organization, store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
		},
		"organization future": func(t *testing.T, store *Store, key ed25519.PrivateKey, seed *Seed) {
			seed.Organization.EffectiveAt = policyNow().Add(time.Hour)
			if err := SignOrganization(&seed.Organization, store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
		},
		"organization signature": func(_ *testing.T, _ *Store, _ ed25519.PrivateKey, seed *Seed) {
			seed.Organization.Name += " tampered"
		},
		"mandate signature": func(_ *testing.T, _ *Store, _ ed25519.PrivateKey, seed *Seed) {
			seed.Mandates[0].AllowedSkills[0] = "aaa-tampered"
		},
		"mandate future": func(t *testing.T, store *Store, key ed25519.PrivateKey, seed *Seed) {
			seed.Mandates[0].EffectiveAt = policyNow().Add(time.Hour)
			if err := SignMandate(&seed.Mandates[0], store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
		},
		"runtime signature": func(t *testing.T, _ *Store, _ ed25519.PrivateKey, seed *Seed) {
			publicKey, _, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			seed.RuntimeAuthority.PublicKey = base64.RawURLEncoding.EncodeToString(publicKey)
		},
		"runtime future": func(t *testing.T, store *Store, key ed25519.PrivateKey, seed *Seed) {
			seed.RuntimeAuthority.EffectiveAt = policyNow().Add(time.Hour)
			if err := SignRuntimeAuthority(&seed.RuntimeAuthority, store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
		},
		"policy signature": func(_ *testing.T, _ *Store, _ ed25519.PrivateKey, seed *Seed) {
			seed.Policies[0].Rules[0].Scope += " tampered"
		},
		"policy future": func(t *testing.T, store *Store, key ed25519.PrivateKey, seed *Seed) {
			seed.Policies[0].EffectiveAt = policyNow().Add(time.Hour)
			if err := SignPolicy(&seed.Policies[0], store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
		},
		"seat signature": func(t *testing.T, store *Store, key ed25519.PrivateKey, seed *Seed) {
			seed.Organization.Departments[0].Seats[0].BindingVersion++
			if err := SignOrganization(&seed.Organization, store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
		},
		"seat future": func(t *testing.T, store *Store, key ed25519.PrivateKey, seed *Seed) {
			seat := &seed.Organization.Departments[0].Seats[0]
			seat.EffectiveAt = policyNow().Add(time.Hour)
			if err := SignSeat(seat, store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
			if err := SignOrganization(&seed.Organization, store.root.KeyID, key); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			store, privateKey, seed, _, _, _ := policyFixture(t)
			mutate(t, store, privateKey, &seed)
			if _, err := store.PublishSeed(context.Background(), seed); err == nil {
				t.Fatal("published invalid seed authority")
			}
		})
	}

	t.Run("invalid clock", func(t *testing.T) {
		store, _, seed, _, _, _ := policyFixture(t)
		store.now = func() time.Time { return time.Time{} }
		if _, err := store.PublishSeed(context.Background(), seed); err == nil {
			t.Fatal("published seed under invalid clock")
		}
	})
}

func TestIntegrationDuplicateSeedDetectsDriftAndIncompleteAuthority(t *testing.T) {
	ctx := context.Background()
	t.Run("drift", func(t *testing.T) {
		store, _, seed, _, _, _ := policyFixture(t)
		if _, err := store.PublishSeed(ctx, seed); err != nil {
			t.Fatal(err)
		}
		if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_records DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = policyPool.Exec(context.Background(), `ALTER TABLE workforce_authority_records ENABLE TRIGGER USER`)
		}()
		if _, err := policyPool.Exec(ctx, `
			UPDATE workforce_authority_records SET canonical_hash=$1
			WHERE tenant_id=$2 AND organization_id=$3
			  AND authority_kind='organization' AND authority_id=$3 AND version=1
		`, "0", store.root.TenantID, store.root.OrganizationID); err != nil {
			t.Fatal(err)
		}
		if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_records ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PublishSeed(ctx, seed); !errors.Is(err, ErrStale) {
			t.Fatalf("drift error = %v, want ErrStale", err)
		}
	})
	t.Run("incomplete", func(t *testing.T) {
		store, _, seed, _, _, _ := policyFixture(t)
		if _, err := store.PublishSeed(ctx, seed); err != nil {
			t.Fatal(err)
		}
		if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_records DISABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		defer func() {
			_, _ = policyPool.Exec(context.Background(), `ALTER TABLE workforce_authority_records ENABLE TRIGGER USER`)
		}()
		if _, err := policyPool.Exec(ctx, `
			DELETE FROM workforce_authority_heads
			WHERE tenant_id=$1 AND organization_id=$2
			  AND authority_kind='mandate' AND authority_id=$3
		`, store.root.TenantID, store.root.OrganizationID, seed.Mandates[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := policyPool.Exec(ctx, `
			DELETE FROM workforce_authority_records
			WHERE tenant_id=$1 AND organization_id=$2
			  AND authority_kind='mandate' AND authority_id=$3
		`, store.root.TenantID, store.root.OrganizationID, seed.Mandates[0].ID); err != nil {
			t.Fatal(err)
		}
		if _, err := policyPool.Exec(ctx, `ALTER TABLE workforce_authority_records ENABLE TRIGGER USER`); err != nil {
			t.Fatal(err)
		}
		if _, err := store.PublishSeed(ctx, seed); !errors.Is(err, ErrIntegrity) {
			t.Fatalf("incomplete seed error = %v, want ErrIntegrity", err)
		}
	})
	t.Run("deduplicated commit failure", func(t *testing.T) {
		store, _, seed, _, _, _ := policyFixture(t)
		if _, err := store.PublishSeed(ctx, seed); err != nil {
			t.Fatal(err)
		}
		expected := errors.New("projection rejected")
		_, err := store.PublishSeedWithCommit(
			ctx,
			seed,
			func(context.Context, pgx.Tx, time.Time) error { return expected },
		)
		if !errors.Is(err, expected) {
			t.Fatalf("deduplicated commit error = %v, want %v", err, expected)
		}
	})
}

func TestRuntimeAuthorityAndSeedRejectEveryBindingViolation(t *testing.T) {
	store, privateKey, seed, writeGrant, _, _ := policyFixture(t)
	authority := seed.RuntimeAuthority
	expiry := authority.EffectiveAt.Add(time.Hour)
	authority.ExpiresAt = &expiry
	if err := authority.Validate(); err != nil {
		t.Fatalf("valid expiring runtime authority: %v", err)
	}
	if err := SignRuntimeAuthority(&authority, store.root.KeyID, privateKey); err != nil {
		t.Fatalf("sign expiring runtime authority: %v", err)
	}
	invalidRuntime := seed.RuntimeAuthority
	invalidRuntime.ID = ""
	if err := SignRuntimeAuthority(nil, store.root.KeyID, privateKey); err == nil {
		t.Fatal("signed nil runtime authority")
	}
	if err := store.PublishRuntimeAuthority(context.Background(), invalidRuntime, writeGrant); err == nil {
		t.Fatal("published invalid runtime authority")
	}
	tamperedRuntime := seed.RuntimeAuthority
	tamperedRuntime.EffectiveAt = tamperedRuntime.EffectiveAt.Add(-time.Minute)
	if err := store.PublishRuntimeAuthority(context.Background(), tamperedRuntime, writeGrant); err == nil {
		t.Fatal("published runtime authority with stale signature")
	}
	if err := SignWakeLease(nil, store.root.KeyID, privateKey); err == nil {
		t.Fatal("signed nil wake lease")
	}
	for name, mutate := range map[string]func(*RuntimeAuthority){
		"schema":              func(value *RuntimeAuthority) { value.SchemaVersion = "bad" },
		"id":                  func(value *RuntimeAuthority) { value.ID = "" },
		"version":             func(value *RuntimeAuthority) { value.Version = 0 },
		"organization":        func(value *RuntimeAuthority) { value.OrganizationID = "" },
		"key id":              func(value *RuntimeAuthority) { value.KeyID = "" },
		"id binding":          func(value *RuntimeAuthority) { value.ID = "runtime-authority:other" },
		"public key encoding": func(value *RuntimeAuthority) { value.PublicKey = "%%%" },
		"public key length":   func(value *RuntimeAuthority) { value.PublicKey = "YQ" },
		"purpose empty":       func(value *RuntimeAuthority) { value.Purposes = nil },
		"purpose many":        func(value *RuntimeAuthority) { value.Purposes = []string{WakeLeaseSigningPurpose, "other"} },
		"purpose wrong":       func(value *RuntimeAuthority) { value.Purposes = []string{"other"} },
		"effective zero":      func(value *RuntimeAuthority) { value.EffectiveAt = time.Time{} },
		"effective non utc": func(value *RuntimeAuthority) {
			value.EffectiveAt = value.EffectiveAt.In(time.FixedZone("non-utc", 3600))
		},
		"expiry non utc": func(value *RuntimeAuthority) {
			expires := value.ExpiresAt.In(time.FixedZone("non-utc", 3600))
			value.ExpiresAt = &expires
		},
		"expiry order": func(value *RuntimeAuthority) {
			expires := value.EffectiveAt
			value.ExpiresAt = &expires
		},
		"signature": func(value *RuntimeAuthority) { value.Signature = contracts.Signature{} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := authority
			candidate.Purposes = append([]string(nil), authority.Purposes...)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("accepted invalid runtime authority")
			}
		})
	}

	for name, mutate := range map[string]func(*Seed){
		"organization":         func(value *Seed) { value.Organization.ID = "" },
		"mandate count":        func(value *Seed) { value.Mandates = value.Mandates[:20] },
		"runtime":              func(value *Seed) { value.RuntimeAuthority.ID = "" },
		"runtime organization": func(value *Seed) { value.RuntimeAuthority.OrganizationID = "other" },
		"policy count":         func(value *Seed) { value.Policies = nil },
		"policy invalid":       func(value *Seed) { value.Policies[0].ID = "" },
		"policy organization":  func(value *Seed) { value.Policies[0].OrganizationID = "other" },
		"mandate invalid":      func(value *Seed) { value.Mandates[0].ID = "" },
		"mandate organization": func(value *Seed) { value.Mandates[0].OrganizationID = "other" },
		"mandate duplicate":    func(value *Seed) { value.Mandates[1] = value.Mandates[0] },
		"seat mandate missing": func(value *Seed) { value.Organization.Departments[0].Seats[0].MandateID = "missing" },
		"seat mandate version": func(value *Seed) { value.Organization.Departments[0].Seats[0].MandateVersion++ },
		"seat mandate kind": func(value *Seed) {
			value.Mandates[0].DepartmentKind = contracts.DepartmentLegal
		},
		"seat mandate role": func(value *Seed) { value.Mandates[0].SeatRole = contracts.SeatExecutor },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneSeed(seed)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatal("accepted invalid seed")
			}
		})
	}

	if _, err := BuildSeedDraft(
		store.root.OrganizationID,
		store.root.OwnerID,
		"Organization",
		policyNow(),
		store.root.KeyID,
		"",
		store.root.PublicKey,
	); err == nil {
		t.Fatal("built seed draft without runtime key id")
	}
	if _, err := BuildSeedDraft(
		store.root.OrganizationID,
		store.root.OwnerID,
		"Organization",
		policyNow(),
		store.root.KeyID,
		store.root.KeyID,
		ed25519.PublicKey{1},
	); err == nil {
		t.Fatal("built seed draft with invalid runtime key")
	}
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	if _, err := BuildSeedDraft(
		"", store.root.OwnerID, "Organization", policyNow(),
		store.root.KeyID, store.root.KeyID, privateKey.Public().(ed25519.PublicKey),
	); err == nil {
		t.Fatal("built seed draft with invalid organization")
	}
}

func TestRuntimeLeaseAuthorityRejectsMissingFutureAndExpiredDelegation(t *testing.T) {
	ctx := context.Background()
	t.Run("missing", func(t *testing.T) {
		store, _, _, _, _, _ := policyFixture(t)
		lease := contracts.WakeLease{Signature: contracts.Signature{KeyID: "missing"}}
		if err := store.verifyRuntimeLeaseAuthority(ctx, lease, policyNow()); !errors.Is(err, ErrLeaseInvalid) {
			t.Fatalf("missing runtime authority error = %v", err)
		}
	})
	t.Run("future", func(t *testing.T) {
		store, privateKey, seed, _, _, scope := policyFixture(t)
		if _, err := store.PublishSeed(ctx, seed); err != nil {
			t.Fatal(err)
		}
		lease := validLease(
			scope,
			store.root.OrganizationID,
			seed.Organization.Departments[0].Seats[0],
			seed.Mandates[0],
			seed.Policies[0],
			canonicalHash(t, &seed.Policies[0]),
		)
		if err := SignWakeLease(&lease, seed.RuntimeAuthority.KeyID, privateKey); err != nil {
			t.Fatal(err)
		}
		if err := store.verifyRuntimeLeaseAuthority(
			ctx, lease, seed.RuntimeAuthority.EffectiveAt.Add(-time.Second),
		); !errors.Is(err, ErrLeaseInvalid) {
			t.Fatalf("future runtime authority error = %v", err)
		}
	})
	t.Run("expired", func(t *testing.T) {
		store, privateKey, seed, writeGrant, _, scope := policyFixture(t)
		authority := seed.RuntimeAuthority
		authority.EffectiveAt = policyNow().Add(-2 * time.Hour)
		expiresAt := policyNow().Add(-time.Hour)
		authority.ExpiresAt = &expiresAt
		if err := SignRuntimeAuthority(&authority, store.root.KeyID, privateKey); err != nil {
			t.Fatal(err)
		}
		if err := store.PublishRuntimeAuthority(ctx, authority, writeGrant); err != nil {
			t.Fatal(err)
		}
		lease := validLease(
			scope,
			store.root.OrganizationID,
			seed.Organization.Departments[0].Seats[0],
			seed.Mandates[0],
			seed.Policies[0],
			canonicalHash(t, &seed.Policies[0]),
		)
		if err := SignWakeLease(&lease, authority.KeyID, privateKey); err != nil {
			t.Fatal(err)
		}
		if err := store.verifyRuntimeLeaseAuthority(ctx, lease, policyNow()); !errors.Is(err, ErrLeaseInvalid) {
			t.Fatalf("expired runtime authority error = %v", err)
		}
	})
}

func TestInvalidClockBlocksPolicyListingAndGrantVerification(t *testing.T) {
	store, _, _, grant, _, _ := policyFixture(t)
	store.now = func() time.Time { return time.Time{} }
	if _, err := store.LoadCurrentPolicyRefs(context.Background()); err == nil {
		t.Fatal("listed policies under invalid clock")
	}
	if _, err := store.verifyGrant(grant, grant.Scope); err == nil {
		t.Fatal("verified owner grant under invalid clock")
	}
}

func cloneSeed(seed Seed) Seed {
	clone := seed
	clone.Mandates = append([]contracts.Mandate(nil), seed.Mandates...)
	clone.Policies = append([]contracts.Policy(nil), seed.Policies...)
	clone.Organization.Departments = append(
		[]contracts.Department(nil), seed.Organization.Departments...,
	)
	for index := range clone.Organization.Departments {
		clone.Organization.Departments[index].Seats = append(
			[]contracts.Seat(nil), seed.Organization.Departments[index].Seats...,
		)
	}
	return clone
}
