# LayerX contracts

The on-chain custody + settlement spine for LayerX on **Paxeer (chain 125)**.
This suite holds the fleet's USDL reserve and can at any time be backing
**hundreds of millions of dollars** of circulating USDX — so it is written to a
correspondingly conservative standard.

## No external dependencies

Production `src/` has **zero external imports** — no OpenZeppelin, no third-party
libraries. Every safeguard (SafeERC20, ReentrancyGuard, Pausable, two-step
ownership, EIP-712, ECDSA) is reimplemented in-house under `src/lib/` so the
entire trusted surface is auditable in this repo and cannot drift with an
upstream package. `forge-std` is vendored under `lib/` for **tests only** and is
never imported by `src/`.

## Contracts

| Contract           | Responsibility |
| ------------------ | -------------- |
| `LayerXVault`      | Custody of the USDL reserve. Deposits (USDL direct, ERC-20 → USDL via the Paxeer DEX, native PAX → USDL), operator-submitted settlement payouts, and the unilateral force-exit escape hatch. |
| `SettlementAnchor` | Immutable, append-only log of settled batch Merkle roots so signed inclusion receipts are independently verifiable. Holds no funds. |

### `src/lib` (in-house safeguards)

- `SafeERC20` — tolerates non-bool tokens (USDT) + approve-race via `forceApprove`.
- `ReentrancyGuard` — storage-based `nonReentrant` (shanghai EVM, no transient storage).
- `Pausable` — guardian emergency stop.
- `Governed` — two-step governor handoff.
- `EIP712` — fork-safe typed-data domain (cached + chain-id re-derive).
- `ECDSA` — recovery with `s`-malleability + `v` guards, zero-address reject.

## Trust model & safeguards

- **Reserve invariant:** circulating USDX == `usdl.balanceOf(vault)`. Deposits use
  balance-delta accounting; every payout is bounded by the live reserve.
- **Operator** (sequencer EVM key) can only *trigger* payouts the contract
  validates. It cannot forge balances and is bounded by the reserve, an optional
  `maxSettlementPerBatch` cap, `MAX_PAYOUTS`, and the exited-account bar.
- **Governor** (two-step) is the protocol root — params + role rotation only;
  it can never move agent funds. **Guardian** can pause in emergencies.
- **Force-exit escape hatch** defends against a dark/withholding operator: any
  agent can `initiateExit` with the operator's last EIP-712 co-signed balance
  proof, wait `exitDelay`, then `finalizeExit` and be paid — so a single
  sequencer can never trap funds. A strictly-higher-epoch `challengeExit`
  corrects a stale balance without extending maturity (no grief). Monotonic
  `epoch` prevents replay; finalize is terminal (bars later settlement → no
  double-pay). The hatch is keyed on the agent's **mapped EVM address**
  (`msg.sender`), since the ed25519 DID can't be cheaply verified on the EVM.
- All value movement routes through `SafeERC20`; all external mutators are
  `nonReentrant`.

## Build & test

```bash
forge build
forge test
```

40 tests cover deposits (USDL/swap/native), slippage/allowlist/deadline guards,
settlement (idempotency, cap, reserve bound, exited bar, access control), the
full force-exit lifecycle (initiate/challenge/finalize, stale-epoch replay,
wrong-signer, expiry), pause, two-step governance, SafeERC20 (USDT non-bool +
approve-race), and reentrancy.

## Mainnet wiring (chain 125 — Sei fork, redeployed 2026-06-30)

Paxeer migrated chain 125 from an Evmos fork to a Sei fork; state was reset and
every contract was redeployed at NEW addresses. RPC: `https://api.hyperpax.xyz`.

| Param | Value |
| ----- | ----- |
| USDL (reserve, 6 dp) | `0x85FcD13735F4309833A503EE804ea32395851479` |
| PECOR V4 router      | `0x63380c384296EeD6AB39379269622156F05D1111` |
| WPAX9 (wrapped PAX)  | `0xD152891923C7D6fE84d3DCF58621aB2be0eFCbc2` |

USDX precision = **1e6 (micro-USDX)**, matching USDL's 6 decimals 1:1.

Deploy via `script/Deploy.s.sol` (requires explicit operator approval; Andrew
drives all mainnet actions).

## Open decisions (frozen spec `[deferred]` — confirm before mainnet)

1. **`exitDelay` default** — challenge window length (clamped `[1h, 30d]`).
2. **`maxSettlementPerBatch`** — per-batch payout cap (0 = uncapped at deploy).
3. **Operator == sequencer key** — the same key signs settlement txs and balance
   proofs; confirm whether to split these into two roles.
4. **Exit terminality** — `finalizeExit` permanently bars an address from future
   settlement. Confirm this vs a governor-clearable flag for re-entry.
