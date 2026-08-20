# Requirements Document

## Introduction

The Construct OS Shell elevates Matrix's Construct surface system from a flat, ephemeral,
chat-adjacent list (a "sideshow" rendered beside a chat thread) into the centerpiece of an OS-like
"cloud computer that Neo is a resident in." Today projected surfaces stream into the client as an
ordered column that vanishes when the live replay buffer drops and is anchored to a single in-flight
run. This feature makes the Construct the durable, spatial environment a user lives in.

The organizing thesis is **calibrated transparency**: give the user just enough visibility to feel
aware and in control without being overwhelmed. The *capability to intervene* — not constant
intervention — is what builds trust, and the shell is the structure that makes that capability always
present.

The work is mostly additive. It reuses the 8 frozen per-primitive renderers on both web and mobile
clients untouched, and adds: a shared persistent surface-state model; a server-side surface store
(generalizing the F3 durable trace) for "never vanishing" rehydration across reload, suspend, and
device-switch; frame inversion (environment primary, chat one panel); shell-level depth navigation
(glance → summary → raw); and an asynchronous, environment-level Ask trust back-channel.

These requirements are derived from the approved design document and trace back to it, including the
design's 9 correctness properties, its hard constraints, and the agreed MVP first-slice scope.

## Glossary

- **Construct**: The frozen agent-to-human surface-projection primitive — the layer that renders raw
  agent-world-state into a finite, trusted, bidirectional set of surfaces a browser-bound human can
  read and act on.
- **Surface**: A single instance of the frozen `Surface` envelope (one of the 8 primitive kinds with
  an id, optional `ref`/`parent`, `attributes`, and exactly one payload). The shell never alters this
  envelope.
- **Primitive**: One of the 8 frozen Construct surface kinds: `Narration`, `Metric`, `Entity`,
  `Structure`, `Stream`, `Timeline`, `Canvas`, `Ask`.
- **Projection**: The server-side mapping of arbitrary agent world-state onto a composition of the 8
  primitives, performed by `packages/construct/projection`.
- **Projection coverage**: The property that every distinct agent action maps to at least one
  projected surface, so the environment is never hollow.
- **Surface_State_Model** (a.k.a. `SurfaceWorkspace`, surface-state model): The shared client-side
  core state both shell adapters read — surfaces wrapped with placement/lifecycle, plus focus, pins,
  Ask inbox, and Metric tray indexes.
- **Placement**: Shell-level metadata wrapping a surface, describing the region it occupies and its
  lifecycle (`region`, `pinned`, `lifecycle`, `zOrder`, `gridSlot`).
- **Placement_Policy**: The deterministic pure function (`placeSurface`) that derives a `Placement`
  from a surface's `kind` + `attributes` + prior placement.
- **Region / placement region**: An OS zone a surface occupies — one of `home`, `window`, `drawer`,
  `activity`, `tray`, `inbox`, `narration`.
- **Shell**: The client-side environment that arranges surfaces as durable, addressable, re-enterable
  apps/windows/panels and mounts at the root of the client route.
- **Shell_Adapter**: A presentation layer over the shared Surface_State_Model. Two exist — the Mobile
  PWA adapter (home-screen/app-grid) and the Desktop Web adapter (windowed/spatial) — sharing one
  model and differing only in layout geometry and chrome.
- **Surface_Store**: The server-side, per-user, append-only durable persistence of the
  `construct.surface[.patch]` frame stream, generalizing the F3 `agents/neo/internal/trace` per-run JSONL
  trace and keyed by conversation.
- **Surface_Feed**: The client consumer that applies both live SSE frames and rehydration frames
  through one reducer (`applySurfaceEvent`).
- **Rehydration / rehydrate**: Reconstructing the Surface_State_Model on a cold open by replaying the
  durable surface frames through the same reducer the live feed uses.
- **Rehydration_Endpoint**: The thin read-only daemon route `GET /construct/state` that returns a
  conversation's ordered durable frames.
- **Frame inversion**: Making the environment the page and `Narration` (chat) one panel/region within
  it, inverting today's "chat is the page, work is a side panel" layout.
- **Depth navigation**: Shell-level descent through three focus levels — glance → summary → raw — via
  the focus stack and the envelope's `ref`/`parent` links.
- **Ask_Inbox**: The environment-level collection of pending `Ask` surfaces (`region: inbox`) that a
  user can answer asynchronously, including after returning or from another device.
