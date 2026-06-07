# 04 — On-Chain Layer

Deus is **native to Paxeer**. The registry, payments, reputation, receipts, and
confidential-execution proofs all use the chain directly. This document defines
the `ServiceRegistry` contract and exactly how each Paxeer precompile is used.

## 4.1 Chain facts

- **EVM chain id:** `125` (Cosmos `hyperpax_125-1`).
- **Native asset:** PAX (`ahpx`, 1 PAX = 10¹⁸ wei).
- **RPC:** `PAXEER_RPC_URL` (env). Explorer: `paxscan.paxeer.app`.
- **solc:** `0.8.27`, Foundry, **`evm_version = "shanghai"`**.
  > Chain 125's EVM is pre-Cancun: the default Cancun target emits `MCOPY`
  > (`0x5E`), which reverts string-returning view calls (`name()`/`symbol()`).
  > Pin `shanghai` in `foundry.toml`. (Same bug class as the Tachyon note.)

### Precompile address map (fixed)
| Addr | Name | Use in Deus |
| ---- | ---- | ----------- |
| `0x0901` | OROB | (not used in v1) optional dynamic/oracle-relative pricing later |
| `0x0903` | Oracle | optional PAX/USD reference for display pricing |
| `0x0904` | **PoFQ** | service **quality score** from delivery outcomes |
| `0x0905` | **Scheduler** | recurring/cron invocations (v1.x) |
| `0x0906` | **PaymentStreams** | continuous/streaming pay-per-second |
| `0x0907` | **TEEAttestor** | verify confidential-service execution |
| `0x0908` | **EIP-712 helper** | hash/recover **signed call receipts & quotes** |

ABIs are already encoded in `protocol/paxeer-embeded-wallets/src/precompiles.ts`
(`SCHEDULER_ABI`, `STREAMS_ABI`, `EIP712_ABI`, `TEE_ATTESTOR_ABI`) and the chain
source `knowledge/HyperPax-OS/precompiles/*/abi.json`. **Reuse those ABIs; do
not re-author them.**

## 4.2 `ServiceRegistry.sol`

Lives in `deus/contracts/src/ServiceRegistry.sol` (Foundry project). This is the
on-chain source of truth for listings. It is the **L3 registry** the agent fee
lane references (`feemarket` `LaneParams.Registry`).

### Storage (per service)
```solidity
struct Service {
    uint256 id;
    address owner;            // developer wallet
    address payout;           // earnings destination
    bytes32 manifestHash;     // keccak256(canonical manifest)
    bytes32 pricingHash;      // keccak256(canonical pricing commitment)
    uint8   status;           // 0 draft, 1 active, 2 paused, 3 delisted
    bool    hosted;           // Deus-hosted vs proxy
    bool    confidential;     // TEE-backed
    uint64  registeredAt;
    uint64  updatedAt;
}
mapping(uint256 => Service) public services;
mapping(address => uint256[]) public servicesByOwner;
uint256 public nextId;
```

### External functions (NatSpec each)
```solidity
function register(
    address payout,
    bytes32 manifestHash,
    bytes32 pricingHash,
    bool hosted,
    bool confidential
) external returns (uint256 id);

function update(uint256 id, bytes32 manifestHash, bytes32 pricingHash) external; // owner only
function setStatus(uint256 id, uint8 status) external;                            // owner/gov
function setPayout(uint256 id, address payout) external;                          // owner only
function transferOwner(uint256 id, address newOwner) external;                    // owner only

// views
function getService(uint256 id) external view returns (Service memory);
function ownerOf(uint256 id) external view returns (address);
function isActive(uint256 id) external view returns (bool);
```

### Events (the indexer's contract)
```solidity
event ServiceRegistered(uint256 indexed id, address indexed owner, bytes32 manifestHash, bytes32 pricingHash, bool hosted, bool confidential);
event ServiceUpdated(uint256 indexed id, bytes32 manifestHash, bytes32 pricingHash);
event ServiceStatusChanged(uint256 indexed id, uint8 status);
event PayoutChanged(uint256 indexed id, address payout);
event OwnerTransferred(uint256 indexed id, address indexed from, address indexed to);
```

### Design notes
- **No content on-chain** — only hashes + addresses. The manifest body lives in
  Postgres/object store; the hash binds it. Cheap, private-friendly, fast.
- **Pricing commitment** prevents bait-and-switch: the gateway's quote must hash
  to the registered `pricingHash` (or a newer registered version) or the call is
  refused.
- **Gas:** developer pays, or Deus relays via the **agent fee lane** (registered
  Deus relayer address gets the substituted lane gas price). See
  `feemarket` `IsAgentLaneCaller` / `LaneGasPrice`.
- **Upgradeability:** deploy behind a minimal owner-gov proxy (or immutable +
  migrate by redeploy + re-register). v1: immutable, version in events.

### Optional: settlement anchoring
A companion `SettlementAnchor.sol` (or a method on the registry) records the
Merkle root of each settled receipt batch:
```solidity
event SettlementAnchored(address indexed developer, bytes32 receiptsRoot, uint256 totalWei, uint256 count, uint64 windowEnd);
function anchor(address developer, bytes32 receiptsRoot, uint256 totalWei, uint256 count) external; // gateway/settler role
```
This makes every payout auditable: a developer (or dispute) can prove a receipt
was included in a settled batch via Merkle proof.

