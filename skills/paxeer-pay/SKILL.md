---
name: paxeer-pay
description: Pay on Paxeer — send native PAX or ERC-20 tokens and open/settle/close continuous PaymentStreams (0x0906) for per-second machine settlement. Signs via the embedded wallet; confirms funds first and records every send as an Event with its tx hash.
origin: Matrix/Paxeer
---

# Paxeer Pay

L2 of the machine-commerce stack: **machine payments**. Agents pay for tools,
compute, data, and services — often in small, continuous amounts. This skill
covers both one-shot transfers and streaming settlement.

## Primitives

- **Transfer** — `transfer` (native PAX or any ERC-20 by symbol/address) for a
  one-shot payment; `approve` when a contract must pull funds.
- **Payment streams (0x0906)** — `stream_open` starts a per-second payment to a
  payee (rate, optional cap/stop time); `stream_settle` pays out accrued;
  `stream_update_rate` retunes it; `stream_close` ends it and refunds the
  remainder; `stream_status` reads accrued + state. This is the rail for
  metered machine work (pay-as-you-use compute/inference/data).

## Custody + enforcement

Signing happens at the network custody service; the agent's spend authority is
enforced **on the wallet at the network layer** plus a local `PaxeerSpendPolicy`
gate. Amounts are given in human units — the bridge converts by token decimals.

## Tool mandate (the planner MUST follow)

1. ALWAYS check funds first with a read-only `get_balance` (native) or
   `token_balance` (ERC-20) before a send, so the report can confirm coverage.
2. Pick the primitive: one-shot -> `transfer`; continuous/metered -> a stream.
   Only emit the sends the request authorizes — never add extra transfers.
3. Every successful write returns a `tx_hash`. The report MUST cite it verbatim.
   Never claim a payment landed without a `tx_hash` in a tool result.

## Hard guardrails (enforced by policy, not optional)

- The per-call native ceiling (`PAXEER_MAX_SPEND_WEI`) and any cortex
  `Constraint` spend limit are enforced below the plan. Do not route around
  them. If an amount is unbounded or exceeds a known limit, STOP and ask.
- A read-only status/monitor request NEVER triggers a settle, close, or
  transfer.

## Reporting

Brief Andrew in natural, conversational prose: what was paid or streamed, to
whom, how much, the tx hash(es), and the balance impact — all grounded in the
tool results. Persist each send/settle as an `Event` (with tx hash) so the
chronology and `/memory/recent` stay accurate.
