# Agent development guide (matrix-core)

## Cursor Cloud specific instructions

### `/root/matrix` symlink

Tests and several CLIs default to `/root/matrix`. Create once per VM:

```bash
sudo mkdir -p /root
sudo ln -sfn /agent/repos/matrix-core /root/matrix
```

### Commands

See root `Makefile` and `CONTRIBUTING.md`. Typical loop:

```bash
make build && make install
make test/cortex test/MCL test/bridge test/executor
```

`make ci` mirrors GitHub Actions but **gateway** rate tests may fail on PAX float expectations; core modules pass with the symlink above.

Live flows need `.env` API keys, Node ≥ 20, and `uv` for MCP servers. No API keys needed for:

```bash
./bin/mclc validate skills/writing-plans/SKILL.mtx
```
