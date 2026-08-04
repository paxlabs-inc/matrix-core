#!/usr/bin/env bash
# scaffold-svelte.sh — SvelteKit app via `sv create` (minimal template, TS).
# No Dockerfile emitted: server output depends on the adapter (auto/node/static).
set -Eeuo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=tools/scaffold/_common.sh
# Resolved through the script directory at runtime.
# shellcheck disable=SC1091
source "$HERE/_common.sh"

common_parse_args "svelte" "$@"
frontend_init
step "SvelteKit → $PROJECT_SLUG"

args=( create "$PROJECT_SLUG" --template minimal --types ts --no-add-ons )
[[ "$SCAFFOLD_INSTALL" == "1" ]] && args+=( --install "$SCAFFOLD_PM" ) || args+=( --no-install )

pm_dlx sv "${args[@]}"

cd "$TARGET_DIR"

write_if_absent docs/deploy.md <<'EOF'
# Deploy — SvelteKit

SvelteKit uses `adapter-auto` by default, which selects an adapter for common
platforms at build time. For an explicit target, add one:

```bash
npx sv add                     # interactive add-ons (adapters, tailwind, ...)
npm i -D @sveltejs/adapter-node # standalone Node server
```

With `adapter-node`, `npm run build` emits `build/`; run `node build`.
EOF
gen_dockerignore

frontend_extras "SvelteKit"

if [[ "$SCAFFOLD_GIT" == "1" && ! -d .git ]]; then finalize_git; fi
common_done "SvelteKit · $SCAFFOLD_PM"
