// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package loop

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"matrix/neo/internal/runtime/records"
)

type timingReporter struct {
	mu       sync.Mutex
	deltas   []recordedDelta
	observed chan recordedDelta
}

func (reporter *timingReporter) Delta(turn int, channel, text string) {
	delta := recordedDelta{Turn: turn, Channel: channel, Text: text}
	reporter.mu.Lock()
	reporter.deltas = append(reporter.deltas, delta)
	reporter.mu.Unlock()
	if channel == "content" || channel == "reasoning" {
		select {
		case reporter.observed <- delta:
		default:
		}
	}
}

func TestFrontendReceivesDurableProvisionalDeltaBeforeProviderCompletion(t *testing.T) {
	providerBlocked := make(chan struct{})
	providerRelease := make(chan struct{})
	gateway := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body, _ := io.ReadAll(request.Body)
		var decoded gatewayRequest
		_ = json.Unmarshal(body, &decoded)
		if handleCapabilityCanary(writer, decoded) {
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		first, _ := json.Marshal(map[string]interface{}{
			"model": "mimo-v2", "choices": []interface{}{map[string]interface{}{
				"index": 0, "delta": map[string]interface{}{"content": "Streaming arrived "},
			}},
		})
		fmt.Fprintf(writer, "data: %s\n\n", first)
		writer.(http.Flusher).Flush()
		close(providerBlocked)
		<-providerRelease
		last, _ := json.Marshal(map[string]interface{}{
			"model": "mimo-v2", "choices": []interface{}{map[string]interface{}{
				"index": 0, "delta": map[string]interface{}{"content": "before provider completion."},
				"finish_reason": "stop",
			}}, "usage": map[string]interface{}{"prompt_tokens": 4, "completion_tokens": 5, "total_tokens": 9},
		})
		fmt.Fprintf(writer, "data: %s\n\n", last)
		fmt.Fprint(writer, "data: [DONE]\n\n")
	}))
	t.Cleanup(gateway.Close)

	turnID := "stream-timing-turn"
	userContent := "Tell me when streaming arrives."
	store := realTurnStore(t, turnID, userContent)
	manager, _ := realNativeManager(t)
	adapter, err := NewToolManagerAdapter(manager, nil)
	if err != nil {
		t.Fatal(err)
	}
	reporter := &timingReporter{observed: make(chan recordedDelta, 4)}
	runtimeLoop, err := New(realMiMoGenerator(t, gateway.URL), adapter, store,
		Config{TurnID: turnID, Model: "mimo-v2", IdleTimeout: 5 * time.Second},
		Dependencies{Observer: NewReporterObserver(reporter, 0)})
	if err != nil {
		t.Fatal(err)
	}
	if runtimeLoop.stream == nil || !runtimeLoop.stream.Durable() {
		t.Fatal("stream transaction is not connected to the canonical cycle store")
	}
	type turnResult struct {
		response Response
		err      error
	}
	finished := make(chan turnResult, 1)
	go func() {
		response, turnErr := runtimeLoop.Turn(t.Context(), userContent)
		finished <- turnResult{response: response, err: turnErr}
	}()
	select {
	case <-providerBlocked:
	case <-time.After(3 * time.Second):
		close(providerRelease)
		t.Fatal("provider never reached the deliberately blocked mid-stream point")
	}
	select {
	case delta := <-reporter.observed:
		if delta.Channel != "content" || delta.Text != "Streaming arrived " {
			close(providerRelease)
			t.Fatalf("first frontend delta=%+v", delta)
		}
	case <-time.After(3 * time.Second):
		close(providerRelease)
		t.Fatal("frontend received no delta while provider was still running")
	}
	midCycle, midErr := store.LoadCycleRecord(t.Context(), turnID, 0)
	if midErr != nil || len(midCycle.StreamedOutputState) != 1 ||
		midCycle.StreamedOutputState[0].Status != records.StreamProvisional {
		close(providerRelease)
		t.Fatalf("provisional delta was not durable before frontend observation: cycle=%+v err=%v", midCycle, midErr)
	}
	select {
	case early := <-finished:
		close(providerRelease)
		t.Fatalf("provider completed before release: %+v", early)
	default:
	}
	close(providerRelease)
	result := <-finished
	if result.err != nil || result.response.Content != "Streaming arrived before provider completion." {
		t.Fatalf("turn response=%+v err=%v", result.response, result.err)
	}
	answer, err := store.LoadAnswerRecord(t.Context(), turnID, "accepted")
	if err != nil || answer.GeneratedAnswer != result.response.Content ||
		answer.StreamCommitState != records.StreamCommitted {
		t.Fatalf("durable accepted answer=%+v err=%v", answer, err)
	}
	provisional, committed := 0, 0
	var lastSequence uint64
	var streamState []records.StreamedOutput
	for generation := uint64(0); generation < 10; generation++ {
		cycle, loadErr := store.LoadCycleRecord(t.Context(), turnID, generation)
		if loadErr == nil {
			streamState = append(streamState, cycle.StreamedOutputState...)
		}
	}
	for _, output := range streamState {
		if output.Sequence <= lastSequence {
			t.Fatalf("non-monotonic stream sequence: %+v", streamState)
		}
		lastSequence = output.Sequence
		switch output.Status {
		case records.StreamProvisional:
			provisional++
		case records.StreamCommitted:
			committed++
		}
	}
	if provisional != 2 || committed != 2 {
		t.Fatalf("durable stream states provisional=%d committed=%d: %+v", provisional, committed, streamState)
	}
}