- **Ask back-channel**: The typed request-for-human round-trip (`choose | input | confirm | sign |
  upload`); an answer is validated server-side and delivered to the parked agent as an INPUT.
- **Side-channel / D11**: The load-bearing invariant that the projection and persistence layers are a
  pure observability/VIEW layer and MUST NOT perturb the D11 replay byte-identity of the agent's
  execution; the only agent feedback is a validated Ask answer entering as a recorded INPUT.
- **Calibrated transparency**: The UX principle of progressive disclosure — surfacing just enough
  visibility (glance) with the capability to descend to detail (summary/raw) on demand.
- **"Never vanishing"**: The guarantee that any surface the agent ever emitted in a conversation
  remains durable, re-enterable, and rehydratable across reload, suspend, redeploy, and device switch.

## Requirements

### Requirement 1: Persistent surface-state and rehydration fidelity ("never vanishing")

**User Story:** As a consumer, I want my computer to reappear exactly as I left it after a reload or
returning later, so that my work and Neo's activity never vanish.

#### Acceptance Criteria

1. WHEN a `construct.surface` or `construct.surface.patch` frame is published on the broker for a conversation, THE Surface_Store SHALL durably append that frame to that conversation's persisted frame log in ascending published `seq` order, persisted across reload, suspend, redeploy, and device switch.
2. WHEN a client cold-opens a conversation, THE Surface_Feed SHALL rehydrate the Surface_State_Model by replaying the conversation's durable frames in ascending `seq` order through the same reducer (`applySurfaceEvent`) the live feed uses.
3. WHERE a conversation has a recorded frame sequence, THE Surface_State_Model produced by rehydration SHALL be observationally equal to the model produced by applying that same frame sequence live — identical surface id set, identical `Surface` envelope payload per id, identical `Placement` per id, and identical `lastSeq`.
4. WHEN the same frame is applied to the Surface_State_Model more than once, THE Surface_Feed SHALL dedup by `seq` so the resulting model equals applying that frame exactly once.
5. WHEN frames are applied during rehydration, THE Surface_State_Model SHALL set `lastSeq` to the highest applied frame `seq` and SHALL keep `lastSeq` monotonically non-decreasing.
6. WHEN a `construct.surface.patch` frame targets a surface id not yet present in the Surface_State_Model, THE Surface_Feed SHALL buffer the patch and apply buffered patches in `seq` order when the base surface arrives, rather than dropping them.
7. IF a buffered patch's base surface never arrives during rehydration, THEN THE Surface_Feed SHALL retain the buffered patch for a later live frame and SHALL NOT misapply or silently discard it.

### Requirement 2: Shared surface-state model with two shell adapters

**User Story:** As a consumer, I want the same computer on my phone and my desktop, so that the
environment behaves consistently while fitting each device.

#### Acceptance Criteria

1. THE Surface_State_Model SHALL be the single shared source of client-side shell state that both the Mobile Shell_Adapter and the Desktop Shell_Adapter read, and neither adapter SHALL maintain a separate copy of surface state.
2. WHEN the Mobile Shell_Adapter and the Desktop Shell_Adapter lay out the same Surface_State_Model at the same `lastSeq`, THE Shell SHALL render an identical set of surface ids in both adapters, and SHALL render each surface through the one per-primitive renderer matching that surface's `kind`.
3. WHERE the Mobile Shell_Adapter and the Desktop Shell_Adapter differ in presentation, THE Shell SHALL limit the difference to placement geometry and chrome (`zOrder`, `gridSlot`, and chrome) only, and the set of surface ids shown and each shown surface's `Surface` envelope (`attributes` and payload) SHALL be identical across both adapters.
4. WHEN a surface is added to the Surface_State_Model, THE Placement_Policy SHALL assign a `region` that is a pure function of the surface `kind` and `attributes`, and that `region` SHALL be exactly one of `home`, `window`, `drawer`, `activity`, `tray`, `inbox`, or `narration`.
5. WHEN the Placement_Policy is applied to the same surface `kind`, the same `attributes`, and the same prior placement, THE Placement_Policy SHALL return a `Placement` equal in every field (`region`, `pinned`, `lifecycle`, `zOrder`, and `gridSlot`).
6. WHERE a prior placement marked a surface as `pinned`, THE Placement_Policy SHALL preserve the `pinned` state as true on the re-projected `Placement` for that surface.
7. IF the Placement_Policy is applied to a surface whose `kind` is not one of the 8 frozen primitives, THEN THE Placement_Policy SHALL assign a deterministic default `region` and SHALL NOT throw or leave the surface without a `Placement`.

