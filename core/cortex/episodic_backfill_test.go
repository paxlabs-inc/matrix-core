// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cortex

import (
	"testing"
	"time"

	"centra/core/cortex/keys"
	"centra/core/cortex/memory"
	"centra/core/cortex/store"
)

func TestEpisodicBackfillResumesIsIdempotentAndMarksHeuristic(t *testing.T) {
	root := t.TempDir()
	now := time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)
	open := func() (*store.Store, *Cortex) {
		s, err := store.Open(root, "andrew", nil)
		if err != nil {
			t.Fatal(err)
		}
		return s, New(s, WithClock(func() time.Time { return now }))
	}
	s, c := open()
	for _, msg := range []Message{
		{ConversationID: "old-conv", Role: RoleUser, Content: "legacy exactword incident", TS: now.Add(-time.Minute).UnixNano()},
		{ConversationID: "old-conv", Role: RoleAssistant, Content: "legacy response", TS: now.UnixNano()},
	} {
		if _, err := c.AppendMessage(msg); err != nil {
			t.Fatal(err)
		}
	}
	uri, err := c.Write(memory.Head{ActorScope: "andrew", DeclaredImportance: 5}, memory.FactData{SchemaVersion: 1, Statement: "legacy incident", Subject: "matrix://knowledge/test", Predicate: "note"}, WriteMeta{CreatedBy: "andrew", Provenance: memory.Provenance{Source: memory.SourceObserved}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.s.ReplaceDerivedPrefix(keys.PrefixLexical, nil); err != nil {
		t.Fatal(err)
	}
	first, err := c.EpisodicBackfill(1, "andrew")
	if err != nil || first.Processed != 1 || first.Complete {
		t.Fatalf("first batch=%+v err=%v", first, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, c = open()
	defer s.Close()
	var final EpisodicBackfillResult
	for i := 0; i < 10; i++ {
		final, err = c.EpisodicBackfill(1, "andrew")
		if err != nil {
			t.Fatal(err)
		}
		if final.Complete {
			break
		}
	}
	if !final.Complete {
		t.Fatal("backfill did not complete")
	}
	hits, err := c.QueryLexical("exactword", time.Time{}, time.Time{}, 5)
	if err != nil || len(hits) != 1 || hits[0].ConversationID != "old-conv" {
		t.Fatalf("backfilled lexical hits=%+v err=%v", hits, err)
	}
	slice, err := c.ExpandToTranscript(uri, 0)
	if err != nil || slice == nil || slice.Exact || slice.ConversationID != "old-conv" {
		t.Fatalf("heuristic provenance=%+v err=%v", slice, err)
	}
	again, err := c.EpisodicBackfill(1, "andrew")
	if err != nil || !again.Complete || again.Processed != 0 || again.Linked != 0 || again.Indexed != 0 {
		t.Fatalf("completed rerun=%+v err=%v", again, err)
	}
}
