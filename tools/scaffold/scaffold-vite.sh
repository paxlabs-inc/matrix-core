#!/usr/bin/env bash
# scaffold-vite.sh — plain React + TypeScript SPA via create-vite (react-ts template).
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "vite" "$@"
frontend_init
step "Vite (React + TS) → $PROJECT_SLUG"

pm_dlx create-vite@latest "$PROJECT_SLUG" --template react-ts

cd "$TARGET_DIR"

# Vite's template does not install deps; honour --install.
if [[ "$SCAFFOLD_INSTALL" == "1" ]]; then info "installing ($SCAFFOLD_PM)"; pm_install; fi

# Add a vitest config (template ships none by default).
write_if_absent vitest.config.ts <<'EOF'
import { defineConfig } from "vitest/config";
import react from "@vitejs/plugin-react";

export default defineConfig({
  plugins: [react()],
  test: { environment: "jsdom", globals: true, css: true },
});
EOF

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
  location /assets/ { expires 1y; add_header Cache-Control "public, immutable"; }
}
EOF
gen_dockerignore

frontend_extras "Vite/React"

if [[ "$SCAFFOLD_GIT" == "1" && ! -d .git ]]; then finalize_git; fi
common_done "Vite React SPA · $SCAFFOLD_PM"
