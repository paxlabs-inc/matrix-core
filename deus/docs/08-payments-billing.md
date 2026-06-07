# 08 — Payments, Billing & Economics

## 8.1 Principles

1. **Take-nothing.** Platform fee = **0**. Developers keep **100%**. Paxeer
   monetizes the on-chain activity Deus generates (gas, settlement, quality
   writes), not a cut of revenue.
2. **Pay per call, down to fractions of a cent.** No per-service accounts, API
   keys, or human onboarding. The caller's wallet pays for exactly what it used.
   (The caller does carry a wallet and, on the channel rail, a short-lived
   prepaid float — see §8.3 for the honest framing of "no accounts.")
3. **Native rails.** All settlement is on Paxeer; instant finality; no external
   payment network.
4. **Custody stays with the caller's wallet.** Deus never holds caller keys and
   never becomes a second spend authority — the embedded wallet policy plane +
   Argus authorize every spend.

## 8.2 The three rails

| Rail | Shape | Mechanism | Default for |
| ---- | ----- | --------- | ----------- |
| **Direct** | single call | inline transfer via caller wallet `agent/send` before result release | **the launch MVP rail** + expensive one-shot calls |
| **Net settlement (channel)** | many tiny calls | per-window channel + caller vouchers → batched on-chain payout per developer per window | per-call APIs (steady-state default; **fast-follow** after MVP) |
| **Stream** | continuous/long | PaymentStreams `0x0906` rate-per-second | sessions, subscriptions-by-the-second, agent services that run a while |

### Why net settlement is the default
Sub-cent calls cannot each afford a chain write. The gateway meters off-chain
into the **append-only ledger** (`invocations`), then once per developer per
window (~seconds to minutes), the **Settlement** component:
1. Selects `finalized`, unsettled ledger rows for the developer.
2. Sums `price_wei` → `total_wei`.
3. Builds a Merkle tree of the covered receipts → `receiptsRoot`.
4. Executes **one** transfer to the developer's `payout` and calls
   `SettlementAnchor.anchor(developer, receiptsRoot, total_wei, count)`.
5. Marks the rows `settled` with the `settlement_id` + `tx_hash`.

This is the lazy-net pattern (5–10× cheaper settlement) applied to services. On
the caller side it runs over per-caller **payment channels**: the caller funds a
channel once per window and co-signs a cumulative voucher per call, so neither
side needs a chain write per invocation (§8.3).

## 8.3 Funding model — the per-window payment channel (D-7a, decided)

The caller is charged at **reserve** and the developer is paid at
**settlement**; the PAX in between is held without Deus ever holding caller keys.
The model is a **unidirectional payment channel** per caller, **funded per
window — not per call**. This is the only construction consistent with *both*
lazy-net economics (§8.2) *and* the "minimize trust in Deus" threat model
(§9.1):

1. **Open / fund — one chain write per caller per window.** The caller's wallet
   funds a per-caller escrow (a Deus settlement-module contract, caller-signed,
   policy-checked) up to a window cap. Funding is **per window, not per
   reserve** — the 5–10× lazy-net saving lives here, so a per-reserve deposit is
   explicitly **forbidden** (it would reintroduce a chain write per call).
2. **Pay — off-chain, bilaterally signed.** On each invocation the gateway
   returns a **monotonically increasing cumulative voucher**
   `{channel_id, cumulative_wei, nonce, last_receipt_hash}` and the **caller
   co-signs it** (EIP-712, `0x0908`). The voucher is the caller's signed
   admission of total spend so far, so the charge is now **bilaterally
   provable** rather than gateway-attested. This is the fix for the
   gateway-only-attestation gap and is **mandatory for `per_unit`** (where the
   gateway would otherwise solely attest the unit count) and hardening for
   `per_call`.
3. **Settle — lazy, one chain write per developer per window.** Settlement
   submits the **highest co-signed voucher**; the escrow releases exactly that
   cumulative amount, split across the window's developers, and anchors the
   receipts root (§4.2). The caller cannot be charged beyond its last signed
   voucher; Deus cannot pay out more than the caller admitted.
4. **Close / refund.** At window end (or on caller request) the unspent escrow
   remainder is returned or rolled into the next window.

Funding the channel and co-signing vouchers are **one mechanism**: this unifies
the receipt-signing scheme with the funding model. A **reserve** is an *atomic
decrement* of the channel's available balance (see [`06-execution-hosting.md`](./06-execution-hosting.md)
§6.2) — never a chain write.

