<!--
Thanks for contributing to Matrix.
Keep PRs scoped to one concern. Squash on merge unless the history is
itself the artefact (e.g. mechanical refactor split into reviewable steps).
-->

## Summary

<!-- One paragraph. What changed and why. Link an issue if applicable. -->

Closes #

## Affected modules

<!-- Tick all that apply. CI fans out per module; reviewers route by this. -->

- [ ] `cortex/` (Pebble store, snapshots, replay, embedder)
- [ ] `MCL/` (compiler, IR, envelope, llm client)
- [ ] `bridge/` (MCL ↔ cortex glue)
- [ ] `executor/` (lifecycle, runtime, MCP, tool registry, daemon, mcl-execute)
- [ ] `deploy/` (Dockerfile, Fly templates, box bootstrap)
- [ ] `skills/` or `agents/` (corpus / manifest)
- [ ] `research/` or `knowledge/` (design docs, canonical .kvx)
- [ ] meta (`.github/`, `Makefile`, root docs)

## Verification

<!-- Paste relevant command output. Anything load-bearing should ship green. -->

```text
$ make ci
...
```

- [ ] `make ci` is green locally (or CI is green on this PR)
- [ ] `go vet ./...` clean in every touched module
- [ ] New behaviour has tests, or the absence is explained
- [ ] If `cortex/` was touched: replay invariant verified
  (`go test -count=1 -run TestRebuild ./...` in `cortex/`)
- [ ] If `skills/` was touched: corpus still validates
  (`make mtx-corpus` or hand-run `mcl-validate`)
- [ ] If `deploy/` was touched: `make docker-daemon` builds clean

## Determinism / replay (cortex changes only)

<!-- Delete this section if cortex/ is untouched. -->

- [ ] No new mutation surface added without a `Kind*` journal entry
- [ ] Atomic batch invariant preserved (`store.BeginWrite` for multi-key writes)
- [ ] `OverallRoot` pre/post test added if a new write path landed
- [ ] Per-Phase invariants in `matrix.kvx` reviewed and (if needed) extended

## Notes for the reviewer

<!--
Anything tricky, future-work, deferred items, or follow-ups.
Cite file:line for non-obvious behaviour.
-->
