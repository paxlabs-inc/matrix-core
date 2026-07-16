// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package store

// vaultseam.go — data-at-rest encryption BELOW the cortex hash boundary.
//
// User-content Pebble VALUES are sealed at this store seam (write) and opened
// here on every read, so every layer above — journal decode, memory decode,
// session/rollup/story records, replay, snapshot staging — operates on the
// canonical plaintext logical encodings. All hashes anchoring the cortex
// (journal MMR leaf hashes, SMT value hashes, OverallRoot) are computed by
// callers OVER THOSE PLAINTEXT ENCODINGS before the value reaches this seam
// (live writes) or after this seam opened it (replay re-hash), so encryption
// changes persisted bytes ONLY: OverallRoot and D11 replay byte-identity are
// unaffected by construction.
//
// Sealed namespaces (user content): j/, m/, mv/, sess/, sessblob/, roll/,
// enr/, story/, chk/, vec/meta/.
//
// Deliberately plaintext (documented residual leakage; volume/database
// encryption is the required complementary layer):
//   - ALL Pebble keys (namespaces, ULIDs, seqs, conversation ids in key form),
//     value sizes, and access patterns;
//   - hash/accumulator state: accum/ (journal MMR), idx/smt/ (SMT node
//     cache), snap/ (snapshot manifests) — hashes over logical data, no
//     user content;
//   - derived rankings and markers: idx/ (empty-valued index rows),
//     salience/ + meta/salience_weights (floats), tomb/, e/ edge rows,
//     meta/ progress markers (recomputable from the journal);
//   - the vector index file under <root>/<actor>/indexes/ (embedding
//     geometry; vec/meta/ payloads ARE sealed).

import (
	"fmt"

	"matrix/cortex/keys"
	"matrix/vault"
)

const (
	storeCortexValue = "cortex.value"
	schemaValue1     = "value.v1"
)

// sealedPrefixes are the namespaces whose values carry user content.
var sealedPrefixes = [][]byte{
	keys.PrefixJournal,       // j/       journal entries (turn content, payloads)
	keys.PrefixMemoryHead,    // m/       memory heads
	keys.PrefixMemoryVersion, // mv/      memory versions
	keys.PrefixSession,       // sess/    session/transcript records
	keys.PrefixSessionBlob,   // sessblob/ spilled tool_result payloads
	keys.PrefixRollup,        // roll/    temporal-ladder rollups
	keys.PrefixEnrich,        // enr/     LLM enrichment records
	keys.PrefixStory,         // story/   story-so-far records
	keys.PrefixCheckpoint,    // chk/     compaction checkpoints
	keys.PrefixVecMeta,       // vec/meta/ embedding metadata payloads
	keys.PrefixLexical,       // lex/      transcript lexical postings
}

// SetVault wires the fail-closed data-at-rest session and owning user DID into
// the store. MUST be called immediately after Open, before any write or read,
// and never concurrently with either (boot-time wiring, same posture as every
// other SetVault in the repo). A nil session leaves the store on legacy
// plaintext (dev/CLI).
func (s *Store) SetVault(sess *vault.Session, user string) {
	if s == nil {
		return
	}
	s.vault = sess
	s.vaultUser = user
}

// valueAD reconstructs a value's associated data from its full Pebble key —
// never stored, so ciphertext moved between users, namespaces, ids, versions,
// or seqs fails authentication.
func (s *Store) valueAD(key []byte) vault.AD {
	return vault.AD{User: s.vaultUser, Store: storeCortexValue, Stream: string(key), Schema: schemaValue1}
}

// sealedNamespace reports whether the key belongs to a namespace whose values
// carry user content and are sealed at rest.
func sealedNamespace(key []byte) bool {
	for _, p := range sealedPrefixes {
		if len(key) >= len(p) && string(key[:len(p)]) == string(p) {
			return true
		}
	}
	return false
}

// sealValue seals a value for its key when the namespace is in scope. The
// caller has already derived every hash it needs from the plaintext.
func (s *Store) sealValue(key, value []byte) ([]byte, error) {
	if !sealedNamespace(key) {
		return value, nil
	}
	out, err := s.vault.MaybeSealFile(s.valueAD(key), value)
	if err != nil {
		return nil, fmt.Errorf("store: seal %q: %w", key, err)
	}
	return out, nil
}

// SealValue seals a value for its key exactly as the write seam would (a
// no-namespace or plaintext-session value returns unchanged). Exported for
// the per-user migrator, which rewrites existing plaintext values in place
// and must produce bytes indistinguishable from live sealed writes.
func (s *Store) SealValue(key, value []byte) ([]byte, error) {
	return s.sealValue(key, value)
}

// openValue opens a possibly-sealed value read at key. Legacy plaintext
// values pass through (sniffing reader carries pre-migration stores); a
// sealed value with no session, the wrong user, or any tampering is a hard
// error — never silently returned as ciphertext.
func (s *Store) openValue(key, value []byte) ([]byte, error) {
	if !vault.IsVault(value) || !sealedNamespace(key) {
		return value, nil
	}
	uv := s.vault.UserVault()
	if uv == nil {
		return nil, fmt.Errorf("store: sealed value at %q without a vault session", key)
	}
	out, err := uv.OpenFile(s.valueAD(key), value)
	if err != nil {
		return nil, fmt.Errorf("store: open %q: %w", key, err)
	}
	return out, nil
}