### Requirement 3: Frame inversion (environment primary, chat a panel)

**User Story:** As a consumer, I want the environment to be the page with chat as one panel within
it, so that Neo's work is the centerpiece rather than a guest beside a chat thread.

#### Acceptance Criteria

1. WHEN the top-level client route mounts, THE Shell SHALL render the environment as the root of the client route.
2. WHEN the top-level client route mounts, THE Shell SHALL render the `Narration` chat as exactly one panel occupying the `narration` region and SHALL NOT render the `Narration` chat as the page that contains the environment.
3. WHERE a surface has `kind` `narration`, THE Placement_Policy SHALL assign it the `narration` region regardless of the surface's `attributes` or prior placement.
4. THE Shell SHALL treat the `narration` region as exactly one of the seven placement regions (`home`, `window`, `drawer`, `activity`, `tray`, `inbox`, `narration`) and SHALL NOT render any region as a page that contains the other six regions.
5. WHILE no `Narration` surface is present in the Surface_State_Model, THE Shell SHALL continue to render the environment as the root and SHALL NOT fall back to rendering chat as the containing page.

### Requirement 4: Depth navigation (glance → summary → raw)

**User Story:** As a consumer, I want to descend from a glance into the detail and then the raw truth
of any surface, so that I can see exactly what Neo did when I choose to look.

#### Acceptance Criteria

1. THE Shell SHALL represent surface depth as a shell-level focus stack with exactly three ordered levels: glance (base), summary, then raw.
2. WHEN a user descends from a focused surface that is not already at raw, THE Shell SHALL push exactly one focus frame advancing the level by one step in the order glance → summary → raw.
3. WHEN a user descends to the raw level of a surface that links to another surface, THE Shell SHALL resolve the link by `ref` first, else by `parent`, and push a raw focus frame targeting the linked surface.
4. IF a descend targets a linked surface not present in the Surface_State_Model, THEN THE Shell SHALL rehydrate that surface by its address before pushing the raw focus frame.
5. IF rehydration of a descend target does not complete within 5 seconds or fails, THEN THE Shell SHALL leave the focus stack unchanged and SHALL indicate the failure in non-jargon language.
6. WHEN a user descends at the raw level of a surface that has no `ref` or `parent` link, THE Shell SHALL treat the action as a no-op and leave the focus stack unchanged.
7. WHEN a user ascends, THE Shell SHALL pop exactly the top focus frame from the focus stack.
8. WHEN a user ascends while the focus stack is at the base glance frame, THE Shell SHALL treat the action as a no-op.
9. WHEN the Shell renders any focus level, THE Shell SHALL use the existing per-primitive renderer for the surface without modifying it.

### Requirement 5: Asynchronous, environment-level Ask with verified liveness

**User Story:** As a consumer, I want to answer Neo's requests later or from another device, so that I
am never forced to block on a prompt and never lose a pending request.

#### Acceptance Criteria

1. WHEN an `Ask` surface is emitted, THE Shell SHALL place it in the Ask_Inbox (`region: inbox`) and raise an environment-level notification without blocking, suspending, or occupying any other surface region or column.
2. WHILE an `Ask` is pending and unexpired, THE Shell SHALL allow a user to answer it, including after returning later or from another device.
3. WHEN a user answers a pending `Ask`, THE Shell SHALL POST the typed response through the existing Ask back-channel endpoint for server-side validation.
4. WHEN an answer passes `backchannel.ValidateResponse`, THE Ask back-channel SHALL resume the parked run exactly once and deliver the answer to the agent as a recorded INPUT.
5. IF a posted `ask_id` is duplicate, expired, or malformed, THEN THE Ask back-channel SHALL reject the answer, SHALL leave the run parked, SHALL leave the Ask_Inbox entry unchanged, and SHALL return a rejection indication to the submitting client.
6. WHEN an `Ask` is answered, THE Shell SHALL patch the `Ask` surface to its answered/settled state and remove it from the Ask_Inbox.
7. WHEN the same `Ask` is answered from more than one device or session, THE Ask back-channel SHALL accept exactly one response per `ask_id` and THE Shell SHALL reconcile every session to the authoritative answered patch so all sessions settle identically.
8. IF an `Ask` is unanswered past the expiry carried by its `Ask` surface, THEN THE Shell SHALL mark the Ask_Inbox entry as expired with a non-jargon message and SHALL NOT leave the entry in a pending state.
9. THE Shell SHALL verify the Ask back-channel round-trip end-to-end (client → daemon → parked agent resumes exactly once and receives the answer as a recorded INPUT) before the feature is considered complete.

