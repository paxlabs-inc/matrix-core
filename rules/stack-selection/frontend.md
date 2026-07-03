# Frontend Framework Selection

Decision table for picking a frontend framework from the app class. Cite this in the SDR.
Default is the doctrine pick; deviation requires a stated, requirement-backed reason.

## Decision table

| App class | Default | Why | Do not default to |
|---|---|---|---|
| chat / realtime / collaborative | React Router framework mode (Remix) | Nested routes map to conversation/thread panes; loaders/actions give a clean server data seam for streaming, optimistic UI, and websocket-adjacent flows; SSR without ceremony | Next just because it is the reflex — its App Router adds RSC/caching complexity you do not need for a live, session-driven app |
| content / marketing / mostly-static | Astro | Ships zero JS by default, islands only where interactivity is real; best Lighthouse/CWV per unit effort; content collections + MD/MDX are first-class | A full SPA framework for pages that are 95% static text and images |
| internal enterprise / forms-heavy admin | Angular | Batteries-included: router, forms (reactive + typed), DI, HttpClient, i18n in one opinionated frame; typed reactive forms shine on dense CRUD; long-term maintainability across a rotating team | A hand-assembled React stack where every team wires forms/validation/routing differently |
| dashboard / data-dense internal tool | Angular or React Router | Angular if forms/CRUD dominate; React Router if the surface is bespoke visualization and interaction | — |
| classic web app, no strong signal | React (Vite SPA) or Next | Only when no class signal dominates. Vite + React for a pure SPA; Next when you genuinely need file-based routing + SSR/SSG out of the box | Reaching for Next reflexively and then not using SSR/RSC at all |

## Reasoning notes

### chat / realtime → React Router (Remix framework mode), not Next

Realtime apps are session- and mutation-heavy, not content-cache-heavy. React Router's
loader/action model gives you a straight line: URL → loader → server data → component, and
`action` → mutation → revalidation. That is exactly the shape of "send message, revalidate
thread." Next's App Router optimizes for a different world (RSC, aggressive route caching,
static/ISR); those are a tax, not a benefit, when almost everything is live and per-user.
Pick React Router v7 framework mode. See the `react-router-framework` skill.

### content / marketing → Astro

The metric that matters for content is time-to-first-byte and JS shipped. Astro renders to
HTML and hydrates only the islands you explicitly opt in. A marketing site built on a SPA
framework ships a runtime to render text — that is backwards. Use Astro; drop in a React or
Svelte island for the one interactive widget. Next is an acceptable deviation only if the
site is really an app with a marketing skin. See the `astro-site` skill.

### internal enterprise → Angular

Internal admin tools are dominated by forms, tables, permissions, and longevity across a
team that turns over. Angular's opinionatedness is the feature here: one way to do routing,
one forms system (typed reactive forms), DI for testable services, built-in HttpClient and
i18n. It removes the per-team bikeshedding that makes React admin apps diverge. Choose
Angular for forms-heavy enterprise UI. See the `angular-app` skill.

### SvelteKit — a defensible deviation

SvelteKit is a strong pick when bundle size and authoring ergonomics are the priority and
the team is comfortable with Svelte: less runtime, less boilerplate, excellent DX. It is a
legitimate deviation for content or app-shaped surfaces — state the reason (bundle budget,
team fluency) in the SDR. See the `sveltekit-app` skill.

## Anti-default guardrail

If your framework answer is "Next.js" no matter whether the app is a chat client, a
marketing page, or an enterprise admin panel — stop. That uniform answer is the failure the
SDR gate exists to catch. Re-derive from the table above. A framework you chose without the
requirements pointing to it is a framework you did not choose.
