package approval

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
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

const approvalPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var approvalPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startApprovalPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "approval integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	approvalPool, err = waitApprovalPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "approval integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, approvalPool, approvalTime()); err != nil {
		approvalPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "approval migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	approvalPool.Close()
	cleanup()
	os.Exit(code)
}

func TestEvaluate_CompiledPermissionsPrecedenceAndTimeouts(t *testing.T) {
	supervised, err := CompilePreset(PresetSupervised)
	if err != nil || !supervised.DataAccess || !supervised.RequiredReview ||
		supervised.Initiation || supervised.Spending {
		t.Fatalf("supervised = %+v, %v", supervised, err)
	}
	bounded, err := CompilePreset(PresetBounded)
	if err != nil || !bounded.Initiation || !bounded.Scheduling ||
		bounded.Publication || bounded.Spending {
		t.Fatalf("bounded = %+v, %v", bounded, err)
	}
	unattended, err := CompilePreset(PresetUnattended)
	if err != nil || !unattended.Publication || !unattended.Spending ||
		unattended.DestructiveEffects {
		t.Fatalf("unattended = %+v, %v", unattended, err)
	}
	if _, err := CompilePreset("scalar-only"); err == nil {
		t.Fatal("unknown scalar autonomy accepted")
	}
	now := approvalTime()
	request := approvalRequest(now)
	start, end := now.Add(-time.Hour), now.Add(time.Hour)
	rules := []Rule{
		{ClauseID: "allow-general", Outcome: OutcomeAutoApprove, SkillID: "payments"},
		{ClauseID: "human-high-cost", Priority: 10, Outcome: OutcomeHumanRequired,
			SkillID: "payments", MaxCostMicrounits: 1000},
		{ClauseID: "deny-counterparty", Outcome: OutcomeDeny,
			Counterparty: "blocked.example", WindowStartUTC: &start, WindowEndUTC: &end},
	}
	decision, err := Evaluate(rules, request)
	if !errors.Is(err, ErrDenied) || decision.ClauseID != "deny-counterparty" {
		t.Fatalf("deny precedence = %+v, %v", decision, err)
	}
	request.Counterparty = "safe.example"
	decision, err = Evaluate(rules, request)
	if !errors.Is(err, ErrHumanRequired) || decision.ClauseID != "human-high-cost" {
		t.Fatalf("review precedence = %+v, %v", decision, err)
	}
	request.SkillID = "unmatched"
	request.Reversible = false
	if decision, err = Evaluate(rules, request); !errors.Is(err, ErrDenied) ||
		decision.Outcome != OutcomeDeny {
		t.Fatalf("irreversible default = %+v, %v", decision, err)
	}
	request.Reversible = true
	if _, err = Evaluate(rules, request); !errors.Is(err, ErrHumanRequired) {
		t.Fatalf("reversible no-match = %v", err)
	}
	request = approvalRequest(now)
	request.Reversible = false
	if _, err := ResolveTimeout(request, nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("irreversible timeout = %v", err)
	}
	request.Reversible = true
	if _, err := ResolveTimeout(request, nil); !errors.Is(err, ErrHumanRequired) {
		t.Fatalf("missing timeout policy = %v", err)
	}
	escalate := OutcomeEscalate
	if _, err := ResolveTimeout(request, &escalate); !errors.Is(err, ErrEscalate) {
		t.Fatalf("explicit timeout = %v", err)
	}
	unsafe := OutcomeAutoApprove
	if _, err := ResolveTimeout(request, &unsafe); err == nil {
		t.Fatal("unsafe timeout auto-approval accepted")
	}
}

