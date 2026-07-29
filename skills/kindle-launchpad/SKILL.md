<!-- Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol -->
<!-- Contact · license@Paxeer.app · legal@Paxeer.app -->

# KindleLaunch Autonomy (`kindle-launchpad`)

The skill that lets Neo **launch**, **trade**, **watch**, **collect fees**, and
**create opticals** for a user on **KindleLaunch** — the Sidiora launchpad on
**Paxeer Network, chain id 125**. This prose body is the executor-LLM's working
brief; the `SKILL.mtx` manifest is the planner's contract. Both carry the same
two non-negotiables: **key isolation** and **no fakes**.

## The one rule that shapes everything: key isolation

Neo holds **no signing key**. This skill holds no key. Every value-moving step is
a **kindle WRITE tool** whose calldata is signed and broadcast **server-side** by
the per-user **embedded wallet** (Paxport, `/v1/agent/*` lane, ed25519 DID auth)
under the user's **one-time leash**:

- **mode** ∈ `read_only | trade_only | full`
- **spend caps** (per-action / daily)

The leash *is* the user's approval. The user is **absent and signs nothing per
action**. The wallet enforces mode + caps and returns a **`403 {error: CODE}`**
when an op is outside the leash (`FROZEN`, `READ_ONLY`, `MODE_NOT_ALLOWED`,
`CAP_EXCEEDED`). A 403 is a **clean denial** — surface it in plain language
("your wallet is set to read-only", "that would exceed your daily cap") and
**never retry it as if transient**.

Reads (`kindle_pools`, `kindle_market`, `kindle_quote`, `kindle_fees`,
`kindle_optical_info`, `kindle_position`) need no signing and never move value.

## Money math

All amount / price / slippage / fee arithmetic is **integer base-unit math** —
never floats. Use each token's exact decimals:

- **USDL** = 6 decimals (the quote token + creation fee)
- **SID** = 6 decimals
- native **PAX** = 18
- **launched tokens** = 18 unless the token reports otherwise (the tools resolve
  ERC-20 `decimals()` on-chain; never assume a uniform 18)

`minOut = quoted × (10000 − slippageBps) / 10000`, computed on the base-unit
quote. The read/quote tools return both the raw base-unit value and the
human-decoded figure; report the human figure to the user.

## Verb routing

| Verb | Mode | Covers |
|---|---|---|
| `build` | create new | **Launch** a token/market; **create an optical** (preset or custom) |
| `acquire` | move value | **Trade** (buy/sell/swap); **collect fees** |
| `monitor` | recurring | **Watch** a pool/position/fee balance and act on a trigger |
| `analyze` | read-only | Pools, market detail, quotes, pending fees, optical info, positions |

## Capability playbook

### 1. Launch (`build`)
`kindle_launch(name, symbol, feeStrategy, optical?, …metadata)`:
- `feeStrategy` ∈ `CLAIM | BURN | AIRDROP | LP_REWARDS` → enum `0..3`. Reject
  anything else with a plain error; never guess.
- `optical` = a preset name, a `0x` address, or omitted (`none` → zero address).
- The tool ensures the **USDL creation-fee allowance** to the Router, calls
  `Router.createMarket`, parses `MarketCreated(token, pool, creator, nftId)` from
  the receipt, and **publishes the display metadata** (description / socials /
  logo / banner) so the token **shows up on the frontend** — `createMarket`
  alone only registers the pool.
- Returns token + pool + **nftId** + explorer link. **Record the pool and
  nftId** — later fee collection needs the nftId.
- `dry_run: true` returns the planned calls without sending.

### 2. Trade (`acquire`)
Quote first: `kindle_quote(pool|token, amount, side, slippageBps?)` → expected
out + quote-bounded `minOut`. Then `kindle_buy` (USDL in), `kindle_sell` (tokens
in), or `kindle_swap` (token→USDL→token). Each write re-derives `minOut` from a
**fresh Quoter read**, ensures the input token is approved to the Router, sets a
**finite deadline**, and passes base-unit amounts. **Never trade without a
slippage-bounded minimum out.**

### 3. Collect fees (`acquire`)
`kindle_fees(pool)` to show pending fees, then `kindle_collect_fees(nftId|pool)`.
The tool **verifies the embedded wallet owns the pool's `SidioraNFT`** and routes
to the function matching the pool's **current** strategy:

| Strategy | FeesRouter call |
|---|---|
| `CLAIM` (0) | `claimFees(nftId)` |
| `BURN` (1) | `executeBurn(nftId)` |
| `AIRDROP` (2) | `executeAirdrop(nftId)` |
| `LP_REWARDS` (3) | `executeLpRewards(nftId)` |

If the wallet is not the owner, the strategy doesn't match, or there are no fees,
it **declines with a plain reason** rather than sending a tx that reverts. To
change strategy, `kindle_set_fee_strategy(nftId|pool, strategy)` first.

