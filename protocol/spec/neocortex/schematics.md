# NEOCORTEX — Schematics

This document is the **authoritative structural ground truth** for the neocortex build. Every component's boundaries, data flow, and naming are defined here; `spec.kvx` tasks cite these sections by number. If an implementation needs to deviate from a schematic, that is an owner decision — stop and surface (see `agent.lock.kvx`). Diagrams are normative, prose here is explanatory; the design rationale lives in `design.body.md`.

## 1. System overview

```text
┌────────────────────────────── user VM (per-user boundary) ──────────────────────────────┐
│                                                                                         │
│  neo daemon (Go)                 executor daemon (Go)              MCP tooling (Go)     │
│  ┌─────────────────────┐         ┌──────────────────┐             ┌──────────────────┐  │
│  │ runtime loop        │         │ cortex routes    │             │ recall/remember/ │  │
│  │  ActivationSource ──┼──┐      │ (daemon_cortex_* ├──┐          │ guard/search     ├─┐│
│  │  TurnRecorder     ──┼──┤      └──────────────────┘  │          └──────────────────┘ ││
│  │  EvidenceJournal  ──┼──┤                            │                               ││
│  │  Consolidator sink──┼──┤   all consumers are CLIENTS of one brain                   ││
│  │  CheckpointStore  ──┼──┤                            │                               ││
│  └─────────────────────┘  │                            │                               ││
│            matrix cortexclient (Go pkg)                │                               ││
│                           │                            │                               ││
│                           ▼                            ▼                               ▼│
│                 ╔══════════════════════════════════════════════════════════╗           │
│                 ║  unix domain socket  /data/neocortex/cortexd.sock        ║           │
│                 ║  versioned FlatBuffers framing + capability tokens       ║           │
│                 ╚═══════════════════════════╤══════════════════════════════╝           │
│                                             │                                          │
│                                   cortexd (C++23 engine)                               │
│                                   NO network egress, NO model calls                    │
│                                             │                                          │
│                                             ▼                                          │
│                              /data/neocortex/<actor>/   (disk, sealed)                 │
└─────────────────────────────────────────────────────────────────────────────────────────┘
```

Policy/mechanism split: everything above the socket may call models and the network; nothing below it can.

## 2. Repository and module layout

```text
neocortex/                          # new top-level module (C++)
├── CMakeLists.txt                  # pinned toolchain + flags (agent.lock.kvx [lock.toolchain])
├── toolchain/                      # toolchain file, clang-tidy config, sanitizer presets
├── third_party/                    # vendored, digest-pinned deps ONLY (lock list)
│   ├── liburing/  lmdb/  blake3/  libsodium/  flatbuffers/  croaring/  highway/  hash/
├── schema/
│   ├── events.fbs                  # THE event schema (kind taxonomy, agent.lock frozen)
│   └── protocol.fbs                # socket protocol schema
├── src/
│   ├── core/                       # deterministic state machine (NO clock/rand/fs — env only)
│   │   ├── env.h                   # injected Clock/Entropy/Storage interfaces
│   │   ├── apply.{h,cc}            # single-writer apply loop + gate dispatch
│   │   └── error.h                 # typed error taxonomy (std::expected payloads)
│   ├── log/                        # segment files, frame codec, group commit, tail recovery
│   ├── seal/                       # vault AEAD (XChaCha20-Poly1305), KEK→UK→DEK
│   ├── mmr/                        # BLAKE3 MMR, signed checkpoints, offline verifier
│   ├── proj/                       # projection substrate (LMDB envs, checkpoints, rebuild)
│   │   ├── beliefs/  entity/  vectors/  lexical/  ladder/  intent/  ledger/  convo/
│   ├── compose/                    # activation composer (tiers, budget, golden vectors)
│   ├── gate/                       # write-gate semantics (upsert, conflict, poisoning rules)
│   ├── serve/                      # cortexd: uds server, sessions, capabilities, admin
│   └── sim/                        # deterministic simulation harness + fault models
├── fuzz/                           # libFuzzer targets + in-tree corpora (one per decoder)
├── test/                           # unit + property + golden-vector suites
└── cmd/
    ├── cortexd/                    # the daemon
    └── cortex-verify/              # offline MMR/AEAD verifier CLI

cortexclient/                       # new top-level Go module (module centra/core/cortexclient)
├── client.go                       # conn mgmt, framing, idempotent append, reconnect
├── loopseam.go                     # ActivationSource/TurnRecorder/EvidenceJournal/
│                                   #   Consolidator-sink/CheckpointStore implementations
├── admin.go                        # admin capability client
└── export/                         # wave-7 Pebble exporter (imports old centra/core/cortex)
```

