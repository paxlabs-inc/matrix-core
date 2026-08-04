# HANDOFF neocortex

Waves 1-6 and Task 7.1 are done. Task 7.2 is the sole task in progress. Neo is hard-cut to Neocortex: its source, tests, module graph, configuration, CLI, Railway launch path, and runtime have no Cortex-v1 selector or fallback. Unrelated Cortex-v1 consumers remain outside Neo and are not authorized for deletion or migration.

Verified on 2026-08-04 for Task 7.1:

- `scripts/check-neo-neocortex-only.sh` passes and is wired into the Neo CI agent gate. It rejects Cortex-v1 imports/module edges, substrate selectors/adapters, and retired Railway Neo environment keys.
- `go test -race -timeout=10m ./...` passes in `neo`, including the real managed cortexd socket path, typed unavailable-engine behavior, Neocortex activation/transcript/checkpoint use, belief writes with provenance, typed retractions, runtime evidence, and the full server/agent regressions.
- `go test ./...` passes in `cortexclient`; `go test ./cmd/mcl-execute -run '^$'` passes in `executor`; `bash -n deploy/railway/entrypoint.sh` passes.
- Railway Neo mode starts cortexd first, injects only the scoped Neocortex socket/token into Neo, passes `-memory-disabled` to the co-located MCL daemon, and has no legacy Neo memory environment or fallback branch.
- No production deployment, restart, unrelated-consumer migration, or Cortex-v1 data deletion was performed.

Verified on 2026-08-03:

- Locked vendored dependency and arm64 sysroot digests verify.
- Native Debug, ASan+UBSan, TSan, fuzz smoke, clang-tidy, and static Release builds pass.
- The real walking skeleton creates an actor directory, appends a CRC32C walking frame, reopens it, reads it, and verifies the payload.
- Two independent static amd64 builds are byte-identical with SHA-256 `ee37bf8b6a7b5124c463854fff419297767a4e382553c7f1363838d061129a91`.
- The arm64 Release output is an AArch64 ELF, statically linked, with no interpreter or dynamic segment. Its locked sysroot tree SHA-256 is `f1230b9ccce9c5f127c91cd3ef4823eb8173fb87fd896a386bbf7e4a584e77fc`.

Task 1.2 evidence:

- The frozen 43-byte frame header and all 21 permitted event kinds round-trip with CRC32C and reject malformed kinds, lengths, truncations, and checksums.
- Bounded segments use aligned physical group commits. A traced real run executed `io_uring_setup` and `io_uring_enter` against an `O_DIRECT` descriptor; an independently forced portable path completed real 3-byte `pwrite` and `pread` loops.
- Segment `fdatasync`, CRC-protected MANIFEST `fdatasync`, and directory `fsync` form the commit barrier. A corrupt MANIFEST rebuilds from the log.
- Boot truncates one torn physical-tail frame, refuses interior corruption at the exact LSN, and preserves acknowledged data after a real `SIGKILL`.
- A deterministic property run covered 96 randomized batches, hundreds of frames, segment rotation, reopen, and byte-exact replay. Debug, ASan+UBSan, TSan, clang-tidy, 10,000 frame-fuzzer runs, static amd64 tests, static arm64 compilation, vendored digests, and fresh-directory reproducibility all pass.
- Current reproducible amd64 SHA-256 values are `f2a5d29cfaffcb8f0c3242d70f777e0cb79801aedd37af79fe66dd2be67ffdad` for the frame test, `f558cc196c1f890212ded0387cb4391af2355109ac1ec7c4aeb8476f8f9e07a6` for the segment-log test, and `44b145500413f85a65576e4c7ee884ab81fffdb8ceb043f2e75319f342187841` for `libneocortex_log.a`.

Task 1.3 evidence:

