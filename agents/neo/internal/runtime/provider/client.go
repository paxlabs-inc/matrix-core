// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"centra/agents/neo/internal/runtime/protocol"
)

type FailureKind string

const (
	FailureRateLimited FailureKind = "rate_limited"
	FailureServer      FailureKind = "server"
	FailureTransport   FailureKind = "transport"
	FailureIdle        FailureKind = "idle"
	FailureRejected    FailureKind = "rejected"
	FailureProtocol    FailureKind = "protocol"
)

type Failure struct {
	Kind       FailureKind
	Attempts   int
	StatusCode int
	Err        error
}

func (failure *Failure) Error() string {
	if failure == nil {
		return "<nil>"
	}
	if failure.StatusCode != 0 {
		return fmt.Sprintf("resurrection provider: %s after %d attempt(s), status %d: %v",
			failure.Kind, failure.Attempts, failure.StatusCode, failure.Err)
	}
	return fmt.Sprintf("resurrection provider: %s after %d attempt(s): %v",
		failure.Kind, failure.Attempts, failure.Err)
}

func (failure *Failure) Unwrap() error {
	if failure == nil {
		return nil
	}
	return failure.Err
}

func IsFailureKind(err error, kind FailureKind) bool {
	var failure *Failure
	return errors.As(err, &failure) && failure.Kind == kind
}

type Config struct {
	GatewayURL     string
	BearerEnv      string
	Bearer         string
	ActorDID       string
	SlotLabel      string
	MaxAttempts    int
	BackoffInitial time.Duration
	BackoffMax     time.Duration
	IdleTimeout    time.Duration
	HTTPClient     *http.Client
	OnRawExchange  func(request, response []byte)
}

type Client struct {
	adapter        Adapter
	endpoint       string
	bearer         string
	actorDID       string
	slotLabel      string
	maxAttempts    int
	backoffInitial time.Duration
	backoffMax     time.Duration
	idleTimeout    time.Duration
	httpClient     *http.Client
	onRawExchange  func(request, response []byte)
}

func New(adapter Adapter, config Config) (*Client, error) {
	if adapter == nil {
		return nil, fmt.Errorf("resurrection provider: nil adapter")
	}
	gateway := strings.TrimSpace(config.GatewayURL)
	parsed, err := url.Parse(gateway)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, fmt.Errorf("resurrection provider: valid Centra AI gateway URL is required")
	}
	bearerEnv := strings.TrimSpace(config.BearerEnv)
	if bearerEnv == "" {
		bearerEnv = "MATRIX_GATEWAY_TOKEN"
	}
	bearer := strings.TrimSpace(config.Bearer)
	if bearer == "" {
		bearer = strings.TrimSpace(os.Getenv(bearerEnv))
	}
	if bearer == "" {
		return nil, fmt.Errorf("resurrection provider: gateway bearer env %s is empty", bearerEnv)
	}
	actorDID := strings.TrimSpace(config.ActorDID)
	if actorDID == "" {
		return nil, fmt.Errorf("resurrection provider: actor DID is required")
	}
	maxAttempts := config.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = 3
	}
	backoffInitial := config.BackoffInitial
	if backoffInitial <= 0 {
		backoffInitial = 250 * time.Millisecond
	}
	backoffMax := config.BackoffMax
	if backoffMax <= 0 {
		backoffMax = 2 * time.Second
	}
	if backoffMax < backoffInitial {
		return nil, fmt.Errorf("resurrection provider: backoff max must be at least initial backoff")
	}
	idleTimeout := config.IdleTimeout
	if idleTimeout <= 0 {
		idleTimeout = 120 * time.Second
	}
	httpClient := config.HTTPClient
	if httpClient == nil {
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.ResponseHeaderTimeout = idleTimeout
		httpClient = &http.Client{Timeout: 0, Transport: transport}
	}
	return &Client{
		adapter:        adapter,
		endpoint:       strings.TrimRight(gateway, "/") + "/v1/chat/completions",
		bearer:         bearer,
		actorDID:       actorDID,
		slotLabel:      strings.TrimSpace(config.SlotLabel),
		maxAttempts:    maxAttempts,
		backoffInitial: backoffInitial,
		backoffMax:     backoffMax,
		idleTimeout:    idleTimeout,
		httpClient:     httpClient,
		onRawExchange:  config.OnRawExchange,
	}, nil
}

