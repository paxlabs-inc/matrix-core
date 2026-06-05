// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package keys encodes and decodes Pebble keys for the Matrix cortex.
//
// Spec: research/04-cortex.md §2 (key encoding).
//
// Invariants:
//   - Single Pebble DB per actor. All namespaces below are KEY PREFIXES inside
//     that DB. Atomic multi-namespace writes (§11.1) require this.
//   - All numeric components are big-endian fixed-width binary so that
//     byte-sort == numeric-sort under Pebble's default comparator.
//   - IDs are 16-byte binary ULIDs (textual only at API boundaries).
//   - Versions and sequence numbers are uint64, 8 bytes BE.
//   - Strings inside keys are 1-byte length-prefixed (caller bounds <=255).
//   - The path separator '/' (0x2F) is forbidden inside path components.
package keys

import (
	"encoding/binary"
	"errors"
	"fmt"
)

// Namespace prefixes (§2.2 / §2.3). Kept as short ASCII so dump tools are
// human-readable. Each prefix ends with '/' for hierarchy.
var (
	PrefixMemoryHead    = []byte("m/")        // m/<id:16>
	PrefixMemoryVersion = []byte("mv/")       // mv/<id:16>/v/<version:8>
	PrefixEdgeFrom      = []byte("e/from/")   // e/from/<src:16>/<edge:1>/<dst:16>
	PrefixEdgeTo        = []byte("e/to/")     // e/to/<dst:16>/<edge:1>/<src:16>
	PrefixJournal       = []byte("j/")        // j/<seq:8>
	PrefixTombstone     = []byte("tomb/")     // tomb/<id:16>
	PrefixSnapshot      = []byte("snap/")     // snap/<seq:8>
	PrefixIdxType       = []byte("idx/type/") // idx/type/<type:1>/<created:8>/<id:16>
	PrefixIdxTag        = []byte("idx/tag/")  // idx/tag/<tag_hash:8>/<created:8>/<id:16>
	PrefixIdxFrame      = []byte("idx/frame/")
	PrefixIdxActorObj   = []byte("idx/actor_obj/")
	PrefixSalience      = []byte("salience/") // salience/<id:16>
	PrefixVecMeta       = []byte("vec/meta/") // vec/meta/<id:16>
	PrefixMeta          = []byte("meta/")     // meta/<key> — store-level metadata (e.g. journal head seq)
	PrefixAccum         = []byte("accum/")    // accum/<...> — Phase 7 journal Merkle accumulator (§7.1)
	PrefixIdxSMT        = []byte("idx/smt/")  // idx/smt/<ns>/n/<depth:2>/<path:32> — Phase 7 sparse Merkle tree node cache (§7.2)
	PrefixCheckpoint    = []byte("chk/")      // chk/<lpstr intent>/<lpstr step> — Phase 9 compaction checkpoint records
)

// MetaJournalHead is the meta-namespace key holding the next-to-allocate
// journal sequence number for this actor. Stored as a uint64 big-endian.
var MetaJournalHead = append(append([]byte{}, PrefixMeta...), []byte("journal_head")...)

// MetaSalienceWeights is the meta-namespace key holding the per-actor
// EMA-learned salience weights (research/04-cortex.md §8.3, "Stored at
// salience/<actor>/weights"). One Pebble key per actor; the cortex is
// per-actor by construction so no actor suffix is needed in-key. Value is
// canonical CBOR salience.Weights; absent key ⇒ DefaultWeights (cold
// start) per §8.2. Lives under meta/ rather than salience/ to avoid
// colliding with the binary-ULID-suffix salience/<id:16> namespace
// (Phase 12 lock).
var MetaSalienceWeights = append(append([]byte{}, PrefixMeta...), []byte("salience_weights")...)

// PrefixMetaCompileCache is the meta/ sub-namespace for the per-actor
// compile-cache sidecar (Session 31d · P4). Each cached compile is
// stored under meta/compile_cache/<sha256_hex:64> where the hex hash
// is the cache key derived from
//
//	sha256(skill_digest || US || prose || US || cortex_snapshot_hash
//	  || US || verb || US || model_digest)
//
// The cache is a SIDECAR (mirrors meta/salience_weights posture from
// Phase 12): runtime policy state only, NEVER part of cortex
// OverallRoot. Tombstoning a memory or rolling a new snapshot does NOT
// invalidate cached entries — they are key-tied to the cortex root
// hash captured at compile time, so the next compile against a fresh
// root simply misses and recomputes. Entry value is canonical-CBOR
// compileCacheEntry (see executor/compilecache).
var PrefixMetaCompileCache = append(append([]byte{}, PrefixMeta...), []byte("compile_cache/")...)

