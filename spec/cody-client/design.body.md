# Design — Cody Client (the /cody surface + engine polish)

## Overview

This feature ships Cody's product surface and the engine addons that make it
sellable: a full sibling app at `/cody` in `apps/client` (its own sidebar, top
bar, viewport, and pages — the claude.ai vs claude.ai/code relationship to the
Neo dashboard), plus the backend polish the brainstorm surfaced — gated
Stack/Design decision phases that structurally kill AI-default output,
first-class spec ingestion, mid-run steering, an answer channel for
`needs_input`, HTTP-exposed undo, sandbox-based dev-server preview behind the
router, and the image/rules/skills additions that make the agent well-informed
on non-changing things.

The engine (spec/cody, P1–P7) is done and proven; this feature builds ON it
and supersedes spec/cody task.7.1 / task.8.6 (the placeholder client tasks)
with a real, brainstormed product plan.

Three load-bearing ideas:

1. **One engine truth, three disclosure tiers.** Modes are people groups —
   Prototype is a vibe coder, Engineer is a working developer, Architect is a
   systems engineer. The UI never changes the data, only how much of it each
   tier discloses: Prototype is preview-first with outcome cards and no
   terminal/tree/diff; Engineer adds the waved task board, diff review,
   verification evidence, file tree, decision cards, named checkpoints;
   Architect adds the terminal, the live spec viewer, property-test results,
   full turn-in detail, and the git surface. Mode is a PROJECT-level setting.

2. **Anti-default by construction, not by prompt.** The path of least
   resistance lives in the model's weights; prompting against it fails. So the
   engine gates it the same way the constitution gates false success: on
   greenfield work, Engineer/Architect cannot start wave 1 until a Stack
   Decision Record survives an anti-default adjudication lens, and no UI task
   ships without conforming to an accepted Design Language Record screened
   against a deterministic banned-AI-tells list. Decision phases run hot with
   N divergent candidates judged; implementation runs cold. Prototype
   deliberately keeps the classic stack — that is what Prototype means.

3. **Preview is a deliverable, not a dev server you babysit.** When work is
   ready to show, Cody snapshots it into a Railway sandbox (PRIVATE network
   mode, same project/environment), starts the app there, and the router
   proxies `/preview/{user}/...` to it under the user's JWT. The user's VM
   stays outbound-quiet (sleep economics intact), previews are isolated from
   the workspace Cody keeps mutating, and nothing is world-readable.

## Locked decisions (user, 2026-07-03)

- **`/cody` is a sibling app surface** — its own route, sidebar, top bar,
  viewport, and pages inside `apps/client`; the Neo dashboard route is
  untouched. Web-first; mobile parity is a follow-up feature.
- **Preview = Railway sandboxes on demand** — spun up when Cody finishes work
  and is ready to show, NOT an always-on dev server in the user VM.
  Sandboxes run PRIVATE; the router adds an authenticated `/preview` proxy
  path (works with zero sandbox-domain support; previews ride the same JWT
  trust model as everything else).
- **Playwright browsers are NOT baked into the user image** — browser-driven
  e2e/screenshot work runs in an on-demand sandbox; the shared browser
  service covers worker screenshots.
- **Quality toolchain IS baked** — golangci-lint, ruff, eslint/prettier, tsc,
  vitest, cargo-clippy: present in the image so the verification runner's
  detected commands actually run.
- **SDR/DLR are the human checkpoint** — cheap to review, expensive to get
  wrong; the approve gate lives there, not on every task. Task-level review
  stays a passive audit surface (diff review after acceptance).
- **Terminal is Architect-only.** Modes are people groups; the tiers above.
- **Anti-default enforcement is engine-structural** — SDR + DLR gated
  artifacts, `rules/stack-selection` decision tables as curated data,
  deterministic banned-defaults screen, hot-decisions/cold-implementation
  creativity split.
- **Vibe-coder failure mode is the target** — Prototype hides the machinery
  entirely; "show me the code" is a single escape hatch, undo is one button.

## Defaulted decision (flagged, cheap to reverse)

- **Projects are `/workspace/<project>` subdirectories** with a per-project
  mode + ledger. Beta testers will build more than one thing; the workspace
  handlers and orchestrator take a project root today (`/workspace`), so a
  project registry + root parameter is a thin addition. If v1 should be
  single-project, delete req.2's registry clauses and pin root = `/workspace`.

## Seam map (grounded in current code)

