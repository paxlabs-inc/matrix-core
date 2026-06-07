# 09 — Security

## 9.1 Threat model (assets → adversaries)

| Asset | Threats |
| ----- | ------- |
| Caller funds | Overspend, double-charge, charge-without-delivery, quote spoofing |
| Developer earnings | Withheld/short settlement, payout redirection, fake quality |
| Hosted code & data | Tenant escape, data exfiltration, supply-chain (malicious upload) |
| Registry integrity | Listing spoofing, manifest/pricing tampering, squatting |
| Platform | DoS, metering ledger tampering, RPC/chain outage abuse |
| Confidential payloads | Leakage from "trusted" services, forged attestations |

Adversaries: malicious developers, malicious callers/agents, compromised
runners, network MITM, and a curious-but-honest operator (minimize trust in Deus
itself).

> **Where the honest-operator assumption currently bites.** Until the caller is
> in the signing loop, two paths trust the operator more than "minimize trust"
> implies: (1) **billing** — a gateway-signed receipt alone attests units within
> the channel/quote cap, so a curious operator could bill up to the cap and the
> caller couldn't disprove it; (2) **reputation** — quality samples are
> operator-computed (§4.3). Both are closed by the **caller-co-signed cumulative
> voucher** ([`08-payments-billing.md`](./08-payments-billing.md) §8.3), which
> makes the charge *and* the `outcome` sample bilaterally provable. The voucher
> is therefore a security control, not just a payments detail — it is mandatory
> for `per_unit` pricing.

## 9.2 Authentication & authorization

- **Callers/agents** authenticate with the embedded-wallet **agent bearer**
  (ed25519 DID challenge/verify). Deus verifies the DID; spend authority comes
  from the **wallet**, not Deus.
- **Developers** authenticate via Supabase JWT (console) or wallet signature
  (programmatic). Listing mutations require proof of `owner` (on-chain check).
- **Internal** services talk only over the private 6PN network with mTLS/shared
  bearer; no internal endpoint is public.
- **Least privilege roles**: `gateway`, `settler`, `indexer`, `orchestrator`
  each have distinct keys/scopes. The settler key can move escrow funds **only**
  per anchored receipts; the indexer is read-only on chain.

## 9.3 Spend safety (the "won't go off the rails" guarantee)

- The authoritative spend policy is the **embedded wallet + Argus VM**
  (`protocol/paxeer-embeded-wallets`). Deus enforces it twice:
  1. **Fast pre-check** against the `spend_grants` cache (reject obviously
     over-budget calls cheaply).
  2. **Authoritative check** on the spend path: the actual reserve/transfer is a
     wallet operation that the wallet policy can deny (`policy_denied`).
- Per-call cap, per-window total cap, per-service allowlist, and rate limits are
  all expressible as wallet policy and **enforced on-chain**, so even a
  compromised Deus cannot exceed them.
- Quotes are **signed and bounded** (`max_total_wei`); a caller can never be
  charged beyond the quote it approved, and — on the channel rail — never beyond
  its **last co-signed voucher**.
- **Concurrency is a spend-safety property, not just correctness.** Because the
  control plane is stateless/N-instance and the grants cache is a fast
  pre-check, the reserve **must** be an *atomic transactional decrement* of the
  caller's channel balance (Postgres row lock bounded by the on-chain escrow
  cap), or two parallel invokes can oversell the channel before the authoritative
  check fires. This invariant is specified in
  [`06-execution-hosting.md`](./06-execution-hosting.md) §6.2 and is required for
  the spend guarantee to hold under load.

## 9.4 Tenant isolation (hosted services)

- Each hosted service runs as an **isolated Paxeer Cloud Function / container
  Site** (the Appwrite fork's per-function sandboxing: separate execution
  context, resource caps, no shared FS between tenants). Heavy/confidential
  services get a dedicated runtime.
- **Network egress allowlist** by default: a hosted service can reach only what
  its manifest declares; no lateral access to other tenants, the control plane's
  DB, or the chain signer.
- **No ambient secrets**: the runner harness injects only the service's own
  declared secrets (sealed, per-service), never platform keys.
- **Resource caps**: memory/CPU/time/response-size enforced by the harness;
  exceeding → killed, `outcome=error`, failure quality sample.
- **Supply chain**: uploaded artifacts are scanned (image/dependency scan)
  before deploy; builds are pinned/reproducible where possible; provenance
  recorded.

## 9.5 Confidential services (TEE)

- For `confidential=true`: the runner executes in a TEE; its output binds the
  result hash into `reportData`; the gateway calls `verifyAndExpect` on `0x0907`
  and releases payment + receipt **only** on a passing, non-stale, non-debug
  attestation (policy from `x/attestor`).
