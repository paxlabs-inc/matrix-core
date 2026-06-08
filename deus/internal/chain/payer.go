package chain

import (
	"context"
	"crypto/ecdsa"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/accounts/abi/bind"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
)

// paymentChannelABI is the minimal ABI for per-caller escrow payout/reads.
const paymentChannelABI = `[
  {"inputs":[{"internalType":"address","name":"payee","type":"address"},{"internalType":"uint256","name":"amountWei","type":"uint256"}],"name":"payout","outputs":[],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[],"name":"fundedWei","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"redeemedWei","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"availableWei","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"}
]`

// settlementAnchorABI is the minimal ABI for anchoring a settled batch root.
const settlementAnchorABI = `[
  {"inputs":[{"internalType":"address","name":"developer","type":"address"},{"internalType":"bytes32","name":"receiptsRoot","type":"bytes32"},{"internalType":"uint256","name":"totalWei","type":"uint256"},{"internalType":"uint256","name":"count","type":"uint256"}],"name":"anchor","outputs":[],"stateMutability":"nonpayable","type":"function"}
]`

// Payer settles net-rail windows on-chain: it releases per-caller channel escrow
// to developers (PaymentChannel.payout) and anchors the receipts root
// (SettlementAnchor.anchor) with the Deus settler key. Implements
// settlement.Payer (docs/08 §8.3, docs/04 §4.5).
type Payer struct {
	eth        *ethclient.Client
	chainID    *big.Int
	key        *ecdsa.PrivateKey
	from       common.Address
	anchorAddr common.Address
	channelABI abi.ABI
	anchorABI  abi.ABI
}

// NewPayer builds a chain-backed settlement payer.
func NewPayer(c *Client, settlerKeyHex, anchorAddr string) (*Payer, error) {
	if c == nil || c.Eth() == nil {
		return nil, fmt.Errorf("chain: nil client")
	}
	key, err := crypto.HexToECDSA(strings.TrimPrefix(strings.TrimSpace(settlerKeyHex), "0x"))
	if err != nil {
		return nil, fmt.Errorf("chain: invalid settler key: %w", err)
	}
	if anchorAddr == "" {
		return nil, fmt.Errorf("chain: empty settlement anchor address")
	}
	chABI, err := abi.JSON(strings.NewReader(paymentChannelABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse channel abi: %w", err)
	}
	anABI, err := abi.JSON(strings.NewReader(settlementAnchorABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse anchor abi: %w", err)
	}
	return &Payer{
		eth:        c.Eth(),
		chainID:    c.ChainID(),
		key:        key,
		from:       crypto.PubkeyToAddress(key.PublicKey),
		anchorAddr: common.HexToAddress(anchorAddr),
		channelABI: chABI,
		anchorABI:  anABI,
	}, nil
}

// SettlerAddress returns the on-chain settler address.
func (p *Payer) SettlerAddress() common.Address { return p.from }

func (p *Payer) transactor(ctx context.Context) (*bind.TransactOpts, error) {
	auth, err := bind.NewKeyedTransactorWithChainID(p.key, p.chainID)
	if err != nil {
		return nil, fmt.Errorf("chain: transactor: %w", err)
	}
	auth.Context = ctx
	return auth, nil
}

// PayoutDeveloper releases amountWei from a caller's channel escrow to the
// developer payout address. escrowAddr is the deployed PaymentChannel for the
// funding caller; payout reverts on-chain if it would exceed funded escrow.
func (p *Payer) PayoutDeveloper(ctx context.Context, escrowAddr, payoutAddr, amountWei string) (string, error) {
	if escrowAddr == "" || strings.EqualFold(escrowAddr, "0xescrow-dev") {
		return "", fmt.Errorf("chain: channel escrow address not set")
	}
	amount, ok := new(big.Int).SetString(amountWei, 10)
	if !ok {
		return "", fmt.Errorf("chain: invalid amount %q", amountWei)
	}
	auth, err := p.transactor(ctx)
	if err != nil {
		return "", err
	}
	bound := bind.NewBoundContract(common.HexToAddress(escrowAddr), p.channelABI, p.eth, p.eth, p.eth)
	tx, err := bound.Transact(auth, "payout", common.HexToAddress(payoutAddr), amount)
	if err != nil {
		return "", fmt.Errorf("chain: payout tx: %w", err)
	}
	receipt, err := bind.WaitMined(ctx, p.eth, tx)
	if err != nil {
		return "", fmt.Errorf("chain: payout wait: %w", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return "", fmt.Errorf("chain: payout reverted")
	}
	return tx.Hash().Hex(), nil
}

// AnchorSettlement records the merkle root of the settled batch on-chain.
func (p *Payer) AnchorSettlement(ctx context.Context, developerAddr, merkleRoot, totalWei string, count int) (string, error) {
	root, err := decodeRoot(merkleRoot)
	if err != nil {
		return "", err
	}
	total, ok := new(big.Int).SetString(totalWei, 10)
	if !ok {
		return "", fmt.Errorf("chain: invalid total %q", totalWei)
	}
	auth, err := p.transactor(ctx)
	if err != nil {
		return "", err
	}
	bound := bind.NewBoundContract(p.anchorAddr, p.anchorABI, p.eth, p.eth, p.eth)
	tx, err := bound.Transact(auth, "anchor", common.HexToAddress(developerAddr), root, total, big.NewInt(int64(count)))
	if err != nil {
		return "", fmt.Errorf("chain: anchor tx: %w", err)
	}
	receipt, err := bind.WaitMined(ctx, p.eth, tx)
	if err != nil {
		return "", fmt.Errorf("chain: anchor wait: %w", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return "", fmt.Errorf("chain: anchor reverted")
	}
	return tx.Hash().Hex(), nil
}

// FundedWei reads the on-chain escrow funded amount for reconciliation (F7).
func (p *Payer) FundedWei(ctx context.Context, escrowAddr string) (string, error) {
	if escrowAddr == "" {
		return "", fmt.Errorf("chain: empty escrow address")
	}
	bound := bind.NewBoundContract(common.HexToAddress(escrowAddr), p.channelABI, p.eth, p.eth, p.eth)
	var out []interface{}
	if err := bound.Call(&bind.CallOpts{Context: ctx}, &out, "fundedWei"); err != nil {
		return "", fmt.Errorf("chain: read fundedWei: %w", err)
	}
	if len(out) == 0 {
		return "", fmt.Errorf("chain: fundedWei returned no data")
	}
	v, ok := out[0].(*big.Int)
	if !ok {
		return "", fmt.Errorf("chain: fundedWei wrong type")
	}
	return v.Text(10), nil
}

func decodeRoot(s string) ([32]byte, error) {
	var root [32]byte
	b := common.FromHex(s)
	if len(b) != 32 {
		return root, fmt.Errorf("chain: merkle root len %d", len(b))
	}
	copy(root[:], b)
	return root, nil
}
