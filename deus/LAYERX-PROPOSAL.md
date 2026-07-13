# Proposal: Deus as a LayerX-native service (LXP — the LayerX HTTP payment handshake)

Status: PROPOSAL — not spec'd, not implemented. Grounded in code as of 2026-07-13.

## 0. The problem

Deus today carries three payment rails, all riding on-chain PAX through machinery
deus owns end-to-end:

| Rail | Mechanism | Code | State |
| ---- | --------- | ---- | ----- |
| `direct` | per-call PAX transfer via the embedded wallet (`/v1/agent/send`), after execution | `internal/gateway/gateway.go:251,405`, `internal/wallet/client.go:83` | wallet HTTP client works only for `Send`; `AuthorizeSpend` is a no-op (`client.go:71-80`); every call pays L1 gas |
| `net` | deus-owned pairwise payment channels + EIP-712 cumulative vouchers + own settler + `PaymentChannel.sol` | `internal/gateway/gateway_net.go`, `internal/channels/`, `internal/settlement/`, `contracts/` | audit HIGH findings: vouchers evidence-only not load-bearing (F2), settler dev-only (F3), channel open unverified vs on-chain escrow (F7), non-atomic cosign (F5) |
| `stream` | PaymentStreams precompile `0x0906` sessions | `internal/streams/`, `internal/gateway/gateway_stream.go` | depends on wallet precompile routes |

Meanwhile LayerX exists and is live: by its own design doc it is *"a
single-operator generalization of the Deus pairwise voucher ledger into a
fleet-wide operator ledger with batched settlement"* and names Deus as a
consumer: *"a purchase is just a `layerx_pay` between two DIDs"*
(`layerx/DESIGN.md:34-39,152-153`).

**Conclusion: do not fix the deus rails — delete them and re-platform deus as a
LayerX application.** The deus audit's HIGH findings live almost entirely in
code this migration removes.

## 1. Target architecture in one line

Deus = **registry + discovery + execution gateway**, with zero payment
machinery of its own. All value movement is **USDX between DIDs on LayerX**,
authorized in-band over HTTP by a challenge/response handshake (**LXP**) that
carries a DID-signed LayerX payment intent, and proven by LayerX's signed,
Merkle-batched, chain-anchored receipts cross-bound to deus's signed execution
receipts.

This covers both product surfaces with one mechanism:

- **Paying for services** — agent calls a listed API, pays per call.
- **Paying agents** — an agent lists *itself* as a service; its endpoint speaks
  LXP; it earns USDX directly to its DID. Registry == marketplace already; now
  payee == DID uniformly.

## 2. LXP — the handshake (what x402 is to USDC, LXP is to LayerX)

Same interaction *shape* as Coinbase's x402 — request, get payment terms, retry
with signed payment attached — but none of its protocol: no EIP-3009, no USDC,
no on-chain transaction per call, no third-party facilitator, no x402 wire
format. The rails are LayerX's: off-chain instant settlement, gasless to the
agent, ed25519 DID identity, provable receipts, batch-anchored to Paxeer.

Roles:

- **Payer** — any agent with a USDX balance under its DID (`executor.key` is
  already the signing key; every daemon has one).
- **Resource server** — the deus gateway, or any service embedding the LXP
  middleware (§6, P4).
- **layerxd** — verifier + settler. Analog of the x402 facilitator, but
  trust-minimized: it cannot forge or steal (invariant i6: the DID signature IS
  the authorization), and its history is anchored + publicly auditable.

Flow:

```
1. POST /v1/invoke/{service}                      (no payment attached)

2. <- HTTP "payment required" challenge
   body {
     "error": "payment_required",
     "lxp": {
       "protocol":    "lxp/1",
       "asset":       "USDX",
       "amount_usdx": "0.031500",              // normalized 6dp (types.FormatUSDX)
       "pay_to":      "did:matrix:<dev>:<fp>", // payee DID
       "mode":        "exact" | "hold",
       "nonce":       "<layerx challenge nonce for the payer DID>",
       "ref":         "0x<32-byte binding digest>",
       "layerx":      "https://public-mapi.matrixlayerx.com",
       "quote_id":    "...",                   // optional, ties to /v1/quote
       "expires_at":  "..."
     }
   }

3. client signs the standard LayerX intent preimage with its DID key:
   IntentMessage("pay", from_did, nonce, to_did, amount)     (v1, zero LayerX change)
   IntentMessage("hold", from_did, nonce, to_did, amount, ref)  (v2, holds)

4. retry with header
   X-LayerX-Payment: base64url(JSON{
     from_did, public_key, nonce, signature,
     to_did, amount_usdx, mode, ref
   })

5. server submits the intent to layerxd, executes the service, responds
   200 + X-LayerX-Receipt: base64url(JSON{seq, leaf_hash, sequencer_sig, ...})
   (inclusion proof + anchor tx retrievable forever at GET /v1/receipt/{seq})
```

### Why this works with zero LayerX auth changes (v1)

LayerX PHASE 4 already accepts a **DID-signed pay intent from anyone who
transports it** — `writeCaller` verifies the signature + consumes the
single-use nonce, and does not care who the HTTP caller is
(`layerx/internal/server/server.go:638-669`). So the deus gateway can submit
the caller's payment on the caller's behalf.

