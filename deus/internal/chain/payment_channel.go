package chain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
)

const paymentChannelABI = `[
  {"inputs":[],"name":"fund","outputs":[],"stateMutability":"payable","type":"function"},
  {"inputs":[{"internalType":"address","name":"payee","type":"address"},{"internalType":"uint256","name":"amountWei","type":"uint256"}],"name":"payout","outputs":[],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[],"name":"fundedWei","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"},
  {"inputs":[],"name":"caller","outputs":[{"internalType":"address","name":"","type":"address"}],"stateMutability":"view","type":"function"}
]`

// PaymentChannel encodes calldata and reads escrow state.
type PaymentChannel struct {
	client *Client
	abi    abi.ABI
}

// NewPaymentChannel returns a PaymentChannel helper (client may be nil for encoding only).
func NewPaymentChannel(c *Client) (*PaymentChannel, error) {
	parsed, err := abi.JSON(strings.NewReader(paymentChannelABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse payment channel abi: %w", err)
	}
	return &PaymentChannel{client: c, abi: parsed}, nil
}

// EncodeFund returns calldata for fund().
func (p *PaymentChannel) EncodeFund() ([]byte, error) {
	return p.abi.Pack("fund")
}

// EncodePayout returns calldata for payout(payee, amountWei).
func (p *PaymentChannel) EncodePayout(payee common.Address, amountWei *big.Int) ([]byte, error) {
	return p.abi.Pack("payout", payee, amountWei)
}

// FundedWei eth_calls fundedWei() (requires a chain client).
func (p *PaymentChannel) FundedWei(ctx context.Context, escrow common.Address) (*big.Int, error) {
	if p.client == nil {
		return nil, fmt.Errorf("chain: client required")
	}
	data, err := p.abi.Pack("fundedWei")
	if err != nil {
		return nil, err
	}
	out, err := p.client.Eth().CallContract(ctx, ethereum.CallMsg{To: &escrow, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("chain: fundedWei: %w", err)
	}
	vals, err := p.abi.Unpack("fundedWei", out)
	if err != nil {
		return nil, fmt.Errorf("chain: fundedWei decode: %w", err)
	}
	funded, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("chain: fundedWei: bad type")
	}
	return funded, nil
}

// Caller eth_calls caller() (requires a chain client).
func (p *PaymentChannel) Caller(ctx context.Context, escrow common.Address) (common.Address, error) {
	if p.client == nil {
		return common.Address{}, fmt.Errorf("chain: client required")
	}
	data, err := p.abi.Pack("caller")
	if err != nil {
		return common.Address{}, err
	}
	out, err := p.client.Eth().CallContract(ctx, ethereum.CallMsg{To: &escrow, Data: data}, nil)
	if err != nil {
		return common.Address{}, fmt.Errorf("chain: channel caller: %w", err)
	}
	vals, err := p.abi.Unpack("caller", out)
	if err != nil {
		return common.Address{}, fmt.Errorf("chain: channel caller decode: %w", err)
	}
	addr, ok := vals[0].(common.Address)
	if !ok {
		return common.Address{}, fmt.Errorf("chain: channel caller: bad type")
	}
	return addr, nil
}

// VerifyFundTx checks a fund() tx credited the escrow for the expected caller.
func (p *PaymentChannel) VerifyFundTx(ctx context.Context, fundTxHash, callerWallet string) (escrowAddr string, fundedWei string, err error) {
	if p.client == nil {
		return "", "", fmt.Errorf("chain: client required")
	}
	hash := common.HexToHash(fundTxHash)
	receipt, err := p.client.Eth().TransactionReceipt(ctx, hash)
	if err != nil {
		return "", "", fmt.Errorf("chain: fund tx receipt: %w", err)
	}
	if receipt.Status != types.ReceiptStatusSuccessful {
		return "", "", fmt.Errorf("chain: fund tx failed")
	}
	tx, _, err := p.client.Eth().TransactionByHash(ctx, hash)
	if err != nil {
		return "", "", fmt.Errorf("chain: fund tx: %w", err)
	}
	if tx.To() == nil {
		return "", "", fmt.Errorf("chain: fund tx has no to address")
	}
	escrow := *tx.To()
	funded, err := p.FundedWei(ctx, escrow)
	if err != nil {
		return "", "", err
	}
	if funded.Sign() <= 0 {
		return "", "", fmt.Errorf("chain: escrow not funded")
	}
	onChainCaller, err := p.Caller(ctx, escrow)
	if err != nil {
		return "", "", err
	}
	want := common.HexToAddress(callerWallet)
	if onChainCaller != want {
		return "", "", fmt.Errorf("chain: escrow caller mismatch")
	}
	return escrow.Hex(), funded.Text(10), nil
}
