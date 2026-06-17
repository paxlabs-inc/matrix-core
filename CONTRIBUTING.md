# Contributing to Matrix

## Contribution policy — please read first

Matrix is open source and you are free to **fork it and modify it as much
as you want**. That is exactly what the license is for: build on it, ship
your own version, take it wherever you like.

What we do **not** run is an open contribution model on `main`. The main
branch is developed strictly by the Matrix core team, and it will continue
to be. This is a deliberate decision, not an oversight or a lack of
bandwidth:

- The codebase is spec-driven and the surface is load-bearing — replay
  determinism, closed vocabularies, signed envelopes. A patch that looks
  correct in isolation can silently break an invariant that is not visible
  at the diff level.
- We merge an outside change only when we have **worked directly with the
  contributor** and understand the quality and intent of their work. That
  trust is earned through collaboration, not inferred from a single pull
  request.

Concretely:

- **Unsolicited pull requests will generally not be merged**, regardless
  of how good they are. This is not a judgment of your work — it is how we
  keep the trunk coherent.
- **Issues, bug reports, and security disclosures are welcome.** Open an
  issue, or for a security issue follow [`SECURITY.md`](./SECURITY.md).
- **If you want to contribute to `main`, talk to us first.** Reach out,
  work with us on something, and once we understand your work we are glad
  to bring you in.
- **Forks are first-class.** Everything below this section is the
  engineering bar we hold ourselves to — useful if you are maintaining a
  fork, or if we have already agreed to collaborate.

---

The rest of this document covers the dev setup and the test/lint discipline
expected of every change — whether you are on the core team, a collaborator
we are working with, or maintaining your own fork.

If you are reporting a security issue, **stop** and read
[`SECURITY.md`](./SECURITY.md) instead.

## Table of contents

