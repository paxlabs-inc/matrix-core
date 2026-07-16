# DEJA-VU — automatic episodic recall

## Mission

When the user references a shared past — "remember when we discussed X", "last time
you said", "that error about Y from last week" — Neo must answer from the actual
record, not from reconstruction. Today it cannot: automatic recall exists only on the
first turn of a conversation, recall reaches memory *summaries* but never the verbatim
exchange, and nothing connects a memory back to the transcript it came from. DEJA-VU
makes remembrance a reflex: deterministic detection on the inbound user message, a
bounded retrieval pipeline that lands on the exact past exchange, and injection into
the generation window *before* the model produces its first token — the epistemic-core
doctrine ("never let the LLM assume; truth resident at generation time") applied to
episodic memory. A deterministic Cassandra doubt trigger backstops the misses.

This feature deliberately touches cortex — the single durable brain shared by every
Matrix agent (unified-agent vision, Andrew 2026-07-10). Everything it adds to cortex
is therefore held to custody-of-memory rigor: derived lanes only, replay determinism
proven, hash-boundary discipline preserved, values vault-sealed.

## The grounded current state (why each move exists)

- **The push exists but is gated to turn 1.** `cmRelevancePush`
  (`neo/internal/agent/continuous.go:143-168`) auto-runs `pager.Retrieve(ctx, userInput)`
  and appends a rendered block onto the `cmTail`; the gate is
  `!a.cfg.FirstTurnRelevancePush || a.turnSeq != 1` (`continuous.go:144`). The
  generalization point is exactly here, inside `prepareTurn` (`agent.go:680-710`),
  which computes the activation bundle once per turn (`agent.go:686-692`) and is the
  only place the user message enters the working transcript (`agent.go:681`).
- **The window law.** `prepareWindow` (`agent.go:729-745`) is the ONE window-assembly
  site: `assembleWindowUserTail(stableSystem, a.working, cmTail+epistemicTail+budgetTail)`
  at `agent.go:742`, counted by `windowAssemblies` (`agent.go:743`); the activation
  bundle is computed once per turn, counted by `activationAssemblies` (`agent.go:687`).
  DEJA-VU injects through the cmTail and nowhere else, so both invariants survive
  untouched.
- **Transcripts are durable but unsearchable.** The cortex session store
  (`cortex/session.go`) persists every message under `sess/<conv>/<seq>` with
  `AppendMessage` (`session.go:236-350`) and reads via `Transcript(conv, sinceSeq, limit)`
  (`session.go:359-402`) — prefix scan by (conversation, seq) only. There is no
  full-text, regex, or semantic search over `sess/` anywhere in cortex.
- **The embedder indexes memories only.** `cortex/embedder.go` walks the journal and
  embeds resolved memory forms into the HNSW index; it never touches `sess/`. So
  semantic search finds the memory *about* an event, never the event's words.
- **RecallDescend bottoms out at memory leaves.** The temporal-ladder descent
  (`cortex/recall.go:146`, depth ladder epoch→day→hour→memory at `recall.go:33-43`)
  resolves `RefKindMemory` leaves; it does not descend into raw session messages. Its
  `AsOf`/horizon options (`recall.go:96-112`) are, however, exactly the time-window
  surface the temporal-deixis mapping needs.
- **Rollups already carry coarse provenance.** Every `RollupRecord` stores the
  inclusive journal-seq span it summarizes (`SeqLo/SeqHi`, `cortex/rollup.go:122-123`)
  — the fallback ladder for memories that predate exact provenance edges.
- **`derived_from` exists as an edge type but is unused for sessions.** Cortex's typed
  edge model includes `derived_from` (provenance); the writeback consolidator
  (`neo/internal/writeback/consolidator.go`) knows the conversation and turn it is
  processing at write time but records no pointer from the memories it writes back to
  the `sess/` refs. This is the Zep-episodic-provenance gap identified in
  `temp/agent-memory-systems-analysis.md`, and closing it is the cheapest source of
  exactness in the whole design.
- **Cassandra is deterministic and post-hoc.** The silent-voice controller
  (`neo/internal/agent/cassandra.go`) makes no model call, consumes only the
  `stepSignals` projection (`signals.go:116-128`), and edits only the last assistant
  message's content (`cassandraEdit`, `cassandra.go:194-224`), firing in `deliberate`
  after generation (`agent.go:869`). It cannot be the primary feed — by the time it
  fires, an ungrounded answer already exists — but it is the right shape for the
  failsafe.
- **Cortex values are sealed.** Since ORACLE task 2.3, Pebble values are
  vault-encrypted below the hash boundary (`cortex/store/vaultseam.go`); leaf/SMT
  hashes are computed over plaintext canonical encodings. Any new persisted value
  family must ride the same seam.
