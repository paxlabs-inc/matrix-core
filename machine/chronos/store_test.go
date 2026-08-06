package chronos

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"matrix/vault"
)

func TestStoreReopenAndCiphertextAtRest(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "chronos", "chronos.db")
	uv := testVault(t)
	store := openTestStore(t, path, "gene-alpha", uv)
	next := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	request := CreateRequest{
		ID: "alarm-private", IdempotencyKey: "private-key", NextFire: next,
		MisfirePolicy: MisfireFireOnce,
		Body:          Body{Payload: json.RawMessage(`{"conversation_id":"secret-conversation"}`), WakeMessage: "private wake body"},
	}
	created, fresh, err := store.Create(ctx, request)
	if err != nil || !fresh || created.ID != request.ID {
		t.Fatalf("create: fresh=%v alarm=%+v err=%v", fresh, created, err)
	}
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		raw, err := os.ReadFile(candidate)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range [][]byte{[]byte("secret-conversation"), []byte("private wake body")} {
			if bytes.Contains(raw, secret) {
				t.Fatalf("plaintext leaked into %s: %q", filepath.Base(candidate), secret)
			}
		}
		info, err := os.Stat(candidate)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("SQLite file %s mode=%v", candidate, info.Mode().Perm())
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range [][]byte{[]byte("secret-conversation"), []byte("private wake body")} {
		if bytes.Contains(raw, secret) {
			t.Fatalf("plaintext leaked into SQLite: %q", secret)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("database mode=%v", info.Mode().Perm())
	}
	reopened := openTestStore(t, path, "gene-alpha", uv)
	defer reopened.Close()
	alarms, err := reopened.List(ctx)
	if err != nil || len(alarms) != 1 || alarms[0].Body.WakeMessage != request.Body.WakeMessage {
		t.Fatalf("reopen list=%+v err=%v", alarms, err)
	}
}

func TestConcurrentIdempotentCreateAndExclusiveClaim(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-concurrent", testVault(t))
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	request := CreateRequest{
		IdempotencyKey: "automatrix-singleton", NextFire: now,
		Interval: time.Hour, MisfirePolicy: MisfireCoalesce,
		Body: Body{Payload: json.RawMessage(`{"kind":"automatrix"}`), WakeMessage: "wake"},
	}
	const workers = 16
	var wg sync.WaitGroup
	ids := make(chan string, workers)
	createErrs := make(chan error, workers)
	var freshCount atomic.Int32
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			alarm, fresh, err := store.Create(ctx, request)
			if err != nil {
				createErrs <- err
				return
			}
			if fresh {
				freshCount.Add(1)
			}
			ids <- alarm.ID
		}()
	}
	wg.Wait()
	close(ids)
	close(createErrs)
	for err := range createErrs {
		t.Fatal(err)
	}
	if freshCount.Load() != 1 {
		t.Fatalf("fresh creates=%d, want 1", freshCount.Load())
	}
	var id string
	for got := range ids {
		if id == "" {
			id = got
		} else if got != id {
			t.Fatalf("idempotent create returned %q and %q", id, got)
		}
	}
	claims := make(chan *Claim, workers)
	claimErrs := make(chan error, workers)
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			claim, err := store.ClaimDue(ctx, now, time.Minute)
			if err != nil {
				claimErrs <- err
				return
			}
			claims <- claim
		}()
	}
	wg.Wait()
	close(claims)
	close(claimErrs)
	for err := range claimErrs {
		t.Fatal(err)
	}
	nonNil := 0
	for claim := range claims {
		if claim != nil {
			nonNil++
		}
	}
	if nonNil != 1 {
		t.Fatalf("claims=%d, want 1", nonNil)
	}
}

