// Package metering implements reserve/finalize/void ledger transitions (docs/06-execution-hosting.md §6.2).
package metering

import (
	"context"
	"fmt"

	"github.com/paxlabs-inc/deus/internal/store"
)

// Ledger wraps invocation state transitions.
type Ledger struct {
	store *store.Store
}

// New returns a metering ledger.
func New(st *store.Store) *Ledger {
	return &Ledger{store: st}
}

// ReserveInput is a pending invocation reservation.
type ReserveInput struct {
	IdempotencyKey string
	ServiceID      string
	EndpointID     string
	CallerDID      string
	CallerWallet   string
	QuoteID        string
	Units          string
	PriceWei       string
	PricingVersion int
	ArgsHash       string
}

// Reserve creates or returns an existing reserved invocation row.
func (l *Ledger) Reserve(ctx context.Context, in ReserveInput) (store.InvocationRow, error) {
	var quoteID *string
	if in.QuoteID != "" {
		quoteID = &in.QuoteID
	}
	id, err := l.store.InsertReservedInvocation(ctx, store.InvocationRow{
		IdempotencyKey: in.IdempotencyKey,
		ServiceID:      in.ServiceID,
		EndpointID:     in.EndpointID,
		CallerDID:      in.CallerDID,
		CallerWallet:   in.CallerWallet,
		QuoteID:        quoteID,
		Units:          in.Units,
		PriceWei:       in.PriceWei,
		PricingVersion: in.PricingVersion,
		ArgsHash:       in.ArgsHash,
	})
	if err != nil {
		return store.InvocationRow{}, err
	}
	row, err := l.store.GetInvocation(ctx, id)
	if err != nil {
		return store.InvocationRow{}, err
	}
	if row.Outcome != "reserved" && row.Outcome != "ok" && row.Outcome != "voided" {
		return store.InvocationRow{}, fmt.Errorf("metering: unexpected outcome %q", row.Outcome)
	}
	return row, nil
}

// Finalize marks delivery and charge.
func (l *Ledger) Finalize(ctx context.Context, invocationID, outcome, resultHash, units, priceWei string, latencyMS int) error {
	return l.store.FinalizeInvocation(ctx, invocationID, outcome, resultHash, units, priceWei, latencyMS)
}

// Void releases a reservation without charge.
func (l *Ledger) Void(ctx context.Context, invocationID string) error {
	return l.store.VoidInvocation(ctx, invocationID)
}
