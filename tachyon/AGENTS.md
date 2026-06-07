# Agent development guide (tachyon-tools)

## Cursor Cloud specific instructions

### Foundry

Install once: `curl -L https://foundry.paradigm.xyz | bash && foundryup`. Ensure `~/.foundry/bin` is on `PATH`.

### Commands

```bash
make deps && make build    # forge-std + tachyond/tachyon binaries
make ci                    # Go tests + forge build --skip test
./bin/tachyond             # :8645
curl http://127.0.0.1:8645/healthz
```

Full Forge test tree needs optional deps: `make forge-test-deps` then `forge test`.

v1 engine exposes compile, test, simulate, deploy, call, chain, artifact, and registry verbs on REST (`/v1/*`), JSON-RPC (`/rpc`), and MCP (`--mcp`). Run `make ci` for Go tests + forge smoke + MCP selftest. Full end-to-end: `make e2e-all` (starts daemon, runs `scripts/e2e-smoke.sh` — 23 checks across REST, RPC, MCP, CLI).

MCP stdio: logs go to **stderr**; stdout is NDJSON-RPC only. MCP clients must not `readline` after `notifications/*` (no response).
