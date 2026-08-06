// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package compose

import (
	"strings"
	"testing"
)

func TestTrimLawPreservesLatestMessageAndUnconsumedToolBatch(t *testing.T) {
	items := []Item{
		{SourceNamespace: "stable", SourceID: "charter", SemanticKind: "stable_identity", Content: "identity", Sector: SectorStableIdentity, NeverTrim: true},
		{SourceNamespace: "memory", SourceID: "old-recall", SemanticKind: "memory", Content: strings.Repeat("old recall ", 200), Sector: SectorLongTermMemory},
		{SourceNamespace: "memory", SourceID: "ambient", SemanticKind: "memory", Content: strings.Repeat("ambient ", 200), Sector: SectorLongTermMemory},
		{SourceNamespace: "capsule", SourceID: "warm", SemanticKind: "capsule", Content: strings.Repeat("warm ", 200), Sector: SectorWarmCapsules},
		{SourceNamespace: "transcript", SourceID: "old", SemanticKind: "transcript_user", Content: strings.Repeat("old transcript ", 200), Sector: SectorRecentTranscript},
		{SourceNamespace: "transcript", SourceID: "latest", SemanticKind: "transcript_user", Content: "latest genuine message", Sector: SectorLatestMessage, NeverTrim: true},
		{SourceNamespace: "tool", SourceID: "result-1", SemanticKind: "tool_result", Content: strings.Repeat("critical evidence ", 100), Sector: SectorUnconsumedToolBatch, NeverTrim: true},
	}
	policy := DefaultSectorPolicy(180, 32)
	result := ApplySectorBudgets(items, policy)
	kept := map[string]bool{}
	for _, item := range result.Items {
		kept[item.SourceID] = true
	}
	if !kept["charter"] || !kept["latest"] || !kept["result-1"] {
		t.Fatalf("never-trim set was lost: %#v", kept)
	}
	if kept["old-recall"] || kept["ambient"] || kept["warm"] || kept["old"] {
		t.Fatalf("optional context survived pressure ahead of mandatory evidence: %#v", kept)
	}
	if len(result.Diagnostics) == 0 {
		t.Fatal("mandatory overflow did not emit diagnostics")
	}
}

func TestNineReservedSectorsRemainDistinct(t *testing.T) {
	sectors := []Sector{
		SectorStableIdentity, SectorLatestMessage, SectorRecentTranscript,
		SectorWorkingState, SectorUnconsumedToolBatch, SectorWarmCapsules,
		SectorLongTermMemory, SectorToolSchemas, SectorResponseReserve,
	}
	seen := map[Sector]bool{}
	for _, sector := range sectors {
		if seen[sector] {
			t.Fatalf("duplicate sector %d", sector)
		}
		seen[sector] = true
	}
	if len(seen) != 9 {
		t.Fatalf("sector count = %d", len(seen))
	}
}
