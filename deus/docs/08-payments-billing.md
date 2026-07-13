# 08 — Payments: the LXP protocol (lxp/1)

LXP is HTTP-native payments on LayerX. Every deus payment is USDX moving
payer-DID → payee-DID on the LayerX ledger, authorized in-band by the HTTP
exchange itself: an unpaid request answers `402 Payment Required` with signed
terms, the payer signs the canonical LayerX intent with its own ed25519 DID
key, retries once, and the response carries the settlement receipt. No
accounts, no API keys, no per-call gas, no third-party facilitator — and no
deus custody: deus (or any service speaking LXP) only transports signatures.

## 8.1 Principles

1. **Take-nothing.** Platform fee = 0. Settlements pay the service's
   `payee_did` directly on LayerX; developers keep 100% and withdraw through
   LayerX's own `/v1/withdraw` — deus has no payout machinery.
2. **The signature IS the authorization** (LayerX invariant i6). No payment
   executes without the payer's ed25519 signature over the canonical intent
   preimage. Nonces are single-use, TTL-bound, and DID-bound; replay is
   structurally impossible.
3. **No free calls.** layerxd unreachable or payment invalid means the
   service does not execute (`503` / fresh `402`). There is no code path that
   serves a priced call unpaid.
4. **No stranded funds.** Hold-mode reservations live inside the payer's own
   account, capture only within payer-consented bounds, release on execution
   failure, and expire on a TTL swept by the ledger.

## 8.2 Wire format

```
1 -> POST /v1/invoke/{service}                    (no payment attached)

2 <- 402 Payment Required
     { "error":  "payment_required",
       "reason": "payment_required",              // machine-readable, see 8.5
       "lxp": {
         "protocol":    "lxp/1",
         "asset":       "USDX",
         "amount_usdx": "0.031500",               // normalized 6dp
         "pay_to":      "did:matrix:<dev>:<fp>",  // payee DID
         "mode":        "exact" | "hold",
         "captor_did":  "did:matrix:<svc>:<fp>",  // hold mode only
         "ttl_s":       120,                      // hold mode only
         "nonce":       "<LayerX challenge nonce, prefetched for the payer>",
         "ref":         "0x<32-byte binding digest>",
         "layerx":      "https://public-mapi.matrixlayerx.com",
         "quote_id":    "...",                    // optional
         "expires_at":  "..."                     // <= nonce TTL
       } }

3    payer signs the canonical LayerX intent preimage (auth.IntentMessage):
       exact: matrix-layerx-intent:pay:<from>:<to>:<amount>[:<ref>]:<nonce>
       hold:  matrix-layerx-intent:hold:<from>:<to>:<amount>:<ttl>:<ref>:<captor>:<nonce>
     (pay omits the ref field entirely when absent; hold always carries it,
      empty or not)

4 -> retry + X-LayerX-Payment: base64url(JSON{
       from_did, public_key, nonce, signature, to_did, amount_usdx, mode, ref })

5 <- 200 + X-LayerX-Receipt: base64url(JSON{
       seq, leaf_hash, sequencer_sig, amount_usdx, ref })
     (inclusion proof + anchor tx forever at GET {layerx}/v1/receipt/{seq})
```

The **nonce trick**: LayerX requires a live single-use challenge nonce inside
every signed intent. The resource server prefetches one from layerxd's public
challenge endpoint for the payer's DID (identified by `X-Caller-DID` or an
attempted payment's `from_did`) and embeds it in the 402 terms. The payer
never talks to layerxd on the pay path. Without a payer DID the server
answers `402` with `reason: identify_payer` and no terms.

The **ref** is `keccak256(canonical{service_id, operation, args_hash,
quote_id, caller_did})`. It appears in the payer-signed intent, on the LayerX
transfer row and receipt, and in the deus execution receipt — *paid* and
*served* are cross-provable from signed artifacts alone.

## 8.3 Modes

- **exact** — settle the full amount, then serve (the deus gateway executes
  then settles, preserving its void-on-failure metering semantics). The only
  exposure is an underfunded payer discovered at settle time, bounded by one
  quote.
- **hold** — reserve → serve → capture on success / release on failure. The
  hold debits the payer's own spendable balance into a ledger hold row with a
  payer-authorized `captor_did` and TTL; capture emits a standard LayerX
  transfer (seq, Merkle leaf, sequencer signature) to the payee and returns
  any remainder; release and TTL expiry return the full amount. Services pick
  the mode per listing (`settlement_mode` / `hold_ttl_s` in the manifest;
  default exact).

## 8.4 Protocol behaviors (law)

