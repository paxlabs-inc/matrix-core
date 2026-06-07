# tachyon-tools

An **agent-native** Solidity / EVM toolbox. Foundry and Hardhat assume a human
at a terminal; tachyon-tools assumes an LLM agent calling a service.

The primary interfaces are **API / MCP / JSON-RPC**. The CLI (`cmd/tachyon`) is
a thin client, not the product.

## Design principles
- **Structured I/O.** JSON in, JSON out. Errors carry machine-stable codes and a `retry` flag.
- **Simulation-first.** `simulate` is a first-class verb; agents dry-run before they commit.
- **Idempotent by key.** Deploys carry an idempotency key (+ optional CREATE2 salt) so retries don't double-deploy.
- **Intent-based.** Describe the desired end state; the deployer plans the transactions.
- **Keys behind policy.** Agents hold scoped capability tokens, never raw keys. Every signature passes a policy gate.
- **Observable by default.** Structured logs/traces — nobody is watching a terminal.

## Layout
- `cmd/tachyond`  — the daemon (API + RPC + MCP over one engine)
- `cmd/tachyon`   — thin client
- `pkg/api|rpc|mcp` — transports
- `pkg/types`     — the shared, typed contract for all surfaces
- `internal/`     — engine: compiler, deployer, evm, wallet, registry, simulate

## Quick start
```sh
make deps                  # forge-std (+ optional test deps)
make build && make run     # boots the daemon on :8645
curl localhost:8645/healthz
./bin/tachyon compile <<< '{"targets":["Create2"]}'
./bin/tachyond --mcp       # MCP stdio mode
./bin/tachyond --selftest  # CI tool drift check
```

See `docs/api.md` and `docs/mcp-tools.md` for the full JSON contract.

## Wallet & config

Configuration lives in `tachyon.config.kvx` (copy from `tachyon.config.kvx.example`).
Precedence is **environment > kvx > defaults**. Two wallet modes:

- **`self_hosted`** — you hold the signing key. `signer` is one of:
  - `env` — `private_key = "${DEPLOYER_PK}"` (key stays in the environment)
  - `keystore` — a web3 secret-storage (geth v3) JSON + password
  - `raw` — a `0x`-hex key in the file (chmod `0600`; gitignored)
- **`embedded`** — the Paxeer embedded wallet signs server-side over the
  agent-native DID lane. The daemon's ed25519 seed (`keyfile`) proves a
  `did:matrix:<label>:<keyfp>` identity; no keys are held locally and custody
  policy (freeze / read-only / spend caps / allow-lists) is enforced by the wallet.

`capability_token` on `deploy`/`call` selects a named `[policy.*]` profile
(spend cap, destination allow-list, chain allow-list), enforced by the local
policy gate in self-host mode.

**Transport auth:** set `[server].auth_token` (or `TACHYON_AUTH_TOKEN`) to require
`Authorization: Bearer <token>` on every request except `GET /healthz` and `GET /`.
Bind to loopback (`127.0.0.1:8645`) unless fronted by TLS.
