#!/usr/bin/env bash
# _common.sh — shared scaffolding library for the Cody scaffolder suite.
# Source this from every scaffold-<stack>.sh; do not execute directly.
#
# Public calling contract (all scripts):
#   scaffold-<stack>.sh <project-name> [parent-dir] [flags]
#
# Flags:
#   --force            allow scaffolding into an existing / non-empty directory
#   --install          install dependencies after scaffolding
#   --no-install       scaffold structure only (default), fast
#   --git              git init + initial commit (default)
#   --no-git           skip all git operations
#   --pm <name>        JS package manager: npm | pnpm | yarn | bun (default pnpm)
#   -h | --help        print usage and exit
#
# Environment overrides (defaults in parens):
#   SCAFFOLD_AUTHOR   (PaxLabs Inc.)      SCAFFOLD_EMAIL   (engineering@paxlabs.io)
#   SCAFFOLD_LICENSE  (MIT)               SCAFFOLD_YEAR    (current year)
#   SCAFFOLD_VCS_ORG  (paxlabs-inc)       SCAFFOLD_MODULE  (github.com/<org>/<name>)
#   SCAFFOLD_INSTALL  (0)                 SCAFFOLD_GIT     (1)
#   SCAFFOLD_PM       (pnpm)              SCAFFOLD_FORCE   (0)
#
# Exit codes: 0 ok · 2 usage error · 3 precondition failed · 4 tool missing

set -Eeuo pipefail

# ------------------------------------------------------------------ defaults --
: "${SCAFFOLD_AUTHOR:=PaxLabs Inc.}"
: "${SCAFFOLD_EMAIL:=engineering@paxlabs.io}"
: "${SCAFFOLD_LICENSE:=MIT}"
: "${SCAFFOLD_YEAR:=$(date +%Y)}"
: "${SCAFFOLD_VCS_ORG:=paxlabs-inc}"
: "${SCAFFOLD_INSTALL:=0}"
: "${SCAFFOLD_GIT:=1}"
: "${SCAFFOLD_PM:=pnpm}"
: "${SCAFFOLD_FORCE:=0}"

# ------------------------------------------------------------------- logging --
if [[ -t 2 && -z "${NO_COLOR:-}" ]]; then
  _C_RST=$'\033[0m'; _C_DIM=$'\033[2m'; _C_RED=$'\033[31m'
  _C_GRN=$'\033[32m'; _C_YLW=$'\033[33m'; _C_BLU=$'\033[34m'; _C_CYN=$'\033[36m'
else
  _C_RST=; _C_DIM=; _C_RED=; _C_GRN=; _C_YLW=; _C_BLU=; _C_CYN=
fi

log()  { printf '%s\n' "${_C_DIM}·${_C_RST} $*" >&2; }
info() { printf '%s\n' "${_C_BLU}▸${_C_RST} $*" >&2; }
ok()   { printf '%s\n' "${_C_GRN}✔${_C_RST} $*" >&2; }
warn() { printf '%s\n' "${_C_YLW}⚠${_C_RST} $*" >&2; }
err()  { printf '%s\n' "${_C_RED}✖${_C_RST} $*" >&2; }
die()  { err "$*"; exit "${2:-1}"; }
step() { printf '%s\n' "${_C_CYN}◆${_C_RST} ${_C_CYN}$*${_C_RST}" >&2; }

trap 'err "failed at ${BASH_SOURCE[0]}:${LINENO} (exit $?)"' ERR

# --------------------------------------------------------------- tool checks --
have_cmd() { command -v "$1" >/dev/null 2>&1; }
require_cmd() {
  local missing=0 c
  for c in "$@"; do
    if ! have_cmd "$c"; then err "required tool not found: $c"; missing=1; fi
  done
  [[ $missing -eq 0 ]] || die "install the missing tool(s) and retry" 4
}

# --------------------------------------------------------------- name helper --
# Slugify to a safe, lowercase, kebab project directory name.
slugify() {
  printf '%s' "$1" \
    | tr '[:upper:]' '[:lower:]' \
    | sed -E 's/[^a-z0-9]+/-/g; s/^-+//; s/-+$//'
}
# Package/module-identifier form (no leading digit, underscores allowed).
identify() {
  local s; s="$(slugify "$1" | tr '-' '_')"
  [[ "$s" =~ ^[0-9] ]] && s="pkg_${s}"
  printf '%s' "$s"
}
# PascalCase form for class names.
pascalize() {
  slugify "$1" | sed -E 's/(^|-)([a-z0-9])/\U\2/g'
}

