package streams_test

import (
	"context"
	"testing"
	"time"

	"github.com/paxlabs-inc/deus/internal/streams"
)

func TestDevBackendAccrualAndRefund(t *testing.T) {
	b := streams.NewDevBackend()
	opened := time.Now().UTC().Add(-2 * time.Second)
	if err := b.Register("1", "1000000000000", "10000000000000000", opened); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	accrued, err := b.Accrued(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if accrued == "0" {
		t.Fatalf("expected accrued > 0, got %s", accrued)
	}
	settled, err := b.Settle(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if settled == "0" {
		t.Fatalf("expected settle delta > 0, got %s", settled)
	}
	refund, err := b.Close(ctx, "1")
	if err != nil {
		t.Fatal(err)
	}
	if refund == "0" {
		t.Fatalf("expected refund of unspent cap, got %s", refund)
	}
}
