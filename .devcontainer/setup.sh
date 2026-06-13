#!/usr/bin/env bash
# Matrix devcontainer post-create setup.
# Installs all toolchain components not covered by devcontainer features.
set -euo pipefail

GO="${GO:-$(which go)}"
GOPATH="$($GO env GOPATH)"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

echo "==> pnpm"
npm install -g pnpm@11

echo "==> golangci-lint v1.61.0"
curl -sSfL https://raw.githubusercontent.com/golangci/golangci-lint/master/install.sh \
  | sh -s -- -b "$GOPATH/bin" v1.61.0

echo "==> Foundry (forge / cast / anvil)"
curl -L https://foundry.paradigm.xyz | bash
export PATH="$HOME/.foundry/bin:$PATH"
foundryup

echo "==> uv (Python tool runner for MCP servers)"
pip3 install --no-cache-dir --break-system-packages uv

echo "==> marketplace node_modules"
(cd "$REPO_ROOT/marketplace" && pnpm install)

echo "==> Go workspace (multi-module gopls)"
if [ ! -f "$REPO_ROOT/go.work" ]; then
  (
    cd "$REPO_ROOT"
    $GO work init
    for mod in MCL bridge cortex executor gateway router neo chronos deus tachyon uwac; do
      [ -f "$mod/go.mod" ] && $GO work use "./$mod"
    done
  )
  echo "  created go.work"
fi

echo "==> Go module dependencies (make tidy)"
make -C "$REPO_ROOT" tidy

echo ""
echo "Matrix devcontainer ready."
echo "  make build   — build all Go modules"
echo "  make test    — run unit tests"
echo "  make lint    — golangci-lint"
echo "  cd marketplace && pnpm dev   — start marketplace dev server"
echo "  cd tachyon && make ci        — tachyon Go tests + forge smoke"
