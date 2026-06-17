// Package settle runs the tiered settlement worker (layerx.frozen.kvx
// [settlement]): on each window it nets the open transfers, builds the batch
// Merkle root, seals the batch, anchors the root on Paxeer, and records the
// anchor — at-least-once (a batch is marked anchored only after the chain
// returns). SettleNow also backs the on-demand force-settle path.
package settle

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/store"
	"github.com/paxlabs-inc/layerx/pkg/types"
)

// Ledger is the subset of the store the settlement worker needs. Narrowing it
// to an interface keeps the worker's orchestration (seal -> anchor -> mark, and
// crash recovery) unit-testable without a live Postgres. *store.Store
// satisfies it.
type Ledger interface {
	ListUnsettled(ctx context.Context, limit int) ([]store.UnsettledTransfer, error)
	SealBatch(ctx context.Context, rootHex string, seqs []int64, windowStart, windowEnd time.Time) (string, error)
	MarkAnchored(ctx context.Context, batchID, anchorTx string) error
	MarkBatchFailed(ctx context.Context, batchID, errText string) error
	GetAccount(ctx context.Context, did string) (types.Account, error)
	ListUnanchoredBatches(ctx context.Context) ([]store.PendingBatch, error)

	// Withdrawal payout (chain-out, PHASE 3).
	ListQueuedWithdrawals(ctx context.Context, limit int) ([]store.QueuedWithdrawal, error)
	ListSubmittedWithdrawals(ctx context.Context) ([]store.QueuedWithdrawal, error)
	SealWithdrawals(ctx context.Context, payoutRoot string, items []store.SealedWithdrawal) error
	MarkWithdrawalSettled(ctx context.Context, id, payoutTx string) error
	RecordWithdrawalError(ctx context.Context, payoutRoot, errText string) error
}

// Worker nets + anchors settlement batches.
type Worker struct {
	st      Ledger
	settler chain.Settler
	log     *slog.Logger
	window  time.Duration
	mu      sync.Mutex // serializes settlement passes (window ticker vs force-settle)
}

// New constructs a settlement Worker.
func New(st Ledger, settler chain.Settler, log *slog.Logger, window time.Duration) *Worker {
	if log == nil {
		log = slog.Default()
	}
	if window <= 0 {
		window = 12 * time.Hour
	}
	return &Worker{st: st, settler: settler, log: log, window: window}
}

// Run ticks every window and settles the accumulated micropayment flow.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.window)
	defer t.Stop()
	w.log.Info("settle worker started", "window", w.window.String())
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if id, err := w.SettleNow(ctx); err != nil {
				w.log.Error("window settlement failed", "error", err.Error())
			} else if id != "" {
				w.log.Info("window settlement anchored", "batch", id)
			}
			if root, err := w.SettleWithdrawals(ctx); err != nil {
				w.log.Error("withdrawal settlement failed", "error", err.Error())
			} else if root != "" {
				w.log.Info("withdrawal payout anchored", "payout_root", root)
			}
		}
	}
}

