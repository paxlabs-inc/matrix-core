package engine

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"time"

	"github.com/paxlabs-inc/layerx/internal/accumulator"
	"github.com/paxlabs-inc/layerx/internal/chain"
	"github.com/paxlabs-inc/layerx/internal/store"
)

const perpEventLeafDomain = "layerx.perps.event.v1"

// PerpEventLeaf hashes one journal row into its Merkle leaf: a domain-prefixed
// digest over the gap-free seq, event type, owner, and payload hash, so any
// party holding the journal re-derives the identical root.
func PerpEventLeaf(e store.PerpEvent) [32]byte {
	payloadSum := sha256.Sum256(e.Payload)
	var b []byte
	b = append(b, []byte(perpEventLeafDomain)...)
	var u8 [8]byte
	binary.BigEndian.PutUint64(u8[:], uint64(e.Seq))
	b = append(b, u8[:]...)
	appendStr := func(s string) {
		var u4 [4]byte
		binary.BigEndian.PutUint32(u4[:], uint32(len(s)))
		b = append(b, u4[:]...)
		b = append(b, s...)
	}
	appendStr(e.EventType)
	appendStr(e.OwnerDID)
	b = append(b, payloadSum[:]...)
	return sha256.Sum256(b)
}

// PerpJournalRoot computes the Merkle root over an ordered journal slice.
func PerpJournalRoot(events []store.PerpEvent) [32]byte {
	leaves := make([][32]byte, len(events))
	for i, e := range events {
		leaves[i] = PerpEventLeaf(e)
	}
	return accumulator.Root(leaves)
}

// AnchorPerpJournalOnce seals every un-anchored journal row into one perps
// batch (a SEPARATE Merkle root from the transfer settlement tree) and anchors
// it through the existing SettlementAnchor settler. Returns the batch id, or
// "" when there is nothing new to anchor.
func (e *Engine) AnchorPerpJournalOnce(ctx context.Context, settler chain.Settler) (string, error) {
	last, err := e.Store.LastAnchoredPerpEventSeq(ctx)
	if err != nil {
		return "", err
	}
	events, err := e.Store.ListPerpJournal(ctx, last, 10_000)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", nil
	}
	root := PerpJournalRoot(events)
	lo := events[0].Seq
	hi := events[len(events)-1].Seq
	batchID, err := e.Store.SealPerpBatch(ctx, hex.EncodeToString(root[:]), lo, hi)
	if err != nil {
		return "", err
	}
	txHash, err := settler.AnchorBatch(ctx, hex.EncodeToString(root[:]), len(events), time.Now().UTC(), nil)
	if err != nil {
		if merr := e.Store.MarkPerpBatchFailed(ctx, batchID, err.Error()); merr != nil {
			return batchID, merr
		}
		return batchID, err
	}
	if err := e.Store.MarkPerpBatchAnchored(ctx, batchID, txHash); err != nil {
		return batchID, err
	}
	return batchID, nil
}

// RecoverPerpBatches re-anchors every sealed/submitted/failed perps batch
// (at-least-once after a crash), re-deriving each root from the journal.
func (e *Engine) RecoverPerpBatches(ctx context.Context, settler chain.Settler) error {
	pending, err := e.Store.ListUnanchoredPerpBatches(ctx)
	if err != nil {
		return err
	}
	for _, b := range pending {
		txHash, err := settler.AnchorBatch(ctx, b.RootHex, int(b.EventSeqHi-b.EventSeqLo+1), b.CreatedAt, nil)
		if err != nil {
			if merr := e.Store.MarkPerpBatchFailed(ctx, b.ID, err.Error()); merr != nil {
				return merr
			}
			continue
		}
		if err := e.Store.MarkPerpBatchAnchored(ctx, b.ID, txHash); err != nil {
			return err
		}
	}
	return nil
}
