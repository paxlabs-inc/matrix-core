// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package scope

import "errors"

// Sentinel errors for scope verification + enforcement.
//
// All cortex read paths that fail an enforceScope check return one of
// these (or wrap one). The CLI, the journaling violation log, and any
// downstream agent runtime can switch on them via errors.Is.
var (
	// ErrSchemaVersion is returned when the scope's SchemaVersion is
	// not the current one (forwards or backwards). Bumping the schema
	// invalidates outstanding scopes.
	ErrSchemaVersion = errors.New("scope: unknown schema version")

	// ErrSignatureInvalid covers any signature failure: bad pubkey,
	// bad sig length, ed25519 verify mismatch.
	ErrSignatureInvalid = errors.New("scope: signature invalid")

	// ErrScopeExpired is returned when now > scope.ExpiresAt.
	ErrScopeExpired = errors.New("scope: expired")

	// ErrSnapshotUnresolved is returned when scope.SnapshotHash does
	// not match any persisted snap/<seq> manifest's OverallRoot
	// (research/06-agents.md §7.2 step 2).
	ErrSnapshotUnresolved = errors.New("scope: snapshot hash not resolvable")

	// ErrProofMismatch is returned when the multi-proof's per-proof
	// KeyHash does not match the corresponding Include.IDs entry, or
	// when proof count != include id count.
	ErrProofMismatch = errors.New("scope: proof / include mismatch")

	// ErrActorMismatch is returned when scope.Actor does not match the
	// store actor of the cortex being read.
	ErrActorMismatch = errors.New("scope: actor mismatch")

	// ErrEmptyInclude is returned when the scope's Include selector
	// has no populated criteria. Empty Include = nothing matches; we
	// reject at the API boundary so a misconfigured grant doesn't
	// silently behave like default-deny without the caller noticing.
	ErrEmptyInclude = errors.New("scope: include selector is empty")

	// ErrViolation is returned when an otherwise-valid scope is
	// applied to a memory the scope does not allow (Include miss, or
	// Exclude hit). The violation log records this with severity:high.
	ErrViolation = errors.New("scope: violation — memory not in scope")

	// ErrNotWritable is returned by write-path enforcement (UpdateHead
	// etc.) when the scope's Writable bit is false.
	ErrNotWritable = errors.New("scope: not writable")

	// ErrUnknownAgent is returned by KeyResolver implementations when
	// the GrantedBy ref cannot be resolved to a public key.
	ErrUnknownAgent = errors.New("scope: unknown agent ref")

	// ErrBudgetExceeded is returned by Context-path enforcement when
	// the requested BudgetTokens exceeds scope.BudgetTokens.
	ErrBudgetExceeded = errors.New("scope: budget exceeds scope cap")
)

// Copyright © 2026 Paxlabs Inc. All rights reserved.
