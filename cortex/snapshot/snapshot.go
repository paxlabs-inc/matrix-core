// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Snapshot orchestration: State coordinates the journal MMR plus per-namespace
// SMTs, produces SnapshotManifest values, and persists them under snap/.
//
// Spec: research/04-cortex.md §7. Locks adopted sess#7 (per matrix.ctx
// phase7_locked_design):
//
//   - 2 anchored namespaces ("memories", "edges"). Tombstones folded into
//     parent canonical bytes; spec_divergence recorded in matrix.ctx.
//   - SignedBy field is metadata only; cortex never signs (no key
//     material at this layer). The tools/attest layer or agent runtime
//     populates + signs the manifest before chain anchoring.
//   - Counters (memory count, edge count, tombstoned count) are sanity
//     fields for replay; NOT inputs to OverallRoot.
//   - Pull-driven snapshots: explicit Cortex.Snapshot(reason) call;
//     periodic via optional StartSnapshotter ticker (cortex.go).
//
// Replay invariant (research/04-cortex.md §13.4):
//
//	Drop(indexes/) — i.e. accum/ + idx/* + salience/ + vec/ +
//	                meta/embed_cursor + meta/embed_vertex_next.
//	                snap/ is canonical (research/04 §1) and is NOT dropped.
//	For each entry in j/<seq> in order:
//	    apply(entry) → mutates indexes only
//	Verify(state_roots equal) against latest snap/<seq>
//
// In Phase 11 the rebuild reads canonical state (m/, mv/, e/, j/) and
// re-emits idx/* + salience/ + accum/mmr/* + idx/smt/<ns>/* keys. The
// kept canonical Head bytes feed StageMemoryUpdate verbatim, so the
// rebuilt memories_root is byte-identical to the pre-drop value
// (analogous holds for edges_root and journalRoot). Tested in
// cortex/replay/replay_test.go and cortex/rebuild_test.go.

package snapshot

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/cockroachdb/pebble"
	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/keys"
	"matrix/cortex/store"
)

// Setter is the minimal write-side interface MMR + SMT need. *store.WriteBatch
// satisfies it directly; *pebble.Batch is wrapped via PebbleBatchSetter so
// the snapshot layer can stage writes from a Store.JournalHook (which is
// invoked with a raw *pebble.Batch).
type Setter interface {
	Set(key, value []byte) error
	Delete(key []byte) error
}

// BatchedReader is an optional capability on a Setter: when present, the
// MMR cascade reads sibling nodes through the reader so in-batch staged
// nodes are visible (required for Phase 12 multi-AppendJournal atomic
// batches — cortex.Attest stages two journal entries + two MMR leaves
// in one batch, and the second leaf's cascade may need to merge with
// the first leaf which was just staged). Implementations MUST consult
// the in-flight batch first, falling back to the committed store.
type BatchedReader interface {
	GetBatched(key []byte) ([]byte, bool, error)
}

// PebbleBatchSetter wraps *pebble.Batch with the Setter interface. The Pebble
// batch's native Set/Delete take an extra *pebble.WriteOptions argument; this
// adapter passes nil (the batch's commit time options control durability,
// not the per-Set call).
//
// When b is an indexed batch (created via NewIndexedBatch — the path used
// by store.BeginWrite), PebbleBatchSetter additionally implements
// BatchedReader so in-batch reads see staged-but-uncommitted writes.
// Non-indexed batches (used directly in some snapshot tests and the
// replay rebuild loops) Get returns pebble.ErrNotIndexed, which
// PebbleBatchSetter surfaces as an error from GetBatched.
type PebbleBatchSetter struct{ b *pebble.Batch }

// NewPebbleBatchSetter returns a Setter view over b.
func NewPebbleBatchSetter(b *pebble.Batch) *PebbleBatchSetter {
	return &PebbleBatchSetter{b: b}
}

// Set implements Setter.
func (p *PebbleBatchSetter) Set(k, v []byte) error { return p.b.Set(k, v, nil) }

// Delete implements Setter.
func (p *PebbleBatchSetter) Delete(k []byte) error { return p.b.Delete(k, nil) }

