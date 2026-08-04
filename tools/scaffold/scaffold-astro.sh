#!/usr/bin/env bash
# scaffold-astro.sh — Astro site via create-astro (minimal template, strict TS).
# Requires Node >= 22.12. No Dockerfile emitted: output target depends on the
# chosen adapter (static vs @astrojs/node) — see docs/runbook.md.
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "astro" "$@"
frontend_init
step "Astro → $PROJECT_SLUG"

args=( "$PROJECT_SLUG" --template minimal --typescript strict --skip-houston -y )
[[ "$SCAFFOLD_INSTALL" == "1" ]] && args+=( --install ) || args+=( --no-install )
[[ "$SCAFFOLD_GIT" == "1" ]]     && args+=( --git )     || args+=( --no-git )

pm_dlx create-astro@latest "${args[@]}"

cd "$TARGET_DIR"

write_if_absent docs/deploy.md <<'EOF'
# Deploy — Astro

Default output is **static** (`dist/`), servable by any static host or the nginx
image pattern used by the Vite/Vue scaffolds.

For SSR, add an adapter and set `output: "server"`:

```bash
npx astro add node        # or @astrojs/cloudflare, @astrojs/vercel, ...
```

Then build with `astro build` and run the emitted server entry.
EOF
gen_dockerignore

frontend_extras "Astro"

if [[ "$SCAFFOLD_GIT" == "1" && ! -d .git ]]; then finalize_git; fi
common_done "Astro · $SCAFFOLD_PM"
