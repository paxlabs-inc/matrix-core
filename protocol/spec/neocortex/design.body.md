# NEOCORTEX — Design

Companion documents: `schematics.md` (authoritative component diagrams — read before implementing any component) and `agent.lock.kvx` (build-conduct lock — read first, every session, drift = fail).

## 1. Why a rewrite, and why C++

The current cortex is two architectures welded together. Generation 1 (journal MMR + SMT-anchored namespaces + OverallRoot + canonical CBOR + replay harness) was built for the D11 replay-determinism invariant, which the owner retired on 2026-07-21. Generation 2 (sessions, activation, rollups, story-so-far, episodic provenance, ToolEvents) is the part in daily use, and every piece of it was contorted into a "derived, journaled-but-not-anchored" lane purely to avoid perturbing a root nobody replays. The failure classes that actually hurt — recall misses on stored identifiers (Moltbook), negative-existence poisoning, duplicate facts, cognition/evidence conflation — live in the seams between the generations, and every fix so far has been a patch at a seam.

On the neo side, "what happened" is recorded six times (cortex `sess/`, the hello-world `sessionjournal`, `conversation.Store`, `turnstate` SQLite, cortex ToolEvents, trace/runrecord), retrieval is five uncoordinated systems fused with string concatenation, and the evidence/cognition separation is enforced by convention at a dozen call sites rather than by any type system.

C++ is chosen because the neocortex is a database kernel, not a service: the index plane needs SIMD and cache control (the exact-scan decision in §7 only exists in a language with vector intrinsics), the log wants io_uring and arena allocation, and crash recovery wants deterministic control of every byte. The cost is the Go seam and a second toolchain; §9 designs for the seam explicitly. The engine is held to a database-kernel robustness bar (§10) precisely because C++ removes the safety net.

## 2. Architectural stance

Two commitments shape everything:

**Deterministic single-writer state machine over one log.** Every write enters as a typed event; a single writer thread applies events in LSN order; all mutable state is a pure function of the log prefix. The core never touches the clock, entropy, or the filesystem except through injected interfaces. This buys deterministic simulation testing (thousands of simulated crash/fault schedules per CI run), the rebuild law (`drop projections + replay = byte-identical state`, turning corruption and schema evolution into rebuilds instead of migrations), and free log-shipping replication later.

**Mechanism in C++, policy in Go.** cortexd enforces gate mechanics: typed events, mandatory provenance, contradiction detection, canonical upserts, AEAD, tamper evidence. Everything requiring a model — consolidation extraction, embeddings, enrichment — stays in Go and arrives as events through the gate. cortexd makes no network connections and never calls a model; this is enforced structurally by linking no client library capable of either.

## 3. The evidence log (ground truth)

One append-only log per actor, in bounded segment files, written with io_uring + O_DIRECT (portable pwrite fallback) and group-commit durability barriers. Frame: `length | crc32c | lsn | kind | wall_ts | actor | conv | sealed FlatBuffers payload`.

The kind taxonomy is frozen in `agent.lock.kvx` and covers four families: conversation (`user_msg`, `delivered_msg`, `tool_call`, `tool_result`, `reasoning`, `provider_frame`, `media_ref`), work (`effect`, `approval`, `outcome`, `checkpoint`, `supervisor`, `recovery`), intent (`intent_set`, `loop_opened`, `loop_closed`), and memory (`assertion`, `consolidation`, `embedding`, `retract`, `attestation`). Guidance, doubt, steering, and rejected answers have **no representable kind** — the cognition/evidence separation is the type system, not a convention. `provider_frame` carries exact provider bytes, absorbing the sessionjournal `api_content` sidecar so window projections can be provider-faithful.

Write-ahead ordering is law: a `tool_call` is durable before the tool executes (the work ledger can always distinguish in-flight from unstarted), and only a `delivered_msg` represents user-visible output (a rejected close cannot poison the record — the loopty-loop class is unrepresentable).

Tamper evidence: BLAKE3 MMR over frame hashes (hashes commit to plaintext, so sealing does not perturb verification), ed25519-signed checkpoint roots, O(log n) range proofs, an offline verifier. This preserves the `MEMORY VERIFIED root=` property while deleting the SMT world-state machinery, canonical-CBOR head hashing, scope multi-proofs, and the replay harness — all D11 remnants.

