# Neo Developer Documentation

Neo is Matrix's default conversational agent: a recursive LLM tool-calling loop with cortex-backed memory, the shared MCP tool surface, and `core_execute` delegation to the MCL pipeline for rigorous / money-moving tasks.

This documentation is written for people working on Neo itself — extending the control loop, adding memory mechanisms, wiring new tools, or understanding how the conversational HTTP service works.

---

## Contents

| Document | What it covers |
|---|---|
| [Control Loop](./control-loop.md) | The recursive `Chat` loop — transcript, tool calls, compaction, termination |
| [Memory System](./memory-system.md) | The pager — pinned block, page-fault retrieval, procedural patterns, compaction |
| [LLM Client](./llm-client.md) | OpenAI-compatible function-calling transport, gateway metering, reasoning channels |
| [Tool Surface](./tool-surface.md) | MCP server pool, execution-surface split (Natural vs Escalate), `core_execute` |
| [Server & HTTP Front](./server-http.md) | Production service — POST /chat, SSE events, reverse proxy, media plane |
| [Conversation Store](./conversation-store.md) | Durable chat-thread memory, resume seeding, unified history with the daemon |
| [core_execute Delegation](./core-execute.md) | The bridge to MCL — async HTTP API, inline approval gates, poll-based status |
| [Config System](./config-system.md) | Runtime .kvx overlay, environment precedence, frozen spec defaults |
| [Write-back Consolidation](./writeback-consolidation.md) | Background pass — facts, outcomes, procedural patterns promoted to cortex |
| [Conversational Recall](./recall-lane.md) | The additive read-lane — relevant past turns beyond the live transcript |
| [Frozen Design Spec](./frozen-spec.md) | The `neo.frozen.kvx` architecture contract — invariants, principles, deferred items |

---

## Repository layout

```
neo/
├── cmd/neo/
│   ├── main.go          # CLI entry: REPL or single -prompt turn
│   └── serve.go         # Production HTTP service entry
├── internal/
│   ├── agent/
│   │   ├── agent.go      # Control loop: Chat, tool dispatch, budget, compaction trigger
│   │   ├── compaction.go # Summary generation + transcript trimming
│   │   ├── prompt.go     # System prompt builder + ground truth injection
│   │   ├── reporter.go   # Say/Status/Notice interface
│   │   ├── validate.go   # High-entropy token verbatim validator
│   │   └── knowledge.md  # Embedded Paxeer grounding facts
│   ├── config/
│   │   ├── config.go     # Config struct, Default(), Load(), env overlay
│   │   └── kvx.go        # .kvx file parser (sectioned key/value)
│   ├── conversation/
│   │   └── store.go      # Durable turn log per conversation_id (JSON on disk)
│   ├── delegate/
│   │   └── client.go     # core_execute HTTP bridge to the MCL daemon
│   ├── llm/
│   │   ├── client.go     # OpenAI chat-completions client with tools
│   │   └── message.go    # Message, ToolCall, Tool types + constructors
│   ├── memory/
│   │   ├── embedder.go   # Embedding backend selection (gateway → direct → hash)
│   │   ├── pager.go      # Memory controller: pinned, retrieve, procedural, write-back
│   │   ├── pattern.go    # PatternSpec schema + encode/decode/render
│   │   └── writeback.go  # Cortex write helpers (fact, outcome, pattern)
│   ├── recall/
│   │   └── recall.go     # Conversational recall lane (embedded turn ranking)
│   ├── server/
│   │   ├── engine.go     # Process-wide dependencies + core_execute wiring
│   │   ├── media.go      # GET /media, POST /upload (machine-volume media plane)
│   │   ├── server.go     # HTTP mux: /chat, /events, /conversations, proxy catch-all
│   │   ├── session.go    # Per-conversation agent + run lifecycle + gate waiters
│   │   └── sse.go        # Event broker: replay buffer + live fan-out
│   ├── tools/
│   │   ├── surface.go    # Natural vs Escalate classifier
│   │   └── tools.go      # MCP manager: spawn, bind, dispatch, schemas
│   └── writeback/
│       └── consolidator.go # Background consolidation pass (cheap model)
└── neo.frozen.kvx        # Frozen architecture spec (design contract)
```

---

## The one-sentence contract

Neo takes a user message, runs it through a recursive tool-calling loop with cortex-backed memory, and returns a final answer. Anything that moves funds or needs a wallet signature is delegated to `core_execute`, which routes through the MCL pipeline with inline user approval. Neo never holds a signing key.

That is the whole point. It's the boundary between the conversational world and the rigorous, replayable, on-chain world.

---

## Key locked decisions

These decisions are frozen in `neo.frozen.kvx`. Don't re-litigate them without an explicit spec version bump.

| ID | Decision |
|---|---|
| **i1** | Neo never holds a signing key; all money crosses into MCL |
| **i2** | Cortex is ground truth; the context window is a cache |
| **i3** | Compaction preserves high-entropy tokens verbatim (addresses, tx hashes, IDs, file paths) |
| **i4** | No throttles/gates on reversible execution; safety is structural (key isolation + Argus) |
| **i5** | Hide the mechanism, surface the intention in human cognitive-state terms |
| **i6** | No false success — honest partials only |
| **i7** | Procedural patterns are gated by preconditions (before) and success_criteria (after) |
