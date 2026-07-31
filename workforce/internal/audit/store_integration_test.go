package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
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
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/testauthority"
)

const auditPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var auditPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, databaseURL, err := startAuditPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "audit integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", container).Run()
	}
	auditPool, err = waitAuditPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "audit integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, auditPool, auditNow()); err != nil {
		auditPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "audit migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	auditPool.Close()
	cleanup()
	os.Exit(code)
}

func TestVerdictStoreSamplingProofAndDisagreementIncident(t *testing.T) {
	ctx := context.Background()
	tenant := "tenant-audit-store"
	organizationID := contracts.OrganizationID("org-audit")
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	current := auditNow()
	authorityPublic, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := policy.New(
		auditPool, session.UserVault(), policy.OwnerRoot{
			TenantID: tenant, OrganizationID: organizationID,
			OwnerID: "owner-audit", KeyID: "owner-key", PublicKey: authorityPublic,
		}, func() time.Time { return current },
	)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(
		auditPool, session.UserVault(), tenant, "audit-key", privateKey,
		authority,
		func() time.Time { return current },
	)
	if err != nil {
		t.Fatal(err)
	}
	packet := auditPacket(t, "intent-store")
	runner := storeAuditRunner(t, &packet)
	decision, err := runner.RunVerified(ctx, packet)
	if err != nil {
		t.Fatal(err)
	}
	executingSeat := signedAuditSeat(
		t, authorityPrivate, packet.ExecutingSeatID, organizationID,
		"department-developer", contracts.SeatExecutor,
	)
	auditorSeat := signedAuditSeat(
		t, authorityPrivate, packet.AuditorSeatID, organizationID,
		"department-legal", contracts.SeatAuditor,
	)
	sameDepartmentAuditor := signedAuditSeat(
		t, authorityPrivate, "seat-auditor-developer", organizationID,
		"department-developer", contracts.SeatAuditor,
	)
	samplingSeats := []contracts.Seat{
		signedAuditSeat(
			t, authorityPrivate, "seat-accounting", organizationID,
			"department-accounting", contracts.SeatAuditor,
		),
		signedAuditSeat(
			t, authorityPrivate, "seat-executive", organizationID,
			"department-executive", contracts.SeatAuditor,
		),
		signedAuditSeat(
			t, authorityPrivate, "seat-research", organizationID,
			"department-research", contracts.SeatAuditor,
		),
	}
	grant := policy.OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      tenant, OrganizationID: organizationID, OwnerID: "owner-audit",
		Scope: "authority:write", IssuedAt: current.Add(-time.Minute),
		ExpiresAt: current.Add(time.Hour),
	}
	if err := policy.SignOwnerGrant(&grant, "owner-key", authorityPrivate); err != nil {
		t.Fatal(err)
	}
	if err := testauthority.PublishRuntimeAuthority(
		ctx, authority, organizationID, "owner-key",
		authorityPublic, authorityPrivate, grant, current.Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	for _, seat := range append(
		[]contracts.Seat{executingSeat, auditorSeat, sameDepartmentAuditor},
		samplingSeats...,
	) {
		if err := authority.PublishSeat(ctx, seat, grant); err != nil {
			t.Fatal(err)
		}
	}
	auditorLeases := make(map[contracts.SeatID]contracts.WakeLease)
	for _, seat := range append(
		[]contracts.Seat{auditorSeat, sameDepartmentAuditor},
		samplingSeats...,
	) {
		auditorLeases[seat.ID] = publishAuditLease(
			t, ctx, authority, authorityPrivate, grant, seat, packet,
		)
	}
	for _, id := range []contracts.VerdictID{"verdict-one", "verdict-two", "verdict-three"} {
		verdict, err := store.Commit(ctx, CommitRequest{
			ID: id, Packet: packet, AuditorLease: auditorLeases[auditorSeat.ID],
			Decision: decision,
		})
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := store.LoadVerdict(ctx, organizationID, id)
		if err != nil || loaded.Signature != verdict.Signature || loaded.Outcome != contracts.VerdictPass {
			t.Fatalf("loaded verdict = %#v, %v", loaded, err)
		}
	}
	sameDepartmentPacket := packet
	sameDepartmentPacket.AuditorSeatID = sameDepartmentAuditor.ID
	sameDepartmentDecision, err := runner.RunVerified(ctx, sameDepartmentPacket)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Commit(ctx, CommitRequest{
		ID: "verdict-self", Packet: sameDepartmentPacket,
		AuditorLease: auditorLeases[sameDepartmentAuditor.ID],
		Decision:     sameDepartmentDecision,
	}); err != ErrUnauthorized {
		t.Fatalf("same-department attestation error = %v", err)
	}
	for _, column := range []string{"sealed_packet", "sealed_verdict"} {
		var sealed []byte
		query := "SELECT " + column + " FROM workforce_verdict_records WHERE tenant_id=$1 LIMIT 1"
		if err := auditPool.QueryRow(ctx, query, tenant).Scan(&sealed); err != nil {
			t.Fatal(err)
		}
		if !vault.IsVault(sealed) || bytes.Contains(sealed, []byte("intent-store")) {
			t.Fatalf("%s is not Vault-sealed", column)
		}
	}

	current = current.Add(2 * time.Second)
	auditors := []contracts.SeatID{
		samplingSeats[0].ID, samplingSeats[1].ID, samplingSeats[2].ID,
	}
	proof, err := store.Sample(
		ctx, organizationID, "epoch-one", current.Add(-time.Second),
		SamplingPolicy{Numerator: 1, Denominator: 1, Minimum: 1, Maximum: 10},
		auditors,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(proof.Population) != 3 || len(proof.Selections) != 3 {
		t.Fatalf("sample population=%d selections=%d", len(proof.Population), len(proof.Selections))
	}
	if err := VerifyProof(proof); err != nil {
		t.Fatalf("valid sample proof rejected: %v", err)
	}
	for _, selection := range proof.Selections {
		if selection.ReauditorDepartmentID == "department-developer" {
			t.Fatal("sample assigned work to its executing department")
		}
	}
	tampered := proof
	tampered.Selections = append([]Selection(nil), proof.Selections...)
	tampered.Selections[0].Score = strings.Repeat("0", 64)
	if err := VerifyProof(tampered); err != ErrIntegrity {
		t.Fatalf("tampered proof error = %v", err)
	}
	var sealedSeed []byte
	if err := auditPool.QueryRow(ctx, `
		SELECT sealed_seed FROM workforce_cross_audit_epochs
		WHERE tenant_id=$1 AND organization_id=$2 AND epoch_id=$3
	`, tenant, organizationID, proof.EpochID).Scan(&sealedSeed); err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(sealedSeed) || bytes.Contains(sealedSeed, []byte(proof.Seed)) {
		t.Fatal("sampling seed was not Vault-sealed")
	}

	selection := proof.Selections[0]
	reauditPacket := packet
	reauditPacket.AuditorSeatID = selection.ReauditorSeatID
	reauditPacket.Predicates = append(
		[]contracts.VerificationPredicate(nil), packet.Predicates...,
	)
	wrong := auditHash("missing-artifact")
	reauditPacket.Predicates[0].ExpectedHash = &wrong
	reauditDecision, err := runner.RunVerified(ctx, reauditPacket)
	if err != nil {
		t.Fatal(err)
	}
	reaudit, disagreement, err := store.CommitReaudit(
		ctx, organizationID, proof.EpochID, selection.VerdictID,
		CommitRequest{
			ID: "reaudit-" + selection.VerdictID, Packet: reauditPacket,
			AuditorLease: auditorLeases[selection.ReauditorSeatID],
			Decision:     reauditDecision,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !disagreement || reaudit.Outcome != contracts.VerdictFail {
		t.Fatalf("reaudit outcome=%q disagreement=%v", reaudit.Outcome, disagreement)
	}
	var incidents int
	if err := auditPool.QueryRow(ctx, `
		SELECT count(*) FROM workforce_cross_audit_incidents
		WHERE tenant_id=$1 AND organization_id=$2 AND epoch_id=$3
	`, tenant, organizationID, proof.EpochID).Scan(&incidents); err != nil {
		t.Fatal(err)
	}
	if incidents != 1 {
		t.Fatalf("disagreement incidents = %d", incidents)
	}
}

func publishAuditLease(
	t *testing.T,
	ctx context.Context,
	authority *policy.Store,
	privateKey ed25519.PrivateKey,
	grant policy.OwnerGrant,
	seat contracts.Seat,
	packet contracts.VerdictPacket,
) contracts.WakeLease {
	t.Helper()
	mandate := contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            seat.MandateID, Version: seat.MandateVersion,
		OrganizationID: seat.OrganizationID,
		DepartmentKind: contracts.DepartmentLegal, SeatRole: contracts.SeatAuditor,
		AllowedSkills: []contracts.SkillID{packet.Skill.ID},
		DataScopes: []contracts.DataScope{{
			Name: "verdict", Classification: contracts.ClassificationOrganization,
			Purpose: "Independently evaluate the exact VerdictPacket",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "A deterministic predicate cannot be established",
			Action:    "Return a fail-closed verdict",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID:    "no-executing-department",
			Description: "Never audit work from the same department",
		}},
		EffectiveAt: auditNow().Add(-time.Hour),
	}
	if err := policy.SignMandate(&mandate, "owner-key", privateKey); err != nil {
		t.Fatal(err)
	}
	policyValue := contracts.Policy{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.PolicyID("policy:audit:" + string(seat.ID)), Version: 1,
		OrganizationID: seat.OrganizationID, Kind: "auditor_wake",
		EffectiveAt: auditNow().Add(-time.Hour),
		Rules: []contracts.PolicyRule{{
			ClauseID: "independent-auditor", Outcome: "allow",
			Scope: "Only the named Auditor may evaluate the selected intent",
		}},
	}
	if err := policy.SignPolicy(&policyValue, "owner-key", privateKey); err != nil {
		t.Fatal(err)
	}
	canonical, err := contracts.EncodeCanonical(&policyValue)
	if err != nil {
		t.Fatal(err)
	}
	policyRef := contracts.PolicyRef{
		ID: policyValue.ID, Version: policyValue.Version, Hash: hashBytes(canonical),
	}
	if err := authority.PublishMandate(ctx, mandate, grant); err != nil {
		t.Fatal(err)
	}
	if err := authority.PublishPolicy(ctx, policyValue, grant); err != nil {
		t.Fatal(err)
	}
	value := contracts.WakeLease{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             contracts.LeaseID("lease:audit:" + string(seat.ID)),
		WakeID:         contracts.WakeID("wake:audit:" + string(seat.ID)),
		OrganizationID: seat.OrganizationID, SeatID: seat.ID, SeatDID: seat.DID,
		Reason:    "independent_verification",
		MandateID: mandate.ID, MandateVersion: mandate.Version,
		Policies:   []contracts.PolicyRef{policyRef},
		GraphScope: []contracts.IntentID{packet.Intent.ID},
		Model:      packet.Model, MGS: packet.MGS, Runtime: packet.Runtime,
		SkillCatalogDigest: auditHash("audit-skill-catalog"),
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64(time.Minute / time.Millisecond),
			MaxSteps:          10, MaxModelCalls: 1, MaxToolCalls: 1,
			MaxCostMinor: 100, Currency: "USD", MaxOutputBytes: 1 << 20,
		},
		IssuedAt: auditNow(), ExpiresAt: auditNow().Add(time.Hour), Fence: 1,
	}
	if err := policy.SignWakeLease(&value, "owner-key", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := authority.RegisterLease(ctx, value); err != nil {
		t.Fatal(err)
	}
	return value
}

func storeAuditRunner(t *testing.T, packet *contracts.VerdictPacket) Runner {
	t.Helper()
	bubblewrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	binary := t.TempDir() + "/workforce-auditor"
	command := exec.Command(goExecutable, "build", "-o", binary, "../../cmd/workforce-auditor")
	command.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build real workforce-auditor: %v: %s", err, output)
	}
	contents, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	packet.Runtime.AuditorBuildDigest = hashBytes(contents)
	return Runner{Bubblewrap: bubblewrap, Binary: binary}
}

func signedAuditSeat(
	t *testing.T,
	privateKey ed25519.PrivateKey,
	id contracts.SeatID,
	organizationID contracts.OrganizationID,
	departmentID contracts.DepartmentID,
	role contracts.SeatRole,
) contracts.Seat {
	t.Helper()
	seat := contracts.Seat{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            id, Version: 1,
		DID:            contracts.SeatDID("did:matrix:" + string(id)),
		OrganizationID: organizationID, DepartmentID: departmentID, Role: role,
		MandateID: contracts.MandateID("mandate:" + string(id)), MandateVersion: 1,
		BindingID: contracts.SeatBindingID("binding:" + string(id)), BindingVersion: 1,
		EffectiveAt: auditNow().Add(-time.Hour),
	}
	if err := policy.SignSeat(&seat, "owner-key", privateKey); err != nil {
		t.Fatal(err)
	}
	return seat
}

func startAuditPostgres(ctx context.Context) (string, string, error) {
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d",
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		auditPostgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	container := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", container, "5432/tcp").CombinedOutput()
	if err != nil {
		return container, "", err
	}
	address := strings.TrimSpace(string(portOutput))
	index := strings.LastIndex(address, ":")
	if index < 0 {
		return container, "", fmt.Errorf("invalid PostgreSQL port %q", address)
	}
	return container, "postgres://postgres:workforce-test-password@127.0.0.1:" +
		address[index+1:] + "/workforce?sslmode=disable", nil
}

func waitAuditPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(50 * time.Millisecond)
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
