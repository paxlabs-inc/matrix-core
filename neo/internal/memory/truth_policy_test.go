// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	cmemory "matrix/cortex/memory"
)

func TestCurrentTruthPolicyAcrossMutationAndRetrievalLanes(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	if _, err := p.SetMemoryConsent(ctx, true, "test user"); err != nil {
		t.Fatalf("enable consent: %v", err)
	}

	created, err := p.Mutate(ctx, MutationRequest{
		IncludeInternalIDs: true,
		Items: []MutationItem{{
			Operation: MutationCreate,
			Value:     &MutationValue{Type: "goal", Content: "The launch codename is Helios."},
		}},
	})
	if err != nil || len(created.Results) != 1 || created.Results[0].URI == "" {
		t.Fatalf("create: result=%+v err=%v", created, err)
	}
	uri := created.Results[0].URI
	if _, err := p.AppendMessage(cortex.Message{ConversationID: "truth-history", Role: cortex.RoleUser, Content: "The launch codename is Helios."}); err != nil {
		t.Fatalf("append lexical history: %v", err)
	}
	materializeTruthRollups(t, p)
	assertTruthLanes(t, p, "The launch codename is Helios.", "")

	updated, err := p.Mutate(ctx, MutationRequest{
		IncludeInternalIDs: true,
		Items: []MutationItem{{
			Operation: MutationUpdate,
			Target:    &MutationTarget{URI: uri},
			Value:     &MutationValue{Content: "The launch codename remains Helios for beta."},
		}},
	})
	if err != nil || updated.Results[0].URI == "" {
		t.Fatalf("update: result=%+v err=%v", updated, err)
	}
	uri = updated.Results[0].URI
	assertTruthLanes(t, p, "The launch codename remains Helios for beta.", "The launch codename is Helios.")

	_, id, _, err := cortex.ParseURI(cmemory.URI(uri))
	if err != nil {
		t.Fatalf("parse updated uri: %v", err)
	}
	beforeSupersede, err := p.cortex.ResolveLatest(id)
	if err != nil {
		t.Fatalf("resolve updated: %v", err)
	}
	asOf := beforeSupersede.Version.CreatedAt
	if wait := time.Until(asOf.Add(time.Second)); wait > 0 {
		time.Sleep(wait)
	}
	superseded, err := p.Mutate(ctx, MutationRequest{
		IncludeInternalIDs: true,
		Items: []MutationItem{{
			Operation: MutationSupersede,
			Target:    &MutationTarget{URI: uri},
			Value:     &MutationValue{Content: "The launch codename is Apollo."},
		}},
	})
	if err != nil || superseded.Results[0].URI == "" {
		t.Fatalf("supersede: result=%+v err=%v", superseded, err)
	}
	uri = superseded.Results[0].URI
	materializeTruthRollups(t, p)
	assertTruthLanes(t, p, "The launch codename is Apollo.", "Helios")
	history, err := p.RecallHits(ctx, "", []string{"goal"}, 20, &asOf)
	if err != nil {
		t.Fatalf("historical recall: %v", err)
	}
	if _, ok := hitWith(history, "remains Helios for beta"); !ok {
		t.Fatalf("historical recall missed prior truth: %+v", history)
	}
	recalled, err := p.Recall(ctx, "launch codename", []string{"goal"}, 20, nil)
	if err != nil {
		t.Fatalf("recall with lexical lane: %v", err)
	}
	if strings.Contains(recalled, "codename is Helios") {
		t.Fatalf("lexical lane reactivated stale truth:\n%s", recalled)
	}

	if _, err := p.Mutate(ctx, MutationRequest{Items: []MutationItem{{
		Operation: MutationDelete,
		Target:    &MutationTarget{URI: uri},
	}}}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	assertTruthLanes(t, p, "", "Apollo")
}

func TestEmptyActivationReportsRelevantMemoryOutsideMaterializedTiers(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	const fact = "The user's release train is called Northstar."
	if _, err := p.RememberUserFact(ctx, fact); err != nil {
		t.Fatalf("write fact: %v", err)
	}
	drain(t, p)
	bundle, err := p.Activate("selection-reason", fact, cortex.Budget{})
	if err != nil {
		t.Fatalf("Activate: %v", err)
	}
	if bundle.SelectionReason != "relevant current memory exists outside the materialized activation tiers; use memory_recall" {
		t.Fatalf("selection reason = %q", bundle.SelectionReason)
	}
}

func materializeTruthRollups(t *testing.T, p *Pager) {
	t.Helper()
	if err := p.cortex.Cascade(cortex.TierEpoch, time.Now().UTC().Add(2*time.Hour).UnixNano()); err != nil {
		t.Fatalf("Cascade: %v", err)
	}
}

func assertTruthLanes(t *testing.T, p *Pager, want, reject string) {
	t.Helper()
	ctx := context.Background()
	texts := map[string][]string{
		"recent":     nil,
		"search":     nil,
		"recall":     nil,
		"semantic":   nil,
		"activation": nil,
	}

	timeline, _, err := p.Timeline(TimelineQuery{Types: []string{"goal"}, Limit: 20})
	if err != nil {
		t.Fatalf("timeline: %v", err)
	}
	for _, item := range timeline {
		texts["recent"] = append(texts["recent"], item.FormMedium)
	}

	search, _, err := p.Timeline(TimelineQuery{Near: "launch codename", Types: []string{"goal"}, Limit: 20})
	if err != nil {
		t.Fatalf("search: %v", err)
	}
	for _, item := range search {
		texts["search"] = append(texts["search"], item.FormMedium)
	}

	hits, err := p.RecallHits(ctx, "", []string{"goal"}, 20, nil)
	if err != nil {
		t.Fatalf("recall hits: %v", err)
	}
	for _, hit := range hits {
		texts["recall"] = append(texts["recall"], hit.Text)
	}

	snips, err := p.Retrieve(ctx, "launch codename")
	if err != nil {
		t.Fatalf("retrieve: %v", err)
	}
	for _, snip := range snips {
		if strings.EqualFold(snip.Type, "Goal") {
			texts["semantic"] = append(texts["semantic"], snip.Text)
		}
	}

	bundle, err := p.Activate("truth-lanes", "launch codename", cortex.Budget{})
	if err != nil {
		t.Fatalf("activate: %v", err)
	}
	for _, mem := range bundle.Pinned {
		if mem.Head.Type == cmemory.TypeGoal {
			texts["activation"] = append(texts["activation"], mem.Version.Forms.Medium)
		}
	}
	for _, episode := range bundle.Recent {
		_, id, _, err := cortex.ParseURI(episode.Ref.URI)
		if err != nil {
			continue
		}
		mem, err := p.cortex.ResolveLatest(id)
		if err == nil && mem.Head.Type == cmemory.TypeGoal {
			texts["activation"] = append(texts["activation"], mem.Version.Forms.Medium)
		}
	}

	for lane, values := range texts {
		joined := strings.Join(values, "\n")
		if want != "" && !strings.Contains(joined, want) {
			t.Fatalf("%s lane missed current truth %q: %q", lane, want, joined)
		}
		if reject != "" && strings.Contains(joined, reject) {
			t.Fatalf("%s lane retained obsolete truth %q: %q", lane, reject, joined)
		}
		if want == "" && strings.TrimSpace(joined) != "" {
			t.Fatalf("%s lane retained deleted truth: %q", lane, joined)
		}
	}
}
