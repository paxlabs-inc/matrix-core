---
name: paxeer-trade
description: Quote, price, and trade on Paxeer — oracle prices (0x0903), oracle-relative pricing (OROB 0x0901), uniform clearing (0x0902), fill-quality reputation (PoFQ 0x0904), market data, and DEX swaps on the wired routers via the embedded wallet with explicit min-out. Quote before swapping.
origin: Matrix/Paxeer
---

# Paxeer Trade

L3 of the machine-commerce stack: **markets, pricing, and reputation**. Paxeer's
native primitives make pricing and trust *queryable* — agents don't guess a
price or a counterparty's reliability, they read it.

## Pricing + reputation primitives (read-only)

- **Oracle (0x0903)** — `oracle_price(marketId)` returns the validator-quorum
  price for a market.
- **OROB (0x0901)** — `orob_resolve(oraclePrice, offsetBps)` expresses a price
  as a basis-point offset from the oracle reference (the generalized pricing
  surface for machine services).
- **Clearing (0x0902)** — `clearing_compute(...)` runs uniform-price batch
  clearing over buy/sell offset+size arrays.
- **PoFQ (0x0904)** — `pofq_score(fillPrice, oraclePrice)` is Proof of Fill
  Quality: reputation grounded in delivery. Use it to score a fill or vet a
  counterparty.
- **Off-chain / market** — `price` (PAX + bridged majors), `market_get`
  (PaxSpot DEX data).

## Swap execution (write)

Swaps execute on the **wired** DEX routers via `contract_write` (signed by the
embedded wallet). Source of truth for swap contracts is what the wallet wires:

- **PECOR V4 PECORRouter** (primary meta-aggregator entry) `0x1D5f3ac9dE43Dd0665C3F527913dD825f67b3Daa`, OracleHub `0x18DA624C9C5Ff17612EC5fC0A5070611053A180f`
- **Sidiora launchpad** — Router `0xB2D63300FE8b3508A83728e8f36B98e845eBD980`, Quoter `0xeDb3B45E320A8ab2306Fa1C303742f2478fd3E0a`
- **HyperPax DEX (v5 ASAMM)** — Router `0x635aC031f7d26035FCc8b138b0835fec0cf6b8AA`, Quoter `0x2092D242Cc5d3673D1644128DBd4D199dE51266e`
- **HLPMM v2** — Router `0xaedb6bB0451F9CA908f884345dEf5c538ca63022`, Quoter `0x131928667BAB3081A3A47e429052617aF5530D87`

Common ERC-20 router signature: `swapExactTokensForTokens(uint256 amountIn, uint256 amountOutMin, address[] path, address to, uint256 deadline)`. HybridDEX PAX↔stable: `swapPAXForStable(address stable, uint256 minStableOut)` (payable) / `swapStableForPAX(address stable, uint256 amount, uint256 minPaxOut)`.

## Tool mandate (the planner MUST follow)

1. **Quote before swap.** Read a quote (`contract_read` against a Quoter, or
   `oracle_price` + `orob_resolve`) and compute an explicit **min-out** from the
   requested slippage. Never swap without a slippage bound.
2. Check the input balance first; for an ERC-20 input, `approve` the router
   before the swap.
3. Execute with `contract_write` to the wired router, passing min-out so a bad
   price reverts. Cite the `tx_hash` verbatim; optionally score the fill with
   `pofq_score`.

## Hard guardrails

- Spend ceilings (`PAXEER_MAX_SPEND_WEI` + cortex `Constraint` limits) are
  enforced below the plan. Unbounded size or missing slippage -> STOP and ask.
- `analyze` (pricing/reputation) never moves value.

## Reporting

Brief Andrew conversationally: pair, size, quoted vs min-out price, tx hash, and
fill-quality if known — grounded in tool results. Persist each fill as an
`Event` (tx hash + PoFQ score) for the chronology and future counterparty vetting.
