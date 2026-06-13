# mclc CLI Reference

`mclc` is the MatrixScript compiler CLI. It wires together the MCL runtime packages and provides commands for compilation, validation, hashing, and parsing of `.mtx` files.

Built from `MCL/cmd/mclc/main.go`. Install with `make install` — binary lands at `./bin/mclc`.

---

## Commands

```
mclc compile   Compile a SKILL.mtx against user prose → Intent IR
mclc validate  Validate a .mtx file against spec §11 rules
mclc hash      Print canonical AST hash (D11 mtx_digest)
mclc parse     Parse a .mtx file and print AST summary
```

---

## compile

Runs the MCL compilation pipeline against a SKILL.mtx file with a given prose input.

```bash
mclc compile \
  -skill <path>       # path to SKILL.mtx (required)
  -prose "text"       # user's natural language goal (required)
  -verb <verb>        # pre-classified D7 verb (skips stage 2)
  -grammar <id>       # grammar constraint ID (default: intent_frame@1)
  -confidence <f>     # current confidence (default: 1.0)
  -model <model>      # LLM model string
  -seed <int>         # D11 seed (default: 42)
  -dry-run            # skip LLM call, print interpolated prompts only
  slot.name=value     # pre-fill a specific slot (any flag with = is treated as a slot)
```

### Output

JSON to stdout:

```json
{
  "mtx_digest": "sha256-hex of the parsed SKILL.mtx AST",
  "matched_condition": "verb=build",
  "executed": true,
  "frame_json": "...LLM output...",
  "prompt_messages": [
    { "role": "system", "content": "..." },
    { "role": "user", "content": "..." }
  ],
  "slots": [
    { "name": "target", "value": "matrix://cortex/...", "status": "resolved", "type": "ArtifactRef" }
  ],
  "unknowns": [
    { "slot_name": "deadline", "severity": "optional", "reason": "..." }
  ],
  "clarify_questions": []
}
```

`status` values for slots: `empty`, `raw`, `resolved`, `default`.

### Dry-run mode

With `-dry-run`, no LLM call is made. `frame_json` is empty, but `prompt_messages` shows the fully interpolated prompt that would have been sent. This is the fastest way to verify that a skill's `§PROCEDURE` produces the prompts you expect.

```bash
mclc compile -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a deployment plan for my Node.js app" \
  -verb build \
  -dry-run
```

If no API key is available, `mclc compile` falls back to dry-run automatically with a warning on stderr.

### Pre-filling slots

Any argument with `=` that doesn't match a known flag is treated as a slot pre-fill:

```bash
mclc compile -skill skills/writing-plans/SKILL.mtx \
  -prose "Build a plan" \
  target=matrix://cortex/artifacts/my-doc@sha256:abc123 \
  deadline=2026-06-30
```

This is equivalent to `intent.draft.body.slot_values` from the API.

### LLM configuration

`-model` accepts a provider-specific model string. Examples:
- `accounts/fireworks/models/deepseek-v4-flash` (Fireworks)
- `deepseek-ai/DeepSeek-V4-Flash` (Together)

If omitted, `llm.DefaultCompilerModel()` is used. Check `MCL/llm/model.go` for the current default.

Environment:
- `FIREWORKS_API_KEY` — API key for Fireworks AI
- `TOGETHER_API_KEY` — API key for Together AI

---

## validate

Validates one or more `.mtx` files against the spec §11 rules.

```bash
mclc validate skills/writing-plans/SKILL.mtx
mclc validate MCL/core/verb.mtx MCL/core/pipeline.mtx
```

Applies `ValidateSkill` for files with a `§SKILL` section, `ValidateCore` for everything else.

Output:
- On success: `<path>: ok`
- On failure: one error per validation rule violation (format: `<path>: <line>:<col>: [V<n>] <message>`)

Exit code is non-zero if any file fails validation.

### What gets checked

| Rule | Check |
|---|---|
| V1 | All 8 required sections present (`SKILL.mtx` only) |
| V2 | `mcl.verbs` values are D7 or `x:` prefixed |
| V4 | No duplicate slot names within a section |
| V5 | `resolve` targets are declared in `§INPUTS` |
| V6 | `unknown` targets are declared in `§INPUTS` |
| V7 | Prompt blocks have both `system=` and `user=` |
| V8 | `§FAILURE_MODES` `reason=` values are in the closed set |
| V9 | `§SUB_SKILLS` URIs are version-pinned |
| V10 | `§TOOLS` URIs are version-pinned |
| V11 | On-block `kind=` values are in the closed `StepKindNames` set |
| V12 | On-block `output_cardinality=` is a strictly positive integer |

---

## hash

Prints the canonical AST hash of one or more `.mtx` files. This is the `mtx_digest` value used in D11 seeds.

```bash
mclc hash skills/writing-plans/SKILL.mtx
# → a3f2c1d4...  skills/writing-plans/SKILL.mtx

mclc hash MCL/core/verb.mtx MCL/core/pipeline.mtx MCL/core/frame.mtx
```

Output format matches `sha256sum` — hex digest followed by the path. You can pipe this into `sha256sum -c` or use it to pin a skill version.

**The hash changes when:**
- Any entry in any section changes (KV values, slot declarations, on-block conditions, prompt text, resolve/unknown/clarify content, failure modes, URI refs)
- Section structure changes (new or removed sections)

**The hash does not change when:**
- Comments are added, removed, or edited
- Blank lines change
- The `§HASH` section changes
- Whitespace within values changes (since values are stored as parsed tokens, not raw text)

This is the key invariant: add all the comments you want, it won't invalidate cached compilations.

---

## parse

Parses a `.mtx` file and prints a summary of its section structure. Useful for quick inspection.

```bash
mclc parse skills/writing-plans/SKILL.mtx
```

Output:

```
skills/writing-plans/SKILL.mtx: 8 sections
  §SKILL: 4 entries
  §INPUTS: 3 entries
  §CORTEX: 1 entries
  §TOOLS: 2 entries
  §SUB_SKILLS: 1 entries
  §PROCEDURE: 3 entries
  §OUTPUTS: 2 entries
  §FAILURE_MODES: 3 entries
```

Entry count includes all entry types (KV pairs, slot declarations, on-blocks, etc.). Nested entries inside on-blocks are not counted separately — each on-block counts as one entry.

Parse errors are printed to stderr and the command exits non-zero. Unlike `validate`, `parse` only checks syntactic correctness, not semantic validity.

---

## Exit codes

| Exit code | Meaning |
|---|---|
| 0 | Success |
| 1 | Error (parse/validate failure, missing args, I/O error) |

---

## Common workflows

### Validate everything before shipping a skill

```bash
mclc validate skills/my-skill/SKILL.mtx && mclc hash skills/my-skill/SKILL.mtx
```

### Check what prompts a skill generates

```bash
mclc compile \
  -skill skills/my-skill/SKILL.mtx \
  -prose "your test input" \
  -verb build \
  -dry-run | jq '.prompt_messages'
```

### Pin a skill version in a manifest

```bash
hash=$(mclc hash skills/my-skill/SKILL.mtx | awk '{print $1}')
echo "matrix://skill/my-skill@sha256:$hash"
```

### Validate all skills in the repository

```bash
find skills -name 'SKILL.mtx' | xargs mclc validate
```
