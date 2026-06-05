---
name: paxeer-guard
description: Audit and clean up a Paxeer wallet's risk surface. Read-only enumeration of live ERC-20 approvals + exposure, then one-tap revoke (allowance -> 0) of the risky ones through the embedded wallet. Never claims a revoke without a real tx hash.
origin: Matrix/Paxeer
---

# Paxeer Guard

Wallet hygiene as a skill: **see what can drain you, then shut it off.**

- **`analyze` (read-only)** — enumerate outstanding ERC-20 approvals, read the
  current on-chain allowance for each `(token, spender)`, label the spender
  (verified? unlimited?), and rank by danger.
- **`deliver` (revoke)** — set the allowance to `0` (`approve(token, spender, 0)`)
  for the approvals the user authorizes, through the embedded wallet.

## Primitives

- **Discover** — `address_transactions` / `paxscan_get` for Approval history.
- **Confirm** — `contract_read` `allowance(address,address) -> uint256` (always
  read the live allowance before revoking; skip those already `0`).
- **Revoke** — `approve` with amount `0`.

## Anti-fake + safety mandate (the planner MUST follow)

1. Always read the live allowance before a revoke; never revoke a `0`.
2. Every revoke MUST cite the `tx_hash` from a wired tool result; never claim a
   revoke without one.
3. Revoke ONLY what the request authorizes; an ambiguous "revoke everything"
   STOPS to confirm.
4. The wallet custody policy + `PaxeerSpendPolicy` + cortex `Constraint`s are the
   authoritative gates — if a revoke is denied, report it honestly.

## Reporting

Plain prose: which approvals were revoked (token, spender), the tx hash(es), and
the resulting allowance. Persist each revoke as an `Event`.