// SettleNow performs one settlement pass over all currently-unsettled transfers:
// build root -> seal batch -> anchor on Paxeer -> mark anchored. Returns the
// batch id (empty when there was nothing to settle). Safe to call concurrently;
// passes are serialized.
func (w *Worker) SettleNow(ctx context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	unsettled, err := w.st.ListUnsettled(ctx, 0)
	if err != nil {
		return "", fmt.Errorf("settle: list unsettled: %w", err)
	}
	if len(unsettled) == 0 {
		return "", nil
	}

	leaves := make([][32]byte, 0, len(unsettled))
	seqs := make([]int64, 0, len(unsettled))
	for _, u := range unsettled {
		raw, err := hex.DecodeString(u.LeafHex)
		if err != nil || len(raw) != 32 {
			return "", fmt.Errorf("settle: bad leaf hex for seq %d", u.Seq)
		}
		var leaf [32]byte
		copy(leaf[:], raw)
		leaves = append(leaves, leaf)
		seqs = append(seqs, u.Seq)
	}
	root := accumulator.Root(leaves)
	rootHex := hex.EncodeToString(root[:])

	now := time.Now().UTC()
	windowStart := now.Add(-w.window)
	batchID, err := w.st.SealBatch(ctx, rootHex, seqs, windowStart, now)
	if err != nil {
		return "", fmt.Errorf("settle: seal batch: %w", err)
	}

	// Compute the per-DID net deltas over the window and resolve each to its
	// mapped EVM address (observability + reserve-reconciliation groundwork;
	// reused for withdrawal payouts in Phase 3). In the fully-reserved model a
	// pure internal transfer moves NO USDL on-chain, so this batch carries no
	// on-chain payouts — the load-bearing action is anchoring the root. The only
	// USDL-moving deltas (withdrawals) are wired into the batch in Phase 3.
	w.logNetDeltas(ctx, unsettled)

	txHash, err := w.settler.AnchorBatch(ctx, rootHex, len(seqs), now, nil)
	if err != nil {
		if mErr := w.st.MarkBatchFailed(ctx, batchID, err.Error()); mErr != nil {
			w.log.Error("mark batch failed", "batch", batchID, "error", mErr.Error())
		}
		return "", fmt.Errorf("settle: anchor batch: %w", err)
	}
	if err := w.st.MarkAnchored(ctx, batchID, txHash); err != nil {
		return "", fmt.Errorf("settle: mark anchored: %w", err)
	}
	w.log.Info("batch anchored", "batch", batchID, "transfers", len(seqs), "root", rootHex, "anchor_tx", txHash)
	return batchID, nil
}

// computeNetDeltas nets a window's transfers per DID: each transfer debits the
// payer and credits the payee. Zero-net DIDs are dropped. Pure (no I/O) so the
// netting math is unit-testable.
func computeNetDeltas(transfers []store.UnsettledTransfer) map[string]int64 {
	net := make(map[string]int64, len(transfers)*2)
	for _, t := range transfers {
		net[t.FromDID] -= t.AmountMicro
		net[t.ToDID] += t.AmountMicro
	}
	for did, v := range net {
		if v == 0 {
			delete(net, did)
		}
	}
	return net
}

// resolveNetDeltas turns the netting map into EVM-addressed deltas, looking up
// each DID's mapped payout address. DIDs ordered for deterministic logging.
func (w *Worker) resolveNetDeltas(ctx context.Context, net map[string]int64) []chain.NetDelta {
	dids := make([]string, 0, len(net))
	for did := range net {
		dids = append(dids, did)
	}
	sort.Strings(dids)
	out := make([]chain.NetDelta, 0, len(dids))
	for _, did := range dids {
		var evm string
		if acct, err := w.st.GetAccount(ctx, did); err == nil {
			evm = acct.EVMAddress
		} else if !errors.Is(err, store.ErrNotFound) {
			w.log.Warn("net delta: account lookup failed", "did", did, "error", err.Error())
		}
		out = append(out, chain.NetDelta{DID: did, EVMAddress: evm, AmountMicro: net[did]})
	}
	return out
}

// logNetDeltas computes + records the window's per-DID net for observability.
func (w *Worker) logNetDeltas(ctx context.Context, transfers []store.UnsettledTransfer) {
	net := computeNetDeltas(transfers)
	if len(net) == 0 {
		return
	}
	deltas := w.resolveNetDeltas(ctx, net)
	unmapped := 0
	for _, d := range deltas {
		if d.AmountMicro > 0 && d.EVMAddress == "" {
			unmapped++
		}
	}
	w.log.Info("window net deltas computed", "accounts", len(deltas), "unmapped_credits", unmapped)
}

