// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"testing"
	"time"

	"matrix/cortex/memory"
	"matrix/cortex/query"
)

// containsID reports whether res surfaced the memory id (post valid-time
// filter). Test helper.
func containsID(res *query.Result, id memory.ID) bool {
	for _, m := range res.Memories {
		if m.Head.ID == id {
			return true
		}
	}
	return false
}

// TestCloseValidityFiltersByAsOf is the v3 #2 end-to-end check: closing a
// memory's valid-time stamps ValidUntil on a NEW journaled version, and
// query.Find honors Query.AsOf against the half-open [ValidFrom, ValidUntil)
// interval — before the close the memory is live, at/after it is filtered,
// default-now filters it, and IncludeTombstoned bypasses the filter for audit.
func TestCloseValidityFiltersByAsOf(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { return clk }

	uri := writePref(t, c, "model-choice", 5)
	id := idOf(uri)

	fresh, err := c.ResolveLatest(id)
	if err != nil {
		t.Fatalf("resolve fresh: %v", err)
	}
	if fresh.Version.ValidUntil != nil {
		t.Fatalf("fresh memory must have open ValidUntil, got %v", fresh.Version.ValidUntil)
	}

	// Close the interval one hour later (CloseValidity defaults until=now).
	closeAt := clk.Add(time.Hour)
	clk = closeAt
	newURI, err := c.CloseValidity(uri, time.Time{}, "andrew")
	if err != nil {
		t.Fatalf("CloseValidity: %v", err)
	}
	if newURI == uri {
		t.Fatalf("CloseValidity must bump the version, got same URI %s", newURI)
	}

	closed, err := c.ResolveLatest(id)
	if err != nil {
		t.Fatalf("resolve closed: %v", err)
	}
	if closed.Head.CurrentVersion != 2 {
		t.Fatalf("CurrentVersion = %d want 2 (close writes a new version)", closed.Head.CurrentVersion)
	}
	if closed.Version.ValidUntil == nil || !closed.Version.ValidUntil.Equal(closeAt) {
		t.Fatalf("ValidUntil = %v want %v", closed.Version.ValidUntil, closeAt)
	}
	// Data is preserved verbatim across the close.
	if !bytes.Equal(closed.Version.Data, fresh.Version.Data) {
		t.Fatalf("CloseValidity must preserve Data bytes")
	}

	before := time.Unix(1700000000, 0).Add(30 * time.Minute).UTC()
	res, err := c.Find(query.Query{Type: []memory.Type{memory.TypePreference}, Limit: 10, AsOf: &before})
	if err != nil {
		t.Fatalf("find before: %v", err)
	}
	if !containsID(res, id) {
		t.Fatalf("AsOf before close should surface the memory; got %d results", len(res.Memories))
	}

	res, err = c.Find(query.Query{Type: []memory.Type{memory.TypePreference}, Limit: 10, AsOf: &closeAt})
	if err != nil {
		t.Fatalf("find at close: %v", err)
	}
	if containsID(res, id) {
		t.Fatalf("AsOf == ValidUntil must be filtered (half-open interval)")
	}

	res, err = c.Find(query.Query{Type: []memory.Type{memory.TypePreference}, Limit: 10})
	if err != nil {
		t.Fatalf("find default: %v", err)
	}
	if containsID(res, id) {
		t.Fatalf("default AsOf=now should filter a memory closed in the past")
	}

	res, err = c.Find(query.Query{Type: []memory.Type{memory.TypePreference}, Limit: 10, IncludeTombstoned: true})
	if err != nil {
		t.Fatalf("find include: %v", err)
	}
	if !containsID(res, id) {
		t.Fatalf("IncludeTombstoned must bypass the validity filter (audit path)")
	}
}

// TestCloseValidityIdempotent verifies that closing an already-closed memory
// is a no-op so a re-relate / EdgeSupersedes revive does not churn versions.
func TestCloseValidityIdempotent(t *testing.T) {
	c := openCortex(t)
	clk := time.Unix(1700000000, 0).UTC()
	c.now = func() time.Time { return clk }

	uri := writePref(t, c, "tone", 5)
	id := idOf(uri)

	clk = clk.Add(time.Hour)
	if _, err := c.CloseValidity(uri, time.Time{}, "andrew"); err != nil {
		t.Fatalf("CloseValidity 1: %v", err)
	}
	first, err := c.ResolveLatest(id)
	if err != nil {
		t.Fatalf("resolve 1: %v", err)
	}

	clk = clk.Add(time.Hour)
	if _, err := c.CloseValidity(uri, time.Time{}, "andrew"); err != nil {
		t.Fatalf("CloseValidity 2: %v", err)
	}
	second, err := c.ResolveLatest(id)
	if err != nil {
		t.Fatalf("resolve 2: %v", err)
	}
	if second.Head.CurrentVersion != first.Head.CurrentVersion {
		t.Fatalf("idempotent close churned version: %d -> %d",
			first.Head.CurrentVersion, second.Head.CurrentVersion)
	}
	if !second.Version.ValidUntil.Equal(*first.Version.ValidUntil) {
		t.Fatalf("idempotent close moved ValidUntil: %v -> %v",
			first.Version.ValidUntil, second.Version.ValidUntil)
	}
}

// TestCloseValidityRejectsTombstoned guards the supersession companion against
// closing a soft-deleted memory.
func TestCloseValidityRejectsTombstoned(t *testing.T) {
	c := openCortex(t)
	uri := writePref(t, c, "tone", 5)
	if err := c.Tombstone(uri, "obsolete", "andrew"); err != nil {
		t.Fatalf("Tombstone: %v", err)
	}
	if _, err := c.CloseValidity(uri, time.Time{}, "andrew"); err == nil {
		t.Fatalf("CloseValidity on a tombstoned memory must error")
	}
}
