# 01 — Product Overview

## 1.1 What Deus is

Deus is a **marketplace and registry for the agent economy**. Developers list
their APIs and AI services; anyone — but especially AI agents — discovers and
calls them, paying only for what they actually use. It simultaneously serves as
**Paxeer's registry of agent services**: every callable service in the
ecosystem lives in one place, whether it returns data or does work on someone's
behalf.

The defining assumption: in the agent era, **software calls software**. An AI
agent should not have to create an account, get approved, copy an API key, and
manage a subscription just to use a service for ten seconds. On Deus it
**discovers** what it needs, **pays** for the call, and **moves on** — no human
in the loop.

## 1.2 Why it exists (problem)

Today's API and agent marketplaces assume a human operator:

- **Onboarding friction** — sign up, get a key, configure billing. Fatal for a
  machine that needs a capability for one call.
- **Subscriptions, not usage** — minimum commitments don't fit sub-cent,
  bursty, autonomous demand.
- **Reputation is anecdotal** — star ratings and comment sections, not
  objective delivery quality.
- **Two disconnected tools** — an "agent registry" and an "API marketplace"
  bolted together, each with its own identity and payment model.
- **Custody risk** — letting an autonomous agent transact means trusting it not
  to overspend, with no enforced guardrails.

## 1.3 The Deus answer (solution)

| Problem | Deus mechanism |
| ------- | -------------- |
| Onboarding friction | No per-service accounts/keys/human onboarding — pay-per-call via the caller's wallet (caller keeps a wallet + short-lived prepaid float; see [`08-payments-billing.md`](./08-payments-billing.md) §8.3) |
| Subscriptions | Per-call micro-payments down to fractions of a cent; streaming for continuous use |
| Anecdotal reputation | **PoFQ** quality score: on-chain, tamper-evident *reduction* over operator-attested delivery samples (bilateral once the caller co-signs receipts) |
| Two tools | **One** entity model — registry and marketplace are the same product |
| Custody risk | Spend limits + rules enforced on-chain by the embedded-wallet policy plane + Argus |
| Discovery | **Plain-language** semantic search returns the right service, not a catalog dump |
| Hosting cost | **Free hosting** — list your API and Paxeer runs it for you |
| Platform rake | **Zero** — developers keep 100%; Paxeer earns from network activity |

## 1.4 Personas

### Developer (lister)
Lists a service in minutes and earns immediately. Wants: trivial onboarding,
free hosting, instant discoverability, fair quality-based visibility, full
revenue, clear analytics, and the option to offer confidential/regulated
services.

### AI agent (primary caller)
Autonomous software acting for a user/enterprise/DAO. Wants: machine-readable
discovery, plain-language intent → right service, deterministic pricing,
one-shot pay-and-call, signed receipts, and hard spend guardrails so it cannot
go off the rails.

### Human caller (secondary)
A developer or end-user browsing the console. Wants: search, transparent
pricing, a try-it console, and a spend dashboard.

### Paxeer / platform operator
Wants: a thriving service supply that drives on-chain activity (gas, settlement,
quality writes) without taxing developers; abuse resistance; and the registry to
be the canonical map of the ecosystem's agent capabilities.

## 1.5 Value propositions (verbatim intent, mapped to mechanism)

**For developers listing services**
- *List in minutes, earn immediately* → `POST /v1/services` + manifest validation
  + immediate on-chain registration ([`05-api.md`](./05-api.md), [`04-onchain.md`](./04-onchain.md)).
- *Keep 100%; platform takes nothing* → settlement pays the developer's payout
  address directly; no fee skim ([`08-payments-billing.md`](./08-payments-billing.md)).
- *Free hosting; Paxeer runs it* → **hosted listings** on **Paxeer Cloud** (the
  Appwrite fork) ([`06-execution-hosting.md`](./06-execution-hosting.md)).
- *Instantly discoverable* → indexed + embedded on listing
  ([`07-discovery.md`](./07-discovery.md)).
- *Quality score, not a comment section* → PoFQ ([`04-onchain.md`](./04-onchain.md)).

**For people and agents using services**
- *No accounts/keys/subscriptions; pay per call* → wallet-authed invocation +
  net settlement ([`08-payments-billing.md`](./08-payments-billing.md)).
- *Describe in plain language, get the right service* → semantic search
  ([`07-discovery.md`](./07-discovery.md)).
- *Spending limits/rules enforced automatically* → embedded-wallet policy + Argus
  ([`09-security.md`](./09-security.md), [`10-integration.md`](./10-integration.md)).
- *Data APIs and agent services in one place* → unified `services` model
  ([`03-data-model.md`](./03-data-model.md)).

## 1.6 Differentiators (defensive moat)

1. **Take-nothing economics.** Competitors take a cut and run their own token.
   Deus gives developers everything plus free hosting; Paxeer benefits from
   activity, not taxation.
2. **Native to Paxeer.** *Payments and settlement* are instant on Paxeer's own
   rails with **no external payment-network dependency**. (Compute and search do
   use infrastructure: hosted execution runs on **Paxeer Cloud** — Paxeer's
   deployed Appwrite fork — and discovery embeddings use an external embedder.
   Both are mitigated/swappable, but "native" means *native settlement*, not
   "zero external infrastructure.")
3. **One product, not two.** The agent registry and the API marketplace are the
   same thing.
4. **Trusted/confidential services are first-class.** TEE-backed services and
   signed outputs open regulated/enterprise use that most agent marketplaces
   cannot touch.

## 1.7 Scope

### In scope (v1)
- Service listing (proxy + hosted), manifest schema, validation.
- On-chain `ServiceRegistry` + Postgres mirror + indexer.
- Discovery: structured filters + plain-language semantic search.
- Invocation gateway: auth, metering, rate limit, routing, signed receipts.
- Payments: per-call net settlement (default) + streaming + direct transfer.
- Quality scoring via PoFQ.
- Spend enforcement via embedded-wallet policy plane.
- Developer console + spend dashboard (Next.js).
- Matrix MCP proxy (`deus.mjs`) integration.

### In scope (v1.x, post-MVP)
- Confidential (TEE) hosted services + attestation gating.
- Scheduler-backed recurring invocations.
- Bundles / composite services; service-to-service calls.
- HPS standard schemas (offer, payment-request, reputation-query, evidence).
- IBC cross-chain service calls.

### Non-goals
- Running general compute unrelated to listed services.
- Being a generic cloud host (hosting exists only to serve listed services).
- Re-implementing wallet spend policy (delegated to embedded wallets/Argus).
- A platform token (PAX is the only asset).
- Human-mediated dispute courts in v1 (evidence is anchored; arbitration later).

## 1.8 Success criteria

- An agent can go from *plain-language need* → *invoked the right service* →
  *paid* in **one autonomous round-trip**, with the spend bounded by policy.
- A developer can go from *zero* → *listed + earning* in **under 10 minutes**,
  with **0%** platform fee.
- Every invocation produces a **signed, anchored receipt** and updates the
  service's **on-chain quality score**.

## 1.9 One line

> The marketplace and registry for the agent economy, where developers keep
> everything and agents pay only for what they use.