**Honest tradeoff (owned, not hidden):** a per-window-funded channel is a
**short-lived prepaid balance**. So "no accounts/subscriptions" precisely means
*no per-service accounts, no API keys, no human onboarding* — the caller still
carries a wallet and a rolling prepaid float. That remains a categorical
improvement over per-service signup, but the spec does not pretend the float
doesn't exist.

**Fallbacks (no channel required).** A caller that declines a channel is served
on the **direct rail** (one inline transfer per call, §8.2) or **streaming**
(`0x0906`), both of which keep the cap on-chain and need no escrow. The launch
MVP ships **direct-rail-only** (see [`14-roadmap.md`](./14-roadmap.md) Phase 2);
the channel is the immediate fast-follow.

> Invariant: a caller is never charged beyond its **last co-signed voucher**,
> and a developer is always paid exactly the sum of finalized receipts in the
> anchored batch. **Both sides hold cryptographic proof** — not just the
> developer.

## 8.4 Pricing model

Defined per operation in the manifest, committed on-chain via `pricingHash`:
- `per_call` — flat `price_wei` per invocation.
- `per_unit` — `price_wei * units` (tokens, requests, bytes…), with
  `min_charge_wei` floor.
- `per_second` — stream rate (`ratePerSecond` on `0x0906`).

Pricing math is a **pure, versioned function** (`pkg/pricing`) used identically
by the quote endpoint, the gateway charge, and settlement — so quote == charge ==
settled, always. A pricing change bumps `pricing_plans.version` and re-registers
`pricingHash`; in-flight quotes pin the old version until they expire.

## 8.5 Quotes (price promises)

- `POST /v1/quote` returns an **EIP-712-signed** quote: `{unit_price_wei,
  max_units, max_total_wei, pricing_version, expires_at, signature}`.
- The agent verifies the signature (`recoverTypedSigner` on `0x0908`) and that
  `max_total_wei` fits its spend policy **before** committing.
- `invoke` must reference a valid, unexpired quote whose `pricing_version`
  matches a currently-registered `pricingHash`. Mismatch → `quote_expired`.

## 8.6 Receipts & auditability

Every finalized invocation produces an **EIP-712 receipt**
([`04-onchain.md`](./04-onchain.md) §4.5): gateway-signed, runner-co-signed for
hosted/confidential, and — on the channel rail — **caller-co-signed via the
cumulative voucher (§8.3)**. Receipts are stored in the object store and hashed
into the settlement Merkle root. A developer or caller can prove any single
charge was (or wasn't) included in a settled batch via Merkle proof against the
on-chain `receiptsRoot`. The caller's voucher signature is what makes the charge
provable *to the caller*, not only auditable by the developer. This is the
dispute-evidence substrate (arbitration itself is post-v1).

> **Evidence, not proof-of-correctness.** `result_hash` binds *"these are the
> bytes you received"* — it is not proof the answer was *correct*. For
> non-deterministic agent/LLM services, correctness is unverifiable outside the
> TEE path (and even there the attestation proves the attested code ran, not
> that its output is "right"). Treat receipts as evidence of what was returned
> and charged, not as a correctness oracle.

## 8.7 Refunds & failures

| Situation | Billing outcome |
| --------- | --------------- |
| Service error / timeout / schema-invalid | `voided` — no charge; failure quality sample |
| Partial delivery (per-unit) | charged for delivered units only |
| Policy denied / quote expired | no call, no charge |
| Confidential attestation fails | `voided` — payment never released |
| Settlement tx fails | rows stay unsettled; retried; never double-paid (idempotent) |

## 8.8 Developer earnings

- Real-time accrual visible in `GET /v1/services/{id}/analytics` and the console.
- Paid to `payout` per settlement window; developer sets `payout` (defaults to
  owner wallet).
- 100% of `price_wei` reaches the developer. Gas for the settlement tx is paid by
  the Deus settler (a network cost, amortized across the batch — not deducted
  from the developer), or relayed via the agent fee lane at the lane gas price.

## 8.9 What Paxeer earns (the business model, for context)

Not from Deus fees. From the **activity** Deus drives: gas on registrations,
settlements, quality writes, streams, and attestations; PAX demand from callers
funding wallets; and the network effect of being the canonical agent-service
registry. Deus is a **demand generator** for Paxeer, not a tollbooth.

## 8.10 Metering integrity

- Ledger is append-only; settlement reads, never edits.
- Idempotency keys dedup retries (no double charge).
- Reserve/finalize/void state machine prevents charging for non-delivery.
- Pricing + receipt hashing are deterministic and reproducible for audit.
- All amounts are integer PAX wei (string big-int) end-to-end — no float drift.