# ----------------------------------------------------------------- arg parse --
# Sets globals: PROJECT_NAME PROJECT_SLUG PARENT_DIR TARGET_DIR
common_parse_args() {
  local stack_name="$1"; shift
  local -a pos=()
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --force)       SCAFFOLD_FORCE=1 ;;
      --install)     SCAFFOLD_INSTALL=1 ;;
      --no-install)  SCAFFOLD_INSTALL=0 ;;
      --git)         SCAFFOLD_GIT=1 ;;
      --no-git)      SCAFFOLD_GIT=0 ;;
      --pm)          SCAFFOLD_PM="${2:?--pm needs a value}"; shift ;;
      --pm=*)        SCAFFOLD_PM="${1#*=}" ;;
      -h|--help)     common_usage "$stack_name"; exit 0 ;;
      --)            shift; while [[ $# -gt 0 ]]; do pos+=("$1"); shift; done; break ;;
      -*)            die "unknown flag: $1" 2 ;;
      *)             pos+=("$1") ;;
    esac
    shift
  done

  [[ ${#pos[@]} -ge 1 ]] || { common_usage "$stack_name"; exit 2; }
  PROJECT_NAME="${pos[0]}"
  PROJECT_SLUG="$(slugify "$PROJECT_NAME")"
  [[ -n "$PROJECT_SLUG" ]] || die "project name '$PROJECT_NAME' slugifies to empty" 2
  PARENT_DIR="${pos[1]:-$PWD}"
  TARGET_DIR="${PARENT_DIR%/}/$PROJECT_SLUG"
}

common_usage() {
  cat >&2 <<EOF
${_C_CYN}scaffold-$1${_C_RST}  —  production scaffold for a $1 project

  usage: scaffold-$1.sh <project-name> [parent-dir] [flags]

  flags:
    --force            scaffold into an existing / non-empty directory
    --install          install dependencies (default: structure only)
    --no-install       skip dependency install
    --no-git           skip git init + initial commit
    --pm <name>        npm | pnpm | yarn | bun   (JS stacks, default: $SCAFFOLD_PM)
    -h, --help         this message

  env:  SCAFFOLD_AUTHOR  SCAFFOLD_EMAIL  SCAFFOLD_LICENSE  SCAFFOLD_MODULE
        SCAFFOLD_VCS_ORG SCAFFOLD_INSTALL SCAFFOLD_GIT     SCAFFOLD_PM
EOF
}

# ------------------------------------------------------------ dir lifecycle ---
common_init_target() {
  if [[ -e "$TARGET_DIR" ]]; then
    if [[ "$SCAFFOLD_FORCE" == "1" ]]; then
      warn "target exists, --force set: $TARGET_DIR"
    elif [[ -d "$TARGET_DIR" && -z "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]]; then
      : # empty dir, fine
    else
      die "target exists and is not empty (use --force): $TARGET_DIR" 3
    fi
  fi
  mkdir -p "$TARGET_DIR"
  cd "$TARGET_DIR"
  info "scaffolding into ${_C_CYN}$TARGET_DIR${_C_RST}"
}

# Write file only if absent (respects generators that come from CLIs).
# usage: write_if_absent <path> <<'EOF' ... EOF
write_if_absent() {
  local path="$1"
  if [[ -f "$path" && "$SCAFFOLD_FORCE" != "1" ]]; then
    log "keep existing $path"; cat >/dev/null; return 0
  fi
  mkdir -p "$(dirname "$path")"
  cat > "$path"
}

# ------------------------------------------------------- shared file writers --
gen_editorconfig() {
  write_if_absent .editorconfig <<'EOF'
root = true

[*]
charset = utf-8
end_of_line = lf
insert_final_newline = true
trim_trailing_whitespace = true
indent_style = space
indent_size = 2

[*.{go,mk}]
indent_style = tab

[Makefile]
indent_style = tab

[*.{py,rs}]
indent_size = 4

[*.md]
trim_trailing_whitespace = false
EOF
}

gen_license() {
  [[ "$SCAFFOLD_LICENSE" == "MIT" ]] || { log "license '$SCAFFOLD_LICENSE' not templated, skipping"; return 0; }
  write_if_absent LICENSE <<EOF
MIT License

Copyright (c) ${SCAFFOLD_YEAR} ${SCAFFOLD_AUTHOR}

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
EOF
}

gen_readme() {
  # args: <stack-label> <install-cmd> <dev-cmd> <build-cmd> <test-cmd> <lint-cmd>
  local label="$1" ins="$2" dev="$3" build="$4" test="$5" lint="$6"
  write_if_absent README.md <<EOF
# ${PROJECT_SLUG}

> ${label} project scaffolded for the Paxeer ecosystem.

## Prerequisites

See \`.tool-versions\` / \`.nvmrc\` / toolchain file for pinned runtime versions.

## Getting started

\`\`\`bash
${ins}      # install dependencies
${dev}      # run in development
${build}    # production build
${test}     # run the test suite
${lint}     # lint + format check
\`\`\`

## Layout

| Path        | Purpose                              |
|-------------|--------------------------------------|
| \`src/\`      | Application / library source         |
| \`tests/\`    | Test suites                          |
| \`docs/\`     | Architecture, ADRs, runbooks         |
| \`.github/\`  | CI workflows                         |

## Documentation

- [Architecture](docs/architecture.md)
- [Architecture Decision Records](docs/adr/)
- [Runbook](docs/runbook.md)
- [Contributing](CONTRIBUTING.md)

## License

${SCAFFOLD_LICENSE} © ${SCAFFOLD_YEAR} ${SCAFFOLD_AUTHOR}
EOF
}

gen_docs() {
  mkdir -p docs/adr
  write_if_absent docs/architecture.md <<EOF
# Architecture — ${PROJECT_SLUG}

## Context

_Describe the problem this component solves and where it sits in the system._

## Components

_Diagram + component responsibilities._

## Data flow

_Inbound → processing → outbound. Trust boundaries. Failure modes._

## Non-goals

_Explicitly out of scope._
EOF

  write_if_absent docs/runbook.md <<EOF
# Runbook — ${PROJECT_SLUG}

## Deploy

## Configuration & secrets

## Health & readiness

## Common failures

| Symptom | Likely cause | Action |
|---------|--------------|--------|
|         |              |        |

## Rollback
EOF

  write_if_absent docs/adr/0001-record-architecture-decisions.md <<EOF
# 1. Record architecture decisions

Date: ${SCAFFOLD_YEAR}-01-01

## Status

Accepted

## Context

We need to record the architectural decisions made on this project.

## Decision

We will use Architecture Decision Records, as described by Michael Nygard.

## Consequences

Decisions are captured in \`docs/adr/\` as immutable, numbered records.
EOF
}

gen_contributing() {
  write_if_absent CONTRIBUTING.md <<EOF
# Contributing to ${PROJECT_SLUG}

## Workflow

1. Branch from \`main\`: \`feat/…\`, \`fix/…\`, \`chore/…\`.
2. Keep changes focused; one logical change per PR.
3. All checks (lint, types, tests) must pass locally before pushing.
4. Conventional Commits for messages (\`feat:\`, \`fix:\`, \`docs:\`, …).

## Local checks

Run the full check suite before opening a PR — see the README task list.

## Code review

At least one approving review required. CI is a gate, not a suggestion.
EOF

  write_if_absent SECURITY.md <<EOF
# Security Policy

## Reporting a vulnerability

Report privately to ${SCAFFOLD_EMAIL}. Do not open public issues for
security-sensitive reports. We aim to acknowledge within 48 hours.

## Supported versions

The \`main\` branch and the latest tagged release receive security fixes.
EOF
}

gen_gitignore_base() {
  write_if_absent .gitignore <<'EOF'
# --- os / editor ---
.DS_Store
Thumbs.db
*.swp
*~
.idea/
.vscode/*
!.vscode/extensions.json
!.vscode/settings.json.sample

# --- env / secrets ---
.env
.env.*
!.env.example
*.local

# --- logs / tmp ---
*.log
logs/
tmp/
.cache/
coverage/
EOF
}

# Append a block to .gitignore (language extras).
gitignore_add() { printf '\n# --- %s ---\n%s\n' "$1" "$2" >> .gitignore; }

gen_github_ci() {
  # arg: <ci yaml body>  (the whole workflow file)
  mkdir -p .github/workflows
  write_if_absent .github/workflows/ci.yml <<EOF
$1
EOF
}

gen_dockerignore() {
  write_if_absent .dockerignore <<'EOF'
.git
.github
**/node_modules
**/target
**/dist
**/build
**/.venv
**/__pycache__
**/*.log
Dockerfile
.dockerignore
docs
EOF
}

# ------------------------------------------------------------------- git ------
finalize_git() {
  [[ "$SCAFFOLD_GIT" == "1" ]] || { log "git disabled"; return 0; }
  have_cmd git || { warn "git not found, skipping repo init"; return 0; }
  if [[ -d .git ]]; then log "git repo already initialised"; return 0; fi
  git init -q -b main
  git add -A
  GIT_AUTHOR_NAME="$SCAFFOLD_AUTHOR"  GIT_AUTHOR_EMAIL="$SCAFFOLD_EMAIL" \
  GIT_COMMITTER_NAME="$SCAFFOLD_AUTHOR" GIT_COMMITTER_EMAIL="$SCAFFOLD_EMAIL" \
    git commit -q -m "chore: scaffold ${PROJECT_SLUG}" || warn "nothing to commit"
  ok "git initialised (branch main)"
}

# JS package-manager dispatch helpers ----------------------------------------
pm_install() {
  case "$SCAFFOLD_PM" in
    npm)  npm install ;;
    pnpm) pnpm install ;;
    yarn) yarn install ;;
    bun)  bun install ;;
    *)    die "unknown package manager: $SCAFFOLD_PM" 2 ;;
  esac
}
pm_add() {  # pm_add [--dev] pkg...
  local dev=0; [[ "$1" == "--dev" ]] && { dev=1; shift; }
  case "$SCAFFOLD_PM" in
    npm)  npm install ${dev:+--save-dev} "$@" ;;
    pnpm) pnpm add ${dev:+--save-dev} "$@" ;;
    yarn) yarn add ${dev:+--dev} "$@" ;;
    bun)  bun add ${dev:+--dev} "$@" ;;
  esac
}
pm_dlx() {  # run a package binary without installing (create-* etc.)
  case "$SCAFFOLD_PM" in
    npm)  npx --yes "$@" ;;
    pnpm) pnpm dlx "$@" ;;
    yarn) yarn dlx "$@" ;;
    bun)  bunx "$@" ;;
  esac
}
pm_run_field() { # emit the pm invocation string for README ("pnpm run dev")
  case "$SCAFFOLD_PM" in
    npm)  echo "npm run $1" ;;
    pnpm) echo "pnpm $1" ;;
    yarn) echo "yarn $1" ;;
    bun)  echo "bun run $1" ;;
  esac
}

# --------------------------------------------- frontend post-scaffold layer ---
# Frontend CLIs generate package.json/.gitignore/README themselves. This layers
# the shared Paxeer skeleton on top without clobbering framework files.
# Also computes TARGET_DIR from where the CLI created the app (must be cwd).
frontend_extras() {
  local label="$1"
  write_if_absent .nvmrc <<<"22"
  if ! ls .prettierrc* prettier.config.* >/dev/null 2>&1; then
    write_if_absent .prettierrc.json <<'JSON'
{ "semi": true, "singleQuote": false, "trailingComma": "all", "printWidth": 100 }
JSON
  fi
  gen_editorconfig
  gen_license
  gen_docs
  gen_contributing
  ok "layered Paxeer skeleton (${label})"
}

# Frontend init: resolve target, guard non-empty, cd to PARENT so the framework
# CLI can create <slug>/ itself (Angular refuses to reuse an existing dir).
frontend_init() {
  if [[ -e "$TARGET_DIR" ]]; then
    if [[ "$SCAFFOLD_FORCE" == "1" ]]; then
      warn "target exists, --force set: $TARGET_DIR"
    elif [[ -d "$TARGET_DIR" && -z "$(ls -A "$TARGET_DIR" 2>/dev/null)" ]]; then
      rmdir "$TARGET_DIR" 2>/dev/null || true   # let the CLI recreate it
    else
      die "target exists and is not empty (use --force): $TARGET_DIR" 3
    fi
  fi
  mkdir -p "$PARENT_DIR"
  cd "$PARENT_DIR"
  export CI=1                     # keeps interactive CLIs from prompting
  require_cmd node
  info "scaffolding into ${_C_CYN}$TARGET_DIR${_C_RST}"
}

# ------------------------------------------------------------- final banner ---
common_done() {
  ok "scaffolded ${_C_CYN}${PROJECT_SLUG}${_C_RST} (${1})"
  printf '%s\n' "${_C_DIM}  cd ${TARGET_DIR}${_C_RST}" >&2
  [[ "$SCAFFOLD_INSTALL" == "1" ]] || \
    printf '%s\n' "${_C_DIM}  deps not installed (--install to install)${_C_RST}" >&2
}