## 3. On-disk layout and frame format

```text
/data/neocortex/<actor>/
├── log/
│   ├── 00000000000000000001.seg    # bounded segment files (default 128 MiB)
│   ├── 00000000000000000002.seg
│   └── MANIFEST                    # segment index: first/last lsn per segment, crc
├── proj/                           # LMDB environments — ALL disposable (rebuild law)
│   ├── beliefs.lmdb/  entity.lmdb/  lexical.lmdb/  ladder.lmdb/
│   ├── intent.lmdb/   ledger.lmdb/  convo.lmdb/
│   └── vectors.flat                # mmap flat vector file + int8/binary strips
├── mmr/
│   ├── peaks                       # write-once peak nodes
│   └── checkpoints/                # signed roots: <lsn>.ckpt (ed25519)
└── keys/                           # sealed DEK wrapping material (vault hierarchy)

Frame (fixed header, little-endian, followed by sealed payload):
┌─────────┬─────────┬─────────┬────────┬───────────┬──────────┬───────────┬─────────────────────┐
│ len u32 │ crc u32 │ lsn u64 │ kind u8│ ts_ns i64 │ actor u16│ conv u128 │ sealed FB payload…  │
└─────────┴─────────┴─────────┴────────┴───────────┴──────────┴───────────┴─────────────────────┘
  len   = total frame length          crc  = crc32c over header(after crc)+sealed payload
  kind  = frozen taxonomy id (§4)     conv = conversation id hash (0 = actor-global)
  AEAD AD = (actor, lsn, kind); frame hash for MMR = BLAKE3 over PLAINTEXT payload + header
  Write path: io_uring + O_DIRECT, group commit; barrier = fdatasync on segment + MANIFEST
  Boot: tail walk → verify crc → truncate ≤1 torn tail frame → interior corruption = refuse
```

## 4. Event kind taxonomy (frozen — changes need spec amendment + owner YES)

```text
conversation                     work                       intent                 memory
├─ user_msg                      ├─ effect                  ├─ intent_set          ├─ assertion
├─ delivered_msg   (ONLY         ├─ approval                ├─ loop_opened         ├─ consolidation
│   user-visible output)         ├─ outcome                 └─ loop_closed         ├─ embedding
├─ tool_call       (durable      ├─ checkpoint                  (reason: done|     ├─ retract
│   BEFORE execution)            ├─ supervisor                   abandoned|        └─ attestation
├─ tool_result                   └─ recovery                     handed_off|
├─ reasoning                                                     superseded)
├─ provider_frame  (exact
│   provider bytes — absorbs
│   sessionjournal api_content)
└─ media_ref

FORBIDDEN (no kind exists, cannot enter the log):
  guidance, doubt/silent-voice lines, steering, rejected/undelivered answers,
  narrative respawn summaries.
```

## 5. State machine, threads, and dataflow

```text
                         clients (uds)
                              │ append(events, client_seq)      queries
                              ▼                                    │
                    ┌──────────────────┐                           │
                    │  serve/ session  │  idempotency: client_seq  │
                    └───────┬──────────┘  dedup per connection     │
                            ▼                                      ▼
   WRITER THREAD (exactly one)                        READER THREADS (N)
   ┌───────────────────────────────────────┐          ┌───────────────────────────┐
   │ 1. validate (schema verify, kind law, │          │ epoch-pinned LMDB read    │
   │    write-ahead ordering rules)        │          │ txns; never block writer; │
   │ 2. belief-policy admission probe      │          │ compose/ queries and      │
   │    (snapshot + earlier batch events;  │          │ descent reads run here    │
   │    typed reject, NO mutation)         │          └───────────────────────────┘
   │ 3. seal payload (AEAD)                │
   │ 4. append frame → group commit        │
   │ 5. MMR append (plaintext hash)        │
   │ 6. APPLY: gate + projections          │
   │    (beliefs, entity, lexical, ladder, │   core/env.h — the ONLY doors to the
   │     intent, ledger, convo heads,      │   world: Clock, Entropy, Storage.
   │     vectors strip)                    │   prod env = real io_uring/clock;
   │    policy-invalid legacy/imported     │   sim env = deterministic fault-
   │    events are deterministic skips     │   injecting harness (src/sim/).
   │ 7. advance per-proj applied-lsn       │
   │ 8. ack(client_seq, lsn)               │
   └───────────────────────────────────────┘
                            │
                 IO THREAD (io_uring completions, group-commit batching)

   Boot sequence:  verify tail → truncate torn frame → load proj checkpoints
                   → replay [min(checkpoint)+1 … tail] through APPLY
                   → mark in-flight effects "needs reconciliation" (ledger)
                   → serve.
   Rebuild law:    drop any proj env → replay from lsn 0 → byte-identical state
                   (simulation CI proves convergence incl. crash-mid-rebuild).
```

