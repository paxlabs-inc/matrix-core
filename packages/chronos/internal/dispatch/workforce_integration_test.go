package dispatch

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/chronos/internal/store"
	"github.com/Sidiora-Labs/centra-llm-agents/chronos/internal/wake"
	"github.com/Sidiora-Labs/centra-llm-agents/chronos/pkg/types"
	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"
	"centra/workforce/scheduler"
)

const workforcePostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

func TestIntegration_ChronosWorkforcedColdStartOutageRetryOnceCronAndCoalescing(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL := startWorkforcePostgres(t, ctx)
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	})
	pool := waitWorkforcePostgres(t, ctx, databaseURL)
	defer pool.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := scheduler.ApplyMigrations(ctx, pool, now); err != nil {
		t.Fatal(err)
	}
	chronosStore, err := store.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	migrations, err := filepath.Abs("../../migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := chronosStore.Migrate(ctx, migrations); err != nil {
		t.Fatal(err)
	}
	tenantID := "tenant:chronos-e2e"
	organizationID := "organization:chronos-e2e"
	insertWorkforceAuthority(t, pool, tenantID, organizationID,
		"seat:developer", "mandate:developer", now)
	workforceStore, err := scheduler.New(pool, workforceVault(t, tenantID), tenantID,
		scheduler.Config{
			MaxOrganizationConcurrency: 4, MaxSeatConcurrency: 1,
			DailyTaskLimit: 100, DailySpendMicrounits: 1_000_000,
			ClaimLease: 2 * time.Minute,
		}, func() time.Time { return time.Now().UTC() })
	if err != nil {
		t.Fatal(err)
	}

	reserved, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := reserved.Addr().String()
	if err := reserved.Close(); err != nil {
		t.Fatal(err)
	}
	targetURL := "http://" + address + "/internal/workforce/wake"
	target := &wake.TargetWaker{
		Workforce: wake.NewWorkforce(targetURL, "workforce-token"),
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	worker := New(chronosStore, target, log, Config{
		Tick: time.Millisecond, Lease: time.Second, Batch: 20, MaxFailures: 3,
	})
	onceEnvelope := workforceEnvelope(tenantID, organizationID,
		"once", scheduler.TriggerOnce, now.Add(-time.Minute), "once:key")
	oncePayload, _ := json.Marshal(onceEnvelope)
	onceAlarm, _, err := chronosStore.CreateAlarm(ctx, types.Alarm{
		OwnerDID: "did:matrix:user:key", UserID: "user",
		Label: "workforce once", Kind: types.KindOnce,
		Target: types.TargetWorkforced, FireAt: &onceEnvelope.ScheduledAt,
		NextFireAt: onceEnvelope.ScheduledAt, Payload: oncePayload,
		IdempotencyKey: "alarm:once", MaxFailures: 3,
	})
	if err != nil {
		t.Fatal(err)
	}

	worker.tickOnce(ctx)
	afterFailure, err := chronosStore.GetAlarm(ctx, onceAlarm.ID,
		onceAlarm.OwnerDID)
	if err != nil || afterFailure.FailureCount != 1 ||
		afterFailure.Status != types.StatusActive {
		t.Fatalf("outage retry state = %+v, %v", afterFailure, err)
	}
	if _, err := pool.Exec(ctx, `
		UPDATE alarms SET next_fire_at=now(),claimed_at=NULL WHERE id=$1
	`, onceAlarm.ID); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		t.Fatal(err)
	}
	httpServer := &http.Server{
		Handler: scheduler.Handler(workforceStore, "workforce-token"),
	}
	go func() { _ = httpServer.Serve(listener) }()
	t.Cleanup(func() { _ = httpServer.Close() })

	chronosStore.Close()
	chronosStore, err = store.New(ctx, databaseURL)
	if err != nil {
		t.Fatal(err)
	}
	defer chronosStore.Close()
	worker = New(chronosStore, target, log, Config{
		Tick: time.Millisecond, Lease: time.Second, Batch: 20, MaxFailures: 3,
	})
	worker.tickOnce(ctx)
	afterSuccess, err := chronosStore.GetAlarm(ctx, onceAlarm.ID,
		onceAlarm.OwnerDID)
	if err != nil || afterSuccess.Status != types.StatusFired {
		t.Fatalf("cold-start retry success = %+v, %v", afterSuccess, err)
	}
	claims, err := workforceStore.ClaimDue(ctx, organizationID, 10)
	if err != nil || len(claims) != 1 ||
		claims[0].Envelope.ScheduleID != onceEnvelope.ScheduleID {
		t.Fatalf("once delivery claims = %+v, %v", claims, err)
	}
	if err := workforceStore.Complete(ctx, organizationID,
		claims[0].Envelope.WakeID, 10); err != nil {
		t.Fatal(err)
	}
	request := wake.Request{
		Target: types.TargetWorkforced, Payload: oncePayload,
		ScheduledAt: onceEnvelope.ScheduledAt,
	}
	if err := target.Wake(ctx, request); err != nil {
		t.Fatal(err)
	}
	var onceRows int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND schedule_id=$3
	`, tenantID, organizationID, onceEnvelope.ScheduleID).Scan(&onceRows); err != nil {
		t.Fatal(err)
	}
	if onceRows != 1 {
		t.Fatalf("duplicate once delivery rows = %d", onceRows)
	}

	for index := 0; index < 2; index++ {
		envelope := workforceEnvelope(tenantID, organizationID,
			fmt.Sprintf("cron-%d", index), scheduler.TriggerRecurring,
			now.Add(-5*time.Minute), "cron:coalesce")
		envelope.ScheduleID = "schedule:daily"
		envelope.IdempotencyKey = fmt.Sprintf("cron-base-%d", index)
		payload, _ := json.Marshal(envelope)
		if _, _, err := chronosStore.CreateAlarm(ctx, types.Alarm{
			OwnerDID: "did:matrix:user:key", UserID: "user",
			Label: fmt.Sprintf("cron %d", index), Kind: types.KindCron,
			Target: types.TargetWorkforced, CronExpr: "* * * * *", Timezone: "UTC",
			NextFireAt: now.Add(time.Duration(index-5) * time.Minute),
			Payload:    payload, IdempotencyKey: fmt.Sprintf("alarm:cron:%d", index),
			MaxFailures: 3,
		}); err != nil {
			t.Fatal(err)
		}
	}
	worker.tickOnce(ctx)
	var queued, coalesced int
	if err := pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE state='queued'),
		       COUNT(*) FILTER (WHERE state='coalesced')
		FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND coalesce_key='cron:coalesce'
	`, tenantID, organizationID).Scan(&queued, &coalesced); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || coalesced != 1 {
		t.Fatalf("missed cron coalescing = queued %d coalesced %d", queued, coalesced)
	}
}

