# AGENTS.md

## Cursor Cloud specific instructions

### Services

| Service     | Command                    | Notes                                  |
| ----------- | -------------------------- | -------------------------------------- |
| Next.js dev | `pnpm dev`                 | http://localhost:3000                  |
| Production  | `pnpm build && pnpm start` | After `pnpm install --frozen-lockfile` |

Unauthenticated local dashboard: `http://localhost:3000/en?dev=1` (non-production only).

Locales are URL-prefixed (`/en`, `/de`, …). Root `/` redirects to `/en`.

Public legal routes (no auth): `/legal`, `/terms`, `/privacy`, `/acceptable-use`, `/cookies`, `/risk-disclosure`, `/dmca`. Content is sourced from `public/legal/*.html` into `lib/legal/documents.ts` (regenerate with the extraction script in that folder’s commit history or re-run the node one-liner against `public/legal`). Pages use app Tailwind/shadcn styling — not `public/legal/legal.css`.

Cookie consent uses `mx_consent` cookie + `window.MatrixConsent` API (`lib/consent/matrix-consent.ts`).

### Quality gates

- `pnpm lint` / `pnpm typecheck` / `pnpm test`
- `pnpm test:coverage` — coverage thresholds in `vitest.config.ts`
- `pnpm check:bundle` — run after `pnpm build`
- `pnpm check:i18n` — required namespaces only
- `pnpm test:e2e` — requires `pnpm build` first; Playwright starts `pnpm start`. First run needs `pnpm exec playwright install chromium`.

### Environment

- Copy `.env` or set `NEXT_PUBLIC_MATRIX_ROUTER_URL`, Supabase vars for auth.
- **Upstash rate limiting**: `UPSTASH_REDIS_REST_URL` + `UPSTASH_REDIS_REST_TOKEN` (optional; in-memory fallback).
- **Sentry**: `NEXT_PUBLIC_SENTRY_DSN` + `NEXT_PUBLIC_RELEASE`.

### Architecture notes

- Edge proxy: `proxy.ts` (CSP nonce, rate limit, Supabase session refresh, auth gate).
- Rendering/cache ADRs: `docs/adr/001-rendering-and-cache.md`, `docs/adr/002-stale-data-tolerance.md`.
- SSE multiplexer: `lib/realtime/sse-hub.ts`.