### 4. Watch (`monitor`)
Recurring autonomy on the chronos scheduler:
1. `alarm_set(kind=once|cron, …, wake_message, payload?)` — the **`wake_message`
   is the only context delivered to the agent on wake**. The scheduler →
   router → daemon `/chat` hop forwards the message text + conversation only;
   the `payload` is kept server-side for dedup/audit and is **not** delivered.
   So make the `wake_message` a **complete, self-contained** instruction:
   pool/token address, trigger condition (price ≥ X, pending fees ≥ Y, position
   ≤ Z), the action to take when met (with size + slippage), and the leash
   bounds. Use a stable `idempotency_key` so a re-delivered wake isn't
   double-acted. Confirm the watch to the user.
2. **On wake**: re-read current state via the kindle read tools, evaluate the
   trigger. **If met** → dispatch the trade/fee action through `core_execute`
   (subject to the leash) and record the outcome. **If not met** → take no
   value-moving action; a `cron` watch stays, a `once` watch reschedules.
3. **Manage**: `alarm_list` / `alarm_get` / `alarm_cancel`; describe each watch
   in plain language.

**Task durability**: a watch-triggered task is decoupled from the user's
connection and survives daemon restart / Fly suspend — it continues to
completion on a fresh agent if the current one fails. A 403 leash denial **stops
the action**, is surfaced plainly, and the watch is kept or cancelled per the
user's instruction — **never a silent retry loop**.

### 5. Create opticals (`build`)
Opticals are Uniswap-v4-hook-style pool plugins (`IOptical`: `beforeSwap` /
`afterSwap` / `beforeFeeDistribution` / `afterFeeDistribution` + a `getFlags()`
bitmap; `BaseOptical` gives no-op defaults + `immutable poolRegistry, owner`,
`onlyPool`, `onlyOwner`).

- **Tier 1 — preset:** `kindle_optical_info` (no args) lists the deployed presets
  (AntiSnipe, MaxWallet, Tax, Cooldown, BuybackBurn). Attach one by passing its
  address as `kindle_launch optical=<address>` — no compile/deploy. For a
  configured per-creator instance, `kindle_create_optical_preset(preset=…)`
  deploys via `LaunchpadOpticalFactory` **only if** the factory is on the
  manifest; otherwise it says so plainly and offers the singletons or the custom
  path.
- **Tier 2 — custom:** `kindle_create_optical_custom(name, hooks[], bodies?,
  immutables?, notes)` authors a `BaseOptical`-derived contract overriding **only**
  the named hooks, with a `getFlags()` bitmap that **exactly matches** the
  overrides (bit0 `beforeSwap`, bit1 `afterSwap`, bit2 `beforeFeeDistribution`,
  bit3 `afterFeeDistribution`) and constructor args `(poolRegistry, owner)`. Then
  build it locally with Foundry: **`forge build` → `forge test -vv` → (only on
  green) broadcast the creation transaction with `contract_write`** on chain 125
  with constructor `[poolRegistry, embedded-wallet owner]` and an idempotency
  key (an `eth_call` dry-run first where useful).
  **Never deploy on a failed compile or failed test.** The custom template keeps
  `onlyPool` on any value-gating hook and the optical immutable — do not generate
  a contract that bypasses the pool-registry caller check.

A freshly deployed optical is **functional immediately**; `OpticalRegistry`
registration is an **admin-only "verified" signal** the user cannot self-grant —
it never blocks use or attachment.

## Canonical addresses (chain 125, 2026-06-20 manifest)

Bound in `tools/kindle/lib/config.mjs` (every value env-overridable). **Not** the
`paxeer-net` `sidioraFun` block (a different/older deployment).

| Contract | Address |
|---|---|
| Router | `0xCC7298801112682e10ee14b8a520309caD80336d` |
| FeesRouter | `0x02Df12a44F2658080E76fbcF7D6B34Baa97843b6` |
| FeeAccumulator | `0x50C69dF6637b3DCE6a7407C5A4b4F99E68514A76` |
| Quoter | `0xB768e183b6EfDeDf8b2AA7af732039D1C3c452d0` |
| PoolRegistry | `0x7684382c89f79104574D8EF9b31eFf2eD2C2BA0b` |
| SidioraNFT | `0xDF73b354ed9dcB473cc9D01541c46f507591e190` |
| SidioraFactory | `0x8a1A09CEe72c1D39dF33B8284E38baeF8371f465` |
| OpticalRegistry | `0x4CdA6e48632d51Ee4Fa735D81BF09F7543f644a1` |
| USDL (6-dec) | `0x85FcD13735F4309833A503EE804ea32395851479` |

## Anti-fake mandate

Every claimed outcome is grounded in a **real tool result**: a tx hash, a quote,
a balance, an explorer link, a compile/test result. Never report a launch, fill,
collection, or deploy as done without the on-chain reference. A `dry_run` plan is
a plan, not a result. If a read misses or a view reverts, say so plainly — do not
invent a number.

## Consumer transparency

Speak in outcomes, not protocol internals. Show "token launched", "bought X for
Y USDL", "fees collected", "optical deployed at …", with an **explorer link** —
never `MCL`, `cortex`, `Merkle`, `replay`, `SSE`, `envelope`, `calldata`,
`selector`, or a raw revert/stack trace. On a denial, give the human reason
("your wallet is set to read-only", "over your daily cap", "you don't own this
pool", "price moved past your slippage", "no fees yet").
