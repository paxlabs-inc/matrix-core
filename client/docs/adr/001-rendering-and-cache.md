# ADR 001: Rendering strategy & cache policy

## Status

Accepted — 2026-06-06

## Context

Matrix is an authenticated agent-operations dashboard. Every page read depends on
the `[locale]` URL segment (next-intl) and the edge proxy auth gate, which forces
dynamic SSR for app routes.

## Decision

| Route                                  | Strategy              | Runtime | Rationale                             |
| -------------------------------------- | --------------------- | ------- | ------------------------------------- |
| `/[locale]/`                           | SSR (`force-dynamic`) | Node.js | Auth + locale segment; live dashboard |
| `/[locale]/login`, `/[locale]/auth/*`  | SSR (`force-dynamic`) | Node.js | Auth flows                            |
| `/`                                    | Redirect              | Edge    | next-intl middleware → `/en`          |
| `/api/vitals`                          | SSR (`force-dynamic`) | Edge    | Low-latency RUM beacon                |
| `/api/auth/session`                    | SSR (`force-dynamic`) | Edge    | httpOnly session sync                 |
| `/manifest.webmanifest`, `/robots.txt` | Static                | N/A     | No user context                       |

### Client data (TanStack Query)

| Data type                   | staleTime                  | Must never be stale?                                         |
| --------------------------- | -------------------------- | ------------------------------------------------------------ |
| Runs / dashboard snapshot   | 30s                        | No — refetch on focus                                        |
| Agent/tool catalogue        | 30s (override 5m possible) | No                                                           |
| Wallet balances / portfolio | 20–60s                     | **Yes for trading** — UI shows “as of” + refetch on mutation |
| SSE event stream            | N/A (push)                 | **Yes** — sequence gaps trigger resync                       |

### HTTP fetch (`apiFetch`)

Default `cache: 'no-store'` for all Matrix Router JSON calls. Opt in to caching
per-call only for immutable catalogue endpoints when the router sends
`Cache-Control`.

## Consequences

- No ISR/SSG for authenticated surfaces — acceptable for a signed-in product.
- CDN caches static assets (`/_next/static`, fonts, icons) only.
- Document per-query `staleTime` overrides in hooks when adding new resources.