## 6. Belief store and write gate

```text
   socket append batch                         committed/replayed event (APPLY)
   ┌──────────────────────────┐                 ┌──────────────────────────────┐
   │ admission probe against  │                 │ run the same deterministic   │
   │ snapshot + earlier batch │                 │ gate; mutate projections only│
   │ events; NO durable writes │                 │ here. Policy rejection = skip│
   └────────────┬─────────────┘                 └──────────────┬───────────────┘
                └──────────────────────┬───────────────────────┘
                                       ▼
                      ┌─────────────────────────┐
                      │  gate/ (deterministic)  │
                      │                         │
              ┌───────┤ 1. provenance present?  │── absent ──▶ typed reject/skip
              │       │ 2. negative-existence?  │── yes, no tool_result
              │       │                         │    in provenance ──▶ reject/skip
              │       │ 3. canonical identity   │
              │       │    resolution           │
              │       └───────────┬─────────────┘
              │                   │
              │        same identity?           different identity, contradicts?
              │                   │                          │
              │                   ▼                          ▼
              │        ┌──────────────────┐       ┌─────────────────────────┐
              │        │ typed UPSERT:    │       │ conflict edge (typed);  │
              │        │ supersession     │       │ BOTH heads live; edge   │
              │        │ chain, version+1 │       │ is obligated-surfacing  │
              │        └──────────────────┘       │ in the composer         │
              │                                   └─────────────────────────┘
              ▼
   belief head (proj/beliefs):
   { id(canonical), type, head_version, valid_from/until, tx_time,
     provenance: [lsn ranges]  ◀── REQUIRED by schema, unrepresentable without,
     supersedes: id@ver, conflict_edges: [..], tombstoned }
   as_of(valid_t, tx_t) reads walk the version chain deterministically.
```

## 7. Index plane

```text
                      APPLY (writer thread, per event/belief)
        ┌─────────────────┬──────────────────┬─────────────────┬────────────────┐
        ▼                 ▼                  ▼                 ▼                ▼
 ┌──────────────┐  ┌──────────────┐  ┌───────────────┐  ┌─────────────┐  ┌───────────┐
 │ entity/      │  │ lexical/     │  │ vectors/      │  │ ladder/     │  │ intent/ + │
 │ DETERMINISTIC│  │ BM25 postings│  │ int8 strip +  │  │ wall-clock  │  │ ledger/   │
 │ id extraction│  │ over roaring │  │ binary prefix │  │ windows w/  │  │ (see §9)  │
 │ domains/urls/│  │ bitmaps      │  │ strip, mmap   │  │ descent     │  └───────────┘
 │ paths/ids/   │  └──────────────┘  │ flat file     │  │ handles     │
 │ names →      │                    │ (embedding    │  └─────────────┘
 │ exact-match  │                    │  events only) │
 │ roaring table│                    └───────────────┘
 └──────────────┘
 Query side:
   entity lane   : detect ids/entities in query+turn → exact lookup → GUARANTEED surfacing
   vector lane   : binary prefilter → int8 SIMD (highway) EXACT scan → recall = 1.0
                   (NO HNSW / ANN structures — banned by agent.lock)
   lexical lane  : BM25 → fuse with vector via RRF, deterministic tiebreak
   ladder lane   : list windows → open window → resolve members (RLM descent)
```

## 8. Activation composer

