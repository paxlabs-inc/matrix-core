# AGENTS.md

## Cursor Cloud specific instructions

### Repository overview

This is the **Matrix** monorepo (`matrix-core`): Go cognition/runtime layers (`cortex`, `MCL`, `bridge`, `executor`, `gateway`, `router`). The `deus/` tree is **spec-only** (Phases 0–2 not implemented yet); see `deus/docs/00-index.md` and `14-roadmap.md`.

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

### Deus (not runnable yet)

Deus quality targets (`deus-build`, `deus-test`, `deus-contracts`, `deus-mcp-selftest`) are documented in `deus/docs/11-modules.md` but **no Makefile targets or Go module exist yet**. Do not expect `deus/` to build until Phase 0 is implemented.

### Running the daemon in tmux

```bash
SESSION_NAME="matrix-daemon"
tmux -f /exec-daemon/tmux.portal.conf new-session -d -s "$SESSION_NAME" -c "/workspace"
tmux -f /exec-daemon/tmux.portal.conf send-keys -t "$SESSION_NAME:0.0" \
  'export PATH="$HOME/go/bin:$HOME/.local/bin:$PATH" && ./bin/mcl-execute daemon -addr :8080 -cortex-root ./runs/dev-cortex -manifest ./agents/default.json -skills-root ./skills' C-m
```
