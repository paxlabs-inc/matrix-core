# Wallet

**Source files:** `internal/wallet/wallet.go`, `internal/wallet/embedded.go`

The wallet subsystem provides signing backends and a policy gate. Two modes: **self-hosted** (operator holds the key) and **embedded** (Paxeer embedded wallet signs server-side via the agent-native DID lane). Every signature passes a capability policy gate.

---

## Design decisions

### Policy gate before signing

The `Gate` struct wraps a `Signer` and a map of `Policy` profiles. Before any signing operation, `Gate.Authorize` resolves the requested `capability_token` to a policy, then `Gate.Sign` validates the intent against that policy:

- **Spend cap:** `tx.Value` must not exceed `policy.SpendCapWei`
- **Chain allow-list:** `chainID` must be in `policy.AllowedChains`
- **Contract allow-list:** `intent.To` must be in `policy.AllowedContracts` (if set)

The requester can only tighten the profile's spend cap, never raise it.

### Two wallet modes

**Self-hosted** (`wallet.mode = "self_hosted"`):
- The operator holds the ECDSA private key
- Three signer variants: `raw` (hex key in config), `env` (key from environment variable), `keystore` (web3 secret-storage JSON + password)
- Local signing: the daemon builds the tx (nonce, gas, EIP-1559 fees), signs with go-ethereum, returns raw RLP-encoded tx
- The policy gate is enforced locally

**Embedded** (`wallet.mode = "embedded"`):
- No local EVM keys. The daemon's ed25519 seed proves a `did:matrix:<label>:<keyfp>` identity
- Signing and broadcasting are delegated to the Paxeer embedded wallet server-side
- Custody policy (frozen, read-only, spend caps, allow-lists) is enforced by the wallet
- Two sub-modes:
  - **Single-tenant:** keyfile configured → daemon authenticates with its own DID
  - **Multi-tenant:** keyfile empty → every request must carry a forwarded `WalletToken` (per-agent bearer)

### SignResult shape

```go
type SignResult struct {
    RawTx  []byte         // locally signed tx; caller must broadcast
    TxHash string         // remote signer already broadcast
    From   common.Address // signer address
}
```

Exactly one of `RawTx` or `TxHash` is populated. This unifies both modes: the caller either broadcasts the raw tx or waits for the remote tx hash.

---

## Self-hosted signer

```go
type LocalSigner struct {
    key  *ecdsa.PrivateKey
    addr common.Address
}
```

### Key loading

```go
func NewLocalSigner(w config.WalletConfig) (*LocalSigner, error)
```

- `SignerKeystore`: decrypts web3 secret-storage JSON with password
- `SignerRaw` / `SignerEnv` / default: parses hex ECDSA key
- Address derived from public key via `crypto.PubkeyToAddress`

### Signing

```go
func (s *LocalSigner) Sign(ctx context.Context, client *evm.Client, intent TxIntent) (SignResult, error)
```

1. Fetches chain ID from client (or cached)
2. Builds unsigned tx via `client.BuildTx` (nonce, gas estimate, EIP-1559 or legacy)
3. Signs with `evm.SignTxKey` (latest signer for chain ID)
4. Returns `RawTx` + `From` address

---

## Embedded signer

```go
type EmbeddedSigner struct {
    baseURL string
    label   string
    priv    ed25519.PrivateKey
    pubHex  string
    did     string
    http    *http.Client
    token   string // cached bearer
}
```

### Authentication (single-tenant)

The embedded signer uses an ed25519 challenge/verify handshake:

1. `POST /v1/agent/auth/challenge {did}` → `{message, nonce}`
2. `ed25519.Sign(priv, message)`
3. `POST /v1/agent/auth/verify {did, public_key, nonce, signature}` → `{token}`
4. Token cached; re-authenticates on 401

### Signing

```go
func (s *EmbeddedSigner) Sign(ctx context.Context, _ *evm.Client, intent TxIntent) (SignResult, error)
```

- Builds tx payload map (`to`, `data`, `value`, `gas`)
- `POST /v1/agent/send {tx}` (Bearer token)
- Returns `TxHash` + `From` address
- The EVM key, nonce, gas, and broadcast all live server-side

### Multi-tenant mode

When `keyfile` is empty, the signer holds no seed. Every `Sign` call must provide `intent.AuthToken` (a forwarded per-agent bearer). The daemon acts as a stateless proxy:

```go
func (s *EmbeddedSigner) send(ctx context.Context, token string, body, out any) error
```

If `token` is non-empty, it is used verbatim. Otherwise, the signer's own token is used (single-tenant).

---

## Policy profiles

Profiles are defined in `tachyon.config.kvx` under `[policy.*]` sections:

```toml
[policy.default]
spend_cap_wei = "100000000000000000"  # 0.1 ETH
allow         = []                     # empty = any destination
chains        = ["paxeer-mainnet"]
```

`buildProfiles` converts config profiles into `Policy` structs with parsed spend caps and address lists.

---

## Modifying the wallet

| What to change | Where |
|---|---|
| Add signer mode | `internal/wallet/wallet.go` — `NewGate` switch |
| Add policy dimension | `internal/wallet/wallet.go` — `Policy` struct + `validatePolicy` |
| Change embedded API | `internal/wallet/embedded.go` — `defaultEmbeddedAPI` |
| Add hardware wallet | New file `internal/wallet/hardware.go` — implement `Signer` |
| Change key loading | `internal/wallet/wallet.go` — `NewLocalSigner` |