func TestIntegration_BatchExactSetCeilingExpiryRevocationAndExecutiveBoundary(t *testing.T) {
	fixture := newApprovalFixture(t, "batch")
	ctx := context.Background()
	batch := fixture.batch(t, "batch:quarter-close",
		[]contracts.IntentID{"intent:invoice-a", "intent:invoice-b"}, 1000,
		fixture.now.Add(time.Hour))
	if err := fixture.store.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PublishBatch(ctx, batch); err != nil {
		t.Fatalf("idempotent batch = %v", err)
	}
	changed := batch
	changed.AggregateCeilingMicrounits = 2000
	if err := SignBatch(&changed, fixture.keyID, fixture.privateKey); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PublishBatch(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed batch identity = %v", err)
	}
	if err := fixture.store.ConsumeBatch(ctx, batch.BatchID,
		"intent:invoice-a", 400, "consume:a"); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.ConsumeBatch(ctx, batch.BatchID,
		"intent:invoice-a", 400, "consume:a"); err != nil {
		t.Fatalf("idempotent consumption = %v", err)
	}
	if err := fixture.store.ConsumeBatch(ctx, batch.BatchID,
		"intent:invoice-a", 401, "consume:a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed consumption = %v", err)
	}
	if err := fixture.store.ConsumeBatch(ctx, batch.BatchID,
		"intent:future", 1, "consume:future"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unspecified future intent = %v", err)
	}
	if err := fixture.store.ConsumeBatch(ctx, batch.BatchID,
		"intent:invoice-b", 601, "consume:b"); !errors.Is(err, ErrCeiling) {
		t.Fatalf("aggregate ceiling = %v", err)
	}
	if err := fixture.store.RevokeBatch(ctx, batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.ConsumeBatch(ctx, batch.BatchID,
		"intent:invoice-b", 100, "consume:after-revoke"); !errors.Is(err, ErrExpired) {
		t.Fatalf("revoked batch = %v", err)
	}
	if err := fixture.store.RevokeBatch(ctx, batch.BatchID); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("duplicate revocation = %v", err)
	}

	insertApprovalSeat(t, fixture, "seat:executive")
	if err := fixture.store.AnnotateAsExecutive(ctx, "annotation:risk",
		"request:payment", "seat:executive", contracts.DepartmentExecutive,
		"Counterparty ownership requires human review."); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.AnnotateAsExecutive(ctx, "annotation:bad",
		"request:payment", "seat:executive", contracts.DepartmentAccounting,
		"attempted decision"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("non-executive annotation path = %v", err)
	}
	if err := AssertDecisionAuthority(contracts.DepartmentExecutive,
		contracts.SeatLead); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Executive decision authority = %v", err)
	}
	if err := AssertDecisionAuthority(contracts.DepartmentAccounting,
		contracts.SeatAuditor); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("Auditor decision authority = %v", err)
	}
	if err := AssertDecisionAuthority(contracts.DepartmentAccounting,
		contracts.SeatLead); err != nil {
		t.Fatalf("human owner lane = %v", err)
	}
}

func TestIntegration_BatchRejectsMissingInvalidExpiredAndTamperedAuthority(t *testing.T) {
	fixture := newApprovalFixture(t, "reject")
	ctx := context.Background()
	if err := fixture.store.ConsumeBatch(ctx, "batch:missing",
		"intent:x", 1, "consume:x"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("missing batch = %v", err)
	}
	expired := fixture.batch(t, "batch:expired", []contracts.IntentID{"intent:x"},
		10, fixture.now.Add(-time.Minute))
	if err := fixture.store.PublishBatch(ctx, expired); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired publication = %v", err)
	}
	tampered := fixture.batch(t, "batch:tampered", []contracts.IntentID{"intent:x"},
		10, fixture.now.Add(time.Hour))
	tampered.AggregateCeilingMicrounits = 20
	if err := fixture.store.PublishBatch(ctx, tampered); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered signature = %v", err)
	}
	if err := fixture.store.ConsumeBatch(ctx, "", "", 0, ""); err == nil {
		t.Fatal("empty consumption accepted")
	}
	invalidClock := *fixture.store
	invalidClock.now = func() time.Time { return time.Time{} }
	if err := invalidClock.PublishBatch(ctx, tampered); !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalid clock publication = %v", err)
	}
	if err := SignBatch(nil, "", nil); err == nil {
		t.Fatal("empty batch signing accepted")
	}
}

