# MatrixScript Language Specification

**File extension:** `.mtx`
**Full name:** MatrixScript
**Version:** 0.1
**Status:** Canonical — implementation must conform.

---

## 1. What MatrixScript is

MatrixScript is the declarative language in which the MCL compiler is written.

The Go binary `mclc` is a runtime that interprets `.mtx` files. It contains no compile logic itself. Every decision the compiler makes — which verb to classify, how to extract Frame slots, how to phrase clarify questions, how to score confidence — is declared in a `.mtx` file that the runtime executes.

This means:
- Changing compiler behaviour = editing a `.mtx` file, not Go code.
- A skill's compile-time procedure is a `.mtx` file (`skills/<slug>/SKILL.mtx`).
- The compiler's own grammar, verb rules, and pipeline wiring are `.mtx` files (`MCL/core/`).
- All `.mtx` files are content-addressable (AST-hashed) and their digests flow into the D11 seed.

---

## 2. Design principles

| Principle | Choice |
|---|---|
| Syntax family | `.kvx`-evolved — dense `key=value` + `§SECTION` headers |
| Semantic model | Pure data — parsed AST; decision trees are literal structures, not code |
| Prompts | Structured typed blocks with named roles |
| Hashing | AST-hash (comments don't break seed determinism) |
| Skill files | `SKILL.mtx` carries everything — frontmatter, procedure, outputs, failure modes |

---

## 3. Lexical conventions

### 3.1 Encoding

UTF-8. Files normalised to NFC before parse.

### 3.2 Line endings

LF (`\n`). CRLF is normalised to LF on read.

### 3.3 Comments

```mtx
# This is a comment. Everything after # to end of line is ignored at runtime.
# Comments ARE preserved in the source AST for display/diff purposes.
# Comments are STRIPPED before canonical hashing (D11).
```

### 3.4 Blank lines

Allowed anywhere. Ignored by parser.

### 3.5 Indentation

Indentation (2 spaces) is used inside block bodies and for slot modifiers. The parser is indentation-aware: a line at column 0 is a top-level entry; a line at column ≥2 is a continuation of the preceding entry.

### 3.6 String literals

Double-quoted: `"value"`. Escape sequences: `\"`, `\\`, `\n`, `\t`.

Slot interpolation is allowed inside strings (see §8): `"user wants to {slot.target}"`.

### 3.7 URI literals

`matrix://` URIs are first-class values — no quoting needed:

```mtx
ref=matrix://skill/writing-plans@1.0.0
```

### 3.8 Identifiers

`[a-zA-Z][a-zA-Z0-9_-]*` — letters, digits, underscores, hyphens. Case-sensitive.

Section names are UPPER_CASE identifiers.

---

## 4. File structure

A `.mtx` file is a sequence of sections:

```
§SECTION_NAME
  entry
  entry
  ...

§NEXT_SECTION
  ...
```

### 4.1 Section header

```mtx
§SKILL
§INPUTS
§PROCEDURE
```

`§` followed immediately by an UPPER_CASE identifier on its own line. No trailing content.

### 4.2 Top-level entry types

Within a section, valid entries are:

| Entry type | Syntax | Used in |
|---|---|---|
| Key-value pair | `key=value` | all sections |
| Typed slot | `slot name: Type` + modifiers | §INPUTS, §OUTPUTS |
| `on`-block | `on condition` ... `end` | §PROCEDURE |
| `prompt` block | `prompt` ... `end` | §PROCEDURE > on-block |
| `resolve` statement | `resolve slot.x <- cortex.find(...)` | §PROCEDURE > on-block |
| `unknown` block | `unknown slot.x` ... `end` | §PROCEDURE > on-block |
| `clarify` block | `clarify slot.x` ... `end` | §PROCEDURE > on-block |
| Failure entry | `reason_name` + action modifiers | §FAILURE_MODES |
| URI reference | bare `matrix://...` | §TOOLS, §SUB_SKILLS |
| `none` | literal keyword | §TOOLS, §SUB_SKILLS |

---

## 5. Type system

### 5.1 Scalar types

| Type | Description |
|---|---|
| `string` | UTF-8 string |
| `uint` | non-negative integer |
| `float` | IEEE 754 32-bit float |
| `bool` | `true` \| `false` |
| `ulid` | ULID (26-char Crockford base32) |
| `iso8601` | ISO 8601 datetime string |
| `did` | Decentralised Identifier string |
| `sha256` | 64-hex SHA-256 digest |

### 5.2 MCL domain types

| Type | Description |
|---|---|
| `ArtifactRef` | Reference to a produced or consumed artifact |
| `Constraint` | Typed constraint (budget/deadline/quality/rule/policy/x:custom) |
| `Predicate` | Success criterion |
| `Unknown` | Typed gap (severity + field path + options) |
| `PlanDraft` | Structured plan tree (sequence of steps) |
| `SkillRef` | `matrix://skill/...` reference |
| `ToolRef` | `matrix://tool/...` reference |
| `CortexRef` | `matrix://cortex/...` reference |
| `SlotPath` | Dot-path into the Intent IR (e.g. `frame.constraints[0].max`) |
| `AssetAmount` | `{ asset: string, amount: float }` |
| `AgentRef` | `matrix://agent/...` |
| `UserRef` | `matrix://user/...` |

### 5.3 Enum types

```mtx
enum<formal|casual|technical>
enum<blocking|preferred|optional>
```

Closed set. Parser rejects any value not in the declared set.

### 5.4 List types

Any type suffixed with `[]`:

```mtx
slot constraints: Constraint[]  optional
slot tags:        string[]       optional
```

### 5.5 Optional vs required

Slots default to `required`. Add `optional` modifier to override.

---

## 6. Section reference

### 6.1 §SKILL — metadata

```mtx
§SKILL
id=writing-plans
version=1.0.0
display=Writing Plans
author=did:pax:0xABC123...
description=Converts a build/modify intent draft into a structured, executable plan
mcl.verbs=build modify delegate
determinism=seedable
seed_policy=per_intent
```

| Field | Type | Required | Notes |
|---|---|---|---|
| `id` | string | yes | slug, matches directory name |
| `version` | string | yes | semver |
| `display` | string | yes | human label |
| `author` | did | yes | signing key |
| `description` | string | yes | ≤ 280 chars |
| `mcl.verbs` | space-separated verb names | yes | subset of D7 closed vocab |
| `determinism` | `seedable` \| `best_effort` | yes | compiler skills must be `seedable` |
| `seed_policy` | `per_intent` \| `per_session` \| `per_actor` | no | default `per_intent` |

### 6.2 §INPUTS — slot declarations

```mtx
§INPUTS
slot target: ArtifactRef
  required
  hint="The artifact to build or modify"

slot style: enum<formal|casual|technical>
  optional
  default=formal

slot constraints: Constraint[]
  optional
```

Each `slot` declaration takes:
- `required` or `optional` (indented modifier)
- `default=<value>` — fill value when absent (only valid on `optional` slots)
- `hint="..."` — human-readable description shown in clarify UI
- `max=<n>` — max list length for `Type[]` slots

### 6.3 §CORTEX — memory access declaration

```mtx
§CORTEX
reads=Preference Goal Constraint Event Fact Pattern
tags=writing planning
```

| Field | Notes |
|---|---|
| `reads` | Space-separated cortex memory types the skill may read. Enforced at API boundary (S2). |
| `tags` | Space-separated tags used for cortex affinity scoring (§3.2 skill selection). |

### 6.4 §TOOLS

```mtx
§TOOLS
none
```

or:

```mtx
§TOOLS
matrix://tool/registry/query@2.0
matrix://tool/payments/stream@1.0
```

Bare `matrix://tool/...` URIs, one per line, version-pinned. `none` if the skill uses no tools.

### 6.5 §SUB_SKILLS

```mtx
§SUB_SKILLS
matrix://skill/executing-plans@1.0.0
```

or `none`.

### 6.6 §PROCEDURE — decision tree

The compile-time procedure. An ordered set of `on`-blocks. The runtime evaluates conditions top-to-bottom; the first matching block runs.

```mtx
§PROCEDURE
on verb=build
  prompt
    system="You are the Matrix plan compiler. Fill the Frame for a 'build' intent."
    user="User goal: {prose}. Context: {cortex.bundle}. Known slots: {slots}. Fill the Frame JSON."
  end
  resolve slot.target <- cortex.find(type=Fact, near=slot.target.prose, limit=5)
  unknown slot.target
    severity=blocking
    reason="Cannot identify the target artifact from the user's phrasing"
    options=cortex.bundle.reachable_uris
  end
end

on verb=modify
  prompt
    system="You are the Matrix plan compiler. Fill the Frame for a 'modify' intent."
    user="User wants to modify: {prose}. Current state: {slot.target}. Fill the Frame JSON."
  end
  resolve slot.target <- cortex.resolve(slot.target.prose)
  unknown slot.target
    severity=blocking
    reason="Need to identify what is being modified"
  end
end

on confidence<0.75
  clarify slot.target
    prompt="Which artifact are you referring to?"
    type=ArtifactRef
    required=true
  end
end
```

See §7 for full `on`-block reference.

### 6.7 §OUTPUTS — produced slots

```mtx
§OUTPUTS
slot plan_draft: PlanDraft
  required

slot alternatives: SkillRef[]
  optional
  max=3
  hint="Alternative skills the user could choose instead"

slot unknowns: Unknown[]
  optional
```

Same slot declaration syntax as §INPUTS.

### 6.8 §FAILURE_MODES

```mtx
§FAILURE_MODES
budget_exceeded
  suggest=raise_budget

policy_violation
  action=fail
  reason=policy_violation

tool_missing
  action=fail
  reason=tool_failure
```

Each entry: a failure mode name (free identifier) followed by indented action modifiers.

| Modifier | Values |
|---|---|
| `action` | `fail` \| `retry` \| `gate` |
| `reason` | Any `FailureReason` (see §9) |
| `suggest` | `raise_budget` \| `extend_deadline` \| `amend_constraint` \| `delegate` \| `abandon` |

### 6.9 §HASH — content hash (compiler-managed)

```mtx
§HASH
v=1
algo=sha256_ast
digest=<hex — written by mtxc at publish, never manually>
```

Never manually authored. `mtxc publish` computes and writes this section. `mtxc validate` verifies it.

---

## 7. Block syntax reference

### 7.1 `on`-block

```mtx
on <condition>
  <on-entries>
end
```

**Conditions:**

| Condition syntax | Meaning |
|---|---|
| `verb=<name>` | Frame verb matches this D7 verb |
| `confidence<0.75` | Overall confidence below threshold |
| `confidence>=0.85` | Confidence above threshold |
| `slot.<name>=<value>` | Named slot equals a specific value |
| `unknown` | Any blocking unknown exists |

`on`-blocks are evaluated in declaration order. First match wins.

**On-entries** (inside an `on`-block):

- `prompt` block
- `resolve` statement
- `unknown` block
- `clarify` block
- Nested `on`-block
- `kv_pair` (metadata — see below)

**On-block metadata KVPairs.** Three recognized keys:

| Key | Value | Effect |
|---|---|---|
| `kind` | string in closed enum (see below) | Routes the synthesized step to a specialist executor model (Session 31b model router). |
| `output_cardinality` | strictly positive integer | Tells the planner this on-block produces N independent items per invocation; planner folds them into one multi-output step or a `parallel{}` fan-out instead of N sequential steps (Session 31c P3c). |
| `skip` | boolean | Legacy sentinel — interpreter takes no action. |

`kind` accepts a quoted string (`kind = "code"`) or a bare identifier
(`kind = code`). Closed value set: `reason` (default), `code`,
`summarize`, `write`, `transform`, `classify`, `hard_reason`.
Validator rule 11 rejects any other value. Skill authors can leave
`kind` unset; the executor routes to the default reason-kind model.

Example:

```mtx
on verb=build
  kind = "code"
  prompt
    system="You are a code-generation assistant…"
    user="{prose}"
  end
end
```

### 7.2 `prompt` block

```mtx
prompt
  system="<system role text>"
  user="<user turn text>"
  assistant="<optional assistant prefix — steers output format>"
end
```

- `system`, `user`, `assistant` are the only valid role names.
- Values are string literals supporting slot interpolation (§8).
- The block defines exactly what the compiler-LLM sees.
- Auditable, diff-able, hashed independently of surrounding `.mtx`.
- Only one `prompt` block per `on`-block.

### 7.3 `resolve` statement

```mtx
resolve slot.<name> <- cortex.find(type=Fact, near=slot.target.prose, limit=5)
resolve slot.<name> <- cortex.resolve(slot.target.prose)
resolve slot.<name> <- cortex.context(verb=slot.verb, budget_tokens=500)
```

Triggers D13 mandatory pre-resolution for the named slot. The runtime calls cortex and pins the result to the slot as a `matrix://cortex/...@v<n>` URI before user sign-off.

Available cortex calls:

| Call | Purpose |
|---|---|
| `cortex.find(type=T, near=expr, limit=n)` | Semantic search over cortex |
| `cortex.resolve(expr)` | Exact resolution by NL hint or partial URI |
| `cortex.context(verb=v, budget_tokens=n)` | Cold-start bundle (3-tier) |

### 7.4 `unknown` block

```mtx
unknown slot.<name>
  severity=<blocking|preferred|optional>
  reason="Human-readable explanation"
  default=<value>
  options=<expr>
end
```

Declares a gap in the compiled IR. Blocking unknowns prevent `intent.accept` until resolved. Preferred unknowns produce clarify questions but do not block. Optional unknowns are surfaced in the UI but do not interrupt flow.

### 7.5 `clarify` block

```mtx
clarify slot.<name>
  prompt="Which artifact are you referring to?"
  type=ArtifactRef
  required=true
  options=[option1 option2 option3]
  default=<value>
end
```

Generates a `Question` in the `intent.clarify` message. The user answers in `intent.answer` which becomes a slot patch.

---

## 8. Slot interpolation

Inside string literals (`"..."`), these interpolation variables are expanded by the runtime before passing to the compiler-LLM:

| Variable | Expands to |
|---|---|
| `{prose}` | Original NL from `intent.draft.prose` |
| `{verb}` | Resolved verb name |
| `{cortex.bundle}` | Formatted `cortex.context()` output (pinned + frame-relevant + outcomes) |
| `{slots}` | Summary of all currently-filled slots |
| `{unknowns}` | Summary of declared unknowns so far |
| `{slot.<name>}` | Current value of the named slot (resolved URI or raw text) |
| `{slot.<name>.prose}` | Raw NL text for a not-yet-resolved slot |

---

## 9. Closed vocabulary tables

### 9.1 Verbs (D7)

```
find  acquire  build  modify  deliver  analyze  negotiate  schedule  monitor  delegate
```

Extension verbs use the `x:` prefix and are not first-class for routing:

```mtx
mcl.verbs=x:brainstorm
```

### 9.2 Failure reasons

```
unknown_information  policy_violation  out_of_budget  out_of_scope
ambiguous_request    tool_failure      external_failure  timeout
cancelled_by_user    correction_invalid
```

### 9.3 Severity levels

```
blocking   preferred   optional
```

### 9.4 Seed policies

```
per_intent   per_session   per_actor
```

---

## 10. Hashing and determinism (D11)

### 10.1 What gets hashed

The **canonical AST hash** (`§HASH.digest`) is computed over:
- All section entries in declaration order
- String literal content (interpolation variables preserved as literals, not expanded)
- Block structure and condition expressions
- Type annotations

**Excluded from hash:**
- Comments (`# ...`)
- Blank lines
- Whitespace within values (values are trimmed before hashing)
- The `§HASH` section itself

### 10.2 How the compiler seed is derived

```
mtx_digest   = sha256(canonical_ast_bytes of SKILL.mtx + all core/*.mtx modules used)
seed         = sha256(intent.id || actor || cortex_snapshot_hash || mtx_digest || model_digest)
```

`compile_metadata` in the emitted `Intent IR` records: `seed`, `mtx_digest`, `model_digest`, `model_version`, `temperature`, per-stage timings. This makes the compilation replay-verifiable.

### 10.3 `mtxfmt` canonicalisation

`mtxfmt` normalises a `.mtx` file to canonical form:
- Consistent 2-space indentation
- Sorted slot modifiers (required < optional < default < hint < max)
- No trailing whitespace
- Single blank line between blocks

`mtxfmt` does not reorder sections or entries (authoring order is intentional).

---

## 11. Validation rules

The `mtx-validate` tool enforces these at parse time:

1. Every `§SKILL` file must contain exactly: `§SKILL`, `§INPUTS`, `§CORTEX`, `§TOOLS`, `§SUB_SKILLS`, `§PROCEDURE`, `§OUTPUTS`, `§FAILURE_MODES`. `§HASH` is optional until publish.
2. `mcl.verbs` entries must all be in the D7 closed set or prefixed with `x:`.
3. Enum literal values must be members of the declared `enum<...>` type.
4. `slot` names must be unique within a section.
5. Every `resolve` statement must name a slot declared in `§INPUTS`.
6. Every `unknown` block must name a slot declared in `§INPUTS`.
7. `prompt` blocks inside `on verb=X` blocks must contain at least `system=` and `user=`.
8. `§FAILURE_MODES` entries must use known `FailureReason` values for the `reason=` modifier.
9. `§SUB_SKILLS` URIs must be version-pinned (`@semver` or `@sha256`). Bare heads rejected (S4).
10. `§TOOLS` URIs must be version-pinned.
11. `on`-block `kind = "<value>"` KVPairs must use a value from the closed
    step-kind enum (`reason`, `code`, `summarize`, `write`, `transform`,
    `classify`, `hard_reason`). The value type must be a quoted string or a
    bare identifier; integers, booleans, lists, and URIs are rejected.
    Empty/absent is allowed (defaults to `reason` at executor time).
    Session 31b model router (matrix.kvx sess#31b).
12. `on`-block `output_cardinality = <int>` KVPairs must be a strictly
    positive integer literal (`1`, `2`, ...). Zero, negative, and non-integer
    types (string, bare identifier, float, boolean, list, URI) are rejected.
    Empty/absent is allowed and leaves the planner free to choose the plan
    shape. Session 31c model router (matrix.kvx sess#31c).

---

## 12. Compiler-core `.mtx` files (`MCL/core/`)

These are not skill files — they define the compiler's own grammar and rules. Same syntax, different sections.

| File | Defines | Section used |
|---|---|---|
| `verb.mtx` | Closed verb vocab + classifier routing | §VERB |
| `frame.mtx` | Frame type schema | §FRAME |
| `constraint.mtx` | Constraint type set | §CONSTRAINT |
| `predicate.mtx` | Success-criteria predicate types | §PREDICATE |
| `unknown.mtx` | Gap typing rules | §UNKNOWN |
| `pre_resolve.mtx` | D13 resolution strategy | §RESOLVE |
| `confidence.mtx` | Confidence scoring formula | §CONFIDENCE |
| `pipeline.mtx` | 6-stage compiler pipeline wiring | §PIPELINE |

These are loaded by `mclc` at startup and hashed into every compilation's `mtx_digest`.

---

## 13. Complete worked example

`skills/writing-plans/SKILL.mtx` — see `MCL/core/` for the reference core files.

```mtx
# Writing Plans — compile-time procedure
# Produces a structured plan_draft from a build/modify/delegate intent

§SKILL
id=writing-plans
version=1.0.0
display=Writing Plans
author=did:pax:0xPLACEHOLDER...
description=Converts a build, modify, or delegate intent into a structured executable plan
mcl.verbs=build modify delegate
determinism=seedable
seed_policy=per_intent

§INPUTS
slot target: ArtifactRef
  required
  hint="The artifact the user wants to create or change"

slot style: enum<formal|casual|technical>
  optional
  default=formal

slot constraints: Constraint[]
  optional

§CORTEX
reads=Preference Goal Constraint Event Fact Pattern
tags=writing planning

§TOOLS
none

§SUB_SKILLS
matrix://skill/executing-plans@1.0.0

§PROCEDURE
on verb=build
  prompt
    system="You are the Matrix plan compiler. Extract the Frame for a 'build' intent as JSON matching the Intent IR schema. Output only valid JSON."
    user="User goal: {prose}\n\nCortex context:\n{cortex.bundle}\n\nCurrently resolved slots:\n{slots}\n\nFill all Frame fields. Use matrix:// URIs for any resolved references."
  end
  resolve slot.target <- cortex.find(type=Fact, near=slot.target.prose, limit=5)
  unknown slot.target
    severity=blocking
    reason="Cannot identify the target artifact — needs explicit reference or selection"
  end
end

on verb=modify
  prompt
    system="You are the Matrix plan compiler. Extract the Frame for a 'modify' intent as JSON matching the Intent IR schema. Output only valid JSON."
    user="User wants to modify: {prose}\n\nCortex context:\n{cortex.bundle}\n\nCurrently resolved slots:\n{slots}\n\nFill all Frame fields."
  end
  resolve slot.target <- cortex.resolve(slot.target.prose)
  unknown slot.target
    severity=blocking
    reason="Need to identify exactly what is being modified"
  end
end

on verb=delegate
  prompt
    system="You are the Matrix plan compiler. Extract the Frame for a 'delegate' intent. Identify the agent to delegate to and the sub-goal."
    user="User wants to delegate: {prose}\n\nCortex context:\n{cortex.bundle}\n\nFill all Frame fields including the target agent reference."
  end
end

on confidence<0.75
  clarify slot.target
    prompt="Which artifact are you referring to?"
    type=ArtifactRef
    required=true
  end
end

§OUTPUTS
slot plan_draft: PlanDraft
  required

slot alternatives: SkillRef[]
  optional
  max=3
  hint="Alternative skills the user could choose instead"

slot unknowns: Unknown[]
  optional

§FAILURE_MODES
budget_exceeded
  suggest=raise_budget

policy_violation
  action=fail
  reason=policy_violation

tool_missing
  action=fail
  reason=tool_failure

§HASH
v=1
algo=sha256_ast
digest=
```

---

## 14. VS Code extension plan

`MCL/mtx/` includes a `vscode/` sub-directory with a TextMate grammar (`.tmLanguage.json`) that forks the `.kvx` grammar and adds:

- `§SECTION` header highlighting (reuses `.kvx` section scope)
- `slot name: Type` declaration scopes
- `on condition` / `end` block pair highlighting
- `prompt` / `end` block pair highlighting
- `resolve`, `unknown`, `clarify`, `end` keyword highlighting
- `enum<...>` type expression highlighting
- Slot interpolation `{slot.name}` inside strings
- `matrix://` URIs (inherited from `.kvx`)
- Closed-verb keyword list colouring (D7: find, acquire, build, ...)
- Severity/action keyword colouring

The icon theme entry for `.mtx` files is added alongside `.kvx` in `MCL/mtx/vscode/package.json`.
