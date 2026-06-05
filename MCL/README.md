# MCL — Matrix Communication Layer

The heart of Matrix. Every interaction passes through here.

**Cortex** is the brain — persistent typed memory.
**MCL** is the heart — every user intent pumps through it, gets typed, gets signed, and is handed to the executor.

---

## What MCL does

1. User types natural language → `intent.draft`
2. MCL **compiler** (small seedable grammar-constrained LLM) converts it → typed `Intent IR`
3. User reviews + signs the IR → `intent.accept`
4. Executor (main frontier LLM) walks the plan inside skills
5. Completion → `intent.attest` (signed, optionally chain-anchored)

No free-form side channels. No prose-only messages. Every input produces a typed artifact.

---

## Structure

```
MCL/
  README.md               # this file
  mtx/                    # MatrixScript runtime
    spec.md               # language specification
    grammar.bnf           # formal EBNF
    lexer/                # Go: tokeniser
    parser/               # Go: .mtx → AST
    ast/                  # Go: AST node types
    validator/            # Go: type-check .mtx against core grammar
    interpreter/          # Go: walks AST + invokes LLM at prompt nodes
    canonical/            # Go: AST → deterministic bytes (D11 hash input)
  core/                   # compiler-core .mtx modules (the framework)
    verb.mtx              # closed verb vocab + classifier rules (D7)
    frame.mtx             # Frame type: objects/constraints/criteria/prefs
    constraint.mtx        # Constraint type set (closed + x: namespace)
    predicate.mtx         # success_criteria predicate types
    unknown.mtx           # gap severity + gap typing rules
    pre_resolve.mtx       # NL ref → matrix:// URI rules (D13)
    confidence.mtx        # confidence scoring formula
    pipeline.mtx          # stage wiring (the 6-stage compiler pipeline)
  ir/                     # Intent IR: Go types + canonical CBOR codec
  envelope/               # MCL message envelope: sign / verify (ed25519)
  patch/                  # D8: typed SlotPatch ↔ RFC 6902
  materiality/            # D9: plan-diff materiality classifier (§18.1)
  uri/                    # matrix:// URI typed parser
  cmd/
    mclc/                 # standalone compiler CLI
    mcl-validate/         # validate any .mtx file against core grammar
    mcl-fmt/              # canonical JSON debug mirror of .mtx
```

## The language: MatrixScript (`.mtx`)

The compiler is meta-programmed. Compiler logic, skill procedures, and the IR grammar itself are written in **MatrixScript** — a Matrix-native declarative DSL. The Go runtime in `mtx/` interprets `.mtx` files; it does not contain compile logic.

- **Syntax**: `§SECTION` headers + `key=value` pairs (extended from `.kvx` DNA)
- **Semantics**: pure data — decision trees are literal data structures, not code
- **Prompts**: structured typed blocks (`prompt { system="..." user="..." }`)
- **Hashing**: AST-hashed (D11 determinism — comments don't break the seed)
- **Skills**: each skill's entire definition lives in `skills/<slug>/SKILL.mtx`

See `mtx/spec.md` for the full language reference.

---

## Key locked decisions

| Decision | What | Where |
|---|---|---|
| D7 | Closed verb vocab — 10 verbs + `x:` extension | `core/verb.mtx` |
| D8 | Typed SlotPatch → RFC 6902 on wire | `patch/` |
| D9 | Materiality algorithm (§18.1) | `materiality/` |
| D11 | Compiler determinism — seed = sha256(intent.id \|\| actor \|\| snapshot_hash \|\| mtx_digest) | `mtx/canonical/` |
| D13 | Pre-resolution mandatory before user sign-off | `core/pre_resolve.mtx` |
| D18 | Compiler/executor split — compiler = small seedable grammar-constrained | `cmd/mclc/` |
| A9 | Compiler slot must be seedable + grammar_constrained | enforced in `cmd/mclc/` |
