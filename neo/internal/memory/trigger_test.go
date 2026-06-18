// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"testing"

	"matrix/cortex/forms"
	cmem "matrix/cortex/memory"
)

// renderFor reproduces the exact text cortex embeds for a typed memory
// (forms.RenderFull over a Type-only head — the same render relation.go uses
// for dedup). Driving Find(Near:) with this text makes the hash-stub embedder's
// "identical text → identical vector" property surface the matching memory
// deterministically, so trigger matching is testable offline.
func renderFor(t *testing.T, typ cmem.Type, data cmem.TypedData) string {
	t.Helper()
	return forms.RenderFull(&cmem.Head{Type: typ}, data)
}

// A learned (non-hard) constraint whose embedded content matches the live turn
// must surface through the trigger lane — independent of global salience — while
// an unrelated constraint stays out. This is the structural fix for "Neo forgets
// a learned behavior".
func TestTriggeredGuidanceSurfacesMatchedConstraint(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const fit = "Render a Construct surface when producing a deliverable"
	const other = "Use the read-only chain tools before answering chain questions"
	if _, err := p.RememberConstraint(ctx, fit, "do", "firm"); err != nil {
		t.Fatalf("RememberConstraint fit: %v", err)
	}
	if _, err := p.RememberConstraint(ctx, other, "do", "firm"); err != nil {
		t.Fatalf("RememberConstraint other: %v", err)
	}
	drain(t, p)

	// Drive the trigger lane with the exact render of the matching constraint
	// so the hash embedder returns it as the nearest neighbor.
	turn := renderFor(t, cmem.TypeConstraint, cmem.ConstraintData{
		SchemaVersion: 1,
		Statement:     fit,
		Polarity:      cmem.PolarityDo,
		StrengthVal:   cmem.StrengthFirm,
		Source:        cmem.ConstraintSourceLearned,
	})

	got := p.TriggeredGuidance(ctx, turn)
	if _, ok := snippetWith(got, "producing a deliverable"); !ok {
		t.Errorf("matched constraint should surface via trigger lane; got %+v", got)
	}
	if _, ok := snippetWith(got, "read-only chain tools"); ok {
		t.Errorf("unrelated constraint must not surface; got %+v", got)
	}
}

// The trigger lane must never re-surface a HARD constraint (already pinned
// verbatim) even when it is the nearest match.
func TestTriggeredGuidanceSkipsHardConstraint(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	const hard = "Never wipe prod chain state"
	if _, err := p.cortex.Write(p.head(9), cmem.ConstraintData{
		SchemaVersion: 1,
		Statement:     hard,
		Polarity:      cmem.PolarityDont,
		StrengthVal:   cmem.StrengthHard,
		Source:        cmem.ConstraintSourceUserDeclared,
	}, p.writeMeta()); err != nil {
		t.Fatalf("write hard constraint: %v", err)
	}
	drain(t, p)

	turn := renderFor(t, cmem.TypeConstraint, cmem.ConstraintData{
		SchemaVersion: 1,
		Statement:     hard,
		Polarity:      cmem.PolarityDont,
		StrengthVal:   cmem.StrengthHard,
		Source:        cmem.ConstraintSourceUserDeclared,
	})

	if got := p.TriggeredGuidance(ctx, turn); len(got) != 0 {
		t.Errorf("hard constraint must not surface via trigger lane (already pinned); got %+v", got)
	}
}

// A Pattern fires through the trigger lane only when it carries an explicit
// trigger; a trigger-less pattern is left to the (coverage-gated) procedural
// lane. The trigger-bearing pattern surfaces regardless of coverage.
func TestTriggeredGuidancePatternRequiresTrigger(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	withTrigger := PatternSpec{
		Name:    "show your work",
		Trigger: "rendering a structured result on screen",
		Steps:   []string{"call construct_render", "narrate briefly"},
	}
	noTrigger := PatternSpec{Name: "internal recipe", Steps: []string{"do x", "do y"}}
	// Coverage 0 for both: below the procedural gate, proving the trigger lane
	// is independent of coverage.
	if _, err := p.WritePattern(ctx, withTrigger, 0.5, 0, nil); err != nil {
		t.Fatalf("WritePattern withTrigger: %v", err)
	}
	if _, err := p.WritePattern(ctx, noTrigger, 0.5, 0, nil); err != nil {
		t.Fatalf("WritePattern noTrigger: %v", err)
	}
	drain(t, p)

	hitTurn := renderFor(t, cmem.TypePattern, cmem.PatternData{
		SchemaVersion: 1, Statement: withTrigger.Encode(), Strength: 0.5, Coverage: 0,
	})
	if got := p.TriggeredGuidance(ctx, hitTurn); func() bool {
		_, ok := snippetWith(got, "show your work")
		return !ok
	}() {
		t.Errorf("trigger-bearing pattern should surface regardless of coverage; got %+v", p.TriggeredGuidance(ctx, hitTurn))
	}

	missTurn := renderFor(t, cmem.TypePattern, cmem.PatternData{
		SchemaVersion: 1, Statement: noTrigger.Encode(), Strength: 0.5, Coverage: 0,
	})
	if _, ok := snippetWith(p.TriggeredGuidance(ctx, missTurn), "internal recipe"); ok {
		t.Errorf("trigger-less pattern must not surface via trigger lane")
	}
}
