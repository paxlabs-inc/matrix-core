// Package chain provides Paxeer EVM RPC access for Deus.
package chain

import (
	"context"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/ethclient"
)

const DefaultChainID = int64(125)

// Client wraps go-ethereum RPC access to Paxeer chain 125.
type Client struct {
	rpcURL  string
	chainID *big.Int
	eth     *ethclient.Client
}

// New dials PAXEER_RPC_URL and verifies chain id when expected > 0.
func New(ctx context.Context, rpcURL string, expectedChainID int64) (*Client, error) {
	if rpcURL == "" {
		return nil, fmt.Errorf("chain: empty rpc url")
	}
	eth, err := ethclient.DialContext(ctx, rpcURL)
	if err != nil {
		return nil, fmt.Errorf("chain: dial: %w", err)
	}
	c := &Client{
		rpcURL:  rpcURL,
		chainID: big.NewInt(expectedChainID),
		eth:     eth,
	}
	if expectedChainID > 0 {
		id, err := eth.ChainID(ctx)
		if err != nil {
			eth.Close()
			return nil, fmt.Errorf("chain: chain id: %w", err)
		}
		if id.Int64() != expectedChainID {
			eth.Close()
			return nil, fmt.Errorf("chain: got chain id %s, want %d", id.String(), expectedChainID)
		}
	}
	return c, nil
}

// Eth returns the underlying ethclient.
func (c *Client) Eth() *ethclient.Client {
	return c.eth
}

// ChainID returns the configured chain id.
func (c *Client) ChainID() *big.Int {
	return new(big.Int).Set(c.chainID)
}

// Ping issues a lightweight RPC call.
func (c *Client) Ping(ctx context.Context) error {
	_, err := c.eth.BlockNumber(ctx)
	if err != nil {
		return fmt.Errorf("chain: ping: %w", err)
	}
	return nil
}

// Close closes the RPC client.
func (c *Client) Close() {
	if c.eth != nil {
		c.eth.Close()
	}
}
