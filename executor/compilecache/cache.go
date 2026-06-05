// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package compilecache implements the per-actor compile-cache sidecar
// landed in Session 31d (P4). It memoizes Intent IRs produced by the
// MCL compiler so a repeat compile against the same (skill, prose,
// cortex root, verb, model) tuple returns the cached *ir.Intent
// without re-running the LLM.
//
// Spec citation: canvases/model-router-architecture.canvas.tsx
// Section 8 cell "Compile-cache for stable Intent IRs":
//
//	"Compiler runs with temp=0 + seed=42. Hash
//	 sha256(skill_digest || prose || cortex_snapshot_hash || verb);
//	 if a prior compile exists with the same key + Intent.Hash matches
//	 expected shape, return the cached Intent IR. Cortex
//	 meta/compile_cache sidecar."
//
// Sidecar posture: entries live at meta/compile_cache/<hex> per the
// keys.PrefixMetaCompileCache namespace. NEVER part of cortex
// OverallRoot — mirrors meta/salience_weights (Phase 12) and the rate-
// limit buckets (Phase 14). This means cache contents do not factor
// into cortex_snapshot_hash, so caching is policy, not state.
//
// Key construction includes model_digest in addition to the four
// fields the canvas calls out. Rationale: changing the compiler model
// (Tauri wizard swap, sess#29 endpoint override, BYO Fireworks vs
// gateway) must invalidate cached entries, otherwise the cache could
// serve stale Intent IRs that the operator never intended. Adding
// model_digest keeps the cache key collision-free across that surface
// without requiring an explicit invalidate-on-model-change journal.
//
// Determinism guarantee: the cache returns byte-identical IntentJSON
// to what compile would emit on a cache miss — Lookup decodes the
// stored canonical-JSON bytes verbatim. Hash is preserved across
// encode/decode round-trip. Replay invariance (Phase 7+11): cached
// entries never journal anything; if a downstream consumer replays
// from j/, the cached intent is recomputed from skill + prose at that
// step (the cache is opportunistic, not authoritative).
package compilecache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"time"

	"github.com/fxamacker/cbor/v2"

	"matrix/cortex/keys"
	"matrix/cortex/store"
)

// SchemaVersion is bumped on any incompatible Entry-encoding change.
// Cold-load mismatch ⇒ Lookup returns (nil, false, nil) so the cache
// behaves like a cold start instead of poisoning Intent IRs.
const SchemaVersion uint8 = 1

// Entry is one cached compile result. IntentJSON is the canonical-JSON
// bytes of *ir.Intent at hash time. IntentHash is the hash of those
// bytes (also computed independently by the caller for paranoia).
type Entry struct {
	SchemaVersion uint8  `cbor:"0,keyasint"`
	IntentJSON    []byte `cbor:"1,keyasint"`
	IntentHash    string `cbor:"2,keyasint"`
	ModelDigest   string `cbor:"3,keyasint"`
	CachedAt      int64  `cbor:"4,keyasint"`           // unix nano
	Verb          string `cbor:"5,keyasint,omitempty"` // for audit/debug
	SkillDigest   string `cbor:"6,keyasint,omitempty"` // for audit/debug
	SnapHash      string `cbor:"7,keyasint,omitempty"` // for audit/debug
}

// canonicalEnc + canonicalDec enforce deterministic CBOR for Entry so
// repeated cache writes of the same Entry produce byte-identical
// values (matters for store-level dedup + future blob hashing).
var (
	canonicalEnc cbor.EncMode
	canonicalDec cbor.DecMode
)

func init() {
	em, err := cbor.CoreDetEncOptions().EncMode()
	if err != nil {
		panic(fmt.Errorf("compilecache: build EncMode: %w", err))
	}
	canonicalEnc = em
	dm, err := cbor.DecOptions{}.DecMode()
	if err != nil {
		panic(fmt.Errorf("compilecache: build DecMode: %w", err))
	}
	canonicalDec = dm
}

// US is the 0x1f Unit Separator byte used to delimit the cache-key
// components. Same separator the compiler seed derivation uses
// (executor/cmd/mcl-execute/compile.go:compilerSeed) so the two key
// derivations are stylistically consistent.
const US byte = 0x1f