### Requirement 6: Reuse of frozen renderers and trusted-render safety

**User Story:** As a system integrator, I want the shell to arrange surfaces without ever synthesizing
UI, so that the agent's expressiveness stays paired with the safety of fixed renderers.

#### Acceptance Criteria

1. WHEN a surface is stored in the Surface_State_Model, THE Shell SHALL wrap the `Surface` envelope without mutating the original, and the stored envelope SHALL remain deep-equal (every field and nested value identical) to the schema-validated input from which it was created.
2. WHEN the Shell renders a surface whose `kind` is one of the 8 frozen primitives (`Narration`, `Metric`, `Entity`, `Structure`, `Stream`, `Timeline`, `Canvas`, `Ask`), THE Shell SHALL render it using the single existing frozen per-primitive renderer for that `kind` on both the web client and the mobile client, without modifying that renderer.
3. IF a surface has a `kind` that is not one of the 8 frozen primitives, or a payload that does not match its declared `kind`, THEN THE Shell SHALL render nothing for that surface, SHALL NOT execute or interpret any agent-provided content as markup, and SHALL continue rendering all other valid surfaces unaffected.
4. THE Shell SHALL arrange and place surfaces only, SHALL render all surface content exclusively through the 8 frozen per-primitive renderers, and SHALL NOT generate, synthesize, template, or execute any UI derived from agent-provided content.
5. IF a frozen per-primitive renderer fails while rendering a valid surface, THEN THE Shell SHALL contain the failure to that surface, SHALL render nothing for that surface, and SHALL continue rendering all other surfaces.

### Requirement 7: Surface addressing and re-enterability

**User Story:** As a consumer, I want to tap a surface I saw earlier and re-enter it, so that nothing
I have seen becomes unreachable.

#### Acceptance Criteria

1. WHEN a surface is added to the Surface_State_Model, THE Shell SHALL assign it a stable address of the form `construct://{conversationId}/{surfaceId}` that remains identical across reload, rehydration, and device switch.
2. WHEN a user opens the address of a surface present in the hot set (surfaces currently in the Surface_State_Model), THE Shell SHALL resolve and present that surface without reading the Surface_Store.
3. WHEN a user opens the address of a surface not in the hot set, THE Shell SHALL rehydrate that surface and its `ref`-linked children from the Surface_Store for the address's `conversationId` and present it.
4. THE Shell SHALL ensure that for every surface the agent ever emitted in a conversation, the surface's `construct://{conversationId}/{surfaceId}` address resolves.
5. THE Shell SHALL resolve every surface address only against the surfaces of the conversation named in the address.
6. IF an opened address names a `surfaceId` that was never emitted (absent from both the hot set and the Surface_Store), THEN THE Shell SHALL present a non-jargon "unavailable" indication and SHALL leave the Surface_State_Model unchanged.
7. IF an opened address names a `conversationId` the current user does not own, THEN THE Shell SHALL refuse resolution and SHALL NOT return any surface.

### Requirement 8: Long-running task visibility while disconnected

**User Story:** As a consumer, I want to see what Neo did while I was away, so that long-running work
stays visible when I reconnect.

#### Acceptance Criteria

1. WHILE no client is connected to a conversation, THE Surface_Store SHALL durably append every `construct.surface` and `construct.surface.patch` frame emitted by ongoing tasks for that conversation to that conversation's persisted frame log, regardless of how long the disconnection lasts.
2. WHEN a user reconnects to a conversation, THE Surface_Feed SHALL subscribe to the live frame stream resuming from the Surface_State_Model's current `lastSeq`.
3. WHEN the Surface_Feed resumes the live stream after a reconnect, THE Surface_Feed SHALL apply each catch-up frame whose `seq` is greater than the Surface_State_Model's `lastSeq` oldest-first, and SHALL NOT alter the model for any frame whose `seq` is less than or equal to the current `lastSeq`.
4. IF one or more frames emitted while the user was disconnected are no longer available on the live stream, THEN THE Surface_Feed SHALL rehydrate the missing frames from the Surface_Store oldest-first before applying further live frames, so that no frame emitted while disconnected is dropped.
5. WHEN reconnect catch-up completes, THE Surface_State_Model SHALL equal the model produced by applying that conversation's full durable frame sequence through the same reducer the live feed uses, including every surface produced by tasks that ran while the user was disconnected.

