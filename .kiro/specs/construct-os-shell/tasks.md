# Implementation Plan: Construct OS Shell

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

## MVP First Slice (independently shippable — proves "one persistent home that breathes")

- [x] 1. Server persistence foundation — `construct/surfacestore` (Go, GENERALIZES `neo/internal/trace`)
  - [x] 1.1 Create the `construct/surfacestore` package generalizing the F3 trace store
    - Port the `neo/internal/trace` design: async background writer, JSONL append, atomic rollup,
      `/data`-rooted `Dir(override, cortexRoot)` resolution, disabled (empty-dir) no-op
    - Define `Frame{Seq,Ts,Phase,Type,Fields}` mirroring the daemon SSE Event shape; key by
      `conversationID` (not run); reject `conversationID` containing path separators
    - Implement `Open`, `Record` (enqueue-only, non-blocking, drop-on-saturation), `Load`
      (oldest-first, capped to newest `retain`, skip crash-truncated final line), `Enabled`,
      `Flush`, `Close`
    - Side-channel: no cortex write, no signing, no plan/walk mutation
    - _Requirements: 1.1, 11.1, 15.2, 16.1, 16.2_
  - [x] 1.2 Port the `neo/internal/trace` unit-test suite to `surfacestore`
    - Async record/flush, atomic rollup at 2×retain, crash-truncated-line skip, disabled no-op,
      path-separator rejection — re-keyed by conversation, asserting `construct.surface[.patch]`
      round-trip
    - _Requirements: 1.1, 15.2, 16.1, 16.2_
  - [x] 1.3 Write Go `testing/quick` property test for persistence durability/ordering
    - For any generated frame sequence, `Load(Record*…)` returns frames oldest-first, capped to
      `retain`, with no reordering — the server-side foundation of Property 1 durability
    - **Property 1 (durability half): rehydration source fidelity**
    - **Validates: Requirements 1.1, 16.3**

- [x] 2. Rehydration read path — `GET /construct/state` (Go, NEW thin route) + codegen
  - [x] 2.1 Define `Frame`/`StateResponse` as codegen'd wire types and regenerate client types
    - Add the persisted `Frame` and `StateResponse{ConversationID,Frames,LastSeq}` to the Go
      schema codegen source (`construct/internal/codegen`) and regenerate
      `apps/client/lib/construct/types.gen.ts` (and the mobile mirror) — never hand-edit generated output
    - _Requirements: 14.2, 15.1_
  - [x] 2.2 Implement the read-only `GET /construct/state?conversation_id=&since_seq=` daemon route
    - Call `surfacestore.Load`, return ordered frames capped to the retained-frame count plus
      `last_seq`; honor `since_seq` for catch-up; scope strictly by `conversation_id` (no cross-user access)
    - Read-only: no writes, no signing, no plan/walk mutation
    - _Requirements: 1.1, 8.1, 14.2, 15.3, 16.3_
  - [x] 2.3 Write unit tests for the rehydration route
    - `since_seq` cursor returns only newer frames; cap bounds the response to `retain`;
      conversation scoping rejects/never leaks another conversation's frames
    - _Requirements: 8.1, 15.3, 16.3_

- [x] 3. Broker tee wiring (Go — sibling of the liaison narrator)
  - [x] 3.1 Subscribe `surfacestore` to the daemon SSE broker and tee construct frames
    - In the daemon broker (`executor/cmd/mcl-execute`), add a subscription sibling to the liaison
      narrator that calls `store.Record(conversationID, Frame{…})` only for `phase == "construct"`
      frames (`construct.surface[.patch]`); add no new wire path
    - Tee is async and best-effort: it must never block the broker publish path
    - _Requirements: 1.1, 11.1, 14.1, 16.1, 16.2_
  - [x] 3.2 Write integration test asserting the tee is non-blocking and drop-on-saturation
    - Saturate the writer queue and assert broker publish latency is unaffected and frames are
      dropped (not blocked); assert no cortex/plan/walk side effects
    - _Requirements: 11.1, 16.1, 16.2_

