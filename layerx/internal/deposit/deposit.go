// Package deposit runs the chain-in watcher (layerx.frozen.kvx
// [topology.direction_chain_in]): it polls the Vault Deposit events, resolves
// each event's DID claim, and credits the off-chain USDX balance exactly once
// (idempotent on the deposit tx + log index, with a durable block cursor so
// restarts resume without re-crediting). It also runs the reserve-reconciliation
// check for invariant i1 (circulating USDX == USDL held in the vault).
package deposit

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"time"

	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/store"
)

// cursorName is the chain_cursors row the deposit watcher persists its progress
// under.
const cursorName = "deposit_watcher"

// maxBlockRange bounds a single FilterLogs span so a large backlog is scanned in
// chunks (RPC providers cap log-range queries). The Paxeer chain-125 RPC
// (https://api.hyperpax.xyz) caps eth_getLogs at 2000 blocks per query, so a
// chunk spans exactly that many blocks ([start, start+2000-1] inclusive = 2000).
const maxBlockRange = uint64(2000)

// Source is the chain-side reader the worker needs. *chain.Watcher satisfies it;
// a fake satisfies it in tests (no live RPC).
type Source interface {
	HeadBlock(ctx context.Context) (uint64, error)
	FilterDeposits(ctx context.Context, from, to uint64) ([]chain.DepositEvent, error)
	ReserveBalance(ctx context.Context) (*big.Int, error)
}

// Ledger is the store subset the worker needs.
type Ledger interface {
	GetCursor(ctx context.Context, name string) (int64, bool, error)
	SetCursor(ctx context.Context, name string, block int64) error
	ResolveDIDClaim(ctx context.Context, claim string) (string, error)
	CreditDeposit(ctx context.Context, did, evm, depositTx string, amountMicro int64) error
	CirculatingUSDX(ctx context.Context) (int64, error)
}

// Worker polls deposits and credits the ledger.
type Worker struct {
	src           Source
	st            Ledger
	log           *slog.Logger
	pollInterval  time.Duration
	startBlock    uint64
	confirmations uint64
}

// New constructs a deposit Worker. startBlock is the Vault deploy block (where to
// begin a first scan); confirmations is the block lag before a head block is
// treated as final (Paxeer has instant CometBFT finality, so 0 is correct, but
// it is configurable for safety).
func New(src Source, st Ledger, log *slog.Logger, pollInterval time.Duration, startBlock, confirmations uint64) *Worker {
	if log == nil {
		log = slog.Default()
	}
	if pollInterval <= 0 {
		pollInterval = 15 * time.Second
	}
	return &Worker{
		src:           src,
		st:            st,
		log:           log,
		pollInterval:  pollInterval,
		startBlock:    startBlock,
		confirmations: confirmations,
	}
}

// Run polls on an interval until ctx is cancelled, syncing deposits then
// reconciling the reserve each tick.
func (w *Worker) Run(ctx context.Context) {
	t := time.NewTicker(w.pollInterval)
	defer t.Stop()
	w.log.Info("deposit watcher started", "poll", w.pollInterval.String(), "start_block", w.startBlock, "confirmations", w.confirmations)
	// One immediate pass so a freshly-started daemon catches up without waiting a
	// full interval.
	if err := w.Sync(ctx); err != nil {
		w.log.Error("deposit sync failed", "error", err.Error())
	}
	w.reconcile(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := w.Sync(ctx); err != nil {
				w.log.Error("deposit sync failed", "error", err.Error())
			}
			w.reconcile(ctx)
		}
	}
}