### Requirement 9: Projection coverage (the environment is never hollow)

**User Story:** As a system integrator, I want every agent action to project into a surface, so that
no activity is invisible and the environment never reads as broken.

#### Acceptance Criteria

1. THE Projection layer SHALL map each agent action class defined in the frozen coverage map onto a composition of one or more of the 8 frozen primitives (`Narration`, `Metric`, `Entity`, `Structure`, `Stream`, `Timeline`, `Canvas`, `Ask`), such that every projected action yields at least one `Surface`.
2. THE Projection layer SHALL apply the frozen coverage map mappings exactly: a tool result onto `Entity`, `Structure`, or `Metric`; a chain transaction onto an irreversible `Entity` plus a sign `Ask`; a browser action onto `Canvas`, `Stream`, and `Timeline`; a memory action onto `Structure` and `Entity`; a plan or swarm action onto `Timeline` and `Structure`; an async action onto `Timeline` and `Metric`; and a cost action onto `Metric`.
3. IF an agent action has no mapped projector in the frozen coverage map, THEN THE Projection layer SHALL emit at least one `Narration` surface representing that action and SHALL NOT drop the action without emitting any surface.
4. WHEN the Projection layer emits a surface, THE Shell SHALL place that surface through the Placement_Policy into exactly one placement region and SHALL render it through the existing per-primitive renderer matching the surface's `kind` without requiring renderer changes.

### Requirement 10: Calibrated transparency / progressive disclosure

**User Story:** As a consumer, I want just enough visibility to feel aware and in control without
being overwhelmed, so that I trust the system and can intervene when I choose.

#### Acceptance Criteria

1. WHEN the Shell first presents a surface, THE Shell SHALL present it at the glance level in its placement-region chrome, and SHALL NOT show its summary or raw level by default.
2. WHEN a user requests more detail on a surface, THE Shell SHALL disclose exactly one deeper level per request in the order glance → summary → raw.
3. WHILE a surface is present in the Surface_State_Model, THE Shell SHALL keep the capability to descend to its detail available.
4. IF a user requests more detail on a surface already at the raw level, THEN THE Shell SHALL treat the request as a no-op and SHALL NOT advance past raw.

### Requirement 11: Side-channel non-perturbation (D11)

**User Story:** As a system integrator, I want persistence and the shell to never change the agent's
execution, so that the D11 replay byte-identity invariant is preserved.

#### Acceptance Criteria

1. WHEN the Surface_Store persists or rehydrates surfaces, THE Surface_Store SHALL leave cortex, plan state, and walk state byte-identical, and SHALL NOT sign anything.
2. WHEN a run executes with the Surface_Store and Shell active, THE system SHALL produce a recorded replayable-input sequence byte-identical to the same run executed with the Surface_Store and Shell inactive.
3. WHEN an `Ask` is answered, THE system SHALL deliver the answer through the same recorded-input path as a user message, with no extra replay-altering metadata, and SHALL NOT introduce any other agent feedback path.
4. IF persistence or rehydration fails, THEN THE system SHALL leave the run's recorded input sequence and execution unperturbed.
5. WHEN a run is replayed with the Surface_Store and Shell active, THE system SHALL preserve the D11 replay byte-identity invariant.

### Requirement 12: Consumer product shows the result, not the tech

**User Story:** As a consumer, I want a computer rather than a debugger, so that I am never shown
protocol internals.

#### Acceptance Criteria

1. THE Shell SHALL NOT display, in any user-visible text, label, control, status, or error of the UI, any term from the protocol-jargon set {MCL, cortex, Merkle, replay, SSE, D11, broker, frame, patch, projection, envelope, rehydration, back-channel}.
2. WHEN the Shell shows a status to the user, THE Shell SHALL phrase the status in terms of the user's deliverable or outcome and SHALL include no term from the protocol-jargon set.
3. IF the Shell shows an error to the user, THEN THE Shell SHALL present a message that states what failed in outcome terms, includes no term from the protocol-jargon set, and SHALL NOT display internal error codes or stack traces.

