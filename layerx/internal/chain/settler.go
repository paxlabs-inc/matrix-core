package chain

import (
	"context"
	"fmt"
	"math/big"
	"sort"
	"time"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	gethtypes "github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
)

// batchIDDomain namespaces the deterministic on-chain batch id derived from a
// batch's Merkle root. Deriving the id from the root makes AnchorBatch
// idempotent on rootHex: a retried submit of the same root maps to the same
// batchId, which the vault's settledBatch / anchor's anchored maps reject a
// second time.
const batchIDDomain = "layerx-settlement-batch:v1"

// PaxeerSettler is the real, on-chain implementation of Settler. It holds the
// Paxeer client, the operator key, and the parsed contract bindings needed to
// submit an operator-signed LayerXVault.settle batch (which records the root on
// the SettlementAnchor) on chain 125.
//
// It builds the operator-signed settle calldata, sends the EIP-1559 tx, waits
// for CometBFT finality (Paxeer has no reorgs), and returns the real tx hash.
// It is idempotent on the batch root: a retried submit of an already-settled
// batch does not double-anchor — it recovers and returns the original anchor tx.
type PaxeerSettler struct {
	client   *Client
	operator *Operator
	bind     *bindings
	vault    common.Address
	anchor   common.Address
	chainID  *big.Int
}

// NewPaxeerSettler wires the real settler. vaultAddr is the LayerXVault (the
// operator calls settle there); anchorAddr is the SettlementAnchor (read-back of
// the recorded root). Both must be non-empty valid addresses.
func NewPaxeerSettler(c *Client, op *Operator, vaultAddr, anchorAddr string) (*PaxeerSettler, error) {
	if c == nil || c.Eth() == nil {
		return nil, fmt.Errorf("chain: nil client")
	}
	if op == nil {
		return nil, fmt.Errorf("chain: nil operator")
	}
	if !common.IsHexAddress(vaultAddr) {
		return nil, fmt.Errorf("chain: invalid vault address %q", vaultAddr)
	}
	if !common.IsHexAddress(anchorAddr) {
		return nil, fmt.Errorf("chain: invalid anchor address %q", anchorAddr)
	}
	b, err := loadBindings()
	if err != nil {
		return nil, err
	}
	return &PaxeerSettler{
		client:   c,
		operator: op,
		bind:     b,
		vault:    common.HexToAddress(vaultAddr),
		anchor:   common.HexToAddress(anchorAddr),
		chainID:  c.ChainID(),
	}, nil
}

// OperatorAddress returns the EVM address that signs settlement txs (safe to log).
func (p *PaxeerSettler) OperatorAddress() common.Address { return p.operator.Address() }

// transactor builds a keyed EIP-1559 transactor bound to the operator key and
// chain id. Used by AnchorBatch in Phase 2.
func (p *PaxeerSettler) transactor(ctx context.Context) (*bind.TransactOpts, error) {
	auth, err := bind.NewKeyedTransactorWithChainID(p.operator.Key(), p.chainID)
	if err != nil {
		return nil, fmt.Errorf("chain: transactor: %w", err)
	}
	auth.Context = ctx
	return auth, nil
}

// AnchorBatch submits the sealed batch's root (and any per-account on-chain
// payouts) to chain 125 via the operator-only LayerXVault.settle, which records
// the root on the SettlementAnchor. It is idempotent on rootHex: if the derived
// batch was already settled on-chain, it returns the original anchor tx instead
// of re-submitting (which would revert BatchAlreadySettled). The batch is only
// reported anchored after the settle tx confirms successfully.
//
// In the fully-reserved model pure internal pays move no USDL, so a normal
// settlement window carries no payouts (deltas == nil) and only anchors the
// root; withdrawal payouts (the only USDL-moving deltas) are supplied by the
// caller in the deltas slice.
func (p *PaxeerSettler) AnchorBatch(ctx context.Context, rootHex string, transferCount int, windowEnd time.Time, deltas []NetDelta) (string, error) {
	batch, err := p.buildBatch(rootHex, windowEnd, deltas)
	if err != nil {
		return "", err
	}

	// Idempotency: never double-anchor a root. If the contract already recorded
	// this batch, recover and return the original anchor tx hash.
	settled, err := p.alreadySettled(ctx, batch.BatchId)
	if err != nil {
		return "", fmt.Errorf("chain: settle idempotency check: %w", err)
	}
	if settled {
		return p.findAnchorTx(ctx, batch.BatchId)
	}

	auth, err := p.transactor(ctx)
	if err != nil {
		return "", err
	}
	eth := p.client.Eth()
	bound := bind.NewBoundContract(p.vault, p.bind.vault, eth, eth, eth)
	tx, err := bound.Transact(auth, "settle", batch)
	if err != nil {
		return "", fmt.Errorf("chain: settle tx: %w", err)
	}
	receipt, err := bind.WaitMined(ctx, eth, tx)
	if err != nil {
		return "", fmt.Errorf("chain: settle wait: %w", err)
	}
	if receipt.Status != gethtypes.ReceiptStatusSuccessful {
		return "", fmt.Errorf("chain: settle reverted (tx %s)", tx.Hash().Hex())
	}
	return tx.Hash().Hex(), nil
}

