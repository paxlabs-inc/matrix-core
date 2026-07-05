# Cody Launch — commercial-grade hardening (the Claude Code bar)

Cody, `cody-client`, and `cody-smoothness` are specced and largely built. Yet a
real run — the user's "make me a video" creative build on 2026-07-05 — was, in
the user's words, "absolutely dog shit": the worker burned ~90 model calls
flailing for a screenshot it could never take, turned in with absolute paths a
blind adjudicator could not read, was rejected three times, and the plan died
having produced nothing the user could see. Neo, separately, still spiralled on
a completion loop after a fix was deployed. This feature is the **launch
pass**: fold every flaw from the 2026-07-05 Neo/Cody/Cassandra audit into a
sequenced, dependency-waved 25-session plan, fix them to a commercial bar, and
PROVE the fix against the exact failure shapes with no-fakes end-to-end tests.

The bar is explicit and non-negotiable: **as good as Claude Code, for users, no
exceptions**. Every session is production/commercial grade — imagine this is
Google, AWS, or Microsoft shipping it. No shortcuts, no sloppy work, no fakes.

## The three load-bearing ideas

1. **The runtime is judged by the run, not the spec.** The prior specs describe
   a correct system; the audit proves the *runtime* fails on ordinary asks
   because of a small number of concrete, compounding defects. Launch-readiness
   is measured by real runs succeeding, not by requirements being written.

2. **One failure class, three faces.** The Cody rejection loop (worker spends
   into a gate it cannot satisfy), the Neo completion loop (loop keeps spending
   while the gate keeps rejecting), and the blind-adjudicator problem are the
   SAME disease: an unbounded/unsatisfiable acceptance interaction. Fixing the
   shared seam (path normalization, capability-aware gates, unified
   unproductive-attempt accounting, one acceptance predicate) removes the class
   across all three modules at once.

3. **Every fix is proven against the shape that broke.** No fix lands without a
   real test that reproduces the original failure and now passes — including a
   full creative-build E2E that produces a real running preview and a real
   screenshot, gate-accepted, with no stub/mock/fake anywhere in the path.

## The audit — flaws folded into this plan (ground truth, with citations)

Severities: **BLOCKER** (launch-stopping), **HIGH**, **MEDIUM**, **LOW**.

### BLOCKERS

- **Y1 — Adjudication is blind to absolute paths.** The planner emits absolute
  paths in goals/verify/grounding; the worker records whatever path the model
  passed (`cody/internal/worker/worker.go` `recordChange`, ~L461), so
  `report.Changes` carries absolute paths; then `gate.renderSource`
  (`cody/internal/gate/gate.go`, ~L318) **skips absolute paths**
  (`filepath.IsAbs(rel)` guard) → `BuildEvidence` hands Cassandra NO file
  source → structural/UI acceptance is rejected as "unconfirmed" → 3 attempts
  fail → plan FAILS. This is the mechanism behind the logged failure. Proven
  end-to-end.

- **N1 / C1 — The two safety-critical test suites do not compile.** A "remove
  citation-matching, judge on the transcript" refactor updated SOURCE
  (`verdictAccepts` dropped its citations arg; `buildAuditContract` went 5-arg;
  `cassandra.Verdict.CheckCitations` deleted) but left tests calling the old
  signatures: `neo/internal/agent/cassandra_test.go` (3-arg `verdictAccepts`,
  6-arg `buildAuditContract`), `neo/internal/agent/completion_test.go` (`salientTokens`,
  ~L150), `cassandra/verdict_test.go` (`CheckCitations`). Result: **zero
  regression protection on the completion gate and the completeness auditor** —
  the exact subsystems that spiralled.

### HIGH

- **Y2 — The worker double-joins absolute paths.** `worker.toolList` (~L531)
  does `filepath.Join(root, rel)` with no absolute guard; proven:
  `filepath.Join("/data/workspace/app", "/data/workspace/app")` →
  `/data/workspace/app/data/workspace/app`. `fs_list` on an absolute path
  always errors; `toolWrite`/`toolRead` stat the wrong path — the fresh-worker
  re-orientation flailing seen in the log. (`edit.resolve` tolerates
  absolute-in-root, masking it as intermittent.)

- **Y3 — The screenshot gate is unsatisfiable without external infra.**
  `gate.ScreenScreenshot` (~L243) hard-rejects a UI turn-in that changed a
  rendered file and has no screenshot; the only real screenshot path is
  `browser_screenshot` → external `MATRIX_BROWSER_URL` playwright service
  (`cody/internal/tools/browser.go`, ~L72). Unset/unreachable → no screenshot
  is ever possible → every genuine UI task is permanently unsatisfiable. No
  graceful degradation. ~40 of 90 logged worker calls were spent hunting a
  screenshot binary.