- Plaintext frame commitments feed an append-only BLAKE3 merkle mountain range with domain-separated parent/root hashing and prefix-root reconstruction.
- Contiguous multi-proofs carry only the logarithmic complement frontier. A property run covered 1,024 appended leaves and 2,048 randomized ranges; every valid range reproduced the root, while forged leaves and reordered ranges failed.
- CRC-protected write-once peak-node records rebuild and verify on boot. Reordered persistent nodes fail boot verification.
- Deterministic ed25519 keys sign fixed-format checkpoint roots. Two checkpoints across 257 frames survived restart; a forged signature failed. `verification_status()` exposes the verified root, leaf count, and last checkpoint for the future admin protocol.
- The real `cortex-verify` process verified a trusted public key, checkpoint signature, persisted MMR prefix, and emitted `MEMORY VERIFIED root=...`. Checkpoint and peak-record decoders each run under the MMR-state fuzzer.
- Debug, ASan+UBSan, TSan, zero-warning clang-tidy, two 10,000-run fuzz targets, static amd64 tests, and static arm64 compilation all pass. Independent amd64 builds are byte-identical, including vendored `libsodium.a`.
- Current reproducible SHA-256 values are `8f977a698a63d11a72951e67d8669248e06c1950fc78face6d4797d866fdc599` for the MMR test, `c48da0aadbc51197980f219b096d20ad9d059a372b1b58319e2923762d03d8ff` for `cortex-verify`, `8a45f62014f41c9c86a92e30c6173881efdc215afaa9fa32bf18bc301b7ae707` for `libneocortex_mmr.a`, and `16ab6765340bfa12f8680cf6e52568a029c71206fda2322014c4b4128e6c2b68` for `libsodium.a`.

Task 2.1 evidence:

- The state-machine core reaches time, entropy, and storage only through injected interfaces. Boot replays durable frames, a thread-owned apply loop rejects a second writer, and acknowledgements are produced only after the storage commit boundary.
- The production adapter binds the same interface to the real segment log and MMR. It verifies log/MMR prefix equality on boot and repairs a crash between durable log commit and MMR append by replaying the missing commitments.
- The deterministic simulator models bounded short reads and writes, reversed completion order at fixed offsets, torn physical tails, dishonest fsync, and process death immediately after durable commit. CRC or sequence damage before the tail fails closed as interior corruption.
- A fixed 32-event workload converges to byte-identical canonical state and byte-identical durable log bytes across 2,048 independently seeded crash schedules. A separate property kills after every LSN in a 64-event workload. The real segment/MMR adapter persists and restarts 37 events through forced 3-byte reads and 5-byte writes.
- Debug, ASan+UBSan, TSan, and zero-warning clang-tidy pass. Two independent static amd64 builds are byte-identical, and the complete core simulator cross-build is a static AArch64 ELF with no interpreter or dynamic segment. Vendored dependency and sysroot digest verification remain green.
- Current reproducible SHA-256 values are `41f11c6aab64b665c91bea433edda3b0bbc1cc9fcec88a575b485232e564c352` for the core simulator test, `9aee6664a19065b9013969796b6c5da84cb08c5a5d294feb283200de7a1d5c1f` for `libneocortex_core.a`, and `188c306ac0474d1d44b8a35fdab138cb16d04115888550f76fcb965663c7ba7d` for `libneocortex_sim.a`.

Task 2.2 evidence:

- Each actor receives one real LMDB environment containing the eight required named projection databases plus an atomic checkpoint database. Projection mutations and their applied-LSN checkpoint commit in one writer transaction; gaps and second-writer calls fail with typed errors.
- ReadSnapshot owns an epoch-pinned LMDB read transaction. A real concurrent reader retained identical canonical bytes while the owner writer advanced the same projection, and a later snapshot observed the new epoch and state.
- Selective reset drops only the named projection and resets its checkpoint. The production conversation-head projector is a first real consumer of the substrate; reset plus log replay reproduced its canonical dump byte-for-byte without changing an independently populated entity projection.
- A child process rebuilt through LSN 37, reported the committed boundary, and was killed with real SIGKILL. Reopen observed checkpoint 37 and resumed through LSN 128 to the exact original canonical bytes.
- The complete five-test Debug suite, affected ASan+UBSan and TSan binaries (including instrumented LMDB), and zero-warning clang-tidy pass. Two independent static amd64 builds are byte-identical, and the projection test cross-build is a static AArch64 ELF.
- Current reproducible SHA-256 values are `803b785fba6b676b75684408dbe13a4ab46a5758d588dad991800c93fe48955b` for the projection test, `9f74a3ad6d8c1953f5bd91858a63f475ea80f7c11f2dfc72579544e7f61e9662` for `libneocortex_proj.a`, and `0f381b3f5b204758d1d97bbc80089116c9cc54b3b9aa116fbdccb00aa1372461` for `libneocortex_lmdb.a`.

