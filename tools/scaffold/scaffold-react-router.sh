#!/usr/bin/env bash
# scaffold-react-router.sh — React Router v8 framework mode (formerly Remix).
# Requires Node >= 22, Vite 7+, React 19 (ESM-only). Default template = Node server.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "react-router" "$@"
frontend_init
step "React Router v8 → $PROJECT_SLUG"

args=( "$PROJECT_SLUG" --yes --no-agent-skills )
[[ "$SCAFFOLD_INSTALL" == "1" ]] && args+=( --install ) || args+=( --no-install )
[[ "$SCAFFOLD_GIT" == "1" ]]     || args+=( --no-git-init )

pm_dlx create-react-router@latest "${args[@]}"

cd "$TARGET_DIR"

# Default template already ships a Dockerfile; only add if absent.
write_if_absent Dockerfile <<'EOF'
# syntax=docker/dockerfile:1
FROM node:22-slim AS base
RUN corepack enable
WORKDIR /app

FROM base AS deps
COPY package.json pnpm-lock.yaml* package-lock.json* ./
RUN (pnpm install --frozen-lockfile 2>/dev/null || npm ci || npm install)

FROM base AS build
COPY --from=deps /app/node_modules ./node_modules
COPY . .
RUN (pnpm build 2>/dev/null || npm run build)

FROM node:22-slim AS runtime
ENV NODE_ENV=production
WORKDIR /app
USER node
COPY --from=build --chown=node:node /app/node_modules ./node_modules
COPY --from=build --chown=node:node /app/build ./build
COPY --from=build --chown=node:node /app/package.json ./package.json
EXPOSE 3000
CMD ["npx", "react-router-serve", "./build/server/index.js"]
EOF
gen_dockerignore

frontend_extras "React Router v8"

if [[ "$SCAFFOLD_GIT" == "1" && ! -d .git ]]; then finalize_git; fi
common_done "React Router v8 · $SCAFFOLD_PM"
