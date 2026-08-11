---
auto_execution_mode: 3
description: Centra AI release pipeline
---
# /deploy — Centra AI release pipeline
# Location in repo: /root/matrix/.claude/commands/deploy.md
# Invoke from Claude Code inside /root/matrix with: /deploy
# Optional argument: /deploy force   (proceed past soft-gate failures — build failure always aborts)

You are executing the Centra AI deploy pipeline. Work through the phases below **in order**. Never skip the leak sweep. Never push if Phase 1 blocked the release (unless the user passed `force`, and even then only past soft gates — a build failure is always terminal).

Argument passed: $ARGUMENTS

## Phase 0 — Preflight
1. `cd /root/matrix`
2. Record: current branch, `git status --porcelain`, `git log --oneline -10` since last tag (`git describe --tags --abbrev=0`).
3. If the working tree has uncommitted changes, list them — these are presumably the changes being shipped. If the tree is clean AND HEAD is already pushed, stop and tell the user there is nothing to deploy.
4. Confirm `gh` CLI is authenticated (`gh auth status`). If not, warn that CI watching will be unavailable and ask whether to continue.

## Phase 1 — Gates
1. Run `bash scripts/deploy-checks.sh` and capture the exit code.
2. **Exit 1 (build failed):** ABORT the entire flow. Show the tail of the build log, write a short failure report (Phase 6 format, status=BUILD FAILED), and stop. `force` does not override this.
3. **Exit 2 (soft-gate failures):** All checks ran; failure logs are saved under `/root/matrix/temp/deploy-fails/` with the run timestamp prefix. Summarize which of test/vet/lint/fmt failed with log paths and the key error lines from each log. Unless `force` was passed, STOP here and write the failure report. With `force`, note the accepted failures explicitly and continue.
4. **Exit 0:** proceed.
5. If the summary shows `fmt.dirty=true`, `make fmt` rewrote files — review `git diff --stat`, confirm changes are formatting-only, and include them in the release commit.

## Phase 2 — Version + Changelog + Docs
1. Determine the bump from the actual diff since the last tag (semver): breaking API/consensus/schema change → major; new module, agent, endpoint, or capability → minor; fixes/refactors/docs → patch. State your reasoning in one line.
2. Update every version declaration the repo actually uses — check for `VERSION`, `Makefile` VERSION var, `package.json` (Centra AI Client), `Cargo.toml`, module manifests, and any `.kvx` spec headers that carry a version field. All must agree.
3. Prepend a `CHANGELOG.md` entry: version, date, sections (Added / Changed / Fixed / Security), written from the real commit diff — not from commit messages alone. Keep it in the Sidiora Labs README house style.
4. Update `README.md` and module docs **only if** the shipped changes alter documented behavior, commands, config, or public interfaces. Do not touch docs for internal-only changes. List every doc file you modified and why.

## Phase 3 — Leak sweep (mandatory, blocking)
Inspect everything that would be pushed: `git status --porcelain` (untracked + modified) and `git diff --cached` after staging. Block the push if any of the following would be committed:
1. **Dev-note files:** `TODO*`, `NOTES*`, `SCRATCH*`, `*.draft.*`, `*-wip.*`, personal notes, meeting notes, prompt/spec scratchpads.
2. **Session/agent artifacts:** `temp/` (including `temp/deploy-fails/`), `.kvx` session records, Cortex memory dumps, `.claude/settings.local.json`, `*.mtx` working files not meant for the repo, OpenCode/IDE-generated local rule files that specgen regenerates.
3. **Secrets:** run a grep over the staged diff for `PRIVATE KEY`, `mnemonic`, `seed`, `api[_-]?key`, `secret`, `token=`, `Bearer `, `.env` contents, RPC auth strings, validator keys. Any hit → unstage, show the user, and stop.
4. **Infra internals:** server IPs/hostnames (Contabo boxes), validator node configs, internal endpoints not already public.
Verify `.gitignore` covers `temp/` and local settings; add entries if missing (that .gitignore change ships with the release). Report the sweep result explicitly: files checked, files excluded, verdict.

## Phase 4 — Commit + Push
1. Stage the intended files explicitly (never `git add -A` blind — stage what the sweep cleared).
2. Commit: `release: vX.Y.Z — <one-line summary>` with a body listing the changelog highlights.
3. Tag: `git tag -a vX.Y.Z -m "vX.Y.Z"`.
4. `git push origin <branch> --follow-tags`.

## Phase 5 — CI watch
1. `gh run list --branch <branch> --limit 3` to find the run triggered by the push, then `gh run watch <run-id> --exit-status`.
2. Poll until terminal state. If CI fails: `gh run view <run-id> --log-failed`, extract the failing job/step and key error lines. Do NOT auto-revert or auto-fix; report and stop.
3. If multiple workflows trigger, watch all of them — "full CI passing" means every triggered workflow is green.

## Phase 6 — Report
Write `/root/matrix/temp/deploy-reports/<timestamp>-vX.Y.Z.md` and show it in full:
- **Status:** DEPLOYED / BLOCKED (soft gates) / BUILD FAILED / CI FAILED / LEAK BLOCKED
- **Version:** old → new, bump rationale
- **Gates:** pass/fail per target, log paths for any failures
- **Docs touched:** changelog entry (verbatim), README/doc files modified
- **Leak sweep:** verdict, anything excluded
- **Git:** branch, commit hash, tag, remote
- **CI:** run URLs, per-workflow result, duration
- **Follow-ups:** anything deferred (accepted soft failures under `force`, doc debt, flaky tests)