- [x] 4. Shared Surface_State_Model core (TypeScript, NEW — consumed by BOTH apps)
  - [x] 4.1 Define the shared `SurfaceWorkspace` model + deterministic `placeSurface` policy
    - In a shared module consumable by both `apps/client` and `apps/mobile`, define
      `PlacedSurface`, `Placement{region,pinned,lifecycle,zOrder?,gridSlot?}`, `FocusState`,
      `AskInboxEntry`, and `SurfaceWorkspace`
    - Implement `placeSurface(surface, prior?)`: `region` is a pure function of `kind` (+
      `attributes` for inbox routing) over the 7 regions; preserve `prior.pinned`; unknown kind →
      deterministic default region (never throws, never leaves a surface unplaced)
    - _Requirements: 2.1, 2.4, 2.6, 2.7, 3.3_
  - [x] 4.2 Write `fast-check` property test for placement determinism
    - Generate surfaces + priors; assert repeated `placeSurface` calls are field-for-field equal;
      assert `region` ∈ the 7 regions; assert pin preservation; assert `narration` → `narration`
    - **Property 3: Placement determinism**
    - **Validates: Requirements 2.4, 2.5, 2.6, 2.7, 3.3**
  - [x] 4.3 Extend `applySurfaceEvent` reducer for hydration, buffering, and idempotence
    - Reuse the existing `apps/client/lib/construct/store.ts` reducer semantics (Stream
      append+dedup-by-seq, Timeline upsert-by-step-id, others replace); add: clone-on-write
      (never mutate the envelope in place), orphan-patch buffering (buffer a patch whose base id is
      absent rather than dropping), monotonic `lastSeq`, and surface→`PlacedSurface` wrapping via
      `placeSurface`
    - _Requirements: 1.4, 1.5, 1.6, 6.1, 16.4_
  - [x] 4.4 Write `fast-check` property test for reducer idempotence
    - Generate frame + workspace; assert `apply(apply(ws,f),f) ≡ apply(ws,f)` (dedup by seq)
    - **Property 2: Reducer idempotence on replay**
    - **Validates: Requirements 1.4**
  - [x] 4.5 Write `fast-check` property test for envelope immutability
    - Assert the stored `Surface` envelope deep-equals the schema-validated input after any frame
      is applied (wrap, never mutate)
    - **Property 4: Renderer reuse / envelope immutability**
    - **Validates: Requirements 6.1**

- [x] 5. Surface_Feed hydrate path (TypeScript, EXTENDS existing feed)
  - [x] 5.1 Implement `SurfaceFeed.hydrate(conversationId)` — durable replay then live subscribe
    - `hydrate` GETs `/construct/state`, replays frames oldest-first through the SAME
      `applySurfaceEvent` reducer the live feed uses, then subscribes SSE `since_seq = lastSeq`;
      `applyEvent` folds live frames; on endpoint failure, open empty + subscribe live (degraded)
    - _Requirements: 1.2, 1.3, 8.2, 8.3_
  - [x] 5.2 Write `fast-check` property test for rehydration fidelity
    - Generate arbitrary valid frame sequences `F`; assert `hydrate(replay(F)) ≡ liveApply(F)`
    - **Property 1: Rehydration fidelity ("never vanishing")**
    - **Validates: Requirements 1.3, 1.5**
  - [x] 5.3 Write unit test for disconnect catch-up
    - Subscribe `since lastSeq`; assert frames emitted while disconnected (a job that kept running)
      are applied and present after rehydration
    - _Requirements: 8.2, 8.3_

- [x] 6. Frame inversion + one shell adapter + one level of descent (MVP)
  - [x] 6.1 Mount the Shell at the client route root with a `NarrationPanel` region (frame inversion)
    - Make the top-level route render the Shell (one adapter — desktop OR mobile, whichever proves
      the feel fastest) as the root; render chat as exactly one `narration` region panel within it,
      never as the containing page; environment stays root even with no Narration surface present
    - _Requirements: 3.1, 3.2, 3.4, 3.5, 17.4, 17.5_
  - [x] 6.2 Render a live Timeline in the `activity` region via the reused renderer
    - Place the live `Timeline` of Neo's activity in the `activity` region; render it through the
      existing `TimelineView` renderer untouched; a patch updates that surface in place (no full
      re-layout)
    - _Requirements: 6.2, 16.4, 17.2_
  - [x] 6.3 Implement one level of descent: Timeline step → linked `Stream` (raw)
    - Implement `descend` to push a raw focus frame that resolves the tapped Timeline step's
      `ref`/`parent` link to its `Stream`; if the linked surface is cold, rehydrate it by address
      first; ascend pops the focus frame; render at depth via the reused renderer
    - _Requirements: 4.2, 4.3, 4.4, 4.5, 4.6, 17.3_
  - [x] 6.4 Write unit test for descend resolving `ref` to the raw Stream
    - Tap a Timeline step → assert a raw focus frame targets the linked Stream; cold link triggers
      rehydration before push; ascend pops
    - _Requirements: 4.3, 4.4, 4.5, 17.3_
  - [x] 6.5 Apply UI house rules and non-jargon copy to the shell chrome
    - Separation by background-color contrast only (no border strokes); no emojis/purple/glow;
      `#004CED` single accent; Inter + JetBrains Mono; all shell status/error copy is
      consumer-readable with no MCL/cortex/Merkle/replay/SSE jargon
    - _Requirements: 12.1, 12.2, 13.1, 13.2, 13.3, 13.4_

