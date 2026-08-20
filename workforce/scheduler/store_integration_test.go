package scheduler

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"
)

const schedulerPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var schedulerPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startSchedulerPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "scheduler integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	schedulerPool, err = waitSchedulerPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "scheduler integration setup:", err)
		os.Exit(1)
	}
	if err := ApplyMigrations(ctx, schedulerPool, schedulerBaseTime()); err != nil {
		schedulerPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "scheduler migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	schedulerPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_EnqueueHandlerDeduplicatesCoalescesAndAuthorizes(t *testing.T) {
	fixture := newSchedulerFixture(t, "enqueue", defaultSchedulerConfig(), 18)
	ctx := context.Background()
	first := fixture.wake("first", TriggerEvent, fixture.now.Add(-time.Minute), "event:customer")
	result, err := fixture.store.Enqueue(ctx, first)
	if err != nil || result.Deduplicated || result.Coalesced {
		t.Fatalf("first enqueue = %+v, %v", result, err)
	}
	result, err = fixture.store.Enqueue(ctx, first)
	if err != nil || !result.Deduplicated {
		t.Fatalf("duplicate enqueue = %+v, %v", result, err)
	}
	changed := first
	changed.Reason = "changed"
	if _, err := fixture.store.Enqueue(ctx, changed); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency conflict = %v", err)
	}
	second := fixture.wake("second", TriggerEvent, fixture.now.Add(-30*time.Second), "event:customer")
	result, err = fixture.store.Enqueue(ctx, second)
	if err != nil || !result.Coalesced {
		t.Fatalf("coalesced enqueue = %+v, %v", result, err)
	}
	var queued, coalesced int
	if err := schedulerPool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE state='queued'),
		       COUNT(*) FILTER (WHERE state='coalesced')
		FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.tenantID, fixture.organizationID).Scan(&queued, &coalesced); err != nil {
		t.Fatal(err)
	}
	if queued != 1 || coalesced != 1 {
		t.Fatalf("queue/coalesced = %d/%d", queued, coalesced)
	}

	server := httptest.NewServer(Handler(fixture.store, "wake-token"))
	defer server.Close()
	body, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodPost,
		server.URL+"/internal/workforce/wake", bytes.NewReader(body))
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthorized handler status = %d", response.StatusCode)
	}
	request, _ = http.NewRequest(http.MethodPost,
		server.URL+"/internal/workforce/wake", bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer wake-token")
	response, err = http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		t.Fatalf("authorized handler status = %d", response.StatusCode)
	}
}