// buildBatch assembles the on-chain Batch for a sealed window: it decodes the
// (non-zero) root, derives the deterministic batchId, and turns the positive
// net deltas into payouts (recipient = mapped EVM address). It is a pure
// function (no I/O), so the batchId determinism + payout building are unit
// testable without a chain.
func (p *PaxeerSettler) buildBatch(rootHex string, windowEnd time.Time, deltas []NetDelta) (Batch, error) {
	root, err := decodeRoot32(rootHex)
	if err != nil {
		return Batch{}, err
	}
	if root == ([32]byte{}) {
		return Batch{}, fmt.Errorf("chain: refusing to anchor a zero root")
	}
	payouts, err := buildPayouts(deltas)
	if err != nil {
		return Batch{}, err
	}
	var we uint64
	if !windowEnd.IsZero() && windowEnd.Unix() > 0 {
		we = uint64(windowEnd.Unix())
	}
	return Batch{
		BatchId:   batchIDForRoot(root),
		Root:      root,
		WindowEnd: we,
		Payouts:   payouts,
	}, nil
}

// batchIDForRoot derives the deterministic on-chain batch id from the root.
func batchIDForRoot(root [32]byte) [32]byte {
	return [32]byte(crypto.Keccak256Hash([]byte(batchIDDomain), root[:]))
}

// decodeRoot32 parses a 32-byte Merkle root from hex (with or without 0x).
func decodeRoot32(rootHex string) ([32]byte, error) {
	var root [32]byte
	b := common.FromHex("0x" + trim0x(rootHex))
	if len(b) != 32 {
		return root, fmt.Errorf("chain: root must be 32 bytes, got %d", len(b))
	}
	copy(root[:], b)
	return root, nil
}

func trim0x(s string) string {
	if len(s) >= 2 && (s[:2] == "0x" || s[:2] == "0X") {
		return s[2:]
	}
	return s
}

// buildPayouts converts positive net deltas into on-chain payouts (USDL owed to
// the mapped EVM recipient). Zero/negative deltas carry no on-chain payout.
// Payouts are sorted by recipient for deterministic calldata. An unmapped or
// invalid EVM address on a positive delta is a hard error (we never pay a
// zero/garbage address).
func buildPayouts(deltas []NetDelta) ([]Payout, error) {
	if len(deltas) == 0 {
		return nil, nil
	}
	out := make([]Payout, 0, len(deltas))
	for _, d := range deltas {
		if d.AmountMicro <= 0 {
			continue
		}
		if !common.IsHexAddress(d.EVMAddress) {
			return nil, fmt.Errorf("chain: net delta for %s has no valid EVM payout address", d.DID)
		}
		out = append(out, Payout{
			Recipient: common.HexToAddress(d.EVMAddress),
			Amount:    big.NewInt(d.AmountMicro),
		})
	}
	if len(out) == 0 {
		return nil, nil
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].Recipient.Cmp(out[j].Recipient) < 0
	})
	return out, nil
}

// alreadySettled reads the vault's settledBatch idempotency flag for batchId.
func (p *PaxeerSettler) alreadySettled(ctx context.Context, batchID [32]byte) (bool, error) {
	eth := p.client.Eth()
	bound := bind.NewBoundContract(p.vault, p.bind.vault, eth, eth, eth)
	var out []interface{}
	if err := bound.Call(&bind.CallOpts{Context: ctx}, &out, "settledBatch", batchID); err != nil {
		return false, fmt.Errorf("chain: read settledBatch: %w", err)
	}
	if len(out) == 0 {
		return false, fmt.Errorf("chain: settledBatch returned no data")
	}
	b, ok := out[0].(bool)
	if !ok {
		return false, fmt.Errorf("chain: settledBatch wrong type %T", out[0])
	}
	return b, nil
}

// findAnchorTx recovers the original anchor tx hash for an already-settled batch
// by filtering the SettlementAnchored event (batchId is its first indexed
// topic). Used on the idempotent path so receipts still expose a real anchor tx.
func (p *PaxeerSettler) findAnchorTx(ctx context.Context, batchID [32]byte) (string, error) {
	ev, ok := p.bind.anchor.Events["SettlementAnchored"]
	if !ok {
		return "", fmt.Errorf("chain: anchor abi missing SettlementAnchored event")
	}
	q := ethereum.FilterQuery{
		Addresses: []common.Address{p.anchor},
		Topics:    [][]common.Hash{{ev.ID}, {common.BytesToHash(batchID[:])}},
	}
	logs, err := p.client.Eth().FilterLogs(ctx, q)
	if err != nil {
		return "", fmt.Errorf("chain: filter anchor logs: %w", err)
	}
	if len(logs) == 0 {
		return "", fmt.Errorf("chain: batch already settled on-chain but no SettlementAnchored log found")
	}
	return logs[0].TxHash.Hex(), nil
}

// PaxeerSettler must satisfy the Settler interface.
var _ Settler = (*PaxeerSettler)(nil)