func workforceEnvelope(
	tenantID, organizationID, label string,
	trigger scheduler.TriggerKind,
	scheduledAt time.Time,
	coalesceKey string,
) scheduler.WakeEnvelope {
	return scheduler.WakeEnvelope{
		SchemaVersion: "workforce.wake.v1",
		WakeID:        "wake:" + label, ScheduleID: "schedule:" + label,
		TenantID: tenantID, OrganizationID: organizationID,
		SeatID: "seat:developer", MandateID: "mandate:developer", MandateVersion: 1,
		Trigger: trigger, Reason: "chronos integration", ScheduledAt: scheduledAt.UTC(),
		Budget: scheduler.Budget{MaxTasks: 1, MaxSpendMicrounits: 100},
		Model:  scheduler.ModelBinding{Provider: "openai", ModelID: "gpt-5"},
		MGS: scheduler.MGSBinding{
			Reference: "mgs:developer:v1", Digest: strings.Repeat("a", 64),
		},
		IdempotencyKey: "wake:" + label, CoalesceKey: coalesceKey,
		GraphScope: "graph:eligible",
	}
}

func insertWorkforceAuthority(
	t *testing.T,
	pool *pgxpool.Pool,
	tenantID, organizationID, seatID, mandateID string,
	now time.Time,
) {
	t.Helper()
	for _, authority := range []struct{ kind, id string }{
		{"seat", seatID}, {"mandate", mandateID},
	} {
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO workforce_authority_records (
				tenant_id,organization_id,authority_kind,authority_id,version,
				owner_id,key_id,effective_at,canonical_hash,sealed_record,
				material_change,created_at
			) VALUES ($1,$2,$3,$4,1,'owner','owner-key',$5,$6,$7,FALSE,$5)
		`, tenantID, organizationID, authority.kind, authority.id,
			now, strings.Repeat("b", 64), []byte{1}); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(context.Background(), `
			INSERT INTO workforce_authority_heads (
				tenant_id,organization_id,authority_kind,authority_id,
				latest_version,updated_at
			) VALUES ($1,$2,$3,$4,1,$5)
		`, tenantID, organizationID, authority.kind, authority.id, now); err != nil {
			t.Fatal(err)
		}
	}
}

func workforceVault(t *testing.T, tenantID string) *vault.UserVault {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenantID, KEKHex: kek,
	})
	if err != nil {
		t.Fatal(err)
	}
	return session.UserVault()
}

func startWorkforcePostgres(
	t *testing.T,
	ctx context.Context,
) (string, string) {
	t.Helper()
	output, err := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=password", "-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", workforcePostgresImage).CombinedOutput()
	if err != nil {
		t.Fatalf("start postgres: %v: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port",
		containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		t.Fatalf("resolve postgres port: %v: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	separator := strings.LastIndex(address, ":")
	if separator < 0 {
		t.Fatalf("invalid postgres port output %q", address)
	}
	return containerID, "postgres://postgres:password@127.0.0.1:" +
		address[separator+1:] + "/workforce?sslmode=disable"
}

func waitWorkforcePostgres(
	t *testing.T,
	ctx context.Context,
	databaseURL string,
) *pgxpool.Pool {
	t.Helper()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}
