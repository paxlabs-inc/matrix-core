---
name: paxeer-audit
description: Read-only smart-contract security review on Paxeer. Pulls verified source (or bytecode), enumerates the privileged surface, flags risky patterns, and returns a severity-rated report (LOW/MEDIUM/HIGH/CRITICAL) with evidence. No transaction is ever signed.
origin: Matrix/Paxeer
---

# Paxeer Audit

Read-only **smart-contract security review** — the audit-firm moat as a skill. A
user hands over a contract address and gets a severity-rated report grounded in
real source/bytecode. **This skill signs nothing and moves no value.**

## What it inspects (all read-only)

- **Source + verification** — `paxscan_get` (Blockscout v2 smart-contract route)
  for verified source/ABI; `address_overview` for verification status + age. An
  **unverified** contract is flagged and never assumed safe.
- **Privileged surface** — `contract_read` / `eth_call`: ownership, EIP-1967
  upgradeability/proxy admin, mint authority, pause, blacklist/allowlist,
  fee/tax setters, withdraw/sweep/rescue, delegatecall/selfdestruct.
- **Recent privileged activity** — `address_transactions`.

## The report

An overall rating — **LOW / MEDIUM / HIGH / CRITICAL** — and an itemised
findings list, each with a severity and the concrete evidence (function
name/selector, source excerpt, or read result).

## Anti-fake mandate (the planner MUST follow)

1. A finding REQUIRES grounding in retrieved source, bytecode, or a read result;
   NEVER report a vulnerability from memory or assumption.
2. Confirmed findings are separated from areas not inspected (marked **unknown**).
3. An unverified contract is flagged as such, never assumed safe.
4. This skill has NO write tools: no transaction, ever.

## Reporting

Plain bottom line: safe to interact with / approve, and under what caution.
Persist each finding as a `Fact`. Automated review — not a substitute for a full
manual audit.
