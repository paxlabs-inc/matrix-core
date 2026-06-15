# Writing a SKILL.mtx

A skill is the unit of compile-time knowledge in MCL. It tells the compiler what kind of NL requests it handles, what it needs to know, how to extract a typed Frame from them, and what can go wrong.

Each skill lives at `skills/<slug>/SKILL.mtx`. The compiler selects the skill that best matches the verb and the user's intent, then runs the skill's `§PROCEDURE` to produce the Frame.

---

## Required sections

Every `SKILL.mtx` must have exactly these 8 sections (validator rule V1):

```
§SKILL
§INPUTS
§CORTEX
§TOOLS
§SUB_SKILLS
§PROCEDURE
§OUTPUTS
§FAILURE_MODES
```

`§HASH` is optional and added by tooling. Don't write it by hand.

---

## A minimal working example

```mtx
§SKILL
name="Writing Plans"
version=1.0.0
mcl.verbs=build modify
description="Creates or updates a structured plan document"

§INPUTS
slot target: ArtifactRef
  required
  hint="The plan document to create or update"

slot goal: string
  required
  hint="What the plan should achieve"

slot deadline: iso8601
  optional
  hint="Target completion date"

§CORTEX
reads=Goal Preference Pattern Fact

§TOOLS
matrix://tool/mcp/filesystem/fs_write@0.1.0
matrix://tool/mcp/filesystem/fs_read@0.1.0

§SUB_SKILLS
none

§PROCEDURE

on verb=build
  kind="write"
  prompt
    system="You are an expert technical planner. Extract a structured plan from the user's request.\n\nYou have access to the user's context:\n{cortex.bundle}\n\nExtract these fields as JSON:\n- goal: string (the high-level objective)\n- milestones: string[] (ordered deliverables)\n- constraints: object[] (budget/deadline/scope constraints)\n- success_criteria: object[] (measurable completion criteria)"
    user="Goal: {prose}\n\nTarget: {slot.target}\nDeadline: {slot.deadline}"
  end
  resolve slot.target <- cortex.find(type="ArtifactRef", near=slot.target.prose)
end

on verb=modify
  kind="write"
  prompt
    system="You are an expert technical planner. The user wants to update an existing plan.\n\nContext:\n{cortex.bundle}\n\nExtract the requested changes as JSON."
    user="Modification request: {prose}\n\nExisting plan: {slot.target}"
  end
  resolve slot.target <- cortex.find(type="ArtifactRef", near=slot.target.prose)
end

on unknown
  unknown slot.target
    severity=blocking
    reason="Cannot proceed without knowing which plan to work on"
  end
end

§OUTPUTS
slot result: ArtifactRef
  hint="The created or updated plan document"

§FAILURE_MODES

target_not_found
  action=gate
  reason=unknown_information
  suggest=amend_constraint

scope_too_broad
  action=retry
  reason=ambiguous_request
  suggest=amend_constraint
```

---

## §SKILL metadata

```mtx
§SKILL
name="Human-readable skill name"    # must be double-quoted
version=1.0.0                        # semver
mcl.verbs=build modify deliver       # D7 closed set; space-separated list
description="What this skill does"  # must be double-quoted
```

`mcl.verbs` controls which verb classifications this skill handles. The compiler's skill router uses this to narrow the candidate set. An intent classified as `verb=find` will never dispatch to a skill that only declares `build modify`.

---

## §INPUTS

Declares the typed slots the compiler needs to fill:

```mtx
§INPUTS
slot target: ArtifactRef
  required
  hint="The document or system to operate on"

slot amount: AssetAmount
  optional
  default=100.00

slot format: enum<pdf|markdown|html>
  optional
  default=markdown
  hint="Output format"
```

Always double-quote `hint=` values. Unquoted, the lexer treats the words as a space-separated ident list and the value is wrong.

Slots declared here are what `resolve` and `unknown` blocks can reference. Referencing a slot name not declared here is a V5/V6 validation error.

---

## §CORTEX

