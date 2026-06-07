# ADR 0001: Go daemon + Foundry subprocess

## Status

Accepted

## Context

Agents need a stable JSON contract for compile → test → simulate → deploy. Foundry is the best-in-class Solidity toolchain but assumes a human at a terminal. Matrix already uses MCP stdio bridges (e.g. `paxeer-net.mjs`) with structured errors and `--selftest` CI guards.

## Decision

1. **Go** implements `tachyond` — one engine (`internal/engine`), three thin transports (REST, JSON-RPC, MCP).
2. **Foundry** runs as subprocess for compile/test; artifacts are normalized from `out/`.
3. **go-ethereum** handles RPC simulate/deploy/call against any EVM chain.
4. **JSON file registry** for artifacts and idempotent deployments (SQLite deferred).

## Consequences

- Pin forge in CI; parse `out/*.json` rather than depend solely on `forge build --json`.
- Wallet signing stays behind policy (`internal/wallet`); raw keys only with `TACHYON_ALLOW_DEV_SIGNER=true`.
- Hardhat JS tests remain v1.1; Forge `.t.sol` is the v1 test loop.