| Seam | Where | Relevance |
| --- | --- | --- |
| codyd HTTP/SSE | `cody/internal/server/server.go` (routes: `/chat` 202 dispatch, `/events` replay-then-follow, `/conversations/{id}/trace`, `/intents/{id}/stop`), `sse.go` (envelope `{seq,ts,phase,type,fields}` byte-identical to Neo) | The client rides Neo's exact transport pattern; new routes slot into the same mux |
| Cody event family | `cody/internal/server/trace.go:18-27` whitelist: `plan.created`, `task.started/accepted/rejected/failed`, `sheet.authored`, `task.turnin`, `plan.completed` + `chat.assistant`, `message.complete` | Disjoint from Neo's `tool.*` vocabulary — the client needs a NEW reducer, not a NeoTask extension |
| Workspace surface | `cody/internal/server/workspace.go`: `GET /workspace/tree|file|diff`, `POST /workspace/exec` (bounded, guarded) | File tree, viewer, diff pane, terminal already served; gains a project param |
| Snapshots (no route) | `cody/internal/server/checkpoint.go` Snapshotter (`.cody/snapshots`) | Undo exists in Go, unexposed — this feature adds the HTTP + UI |
| Run ledger + resume | `cody/internal/server/engine.go` (`run.json`, statuses `running|completed|failed|stopped|needs_input`, `ResumeOrphanedPlans`) | `needs_input` exists with no answer channel — this feature adds one |
| Orchestrator gate | `cody/internal/orchestrator/orchestrator.go:364-404` (independent verify → constitution screens → adjudication) | SDR/DLR reuse this exact layered-gate shape at the plan level |
| Mode tuples | `cody/internal/mode/mode.go:89-138` (PlanningDepth, VerifyCadence, Creativity, Autonomy, Register, models) | Gains the split creativity policy + decision-phase candidate count |
| Worker ExtraTools | `cody/internal/worker/worker.go:54-57,337-347` (seam wired, unpopulated by `engine.go:465-483`) | Browser/fetch/web-search injection point; screenshot-as-evidence rides it |
| Client transport | `apps/client/lib/api/client.ts`, `lib/api/events.ts`, `lib/realtime/sse.ts` + `sse-hub.ts`, `lib/auth/session.ts` | Reused wholesale — auth, retry, seq-gap resume, replay-then-tail |
| Client reducer pattern | `apps/client/hooks/api/useChat.ts` (`buildTaskFromTrace` :467, live `onUpdate` :914) | The proven durable-trace + live-SSE fold; `useCody` mirrors the shape over the Cody family |
| Route seam | `apps/client/app/[locale]/page.tsx` (single authed route today) | `/[locale]/cody` becomes the second authed route with its own shell |
| Code rendering | `apps/client/components/ai-elements/code-block.tsx` (Shiki; uses border strokes) | Reuse highlighting, restyle to background-contrast separation (house rule) |
| Design doctrine | `rules/web/design-quality.md` (banned patterns + 10 required qualities) | The DLR screen enforces THIS file; `rules/stack-selection/` is the new sibling |
| Router proxy | `router/internal/proxy/proxy.go` (`h.forward`, JWT auth, provider-aware wake) | `/preview/{...}` mounts beside it; same trust model |
| Railway API client | `router/internal/railway/railway.go` (GraphQL, service/volume lifecycle) | The Go-native seam to extend for sandbox create/destroy (SDK is TS-only; Go client preferred, Node sidecar is the fallback if the sandbox API is not on GraphQL) |
| Railway image | `deploy/railway/Dockerfile` + `entrypoint.sh` (builds mcl-execute + neo only) | codyd, local DBs, quality toolchain land here |

## Architecture

### The /cody app shell

`apps/client/app/[locale]/cody/page.tsx` mounts a `CodyApp` with its own
shell: left sidebar (projects list, new project, per-project recent runs,
settings, back-to-Matrix), top bar (project name, mode chip, run status,
stop), and a main viewport whose composition is tier-driven. Pages within the
surface: Workspace (the hero), History (past runs + outcomes), Settings
(project mode, preview TTL, danger zone). All separation by background tone
(`background → card → popover`), single Paxeer Blue accent, no borders, no
emojis. Consumer copy per register: Prototype speaks outcomes, never
"orchestrator/worker/sheet".

Tier composition of Workspace:

- **Prototype**: preview viewport as hero + chat rail + outcome cards
  (what-changed prose, design-direction card in plain language) + one-button
  undo + "show me the code" escape hatch (opens read-only viewer).
- **Engineer** (default): chat rail left, waved task board center (tasks
  flipping pending → started → accepted/rejected live, turn-in cards with
  changes/verification evidence/gaps), right tab strip: Files / Diff /
  Preview / Checkpoints; SDR/DLR decision cards inline in the board.
- **Architect**: Engineer plus Terminal tab (bounded `/workspace/exec`, v1 is
  command-run not PTY), Spec tab (`.cody/spec/requirements.md` + `tasks.md`
  rendered live with checkbox state), property-test results in evidence,
  full turn-in detail, git surface (staged diff + proposed commit message;
  the user clicks commit — constitution holds).

### State: the useCody reducer