- Invalid / expired / replayed / underfunded payment → fresh `402` (new
  nonce) with a machine-readable `reason`. Never execution.
- layerxd unreachable → `503 {"error":"payment_unavailable"}`. Never a free
  call.
- Replay (same idempotency key + payment) returns the stored result without
  a second charge — the metering ledger is the replay spine.
- A failed execution behind a hold releases the hold — no charge; a capture
  failure after execution also releases, so no funds strand either way.

## 8.5 Error reasons

| `reason` | meaning |
| --- | --- |
| `payment_required` | no payment attached; terms follow |
| `identify_payer` | no payer DID to prefetch a nonce for; send `X-Caller-DID` |
| `invalid_payment` | header malformed or intent refused by layerxd |
| `invalid_signature` | signature does not verify over the canonical preimage |
| `terms_mismatch` | amount/payee/mode/ref differ from the priced terms |
| `payment_rejected` | expired/replayed nonce or unauthorized intent |
| `insufficient_funds` | payer's LayerX balance cannot cover the charge |

## 8.6 The kit (one protocol, three halves of the same vectors)

- **Go** — `deus/pkg/lxp`: `Server` (challenge/verify/settle/holds) +
  `Middleware` for any `net/http` service; the deus gateway consumes this
  same package (dogfooded). See `pkg/lxp/README.md`.
- **Node** — `runner/src/lxp.js`: `createLXP(cfg).guard(price, handler)` for
  hosted and self-hosted JS services; the runner harness enables it with
  `LXP_LAYERX_URL` + `LXP_PRICE_USDX` + `LXP_PAY_TO` (+ `LXP_KEY`,
  `LXP_MODE`, `LXP_HOLD_TTL_S`).
- **Client** — `tools/deus/deus.mjs`: on a 402 it enforces the owner leash
  (`LAYERX_MAX_SPEND_USDX` per call, `LAYERX_MAX_DAILY_USDX` rolling daily)
  BEFORE signing with the executor key, retries once, and surfaces the
  LayerX receipt in the tool result. No leash configured = no invisible
  payments.

Preimage lockstep across implementations is pinned by cross-implementation
vector tests (`pkg/lxp/crossimpl_test.go`, `pkg/lxp/nodemw_test.go`).

## 8.7 Pricing

Prices are micro-USDX (6dp decimal strings) in manifests
(`unit_price_usdx` / `min_charge_usdx`); no wei/PAX amount appears on any
LayerX pay path. Pricing math is a pure, versioned function
(`pkg/pricingmath`) used identically by the quote endpoint and the gateway
charge — quote == charge, always. `POST /v1/quote/{id}` remains for
pre-flight discovery (EIP-712-signed); the 402 challenge carries equivalent
terms, so the quote round-trip is optional.

## 8.8 Receipts & auditability

Every settlement is a standard LayerX transfer: monotonic seq, Merkle leaf,
sequencer signature, batched and anchored on-chain — forever re-readable at
`GET {layerx}/v1/receipt/{seq}` with an inclusion proof. The deus execution
receipt (EIP-712, gateway-signed, runner-co-signed for hosted) carries the
same `ref` and the settlement's `layerx_seq`, so *paid* ↔ *served* is
decidable from signed artifacts alone.

> **Evidence, not proof-of-correctness.** `result_hash` binds *"these are the
> bytes you received"* — it is not proof the answer was *correct*. Treat
> receipts as evidence of what was returned and charged, not as a
> correctness oracle.

## 8.9 Refunds & failures

| Situation | Billing outcome |
| --------- | --------------- |
| Service error / timeout / schema-invalid | `voided` — no charge (hold released) |
| Policy denied / quote expired | no call, no charge |
| Settlement fails after execution (exact) | row `voided`; retry re-runs cleanly |
| Capture fails after execution (hold) | hold released; row `voided`; no charge |
| layerxd unreachable | `503 payment_unavailable`; zero executions |

## 8.10 Developer earnings

- Every settlement pays the listing's `payee_did` instantly on LayerX —
  there is no payout window, no float, no deus custody.
- `GET /v1/me/earnings` joins deus invocation aggregates with live LayerX
  reads (`GET /v1/account/{did}`); withdrawal is a link-out to LayerX
  `/v1/withdraw`.
- 100% of the charge reaches the developer; Paxeer monetizes the activity
  LayerX anchors on-chain, not a cut of revenue.

## 8.11 Metering integrity

- The deus ledger is append-only; replay reads, never edits.
- Idempotency keys dedup retries (no double charge, exactly one settlement).
- Reserve/finalize/void maps 1:1 onto hold/capture/release at the ledger.
- All amounts are integer micro-USDX end-to-end — no float drift.
