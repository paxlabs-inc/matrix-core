package audit

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/testauthority"
)

func TestSamplingPolicyAndProofHelpersCoverEveryBoundary(t *testing.T) {
	valid := SamplingPolicy{Numerator: 1, Denominator: 2, Minimum: 1, Maximum: 10}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid policy: %v", err)
	}
	for _, value := range []SamplingPolicy{
		{},
		{Numerator: 1, Denominator: 0, Maximum: 1},
		{Numerator: 2, Denominator: 1, Maximum: 1},
		{Numerator: 1, Denominator: 1, Maximum: 0},
		{Numerator: 1, Denominator: 1, Minimum: 2, Maximum: 1},
		{Numerator: 1, Denominator: 1, Maximum: 10001},
	} {
		if err := value.Validate(); err == nil {
			t.Fatalf("accepted invalid sampling policy %#v", value)
		}
	}

	auditors := []Seat{
		{ID: "auditor-b", DepartmentID: "department-b"},
		{ID: "auditor-a2", DepartmentID: "department-a"},
		{ID: "auditor-a1", DepartmentID: "department-a"},
	}
	sorted := sortedAuditors(auditors)
	if sorted[0].ID != "auditor-a1" || sorted[1].ID != "auditor-a2" {
		t.Fatalf("auditors not sorted by department and id: %#v", sorted)
	}
	if !validAuditors(auditors) {
		t.Fatal("valid auditors rejected")
	}
	for _, values := range [][]Seat{
		{{ID: "", DepartmentID: "department-a"}, {ID: "b", DepartmentID: "department-b"}},
		{{ID: "a", DepartmentID: ""}, {ID: "b", DepartmentID: "department-b"}},
		{{ID: "a", DepartmentID: "department-a"}, {ID: "a", DepartmentID: "department-b"}},
		{{ID: "a", DepartmentID: "department-a"}, {ID: "b", DepartmentID: "department-a"}},
	} {
		if validAuditors(values) {
			t.Fatalf("invalid auditors accepted: %#v", values)
		}
	}

	population := []PopulationEntry{
		{VerdictID: "verdict-1", VerdictHash: auditHash("verdict-1"), ExecutingDepartmentID: "department-x"},
		{VerdictID: "verdict-2", VerdictHash: auditHash("verdict-2"), ExecutingDepartmentID: "department-x"},
		{VerdictID: "verdict-3", VerdictHash: auditHash("verdict-3"), ExecutingDepartmentID: "department-x"},
	}
	seed := bytes.Repeat([]byte{7}, 32)
	cutoff := auditNow().Add(-time.Minute)
	proof, err := buildProof(
		"org-proof", "epoch-proof", cutoff, auditNow(),
		SamplingPolicy{Numerator: 1, Denominator: 10, Minimum: 2, Maximum: 10},
		auditors[:2], population, seed,
	)
	if err != nil || len(proof.Selections) != 2 {
		t.Fatalf("minimum selection proof = %#v, %v", proof.Selections, err)
	}
	maximum, err := buildProof(
		"org-proof", "epoch-maximum", cutoff, auditNow(),
		SamplingPolicy{Numerator: 1, Denominator: 1, Minimum: 0, Maximum: 1},
		auditors[:2], population, seed,
	)
	if err != nil || len(maximum.Selections) != 1 {
		t.Fatalf("maximum selection proof = %#v, %v", maximum.Selections, err)
	}
	bounded, err := buildProof(
		"org-proof", "epoch-bounded", cutoff, auditNow(),
		SamplingPolicy{Numerator: 1, Denominator: 10, Minimum: 5, Maximum: 10},
		auditors[:2], population, seed,
	)
	if err != nil || len(bounded.Selections) != len(population) {
		t.Fatalf("population-bounded proof = %#v, %v", bounded.Selections, err)
	}
	if _, err := buildProof(
		"org-proof", "epoch-denied", cutoff, auditNow(), valid,
		[]Seat{{ID: "auditor-a", DepartmentID: "department-x"}}, population, seed,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("same-department proof error = %v", err)
	}
	if packetHash(contracts.VerdictPacket{}) != (contracts.ContentHash{}) {
		t.Fatal("invalid packet produced a content hash")
	}
}

