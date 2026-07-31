// Package provider exposes Neo's production model-provider connectors without
// exposing Neo cognition, conversation state, or runtime supervision.
package provider

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"matrix/neo/internal/runtime/protocol"
	runtimeprovider "matrix/neo/internal/runtime/provider"
)

const (
	// MiMoV25ProModel is the sole production text-model identity.
	MiMoV25ProModel = "mimo-v2.5-pro"
	// MiMoChatEndpoint is Xiaomi's native MiMo chat-completions endpoint.
	MiMoChatEndpoint = "https://api.xiaomimimo.com/v1/chat/completions"
	// MiMoTemperature is Neo's production sampling temperature.
	MiMoTemperature = 0.3
	// MiMoTopP is Neo's production nucleus-sampling ceiling.
	MiMoTopP            = 0.95
	defaultMiMoAttempts = 3
)

// MiMoConfig binds one credentialed Neo MiMo connector.
type MiMoConfig struct {
	APIKey      string
	Endpoint    string
	ActorDID    string
	SlotLabel   string
	MaxAttempts int
	IdleTimeout time.Duration
}

// MiMoRequest is one stateless system-and-user generation request.
type MiMoRequest struct {
	System          string
	User            string
	MaxOutputTokens int
}

// MiMoExchange retains exact request and response evidence with normalized output.
type MiMoExchange struct {
	Request   []byte
	Response  []byte
	Content   string
	Reasoning string
	Usage     TokenUsage
}

// TokenUsage is the provider-reported token accounting for one exchange.
type TokenUsage struct {
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	ReasoningTokens  int
	CachedTokens     int
}

// MiMoClient is Neo's production MiMo adapter and transport composition.
type MiMoClient struct {
	generator *runtimeprovider.MiMoGenerator
	capture   *exchangeCapture
	call      sync.Mutex
}

type exchangeCapture struct {
	mu       sync.Mutex
	request  []byte
	response []byte
}

// NewMiMo constructs Neo's MiMo connector without exposing Neo cognitive state.
func NewMiMo(config MiMoConfig) (*MiMoClient, error) {
	endpoint := strings.TrimSpace(config.Endpoint)
	if endpoint == "" {
		endpoint = MiMoChatEndpoint
	}
	base, ok := strings.CutSuffix(strings.TrimRight(endpoint, "/"), "/v1/chat/completions")
	if !ok || strings.TrimSpace(base) == "" {
		return nil, fmt.Errorf("neo provider: MiMo chat-completions endpoint is required")
	}
	if strings.TrimSpace(config.APIKey) == "" || strings.TrimSpace(config.ActorDID) == "" {
		return nil, fmt.Errorf("neo provider: MiMo API key and actor DID are required")
	}
	maxAttempts := config.MaxAttempts
	if maxAttempts <= 0 {
		maxAttempts = defaultMiMoAttempts
	}
	capture := &exchangeCapture{}
	adapter := &runtimeprovider.MiMoAdapter{}
	client, err := runtimeprovider.New(adapter, runtimeprovider.Config{
		GatewayURL: base, Bearer: config.APIKey,
		ActorDID: config.ActorDID, SlotLabel: config.SlotLabel,
		MaxAttempts: maxAttempts, IdleTimeout: config.IdleTimeout,
		OnRawExchange: capture.set,
	})
	if err != nil {
		return nil, err
	}
	generator, err := runtimeprovider.NewMiMoGenerator(client, adapter)
	if err != nil {
		return nil, err
	}
	return &MiMoClient{generator: generator, capture: capture}, nil
}

// Complete executes one stateless MiMo exchange and returns replay evidence.
func (client *MiMoClient) Complete(ctx context.Context, request MiMoRequest) (MiMoExchange, error) {
	if client == nil || client.generator == nil || client.capture == nil {
		return MiMoExchange{}, fmt.Errorf("neo provider: MiMo client is unavailable")
	}
	if strings.TrimSpace(request.System) == "" || strings.TrimSpace(request.User) == "" ||
		request.MaxOutputTokens <= 0 {
		return MiMoExchange{}, fmt.Errorf("neo provider: complete MiMo request is required")
	}
	client.call.Lock()
	defer client.call.Unlock()
	client.capture.reset()
	generation, err := client.generator.Generate(ctx, protocol.GenerationRequest{
		Model: MiMoV25ProModel,
		Messages: []protocol.Message{
			{Role: protocol.RoleSystem, Content: request.System},
			{Role: protocol.RoleUser, Content: request.User},
		},
		MaxOutputTokens: request.MaxOutputTokens,
		Stream:          true,
	}, nil)
	if err != nil {
		return MiMoExchange{}, err
	}
	rawRequest, rawResponse := client.capture.take()
	if len(rawRequest) == 0 || len(rawResponse) == 0 {
		return MiMoExchange{}, fmt.Errorf("neo provider: MiMo replay evidence is unavailable")
	}
	return MiMoExchange{
		Request: rawRequest, Response: rawResponse,
		Content: generation.Content, Reasoning: generation.Reasoning,
		Usage: TokenUsage{
			PromptTokens:     generation.Usage.PromptTokens,
			CompletionTokens: generation.Usage.CompletionTokens,
			TotalTokens:      generation.Usage.TotalTokens,
			ReasoningTokens:  generation.Usage.ReasoningTokens,
			CachedTokens:     generation.Usage.CachedTokens,
		},
	}, nil
}

func (capture *exchangeCapture) set(request, response []byte) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.request = append([]byte(nil), request...)
	capture.response = append([]byte(nil), response...)
}

func (capture *exchangeCapture) reset() {
	capture.set(nil, nil)
}

func (capture *exchangeCapture) take() ([]byte, []byte) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return append([]byte(nil), capture.request...), append([]byte(nil), capture.response...)
}
