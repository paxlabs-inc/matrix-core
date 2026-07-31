package provider

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func TestIntegration_MiMoV25Pro_ReturnsReplayEvidence(t *testing.T) {
	apiKey := strings.TrimSpace(os.Getenv("MIMO_API_KEY"))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("XIAOMI_API_KEY"))
	}
	if apiKey == "" {
		t.Skip("set MIMO_API_KEY or XIAOMI_API_KEY to run the live MiMo provider proof")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	client, err := NewMiMo(MiMoConfig{
		APIKey: apiKey, Endpoint: MiMoChatEndpoint,
		ActorDID: "did:matrix:provider-proof", SlotLabel: "provider-proof",
		MaxAttempts: 1, IdleTimeout: 90 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	exchange, err := client.Complete(ctx, MiMoRequest{
		System:          "Return only one JSON object with no markdown or explanation.",
		User:            `Return exactly {"schema_version":"workforce.v1","status":"ready"}.`,
		MaxOutputTokens: 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(exchange.Request) == 0 || len(exchange.Response) == 0 {
		t.Fatal("live MiMo exchange omitted exact replay evidence")
	}
	var output struct {
		SchemaVersion string `json:"schema_version"`
		Status        string `json:"status"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(exchange.Content)), &output); err != nil {
		t.Fatalf("live MiMo content is not the required JSON object: %v", err)
	}
	if output.SchemaVersion != "workforce.v1" || output.Status != "ready" {
		t.Fatalf("live MiMo output = %#v", output)
	}
}