Declares which cortex memory types this skill reads for its context bundle:

```mtx
§CORTEX
reads=Goal Preference Constraint Fact Pattern
```

This is a space-separated list of cortex memory type names. The stage 3 cortex pre-fetch uses this (combined with the verb routing table in `core/verb.mtx`) to build the context bundle fed into the skill's prompt.

---

## §TOOLS

Lists the version-pinned tools the skill's plan steps may call. Must be version-pinned (V10):

```mtx
§TOOLS
matrix://tool/mcp/filesystem/fs_write@0.1.0
matrix://tool/mcp/filesystem/fs_read@0.1.0
matrix://tool/mcp/tachyon/tachyon_compile@0.1.0
```

`@latest` is rejected. If a tool is undeclared here but a plan step tries to call it, the executor's allowlist check fails.

Use `none` if the skill doesn't call any tools:

```mtx
§TOOLS
none
```

---

## §SUB_SKILLS

Lists sub-skills this skill may dispatch to. Same rules as §TOOLS:

```mtx
§SUB_SKILLS
matrix://skill/code-review@1.0.0
matrix://skill/writing-plans@2.1.0
```

Use `none` if there are no sub-skills.

---

## §PROCEDURE

This is the main event. One or more `on`-blocks that define what the compiler does for each verb or condition.

### Top-to-bottom, first-match-wins

The interpreter walks on-blocks from top to bottom and executes the first one whose condition matches. Order matters.

### Standard pattern: verb-branch per verb + unknown fallback

```mtx
§PROCEDURE

on verb=build
  prompt
    system="..."
    user="..."
  end
  resolve slot.target <- cortex.find(type="ArtifactRef", near=slot.target.prose)
end

on verb=modify
  prompt
    system="..."
    user="..."
  end
  resolve slot.target <- cortex.find(type="ArtifactRef", near=slot.target.prose)
end

on unknown
  unknown slot.target
    severity=blocking
    reason="Tell me what you want to work on."
  end
end
```

### Prompts

The prompt block is what drives stage 4. It gets interpolated and sent to the LLM with the grammar constraint.

```mtx
on verb=build
  kind="write"
  prompt
    system="You are a plan writer. Extract a structured plan.\n\nContext: {cortex.bundle}"
    user="User goal: {prose}\n\nDeadline: {slot.deadline}"
  end
end
```

Keep prompts focused and concrete. The grammar constraint (`intent_frame@1`) already shapes the output — the prompt's job is to focus the model on the right fields. Over-prompting tends to hurt more than help.

### Resolving entity references (D13)

Every NL entity reference in the user's input must be resolved to a `matrix://` URI before the user signs. This happens via `resolve` statements:

```mtx
resolve slot.target <- cortex.find(type="ArtifactRef", near=slot.target.prose)
```

`cortex.find` is the most common: it does a typed predicate lookup in the actor's cortex. The `near=` argument specifies the NL text to match semantically.

`cortex.resolve` is for exact resolution when you know the entity name:
```mtx
resolve slot.project <- cortex.resolve(slot.project.prose)
```

`cortex.context` is for fetching a full context bundle into a slot (useful when a slot holds context rather than a specific entity):
```mtx
resolve slot.user_context <- cortex.context(verb=slot.verb)
```

### Declaring gaps

When a slot is unavailable and resolution fails, declare it as a gap:

```mtx
on unknown
  unknown slot.target
    severity=blocking
    reason="I need to know which document to work on."
    options=[README CHANGELOG spec ARCHITECTURE]
  end
  unknown slot.deadline
    severity=optional
    reason="A deadline would help prioritize the plan."
    default="next sprint"
  end
end
```

`blocking` severity stops execution. `preferred` and `optional` are advisory — the intent can proceed without them, but a clarify question is still generated.

### Confidence branching

Handle low-confidence cases explicitly:

```mtx
on confidence<0.75
  prompt
    system="The user's intent is ambiguous. Ask exactly one clarifying question."
    user="Original: {prose}\n\nWhat is unclear?"
  end
  clarify slot.target
    prompt="Which document or system are you referring to?"
    type=ArtifactRef
    required=true
  end
end
```

