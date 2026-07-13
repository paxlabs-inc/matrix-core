// Package ledger orchestrates value movement on top of the store: it assigns
// the settlement tier, drives the atomic Pay (debit/credit/append/leaf in one DB
// tx), signs the inclusion receipt with the sequencer key, and reconstructs the
// Merkle proof for a receipt once its batch is sealed (layerx.frozen.kvx
// [ledger] + [receipts]).
package ledger

import (
	"context"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// receiptSigDomain separates the sequencer's receipt signature from any other
// signing context.
const receiptSigDomain = "layerx.receipt.sig.v1"

// Ledger ties the store, sequencer signer, and tier policy together.
type Ledger struct {
	st             *store.Store
	signer         *sig.Signer
	microThreshold int64 // micro-USDX; below = micropayment, at/above = material
}

// New constructs a Ledger.
func New(st *store.Store, signer *sig.Signer, microThresholdMicroUSDX int64) *Ledger {
	return &Ledger{st: st, signer: signer, microThreshold: microThresholdMicroUSDX}
}

// tierFor classifies an amount per the tiered-settlement policy.
func (l *Ledger) tierFor(amountMicro int64) string {
	if amountMicro < l.microThreshold {
		return types.TierMicropayment
	}
	return types.TierMaterial
}

// receiptSigningBytes is the deterministic preimage the sequencer signs.
func receiptSigningBytes(seq int64, leafHex string) []byte {
	return []byte(fmt.Sprintf("%s:%d:%s", receiptSigDomain, seq, leafHex))
}

// Pay moves amountMicro USDX from fromDID to toDID and returns a signed receipt.
// The leaf + signature are computed inside the store's Pay transaction so the
// transfer and its commitment land atomically. ref is the optional 32-byte
// binding digest carried on the row + receipt JSON (row-level v1; the leaf
// preimage is unchanged — folding ref in is a v2 domain bump).
func (l *Ledger) Pay(ctx context.Context, fromDID, toDID string, amountMicro int64, ref string) (types.Receipt, error) {
	tier := l.tierFor(amountMicro)
	finalize := func(seq int64, ts time.Time) (string, string) {
		leafHex := accumulator.LeafHashHex(accumulator.CanonicalLeaf(seq, fromDID, toDID, amountMicro, ts.UnixNano()))
		sigHex := l.signer.Sign(receiptSigningBytes(seq, leafHex))
		return leafHex, sigHex
	}
	res, err := l.st.Pay(ctx, fromDID, toDID, amountMicro, tier, ref, finalize)
	if err != nil {
		return types.Receipt{}, err
	}
	return types.Receipt{
		Seq:          res.Seq,
		FromDID:      fromDID,
		ToDID:        toDID,
		AmountUSDX:   types.FormatUSDX(amountMicro),
		Tier:         res.Tier,
		TS:           res.TS,
		LeafHashHex:  res.LeafHex,
		SequencerSig: res.SigHex,
		SequencerKey: l.signer.PublicHex(),
		Ref:          ref,
		Settled:      false,
	}, nil
}

// CaptureHold consumes an open hold as its payer-authorized captor: the payee
// is credited amountMicro through the SAME transfer commitment path as Pay
// (monotonic seq, Merkle leaf, sequencer signature), any remainder returns to
// the payer, and the signed receipt (carrying the hold's ref) is returned.
// Idempotent per the store's capture semantics.
func (l *Ledger) CaptureHold(ctx context.Context, holdID, captorDID string, amountMicro int64) (types.Receipt, store.Hold, error) {
	tier := l.tierFor(amountMicro)
	var payer, payee string
	finalize := func(seq int64, ts time.Time) (string, string) {
		leafHex := accumulator.LeafHashHex(accumulator.CanonicalLeaf(seq, payer, payee, amountMicro, ts.UnixNano()))
		sigHex := l.signer.Sign(receiptSigningBytes(seq, leafHex))
		return leafHex, sigHex
	}
	// The store locks + validates the hold before finalize runs; payer/payee are
	// read back through the closure so the leaf commits to the hold's parties.
	h, err := l.st.GetHold(ctx, holdID)
	if err != nil {
		return types.Receipt{}, store.Hold{}, err
	}
	payer, payee = h.PayerDID, h.PayeeDID
	res, h, err := l.st.CaptureHold(ctx, holdID, captorDID, amountMicro, tier, finalize)
	if err != nil {
		return types.Receipt{}, store.Hold{}, err
	}
	return types.Receipt{
		Seq:          res.Seq,
		FromDID:      h.PayerDID,
		ToDID:        h.PayeeDID,
		AmountUSDX:   types.FormatUSDX(amountMicro),
		Tier:         res.Tier,
		TS:           res.TS,
		LeafHashHex:  res.LeafHex,
		SequencerSig: res.SigHex,
		SequencerKey: l.signer.PublicHex(),
		Ref:          h.Ref,
		Settled:      false,
	}, h, nil
}

// Receipt returns the signed receipt for a transfer the caller is party to. If
// the transfer's batch is sealed, the Merkle root + inclusion path (and anchor
// tx once anchored) are populated so the receipt is independently verifiable.
func (l *Ledger) Receipt(ctx context.Context, seq int64, callerDID string) (types.Receipt, error) {
	row, err := l.st.GetTransfer(ctx, seq, callerDID)
	if err != nil {
		return types.Receipt{}, err
	}
	r := types.Receipt{
		Seq:          row.Seq,
		BatchID:      row.BatchID,
		FromDID:      row.FromDID,
		ToDID:        row.ToDID,
		AmountUSDX:   types.FormatUSDX(row.AmountMicro),
		Tier:         row.Tier,
		TS:           row.TS,
		LeafHashHex:  row.LeafHex,
		SequencerSig: row.SigHex,
		SequencerKey: l.signer.PublicHex(),
		Ref:          row.Ref,
		BatchRootHex: row.BatchRootHex,
		AnchorTxHash: row.AnchorTx,
		Settled:      row.Settled,
	}
	if row.BatchID != "" && row.BatchRootHex != "" {
		path, perr := l.inclusionPath(ctx, row.BatchID, seq)
		if perr == nil {
			r.InclusionPath = path
		}
	}
	return r, nil
}

// ReceiptPublic returns the signed receipt for any transfer by seq, WITHOUT
// caller-DID ownership scoping — the public explorer/RPC receipt read (PHASE 4,
// full-transparency model). Like Receipt, it populates the Merkle root +
// inclusion path (and anchor tx once anchored) so the receipt is independently
// verifiable by anyone.
func (l *Ledger) ReceiptPublic(ctx context.Context, seq int64) (types.Receipt, error) {
	row, err := l.st.GetTransferPublic(ctx, seq)
	if err != nil {
		return types.Receipt{}, err
	}
	r := types.Receipt{
		Seq:          row.Seq,
		BatchID:      row.BatchID,
		FromDID:      row.FromDID,
		ToDID:        row.ToDID,
		AmountUSDX:   types.FormatUSDX(row.AmountMicro),
		Tier:         row.Tier,
		TS:           row.TS,
		LeafHashHex:  row.LeafHex,
		SequencerSig: row.SigHex,
		SequencerKey: l.signer.PublicHex(),
		Ref:          row.Ref,
		BatchRootHex: row.BatchRootHex,
		AnchorTxHash: row.AnchorTx,
		Settled:      row.Settled,
	}
	if row.BatchID != "" && row.BatchRootHex != "" {
		if path, perr := l.inclusionPath(ctx, row.BatchID, seq); perr == nil {
			r.InclusionPath = path
		}
	}
	return r, nil
}

// inclusionPath recomputes the Merkle proof for seq within its sealed batch.
func (l *Ledger) inclusionPath(ctx context.Context, batchID string, seq int64) ([]string, error) {
	leavesRows, err := l.st.ListBatchLeaves(ctx, batchID)
	if err != nil {
		return nil, err
	}
	leaves := make([][32]byte, 0, len(leavesRows))
	idx := -1
	for i, br := range leavesRows {
		raw, derr := hex.DecodeString(br.LeafHex)
		if derr != nil || len(raw) != 32 {
			return nil, fmt.Errorf("ledger: bad leaf hex for seq %d", br.Seq)
		}
		var leaf [32]byte
		copy(leaf[:], raw)
		leaves = append(leaves, leaf)
		if br.Seq == seq {
			idx = i
		}
	}
	if idx < 0 {
		return nil, fmt.Errorf("ledger: seq %d not in batch %s", seq, batchID)
	}
	path, err := accumulator.Proof(leaves, idx)
	if err != nil {
		return nil, err
	}
	return accumulator.EncodePath(path), nil
}