func (client *Client) Generate(
	ctx context.Context,
	request protocol.GenerationRequest,
	turnUsage *TurnUsage,
) (protocol.NormalizedGeneration, error) {
	return client.generate(ctx, request, turnUsage, nil)
}

func (client *Client) GenerateStream(
	ctx context.Context,
	request protocol.GenerationRequest,
	turnUsage *TurnUsage,
	deliver func(protocol.StreamChunk) error,
) (protocol.NormalizedGeneration, error) {
	if deliver == nil {
		return protocol.NormalizedGeneration{}, &Failure{Kind: FailureProtocol, Err: fmt.Errorf("stream callback is required")}
	}
	request.Stream = true
	return client.generate(ctx, request, turnUsage, deliver)
}

func (client *Client) generate(
	ctx context.Context,
	request protocol.GenerationRequest,
	turnUsage *TurnUsage,
	deliver func(protocol.StreamChunk) error,
) (protocol.NormalizedGeneration, error) {
	body, err := client.adapter.TranslateRequest(request)
	if err != nil {
		return protocol.NormalizedGeneration{}, &Failure{
			Kind: FailureProtocol, Attempts: 0, Err: err,
		}
	}
	started := time.Now()
	var last *Failure
	for attempt := 1; attempt <= client.maxAttempts; attempt++ {
		generation, failure, delivered := client.generateAttempt(ctx, body, request.Stream, deliver)
		if failure == nil {
			generation.Provider = client.adapter.Name()
			if generation.Model == "" {
				generation.Model = request.Model
			}
			generation.Duration = time.Since(started)
			if finalizer, ok := client.adapter.(GenerationFinalizer); ok {
				generation, err = finalizer.FinalizeGeneration(generation)
				if err != nil {
					return protocol.NormalizedGeneration{}, &Failure{
						Kind: FailureProtocol, Attempts: attempt, Err: err,
					}
				}
			}
			if err := generation.Validate(); err != nil {
				return protocol.NormalizedGeneration{}, &Failure{
					Kind: FailureProtocol, Attempts: attempt, Err: err,
				}
			}
			if turnUsage != nil {
				turnUsage.Add(generation.Usage)
			}
			return generation, nil
		}
		failure.Attempts = attempt
		last = failure
		// Once a provisional delta escaped, this provider attempt belongs to the
		// outer StreamTransaction. It must retract that exact attempt before any
		// retry; a hidden client retry would merge two attempts into one stream.
		if delivered {
			return protocol.NormalizedGeneration{}, failure
		}
		if !failure.transient() || attempt == client.maxAttempts {
			return protocol.NormalizedGeneration{}, failure
		}
		if err := waitBackoff(ctx, client.backoff(attempt)); err != nil {
			return protocol.NormalizedGeneration{}, &Failure{
				Kind: FailureTransport, Attempts: attempt, Err: err,
			}
		}
	}
	return protocol.NormalizedGeneration{}, last
}