- [x] 7. Ask back-channel liveness (MVP — required end-to-end check)
  - [x] 7.1 Wire/verify `onRespond` end-to-end: client → daemon POST → parked agent resume
    - Confirm and complete the Phase-5 wiring so a posted Ask response reaches the parked agent;
      `respondToAsk` POSTs `{conversation_id,intent_id,ask_id,response}`; the answer is validated by
      `backchannel.ValidateResponse` and delivered as a recorded INPUT (replay-safe), never a
      plan/walk mutation; if wiring is incomplete in the target client, fixing it is part of this task
    - _Requirements: 5.3, 5.4, 5.8, 11.3, 17.6_
  - [x] 7.2 Write integration test for the Ask round-trip
    - Park on an Ask; answer once → assert the run resumes exactly once and the Ask surface is
      patched to settled; a duplicate/expired/malformed `ask_id` is rejected and leaves the run parked
    - **Property 6: Ask liveness + safety**
    - **Validates: Requirements 5.4, 5.5, 5.6, 17.6**

- [x] 8. MVP integration, wiring, and side-channel verification
  - [x] 8.1 Wire feed → shared model → shell on the one adapter (cold open rehydrates)
    - Connect `SurfaceFeed.hydrate` → `SurfaceWorkspace` → the single shell adapter so a cold open
      rehydrates the workspace and the home/activity surfaces survive reload; assign each surface a
      `construct://{conversationId}/{surfaceId}` address on add; no orphaned/disconnected code
    - _Requirements: 1.2, 7.1, 7.5, 17.1, 17.4, 17.5_
  - [x] 8.2 Write the side-channel non-perturbation integration test (D11)
    - Run with the shell + `surfacestore` active (including answering an Ask) and assert the run's
      set of replayable inputs is byte-identical to the same run without the shell
    - **Property 5: Side-channel non-perturbation (D11)**
    - **Validates: Requirements 11.1, 11.2, 11.3**
  - [x] 8.3 Write `fast-check` property test for MVP addressing re-enterability
    - For a generated emitted-surface set, assert every `construct://C/{id}` address resolves after
      rehydration (hot set or by rehydration)
    - **Property 7: Addressing re-enterability**
    - **Validates: Requirements 7.1, 7.4, 7.5**

- [x] 9. Checkpoint — MVP first slice complete and verifiable
  - Ensure all tests pass, ask the user if questions arise.

## Expansion (post-MVP, in dependency order E1–E6)

- [ ] 10. E1 — Full placement model + spatial desktop adapter + mobile app-grid adapter
  - [ ] 10.1 Implement the full placement model across all 7 regions in the shared core
    - Route window/home/drawer/tray/activity/inbox/narration per the policy; expose layout-relevant
      fields (`zOrder`, `gridSlot`) without adapter-specific logic in the core
    - _Requirements: 2.4, 2.7_
  - [ ] 10.2 Build the desktop windowed/spatial adapter
    - `layout` projects the shared model into windows by `zOrder`, a dock of pinned apps, and a tray;
      renders each surface through the existing per-primitive renderer untouched
    - _Requirements: 2.2, 2.3, 6.2_
  - [ ] 10.3 Build the mobile app-grid/home-screen adapter
    - `layout` projects the shared model into a home grid by `gridSlot` with full-screen focus and a
      bottom tray; renders through the existing mobile renderers untouched
    - _Requirements: 2.2, 2.3, 6.2_
  - [ ] 10.4 Write unit test asserting both adapters render an identical surface-id set
    - From one workspace at one `lastSeq`, assert desktop and mobile produce the same set of surface
      ids and identical envelopes, differing only in geometry/chrome
    - **Property 8: Shared-core / two-shell equivalence**
    - **Validates: Requirements 2.2, 2.3**

