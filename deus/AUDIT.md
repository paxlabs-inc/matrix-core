# Deus Code Audit — Phases 0–2.5, 3, 4, 6 (commit `1d9a4dd`)

Audited against `deus/docs/*` spec. Scope: Go control plane, contracts, migrations.
Method: full read of net-settlement, streaming, discovery, hosting, gateway, store,
wallet, and contract code; build/vet/test/gofmt/lint gates.

## Verdict

The code is **well-structured and idiomatic** — it matches Matrix conventions
(zerolog, `envOr` config, typed errors, package docs, parameterized SQL) and the
read/dev paths are A-tier. **However, it is NOT production-complete, and the
single most important spec invariant (the caller-co-signed voucher bounding
settlement) is collected but never enforced.** Several "real money" paths are
dev-only stubs. Treat current state as **feature-complete in dev, not
settlement-safe in prod.**

## Quality gates

- `go build ./...` — PASS
- `go vet ./...` — PASS
- `go test ./...` (unit) — PASS (e2e gated behind `DEUS_RUN_ANVIL_TESTS=1`)
- `gofmt` — clean **except** `internal/receipts/merkle.go` (see F6)
- `golangci-lint` — clean (only an `exportloopref` deprecation warning)

---

## Findings (severity-ranked)

### F1 — [HIGH] Production wallet is entirely stubbed
`internal/wallet/client.go` — `HTTPClient.Send` (L61), `OpenStream` (L73),
`StreamSettle` (L84), `StreamClose` (L95) all return `"not implemented for HTTP
client"`; `AuthorizeSpend` (L49) is a no-op `return nil`. Only `DevClient` works.
**Impact:** with `MATRIX_WALLET_API_URL` set (non-dev), every paid invoke fails at
`g.wallet.Send` → 402. The direct, stream, and net rails are all non-functional in
production. The core value prop (per-call payment) cannot run live.
**Spec:** `docs/08-payments-billing.md` §8.2, `docs/10-integration.md` (embedded
wallet `agent/send`). **Fix:** implement the embedded-wallet REST calls; until then
`AuthorizeSpend` should not silently succeed.

### F2 — [HIGH] Settlement ignores the caller-co-signed voucher
`internal/settlement/settler.go:34-78` computes payout from the **metering ledger**
(`UnsettledInvocations` → sum of `price_wei`) and calls `payout`/`anchor` on that
total. `store.HighestVoucherForChannel` (`internal/store/vouchers.go:39`) is **dead
code — never called**. On-chain, `PaymentChannel.payout()`
(`contracts/src/PaymentChannel.sol:47`) is bounded only by `fundedWei - redeemedWei`,
**not** by any voucher cumulative.
**Impact:** the voucher machinery (cosign, EIP-712, storage) is built but is **pure
evidence** — it is not load-bearing at settlement. The spec's central invariant is
violated:
> §8.3: "Settle submits the **highest co-signed voucher**; the escrow releases
> exactly that cumulative amount … The caller cannot be charged beyond its last
> signed voucher."
This is the exact gap the voucher amendment was meant to close, re-emerging one
layer down. **Fix:** settler must redeem against `HighestVoucherForChannel`
cumulative; `PaymentChannel.payout` (or a redeem fn) should verify the caller
signature / cumulative on-chain or be strictly bounded by the submitted voucher.

### F3 — [HIGH] Net settlement is dev-only; no chain `Payer` exists
`cmd/deusd/main.go:207-208` wires the settler only `if cfg.Dev` with
`&settlement.DevPayer{}`. The only `Payer` implementation is `DevPayer`
(`internal/settlement/rails.go:31`). There is no chain-backed payer/anchor client.
**Impact:** net settlement cannot pay out or anchor in production. Combined with F1,
no rail actually moves PAX outside dev. **Fix:** implement a chain `Payer`
(PaymentChannel.payout + SettlementAnchor.anchor via signed txns) and wire it in
non-dev.

### F4 — [MED] Net invoke leaks channel reservations on the async-cosign path
`internal/gateway/gateway_net.go:138-176`: when the caller does **not** pass
`CallerVoucherSig` inline (`NeedsSignature=true`, the normal async case), the meter
is `Finalize`d (L138) and the receipt stored (L142), the channel `reserved_wei`
stays incremented, but `FinalizeChannelCharge` (which releases reserve + advances
cumulative) runs **only inside `Cosign`**. If the caller never co-signs, the
reservation is locked for the window and revenue is recorded with no bilateral
proof. There is no timeout/reaper to release stuck reserves.
**Impact:** balance leak + ledger revenue not backed by a signed voucher. **Fix:**
release/expire stale reservations at window end; don't finalize ledger revenue
before the matching voucher exists (or reconcile at settlement).