`hooks/api/useCody.ts` mirrors the proven shape: a pure
`buildRunFromTrace(events)` folding the Cody family into a `CodyRun` model
(plan+waves, task states, turn-in cards, decision records, preview state,
needs-input prompt), used both by trace hydration on reopen and the live SSE
`onUpdate` — byte-identical rebuild, vitest-proven (Property: reducer(live
sequence) == reducer(trace replay)). New event types this feature adds to the
family and the trace whitelist: `decision.stack` / `decision.design` (record
authored, awaiting approval), `decision.resolved` (approved/overridden),
`run.needs_input` (question payload), `preview.pending/ready/failed/expired`,
`snapshot.created/restored`, `plan.adopted` (spec ingestion).

### Engine addons

**SDR — Stack Decision phase.** On greenfield detection (workspace model:
empty/no-manifest root) in Engineer/Architect, the orchestrator authors a
Stack Decision Record before planning: requirements profile (app class,
audience, deployment target, team context), 2–3 genuinely divergent candidate
stacks generated hot (decision temperature, N candidates), fit rationale per
candidate citing `rules/stack-selection/`, and a pick. The record passes the
layered gate with an anti-default lens — "would this exact choice have been
made regardless of the requirements? if yes, reject" — then pauses the run
(`needs_input`-shaped decision gate) for user approve/override in the client.
Wave 1 is structurally unreachable without a resolved SDR. Prototype skips
SDR and takes the classic stack (Node/TS, Vite or Next, npm).

**DLR — Design Language phase.** For UI-bearing projects (any mode), a Design
Language Record before the first UI task: typography, palette, spacing/layout
system, motion posture, component idiom, and a named style direction —
screened deterministically against the banned-defaults list
(`rules/web/design-quality.md` banned patterns + stock-shadcn/purple-gradient
/glassmorphism/emoji/default-blue tells) and adjudicated with the lens "could
you tell an AI made this?". Every subsequent UI task sheet carries the DLR as
a constraint; the acceptance gate rejects drift (fresh-context workers regress
to the mean without a binding artifact — this is what makes it stick across
40 tasks). Engineer/Architect: blocking approve/override card. Prototype: the
card is informational in plain language with a "change the look" affordance;
the run proceeds.

**Creativity split.** The mode tuple's single Creativity value becomes
`DecisionCreativity` (hot: Prototype 0.9 / Engineer 0.8 / Architect 0.8, with
`DecisionCandidates` 2–3) and `ImplementationCreativity` (cold: 0.7 / 0.3 /
0.2 as today). SDR/DLR/plan-shape author at decision temperature with N
divergent candidates judged; workers implement cold.

**Spec ingestion.** `/chat` (and the client's new-run flow) accepts a spec
reference — a workspace path (e.g. `SPEC.md`, `.cody/spec/requirements.md`)
or pasted document. The planner ADOPTS it: requirements map to the plan's
acceptance grounding, tasks/waves derive from the document structure where
present, and the ledger records the source (`plan.adopted` event). Resumable
against the adopted spec like any plan.

**Steering + answer channel.** Two routes: `POST /intents/{id}/answer`
resolves a `needs_input` stall (decision gates and stop-and-ask both ride it;
payload = free text or a decision verdict approve/override{...}) and `POST
/intents/{id}/steer` folds a mid-run correction into the live run at the next
orchestrator boundary (before the next sheet is authored) — never interrupts
a mid-flight worker; the orchestrator weighs the steer into subsequent sheets
or re-plans the remaining waves if the steer invalidates them.

**Worker ExtraTools + screenshot evidence.** codyd's `workerFunc` populates
the worker's ExtraTools with browser (shared browser service via
`MATRIX_BROWSER_URL`), fetch, and web-search bridges. UI-task sheets require
a screenshot artifact in the turn-in evidence; the gate rejects UI turn-ins
without one. Screenshots surface in the client's turn-in cards.

**Undo over HTTP.** `POST /workspace/snapshot` (named), `GET
/workspace/snapshots`, `POST /workspace/restore` — thin routes over the
existing Snapshotter. The engine keeps auto-snapshotting before risky
multi-file changes; the client's undo button restores the latest (Prototype)
or a picked (Engineer/Architect) snapshot. Restore refuses while a worker is
alive.

**Projects.** A project registry (`/data/cody/projects.json`) maps project id
→ `{name, root:/workspace/<dir>, mode, created_at}`. `/chat`, `/workspace/*`,
and the ledger take a project id; the orchestrator's workspace model, spec
files, and snapshots scope to the project root. Default project = bare
`/workspace` for retro-compat.

### Preview architecture