func (client *Client) generateAttempt(
	ctx context.Context,
	body []byte,
	stream bool,
	deliver func(protocol.StreamChunk) error,
) (protocol.NormalizedGeneration, *Failure, bool) {
	attemptContext, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := http.NewRequestWithContext(
		attemptContext,
		http.MethodPost,
		client.endpoint,
		bytes.NewReader(body),
	)
	if err != nil {
		return protocol.NormalizedGeneration{}, &Failure{Kind: FailureProtocol, Err: err}, false
	}
	request.Header.Set("Authorization", "Bearer "+client.bearer)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if stream {
		request.Header.Set("Accept", "text/event-stream")
	}
	request.Header.Set("X-Matrix-Actor-DID", client.actorDID)
	if client.slotLabel != "" {
		request.Header.Set("X-Matrix-Slot", client.slotLabel)
	}
	response, err := client.httpClient.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return protocol.NormalizedGeneration{}, &Failure{Kind: FailureTransport, Err: ctx.Err()}, false
		}
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			return protocol.NormalizedGeneration{}, &Failure{Kind: FailureIdle, Err: err}, false
		}
		return protocol.NormalizedGeneration{}, &Failure{Kind: FailureTransport, Err: err}, false
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		message, readErr := io.ReadAll(io.LimitReader(response.Body, 64<<10))
		client.recordRawExchange(body, message)
		if readErr != nil {
			message = []byte(readErr.Error())
		}
		failure := &Failure{
			StatusCode: response.StatusCode,
			Err:        fmt.Errorf("%s", strings.TrimSpace(string(message))),
		}
		switch {
		case response.StatusCode == http.StatusTooManyRequests:
			failure.Kind = FailureRateLimited
		case response.StatusCode >= http.StatusInternalServerError:
			failure.Kind = FailureServer
		default:
			failure.Kind = FailureRejected
		}
		return protocol.NormalizedGeneration{}, failure, false
	}

	var rawResponse bytes.Buffer
	var idle atomic.Bool
	reader := newIdleReader(io.TeeReader(response.Body, &rawResponse), client.idleTimeout, func() {
		idle.Store(true)
		cancel()
	})
	defer reader.Stop()
	var generation protocol.NormalizedGeneration
	delivered := false
	if stream {
		generation, delivered, err = client.decodeStream(reader, deliver)
	} else {
		var raw []byte
		raw, err = io.ReadAll(reader)
		if err == nil {
			generation, err = client.adapter.TranslateResponse(raw)
		}
	}
	client.recordRawExchange(body, rawResponse.Bytes())
	if idle.Load() {
		return protocol.NormalizedGeneration{}, &Failure{
			Kind: FailureIdle, Err: fmt.Errorf("provider stream idle for %s", client.idleTimeout),
		}, delivered
	}
	if err != nil {
		return protocol.NormalizedGeneration{}, &Failure{Kind: FailureProtocol, Err: err}, delivered
	}
	return generation, nil, delivered
}

func (client *Client) recordRawExchange(request, response []byte) {
	if client == nil || client.onRawExchange == nil {
		return
	}
	requestCopy := append([]byte(nil), request...)
	responseCopy := append([]byte(nil), response...)
	defer func() { _ = recover() }()
	client.onRawExchange(requestCopy, responseCopy)
}

