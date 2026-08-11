# MCL Developer Documentation

MCL is the compiler and protocol backbone of Centra AI. Every user interaction starts here: natural language goes in, a typed signed Intent IR comes out, and the executor picks it up from there.

This documentation is written for people working on MCL itself — extending the language, adding skills, touching the pipeline, or understanding how the wire protocol works.

---

## Contents

| Document | What it covers |
|---|---|
| [CentraScript Language](./centrascript.md) | The `.mtx` DSL — syntax, constructs, grammar, all the details |
| [Compiler Pipeline](./compiler-pipeline.md) | The 6-stage pipeline from `intent.draft` to `intent.compiled` |
| [Intent IR](./intent-ir.md) | The `ir` package — Intent, Frame, PlanTree, all the Go types |
| [Envelope & Wire Protocol](./envelope.md) | The 15 message kinds, CBOR encoding, ed25519 signing |
| [LLM Client](./llm-client.md) | Provider abstraction, grammar-constrained decoding, seeding |
| [Writing a SKILL.mtx](./skill-authoring.md) | Practical guide to authoring and validating skill files |
| [mclc CLI Reference](./mclc-cli.md) | `compile`, `validate`, `hash`, `parse` commands |

---

## Repository layout

```
MCL/
├── cmd/mclc/           standalone compiler CLI (Go)
├── core/               compiler-core .mtx modules
│   ├── pipeline.mtx    6-stage pipeline wiring
│   ├── verb.mtx        D7 closed vocabulary + classifier prompt
│   ├── frame.mtx       Frame type schema
│   └── confidence.mtx  confidence scoring formula
├── envelope/           wire codec: 15 message kinds, CBOR, ed25519
├── ir/                 Intent IR Go types + PlanTree
│   ├── intent.go       Intent, Frame, Unknown, Budget, CompileMetadata
│   ├── plan.go         PlanTree, PlanNode, StepPayload, ToolCallPayload
│   ├── encode.go       canonical JSON helpers
│   └── plan_validate.go plan validation rules
├── llm/                LLM client: Together, Fireworks, grammar constraints
└── mtx/                CentraScript runtime
    ├── grammar.bnf     formal EBNF
    ├── spec.md         language specification
    ├── token/          token types
    ├── lexer/          scanner
    ├── parser/         recursive-descent parser → AST
    ├── ast/            AST node types
    ├── validator/      semantic validation (12 rules)
    ├── canonical/      deterministic AST hash (D11)
    └── interpreter/    AST walker + LLM/Cortex interface
```

---

## The one-sentence contract

MCL takes `intent.draft` (prose + optional slot pre-fills) and produces `intent.compiled` (a fully-typed, signed, deterministically-hashed Intent IR). Everything downstream — executors, walkers, auditors, replays — operates on the IR, never on prose again.

That is the whole point. It's the boundary between the natural-language world and the executable world.

---

## Key locked decisions

These decisions are frozen. Don't re-litigate them without an explicit protocol version bump.

| ID | Decision |
|---|---|
| **D7** | Closed verb vocabulary — exactly 10 verbs (`find`, `acquire`, `build`, `modify`, `deliver`, `analyze`, `negotiate`, `schedule`, `monitor`, `delegate`) plus `x:` extension namespace |
| **D8** | Typed `SlotPatch` compiles to RFC 6902 JSON Patch on the wire (`intent.answer`, `intent.correct`) |
| **D9** | Plan-diff materiality classifier (`materiality/`) determines whether a correction triggers a new plan |
| **D11** | Compiler determinism: `seed = sha256(intent.id || actor || snapshot_hash || mtx_digest || model_digest)`. Same inputs always produce byte-identical output |
| **D13** | Mandatory pre-resolution: all NL entity references resolved to `matrix://` URIs before user sign-off. Unresolvable → `Unknown` |
| **D18** | Compiler/executor split: compiler = small seedable grammar-constrained model. Executor = frontier model. They are never the same slot |
| **A9** | Compiler model slot must be seedable and grammar-constrained |
