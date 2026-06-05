---
name: paxeer-read
description: Read anything on Paxeer — direct node RPC, the PaxScan explorer, the Argus portfolio + PaxSpot market indexers, price feeds, and agent-economy precompile views (oracle, OROB, PoFQ, streams, scheduler, staking). Strictly read-only.
origin: Matrix/Paxeer
---

# Paxeer Read

The data layer agents get on Paxeer that no other chain offers: **direct node
RPC, the full PaxScan explorer API, every protocol indexer, and the native
agent-economy precompile views** — all read-only, all behind one skill. This is
how an agent answers "what is the price", "what's in this wallet", "who holds
this token", "is my stream still paying", "what's my reputation" before it acts.

## Sources (route to the right one)

- **Direct node (EVM JSON-RPC)** — `chain_info`, `rpc_call`, `eth_call`,
  `contract_read`, `get_balance`, `token_balance`. Ground truth, lowest level.
- **PaxScan (Blockscout v2 explorer)** — `address_overview`,
  `address_transactions`, `tx`, `token_info`, `search`, `network_stats`, and
  the generic `paxscan_get` passthrough for any other documented route.
- **Argus portfolio indexer** — `portfolio` (pnl, rank, performance).
- **Markets** — `trending`, `price` (PAX + bridged majors), `market_get`
  (PaxSpot DEX data), `points` (rewards).
- **Agent-economy precompiles** — `oracle_price` (0x0903 validator price),
  `orob_resolve` (0x0901 oracle-relative price from a basis-point offset),
  `stream_status` (0x0906), `job_status` (0x0905), `delegation` (0x0800).

## Tool mandate

1. A network-data question ALWAYS produces at least one read tool_call, then
   one report step that consumes it. Never answer chain data from memory.
2. Prefer `address_overview` for "tell me about this address" (it bundles info
   + counters + token balances). Use `paxscan_get` / `market_get` for any
   documented route not covered by a named tool — nothing is locked out.
3. Quote concrete values (balances, prices, holder counts, tx hashes) verbatim
   from the tool result. If a tool returns empty, say so — never fabricate.

## Guardrails

Strictly read-only. This skill sends no transactions and needs no wallet auth.
Anything that moves value belongs to `paxeer-pay`, `paxeer-trade`,
`paxeer-stake`, or `paxeer-schedule`.

## Reporting

Answer Andrew in natural, conversational prose — plain sentences plus short
bullet lists for multi-entity results — grounded entirely in the tool results
wired into the report step. Persist notable findings as `Event` or `Fact`
memories when they're worth remembering (e.g. a watched address's balance).
