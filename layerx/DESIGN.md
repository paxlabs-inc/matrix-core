# LayerX — Design Notes

> Companion to `layerx.frozen.kvx` (the locked spec). This doc holds the
> reasoning, the trust model, and the re-platforming narrative — the prose that
> does not fit the sectioned-KV form. **Status: FROZEN design, locked with Andrew
> 2026-06-15. No code yet.**

## What it is, in one line

**LayerX is the agent-native money + accounting layer of the Matrix economy:** one
always-on sequencer that gives every agent an instant-settling, gasless,
USD-denominated balance (USDX), backed 1:1 by on-chain USDL, with a
cryptographically provable receipt for every transfer, net-settled to Paxeer
mainnet on a tiered schedule. **An agent's DID is its account.**

It is **infrastructure, not a product.** Deus (the service marketplace), the
gateway's LLM metering, and owner-set agent budgets are all *applications that
settle on LayerX* — its utility is not limited to any one of them.

## Why this is mostly an extension, not a new chain

The single hardest thing about any rollup is producing a **deterministic,
tamper-evident, independently-verifiable execution trace** plus a **batched
settlement rail**. Both already exist in this repo:

1. **Verifiable trace / receipt primitive — `cortex/journal`.** The cortex
   journal is an append-only canonical-CBOR log whose leaves are
   domain-separated SHA-256 (`sha256("matrix.cortex.journal.v1" || bytes)`),
   staged into an MMR + SMT → `OverallRoot`, with a byte-identical replay
   invariant (D11). That is exactly the Merkle-receipt + anchor construction a
   settlement layer needs. LayerX reuses the *construction* (its own accumulator,
   domain `layerx.settlement.receipt.v1`) — not the cortex store.

2. **Settlement rail — `deus/internal/{channels,receipts,chain}`.** Deus already
   has EIP-712 co-signed vouchers, a `SettlementAnchor`, an atomic
   conditional-reserve (`channels.go`: conditional `UPDATE` + `RowsAffected`),
   and a chain `Payer`. LayerX is a **single-operator generalization** of the
   Deus *pairwise* voucher ledger into a fleet-wide operator ledger with batched
   settlement.

So "Layer X" is: **Deus's voucher ledger generalized + a deposit-swap vault + a
batched fleet settlement + an agentic tool API.** Weeks of focused work reusing
real code, not a months-long consensus build.

## The correction to the original idea

Andrew's first framing was "a small sequencer in *every* daemon, connecting them
into a chain." That collides fatally with the deployment model: per-user daemons
run on Fly with `auto_stop_machines='suspend'`, `min_machines_running=0` — they
are **asleep ~95% of the time**. You cannot run ordering or consensus across a
fleet that suspends when its user goes idle; liveness dies the moment quorum
sleeps. And "a sequencer in every node" implies *decentralized sequencing*, the
single biggest unsolved problem in the rollup industry.

**Resolution:** one **centralized, always-on sequencer** (`layerxd`, co-located
with the Deus payments plane — same shape as Chronos). The ephemeral daemons are
**clients**, not sequencers. This is the same architectural move Chronos made for
scheduling ("no per-daemon timer that dies on scale-to-zero").

## USDX: fully reserved, no oracle, no FX hole

The naive version of "deposit 1 PAX (≈$7.17) → get 7.17 USDX" hides a stablecoin
failure mode: if the vault keeps the deposit *as PAX* and PAX falls to $5, you now
have 7.17 USDX claims backed by $5 — under-collateralized, the exact pattern that
has killed every algorithmic stable.

**The fix is the elegant part.** Price PAX deposits *at the moment of deposit by
swapping*:

- Deposit **USDL** → mint **1:1 USDX**.
- Deposit **USDC / USDT / PAX** → the vault **atomically swaps to USDL on the
  Paxeer DEX**, and mints USDX equal to the **USDL actually received**.

The DEX swap return **is** both the price oracle and the reserve mechanism, in
one atomic step. No external oracle to trust or manipulate, no FX risk to the
operator — the vault always holds **100% USDL**, and `circulating USDX == USDL in
vault` is auditable on-chain at any block. Andrew's "1 PAX → 7.17 USDX" UX works
exactly as described, because the PAX is sold for ~$7.17 of USDL at deposit.

**Reserve = USDL** (Paxeer-native stable, highest ecosystem volume). This makes
USDX a clean accounting wrapper over the native stable; the explicit trust
assumption is **USDX robustness == USDL robustness** — a deliberate ecosystem
alignment, not a flaw.

**Liquidity is not a concern:** ~$200M PAX and ~$98M combined USDC/USDT/USDL in
the pools means agent-scale deposit swaps have negligible slippage. A `min_out`
guard on every swap reverts (rather than haircuts) in a thin-liquidity edge.

