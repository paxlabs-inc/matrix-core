// Package chain abstracts the Paxeer (chain 125) interactions: anchoring a
// sealed batch's Merkle root on the SettlementAnchor contract, and (later)
// watching the Vault for Deposit events. v1 ships a DevSettler so the off-chain
// ledger + receipt path runs end-to-end without a live chain; the real settler
// (deus/internal/chain Payer pattern: EIP-1559 tx to SettlementAnchor.anchor)
// is a [deferred] next step in the frozen spec.
package chain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
)

// NetDelta is a per-DID net settlement amount (micro-USDX; positive = net
// credit owed to the agent, negative = net debit) resolved to an EVM payout
// address. Carried to the on-chain settlement for batched payout/netting.
type NetDelta struct {
	DID         string
	EVMAddress  string
	AmountMicro int64
}

// Settler submits a sealed batch's commitment to Paxeer and returns the anchor
// tx hash. Implementations MUST be idempotent on rootHex (a retried submit of
// the same root must not double-anchor).
type Settler interface {
	AnchorBatch(ctx context.Context, rootHex string, transferCount int, deltas []NetDelta) (txHash string, err error)
}

// DevSettler is an offline stand-in: it derives a deterministic pseudo tx hash
// from the batch root so the settlement pipeline completes without a chain.
// NOT for production — it performs no on-chain action.
type DevSettler struct{}

// NewDevSettler returns the offline settler.
func NewDevSettler() *DevSettler { return &DevSettler{} }

// AnchorBatch returns a deterministic fake tx hash (sha256 of the root) so dev
// flows can mark a batch anchored.
func (d *DevSettler) AnchorBatch(_ context.Context, rootHex string, _ int, _ []NetDelta) (string, error) {
	sum := sha256.Sum256([]byte("dev-anchor:" + rootHex))
	return "0x" + hex.EncodeToString(sum[:]), nil
}
