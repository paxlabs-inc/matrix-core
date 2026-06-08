// Package channels manages per-window payment channels (docs/08-payments-billing.md §8.3).
package channels

import (
	"context"
	"fmt"
	"math/big"
	"time"

	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

const defaultWindow = 10 * time.Minute

// Service coordinates channel lifecycle.
type Service struct {
	store  *store.Store
	wallet wallet.Client
}

// New returns a channel service.
func New(st *store.Store, wal wallet.Client) *Service {
	return &Service{store: st, wallet: wal}
}

// OpenInput funds a new channel window.
type OpenInput struct {
	CallerDID    string
	CallerWallet string
	CapWei       string
	EscrowAddr   string
	FundTx       string
	Bearer       string
}

// Open creates a channel row after wallet funding.
func (s *Service) Open(ctx context.Context, in OpenInput) (store.ChannelRow, error) {
	if in.CapWei == "" {
		return store.ChannelRow{}, fmt.Errorf("channels: cap required")
	}
	now := time.Now().UTC()
	end := now.Add(defaultWindow)
	if in.EscrowAddr == "" {
		in.EscrowAddr = "0xescrow-dev"
	}
	_, err := s.store.OpenChannel(ctx, store.ChannelRow{
		CallerDID:    in.CallerDID,
		CallerWallet: in.CallerWallet,
		EscrowAddr:   in.EscrowAddr,
		BalanceWei:   in.CapWei,
		WindowStart:  now,
		WindowEnd:    end,
		FundTx:       &in.FundTx,
	})
	if err != nil {
		return store.ChannelRow{}, err
	}
	return s.store.ActiveChannelForCaller(ctx, in.CallerDID)
}

// Reserve atomically decrements available channel balance.
func (s *Service) Reserve(ctx context.Context, channelID, amountWei string) error {
	return s.store.ReserveChannelBalance(ctx, channelID, amountWei)
}

// Void releases a reservation without charge.
func (s *Service) Void(ctx context.Context, channelID, amountWei string) error {
	return s.store.ReleaseChannelReserve(ctx, channelID, amountWei)
}

// Finalize applies charge and updates cumulative voucher state on the channel row.
func (s *Service) Finalize(ctx context.Context, channelID, chargeWei string, nonce int64, cumulativeWei, voucherSig string) error {
	return s.store.FinalizeChannelCharge(ctx, channelID, chargeWei, nonce, cumulativeWei, voucherSig)
}

// Active returns the caller's open channel or error.
func (s *Service) Active(ctx context.Context, callerDID string) (store.ChannelRow, error) {
	return s.store.ActiveChannelForCaller(ctx, callerDID)
}

// AddCumulative adds charge to cumulative total (big.Int string math).
func AddCumulative(current, charge string) (string, error) {
	cur, ok := new(big.Int).SetString(current, 10)
	if !ok {
		cur = big.NewInt(0)
	}
	add, ok := new(big.Int).SetString(charge, 10)
	if !ok {
		return "", fmt.Errorf("channels: invalid charge")
	}
	return new(big.Int).Add(cur, add).Text(10), nil
}
