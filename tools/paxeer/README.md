# paxeer-net — Paxeer network MCP bridge

The tool surface that embeds Matrix agents into the **Paxeer** network (EVM
chain `125`). One stdio MCP server exposes the whole network to the planner:
read everything, and act through a network-custody wallet. It backs the
`paxeer-identity / paxeer-read / paxeer-pay / paxeer-trade / paxeer-stake /
paxeer-schedule` skills.

## Why this design

- **Reads are free and vast.** Direct node RPC + the PaxScan (Blockscout v2)
  explorer + the Argus portfolio indexer + PaxSpot market data + price feeds +
  the agent-economy precompile views. A data layer no other chain hands agents.
- **Writes go through the embedded wallet** at `connect.paxportwallet.com`.
  Key material and signing live **network-side**, so spend limits, allow-lists,
  and policy are enforced *on the wallet at the network layer* — not trusted to
  agent code. Generic `{to,data,value}` means any precompile/contract call
  routes through one authenticated path.

## Layout

```
tools/paxeer/
├── paxeer-net.mjs        # MCP stdio server (wire + tools/list + tools/call)
└── lib/
    ├── config.mjs        # endpoints, chain, token registry, precompiles, contracts
    ├── keccak.mjs        # correct Keccak-256 (Ethereum, not NIST) + selectors  [self-test]
    ├── abi.mjs           # minimal ABI encode/decode for the types used         [self-test]
    ├── net.mjs           # fetch + MCP result shaping + amount math
    ├── rpc.mjs           # EVM JSON-RPC reads + typed eth_call
    ├── paxscan.mjs       # PaxScan / Blockscout v2 explorer
    ├── markets.mjs       # portfolio (Argus) + spot + price + points
    ├── wallet.mjs        # embedded-wallet REST client (headless auth)
    ├── precompiles.mjs   # streams/scheduler/staking/oracle/orob/clearing/pofq encoders+readers
    └── tools.mjs         # tool registry + dispatch (43 tools)
```

## Configuration (env)

All values have mainnet defaults; override via env.

| Var | Purpose | Default |
|---|---|---|
| `PAXEER_RPC_URL` | EVM JSON-RPC node | `https://public-mainnet.rpcpaxeer.online/evm` |
| `PAXEER_PAXSCAN_URL` | PaxScan explorer base | `https://paxscan.paxeer.app` |
| `PAXEER_PORTFOLIO_URL` | Argus portfolio indexer | `https://us-east-1.user-stats.sidiora.exchange` |
| `PAXEER_SPOT_URL` | PaxSpot market data | `https://us-east-1.spot-api.sidiora.exchange` |
| `PAXEER_PRICE_URL` | Price/OHLC API | `https://data-api.crossverse.app/api` |
| `PAXEER_CHAIN_ID` | EVM chain id | `125` |
| `PAXEER_MAX_SPEND_WEI` | per-call native PAX ceiling (0 = unlimited) | `0` |

### Headless wallet auth (required for writes)

Reads need no auth. Writes need a Supabase bearer for the custody API. Provide
**one** of:

- `PAXEER_WALLET_TOKEN` — a ready Supabase `access_token` (Bearer JWT), or
- `PAXEER_WALLET_EMAIL` + `PAXEER_WALLET_PASSWORD` (+ `PAXEER_SUPABASE_ANON_KEY`)
  — the bridge exchanges these for a token via the Supabase password grant and
  refreshes on 401.

Without wallet auth the bridge runs **read-only**: write tools return an
explanatory result (with the intended tx) instead of sending.

## Spend policy contract

Two independent gates protect value movement; agents must not route around them:

1. **Network-side custody enforcement** — the wallet API enforces the wallet's
   own spend limits / allow-lists. This is the authoritative gate.
2. **`PaxeerSpendPolicy`** (runtime, evaluated on the synthesized plan, mirrors
   `GideonOpsPolicy`) — per-call `PAXEER_MAX_SPEND_WEI` ceiling + any cortex
   `Constraint` spend limit. The bridge also enforces the wei ceiling locally
   in `tools.mjs::guardSpend` as a backstop.

Skills are instructed to STOP and ask when an amount is unbounded or exceeds a
known constraint.

## Tools

**Reads (no auth):** `rpc_call`, `eth_call`, `contract_read`, `encode_call`,
`chain_info`, `get_balance`, `token_balance`, `paxscan_get`, `address_overview`,
`address_transactions`, `tx`, `token_info`, `search`, `network_stats`,
`portfolio`, `trending`, `price`, `market_get`, `points`, `oracle_price`,
`orob_resolve`, `clearing_compute`, `pofq_score`, `stream_status`, `job_status`,
`jobs_pending`, `delegation`, `wallet_info`, `sign_message`.

**Writes (embedded wallet, gated):** `transfer`, `approve`, `stream_open`,
`stream_settle`, `stream_close`, `stream_update_rate`, `schedule_job`,
`cancel_job`, `reschedule_job`, `delegate`, `undelegate`, `redelegate`,
`contract_write`.

## Contract source of truth

- **Swap/DEX execution** uses what is **wired into the wallet**
  (`Paxport-Mobile-Wallet/src/lib/swap/sdk` — PECOR V4 router + Sidiora). The
  wired V4 `PECORRouter` (`0x1D5f…`) is the swap entry, distinct from the docs
  Sidiora.ag aggregator router.
- **Sidiora.fun launchpad** and **HyperPax Perps / DEX v5** addresses come from
  `docs.paxeer.app`. All are pinned in `lib/config.mjs::CONTRACTS` with
  provenance.

## Run + verify

```bash
# crypto + codec self-tests (offline)
node tools/paxeer/lib/keccak.mjs
node tools/paxeer/lib/abi.mjs

# registry loads + lists tools (offline)
node tools/paxeer/paxeer-net.mjs --selftest

# live read smoke (network)
node --input-type=module -e "import('./tools/paxeer/lib/tools.mjs').then(m=>m.dispatch('chain_info')).then(r=>console.log(r.content[0].text))"
```

Wired into an agent via `agents/paxeer.json` (server alias `paxeer-net`).
