#!/usr/bin/env bash
# scaffold-vue.sh — Vue 3 SPA via create-vue (TS, Router, Pinia, Vitest, ESLint+Prettier).
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=_common.sh
source "$HERE/_common.sh"

common_parse_args "vue" "$@"
frontend_init
step "Vue 3 → $PROJECT_SLUG"

# create-vue reads feature flags → fully non-interactive when all are supplied.
pm_dlx create-vue@latest "$PROJECT_SLUG" \
  --typescript --router --pinia --vitest --eslint-with-prettier --force

cd "$TARGET_DIR"

if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then info "installing ($SCAFFOLD_PM)"; pm_install; fi

write_if_absent Dockerfile <<'EOF'
# syntax=docker/dockerfile:1
FROM node:22-slim AS build
RUN corepack enable
WORKDIR /app
COPY . .
RUN (pnpm install --frozen-lockfile 2>/dev/null || npm ci || npm install) && \
    (pnpm build 2>/dev/null || npm run build)

FROM nginx:1.27-alpine AS runtime
COPY --from=build /app/dist /usr/share/nginx/html
COPY nginx.conf /etc/nginx/conf.d/default.conf
EXPOSE 80
EOF

write_if_absent nginx.conf <<'EOF'
server {
  listen 80;
  root /usr/share/nginx/html;
  index index.html;
  location / { try_files $uri $uri/ /index.html; }
}
EOF
gen_dockerignore

frontend_extras "Vue"

if [[ "$SCAFFOLD_GIT" == "1" && ! -d .git ]]; then finalize_git; fi
common_done "Vue 3 SPA · $SCAFFOLD_PM"
