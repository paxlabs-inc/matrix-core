// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package envelope

import (
	"crypto/ed25519"
	"fmt"
)

// KeyResolver resolves an envelope's From principal to its ed25519
// public key. Implementations live in the agent runtime / executor /
// tools/registry layer; envelope uses the interface so it has no
// dependency on DID resolution mechanics (D4: chain coupling lives
// in tools/).
//
// Mirrors cortex/scope/verify.KeyResolver pattern.
type KeyResolver interface {
	// ResolveKey returns the public key for principal. Returns
	// a wrapped ErrUnknownPrincipal if the ref is not known.
	//
	// Principal format is matrix://agent/<did> or matrix://user/<did>.
	// Bare DIDs (without the matrix:// prefix) are NOT auto-rewritten;
	// callers normalise upstream.
	ResolveKey(principal string) (ed25519.PublicKey, error)
}

// StaticKeyResolver maps known principals to known public keys. Used
// by tests and the CLI; production swaps in a registry-backed resolver
// that uses tools/registry to look up agent + user DIDs.
type StaticKeyResolver map[string]ed25519.PublicKey

// ResolveKey implements KeyResolver.
func (s StaticKeyResolver) ResolveKey(principal string) (ed25519.PublicKey, error) {
	pk, ok := s[principal]
	if !ok {
		return nil, fmt.Errorf("%w: %q", ErrUnknownPrincipal, principal)
	}
	if len(pk) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("%w: %q has wrong key size %d", ErrUnknownPrincipal, principal, len(pk))
	}
	return pk, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
