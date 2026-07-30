// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"

	cmem "matrix/cortex/memory"
	"matrix/cortex/query"
)

func TestTypedUpsertsKeepOneCurrentMemoryPerCanonicalIdentity(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	if _, err := p.UpsertUserFact(ctx, "name", "The user's name is Andrew"); err != nil {
		t.Fatalf("UpsertUserFact initial: %v", err)
	}
	if _, err := p.UpsertUserFact(ctx, "Name", "The user's name is Andy"); err != nil {
		t.Fatalf("UpsertUserFact replacement: %v", err)
	}
	if got := countType(t, p, cmem.TypeFact); got != 1 {
		t.Fatalf("current facts after replacement = %d, want 1", got)
	}
	profile := p.UserProfile(ctx)
	if len(profile) != 1 || profile[0] != "The user's name is Andy" {
		t.Fatalf("profile after replacement = %v", profile)
	}

	const writers = 24
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, writeErr := p.UpsertUserFact(ctx, "name", fmt.Sprintf("Name revision %d", i))
			errs <- writeErr
		}(i)
	}
	wg.Wait()
	close(errs)
	for writeErr := range errs {
		if writeErr != nil {
			t.Fatalf("concurrent UpsertUserFact: %v", writeErr)
		}
	}
	if got := countType(t, p, cmem.TypeFact); got != 1 {
		t.Fatalf("current facts after concurrent replacements = %d, want 1", got)
	}

	if _, err := p.UpsertOutcome(ctx, "first summary", cmem.OutcomeFailure, "run-42"); err != nil {
		t.Fatalf("UpsertOutcome initial: %v", err)
	}
	if _, err := p.UpsertOutcome(ctx, "terminal summary", cmem.OutcomeSuccess, "run-42"); err != nil {
		t.Fatalf("UpsertOutcome replacement: %v", err)
	}
	if got := countType(t, p, cmem.TypeEvent); got != 1 {
		t.Fatalf("current outcomes for one intent = %d, want 1", got)
	}
}

func TestPatternIdentityIgnoresModelGeneratedName(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	first := PatternSpec{Name: "Deploy safely", Trigger: "release approved", Steps: []string{"compile", "verify", "deploy"}}
	second := PatternSpec{Name: "Production release recipe", Trigger: "release approved", Steps: []string{"compile", "verify", "deploy"}}
	if _, err := p.ReinforcePattern(ctx, first, []string{"run-1"}); err != nil {
		t.Fatalf("ReinforcePattern first: %v", err)
	}
	if _, err := p.ReinforcePattern(ctx, second, []string{"run-2"}); err != nil {
		t.Fatalf("ReinforcePattern second: %v", err)
	}

	res, err := p.cortex.Find(query.Query{Type: []cmem.Type{cmem.TypePattern}, Limit: 16})
	if err != nil {
		t.Fatalf("Find patterns: %v", err)
	}
	if res == nil || len(res.Memories) != 1 {
		t.Fatalf("current patterns = %#v, want exactly one", res)
	}
	data, err := cmem.DecodeData(res.Memories[0].Version.Type, res.Memories[0].Version.Data)
	if err != nil {
		t.Fatalf("DecodeData: %v", err)
	}
	pattern, ok := data.(cmem.PatternData)
	if !ok {
		t.Fatalf("decoded pattern type = %T", data)
	}
	if pattern.Coverage != 2 {
		t.Fatalf("pattern coverage = %d, want 2", pattern.Coverage)
	}
	if len(pattern.DerivedFrom) != 2 {
		t.Fatalf("pattern provenance = %v, want two intents", pattern.DerivedFrom)
	}
}

func TestUserProfileDefensivelyCollapsesLegacyDuplicateFacts(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	for i := 0; i < 9; i++ {
		if _, err := p.cortex.Write(p.head(7), cmem.FactData{
			SchemaVersion: 1,
			Subject:       userFactSubject,
			Predicate:     userFactPredicate,
			Statement:     "The user's name is Andrew",
		}, p.writeMeta()); err != nil {
			t.Fatalf("write legacy duplicate %d: %v", i, err)
		}
	}
	if got := countType(t, p, cmem.TypeFact); got != 9 {
		t.Fatalf("test setup current facts = %d, want 9", got)
	}
	profile := p.UserProfile(ctx)
	if len(profile) != 1 || profile[0] != "The user's name is Andrew" {
		t.Fatalf("deduped legacy profile = %v", profile)
	}

	snippets, err := p.Retrieve(ctx, "")
	if err != nil {
		t.Fatalf("Retrieve: %v", err)
	}
	nameFacts := 0
	for _, snippet := range snippets {
		if strings.Contains(snippet.Text, "The user's name is Andrew") {
			nameFacts++
		}
	}
	if nameFacts != 1 {
		t.Fatalf("ambient retrieval surfaced %d copies of one canonical fact, want 1", nameFacts)
	}
	if _, err := p.UpsertUserFact(ctx, "name", "The user's name is Andrew"); err != nil {
		t.Fatalf("canonical repair upsert: %v", err)
	}
	if got := countType(t, p, cmem.TypeFact); got != 1 {
		res, _ := p.cortex.Find(query.Query{Type: []cmem.Type{cmem.TypeFact}, Limit: 32})
		if res != nil {
			for _, current := range res.Memories {
				data, _ := cmem.DecodeData(current.Version.Type, current.Version.Data)
				t.Logf("current fact: %#v", data)
			}
		}
		t.Fatalf("canonical repair left %d current legacy duplicates, want 1", got)
	}
}