func TestVerifyProofRejectsEveryTamperedComponent(t *testing.T) {
	seed := bytes.Repeat([]byte{3}, 32)
	proof, err := buildProof(
		"org-proof", "epoch-proof", auditNow().Add(-time.Minute), auditNow(),
		SamplingPolicy{Numerator: 1, Denominator: 1, Minimum: 1, Maximum: 10},
		[]Seat{
			{ID: "auditor-a", DepartmentID: "department-a"},
			{ID: "auditor-b", DepartmentID: "department-b"},
		},
		[]PopulationEntry{{
			VerdictID: "verdict-1", VerdictHash: auditHash("verdict-1"),
			ExecutingDepartmentID: "department-c",
		}},
		seed,
	)
	if err != nil || VerifyProof(proof) != nil {
		t.Fatalf("valid proof: %v", err)
	}
	tests := map[string]func(*SampleProof){
		"schema":       func(value *SampleProof) { value.SchemaVersion = "bad" },
		"organization": func(value *SampleProof) { value.OrganizationID = "" },
		"epoch":        func(value *SampleProof) { value.EpochID = "" },
		"cutoff zone": func(value *SampleProof) {
			value.CutoffAt = value.CutoffAt.In(time.FixedZone("non-utc", 3600))
		},
		"created zone": func(value *SampleProof) {
			value.CreatedAt = value.CreatedAt.In(time.FixedZone("non-utc", 3600))
		},
		"time order":      func(value *SampleProof) { value.CutoffAt = value.CreatedAt },
		"policy":          func(value *SampleProof) { value.Policy.Numerator = 0 },
		"auditors":        func(value *SampleProof) { value.Auditors[1].DepartmentID = value.Auditors[0].DepartmentID },
		"seed encoding":   func(value *SampleProof) { value.Seed = "%%%" },
		"seed length":     func(value *SampleProof) { value.Seed = "00" },
		"seed commitment": func(value *SampleProof) { value.SeedCommitment = auditHash("wrong") },
		"population root": func(value *SampleProof) { value.PopulationRoot = auditHash("wrong") },
		"selection":       func(value *SampleProof) { value.Selections[0].Score = strings.Repeat("0", 64) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := proof
			candidate.Auditors = append([]Seat(nil), proof.Auditors...)
			candidate.Selections = append([]Selection(nil), proof.Selections...)
			mutate(&candidate)
			if err := VerifyProof(candidate); err == nil {
				t.Fatal("verified tampered sample proof")
			}
		})
	}
}

func TestRealAuditStoreConstructionSigningAndClockBoundaries(t *testing.T) {
	fixture := newRealAuditFixture(t)
	if _, err := NewStore(
		nil, fixture.store.vault, fixture.store.tenantID, fixture.store.keyID,
		fixture.store.privateKey, fixture.authority, auditNow,
	); err == nil {
		t.Fatal("constructed audit store without database")
	}
	otherSession, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: "other-tenant",
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		auditPool, otherSession.UserVault(), fixture.store.tenantID, fixture.store.keyID,
		fixture.store.privateKey, fixture.authority, auditNow,
	); err == nil {
		t.Fatal("constructed audit store with another tenant's Vault")
	}

	verdict := contracts.Verdict{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "verdict-sign", OrganizationID: fixture.organizationID,
		IntentID: fixture.packet.Intent.ID, AuditorSeatID: fixture.packet.AuditorSeatID,
		Outcome: contracts.VerdictPass, VerifierDigest: fixture.packet.VerifierDigest,
		Evidence: fixture.packet.Observations, ReasonCode: "verified", CreatedAt: auditNow(),
	}
	if err := fixture.store.signVerdict(&verdict); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.verifyVerdict(verdict); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*contracts.Verdict){
		"algorithm": func(value *contracts.Verdict) { value.Signature.Algorithm = "other" },
		"key":       func(value *contracts.Verdict) { value.Signature.KeyID = "other" },
		"encoding":  func(value *contracts.Verdict) { value.Signature.Value = "%%%" },
		"tamper":    func(value *contracts.Verdict) { value.ReasonCode = "tampered" },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := verdict
			mutate(&candidate)
			if err := fixture.store.verifyVerdict(candidate); !errors.Is(err, ErrIntegrity) {
				t.Fatalf("tampered verdict error = %v", err)
			}
		})
	}
	invalid := verdict
	invalid.ID = ""
	if err := fixture.store.signVerdict(&invalid); err == nil {
		t.Fatal("signed invalid verdict")
	}
	fixture.store.now = func() time.Time { return time.Time{} }
	if _, err := fixture.store.currentTime(); err == nil {
		t.Fatal("accepted invalid audit clock")
	}
}