Flow: gate accepts the last task of a plan (or the user asks to see it) →
codyd exports the workspace state into a sandbox (create from template →
`files.write` the project tree, or `git clone` over the private network),
runs the project's start command (verification runner already detects it),
health-probes it, then emits `preview.ready {url}` — where url is
`https://<router>/preview/{userID}/` — and the client renders the preview
pane pointing at it. TTL reaper destroys idle sandboxes (config knob, default
30m) and emits `preview.expired`; re-preview is one click (sandbox templates
make respins cheap).

Router: `/preview/{userID}/...` authenticates the JWT (path user must match
token user, same as daemon proxying), looks up the user's active preview
target (codyd registers it via the existing admin/internal listener), and
reverse-proxies over the private network. No sandbox public domains needed;
previews are never world-readable. A later "share link" feature can mint
scoped tokens — non-goal now.

Sandbox client: prefer Go-native — extend `router/internal/railway`'s
GraphQL client (or a `cody/internal/sandbox` sibling using the same
transport) with sandbox create/exec/destroy. VERIFY against the live API
first (the TS SDK is documented, the underlying API surface for sandboxes is
not); if sandboxes are not reachable from Go cleanly, ship a minimal Node
sidecar in the image (Node 22 is already baked) exposing
create/start/destroy on localhost — the seam is identical either way.
Workers may also request an ephemeral sandbox for browser-driven e2e
(playwright runs there, not in the user VM).

### Image, rules, skills

Image (`deploy/railway/Dockerfile`): build + supervise codyd beside neo/mcl
(entrypoint gains the codyd process with `CODY_*` envs); bake PostgreSQL,
Redis, SQLite as exec-bridge-startable local services (no nested Docker on
Railway); bake the quality toolchain (golangci-lint, ruff, eslint, prettier,
tsc, vitest, cargo-clippy — playwright browsers deliberately NOT baked);
sandbox client (Go binary or Node sidecar) included.

Rules: author `rules/stack-selection/` — the app-class → stack decision
tables (chat/realtime → React Router framework mode, not Next;
content/marketing → Astro/Next; internal enterprise → Angular;
high-throughput backend → Go; correctness-critical → Rust; ML-adjacent →
Python/uv; plus escape valve: deviations demand explicit rationale). Andrew
curates; the SDR gate cites it as doctrine.

Skills: framework playbooks for the non-default picks (Angular, React Router
v7/Remix idiom, Astro, SvelteKit), a Railway-deploy skill (Cody's apps deploy
there), a database-schema-design playbook if absent, and a
toolchain-bootstrap skill (apt/mise install of long-tail stacks on demand —
the image bakes the high-probability set only, keeping cold-wake fast).

## Non-goals

- Mobile parity (follow-up feature; the CodyRun model + event family are the
  contract it will consume).
- Cross-plan autonomy (one message/spec = one plan = one run stays correct).
- Task-level blocking review (SDR/DLR are the human checkpoint; task review
  is passive audit).
- Public/shareable preview links (previews stay behind the user's JWT).
- PTY/interactive terminal (v1 is bounded command-run via `/workspace/exec`).
- Parallel workers (sequential one-at-a-time invariant holds).
- Any MCL/signing/wallet surface (constitution: coding is not value-moving).

## Verification strategy (no fakes)

Real code paths, real types, real repos (tempdir git), real httptest servers;
no stub/mock/fake doubles anywhere.

1. **SDR/DLR structural**: a greenfield Engineer run cannot reach wave 1
   without a resolved SDR (real orchestrator, seeded empty repo); a
   default-stack SDR for an internal-enterprise profile is rejected by the
   anti-default lens; a DLR violating the banned-defaults screen is rejected
   deterministically; a UI turn-in drifting from the accepted DLR is rejected
   at the gate; Prototype skips SDR and proceeds.
2. **Decision gate round-trip**: run pauses `needs_input` on SDR; `POST
   /intents/{id}/answer` with override resolves it; the plan proceeds against
   the override; trace replays the decision events byte-identically.
3. **Steering**: a steer mid-plan lands in the next authored sheet (assert
   sheet content), never kills the live worker.
4. **Spec ingestion**: a seeded SPEC.md adopts into a plan whose tasks map to
   the document; kill codyd mid-plan; resume continues against the adopted
   spec.
5. **Preview**: httptest GraphQL fixtures for sandbox create/destroy; router
   `/preview` proxy integration test — JWT-authenticated request reaches a
   fake sandbox listener; cross-user path is 403; TTL reaper destroys and
   emits `preview.expired`.
6. **Undo**: snapshot → mutate → restore round-trips bytes; restore refused
   while a worker is alive.
7. **Client reducer**: vitest — `buildRunFromTrace(trace) ==` live-folded
   state for the full event family including decisions/preview/needs-input;
   tier rendering (Prototype hides terminal/tree/diff; Architect shows spec
   viewer) asserted at the component level.
8. **Screenshot evidence**: a UI-task turn-in without a screenshot artifact
   is rejected; with one, accepted (real gate, seeded sheet).