## The trust spine (why a *centralized* sequencer is acceptable)

A single sequencer is a SPOF for **liveness and censorship**, but it is
architecturally incapable of **stealing or forging**:

- **It never holds agent keys.** It holds *signatures*. Agent DID keys stay in
  each daemon (`executor.key`) / embedded wallet — identical to Tachyon's
  multi-tenant token-forwarding posture (the sequencer holds zero agent seeds).
- **It cannot create value.** Every balance delta is backed by a DID-signed
  transfer and bounded by on-chain escrow; v1 issues no unbacked credit.
- **It cannot equivocate undetected.** Signed receipts + batch roots anchored on
  Paxeer mean two conflicting histories are cryptographically provable.
- **It cannot trap funds.** The L1 contract exposes a **unilateral
  force-withdraw**, bounded by the agent's last sequencer-co-signed balance, after
  a timeout. This is the one piece that must be airtight — it is what makes
  centralization safe rather than custodial.

The honest residual: the sequencer can **delay or reorder** within a window. For
an agent micropayment fabric that is an acceptable tradeoff, and the escape hatch
caps the downside.

## DID = account (the ed25519 ↔ EVM bridge)

Agent DIDs are **ed25519** (`did:matrix:<user_id>:<keyfp>`, the daemon's
`executor.key`); Paxeer L1 is **EVM / secp256k1**. So:

- The **ed25519 DID is the LayerX account identity** — it signs all off-chain
  transfer intents.
- On settlement, the contract pays the DID's **mapped Paxeer EVM address** (its
  embedded wallet, `connect.paxportwallet.com` / `/v1/agent/send`).
- A small **DID ↔ EVM binding** is recorded at deposit (the deposit carries a DID
  claim) so settlement nets to the right address.

## Settlement: tiered, not flat 12h

Flat 12h is wrong for material value. 12h batching only pays off for
**high-frequency, low-value** flow, where amortizing one L1 tx across thousands of
transfers drives per-tx gas → ~0.

- **Micropayments** (below `LAYERX_MICRO_THRESHOLD`, e.g. < 1 USDX): netted and
  batch-settled every 12h. **Gasless to the agent.** The headline feature.
- **Material transfers / withdrawals** (at/above threshold, or on demand):
  **force-settle now** — immediate L1 anchor, initiator pays that one gas.

"Free gas" stated precisely: **gasless to the agent, operator-subsidized at the
batch.** The operator funds the single window tx; recover via a thin spread or
carry as a growth cost.

## How the rest of Matrix re-platforms onto it

This is the "it's not just a marketplace" point made concrete. Once the fabric
exists:

- **Agent treasuries** — every agent natively holds a USDX balance under its DID.
  Having money requires no marketplace.
- **Programmable budgets & leashes** — owner spend caps, the wallet leash, the
  `PoliciesPanel`, the gateway credit ledger all become **native LayerX escrow
  allowances** on a DID. A budget is a bounded USDX reserve.
- **Metering / usage billing** — the gateway's per-token LLM metering settles in
  native USDX instead of a side ledger.
- **Conditional / streaming payments** — pay-on-delivery, milestone escrow,
  subscriptions, recurring data/compute rental (scheduled via Chronos), between
  *any* two agents — no listing involved.
- **Deus marketplace** — one consumer: a purchase is just a `layerx_pay` between
  two DIDs.

## The one dependency, resolved

A Paxeer DEX with PAX/stable liquidity was the gating question for the
oracle-free deposit-swap model. **Confirmed:** Paxeer has a working DEX with
multi-stable pools — ~$73M combined USDC/USDT, ~$25M USDL, ~$200M PAX. The clean
fully-reserved USDX model is unblocked, PAX deposits live day one.

## Open decisions (carried in the spec `[deferred]`)

1. USDX precision (USD-cent vs `1e6`/`1e18` fixed-point) + rounding on the
   deposit-swap mint.
2. Force-withdraw timeout + the co-signed-balance proof format (reuse the Deus
   EIP-712 voucher shape).
3. Operator gas-subsidy recovery (thin spread vs free) + who funds the operator
   key.
4. Whether the gateway `credit_ledger` migrates onto LayerX in v1 or later.
5. Single `layerx` DB vs a schema in the shared matrix DB on the payments box.

## Invariants (the non-negotiables)

See `layerx.frozen.kvx [invariants]` — fully reserved, no agent keys held,
escrow-bounded spend, provable receipts, no undetected equivocation, no trapped
funds, DID-scoped, tiered settlement, one always-on sequencer, honest settlement,
strict tool↔manifest bijection.
