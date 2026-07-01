<h1 align="center">Matrix Client</h1>

<p align="center">
  <img src="https://docs.matrixmcl.com/_jd/logo/wordmark.webp?v=mqi8nza6"
</p>

<p align="center">
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Project-Matrix-0A0A0A?style=flat-square" alt="Project: Matrix" /></a>
  <a href="https://labs.paxeer.app"><img src="https://img.shields.io/badge/Built%20by-PaxLabs-0A0A0A?style=flat-square" alt="Built by PaxLabs" /></a>
  <a href="../LICENSE.md"><img src="https://img.shields.io/badge/License-Matrix--Protocol-0A0A0A?style=flat-square" alt="License: Matrix-Protocol" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Status-Active-0A0A0A?style=flat-square" alt="Status: Active" /></a>
  <a href="#"><img src="https://img.shields.io/badge/Framework-Next.js%2016-000000?style=flat-square&logo=next.js&logoColor=white" alt="Next.js 16" /></a>
  <a href="#"><img src="https://img.shields.io/badge/React-19-61DAFB?style=flat-square&logo=react&logoColor=white" alt="React 19" /></a>
</p>

<p align="center">
  <img src="https://img.shields.io/badge/TypeScript-Strict-3178C6?style=flat-square&logo=typescript&logoColor=white" alt="TypeScript" />
  <img src="https://img.shields.io/badge/TailwindCSS-4-06B6D4?style=flat-square&logo=tailwindcss&logoColor=white" alt="TailwindCSS 4" />
  <img src="https://img.shields.io/badge/TanStack%20Query-5-FF4154?style=flat-square&logo=reactquery&logoColor=white" alt="TanStack Query" />
  <img src="https://img.shields.io/badge/i18n-5%20locales-0A0A0A?style=flat-square" alt="i18n" />
  <img src="https://img.shields.io/badge/Playwright-E2E-2EAD33?style=flat-square&logo=playwright&logoColor=white" alt="Playwright" />
  <img src="https://img.shields.io/badge/Vitest-Unit-6E9F18?style=flat-square&logo=vitest&logoColor=white" alt="Vitest" />
</p>

---

## What is the Matrix Client?

The Matrix Client is the consumer-facing web application for [Matrix Core](../README.md) -- the surface where a human actually talks to their agent, watches it work, and manages the wallet and credentials that back it. It is a Next.js (App Router) application that authenticates the user, provisions their personal per-user daemon through the [router](../router), and streams the agent's live transcript back over Server-Sent Events.

It is not a static dashboard bolted onto an API. The chat surface is the **default post-login view**: every message goes to **Neo**, the conversational Liaison agent, which triages between answering directly and dispatching a full MCL walk, then narrates that walk back in natural language while the underlying compiler/planner/executor pipeline runs. Wallets, agent policies, skills, and settings live one tab away, in the sidebar.

## Core Capabilities

| Surface | What it does |
|---------|---------------|
| **Agent Chat** | Chat-first home surface (`components/matrix/agent-chat.tsx`). Streams `chat.*` narration and tool/step events over SSE, renders Neo's live workspace (thinking strip, terminal/browser/editor/search steps, sub-agent swarm cards). |
| **Wallet** | Primary embedded wallet (Supabase-authenticated) that owns one or more agent wallets. Balance, holdings, transaction history via the Paxeer explorer, plus fund / sweep / freeze / policy controls per agent. |
| **Run Transcript** | Per-intent replay of the deterministic MCL walk -- every compile, plan, gate, tool call, and envelope signature, journaled and inspectable. |
| **Provisioning** | Gated onboarding flow that provisions a user's dedicated Fly Machine daemon on first login before the dashboard is reachable. |
| **Settings & Policies** | Model selection, autonomy level, spend caps, and skill/tool visibility, all flowing into agent dispatch. |
| **Legal & Compliance** | Statically served legal documents (`/legal`, `/terms`, `/privacy`, ...) reachable without a session, plus cookie consent management. |

## Construct -- The Agent Surface Protocol

The [Construct](../construct) is how Neo shows its work instead of just describing it. Most agent UIs are a single channel -- a token stream appended to a transcript -- and everything richer than that is either opaque or bespoke and unsafe. Construct closes that gap with a **frozen, finite alphabet of eight surface primitives** (`construct.frozen.kvx`) that the agent fills but never escapes:

- `Narration` -- prose commentary
- `Metric` -- a number with a trend
- `Entity` -- a structured object with affordances
- `Structure` -- tabular / nested data
- `Stream` -- a live, appendable feed
- `Timeline` -- ordered, timestamped events
- `Canvas` -- a freeform spatial region
- `Ask` -- a typed round-trip question back to the agent

