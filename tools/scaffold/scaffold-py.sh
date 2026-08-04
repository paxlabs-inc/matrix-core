#!/usr/bin/env bash
# scaffold-py.sh — production Python package (src layout).
# Tooling: uv · ruff (lint+format) · mypy (strict) · pytest · pre-commit · Docker.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "py" "$@"
common_init_target
step "Python package → $PROJECT_SLUG"

PKG="$(identify "$PROJECT_SLUG")"
PYVER="3.12"

mkdir -p "src/${PKG}" tests

write_if_absent pyproject.toml <<EOF
[project]
name = "${PROJECT_SLUG}"
version = "0.1.0"
description = ""
readme = "README.md"
requires-python = ">=${PYVER}"
license = { text = "${SCAFFOLD_LICENSE}" }
authors = [{ name = "${SCAFFOLD_AUTHOR}", email = "${SCAFFOLD_EMAIL}" }]
dependencies = []

[project.scripts]
${PROJECT_SLUG} = "${PKG}.cli:main"

[build-system]
requires = ["hatchling"]
build-backend = "hatchling.build"

[tool.hatch.build.targets.wheel]
packages = ["src/${PKG}"]

[dependency-groups]
dev = [
  "mypy>=1.14",
  "pytest>=8.3",
  "pytest-cov>=6.0",
  "ruff>=0.9",
]

[tool.ruff]
line-length = 100
target-version = "py312"
src = ["src", "tests"]

[tool.ruff.lint]
select = ["E", "F", "I", "N", "UP", "B", "SIM", "C4", "PTH", "RUF", "S"]
ignore = ["S101"]

[tool.ruff.lint.per-file-ignores]
"tests/**" = ["S101"]

[tool.mypy]
python_version = "${PYVER}"
strict = true
warn_unreachable = true
files = ["src", "tests"]

[tool.pytest.ini_options]
addopts = "-q --cov=${PKG} --cov-report=term-missing"
testpaths = ["tests"]
EOF

write_if_absent "src/${PKG}/__init__.py" <<EOF
"""${PROJECT_SLUG} package."""

from ${PKG}.core import greet

__all__ = ["greet"]
__version__ = "0.1.0"
EOF

write_if_absent "src/${PKG}/core.py" <<'EOF'
"""Core library functions."""


def greet(name: str, *, loud: bool = False) -> str:
    """Return a greeting for ``name``; shout when ``loud``."""
    msg = f"Hello, {name}"
    return f"{msg.upper()}!" if loud else msg
EOF

write_if_absent "src/${PKG}/cli.py" <<EOF
"""Command-line entrypoint."""

import argparse
import sys

from ${PKG}.core import greet


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="${PROJECT_SLUG}")
    parser.add_argument("name", nargs="?", default="world")
    parser.add_argument("--loud", action="store_true")
    args = parser.parse_args(argv)
    print(greet(args.name, loud=args.loud))
    return 0


if __name__ == "__main__":
    sys.exit(main())
EOF

write_if_absent "src/${PKG}/py.typed" <<'EOF'
EOF

write_if_absent tests/test_core.py <<EOF
from ${PKG}.core import greet


def test_greet() -> None:
    assert greet("world") == "Hello, world"


def test_greet_loud() -> None:
    assert greet("world", loud=True) == "HELLO, WORLD!"
EOF

# --- runtime pin + pre-commit ----------------------------------------------
write_if_absent .python-version <<EOF
${PYVER}
EOF

write_if_absent .pre-commit-config.yaml <<'EOF'
repos:
  - repo: https://github.com/astral-sh/ruff-pre-commit
    rev: v0.9.0
    hooks:
      - id: ruff
        args: [--fix]
      - id: ruff-format
  - repo: https://github.com/pre-commit/mirrors-mypy
    rev: v1.14.0
    hooks:
      - id: mypy
        additional_dependencies: []
EOF

write_if_absent Makefile <<'EOF'
.PHONY: install dev test lint fmt type clean
install: ; uv sync
dev:     ; uv run $(notdir $(CURDIR))
test:    ; uv run pytest
lint:    ; uv run ruff check . && uv run ruff format --check .
fmt:     ; uv run ruff check --fix . && uv run ruff format .
type:    ; uv run mypy
clean:   ; rm -rf .pytest_cache .mypy_cache .ruff_cache dist build *.egg-info
EOF

write_if_absent Dockerfile <<EOF
# syntax=docker/dockerfile:1
FROM ghcr.io/astral-sh/uv:python${PYVER}-bookworm-slim AS build
WORKDIR /app
ENV UV_COMPILE_BYTECODE=1 UV_LINK_MODE=copy
COPY pyproject.toml uv.lock* ./
RUN --mount=type=cache,target=/root/.cache/uv uv sync --no-dev --no-install-project
COPY . .
RUN --mount=type=cache,target=/root/.cache/uv uv sync --no-dev

FROM python:${PYVER}-slim AS runtime
WORKDIR /app
COPY --from=build /app /app
ENV PATH="/app/.venv/bin:\$PATH"
RUN useradd -m app && chown -R app /app
USER app
ENTRYPOINT ["${PROJECT_SLUG}"]
EOF

gen_dockerignore

gen_gitignore_base
gitignore_add "python" "__pycache__/
*.py[cod]
.venv/
.mypy_cache/
.ruff_cache/
.pytest_cache/
dist/
build/
*.egg-info/
.coverage"

gen_github_ci "$(cat <<'YAML'
name: ci
on:
  push: { branches: [main] }
  pull_request:
jobs:
  build:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v4
      - uses: astral-sh/setup-uv@v4
        with: { enable-cache: true }
      - run: uv sync --dev
      - run: uv run ruff check .
      - run: uv run ruff format --check .
      - run: uv run mypy
      - run: uv run pytest
YAML
)"

gen_editorconfig
gen_license
gen_docs
gen_contributing
gen_readme "Python package" \
  "uv sync" "uv run ${PROJECT_SLUG}" "uv build" "uv run pytest" "make lint"

if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then
  if have_cmd uv; then info "uv sync"; uv sync --dev || warn "uv sync failed"; else warn "uv not installed"; fi
fi

finalize_git
common_done "Python package · ${PKG}"