// MetaCompileCacheKey returns meta/compile_cache/<hex>. hex must be the
// lowercase hex-encoded sha256 of the cache-key tuple. Caller validates
// length=64 + lowercase-hex; this helper does not (so the same builder
// works for prefix scans during cache prune / inspect).
func MetaCompileCacheKey(hex string) []byte {
	out := make([]byte, 0, len(PrefixMetaCompileCache)+len(hex))
	out = append(out, PrefixMetaCompileCache...)
	out = append(out, hex...)
	return out
}

// PrefixMetaGoalState is the meta/ sub-namespace for the per-actor
// goal runtime state sidecar (Session 32 ambient architect).
//
// One Pebble key per Goal at meta/goal_state/<goal_id:16> holding a
// canonical-CBOR cortex.GoalRuntimeState (see cortex/goal_state.go):
// scheduler debounce timestamp, daily counters, failure streak, last
// decision, etc.
//
// SIDECAR posture: mirrors meta/salience_weights (Phase 12) and
// meta/compile_cache (sess#31d). Runtime policy state ONLY; NEVER part
// of the cortex OverallRoot. Replay rebuilds drop these keys (replay/
// drop.go); a freshly-rebuilt actor has cold-start scheduler state and
// the next scheduler tick simply re-derives debounce/counters from
// cortex memories + journaled cost events.
//
// Why a sidecar: scheduler state mutates on every tick (≈1Hz under load).
// Including it in the canonical root would force a journal entry per
// tick — an expensive footgun for an audit-grade ledger. The plan
// (S32Q4) explicitly pins this posture.
var PrefixMetaGoalState = append(append([]byte{}, PrefixMeta...), []byte("goal_state/")...)

// MetaGoalStateKey returns meta/goal_state/<goal_id:16>.
func MetaGoalStateKey(goalID ULID) []byte {
	out := make([]byte, 0, len(PrefixMetaGoalState)+ULIDSize)
	out = append(out, PrefixMetaGoalState...)
	out = append(out, goalID[:]...)
	return out
}

// ParseMetaGoalStateKey extracts the goal ULID from a meta/goal_state/ key.
// Returns ErrShortKey on truncation, ErrBadPrefix when k is not a goal-state
// key.
func ParseMetaGoalStateKey(k []byte) (ULID, error) {
	wantLen := len(PrefixMetaGoalState) + ULIDSize
	if len(k) != wantLen {
		return ULID{}, ErrShortKey
	}
	if !hasPrefix(k, PrefixMetaGoalState) {
		return ULID{}, ErrBadPrefix
	}
	var id ULID
	copy(id[:], k[len(PrefixMetaGoalState):])
	return id, nil
}

// ULIDSize is the binary ULID length in bytes.
const ULIDSize = 16

// ULID is a 16-byte binary ULID. We mirror oklog/ulid/v2's representation but
// keep keys/ free of that import so callers can choose.
type ULID [ULIDSize]byte

// Errors returned by decoders.
var (
	ErrShortKey  = errors.New("keys: key shorter than namespace+payload")
	ErrBadPrefix = errors.New("keys: prefix mismatch")
	ErrBadStrLen = errors.New("keys: string length exceeds 255")
)

// PutUint64BE appends an 8-byte big-endian uint64 to dst.
func PutUint64BE(dst []byte, v uint64) []byte {
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], v)
	return append(dst, buf[:]...)
}

// ReadUint64BE reads an 8-byte big-endian uint64 from src; returns the value
// and the remainder of src after the 8 bytes.
func ReadUint64BE(src []byte) (uint64, []byte, error) {
	if len(src) < 8 {
		return 0, nil, ErrShortKey
	}
	return binary.BigEndian.Uint64(src[:8]), src[8:], nil
}

// PutLPString appends a 1-byte length-prefixed ASCII/UTF-8 string. Returns an
// error if s exceeds 255 bytes; in that case caller should hash s instead.
func PutLPString(dst []byte, s string) ([]byte, error) {
	if len(s) > 255 {
		return nil, ErrBadStrLen
	}
	dst = append(dst, byte(len(s)))
	dst = append(dst, s...)
	return dst, nil
}

// ReadLPString reads a 1-byte length-prefixed string.
func ReadLPString(src []byte) (string, []byte, error) {
	if len(src) < 1 {
		return "", nil, ErrShortKey
	}
	n := int(src[0])
	if len(src) < 1+n {
		return "", nil, ErrShortKey
	}
	return string(src[1 : 1+n]), src[1+n:], nil
}