func TestApprovalValidationAndIntentHash(t *testing.T) {
	hashA, err := IntentSetHash([]contracts.IntentID{"intent:b", "intent:a"})
	if err != nil {
		t.Fatal(err)
	}
	hashB, err := IntentSetHash([]contracts.IntentID{"intent:a", "intent:b"})
	if err != nil || hashA != hashB {
		t.Fatalf("order-independent hashes = %q/%q, %v", hashA, hashB, err)
	}
	if _, err := IntentSetHash(nil); err == nil {
		t.Fatal("empty intent set accepted")
	}
	if _, err := IntentSetHash([]contracts.IntentID{"intent:a", "intent:a"}); err == nil {
		t.Fatal("duplicate intent accepted")
	}
	if (Rule{}).Validate() == nil || (Request{}).Validate() == nil ||
		Outcome("unknown").Valid() {
		t.Fatal("invalid approval types accepted")
	}
	now := approvalTime()
	end := now.Add(time.Hour)
	start := now.Add(-time.Hour)
	for name, rule := range map[string]Rule{
		"reversibility":    {ClauseID: "rule:x", Outcome: OutcomeDeny, Reversibility: "sometimes"},
		"one_window_bound": {ClauseID: "rule:x", Outcome: OutcomeDeny, WindowStartUTC: &start},
		"reversed_window":  {ClauseID: "rule:x", Outcome: OutcomeDeny, WindowStartUTC: &end, WindowEndUTC: &start},
	} {
		if err := rule.Validate(); err == nil {
			t.Fatalf("invalid rule %s accepted", name)
		}
	}
	request := approvalRequest(now)
	request.Reversible = true
	for _, test := range []struct {
		rule    Rule
		outcome Outcome
		err     error
	}{
		{Rule{ClauseID: "auto", Outcome: OutcomeAutoApprove,
			SkillID: request.SkillID, Operation: request.Operation,
			EffectClass: request.EffectClass, Reversibility: "reversible",
			MaxCostMicrounits: request.CostMicrounits,
			Counterparty:      request.Counterparty, DataScope: request.DataScope,
			Channel: request.Channel, Jurisdiction: request.Jurisdiction,
			WindowStartUTC: &start, WindowEndUTC: &end,
			SeatID: string(request.SeatID), PriorVerdict: request.PriorVerdict},
			OutcomeAutoApprove, nil},
		{Rule{ClauseID: "batch", Outcome: OutcomeBatchable},
			OutcomeBatchable, nil},
		{Rule{ClauseID: "escalate", Outcome: OutcomeEscalate},
			OutcomeEscalate, ErrEscalate},
	} {
		decision, err := Evaluate([]Rule{test.rule}, request)
		if decision.Outcome != test.outcome || !errors.Is(err, test.err) {
			t.Fatalf("evaluation %s = %+v, %v", test.rule.ClauseID, decision, err)
		}
	}
	mismatches := []Rule{
		{ClauseID: "skill", Outcome: OutcomeAutoApprove, SkillID: "other"},
		{ClauseID: "reversibility", Outcome: OutcomeAutoApprove, Reversibility: "irreversible"},
		{ClauseID: "cost", Outcome: OutcomeAutoApprove, MaxCostMicrounits: 1},
		{ClauseID: "window", Outcome: OutcomeAutoApprove,
			WindowStartUTC: &end, WindowEndUTC: func() *time.Time {
				value := end.Add(time.Hour)
				return &value
			}()},
	}
	for _, rule := range mismatches {
		if _, err := Evaluate([]Rule{rule}, request); !errors.Is(err, ErrHumanRequired) {
			t.Fatalf("mismatch %s = %v", rule.ClauseID, err)
		}
	}
	deny := OutcomeDeny
	if _, err := ResolveTimeout(request, &deny); !errors.Is(err, ErrDenied) {
		t.Fatalf("explicit deny timeout = %v", err)
	}
	human := OutcomeHumanRequired
	if _, err := ResolveTimeout(request, &human); !errors.Is(err, ErrHumanRequired) {
		t.Fatalf("explicit human timeout = %v", err)
	}
	invalid := Outcome("invalid")
	if _, err := ResolveTimeout(request, &invalid); err == nil {
		t.Fatal("invalid timeout outcome accepted")
	}
	if _, err := Evaluate(nil, Request{}); err == nil {
		t.Fatal("invalid request evaluated")
	}
	if _, err := Evaluate([]Rule{{ClauseID: "bad", Outcome: "invalid"}}, request); err == nil {
		t.Fatal("invalid rule evaluated")
	}
}

func TestIntegration_ApprovalStoreConstructionAndAnnotationRejections(t *testing.T) {
	fixture := newApprovalFixture(t, "construction")
	ctx := context.Background()
	if _, err := New(nil, nil, "", "", "", "", nil, nil); err == nil {
		t.Fatal("empty approval store accepted")
	}
	if _, err := New(approvalPool, approvalVault(t, "tenant:other"),
		fixture.tenantID, fixture.organizationID, fixture.ownerID,
		fixture.keyID, fixture.publicKey, func() time.Time { return *fixture.now }); err == nil {
		t.Fatal("mismatched approval Vault accepted")
	}
	if err := fixture.store.AnnotateAsExecutive(ctx, "", "", "", "",
		""); err == nil {
		t.Fatal("empty annotation accepted")
	}
	if err := fixture.store.AnnotateAsExecutive(ctx, "annotation:unknown",
		"request:x", "seat:unknown", contracts.DepartmentExecutive,
		"triage only"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown executive seat = %v", err)
	}
	if err := AssertDecisionAuthority(contracts.DepartmentAccounting,
		contracts.SeatRole("unknown")); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown decision role = %v", err)
	}
	batch := fixture.batch(t, "batch:wrong-tenant",
		[]contracts.IntentID{"intent:x"}, 1, fixture.now.Add(time.Hour))
	batch.TenantID = "tenant:other"
	if err := SignBatch(&batch, fixture.keyID, fixture.privateKey); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.PublishBatch(ctx, batch); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("wrong-tenant batch = %v", err)
	}
	batch = fixture.batch(t, "batch:bad-signature",
		[]contracts.IntentID{"intent:x"}, 1, fixture.now.Add(time.Hour))
	prefix := "A"
	if strings.HasPrefix(batch.Signature.Value, prefix) {
		prefix = "B"
	}
	batch.Signature.Value = prefix + batch.Signature.Value[1:]
	if err := fixture.store.PublishBatch(ctx, batch); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid batch signature = %v", err)
	}
}

