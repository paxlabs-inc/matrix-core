package settlement

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
)

// Settler runs net-settlement windows per developer.
type Settler struct {
	store  *store.Store
	payer  Payer
}

// NewSettler wires a settler.
func NewSettler(st *store.Store, payer Payer) *Settler {
	return &Settler{store: st, payer: payer}
}

// WindowResult is the outcome of RunWindow.
type WindowResult struct {
	SettlementID string
	TotalWei     string
	Count        int
	MerkleRoot   string
	TxHash       string
}

// RunWindow selects unsettled invocations, builds merkle, pays, anchors, marks settled.
func (s *Settler) RunWindow(ctx context.Context, developerID, payoutAddr string) (WindowResult, error) {
	inv, err := s.store.UnsettledInvocations(ctx, developerID)
	if err != nil {
		return WindowResult{}, err
	}
	if len(inv) == 0 {
		return WindowResult{}, fmt.Errorf("settlement: nothing to settle")
	}
	total := big.NewInt(0)
	ids := make([]string, 0, len(inv))
	for i := range inv {
		row := inv[i]
		ids = append(ids, row.ID)
		amt, ok := new(big.Int).SetString(row.PriceWei, 10)
		if !ok {
			return WindowResult{}, fmt.Errorf("settlement: invalid price %s", row.PriceWei)
		}
		total.Add(total, amt)
	}
	digests, err := s.store.ReceiptDigestsForInvocations(ctx, ids)
	if err != nil {
		return WindowResult{}, err
	}
	root, err := receipts.MerkleRoot(digests)
	if err != nil {
		return WindowResult{}, err
	}
	now := time.Now().UTC()
	settleID, err := s.store.InsertSettlement(ctx, store.SettlementRow{
		DeveloperID:     developerID,
		Rail:            string(RailNet),
		TotalWei:        total.Text(10),
		InvocationCount: len(inv),
		MerkleRoot:      root,
		WindowStart:     now.Add(-10 * time.Minute),
		WindowEnd:       now,
	})
	if err != nil {
		return WindowResult{}, err
	}
	tx, err := s.payer.PayoutDeveloper(ctx, payoutAddr, total.Text(10))
	if err != nil {
		return WindowResult{}, err
	}
	anchorTx, err := s.payer.AnchorSettlement(ctx, payoutAddr, root, total.Text(10), len(inv))
	if err != nil {
		return WindowResult{}, err
	}
	if err := s.store.MarkInvocationsSettled(ctx, settleID, ids); err != nil {
		return WindowResult{}, err
	}
	_ = s.store.CompleteSettlement(ctx, settleID, anchorTx)
	return WindowResult{
		SettlementID: settleID,
		TotalWei:     total.Text(10),
		Count:        len(inv),
		MerkleRoot:   root,
		TxHash:       tx,
	}, nil
}