// MemoryHeadKey returns m/<id:16>.
func MemoryHeadKey(id ULID) []byte {
	out := make([]byte, 0, len(PrefixMemoryHead)+ULIDSize)
	out = append(out, PrefixMemoryHead...)
	out = append(out, id[:]...)
	return out
}

// MemoryVersionKey returns mv/<id:16>/v/<version:8>.
//
// The literal "/v/" in the middle keeps versions of one memory contiguous
// under the same id prefix scan: mv/<id>/v/.
func MemoryVersionKey(id ULID, version uint64) []byte {
	out := make([]byte, 0, len(PrefixMemoryVersion)+ULIDSize+3+8)
	out = append(out, PrefixMemoryVersion...)
	out = append(out, id[:]...)
	out = append(out, '/', 'v', '/')
	out = PutUint64BE(out, version)
	return out
}

// MemoryVersionPrefix returns mv/<id:16>/v/, the prefix for a single memory's
// version history (byte-sorted ascending by version).
func MemoryVersionPrefix(id ULID) []byte {
	out := make([]byte, 0, len(PrefixMemoryVersion)+ULIDSize+3)
	out = append(out, PrefixMemoryVersion...)
	out = append(out, id[:]...)
	out = append(out, '/', 'v', '/')
	return out
}

// EdgeFromKey returns e/from/<src:16>/<edge:1>/<dst:16>.
//
// Layout: a single byte slash separates the src ULID from the 1-byte edge
// type, then another slash separates edge type from dst ULID. Slashes inside
// ULIDs are not possible (binary ULIDs may contain 0x2F, so use FIXED widths,
// not slash as delimiter inside the binary fields). Here the slash is
// COSMETIC after fixed-width src; the parser uses widths, not delimiters.
func EdgeFromKey(src ULID, edge byte, dst ULID) []byte {
	out := make([]byte, 0, len(PrefixEdgeFrom)+ULIDSize+1+ULIDSize)
	out = append(out, PrefixEdgeFrom...)
	out = append(out, src[:]...)
	out = append(out, edge)
	out = append(out, dst[:]...)
	return out
}

// EdgeToKey returns e/to/<dst:16>/<edge:1>/<src:16>.
func EdgeToKey(dst ULID, edge byte, src ULID) []byte {
	out := make([]byte, 0, len(PrefixEdgeTo)+ULIDSize+1+ULIDSize)
	out = append(out, PrefixEdgeTo...)
	out = append(out, dst[:]...)
	out = append(out, edge)
	out = append(out, src[:]...)
	return out
}

// EdgeFromPrefix returns e/from/<src:16>/, suitable for scanning all outgoing
// edges from src (any edge type, any dst, sorted by edge byte then dst).
func EdgeFromPrefix(src ULID) []byte {
	out := make([]byte, 0, len(PrefixEdgeFrom)+ULIDSize)
	out = append(out, PrefixEdgeFrom...)
	out = append(out, src[:]...)
	return out
}

// EdgeFromTypePrefix returns e/from/<src:16>/<edge:1>, suitable for scanning
// outgoing edges of one specific type from src. Sorted ascending by dst.
func EdgeFromTypePrefix(src ULID, edge byte) []byte {
	out := make([]byte, 0, len(PrefixEdgeFrom)+ULIDSize+1)
	out = append(out, PrefixEdgeFrom...)
	out = append(out, src[:]...)
	out = append(out, edge)
	return out
}

// EdgeToPrefix returns e/to/<dst:16>/.
func EdgeToPrefix(dst ULID) []byte {
	out := make([]byte, 0, len(PrefixEdgeTo)+ULIDSize)
	out = append(out, PrefixEdgeTo...)
	out = append(out, dst[:]...)
	return out
}

// EdgeToTypePrefix returns e/to/<dst:16>/<edge:1>, suitable for scanning
// incoming edges of one specific type into dst. Sorted ascending by src.
func EdgeToTypePrefix(dst ULID, edge byte) []byte {
	out := make([]byte, 0, len(PrefixEdgeTo)+ULIDSize+1)
	out = append(out, PrefixEdgeTo...)
	out = append(out, dst[:]...)
	out = append(out, edge)
	return out
}

// ParseEdgeFromKey extracts (src, edge, dst) from an e/from/ key. Returns
// ErrShortKey on wrong length and ErrBadPrefix when k does not start with
// e/from/. Mirrors ParseEdgeToKey.
func ParseEdgeFromKey(k []byte) (src ULID, edge byte, dst ULID, err error) {
	wantLen := len(PrefixEdgeFrom) + ULIDSize + 1 + ULIDSize
	if len(k) != wantLen {
		err = ErrShortKey
		return
	}
	if !hasPrefix(k, PrefixEdgeFrom) {
		err = ErrBadPrefix
		return
	}
	tail := k[len(PrefixEdgeFrom):]
	copy(src[:], tail[:ULIDSize])
	edge = tail[ULIDSize]
	copy(dst[:], tail[ULIDSize+1:])
	return
}

