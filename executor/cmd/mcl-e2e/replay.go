// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"encoding/hex"
	"fmt"

	"matrix/cortex"
	"matrix/cortex/replay"
)

// VerifyReplayInvariant exercises the §13.4 byte-identical replay path:
//  1. Stop the embedder (Phase 11 invariant: Rebuild requires no embedder).
//  2. Take a final snapshot for audit.
//  3. Capture pre-rebuild OverallRoot.
//  4. Run cortex.Rebuild — drops indexes/, walks j/, re-derives every
//     derived projection.
//  5. Verify Pre==Post via replay.VerifyPreservesRoot.
//
// Returns Pre/Post for the assertion layer to log + diff.
func VerifyReplayInvariant(c *cortex.Cortex, t *Transcript, assert *AssertCtx) (preHex, postHex string, err error) {
	if err := c.StopEmbedder(); err != nil {
		return "", "", fmt.Errorf("replay: StopEmbedder: %w", err)
	}
	t.Event("replay.embedder.stopped", "replay", nil)

	snap, err := c.Snapshot("e2e-final")
	if err != nil {
		return "", "", fmt.Errorf("replay: Snapshot: %w", err)
	}
	t.Event("replay.snapshot.taken", "replay", map[string]interface{}{
		"seq":          snap.JournalSeq,
		"overall_root": hex.EncodeToString(snap.OverallRoot[:]),
		"memories":     snap.Counters.Memories,
		"edges":        snap.Counters.Edges,
		"tombstoned":   snap.Counters.Tombstoned,
	})

	pre, err := c.OverallRoot()
	if err != nil {
		return "", "", fmt.Errorf("replay: pre-OverallRoot: %w", err)
	}
	preHex = hex.EncodeToString(pre[:])

	res, err := c.Rebuild(cortex.RebuildOptions{})
	if err != nil {
		return "", "", fmt.Errorf("replay: Rebuild: %w", err)
	}

	post, err := c.OverallRoot()
	if err != nil {
		return "", "", fmt.Errorf("replay: post-OverallRoot: %w", err)
	}
	postHex = hex.EncodeToString(post[:])

	t.Event("replay.rebuild.complete", "replay", map[string]interface{}{
		"pre_overall_root":        preHex,
		"post_overall_root":       postHex,
		"memories_scanned":        res.MemoriesScanned,
		"edges_scanned":           res.EdgesScanned,
		"journal_leaves_appended": res.JournalLeavesAppended,
		"salience_bumps_applied":  res.SalienceBumpsApplied,
	})

	if verr := replay.VerifyPreservesRoot(res); verr != nil {
		assert.True("replay: VerifyPreservesRoot", false, verr.Error())
		return preHex, postHex, fmt.Errorf("replay: VerifyPreservesRoot: %w", verr)
	}
	assert.True("replay: VerifyPreservesRoot pre==post", true, fmt.Sprintf("root=%s", preHex[:16]))
	assert.Equal("replay: byte-identical OverallRoot Pre==Post", preHex, postHex)
	return preHex, postHex, nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