func TestBoundedWriterUsesRealWriterFailures(t *testing.T) {
	writer := &boundedWriter{target: &bytes.Buffer{}, remaining: 2}
	if _, err := writer.Write([]byte("too long")); err == nil {
		t.Fatal("bounded writer accepted oversized output")
	}
	file, err := os.CreateTemp(t.TempDir(), "closed-writer")
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	writer = &boundedWriter{target: file, remaining: 10}
	if _, err := writer.Write([]byte("value")); err == nil {
		t.Fatal("bounded writer hid real filesystem write failure")
	}
}

type realAuditFixture struct {
	store          *Store
	authority      *policy.Store
	organizationID contracts.OrganizationID
	packet         contracts.VerdictPacket
	decision       VerifiedDecision
	lease          contracts.WakeLease
	auditorIDs     []contracts.SeatID
}

func newRealAuditFixture(t *testing.T) realAuditFixture {
	t.Helper()
	ctx := context.Background()
	digest := sha256.Sum256([]byte(t.Name()))
	suffix := hex.EncodeToString(digest[:6])
	tenantID := "tenant-audit-" + suffix
	organizationID := contracts.OrganizationID("org-audit-" + suffix)
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenantID,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := policy.New(
		auditPool, session.UserVault(), policy.OwnerRoot{
			TenantID: tenantID, OrganizationID: organizationID,
			OwnerID: "owner-" + contracts.OwnerID(suffix),
			KeyID:   "owner-key", PublicKey: ownerPublic,
		}, auditNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	auditPublic, auditPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = auditPublic
	store, err := NewStore(
		auditPool, session.UserVault(), tenantID, "audit-key", auditPrivate,
		authority, auditNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	packet := auditPacket(t, contracts.IntentID("intent-"+suffix))
	packet.OrganizationID = organizationID
	packet.Intent.OrganizationID = organizationID
	decision, err := Evaluate(packet)
	if err != nil {
		t.Fatal(err)
	}
	decisionBytes, err := contracts.EncodeCanonical(&decision)
	if err != nil {
		t.Fatal(err)
	}
	verified := VerifiedDecision{
		decision: decision, packetDigest: packetHash(packet),
		decisionDigest: hashBytes(decisionBytes),
	}
	grant := policy.OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      tenantID, OrganizationID: organizationID, OwnerID: "owner-" + contracts.OwnerID(suffix),
		Scope: "authority:write", IssuedAt: auditNow().Add(-time.Minute),
		ExpiresAt: auditNow().Add(time.Hour),
	}
	if err := policy.SignOwnerGrant(&grant, "owner-key", ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := testauthority.PublishRuntimeAuthority(
		ctx, authority, organizationID, "owner-key", ownerPublic, ownerPrivate,
		grant, auditNow().Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	executing := signedAuditSeat(
		t, ownerPrivate, packet.ExecutingSeatID, organizationID,
		"department-developer", contracts.SeatExecutor,
	)
	auditors := []contracts.Seat{
		signedAuditSeat(t, ownerPrivate, packet.AuditorSeatID, organizationID, "department-legal", contracts.SeatAuditor),
		signedAuditSeat(t, ownerPrivate, "seat-auditor-accounting-"+contracts.SeatID(suffix), organizationID, "department-accounting", contracts.SeatAuditor),
		signedAuditSeat(t, ownerPrivate, "seat-auditor-research-"+contracts.SeatID(suffix), organizationID, "department-research", contracts.SeatAuditor),
	}
	if err := authority.PublishSeat(ctx, executing, grant); err != nil {
		t.Fatal(err)
	}
	var lease contracts.WakeLease
	for index, seat := range auditors {
		if err := authority.PublishSeat(ctx, seat, grant); err != nil {
			t.Fatal(err)
		}
		current := publishAuditLease(t, ctx, authority, ownerPrivate, grant, seat, packet)
		if index == 0 {
			lease = current
		}
	}
	return realAuditFixture{
		store: store, authority: authority, organizationID: organizationID,
		packet: packet, decision: verified, lease: lease,
		auditorIDs: []contracts.SeatID{auditors[1].ID, auditors[2].ID},
	}
}

func closedAuditPool(t *testing.T) *pgxpool.Pool {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), auditPool.Config().ConnString())
	if err != nil {
		t.Fatal(err)
	}
	pool.Close()
	return pool
}
