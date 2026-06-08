package channels

import (
	"context"
	"fmt"

	"github.com/paxlabs-inc/deus/internal/receipts"
	"github.com/paxlabs-inc/deus/internal/store"
)

// VoucherService builds and verifies caller-co-signed vouchers.
type VoucherService struct {
	store  *store.Store
	signer *receipts.Signer
}

// NewVoucherService wires voucher helpers.
func NewVoucherService(st *store.Store, signer *receipts.Signer) *VoucherService {
	return &VoucherService{store: st, signer: signer}
}

// PendingVoucher is returned before caller co-signs.
type PendingVoucher struct {
	ChannelID       string
	CumulativeWei   string
	Nonce           int64
	LastReceiptHash string
	Digest          string
}

// BuildPending computes the next voucher digest for caller signing.
func (v *VoucherService) BuildPending(ch store.ChannelRow, chargeWei, receiptDigest string) (PendingVoucher, error) {
	cumulative, err := AddCumulative(ch.CumulativeWei, chargeWei)
	if err != nil {
		return PendingVoucher{}, err
	}
	nonce := ch.Nonce + 1
	fields := receipts.VoucherFields{
		ChannelID:       ch.ID,
		CumulativeWei:   cumulative,
		Nonce:           uint64(nonce),
		LastReceiptHash: receiptDigest,
	}
	digest, err := v.signer.VoucherDigest(fields)
	if err != nil {
		return PendingVoucher{}, err
	}
	return PendingVoucher{
		ChannelID:       ch.ID,
		CumulativeWei:   cumulative,
		Nonce:           nonce,
		LastReceiptHash: receiptDigest,
		Digest:          digest,
	}, nil
}

// CosignInput persists a caller-signed voucher.
type CosignInput struct {
	ChannelID       string
	CumulativeWei   string
	ChargeWei       string
	Nonce           int64
	LastReceiptHash string
	Digest          string
	CallerSig       string
	CallerWallet    string
}

// Cosign verifies and stores the voucher; rejects non-monotonic nonce.
func (v *VoucherService) Cosign(ctx context.Context, in CosignInput) (string, error) {
	ch, err := v.store.GetChannelByID(ctx, in.ChannelID)
	if err != nil {
		return "", err
	}
	if in.Nonce <= ch.Nonce {
		return "", fmt.Errorf("channels: voucher nonce not monotonic")
	}
	if err := v.signer.VerifyVoucherCaller(in.Digest, in.CallerSig, in.CallerWallet); err != nil {
		return "", err
	}
	// Finalize + persist atomically (audit F5): the channel nonce/cumulative must
	// never advance without a matching voucher row, or the chain corrupts.
	return v.store.CosignVoucher(ctx, in.ChargeWei, store.VoucherRow{
		ChannelID:       in.ChannelID,
		CumulativeWei:   in.CumulativeWei,
		Nonce:           in.Nonce,
		LastReceiptHash: in.LastReceiptHash,
		Digest:          in.Digest,
		CallerSig:       in.CallerSig,
	})
}
