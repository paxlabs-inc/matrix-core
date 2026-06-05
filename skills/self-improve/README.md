# Matrix recursive self-optimization skill suite

Five `SKILL.mtx` manifests that let the Matrix system improve itself through the Forge surface
without falling into the sess#37 failure mode (reason-only plans that read nothing and persist
nothing). Each one is the compiler-participating manifest; author a matching `SKILL.md` prose
body in the same directory — the **planner reads `SKILL.md`**, so the tool-call mandate in each
manifest must be echoed there too.

## The loop

```
self-improve  (verb=build|delegate)   ← the recursive driver
   ├─ sub_dispatch self-map        (verb=analyze)         OBSERVE  → Fact per file
   ├─ sub_dispatch self-diagnose   (verb=analyze)         ORIENT   → ranked Pattern
   ├─ gate → architect (Andrew)                           DECIDE
   ├─ sub_dispatch self-optimize   (verb=modify|build)    ACT      → edit + Event
   └─ sub_dispatch self-verify     (verb=analyze|monitor) VERIFY   → build-health Fact + verdict
```

It is *recursive* because every leaf skill **writes** typed memories (Fact / Pattern / Event)
that the next pass's cold-start `cortex.context()` bundle **reads**. Self-knowledge compounds
pass over pass instead of evaporating into a transcript.

## What it fixes (sess#37 / your "what is the compiler there for" question)

The bulk-converted skills declared `§TOOLS=none`, so the planner inherited an empty tool list and
emitted `kind:step / kind:reason` nodes only — it rationalised about a path it never read, then
hallucinated "memories have been drafted." The compiler was fine; the *skill* (the translation
layer) was the failure. These five fix it at the load-bearing input:

1. **Real `§TOOLS`** — version-pinned `matrix://tool/mcp/...` URIs, never `none`. This alone
   stops the planner reading "no tools available."
2. **Explicit tool-call mandate in every `prompt` block** — each on-block tells the planner the
   exact `tool_call` sequence (list → read fan-out → write → diff → persist) and forbids
   reason-only plans and "edit a file you didn't read."
3. **`kind=` + `output_cardinality=` annotations** — route step nodes to the right executor model
   and tell the planner to fan out (`NodeParallel` with N children) instead of N sequential steps.
4. **`resolve` reads + a persistence `tool_call`** — compile-time `cortex.find` pulls prior
   self-knowledge (D13, pure read; honours the S2 "no cortex writes at compile" lock); execution-time
   `forge-bridge.shell_exec` POSTs to the daemon `/memory` route to write the new memories.

## Prerequisites (wire these or the URIs won't resolve)

`agents/forge.json` must declare these aliases + tool names + versions (adjust the version pins to
match your actually-installed MCP servers):

| alias          | tools used                                   | version pin in `§TOOLS` |
|----------------|----------------------------------------------|-------------------------|
| `fs`           | `directory_tree`, `list_directory`, `read_text_file`, `write_file` | `@2024.11.1` |
| `git`          | `git_status`, `git_diff`                     | `@2026.1.14`            |
| `forge-bridge` | `shell_exec`, `opencode_run`                 | `@0.1.0`                |

`forge-bridge.shell_exec` needs `$MATRIX_DAEMON` (or equivalent) in its environment so the persist
step can `curl -X POST $MATRIX_DAEMON/memory`. The daemon `POST /memory` write surface (sess#29)
and `-forge-mode` fs routes (sess#34) must be live. `self-optimize` writes are confined to
`ForgeFSPolicy` AllowRoots=`/root/matrix` with cortex/knowledge/journal denied — keep that denylist
so the loop can't edit its own memory store.

**Defense-in-depth (recommended, fix #2 from the transcript):** add the planner-prompt clause in
`synthesize.go:buildSystemPrompt` — *"when the Intent prose references a filesystem path or any
persistence verb, the PlanTree MUST contain at least one `tool_call`; reason-only plans are
forbidden; if a skill declares `§TOOLS=none`, ignore it and use the agent manifest's full tool
set."* That catches any future thin skill that lies about its tool surface.

## Install + validate + run

```bash
# 1. drop each dir into the corpus
cp -r self-map self-diagnose self-optimize self-verify self-improve /root/matrix/skills/

# 2. author the matching SKILL.md prose bodies (the planner reads them) — same tool-call mandate

# 3. validate every manifest (canonical surface per sess#29)
for s in self-map self-diagnose self-optimize self-verify self-improve; do
  mclc validate /root/matrix/skills/$s/SKILL.mtx
done   # each should print: ok

# 4. seed the loop's identity/goal once (optional) then drive a pass from Forge or the daemon
curl -s -X POST $MATRIX_DAEMON/messages \
  -d '{"skill_uri":"matrix://skill/self-improve@0.1.0","verb":"build",
       "prose":"Improve the planner so it never emits a reason-only plan for a filesystem path",
       "slots":{"target":"executor/cmd/mcl-execute"}}'
```

Start with `self-map` against one small subsystem to confirm tool_call nodes actually fire and
Facts land in cortex (`GET /memory/recent` should be non-zero — the exact check that read `0` in
sess#37). Then run `self-improve` for the full gated loop.

## Notes

- `§HASH.digest` is intentionally empty; `mtxc` fills it at publish time. A hand-authored digest
  fails `mtx-validate`.
- `verb=delegate` on `self-improve` runs the analysis half only (map → diagnose → gate → stop) for
  architect-in-the-loop passes where you want to pick before anything is written.
- `self-verify` gates `keep|retry|revert` on real `go vet/build/test` exit codes — an optimization
  is not "done" until all three return 0.
