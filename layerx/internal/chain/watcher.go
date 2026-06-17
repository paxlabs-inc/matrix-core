package chain

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/big"

	ethereum "github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/crypto"
)

// DIDClaimHex is the canonical on-chain deposit claim for a DID: the lowercase
// 64-hex keccak256 of the DID string (no 0x prefix). The depositor passes this
// (as the Vault Deposit `did` bytes32) so the watcher — which only sees the
// hash, keccak being one-way — can reverse it via the did_claims registry that
// the agent populated when it fetched GET /v1/deposit.
func DIDClaimHex(did string) string {
	h := crypto.Keccak256([]byte(did))
	return hex.EncodeToString(h)
}

// DepositEvent is a decoded Vault Deposit log: the chain-in signal that mints an
// off-chain USDX credit for the claiming DID.
type DepositEvent struct {
	ClaimHex   string         // hex(keccak256(did)) — the registry key
	Depositor  common.Address // msg.sender; the DID's mapped EVM payout address
	TokenIn    common.Address // deposited asset (USDL/USDC/USDT/WPAX9; zero = native PAX)
	AmountIn   *big.Int       // raw input amount
	USDXMinted *big.Int       // USDL actually received == micro-USDX to credit
	TxHash     string
	LogIndex   uint
	Block      uint64
}

// Watcher reads Vault Deposit events and the on-chain reserve from chain 125. It
// is a thin chain-side reader with no store dependency; the deposit worker
// (internal/deposit) wires it to the ledger.
type Watcher struct {
	client *Client
	bind   *bindings
	vault  common.Address
}

// NewWatcher constructs a deposit watcher bound to the Vault address.
func NewWatcher(c *Client, vaultAddr string) (*Watcher, error) {
	if c == nil || c.Eth() == nil {
		return nil, fmt.Errorf("chain: nil client")
	}
	if !common.IsHexAddress(vaultAddr) {
		return nil, fmt.Errorf("chain: invalid vault address %q", vaultAddr)
	}
	b, err := loadBindings()
	if err != nil {
		return nil, err
	}
	return &Watcher{client: c, bind: b, vault: common.HexToAddress(vaultAddr)}, nil
}

// HeadBlock returns the current chain head block number.
func (w *Watcher) HeadBlock(ctx context.Context) (uint64, error) {
	n, err := w.client.Eth().BlockNumber(ctx)
	if err != nil {
		return 0, fmt.Errorf("chain: head block: %w", err)
	}
	return n, nil
}

// FilterDeposits returns the Vault Deposit events in [from, to] (inclusive),
// decoded into DepositEvents. Indexed args (did, depositor, tokenIn) come from
// the log topics; the amounts come from the data section.
func (w *Watcher) FilterDeposits(ctx context.Context, from, to uint64) ([]DepositEvent, error) {
	ev, ok := w.bind.vault.Events["Deposit"]
	if !ok {
		return nil, fmt.Errorf("chain: vault abi missing Deposit event")
	}
	q := ethereum.FilterQuery{
		FromBlock: new(big.Int).SetUint64(from),
		ToBlock:   new(big.Int).SetUint64(to),
		Addresses: []common.Address{w.vault},
		Topics:    [][]common.Hash{{ev.ID}},
	}
	logs, err := w.client.Eth().FilterLogs(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("chain: filter deposit logs: %w", err)
	}
	out := make([]DepositEvent, 0, len(logs))
	for _, lg := range logs {
		if lg.Removed {
			continue
		}
		if len(lg.Topics) < 4 {
			return nil, fmt.Errorf("chain: deposit log %s#%d has %d topics, want 4", lg.TxHash.Hex(), lg.Index, len(lg.Topics))
		}
		var data struct {
			AmountIn   *big.Int
			UsdxMinted *big.Int
		}
		if err := w.bind.vault.UnpackIntoInterface(&data, "Deposit", lg.Data); err != nil {
			return nil, fmt.Errorf("chain: unpack deposit %s#%d: %w", lg.TxHash.Hex(), lg.Index, err)
		}
		out = append(out, DepositEvent{
			ClaimHex:   hex.EncodeToString(lg.Topics[1].Bytes()),
			Depositor:  common.BytesToAddress(lg.Topics[2].Bytes()),
			TokenIn:    common.BytesToAddress(lg.Topics[3].Bytes()),
			AmountIn:   data.AmountIn,
			USDXMinted: data.UsdxMinted,
			TxHash:     lg.TxHash.Hex(),
			LogIndex:   lg.Index,
			Block:      lg.BlockNumber,
		})
	}
	return out, nil
}

// ReserveBalance reads the Vault's USDL reserve (the on-chain side of invariant
// i1: circulating USDX == reserveBalance).
func (w *Watcher) ReserveBalance(ctx context.Context) (*big.Int, error) {
	eth := w.client.Eth()
	bound := bind.NewBoundContract(w.vault, w.bind.vault, eth, eth, eth)
	var out []interface{}
	if err := bound.Call(&bind.CallOpts{Context: ctx}, &out, "reserveBalance"); err != nil {
		return nil, fmt.Errorf("chain: read reserveBalance: %w", err)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("chain: reserveBalance returned no data")
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("chain: reserveBalance wrong type %T", out[0])
	}
	return v, nil
}
