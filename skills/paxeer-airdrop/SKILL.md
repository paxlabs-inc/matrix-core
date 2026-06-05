---
name: paxeer-airdrop
description: Check airdrop/claim eligibility on Paxeer and claim what is owed. Read-only eligibility from a confirming contract read, then claim via the embedded wallet only when a nonzero claimable is confirmed. Never reports a claim without a real tx hash.
origin: Matrix/Paxeer
---

# Paxeer Airdrop

Find what you're owed and collect it — grounded in real contract reads, never
guessed.

- **`analyze` (read-only)** — resolve the distributor contract, read its ABI to
  find the eligibility view (`claimable(address)`, `isEligible(address)`, ...),
  and report the claimable amount + token.
- **`deliver` (claim)** — execute the claim via `contract_write` only when a read
  confirmed a nonzero claimable and the user authorized it.

## Primitives

- **Resolve** — `search` / `paxscan_get` (ABI) / `wallet_info` (recipient).
- **Confirm** — `contract_read` of the discovered eligibility view; `token_info`
  for decimals/symbol; `points` for off-chain reward balances.
- **Claim** — `contract_write` against the distributor (inspect with
  `encode_call` first if needed).

## Anti-fake + safety mandate (the planner MUST follow)

1. Eligibility is ALWAYS grounded in a contract read — never asserted from
   memory.
2. Claim only the EXACT function discovered from the verified ABI; an unknown
   interface/amount STOPS to ask.
3. Every claim MUST cite the `tx_hash` from a wired tool result.
4. `PaxeerSpendPolicy` + cortex `Constraint`s + network custody policy are the
   authoritative gates; report any denial honestly.

## Reporting

Plain prose: campaign, amount + token, tx hash. Persist each claim as an `Event`.