- Deus operators cannot read confidential payloads in transit (TLS) or at rest
  (the sensitive compute happens inside the enclave); only hashes/attestations
  touch Deus storage.
- This is what makes regulated/enterprise services first-class without trusting
  Deus.

## 9.6 Registry & manifest integrity

- `manifestHash` and `pricingHash` are on-chain; any off-chain tampering with the
  Postgres mirror is detectable (rehash + compare to chain).
- **Squatting/impersonation**: slugs are first-come but flagged; verified
  developers (wallet-linked identity, optional domain proof for proxy URLs) get a
  verification badge; impersonating listings can be `delisted` by gov.
- Pricing **bait-and-switch** is blocked: the quote must hash to a registered
  `pricingHash`.

## 9.7 Metering & settlement integrity

- Ledger is append-only; settlement is read-only over it; idempotency keys
  prevent double charges.
- Receipts are signed and Merkle-anchored, so neither over- nor under-payment
  can be hidden. On the channel rail the **caller co-signs a cumulative
  voucher**, so the charge is bilaterally provable — the caller holds proof, not
  just the developer.
- The escrow/settlement contract moves caller funds **only** against the
  highest caller-co-signed voucher and the anchored receipts — it can never pay
  out more than the caller admitted.

## 9.8 Transport & data protection

- TLS everywhere public; mTLS internal. HSTS on the console.
- Request/response bodies above an inline threshold are stored hashed in the
  object store with short TTLs; PII-bearing bodies for confidential services are
  not persisted in plaintext.
- Secrets in env / `/etc/matrix/*.env` only; never in repo, never in cortex
  memory, never in logs (log redaction on known secret keys).

## 9.9 Abuse & DoS

- Token-bucket rate limits per DID and per IP; invoke additionally gated by
  wallet policy (an attacker pays for every call they make).
- Discovery is cache-friendly and read-replica served; expensive LLM
  normalization is optional and rate-limited.
- Hosted-service abuse (mining, egress abuse) caught by egress allowlist +
  resource caps + anomaly metrics; offending services suspended.
- Sybil listings are economically bounded (gas to register) and quality-gated
  (must actually deliver to rank).

## 9.10 Auditing & observability

- Structured audit log for every listing mutation, settlement, and policy denial
  (who/what/when, signed).
- Prometheus metrics: invoke latency, charge totals, denial rates, runner cold
  starts, settlement lag, indexer lag, attestation pass/fail.
- Alarms on: settlement lag, indexer lag, abnormal denial spikes, escrow balance
  drift vs ledger.

## 9.11 Key management

- Gateway signing key (receipts/quotes), settler key (escrow moves), and any
  hosted-runner co-sign key are distinct, rotatable, and stored in the platform
  secret store. Rotation is supported via versioned key ids in receipts.
- The chain signer for registry/settlement txns uses the agent-wallet
  forwarding model where possible (no long-lived seed on the box), mirroring
  Tachyon's multi-tenant token-forwarding signer.

## 9.12 Compliance posture (forward-looking)

- Confidential + attested services + signed receipts + on-chain audit trail are
  the substrate for regulated use (data residency via region pinning, evidence
  retention, provable execution). Full compliance certifications are post-v1 but
  the architecture does not preclude them.

## 9.13 Honest limitations & trust assumptions (own them)

The spec deliberately states what Deus does **not** guarantee, so claims match
the threat model:

- **External infrastructure exists.** "Native to Paxeer" is about *settlement*,
  not infrastructure. Hosted execution depends on **Paxeer Cloud** (the Appwrite
  fork) and discovery on an **external embedder**. Both are mitigated (search
  degrades to lexical+filters; hosting is swappable) but they are real
  dependencies, not zero.
- **Operator-attested inputs (until caller co-sign).** Billing units and quality
  samples are operator-computed; the caller-co-signed voucher (§8.3) is what
  removes this. Stated plainly in §9.1.
- **Proxy reachability is TOCTOU.** The registration reachability probe (§2.5,
  [`06-execution-hosting.md`](./06-execution-hosting.md) §6.4) is necessary, not
  sufficient: a proxy endpoint can pass at listing time then rot. The real
  backstop is **continuous health-checking + a circuit breaker** that auto-pauses
  a flapping listing, with PoFQ decay as a lagging signal — not the one-time
  probe.
- **`result_hash` is evidence, not proof-of-correctness.** It binds *the bytes
  returned*, not *that the answer is right*. For non-deterministic agent/LLM
  services, correctness is unverifiable outside the TEE path (and TEE proves the
  attested code ran, not that its output is correct). Receipts are
  dispute-evidence, not a correctness oracle ([`08-payments-billing.md`](./08-payments-billing.md) §8.6).