func TestRecoveryMisfiresLeaseAndRecurringAcknowledge(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-recovery", testVault(t))
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	create := func(id string, policy MisfirePolicy, interval time.Duration) {
		t.Helper()
		_, _, err := store.Create(ctx, CreateRequest{
			ID: id, IdempotencyKey: "key-" + id, NextFire: now.Add(-5 * time.Hour),
			Interval: interval, MisfirePolicy: policy,
			Body: Body{Payload: json.RawMessage(`{"id":"` + id + `"}`), WakeMessage: id},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	create("skip-once", MisfireSkip, 0)
	create("skip-recurring", MisfireSkip, time.Hour)
	create("fire-once", MisfireFireOnce, 0)
	create("coalesce-recurring", MisfireCoalesce, time.Hour)
	recovery, err := store.Recover(ctx, now)
	if err != nil || recovery.Skipped != 2 || recovery.Coalesced != 2 {
		t.Fatalf("recovery=%+v err=%v", recovery, err)
	}
	alarms, err := store.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := make(map[string]Alarm, len(alarms))
	for _, alarm := range alarms {
		byID[alarm.ID] = alarm
	}
	if byID["skip-once"].Status != StatusSkipped || !byID["skip-recurring"].NextFire.After(now) {
		t.Fatalf("skip recovery incorrect: %+v %+v", byID["skip-once"], byID["skip-recurring"])
	}
	claimed := make(map[string]bool)
	for range 2 {
		claim, err := store.ClaimDue(ctx, now, time.Second)
		if err != nil || claim == nil {
			t.Fatalf("claim overdue: %+v err=%v", claim, err)
		}
		claimed[claim.Alarm.ID] = true
		if err := store.Acknowledge(ctx, claim.Alarm.ID, claim.LeaseToken, now); err != nil {
			t.Fatal(err)
		}
	}
	if !claimed["fire-once"] || !claimed["coalesce-recurring"] {
		t.Fatalf("claimed=%v", claimed)
	}
	alarms, _ = store.List(ctx)
	byID = make(map[string]Alarm, len(alarms))
	for _, alarm := range alarms {
		byID[alarm.ID] = alarm
	}
	if byID["fire-once"].Status != StatusCompleted || !byID["coalesce-recurring"].NextFire.After(now) {
		t.Fatalf("ack state incorrect: %+v %+v", byID["fire-once"], byID["coalesce-recurring"])
	}

	_, _, err = store.Create(ctx, CreateRequest{
		ID: "lease-recovery", IdempotencyKey: "lease-recovery", NextFire: now,
		MisfirePolicy: MisfireFireOnce, Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "lease"},
	})
	if err != nil {
		t.Fatal(err)
	}
	leaseClaim, err := store.ClaimDue(ctx, now, time.Second)
	if err != nil || leaseClaim == nil {
		t.Fatalf("lease claim=%+v err=%v", leaseClaim, err)
	}
	recovery, err = store.Recover(ctx, now.Add(2*time.Second))
	if err != nil || recovery.RecoveredLeases != 1 {
		t.Fatalf("lease recovery=%+v err=%v", recovery, err)
	}
	secondClaim, err := store.ClaimDue(ctx, now.Add(2*time.Second), time.Minute)
	if err != nil || secondClaim == nil || secondClaim.Alarm.ID != "lease-recovery" {
		t.Fatalf("second claim=%+v err=%v", secondClaim, err)
	}
	if err := store.Acknowledge(ctx, secondClaim.Alarm.ID, leaseClaim.LeaseToken, now); !errors.Is(err, ErrLease) {
		t.Fatalf("stale token error=%v", err)
	}
}

func TestCancelRescheduleConflictAndGeneBinding(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "chronos", "chronos.db")
	uv := testVault(t)
	store := openTestStore(t, path, "gene-one", uv)
	next := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	request := CreateRequest{ID: "alarm-1", IdempotencyKey: "key-1", NextFire: next,
		MisfirePolicy: MisfireFireOnce, Body: Body{Payload: json.RawMessage(`{"secret":"value"}`), WakeMessage: "wake"}}
	if _, _, err := store.Create(ctx, request); err != nil {
		t.Fatal(err)
	}
	conflict := request
	conflict.NextFire = next.Add(time.Hour)
	if _, _, err := store.Create(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflict error=%v", err)
	}
	changed, err := store.Reschedule(ctx, request.ID, next.Add(2*time.Hour))
	if err != nil || !changed {
		t.Fatalf("reschedule changed=%v err=%v", changed, err)
	}
	changed, err = store.Cancel(ctx, request.ID)
	if err != nil || !changed {
		t.Fatalf("cancel changed=%v err=%v", changed, err)
	}
	changed, err = store.Cancel(ctx, request.ID)
	if err != nil || changed {
		t.Fatalf("second cancel changed=%v err=%v", changed, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	wrongGene := openTestStore(t, path, "gene-two", uv)
	defer wrongGene.Close()
	if _, err := wrongGene.List(ctx); err == nil {
		t.Fatal("different machine Gene opened sealed alarm")
	}
}

func TestRescheduleDuringLeaseSurvivesAcknowledgement(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-override", testVault(t))
	defer store.Close()
	now := time.Now().UTC().Truncate(time.Microsecond)
	_, _, err := store.Create(ctx, CreateRequest{ID: "automatrix", IdempotencyKey: "automatrix",
		NextFire: now, CronExpr: "@every 45m", Timezone: "UTC", MisfirePolicy: MisfireCoalesce,
		Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "AUTOMATRIX"}})
	if err != nil {
		t.Fatal(err)
	}
	claim, err := store.ClaimDue(ctx, now, time.Minute)
	if err != nil || claim == nil {
		t.Fatalf("claim=%+v err=%v", claim, err)
	}
	override := now.Add(73 * time.Minute)
	if changed, err := store.Reschedule(ctx, "automatrix", override); err != nil || !changed {
		t.Fatalf("reschedule changed=%v err=%v", changed, err)
	}
	if err := store.Acknowledge(ctx, "automatrix", claim.LeaseToken, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	alarms, err := store.List(ctx)
	if err != nil || len(alarms) != 1 || alarms[0].Status != StatusScheduled || !alarms[0].NextFire.Equal(override) {
		t.Fatalf("alarm=%+v err=%v", alarms, err)
	}
}

func TestCronScheduleUsesIANAZoneAndAdvancesOnce(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-cron", testVault(t))
	defer store.Close()
	before := time.Now().UTC()
	alarm, _, err := store.Create(ctx, CreateRequest{ID: "daily", IdempotencyKey: "daily",
		CronExpr: "0 9 * * *", Timezone: "America/New_York", MisfirePolicy: MisfireCoalesce,
		Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "daily"}})
	if err != nil {
		t.Fatal(err)
	}
	location, _ := time.LoadLocation("America/New_York")
	local := alarm.NextFire.In(location)
	if !alarm.NextFire.After(before) || local.Hour() != 9 || local.Minute() != 0 {
		t.Fatalf("next fire=%s local=%s", alarm.NextFire, local)
	}
}

func testVault(t *testing.T) *vault.UserVault {
	t.Helper()
	kek := bytes.Repeat([]byte{0xA5}, 32)
	provider, err := vault.NewStaticKeyProvider(map[string][]byte{"kek1": kek}, "kek1")
	if err != nil {
		t.Fatal(err)
	}
	v := vault.New(provider)
	keyfile, err := v.ProvisionUser(context.Background(), "did:matrix:chronos-test")
	if err != nil {
		t.Fatal(err)
	}
	uv, err := v.OpenUser(context.Background(), keyfile)
	if err != nil {
		t.Fatal(err)
	}
	return uv
}

func openTestStore(t *testing.T, path, gene string, uv *vault.UserVault) *Store {
	t.Helper()
	store, err := Open(context.Background(), Config{Path: path, MachineGene: gene, Vault: uv})
	if err != nil {
		t.Fatal(err)
	}
	return store
}
