# Canonical memory intake

`MemoryService::ingest_committed_entry` accepts an opaque receipt from the real
session writer. It makes current ingress available without advancing replay past
older history. `ingest_committed_page` accepts a profile-checked, checksum-verified
append-order page from `SessionStore`; its input cursor must match the saved
cursor. Production has no raw `SessionEntry` queue that can attest arbitrary text.

The existing memory vault owns evidence and source commitments. A
`SourceCommitted` event records a session/entry/checksum receipt and creates no
claim, atlas anchor, or independent supporting episode. Commitment conflicts fail
closed. A completed canonical append remains committed if atlas replacement fails;
`repair_ingestion_projection` and subsequent reads retry the derived projection.

`.keith/memory-source-cursors.json` is a disposable checkpoint. Intake commits the
vault before saving cursor progress. A crash or failed checkpoint write leaves
replay safe to repeat. Pending final/compaction text and delayed lineage are
checked against canonical vault commitments before reuse; a recomputed checksum
inside this disposable file cannot attest different text. Corrupt JSON is
quarantined for canonical replay. Invalid profile, version, or committed-source
identity is an explicit error.

Complete `AssistantFinal` bytes wait for their matching terminal and referenced
outbox/snapshot records. A compaction summary waits for its matching checkpoint.
The summary and checkpoint are generated representations with the same original
context roots. Copying the checkpoint retains those roots. Final candidates do
not enter the evidence vault. These checks preserve intake eligibility; they do
not certify all turn finalization transactions or delivery behavior.

`EvidenceCausalMetadata.source_roots` and `derived_from` describe context lineage,
not positive support for every claim in a summary. Exact quotation retains source
authority; rewritten memory is `DerivedInference`. Memory-tool results that carry
canonical records are transclusions and acquire no fresh observation root.
Unavailable, deleted, conflicting, unsupported, and limited origins remain
explicit gaps. Late original sources can repair copied/summary lineage through
append-only annotations, without upgrading an already generated record. Existing
roots and effective intervals survive repair.

Legacy vault bytes remain unchanged. Canonical replay recognizes the older
compaction-summary identity and annotates that record in place. Legacy generated
memory-tool observations can be conservatively downgraded through an annotation.
Legacy synthetic digest arrays are not accepted as exact citations. Consolidation
waits for actual canonical source checksums instead of manufacturing digests;
daily and unresolved records retain the checkpoint's source range. Historical
compaction drafts that were never durably stored cannot be reconstructed exactly.

The checkpoint is capped at 8 MiB and 4,096 sessions, with at most 1,024 pending
entries per session. Each intake retries at most 128 delayed lineage entries with
a durable rotation cursor. Root/reference/gap sets are capped at 256 and expose
limit gaps. Oversized evidence projections report a consumed gap so later valid
entries can progress. The session reader independently bounds source pages.
These bounds do **not** bound existing full manifest discovery or whole-vault
refresh/projection work.

A process-shared vault lock refreshes canonical state before reads and writes.
Short contention waits up to two seconds, then reports `Busy`; no stale snapshot
is substituted. Exact create/correct/forget citations and targets are checked
again while that lock is held. The separate ingestion lock serializes checkpoint
progress and can report `IngestionBusy`. Source commitments remain internal vault
metadata and must participate in future retention/invalidation work.

Focused validation uses real temporary stores:

```sh
CARGO_TARGET_DIR=/tmp/keith-causal-memory21 CARGO_INCREMENTAL=0 cargo test -p keith-memory --locked
CARGO_TARGET_DIR=/tmp/keith-causal-memory21 CARGO_INCREMENTAL=0 cargo clippy -p keith-memory --all-targets --no-deps --locked -- -D warnings
```

The real daemon/worker/provider/Web qualification is separate from these crate
checks and is owned by the active causal-intelligence qualification suite.
