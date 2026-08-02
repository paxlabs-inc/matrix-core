// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"strings"
	"testing"
	"time"

	cmemory "matrix/cortex/memory"
)

func TestMutateTwoFactCorrectionCurrentHistoricalAndDelete(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	numberURI, err := p.RememberUserFact(ctx, "The user's favorite number is 7.")
	if err != nil {
		t.Fatalf("write number: %v", err)
	}
	codeURI, err := p.RememberUserFact(ctx, "The user's project codename is Helios.")
	if err != nil {
		t.Fatalf("write codename: %v", err)
	}
	old, err := p.cortex.Resolve(cmemory.URI(numberURI))
	if err != nil {
		t.Fatalf("resolve old: %v", err)
	}
	oldCode, err := p.cortex.Resolve(cmemory.URI(codeURI))
	if err != nil {
		t.Fatalf("resolve old codename: %v", err)
	}
	asOf := old.Version.CreatedAt
	if oldCode.Version.CreatedAt.After(asOf) {
		asOf = oldCode.Version.CreatedAt
	}
	if wait := time.Until(asOf.Add(time.Second)); wait > 0 {
		time.Sleep(wait)
	}

	result, err := p.Mutate(ctx, MutationRequest{Items: []MutationItem{
		{Operation: MutationSupersede, Target: &MutationTarget{URI: numberURI}, Value: &MutationValue{Content: "The user's favorite number is 11."}},
		{Operation: MutationSupersede, Target: &MutationTarget{URI: codeURI}, Value: &MutationValue{Content: "The user's project codename is Apollo."}},
	}})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}
	if len(result.Results) != 2 {
		t.Fatalf("results = %d, want 2", len(result.Results))
	}
	for _, item := range result.Results {
		if item.URI != "" || strings.Contains(item.Description, "matrix://cortex/") {
			t.Fatalf("default confirmation exposed internal URI: %+v", item)
		}
	}

	current, err := p.RecallHits(ctx, "", []string{"fact"}, 20, nil)
	if err != nil {
		t.Fatalf("current recall: %v", err)
	}
	if _, ok := hitWith(current, "11"); !ok {
		t.Fatalf("current recall missing replacement: %+v", current)
	}
	if _, ok := hitWith(current, "Apollo"); !ok {
		t.Fatalf("current recall missing replacement: %+v", current)
	}
	if _, ok := hitWith(current, "number is 7"); ok {
		t.Fatalf("current recall retained stale number: %+v", current)
	}
	if _, ok := hitWith(current, "Helios"); ok {
		t.Fatalf("current recall retained stale codename: %+v", current)
	}
	history, err := p.RecallHits(ctx, "", []string{"fact"}, 20, &asOf)
	if err != nil {
		t.Fatalf("historical recall: %v", err)
	}
	if _, ok := hitWith(history, "number is 7"); !ok {
		t.Fatalf("historical recall missing old number: %+v", history)
	}
	if _, ok := hitWith(history, "Helios"); !ok {
		t.Fatalf("historical recall missing old codename: %+v", history)
	}

	newNumber, ok := hitWith(current, "11")
	if !ok {
		t.Fatal("replacement number URI unavailable")
	}
	if _, err := p.Mutate(ctx, MutationRequest{Items: []MutationItem{{
		Operation: MutationDelete,
		Target:    &MutationTarget{URI: newNumber.URI},
	}}}); err != nil {
		t.Fatalf("delete: %v", err)
	}
	current, err = p.RecallHits(ctx, "", []string{"fact"}, 20, nil)
	if err != nil {
		t.Fatalf("recall after delete: %v", err)
	}
	if _, ok := hitWith(current, "11"); ok {
		t.Fatalf("deleted fact remained current: %+v", current)
	}
}

func TestMutateSemanticTargetIsBoundedAndExact(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	const oldText = "The user's launch city is Berlin."
	if _, err := p.RememberUserFact(ctx, oldText); err != nil {
		t.Fatalf("write: %v", err)
	}
	drain(t, p)
	_, err = p.Mutate(ctx, MutationRequest{Items: []MutationItem{{
		Operation: MutationUpdate,
		Target:    &MutationTarget{Query: oldText, Types: []string{"fact"}},
		Value:     &MutationValue{Content: "The user's launch city is Lisbon."},
	}}})
	if err != nil {
		t.Fatalf("semantic update: %v", err)
	}
	hits, err := p.RecallHits(ctx, "", []string{"fact"}, 20, nil)
	if err != nil {
		t.Fatalf("recall: %v", err)
	}
	if _, ok := hitWith(hits, "Lisbon"); !ok {
		t.Fatalf("semantic target was not updated: %+v", hits)
	}
}

func TestExactCurrentMutationTargetRecoversFromSemanticIndexMiss(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()
	const text = "LayerX certified benchmark records instant finality and zero block time."
	if _, err := p.RememberUserFact(ctx, text); err != nil {
		t.Fatalf("write: %v", err)
	}
	uri, mem, found, err := p.resolveExactCurrentMutationTarget(text, []string{"fact"})
	if err != nil {
		t.Fatalf("exact fallback: %v", err)
	}
	if !found || uri == "" || mem == nil || !strings.Contains(primaryMutationText(mem), "zero block time") {
		t.Fatalf("exact current target not recovered: found=%v uri=%q mem=%+v", found, uri, mem)
	}
}
