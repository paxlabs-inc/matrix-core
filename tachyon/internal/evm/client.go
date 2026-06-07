package evm

import (
	"context"
	"crypto/ecdsa"
	"encoding/hex"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/ethereum/go-ethereum"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/rpc"
)

// Client wraps go-ethereum RPC operations.
type Client struct {
	rpcURL  string
	chainID *big.Int
}

// Dial connects to an RPC endpoint.
func Dial(rpcURL string, chainID uint64) (*Client, error) {
	c := &Client{rpcURL: rpcURL}
	if chainID > 0 {
		c.chainID = new(big.Int).SetUint64(chainID)
	}
	return c, nil
}

func (c *Client) ethClient(ctx context.Context) (*ethclient.Client, error) {
	return ethclient.DialContext(ctx, c.rpcURL)
}

// CallMessage performs eth_call.
func (c *Client) CallMessage(ctx context.Context, from, to, data, value, block string) ([]byte, error) {
	client, err := c.ethClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()

	msg := ethereum.CallMsg{
		To:   addrPtr(to),
		Data: hexData(data),
	}
	if from != "" {
		a := common.HexToAddress(from)
		msg.From = a
	}
	if value != "" {
		v, ok := new(big.Int).SetString(strings.TrimPrefix(value, "0x"), 0)
		if !ok {
			v, _ = new(big.Int).SetString(value, 10)
		}
		if v != nil {
			msg.Value = v
		}
	}
	var blockNum *big.Int
	if block != "" && block != "latest" {
		blockNum, _ = new(big.Int).SetString(strings.TrimPrefix(block, "0x"), 0)
	}
	return client.CallContract(ctx, msg, blockNum)
}

// EstimateGas estimates gas for a call.
func (c *Client) EstimateGas(ctx context.Context, from, to, data, value string) (uint64, error) {
	client, err := c.ethClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	msg := ethereum.CallMsg{
		To:   addrPtr(to),
		Data: hexData(data),
	}
	if from != "" {
		msg.From = common.HexToAddress(from)
	}
	if value != "" {
		v, _ := new(big.Int).SetString(value, 10)
		msg.Value = v
	}
	return client.EstimateGas(ctx, msg)
}

// CodeAt returns contract bytecode at address.
func (c *Client) CodeAt(ctx context.Context, address string) ([]byte, error) {
	client, err := c.ethClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	return client.CodeAt(ctx, common.HexToAddress(address), nil)
}

// TraceCall runs debug_traceCall when supported.
func (c *Client) TraceCall(ctx context.Context, from, to, data, value string) (any, error) {
	rc, err := rpc.DialContext(ctx, c.rpcURL)
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	call := map[string]string{
		"to":   to,
		"data": data,
	}
	if from != "" {
		call["from"] = from
	}
	if value != "" {
		call["value"] = value
	}
	var result any
	err = rc.CallContext(ctx, &result, "debug_traceCall", call, "latest", map[string]bool{"disableStorage": true})
	return result, err
}

// SendRawTransaction broadcasts a signed tx.
func (c *Client) SendRawTransaction(ctx context.Context, rawTx []byte) (string, error) {
	client, err := c.ethClient(ctx)
	if err != nil {
		return "", err
	}
	defer client.Close()
	tx := new(types.Transaction)
	if err := tx.UnmarshalBinary(rawTx); err != nil {
		return "", err
	}
	if err := client.SendTransaction(ctx, tx); err != nil {
		return "", err
	}
	return tx.Hash().Hex(), nil
}

// WaitReceipt polls for a transaction receipt.
func (c *Client) WaitReceipt(ctx context.Context, txHash string) (*types.Receipt, error) {
	client, err := c.ethClient(ctx)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	hash := common.HexToHash(txHash)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	for {
		receipt, err := client.TransactionReceipt(ctx, hash)
		if err == nil {
			return receipt, nil
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

// GetNonce returns account nonce.
func (c *Client) GetNonce(ctx context.Context, from string) (uint64, error) {
	client, err := c.ethClient(ctx)
	if err != nil {
		return 0, err
	}
	defer client.Close()
	return client.PendingNonceAt(ctx, common.HexToAddress(from))
}

// SignTx signs with a private key hex.
func SignTx(tx *types.Transaction, chainID *big.Int, privateKeyHex string) ([]byte, common.Address, error) {
	key, err := crypto.HexToECDSA(strings.TrimPrefix(privateKeyHex, "0x"))
	if err != nil {
		return nil, common.Address{}, err
	}
	return SignTxKey(tx, chainID, key)
}

// SignTxKey signs with a parsed ECDSA key and returns the RLP-encoded raw tx.
func SignTxKey(tx *types.Transaction, chainID *big.Int, key *ecdsa.PrivateKey) ([]byte, common.Address, error) {
	signer := types.LatestSignerForChainID(chainID)
	signed, err := types.SignTx(tx, signer, key)
	if err != nil {
		return nil, common.Address{}, err
	}
	from := crypto.PubkeyToAddress(key.PublicKey)
	raw, err := signed.MarshalBinary()
	return raw, from, err
}

func addrPtr(to string) *common.Address {
	if to == "" {
		return nil
	}
	a := common.HexToAddress(to)
	return &a
}

func hexData(data string) []byte {
	if data == "" {
		return nil
	}
	b, err := hex.DecodeString(strings.TrimPrefix(data, "0x"))
	if err != nil {
		return nil
	}
	return b
}

// EncodeRevertReason attempts to stringify revert data.
func EncodeRevertReason(data []byte) string {
	if len(data) == 0 {
		return ""
	}
	return fmt.Sprintf("0x%s", hex.EncodeToString(data))
}
