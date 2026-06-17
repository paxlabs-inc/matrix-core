package chain

import (
	"context"
	"os"
	"testing"
)

func TestNewClientRequiresRPCURL(t *testing.T) {
	if _, err := NewClient(context.Background(), "", DefaultChainID); err == nil {
		t.Fatal("expected error for empty rpc url")
	}
	if _, err := NewClient(context.Background(), "   ", DefaultChainID); err == nil {
		t.Fatal("expected error for blank rpc url")
	}
}

func TestDefaultChainID(t *testing.T) {
	if DefaultChainID != 125 {
		t.Fatalf("DefaultChainID = %d, want 125 (Paxeer mainnet)", DefaultChainID)
	}
}

// TestPingOptional exercises a live endpoint only when one is provided.
func TestPingOptional(t *testing.T) {
	rpc := os.Getenv("LAYERX_CHAIN_RPC")
	if rpc == "" {
		t.Skip("LAYERX_CHAIN_RPC not set")
	}
	ctx := context.Background()
	c, err := NewClient(ctx, rpc, DefaultChainID)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	defer c.Close()
	if err := c.Ping(ctx); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if c.ChainID().Int64() != DefaultChainID {
		t.Fatalf("ChainID = %s, want %d", c.ChainID(), DefaultChainID)
	}
}
