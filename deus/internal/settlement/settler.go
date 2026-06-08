package settlement

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/paxlabs-inc/deus/internal/channels"
	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
)

// Settler runs net-settlement windows per developer.
type Settler struct {
	store    *store.Store
	payer    Payer
	channels *channels.Service
}

// NewSettler wires a settler.
func NewSettler(st *store.Store, payer Payer, chSvc *channels.Service) *Settler {
	return &Settler{store: st, payer: payer, channels: chSvc}
}

// WindowResult is the outcome of RunWindow.
type WindowResult struct {
	SettlementID string
	TotalWei     string
	Count        int
	MerkleRoot   string
	TxHash       string
}

// RunWindow selects unsettled invocations, bounds payout by co-signed vouchers (§8.3), pays, anchors.
func (s *Settler) RunWindow(ctx context.Context, developerID, payoutAddr string) (WindowResult, error) {
	inv, err := s.store.UnsettledInvocations(ctx, developerID, string(RailNet))
	if err != nil {
		return WindowResult{}, err
	}
	if len(inv) == 0 {
		return WindowResult{}, fmt.Errorf("settlement: nothing to settle")
	}

	windowStart := inv[0].CreatedAt
	byCaller := make(map[string][]store.InvocationRow)
	for i := range inv {
		row := inv[i]
		if row.CreatedAt.Before(windowStart) {
			windowStart = row.CreatedAt
		}
		byCaller[row.CallerDID] = append(byCaller[row.CallerDID], row)
	}

	total := big.NewInt(0)
	ids := make([]string, 0, len(inv))
	redeemedVouchers := make(map[string]string) // channelID -> voucherID
	var lastPayoutTx string

	for callerDID, rows := range byCaller {
		ch, err := s.store.ActiveChannelForCaller(ctx, callerDID)
		if err != nil {
			return WindowResult{}, fmt.Errorf("settlement: no active channel for caller %s: %w", callerDID, err)
		}
		if s.channels != nil {
			_ = s.channels.ReapPending(ctx, callerDID, ch.ID)
		}
		voucher, err := s.store.HighestVoucherForChannel(ctx, ch.ID)
		if err != nil {
			return WindowResult{}, fmt.Errorf("settlement: missing co-signed voucher for channel %s", ch.ID)
		}
		if voucher.RedeemedIn != nil && *voucher.RedeemedIn != "" {
			return WindowResult{}, fmt.Errorf("settlement: voucher %s already redeemed", voucher.ID)
		}

		callerSum := big.NewInt(0)
		for i := range rows {
			ids = append(ids, rows[i].ID)
			amt, ok := new(big.Int).SetString(rows[i].PriceWei, 10)
			if !ok {
				return WindowResult{}, fmt.Errorf("settlement: invalid price %s", rows[i].PriceWei)
			}
			callerSum.Add(callerSum, amt)
		}

		voucherCap, ok := new(big.Int).SetString(voucher.CumulativeWei, 10)
		if !ok {
			return WindowResult{}, fmt.Errorf("settlement: invalid voucher cumulative")
		}
		if callerSum.Cmp(voucherCap) > 0 {
			return WindowResult{}, fmt.Errorf(
				"settlement: ledger sum %s exceeds co-signed voucher cap %s for caller %s",
				callerSum.Text(10), voucher.CumulativeWei, callerDID,
			)
		}
		tx, err := s.payer.PayoutFromEscrow(ctx, ch.EscrowAddr, payoutAddr, callerSum.Text(10))
		if err != nil {
			return WindowResult{}, err
		}
		lastPayoutTx = tx
		total.Add(total, callerSum)
		redeemedVouchers[ch.ID] = voucher.ID
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
		WindowStart:     windowStart,
		WindowEnd:       now,
	})
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
	for _, vid := range redeemedVouchers {
		_ = s.store.MarkVoucherRedeemed(ctx, vid, settleID)
	}
	_ = s.store.CompleteSettlement(ctx, settleID, anchorTx)
	return WindowResult{
		SettlementID: settleID,
		TotalWei:     total.Text(10),
		Count:        len(inv),
		MerkleRoot:   root,
		TxHash:       lastPayoutTx,
	}, nil
}