### Slot value branching

Branch on a specific slot value:

```mtx
on slot.format=pdf
  prompt
    system="Generate a PDF-formatted plan structure."
    user="{prose}"
  end
end
```

### Step kind annotations

Add `kind=` inside the on-block to route the executor's plan step to the right model tier:

```mtx
on verb=build
  kind="write"     # prose specialist model
  ...
end

on verb=analyze
  kind="reason"    # default reasoning model
  ...
end
```

This is metadata for the executor — it doesn't affect compile-time behaviour. Leave it out if you're not sure; `"reason"` is the default.

### Output cardinality hints

When the skill naturally produces N independent outputs (e.g. "write 5 blog post titles"), annotate it:

```mtx
on verb=build
  output_cardinality=5
  ...
end
```

The planner folds this into a single multi-output step or a `parallel{}` fan-out. Must be a strictly positive integer. Validator rule V12 rejects zero, negatives, or non-integer values.

---

## §OUTPUTS

Declares what the skill produces:

```mtx
§OUTPUTS
slot result: ArtifactRef
  hint="The created document"

slot summary: string
  hint="A one-paragraph summary of what was done"
```

Output slots are what plan steps write to. The executor validates that the declared expected outputs were produced.

---

## §FAILURE_MODES

Named failure scenarios with action and reason:

```mtx
§FAILURE_MODES

cannot_locate_target
  action=gate
  reason=unknown_information
  suggest=amend_constraint

budget_exceeded
  action=fail
  reason=out_of_budget
  suggest=raise_budget

ambiguous_scope
  action=retry
  reason=ambiguous_request
  suggest=amend_constraint
```

The `reason=` value must be in the closed set (V8): `unknown_information`, `policy_violation`, `out_of_budget`, `out_of_scope`, `ambiguous_request`, `tool_failure`, `external_failure`, `timeout`, `cancelled_by_user`, `correction_invalid`.

---

## Validation

```bash
# Validate a SKILL.mtx
mclc validate skills/my-skill/SKILL.mtx

# Compute the canonical hash
mclc hash skills/my-skill/SKILL.mtx

# Check what the compiled prompt would look like without calling the LLM
mclc compile \
  -skill skills/my-skill/SKILL.mtx \
  -prose "Build a deployment plan for my Node.js app" \
  -verb build \
  -dry-run
```

Validation is strict about a few things that are easy to get wrong:

1. **String values for `hint=`, `reason=`, `prompt=`, `description=` must be double-quoted.** The lexer invariant means unquoted text parses as a space-separated ident list, not a string. The validator doesn't always catch this directly, but the interpreter will silently get the wrong value.

2. **`§TOOLS` and `§SUB_SKILLS` URIs must be version-pinned.** `@latest` is rejected by V9/V10. Pin to `@semver` or `@sha256:...`.

3. **Prompt blocks inside on-blocks must have both `system=` and `user=`.** Missing either triggers V7.

4. **Slot names in `resolve` and `unknown` must be declared in `§INPUTS`.** V5 and V6.

5. **`kind=` values must be in the closed set.** V11.

---

## Testing a skill

The simplest way to test whether a skill parses and validates:

```bash
mclc validate skills/my-skill/SKILL.mtx
```

To see what prompts get generated without an API key:

```bash
mclc compile -skill skills/my-skill/SKILL.mtx \
  -prose "your test input here" \
  -verb build \
  -dry-run
```

Output is a JSON object with `prompt_messages`, `slots`, `unknowns`, and `clarify_questions`. The `prompt_messages` show the exact interpolated text that would be sent to the LLM.

With an API key set:

```bash
FIREWORKS_API_KEY=your_key mclc compile \
  -skill skills/my-skill/SKILL.mtx \
  -prose "Build a deployment pipeline for my Node.js app" \
  -verb build
```

This runs the full pipeline and outputs the `frame_json` the LLM produced.