Sealing: XChaCha20-Poly1305 record AEAD below the hash boundary, KEK → per-user UK → per-object DEK preserved from ORACLE, associated data binding actor/lsn/kind, key destruction = crypto-deletion with a verifiable receipt. XChaCha20's 192-bit nonces remove the nonce-management hazard class that AES-GCM carries.

Boot: verify the tail by checksum, truncate at most one torn frame at the physical tail, refuse to start on interior corruption with the exact LSN. The log is the WAL; there is no second write-ahead structure to disagree with it.

## 4. Projections and the rebuild law

All derived state lives in per-actor LMDB environments: belief store, entity index, vector lane, BM25, temporal ladder, intent frame, work ledger, conversation heads. Each projection records its applied-LSN checkpoint; boot replays the delta. LMDB is chosen over an LSM store deliberately: projections are rebuildable so write amplification is irrelevant, while zero-copy mmap reads serve the activation composer's thousands-of-small-reads pattern, and its single-writer semantics match the state machine. Readers serve from epoch-pinned snapshots and never block the writer.

The rebuild law is an operation, not a doctrine: the admin surface can drop any projection and replay; simulation CI proves byte-identical convergence, including crash-mid-rebuild.

## 5. The belief store and the write gate

Beliefs (facts, preferences, constraints, goals, identity) exist only as gate output from `assertion`/`consolidation` events. The schema makes a provenance-free belief unrepresentable: every belief carries LSN ranges into the log. Typed canonical-identity upsert with supersession chains and bi-temporal heads (as_of over valid and transaction time) replaces HNSW-proximity dedup — the NE11 typed-upsert patch becomes the only write path there is.

Gate semantics are deterministic: same canonical identity supersedes; cross-identity contradiction records a typed conflict edge that the composer is *obligated* to surface whenever either side surfaces; negative-existence assertions (gone/defunct/nonexistent) are rejected unless provenance includes corroborating `tool_result` evidence — the Moltbook poisoning rule, structural. Socket batches run a non-mutating pre-commit admission probe against the current snapshot plus earlier events in the batch. Actual belief mutation remains exclusively in APPLY; policy-invalid events encountered during legacy/import replay are deterministic skips so they cannot make recovery unbootable. Explicit remembers (MCP verbs, `memory_mutate`, preference feedback, profile writes) are assertion events through the same gate; retraction is a typed `retract` event honored everywhere.

## 6. The index plane: deterministic first

**Entity index.** Deterministic identifier extraction at apply time (domains, URLs, paths, hex/structured ids, proper names) into an exact-match table over roaring bitmaps. When an identifier or its entity appears in a query or turn, its records surface with guaranteed recall. This is the lane the current system lacks entirely, and it is the structural fix for "the fact was stored and recall missed it."

**Exact vector scan.** Per-actor scale (10³–10⁶ records) makes approximate indexes pointless: a SIMD (highway) scan over int8-quantized, binary-prefiltered, mmap-resident vectors is single-digit milliseconds at the top of that range and returns *exact* nearest neighbors — recall = 1.0, deterministically. HNSW and every ANN structure are banned by the lock. Probabilistic recall misses stop being a tuning problem because approximation no longer exists in the read path. Embeddings arrive from clients as `embedding` events (quantized in the log), so the lane rebuilds from the log alone; records without embeddings stay reachable via entity and lexical lanes.

**BM25** over roaring bitmaps for messages and beliefs; RRF fusion with the vector lane under a deterministic tiebreak.

**Temporal ladder.** Wall-clock windowed rollups with coarse-to-fine descent handles — the RLM navigation pattern preserved as a projection.

## 7. INV-1: thread continuity

The prime invariant, owner-stated: *Neo can never lose track of the current conversation, the work done in the active thread, and what he needs to do next.* Three clauses, each structural:

1. **The conversation cannot be lost** because the log is the conversation and the window is a projection. Long threads coarsen in view, never in substance; verbatim events stay addressable via descent.
2. **The work ledger is evidence, not memory**: dispatched/committed/returned/outcome-unknown states derive from write-ahead `tool_call`/`tool_result` events; boot marks in-flight effects for reconciliation before the thread continues.
3. **Intent is write-ahead**: `intent_set` before dependent work; `loop_opened` obligations appear in every activation until an explicit `loop_closed` with a typed reason (done, abandoned-with-cause, handed_off, superseded). Nothing closes silently; delivery itself is a loop only `delivered_msg` closes.