// ParseEdgeToKey extracts (dst, edge, src) from an e/to/ key.
func ParseEdgeToKey(k []byte) (dst ULID, edge byte, src ULID, err error) {
	wantLen := len(PrefixEdgeTo) + ULIDSize + 1 + ULIDSize
	if len(k) != wantLen {
		err = ErrShortKey
		return
	}
	if !hasPrefix(k, PrefixEdgeTo) {
		err = ErrBadPrefix
		return
	}
	tail := k[len(PrefixEdgeTo):]
	copy(dst[:], tail[:ULIDSize])
	edge = tail[ULIDSize]
	copy(src[:], tail[ULIDSize+1:])
	return
}

// JournalKey returns j/<seq:8>. Sorted ascending by seq.
func JournalKey(seq uint64) []byte {
	out := make([]byte, 0, len(PrefixJournal)+8)
	out = append(out, PrefixJournal...)
	out = PutUint64BE(out, seq)
	return out
}

// ParseJournalKey extracts the seq from a journal key. Returns ErrBadPrefix
// if k does not start with j/.
func ParseJournalKey(k []byte) (uint64, error) {
	if len(k) < len(PrefixJournal)+8 {
		return 0, ErrShortKey
	}
	if !hasPrefix(k, PrefixJournal) {
		return 0, ErrBadPrefix
	}
	return binary.BigEndian.Uint64(k[len(PrefixJournal):]), nil
}

// PrefixRange returns [prefix, upper) such that every Pebble key beginning
// with prefix lies in the half-open range. Returns nil upper if prefix has no
// successor (all 0xFF bytes); callers should treat that as "scan to end".
func PrefixRange(prefix []byte) (lower, upper []byte) {
	lower = append([]byte{}, prefix...)
	upper = append([]byte{}, prefix...)
	for i := len(upper) - 1; i >= 0; i-- {
		if upper[i] != 0xff {
			upper[i]++
			upper = upper[:i+1]
			return lower, upper
		}
	}
	// prefix is all 0xff; no upper bound.
	return lower, nil
}

// JournalRange returns the [start, end) byte range covering all journal keys.
// Pass this to a Pebble iterator to walk the full journal in seq order.
func JournalRange() (start, end []byte) {
	return PrefixRange(PrefixJournal)
}

// TombstoneKey returns tomb/<id:16>.
func TombstoneKey(id ULID) []byte {
	out := make([]byte, 0, len(PrefixTombstone)+ULIDSize)
	out = append(out, PrefixTombstone...)
	out = append(out, id[:]...)
	return out
}

// SnapshotKey returns snap/<seq:8>.
func SnapshotKey(seq uint64) []byte {
	out := make([]byte, 0, len(PrefixSnapshot)+8)
	out = append(out, PrefixSnapshot...)
	out = PutUint64BE(out, seq)
	return out
}

// SalienceKey returns salience/<id:16>.
func SalienceKey(id ULID) []byte {
	out := make([]byte, 0, len(PrefixSalience)+ULIDSize)
	out = append(out, PrefixSalience...)
	out = append(out, id[:]...)
	return out
}

// VecMetaKey returns vec/meta/<id:16>.
func VecMetaKey(id ULID) []byte {
	out := make([]byte, 0, len(PrefixVecMeta)+ULIDSize)
	out = append(out, PrefixVecMeta...)
	out = append(out, id[:]...)
	return out
}

// IdxTypeKey returns idx/type/<type:1>/<created:8>/<id:16>.
// `created` is a Unix nanosecond timestamp (int64 cast to uint64 BE — sign bit
// handled by caller; negative times are unused in production).
func IdxTypeKey(memType byte, createdUnixNano uint64, id ULID) []byte {
	out := make([]byte, 0, len(PrefixIdxType)+1+8+ULIDSize)
	out = append(out, PrefixIdxType...)
	out = append(out, memType)
	out = PutUint64BE(out, createdUnixNano)
	out = append(out, id[:]...)
	return out
}

// IdxTypePrefix returns idx/type/<type:1>/, the prefix for scanning all
// memories of a given type in time order.
func IdxTypePrefix(memType byte) []byte {
	out := make([]byte, 0, len(PrefixIdxType)+1)
	out = append(out, PrefixIdxType...)
	out = append(out, memType)
	return out
}

