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
	store *store.Store
	payer Payer
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

// channelBatch accumulates one channel's voucher-bounded contribution.
type channelBatch struct {
	channelID string
	escrow    string
	voucherID string
	amount    *big.Int
}

// RunWindow settles a developer's net-rail invocations, bounded by each funding
// channel's highest caller-co-signed voucher (docs/08 §8.3, audit F2). For every
// channel it includes invocations oldest-first only while the running spend
// stays within the voucher cumulative minus what the escrow already redeemed;
// un-co-signed overflow stays unsettled until the caller signs a higher voucher.
// Deus therefore never pays out more than the caller cryptographically admitted.
func (s *Settler) RunWindow(ctx context.Context, developerID, payoutAddr string) (WindowResult, error) {
	// Release any reservations stranded by callers who never co-signed (F4).
	_, _ = s.store.ReleaseExpiredChannelReserves(ctx)

	inv, err := s.store.UnsettledNetInvocations(ctx, developerID)
	if err != nil {
		return WindowResult{}, err
	}
	if len(inv) == 0 {
		return WindowResult{}, fmt.Errorf("settlement: nothing to settle")
	}

	// Group by funding channel, preserving the oldest-first order from the query.
	order := make([]string, 0)
	byChannel := make(map[string][]store.InvocationRow)
	for i := range inv {
		row := inv[i]
		if row.ChannelID == nil {
			continue
		}
		cid := *row.ChannelID
		if _, seen := byChannel[cid]; !seen {
			order = append(order, cid)
		}
		byChannel[cid] = append(byChannel[cid], row)
	}

	total := big.NewInt(0)
	ids := make([]string, 0, len(inv))
	batches := make([]channelBatch, 0, len(order))
	var earliest time.Time

	for _, cid := range order {
		ch, err := s.store.GetChannelByID(ctx, cid)
		if err != nil {
			return WindowResult{}, err
		}
		// Headroom = highest co-signed cumulative minus what escrow already paid.
		voucher, verr := s.store.HighestVoucherForChannel(ctx, cid)
		if verr != nil {
			// No co-signed voucher yet → zero coverage; hold these for a later window.
			continue
		}
		signed, ok := new(big.Int).SetString(voucher.CumulativeWei, 10)
		if !ok {
			return WindowResult{}, fmt.Errorf("settlement: invalid voucher cumulative %q", voucher.CumulativeWei)
		}
		redeemed, ok := new(big.Int).SetString(emptyZero(ch.RedeemedWei), 10)
		if !ok {
			return WindowResult{}, fmt.Errorf("settlement: invalid redeemed %q", ch.RedeemedWei)
		}
		headroom := new(big.Int).Sub(signed, redeemed)
		if headroom.Sign() <= 0 {
			continue
		}

		batch := channelBatch{channelID: cid, escrow: ch.EscrowAddr, voucherID: voucher.ID, amount: big.NewInt(0)}
		running := new(big.Int)
		rows := byChannel[cid]
		for ri := range rows {
			row := &rows[ri]
			amt, ok := new(big.Int).SetString(row.PriceWei, 10)
			if !ok {
				return WindowResult{}, fmt.Errorf("settlement: invalid price %s", row.PriceWei)
			}
			next := new(big.Int).Add(running, amt)
			if next.Cmp(headroom) > 0 {
				break // beyond co-signed coverage; leave unsettled
			}
			running = next
			batch.amount.Add(batch.amount, amt)
			ids = append(ids, row.ID)
			if earliest.IsZero() || row.CreatedAt.Before(earliest) {
				earliest = row.CreatedAt
			}
		}
		if batch.amount.Sign() > 0 {
			total.Add(total, batch.amount)
			batches = append(batches, batch)
		}
	}

	if len(ids) == 0 {
		return WindowResult{}, fmt.Errorf("settlement: no co-signed voucher coverage to settle")
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
	if earliest.IsZero() {
		earliest = now
	}
	settleID, err := s.store.InsertSettlement(ctx, store.SettlementRow{
		DeveloperID:     developerID,
		Rail:            string(RailNet),
		TotalWei:        total.Text(10),
		InvocationCount: len(ids),
		MerkleRoot:      root,
		WindowStart:     earliest,
		WindowEnd:       now,
	})
	if err != nil {
		return WindowResult{}, err
	}

	// Release each channel's bounded amount from its escrow to the developer.
	var firstPayoutTx string
	for _, b := range batches {
		tx, err := s.payer.PayoutDeveloper(ctx, b.escrow, payoutAddr, b.amount.Text(10))
		if err != nil {
			return WindowResult{}, err
		}
		if firstPayoutTx == "" {
			firstPayoutTx = tx
		}
	}
	anchorTx, err := s.payer.AnchorSettlement(ctx, payoutAddr, root, total.Text(10), len(ids))
	if err != nil {
		return WindowResult{}, err
	}
	if err := s.store.MarkInvocationsSettled(ctx, settleID, ids); err != nil {
		return WindowResult{}, err
	}
	for _, b := range batches {
		_ = s.store.AdvanceChannelRedeemed(ctx, b.channelID, b.amount.Text(10))
		_ = s.store.MarkVoucherRedeemed(ctx, b.voucherID, settleID)
	}
	_ = s.store.CompleteSettlement(ctx, settleID, anchorTx)
	return WindowResult{
		SettlementID: settleID,
		TotalWei:     total.Text(10),
		Count:        len(ids),
		MerkleRoot:   root,
		TxHash:       firstPayoutTx,
	}, nil
}

func emptyZero(s string) string {
	if s == "" {
		return "0"
	}
	return s
}
