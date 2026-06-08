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
	PayoutFromEscrow(ctx context.Context, escrowAddr, payee, amountWei string) (txHash string, err error)
	AnchorSettlement(ctx context.Context, developerAddr, merkleRoot, totalWei string, count int) (txHash string, err error)
}

// StreamSettler settles PaymentStreams 0x0906 accrued amounts.
type StreamSettler interface {
	StreamSettle(ctx context.Context, chainStreamID string) (settledWei string, err error)
	StreamClose(ctx context.Context, chainStreamID string) (refundWei string, err error)
}

// DevPayer records settlements without chain writes.
type DevPayer struct {
	Payouts []PayoutRecord
	Anchors []AnchorRecord
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

// PayoutFromEscrow records a dev payout from a caller escrow.
func (d *DevPayer) PayoutFromEscrow(ctx context.Context, escrowAddr, payee, amountWei string) (string, error) {
	_ = ctx
	_ = escrowAddr
	return d.PayoutDeveloper(ctx, payee, amountWei)
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

// StreamSettle calls 0x0906 settle via the stream settler backend.
func StreamSettle(ctx context.Context, settler StreamSettler, chainStreamID string) (settledWei, txHash string, err error) {
	if settler == nil {
		return "", "", fmt.Errorf("settlement: stream settler not configured")
	}
	settledWei, err = settler.StreamSettle(ctx, chainStreamID)
	if err != nil {
		return "", "", err
	}
	return settledWei, fmt.Sprintf("0xstreamsettle%s", chainStreamID), nil
}

// StreamClose calls 0x0906 close and returns refund wei.
func StreamClose(ctx context.Context, settler StreamSettler, chainStreamID string) (refundWei, txHash string, err error) {
	if settler == nil {
		return "", "", fmt.Errorf("settlement: stream settler not configured")
	}
	refundWei, err = settler.StreamClose(ctx, chainStreamID)
	if err != nil {
		return "", "", err
	}
	return refundWei, fmt.Sprintf("0xstreamclose%s", chainStreamID), nil
}