```text
 Activate(conv, query, budget, token_model)          — pure read, reader thread
 ────────────────────────────────────────────────────────────────────────────
 tier  source                              trim class
 ────  ─────────────────────────────────  ───────────────────────────────────
  T0   resident: identity, hard            NEVER TRIMMED. If T0+T1+T2 alone
       constraints, active goals           exceed budget → typed invariant
  T1   intent frame: objective +           violation (NOT a trim) — surfaces
       ALL open loops                      to the client as an engine error.
  T2   work ledger tail (reconciled
       effect states, this conv)
 ────  ─────────────────────────────────  ───────────────────────────────────
  T3   conversation projection             coarsened second (recent verbatim,
       (window over convo heads +          older via ladder summaries; events
        provider-faithful frames)          remain addressable, never dropped)
 ────  ─────────────────────────────────  ───────────────────────────────────
  T4   entity-lane hits (guaranteed)       trimmed first, in reverse-tier
  T5   conflict-obligated beliefs          order (T7 → T6 → T5 → T4); every
  T6   RRF-fused semantic+lexical          trim recorded in bundle.trimmed
  T7   ladder descent handles (URIs)
 ────────────────────────────────────────────────────────────────────────────
 Output bundle: typed sections, every item carries {uri, provenance lsns,
 tier, tokens}; per-tier spend + trim report; rendering is the CLIENT's job.
 Determinism: same log prefix + query + budget ⇒ byte-identical bundle
 (golden vectors). Attestation (used/ignored) returns as attestation events.
```

## 9. INV-1 recovery sequence

```text
   kill -9 at ANY lsn (simulation runs literally every lsn; CI hard gate)
        │
        ▼
 ┌─ boot ────────────────────────────────────────────────────────────────┐
 │ verify tail → truncate ≤1 torn frame → replay proj deltas             │
 │ ledger: any tool_call without tool_result/outcome → "in-flight,       │
 │         needs reconciliation" (NEVER silently done, NEVER lost)       │
 └───────────────┬───────────────────────────────────────────────────────┘
                 ▼
        next Activate(conv) MUST contain (asserted per-lsn in CI):
        1. conversation: every exchanged turn (coarsened, never dropped)
        2. ledger tail: every effect w/ reconciled or needs-reconcile state
        3. intent frame: current objective + EVERY open loop
        4. briefing source = projections only (no narrative summary kind
           exists, so predecessor prose CANNOT be the successor's authority)

 Loop lifecycle (closure discipline):
   loop_opened ──▶ appears in EVERY activation ──▶ loop_closed(reason)
                       ▲                              done | abandoned(cause)
                       └── crash/respawn/overflow ────  | handed_off | superseded
                           cannot remove it             (explicit event ONLY)
```

## 10. Socket protocol

```text
 frame: [len u32][crc32c u32][FlatBuffers Request|Response|Event]
 handshake: Hello{proto_version, capability_token} → Welcome{actor_ns, limits}
            version mismatch → typed reject (client surfaces upgrade need)

 requests (capability: actor)          requests (capability: admin)
 ├─ Append{events[], client_seq}       ├─ Health / Stats / LatencyHistograms
 ├─ Activate{conv, query, budget}      ├─ VerifyStatus (MMR root, last ckpt)
 ├─ Transcript{conv, since, limit}     ├─ RebuildProjection{name}
 ├─ Recall{query, opts} (descent)      └─ CryptoDelete{actor}  (owner-gated
 ├─ AsOf{query, valid_t, tx_t}             upstream; engine executes only)
 ├─ Checkpoint{turn_id, blob} / LatestCheckpoint{turn_id}
 ├─ Attest{used[], ignored[]}
 └─ Subscribe{conv|actor, since_lsn} → server-push Event stream
 Guarantees: Append idempotent by (connection, client_seq); ack carries lsn;
 reconnect resumes: acked writes never lost, unacked never double-applied.
```

## 11. Neo hard cutover flow (wave 7)

```text
 Neo source + module graph                 one permitted memory path
 ┌────────────────────────┐               ┌──────────────────────────────┐
 │ no centra/core/cortex import│               │ Neocortex-native contracts   │
 │ no cortex-root config  │──────────────▶│ → UDS client → cortexd       │
 │ no substrate selector  │               │ → /data/neocortex/<actor>    │
 │ no legacy proxy/fallback│              └──────────────────────────────┘
 └────────────────────────┘
             │
             └─ structural CI: source scan + complete Go module graph rejection

 cortexd unavailable → typed Neocortex failure → supervisor restart/reconnect
                    NEVER → Cortex-v1 store, route, proxy, or compatibility branch

 Cortex v1 may remain for unrelated Matrix consumers only outside Neo's source,
 dependency graph, build artifacts, configuration, and runtime reachability.
```
