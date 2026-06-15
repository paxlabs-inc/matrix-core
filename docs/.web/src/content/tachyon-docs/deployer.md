# Deployer

**Source file:** `internal/deployer/deployer.go`

The deployer plans and executes intent-based contract deployments with idempotency guarantees. It handles both plain contract creation and CREATE2 deterministic deployments, with on-chain confirmation and registry write-back.

---

## Design decisions

### Idempotency by key, not by state

Deployments are keyed by `idempotency_key + chain_id`. Before broadcasting, the deployer checks the registry for an existing record. If found and confirmed on-chain (bytecode > 0 at the address), it returns the existing deployment with `Existing: true`. This makes retries safe: the same key always returns the same address.

### CREATE2 support

CREATE2 deployments use a deterministic factory pattern. The deployer:

1. Computes the deterministic address from `deployer` (factory), `salt` (32 bytes), and `keccak256(initCode)`
2. Checks if code already exists at that address
3. If not, builds a factory call: `to = deployer`, `data = salt ++ initCode`
4. After broadcast, uses the precomputed `create2Addr` as the deployment address (not the receipt's `ContractAddress`, which is the factory call's receipt)

```go
func computeCreate2(deployerHex, saltHex string, initCode []byte) (common.Address, error)
```

### On-chain confirmation

Before returning an existing deployment, the deployer verifies the contract is actually on-chain:

```go
func (d *Deployer) confirmOnChain(ctx context.Context, chainID, address string) (bool, error)
```

This calls `eth_getCode` and checks `len(code) > 0`. This prevents returning stale registry entries from reverted deployments or chain resets.

### Constructor arg packing

Constructor arguments are ABI-encoded and appended to the creation bytecode:

```go
func packConstructor(art registry.ArtifactRecord, args json.RawMessage) ([]byte, error)
```

- Decodes bytecode from hex
- If args present and not `null`: calls `abienc.PackConstructorArgs(art.ABI, args)`
- Returns `bytecode ++ packedArgs`

### Wallet integration

The deployer delegates signing and broadcasting to the wallet gate:

1. `Wallet.Authorize(capabilityToken, spendCap, chainID)` — policy check
2. `Wallet.Sign(ctx, client, intent, policy)` — sign or delegate
3. If `RawTx` returned: `client.SendRawTransaction(ctx, res.RawTx)` — broadcast
4. If `TxHash` returned: signer already broadcast (embedded wallet mode)
5. `client.WaitReceipt(ctx, txHash)` — poll for confirmation

### Registry write-back

After successful deployment, the record is written to the registry:

```go
registry.DeploymentRecord{
    IdempotencyKey: key,
    ChainID:        chainKey,
    Contract:       contract,
    Address:        address,
    TxHash:         txHash,
    Confirmed:      txHash != "" || existing,
    ProjectID:      projectID,
}
```

---

## Deployment flow

```
DeployRequest{idempotency_key, chain_id, contract, constructor_args, create2?}
    │
    ▼
Check registry for existing deployment by key+chain
    │
    ├── Found + confirmed on-chain → return Existing: true
    │
    └── Not found / not confirmed
        │
        ▼
    Resolve artifact from registry (projectID:contract)
        │
        ▼
    Resolve chain profile
        │
        ▼
    Pack constructor args + bytecode
        │
        ▼
    Build TxIntent (plain or CREATE2)
        │
        ▼
    Authorize via wallet policy gate
        │
        ▼
    Sign + broadcast
        │
        ▼
    Wait for receipt
        │
        ▼
    Record in registry
        │
        ▼
    Return DeployResponse
```

---

## Error codes

| Code | Retry | Meaning |
|---|---|---|
| `DEPLOY_FAILED` | varies | Deployment planning or broadcast failed |
| `ARTIFACT_NOT_FOUND` | no | Compile first or wrong project_id |
| `WALLET_NOT_CONFIGURED` | no | No signer configured |
| `WALLET_POLICY_DENIED` | no | Spend cap or allow-list rejected |
| `CHAIN_NOT_FOUND` | no | Unknown chain_id |
| `CHAIN_RPC_FAILED` | yes | RPC dial or transport error |

---

## Modifying the deployer

| What to change | Where |
|---|---|
| Add deployment metadata | `pkg/types/deploy.go` — `DeployRequest`/`DeployResponse` |
| Change CREATE2 formula | `internal/deployer/deployer.go` — `computeCreate2` |
| Add proxy deployment | New method — UUPS/transparent proxy pattern |
| Change confirmation logic | `internal/deployer/deployer.go` — `confirmOnChain` |
| Add multi-chain deploy | `internal/deployer/deployer.go` — loop over chain IDs |
