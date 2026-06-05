---
name: paxeer-forensics
description: "Where did my money go?" — read-only on-chain investigation on Paxeer. Traces value hop by hop from a tx hash, address, or victim wallet, labels each address, finds the sink(s), and produces a readable incident report. No transaction is ever signed.
origin: Matrix/Paxeer
---

# Paxeer Forensics

Read-only **fund tracing + incident investigation**. A user hands over a tx
hash, an address, or "I got drained" and gets an ordered, evidence-grounded
trail of where the value went. **This skill signs nothing and moves no value.**

## How it traces (all read-only)

- **Anchor** — `tx` (full transaction detail) or `search` + `address_overview`
  for an address/symbol.
- **Follow** — `address_transactions` hop by hop on each receiving address,
  prioritising the largest outflows that match the incident.
- **Label** — `address_overview` on every address (EOA vs contract, verified
  name, holdings); `token_info` when a specific token is traced.
- **Stop** — at a sink (exchange/bridge deposit, dormant holder) or the hop
  budget in `constraints`.

## The report

An ordered hop list (`from -> to`, amount, token, **tx hash verbatim**), each
address labelled, the identified sink(s), and total traced vs unaccounted.

## Anti-fake mandate (the planner MUST follow)

1. A trace REQUIRES real read tool_calls; NEVER reconstruct a trail from memory.
2. Every address, amount, and hash is quoted verbatim from a tool result.
3. A hop that could not be resolved is marked **unknown** — never invented.
4. This skill has NO write tools: no transaction, ever.

## Reporting

Plain prose for a non-technical victim: the trail, the sink, what is
recoverable, and where it was reported. Persist each labelled address/sink as a
`Fact`. Investigative, not legal advice.
