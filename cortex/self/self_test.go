// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package self

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"matrix/cortex"
	cmem "matrix/cortex/memory"
	"matrix/cortex/store"
)

// openStore opens a real cortex store in a temp dir — no fakes.
func openStore(t *testing.T) *cortex.Cortex {
	t.Helper()
	s, err := store.Open(t.TempDir(), "self-test", nil)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return cortex.New(s)
}

func writeStructural(t *testing.T, cx *cortex.Cortex, structural StructuralSelf) {
	t.Helper()
	params, err := json.Marshal(structural)
	if err != nil {
		t.Fatalf("marshal structural: %v", err)
	}
	_, err = cx.Write(
		cmem.Head{ActorScope: "self-test", DeclaredImportance: 8},
		cmem.CapabilityData{
			SchemaVersion: 1,
			Subject:       Subject,
			Capability:    StructuralCapability,
			Parameters:    params,
			Verified:      true,
			LastObserved:  time.Now().UTC(),
		},
		cortex.WriteMeta{CreatedBy: "self-test", Provenance: cmem.Provenance{Source: cmem.SourceObserved}},
	)
	if err != nil {
		t.Fatalf("write structural: %v", err)
	}
}

func writeFailure(t *testing.T, cx *cortex.Cortex, statement string, evidence []string) {
	t.Helper()
	_, err := cx.Write(
		cmem.Head{ActorScope: "self-test", DeclaredImportance: 7},
		cmem.BeliefData{
			SchemaVersion: 1,
			Statement:     statement,
			Subject:       Subject,
			Stance:        cmem.StanceBelieve,
			EvidenceFor:   evidence,
		},
		cortex.WriteMeta{CreatedBy: "self-test", Provenance: cmem.Provenance{Source: cmem.SourceObserved}},
	)
	if err != nil {
		t.Fatalf("write failure pattern: %v", err)
	}
}

// TestFacultiesResolveTheSameSelfModelArtifact proves req.6.4: two faculties
// reading the SAME cortex store through the shared resolver observe the
// byte-identical self-model — same identity, same structural summary, same
// failure patterns — not divergent per-faculty copies. Real cortex, no fakes.
func TestFacultiesResolveTheSameSelfModelArtifact(t *testing.T) {
	cx := openStore(t)

	structural := StructuralSelf{
		Summary:      "Neo assembles system, transcript, then a trailing memory tail; coding and execution are faculties; core_execute is the value-transfer wall.",
		GraphURI:     "matrix://self-graph/neo",
		Scope:        []string{"neo", "cody", "executor/cmd/mcl-execute", "cortex"},
		TokenBudget:  800,
		ContextLimit: 131072,
	}
	writeStructural(t, cx, structural)
	writeFailure(t, cx, "I loop when a trailing memory tail is treated as the live request", []string{"matrix://cortex/Event/death#1"})

	// Two faculties (e.g. the conversation faculty and the execution faculty)
	// each resolve the model from the same store with the same agent name.
	conversation, err := Resolve(cx, "Neo")
	if err != nil {
		t.Fatalf("conversation Resolve: %v", err)
	}
	execution, err := Resolve(cx, "")
	if err != nil {
		t.Fatalf("execution Resolve: %v", err)
	}

	if conversation.Identity != "Neo" {
		t.Fatalf("conversation identity = %q, want Neo", conversation.Identity)
	}
	if execution.Identity != DefaultName {
		t.Fatalf("execution identity = %q, want the default %q", execution.Identity, DefaultName)
	}
	if conversation.Structural.Summary != structural.Summary || execution.Structural.Summary != structural.Summary {
		t.Fatalf("structural summaries diverged:\n conversation=%q\n execution=%q", conversation.Structural.Summary, execution.Structural.Summary)
	}
	if conversation.Structural.ContextLimit != structural.ContextLimit {
		t.Fatalf("context limit not resolved: got %d", conversation.Structural.ContextLimit)
	}
	if len(conversation.FailurePatterns) != 1 || len(execution.FailurePatterns) != 1 {
		t.Fatalf("failure patterns diverged: conversation=%d execution=%d", len(conversation.FailurePatterns), len(execution.FailurePatterns))
	}
	if conversation.FailurePatterns[0].Statement != execution.FailurePatterns[0].Statement {
		t.Fatalf("failure pattern statements diverged:\n conversation=%q\n execution=%q", conversation.FailurePatterns[0].Statement, execution.FailurePatterns[0].Statement)
	}
	if conversation.StructuralURI == "" || conversation.StructuralURI != execution.StructuralURI {
		t.Fatalf("structural URIs diverged: %q vs %q", conversation.StructuralURI, execution.StructuralURI)
	}
	if conversation.FailurePatterns[0].URI != execution.FailurePatterns[0].URI {
		t.Fatalf("failure pattern URIs diverged: %q vs %q", conversation.FailurePatterns[0].URI, execution.FailurePatterns[0].URI)
	}
}

