// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package compose

import (
	"reflect"
	"testing"
	"testing/quick"
)

func TestDedupOrderAndOverlapReasons(t *testing.T) {
	items := []Item{
		{SourceNamespace: "stable", SourceID: "identity", SemanticKind: "stable_identity", RevisionIdentity: "2", Content: "Ada is the preferred person name"},
		{SourceNamespace: "stable", SourceID: "identity", SemanticKind: "stable_identity", RevisionIdentity: "2", Content: "duplicate identity bytes"},
		{SourceNamespace: "stable", SourceID: "identity", SemanticKind: "stable_identity", RevisionIdentity: "1", Content: "old identity"},
		{SourceNamespace: "transcript", SourceID: "7", SemanticKind: "transcript_user", Content: "Inspect cobalt"},
		{SourceNamespace: "memory", SourceID: "8", SemanticKind: "recall", Content: "user: inspect cobalt"},
		{SourceNamespace: "tool", SourceID: "call-1", SemanticKind: "tool_result", Content: "tool result: service healthy"},
		{SourceNamespace: "memory", SourceID: "9", SemanticKind: "memory", Content: "service healthy"},
		{SourceNamespace: "memory", SourceID: "10", SemanticKind: "memory", Content: "memory: ada is the preferred person name"},
	}
	got, manifest := Deduplicate(items)
	if len(got) != 3 {
		t.Fatalf("included = %#v", got)
	}
	wantReasons := []string{"included", "duplicate_source_identity", "superseded_revision", "included", "transcript_recall_overlap", "included", "tool_result_memory_overlap", "stable_identity_memory_overlap"}
	for index, want := range wantReasons {
		if manifest.Entries[index].Reason != want {
			t.Fatalf("reason[%d] = %q, want %q", index, manifest.Entries[index].Reason, want)
		}
	}
}

func TestDedupIsDeterministicAndDoesNotMutateInput(t *testing.T) {
	property := func(values []string) bool {
		items := make([]Item, len(values))
		for index, value := range values {
			items[index] = Item{SourceNamespace: "property", SourceID: value, SemanticKind: "memory", Content: value}
		}
		before := append([]Item{}, items...)
		first, firstManifest := Deduplicate(items)
		second, secondManifest := Deduplicate(items)
		return reflect.DeepEqual(items, before) && reflect.DeepEqual(first, second) && reflect.DeepEqual(firstManifest, secondManifest)
	}
	if err := quick.Check(property, &quick.Config{MaxCount: 500}); err != nil {
		t.Fatal(err)
	}
}
