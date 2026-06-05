---
name: paxeer-schedule
description: Schedule future on-chain actions via the Scheduler precompile (0x0905) — encode any call, pick a future block, fund a gas deposit, then cancel/reschedule/inspect jobs. The network executes with no agent online. Signs via the embedded wallet; records every job as an Event with jobId + tx hash.
origin: Matrix/Paxeer
---

# Paxeer Schedule

The **autonomy-over-time** rail. An agent can commit a future action now —
"settle this stream at block N", "rebalance at midnight", "retry this swap in
an hour" — and the network executes it at the target block with no agent
online. This drives the Scheduler precompile at `0x0905`.

## How a scheduled job is built

1. **Pick the block.** Read the live head with `chain_info`, then resolve a
   relative request ("in ~1 hour") to an absolute `executeAtBlock` using
   Paxeer's sub-second block cadence. Never schedule into the past.
2. **Encode the action.** Use `encode_call(signature, args)` to produce the
   `callData` for the target precompile/contract method (pure, no send). This
   composes with every other paxeer primitive — a job can call streams,
   staking, a DEX router, anything.
3. **Schedule it.** `schedule_job(target, callData, executeAtBlock, gasLimit,
   deposit)` — the `deposit` (human PAX) funds the future execution's gas.

Manage jobs with `job_status`, `jobs_pending`, `cancel_job` (refunds the
deposit), and `reschedule_job` (move to a new block).

## Custody + enforcement

Signing happens at the network custody service; the gas deposit and any spend
are enforced on the wallet network-side plus the local `PaxeerSpendPolicy`
gate.

## Tool mandate (the planner MUST follow)

1. ALWAYS `chain_info` first so `executeAtBlock` is absolute and in the future.
2. Build `callData` with `encode_call` unless raw calldata is supplied. Only
   schedule the single action the request authorizes.
3. `schedule_job` returns a `jobId` + `tx_hash`; cite both verbatim. Never claim
   a job was scheduled without a `tx_hash` in a tool result.

## Hard guardrails

- Spend ceilings (`PAXEER_MAX_SPEND_WEI` + cortex `Constraint` limits) gate the
  gas deposit. Unbounded deposit or a disallowed target -> STOP and ask.
- A "show my pending jobs" request is read-only and never sends a transaction.

## Reporting

Brief Andrew conversationally: what will run, the target block (with a rough
wall-clock estimate), the gas deposit, and the `jobId` + tx hash — grounded in
tool results. Persist each job as an `Event` (jobId + tx hash) so the chronology
tracks pending autonomous actions.
