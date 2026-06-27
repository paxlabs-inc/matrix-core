## **MUST RUN cortex_recall MCP hook or command before starting**

## Overview

This plan implements the Construct OS Shell as a **mostly additive** layer over the existing,
frozen stack: the 8 per-primitive renderers (web `apps/client/components/matrix/construct/*` and
mobile `apps/mobile/src/components/neo/construct/*`) and the Go `construct/{schema,transport,
projection,backchannel}` packages are **REUSED, not rebuilt**. Server work is Go (a new
`construct/surfacestore` generalizing `neo/internal/trace`, a thin `GET /construct/state` read
route, and a broker tee wired as a sibling of the liaison narrator). Client work is TypeScript
across both `apps/client` (Next.js web) and `apps/mobile` (Expo/RN), built on a shared
Surface_State_Model from day one.

The plan is ordered so the **MVP first-slice (tasks 1–9) is independently shippable and
verifiable** — one persistent home that survives reload, a live activity Timeline, one level of
descent (Timeline step → linked Stream raw), chat-as-panel (minimal frame inversion), on ONE shell
adapter, with the shared model and a verified live Ask back-channel. Tasks 10+ are the expansion
roadmap (E1–E6), each building on the MVP core.

Implementation languages are fixed by the design: **Go** (server) and **TypeScript** (both
clients). This is a property-based-testing spec; property tests use `fast-check` (TS client) and
Go `testing/quick` (server), per the design Testing Strategy.

### Hard constraints (apply to every task; encoded as acceptance notes throughout)

- **Side-channel invariant (D11):** `surfacestore` + shell are a pure VIEW layer — never sign,
  never write cortex, never mutate plan/walk. The only agent feedback is a validated `Ask` answer
  entering as a recorded INPUT. (R11, Property 5.)
- **No protocol jargon in UI:** no MCL/cortex/Merkle/replay/SSE strings; consumer-readable copy. (R12.)
- **UI house rules:** background-color contrast for separation only — no border strokes; no emojis,
  no purple gradients, no glow; `#004CED` single accent; Inter + JetBrains Mono. (R13.)
- **Transport invariant:** surfaces ride the existing chat SSE/WS transport; no new agent→client
  wire path. (R14, i7.)
- **Codegen'd types are never hand-edited:** Go-side types that reach the client are regenerated
  into `types.gen.ts`. (R15.1.)
- **Persistence off the hot path:** async writer, drop-on-saturation; per-user `/data` isolation;
  conversation-scoped addresses. (R16, R15.2/15.3.)
- **Frozen ontology:** 8 primitives + 5 attributes are frozen; renderers reused untouched. (R6.)
- **Dev box:** never `git commit`/`git push`; no deployment (production redeploy is Andrew-gated,
  out of scope).

---

## Tasks
