# AGENTS.md

## Cursor Cloud specific instructions

### Repository overview

This is the **Matrix** monorepo (`matrix-core`): Go cognition/runtime layers (`cortex`, `MCL`, `bridge`, `executor`, `gateway`, `router`) plus **`deus/`** (agent-service registry + invoke gateway). Deus Phases 0–4 and Phase 6 (streaming rail) are implemented; see `deus/docs/14-roadmap.md`.

Standard build/test commands live in the root `Makefile` and `CONTRIBUTING.md`.

### Path convention

Many defaults hardcode `/root/matrix` (agent manifest MCP args, skill loader, e2e harness). In Cloud Agent VMs the repo is at `/workspace`. A one-time symlink is required:

```bash
sudo ln -sfn /workspace /root/matrix
```

### PATH

Add these to your shell before `make lint` or starting the daemon:

```bash
export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH"
```

- `$HOME/go/bin` — `golangci-lint` (via `make lint-install`)
- `$HOME/.local/bin` — `uvx` (required by the `fetch` MCP server in `agents/default.json`)

### Core services

| Service | Command | Port | Notes |
| ------- | ------- | ---- | ----- |
| Matrix daemon | `./bin/mcl-execute daemon -addr :8080 -cortex-root ./runs/dev-cortex -manifest ./agents/default.json -skills-root ./skills` | 8080 | Primary runtime; spawns MCP servers from manifest |
| MCL compiler CLI | `./bin/mclc compile -skill skills/writing-plans/SKILL.mtx -prose "..." -verb build` | — | Dry-runs without `FIREWORKS_API_KEY` |
| MCP tool smoke | `./bin/mcl-tools call -manifest agents/default.json -uri matrix://tool/mcp/fs/list_directory@2026.1.14 -args '{"path":"/workspace"}'` | — | Works without LLM keys |

Daemon health check: `curl http://localhost:8080/healthz`

### Secrets for live LLM flows

Copy `.env.example` → `.env` and set at minimum:

- `FIREWORKS_API_KEY` — compiler + most executor models (`accounts/fireworks/...`)
- `TOGETHER_API_KEY` — non-Fireworks model routes

Without these, `mclc compile` dry-runs and `POST /messages` fails at compile time (expected).

### Quality gates

From repo root (with `PATH` set as above):

```bash
make build && make install    # compile all modules + install ./bin CLIs
make ci                       # fmt-check + vet + test (see caveats below)
make lint                     # golangci-lint per module
```

**Known pre-existing gaps on main (as of setup):**

- `make ci` may fail `fmt-check` on a few Go files under `executor/`.
- Some `MCL/llm` and `executor/cmd/mcl-execute` tests expect model slug fragments like `glm-5.1` while the registry now emits `glm-5p1-fast`.
- `gateway/internal/rates` and `proxy` tests expect different PAX pricing constants.
- `router` and `cortex` test suites pass cleanly.

### Deus control plane

From `deus/` (Postgres + optional Anvil for integration tests):

```bash
export DEUS_POSTGRES_URI='postgres://deus:deus@127.0.0.1:5432/deus?sslmode=disable'
export DEUS_DEV=1 DEUS_ROOT=/workspace/deus
export PAXEER_RPC_URL=http://127.0.0.1:8545 DEUS_CHAIN_ID=31337
export DEUS_SERVICE_REGISTRY_ADDR=<deployed> DEUS_PUBLISH_PRIVATE_KEY=<anvil-key>
export DEUS_GATEWAY_SIGNING_KEY=<same-or-other-key>
make -C deus deus-build deus-test deus-lint deus-contracts deus-mcp-selftest
DEUS_RUN_ANVIL_TESTS=1 go test -tags=integration ./test/e2e/...   # from deus/
```

**Phase 2 invoke loop:** `POST /v1/quote/{id}` → `POST /v1/invoke/{id}` (direct rail) → EIP-712 receipt.

**Phase 3 hosted listings:** `POST /v1/services/{id}/artifacts` → `POST /v1/services/{id}/deploy` → publish (requires active deployment) → gateway invokes `deployments.exec_endpoint` at `POST /invoke`. Dev: `DEUS_HOSTING_DEV_EXEC_URL` + in-memory objstore. Prod: `DEUS_APPWRITE_ENDPOINT`, `DEUS_APPWRITE_PROJECT_ID`, `DEUS_APPWRITE_API_KEY`. Budget: `DEUS_HOSTING_KILL_SWITCH`, `DEUS_HOSTING_MAX_ALWAYS_WARM`.

**Phase 4 discovery:** plain-language `POST /v1/discover` runs constraint extraction → lexical (`websearch_to_tsquery`) + optional vector KNN (when `DEUS_EMBED_ENDPOINT` set). Ranking weights in `deus/configs/ranking.yaml`. Listings are indexed on create/publish via `SetManifestIndexer`. Dev uses hash embedder (lexical-only search path). Migration `003_discovery_search.sql` adds `search_document` + HNSW index.

Dev caller auth: `Authorization: Bearer …` plus `X-Caller-DID` / `X-Caller-Wallet`. Gateway uses `wallet.DevClient` when `DEUS_DEV=1` and no `MATRIX_WALLET_API_URL`.

**MCP:** `tools/deus/deus.mjs` + `agents/default.json` `deus` server; router injects `MATRIX_DEUS_URL` (default `http://deus-control.internal:9095`).

**Swarm rule:** Deus work is staged locally; the user commits (cloud agents should not push Deus feature commits unless asked).

### Running the daemon in tmux

```bash
SESSION_NAME="matrix-daemon"
tmux -f /exec-daemon/tmux.portal.conf new-session -d -s "$SESSION_NAME" -c "/workspace"
tmux -f /exec-daemon/tmux.portal.conf send-keys -t "$SESSION_NAME:0.0" \
  'export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && ./bin/mcl-execute daemon -addr :8080 -cortex-root ./runs/dev-cortex -manifest ./agents/default.json -skills-root ./skills' C-m
```