// TagHashSize is the prefix length of the tag hash kept in idx/tag keys.
// Eight bytes is sufficient: collision probability ≈ 1/2^32 between any two
// tags in an actor's tag set, which is bounded at MaxTagsPerMemory*N memories
// (well under 2^16 tags per actor in realistic loads).
const TagHashSize = 8

// IdxTagKey returns idx/tag/<tag_hash:8>/<created:8>/<id:16>.
//
// `tagHash` is the first 8 bytes of sha256(tag) (caller computed); we hash
// because tags are arbitrary user strings up to 64 bytes and we want
// fixed-width keys. Uniqueness is enforced by appending the full ID at the
// tail, so collisions in the 8-byte prefix only inflate scan candidates,
// they never silently merge memories.
func IdxTagKey(tagHash [TagHashSize]byte, createdUnixNano uint64, id ULID) []byte {
	out := make([]byte, 0, len(PrefixIdxTag)+TagHashSize+8+ULIDSize)
	out = append(out, PrefixIdxTag...)
	out = append(out, tagHash[:]...)
	out = PutUint64BE(out, createdUnixNano)
	out = append(out, id[:]...)
	return out
}

// IdxTagPrefix returns idx/tag/<tag_hash:8>/, the prefix for scanning every
// memory tagged with a given tag in creation-time order.
func IdxTagPrefix(tagHash [TagHashSize]byte) []byte {
	out := make([]byte, 0, len(PrefixIdxTag)+TagHashSize)
	out = append(out, PrefixIdxTag...)
	out = append(out, tagHash[:]...)
	return out
}

// ParseIdxTagKey extracts (createdUnixNano, id) from an idx/tag key. Returns
// ErrShortKey if k is the wrong length and ErrBadPrefix if it does not begin
// with idx/tag/. The tag-hash component is opaque to consumers and not
// returned (the prefix already pinned which tag we scanned).
func ParseIdxTagKey(k []byte) (uint64, ULID, error) {
	wantLen := len(PrefixIdxTag) + TagHashSize + 8 + ULIDSize
	if len(k) != wantLen {
		return 0, ULID{}, ErrShortKey
	}
	if !hasPrefix(k, PrefixIdxTag) {
		return 0, ULID{}, ErrBadPrefix
	}
	tail := k[len(PrefixIdxTag)+TagHashSize:]
	created, rest, err := ReadUint64BE(tail)
	if err != nil {
		return 0, ULID{}, err
	}
	var id ULID
	copy(id[:], rest)
	return created, id, nil
}

// ObjHashSize is the byte length of obj_id components in idx/frame and
// idx/actor_obj keys. Mirrors memory.ObjHashSize but kept here so the
// keys package has no import on memory (which would create a dependency
// cycle: memory → keys for ULID + keys → memory for ObjHashSize).
const ObjHashSize = 16

// IdxFrameKey returns idx/frame/<verb:1>/<obj_kind:1>/<obj_hash:16>/<id:16>.
//
// Spec: research/04-cortex.md §2.3 — the frame-relevance secondary
// index. Scanned by cortex.context's Frame-relevant tier with the
// (verb, kind, obj_hash) prefix to retrieve every memory a skill
// stamped as relevant for that frame.
//
// Phase 8 caller: cortex.Write emits one of these per FrameRef in
// h.Frames (auto-derivation across all memory types). Insertion order
// inside the prefix is byte-sort over the trailing memory ID, which
// equals creation order under ULID monotonicity — good enough for the
// salience-rank step downstream.
//
// No `created` byte is stored in the key (cf. idx/tag and idx/type):
// the composer always salience-ranks frame matches, never time-ranks,
// so adding 8 bytes per row would inflate the index without giving
// any new scan affordance.
func IdxFrameKey(verb, objKind byte, objHash [ObjHashSize]byte, id ULID) []byte {
	out := make([]byte, 0, len(PrefixIdxFrame)+1+1+ObjHashSize+ULIDSize)
	out = append(out, PrefixIdxFrame...)
	out = append(out, verb)
	out = append(out, objKind)
	out = append(out, objHash[:]...)
	out = append(out, id[:]...)
	return out
}

// IdxFramePrefixVerb returns idx/frame/<verb:1>/, the broadest scan
// prefix. Used by debug tools that want to enumerate every frame entry
// for one verb; the cortex.context composer never scans this wide.
func IdxFramePrefixVerb(verb byte) []byte {
	out := make([]byte, 0, len(PrefixIdxFrame)+1)
	out = append(out, PrefixIdxFrame...)
	out = append(out, verb)
	return out
}

