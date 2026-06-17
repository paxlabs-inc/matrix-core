package chain

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// Minimal ABI fragments for the LayerX contract surface the sequencer touches:
// LayerXVault.settle (the operator-only batch payout + root anchor, the
// chain-out write), its Deposit/Settled events (chain-in watcher + receipts),
// the SettlementAnchor read-back, plus the ERC-20 / PECOR router pieces the
// deposit-reconciliation and withdrawal-swap paths need later. Authored inline
// (mirroring deus/internal/chain.payer) rather than abigen so the surface stays
// small and auditable against contracts/src.

// layerXVaultABI: settle + deposit events + force-exit views + reserve reads.
const layerXVaultABI = `[
  {"inputs":[{"components":[{"internalType":"bytes32","name":"batchId","type":"bytes32"},{"internalType":"bytes32","name":"root","type":"bytes32"},{"internalType":"uint64","name":"windowEnd","type":"uint64"},{"components":[{"internalType":"address","name":"recipient","type":"address"},{"internalType":"uint256","name":"amount","type":"uint256"}],"internalType":"struct ILayerXVault.Payout[]","name":"payouts","type":"tuple[]"}],"internalType":"struct ILayerXVault.Batch","name":"batch","type":"tuple"}],"name":"settle","outputs":[],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[],"name":"reserveBalance","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"operator","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"},
  {"inputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"name":"settledBatch","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"view","type":"function"},
  {"inputs":[{"internalType":"address","name":"","type":"address"}],"name":"exited","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"view","type":"function"},
  {"anonymous":false,"inputs":[{"indexed":true,"internalType":"bytes32","name":"did","type":"bytes32"},{"indexed":true,"internalType":"address","name":"depositor","type":"address"},{"indexed":true,"internalType":"address","name":"tokenIn","type":"address"},{"indexed":false,"internalType":"uint256","name":"amountIn","type":"uint256"},{"indexed":false,"internalType":"uint256","name":"usdxMinted","type":"uint256"}],"name":"Deposit","type":"event"},
  {"anonymous":false,"inputs":[{"indexed":true,"internalType":"bytes32","name":"batchId","type":"bytes32"},{"indexed":false,"internalType":"bytes32","name":"root","type":"bytes32"},{"indexed":false,"internalType":"uint256","name":"totalSettled","type":"uint256"},{"indexed":false,"internalType":"uint256","name":"count","type":"uint256"},{"indexed":false,"internalType":"uint64","name":"windowEnd","type":"uint64"}],"name":"Settled","type":"event"}
]`

// settlementAnchorABI: the immutable root log read-back (record is invoked by the
// Vault, not the operator, so the operator only reads it here).
const settlementAnchorABI = `[
  {"inputs":[{"internalType":"bytes32","name":"batchId","type":"bytes32"}],"name":"rootOf","outputs":[{"internalType":"bytes32","name":"","type":"bytes32"}],"stateMutability":"view","type":"function"},
  {"inputs":[{"internalType":"bytes32","name":"batchId","type":"bytes32"}],"name":"anchored","outputs":[{"internalType":"bool","name":"","type":"bool"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"writer","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"},
  {"anonymous":false,"inputs":[{"indexed":true,"internalType":"bytes32","name":"batchId","type":"bytes32"},{"indexed":false,"internalType":"bytes32","name":"root","type":"bytes32"},{"indexed":false,"internalType":"uint256","name":"totalSettled","type":"uint256"},{"indexed":false,"internalType":"uint256","name":"count","type":"uint256"},{"indexed":false,"internalType":"uint64","name":"windowEnd","type":"uint64"},{"indexed":false,"internalType":"uint64","name":"anchoredAt","type":"uint64"}],"name":"SettlementAnchored","type":"event"}
]`

// erc20ABI: USDL reserve reads (reserve-reconciliation invariant i1).
const erc20ABI = `[
  {"inputs":[{"internalType":"address","name":"account","type":"address"}],"name":"balanceOf","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"decimals","outputs":[{"internalType":"uint8","name":"","type":"uint8"}],"stateMutability":"view","type":"function"}
]`

// pecorRouterABI: the deposit/withdrawal swap path (used in the chain-in/out flows).
const pecorRouterABI = `[
  {"inputs":[{"internalType":"address","name":"tokenIn","type":"address"},{"internalType":"address","name":"tokenOut","type":"address"},{"internalType":"uint256","name":"amountIn","type":"uint256"},{"internalType":"uint256","name":"amountOutMin","type":"uint256"},{"internalType":"uint256","name":"deadline","type":"uint256"}],"name":"swapBestRoute","outputs":[{"internalType":"uint256","name":"amountOut","type":"uint256"}],"stateMutability":"nonpayable","type":"function"}
]`

// Payout is the Go wire mirror of ILayerXVault.Payout (a per-account net
// withdrawal delta: the DID's mapped EVM recipient + USDL amount in micro-USDX).
type Payout struct {
	Recipient common.Address
	Amount    *big.Int
}

// Batch is the Go wire mirror of ILayerXVault.Batch — one operator-submitted
// settlement window. Field order/names match the ABI tuple components so
// abi.Pack maps them by reflection.
type Batch struct {
	BatchId   [32]byte
	Root      [32]byte
	WindowEnd uint64
	Payouts   []Payout
}

// bindings holds the parsed ABIs shared by the settler, deposit watcher, and
// reconciliation reads.
type bindings struct {
	vault  abi.ABI
	anchor abi.ABI
	erc20  abi.ABI
	router abi.ABI
}

// loadBindings parses every ABI fragment once. A parse error is a programming
// bug in the constants above, surfaced loudly at construction.
func loadBindings() (*bindings, error) {
	vault, err := abi.JSON(strings.NewReader(layerXVaultABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse vault abi: %w", err)
	}
	anchor, err := abi.JSON(strings.NewReader(settlementAnchorABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse anchor abi: %w", err)
	}
	erc20, err := abi.JSON(strings.NewReader(erc20ABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse erc20 abi: %w", err)
	}
	router, err := abi.JSON(strings.NewReader(pecorRouterABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse router abi: %w", err)
	}
	return &bindings{vault: vault, anchor: anchor, erc20: erc20, router: router}, nil
}

// PackSettle ABI-encodes a LayerXVault.settle(batch) call (selector + args). It
// is the chain-out write payload; broadcasting it is wired in Phase 2.
func (b *bindings) PackSettle(batch Batch) ([]byte, error) {
	data, err := b.vault.Pack("settle", batch)
	if err != nil {
		return nil, fmt.Errorf("chain: pack settle: %w", err)
	}
	return data, nil
}