Task 2.3 evidence:

- The real SegmentStorage boundary now seals every record payload with XChaCha20-Poly1305 before the segment log sees it and unseals only after recovery. Associated data binds the actor, LSN, kind, and user; key material follows a KEK-wrapped user key and user-key-wrapped actor data key hierarchy.
- The MMR receives the plaintext frame while the log receives independently randomized ciphertext. Two actor stores using different generated key material and nonces produced different log bytes but the exact same MMR root for the same 41 plaintext frames.
- Keyrings and nonces use only injected entropy. Reopen rejects a wrong user or KEK. Missing key material beside an existing log and any record lacking the sealed-format magic fail as typed legacy-plaintext errors.
- Crypto-deletion durably prepares an Ed25519-signed receipt, removes and syncs the keyring, publishes and syncs the receipt, zeroes in-memory keys, and makes reads and reopen fail with kKeyDestroyed. Restart completes a signed pending receipt if death occurred between unlink and publication; a forged receipt fails verification.
- The property suite covers 512 variable-size AEAD round trips plus associated-data substitution and tamper rejection. The unseal boundary completed 10,000 libFuzzer runs. The six-test Debug suite, ASan+UBSan, TSan, and zero-warning clang-tidy pass.
- Two independent static amd64 builds are byte-identical. The sealing module is at the locked `src/seal` boundary, and its test is a static AArch64 ELF with no dynamic segment. Current reproducible SHA-256 values are `33fa89e7cb062adb6f49312d017b6a6796a1610247a3622a869023ae243636c3` for the sealing test and `54882722694d9a5fe14781fb8da5a82e3d4ec271f3e495baba12cddef3b6564b` for `libneocortex_seal.a`.

Task 3.1 evidence:

- The vendored FlatBuffers schema freezes the exact 21-kind taxonomy as a typed payload union. Guidance, doubt, steering, rejected answers, narrative respawn summaries, unknown unions, future schema versions, frame-kind mismatches, and malformed payload semantics all fail with typed errors.
- Disk, socket, and import boundaries run the bounded verifier before trust. The core apply loop validates schema and ordering before durable commit and again during replay; tool results and effects must reference an earlier durable `tool_call`.
- Assertion, consolidation, and retract schemas require non-empty provenance ranges. Canonical fixtures cover every event kind, with a pinned BLAKE3 encoding golden of `1e3fd0fdec0e345f38d5ec066510624f6651664a4b9bf8058247815533736032`.
- Generated-source integrity is pinned: `schema/events.fbs` SHA-256 is `1174b7f63008ed5717dc4db3d4cb33d886ea59441566680e2d9f701e4890ee91` and `src/schema/events_generated.h` SHA-256 is `cdaf1e5ed3bd991e5ad4a1ba71d5eea78e176ea2fe66b1302ef1506f27bf627b`.
- The seven-test Debug suite, ASan+UBSan, TSan, zero-warning clang-tidy, and all four 10,000-run fuzz targets pass. Two independent static amd64 builds are byte-identical, and the schema test cross-build is a static AArch64 ELF.
- Current reproducible SHA-256 values are `f9156ec1645cf3b4b04ff0e9bf5396b9a022d1bc4b31bbe81e94ab61296dd3a4` for the schema test and `edbdfe6542b4ccbfed6efe6fdfb0c20b4ef509721336f752e7bca855fa57ad94` for `libneocortex_schema.a`.

Task 3.2 evidence:

- Direct `ProjectionStore::Apply` calls against the belief database fail with `kBeliefWriteGate`; only the event-consuming `BeliefProjection` friend can advance that projection and its checkpoint.
- Assertion and consolidation events produce typed canonical-identity heads, globally indexed belief ids, immutable version records, and exact supersession links. Replaying an identical belief id and assertion is idempotent; reusing an id with different content or identity fails typed.
- `ReadAsOf` walks the immutable chain by logical transaction time (LSN) and valid-time interval. A later version can coexist with a historical valid interval, while a retract event appends a tombstone that hides the chain for later transaction snapshots without destroying earlier `as_of` reads.
- The property corpus covers 64 independently keyed assertions duplicated across 128 ordered events, proves every duplicate remains at version 1, and reproduces the complete LMDB canonical dump byte-for-byte after reset and replay. Consolidation, supersession, tombstone, unknown-retract, direct-write, and provenance-free rejection paths are also exercised.
- All eight Debug tests pass. The real LMDB projection and belief tests pass under ASan+UBSan and TSan, and the affected strict clang-tidy build is warning-free. Two fresh static amd64 builds, including the belief executable and projection library, are byte-identical; the belief test cross-build is a static AArch64 ELF.
- Current reproducible SHA-256 values are `228cecefbb9c20dd280ba4aa848784ab4176df17989319bbf374243ef720d9f8` for the belief-store test and `67620361061e64a2dd8bf2e627f743072abb899c79848494f183b4092bbd5301` for `libneocortex_proj.a`.

Task 3.3 evidence:

- The apply-time gate interprets an explicit exclusive conflict domain: same canonical identity supersedes its live head, while different identities in the same typed domain create symmetric, transaction-time conflict intervals. Every active edge is returned with `obligated_surfacing=true`; retracting either side resolves the live edge without destroying historical as-of reads.
- Negative-existence is an additive typed assertion claim and is rejected with `kNegativeExistenceUncorroborated` unless one of its mandatory provenance ranges contains a successful `tool_result`. Failed tool results do not corroborate it, and direct belief writes remain structurally blocked.
- A real incident stream reproduces the Moltbook negative-existence poison, failed and successful corroboration, duplicate same-identity assertions, cross-identity conflict creation, symmetric obligated surfacing, retraction, historical conflict visibility, and byte-identical reset/replay.
- All eight Debug, ASan+UBSan, and TSan tests pass; clang-tidy is warning-free and all four decoder fuzzers complete 10,000 runs. Two fresh static amd64 builds compare byte-for-byte, including `libneocortex_gate.a`; the belief test and its full dependency graph cross-build as static AArch64 ELFs with no interpreter or dynamic segment.
- Current reproducible SHA-256 values are `0f5cfd3bd358280c5c417bdba234db751589734b28383c7690281d0ab4f05c29` for `libneocortex_gate.a`, `228cecefbb9c20dd280ba4aa848784ab4176df17989319bbf374243ef720d9f8` for the belief-store test, and `67620361061e64a2dd8bf2e627f743072abb899c79848494f183b4092bbd5301` for `libneocortex_proj.a`.

Task 4.1 evidence:

- The `proj/entity` writer deterministically extracts domains, URLs, paths, hex ids, structured ids, and proper names from verified typed event and assertion fields. Canonical entries map to 64-bit portable Roaring postings in the entity LMDB, so full log LSNs are preserved rather than narrowed to 32 bits.
- Query and current-turn text run through the same canonical detector plus entity-name candidates. Exact domain, full URL, path, id, proper-name, and domain-derived entity aliases return typed LSN hits; a domain-derived `Moltbook` lookup therefore cannot miss the record that stored `moltbook.com`.
- The property corpus covers all six identifier classes and 96 independently keyed domains. Every verbatim identifier returns its exact source LSN, assertion values participate alongside ordinary events, bounded rebuild resumes from checkpoint 37, and drop plus replay reproduces the canonical LMDB bytes exactly.
- All nine Debug tests and the full deterministic crash simulation pass. The entity test and instrumented CRoaring dependency pass ASan+UBSan and TSan; strict clang-tidy is warning-free. Two fresh static amd64 builds compare byte-for-byte, and the entity test cross-build is a static AArch64 ELF with no interpreter or dynamic segment.
- Current reproducible SHA-256 values are `fd45712c74e07a30cb13783ba81df0fbe5e8d76f806277c7b36b06e3653986f0` for the entity-index test, `8dffca52812cb553dbaff50ed99e3ca90c693ee5bf3f975e4ce30e8d611cf7f1` for `libneocortex_proj.a`, and `5892c474ddfac81092ace08d9448dbe176c9acfd4ae65e91b9de04e89153412d` for `libneocortex_roaring.a`.

