// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package scope

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"time"

	"matrix/cortex/memory"
	"matrix/cortex/snapshot"
)

// KeyResolver resolves a GrantedBy agent ref to its ed25519 public
// key. Implementations live in the agent runtime / tools/registry
// layer; cortex uses the interface so it has no dependency on DID
// resolution mechanics (D4: chain coupling lives in tools/).
type KeyResolver interface {
	// ResolveAgentKey returns the public key for ref. Returns
	// ErrUnknownAgent if the ref is not known to the resolver.
	ResolveAgentKey(ref string) (ed25519.PublicKey, error)
}

// StaticKeyResolver maps known refs to known public keys. Used by
// tests and the CLI; production swaps in a registry-backed resolver.
// Map values must be ed25519 public keys (32 bytes).
type StaticKeyResolver map[string]ed25519.PublicKey

// ResolveAgentKey implements KeyResolver.
func (s StaticKeyResolver) ResolveAgentKey(ref string) (ed25519.PublicKey, error) {
	pk, ok := s[ref]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownAgent, ref)
	}
	if len(pk) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: %q has wrong key size %d", ErrUnknownAgent, ref, len(pk))
	}
	return pk, nil
}

// VerifyOpts holds the optional inputs to Verify. Required inputs (the
// Scope, the snapshot.State, the resolver) are positional on Verify
// for clarity at the call site.
type VerifyOpts struct {
	// Now is the wall-clock used for expiry comparison. Zero defers
	// to time.Now(). Tests always populate this for determinism.
	Now time.Time

	// SkipSnapshotResolution skips the FindSnapshotByRoot check.
	// Reserved for replay tooling that walks the journal forward and
	// already knows the snapshot existed; never used on the hot read
	// path. Default false.
	SkipSnapshotResolution bool
}

// Verify runs the full scope verification chain (research/06-agents.md
// §7.2). Returns nil iff:
//
//  1. SchemaVersion matches the package constant.
//  2. Include is non-empty (at least one criterion populated).
//  3. Now ≤ ExpiresAt (or ExpiresAt is zero).
//  4. Signature is a valid ed25519 sig from GrantedBy's pubkey over
//     UnsignedBytes(s).
//  5. SnapshotHash is resolvable: there exists a snap/<seq> manifest
//     with OverallRoot == SnapshotHash. Skipped if
//     opts.SkipSnapshotResolution.
//  6. If Proofs is non-nil:
//     - len(Proofs.Proofs) == len(Include.IDs)
//     - each proof's KeyHash matches snapshot.HashMemoryKey(IDs[i])
//     - Proofs.VerifyAgainstManifest(found-manifest) passes
//
// Verify does NOT enforce against any specific memory — that is the
// per-read enforcement done by Scope.Allows / cortex.enforceScope.
func Verify(s *Scope, snapState *snapshot.State, resolver KeyResolver, opts VerifyOpts) error {
	if s == nil {
		return errors.New("scope.Verify: nil scope")
	}
	if s.SchemaVersion != SchemaVersion {
		return fmt.Errorf("%w: got %d want %d", ErrSchemaVersion, s.SchemaVersion, SchemaVersion)
	}
	if s.Include.IsEmpty() {
		return ErrEmptyInclude
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if !s.ExpiresAt.IsZero() && now.After(s.ExpiresAt) {
		return fmt.Errorf("%w: now=%s expires_at=%s", ErrScopeExpired, now.UTC(), s.ExpiresAt.UTC())
	}

	if resolver == nil {
		return errors.New("scope.Verify: nil KeyResolver")
	}
	pub, err := resolver.ResolveAgentKey(s.GrantedBy)
	if err != nil {
		return err
	}
	if err := VerifySignature(s, pub); err != nil {
		return err
	}

	if !opts.SkipSnapshotResolution {
		if snapState == nil {
			return errors.New("scope.Verify: nil snapshot state")
		}
		manifest, err := snapState.FindSnapshotByRoot(s.SnapshotHash)
		if err != nil {
			if errors.Is(err, snapshot.ErrSnapshotNotFound) {
				return fmt.Errorf("%w: %x", ErrSnapshotUnresolved, s.SnapshotHash[:8])
			}
			return err
		}
		if err := verifyProofsAgainstInclude(s, manifest); err != nil {
			return err
		}
	}

	return nil
}

// verifyProofsAgainstInclude checks that scope.Proofs is consistent
// with scope.Include.IDs and verifies the multi-proof against the
// resolved manifest's memories root.
//
// If Include.IDs is empty, Proofs MUST be nil (a Type/Tag/Frame-only
// scope ships no proofs because there are no specific keys to prove).
// If Include.IDs is non-empty, Proofs MUST have exactly that many
// proofs in matching order, each KeyHash matching the corresponding
// id's HashMemoryKey.
func verifyProofsAgainstInclude(s *Scope, manifest *snapshot.Manifest) error {
	if len(s.Include.IDs) == 0 {
		if s.Proofs != nil {
			return fmt.Errorf("%w: scope has Proofs but no Include.IDs", ErrProofMismatch)
		}
		return nil
	}
	if s.Proofs == nil {
		return fmt.Errorf("%w: scope has Include.IDs but no Proofs", ErrProofMismatch)
	}
	if len(s.Proofs.Proofs) != len(s.Include.IDs) {
		return fmt.Errorf("%w: %d proofs for %d ids", ErrProofMismatch, len(s.Proofs.Proofs), len(s.Include.IDs))
	}
	for i, id := range s.Include.IDs {
		want := snapshot.HashMemoryKey(memoryIDToBytes(id))
		if s.Proofs.Proofs[i].KeyHash != want {
			return fmt.Errorf("%w: proof %d KeyHash mismatch", ErrProofMismatch, i)
		}
	}
	if err := s.Proofs.VerifyAgainstManifest(manifest); err != nil {
		return err
	}
	return nil
}

// memoryIDToBytes copies a memory.ID (which is [16]byte) into the
// [16]byte alias the snapshot package expects. Both are 16-byte
// arrays; the alias is just a different named type so we re-cast.
func memoryIDToBytes(id memory.ID) [16]byte {
	var out [16]byte
	copy(out[:], id[:])
	return out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