The respawn seed dies: successors are briefed by projections (window + ledger + intent frame), never by a predecessor's narrative summary — the o1-budget-kill poisoning class becomes unrepresentable.

The budget law protects all three: trims eat enrichment first, then coarsen the mid-conversation; the resident tier, intent frame, and ledger tail are never trimmed. An intent frame alone exceeding budget is an engine invariant violation, not a trim.

INV-1 is machine-checked: simulation CI kills the engine at every LSN of a scripted multi-turn/multi-tool/multi-loop workload, boots a successor, composes the next activation, and asserts all clauses. A clause failure at any LSN fails the build.

## 8. The activation composer

One `Activate(conv, query, budget)` replaces the five legacy lanes (Activate, Retrieve, LexicalConversation, episodic, conversational recaller). Strict tier order: resident (identity, hard constraints, active goals) → intent frame → work ledger tail → conversation projection → entity hits → conflict-obligated beliefs → RRF-fused enrichment → descent handles. Budgeting uses a caller-supplied token model, not a bytes-per-token heuristic; each tier reports spend and trims. The bundle is structured (typed sections, URIs, provenance) and rendering belongs to the client. The composer is a pure read pinned by golden vectors: identical prefix + query + budget → byte-identical bundle. Surfaced-attestation (used/ignored) returns as events so salience learning remains a projection concern.

## 9. cortexd and the Matrix seam

One cortexd process per user, serving a Unix domain socket with versioned FlatBuffers framing: request/response plus subscription streams, per-connection capability tokens scoping actor namespaces, idempotent append keyed by client sequence. neo, the executor daemon's cortex routes, and MCP tooling become clients of one brain instead of co-owners of store files.

The Go client implements the existing resurrection loop interfaces — `ActivationSource`, `TurnRecorder`, `EvidenceJournal`, `Consolidator` sink, `CheckpointStore` — so the runtime loop swaps substrates without loop changes. Turn checkpoints become `checkpoint` events plus a latest-checkpoint projection, folding the turnstate role into the same mechanism as thread continuity.

Crash isolation: a cortexd death never takes the agent down. The client degrades to a bounded queue then honest typed failure; the supervisor restarts cortexd; reconnect loses no acknowledged write and duplicates no unacknowledged one. Admin capability (same socket, separate token): health, latency histograms, stats, verification status, manual rebuild.

## 10. Robustness regime

C++23, pinned toolchain, hardened libc++, `-fno-exceptions` core with `std::expected` typed errors on every fallible path. No raw owning pointers or naked new/delete; per-request `std::pmr` arenas; static allocation budget with bounded queues. Dependencies are exactly the vendored, digest-pinned set in the lock: liburing, LMDB, BLAKE3, libsodium, FlatBuffers, CRoaring, highway, xxhash+crc32c. CI: ASan/UBSan/TSan matrices, libFuzzer on every decoder (frame parser, schema verifier, protocol framing, query input) with corpora in-tree, rapidcheck-style properties (replay convergence, upsert idempotency, MMR consistency, AEAD round-trips), the deterministic simulation harness, golden vectors, clang-tidy at zero warnings, reproducible cross-compiled amd64/arm64 static builds.

## 11. Neo hard cutover

The owner approved a clean-memory hard cutover for Neo on 2026-08-04. Neo has one memory substrate: Neocortex. Its source tree, tests, complete Go module graph, configuration, CLI, constructors, proxy routes, and runtime artifacts contain no reachable Cortex-v1 path. There is no substrate selector and no compatibility fallback. Missing or unhealthy cortexd is an honest typed Neocortex failure handled by the restart/reconnect discipline; it never causes a legacy store to open.

Cortex v1 may temporarily remain for unrelated Matrix consumers, but it is outside Neo's build and runtime boundary. External-consumer re-pointing and deletion of unrelated data remain separately owner-gated. The Neo cutover gate is structural as well as behavioral: source and module-graph checks prove exclusion, and real-daemon qualification proves the sole Neocortex path.

## 12. Explicit v1 omissions

No distributed replication (designed-for, not built). No ANN structures. No general query language. No cross-actor shared memory. No embedded scripting. No remote or TCP exposure of cortexd. No engine-side LLM calls of any kind.
