#!/usr/bin/env bash
# scaffold-nextjs.sh — Next.js app (App Router, TS, Tailwind, ESLint) via create-next-app.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_common.sh
source "$HERE/_common.sh"

common_parse_args "nextjs" "$@"
frontend_init
step "Next.js → $PROJECT_SLUG"

args=( "$PROJECT_SLUG"
  --ts --eslint --app --src-dir --tailwind --turbopack
  --import-alias "@/*" "--use-${SCAFFOLD_PM}" --yes )
[[ "$SCAFFOLD_INSTALL" == "1" ]] || args+=( --skip-install )
[[ "$SCAFFOLD_GIT" == "1" ]]     || args+=( --disable-git )

pm_dlx create-next-app@latest "${args[@]}"

cd "$TARGET_DIR"

write_if_absent Dockerfile <<'EOF'
# syntax=docker/dockerfile:1
# Requires next.config: `output: "standalone"` for the slimmest image.
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
COPY --from=build --chown=node:node /app/.next/standalone ./
COPY --from=build --chown=node:node /app/.next/static ./.next/static
COPY --from=build --chown=node:node /app/public ./public
EXPOSE 3000
CMD ["node", "server.js"]
EOF
gen_dockerignore
gen_license >/dev/null 2>&1 || true

frontend_extras "Next.js"

if [[ "$SCAFFOLD_GIT" == "1" && ! -d .git ]]; then finalize_git; fi
common_done "Next.js · $SCAFFOLD_PM"