decorated by five shared attributes -- `stakes`, `ref`, `confidence`, `cost`, `temporality`. **The keystone invariant:** the human authors the alphabet and the fixed renderers; the *agent* only chooses which primitive to fill and how. It can never emit arbitrary markup -- an unknown surface kind degrades to nothing, never to executed UI.

On the backend, `construct/internal/projection` turns raw agent-world-state into typed surfaces, streamed over the existing SSE/WS pipe by `construct/internal/transport`, with `construct/internal/backchannel` carrying typed `Ask` responses back mid-run. On the client:

| Path | Role |
|------|------|
| `components/matrix/construct/` | The trusted renderer per primitive (`narration.tsx`, `metric.tsx`, `entity.tsx`, `structure.tsx`, `stream.tsx`, `timeline.tsx`, `canvas.tsx`, `ask.tsx`) plus `surface-renderer.tsx` -- the dispatcher that switches on `surface.kind` -- and `decoration.tsx` for the shared attribute row. |
| `components/matrix/shell/` | The Construct OS shell: a **frame inversion** where the environment is the page ROOT and the Neo conversation is exactly one `narration` region living inside it, never the other way around. Lays placed surfaces out by region (`shell.tsx`), with an activity timeline (`activity-region.tsx`) and a descent-to-raw overlay (`raw-focus-overlay.tsx`) for dropping back to the unstructured transcript. |
| `lib/construct/` | `workspace.ts` (the shared `SurfaceWorkspace` model both the chat and shell adapters read), `feed.ts` / `use-surface-feed.ts` (live SSE ingestion into surfaces), `adapter.ts`, `focus.ts`, `use-descent.ts`, and `types.gen.ts` (TypeScript types generated from the Go schema -- never hand-edited). |

Separation between regions is by background-tone contrast only -- never a border, shadow, or glow -- and the shell's only accent color is Paxeer Blue (`text-pax`). See the [Construct README](../construct/README.md) and `construct.frozen.kvx` for the full ontology and invariants.

## Architecture

```
                     browser
                        |
                        v
              +-------------------+
              |   proxy.ts (edge) |   CSP nonce, rate limit,
              |                   |   Supabase session refresh,
              +---------+---------+   auth gate, i18n routing
                        |
                        v
              +-------------------+
              |  Next.js App      |   RSC by default, "use client"
              |  Router (app/)    |   at the leaves
              +---------+---------+
                        |
          +-------------+-------------+
          |                           |
          v                           v
+-------------------+       +-----------------------+
|  TanStack Query    |       |  SSE hub               |
|  (hooks/api/*)     |       |  (lib/realtime/*)      |
+---------+----------+       +-----------+------------+
          |                             |
          v                             v
   Matrix Router  <---------------------+
   (auth, provisioning, proxy)
          |
          v
   Per-user daemon (Fly Machine)
   Neo (chat) + MCL (compiler -> planner -> executor)
          |
          v
   cortex memory graph + MCP tool dispatch
```

