package evm // ethclient wrappers, gas, nonce, tx building

import (
	"context"
	"math/big"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/params"
)

// TxParams describes an unsigned transaction intent.
type TxParams struct {
	From  common.Address
	To    *common.Address // nil => contract creation
	Data  []byte
	Value *big.Int
	Gas   uint64 // 0 => estimate
}

// ChainID returns the configured chain id, falling back to the network id.
func (c *Client) ChainID(ctx context.Context) (*big.Int, error) {
	if c.chainID != nil && c.chainID.Sign() > 0 {
		return new(big.Int).Set(c.chainID), nil
	}
	client, err := c.ethClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	id, err := client.NetworkID(ctx)
	if err != nil {
		return nil, err
	}
	c.chainID = id
	return id, nil
}

// BuildTx assembles an unsigned transaction with a live nonce, gas estimate,
// and EIP-1559 fees (falling back to a legacy gas price on pre-1559 chains).
func (c *Client) BuildTx(ctx context.Context, p TxParams) (*types.Transaction, error) {
	client, err := c.ethClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	chainID := c.chainID
	if chainID == nil || chainID.Sign() == 0 {
		chainID, err = client.NetworkID(ctx)
		if err != nil {
			return nil, err
		}
		c.chainID = chainID
	}

	value := p.Value
	if value == nil {
		value = big.NewInt(0)
	}

	nonce, err := client.PendingNonceAt(ctx, p.From)
	if err != nil {
		return nil, err
	}

	gas := p.Gas
	if gas == 0 {
		gas, err = client.EstimateGas(ctx, ethereum.CallMsg{
			From:  p.From,
			To:    p.To,
			Data:  p.Data,
			Value: value,
		})
		if err != nil || gas == 0 {
			gas = 3_000_000
		}
	}

	head, headErr := client.HeaderByNumber(ctx, nil)
	if headErr == nil && head.BaseFee != nil {
		tip, tipErr := client.SuggestGasTipCap(ctx)
		if tipErr != nil || tip == nil {
			tip = big.NewInt(params.GWei)
		}
		// feeCap = 2*baseFee + tip, a standard headroom for one base-fee bump.
		feeCap := new(big.Int).Add(new(big.Int).Mul(head.BaseFee, big.NewInt(2)), tip)
		return types.NewTx(&types.DynamicFeeTx{
			ChainID:   chainID,
			Nonce:     nonce,
			GasTipCap: tip,
			GasFeeCap: feeCap,
			Gas:       gas,
			To:        p.To,
			Value:     value,
			Data:      p.Data,
		}), nil
	}

	gasPrice, gpErr := client.SuggestGasPrice(ctx)
	if gpErr != nil || gasPrice == nil {
		gasPrice = big.NewInt(params.GWei)
	}
	return types.NewTx(&types.LegacyTx{
		Nonce:    nonce,
		GasPrice: gasPrice,
		Gas:      gas,
		To:       p.To,
		Value:    value,
		Data:     p.Data,
	}), nil
}