// RecoverPending re-submits batches that were sealed but never confirmed
// anchored (e.g. a crash between seal and confirm). AnchorBatch is idempotent on
// the root, so a batch already settled on-chain is reconciled (its real anchor
// tx recovered) rather than double-anchored — honoring at-least-once. Safe to
// call once at startup; serialized against live settlement passes.
func (w *Worker) RecoverPending(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	pending, err := w.st.ListUnanchoredBatches(ctx)
	if err != nil {
		return fmt.Errorf("settle: list unanchored batches: %w", err)
	}
	if len(pending) == 0 {
		return nil
	}
	w.log.Info("recovering unanchored batches", "count", len(pending))
	for _, b := range pending {
		txHash, err := w.settler.AnchorBatch(ctx, b.RootHex, b.TransferCount, b.WindowEnd, nil)
		if err != nil {
			if mErr := w.st.MarkBatchFailed(ctx, b.ID, err.Error()); mErr != nil {
				w.log.Error("recovery mark batch failed", "batch", b.ID, "error", mErr.Error())
			}
			w.log.Error("batch recovery anchor failed", "batch", b.ID, "error", err.Error())
			continue
		}
		if err := w.st.MarkAnchored(ctx, b.ID, txHash); err != nil {
			w.log.Error("recovery mark anchored failed", "batch", b.ID, "error", err.Error())
			continue
		}
		w.log.Info("recovered batch anchored", "batch", b.ID, "anchor_tx", txHash)
	}
	return nil
}

// ─── withdrawal payout (chain-out, PHASE 3) ──────────────────────────────────

// withdrawalLeafDomain version-prefixes the withdrawal commitment leaf so a
// format change forces a clean re-derivation (mirrors accumulator.LeafDomain).
const withdrawalLeafDomain = "layerx.withdrawal.payout.v1"

// withdrawalLeaf is the deterministic, byte-stable commitment over one
// withdrawal's (id, recipient, amount). Domain-separated SHA-256 so the payout
// root is independently re-derivable + tamper-evident.
func withdrawalLeaf(id, evm string, amountMicro int64) [32]byte {
	h := sha256.New()
	h.Write([]byte(withdrawalLeafDomain))
	h.Write([]byte(id))
	h.Write([]byte{0})
	h.Write([]byte(strings.ToLower(evm)))
	h.Write([]byte{0})
	var u8 [8]byte
	binary.BigEndian.PutUint64(u8[:], uint64(amountMicro))
	h.Write(u8[:])
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// buildWithdrawalGroup turns a payable withdrawal set into the on-chain inputs:
// it sorts by id (deterministic leaf order), computes the payout root, and
// produces the seal items + chain deltas. Pure (no I/O), so the commitment +
// idempotency are unit-testable. Returns ("", ...) when there is nothing
// payable.
func buildWithdrawalGroup(payable []store.QueuedWithdrawal) (rootHex string, items []store.SealedWithdrawal, deltas []chain.NetDelta) {
	if len(payable) == 0 {
		return "", nil, nil
	}
	sorted := make([]store.QueuedWithdrawal, len(payable))
	copy(sorted, payable)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].ID < sorted[j].ID })

	leaves := make([][32]byte, 0, len(sorted))
	items = make([]store.SealedWithdrawal, 0, len(sorted))
	deltas = make([]chain.NetDelta, 0, len(sorted))
	for _, q := range sorted {
		leaves = append(leaves, withdrawalLeaf(q.ID, q.EVMAddress, q.AmountMicro))
		items = append(items, store.SealedWithdrawal{ID: q.ID, EVMAddress: q.EVMAddress})
		deltas = append(deltas, chain.NetDelta{DID: q.DID, EVMAddress: q.EVMAddress, AmountMicro: q.AmountMicro})
	}
	root := accumulator.Root(leaves)
	return hex.EncodeToString(root[:]), items, deltas
}

