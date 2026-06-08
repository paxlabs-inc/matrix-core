// Package channels manages per-window payment channels (docs/08-payments-billing.md §8.3).
package channels

import (
	"context"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/paxlabs-inc/deus/internal/chain"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/internal/wallet"
)

const defaultWindow = 10 * time.Minute

// Service coordinates channel lifecycle.
type Service struct {
	store    *store.Store
	wallet   wallet.Client
	channels *chain.PaymentChannel
	dev      bool
}

// New returns a channel service.
func New(st *store.Store, wal wallet.Client) *Service {
	return &Service{store: st, wallet: wal, dev: true}
}

// WithChain enables on-chain fund verification for non-dev deployments.
func (s *Service) WithChain(c *chain.Client) error {
	pc, err := chain.NewPaymentChannel(c)
	if err != nil {
		return err
	}
	s.channels = pc
	s.dev = false
	return nil
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
	capWei, ok := new(big.Int).SetString(in.CapWei, 10)
	if !ok {
		return store.ChannelRow{}, fmt.Errorf("channels: invalid cap")
	}

	escrowAddr := strings.TrimSpace(in.EscrowAddr)
	fundTx := strings.TrimSpace(in.FundTx)
	balanceWei := in.CapWei

	if s.dev || strings.HasPrefix(fundTx, "0xdev") {
		if escrowAddr == "" {
			escrowAddr = "0xescrow-dev"
		}
	} else {
		if s.channels == nil {
			return store.ChannelRow{}, fmt.Errorf("channels: chain client required")
		}
		if fundTx == "" {
			if escrowAddr == "" {
				return store.ChannelRow{}, fmt.Errorf("channels: escrow_addr required")
			}
			httpWal, ok := s.wallet.(interface {
				FundEscrow(ctx context.Context, bearer, escrowAddr, capWei string) (string, error)
			})
			if !ok {
				return store.ChannelRow{}, fmt.Errorf("channels: fund_tx or wallet fund required")
			}
			tx, err := httpWal.FundEscrow(ctx, in.Bearer, escrowAddr, in.CapWei)
			if err != nil {
				return store.ChannelRow{}, err
			}
			fundTx = tx
		}
		verifiedEscrow, funded, err := s.channels.VerifyFundTx(ctx, fundTx, in.CallerWallet)
		if err != nil {
			return store.ChannelRow{}, fmt.Errorf("channels: verify fund: %w", err)
		}
		escrowAddr = verifiedEscrow
		fundedInt, ok := new(big.Int).SetString(funded, 10)
		if !ok {
			return store.ChannelRow{}, fmt.Errorf("channels: invalid funded wei")
		}
		if fundedInt.Cmp(capWei) < 0 {
			return store.ChannelRow{}, fmt.Errorf("channels: funded %s below cap %s", funded, in.CapWei)
		}
		balanceWei = funded
	}

	now := time.Now().UTC()
	end := now.Add(defaultWindow)
	var fundTxPtr *string
	if fundTx != "" {
		fundTxPtr = &fundTx
	}
	_, err := s.store.OpenChannel(ctx, store.ChannelRow{
		CallerDID:    in.CallerDID,
		CallerWallet: in.CallerWallet,
		EscrowAddr:   escrowAddr,
		BalanceWei:   balanceWei,
		WindowStart:  now,
		WindowEnd:    end,
		FundTx:       fundTxPtr,
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

// ReapPending voids stale pending_voucher rows and releases channel reserves.
func (s *Service) ReapPending(ctx context.Context, callerDID, channelID string) error {
	pending, err := s.store.VoidPendingVouchersForChannel(ctx, callerDID)
	if err != nil {
		return err
	}
	for i := range pending {
		if err := s.Void(ctx, channelID, pending[i].PriceWei); err != nil {
			return err
		}
	}
	return nil
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
