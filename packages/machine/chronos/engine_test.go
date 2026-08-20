package chronos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestEngineMutationPreemptsDeadlineAndAcknowledgesRealHTTP(t *testing.T) {
	deliveries := make(chan string, 4)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer machine-capability" {
			http.Error(response, "unauthorized", http.StatusUnauthorized)
			return
		}
		var body struct {
			AlarmID string `json:"alarm_id"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			http.Error(response, "bad request", http.StatusBadRequest)
			return
		}
		deliveries <- body.AlarmID
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	ctx := context.Background()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-engine", testVault(t))
	defer store.Close()
	engine, err := Start(ctx, EngineConfig{Store: store,
		Target: LoopbackTarget{URL: server.URL, Capability: "machine-capability"},
		Lease:  time.Second, RetryBase: 20 * time.Millisecond, RetryLimit: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	now := time.Now().UTC()
	_, _, err = engine.Create(ctx, CreateRequest{ID: "later", IdempotencyKey: "later", NextFire: now.Add(time.Second),
		MisfirePolicy: MisfireFireOnce, Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "later"}})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond)
	started := time.Now()
	_, _, err = engine.Create(ctx, CreateRequest{ID: "earlier", IdempotencyKey: "earlier", NextFire: time.Now().Add(40 * time.Millisecond),
		MisfirePolicy: MisfireFireOnce, Body: Body{Payload: json.RawMessage(`{"real":true}`), WakeMessage: "earlier"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-deliveries:
		if id != "earlier" {
			t.Fatalf("first delivery=%q", id)
		}
		if time.Since(started) > 400*time.Millisecond {
			t.Fatalf("mutation did not preempt sleeping deadline")
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("earlier alarm was not delivered")
	}
	if _, err := engine.Cancel(ctx, "later"); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, engine, "earlier", StatusCompleted)
	select {
	case duplicate := <-deliveries:
		t.Fatalf("duplicate delivery %q", duplicate)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEngineRetriesFailureThenAcknowledges(t *testing.T) {
	var attempts atomic.Int32
	delivered := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			http.Error(response, "retry", http.StatusServiceUnavailable)
			return
		}
		delivered <- struct{}{}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-retry", testVault(t))
	defer store.Close()
	engine, err := Start(context.Background(), EngineConfig{Store: store,
		Target: LoopbackTarget{URL: server.URL, Capability: "cap"}, Lease: time.Second,
		RetryBase: 25 * time.Millisecond, RetryLimit: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	_, _, err = engine.Create(context.Background(), CreateRequest{ID: "retry", IdempotencyKey: "retry",
		NextFire: time.Now().Add(10 * time.Millisecond), MisfirePolicy: MisfireFireOnce,
		Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "retry"}})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("retry did not deliver")
	}
	waitForStatus(t, engine, "retry", StatusCompleted)
	alarms, err := engine.List(context.Background())
	if err != nil || len(alarms) != 1 {
		t.Fatalf("alarms=%+v err=%v", alarms, err)
	}
	if alarms[0].Status != StatusCompleted || alarms[0].DeliveryAttempts != 2 || alarms[0].LastError != "" {
		t.Fatalf("retry state=%+v", alarms[0])
	}
}

func TestEngineReopenDeliversOneCoalescedOverdueWake(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "chronos", "chronos.db")
	uv := testVault(t)
	store := openTestStore(t, path, "gene-reopen", uv)
	_, _, err := store.Create(ctx, CreateRequest{ID: "overdue", IdempotencyKey: "overdue",
		NextFire: time.Now().Add(-time.Hour), Interval: time.Minute, MisfirePolicy: MisfireCoalesce,
		Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "overdue"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	delivered := make(chan struct{}, 2)
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		delivered <- struct{}{}
		response.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	reopened := openTestStore(t, path, "gene-reopen", uv)
	defer reopened.Close()
	engine, err := Start(ctx, EngineConfig{Store: reopened, Target: LoopbackTarget{URL: server.URL, Capability: "cap"}})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	select {
	case <-delivered:
	case <-time.After(time.Second):
		t.Fatal("overdue alarm was not delivered after reopen")
	}
	select {
	case <-delivered:
		t.Fatal("coalesced overdue recurrence delivered more than once")
	case <-time.After(100 * time.Millisecond):
	}
	alarms, _ := engine.List(ctx)
	if len(alarms) != 1 || !alarms[0].NextFire.After(time.Now()) {
		t.Fatalf("recurrence was not advanced: %+v", alarms)
	}
}

func TestEngineStopsAtConfiguredFailureCeiling(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		http.Error(response, "still unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	store := openTestStore(t, filepath.Join(t.TempDir(), "chronos", "chronos.db"), "gene-failure-limit", testVault(t))
	defer store.Close()
	engine, err := Start(context.Background(), EngineConfig{Store: store,
		Target:    LoopbackTarget{URL: server.URL, Capability: "cap"},
		RetryBase: 10 * time.Millisecond, RetryLimit: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close(context.Background())
	_, _, err = engine.Create(context.Background(), CreateRequest{ID: "bounded", IdempotencyKey: "bounded",
		NextFire: time.Now().Add(5 * time.Millisecond), MisfirePolicy: MisfireFireOnce,
		Body: Body{Payload: json.RawMessage(`{}`), WakeMessage: "bounded", MaxFailures: 2}})
	if err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, engine, "bounded", StatusFailed)
	alarms, _ := engine.List(context.Background())
	if alarms[0].DeliveryAttempts != 2 {
		t.Fatalf("attempts=%d, want 2", alarms[0].DeliveryAttempts)
	}
}

func TestLoopbackTargetRejectsNonLoopback(t *testing.T) {
	err := (LoopbackTarget{URL: "https://example.com/wake", Capability: "cap"}).Deliver(
		context.Background(), Delivery{Alarm: Alarm{ID: "alarm"}},
	)
	if err == nil {
		t.Fatal("non-loopback delivery target was accepted")
	}
}

func waitForStatus(t *testing.T, engine *Engine, id string, want Status) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		alarms, err := engine.List(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		for _, alarm := range alarms {
			if alarm.ID == id && alarm.Status == want {
				return
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("alarm %s did not reach status %s", id, want)
}
