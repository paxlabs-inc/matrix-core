// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package snapshot

import "errors"

// ErrNodeMissing is returned by MMR.Node when a position is queried but
// the underlying accum/ key is absent. This indicates either a programmer
// error (querying a not-yet-appended position) or accum/ corruption that
// the replay harness will fix.
var ErrNodeMissing = errors.New("snapshot: MMR node missing")

// ErrUnknownNamespace is returned by State.Stage / Root when the namespace
// argument is not one of the anchored set ("memories" | "edges").
var ErrUnknownNamespace = errors.New("snapshot: unknown namespace")

// ErrInvalidProof is returned by VerifyMembership / VerifyNonMembership
// when the proof's structural fields are malformed (e.g. depth out of
// range, sibling list length wrong). Cryptographic mismatch is also
// reported through this error so callers don't have to distinguish.
var ErrInvalidProof = errors.New("snapshot: invalid proof")

// ErrSnapshotNotFound is returned by State.LoadSnapshot when the
// requested seq has no persisted snap/ entry.
var ErrSnapshotNotFound = errors.New("snapshot: not found")

// Copyright © 2026 Paxlabs Inc. All rights reserved.