Task 4.2 evidence:

- Embedding events append to a deterministic `vectors.flat` strip with LMDB offset metadata and per-event vector-lane checkpoints. The strip is read through a real read-only mmap, binary prefixes establish deterministic prefilter order without excluding candidates, and every compatible int8 vector is still scanned exactly.
- The Highway implementation multiplies signed int8 lanes and widens before accumulation. A scalar-reference property covers every dimension from 1 through 513; exact rankings compare score first and then target and embedding LSN, with no ANN structure or probabilistic pruning.
- The lexical projection indexes typed messages, evidence fields, and beliefs with term frequencies, document lengths, global statistics, and portable 64-bit Roaring postings. Its fixed-point BM25 ranking is fused with vector ranks through integer RRF and an LSN tiebreak. A `rareterm` record with no embedding remains retrievable through BM25.
- Reset and replay reproduce both the mmap strip and the BM25 LMDB dump byte-for-byte. A real 1,024-record, 128-dimensional mmap corpus completes an optimized static exact scan in 597 microseconds on the qualification host; hardened Debug completed in about 16 milliseconds.
- All ten Debug tests and the deterministic crash simulation pass. The full index-plane path passes ASan+UBSan, TSan, and strict clang-tidy; two fresh static amd64 builds compare byte-for-byte, and the complete test cross-builds as a static AArch64 ELF with no interpreter or dynamic segment.
- Current reproducible SHA-256 values are `2b4d25e98c62aca27fbcde66203647fd9570ba8b3c4848cad3712f8e6ac8d37c` for the index-plane test and `4b8401ebd94a9d71439d81787cfb1672f4d713d49ec9001d6285d73d99f916f6` for `libneocortex_proj.a`.

Task 4.3 evidence:

- The temporal projection derives minute, hour, day, and week windows from verified event wall timestamps and stores full 64-bit LSN membership as portable Roaring bitmaps. Signed timestamps use deterministic floor windows, while sortable big-endian signed keys permit direct range seeks without scanning older history.
- Read snapshots expose ordered window listing, recursive week-to-day-to-hour-to-minute descent, and exact sorted member resolution. Every handle validates its level and fixed window width, and malformed projection values fail with `kTemporalLadderCorrupt`.
- A 256-event property corpus spans pre- and post-epoch boundaries, exercises bounded rebuild/resume, every resolution, recursive descent, and exact member counts. Dropping the projection and replaying it repairs missing tiers byte-identically; deliberately injecting stale state and an advanced checkpoint is also repaired byte-identically by reset/replay.
- All eleven Debug tests and the deterministic crash simulation pass. The ladder path passes ASan+UBSan, TSan, and strict clang-tidy; two fresh static amd64 builds compare byte-for-byte, and the test cross-builds as a static AArch64 ELF with no interpreter or dynamic segment.
- Current reproducible SHA-256 values are `b6009c5bd0b4d9568c1ac51cfa854850814ee834a485b11fddb0f315707388cb` for the temporal-ladder test and `b104e7e47528cc1300c190b5e5e647990c2d363f0b485b4347803bce21288a51` for `libneocortex_proj.a`.

Task 5.1 evidence:

- The intent projection retains the latest conversation objective with actor-global fallback, globally unique typed loop identities, every open loop, and immutable typed close records. A loop leaves the active frame only through `loop_closed`; mismatched conversations, duplicate IDs, and missing loops fail typed.
- The work ledger stores tool calls and each effect as separate ordered entries, preserving call IDs, tool names, arguments, outcomes, and state evidence. Tool results and effects must resolve their durable write-ahead call; outcomes must resolve an effect. Dispatched, committed, and outcome-unknown entries derive `requires_reconciliation=true`, while returned entries do not.
- Reverse LMDB prefix scans return the newest bounded ledger tail without historical full scans. Both projections advance on every LSN, resume from independent checkpoints, and reproduce canonical bytes exactly after reset/replay.
- A multi-loop, multi-tool property reboots fresh projection stores at every durable prefix and checks the exact objective, open loops, ledger membership, and reconciliation state. A child process also commits both projections through LSN 10, dies by real `SIGKILL`, and resumes to the byte-identical full state.
- All twelve Debug tests and the deterministic crash simulation pass. The continuity path passes ASan+UBSan, TSan, and strict clang-tidy; two fresh static amd64 builds compare byte-for-byte, and the test cross-builds as a static AArch64 ELF.
- Current reproducible SHA-256 values are `3061bb3bb7e295e177ec778124762bc23452966833601ced21bd8fae6199db5b` for the continuity projection test and `2da260a25717266d16e7f8d758f80a8571ec481a24da3fdc30cb7f248b2bf40c` for `libneocortex_proj.a`.

