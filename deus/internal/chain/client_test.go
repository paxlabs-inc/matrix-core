package chain

import (
	"context"
	"os"
	"testing"
)

func TestNewRequiresRPCURL(t *testing.T) {
	_, err := New(context.Background(), "", DefaultChainID)
	if err == nil {
		t.Fatal("expected error for empty rpc url")
	}
}

func TestPingOptional(t *testing.T) {
	rpc := os.Getenv("PAXEER_RPC_URL")
	if rpc == "" {
		t.Skip("PAXEER_RPC_URL not set")
	}
	ctx := context.Background()
	c, err := New(ctx, rpc, DefaultChainID)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer c.Close()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}
