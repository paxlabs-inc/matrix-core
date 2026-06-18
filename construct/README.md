# Construct

**The agent-to-human surface projection primitive.**

The Construct is the layer that renders raw agent-world-state into a finite,
trusted, bidirectional set of surfaces a browser-bound human can actually read
and act on. An agent is a server-side resident with full-duplex perception and
action; the human is a thin browser-side transient. "Chat" (token streams over
SSE/WebSocket) is the only mature channel between them, and everything richer
than appending text to a transcript is opaque or buggy. The Construct closes
that gap.

> **The keystone:** the human authors only the *alphabet* (a finite, trusted set
> of surface primitives and how each renders); the *agent* does the projection
> (chooses which primitive and how to fill it). The agent fills trusted
> primitives — it never emits arbitrary UI. Expressiveness from the agent, safety
> from the fixed renderers.

## The alphabet (8 primitives)

`Narration` · `Metric` · `Entity` · `Structure` · `Stream` · `Timeline` ·
`Canvas` · `Ask` — decorated by 5 attributes (`stakes`, `ref`, `confidence`,
`cost`, `temporality`). See the frozen spec for the full derivation and the
coverage map.

## Source of truth & status

- **Spec (FROZEN):** `construct.frozen.kvx` — ontology + vocabulary are locked
  (Andrew, 2026-06-17). The four derived layers (schema, transport, renderers,
  projection engine, back-channel) are NOT yet designed.
- **Plan:** `IMPLEMENTATION_PLAN.md` — the multi-session build plan.

## Layout

```
construct/
  construct.frozen.kvx     FROZEN ontology + vocabulary (source of truth)
  IMPLEMENTATION_PLAN.md   the build plan
  schema/                  the typed wire schema (primitives + attributes); source of truth for codegen
    primitives/            per-primitive schema
  internal/
    projection/            the projection engine (world-state -> primitives)
    transport/             streaming envelope over the existing SSE/WS pipe
    backchannel/           the Ask round-trip (typed human response -> mid-run agent)
    codegen/               emit TS types into the client from the Go schema
  cmd/construct/           CLI: validate / selftest / codegen
  docs/                    per-layer design notes
```

The trusted client renderers live in the `client/` repo (Next.js) under
`components/matrix/construct/`, consuming the codegen'd types — see the plan.
