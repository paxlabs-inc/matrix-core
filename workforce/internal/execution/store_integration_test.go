package execution

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/ledger"
)

const executionPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var executionPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	container, databaseURL, err := startExecutionPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "execution integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", container).Run()
	}
	executionPool, err = waitExecutionPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "execution integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, executionPool, executionTime()); err != nil {
		executionPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "execution migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	executionPool.Close()
	cleanup()
	os.Exit(code)
}

func TestExecutionLoopPersistsEveryCrashBoundaryAndNeverReplaysCommit(t *testing.T) {
	store, newStore, packet, now := executionFixture(t, "all-stages")
	ctx := context.Background()
	state, err := store.Start(ctx, packet)
	if err != nil {
		t.Fatal(err)
	}
	visited := []Stage{state.Stage}
	for state.Stage != StageSleep {
		restarted := newStore()
		loaded, err := restarted.Load(ctx, state.OrganizationID, state.WakeID)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.Stage != state.Stage || loaded.Version != state.Version {
			t.Fatalf("crash resume changed boundary: got %#v want %#v", loaded, state)
		}
		request := AdvanceRequest{
			OrganizationID: state.OrganizationID, WakeID: state.WakeID,
			ExpectedVersion: state.Version, Decision: DecisionAdvance,
			IdempotencyKey: "advance-" + string(state.Stage),
		}
		if state.Stage == StageCommit {
			request.ReceiptID = "receipt-all-stages"
		}
		if state.Stage == StageYield {
			request.FinalDisposition = contracts.DispositionProgressed
			request.ReasonCode = "bounded_progress"
		}
		state, err = restarted.Advance(ctx, request)
		if err != nil {
			t.Fatalf("advance %s: %v", loaded.Stage, err)
		}
		replayed, err := restarted.Advance(ctx, request)
		if err != nil {
			t.Fatalf("idempotent replay %s: %v", loaded.Stage, err)
		}
		if replayed.Version != state.Version || replayed.Stage != state.Stage {
			t.Fatalf("idempotent replay changed checkpoint: %#v", replayed)
		}
		visited = append(visited, state.Stage)
	}
	if len(visited) != len(orderedStages) {
		t.Fatalf("visited %v, want every ordered stage", visited)
	}
	if state.Disposition != contracts.DispositionProgressed {
		t.Fatalf("terminal disposition = %q", state.Disposition)
	}
	var commits int
	if err := executionPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_wake_commits
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, "tenant-"+shortExecutionScope("all-stages"), state.OrganizationID, state.WakeID).Scan(&commits); err != nil {
		t.Fatal(err)
	}
	if commits != 1 {
		t.Fatalf("commit markers = %d, want 1", commits)
	}
	resumed, err := newStore().Resume(ctx, state.OrganizationID, state.WakeID, "resume-after-commit")
	if err != nil {
		t.Fatal(err)
	}
	if resumed.Stage != StageSleep || resumed.Version != state.Version {
		t.Fatal("terminal resume attempted to replay committed work")
	}
	if _, err := store.Advance(ctx, AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: state.Version, Decision: DecisionAdvance,
		IdempotencyKey: "after-sleep",
	}); !errorsIs(err, ErrTerminal) {
		t.Fatalf("sleep transition error = %v", err)
	}
	*now = now.Add(time.Second)
}

