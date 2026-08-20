// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"centra/agents/neo/internal/runtime/records"
)

type failingDeliveryReporter struct {
	mu        sync.Mutex
	failUntil int
	calls     []string
}

func (reporter *failingDeliveryReporter) Say(string, bool) {}

func (reporter *failingDeliveryReporter) SayResult(content string, _ bool) error {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	reporter.calls = append(reporter.calls, content)
	if len(reporter.calls) <= reporter.failUntil {
		return errors.New("frontend acknowledgement unavailable")
	}
	return nil
}

func (reporter *failingDeliveryReporter) count() int {
	reporter.mu.Lock()
	defer reporter.mu.Unlock()
	return len(reporter.calls)
}

func TestDeliveryRetryPreservesAnswerAndNeverRestartsCognition(t *testing.T) {
	for _, test := range []struct {
		name       string
		failUntil  int
		turnFails  bool
		turnTries  int
		retryLater bool
	}{
		{name: "bounded inline retry", failUntil: 2, turnTries: 3},
		{name: "durable retry later", failUntil: 100, turnFails: true, turnTries: 3, retryLater: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var providerCalls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				body, _ := io.ReadAll(request.Body)
				var decoded gatewayRequest
				_ = json.Unmarshal(body, &decoded)
				if handleCapabilityCanary(writer, decoded) {
					return
				}
				providerCalls.Add(1)
				writeSSEText(writer, "The accepted answer survives delivery independently.")
			}))
			t.Cleanup(gateway.Close)
			turnID := "delivery-retry-" + test.name
			userContent := "Explain what survives delivery."
			store := realTurnStore(t, turnID, userContent)
			manager, _ := realNativeManager(t)
			adapter, err := NewToolManagerAdapter(manager, nil)
			if err != nil {
				t.Fatal(err)
			}
			reporter := &failingDeliveryReporter{failUntil: test.failUntil}
			runtimeLoop, err := New(realMiMoGenerator(t, gateway.URL), adapter, store,
				Config{TurnID: turnID, Model: "mimo-v2", IdleTimeout: 5 * time.Second},
				Dependencies{Delivery: &DeliveryChoke{Reporter: reporter}})
			if err != nil {
				t.Fatal(err)
			}
			response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
			var retry *DeliveryRetry
			if test.turnFails != errors.As(turnErr, &retry) {
				t.Fatalf("response=%+v err=%v retry=%+v", response, turnErr, retry)
			}
			if providerCalls.Load() != 1 || reporter.count() != test.turnTries {
				t.Fatalf("provider calls=%d delivery calls=%d", providerCalls.Load(), reporter.count())
			}
			answer, err := store.LoadAnswerRecord(t.Context(), turnID, "accepted")
			if err != nil || answer.GeneratedAnswer != "The accepted answer survives delivery independently." {
				t.Fatalf("answer=%+v err=%v", answer, err)
			}
			delivery, err := store.LoadDeliveryRecord(t.Context(), turnID, "primary")
			if err != nil || len(delivery.Attempts) != test.turnTries {
				t.Fatalf("delivery=%+v err=%v", delivery, err)
			}
			turn, err := store.LoadTurnRecord(t.Context(), turnID)
			wantState := records.StateDelivered
			if test.turnFails {
				wantState = records.StateDeliveryRetry
			}
			if err != nil || turn.CurrentState != wantState || turn.AnswerIdentity != "accepted" {
				t.Fatalf("turn=%+v err=%v", turn, err)
			}
			if !test.retryLater {
				return
			}
			completedAttempts := reporter.count()
			reporter.mu.Lock()
			reporter.failUntil = completedAttempts
			reporter.mu.Unlock()
			retried, err := runtimeLoop.RetryDelivery(t.Context())
			if err != nil || retried.Content != answer.GeneratedAnswer || providerCalls.Load() != 1 {
				t.Fatalf("delivery-only retry=%+v err=%v provider=%d", retried, err, providerCalls.Load())
			}
			turn, err = store.LoadTurnRecord(t.Context(), turnID)
			if err != nil || turn.CurrentState != records.StateDelivered {
				t.Fatalf("retried turn=%+v err=%v", turn, err)
			}
		})
	}
}
