# EVM Client

**Source files:** `internal/evm/client.go`, `internal/evm/evm.go`

The EVM client wraps go-ethereum RPC operations for chain interaction: tx building, gas estimation, signing, raw broadcast, receipt polling, and debug tracing. It is a thin, stateless wrapper around `ethclient.Client` with per-call connection management.

---

## Design decisions

### Per-call connection management

The client stores only the RPC URL and optional chain ID. Each method dials a fresh `ethclient.Client` and defers `Close`. This avoids connection pool complexity and stale connection issues. The tradeoff is connection overhead per call; for high-throughput scenarios, a persistent connection pool could be added.

```go
type Client struct {
    rpcURL  string
    chainID *big.Int // cached from first call
}
```

### EIP-1559 with legacy fallback

`BuildTx` attempts EIP-1559 first:

1. Fetches latest header; if `BaseFee` is present, uses dynamic fee tx
2. `GasTipCap` = suggested tip (fallback to 1 gwei)
3. `GasFeeCap` = `2*BaseFee + tip` (standard headroom for one base-fee bump)
4. If header lacks `BaseFee`, falls back to legacy tx with `GasPrice`

This handles both modern chains (Paxeer, Ethereum mainnet) and legacy chains without EIP-1559.

### Gas estimation with fallback

If `TxParams.Gas` is 0, the client estimates gas via `eth_estimateGas`. If estimation fails or returns 0, it falls back to 3,000,000 gas. This is a safety net for contracts with complex constructor logic or chains with unreliable estimators.

### Raw tx broadcast

`SendRawTransaction` unmarshals the RLP-encoded tx, validates it, and broadcasts via `SendTransaction`. This ensures the tx is well-formed before broadcast and returns the computed hash.

### Receipt polling

`WaitReceipt` polls every 2 seconds until the receipt is found or context is cancelled. This is a simple blocking wait suitable for daemon use. For production, a subscription-based approach (`eth_newFilter`) could reduce latency.

### Debug tracing

`TraceCall` uses the raw `rpc.Client` (not `ethclient`) to call `debug_traceCall` with `disableStorage: true`. This reduces trace size and is sufficient for most agent debugging needs. Returns raw JSON (`any`) — the caller handles formatting.

---

## Key methods

```go
func Dial(rpcURL string, chainID uint64) (*Client, error)
func (c *Client) ChainID(ctx context.Context) (*big.Int, error)
func (c *Client) BuildTx(ctx context.Context, p TxParams) (*types.Transaction, error)
func (c *Client) CallMessage(ctx context.Context, from, to, data, value, block string) ([]byte, error)
func (c *Client) EstimateGas(ctx context.Context, from, to, data, value string) (uint64, error)
func (c *Client) CodeAt(ctx context.Context, address string) ([]byte, error)
func (c *Client) TraceCall(ctx context.Context, from, to, data, value string) (any, error)
func (c *Client) SendRawTransaction(ctx context.Context, rawTx []byte) (string, error)
func (c *Client) WaitReceipt(ctx context.Context, txHash string) (*types.Receipt, error)
func (c *Client) GetNonce(ctx context.Context, from string) (uint64, error)
```

### Tx signing helpers

```go
func SignTx(tx *types.Transaction, chainID *big.Int, privateKeyHex string) ([]byte, common.Address, error)
func SignTxKey(tx *types.Transaction, chainID *big.Int, key *ecdsa.PrivateKey) ([]byte, common.Address, error)
```

`SignTxKey` uses `types.LatestSignerForChainID` to select the correct signer (EIP-155, EIP-2930, EIP-1559) and returns the RLP-encoded raw tx.

---

## Modifying the EVM client

| What to change | Where |
|---|---|
| Add connection pooling | `internal/evm/client.go` — add `sync.Pool` or persistent client |
| Change gas fallback | `internal/evm/evm.go` — `BuildTx` gas default |
| Add batch RPC | `internal/evm/client.go` — new method using `rpc.BatchCall` |
| Change receipt poll interval | `internal/evm/client.go` — `WaitReceipt` ticker |
| Add websocket support | `internal/evm/client.go` — `DialContext` with ws:// |