type approvalFixture struct {
	store          *Store
	tenantID       string
	organizationID contracts.OrganizationID
	ownerID        contracts.OwnerID
	keyID          string
	publicKey      ed25519.PublicKey
	privateKey     ed25519.PrivateKey
	now            *time.Time
}

func newApprovalFixture(t *testing.T, label string) approvalFixture {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := approvalTime()
	fixture := approvalFixture{
		tenantID:       "tenant:" + label,
		organizationID: contracts.OrganizationID("organization:" + label),
		ownerID:        "owner:" + contracts.OwnerID(label), keyID: "key:owner",
		publicKey: publicKey, privateKey: privateKey, now: &now,
	}
	store, err := New(approvalPool, approvalVault(t, fixture.tenantID),
		fixture.tenantID, fixture.organizationID, fixture.ownerID, fixture.keyID,
		fixture.publicKey, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store
	return fixture
}

func (fixture approvalFixture) batch(
	t *testing.T,
	id string,
	intents []contracts.IntentID,
	ceiling uint64,
	expiresAt time.Time,
) BatchApproval {
	t.Helper()
	batch := BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       contracts.ApprovalID(id), TenantID: fixture.tenantID,
		OrganizationID: fixture.organizationID, IntentIDs: intents,
		AggregateCeilingMicrounits: ceiling, ExpiresAt: expiresAt.UTC(),
		OwnerID: fixture.ownerID,
	}
	if err := SignBatch(&batch, fixture.keyID, fixture.privateKey); err != nil {
		t.Fatal(err)
	}
	return batch
}

func approvalRequest(now time.Time) Request {
	return Request{
		RequestID: "request:payment", IntentID: "intent:payment",
		SkillID: "payments", Operation: "send", EffectClass: "financial",
		Reversible: false, CostMicrounits: 500,
		Counterparty: "blocked.example", DataScope: "accounting",
		Channel: "bank", Jurisdiction: "DE", SeatID: "seat:accounting",
		PriorVerdict: "pass", RequestedAt: now,
	}
}

func insertApprovalSeat(t *testing.T, fixture approvalFixture, seatID string) {
	t.Helper()
	if _, err := approvalPool.Exec(context.Background(), `
		INSERT INTO workforce_authority_records (
			tenant_id,organization_id,authority_kind,authority_id,version,
			owner_id,key_id,effective_at,canonical_hash,sealed_record,
			material_change,created_at
		) VALUES ($1,$2,'seat',$3,1,$4,$5,$6,$7,$8,FALSE,$6)
	`, fixture.tenantID, fixture.organizationID, seatID, fixture.ownerID,
		fixture.keyID, *fixture.now, strings.Repeat("b", 64), []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := approvalPool.Exec(context.Background(), `
		INSERT INTO workforce_authority_heads (
			tenant_id,organization_id,authority_kind,authority_id,
			latest_version,updated_at
		) VALUES ($1,$2,'seat',$3,1,$4)
	`, fixture.tenantID, fixture.organizationID, seatID, *fixture.now); err != nil {
		t.Fatal(err)
	}
}

func approvalVault(t *testing.T, tenant string) *vault.UserVault {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant, KEKHex: kek,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.UserVault()
}

func approvalTime() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startApprovalPostgres(ctx context.Context) (string, string, error) {
	output, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=password", "-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", approvalPostgresImage).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start postgres: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port",
		containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("resolve postgres port: %w: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	separator := strings.LastIndex(address, ":")
	if separator < 0 {
		return containerID, "", fmt.Errorf("invalid postgres port output %q", address)
	}
	return containerID, "postgres://postgres:password@127.0.0.1:" +
		address[separator+1:] + "/workforce?sslmode=disable", nil
}

func waitApprovalPostgres(
	ctx context.Context,
	databaseURL string,
) (*pgxpool.Pool, error) {
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