- **The derived-lane discipline.** Rollups, story-so-far, and checkpoint records are
  derived: journal-rebuildable, deterministic, no SMT writes; the anchored namespaces
  (memories, edges) stay byte-identical whether derived lanes are active or not
  (continuous-memory task 1.2 invariant). The lexical index and sweep checkpoints join
  this lane. Provenance edges are the one deliberate exception — they are *canonical*
  `AddEdge` mutations, journaled and SMT-anchored like every edge, because provenance
  is ground truth, not a rebuildable cache.

## Locked decisions

1. **Feed channel (Andrew, 2026-07-16): cmTail primary, Cassandra failsafe.** The
   episodic block rides the cmTail computed in `prepareTurn` — pre-generation,
   through the existing single assembly site. The user turn in `a.working` is NEVER
   modified (option B rejected); cortex ground truth records originals only
   (`cmRecordUser` at `agent.go:684`, `cmRecordAssistant` at `agent.go:851`).
   Cassandra gains one deterministic doubt trigger as the net under the push.
2. **Hybrid detection.** A deterministic lexicon decides WHETHER to fire (zero
   latency, table-driven, testable); a single cheap-lane extraction call decides WHAT
   to search for (clean referent + time hint) only on turns that already fired. The
   expensive step never runs on non-trigger turns; its failure degrades to
   raw-message-as-query.
3. **Provenance edges at write time.** The consolidator stamps `derived_from` edges
   from every memory it writes to the `matrix://cortex/session/<conv>/<seq>` refs of
   the processed turn. One edge at the moment the information is free; every future
   memory becomes an exact pointer into its source transcript.
4. **Three retrieval lanes, fused.** Lane 1: semantic HNSW `Find` over memories
   (exists). Lane 2: provenance-edge expansion of memory hits to verbatim
   `Transcript` slices (wave 1 resolver). Lane 3: BM25-style lexical index over
   `sess/` for exact words that never consolidated into a memory (wave 3). Fusion by
   reciprocal rank. Plus the zero-cost current-conversation lane via
   `neo/internal/recall` for references to earlier turns of the live thread.
5. **Lexical over semantic for transcripts.** "Find where I said X" is a lexical
   problem. Session messages are NOT embedded in v1 (volume is far higher than
   memories; BM25 answers the exact-match use case); semantic transcript search is an
   explicit non-goal, revisitable once the lexical lane's hit rates are observable.
6. **Backfill runs asleep.** History (postings for old sessions, heuristic provenance
   edges for old memories) is built by a Chronos idle sweep reusing the
   busy-check/reschedule conventions — the Letta "sleep-time compute" pattern mapped
   onto infrastructure Matrix already has. Deterministic and idempotent; no LLM in the
   sweep.
7. **Fail-open, everywhere on the read path.** Unlike the vault (fail-closed on
   writes of user data), deja-vu is an enhancement to a turn that must never block it:
   lexicon miss, extraction timeout, broken embedder, corrupted index, resolver error
   — every failure yields "no episodic block, normal turn". The write-path exception:
   a failed provenance edge never fails the memory write either (the memory is the
   payload; the edge is enrichment).

## System map (one episodic turn)

```
user: "remember when we discussed the deposit bug?"
  │
  ▼ Chat → prepareTurn (agent.go:680)
  ├─ a.working += UserMessage(raw)              (unmodified, agent.go:681)
  ├─ cmRecordUser(raw) → cortex sess/           (ground truth, agent.go:684)
  ├─ episodicClassify(raw)  ── lexicon fires ──►  cheap-lane extract
  │                                               {referent:"deposit bug",
  │                                                timeHint:→ window}
  ├─ EpisodicRetrieve(referent, window, budget)
  │    ├─ lane 1: cortex.Find{Near} over memories (HNSW)
  │    ├─ lane 2: derived_from edges → Transcript(conv, seq±r)  [verbatim]
  │    ├─ lane 3: lexindex.Query(terms, window)   [wave 3]
  │    ├─ current-conversation lane (neo/internal/recall)
  │    └─ RRF fuse → dedup → token/k/deadline caps
  ├─ cmTail = renderActivationBundle(bundle)
  │         + episodicBlock(excerpts w/ provenance)   ◄── THE feed (locked)
  │         [+ first-turn relevance push, unchanged]
  ├─ turn.episodicPending = fired && !surfaced
  └─ memory.activation event (+ episodic fields) → client timeline
  │
  ▼ prepareWindow (agent.go:729) — unchanged, one assembly site
  ▼ generate — first token already grounded
  ▼ deliberate → governVoice:
       trigEpisodicUngrounded fires iff pending ∧ ungrounded ∧ closing
```

## Component notes

### Detection (`neo/internal/agent/episodic.go`, new)

