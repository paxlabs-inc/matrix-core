// Package settle runs the tiered settlement worker (layerx.frozen.kvx
// [settlement]): on each window it nets the open transfers, builds the batch
// Merkle root, seals the batch, anchors the root on Paxeer, and records the
// anchor — at-least-once (a batch is marked anchored only after the chain
// returns). SettleNow also backs the on-demand force-settle path.
package settle

import (
	"context"
	"encoding/hex"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/store"
)

// Worker nets + anchors settlement batches.
type Worker struct {
	st      *store.Store
	settler chain.Settler
	log     *slog.Logger
	window  time.Duration
	mu      sync.Mutex // serializes settlement passes (window ticker vs force-settle)
}

// New constructs a settlement Worker.
func New(st *store.Store, settler chain.Settler, log *slog.Logger, window time.Duration) *Worker {
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

	// Net deltas are computed off the window for the on-chain payout/netting.
	// In the fully-reserved model pure internal transfers net within the vault;
	// the load-bearing on-chain action is anchoring the root. Withdrawal payout
	// netting is a [deferred] next step.
	txHash, err := w.settler.AnchorBatch(ctx, rootHex, len(seqs), nil)
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