// IdxFramePrefixVerbKind returns idx/frame/<verb:1>/<kind:1>/, useful
// for "every (verb, kind) pair regardless of object" scans. Mainly a
// debug surface; the Phase 8 composer uses IdxFramePrefixVerbKindObj.
func IdxFramePrefixVerbKind(verb, objKind byte) []byte {
	out := make([]byte, 0, len(PrefixIdxFrame)+1+1)
	out = append(out, PrefixIdxFrame...)
	out = append(out, verb)
	out = append(out, objKind)
	return out
}

// IdxFramePrefixVerbKindObj returns idx/frame/<verb:1>/<kind:1>/<obj_hash:16>/,
// the prefix the cortex.context Frame-relevant tier scans for each
// (verb, kind, obj_ref) tuple in ContextOpts.Objects. One scan per
// tuple; results unioned across tuples then salience-ranked.
func IdxFramePrefixVerbKindObj(verb, objKind byte, objHash [ObjHashSize]byte) []byte {
	out := make([]byte, 0, len(PrefixIdxFrame)+1+1+ObjHashSize)
	out = append(out, PrefixIdxFrame...)
	out = append(out, verb)
	out = append(out, objKind)
	out = append(out, objHash[:]...)
	return out
}

// ParseIdxFrameKey extracts (verb, objKind, objHash, id) from an
// idx/frame key. Strict-length check; bad-prefix / short-key errors as
// elsewhere. The composer relies on the returned ID alone (the prefix
// already pinned verb+kind+obj_hash) but the full tuple is returned
// for symmetry with ParseIdxTagKey / ParseIdxTypeKey.
func ParseIdxFrameKey(k []byte) (verb, objKind byte, objHash [ObjHashSize]byte, id ULID, err error) {
	wantLen := len(PrefixIdxFrame) + 1 + 1 + ObjHashSize + ULIDSize
	if len(k) != wantLen {
		err = ErrShortKey
		return
	}
	if !hasPrefix(k, PrefixIdxFrame) {
		err = ErrBadPrefix
		return
	}
	tail := k[len(PrefixIdxFrame):]
	verb = tail[0]
	objKind = tail[1]
	copy(objHash[:], tail[2:2+ObjHashSize])
	copy(id[:], tail[2+ObjHashSize:])
	return
}

// IdxActorObjKey returns idx/actor_obj/<verb:1>/<obj_hash:16>/<created:8>/<id:16>.
//
// Spec: research/04-cortex.md §2.3 — the outcome-history secondary
// index. Scanned by cortex.context's Outcomes tier with the
// (verb, obj_hash) prefix; results sort ascending by created and the
// composer takes the LAST N entries (= most recent N outcomes).
//
// Phase 8 caller: cortex.Write emits one of these per FrameRef in
// h.Frames ONLY when h.Type == TypeEvent. Other memory types
// (Preference, Belief, ...) stamp idx/frame but never idx/actor_obj,
// because outcomes-by-(verb,object) is an Event-only concept per
// research/03-retrieval-patterns.md §2.1 ("1-3 prior similar intents
// and their outcomes").
//
// The created field is a Unix-nanosecond timestamp cast to uint64
// big-endian, identical to idx/type's created handling — keeps
// byte-sort=time-sort over the plausible post-1970 range.
func IdxActorObjKey(verb byte, objHash [ObjHashSize]byte, createdUnixNano uint64, id ULID) []byte {
	out := make([]byte, 0, len(PrefixIdxActorObj)+1+ObjHashSize+8+ULIDSize)
	out = append(out, PrefixIdxActorObj...)
	out = append(out, verb)
	out = append(out, objHash[:]...)
	out = PutUint64BE(out, createdUnixNano)
	out = append(out, id[:]...)
	return out
}

// IdxActorObjPrefixVerb returns idx/actor_obj/<verb:1>/, the broadest
// scan prefix (debug-only).
func IdxActorObjPrefixVerb(verb byte) []byte {
	out := make([]byte, 0, len(PrefixIdxActorObj)+1)
	out = append(out, PrefixIdxActorObj...)
	out = append(out, verb)
	return out
}

// IdxActorObjPrefixVerbObj returns idx/actor_obj/<verb:1>/<obj_hash:16>/,
// the prefix the Outcomes tier scans for one (verb, obj_ref) tuple.
// Entries inside this prefix sort ascending by created; the composer
// reverses to take the most-recent N.
func IdxActorObjPrefixVerbObj(verb byte, objHash [ObjHashSize]byte) []byte {
	out := make([]byte, 0, len(PrefixIdxActorObj)+1+ObjHashSize)
	out = append(out, PrefixIdxActorObj...)
	out = append(out, verb)
	out = append(out, objHash[:]...)
	return out
}

