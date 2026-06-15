# Cortex Developer Documentation

Cortex is Matrix's per-actor persistent memory store: a Pebble-backed key-value database with a typed memory taxonomy, a salience-ranked query engine, an async embedding pipeline, a Merkle-authenticated snapshot system, and a full replay invariant. Every durable thing an agent knows — facts, preferences, goals, constraints, past events, procedural patterns — lives here.

This documentation is written for people working on cortex itself — extending the memory types, tuning the salience formula, adding index layers, or understanding how the replay and snapshot machinery works.

---

## Contents

| Document | What it covers |
|---|---|
| [Memory Taxonomy](./memory-taxonomy.md) | The 9 typed memory kinds, Head, Version, Forms, Tombstone |
| [Store & Journal](./store-and-journal.md) | Per-actor Pebble DB, key encoding, atomic write batches, journal |
| [Write API](./write-api.md) | `Write`, `Update`, `Tombstone`, `UpdateHead` — mutations and their journal footprint |
| [Find Query](./find-query.md) | The typed query engine — predicates, planner, ordering, graph traversal |
| [Context Bundle](./context-bundle.md) | The three-tier cold-start composer — Pinned, Frame-relevant, Outcomes |
| [Embedder & Vector](./embedder-and-vector.md) | Async embedding worker, pure-Go HNSW, `Find Near` / `Find NearURI` |
| [Edges & Graph](./edges-and-graph.md) | Typed edges, `AddEdge`, `RemoveEdge`, bounded BFS |
| [Snapshot & Proofs](./snapshot-and-proofs.md) | Journal MMR, per-namespace SMTs, `OverallRoot`, multi-proofs |
| [Salience](./salience.md) | The 5-factor formula, cold weights, EMA per-actor weight learning |
| [Attest & Compact](./attest-and-compact.md) | Intent attestation, salience feedback, budget-aware compaction |
| [Scope](./scope.md) | Sub-agent `CortexScope` — Merkle-authenticated read/write gating |
| [Replay](./replay.md) | Drop-and-rebuild — the derivability invariant and `Cortex.Rebuild` |

---

## Repository layout

```
cortex/
├── go.mod
├── cortex.go               # top-level facade: Write / Update / Tombstone / Find / Context / Snapshot
├── attest.go               # Attest — intent-outcome salience feedback + EMA weight update
├── compact.go              # Compact — budget-aware context compaction + checkpoint records
├── context.go              # Context — three-tier cold-start bundle composer
├── edges.go                # AddEdge / RemoveEdge / GetEdge / IterEdgesOut / IterEdgesIn
├── embedder.go             # StartEmbedder / StopEmbedder / DrainEmbedder + HNSW worker
├── goal_state.go           # GoalRuntimeState sidecar (ambient scheduler bookkeeping)
├── ratelimit.go            # Phase 14 token-bucket DoS guards (scope violations + attests)
├── rebuild.go              # Cortex.Rebuild facade → replay.Rebuild
├── scope_enforce.go        # VerifyScope / enforceRead / enforceWrite / logScopeViolation
├── update_head.go          # UpdateHead — Head-only mutations without a Data version bump
├── keys/
│   └── keys.go             # all Pebble key constructors + namespace prefixes
├── journal/
│   └── journal.go          # Entry, Kind, payload types, canonical CBOR, leaf hash
├── memory/
│   ├── types.go            # Type, Visibility, Head, Version, Forms, Tombstone, SourceKind
│   ├── data.go             # IdentityData, FactData, …, PatternData (9 schemas)
│   ├── codec.go            # canonical CBOR encode/decode + HashVersion
│   ├── validate.go         # write-time schema validation
│   ├── verb.go             # Verb (10 D7 verbs), ObjKind, FrameRef
│   └── edge.go             # EdgeType, EdgeRecord
├── store/
│   ├── store.go            # Open, Get, PrefixIter, JournalHook, NextSeq
│   └── writebatch.go       # BeginWrite → AppendJournal → Commit / Abort
├── salience/
│   └── salience.go         # Score, ColdScore, ColdScoreWith, Weights, EMA helpers
├── forms/
│   ├── forms.go            # Render (short + medium), RenderFull (on-demand)
│   └── truncate.go         # TruncateToTokens (UTF-8-safe, bytes/4 heuristic)
├── query/
│   ├── predicate.go        # Predicate AST: Eq Ne Gt Gte Lt Lte In HasTag Matches And Or Not
│   ├── eval.go             # field resolver + scalar comparator
│   ├── find.go             # Run: planner → candidate scan → predicate eval → sort → render
│   └── graph.go            # BFS traversal for Query.From / Query.Follow
├── embed/
│   ├── embed.go            # Embedder interface + HashEmbedder (deterministic sha256 stub)
│   └── api_embedder.go     # HTTP-backed API embedder (nomic-embed-text-v1.5)
├── vector/
│   └── vector.go           # NewIndex, Add, Search, Save, Load, MapStore, VectorStore
├── snapshot/
│   ├── snapshot.go         # State, Snapshot(), CurrentRoots(), FindSnapshotByRoot()
│   ├── mmr.go              # journal MMR append + root computation
│   ├── smt.go              # per-namespace sparse Merkle tree (memories + edges)
│   └── multiproof.go       # MultiProof, BuildMultiProof, VerifyAgainstManifest
├── scope/
│   ├── scope.go            # Scope struct, Selector, Sign, Encode, Allows
│   ├── verify.go           # Verify, KeyResolver, StaticKeyResolver
│   ├── match.go            # Allows per-Head matching logic
│   └── errors.go           # ErrViolation, ErrNotWritable, ErrScopeExpired, …
├── replay/
│   ├── replay.go           # Rebuild, Result, Options, VerifyPreservesRoot
│   └── drop.go             # DropDerived, CountDerived
└── cmd/cortex-shell/       # smoke-test CLI: write/resolve/update/tombstone/find/snapshot
```

---

## The one-sentence contract

Cortex is the ground truth for all durable agent memory. The context window is a cache; cortex is the disk. Every write is atomically journaled; the journal feeds a Merkle accumulator; snapshots seal the state root. Drop the derived indexes — replay the journal — roots match. That is the invariant everything else depends on.

---

## Key locked decisions

These decisions are frozen. Don't re-litigate them without a documented protocol-level change.

| ID | Decision |
|---|---|
| **D4** | Cortex holds no key material. Signing lives in the agent runtime or tools layer. |
| **D11** | Compiler seed = sha256(intent.id \|\| actor \|\| OverallRoot \|\| mtx_digest \|\| model_digest). Same inputs → same output. |
| **D13** | `#latest` is forbidden at `ParseURI` time. All versions must be pinned before sign-off. |
| **D17** | One Pebble DB per actor. All namespaces are key prefixes inside that DB — the only way to satisfy §11.1 batch atomicity across journal + head + version + indexes. |
| **D7** | Verb vocabulary is closed at exactly 10 values: `find acquire build modify deliver analyze negotiate schedule monitor delegate`. ObjKind is similarly closed. Extending either requires a journaled migration. |
| **§8.2** | Pinned memories carry a salience floor of 0.7. Tombstoned memories rank to zero and are filtered upstream. |
| **§11.1** | Every store mutation — Write, Update, Tombstone, AddEdge, UpdateHead, Compact, Attest — commits journal + primary keys + index keys in a single atomic Pebble batch. A write without a journal entry fails with `ErrBatchNoJournal`. |
| **Phase 10 Q7** | Sub-agent `UpdateHead` requires `Scope.Writable=true`. Default deny. |
