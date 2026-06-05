---
name: paxeer-ape-check
description: "Check this before I ape" — a read-only, pre-trade safety verdict for any Paxeer token or contract. Returns BUY-OK / CAUTION / AVOID with evidence (verification, owner privileges, holder concentration, liquidity, recent activity). No transaction is ever signed.
origin: Matrix/Paxeer
---

# Paxeer Ape Check

The wedge skill: **"check this before I ape."** A user pastes a token or contract
(0x address or symbol) and gets a clear, evidence-grounded safety verdict before
they risk a cent. **Strictly read-only — this skill signs nothing and moves no
value.**

## What it checks (all on-chain, all read-only)

- **Contract** — verified source? age + transaction counters (PaxScan
  `address_overview`).
- **Owner/admin privileges** — mint authority, pause, blacklist/allowlist,
  transfer fee/tax, upgradeable proxy (`contract_read` / `eth_call`).
- **Holder concentration** — top-1 / top-10 share, LP + burn addresses
  (`token_info` top holders).
- **Liquidity + activity** — market depth/price (`market_get`, `price`,
  `trending`) and recent large transfers / LP pulls (`address_transactions`).

## The verdict

One of **BUY-OK**, **CAUTION**, or **AVOID**, followed by the specific red and
green flags — each grounded verbatim in a tool result (the actual top-holder %,
owner address, tax %, liquidity, contract age). Ends with a one-line, plain
bottom line.

## Anti-fake mandate (the planner MUST follow)

1. A verdict REQUIRES real read tool_calls; NEVER judge from memory or reasoning
   alone.
2. Every red/green flag MUST cite the concrete number from a tool result.
3. A check that could not be completed is reported as **unknown** — never
   inferred, never fabricated.
4. This skill has NO write tools: no approval, no transaction, ever.

## Reporting

Brief the user in plain, non-technical prose: the verdict, the why (red/green
flags with real numbers), and a clear bottom line. Persist each material flag as
a `Fact` so a later `monitor` re-check can report what changed. Informational,
read-only, not financial advice.