## 4.3 Quality scoring via PoFQ (`0x0904`)

PoFQ on Paxeer scores a "fill" `0..1e18` vs an oracle and maintains an
exponentially-decayed rolling score (`scoreFill`, `scoreBatch`,
`updateRollingScore`). Deus **repurposes** it for service quality:

- After each invocation, Deus computes a **delivery sample** in `0..1e18` from
  objective signals: success (1e18) vs error/timeout (0), adjusted by SLA
  adherence (latency ≤ p99 target) and (for data) result-schema validity.
- Periodically Deus calls `updateRollingScore(currentScore, currentWeight,
  newScore, newWeight, decayBps)` to fold the new batch into the service's
  rolling quality, weighting by invocation volume.
- The resulting score is cached in `services.quality_score` and is the primary
  **visibility/ranking** signal in discovery — reliable services rank higher.

> PoFQ math is stateless precompile math; Deus holds the per-service
> `(score, weight)` state (in Postgres, mirrored/anchored), and uses the
> precompile as the canonical, audited reducer so scores are reproducible and
> portable across the ecosystem.

`scoreBatch` is used for efficient bulk folding when settling a window.

## 4.4 Payments (precompile rails)

Full economics in [`08-payments-billing.md`](./08-payments-billing.md). On-chain
mechanics:

- **Per-call net settlement (default).** No on-chain write per call. The gateway
  meters off-chain; per developer per window the **Settlement** component pays
  the net total via a single transfer from a Deus settlement module (funded from
  caller wallets at reserve time) to `payout`, and anchors the receipts root.
  This is the "5–10× cheaper settlement" lazy-net pattern, applied to services.
- **Streaming (`0x0906`).** For continuous services the caller opens a stream
  (`open(payee=payout, token, ratePerSecond, start, stop, cap)`); the gateway
  meters against `accrued()` and calls `settle()` on intervals; `close()`
  refunds unspent cap. Native PAX or ERC-20 (caller `approve`s the streams
  precompile first).
- **Direct transfer.** High-value one-shot calls settle inline via the caller's
  embedded wallet `agent/send` before the result is released.

All three are initiated through the **caller's embedded agent wallet**
(`protocol/paxeer-embeded-wallets`), so the wallet's policy plane authorizes
every spend. Deus never holds caller keys.

## 4.5 Signed receipts & quotes via EIP-712 (`0x0908`)

- **Quote**: `domainSeparator(name="DeusQuote", version="1", chainId=125,
  verifyingContract=registry)`; struct hash over `{serviceId, endpoint,
  pricingVersion, unitPriceWei, maxUnits, caller, expiresAt}`. The gateway signs
  the digest; the caller (agent) can `recoverTypedSigner` to verify the quote is
  genuine before committing spend.
- **Receipt**: `domainSeparator(name="DeusReceipt", ...)`; struct hash over
  `{invocationId, serviceId, caller, argsHash, resultHash, priceWei, units,
  outcome, ts}`. Signed by the gateway and (for hosted/confidential) co-signed by
  the runner. Anchored in batches (4.2).
- Both reuse the `EIP712_ABI` helper for `hashTypedData` / `domainSeparator` /
  `recoverTypedSigner` so on-chain and off-chain agree byte-for-byte.

## 4.6 Confidential services via TEEAttestor (`0x0907`)

For `confidential=true` services (v1.x):
1. The hosted runner executes inside a TEE (Intel TDX / AMD SEV-SNP / NVIDIA
   H100 / Intel SGX — families `0..3`).
2. The runner returns a **quote** plus a `reportData` binding the result hash.
3. The gateway calls `verifyAndExpect(family, quote, expectedReportData=resultHash)`
   on `0x0907`. Only a passing attestation finalizes the receipt and releases
   payment.
4. Root certs are governed on-chain (`x/attestor`); Deus does not manage roots.

This is what makes regulated/enterprise + verifiable-compute services
first-class.

## 4.7 Recurring invocations via Scheduler (`0x0905`) — v1.x

For "call this service every block N / every interval" agent workflows, Deus can
register a `schedule(target=invokeRelay, callData, executeAtBlock, gasLimit)` job
funded by the caller, replacing external cron/keepers. The relay target validates
the caller's standing spend grant before invoking.

## 4.8 Contracts project layout (Foundry)

```text
deus/contracts/
  foundry.toml            # solc 0.8.27, evm_version="shanghai", src/test/lib
  remappings.txt
  src/
    ServiceRegistry.sol
    SettlementAnchor.sol
    interfaces/
      IServiceRegistry.sol
      precompiles/         # thin Solidity ifaces mirroring chain abis (optional)
  test/
    ServiceRegistry.t.sol  # register/update/status/owner transfer, event asserts
    SettlementAnchor.t.sol
  script/
    Deploy.s.sol           # broadcasts to chain 125 via PAXEER_RPC_URL
```

Build/test/deploy via the existing **Tachyon** engine (it already speaks chain
125 + agent-wallet signing) or `forge` directly. Deployment addresses recorded in
`deus/configs/chain.<env>.json`.