func TestIntegration_ClaimDueOrderingConcurrencyRecoveryAndLimits(t *testing.T) {
	config := defaultSchedulerConfig()
	config.MaxOrganizationConcurrency = 2
	config.MaxSeatConcurrency = 1
	config.DailyTaskLimit = 4
	config.DailySpendMicrounits = 400
	config.ClaimLease = time.Minute
	fixture := newSchedulerFixture(t, "claim", config, 18)
	ctx := context.Background()
	early := fixture.wake("early", TriggerDeadline,
		fixture.now.Add(-2*time.Minute), "deadline:early")
	lateSameSeat := fixture.wake("late-same-seat", TriggerRetry,
		fixture.now.Add(-time.Minute), "retry:late")
	other := fixture.wake("other", TriggerApproval,
		fixture.now.Add(-30*time.Second), "approval:other")
	other.SeatID = "seat:other"
	insertSchedulerAuthority(t, fixture.tenantID, fixture.organizationID,
		other.SeatID, other.MandateID, other.MandateVersion, fixture.now)
	for _, wake := range []WakeEnvelope{lateSameSeat, other, early} {
		if _, err := fixture.store.Enqueue(ctx, wake); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := fixture.store.ClaimDue(ctx, fixture.organizationID, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(claims) != 2 || claims[0].Envelope.WakeID != early.WakeID ||
		claims[1].Envelope.WakeID != other.WakeID {
		t.Fatalf("ordered bounded claims = %+v", claims)
	}
	if err := fixture.store.Complete(ctx, fixture.organizationID,
		early.WakeID, 50); err != nil {
		t.Fatal(err)
	}
	if err := fixture.store.Fail(ctx, fixture.organizationID,
		other.WakeID, "authoritative provider outage"); err != nil {
		t.Fatal(err)
	}
	claims, err = fixture.store.ClaimDue(ctx, fixture.organizationID, 10)
	if err != nil || len(claims) != 1 || claims[0].Envelope.WakeID != lateSameSeat.WakeID {
		t.Fatalf("released seat claim = %+v, %v", claims, err)
	}
	*fixture.clock = fixture.now.Add(2 * time.Minute)
	claims, err = fixture.store.ClaimDue(ctx, fixture.organizationID, 10)
	if err != nil || len(claims) != 1 || claims[0].Envelope.WakeID != lateSameSeat.WakeID {
		t.Fatalf("expired claim recovery = %+v, %v", claims, err)
	}
	var recoveryError string
	if err := schedulerPool.QueryRow(ctx, `
		SELECT COALESCE(last_error,'') FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, fixture.tenantID, fixture.organizationID, lateSameSeat.WakeID).Scan(
		&recoveryError,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recoveryError, "recovered") {
		t.Fatalf("recovery evidence = %q", recoveryError)
	}
}

func TestIntegration_CompleteAndEnqueueIsAtomic(t *testing.T) {
	fixture := newSchedulerFixture(t, "complete-enqueue", defaultSchedulerConfig(), 18)
	ctx := context.Background()
	current := fixture.wake(
		"current", TriggerDependency, fixture.now.Add(-time.Minute),
		"work-order:atomic",
	)
	if _, err := fixture.store.Enqueue(ctx, current); err != nil {
		t.Fatal(err)
	}
	claims, err := fixture.store.ClaimDue(ctx, fixture.organizationID, 1)
	if err != nil || len(claims) != 1 ||
		claims[0].Envelope.WakeID != current.WakeID {
		t.Fatalf("current claim = %+v, %v", claims, err)
	}
	next := fixture.wake(
		"next", TriggerDependency, fixture.now,
		current.CoalesceKey,
	)
	result, err := fixture.store.CompleteAndEnqueue(
		ctx, fixture.organizationID, current.WakeID, 41, next,
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.WakeID != next.WakeID || result.Coalesced ||
		result.Deduplicated {
		t.Fatalf("successor enqueue = %+v", result)
	}
	var currentState, nextState string
	var actualSpend uint64
	if err := schedulerPool.QueryRow(ctx, `
		SELECT current.state,current.actual_spend_microunits,next.state
		FROM workforce_scheduled_wakes current
		JOIN workforce_scheduled_wakes next
		  ON next.tenant_id=current.tenant_id
		 AND next.organization_id=current.organization_id
		WHERE current.tenant_id=$1 AND current.organization_id=$2
		  AND current.wake_id=$3 AND next.wake_id=$4
	`, fixture.tenantID, fixture.organizationID,
		current.WakeID, next.WakeID).Scan(
		&currentState, &actualSpend, &nextState,
	); err != nil {
		t.Fatal(err)
	}
	if currentState != "completed" || nextState != "queued" ||
		actualSpend != 41 {
		t.Fatalf(
			"atomic success states current=%s next=%s spend=%d",
			currentState, nextState, actualSpend,
		)
	}
	replayed, err := fixture.store.CompleteAndEnqueue(
		ctx, fixture.organizationID, current.WakeID, 41, next,
	)
	if err != nil || !replayed.Deduplicated ||
		replayed.WakeID != next.WakeID {
		t.Fatalf("exact terminal replay = %+v, %v", replayed, err)
	}
	if err := fixture.store.Complete(
		ctx, fixture.organizationID, current.WakeID, 41,
	); err != nil {
		t.Fatalf("exact completion replay = %v", err)
	}
	if err := fixture.store.Complete(
		ctx, fixture.organizationID, current.WakeID, 42,
	); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting completion replay = %v", err)
	}

	rollbackCurrent := fixture.wake(
		"rollback-current", TriggerDependency,
		fixture.now.Add(-30*time.Second), "work-order:rollback",
	)
	if _, err := fixture.store.Enqueue(ctx, rollbackCurrent); err != nil {
		t.Fatal(err)
	}
	claims, err = fixture.store.ClaimDue(ctx, fixture.organizationID, 1)
	if err != nil || len(claims) != 1 ||
		claims[0].Envelope.WakeID != rollbackCurrent.WakeID {
		t.Fatalf("rollback claim = %+v, %v", claims, err)
	}
	invalidNext := fixture.wake(
		"invalid-next", TriggerDependency, fixture.now,
		rollbackCurrent.CoalesceKey,
	)
	invalidNext.TenantID = "tenant:unauthorized"
	if _, err := fixture.store.CompleteAndEnqueue(
		ctx, fixture.organizationID, rollbackCurrent.WakeID, 7, invalidNext,
	); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("invalid successor completion = %v", err)
	}
	var rollbackState string
	var successorCount int
	if err := schedulerPool.QueryRow(ctx, `
		SELECT state FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, fixture.tenantID, fixture.organizationID,
		rollbackCurrent.WakeID).Scan(&rollbackState); err != nil {
		t.Fatal(err)
	}
	if err := schedulerPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_scheduled_wakes
		WHERE tenant_id=$1 AND organization_id=$2 AND wake_id=$3
	`, fixture.tenantID, fixture.organizationID,
		invalidNext.WakeID).Scan(&successorCount); err != nil {
		t.Fatal(err)
	}
	if rollbackState != "dispatched" || successorCount != 0 {
		t.Fatalf(
			"rollback state=%s successor_count=%d",
			rollbackState, successorCount,
		)
	}
}

func TestIntegration_QuietHoursForceWakeAndDailyCeilings(t *testing.T) {
	config := defaultSchedulerConfig()
	config.QuietHoursStartUTC = 0
	config.QuietHoursEndUTC = 6
	config.DailyTaskLimit = 1
	config.DailySpendMicrounits = 100
	fixture := newSchedulerFixture(t, "quiet", config, 1)
	ctx := context.Background()
	normal := fixture.wake("normal", TriggerCorrection,
		fixture.now.Add(-time.Minute), "correction:normal")
	if _, err := fixture.store.Enqueue(ctx, normal); err != nil {
		t.Fatal(err)
	}
	claims, err := fixture.store.ClaimDue(ctx, fixture.organizationID, 10)
	if err != nil || len(claims) != 0 {
		t.Fatalf("quiet-hours claims = %+v, %v", claims, err)
	}
	force := fixture.wake("force", TriggerForce,
		fixture.now.Add(-30*time.Second), "force:owner")
	force.CoalesceKey = "force:owner"
	if _, err := fixture.store.Enqueue(ctx, force); err != nil {
		t.Fatal(err)
	}
	claims, err = fixture.store.ClaimDue(ctx, fixture.organizationID, 10)
	if err != nil || len(claims) != 1 || claims[0].Envelope.WakeID != force.WakeID {
		t.Fatalf("force wake = %+v, %v", claims, err)
	}
	if err := fixture.store.Complete(ctx, fixture.organizationID, force.WakeID, 100); err != nil {
		t.Fatal(err)
	}
	*fixture.clock = time.Date(2026, time.July, 30, 10, 0, 0, 0, time.UTC)
	claims, err = fixture.store.ClaimDue(ctx, fixture.organizationID, 10)
	if err != nil || len(claims) != 0 {
		t.Fatalf("daily ceiling claims = %+v, %v", claims, err)
	}
	var quietEvents, ceilingEvents int
	if err := schedulerPool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE event_kind='deferred_quiet_hours'),
		       COUNT(*) FILTER (WHERE event_kind IN (
		         'deferred_task_ceiling','deferred_spend_ceiling'
		       ))
		FROM workforce_wake_events
		WHERE tenant_id=$1 AND organization_id=$2
	`, fixture.tenantID, fixture.organizationID).Scan(
		&quietEvents, &ceilingEvents,
	); err != nil {
		t.Fatal(err)
	}
	if quietEvents == 0 || ceilingEvents == 0 {
		t.Fatalf("defer evidence quiet=%d ceiling=%d", quietEvents, ceilingEvents)
	}
}

func TestIntegration_RejectionsHandlerAndSpendConcurrencyDeferrals(t *testing.T) {
	fixture := newSchedulerFixture(t, "rejections", defaultSchedulerConfig(), 18)
	ctx := context.Background()
	if _, err := New(nil, nil, "", Config{}, nil); err == nil {
		t.Fatal("empty store construction accepted")
	}
	if _, err := New(schedulerPool, schedulerVault(t, "tenant:other"),
		fixture.tenantID, defaultSchedulerConfig(), func() time.Time { return fixture.now }); err == nil {
		t.Fatal("mismatched Vault accepted")
	}
	if _, err := fixture.store.Enqueue(ctx, WakeEnvelope{}); err == nil {
		t.Fatal("invalid wake enqueued")
	}
	crossTenant := fixture.wake("cross-tenant", TriggerOnce, fixture.now, "cross")
	crossTenant.TenantID = "tenant:other"
	if _, err := fixture.store.Enqueue(ctx, crossTenant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-tenant enqueue = %v", err)
	}
	unknownSeat := fixture.wake("unknown-seat", TriggerOnce, fixture.now, "unknown")
	unknownSeat.SeatID = "seat:unknown"
	if _, err := fixture.store.Enqueue(ctx, unknownSeat); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("unknown-seat enqueue = %v", err)
	}
	if _, err := fixture.store.ClaimDue(ctx, "", 1); err == nil {
		t.Fatal("invalid organization claim accepted")
	}
	if _, err := fixture.store.ClaimDue(ctx, fixture.organizationID, 0); err == nil {
		t.Fatal("invalid claim limit accepted")
	}
	invalidClock := *fixture.store
	invalidClock.now = func() time.Time { return time.Time{} }
	valid := fixture.wake("invalid-clock", TriggerOnce, fixture.now, "clock")
	if _, err := invalidClock.Enqueue(ctx, valid); !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalid-clock enqueue = %v", err)
	}
	if _, err := invalidClock.ClaimDue(ctx, fixture.organizationID, 1); !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalid-clock claim = %v", err)
	}
	if err := invalidClock.Complete(ctx, fixture.organizationID,
		"wake:missing", 0); !errors.Is(err, ErrUncertain) {
		t.Fatalf("invalid-clock completion = %v", err)
	}
	if err := fixture.store.Fail(ctx, fixture.organizationID,
		"wake:missing", "bad value"); err == nil {
		t.Fatal("invalid failure reason accepted")
	}

	server := httptest.NewServer(Handler(fixture.store, "handler-token"))
	defer server.Close()
	for _, test := range []struct {
		method, path string
		body         []byte
		status       int
	}{
		{http.MethodGet, "/internal/workforce/wake", nil, http.StatusNotFound},
		{http.MethodPost, "/wrong", nil, http.StatusNotFound},
		{http.MethodPost, "/internal/workforce/wake", []byte("{"), http.StatusUnauthorized},
	} {
		request, _ := http.NewRequest(test.method, server.URL+test.path, bytes.NewReader(test.body))
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode != test.status {
			t.Fatalf("%s %s status = %d", test.method, test.path, response.StatusCode)
		}
	}
	post := func(body []byte) int {
		request, _ := http.NewRequest(http.MethodPost,
			server.URL+"/internal/workforce/wake", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer handler-token")
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		return response.StatusCode
	}
	if status := post([]byte("{")); status != http.StatusBadRequest {
		t.Fatalf("invalid JSON status = %d", status)
	}
	if status := post(bytes.Repeat([]byte("x"), maxWakeBodyBytes+1)); status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body status = %d", status)
	}
	unauthorizedBody, _ := json.Marshal(crossTenant)
	if status := post(unauthorizedBody); status != http.StatusForbidden {
		t.Fatalf("unauthorized wake status = %d", status)
	}
	handlerWake := fixture.wake("handler", TriggerOnce, fixture.now, "handler")
	handlerBody, _ := json.Marshal(handlerWake)
	if status := post(handlerBody); status != http.StatusAccepted {
		t.Fatalf("handler enqueue status = %d", status)
	}
	handlerWake.Reason = "changed"
	conflictBody, _ := json.Marshal(handlerWake)
	if status := post(conflictBody); status != http.StatusConflict {
		t.Fatalf("handler conflict status = %d", status)
	}

	concurrencyConfig := defaultSchedulerConfig()
	concurrencyConfig.MaxOrganizationConcurrency = 1
	concurrencyConfig.MaxSeatConcurrency = 1
	concurrency := newSchedulerFixture(t, "org-concurrency", concurrencyConfig, 18)
	first := concurrency.wake("first", TriggerEvent,
		concurrency.now.Add(-time.Minute), "first")
	second := concurrency.wake("second", TriggerEvent,
		concurrency.now.Add(-30*time.Second), "second")
	second.SeatID = "seat:other"
	insertSchedulerAuthority(t, concurrency.tenantID, concurrency.organizationID,
		second.SeatID, second.MandateID, second.MandateVersion, concurrency.now)
	for _, wake := range []WakeEnvelope{first, second} {
		if _, err := concurrency.store.Enqueue(ctx, wake); err != nil {
			t.Fatal(err)
		}
	}
	claims, err := concurrency.store.ClaimDue(ctx, concurrency.organizationID, 10)
	if err != nil || len(claims) != 1 {
		t.Fatalf("organization concurrency claims = %+v, %v", claims, err)
	}

	spendConfig := defaultSchedulerConfig()
	spendConfig.DailySpendMicrounits = 50
	spend := newSchedulerFixture(t, "spend", spendConfig, 18)
	expensive := spend.wake("expensive", TriggerDeadline,
		spend.now.Add(-time.Minute), "expensive")
	if _, err := spend.store.Enqueue(ctx, expensive); err != nil {
		t.Fatal(err)
	}
	claims, err = spend.store.ClaimDue(ctx, spend.organizationID, 10)
	if err != nil || len(claims) != 0 {
		t.Fatalf("spend ceiling claims = %+v, %v", claims, err)
	}
	var spendEvents int
	if err := schedulerPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_wake_events
		WHERE tenant_id=$1 AND organization_id=$2
		  AND event_kind='deferred_spend_ceiling'
	`, spend.tenantID, spend.organizationID).Scan(&spendEvents); err != nil {
		t.Fatal(err)
	}
	if spendEvents != 1 {
		t.Fatalf("spend defer events = %d", spendEvents)
	}
}

func TestSchedulerValidation(t *testing.T) {
	if TriggerKind("unknown").Valid() || !TriggerDependency.Valid() {
		t.Fatal("trigger closed set changed")
	}
	if err := (Config{}).Validate(); err == nil {
		t.Fatal("invalid config accepted")
	}
	if err := (WakeEnvelope{}).Validate(); err == nil {
		t.Fatal("invalid wake accepted")
	}
	valid := WakeEnvelope{
		SchemaVersion: "workforce.wake.v1",
		WakeID:        "wake:x", ScheduleID: "schedule:x", TenantID: "tenant:x",
		OrganizationID: "organization:x", SeatID: "seat:x",
		MandateID: "mandate:x", MandateVersion: 1, Trigger: TriggerOnce,
		Reason: "reason", ScheduledAt: schedulerBaseTime(),
		Budget:         Budget{MaxTasks: 1},
		Model:          ModelBinding{Provider: "provider", ModelID: "model"},
		MGS:            MGSBinding{Reference: "mgs:x", Digest: strings.Repeat("a", 64)},
		IdempotencyKey: "wake:x", CoalesceKey: "coalesce:x",
		GraphScope: "graph:x",
	}
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*WakeEnvelope){
		"version":         func(wake *WakeEnvelope) { wake.SchemaVersion = "other" },
		"trigger":         func(wake *WakeEnvelope) { wake.Trigger = "other" },
		"digest_length":   func(wake *WakeEnvelope) { wake.MGS.Digest = "a" },
		"digest_encoding": func(wake *WakeEnvelope) { wake.MGS.Digest = strings.Repeat("z", 64) },
	} {
		candidate := valid
		mutate(&candidate)
		if err := candidate.Validate(); err == nil {
			t.Fatalf("invalid %s accepted", name)
		}
	}
}

type schedulerFixture struct {
	store          *Store
	tenantID       string
	organizationID string
	seatID         string
	mandateID      string
	now            time.Time
	clock          *time.Time
}

func newSchedulerFixture(
	t *testing.T,
	label string,
	config Config,
	hour int,
) schedulerFixture {
	t.Helper()
	now := time.Date(2026, time.July, 30, hour, 0, 0, 0, time.UTC)
	clock := now
	fixture := schedulerFixture{
		tenantID: "tenant:" + label, organizationID: "organization:" + label,
		seatID: "seat:developer", mandateID: "mandate:developer",
		now: now, clock: &clock,
	}
	insertSchedulerAuthority(t, fixture.tenantID, fixture.organizationID,
		fixture.seatID, fixture.mandateID, 1, now)
	store, err := New(schedulerPool, schedulerVault(t, fixture.tenantID),
		fixture.tenantID, config, func() time.Time { return clock })
	if err != nil {
		t.Fatal(err)
	}
	fixture.store = store
	return fixture
}

func (fixture schedulerFixture) wake(
	label string,
	trigger TriggerKind,
	scheduledAt time.Time,
	coalesceKey string,
) WakeEnvelope {
	return WakeEnvelope{
		SchemaVersion: "workforce.wake.v1",
		WakeID:        "wake:" + label, ScheduleID: "schedule:" + label,
		TenantID: fixture.tenantID, OrganizationID: fixture.organizationID,
		SeatID: fixture.seatID, MandateID: fixture.mandateID, MandateVersion: 1,
		Trigger: trigger, Reason: "bounded " + label, ScheduledAt: scheduledAt.UTC(),
		Budget:         Budget{MaxTasks: 1, MaxSpendMicrounits: 100},
		Model:          ModelBinding{Provider: "mimo", ModelID: "mimo-v2.5-pro"},
		MGS:            MGSBinding{Reference: "mgs:developer:v1", Digest: strings.Repeat("a", 64)},
		IdempotencyKey: "wake:" + label, CoalesceKey: coalesceKey,
		GraphScope: "graph:eligible",
	}
}

func defaultSchedulerConfig() Config {
	return Config{
		MaxOrganizationConcurrency: 4, MaxSeatConcurrency: 1,
		DailyTaskLimit: 100, DailySpendMicrounits: 1_000_000,
		QuietHoursStartUTC: 0, QuietHoursEndUTC: 0,
		ClaimLease: 2 * time.Minute,
	}
}

func insertSchedulerAuthority(
	t *testing.T,
	tenantID, organizationID, seatID, mandateID string,
	mandateVersion uint64,
	now time.Time,
) {
	t.Helper()
	for _, authority := range []struct {
		kind, id string
		version  uint64
	}{
		{"seat", seatID, 1},
		{"mandate", mandateID, mandateVersion},
	} {
		if _, err := schedulerPool.Exec(context.Background(), `
			INSERT INTO workforce_authority_records (
				tenant_id,organization_id,authority_kind,authority_id,version,
				owner_id,key_id,effective_at,canonical_hash,sealed_record,
				material_change,created_at
			) VALUES ($1,$2,$3,$4,$5,'owner','owner-key',$6,$7,$8,FALSE,$6)
			ON CONFLICT DO NOTHING
		`, tenantID, organizationID, authority.kind, authority.id,
			authority.version, now, strings.Repeat("b", 64), []byte{1}); err != nil {
			t.Fatal(err)
		}
		if _, err := schedulerPool.Exec(context.Background(), `
			INSERT INTO workforce_authority_heads (
				tenant_id,organization_id,authority_kind,authority_id,
				latest_version,updated_at
			) VALUES ($1,$2,$3,$4,$5,$6)
			ON CONFLICT DO NOTHING
		`, tenantID, organizationID, authority.kind, authority.id,
			authority.version, now); err != nil {
			t.Fatal(err)
		}
	}
}

func schedulerVault(t *testing.T, tenant string) *vault.UserVault {
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

func schedulerBaseTime() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startSchedulerPostgres(ctx context.Context) (string, string, error) {
	command := exec.CommandContext(ctx, "docker", "run", "-d", "--rm",
		"-e", "POSTGRES_PASSWORD=password", "-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432", schedulerPostgresImage)
	output, err := command.CombinedOutput()
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

func waitSchedulerPostgres(
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