- **N2 — Neo's residual completion loop.** The `task_complete` gate branch
  (`neo/internal/agent/agent.go`, ~L883–938) `continue`s BEFORE the
  no-progress/stall detector (~L940+), and any genuine tool call resets
  `guidanceNudges = 0` (~L1004). So `[genuine work → premature task_complete →
  reject]*` is bounded ONLY by `StepBudget` (50) — not the nudge cap (3) nor the
  stall detector (4). With `TaskMaxRespawns=50`, worst case ≈ 2,500 model calls
  before an honest partial. This is the "deployed the fix, still looped"
  residue: Fix 1 closed the *bundled* case; the *interleaved* case remains.

- **Y8 — The intended spec pipeline was never built.** Cody's Architect mode
  writes a hand-rolled `.cody/spec/{requirements,tasks}.md`
  (`cody/internal/orchestrator/specfiles.go`, ~L14–30); there is no `spec.kvx`,
  no `design.body.md`, `specgen` is never invoked, and there is no research or
  clarifying-questions phase before planning.

### MEDIUM

- **Y4 — Muddy capability contract.** `browserScreenshot` returns the "not
  configured" message in the success/path slot with `nil` error
  (`browser.go`, ~L73) — a caller could treat the message as a path.

- **Y5 — Ceremony latency before any code.** A trivial creative ask runs SDR + 3
  DLR candidates + design adjudicator + 2 planner candidates + plan adjudicator
  ≈ 8 sequential LLM calls before a line is written (`cody/internal/server/engine.go`,
  ~L788–848). Claude Code writes immediately.

- **Y6 — No detection of a structurally-unsatisfiable gate.** `runTask`
  re-authors the SAME sheet up to `MaxAttempts=3` with feedback, then fails the
  whole plan (`cody/internal/orchestrator/orchestrator.go`, ~L233–235). When the
  gate itself cannot be satisfied (Y1/Y3), all 3 attempts fail identically —
  there is no "the verifier can't be satisfied → replan / relax / ask" path.

- **Y7 — Fresh-worker recon waste.** Every worker re-scans the workspace (5–8
  steps) because no cached workspace map is handed in the sheet grounding.

- **C2 / C3 — Cassandra fails toward success on a degenerate verdict.** A parsed
  verdict with only a rationale normalizes to `coverage=full`
  (`cassandra/parse.go`, ~L74) and `grounded=true` (~L85), so `Sound()` returns
  true; and escalation unconditionally replaces the primary verdict even if the
  escalated one is less certain (`cassandra/adjudicator.go`, ~L74). Wrong default
  direction for a faculty whose thesis is "absence ≠ success."

- **C5 — Goal-type classification is unpinned.** The QUESTION/STATUS/"truthful
  no" logic (`cassandra/prompt.go`, ~L25–34) compiles but has no test, and it
  loosens the auditor — it needs a grounding-guard test so an *unchecked* "no"
  cannot pass.

### CONTRADICTIONS / LOW

- **X1 — "Acceptance" is defined three times:** `cassandra.Sound()`
  (`verdict.go`, ~L122), `neo.verdictAccepts` (`cassandra.go`, ~L149), and
  Cody's inline check (`gate.go`, ~L391). Three copies drift; there must be one
  shared predicate.

- **Y9 — Cody tests pass but use only relative paths,** so Y1/Y2 are completely
  uncovered — green tests over a broken real shape.

- **X2 (process)** — the citation-removal refactor landed without running the
  `neo`/`cassandra` module tests; a CI gate must forbid a red or non-compiling
  module test package.

## The fix architecture

- **One path seam (Y1, Y2, Y9).** Introduce a single workspace-path
  normalization function used at EVERY Cody tool/gate boundary: given a
  model-supplied path, resolve it against the root, reject escapes, and store /
  compare it as a clean **workspace-relative** path. `toolList/toolRead/
  toolWrite/toolDelete` normalize on entry; `recordChange` stores relative;
  `gate.renderSource` and the do-not-touch screens relativize before matching;
  the planner emits relative paths in sheets. One seam, tested against the
  absolute-path shape that broke.

- **Capability-aware gates (Y3, Y4, Y6).** UI verification asks the environment
  what it can do. If a real screenshot capability exists (browser service
  reachable), the screenshot gate binds as today. If not, it degrades to a
  non-blocking advisory plus a deterministic render check — never an
  unsatisfiable hard rejection. `browserScreenshot` returns a typed capability
  signal, not a message-as-path. The orchestrator detects a repeated gate reason
  with no possible worker action (an unsatisfiable gate) and stops-and-asks or
  replans instead of burning attempts.