- [ ] 11. E2 — Pins, lifecycle, and addressing re-entry of older surfaces
  - [ ] 11.1 Persist placement: add `PlacementFrame` to the store/reducer (+ codegen)
    - Persist `PlacementFrame{SurfaceID,Region,Pinned,Lifecycle,ZOrder,GridSlot}` as a
      `construct.placement` frame in the same JSONL; reducer applies it after its target surface
      (buffer if target absent); regenerate codegen types; validate `region`/`lifecycle` enums
    - _Requirements: 2.6, 15.1_
  - [ ] 11.2 Implement lifecycle transitions (live → settled → archived)
    - Mark surfaces `live` while patching, `settled` when the run ends, `archived` when aged out of
      the hot set but still re-enterable
    - _Requirements: 7.4_
  - [ ] 11.3 Implement addressing resolution with rehydrate-on-open for cold surfaces
    - Opening an address in the hot set resolves directly; opening a cold address rehydrates that
      surface and its `ref`-linked children from the store; all addresses scoped by `conversationId`
    - _Requirements: 7.2, 7.3, 7.5_
  - [ ] 11.4 Write `fast-check` property test for addressing re-enterability across rehydration
    - For every emitted surface (including archived), assert its address resolves after a cold open
    - **Property 7: Addressing re-enterability**
    - **Validates: Requirements 7.3, 7.4**
  - [ ] 11.5 Write unit test for pin preservation across re-projection
    - A pinned surface stays pinned and survives cleanup across re-projection and rehydration
    - _Requirements: 2.6_

- [ ] 12. E3 — Async, environment-level Ask inbox + cross-device answer + expiry
  - [ ] 12.1 Implement the Ask_Inbox region and non-blocking environment notification
    - An emitted `Ask` enters `region: inbox` and raises a non-blocking tray badge/notification
      instead of blocking a column
    - _Requirements: 5.1_
  - [ ] 12.2 Implement cross-device answer and expiry surfacing
    - A returning user (or another device) sees the still-pending Ask and can answer; an unanswered
      Ask past expiry flips the inbox entry to expired with a non-jargon message (no silent hang)
    - _Requirements: 5.2, 5.7_
  - [ ] 12.3 Implement `respondToAsk` settle: validate, patch to answered, remove from inbox
    - On a validated answer, patch the Ask surface to answered/settled and remove it from the inbox;
      reconcile optimistic state from the authoritative answered patch
    - _Requirements: 5.3, 5.6_
  - [ ] 12.4 Write integration test for cross-device single-resume + device-switch race
    - Park on client A, answer on client B → assert single resume and a settled patch on both; two
      shells answering the same `ask_id` → exactly one accepted, the other rejected
    - **Property 6: Ask liveness + safety**
    - **Validates: Requirements 5.2, 5.4, 5.5**

- [ ] 13. E4 — Full depth navigation (all kinds) + Metric system-tray
  - [ ] 13.1 Implement full glance → summary → raw depth for all surface kinds
    - Generalize the focus stack so every kind descends glance → summary → raw via `ref`/`parent`;
      ascend pops; the capability to descend is available for every surface at all times
    - _Requirements: 4.1, 4.2, 10.1, 10.2, 10.3_
  - [ ] 13.2 Implement the Metric system-tray region
    - Place `Metric` surfaces (cost / PAX spend / progress) in the persistent `tray` status area via
      the reused Metric renderer
    - _Requirements: 2.4_
  - [ ] 13.3 Write unit test for focus push/pop ordering and always-available descent
    - Assert level order glance → summary → raw, correct pop on ascend, and descent available for
      every kind
    - _Requirements: 4.1, 4.2, 4.5, 10.3_

