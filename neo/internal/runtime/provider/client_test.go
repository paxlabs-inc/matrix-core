// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestClientGatewayOnlyRetriesStreamsAndAccountsUsage(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempt := attempts.Add(1)
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path = %q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer gateway-secret" {
			t.Errorf("authorization = %q", request.Header.Get("Authorization"))
		}
		if request.Header.Get("X-Matrix-Actor-DID") != "did:matrix:test" {
			t.Errorf("actor = %q", request.Header.Get("X-Matrix-Actor-DID"))
		}
		if request.Header.Get("X-Matrix-Slot") != "neo" {
			t.Errorf("slot = %q", request.Header.Get("X-Matrix-Slot"))
		}
		switch attempt {
		case 1:
			http.Error(writer, "busy", http.StatusTooManyRequests)
		case 2:
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
		default:
			writeSuccessfulStream(writer, "delivered", 3, 2)
		}
	}))
	defer server.Close()
	t.Setenv("TEST_GATEWAY_TOKEN", "gateway-secret")
	t.Setenv("MIMO_API_KEY", "must-not-be-used")

	client, err := New(OpenAIAdapter{}, Config{
		GatewayURL:     server.URL,
		BearerEnv:      "TEST_GATEWAY_TOKEN",
		ActorDID:       "did:matrix:test",
		SlotLabel:      "neo",
		MaxAttempts:    3,
		BackoffInitial: time.Millisecond,
		BackoffMax:     time.Millisecond,
		IdleTimeout:    time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	var usage TurnUsage
	generation, err := client.Generate(context.Background(), providerRequest(true), &usage)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if generation.Content != "delivered" || generation.Usage.TotalTokens != 5 {
		t.Fatalf("generation = %+v", generation)
	}
	if attempts.Load() != 3 {
		t.Fatalf("attempts = %d, want 3", attempts.Load())
	}
	snapshot := usage.Snapshot()
	if snapshot.Calls != 1 || snapshot.Usage.PromptTokens != 3 ||
		snapshot.Usage.CompletionTokens != 2 || snapshot.Usage.TotalTokens != 5 {
		t.Fatalf("usage = %+v", snapshot)
	}
}

func TestClientRetryExhaustionIsTyped(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	t.Setenv("TEST_GATEWAY_TOKEN", "gateway-secret")
	client, err := New(OpenAIAdapter{}, Config{
		GatewayURL: server.URL, BearerEnv: "TEST_GATEWAY_TOKEN",
		ActorDID: "did:matrix:test", MaxAttempts: 2,
		BackoffInitial: time.Millisecond, BackoffMax: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.Generate(context.Background(), providerRequest(true), nil)
	if !IsFailureKind(err, FailureServer) {
		t.Fatalf("Generate() error = %T %v", err, err)
	}
	var failure *Failure
	if !errors.As(err, &failure) || failure.Attempts != 2 ||
		failure.StatusCode != http.StatusServiceUnavailable || attempts.Load() != 2 {
		t.Fatalf("failure = %+v, attempts = %d", failure, attempts.Load())
	}
}

func TestClientRetriesProviderIdle(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if attempts.Add(1) == 1 {
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusOK)
			writer.(http.Flusher).Flush()
			<-request.Context().Done()
			return
		}
		writeSuccessfulStream(writer, "resumed", 1, 1)
	}))
	defer server.Close()
	t.Setenv("TEST_GATEWAY_TOKEN", "gateway-secret")
	client, err := New(OpenAIAdapter{}, Config{
		GatewayURL: server.URL, BearerEnv: "TEST_GATEWAY_TOKEN",
		ActorDID: "did:matrix:test", MaxAttempts: 2,
		BackoffInitial: time.Millisecond, BackoffMax: time.Millisecond,
		IdleTimeout: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	generation, err := client.Generate(context.Background(), providerRequest(true), nil)
	if err != nil {
		t.Fatalf("Generate() error = %v", err)
	}
	if generation.Content != "resumed" || attempts.Load() != 2 {
		t.Fatalf("generation = %+v, attempts = %d", generation, attempts.Load())
	}
}

func TestClientUsageAccountingIsRaceClean(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writeSuccessfulStream(writer, "ok", 2, 1)
	}))
	defer server.Close()
	t.Setenv("TEST_GATEWAY_TOKEN", "gateway-secret")
	client, err := New(OpenAIAdapter{}, Config{
		GatewayURL: server.URL, BearerEnv: "TEST_GATEWAY_TOKEN",
		ActorDID: "did:matrix:test", MaxAttempts: 1, IdleTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	const generations = 16
	var usage TurnUsage
	var group sync.WaitGroup
	errs := make(chan error, generations)
	for index := 0; index < generations; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			_, generateErr := client.Generate(context.Background(), providerRequest(true), &usage)
			errs <- generateErr
		}()
	}
	group.Wait()
	close(errs)
	for generateErr := range errs {
		if generateErr != nil {
			t.Fatalf("Generate() error = %v", generateErr)
		}
	}
	snapshot := usage.Snapshot()
	if snapshot.Calls != generations || snapshot.Usage.PromptTokens != generations*2 ||
		snapshot.Usage.CompletionTokens != generations ||
		snapshot.Usage.TotalTokens != generations*3 {
		t.Fatalf("usage = %+v", snapshot)
	}
}

func TestClientNeverFallsBackToDirectProviderCredentials(t *testing.T) {
	t.Setenv("MATRIX_GATEWAY_TOKEN", "")
	t.Setenv("MIMO_API_KEY", "direct-provider-secret")
	if _, err := New(OpenAIAdapter{}, Config{
		GatewayURL: "https://gateway.invalid", ActorDID: "did:matrix:test",
	}); err == nil {
		t.Fatal("client accepted a direct-provider credential without the gateway bearer")
	}
	if _, err := New(OpenAIAdapter{}, Config{
		ActorDID: "did:matrix:test",
	}); err == nil {
		t.Fatal("client accepted an empty gateway URL")
	}
}

func writeSuccessfulStream(writer http.ResponseWriter, content string, prompt, completion int) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprintf(writer, "data: {\"model\":\"mimo-v2.5-pro\",\"choices\":[{\"delta\":{\"content\":%q},\"finish_reason\":\"stop\"}]}\n\n", content)
	_, _ = fmt.Fprintf(writer, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d}}\n\n", prompt, completion, prompt+completion)
	_, _ = fmt.Fprint(writer, "data: [DONE]\n\n")
}