// GetBatched implements BatchedReader. Returns (nil, false, nil) on miss
// (key not in batch) or on a non-indexed batch (pebble.ErrNotIndexed) so
// callers transparently fall back to store reads.
func (p *PebbleBatchSetter) GetBatched(key []byte) ([]byte, bool, error) {
	val, closer, err := p.b.Get(key)
	if err == pebble.ErrNotFound || err == pebble.ErrNotIndexed {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	defer closer.Close()
	out := make([]byte, len(val))
	copy(out, val)
	return out, true, nil
}

// SchemaVersion is the SnapshotManifest schema version. Bumped on any
// change that would alter the OverallRoot computation; downstream
// consumers (anchoring contracts) MUST gate on this.
const SchemaVersion uint8 = 1

// AnchoredNamespaces are the namespaces folded into OverallRoot. Order
// is fixed (alphabetical) for canonical encoding. Adding a namespace is
// a SchemaVersion bump.
var AnchoredNamespaces = []string{"edges", "memories"}

// Domain prefix for OverallRoot. Includes the schema version so
// distinct schemas can never share an OverallRoot value.
const OverallRootDomain = "matrix.cortex.snapshot.overall.v1"

// Trigger reason strings. Free-form but conventional values land in
// snapshot manifests for ops visibility. Never participates in
// OverallRoot.
const (
	TriggerCompile  = "compile"
	TriggerAttest   = "attest"
	TriggerPeriodic = "periodic"
	TriggerExplicit = "explicit"
)

// Counters tracks per-namespace cardinality at snapshot time. Useful as
// a replay sanity check ("did we re-derive the same number of leaves?")
// but explicitly NOT a cryptographic input to OverallRoot — counter drift
// from a corrupted accum/ would otherwise force a divergent root before
// the actual error surfaced.
type Counters struct {
	Memories   uint64 `cbor:"1,keyasint"`
	Edges      uint64 `cbor:"2,keyasint"`
	Tombstoned uint64 `cbor:"3,keyasint,omitempty"` // memory tombstones (Head.Tombstoned set)
}

// Manifest is the canonical SnapshotManifest persisted at snap/<seq>.
//
// Field set follows spec §7.3 (Actor, SeqAtSnapshot, JournalRoot,
// StateRoots, OverallRoot, CreatedAt, SignedBy) plus three pragmatic
// additions documented in matrix.ctx phase7_locked_design:
//
//   - SchemaVersion: explicit byte; future-proofing for namespace adds.
//   - Trigger: free-form string ("compile" | "attest" | "periodic" |
//     "explicit"). Not in OverallRoot.
//   - Counters: per-namespace cardinality. Not in OverallRoot.
type Manifest struct {
	SchemaVersion uint8               `cbor:"0,keyasint"`
	Actor         string              `cbor:"1,keyasint"`
	SeqAtSnapshot uint64              `cbor:"2,keyasint"`
	JournalSeq    uint64              `cbor:"3,keyasint"` // max j/<seq> covered (inclusive); == LeafCount
	JournalRoot   [32]byte            `cbor:"4,keyasint"`
	StateRoots    map[string][32]byte `cbor:"5,keyasint"`
	OverallRoot   [32]byte            `cbor:"6,keyasint"`
	CreatedAt     int64               `cbor:"7,keyasint"` // unix nanos
	Trigger       string              `cbor:"8,keyasint,omitempty"`
	SignedBy      string              `cbor:"9,keyasint,omitempty"`
	Signature     []byte              `cbor:"10,keyasint,omitempty"`
	Counters      Counters            `cbor:"11,keyasint,omitempty"`
}

// canonicalEnc / canonicalDec mirror journal/journal.go and memory/codec.go.
var canonicalEnc cbor.EncMode
var canonicalDec cbor.DecMode

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("snapshot: build EncMode: %w", err))
	}
	canonicalEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("snapshot: build DecMode: %w", err))
	}
	canonicalDec = dm
}

// EncodeManifest returns canonical CBOR bytes for m.
func EncodeManifest(m *Manifest) ([]byte, error) {
	if m == nil {
		return nil, errors.New("snapshot: nil Manifest")
	}
	return canonicalEnc.Marshal(m)
}

// DecodeManifest parses canonical CBOR into out.
func DecodeManifest(b []byte, out *Manifest) error {
	return canonicalDec.Unmarshal(b, out)
}

