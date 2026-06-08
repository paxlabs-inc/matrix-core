// Package chain — PaymentStreams 0x0906 helpers (docs/04-onchain.md §4.5, tools/paxeer/lib/precompiles.mjs).
package chain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

// PaymentStreams precompile address on Paxeer chain 125.
var PaymentStreamsAddr = common.HexToAddress("0x0000000000000000000000000000000000000906")

const paymentStreamsABI = `[
  {"inputs":[{"internalType":"address","name":"payee","type":"address"},{"internalType":"address","name":"token","type":"address"},{"internalType":"uint256","name":"ratePerSecond","type":"uint256"},{"internalType":"uint64","name":"startTime","type":"uint64"},{"internalType":"uint64","name":"stopTime","type":"uint64"},{"internalType":"uint256","name":"cap","type":"uint256"}],"name":"open","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[{"internalType":"uint256","name":"streamId","type":"uint256"}],"name":"settle","outputs":[],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[{"internalType":"uint256","name":"streamId","type":"uint256"}],"name":"close","outputs":[],"stateMutability":"nonpayable","type":"function"},
  {"inputs":[{"internalType":"uint256","name":"streamId","type":"uint256"}],"name":"accrued","outputs":[{"internalType":"uint256","name":"","type":"uint256"}],"stateMutability":"view","type":"function"}
]`

// PaymentStreams encodes calldata and eth_calls the streams precompile.
type PaymentStreams struct {
	client *Client
	abi    abi.ABI
}

// NewPaymentStreams returns a PaymentStreams helper (client may be nil for encoding only).
func NewPaymentStreams(c *Client) (*PaymentStreams, error) {
	parsed, err := abi.JSON(strings.NewReader(paymentStreamsABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse streams abi: %w", err)
	}
	return &PaymentStreams{client: c, abi: parsed}, nil
}

// EncodeOpen returns calldata for open(payee, token, rate, start, stop, cap).
func (p *PaymentStreams) EncodeOpen(payee, token common.Address, ratePerSecond, capWei *big.Int, start, stop uint64) ([]byte, error) {
	return p.abi.Pack("open", payee, token, ratePerSecond, start, stop, capWei)
}

// EncodeSettle returns calldata for settle(streamId).
func (p *PaymentStreams) EncodeSettle(streamID *big.Int) ([]byte, error) {
	return p.abi.Pack("settle", streamID)
}

// EncodeClose returns calldata for close(streamId).
func (p *PaymentStreams) EncodeClose(streamID *big.Int) ([]byte, error) {
	return p.abi.Pack("close", streamID)
}

// Accrued eth_calls accrued(streamId) (requires a chain client).
func (p *PaymentStreams) Accrued(ctx context.Context, streamID *big.Int) (*big.Int, error) {
	if p.client == nil {
		return nil, fmt.Errorf("chain: client required")
	}
	data, err := p.abi.Pack("accrued", streamID)
	if err != nil {
		return nil, err
	}
	out, err := p.client.Eth().CallContract(ctx, ethereum.CallMsg{To: &PaymentStreamsAddr, Data: data}, nil)
	if err != nil {
		return nil, fmt.Errorf("chain: streams accrued: %w", err)
	}
	vals, err := p.abi.Unpack("accrued", out)
	if err != nil {
		return nil, fmt.Errorf("chain: streams accrued decode: %w", err)
	}
	if len(vals) != 1 {
		return nil, fmt.Errorf("chain: streams accrued: unexpected outputs")
	}
	acc, ok := vals[0].(*big.Int)
	if !ok {
		return nil, fmt.Errorf("chain: streams accrued: bad type")
	}
	return acc, nil
}