Task 5.2 evidence:

- `Composer::Activate` reads one epoch-pinned snapshot and emits eight typed sections in the frozen order: resident beliefs, intent, ledger, conversation, entity hits, conflict obligations, fused lexical/vector hits, and temporal handles. Every item has a typed tier, stable URI, log-LSN provenance, content, and caller-model token spend.
- The conversation projection now retains every per-conversation typed event plus its latest head and a global LSN index. It remains derived and byte-rebuildable, while recent bounded reads seek backward directly rather than scanning from conversation start.
- Budgeting invokes the caller callback for each structured item. T7 through T4 trim first in reverse tier order; T3 then coarsens payloads to event-kind/LSN handles without dropping a turn; T0 through T2 are never trimmed, and an over-budget protected set returns `kInvariantViolation`.
- Resident identity/constraint/goal heads come from the belief projection. Active conflict pairs are surfaced together when their evidence surfaces. Lexical and exact-vector ranks use the existing deterministic RRF lane. Temporal handles contain exact member-LSN provenance. Used/ignored feedback emits schema-verified attestation frames.
- Identical snapshot, request, and budget produce equal bundles and a pinned canonical FNV-1a golden of `16555579470815848931`. Hard request candidate, conversation-record, and query-size bounds reject unbounded request growth.
- All thirteen Debug tests pass. The composer path passes ASan+UBSan, TSan, and strict clang-tidy; two fresh static amd64 builds compare byte-for-byte, and the composer test cross-builds as a static AArch64 ELF.
- Current reproducible SHA-256 values are `1e2c71a18b3d507f79a9a1edfb3ce22f89db0b6a15f95ad86a1ca16c16640be1` for the composer test, `2dc66bdbd4fd4ef2bf16f9d07dbef915259d0c35013dc143e552749e9a1286c2` for `libneocortex_activate.a`, and `260276c165fef216aa5cf1c99abf2e881392ee15467059731ef9df6fc6b7b19d` for `libneocortex_proj.a`.

Task 5.3 evidence:

- A 13-LSN scripted workload includes objective replacement, two explicit loops, user and delivered turns, two tool calls, dispatched and committed effects, known return/outcome, and an outcome-unknown crash case.
- At every LSN, a fresh child builds all seven participating projections through exactly that durable prefix, signals completion, and dies by real `SIGKILL`. A successor process opens the same actor store and immediately composes an activation without predecessor narrative state.
- Every successor activation contains exactly every durable conversation event, the current objective, every not-explicitly-closed loop, and every tool/effect entry. Dispatched, committed, and outcome-unknown work is marked for reconciliation; known returned work is not.
- Each prefix is recomposed under a budget forcing all enrichment removal and conversation coarsening. Every turn remains represented by its typed event/LSN handle. At the full prefix, dropping and replaying the conversation projection leaves all eight activation sections byte-equivalent.
- All thirteen Debug tests pass. The literal kill-at-every-LSN path passes ASan+UBSan, TSan, and strict clang-tidy; two fresh static amd64 builds compare byte-for-byte, and the property cross-builds as a static AArch64 ELF.
- Current reproducible SHA-256 values are `e823819d957f894630badd6a7e0c6c901598c82d1f885d2d4164fe3e89b79fa6` for the INV-1 continuity test and `2dc66bdbd4fd4ef2bf16f9d07dbef915259d0c35013dc143e552749e9a1286c2` for `libneocortex_activate.a`.

Next work is Task 7.2: run the fixed-incident/INV-1 qualification corpus and real dev-daemon cortexd loss/restart conversations, then record exact evidence before any consumer re-point or production gate.