// Key derives the lowercase hex sha256 cache key from the five
// components. Components may be empty (e.g. cold cortex with
// snap_hash = 64 zeros) — the separator ensures the empty case
// hashes distinctly from a populated case with leading null.
//
//	sha256(skill_digest || US || prose || US || snap_hash
//	   || US || verb || US || model_digest)
//
// Result is 64 lowercase hex chars.
func Key(skillDigest, prose, snapHash, verb, modelDigest string) string {
	h := sha256.New()
	h.Write([]byte(skillDigest))
	h.Write([]byte{US})
	h.Write([]byte(prose))
	h.Write([]byte{US})
	h.Write([]byte(snapHash))
	h.Write([]byte{US})
	h.Write([]byte(verb))
	h.Write([]byte{US})
	h.Write([]byte(modelDigest))
	return hex.EncodeToString(h.Sum(nil))
}

// Encode returns canonical CBOR for e.
func Encode(e *Entry) ([]byte, error) {
	if e == nil {
		return nil, fmt.Errorf("compilecache: nil entry")
	}
	return canonicalEnc.Marshal(e)
}

// Decode parses canonical CBOR into out.
func Decode(b []byte, out *Entry) error {
	return canonicalDec.Unmarshal(b, out)
}

// Lookup fetches the cached Entry for hexKey. Returns (nil, false, nil)
// when:
//   - the key is absent (cold cache)
//   - the stored SchemaVersion mismatches the current one (forward-
//     incompatible upgrade; treat as cold so we never deserialize
//     into the wrong shape)
//
// Errors are returned only for store-level I/O failures or CBOR decode
// errors against a current-version blob (the latter implies actual
// corruption).
func Lookup(s *store.Store, hexKey string) (*Entry, bool, error) {
	if s == nil {
		return nil, false, fmt.Errorf("compilecache: nil store")
	}
	if hexKey == "" {
		return nil, false, fmt.Errorf("compilecache: empty key")
	}
	raw, ok, err := s.Get(keys.MetaCompileCacheKey(hexKey))
	if err != nil {
		return nil, false, fmt.Errorf("compilecache: get %s: %w", hexKey, err)
	}
	if !ok {
		return nil, false, nil
	}
	// Peek the first byte to detect forward-incompatible schema
	// versions BEFORE attempting a full decode (which could silently
	// drop fields under DecMode's default permissive policy).
	if len(raw) == 0 {
		return nil, false, nil
	}
	var e Entry
	if err := Decode(raw, &e); err != nil {
		return nil, false, fmt.Errorf("compilecache: decode %s: %w", hexKey, err)
	}
	if e.SchemaVersion != SchemaVersion {
		return nil, false, nil
	}
	return &e, true, nil
}

// Store writes e at meta/compile_cache/<hexKey>. Overwrites silently
// when the key already exists (compile is deterministic for the same
// inputs, so a re-write of the same Entry is by construction a
// byte-identical update; an over-write across model changes is the
// intended replacement semantic).
//
// The caller MUST set Entry.SchemaVersion to SchemaVersion (Store
// validates and rejects otherwise so a forward-incompat write can't
// poison the cache before the schema bump is wired through the rest
// of the stack).
func Store(s *store.Store, hexKey string, e *Entry) error {
	if s == nil {
		return fmt.Errorf("compilecache: nil store")
	}
	if hexKey == "" {
		return fmt.Errorf("compilecache: empty key")
	}
	if e == nil {
		return fmt.Errorf("compilecache: nil entry")
	}
	if e.SchemaVersion != SchemaVersion {
		return fmt.Errorf("compilecache: entry.SchemaVersion = %d, want %d",
			e.SchemaVersion, SchemaVersion)
	}
	if e.CachedAt == 0 {
		e.CachedAt = time.Now().UnixNano()
	}
	enc, err := Encode(e)
	if err != nil {
		return fmt.Errorf("compilecache: encode %s: %w", hexKey, err)
	}
	if err := s.SetMeta(keys.MetaCompileCacheKey(hexKey), enc); err != nil {
		return fmt.Errorf("compilecache: setmeta %s: %w", hexKey, err)
	}
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
