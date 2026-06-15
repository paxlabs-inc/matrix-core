# Snapshot & Proofs

`cortex/snapshot` manages the cryptographic commitment layer: a journal MMR (Merkle Mountain Range) over every write, per-namespace SMTs (Sparse Merkle Trees) over canonical memory and edge records, and `SnapshotManifest` values that seal a point-in-time `OverallRoot`. `cortex.Proof` builds multi-proofs for sub-agent scope shipping.

Source files: `cortex/snapshot/snapshot.go`, `cortex/snapshot/mmr.go`, `cortex/snapshot/smt.go`, `cortex/snapshot/multiproof.go`.

---

## Design decisions

**Two anchored namespaces: `memories` and `edges`.** The SMT covers the canonical bytes of `m/<id>` (MemoryHead) and `e/from/<src>/<t>/<dst>` (EdgeRecord). Tombstones fold into the parent canonical bytes — a tombstoned Head carries `Tombstoned` set; the SMT value-hash changes. Salience, forms, and embedding metadata are NOT anchored (derived state).

**Cortex never signs.** The `SignedBy` field in `SnapshotManifest` is populated by the agent runtime or `tools/attest` layer before chain anchoring. Cortex holds no key material (D4).

**Pull-driven snapshots.** `cortex.Snapshot(reason)` is called explicitly by the skill compiler, `tools/attest`, or a periodic snapshotter. There is no auto-snapshot. The `reason` is recorded in the manifest for ops visibility but does NOT feed `OverallRoot`.

**MMR hook is always running.** `cortex.New` installs a `JournalHook` on the store. Every `AppendJournal` call — Write, Update, Tombstone, AddEdge, Embed, Compact, Attest, everything — atomically appends an MMR leaf in the same Pebble batch.

---

## OverallRoot

```go
root, err := c.OverallRoot()  // [32]byte
```

`OverallRoot` is a single 32-byte hash that commits to the entire cortex state:

```
OverallRoot = sha256(
    journal_root   ||    // sha256 over all MMR peaks
    memories_root  ||    // SMT root over m/<id> canonical bytes
    edges_root           // SMT root over e/from/* canonical bytes
)
```

This is the `cortex_snapshot_hash` used in the D11 compiler seed:
```
D11 seed = sha256(intent.id || actor || OverallRoot || mtx_digest || model_digest)
```

---

## Snapshot

```go
manifest, err := c.Snapshot("pre-compile")
```

Persists a `SnapshotManifest` at `snap/<seq>` and returns it. The manifest captures:

```go
type Manifest struct {
    SeqAtSnapshot uint64
    CreatedAt     time.Time
    Trigger       string      // "pre-compile", "post-attest", etc. — NOT in OverallRoot
    Actor         string
    SignedBy      string      // populated by agent runtime before chain anchor
    JournalRoot   [32]byte
    StateRoots    map[string][32]byte  // {"memories": root, "edges": root}
    OverallRoot   [32]byte
    // sanity counters (not in OverallRoot):
    MemoryCount   uint64
    EdgeCount     uint64
    TombstonedCount uint64
}
```

---

## Journal MMR

The journal MMR is an append-only Merkle accumulator over every journal entry's leaf hash. Peaks are persisted under `accum/mmr/`.

```
leaf_hash = sha256("matrix.cortex.journal.v1" || canonical_CBOR_entry)
```

Each `AppendJournal` call (inside a `WriteBatch`) invokes the installed `MMRHook`, which stages the leaf into the accumulator in the same atomic Pebble batch. The `journal_root` component of `OverallRoot` is derived from the current MMR peak set.

---

## Per-namespace SMTs

Two sparse Merkle trees track canonical state:

**`memories` SMT:** keyed by `sha256(m/<id:16>)` → value is `sha256(canonical_Head_CBOR)`. Updated on every `Write`, `Update`, `Tombstone`, `UpdateHead`, and embedder `KindEmbed` (the embedder rewrites `m/<id>` with `EmbeddingRef` set).

**`edges` SMT:** keyed by `sha256(src || edgeTypeByte || dst)` → value is `sha256(canonical_EdgeRecord_CBOR)`. Updated on every `AddEdge` and `RemoveEdge`.

SMT node cache lives under `idx/smt/`, which is in the derived (droppable) namespace. Rebuild re-derives the SMTs from the canonical `m/` and `e/from/` keys.

---

## Multi-proofs for sub-agent scopes

```go
// 1. Take a snapshot (fixes the root the proof verifies against)
manifest, _ := c.Snapshot("for-scope")

// 2. Build a multi-proof for the URIs the sub-agent needs
proof, err := c.Proof([]memory.URI{uri1, uri2}, manifest)

// 3. Include the proof in the sub-agent's CortexScope
scope := &scope.Scope{
    SnapshotHash: manifest.OverallRoot,
    Proofs:       proof,
    // ... other fields
}
```

`Proof` returns a `snapshot.MultiProof` — a compact proof bundle that lets the sub-agent verify its included keys against `manifest.OverallRoot` without trusting the API.

If the memories SMT root has drifted since `manifest` was taken (i.e. new writes happened), `Proof` returns `ErrManifestRootMismatch`. Re-Snapshot before re-Proof.

### MultiProof structure

```go
type MultiProof struct {
    Root   [32]byte
    Proofs []SingleProof   // one per requested key
}

type SingleProof struct {
    KeyHash  [32]byte      // sha256(memory ULID bytes)
    Leaf     []byte        // canonical Head CBOR (nil = non-membership proof)
    Siblings [][]byte      // sibling nodes along the SMT path
}

func (mp *MultiProof) VerifyAgainstManifest(m *Manifest) error
```

---

## Finding a snapshot by root

```go
manifest, err := c.snap.FindSnapshotByRoot(hash)
// err == snapshot.ErrSnapshotNotFound if no manifest has that OverallRoot
```

Used by the scope verifier to resolve `Scope.SnapshotHash` against existing manifests. Returns the manifiest whose `OverallRoot` matches `hash`.

---

## CurrentRoots

```go
journalRoot, stateRoots, overallRoot, err := c.snap.CurrentRoots()
```

Returns the live roots without persisting a manifest. Used internally by `OverallRoot()` and the rebuild verifier.

---

## Modifying snapshots

| What to change | Where |
|---|---|
| Add an anchored namespace | `snapshot/smt.go` — new SMT; `snapshot/snapshot.go` — add to `StateRoots`; `replay/rebuild.go` — re-derive in rebuild |
| Change MMR hash domain prefix | `journal/journal.go` — `LeafDomain` — invalidates all existing journals |
| Add fields to SnapshotManifest | `snapshot/snapshot.go` — bump schema version; add to CBOR key map |
| Change which keys are derived vs canonical | `replay/drop.go` — `derivedPrefixes`; document whether the new key participates in OverallRoot |
