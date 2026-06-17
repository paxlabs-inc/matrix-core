package chain

import (
	"context"
	"fmt"
	"math/big"
	"strings"

	"github.com/ethereum/go-ethereum/ethclient"
)

// DefaultChainID is the Paxeer mainnet chain id LayerX settles to.
const DefaultChainID = int64(125)

// Client wraps go-ethereum RPC access to Paxeer (chain 125). It is the shared
// transport the on-chain settler (chain-out) and the deposit watcher (chain-in)
// build on. Mirrors deus/internal/chain.Client (frozen spec
// [topology.direction_chain_out]).
type Client struct {
	rpcURL  string
	chainID *big.Int
	eth     *ethclient.Client
}

// NewClient dials the Paxeer RPC and verifies the chain id matches when
// expectedChainID > 0 (0 skips the check for local anvil/mock use). The caller
// owns Close.
func NewClient(ctx context.Context, rpcURL string, expectedChainID int64) (*Client, error) {
	if strings.TrimSpace(rpcURL) == "" {
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

// Eth returns the underlying ethclient for log/receipt reads and tx sends.
func (c *Client) Eth() *ethclient.Client { return c.eth }

// ChainID returns a copy of the configured chain id.
func (c *Client) ChainID() *big.Int { return new(big.Int).Set(c.chainID) }

// Ping issues a lightweight RPC round-trip to confirm the endpoint is live.
func (c *Client) Ping(ctx context.Context) error {
	if _, err := c.eth.BlockNumber(ctx); err != nil {
		return fmt.Errorf("chain: ping: %w", err)
	}
	return nil
}

// Close releases the RPC connection.
func (c *Client) Close() {
	if c.eth != nil {
		c.eth.Close()
	}
}
