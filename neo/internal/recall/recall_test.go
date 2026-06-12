// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package recall

import (
	"context"
	"testing"

	"matrix/cortex/embed"
	"matrix/neo/internal/conversation"
)

// The HashEmbedder is deterministic and L2-normalized: identical text yields an
// identical unit vector (cosine 1.0), so an exact-match query is guaranteed to
// rank its source turn first. That is the property these tests pin.

func TestRelevantRanksExactMatchFirst(t *testing.T) {
	conv := conversation.Open(t.TempDir())
	conv.AppendUser("c", "how do I deploy an ERC-20 on Paxeer")
	conv.AppendAssistant("c", "", "call paxeer-net deploy_token with the supply and symbol")
	conv.AppendUser("c", "what is the current gas price")
	conv.AppendAssistant("c", "", "gas is around 1 gwei right now")

	r := New(conv, "c", embed.NewHashEmbedder(), 2, 2500)
	hits := r.Relevant(context.Background(), "how do I deploy an ERC-20 on Paxeer")
	if len(hits) == 0 {
		t.Fatal("expected at least one hit")
	}
	if hits[0].Text != "how do I deploy an ERC-20 on Paxeer" {
		t.Fatalf("exact match should rank first, got %q", hits[0].Text)
	}
	if len(hits) > 2 {
		t.Fatalf("topK=2 should bound results, got %d", len(hits))
	}
}

func TestRelevantIncremental(t *testing.T) {
	conv := conversation.Open(t.TempDir())
	conv.AppendUser("c", "first turn")
	r := New(conv, "c", embed.NewHashEmbedder(), 5, 2500)

	if got := r.Relevant(context.Background(), "first turn"); len(got) != 1 {
		t.Fatalf("expected 1 cached turn, got %d", len(got))
	}
	// A turn appended after the first recall must be picked up on the next call.
	conv.AppendUser("c", "second turn arrives later")
	hits := r.Relevant(context.Background(), "second turn arrives later")
	if len(hits) == 0 || hits[0].Text != "second turn arrives later" {
		t.Fatalf("incremental refresh should surface the new turn, got %+v", hits)
	}
}

func TestDisabledRecallers(t *testing.T) {
	conv := conversation.Open(t.TempDir())
	conv.AppendUser("c", "anything")

	// Nil embedder → no-op.
	if hits := New(conv, "c", nil, 5, 2500).Relevant(context.Background(), "anything"); hits != nil {
		t.Error("nil embedder should yield nil hits")
	}
	// Disabled store (empty dir) → no-op.
	if hits := New(conversation.Open(""), "c", embed.NewHashEmbedder(), 5, 2500).Relevant(context.Background(), "anything"); hits != nil {
		t.Error("disabled store should yield nil hits")
	}
	// Empty query → no-op.
	if hits := New(conv, "c", embed.NewHashEmbedder(), 5, 2500).Relevant(context.Background(), "   "); hits != nil {
		t.Error("empty query should yield nil hits")
	}
}

func TestBudgetBound(t *testing.T) {
	conv := conversation.Open(t.TempDir())
	long := ""
	for i := 0; i < 400; i++ {
		long += "word "
	}
	conv.AppendUser("c", long+"alpha")
	conv.AppendUser("c", long+"beta")
	conv.AppendUser("c", long+"gamma")

	// A tiny budget admits the single best hit and stops (always returns >=1).
	r := New(conv, "c", embed.NewHashEmbedder(), 5, 50)
	hits := r.Relevant(context.Background(), long+"alpha")
	if len(hits) != 1 {
		t.Fatalf("tight budget should admit exactly one hit, got %d", len(hits))
	}
}