// ParseIdxActorObjKey extracts (verb, objHash, createdUnixNano, id)
// from an idx/actor_obj key.
func ParseIdxActorObjKey(k []byte) (verb byte, objHash [ObjHashSize]byte, createdUnixNano uint64, id ULID, err error) {
	wantLen := len(PrefixIdxActorObj) + 1 + ObjHashSize + 8 + ULIDSize
	if len(k) != wantLen {
		err = ErrShortKey
		return
	}
	if !hasPrefix(k, PrefixIdxActorObj) {
		err = ErrBadPrefix
		return
	}
	tail := k[len(PrefixIdxActorObj):]
	verb = tail[0]
	copy(objHash[:], tail[1:1+ObjHashSize])
	createdUnixNano = binary.BigEndian.Uint64(tail[1+ObjHashSize : 1+ObjHashSize+8])
	copy(id[:], tail[1+ObjHashSize+8:])
	return
}

// ParseIdxTypeKey extracts (memType, createdUnixNano, id) from an idx/type
// key. Mirrors ParseIdxTagKey for symmetry.
func ParseIdxTypeKey(k []byte) (byte, uint64, ULID, error) {
	wantLen := len(PrefixIdxType) + 1 + 8 + ULIDSize
	if len(k) != wantLen {
		return 0, 0, ULID{}, ErrShortKey
	}
	if !hasPrefix(k, PrefixIdxType) {
		return 0, 0, ULID{}, ErrBadPrefix
	}
	t := k[len(PrefixIdxType)]
	created, rest, err := ReadUint64BE(k[len(PrefixIdxType)+1:])
	if err != nil {
		return 0, 0, ULID{}, err
	}
	var id ULID
	copy(id[:], rest)
	return t, created, id, nil
}

// ParseSnapshotKey extracts the seq from a snap/ key. Returns ErrBadPrefix
// if k does not start with snap/. Used by snapshot iteration helpers.
func ParseSnapshotKey(k []byte) (uint64, error) {
	if len(k) < len(PrefixSnapshot)+8 {
		return 0, ErrShortKey
	}
	if !hasPrefix(k, PrefixSnapshot) {
		return 0, ErrBadPrefix
	}
	return binary.BigEndian.Uint64(k[len(PrefixSnapshot):]), nil
}

// AccumMMRLeafCount returns accum/mmr/leafcount, the MMR leaf-count key.
// Value is uint64 BE; absent means zero leaves appended.
var AccumMMRLeafCount = append(append([]byte{}, PrefixAccum...), []byte("mmr/leafcount")...)

// AccumMMRNodeKey returns accum/mmr/n/<pos:8> for one MMR node hash. Position
// uses the standard 1-indexed depth-first MMR numbering (research/04-cortex.md
// §7.1 references the Grin/CT MMR shape).
func AccumMMRNodeKey(pos uint64) []byte {
	out := make([]byte, 0, len(PrefixAccum)+len("mmr/n/")+8)
	out = append(out, PrefixAccum...)
	out = append(out, []byte("mmr/n/")...)
	out = PutUint64BE(out, pos)
	return out
}

// IdxSMTNodeKey returns idx/smt/<ns>/n/<depth:2>/<path:32> for one SMT
// internal node. Depth 0 = root level (subtree below has remaining depth
// 256); depth 256 = leaf level. Path is the bit string from the root with
// the top `depth` bits significant; lower bits MUST be zero (caller
// responsibility, normalized via SMTNormalizePath).
//
// Namespace is a short ASCII tag ("memories", "edges"). The validator
// rejects '/' so the key is unambiguous.
func IdxSMTNodeKey(ns string, depth uint16, path [32]byte) ([]byte, error) {
	if err := ValidateNoSeparator(ns); err != nil {
		return nil, fmt.Errorf("keys.IdxSMTNodeKey: %w", err)
	}
	if depth > 256 {
		return nil, fmt.Errorf("keys.IdxSMTNodeKey: depth %d > 256", depth)
	}
	out := make([]byte, 0, len(PrefixIdxSMT)+len(ns)+len("/n/")+2+32)
	out = append(out, PrefixIdxSMT...)
	out = append(out, ns...)
	out = append(out, []byte("/n/")...)
	var d [2]byte
	binary.BigEndian.PutUint16(d[:], depth)
	out = append(out, d[:]...)
	out = append(out, path[:]...)
	return out, nil
}