- [ ] 14. E5 — Projection coverage audit + close gaps (Go, server-side)
  - [ ] 14.1 Audit `construct/projection` against the frozen `[coverage]` map
    - Compare every distinct agent action (tool result, chain tx, browser, memory, plan/swarm,
      async, cost, human-ask) against the frozen coverage map; record gaps where an action has no projector
    - _Requirements: 9.1, 9.2_
  - [ ] 14.2 Implement projectors to close identified coverage gaps
    - Add the missing projectors so every agent action maps to ≥1 surface; the shell consumes
      whatever is emitted without renderer changes
    - _Requirements: 9.1, 9.3_
  - [ ] 14.3 Write unit test asserting each coverage-map entry projects at least one surface
    - Table-driven over the frozen coverage map; assert no action yields an empty projection
    - _Requirements: 9.1, 9.2_

- [ ] 15. E6 — Second shell adapter parity + device-switch reconciliation
  - [ ] 15.1 Bring the second adapter to full parity over the shared core
    - Whichever adapter the MVP deferred is completed against the shared model with no separate
      surface-state copy
    - _Requirements: 2.1, 2.2, 2.3_
  - [ ] 15.2 Implement device-switch reconciliation by authoritative patch
    - On two clients over one conversation, reconcile the losing client's optimistic state from the
      authoritative answered/settled patch so both settle identically
    - _Requirements: 8.2, 8.3_
  - [ ] 15.3 Write `fast-check` property test for shared-core / two-shell equivalence
    - For any workspace, assert both adapters render the same surface set through the same renderers,
      differing only in geometry/chrome
    - **Property 8: Shared-core / two-shell equivalence**
    - **Validates: Requirements 2.1, 2.2, 2.3**
  - [ ] 15.4 Write integration test for device-switch reconciliation
    - Drive divergent optimistic state on two clients; assert both converge on the authoritative patch
    - _Requirements: 8.2, 8.3_

- [ ] 16. Final checkpoint — full shell complete
  - Ensure all tests pass, ask the user if questions arise.

## Notes

- Tasks marked with `*` are optional test sub-tasks and can be skipped for a faster MVP; core
  implementation and wiring tasks are never optional.
- The MVP slice (tasks 1–9) is independently shippable: it delivers persistence + rehydration, a
  live activity Timeline, one level of descent, frame inversion on one adapter, the shared model,
  and a verified-live Ask back-channel — then proves the side-channel (D11) invariant holds.
- Reuse-vs-new is explicit: tasks 1.1 and 4.3 generalize/extend existing code (`neo/internal/trace`,
  `store.ts`); the 8 renderers and the `construct/{schema,transport,projection,backchannel}` packages
  are reused untouched; only `surfacestore`, the read route, the shared model, the feed hydrate path,
  and the shell adapters are new.
- Each task references the requirement clauses (R1–R17) and/or the design correctness property it
  implements; property tests are placed close to the code they validate to catch errors early.
- No deployment tasks: production redeploy is Andrew-gated and out of scope; no `git commit`/`push`
  on the dev box.

## Task Dependency Graph

```json
{
  "waves": [
    { "id": 0,  "tasks": ["1.1", "4.1"] },
    { "id": 1,  "tasks": ["1.2", "1.3", "2.1", "4.2", "4.3"] },
    { "id": 2,  "tasks": ["2.2", "3.1", "4.4", "4.5"] },
    { "id": 3,  "tasks": ["2.3", "3.2", "5.1"] },
    { "id": 4,  "tasks": ["5.2", "5.3", "6.1"] },
    { "id": 5,  "tasks": ["6.2", "6.3", "6.5", "7.1"] },
    { "id": 6,  "tasks": ["6.4", "7.2", "8.1"] },
    { "id": 7,  "tasks": ["8.2", "8.3"] },
    { "id": 8,  "tasks": ["10.2", "10.3", "14.1"] },
    { "id": 9,  "tasks": ["10.1", "14.2"] },
    { "id": 10, "tasks": ["10.4", "14.3", "11.1"] },
    { "id": 11, "tasks": ["11.2", "11.3"] },
    { "id": 12, "tasks": ["11.4", "11.5", "12.1"] },
    { "id": 13, "tasks": ["12.2", "12.3"] },
    { "id": 14, "tasks": ["13.1", "13.2"] },
    { "id": 15, "tasks": ["12.4", "13.3", "15.1"] },
    { "id": 16, "tasks": ["15.2"] },
    { "id": 17, "tasks": ["15.3", "15.4"] }
  ]
}
```