// TestResolveEmptyStore proves a fresh store yields the identity default and no
// structural/experiential facets, and a nil cortex degrades to identity-only —
// the resolver never errors a faculty out for lacking a populated self-model.
func TestResolveEmptyStore(t *testing.T) {
	cx := openStore(t)
	m, err := Resolve(cx, "")
	if err != nil {
		t.Fatalf("Resolve empty: %v", err)
	}
	if m.Identity != DefaultName || m.Structural.Summary != "" || len(m.FailurePatterns) != 0 {
		t.Fatalf("empty store resolved a non-empty model: %#v", m)
	}
	nilModel, err := Resolve(nil, "Neo")
	if err != nil {
		t.Fatalf("Resolve nil cortex: %v", err)
	}
	if nilModel.Identity != "Neo" {
		t.Fatalf("nil-cortex identity = %q", nilModel.Identity)
	}
}

// machineryMarkers are internal package-structure tokens that MUST NOT appear
// in the user-facing identity persona (req.7.2): package paths, codegraph node
// syntax, and self-package Go symbol names. The persona is the identity facet
// only — the structural facet (which is full of these) never leaks into it.
var machineryMarkers = []string{
	"neo/internal", "cody/internal", "executor/cmd", "codegraph",
	"loc=", "func ", "AssertNoValueTransferTools", "SubagentSchemas",
	"assembleWindow", "runSwarm", "coreExecuteSchema",
}

// TestPersonaIsFirstPersonAndMachineryFree proves req.7.2/7.3: the derived
// persona is non-empty, first-person, and free of internal package structure —
// even when the resolved model carries a rich structural summary full of
// package paths and symbols, none of it bleeds into the persona.
func TestPersonaIsFirstPersonAndMachineryFree(t *testing.T) {
	rich := SelfModel{
		Identity: "Neo",
		Structural: StructuralSelf{
			Summary: "neo/internal/agent.Chat assembles the window; runSwarm spawns SubagentSchemas; AssertNoValueTransferTools guards the wall (loc=neo/internal/tools/automatrix.go).",
		},
		FailurePatterns: []FailurePattern{{Statement: "I loop on a trailing tail"}},
	}
	persona := Persona(rich)
	if strings.TrimSpace(persona) == "" {
		t.Fatal("persona is empty")
	}
	// First person: the agent speaks as itself.
	for _, marker := range []string{"You are Matrix", "I'll", "my", "I remember"} {
		if !strings.Contains(persona, marker) {
			t.Fatalf("persona missing first-person marker %q:\n%s", marker, persona)
		}
	}
	// No internal package structure leaks in from the structural facet.
	for _, m := range machineryMarkers {
		if strings.Contains(persona, m) {
			t.Fatalf("persona leaked internal machinery marker %q:\n%s", m, persona)
		}
	}
	// The structural summary text itself must never appear in the persona.
	if strings.Contains(persona, rich.Structural.Summary) {
		t.Fatalf("persona leaked the structural summary verbatim:\n%s", persona)
	}
}
