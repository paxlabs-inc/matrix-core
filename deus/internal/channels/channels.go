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

// EscrowReader reads the on-chain funded balance of a caller's PaymentChannel so
// the off-chain mirror can be bounded by real escrow (docs/06 §6.2, §8.3; F7).
type EscrowReader interface {
	FundedWei(ctx context.Context, escrowAddr string) (string, error)
}

// Service coordinates channel lifecycle.
type Service struct {
	store  *store.Store
	wallet wallet.Client
	escrow EscrowReader
}

// New returns a channel service. escrow may be nil in dev (no on-chain check).
func New(st *store.Store, wal wallet.Client, escrow EscrowReader) *Service {
	return &Service{store: st, wallet: wal, escrow: escrow}
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

// Open creates a channel row after verifying on-chain escrow funding. The
// credited balance is bounded by the contract's fundedWei so the off-chain
// reserve can never exceed real escrow (audit F7).
func (s *Service) Open(ctx context.Context, in OpenInput) (store.ChannelRow, error) {
	if in.CapWei == "" {
		return store.ChannelRow{}, fmt.Errorf("channels: cap required")
	}
	capWei, ok := new(big.Int).SetString(in.CapWei, 10)
	if !ok || capWei.Sign() <= 0 {
		return store.ChannelRow{}, fmt.Errorf("channels: invalid cap")
	}
	now := time.Now().UTC()
	end := now.Add(defaultWindow)

	balanceWei := in.CapWei
	if s.escrow != nil {
		// Production: the escrow contract must exist and be funded on-chain.
		if in.EscrowAddr == "" {
			return store.ChannelRow{}, fmt.Errorf("channels: escrow_addr required")
		}
		fundedStr, err := s.escrow.FundedWei(ctx, in.EscrowAddr)
		if err != nil {
			return store.ChannelRow{}, fmt.Errorf("channels: verify escrow: %w", err)
		}
		funded, ok := new(big.Int).SetString(fundedStr, 10)
		if !ok || funded.Sign() <= 0 {
			return store.ChannelRow{}, fmt.Errorf("channels: escrow not funded")
		}
		// Credit no more than what is actually escrowed on-chain.
		if funded.Cmp(capWei) < 0 {
			balanceWei = funded.Text(10)
		}
	} else if in.EscrowAddr == "" {
		// Dev only: no chain verifier wired.
		in.EscrowAddr = "0xescrow-dev"
	}

	_, err := s.store.OpenChannel(ctx, store.ChannelRow{
		CallerDID:    in.CallerDID,
		CallerWallet: in.CallerWallet,
		EscrowAddr:   in.EscrowAddr,
		BalanceWei:   balanceWei,
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
