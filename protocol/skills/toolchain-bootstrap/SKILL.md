---
name: toolchain-bootstrap
description: On-demand install of long-tail stacks in a fresh environment — Angular CLI, Rust, uv/Python, Bun, Go, SvelteKit, via apt and mise. Use when the doctrine stack is not preinstalled on the box.
origin: Centra AI
---

# Toolchain Bootstrap Playbook

A fresh box does not have Angular CLI, the Rust toolchain, `uv`, Bun, or a current Go on it.
Do **not** silently downgrade the stack to whatever is already installed — that reintroduces
the anti-default failure the stack doctrine exists to prevent (see
`rules/stack-selection/`). Install the chosen toolchain on demand, then scaffold.

Use when: the stack-selection doctrine picked a toolchain the environment lacks, and you
need to bring it up before running the framework playbook.

## Strategy

- Prefer **`mise`** (formerly rtx) for language runtimes — per-project pinned versions, no
  sudo, reproducible across boxes. Fall back to **apt** / official installers for system
  packages and where mise has no plugin.
- Pin versions; do not install "latest" blindly into a project you want reproducible.
- Verify with `--version` after every install before proceeding. An install that did not
  land is a stop condition, not something to assume.

## mise (preferred runtime manager)

```bash
# install mise once
curl https://mise.run | sh
echo 'eval "$(~/.local/bin/mise activate bash)"' >> ~/.bashrc && source ~/.bashrc

# pin runtimes per project (writes .mise.toml / .tool-versions)
mise use node@22
mise use go@latest
mise use python@3.12
mise use rust@latest        # or use rustup below for full toolchain control
mise install                # installs everything pinned
```

## Per-stack bring-up

### Node / TS (React Router, Astro, SvelteKit, Next)

```bash
mise use node@22
node --version && npm --version
```

### Angular CLI

```bash
mise use node@22
npm i -g @angular/cli
ng version
```

### Bun (alt JS runtime/pm/test)

```bash
curl -fsSL https://bun.sh/install | bash
exec $SHELL && bun --version
```

### Rust (correctness-critical / systems)

```bash
# rustup gives full toolchain control (components, targets, cross-compile)
curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh -s -- -y
source "$HOME/.cargo/env"
rustc --version && cargo --version
rustup component add clippy rustfmt
```

### Python + uv (ML / data / scientific)

```bash
# uv: fast installs, reproducible lockfiles; the doctrine default for Python
curl -LsSf https://astral.sh/uv/install.sh | sh
source "$HOME/.local/bin/env" 2>/dev/null || true
uv --version
uv python install 3.12       # uv can manage the interpreter too
uv init myproj && cd myproj && uv add numpy pandas
```

### Go (high-throughput backend)

```bash
mise use go@latest           # or apt: sudo apt-get install -y golang-go (often older)
go version
```

## apt (system packages / build deps)

```bash
sudo apt-get update
sudo apt-get install -y build-essential pkg-config libssl-dev curl git
```

Use apt for compiler/system dependencies (needed for many Rust/Python native builds), not as
the primary source of language runtimes — distro packages lag. Prefer mise/rustup/uv for the
runtimes themselves.

## Verify before you build

```bash
node --version; go version; rustc --version; uv --version; ng version
```

Only proceed to the framework playbook once the exact toolchain the doctrine chose reports a
version. If an install failed, fix it — do not fall back to a different stack to route around
a missing tool.

## Common pitfalls

- Downgrading the stack to what is already on the box — the exact anti-default the doctrine
  forbids. Install the right tool instead.
- Installing "latest" unpinned into a project meant to be reproducible.
- Forgetting to re-source the shell / update PATH after an installer, then reporting "not
  found" as if the tool is unavailable.
- Using apt's old Go/Node instead of mise's current version and hitting feature gaps.
- Missing `build-essential`/`libssl-dev` before a Rust or Python native build, then
  misreading the compile error as a code problem.
