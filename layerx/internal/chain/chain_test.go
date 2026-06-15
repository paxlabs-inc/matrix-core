package chain

import (
	"context"
	"testing"
)

func TestDevSettlerDeterministicAndIdempotent(t *testing.T) {
	d := NewDevSettler()
	ctx := context.Background()

	tx1, err := d.AnchorBatch(ctx, "abc123", 3, nil)
	if err != nil {
		t.Fatalf("AnchorBatch: %v", err)
	}
	tx2, err := d.AnchorBatch(ctx, "abc123", 3, nil)
	if err != nil {
		t.Fatalf("AnchorBatch retry: %v", err)
	}
	if tx1 != tx2 {
		t.Fatal("same root must yield the same anchor tx (idempotent)")
	}
	other, err := d.AnchorBatch(ctx, "def456", 1, nil)
	if err != nil {
		t.Fatalf("AnchorBatch other: %v", err)
	}
	if tx1 == other {
		t.Fatal("different roots must yield different anchor tx hashes")
	}
	if len(tx1) < 3 || tx1[:2] != "0x" {
		t.Fatalf("anchor tx should be 0x-prefixed hex, got %q", tx1)
	}
}

// DevSettler must satisfy the Settler interface.
var _ Settler = (*DevSettler)(nil)