### Requirement 13: UI house rules

**User Story:** As a consumer, I want a minimal, dark, high-signal interface, so that the environment
feels calm and focused.

#### Acceptance Criteria

1. THE Shell SHALL separate adjacent surface layers using background-color contrast only.
2. THE Shell SHALL NOT use border strokes to indicate depth or separation between surface layers.
3. THE Shell SHALL NOT render emojis in any UI chrome it controls.
4. THE Shell SHALL NOT use purple gradients in any UI chrome it controls.
5. THE Shell SHALL NOT use glow effects in any UI chrome it controls.
6. THE Shell SHALL use Paxeer Blue `#004CED` as its single accent color and SHALL NOT introduce any other accent color.
7. THE Shell SHALL use the Inter typeface for UI text.
8. THE Shell SHALL use the JetBrains Mono typeface for monospace text.

### Requirement 14: Transport invariant (rides existing chat transport)

**User Story:** As a system integrator, I want surfaces to ride the existing chat transport, so that
no new agent-to-client wire path is introduced.

#### Acceptance Criteria

1. THE Shell SHALL consume surfaces over the existing chat SSE/WS transport.
2. THE Shell SHALL NOT add any agent-to-client wire path beyond the existing chat SSE/WS transport.
3. WHERE persistence and rehydration are added, THE system SHALL add exactly three components: a client-side state model, a server-side persistence sidecar, and a read-only rehydration read path.
4. WHERE persistence and rehydration are added, THE system SHALL NOT add any agent-to-client wire path beyond the existing chat transport.

### Requirement 15: Codegen'd types and per-user isolation

**User Story:** As a system integrator, I want types generated from the Go schema and per-user data
isolation, so that the wire contract stays authoritative and users are isolated.

#### Acceptance Criteria

1. WHERE a Go-side type reaches the client, THE system SHALL generate that type from the Go schema via codegen.
2. THE system SHALL NOT hand-edit any codegen-generated type.
3. THE Surface_Store SHALL root all persisted surface data in the per-user machine's `/data` volume.
4. WHEN a surface address or stored frame is resolved, THE system SHALL scope the resolution to its owning conversation.
5. IF a resolution would access a surface or frame owned by a different user, THEN THE system SHALL refuse the access and SHALL NOT return the surface or frame.

### Requirement 16: Performance — persistence off the hot path, bounded rehydration

**User Story:** As a consumer, I want the environment to stay responsive, so that persistence never
slows Neo down and reopening is fast.

#### Acceptance Criteria

1. WHEN `Store.Record` is called on the broker publish hot path, THE Surface_Store SHALL return without performing disk I/O on the caller goroutine.
2. WHEN `Store.Record` is called, THE Surface_Store SHALL perform the durable write on a separate asynchronous writer.
3. IF the asynchronous writer queue is saturated, THEN THE Surface_Store SHALL drop the incoming frame rather than block the broker publish path.
4. WHEN a client cold-opens a conversation, THE Rehydration_Endpoint SHALL return at most the configured retained-frame cap of newest frames so that cold-open cost is bounded by that cap.
5. WHEN a `construct.surface.patch` is applied, THE Shell SHALL update only the targeted surface in place and SHALL NOT re-lay-out the whole environment.

### Requirement 17: MVP first-slice scope

**User Story:** As a consumer, I want an early version that proves the feeling of a persistent
computer, so that the core value is validated before the full environment is built.

#### Acceptance Criteria

1. THE Surface_Store SHALL persist one conversation's surfaces and the Rehydration_Endpoint SHALL serve them.
2. WHEN a client cold-opens the MVP conversation, THE Shell SHALL rehydrate the workspace so that the home surface survives reload.
3. THE Shell SHALL place a live `Timeline` of Neo's activity in the `activity` region using the existing Timeline renderer.
4. WHEN a user taps a `Timeline` step in the MVP, THE Shell SHALL descend one level to that step's linked `Stream` at the raw level.
5. THE Shell SHALL host chat as a `NarrationPanel` region mounted within the root shell and SHALL NOT host chat as the page that contains the environment.
6. THE MVP SHALL read shell state from the shared Surface_State_Model and SHALL deliver on exactly one Shell_Adapter first.
7. THE MVP SHALL verify that the Ask back-channel is live end-to-end before the MVP is considered complete.

