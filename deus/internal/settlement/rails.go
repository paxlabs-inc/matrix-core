// Package settlement batches finalized invocations and pays developers (docs/08-payments-billing.md §8.2).
package settlement

import (
	"context"
	"fmt"
)

// Rail selects settlement mechanism.
type Rail string

const (
	RailDirect Rail = "direct"
	RailNet    Rail = "net"
	RailStream Rail = "stream"
)

// Payer executes on-chain or wallet payouts.
type Payer interface {
	PayoutDeveloper(ctx context.Context, payoutAddr, amountWei string) (txHash string, err error)
	AnchorSettlement(ctx context.Context, developerAddr, merkleRoot, totalWei string, count int) (txHash string, err error)
}

// DevPayer records settlements without chain writes.
type DevPayer struct {
	Payouts  []PayoutRecord
	Anchors  []AnchorRecord
}

// PayoutRecord is a dev-mode developer payout.
type PayoutRecord struct {
	To        string
	AmountWei string
}

// AnchorRecord is a dev-mode anchor call.
type AnchorRecord struct {
	Developer string
	Merkle    string
	TotalWei  string
	Count     int
}

// PayoutDeveloper records a dev payout.
func (d *DevPayer) PayoutDeveloper(ctx context.Context, payoutAddr, amountWei string) (string, error) {
	_ = ctx
	d.Payouts = append(d.Payouts, PayoutRecord{To: payoutAddr, AmountWei: amountWei})
	return fmt.Sprintf("0xsettle%08x", len(d.Payouts)), nil
}

// AnchorSettlement records a dev anchor.
func (d *DevPayer) AnchorSettlement(ctx context.Context, developerAddr, merkleRoot, totalWei string, count int) (string, error) {
	_ = ctx
	d.Anchors = append(d.Anchors, AnchorRecord{
		Developer: developerAddr,
		Merkle:    merkleRoot,
		TotalWei:  totalWei,
		Count:     count,
	})
	return fmt.Sprintf("0xanchor%08x", len(d.Anchors)), nil
}