`proxy.ts` is the single edge entry point: it issues a per-request CSP nonce, applies Upstash-backed rate limiting, refreshes the Supabase session, gates every non-public route behind auth, and delegates locale routing to `next-intl`. Once past the edge, the dashboard talks to the [Matrix Router](../router) for provisioning and to the per-user daemon (via the router's reverse proxy) for chat, transcripts, and settings -- never directly to backend services.

## The Stack

| Area | Choice |
|------|--------|
| **Framework** | Next.js 16 (App Router), React 19, TypeScript strict |
| **Styling** | Tailwind CSS 4, shadcn/ui primitives (`components/ui`), custom design tokens (`lib/design`) |
| **Data fetching** | TanStack Query for server state, Zustand where local UI state needs a store |
| **Real-time** | Server-Sent Events via a multiplexed hub (`lib/realtime/sse-hub.ts`), `hooks/api/useChat.ts` / `useRunEvents.ts` |
| **Auth** | Supabase (`@supabase/ssr`), session cookie refreshed at the edge in `proxy.ts` |
| **i18n** | `next-intl`, URL-prefixed locales (`/en`, `/de`, `/es`, `/ja`, `/zh-CN`), required-namespace CI check |
| **Chat / AI UI** | `ai-elements` primitive library, `streamdown` for streaming markdown, `assistant-ui` |
| **Wallet & chain** | Paxeer embedded-wallet REST client (`lib/paxeer`), Blockscout-compatible explorer facade |
| **Observability** | Sentry (`@sentry/nextjs`), Web Vitals reporting, structured console logging |
| **Testing** | Vitest + Testing Library (unit/component), Playwright (E2E), axe (accessibility), Lighthouse CI |
| **Quality gates** | ESLint + Prettier, Husky + lint-staged, `tsc --noEmit` strict, bundle-budget and i18n-key checks in CI |

## Quickstart

### Prerequisites

- Node.js 20+
- pnpm 10.x (`packageManager` pinned in `package.json`)
- A running [Matrix Router](../router) endpoint (or point at a shared staging instance)
- A Supabase project for auth

### Install & Configure

```bash
cd client
pnpm install --frozen-lockfile

# Copy and fill in the environment file
cp .env.example .env.local
```

```bash
# .env.local
NEXT_PUBLIC_MATRIX_ROUTER_URL=       # Matrix Router base URL
NEXT_PUBLIC_SUPABASE_URL=            # Supabase project URL
NEXT_PUBLIC_SUPABASE_ANON_KEY=       # Supabase anon key (safe to expose)
NEXT_PUBLIC_SENTRY_DSN=              # optional
NEXT_PUBLIC_RELEASE=1.0.0            # optional
NEXT_PUBLIC_DEBUG=false              # verbose console logs in dev
```

### Run

```bash
# Development server
pnpm dev                 # http://localhost:3000

# Unauthenticated local dashboard (non-production only)
# http://localhost:3000/en?dev=1

# Production build
pnpm build
pnpm start
```

### Quality Gates

```bash
pnpm lint             # ESLint
pnpm typecheck        # tsc --noEmit
pnpm test             # Vitest unit/component tests
pnpm test:coverage    # with coverage thresholds (vitest.config.ts)
pnpm test:e2e         # Playwright (requires pnpm build first)
pnpm check:bundle     # bundle-size budget (run after pnpm build)
pnpm check:i18n       # required i18n namespaces present in every locale
pnpm lhci             # Lighthouse CI
```

## Project Layout

| Path | Contents |
|------|----------|
| `app/[locale]/` | Locale-scoped routes: dashboard, login, auth callback, legal pages |
| `app/api/` | Route handlers (Web Vitals ingestion, auth session) |
| `components/matrix/` | Product components: agent chat, wallet panel, run transcript, provisioning |
| `components/matrix/construct/` | Trusted per-primitive Construct renderers + the `SurfaceRenderer` dispatcher |
| `components/matrix/shell/` | Construct OS shell -- environment-as-root frame, region layout, descent-to-raw |
| `components/ai-elements/` | Streaming chat/markdown primitives |
| `components/tool-ui/` | Structured renderers for tool-call results (plans, approvals, stats, order summaries) |
| `components/ui/` | shadcn/ui-based design system primitives |
| `hooks/api/` | TanStack Query hooks and SSE-backed hooks (`useChat`, `useRunEvents`, `useWallet`, ...) |
| `lib/api/` | Typed API clients (`chat.ts`, `events.ts`, ...) |
| `lib/paxeer/` | Embedded-wallet REST client, token registry, explorer facade, formatting helpers |
| `lib/realtime/` | SSE hub and stream utilities |
| `lib/security/` | CSP builder and related edge security helpers |
| `lib/construct/` | Shared `SurfaceWorkspace` model, live surface feed ingestion, generated Construct types |
| `lib/auth/` | Supabase client/session helpers (browser + server) |
| `i18n/` | `next-intl` routing config and message loading |
| `messages/` | Locale message bundles (`en.json`, `de.json`, `es.json`, `ja.json`, `zh-CN.json`) |
| `docs/adr/` | Architecture Decision Records (rendering/cache strategy, stale-data tolerance) |
| `e2e/`, `tests/` | Playwright specs and Vitest test suites |
| `proxy.ts` | Edge middleware: CSP nonce, rate limiting, session refresh, auth gate, i18n |

## Documentation

| Resource | Description |
|----------|-------------|
| [Rendering & Cache ADR](docs/adr/001-rendering-and-cache.md) | Rendering strategy per route and caching policy |
| [Stale-Data Tolerance ADR](docs/adr/002-stale-data-tolerance.md) | Per-data-type freshness guarantees (prices/balances vs static content) |
| [Construct README](../construct/README.md) | The frozen surface-primitive ontology, projection engine, and back-channel this UI renders |
| [AGENTS.md](AGENTS.md) | Cursor Cloud environment notes: services, quality gates, architecture pointers |
| [Matrix Core README](../README.md) | The full agent operating framework this client is a surface for |
| [Full Documentation](https://docs.matrixmcl.com) | Complete documentation site |

## Contributing

The Matrix Client follows the same contribution policy as the rest of Matrix Core: the `main` branch is developed by the core team, and unsolicited pull requests are generally not merged without prior coordination. Read the [Contributing Guide](../CONTRIBUTING.md) before opening anything. Issues, bug reports, and security disclosures are always welcome.

## License

Matrix Core, including this client, is source-available under the [Matrix-Protocol License](../LICENSE.md). See the root [README](../README.md#license) for full terms and commercial trigger thresholds.

---

<p align="center">
  Built by <a href="https://labs.paxeer.app"><strong>PaxLabs Inc.</strong></a>
</p>

<p align="center">
  <sub>SPDX-License-Identifier: Matrix-Protocol</sub>
</p>