func TestExecutionLoopCrashAfterDispatchReconcilesBeforeRetry(t *testing.T) {
	store, newStore, packet, _ := executionFixture(t, "ambiguous")
	ctx := context.Background()
	state, err := store.Start(ctx, packet)
	if err != nil {
		t.Fatal(err)
	}
	for state.Stage != StageExecute {
		state, err = store.Advance(ctx, AdvanceRequest{
			OrganizationID: state.OrganizationID, WakeID: state.WakeID,
			ExpectedVersion: state.Version, Decision: DecisionAdvance,
			IdempotencyKey: "to-" + string(nextStageValue(state.Stage)),
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	state, err = store.Advance(ctx, AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: state.Version, Decision: DecisionDispatch,
		IdempotencyKey: "dispatch-effect", EffectID: "effect-ambiguous",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Advance(ctx, AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: state.Version, Decision: DecisionDispatch,
		IdempotencyKey: "duplicate-dispatch", EffectID: "effect-ambiguous",
	}); !errorsIs(err, ErrInvalidTransition) {
		t.Fatalf("duplicate dispatch error = %v", err)
	}
	state, err = newStore().Resume(
		ctx, state.OrganizationID, state.WakeID, "resume-ambiguous",
	)
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != StageReconcile || state.ResumeStage != StageExecute {
		t.Fatalf("crash resumed at %#v, want effect reconciliation", state)
	}
	state, err = newStore().Advance(ctx, AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: state.Version, Decision: DecisionReconcileCompleted,
		IdempotencyKey: "reconcile-completed", EffectID: "effect-ambiguous",
	})
	if err != nil {
		t.Fatal(err)
	}
	if state.Stage != StageObserve {
		t.Fatalf("reconciled effect stage = %s", state.Stage)
	}
}

func TestExecutionLoopRejectsStaleConflictingAndInvalidTransitions(t *testing.T) {
	store, _, packet, _ := executionFixture(t, "conflicts")
	ctx := context.Background()
	state, err := store.Start(ctx, packet)
	if err != nil {
		t.Fatal(err)
	}
	request := AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: state.Version, Decision: DecisionAdvance,
		IdempotencyKey: "lease-complete",
	}
	state, err = store.Advance(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	conflicting := request
	conflicting.ReasonCode = "different"
	if _, err := store.Advance(ctx, conflicting); !errorsIs(err, ErrConflict) {
		t.Fatalf("conflicting idempotency error = %v", err)
	}
	if _, err := store.Advance(ctx, AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: request.ExpectedVersion, Decision: DecisionAdvance,
		IdempotencyKey: "stale-version",
	}); !errorsIs(err, ErrConflict) {
		t.Fatalf("stale version error = %v", err)
	}
	if _, err := store.Advance(ctx, AdvanceRequest{
		OrganizationID: state.OrganizationID, WakeID: state.WakeID,
		ExpectedVersion: state.Version, Decision: DecisionObserved,
		IdempotencyKey: "invalid-reconcile-observation",
	}); !errorsIs(err, ErrInvalidTransition) {
		t.Fatalf("invalid transition error = %v", err)
	}
	var sealed []byte
	if err := executionPool.QueryRow(ctx, `
		SELECT sealed_state FROM workforce_wake_checkpoints
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, "tenant-"+shortExecutionScope("conflicts"), state.OrganizationID, state.WakeID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(sealed) || bytes.Contains(sealed, []byte(state.IntentID)) {
		t.Fatal("checkpoint is not Vault-sealed")
	}
}

func TestExecutionRuntimeArtifactsAreSealedImmutableAndRestartSafe(t *testing.T) {
	store, newStore, packet, _ := executionFixture(t, "runtime-artifacts")
	ctx := context.Background()
	content := []byte(`{"schema_version":"workforce.v1","result":"bounded"}`)
	hash, err := store.PutArtifact(
		ctx, packet.Lease.OrganizationID, packet.Lease.WakeID,
		"model_output", content,
	)
	if err != nil {
		t.Fatal(err)
	}
	repeated, err := store.PutArtifact(
		ctx, packet.Lease.OrganizationID, packet.Lease.WakeID,
		"model_output", append([]byte(nil), content...),
	)
	if err != nil || repeated != hash {
		t.Fatalf("exact artifact replay = %#v, %v", repeated, err)
	}
	if _, err := store.PutArtifact(
		ctx, packet.Lease.OrganizationID, packet.Lease.WakeID,
		"model_output", []byte(`{"result":"different"}`),
	); !errorsIs(err, ErrConflict) {
		t.Fatalf("conflicting artifact replay = %v", err)
	}
	opened, openedHash, err := newStore().OpenArtifact(
		ctx, packet.Lease.OrganizationID, packet.Lease.WakeID,
		"model_output",
	)
	if err != nil || !bytes.Equal(opened, content) || openedHash != hash {
		t.Fatalf("restart artifact = %q, %#v, %v", opened, openedHash, err)
	}
	var sealed []byte
	if err := executionPool.QueryRow(ctx, `
		SELECT sealed_content
		FROM workforce_wake_runtime_artifacts
		WHERE tenant_id=$1 AND organization_id=$2
		  AND wake_id=$3 AND artifact_kind='model_output'
	`, "tenant-"+shortExecutionScope("runtime-artifacts"),
		packet.Lease.OrganizationID, packet.Lease.WakeID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(sealed) || bytes.Contains(sealed, content) {
		t.Fatal("runtime artifact is not Vault-sealed")
	}
	if _, err := executionPool.Exec(ctx, `
		UPDATE workforce_wake_runtime_artifacts
		SET content_hash=$1
		WHERE tenant_id=$2 AND organization_id=$3
		  AND wake_id=$4 AND artifact_kind='model_output'
	`, strings.Repeat("f", 64),
		"tenant-"+shortExecutionScope("runtime-artifacts"),
		packet.Lease.OrganizationID, packet.Lease.WakeID); err == nil {
		t.Fatal("immutable runtime artifact accepted an update")
	}
	if _, _, err := store.OpenArtifact(
		ctx, packet.Lease.OrganizationID, packet.Lease.WakeID,
		"missing",
	); !errorsIs(err, ErrConflict) {
		t.Fatalf("missing runtime artifact = %v", err)
	}
}

func TestExecutionLoopClosedDispositionsBudgetsAndLeaseExpiry(t *testing.T) {
	decisions := []struct {
		decision    Decision
		disposition contracts.WakeDisposition
	}{
		{DecisionWaitDependency, contracts.DispositionWaitingDependency},
		{DecisionWaitApproval, contracts.DispositionWaitingApproval},
		{DecisionBlock, contracts.DispositionBlocked},
		{DecisionExhaustBudget, contracts.DispositionBudgetExhausted},
		{DecisionExpireLease, contracts.DispositionLeaseExpired},
		{DecisionCancel, contracts.DispositionCancelled},
		{DecisionFail, contracts.DispositionFailed},
	}
	for _, item := range decisions {
		t.Run(string(item.decision), func(t *testing.T) {
			store, _, packet, _ := executionFixture(t, string(item.decision))
			state, err := store.Start(context.Background(), packet)
			if err != nil {
				t.Fatal(err)
			}
			state, err = store.Advance(context.Background(), AdvanceRequest{
				OrganizationID: state.OrganizationID, WakeID: state.WakeID,
				ExpectedVersion: state.Version, Decision: item.decision,
				IdempotencyKey: "terminal-" + string(item.decision),
				ReasonCode:     string(item.decision),
			})
			if err != nil {
				t.Fatal(err)
			}
			if state.Stage != StageSleep || state.Disposition != item.disposition {
				t.Fatalf("terminal state = %#v", state)
			}
		})
	}
	t.Run("budget", func(t *testing.T) {
		store, _, packet, _ := executionFixture(t, "budget")
		packet.Lease.Budget.MaxSteps = 1
		state, err := store.Start(context.Background(), packet)
		if err != nil {
			t.Fatal(err)
		}
		state, err = store.Advance(context.Background(), AdvanceRequest{
			OrganizationID: state.OrganizationID, WakeID: state.WakeID,
			ExpectedVersion: state.Version, Decision: DecisionAdvance,
			IdempotencyKey: "budget-one",
		})
		if err != nil {
			t.Fatal(err)
		}
		state, err = store.Advance(context.Background(), AdvanceRequest{
			OrganizationID: state.OrganizationID, WakeID: state.WakeID,
			ExpectedVersion: state.Version, Decision: DecisionAdvance,
			IdempotencyKey: "budget-two",
		})
		if err != nil {
			t.Fatal(err)
		}
		if state.Disposition != contracts.DispositionBudgetExhausted {
			t.Fatalf("budget disposition = %q", state.Disposition)
		}
	})
	t.Run("lease-expiry", func(t *testing.T) {
		store, _, packet, now := executionFixture(t, "lease-expiry")
		state, err := store.Start(context.Background(), packet)
		if err != nil {
			t.Fatal(err)
		}
		*now = packet.Lease.ExpiresAt
		state, err = store.Advance(context.Background(), AdvanceRequest{
			OrganizationID: state.OrganizationID, WakeID: state.WakeID,
			ExpectedVersion: state.Version, Decision: DecisionAdvance,
			IdempotencyKey: "expired",
		})
		if err != nil {
			t.Fatal(err)
		}
		if state.Disposition != contracts.DispositionLeaseExpired {
			t.Fatalf("expiry disposition = %q", state.Disposition)
		}
	})
}

func executionFixture(
	t *testing.T,
	label string,
) (*Store, func() *Store, contracts.WorkPacket, *time.Time) {
	t.Helper()
	scope := shortExecutionScope(label)
	tenant := "tenant-" + scope
	now := executionTime()
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	newStore := func() *Store {
		value, err := New(executionPool, session.UserVault(), tenant, func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	packet := executionPacket(t, scope, now)
	return newStore(), newStore, packet, &now
}

func executionPacket(t *testing.T, scope string, now time.Time) contracts.WorkPacket {
	t.Helper()
	signature := contracts.Signature{
		Algorithm: "ed25519", KeyID: "owner-key",
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	organizationID := contracts.OrganizationID("org-" + scope)
	seat := contracts.Seat{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "seat-" + contracts.SeatID(scope), Version: 1,
		DID:            "did:matrix:" + contracts.SeatDID(scope),
		OrganizationID: organizationID, DepartmentID: "department-developer",
		Role: contracts.SeatLead, MandateID: "mandate-" + contracts.MandateID(scope),
		MandateVersion: 1, BindingID: "binding-" + contracts.SeatBindingID(scope),
		BindingVersion: 1, EffectiveAt: now.Add(-time.Hour), Signature: signature,
	}
	mandate := contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            seat.MandateID, Version: 1, OrganizationID: organizationID,
		DepartmentKind: contracts.DepartmentDeveloper, SeatRole: contracts.SeatLead,
		AllowedSkills: []contracts.SkillID{"inspect"},
		DataScopes: []contracts.DataScope{{
			Name: "source", Classification: contracts.ClassificationProject,
			Purpose: "Current wake",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "authority missing", Action: "escalate",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID: "no-prod", Description: "No production",
		}},
		EffectiveAt: now.Add(-time.Hour), Signature: signature,
	}
	policyRef := contracts.PolicyRef{ID: "policy-" + contracts.PolicyID(scope), Version: 1, Hash: executionHash("policy")}
	intentID := contracts.IntentID("intent-" + scope)
	lease := contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "lease-" + contracts.LeaseID(scope), WakeID: "wake-" + contracts.WakeID(scope),
		OrganizationID: organizationID, SeatID: seat.ID, SeatDID: seat.DID,
		Reason: "eligible_work", MandateID: mandate.ID, MandateVersion: 1,
		Policies:   []contracts.PolicyRef{policyRef},
		GraphScope: []contracts.IntentID{intentID},
		Model: contracts.ModelBinding{
			SchemaVersion: contracts.SchemaVersionV1, ID: "model-" + contracts.ModelBindingID(scope),
			Provider: "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
			SamplingDigest: executionHash("sampling"),
		},
		MGS: contracts.MGSGenomeRef{Reference: "mgs-" + scope, Digest: executionHash("mgs")},
		Runtime: contracts.RuntimeBinding{
			BuildDigest:             executionHash("build"),
			AuditorBuildDigest:      executionHash("auditor-build"),
			OperationRegistryDigest: executionHash("operations"),
		},
		SkillCatalogDigest: executionHash("skills"),
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64((30 * time.Minute) / time.Millisecond),
			MaxSteps:          40, MaxModelCalls: 10, MaxToolCalls: 40,
			MaxCostMinor: 1000, Currency: "USD", MaxOutputBytes: 1 << 20,
		},
		IssuedAt: now, ExpiresAt: now.Add(time.Hour), Fence: 1, Signature: signature,
	}
	packet := contracts.WorkPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		Lease:         lease, Seat: seat, Mandate: mandate,
		Goal: contracts.Goal{
			SchemaVersion: contracts.SchemaVersionV1, ID: "goal-" + contracts.GoalID(scope),
			OrganizationID: organizationID, WorkOrderID: "order-" + contracts.WorkOrderID(scope),
			Title: "Execute bounded wake", SuccessCriteria: []string{"Receipt committed"},
			CreatedAt: now.Add(-time.Hour),
		},
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1, ID: intentID,
			OrganizationID: organizationID, GoalID: "goal-" + contracts.GoalID(scope),
			OwnerSeatID: seat.ID, Summary: "Execute bounded wake",
			Priority: 1, CreatedAt: now.Add(-time.Hour),
		},
		Skills: []contracts.SkillRef{{
			ID: "inspect", Version: 1, Digest: executionHash("skill"),
		}},
		Policies: []contracts.PolicyRef{policyRef},
		RequiredOutputs: []contracts.RequiredOutput{{
			Kind: "receipt", SuccessPredicate: "receipt is committed",
		}},
		AssembledAt: now,
	}
	if err := packet.Validate(); err != nil {
		t.Fatal(err)
	}
	return packet
}

func executionHash(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func nextStageValue(stage Stage) Stage {
	next, _ := nextStage(stage)
	return next
}

func shortExecutionScope(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:6])
}

func executionTime() time.Time {
	return time.Date(2026, time.July, 30, 20, 0, 0, 0, time.UTC)
}

func errorsIs(err, target error) bool {
	for err != nil {
		if err == target {
			return true
		}
		type unwrapper interface{ Unwrap() error }
		value, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = value.Unwrap()
	}
	return false
}

func startExecutionPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	output, err := exec.CommandContext(
		ctx, "docker", "run", "--rm", "-d",
		"--name", "workforce-execution-"+hex.EncodeToString(suffix[:]),
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		executionPostgresImage,
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

func waitExecutionPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
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