Table-driven compiled lexicon over the raw user message. Classes: remembrance verbs
("remember when/how/that", "recall", "you said/mentioned/told me", "we
discussed/talked about/decided"), shared-past deixis ("last time", "that
thing/bug/plan we", "back when", "earlier you"), temporal deixis ("yesterday", "last
week", "in June"). Negative guards: prospective "remember to …", imperative "remind
me to …" are NOT episodic. Skipped entirely for heartbeat/Automatrix wake markers
(`isAutomatrixWake` and heartbeat siblings) and guidance messages. The extraction call
uses the consolidation-lane model client (already cheap, already wired), one call,
hard deadline (config), JSON `{referent, time_hint, scope_hint}`; parse errors or
timeout → referent = raw message, default horizon.

### Provenance resolver (cortex-side)

`ExpandToTranscript(memoryURI, radius)`: follow `derived_from` edges whose target
parses as a session URI → group by conversation → `Transcript(conv, seqLo-r, seqHi+r)`
→ excerpt with `{conv, date, seqLo, seqHi, exact:true}`. Fallback for edge-less
memories: locate the hour rollup containing the memory's CreatedAt, use its
`SeqLo/SeqHi` journal span to bound a session scan by timestamp proximity, mark
`exact:false`. Heuristic sweep-backfilled edges (wave 4) carry a distinguishable
marker so exactness is never overstated.

### The episodic block (rendering)

Header: "The user is referencing a past exchange. Auto-recalled from memory — ground
your answer in this; if it does not match what they mean, say so plainly." Then per
excerpt: date, conversation handle, exact/approximate tag, the verbatim slice; then
related memory summaries; then the standing pointer to `memory_recall` for more. Token
cap independent of the Activate trim (the block must not be silently starved by a fat
bundle, nor allowed to starve the transcript — its own budget, config-capped).
Excerpt refs join the turn's surfaced sets for dedup within a conversation.

### Lexical index (cortex derived lane)

Deterministic tokenizer (lowercase, unicode word segmentation, no stemming in v1 —
determinism beats recall here), postings under a dedicated derived key family keyed
`(term → conv/seq, positions, freq)` with BM25 parameters fixed in code. Built
incrementally inside `AppendMessage`'s derived path; rebuild = delete family + walk
`sess/` in journal order (byte-deterministic by construction: same input order, same
encoding). Values through the vault seal seam. Query: terms/phrase + time range + k →
scored (conv, seq). No SMT writes; no new journal kinds (the index derives from
already-journaled session appends). Root-identity and replay proofs are wave-3
gating tests, not afterthoughts.

### Cassandra failsafe

One new `stepSignals` field: `episodicPending bool` (set in prepareTurn when the
lexicon fired and the pipeline surfaced nothing; cleared when an episodic block was
injected or a `memory_recall` call dispatched this turn). New doubt trigger
`trigEpisodicUngrounded`, classified after `trigRefutedPremise` but before generic
close triggers; template: first-person "I'm about to assert something about our past
from reconstruction — I should check memory_recall before claiming it." All existing
guardrails (content-only, assistant-role-only, dual-record, cooldown, casMaxMods)
untouched; regression sweep proves non-episodic turns byte-unchanged.

### Backfill sweep

Chronos-scheduled (sibling convention of AUTOMATRIX/heartbeat markers), busy-check
defers when a run is live, batch-capped per wake, checkpoint = derived record
(resumable). Two jobs: (a) postings for historical `sess/` records; (b) heuristic
`derived_from` edges for pre-existing memories via actor + CreatedAt + conversation
containment. Idempotent: a completed sweep re-run is a proven no-op.

## Config (all `[dejavu]`-style knobs, env-overridable, defaults conservative)

- `NEO_DEJAVU` master switch (default on; off = byte-identical to today)
- extraction deadline ms; episodic block token cap; excerpt radius (turns);
  max excerpts; pipeline wall-clock deadline; lexical k; sweep batch size
- `FirstTurnRelevancePush` untouched and independent

## Observability

`memory.activation` gains episodic fields (excerpts + provenance + trigger class),
persisted via `traceWorkspaceTypes`. One structured log/counter line per episodic
turn: trigger class, lanes queried, hit counts, injected tokens, latency — the tuning
loop for the lexicon and budgets runs off prod logs, not guesses.

## Non-goals

No session-message embeddings (deferred; lexical lane first). No modification of the
user turn in the window. No new memory types. No client search UI. No cortex-shell
verb additions beyond fusion wiring. No change to the first-turn relevance push, the
window law, Cassandra's edit primitive, or any anchored-namespace write path outside
canonical edge mutations. UI house rules hold.

## Verification doctrine (no fakes — custody-of-memory grade)

Every test drives real components: real Pebble cortex, real consolidator against the
established httptest SSE LLM seam, the real agent Chat loop, real edges, real
`Transcript` reads, real Chronos alarm builders. The suite must prove the feature
works (cross-conversation verbatim excerpt resident pre-generation) AND that cortex is
unharmed: OverallRoot/anchored-root byte-identity with the index on/off and across
delete-then-rebuild, replay byte-identity, deterministic rebuild, and full fail-open
under a broken embedder, dead cheap lane, and corrupted index. A green test driven by
a fake is not done. A red integrity check is a stop condition for the whole feature.