The only friction is the nonce: LayerX requires a live server-issued challenge
nonce (`auth.Challenges`, `identity.go:98-145`). Resolution: **the resource
server prefetches a challenge for the payer's DID** (the challenge endpoint is
public, `POST /v1/agent/auth/challenge`) **and embeds it in the LXP challenge
response**. Replay safety is preserved (single-use, TTL-bound, DID-bound); the
payer pays exactly one extra HTTP round trip, to deus not to layerxd.

### What the payer's side looks like

`tools/deus/deus.mjs` (the MCP proxy) implements the client half invisibly:
on an LXP challenge it (a) checks the daemon-side spend leash
(`LAYERX_MAX_SPEND_USDX` per-call/daily, mirroring the PaxeerSpendPolicy
posture), (b) signs with `executor.key` (same ed25519 code path as
`tools/paxeer/lib/agentauth.mjs`), (c) retries. Agents never see the handshake;
they see "called tool, got result, receipt attached".

## 3. Changes to deus

### 3.1 New

- **`internal/layerx`** — REST client for layerxd: prefetch challenge, submit
  pay/hold/capture/release, fetch receipt, read account. Config
  `DEUS_LAYERX_URL` (+ optional transport bearer).
- **LXP challenge/verify in the gateway** — `Invoke` grows a single unified
  payment step replacing `wallet.AuthorizeSpend` + `wallet.Send` +
  `channels.Reserve` + `vouchers.BuildPending`:
  - no `X-LayerX-Payment` → build terms (price from `validateQuote`), prefetch
    nonce, return the challenge;
  - payment attached → verify shape, submit to layerxd, execute, respond with
    both receipts.
- **`ref` binding digest** — `keccak256(canonical{service_id, operation,
  args_hash, quote_id, caller_did})`. Carried in the signed intent (v2) and
  stored on both sides, so the LayerX payment receipt and the deus execution
  receipt (`receipts.ReceiptFields` + LayerX `seq` added) prove *paid* ↔
  *served* for the same call. Dispute = present both receipts.
- **USDX-native pricing** — quote and charge in micro-USDX (6dp) instead of
  PAX wei. Services price in USD terms; no PAX volatility exposure on pricing.
  `pricingmath` gains USDX helpers; `unit_price_wei`/`max_total_wei` become
  `unit_price_usdx`/`max_total_usdx` (manifest + quote schema bump).
- **Payee DID** — service manifest + developer record gain `payee_did` (the
  LayerX account). Developer earnings = instant USDX credit at
  payment/capture; withdrawal to their EVM address is LayerX's existing
  `/v1/withdraw` + explorer, not deus code. `/v1/me/earnings` joins the deus
  invocation ledger with the public LayerX explorer reads
  (`GET /v1/account/{did}`, `GET /v1/transfers`).

### 3.2 Kept (repurposed)

- **`metering.Ledger`** — stays as the idempotency + replay + analytics spine
  (`Reserve/Finalize/Void` semantics map 1:1 onto hold/capture/release). Gains
  `layerx_seq`, `hold_id` columns; `rail` value `"layerx"`.
- **`receipts` EIP-712 execution receipt** — stays; deus still attests *what
  ran* (args/result hash, units, outcome). Gains the LayerX `seq` + `ref`.
- **Quotes** (`POST /v1/quote/{id}`) — stays for pre-flight price discovery,
  but the LXP challenge carries the same terms inline, so a client can skip
  the quote round trip entirely.
- Registry, discovery, hosting, quality, console — untouched by the rail swap.

### 3.3 Deleted (the audit die-off)

- `internal/channels/` (channels + vouchers) — superseded by the LayerX ledger
  itself; kills F2/F5/F7.
- `internal/settlement/` (settler, rails, dead ranking code) — LayerX owns
  settlement (batch windows, anchoring, withdrawals); kills F3/F9/F10.
- `contracts/PaymentChannel.sol` — LayerX vault + anchor replace it.
- `internal/wallet/` — the pay path no longer touches the embedded EVM wallet
  at all; kills F1/F8. (The embedded wallet remains the *owner* custody plane —
  depositing into LayerX, leash policy — but deus never calls it.)
- `stream` rail (`internal/streams/`, precompile sessions) — replaced by:
  subscriptions = Chronos-scheduled LXP payments; metered/session usage = one
  LayerX hold with periodic partial captures (v2). No precompile dependency in
  the pay path.
- `/v1/channels`, `/v1/vouchers/cosign`, `/internal/settle/run` routes
  (`handlers_invoke.go:23-26`, `handlers_channels.go`).

## 4. Changes to LayerX (the tweaks)

### 4.1 Holds — authorize → capture/release (the one structural addition)

Deus's core safety property today is *reserve → execute → finalize-or-void*
(`metering.Reserve/Void`). Exact-pay before execution loses "pay only on
success"; pay after execution loses the funds guarantee. A hold primitive
preserves both, and deus **never custodies** — funds lock inside the payer's
own account:

- `POST /v1/hold` — DID-signed intent
  `IntentMessage("hold", from, nonce, to_did, amount, ref, captor_did)`.
  Moves `amount` from `balance_usdx` into a new `holds` table (new
  `held_usdx` accounting column; the existing `escrow_usdx` stays untouched as
  the net-reserve audit counter it is — see `store/accounts.go:182-185`).
  Returns `hold_id` + a sequencer-signed hold receipt. `captor_did` is who the
  payer authorizes to capture (the deus gateway DID, or the payee itself) —
  the exact card-network auth model, payer-consented, bounded by amount + TTL
  + fixed payee.
- `POST /v1/hold/{id}/capture {amount ≤ held}` — authorized by `captor_did`
  (principal token or signed intent). Emits a **normal transfer leaf** payer →
  payee through the existing `store.Pay` path, so receipts, batching,
  anchoring, tiers, and the reserve proof are all unchanged. Remainder
  releases.
- `POST /v1/hold/{id}/release` + TTL auto-release sweep in the settle worker —
  no funds ever stuck (mirrors the audit F4 lesson from deus's own
  reservation leak).

### 4.2 `ref` on intents and receipts

Optional 32-byte hex commitment included in the signed preimage and stored on
the transfer row + receipt JSON. v1: row-level (no accumulator change). v2:
fold into the leaf preimage under a bumped domain
(`layerx.settlement.receipt.v2`) so the binding itself is chain-anchored.

### 4.3 One-shot nonces (optional, later)

Challenge-prefetch (§2) makes this non-blocking, but a v2 nicety: accept
client-minted nonces (server-side uniqueness table + expiry window) so a payer
can construct a complete signed payment offline with zero prior round trips.

### 4.4 Explicitly not needed

Pay, withdraw, batching, anchoring, receipts/inclusion proofs, the public
explorer, SSE stream, deposit/DID-claim, EVM binding — all already fit as-is.

## 5. Trust and custody analysis

- **Deus holds no funds, ever.** Payments flow payer-DID → payee-DID on the
  LayerX ledger; holds lock funds inside the payer's account. Deus is a
  transporter of signed intents plus an authorized captor within
  payer-consented bounds.
- **Spend policy** lives at the signer, per house doctrine: the daemon leash
  gates signing (per-call/daily USDX caps); the owner plane can freeze the
  agent key. v2: LayerX-native owner allowances (already on the LayerX roadmap
  — "budgets and leashes become native LayerX escrow allowances",
  `DESIGN.md:144-146`).
- **Sequencer trust** is unchanged from LayerX's frozen posture: it can delay
  or censor, it cannot forge, steal, or equivocate undetected; force-withdraw
  caps the downside.
- **Gateway misbehavior is provable**: capture-after-execute ordering plus the
  shared `ref` across the payment receipt and the signed execution receipt
  means "charged but not served" and "served but not charged" are both
  evidentiable from public data.
- **Failure modes**: layerxd down → 503 `payment_unavailable` (no silent free
  calls); insufficient balance → challenge response includes the deposit
  pointer (`GET /v1/deposit` semantics); execution failure after hold →
  release, exactly today's `Void`.

## 6. Migration plan

- **P1 — LayerX holds + ref** (`layerx` module): holds table + lifecycle,
  capture-through-`store.Pay`, TTL sweep, ref on row/receipt. Property tests:
  reserve invariant (`circulating == vault USDL`) unbroken by hold/capture/
  release in any order; no fund creation; TTL release idempotent.
- **P2 — deus exact-pay LXP** behind `DEUS_RAIL_LAYERX=1`: `internal/layerx`
  client, challenge/verify in `Invoke`, USDX pricing, receipts cross-binding.
  Old rails untouched behind the flag.
- **P3 — holds mode + payee UX**: hold/capture in the gateway, payee DIDs,
  earnings from explorer, withdraw link-out, `deus.mjs` client handshake +
  leash, console updates.
- **P4 — the deletion**: rails, channels, settlement, wallet, streams,
  `PaymentChannel.sol`, dead routes; docs/08 rewritten as the LXP spec; ship
  the LXP middleware kit (Go `net/http` middleware + Node/runner middleware)
  so any listed service can also self-serve payments without the deus gateway
  in its data path — same protocol, layerxd as the verifier.

Each phase becomes a `spec/` feature (EARS reqs + waved tasks) on approval.

## 7. Open decisions

1. **Challenge status code** — recommend plain HTTP `402 Payment Required`
   (deus and layerxd both already use it: `gateway.go:180`,
   `server.go:486`) with our own `lxp` body/headers; alternatives 409/428 if
   zero association with "402" branding is wanted.
2. **USDX-native pricing** (recommended) vs keeping wei/PAX prices and
   converting at quote time.
3. **Capture authority** — payer-authorized `captor_did` (recommended) vs
   payee-signed captures only.
4. **Stream rail** — delete outright (recommended) vs keep the precompile path
   as a legacy option until Chronos-scheduled subscriptions ship.
5. **Middleware kit in v1 scope** or deus-gateway-only first.
6. **`ref` anchoring depth** — row-level v1, leaf-domain-v2 bump later.
