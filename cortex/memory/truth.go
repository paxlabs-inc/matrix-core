// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import "time"

// TruthStatus is the non-sensitive reason a memory is included or excluded
// from a current-truth retrieval lane.
type TruthStatus string

const (
	TruthCurrent     TruthStatus = "current"
	TruthMissing     TruthStatus = "missing"
	TruthTombstoned  TruthStatus = "tombstoned"
	TruthNotYetValid TruthStatus = "not_yet_valid"
	TruthSuperseded  TruthStatus = "superseded"
	TruthExpired     TruthStatus = "expired"
)

// CurrentTruthAt applies the canonical current-truth policy shared by every
// retrieval lane. The valid interval is half-open [ValidFrom, ValidUntil),
// with ValidFrom defaulting to the version transaction time.
func CurrentTruthAt(h *Head, v *Version, asOf time.Time) (bool, TruthStatus) {
	if h == nil || v == nil {
		return false, TruthMissing
	}
	if h.Tombstoned != nil {
		return false, TruthTombstoned
	}
	return VersionCurrentAt(v, asOf)
}

// VersionCurrentAt is the validity-only half of CurrentTruthAt for callers
// that intentionally audit a head separately.
func VersionCurrentAt(v *Version, asOf time.Time) (bool, TruthStatus) {
	if v == nil {
		return false, TruthMissing
	}
	from := v.CreatedAt
	if v.ValidFrom != nil {
		from = *v.ValidFrom
	}
	if asOf.Before(from) {
		return false, TruthNotYetValid
	}
	if v.ValidUntil != nil && !asOf.Before(*v.ValidUntil) {
		return false, TruthSuperseded
	}
	if v.ExpiresAt != nil && !asOf.Before(*v.ExpiresAt) {
		return false, TruthExpired
	}
	return true, TruthCurrent
}