// ComputeOverallRoot derives OverallRoot from journalRoot + per-namespace
// state roots. Pure function: identical inputs → identical bytes,
// regardless of map iteration order in the caller.
//
//	overall = sha256(
//	    OverallRootDomain ||
//	    SchemaVersion(1B) ||
//	    journalRoot ||
//	    nsCount(2B BE) ||
//	    for each ns sorted ascending: lpString(ns) || stateRoot
//	)
//
// nsCount and the length-prefixed name pin both the count and the names
// into the root, so adding a namespace IS a SchemaVersion bump.
func ComputeOverallRoot(journalRoot [32]byte, stateRoots map[string][32]byte) [32]byte {
	names := make([]string, 0, len(stateRoots))
	for n := range stateRoots {
		names = append(names, n)
	}
	sort.Strings(names)

	h := sha256.New()
	h.Write([]byte(OverallRootDomain))
	h.Write([]byte{SchemaVersion})
	h.Write(journalRoot[:])
	var nsCount [2]byte
	binary.BigEndian.PutUint16(nsCount[:], uint16(len(names)))
	h.Write(nsCount[:])
	for _, n := range names {
		h.Write([]byte{byte(len(n))})
		h.Write([]byte(n))
		root := stateRoots[n]
		h.Write(root[:])
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}

// State is the orchestration object exposed to the cortex layer. It
// holds references to the underlying store, the MMR, and the per-
// namespace SMTs. All Stage* methods take a *store.WriteBatch and write
// onto it without committing — the caller (cortex.Write etc.) commits
// the whole atomic batch.
type State struct {
	s    *store.Store
	mmr  *MMR
	smts map[string]*SMT
}

// New returns a State bound to s. The MMR + SMTs are constructed eagerly
// (cheap: no I/O until Stage* / Root is called).
func New(s *store.Store) *State {
	st := &State{
		s:    s,
		mmr:  NewMMR(s),
		smts: make(map[string]*SMT, len(AnchoredNamespaces)),
	}
	for _, ns := range AnchoredNamespaces {
		st.smts[ns] = NewSMT(s, ns)
	}
	return st
}

// Store returns the underlying handle. Reserved for sibling packages.
func (st *State) Store() *store.Store { return st.s }

// MMR returns the journal accumulator handle. Reserved for tests + the
// CLI prove subcommand.
func (st *State) MMR() *MMR { return st.mmr }

// SMT returns the per-namespace tree handle for ns, or nil if ns is not
// an anchored namespace.
func (st *State) SMT(ns string) *SMT { return st.smts[ns] }

// StageJournalLeaf appends one leaf to the MMR via setter. The leaf hash
// is the canonical journal-leaf hash already computed by the caller.
//
// Most callers should NOT use this directly: install MMRHook() as the
// store's JournalHook and the MMR will track every journal write
// transparently. StageJournalLeaf remains exposed for tests + the
// replay harness (Phase 11).
func (st *State) StageJournalLeaf(setter Setter, leafHash [32]byte) error {
	return st.mmr.StageAppend(setter, leafHash)
}

// MMRHook returns a store.JournalHook that appends one MMR leaf per
// journal entry. Install via store.SetJournalHook. The hook wraps the
// raw *pebble.Batch with a PebbleBatchSetter so the MMR can stage its
// nodes inside the journal write's atomic batch.
func (st *State) MMRHook() store.JournalHook {
	return func(b *pebble.Batch, _ uint64, leafHash [32]byte) error {
		return st.mmr.StageAppend(NewPebbleBatchSetter(b), leafHash)
	}
}

// StageMemoryUpdate stages an SMT update for the memories namespace.
// canonicalHead is the canonical-CBOR encoding of the new memory.Head;
// pass nil to express deletion (which doesn't happen in the cortex API
// today — we tombstone via Head.Tombstoned, which still produces a non-
// nil Head and therefore a non-zero value hash; callers don't need the
// delete path until retention-policy hard-delete lands).
func (st *State) StageMemoryUpdate(setter Setter, id [16]byte, canonicalHead []byte) error {
	keyHash := HashMemoryKey(id)
	var valueHash [32]byte
	if len(canonicalHead) > 0 {
		valueHash = HashValue(canonicalHead)
	}
	return st.smts["memories"].StageUpdate(setter, keyHash, valueHash)
}

// StageEdgeUpdate stages an SMT update for the edges namespace. Forward
// direction only — the reverse e/to record is byte-identical and would
// double-anchor the same fact. canonicalEdge is the canonical-CBOR
// encoding of the new EdgeRecord (Phase 6 EncodeEdge output); nil
// expresses deletion (not used today since RemoveEdge is a soft tombstone
// that still produces a non-empty EdgeRecord).
func (st *State) StageEdgeUpdate(setter Setter, src [16]byte, edgeType byte, dst [16]byte, canonicalEdge []byte) error {
	keyHash := HashEdgeKey(src, edgeType, dst)
	var valueHash [32]byte
	if len(canonicalEdge) > 0 {
		valueHash = HashValue(canonicalEdge)
	}
	return st.smts["edges"].StageUpdate(setter, keyHash, valueHash)
}

// CurrentRoots returns (journalRoot, stateRoots, overallRoot) for the
// current persisted state. Pure function of committed state — no batch,
// no in-memory caching. Used by Snapshot and by Cortex.OverallRoot for
// the compiler determinism seed (D11).
func (st *State) CurrentRoots() ([32]byte, map[string][32]byte, [32]byte, error) {
	jr, err := st.mmr.Root()
	if err != nil {
		return [32]byte{}, nil, [32]byte{}, err
	}
	stateRoots := make(map[string][32]byte, len(st.smts))
	for ns, smt := range st.smts {
		r, err := smt.Root()
		if err != nil {
			return [32]byte{}, nil, [32]byte{}, fmt.Errorf("snapshot.State.CurrentRoots: %s: %w", ns, err)
		}
		stateRoots[ns] = r
	}
	overall := ComputeOverallRoot(jr, stateRoots)
	return jr, stateRoots, overall, nil
}

// Snapshot assembles a Manifest at the current state and persists it
// under snap/<seq>. Returns the manifest. The seq monotonically advances
// across snapshots independent of the journal seq — multiple snapshots
// at the same JournalSeq are legal (e.g. one for "compile" plus one for
// "attest" if no journal mutation happened in between), each with its
// own snap/<seq>.
//
// Counters are computed by scanning idx/type/ + e/from/ prefix counts
// (cheap relative to the SMT walks already done). Counters are NOT in
// OverallRoot.
func (st *State) Snapshot(actor string, trigger string, now time.Time) (*Manifest, error) {
	jr, stateRoots, overall, err := st.CurrentRoots()
	if err != nil {
		return nil, err
	}
	leafCount, err := st.mmr.LeafCount()
	if err != nil {
		return nil, err
	}

	// Counters: cheap prefix counts. Each Pebble PrefixIter walks key-
	// only — no value bytes loaded.
	memCount, tombCount, err := countMemories(st.s)
	if err != nil {
		return nil, err
	}
	edgeCount, err := countEdges(st.s)
	if err != nil {
		return nil, err
	}

	// Allocate the snapshot seq. We use a dedicated counter at
	// meta/snapshot_seq; this is intentionally separate from journal
	// seq so snapshots can be taken without bumping j/. Read-modify-
	// write under the underlying DB; no batch (snapshots don't need
	// the journaled-batch invariant — they're an audit artifact, not
	// a state mutation).
	seq, err := allocSnapshotSeq(st.s)
	if err != nil {
		return nil, err
	}

	m := &Manifest{
		SchemaVersion: SchemaVersion,
		Actor:         actor,
		SeqAtSnapshot: seq,
		JournalSeq:    leafCount,
		JournalRoot:   jr,
		StateRoots:    stateRoots,
		OverallRoot:   overall,
		CreatedAt:     now.UnixNano(),
		Trigger:       trigger,
		Counters: Counters{
			Memories:   memCount,
			Edges:      edgeCount,
			Tombstoned: tombCount,
		},
	}
	enc, err := EncodeManifest(m)
	if err != nil {
		return nil, err
	}
	if err := st.s.DB().Set(keys.SnapshotKey(seq), enc, nil); err != nil {
		return nil, fmt.Errorf("snapshot.State.Snapshot: persist snap/%d: %w", seq, err)
	}
	return m, nil
}

// LoadSnapshot reads the persisted manifest at snap/<seq>. Returns
// ErrSnapshotNotFound if absent.
func (st *State) LoadSnapshot(seq uint64) (*Manifest, error) {
	raw, ok, err := st.s.Get(keys.SnapshotKey(seq))
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrSnapshotNotFound
	}
	var m Manifest
	if err := DecodeManifest(raw, &m); err != nil {
		return nil, fmt.Errorf("snapshot.State.LoadSnapshot: decode: %w", err)
	}
	return &m, nil
}