// SettleWithdrawals pays out queued withdrawals as real USDL on Paxeer: it
// seals the payable set under a deterministic payout root (freezing the
// recipients), submits the operator-signed vault payout, and marks each
// withdrawal settled with the on-chain tx — only after it confirms. Returns the
// payout root (empty when there was nothing payable). Idempotent + crash-safe:
// the frozen root makes the on-chain batch id stable, so a retried submit never
// double-pays. Withdrawals whose DID has no mapped EVM address are skipped (left
// queued) until a binding exists. Safe to call concurrently; serialized.
func (w *Worker) SettleWithdrawals(ctx context.Context) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	queued, err := w.st.ListQueuedWithdrawals(ctx, 0)
	if err != nil {
		return "", fmt.Errorf("settle: list queued withdrawals: %w", err)
	}
	if len(queued) == 0 {
		return "", nil
	}

	payable := make([]store.QueuedWithdrawal, 0, len(queued))
	for _, q := range queued {
		if q.EVMAddress == "" {
			w.log.Warn("withdrawal has no mapped EVM payout address; leaving queued", "withdrawal", q.ID, "did", q.DID)
			continue
		}
		if q.SwapOut != "" {
			// swap-out withdrawals are a [deferred] feature (the vault settle path
			// releases USDL only); pay USDL and surface the unhonored choice.
			w.log.Warn("withdrawal swap_out not yet supported on-chain; paying USDL", "withdrawal", q.ID, "swap_out", q.SwapOut)
		}
		payable = append(payable, q)
	}
	if len(payable) == 0 {
		return "", nil
	}

	rootHex, items, deltas := buildWithdrawalGroup(payable)
	if err := w.st.SealWithdrawals(ctx, rootHex, items); err != nil {
		return "", fmt.Errorf("settle: seal withdrawals: %w", err)
	}

	now := time.Now().UTC()
	txHash, err := w.settler.AnchorBatch(ctx, rootHex, len(items), now, deltas)
	if err != nil {
		if mErr := w.st.RecordWithdrawalError(ctx, rootHex, err.Error()); mErr != nil {
			w.log.Error("record withdrawal error", "payout_root", rootHex, "error", mErr.Error())
		}
		return "", fmt.Errorf("settle: anchor withdrawal payout: %w", err)
	}
	for _, it := range items {
		if err := w.st.MarkWithdrawalSettled(ctx, it.ID, txHash); err != nil {
			return "", fmt.Errorf("settle: mark withdrawal settled: %w", err)
		}
	}
	w.log.Info("withdrawals paid out", "count", len(items), "payout_root", rootHex, "payout_tx", txHash)
	return rootHex, nil
}

// RecoverPendingWithdrawals re-submits withdrawal payouts that were sealed
// (queued -> submitted) but never confirmed settled (e.g. a crash between seal
// and confirm). It groups the submitted rows by their frozen payout root,
// re-derives the SAME root from the frozen recipients/amounts (a mismatch is a
// data-integrity bug and is skipped rather than paid), and re-anchors —
// idempotent on the root, so an already-settled payout is reconciled (its real
// tx recovered) rather than double-paid. Safe to call once at startup.
func (w *Worker) RecoverPendingWithdrawals(ctx context.Context) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	submitted, err := w.st.ListSubmittedWithdrawals(ctx)
	if err != nil {
		return fmt.Errorf("settle: list submitted withdrawals: %w", err)
	}
	if len(submitted) == 0 {
		return nil
	}

	groups := make(map[string][]store.QueuedWithdrawal)
	order := make([]string, 0)
	for _, q := range submitted {
		if _, seen := groups[q.PayoutRoot]; !seen {
			order = append(order, q.PayoutRoot)
		}
		groups[q.PayoutRoot] = append(groups[q.PayoutRoot], q)
	}
	w.log.Info("recovering submitted withdrawal payouts", "groups", len(order))

	for _, root := range order {
		group := groups[root]
		derivedRoot, items, deltas := buildWithdrawalGroup(group)
		if derivedRoot != root {
			w.log.Error("withdrawal recovery root mismatch; skipping group (data integrity)", "payout_root", root, "derived", derivedRoot)
			continue
		}
		now := time.Now().UTC()
		txHash, err := w.settler.AnchorBatch(ctx, root, len(items), now, deltas)
		if err != nil {
			if mErr := w.st.RecordWithdrawalError(ctx, root, err.Error()); mErr != nil {
				w.log.Error("recovery record withdrawal error", "payout_root", root, "error", mErr.Error())
			}
			w.log.Error("withdrawal payout recovery failed", "payout_root", root, "error", err.Error())
			continue
		}
		for _, it := range items {
			if err := w.st.MarkWithdrawalSettled(ctx, it.ID, txHash); err != nil {
				w.log.Error("recovery mark withdrawal settled failed", "withdrawal", it.ID, "error", err.Error())
			}
		}
		w.log.Info("recovered withdrawal payout", "payout_root", root, "payout_tx", txHash, "count", len(items))
	}
	return nil
}
