package circuit

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/ledger"
)

const circuitPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var circuitPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startCircuitPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "circuit integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	circuitPool, err = waitCircuitPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "circuit integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, circuitPool, circuitBaseTime()); err != nil {
		circuitPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "circuit migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	circuitPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_CircuitStore_StormOpensPersistsAndRecovers(t *testing.T) {
	now := circuitBaseTime()
	clock := func() time.Time { return now }
	config := Config{
		FailureThreshold: 3, SuccessThreshold: 2, Window: time.Minute,
		OpenDuration: 5 * time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}
	store, err := New(circuitPool, "tenant:storm", config, clock)
	if err != nil {
		t.Fatal(err)
	}
	keys := circuitKeys(t, "organization:storm")
	permits := make([]Permit, 12)
	for index := range permits {
		permits[index], err = store.Admit(context.Background(), keys, false)
		if err != nil {
			t.Fatalf("admit storm permit %d: %v", index, err)
		}
	}
	var wait sync.WaitGroup
	failures := make(chan error, len(permits))
	for index := range permits {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			failures <- store.Fail(context.Background(), permits[index], "provider_unavailable")
		}(index)
	}
	wait.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.Admit(context.Background(), keys, false); !errors.Is(err, ErrOpen) {
		t.Fatalf("storm did not open breaker: %v", err)
	}
	snapshots, err := store.Inspect(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range snapshots {
		if snapshot.State != StateOpen || snapshot.Reason != "provider_unavailable" ||
			snapshot.RetryAt == nil || snapshot.FailureCount < config.FailureThreshold {
			t.Fatalf("open snapshot = %+v", snapshot)
		}
	}
	restarted, err := New(circuitPool, "tenant:storm", config, clock)
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := restarted.Inspect(context.Background(), keys)
	if err != nil || persisted[0].State != StateOpen {
		t.Fatalf("restart state = %+v, %v", persisted, err)
	}

	now = now.Add(6 * time.Minute)
	trial, err := restarted.Admit(context.Background(), keys, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := restarted.Admit(context.Background(), keys, false); !errors.Is(err, ErrOpen) {
		t.Fatalf("half-open limit bypassed: %v", err)
	}
	if _, err := restarted.Admit(context.Background(), keys, true); !errors.Is(err, ErrOpen) {
		t.Fatalf("irreversible half-open trial admitted: %v", err)
	}
	if err := restarted.Release(context.Background(), trial); err != nil {
		t.Fatal(err)
	}
	firstSuccess, err := restarted.Admit(context.Background(), keys, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Succeed(context.Background(), firstSuccess); err != nil {
		t.Fatal(err)
	}
	halfOpen, err := restarted.Inspect(context.Background(), keys)
	if err != nil || halfOpen[0].State != StateHalfOpen {
		t.Fatalf("first recovery success = %+v, %v", halfOpen, err)
	}
	secondSuccess, err := restarted.Admit(context.Background(), keys, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := restarted.Succeed(context.Background(), secondSuccess); err != nil {
		t.Fatal(err)
	}
	closed, err := restarted.Inspect(context.Background(), keys)
	if err != nil {
		t.Fatal(err)
	}
	for _, snapshot := range closed {
		if snapshot.State != StateClosed || snapshot.FailureCount != 0 ||
			snapshot.SuccessCount != 0 || snapshot.Reason != "" ||
			snapshot.RetryAt != nil {
			t.Fatalf("recovered snapshot = %+v", snapshot)
		}
	}
}

func TestIntegration_CircuitStore_HalfOpenFailureAndExpiredTrialRecover(t *testing.T) {
	now := circuitBaseTime()
	clock := func() time.Time { return now }
	config := Config{
		FailureThreshold: 1, SuccessThreshold: 1, Window: time.Minute,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: 30 * time.Second,
	}
	store, err := New(circuitPool, "tenant:half-fail", config, clock)
	if err != nil {
		t.Fatal(err)
	}
	keys := circuitKeys(t, "organization:half-fail")
	permit, err := store.Admit(context.Background(), keys, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Fail(context.Background(), permit, "timeout"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Minute)
	expiring, err := store.Admit(context.Background(), keys, false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	replacement, err := store.Admit(context.Background(), keys, false)
	if err != nil {
		t.Fatalf("expired half-open trial retained capacity: %v", err)
	}
	if err := store.Fail(context.Background(), replacement, "still_unavailable"); err != nil {
		t.Fatal(err)
	}
	snapshots, err := store.Inspect(context.Background(), keys)
	if err != nil || snapshots[0].State != StateOpen ||
		snapshots[0].Reason != "still_unavailable" {
		t.Fatalf("half-open failure = %+v, %v", snapshots, err)
	}
	if err := store.Release(context.Background(), expiring); err != nil {
		t.Fatalf("expired permit release was not idempotent: %v", err)
	}
}

func TestCircuitStore_RejectsInvalidInputsAndDatabaseUncertainty(t *testing.T) {
	validConfig := Config{
		FailureThreshold: 1, SuccessThreshold: 1, Window: time.Minute,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}
	invalidConfigs := []Config{
		{}, {FailureThreshold: 1001, SuccessThreshold: 1, Window: time.Minute, OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute},
		{FailureThreshold: 1, SuccessThreshold: 1001, Window: time.Minute, OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute},
		{FailureThreshold: 1, SuccessThreshold: 1, Window: 0, OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute},
		{FailureThreshold: 1, SuccessThreshold: 1, Window: time.Minute, OpenDuration: 25 * time.Hour, HalfOpenLimit: 1, TrialDuration: time.Minute},
		{FailureThreshold: 1, SuccessThreshold: 1, Window: time.Minute, OpenDuration: time.Minute, HalfOpenLimit: 101, TrialDuration: time.Minute},
		{FailureThreshold: 1, SuccessThreshold: 1, Window: time.Minute, OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: 31 * time.Minute},
	}
	for index, config := range invalidConfigs {
		if err := config.Validate(); err == nil {
			t.Fatalf("invalid config %d accepted", index)
		}
	}
	if _, err := New(nil, "tenant", validConfig, time.Now); err == nil {
		t.Fatal("nil pool accepted")
	}
	if _, err := New(circuitPool, "", validConfig, time.Now); err == nil {
		t.Fatal("empty tenant accepted")
	}
	if _, err := New(circuitPool, "tenant", validConfig, nil); err == nil {
		t.Fatal("nil clock accepted")
	}
	store, err := New(circuitPool, "tenant:invalid", validConfig, circuitBaseTime)
	if err != nil {
		t.Fatal(err)
	}
	badKeys := [][]Key{
		nil,
		{{OrganizationID: "", Kind: KindProvider, Name: "provider"}},
		{{OrganizationID: "organization", Kind: "other", Name: "provider"}},
		{{OrganizationID: "organization", Kind: KindProvider, Name: "bad name"}},
		{{OrganizationID: "organization", Kind: KindProvider, Name: "same"}, {OrganizationID: "organization", Kind: KindProvider, Name: "same"}},
	}
	for index, keys := range badKeys {
		if _, err := store.Admit(context.Background(), keys, false); err == nil {
			t.Fatalf("invalid keys %d accepted", index)
		}
	}
	if _, err := Keys("", "provider", "skill", "read"); err == nil {
		t.Fatal("invalid composite key accepted")
	}
	if err := store.Fail(context.Background(), Permit{}, "failure"); err == nil {
		t.Fatal("invalid permit accepted")
	}
	if err := store.Fail(context.Background(), Permit{
		ID: "permit", Keys: circuitKeys(t, "organization:invalid"),
		ExpiresAt: circuitBaseTime(),
	}, ""); err == nil {
		t.Fatal("empty failure reason accepted")
	}
	if err := store.Release(context.Background(), Permit{}); err == nil {
		t.Fatal("invalid release permit accepted")
	}
	absent, err := store.Inspect(context.Background(), circuitKeys(t, "organization:absent"))
	if err != nil || absent[0].State != StateClosed {
		t.Fatalf("absent breaker projection = %+v, %v", absent, err)
	}
	badClock, err := New(circuitPool, "tenant:bad-clock", validConfig, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badClock.Admit(context.Background(),
		circuitKeys(t, "organization:bad-clock"), false); !errors.Is(err, ErrUncertain) {
		t.Fatalf("non-UTC clock error = %v", err)
	}
	closed, err := pgxpool.New(context.Background(),
		"postgres://postgres:password@127.0.0.1:1/database?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	uncertain, err := New(closed, "tenant:closed", validConfig, circuitBaseTime)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := uncertain.Admit(context.Background(),
		circuitKeys(t, "organization:closed"), false); !errors.Is(err, ErrUncertain) {
		t.Fatalf("closed database admission error = %v", err)
	}
	if _, err := uncertain.Inspect(context.Background(),
		circuitKeys(t, "organization:closed")); !errors.Is(err, ErrUncertain) {
		t.Fatalf("closed database inspect error = %v", err)
	}
}

func TestCircuitTypes_RecognizeClosedSets(t *testing.T) {
	for _, kind := range []Kind{KindProvider, KindSkill, KindEffectClass} {
		if !kind.Valid() {
			t.Fatalf("kind %q rejected", kind)
		}
	}
	if Kind("other").Valid() {
		t.Fatal("unknown kind accepted")
	}
	for _, state := range []State{StateClosed, StateOpen, StateHalfOpen} {
		if !state.Valid() {
			t.Fatalf("state %q rejected", state)
		}
	}
	if State("other").Valid() {
		t.Fatal("unknown state accepted")
	}
}

func circuitKeys(t *testing.T, organization string) []Key {
	t.Helper()
	keys, err := Keys(contracts.OrganizationID(organization),
		"provider", "skill", "reversible")
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func circuitBaseTime() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startCircuitPostgres(ctx context.Context) (string, string, error) {
	command := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=password", "-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", circuitPostgresImage)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start postgres: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "5432/tcp").CombinedOutput()
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

func waitCircuitPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
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
