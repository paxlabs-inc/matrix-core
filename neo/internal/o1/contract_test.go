// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"strings"
	"testing"
)

func TestCompleteRequestCompilesReadyWithProvenance(t *testing.T) {
	c := Compile(CompileInput{
		Request:         "Create a bounded file writer and prove byte integrity.",
		RepositoryRules: []string{"Do not run git commands.", "Use real code paths."},
		FrameworkRules:  []string{"Run the installed framework validator."},
	})
	if !c.ReadyForMutation() {
		t.Fatal("complete request did not compile ready")
	}
	if c.Version != 1 || c.Objective.Provenance != User || len(c.Constraints) != 3 || len(c.Criteria) != 5 {
		t.Fatalf("contract lost required structure: %#v", c)
	}
	if len(c.RequiredCapabilities) != 2 || c.RequiredCapabilities[0] != "filesystem" || c.RequiredCapabilities[1] != "execution" {
		t.Fatalf("required capability was not compiled: %#v", c.RequiredCapabilities)
	}
	if got := c.Render(); !strings.Contains(got, `"completion_predicate"`) || !strings.Contains(got, `"repository"`) {
		t.Fatalf("render omitted structured contract: %s", got)
	}
}

func TestEmptyRequestIsTheOnlyCompilerInferredAmbiguity(t *testing.T) {
	c := Compile(CompileInput{})
	if c.ReadyForMutation() {
		t.Fatal("empty request compiled ready")
	}
	if len(c.Ambiguities) != 1 || c.Ambiguities[0].Question != "A required request value is missing." {
		t.Fatalf("unexpected ambiguity: %#v", c.Ambiguities)
	}
}

func TestCodeSyntaxAndLiteralPlaceholdersDoNotBlockExecution(t *testing.T) {
	request := "Build the React view with initial={{ opacity: 0 }} and document Deploy {{service_name}} as an example."
	c := Compile(CompileInput{Request: request})
	if !c.ReadyForMutation() || len(c.Ambiguities) != 0 {
		t.Fatalf("valid free-form code request was treated as a template: %#v", c.Ambiguities)
	}
}

func TestRevisionIsMonotonicAndPreservesProof(t *testing.T) {
	c := Compile(CompileInput{Request: "Create and verify the artifact."})
	c.Criteria[0].Closed = true
	revised := c.Revise("Keep the existing artifact and add a second format.")
	if revised.Version != 2 || len(revised.Revisions) != 1 || len(revised.Revisions[0].PriorProof) != 1 {
		t.Fatalf("revision lost version or proof: %#v", revised)
	}
	if !c.Criteria[0].Closed || c.Version != 1 {
		t.Fatal("revision mutated the prior contract")
	}
}

func TestCapabilityInferenceUsesWordBoundaries(t *testing.T) {
	contract := Compile(CompileInput{Request: "Explain what happened yesterday."})
	for _, capability := range contract.RequiredCapabilities {
		if capability == "workspace_preview" {
			t.Fatal("the substring app in happened must not expose workspace preview")
		}
	}
}
