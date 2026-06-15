# MatrixScript Language Reference

MatrixScript (`.mtx`) is the declarative language in which MCL is written. Compiler logic, skill procedures, type schemas, and the pipeline wiring all live in `.mtx` files. The Go runtime in `MCL/mtx/` interprets them. The Go binaries contain no compile logic themselves.

This distinction matters: if you want to change how a skill frames a user request, you edit a `.mtx` file. You don't touch Go.

---

## File structure

A `.mtx` file is a sequence of named sections:

```
§SECTION_NAME
key=value
slot name: Type
on condition
  ...
end
```

Sections start with `§` (U+00A7) followed by an uppercase identifier. Everything between one section header and the next belongs to that section. The parser is non-indented at the section level — individual block interiors use exactly 2-space indentation for their child lines.

The formal grammar lives at `MCL/mtx/grammar.bnf`. What follows here is a practical guide.

---

## Lexical rules

**Encoding:** UTF-8. Files are expected to be NFC-normalised (standard from any modern editor). CRLF is normalised to LF on read.

**Comments:** `#` to end of line. Comments are preserved in the parse tree for display/diff purposes but stripped before canonical hashing (D11), so they don't affect the compiler seed.

**Identifiers:** `[a-zA-Z][a-zA-Z0-9_-]*`. Case-sensitive. Hyphens are valid in idents, which is why `best_effort` and `per_intent` are keywords but `foo-bar` is a valid ident too.

**Section names** must be uppercase: `§SKILL`, `§INPUTS`, `§PROCEDURE`, etc.

**Indentation:** Exactly 2 spaces signals a continuation line (slot modifier, prompt role entry, unknown modifier, fail modifier). On-block body entries are at column 0 — they are not indented relative to their enclosing `on` header.

---

## Value types

| Literal | Example | Notes |
|---|---|---|
| String | `"text with {slot.name}"` | Double-quoted. Escape `\"` `\\` `\n` `\t`. Supports `{...}` interpolation |
| Integer | `42` | |
| Float | `0.75` | |
| Bool | `true` / `false` | |
| URI | `matrix://skill/writing-plans@1.0.0` | `matrix://` is first-class — no quotes needed |
| Enum type | `enum<a\|b\|c>` | Used in slot type annotations |
| Space list | `find acquire build` | Space-separated idents — used for `mcl.verbs` and similar list-valued fields |
| Bare ident | `best_effort` | When a keyword or identifier is used directly as a value |

---

## Key-value pairs

The most common construct. One per line, at the section level:

```mtx
§SKILL
name="Writing Plans"
version=1.0.0
mcl.verbs=build modify deliver
```

Keys can be dotted — this is how the pipeline module declares its stages:

```mtx
stage.1.id=normalise
stage.1.type=pure
stage.1.input=intent.draft.prose
```

The parser splits dotted keys on `.` and stores them as a string slice in `ast.KVPair.Key`. There is no nested object syntax — dotted paths are purely a naming convention.

---

## Slot declarations

Slots are typed named fields, used to declare the inputs and outputs of a skill:

```mtx
§INPUTS
slot target: ArtifactRef
  required
  hint="The document or codebase to operate on"

slot deadline: iso8601
  optional
  default="next friday"
```

Syntax: `slot <name>: <TypeRef>` on one line, followed by indented modifiers.

### Type annotations

```
string         free text
uint           unsigned integer
float          floating point
bool           boolean
ulid           ULID identifier string
iso8601        ISO 8601 datetime string
did            DID string
sha256         hex-encoded SHA-256 digest
ArtifactRef    matrix:// URI pointing to an artifact
Constraint     typed constraint (see ir.Constraint)
Predicate      success predicate (see ir.Predicate)
Unknown        gap marker
PlanDraft      planning sketch
SkillRef       matrix://skill/... reference
ToolRef        matrix://tool/... reference
CortexRef      matrix://cortex/... reference
SlotPath       dot-path into a Frame
AssetAmount    { asset, amount }
AgentRef       matrix://agent/... DID
UserRef        matrix://user/... DID
```

Append `[]` for a list: `string[]`, `Constraint[]`.

Enums are inline: `enum<draft|proposed|accepted>`.

### Slot modifiers (indented)

| Modifier | Meaning |
|---|---|
| `required` | Slot must be filled; if empty at sign-off → blocking Unknown |
| `optional` | Slot may be absent |
| `default=<value>` | Pre-fill if no value supplied |
| `hint="text"` | Human-readable description fed into LLM context |
| `max=<int>` | Maximum list length (for `[]` types) |