- [Contribution policy](#contribution-policy--please-read-first)
- [Ground rules](#ground-rules)
- [Dev setup](#dev-setup)
- [Branching and commits](#branching-and-commits)
- [Testing and quality gates](#testing-and-quality-gates)
- [Per-module conventions](#per-module-conventions)
- [Working with `matrix.kvx`](#working-with-matrixkvx)
- [Pull request process](#pull-request-process)
- [Design changes](#design-changes)

## Ground rules

1. **No silent design changes.** Matrix is design-doc-driven. Locked
   decisions live in `research/00-decisions.md` (D1–D18) and the
   per-phase Q-locks live in `knowledge/matrix.kvx`. If your PR moves a
   locked decision, say so explicitly and propose the new lock.
2. **Replay determinism is load-bearing.** Any change to `cortex/` that
   touches mutation paths must preserve byte-identical `OverallRoot`
   under `cortex-shell rebuild -verify-only`. The §13.4 invariant is
   non-negotiable.
3. **Closed vocabularies are closed.** The 10-verb (D7) and 8-`obj_kind`
   (v1) enums are not extension points. Adding a new verb or kind is a
   journaled migration with explicit schema-version bump — not a
   one-line code change.
4. **No new dependencies without justification.** The 4 modules deliberately
   ship small dep graphs (Pebble, cbor/v2, ulid, x/text, x/time on the
   cortex side; stdlib-only on MCL). Bring receipts.
5. **Public surfaces are stable.** If you add a Go exported symbol, you
   own its compatibility story.

## Dev setup

### Required toolchain

- Go **1.21** (every `go.mod` pins `go 1.21`).
- GNU `make` 4.x.
- For MCP-driven flows: `node` ≥ 20 with `npx`, `python3` ≥ 3.11, `uv`.
- For the daemon image: `docker` ≥ 24 with `buildx`.

### First-time setup

```bash
git clone https://github.com/paxlabs-inc/matrix.git
cd matrix
cp .env.example .env          # fill in API keys if you want to run live LLM flows
make build                    # builds all 4 modules
make install                  # drops the 8 CLIs into ./bin
make lint-install             # optional: installs golangci-lint v1.61.0
```

### Smoke check the toolchain

```bash
make version                  # Go + golangci-lint + Docker versions
make ci                       # full local CI gate (fmt-check + vet + test)
```

A clean `make ci` is the bar for opening a PR.

## Branching and commits

We use a flat trunk model on `main`.

### Branch naming

```
<who>/<short-slug>
e.g.
devin/1779567557-phase-12-ema-weights
andrew/fix-cortex-attest-rate-limit
dependabot/gomod/cortex/...
```

### Commit messages

Conventional commits with a scope per module:

```
<type>(<scope>): <summary>

<optional body — wrap at 80 columns>

<optional footer — Refs / Closes / Co-authored-by>
```

Allowed `type` values: `feat`, `fix`, `refactor`, `perf`, `test`, `docs`,
`build`, `ci`, `chore`, `revert`.

Allowed `scope` values:

- `cortex`, `mcl`, `bridge`, `executor` — Go modules.
- `deploy`, `skills`, `agents`, `rules`, `research`, `knowledge`,
  `tools`, `journal` — non-code trees.
- `ci`, `meta` — repo plumbing.

Examples:

```
feat(cortex): rate-limit cortex.Attest at 1/s burst 5

Adds token-bucket gate to logScopeViolation and Cortex.Attest per
phase14_locked_design Q3. Defaults preserved across go test ./...

Refs: matrix.kvx phase14_status
```

```
fix(executor): coerce string args to int/bool for MCP dispatch

JSON-Schema int/bool slots receive strings from LLM-authored plans;
verbatim port of cmd/mcl-e2e/walk.go:249-313 keeps the harness +
production walker aligned.
```

```
docs(meta): add CONTRIBUTING + CODE_OF_CONDUCT + SECURITY
```

### Signing

Sign your commits with `git commit -S` if your key is on GitHub.
Unsigned commits are accepted but signed commits are preferred for
anything touching `cortex/snapshot/`, `cortex/store/`,
`MCL/envelope/`, or `deploy/`.

## Testing and quality gates

Every PR must:

1. **Pass `make ci`** (`gofmt -l` empty + `go vet ./...` + `go test -race
   -count=1 ./...` across all 4 modules).
2. **Pass `golangci-lint`** with the repo config (`make lint`).
3. **Not weaken existing tests.** Removing or skipping a test requires
   an explicit reason in the PR description.
4. **Add tests for new behaviour** — at a minimum a happy-path test plus
   one error path. For cortex mutation paths: add a `Rebuild`-after-mutation
   test (see `cortex/rebuild_test.go` for the pattern).
5. **Update `knowledge/matrix.kvx`** when the change moves a phase status,
   adds an invariant, or closes a deferral.

### Cortex-specific gates

- Touching anything that journals: extend `rebuild_test.go` to cover the
  new mutation surface.
- Touching SMT staging: add a `TestRebuildPreservesOverallRoot` variant.
- Touching the embedder: keep `embedder_migrate_test.go` green and add a
  case if you introduce a new model-pin scenario.
- Touching rate limits: extend `phase14_test.go`.

### MCL-specific gates

- Touching the lexer/parser: any new keyword or symbol needs a token test
  and a parser test.
- Touching the validator: every new rule needs a green-path and a
  red-path test.
- Touching `canonical.go`: hash digests of `core/*.mtx` and
  `skills/writing-plans/SKILL.mtx` must remain stable (or you must
  update the canonical reference hashes in `matrix.kvx` and explain why).

### Skill corpus

If you add or modify a `SKILL.mtx`, run:

```bash
./bin/mcl-validate skills/<slug>/SKILL.mtx
```

The CI job `mtx-corpus` validates every `SKILL.mtx` in the corpus on
every PR. If you regenerated the corpus via
`tools/skills/convert_to_mtx.py`, make sure the hand-authored fixture
`skills/writing-plans/SKILL.mtx` was preserved (it is in `RESERVED_SLUGS`
for a reason).

## Per-module conventions

### `cortex/`

- One Pebble DB per actor. Namespaces are key prefixes — see `keys/`.
- Every mutation goes through `store.BeginWrite` so the journal-batch
  invariant is enforced.
- Tests live next to their packages. The cross-cutting integration
  surface lives in `cortex/<phase>_test.go` files.
- No new top-level package without a phase entry in `matrix.kvx`.

### `MCL/`

- Stdlib-only outside `MCL/llm/` (which talks HTTP via stdlib too) and
  cbor/v2 for envelope encoding.
- Lexer / parser changes must keep `grammar.bnf` in sync.
- New `.mtx` syntax: extend `spec.md` first, then grammar, then code.

### `bridge/`

- Stateless adapter. No state in the `Adapter` struct beyond options.
- `WithLateBinding(true)` is opt-in; default preserves the Phase 3
  "compile-time `Find` does NOT journal" invariant.

### `executor/`

- `runtime/walker.go` is the canonical walker. Harness walkers in
  `cmd/mcl-e2e/` are allowed to drift for testability but are not the
  source of truth.
- Tool URIs must be version-pinned (`@<semver>` or `@sha256:...`) at
  parse time. `ErrUnpinnedTool` fires on bare-head URIs.
- MCP credentials are `$env:NAME` refs, never literal values.

### `deploy/`

- `Dockerfile` images install everything they need at build time. No
  runtime `apt-get install` or `npm install` — that's a cold-start
  killer.
- `entrypoint.sh` is idempotent. Run twice in a row, get the same state.
- `bootstrap.sh` (box) is also idempotent. Test by running it on a
  freshly-rebooted box.

## Working with `matrix.kvx`

`knowledge/matrix.kvx` is the canonical project state across sessions. It
is **not** prose documentation — it is dense `key=value` facts intended to
be loaded at the top of every working session.

Rules:

1. **Every claim must be citeable.** A file:line reference, a measured
   number (LOC count, test count), a previously-locked decision, or a
   direct user-stated choice.
2. **No hallucinations.** When in doubt, omit. The kvx is load-bearing;
   a single fabricated entry corrupts future-session reasoning.
3. **Append-only-ish.** New session entries land at the bottom; you may
   edit prior entries only to correct factual errors. Mark corrections
   with a brief note.
4. **Bump the version header** (`MATRIX.kvx v<version> | <date>`) when
   you land a substantive change.

## Pull request process

1. **Open a draft early.** Even a stub PR with the description filled in
   is better than a surprise patch.
2. **Tick the affected-modules boxes** in the PR template so reviewers
   can route by ownership (`.github/CODEOWNERS`).
3. **Paste verification output.** `make ci`, smoke transcripts, or
   relevant snippets — whatever proves the change does what it says.
4. **Keep PRs scoped.** One concern per PR. Refactors split from
   behaviour changes whenever possible.
5. **Respond to review.** Push fix-up commits; do not rewrite history
   while review is in flight.
6. **Squash on merge.** History on `main` is one commit per landed PR,
   with the PR title as the commit subject and the PR body as the
   commit body.

## Design changes

Anything that moves a locked decision (D1–D18 + phase Q-locks) requires a
design-review PR before code lands.

A design-review PR:

- Updates the relevant chapter in `research/`.
- Adds the new lock (Q-number) to `matrix.kvx` under the right phase.
- Has at least one paragraph in the PR body answering: *what changes,
  why now, what previously-locked decision does this supersede, and
  what is the migration plan for any external consumer?*

If the design change requires a journaled schema bump (cortex SMT,
envelope schema, IR canonical JSON, etc.), include a migration test
covering both directions.

---

The bar is high because the surface is load-bearing. If you are working in
your own fork, this is the standard the code was built to — hold it and it
will serve you well. If you are collaborating with us directly, this is the
standard we will hold the work to together. Either way, we appreciate the
care.