### F5 — [MED] Voucher `Cosign` is not transactional
`internal/channels/voucher.go:78-91` calls `store.FinalizeChannelCharge`
(`store/channels.go:102`) and then `store.InsertVoucher` (`store/vouchers.go:23`)
as **two separate Execs**. If `InsertVoucher` fails, the channel `nonce`/
`cumulative_wei` have already advanced with **no persisted voucher row** → the
voucher chain is corrupted (next `BuildPending` uses the advanced nonce;
`HighestVoucherForChannel` would miss the row). **Fix:** wrap both in one DB
transaction.

### F6 — [MED] `merkle.go`: formatting + cryptographic hygiene
`internal/receipts/merkle.go`:
- L3-11 import block is **not gofmt-sorted** (`bytes` appears after `fmt`) →
  fails `gofmt`/`goimports`.
- L29-43 builds a sorted Merkle tree with **no leaf/node domain separation**
  (no `0x00`/`0x01` prefix) → second-preimage ambiguity, and **odd-node promotion**
  (carry-up of the unpaired node) is malleable.
Currently evidence-only (SettlementAnchor just emits the root; no on-chain proof
verification), so risk is low today — but it must be domain-separated and
gofmt-clean before any on-chain Merkle verification is added, and to match
Matrix's crypto bar. **Fix:** `gofmt -w`; prefix leaves/nodes; duplicate the last
node on odd layers (or use an explicit scheme).

### F7 — [MED] Channel balance is asserted, not reconciled with on-chain escrow
`internal/channels/channels.go:38-60`: `Open` records `BalanceWei = CapWei` from the
request with an unverified `FundTx` string and a hardcoded `EscrowAddr =
"0xescrow-dev"` (L45). Nothing checks that the caller actually funded the
`PaymentChannel` on-chain. **Spec §6.2 / §8.3** require the reserve to be bounded by
**on-chain escrow**. **Impact:** off-chain balance can exceed real escrow; the
"bounded by on-chain escrow" invariant is not enforced. **Fix:** verify the fund tx
/ read `fundedWei` from the channel contract before crediting `balance_wei`.

### F8 — [LOW] `HTTPClient.AuthorizeSpend` returns nil (false sense of enforcement)
`internal/wallet/client.go:49-58` returns `nil` without calling the wallet. If
`Send` were later implemented but this left as-is, policy checks would be silently
skipped. **Fix:** implement or return not-implemented.

### F9 — [LOW] Dead code in discovery ranking
`internal/discovery/rank.go`: `BlendScore` (L54), `RankInput` (L48), `priceAffinity`
(L127) are unused — superseded by `BlendScoreWithPrice`. Exported dead API.
**Fix:** remove.

### F10 — [LOW] Settler settlement-window timestamp is hardcoded
`internal/settlement/settler.go:68` records `WindowStart = now-10m` rather than the
earliest covered invocation time. Cosmetic for the settlement record.

---

## What's correct / A-tier (for the record)

- **Atomic channel reserve** (`store/channels.go:73-89`) is implemented exactly per
  §6.2: a single conditional `UPDATE … WHERE balance-reserved >= amount` with a
  `RowsAffected()==0` guard. Correct concurrency primitive.
- **SQL** is fully parameterized; `fmt.Sprintf` only builds placeholder *numbers*,
  never interpolates user data.
- **EIP-712** quote/receipt/voucher typed data is well-formed; voucher caller
  signature is recovered and matched (`receipts/voucher.go:59`).
- **Idempotency replay + compensating `Void`** are consistently applied across
  direct/net/stream gateways.
- **Discovery** has proper graceful degradation (vector → lexical → browse),
  constraint extraction, blended ranking, and a dev hash embedder vs prod HTTP
  embedder.
- **Contracts** follow checks-effects-interactions (no reentrancy in
  `PaymentChannel`), use `onlySettler`/`onlyOwner` guards and custom errors;
  `SettlementAnchor` is event-only (mirror rebuildable from chain).
- **Conventions** match the rest of Matrix and `AGENTS.md` (zerolog, `envOr`,
  typed errors, package-level doc comments).

## Priority order to reach "production A-tier"

1. F2 + F7 — make the voucher/escrow load-bearing at settlement (the headline
   invariant).
2. F1 + F3 — implement the real embedded-wallet client and a chain `Payer`.
3. F4 + F5 — transactional cosign + reservation lifecycle/reaper.
4. F6 — merkle domain separation + gofmt.
5. F8–F10 — cleanups.
