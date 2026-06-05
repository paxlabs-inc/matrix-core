---
name: paxeer-stake
description: Stake PAX on Paxeer via the staking precompile (0x0800) — delegate, undelegate, redelegate, and read delegations. Validators are bech32 paxvaloper.... Signs via the embedded wallet; confirms balance and records every action as an Event with its tx hash.
origin: Matrix/Paxeer
---

# Paxeer Stake

The capital layer: an agent can commit PAX as **stake** to secure the network
and earn yield — the building block of agent-capital assignment and
Programmable-Liquidity-Vault-style underwriting (back service capacity with
staked capital). This skill drives the staking precompile at `0x0800`.

## Primitives

- **`delegate`** — stake PAX to a validator (`validator` is a bech32
  `paxvaloper...` string; `amount` in human PAX).
- **`undelegate`** — begin unbonding from a validator (returns a completion
  time after the unbonding period).
- **`redelegate`** — move stake from one validator to another without a full
  unbond.
- **`delegation`** (read) — current shares + token balance for a
  delegator/validator pair.

## Custody + enforcement

Signing happens at the network custody service; spend authority is enforced on
the wallet network-side plus the local `PaxeerSpendPolicy` gate. Amounts are
human units (the bridge converts at 18 decimals for PAX).

## Tool mandate (the planner MUST follow)

1. Before delegating, ALWAYS `get_balance` so the report can confirm the agent
   keeps enough liquid PAX for gas and any required self-balance. Prefer also
   reading `delegation` for the target validator first.
2. Only stake the amount the request authorizes — never sweep the whole
   balance unless explicitly told.
3. Every successful action returns a `tx_hash`; cite it verbatim. Never claim a
   delegation/undelegation succeeded without a `tx_hash` in a tool result.

## Hard guardrails

- Spend ceilings (`PAXEER_MAX_SPEND_WEI` + cortex `Constraint` limits) are
  enforced below the plan. If staking would leave too little for gas, or the
  amount is unbounded, STOP and ask.
- A "show my delegations" request is read-only and never sends a transaction.
- Unbonding locks funds for the network unbonding period — state this in the
  report when undelegating so the user isn't surprised by the delay.

## Reporting

Brief Andrew conversationally: validator, amount, resulting delegation/liquid
balance, unbonding completion time when relevant, and the tx hash — grounded in
tool results. Persist each action as an `Event` (tx hash) for the chronology.