// IdxSMTNamespacePrefix returns idx/smt/<ns>/n/, the prefix for scanning all
// node entries of one SMT (used during a drop+rebuild on replay).
func IdxSMTNamespacePrefix(ns string) ([]byte, error) {
	if err := ValidateNoSeparator(ns); err != nil {
		return nil, fmt.Errorf("keys.IdxSMTNamespacePrefix: %w", err)
	}
	out := make([]byte, 0, len(PrefixIdxSMT)+len(ns)+len("/n/"))
	out = append(out, PrefixIdxSMT...)
	out = append(out, ns...)
	out = append(out, []byte("/n/")...)
	return out, nil
}

// CheckpointKey returns chk/<lpstr intent_id>/<lpstr step_id>.
//
// Spec: research/03-retrieval-patterns.md §5.1 step 3 + §5.3 (the
// cortex.compact primitive). Phase 9 / Andrew lock A1.
//
// Both components are 1-byte length-prefixed strings (PutLPString), so
// the variable-width tail is unambiguously parseable. The literal '/'
// between them is cosmetic — the parser uses lengths, not delimiters —
// but matches the visual style of EdgeFromKey/EdgeToKey.
//
// Caller-supplied ids may not contain '/' (validated). Empty ids
// rejected at the API surface (cortex.Compact); we only enforce the
// 255-byte and no-slash constraints here.
func CheckpointKey(intentID, stepID string) ([]byte, error) {
	if err := ValidateNoSeparator(intentID); err != nil {
		return nil, fmt.Errorf("keys.CheckpointKey: intent: %w", err)
	}
	if err := ValidateNoSeparator(stepID); err != nil {
		return nil, fmt.Errorf("keys.CheckpointKey: step: %w", err)
	}
	out := make([]byte, 0, len(PrefixCheckpoint)+1+len(intentID)+1+1+len(stepID))
	out = append(out, PrefixCheckpoint...)
	var err error
	out, err = PutLPString(out, intentID)
	if err != nil {
		return nil, fmt.Errorf("keys.CheckpointKey: intent: %w", err)
	}
	out = append(out, '/')
	out, err = PutLPString(out, stepID)
	if err != nil {
		return nil, fmt.Errorf("keys.CheckpointKey: step: %w", err)
	}
	return out, nil
}

// CheckpointIntentPrefix returns chk/<lpstr intent_id>/, the prefix
// for scanning every step's checkpoint under one intent (audit and
// re-entry helpers). Same constraints as CheckpointKey.
func CheckpointIntentPrefix(intentID string) ([]byte, error) {
	if err := ValidateNoSeparator(intentID); err != nil {
		return nil, fmt.Errorf("keys.CheckpointIntentPrefix: %w", err)
	}
	out := make([]byte, 0, len(PrefixCheckpoint)+1+len(intentID)+1)
	out = append(out, PrefixCheckpoint...)
	var err error
	out, err = PutLPString(out, intentID)
	if err != nil {
		return nil, fmt.Errorf("keys.CheckpointIntentPrefix: %w", err)
	}
	out = append(out, '/')
	return out, nil
}

// ParseCheckpointKey extracts (intentID, stepID) from a chk/ key.
// Returns ErrBadPrefix if k doesn't start with chk/, ErrShortKey on
// truncation, or wraps lpstring decode errors.
func ParseCheckpointKey(k []byte) (intentID, stepID string, err error) {
	if !hasPrefix(k, PrefixCheckpoint) {
		err = ErrBadPrefix
		return
	}
	tail := k[len(PrefixCheckpoint):]
	intentID, tail, err = ReadLPString(tail)
	if err != nil {
		return "", "", err
	}
	if len(tail) < 1 || tail[0] != '/' {
		return "", "", ErrShortKey
	}
	tail = tail[1:]
	stepID, tail, err = ReadLPString(tail)
	if err != nil {
		return "", "", err
	}
	if len(tail) != 0 {
		return "", "", fmt.Errorf("keys.ParseCheckpointKey: trailing %d bytes", len(tail))
	}
	return intentID, stepID, nil
}

// hasPrefix reports whether b begins with p, allocation-free.
func hasPrefix(b, p []byte) bool {
	if len(b) < len(p) {
		return false
	}
	for i := range p {
		if b[i] != p[i] {
			return false
		}
	}
	return true
}

// ValidateNoSeparator returns an error if s contains '/' (0x2F). Path
// components inside keys must not contain the separator.
func ValidateNoSeparator(s string) error {
	for i := 0; i < len(s); i++ {
		if s[i] == '/' {
			return fmt.Errorf("keys: component %q contains forbidden '/'", s)
		}
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
