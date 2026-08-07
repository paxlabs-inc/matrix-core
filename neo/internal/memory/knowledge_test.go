// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package memory

import (
	"context"
	"encoding/json"
	"testing"

	"matrix/neo/internal/config"
)

func TestKnowledgePersistsTypedGraphAndSupportsExactSemanticLifecycle(t *testing.T) {
	ctx := context.Background()
	pager, err := Open(config.Config{DataRoot: t.TempDir(), NeocortexActor: "knowledge-test"})
	if err != nil {
		t.Fatal(err)
	}
	defer pager.Close()

	topic, err := pager.CreateKnowledgeTopic(ctx, "Distributed systems", "")
	if err != nil {
		t.Fatal(err)
	}
	document, err := pager.ImportKnowledge(ctx, KnowledgeImportRequest{
		TopicID: topic.ID, Title: "Consensus notes", Content: "Raft uses a replicated log and a leader election protocol.",
		SourceKind: "article", SourceTitle: "Consensus reference", SourceURL: "https://example.com/raft", RetentionDays: 30,
		Entities:      []KnowledgeEntityInput{{Name: "Raft", Kind: "protocol"}, {Name: "Replicated log", Kind: "concept"}},
		Relationships: []KnowledgeRelationshipInput{{From: "Raft", To: "Replicated log", Kind: "uses"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if document.SourceID == "" || len(pager.KnowledgeSnapshot().Relationships) != 1 {
		t.Fatalf("typed import = %+v", pager.KnowledgeSnapshot())
	}
	exact, err := pager.SearchKnowledge("leader election", 10)
	if err != nil || len(exact) != 1 || !exact[0].Exact || exact[0].Source.URL != "https://example.com/raft" {
		t.Fatalf("exact search = %+v err=%v", exact, err)
	}
	semantic, err := pager.SearchKnowledge("replicated protocol", 10)
	if err != nil || len(semantic) != 1 || semantic[0].Score <= 0 {
		t.Fatalf("semantic search = %+v err=%v", semantic, err)
	}
	renamed := "Consensus and coordination"
	moved, err := pager.UpdateKnowledgeDocument(ctx, document.ID, KnowledgeDocumentUpdate{Title: &renamed})
	if err != nil || moved.Version != 2 || len(moved.Versions) != 2 || moved.Versions[1].Supersedes != 1 {
		t.Fatalf("versioned update = %+v err=%v", moved, err)
	}

	blob, _, err := pager.client.LatestCheckpoint(ctx, knowledgeCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	var persisted KnowledgeState
	if err := json.Unmarshal(blob, &persisted); err != nil {
		t.Fatal(err)
	}
	if persisted.Documents[document.ID].Title != renamed || persisted.Sources[document.SourceID].ContentHash == "" {
		t.Fatalf("persisted state = %+v", persisted)
	}

	isolated, err := Open(config.Config{DataRoot: t.TempDir(), NeocortexActor: "other-tenant"})
	if err != nil {
		t.Fatal(err)
	}
	defer isolated.Close()
	if len(isolated.KnowledgeSnapshot().Documents) != 0 {
		t.Fatal("knowledge crossed tenant pager boundaries")
	}
}