// IterSnapshots invokes fn for every persisted snap/<seq> in seq order.
// fn returns an error to abort the iteration; that error is propagated.
func (st *State) IterSnapshots(fn func(*Manifest) error) error {
	return st.s.PrefixIter(keys.PrefixSnapshot, func(_, value []byte) error {
		var m Manifest
		if err := DecodeManifest(value, &m); err != nil {
			return fmt.Errorf("snapshot.State.IterSnapshots: decode: %w", err)
		}
		return fn(&m)
	})
}

// FindSnapshotByRoot returns the manifest whose OverallRoot matches the
// supplied hash, or ErrSnapshotNotFound if no persisted snap/<seq> has
// that root.
//
// Used by the scope verifier (research/06-agents.md §7.2 step 2:
// "Verifies the scope's snapshot_hash is still resolvable") so a sub-
// agent can be certain the root the scope was anchored against is one
// the cortex still recognises (i.e. has not been retention-policy-GC'd).
//
// O(N) scan over snap/. For v1 actor counts this is microseconds; the
// reverse index idx/snap_root/<root:32> can land later if profiling
// demands it.
func (st *State) FindSnapshotByRoot(root [32]byte) (*Manifest, error) {
	var found *Manifest
	err := st.IterSnapshots(func(m *Manifest) error {
		if m.OverallRoot == root {
			c := *m
			found = &c
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrSnapshotNotFound
	}
	return found, nil
}

// allocSnapshotSeq atomically reads + bumps meta/snapshot_seq. The first
// snapshot for a fresh actor returns seq=0; the next returns 1; etc.
// Persisted under SetMeta so the counter is durable across process
// restarts.
func allocSnapshotSeq(s *store.Store) (uint64, error) {
	raw, ok, err := s.Get(keyMetaSnapshotSeq)
	if err != nil {
		return 0, err
	}
	var next uint64
	if ok {
		if len(raw) != 8 {
			return 0, fmt.Errorf("snapshot.allocSnapshotSeq: malformed seq (len=%d)", len(raw))
		}
		next = binary.BigEndian.Uint64(raw)
	}
	var bumped [8]byte
	binary.BigEndian.PutUint64(bumped[:], next+1)
	if err := s.SetMeta(keyMetaSnapshotSeq, bumped[:]); err != nil {
		return 0, err
	}
	return next, nil
}

// keyMetaSnapshotSeq is meta/snapshot_seq, the next-to-allocate snapshot
// seq counter. Distinct from MetaJournalHead so journal ops and snapshot
// ops don't interleave on the same counter.
var keyMetaSnapshotSeq = append(append([]byte{}, keys.PrefixMeta...), []byte("snapshot_seq")...)

// countMemories returns (live_count, tombstoned_count). Walks idx/type/
// (a key-only iterator that does NOT load Head bytes) + per-id Head
// load to inspect Tombstoned. For 100k memories this is ~100k Pebble
// point lookups, ~30ms on commodity hardware — the snapshot path is
// pull-driven and not on the hot write path, so this is acceptable.
func countMemories(s *store.Store) (live uint64, tombstoned uint64, err error) {
	// Iterate the full m/ prefix (heads only). For each, decode just
	// enough to inspect Tombstoned. Cheap because Head CBOR is small.
	err = s.PrefixIter(keys.PrefixMemoryHead, func(_, value []byte) error {
		// We don't decode the full Head — we only need the Tombstoned
		// presence bit. CBOR canonical encoding makes the Tombstoned
		// field at key 14 (memory.Head.Tombstoned). Rather than
		// re-implement CBOR parsing, the simpler answer is to count
		// total + scan tomb/ for tombstoned (one extra Pebble scan).
		live++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	// Tombstoned: count tomb/ keys.
	err = s.PrefixIter(keys.PrefixTombstone, func(_, _ []byte) error {
		tombstoned++
		return nil
	})
	if err != nil {
		return 0, 0, err
	}
	// `live` currently counts ALL memories (including tombstoned). Net
	// live = total - tombstoned.
	if tombstoned > live {
		// Defensive: shouldn't happen, but a stray tomb/ entry without a
		// matching m/ would break the subtract. Treat as zero live.
		return 0, tombstoned, nil
	}
	live -= tombstoned
	return live, tombstoned, nil
}

// countEdges returns the number of forward edge keys (live + tombstoned).
// Forward direction only; reverse is by construction the same count.
func countEdges(s *store.Store) (uint64, error) {
	var n uint64
	err := s.PrefixIter(keys.PrefixEdgeFrom, func(_, _ []byte) error {
		n++
		return nil
	})
	return n, err
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
