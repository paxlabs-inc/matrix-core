package chain

import (
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/accounts/abi"
	"github.com/ethereum/go-ethereum/common"
)

const settlementAnchorABI = `[
  {"inputs":[{"internalType":"address","name":"developer","type":"address"},{"internalType":"bytes32","name":"receiptsRoot","type":"bytes32"},{"internalType":"uint256","name":"totalWei","type":"uint256"},{"internalType":"uint256","name":"count","type":"uint256"}],"name":"anchor","outputs":[],"stateMutability":"nonpayable","type":"function"}
]`

// SettlementAnchor encodes anchor calldata.
type SettlementAnchor struct {
	abi abi.ABI
}

// NewSettlementAnchor returns an anchor encoder.
func NewSettlementAnchor() (*SettlementAnchor, error) {
	parsed, err := abi.JSON(strings.NewReader(settlementAnchorABI))
	if err != nil {
		return nil, fmt.Errorf("chain: parse settlement anchor abi: %w", err)
	}
	return &SettlementAnchor{abi: parsed}, nil
}

// EncodeAnchor returns calldata for anchor(developer, receiptsRoot, totalWei, count).
func (a *SettlementAnchor) EncodeAnchor(developer common.Address, receiptsRoot [32]byte, totalWei *big.Int, count int) ([]byte, error) {
	return a.abi.Pack("anchor", developer, receiptsRoot, totalWei, big.NewInt(int64(count)))
}
