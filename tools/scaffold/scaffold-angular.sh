#!/usr/bin/env bash
# scaffold-angular.sh — Angular app via @angular/cli new (routing, SCSS, no SSR).
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "angular" "$@"
frontend_init
step "Angular → $PROJECT_SLUG"

# Angular only supports npm/pnpm/yarn/cnpm/bun as --package-manager.
apm="$SCAFFOLD_PM"

args=( new "$PROJECT_SLUG"
  --routing --style=scss --ssr=false
  --package-manager="$apm" --skip-git --defaults )
[[ "$SCAFFOLD_INSTALL" == "1" ]] || args+=( --skip-install )

pm_dlx @angular/cli@latest "${args[@]}"

cd "$TARGET_DIR"

# Angular 17+ emits dist/<project>/browser
write_if_absent Dockerfile <<EOF
# syntax=docker/dockerfile:1
FROM node:22-slim AS build
RUN corepack enable
WORKDIR /app
COPY . .
RUN (pnpm install --frozen-lockfile 2>/dev/null || npm ci || npm install) && \\
    (pnpm build 2>/dev/null || npm run build)

FROM nginx:1.27-alpine AS runtime
COPY --from=build /app/dist/${PROJECT_SLUG}/browser /usr/share/nginx/html
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

frontend_extras "Angular"

if [[ "$SCAFFOLD_GIT" == "1" && ! -d .git ]]; then finalize_git; fi
common_done "Angular · $SCAFFOLD_PM"