- **Unified unproductive-attempt accounting (N2, N3, N4).** Neo tracks
  *unproductive* attempts on a single counter that completion-gate rejections,
  stalls, and nudges all feed, and that only genuine *accepted progress* resets
  — so an interleaved `[work → premature complete → reject]*` pattern is bounded
  by that counter, not merely by the step budget. The completion branch runs the
  no-progress read before it `continue`s.

- **One acceptance predicate (X1, C2, C3, C5).** `cassandra.Sound()` becomes the
  single source of truth; `neo.verdictAccepts` and Cody's gate call it. It fails
  toward refusal on degenerate/low-confidence verdicts; escalation keeps the
  stricter verdict; the goal-type classification gets a grounding-guard test.

- **Cody depth & economy (Y5, Y7, Y8).** The cached workspace map rides the
  sheet grounding so workers skip re-recon; a fast-path collapses ceremony for
  low-stakes intents while the constitution binds identically; and Architect mode
  authors real `spec.kvx` + `design.body.md` through `specgen`, preceded by a
  research phase and a clarifying-questions phase when the ask is underspecified.

## The coding UI — to the Claude Code bar

The surface is specced in `cody-client` (the `/cody` app, three disclosure tiers,
SDR/DLR, preview-as-deliverable) and `cody-smoothness` (always-live progress
spine, one context-aware composer, total persistence). This feature does NOT
re-spec them; it **hardens and polishes the built surface to commercial quality
and verifies it live**:

- **Design-quality conformance** — the built `/cody` surface is audited against
  `rules/web/design-quality.md` and the house rules: separation by
  background-color contrast only (never border strokes for depth), no emojis, no
  purple/indigo gradients, no glow. Any drift is fixed.
- **Progress + narration** — no dead-silent gaps (including the approve→next
  gap); Cody narrates as it works; the viewport is never blank while running or
  awaiting.
- **Diff / tree fidelity** — real diffs with syntax, real navigation, real file
  tree state.
- **Terminal + preview** — a live terminal attached to the environment and a
  dev-server preview pane wired to the preview Manager: the user SEES the running
  result, which is the entire point of a "make me a X" ask.
- **Plan board + evidence cards** — the waved task list, per-task status, and
  turn-in cards carry real verification evidence, in result language, with zero
  orchestrator/worker/sheet jargon leaking to consumer copy.

### The per-tier reference target (locked 2026-07-05)

The three disclosure tiers each have a best-in-class reference surface — one
reference per mode, not a single unified layout. Reference screenshots live at
`temp/ui_options_for_cody/{emergent_style,leap_new_style,replit_style}`.

- **Prototype → Emergent style.** A cloud/atmosphere hero ("What will you build
  today?"), an intent-tabbed composer (Full Stack / Mobile / Landing /
  Brainstorm), then a chat-left + live **App Preview**-right split with a
  run-details panel (credits, machine, model, assets, GitHub) and a prominent
  Deploy. Vibe-first, preview-as-hero, minimal machinery.
- **Engineer → Leap.new style.** A left rail of change history + task-step
  progress + a "What's next?" composer (Scope / Thinking / Debug chips); the main
  area is a top-tab workspace **Preview | Code | Architecture | Infrastructure |
  Service Catalog** with a file-tree + diff-line-count code view, an architecture
  diagram, and a DB/infra panel; a bottom **BUILD / LOGS / TESTS** console. The
  engineering cockpit.
- **Architect → Replit style.** A three-pane layout: a chat rail (transcript +
  "Checkpoint made" / "Worked for Ns" + Plan/Economy composer) | a tabbed center
  (Preview / Shell / Database / editor + a "search tools & files" command
  palette) | a right **Library** file tree; Invite/Publish affordances; full
  control including the terminal/shell. Maximum machinery.

**Non-negotiable overlay.** Adopt each reference's *information architecture and
interaction patterns only* — never its chrome. All three references use border
strokes for depth and glow/gradient accents, which are BANNED by the house
rules. Every tier renders in Matrix styling: separation by background-tone
contrast only, single Paxeer Blue accent, no glow, no gradients, no emojis,
result-over-protocol copy. This mapping is the durable design target for the
`task.5.x` surface work (`req.12`) and refines the `cody-client` tier
composition (`req.4`); the built surface lives in
`apps/client/components/matrix/cody/`.

## The 25-session working protocol (baked in — follow every session)

Each of the 25 tasks is ONE session. The protocol is the durable operating
contract for this feature and is enforced, not suggested:

1. **Get context.** Run `cortex_recall` (MCP) once at session start; if any tool
   output is flagged truncated, READ IT IN FULL before reasoning (hard rule).
   Then read this feature's `spec.kvx` and the **previous session's handoff**
   (the most recent `cody-launch` cortex outcome/handoff note).
2. **Straight to work.** All needed context comes from the handoff + recall —
   gather what the task needs, then work the ONE selected task (lowest eligible
   wave, all `requires` done). Set its status to `in_progress` in `spec.kvx`
   before starting; exactly one task is `in_progress` at a time.
3. **Build on real code.** Implement against the task's referenced acceptance
   criteria. Deliver complete, runnable artifacts — never diffs, never
   fragments. Follow existing repo style; add no comments/docs unless asked.
4. **Write and run tests — fix ALL broken tests, even pre-existing ones.** Every
   task ships table-driven tests (happy + error + boundary) exercising REAL code
   paths with REAL types — no stub/mock/fake doubles, ever. Run the affected
   module's `go build ./... && go vet ./... && go test ./...` (with `-race`
   where concurrency is touched). If a test is broken — even one you did not
   write — FIX it; a non-compiling or red test package is a stop condition, not
   a footnote.
5. **Green, then close the loop.** Only when everything is genuinely green, mark
   the task `done` in `spec.kvx`. Then write a concise **handoff** for the next
   session (what shipped, what is green, exact next task + any gotchas), record a
   `cortex_note_outcome` (success|partial|failure) plus any durable
   `cortex_remember_*` learnings, and close out. Never attest a completion that
   did not happen; surface honest partials.
6. **Guard before risk.** Before any destructive/irreversible/prod/secret/
   network-write action, run `cortex_guard` and self-check against the HARD
   rules; stop for the user's explicit YES if it risks a HARD rule. Do not run
   git on the dev box; the user drives commits.

## The quality bar (non-negotiable)

- **Commercial/production grade only.** Imagine Google/AWS/Microsoft shipping
  this. No shortcuts, no sloppy work, no "good enough for now."
- **No fakes.** Real code paths, real types, real evidence. A green test driven
  by a fake is false completeness — the exact failure this whole feature exists
  to eliminate.
- **Backpressure, bounds, and honesty.** Loops are bounded; gates are
  satisfiable or they stop-and-ask; partials are surfaced as partials;
  truncated output is read in full before reasoning.
- **The constitution binds in every mode:** no fakes, verify-before-done,
  read-full, no false success, complete artifacts, user drives git, respect the
  project. Modes dial ceremony, never the standard.

## Sequencing rationale (waves)

Wave 1 unblocks regression safety (the test suites must compile before anything
is trusted). Wave 2 lands the independent seams (Cody path seam, the shared
acceptance predicate, the UI design-quality audit). Waves 3–5 build the
dependent fixes (gate-sees-source, Neo loop bound, Cassandra safety, spec
pipeline, progress/diff surfaces) and their regressions. Waves 6–7 finish the UI
fidelity and prove the whole thing with the creative-build E2E and a full green
+ coverage gate. Wave 8 is the honest launch-readiness checkpoint.

## Non-goals

- Re-speccing `cody`, `cody-client`, or `cody-smoothness` surfaces — this
  feature hardens and proves them, it does not redesign them.
- Any MCL / signing / wallet path for Cody (coding is not value-moving).
- Touching Neo's conversational product surface beyond the completion-loop bound
  and the shared acceptance predicate.
- Rebuilding the client from scratch; the UI phase is conformance + fidelity +
  live verification against the Claude Code bar.

## No-fakes verification strategy

- **Path seam:** a test drives a real worker writing an absolute-path file, then
  asserts the real gate's evidence contains the file source and the real
  adjudicator grounds on it (the Y1/Y2/Y9 shape).
- **Capability gates:** a real orchestrator run with the browser capability
  ABSENT accepts a correct UI turn-in via graceful degradation; with it PRESENT
  the screenshot gate binds; an unsatisfiable gate stops-and-asks rather than
  burning attempts.
- **Neo loop:** a real agent loop driven to emit `[work → premature complete →
  reject]*` terminates with an honest partial within the unified bound, not the
  step budget.
- **One predicate:** the same degenerate/low-confidence `cassandra.Verdict` is
  rejected identically by Neo, Cody, and Cassandra's own `Sound()`.
- **Creative-build E2E:** a seeded "make me a small web app" run produces a real
  running dev-server preview and a real screenshot artifact, is gate-accepted,
  and leaves the user with a viewable result — with no stub/mock/fake in the
  path.
- **Full green + coverage:** `go build/vet/test ./... ` (with `-race` where
  concurrency is touched) green across `cody`, `neo`, and `cassandra`; the
  client reducer tests green; a coverage gate on the changed packages.
