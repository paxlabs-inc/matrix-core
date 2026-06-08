// Package indexer mirrors ServiceRegistry chain events into Postgres.
package indexer

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/paxlabs-inc/deus/internal/chain"
	"github.com/paxlabs-inc/deus/internal/chain/bindings"
	"github.com/paxlabs-inc/deus/internal/store"
)

// Indexer tails ServiceRegistry logs into the mirror tables.
type Indexer struct {
	registry *chain.Registry
	store    *store.Store
}

// New returns an Indexer.
func New(reg *chain.Registry, st *store.Store) *Indexer {
	return &Indexer{registry: reg, store: st}
}

// Sync processes ServiceRegistered events from fromBlock (inclusive) and advances the cursor.
func (ix *Indexer) Sync(ctx context.Context, fromBlock int64) error {
	events, err := ix.registry.FilterRegistered(ctx, fromBlock)
	if err != nil {
		return err
	}
	for i := range events {
		if err := ix.applyRegistered(ctx, events[i]); err != nil {
			return err
		}
	}
	if len(events) > 0 {
		last := events[len(events)-1]
		if err := ix.store.SetIndexCursor(ctx, store.IndexCursor{
			LastBlock:    int64(last.Raw.BlockNumber),
			LastLogIndex: int(last.Raw.Index),
		}); err != nil {
			return err
		}
	}
	return nil
}

// ReplayFrom replays from genesis block 0 for mirror-rebuild tests.
func (ix *Indexer) ReplayFrom(ctx context.Context, fromBlock int64) error {
	return ix.Sync(ctx, fromBlock)
}

func (ix *Indexer) applyRegistered(ctx context.Context, ev bindings.ServiceRegistryServiceRegistered) error {
	manifestHash := "0x" + hex.EncodeToString(ev.ManifestHash[:])
	owner := strings.ToLower(ev.Owner.Hex())
	_, err := ix.store.UpsertFromChainEvent(
		ctx,
		int64(ev.Id.Uint64()),
		owner,
		manifestHash,
		"0x"+hex.EncodeToString(ev.PricingHash[:]),
		ev.Hosted,
		ev.Confidential,
		json.RawMessage(`{}`),
	)
	if err != nil {
		return fmt.Errorf("indexer: upsert service %d: %w", ev.Id.Uint64(), err)
	}
	return nil
}

// MirrorCount returns active services with non-null chain_id (test helper).
func MirrorCount(ctx context.Context, st *store.Store) (int, error) {
	var n int
	err := st.Pool().QueryRow(ctx, `SELECT COUNT(1) FROM services WHERE chain_id IS NOT NULL`).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("indexer: mirror count: %w", err)
	}
	return n, nil
}
