# 08 — Payments, Billing & Economics

## 8.1 Principles

1. **Take-nothing.** Platform fee = **0**. Developers keep **100%**. Paxeer
   monetizes the on-chain activity Deus generates (gas, settlement, quality
   writes), not a cut of revenue.
2. **Pay per call, down to fractions of a cent.** No accounts, keys, or
   subscriptions. The caller's wallet pays for exactly what it used.
3. **Native rails.** All settlement is on Paxeer; instant finality; no external
   payment network.
4. **Custody stays with the caller's wallet.** Deus never holds caller keys and
   never becomes a second spend authority — the embedded wallet policy plane +
   Argus authorize every spend.

## 8.2 The three rails

| Rail | Shape | Mechanism | Default for |
| ---- | ----- | --------- | ----------- |
| **Net settlement** | many tiny calls | off-chain meter → batched on-chain payout per developer per window | per-call APIs (the default) |
| **Stream** | continuous/long | PaymentStreams `0x0906` rate-per-second | sessions, subscriptions-by-the-second, agent services that run a while |
| **Direct** | single high-value | inline transfer via caller wallet `agent/send` before result release | expensive one-shot calls |

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

This is the lazy-net pattern (5–10× cheaper settlement) applied to services.

## 8.3 Funding model — who holds the float between call and settlement?

The caller is charged at **reserve** time, the developer is paid at
**settlement** time. The PAX in between must be custodied without Deus holding
caller keys. Options (decision **D-7a**, pick at implementation):

- **(A) Reserve-to-escrow (recommended).** At reserve, the gateway has the
  caller wallet move `max_total_wei` into a **per-caller escrow** controlled by a
  Deus settlement module contract (caller-signed, policy-checked). Finalize
  draws the real charge; the remainder is returned at window close or reused for
  the caller's next calls. Settlement pays developers from escrow. Caller funds
  never leave a contract the caller can audit; Deus can only move funds per
  signed, anchored receipts.
- **(B) Prepaid balance.** Caller tops up a Deus balance (on-chain deposit);
  calls debit it; refundable on withdrawal. Simpler UX, weaker "no account"
  story.
- **(C) Streaming-only for untrusted callers.** Force a stream for any caller
  without escrow, so the chain holds the cap.

v1 default: **(A)** with a short window so float is minimal; **(C)** as the
fallback when a caller declines escrow.

> Whichever is chosen, the invariant holds: **a caller is never charged more
> than its signed quote, and a developer is always paid exactly the sum of
> finalized receipts in the anchored batch.**

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
([`04-onchain.md`](./04-onchain.md) §4.5), stored in the object store and hashed
into the settlement Merkle root. A developer or caller can prove any single
charge was (or wasn't) included in a settled batch via Merkle proof against the
on-chain `receiptsRoot`. This is the dispute-evidence substrate (arbitration
itself is post-v1).

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