// Sync processes all newly-confirmed Deposit events up to the safe head,
// advancing the durable cursor per chunk so partial progress survives a crash.
func (w *Worker) Sync(ctx context.Context) error {
	cursor, ok, err := w.st.GetCursor(ctx, cursorName)
	if err != nil {
		return err
	}
	var from uint64
	if ok {
		from = uint64(cursor) + 1
	} else {
		from = w.startBlock
	}

	head, err := w.src.HeadBlock(ctx)
	if err != nil {
		return err
	}
	if head < w.confirmations {
		return nil // chain younger than the confirmation depth; nothing final yet
	}
	safeHead := head - w.confirmations
	if safeHead < from {
		return nil // nothing new to process
	}

	for start := from; start <= safeHead; start += maxBlockRange {
		end := start + maxBlockRange - 1
		if end > safeHead {
			end = safeHead
		}
		events, err := w.src.FilterDeposits(ctx, start, end)
		if err != nil {
			return fmt.Errorf("deposit: filter [%d,%d]: %w", start, end, err)
		}
		for _, ev := range events {
			if err := w.credit(ctx, ev); err != nil {
				return fmt.Errorf("deposit: credit %s#%d: %w", ev.TxHash, ev.LogIndex, err)
			}
		}
		if err := w.st.SetCursor(ctx, cursorName, int64(end)); err != nil {
			return fmt.Errorf("deposit: set cursor %d: %w", end, err)
		}
	}
	return nil
}

// credit resolves the DID claim and credits the deposit exactly once. An
// unresolved claim (the agent never called GET /v1/deposit) is logged and
// skipped — it cannot be attributed to a DID, and re-scanning is avoided by the
// advancing cursor; a zero/over-range mint is also skipped defensively.
func (w *Worker) credit(ctx context.Context, ev chain.DepositEvent) error {
	did, err := w.st.ResolveDIDClaim(ctx, ev.ClaimHex)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			w.log.Warn("unresolved deposit claim; skipping (DID never registered via /v1/deposit)",
				"claim", ev.ClaimHex, "depositor", ev.Depositor.Hex(), "tx", ev.TxHash)
			return nil
		}
		return err
	}
	if ev.USDXMinted == nil || ev.USDXMinted.Sign() <= 0 || !ev.USDXMinted.IsInt64() {
		w.log.Warn("deposit with non-creditable mint amount; skipping",
			"did", did, "tx", ev.TxHash, "minted", ev.USDXMinted)
		return nil
	}
	// Idempotency key is tx hash + log index so multiple Deposit events in one tx
	// each credit independently (CreditDeposit is idempotent on this key).
	depositKey := fmt.Sprintf("%s:%d", ev.TxHash, ev.LogIndex)
	if err := w.st.CreditDeposit(ctx, did, ev.Depositor.Hex(), depositKey, ev.USDXMinted.Int64()); err != nil {
		return err
	}
	w.log.Info("deposit credited", "did", did, "evm", ev.Depositor.Hex(),
		"micro_usdx", ev.USDXMinted.Int64(), "tx", ev.TxHash, "block", ev.Block)
	return nil
}

// reconcile reads the on-chain reserve and the off-chain circulating supply and
// logs any drift (invariant i1). It is diagnostic only — transient drift is
// expected while a deposit is mid-confirmation — so it never fails the loop.
func (w *Worker) reconcile(ctx context.Context) {
	reserve, err := w.src.ReserveBalance(ctx)
	if err != nil {
		w.log.Warn("reserve reconciliation: reserve read failed", "error", err.Error())
		return
	}
	circulating, err := w.st.CirculatingUSDX(ctx)
	if err != nil {
		w.log.Warn("reserve reconciliation: circulating read failed", "error", err.Error())
		return
	}
	reserveMicro := reserve
	circ := big.NewInt(circulating)
	drift := new(big.Int).Sub(circ, reserveMicro)
	if drift.Sign() == 0 {
		w.log.Info("reserve reconciled", "circulating_micro_usdx", circulating, "reserve_micro_usdl", reserveMicro.String())
		return
	}
	w.log.Warn("reserve drift detected (i1)",
		"circulating_micro_usdx", circulating,
		"reserve_micro_usdl", reserveMicro.String(),
		"drift_micro", drift.String())
}
