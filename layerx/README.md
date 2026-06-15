# LayerX — agent-native settlement fabric (`layerxd`)

One always-on sequencer that gives every Matrix agent an instant-settling,
gasless, USD-denominated balance (**USDX**), fully reserved 1:1 by **USDL**,
with a Merkle-provable signed receipt for every transfer, net-settled to **Paxeer
mainnet (chain 125)** on a tiered schedule. An agent's **DID is its account**.

## Layout

```
cmd/layerxd            # the sequencer daemon
internal/config        # env-first + layerx.config.kvx overlay
internal/auth          # ed25519 agent-DID challenge/verify + HMAC principal token
internal/store         # Postgres ledger: accounts, transfers, batches, deposits, withdrawals
internal/accumulator   # domain-separated Merkle leaves + root + inclusion proofs
internal/sig           # the sequencer's ed25519 receipt-signing key
internal/ledger        # atomic Pay + signed receipts + proof reconstruction
internal/settle        # tiered settlement worker (window net + force-settle + anchor)
internal/chain         # Settler interface + DevSettler (real Paxeer settler deferred)
internal/server        # HTTP surface
pkg/types              # {ok,data,error} envelope + wire contracts
migrations             # forward-only SQL
```

## Run (dev)

```bash
export LAYERX_DEV=1
export LAYERX_POSTGRES_URI='postgres://matrix:...@127.0.0.1:5432/layerx'
go run ./cmd/layerxd
```

## API (all `/v1/*` require the transport bearer + an `X-LayerX-Agent` token)

| Method | Path | Purpose |
| --- | --- | --- |
| POST | `/v1/agent/auth/challenge` | open the ed25519 DID auth lane |
| POST | `/v1/agent/auth/verify` | mint a short-lived principal token |
| GET | `/v1/balance` | USDX balance + escrow bound + payout address |
| GET | `/v1/deposit` | vault address + DID-claim payload |
| POST | `/v1/pay` | pay another agent by DID → signed receipt |
| GET | `/v1/receipt/{seq}` | signed inclusion receipt (+ anchor proof once settled) |
| POST | `/v1/withdraw` | burn USDX → release USDL |
| POST | `/v1/settle` | force-settle the open window now |

## Invariants

Fully reserved (`USDX == USDL in vault`), sequencer holds no agent keys,
escrow-bounded spend, domain-separated Merkle receipts, Paxeer-anchored roots,
L1 force-withdraw escape hatch, DID-scoped, tiered settlement, one always-on
sequencer. See `layerx.frozen.kvx [invariants]`.
