# 10 — Integration

How Deus plugs into Paxeer, the embedded wallets, and the Matrix agent runtime.

## 10.1 Integration map

```text
Matrix per-user daemon ──(stdio MCP)── deus.mjs ──(HTTP agent API)── Deus gateway
        │                                                               │
        │ embedded agent wallet (DID, bearer, policy)                   │
        └──────────────► paxeer-embeded-wallets ◄──────────── spend authorization
                                  │
                                  ▼
                         Paxeer chain 125 (ServiceRegistry + precompiles)
```

## 10.2 Matrix agent runtime (the hero integration)

Deus services become **MCP tools** available to every Matrix per-user daemon via
a stdio proxy, exactly like `tools/browser/browser.mjs` and
`tools/tachyon/tachyon.mjs`.

### `tools/deus/deus.mjs`
- A Node stdio MCP server that answers `initialize` / `tools/list` **locally**
  from a baked `tools/deus/deus-tools.json`, and forwards `tools/call` to the
  Deus gateway over HTTP.
- **Why local tools/list:** `executor/mcp` `Manager.verifyTools` requires the
  advertised tool set to EXACTLY equal the manifest, or daemon boot is fatal.
  So the tool list is static + drift-guarded (`--selftest`).
- **Lazy remote:** the gateway is contacted only on the first `tools/call`, so an
  unreachable Deus never bricks daemon boot — `deus_*` tools just return a
  structured error until reachable.
- **Env:** `MATRIX_DEUS_URL` (default `https://deus.paxeer.app` or 6PN
  `http://deus-control.internal:PORT`), `MATRIX_DEUS_TIMEOUT_MS`.
- **Auth/spend:** for write tools (`deus_invoke`), the proxy mints + forwards the
  daemon's **own agent wallet bearer** per request via the executor.key ed25519
  agent-auth handshake (same pattern as `tools/tachyon/tachyon.mjs` and
  `tools/paxeer/lib/agentauth.mjs`). Deus then settles via that wallet under its
  policy.

### Tool set (`deus-tools.json`) — proposed v1
| Tool | Maps to | Kind |
| ---- | ------- | ---- |
| `deus_discover` | `POST /v1/discover` | read |
| `deus_get_service` | `GET /v1/services/{id}` | read |
| `deus_quote` | `POST /v1/quote/{id}` | read |
| `deus_invoke` | `POST /v1/invoke/{id}` | **write** (spend) |
| `deus_invocation_status` | `GET /v1/invocations/{id}` | read |
| `deus_my_spend` | `GET /v1/me/spend` | read |

Keep the set small and stable; bijection with `deus-tools.json` is enforced at
daemon boot. Bake `tools/deus` into the daemon image (`deploy/daemon/Dockerfile`
`COPY tools/deus`).

### `agents/default.json` (+ skill)
- Add a `deus` server entry (alias `deus`, `command = node
  /root/matrix/tools/deus/deus.mjs`) with EXACTLY the tools above.
- Optionally add the tools to `skills/paxeer-assistant` §TOOLS so the freeform
  hero path can discover + call services.
- Consider a dedicated skill `skills/deus-broker` (SKILL.mtx + SKILL.md):
  "find the right service for a need, quote it, check it fits the spend policy,
  invoke it, report the result + cost." `.mtx` rule: double-quote all string KV
  values.

### Router env injection
`router/cmd/matrix-router/main.go` `MachineEnv` injects `MATRIX_DEUS_URL`
(+ token if needed) into each provisioned Machine, exactly like
`MATRIX_BROWSER_URL` / `MATRIX_TACHYON_URL`.

## 10.3 Embedded wallet integration

`protocol/paxeer-embeded-wallets` is the **spend authority and signer** for both
callers and (optionally) developers.

- **Caller identity**: the agent's DID + bearer (from `/v1/agent/auth/*`). Deus
  verifies the bearer and resolves the agent wallet address.
- **Spend authorization**: Deus does not move funds itself. Reserve/charge is a
  wallet operation (e.g. escrow funding, stream open, direct send) that the
  wallet's **owner-set policy** (per-call cap, total cap, allowlist) authorizes
  or denies. Deus surfaces the outcome (`policy_denied`).
- **Owner control plane**: spend limits/rules are set by the human owner in the
  wallet's owner routes (`/v1/agents/*`), not in Deus. Deus reads a **mirror**
  (`spend_grants`) for fast pre-checks and links out to the wallet UI for
  changes.
- **Precompile helpers**: reuse `src/precompiles.ts` builders
  (`streams.open/settle/close`, `eip712.*`, `teeAttestor.verify*`,
  `scheduler.*`) rather than re-encoding calldata. Deus's Go side mirrors these
  via go-ethereum bindings generated from the same `abi.json`.

## 10.4 Agent fee lane (gas)

- Deus registers its **relayer/settler address** in the `x/feemarket` agent fee
  lane registry so registry + settlement txns get the substituted lane gas price
  (predictable, cheap). `IsAgentLaneCaller(signer, target)` → `LaneGasPrice`.
- This lets Deus relay developer registrations (sponsor gas) and run settlement
  cheaply, supporting the take-nothing model.

## 10.5 Paxeer precompiles (summary of touchpoints)

| Precompile | Deus use |
| ---------- | -------- |
| PoFQ `0x0904` | service quality score (visibility/ranking) |
| PaymentStreams `0x0906` | streaming pay-per-second |
| EIP-712 `0x0908` | signed quotes + receipts |
| TEEAttestor `0x0907` | confidential-service verification |
| Scheduler `0x0905` | recurring invocations (v1.x) |
| Oracle `0x0903` | optional PAX/USD display pricing |

Full mechanics in [`04-onchain.md`](./04-onchain.md).

## 10.6 Relationship to Tachyon, Browser, paxeer-net

Deus is a **sibling** of the existing shared services:
- `matrix-browser` (shared headless browser), `matrix-tachyon` (shared Solidity
  engine), `paxeer-net` (chain tools) — all reached by per-user daemons via
  stdio MCP proxies.
- Deus follows the same idiom: a shared private control plane + a `deus.mjs`
  proxy. Conceptually, **Tachyon/Browser/paxeer-net are first-class candidates to
  be listed as Deus services themselves**, making Deus the registry that indexes
  the rest of the agent toolset.

## 10.7 Console ↔ wallet ↔ Supabase

- The Next.js console reuses `client/`'s Supabase auth (project + JWT) and design
  system. A logged-in developer links a wallet (signature challenge) to claim
  on-chain `owner`.
- The spend dashboard reads `GET /v1/me/spend` and deep-links to the wallet owner
  control plane for policy changes.

## 10.8 Data/embedding services reuse

- Embeddings for discovery reuse the `cortex/embed` Fireworks client (or the
  Matrix gateway embedding route) so Deus does not introduce a new embedding
  vendor.
- Object storage reuses the box MinIO/S3 already used by Matrix snapshots.
- Postgres is the same box Postgres instance (separate database/schema `deus`),
  mirroring how gateway + router share one box DB.
