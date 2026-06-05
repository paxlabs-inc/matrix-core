---
name: paxeer-defi-guardian
description: Watch a Paxeer wallet's DeFi positions (LP, lending, staking, payment streams), assess concrete risk (IL, liquidation, depeg, stream drain), and execute an authorized protective unwind. Grounded in real reads; never acts on an unread position or claims an action without a tx hash.
origin: Matrix/Paxeer
---

# Paxeer DeFi Guardian

Set-and-watch protection for on-chain positions.

- **`monitor` (read-only)** — read LP/lending/staking/stream positions, price the
  exposure, and report concrete risk + how close each is to danger.
- **`deliver` (protect)** — execute the authorized protective action: exit /
  withdraw / repay, close or settle a stream, or move funds to safety.

## Primitives

- **Read positions** — `portfolio`, `token_balance`, `contract_read` (LP
  reserves + share, lending health factor), `delegation`, `stream_status`.
- **Price** — `oracle_price`, `price`, `market_get`.
- **Protect** — `contract_write` (exit/repay), `stream_close` / `stream_settle`,
  `transfer` (to safety).

## Anti-fake + safety mandate (the planner MUST follow)

1. Every risk number (value, health factor, IL) is grounded in a real read;
   unreadable items are marked **unknown**.
2. Before acting, READ the position + the correct exit function — never act on a
   guessed function or unread position.
3. Act ONLY within the authorized rule/size; unbounded or undetermined exits
   STOP to ask.
4. Every protective action MUST cite a `tx_hash`; `PaxeerSpendPolicy` + custody
   policy are the authoritative gates.

## Reporting

Plain prose: the position, what was done, the resulting state, and tx hash(es).
Persist each action as an `Event`.
