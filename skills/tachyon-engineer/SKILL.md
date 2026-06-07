---
name: tachyon-engineer
description: Write, compile, test, and ship smart contracts on Paxeer (and any EVM chain) with the Tachyon engine. Uploads Solidity (OpenZeppelin + forge-std available), compiles with forge, runs tests, dry-runs, then deploys/interacts on-chain. Writes sign via the embedded wallet; confirms a clean compile + green tests first and records every on-chain action with its tx hash.
origin: Matrix/Paxeer
---

# Tachyon Engineer

Solidity/EVM engineering through the shared **Tachyon** engine
(`matrix-tachyon`): author a contract, compile it with `forge`, run its test
suite, dry-run a call, then deploy and interact on-chain. The engine is shared
and stateless about your files — **sources are uploaded per request** — and it
holds **no wallet seed**: writes are signed by the calling agent's embedded
wallet, with custody + spend policy enforced network-side.

## Source-upload model

Tachyon compiles in an ephemeral Foundry workdir built from the `sources` map
you pass (workdir-relative path -> file content):

- Contracts go under `src/` (e.g. `src/MyToken.sol`).
- Tests go under `test/` (e.g. `test/MyToken.t.sol`).
- `@openzeppelin/contracts/...` and `forge-std/...` imports resolve
  automatically against the engine's baked corpus — no need to upload them.
- `tachyon_compile` returns a **`project_id`**. Capture it: a later
  `tachyon_deploy` / `tachyon_call` uses `project_id` + the contract name to
  resolve the ABI/bytecode.

## Primitives

- **Compile** — `tachyon_compile` (sources in, artifacts + `project_id` out).
- **Test** — `tachyon_test` (same sources; structured per-case pass/fail/gas).
- **Simulate** — `tachyon_simulate` / `tachyon_call` with `simulate_only=true`
  (read-only eth_call; no wallet, no broadcast).
- **Deploy** — `tachyon_deploy` (by `contract` + `project_id`; `idempotency_key`
  makes retries safe). WRITE.
- **Interact** — `tachyon_call` (encode by `method` + `args`, ABI from inline
  `abi` or `contract`+`project_id`). WRITE when `simulate_only` is false.
- **Chains** — `tachyon_chain_list` (read), `tachyon_chain_register` (add a
  custom RPC profile).
- **Lookup** — `tachyon_artifact_get`, `tachyon_registry_lookup` (read).

## Custody + enforcement

Deploys and broadcast calls sign through the embedded wallet at the network
custody service; the agent's spend authority is enforced **on the wallet
network-side**. The proxy forwards the agent's own `wallet_token` per write — the
shared engine never holds a key.

## Tool mandate (the planner MUST follow)

1. ALWAYS `tachyon_compile` (with inline `sources`) before a deploy — never
   deploy an uncompiled contract. Capture and reuse the returned `project_id`.
2. When tests are implied, `tachyon_test` the same sources and report results.
   If compile or tests fail, surface the forge/solc error verbatim and STOP.
3. For a state-changing call, dry-run first (`simulate_only=true`); only
   broadcast (`simulate_only=false`) if it does not revert.
4. Every successful deploy/broadcast returns a `tx_hash` (deploy also an
   `address`). The report MUST cite them verbatim. Never claim an on-chain
   action landed without a `tx_hash` in a tool result.

## Hard guardrails (enforced by policy, not optional)

- Spend caps + cortex `Constraint` limits are enforced below the plan. Do not
  route around them. If an amount is unbounded or exceeds a known limit, STOP
  and ask.
- A read-only / dry-run request NEVER triggers a deploy or broadcast.

## Reporting

Brief Andrew in natural, conversational prose: what compiled, the artifact name
+ `project_id`, test results, and for any on-chain action the contract address
and tx hash(es) — all grounded in tool results. Persist each deploy/broadcast as
an `Event` (with tx hash) so the chronology stays accurate.