**Lexer invariant (firm rule, sess#22c):** String-typed modifier values like `hint` and `default` must be double-quoted. Unquoted, the lexer parses the words as a space-separated ident list (`SpaceListValue`), not a string. This is a known footgun.

---

## On-blocks (decision trees)

The primary control flow construct. The interpreter walks `on`-blocks top-to-bottom in a `§PROCEDURE` section and executes the first one that matches.

```mtx
on verb=build
  prompt
    system="You are a planning expert. Extract a typed plan from the user's goal."
    user="{prose}\n\nContext: {cortex.bundle}"
  end
  resolve slot.target <- cortex.find(type="ArtifactRef", near=slot.target.prose)
end
```

### Conditions

| Condition | Syntax | Matches when |
|---|---|---|
| Verb match | `verb=build` | Classified verb equals `build` |
| Confidence | `confidence<0.75` | Confidence is below threshold |
| Confidence | `confidence>=0.85` | Confidence at or above |
| Slot value | `slot.phase=draft` | Slot `phase` holds the value `"draft"` |
| Unknown | `unknown` | Any blocking Unknown is registered |

Comparators: `<` `<=` `>` `>=` `==`.

On-blocks can be nested. Each has its own `end` keyword at column 0.

### On-block body entries

Inside an `on` block you can have:

- `prompt` blocks — sends messages to the LLM
- `resolve` statements — fills a slot from cortex
- `unknown` blocks — declares a gap
- `clarify` blocks — generates a question for the user
- KV pairs — metadata hints for the executor (`kind`, `output_cardinality`)
- nested `on` blocks

All of these are at column 0 inside the `on` body.

---

## Prompt blocks

```mtx
prompt
  system="You are the Matrix Frame Extractor..."
  user="Goal: {prose}\n\nContext: {cortex.bundle}\n\nExtract the frame."
end
```

Must contain at least `system=` and `user=` role entries (validation rule V7). `assistant=` is optional (few-shot priming).

Role values are string literals and support slot interpolation via `{...}`.

### Interpolation variables

| Variable | Expands to |
|---|---|
| `{prose}` | Original NL input from `RunInput.Prose` |
| `{verb}` | Classified verb |
| `{cortex.bundle}` | Formatted cortex context bundle |
| `{slots}` | Summary of all current slot values |
| `{unknowns}` | Summary of registered unknowns |
| `{slot.NAME}` | Value of a specific slot |
| `{slot.NAME.prose}` | Raw prose text for a specific slot (pre-resolution) |

Unknown variable names are preserved as-is (e.g. `{foo}` → `{foo}`) for debugging visibility.

---

## Resolve statements

Mandatory pre-resolution (D13): filling a slot from the actor's cortex before the user signs.

```mtx
resolve slot.target <- cortex.find(type="ArtifactRef", near=slot.target.prose)
resolve slot.user_prefs <- cortex.context(verb=slot.verb, actor=slot.actor)
```

The three cortex functions:

| Function | What it does |
|---|---|
| `cortex.find(key=val, ...)` | Typed predicate lookup — finds memories matching the criteria |
| `cortex.resolve(expr)` | Exact resolution — resolves a NL expression to a URI |
| `cortex.context(key=val, ...)` | Cold-start bundle — returns a formatted string of relevant memories |

Arguments can be named (`near=slot.target.prose`) or positional. Slot expressions (`slot.target.prose`) resolve against current slot state at execution time.

If the slot is already resolved, the statement is a no-op.

---

## Unknown blocks

Declares that a slot is missing or ambiguous and registers a typed gap:

```mtx
unknown slot.target
  severity=blocking
  reason="Cannot proceed without knowing which document to operate on"
  options=[README CHANGELOG spec]
end
```

Modifiers:

| Key | Values | Meaning |
|---|---|---|
| `severity` | `blocking` / `preferred` / `optional` | `blocking` stops execution; others are best-effort fills |
| `reason` | string | Human-readable explanation |
| `default` | value | Fallback fill if the user doesn't answer |
| `options` | `[item1 item2]` or `slot.expr` | Suggested choices |

If the named slot is already resolved (from a `resolve` statement or pre-fill), the unknown block is silently skipped.

---

## Clarify blocks

Generates a structured question for the user (produces `intent.clarify`):

```mtx
clarify slot.deadline
  prompt="When do you need this done by?"
  type=iso8601
  required=false
  default="next friday"
  options=[today tomorrow "next week" "next friday"]
end
```

Modifiers:

| Key | Meaning |
|---|---|
| `prompt` | Question text shown to the user |
| `type` | Expected answer type |
| `required` | Whether the answer is mandatory |
| `default` | Suggested default |
| `options` | Suggested options |

---

## Failure mode entries

Declared in `§FAILURE_MODES`. Each is a named failure scenario with action and reason:

```mtx
§FAILURE_MODES

target_not_found
  action=gate
  reason=unknown_information
  suggest=amend_constraint

budget_exceeded
  action=fail
  reason=out_of_budget
  suggest=raise_budget
```

### Failure actions
| Action | Meaning |
|---|---|
| `fail` | Immediately emit `intent.fail` |
| `retry` | Re-run the failing step |
| `gate` | Pause and emit `policy.gate` for human review |

### Failure reasons (closed set)
`unknown_information`, `policy_violation`, `out_of_budget`, `out_of_scope`, `ambiguous_request`, `tool_failure`, `external_failure`, `timeout`, `cancelled_by_user`, `correction_invalid`

### Suggest actions
`raise_budget`, `extend_deadline`, `amend_constraint`, `delegate`, `abandon`

---

## On-block metadata KV pairs

Two KV keys inside `on` blocks have special meaning to the executor:

### `kind`

Routes the synthesized step to a specialist model (Session 31b model router):

```mtx
on verb=build
  kind="code"
  ...
end
```

Valid values (closed — validated by V11):

| Kind | Model tier | When to use |
|---|---|---|
| `reason` | Default GLM-5.1 | General agentic step (default if absent) |
| `code` | Code specialist | Code generation |
| `summarize` | Long-context specialist | Summarization of long inputs |
| `write` | Prose specialist | Free-form writing |
| `transform` | Structured I/O | Deterministic format conversions |
| `classify` | Grammar-constrained | Classification / pick-from-list |
| `hard_reason` | Frontier reasoning | Expensive step requiring deep reasoning |

### `output_cardinality`

Tells the planner this on-block produces N independent outputs (Session 31c):

```mtx
on verb=build
  output_cardinality=3
  ...
end
```

The planner folds this into a single multi-output step or a `parallel{}` fan-out instead of N sequential nodes. Must be a strictly positive integer. Absent = planner decides.

---

## URI reference entries

In `§TOOLS` and `§SUB_SKILLS`, bare URIs on their own line declare dependencies:

```mtx
§TOOLS
matrix://tool/mcp/paxeer/paxeer__chain_info@0.1.0
matrix://tool/mcp/tachyon/tachyon_compile@0.1.0
```

Validation rules V9 and V10 require these to be version-pinned (`@semver` or `@sha256:...`). `@latest` is rejected.

---

## None entries

```mtx
§SUB_SKILLS
none
```

`none` is the explicit way to say "this section is intentionally empty." Without it, an empty section is also valid, but `none` documents intent.

---

## Canonical hashing (D11)

Every `.mtx` file has a deterministic AST hash computed by `MCL/mtx/canonical/`. This is the `mtx_digest` that feeds into the D11 compiler seed.

What is included:
- All section names
- All entries (KV pairs, slot declarations, on-blocks, prompt blocks, resolve/unknown/clarify, fail entries, URI refs)
- String literal content (with interpolation preserved as raw text)
- Block structure and condition expressions
- Type annotations
- Slot modifier content

What is excluded:
- Comments
- Blank lines
- Whitespace variations within values
- The `§HASH` section itself

This means you can freely add or reformat comments and blank lines without breaking the seed. Moving an on-block or changing a prompt string does break it — that's intentional.

The hash is computed as `sha256(canonical_bytes)` where `canonical_bytes` is a normalized byte sequence produced by `canonical.Bytes()`. Inspect it with `mclc hash <path>`.

---

## The `§HASH` section

At publish time, `mclc` writes a `§HASH` section at the bottom of the file:

```mtx
§HASH
digest=sha256:abcdef1234...
published_at=2026-01-01T00:00:00Z
```

This section is excluded from the hash computation (you can't hash something that includes its own hash). The `mcl-validate` tool checks that the stored digest matches the recomputed one. A manually-edited digest that doesn't match is rejected.

---

## Required sections in SKILL.mtx

A `SKILL.mtx` file must contain exactly these 8 sections (validation rule V1):

| Section | Purpose |
|---|---|
| `§SKILL` | Metadata: name, version, `mcl.verbs`, description |
| `§INPUTS` | Typed slot declarations |
| `§CORTEX` | What memory types this skill reads from cortex |
| `§TOOLS` | Version-pinned tool URIs |
| `§SUB_SKILLS` | Version-pinned sub-skill URIs |
| `§PROCEDURE` | On-blocks that run the compile-time procedure |
| `§OUTPUTS` | What the skill produces |
| `§FAILURE_MODES` | Named failure scenarios |

`§HASH` is optional (added by tooling, not hand-written).

---

## Validation rules reference

The validator (`MCL/mtx/validator/`) enforces 12 semantic rules on top of the parser's syntactic check:

| Rule | What it checks |
|---|---|
| V1 | `SKILL.mtx` contains all 8 required sections |
| V2 | `mcl.verbs` values are D7 closed set or `x:` prefixed |
| V3 | Enum literal values are members of the declared type (planned, not yet enforced) |
| V4 | Slot names are unique within a section |
| V5 | Every `resolve` names a slot declared in `§INPUTS` |
| V6 | Every `unknown` block names a slot declared in `§INPUTS` |
| V7 | Prompt blocks inside on-blocks have both `system=` and `user=` |
| V8 | `§FAILURE_MODES` `reason=` values are in the closed set |
| V9 | `§SUB_SKILLS` URIs are version-pinned |
| V10 | `§TOOLS` URIs are version-pinned |
| V11 | On-block `kind=` values are in the closed `StepKindNames` set |
| V12 | On-block `output_cardinality=` is a strictly positive integer literal |

V1–V8 apply to `SKILL.mtx` files. V4, V7, V11, V12 also apply to core `.mtx` files (run via `ValidateCore`).
