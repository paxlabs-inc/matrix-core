// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"reflect"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	"matrix/cortex/keys"
)

func TestEpisodicRetrieveExpandsCrossConversationAndBounds(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ctx := context.Background()
	conv := "conv-zephyr"
	var lo, hi uint64
	for i, msg := range []cortex.Message{
		{ConversationID: conv, Role: cortex.RoleUser, Content: "The Zephyr deploy failed on the canary."},
		{ConversationID: conv, Role: cortex.RoleAssistant, Content: "We fixed Zephyr by rotating the Buildkite token."},
	} {
		uri, aerr := p.AppendMessage(msg)
		if aerr != nil {
			t.Fatal(aerr)
		}
		_, seq, ok := cortex.ParseSessionURI(uri)
		if !ok {
			t.Fatalf("bad session URI %q", uri)
		}
		if i == 0 {
			lo = seq
		}
		hi = seq
	}
	memURI, err := p.RememberFact(ctx, "Zephyr deploy uses Buildkite")
	if err != nil {
		t.Fatal(err)
	}
	if err := p.LinkSessionProvenance(memURI, conv, lo, hi); err != nil {
		t.Fatal(err)
	}
	if err := p.cortex.DrainEmbedder(ctx); err != nil {
		t.Fatal(err)
	}

	hits := p.EpisodicRetrieve(ctx, "Zephyr Buildkite", EpisodicTimeWindow{}, EpisodicBudget{Tokens: 200, Hits: 1}, nil)
	if len(hits) != 1 {
		t.Fatalf("hits=%d want 1", len(hits))
	}
	if hits[0].ConversationID != conv || !hits[0].Exact || !strings.Contains(hits[0].Text, "rotating the Buildkite token") {
		t.Fatalf("unexpected excerpt: %+v", hits[0])
	}
	if len(hits[0].RelatedMemories) != 1 || hits[0].RelatedMemories[0].URI != memURI {
		t.Fatalf("missing related memory: %+v", hits[0].RelatedMemories)
	}

	future := time.Now().UTC().AddDate(1, 0, 0)
	filtered := p.EpisodicRetrieve(ctx, "Zephyr", EpisodicTimeWindow{From: future, Until: future.Add(time.Hour)}, EpisodicBudget{}, nil)
	if len(filtered) != 0 {
		t.Fatalf("time-filtered hits=%d want 0", len(filtered))
	}
}

func TestEpisodicRetrieveCurrentLaneAndDeadlineFailOpen(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	hits := p.EpisodicRetrieve(context.Background(), "old phrase", EpisodicTimeWindow{}, EpisodicBudget{Tokens: 20, Hits: 1}, []EpisodicCurrentHit{{Role: "user", Text: "old phrase from this conversation"}})
	if len(hits) != 1 || hits[0].ConversationID != "current" {
		t.Fatalf("current lane = %+v", hits)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if got := p.EpisodicRetrieve(canceled, "old phrase", EpisodicTimeWindow{}, EpisodicBudget{}, nil); len(got) != 0 {
		t.Fatalf("canceled retrieval returned %d hits", len(got))
	}
}

func TestEpisodicRetrieveBrokenDerivedLanesFailOpen(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.AppendMessage(cortex.Message{ConversationID: "corrupt", Role: cortex.RoleUser, Content: "corrupt-lane-only needle"}); err != nil {
		t.Fatal(err)
	}
	if err := p.cortex.StopEmbedder(); err != nil {
		t.Fatal(err)
	}
	badKey := append(append([]byte(nil), keys.PrefixLexicalDoc...), []byte("broken")...)
	if err := p.cortex.Store().ReplaceDerivedPrefix(keys.PrefixLexical, map[string][]byte{string(badKey): []byte("corrupt")}); err != nil {
		t.Fatal(err)
	}
	if got := p.EpisodicRetrieve(context.Background(), "corrupt-lane-only", EpisodicTimeWindow{}, EpisodicBudget{}, nil); len(got) != 0 {
		t.Fatalf("broken derived lanes returned %+v", got)
	}
}

func TestEpisodicLexicalLaneFeedsAutomaticAndReactiveRecall(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if _, err := p.AppendMessage(cortex.Message{ConversationID: "lex-only", Role: cortex.RoleUser, Content: "the exact error was QZK-9471 frobnicator collapse"}); err != nil {
		t.Fatal(err)
	}
	first := p.EpisodicRetrieve(context.Background(), "QZK-9471", EpisodicTimeWindow{}, EpisodicBudget{Hits: 4, Tokens: 100}, nil)
	second := p.EpisodicRetrieve(context.Background(), "QZK-9471", EpisodicTimeWindow{}, EpisodicBudget{Hits: 4, Tokens: 100}, nil)
	if len(first) == 0 || first[0].ConversationID != "lex-only" || !strings.Contains(first[0].Text, "QZK-9471") {
		t.Fatalf("automatic lexical lane = %+v", first)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("fusion ordering drifted:\nfirst=%+v\nsecond=%+v", first, second)
	}
	recalled, err := p.Recall(context.Background(), "QZK-9471", nil, 4, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(recalled, "Verbatim transcript matches") || !strings.Contains(recalled, "QZK-9471") {
		t.Fatalf("reactive recall missed lexical transcript:\n%s", recalled)
	}
}