func (client *Client) decodeStream(reader io.Reader, deliver func(protocol.StreamChunk) error) (protocol.NormalizedGeneration, bool, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64<<10), 4<<20)
	var generation protocol.NormalizedGeneration
	type callBuilder struct {
		id        string
		name      string
		arguments strings.Builder
	}
	calls := map[int]*callBuilder{}
	delivered := false
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] == ':' ||
			bytes.HasPrefix(line, []byte("event:")) ||
			bytes.HasPrefix(line, []byte("id:")) {
			continue
		}
		if !bytes.HasPrefix(line, []byte("data:")) {
			continue
		}
		chunk, err := client.adapter.TranslateStreamEvent(line)
		if err != nil {
			return protocol.NormalizedGeneration{}, delivered, err
		}
		if chunk.Done {
			break
		}
		if deliver != nil && (chunk.ContentDelta != "" || chunk.ReasoningDelta != "") {
			if err := deliver(chunk); err != nil {
				return protocol.NormalizedGeneration{}, delivered, err
			}
			delivered = true
		}
		generation.Content += chunk.ContentDelta
		generation.Reasoning += chunk.ReasoningDelta
		if chunk.FinishReason != "" {
			generation.FinishReason = chunk.FinishReason
		}
		if chunk.Usage != nil {
			generation.Usage = *chunk.Usage
		}
		for _, delta := range chunk.ToolCallDeltas {
			call := calls[delta.Index]
			if call == nil {
				call = &callBuilder{}
				calls[delta.Index] = call
			}
			if delta.ID != "" {
				call.id = delta.ID
			}
			if delta.Name != "" {
				call.name += delta.Name
			}
			call.arguments.WriteString(delta.Arguments)
		}
	}
	if err := scanner.Err(); err != nil {
		return protocol.NormalizedGeneration{}, delivered, err
	}
	indexes := make([]int, 0, len(calls))
	for index := range calls {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		source := calls[index]
		arguments := strings.TrimSpace(source.arguments.String())
		if arguments == "" {
			arguments = "{}"
		}
		call := protocol.NormalizedToolCall{
			ID:        source.id,
			Name:      source.name,
			Arguments: json.RawMessage(arguments),
		}
		if err := call.Validate(); err != nil {
			return protocol.NormalizedGeneration{}, delivered, err
		}
		generation.ToolCalls = append(generation.ToolCalls, call)
	}
	return generation, delivered, nil
}

func (client *Client) backoff(attempt int) time.Duration {
	delay := client.backoffInitial
	for step := 1; step < attempt && delay < client.backoffMax; step++ {
		if delay > client.backoffMax/2 {
			return client.backoffMax
		}
		delay *= 2
	}
	if delay > client.backoffMax {
		return client.backoffMax
	}
	return delay
}

func waitBackoff(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (failure *Failure) transient() bool {
	switch failure.Kind {
	case FailureRateLimited, FailureServer, FailureTransport, FailureIdle:
		return true
	default:
		return false
	}
}

type TurnUsage struct {
	mu    sync.Mutex
	calls int
	usage protocol.TokenUsage
}

type UsageSnapshot struct {
	Calls int
	Usage protocol.TokenUsage
}

func (usage *TurnUsage) Add(next protocol.TokenUsage) {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	if usage.calls == 0 {
		usage.usage.ModelCostKnown = next.ModelCostKnown
		usage.usage.ProviderSpendKnown = next.ProviderSpendKnown
	} else {
		usage.usage.ModelCostKnown = usage.usage.ModelCostKnown && next.ModelCostKnown
		usage.usage.ProviderSpendKnown = usage.usage.ProviderSpendKnown && next.ProviderSpendKnown
	}
	usage.calls++
	usage.usage.PromptTokens += next.PromptTokens
	usage.usage.CompletionTokens += next.CompletionTokens
	usage.usage.TotalTokens += next.TotalTokens
	usage.usage.ReasoningTokens += next.ReasoningTokens
	usage.usage.CachedTokens += next.CachedTokens
	usage.usage.ModelCostMicrocents += next.ModelCostMicrocents
	usage.usage.ProviderSpendMicrocents += next.ProviderSpendMicrocents
}

func (usage *TurnUsage) Snapshot() UsageSnapshot {
	usage.mu.Lock()
	defer usage.mu.Unlock()
	return UsageSnapshot{Calls: usage.calls, Usage: usage.usage}
}

type idleReader struct {
	reader io.Reader
	idle   time.Duration
	timer  *time.Timer
}

func newIdleReader(reader io.Reader, idle time.Duration, onIdle func()) *idleReader {
	timer := time.AfterFunc(idle, onIdle)
	timer.Stop()
	return &idleReader{reader: reader, idle: idle, timer: timer}
}

func (reader *idleReader) Read(buffer []byte) (int, error) {
	reader.timer.Reset(reader.idle)
	n, err := reader.reader.Read(buffer)
	reader.timer.Stop()
	return n, err
}

func (reader *idleReader) Stop() {
	reader.timer.Stop()
}
