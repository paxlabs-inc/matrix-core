// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"path"
)

// QualificationCase is one representative complete-request workflow.
// Per O1 req.12 ac.1: covers code, large-file, framework, browser, media,
// sourced-web, memory, scheduling, long-running, cancellation, and guarded effects.
type QualificationCase struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Category         string   `json:"category"`
	InitialPrompt    string   `json:"initial_prompt"`
	RequiredTools    []string `json:"required_tools"`
	ExpectedCriteria []string `json:"expected_criteria"`
	ProofManifest    string   `json:"proof_manifest,omitempty"`
	Passed           bool     `json:"passed"`
	Evidence         string   `json:"evidence"`
}

// FailureReplayCase is one genuine failure to replay through prevention
// and bounded recovery. Per O1 req.12 ac.3.
type FailureReplayCase struct {
	ID               string           `json:"id"`
	Name             string           `json:"name"`
	FailureClass     FailureLayer     `json:"failure_class"`
	Source           string           `json:"source"`
	ExpectedBehavior string           `json:"expected_behavior"` // "prevented" or "typed_recovery"
	Outcome          OperationOutcome `json:"outcome"`
	Passed           bool             `json:"passed"`
}

// QualificationResult is the aggregate result of running the qualification corpus.
type QualificationResult struct {
	Total     int    `json:"total"`
	Passed    int    `json:"passed"`
	Failed    int    `json:"failed"`
	Skipped   int    `json:"skipped"`
	AllPassed bool   `json:"all_passed"`
	Hash      string `json:"hash"`
}

// StandardQualificationCorpus returns the representative qualification cases
// that the O1 release gate requires.
func StandardQualificationCorpus() []QualificationCase {
	return []QualificationCase{
		{ID: "qual-code-01", Category: "code", Name: "code modification with verification",
			InitialPrompt: "Create a Go function that validates email addresses and write a test that proves it handles edge cases.",
			RequiredTools: []string{"build_project"}},
		{ID: "qual-largefile-01", Category: "large_file", Name: "large file creation with bounded chunks",
			InitialPrompt: "Generate a 100KB CSV file with realistic test data.",
			RequiredTools: []string{"build_project"}},
		{ID: "qual-framework-01", Category: "framework", Name: "framework project generation",
			InitialPrompt: "Initialize a new React project with TypeScript and add a component.",
			RequiredTools: []string{"build_project"}},
		{ID: "qual-browser-01", Category: "browser", Name: "browser interaction and screenshot",
			InitialPrompt: "Navigate to example.com and take a screenshot of the page.",
			RequiredTools: []string{"browser__navigate", "browser__take_screenshot"}},
		{ID: "qual-media-01", Category: "media", Name: "media generation",
			InitialPrompt: "Generate a 200x200 PNG image with the text 'Hello'.",
			RequiredTools: []string{"media__generate"}},
		{ID: "qual-web-01", Category: "sourced_web", Name: "sourced web research",
			InitialPrompt: "Search for the current weather in New York and cite your source.",
			RequiredTools: []string{"web-search__search", "fetch__fetch"}},
		{ID: "qual-memory-01", Category: "memory", Name: "memory recall and mutation",
			InitialPrompt: "Remember that my favorite color is blue, then recall it.",
			RequiredTools: []string{"memory_mutate", "memory_recall"}},
		{ID: "qual-schedule-01", Category: "scheduling", Name: "scheduled task",
			InitialPrompt: "Set a reminder for tomorrow at 9am to review the PR.",
			RequiredTools: []string{"chronos__set_alarm"}},
		{ID: "qual-cancel-01", Category: "cancellation", Name: "cancellation handling",
			InitialPrompt: "Start a long-running task and demonstrate clean cancellation.",
			RequiredTools: []string{"build_project"}},
		{ID: "qual-guarded-01", Category: "guarded_effect", Name: "guarded external effect",
			InitialPrompt: "Create a file that requires destructive-operation authorization.",
			RequiredTools: []string{"build_project"}},
	}
}

// StandardFailureCorpus returns the representative failure replay cases.
func StandardFailureCorpus() []FailureReplayCase {
	return []FailureReplayCase{
		{ID: "fail-protocol-01", Name: "malformed tool call", FailureClass: LayerProtocol,
			ExpectedBehavior: "prevented", Source: "O1-HZ-PROTOCOL-001"},
		{ID: "fail-process-01", Name: "process crash", FailureClass: LayerProcess,
			ExpectedBehavior: "typed_recovery", Source: "O1-HZ-PROCESS-001"},
		{ID: "fail-network-01", Name: "network timeout", FailureClass: LayerTransport,
			ExpectedBehavior: "typed_recovery", Source: "O1-HZ-EXTERNAL-001"},
		{ID: "fail-validation-01", Name: "oversized payload", FailureClass: LayerValidation,
			ExpectedBehavior: "prevented", Source: "O1-HZ-FILE-001"},
		{ID: "fail-auth-01", Name: "missing permission", FailureClass: LayerAuthorization,
			ExpectedBehavior: "prevented", Source: "O1-HZ-GRANT-001"},
		{ID: "fail-conflict-01", Name: "stale state conflict", FailureClass: LayerConflict,
			ExpectedBehavior: "typed_recovery", Source: "O1-HZ-STATE-001"},
		{ID: "fail-cancel-01", Name: "cancellation during execution", FailureClass: LayerCancellation,
			ExpectedBehavior: "typed_recovery", Source: "O1-HZ-PROCESS-001"},
		{ID: "fail-http-01", Name: "HTTP 500 from external service", FailureClass: LayerHTTP,
			ExpectedBehavior: "typed_recovery", Source: "O1-HZ-EXTERNAL-001"},
	}
}

// EvaluateQualification derives the aggregate solely from durable proof
// manifests. Caller-supplied Passed booleans are ignored so a catalog entry
// cannot manufacture release evidence.
func EvaluateQualification(cases []QualificationCase) QualificationResult {
	result := QualificationResult{Total: len(cases)}
	for i := range cases {
		var proof ProofManifest
		if cases[i].ProofManifest == "" || json.Unmarshal([]byte(cases[i].ProofManifest), &proof) != nil ||
			proof.Verify() != nil || cases[i].Evidence == "" {
			result.Failed++
			continue
		}
		cases[i].Passed = true
		result.Passed++
	}
	result.AllPassed = result.Total > 0 && result.Passed == result.Total
	encoded, _ := json.Marshal(result)
	result.Hash = ContentHash(encoded)
	return result
}

// EvaluateFailureReplay derives pass/fail from a valid typed outcome and the
// expected prevention/recovery posture. Static Passed fields are ignored.
func EvaluateFailureReplay(cases []FailureReplayCase) QualificationResult {
	result := QualificationResult{Total: len(cases)}
	for i := range cases {
		outcome := cases[i].Outcome
		valid := outcome.Verify() == nil && !outcome.Success
		switch cases[i].ExpectedBehavior {
		case "prevented":
			valid = valid && !outcome.Retryable &&
				(outcome.FailureLayer == LayerValidation || outcome.FailureLayer == LayerProtocol ||
					outcome.FailureLayer == LayerAuthorization || outcome.FailureLayer == LayerInvocation)
		case "typed_recovery":
			valid = valid && len(outcome.Transitions) > 0
		default:
			valid = false
		}
		if valid {
			result.Passed++
		} else {
			result.Failed++
		}
	}
	result.AllPassed = result.Total > 0 && result.Passed == result.Total
	encoded, _ := json.Marshal(result)
	result.Hash = ContentHash(encoded)
	return result
}

// ExtensionGate checks a new or changed capability from the same concrete
// evidence used by execution. No caller-supplied compliance boolean can clear
// the gate.
type ExtensionGate struct {
	Manifest    OperationManifest  `json:"manifest"`
	Registry    *Registry          `json:"registry"`
	Preflight   PreflightResult    `json:"preflight"`
	Outcomes    []OperationOutcome `json:"outcomes"`
	Recovery    RecoveryAutomaton  `json:"recovery"`
	Conformance QualificationCase  `json:"conformance"`
}

// IsClear returns true only when every extension requirement is supported by
// valid operation-specific evidence.
func (g ExtensionGate) IsClear() bool {
	return len(g.Unmet()) == 0
}

// Unmet returns the list of requirements not yet satisfied.
func (g ExtensionGate) Unmet() []string {
	var out []string
	op := g.Manifest.Operation
	if op == "" || g.Manifest.Description == "" || g.Manifest.Effects == "" ||
		g.Manifest.Idempotency == "" || g.Manifest.Reversibility == "" {
		out = append(out, "manifest")
	}
	if !extensionHazardsCovered(g.Registry, op) {
		out = append(out, "hazard_entries")
	}
	if !g.Preflight.OK || g.Preflight.Operation != op || len(g.Preflight.Failed) > 0 {
		out = append(out, "preflight")
	}
	if !extensionOutcomesCovered(op, g.Manifest.OutcomeVariants, g.Outcomes) {
		out = append(out, "typed_outcomes")
	}
	if g.Recovery.Operation != op || ValidateRecoveryAutomaton(g.Recovery) != nil {
		out = append(out, "recovery_automata")
	}
	if g.Manifest.Verifier == "" {
		out = append(out, "verifier")
	}
	if g.Manifest.ContextPolicy == "" || !boundedInputs(g.Manifest.InputBounds) {
		out = append(out, "context_policy")
	}
	conformance := g.Conformance
	if conformance.ID == "" || conformance.Evidence == "" ||
		!containsString(conformance.RequiredTools, op) ||
		!EvaluateQualification([]QualificationCase{conformance}).AllPassed {
		out = append(out, "conformance_evidence")
	}
	return out
}

func extensionHazardsCovered(registry *Registry, operation string) bool {
	if registry == nil || operation == "" {
		return false
	}
	filtered := &Registry{Version: registry.Version, Operations: []string{operation}}
	for _, hazard := range registry.Hazards {
		matched, err := path.Match(hazard.Operation, operation)
		if err != nil {
			return false
		}
		if matched {
			filtered.Hazards = append(filtered.Hazards, hazard)
		}
	}
	return len(filtered.Hazards) > 0 && filtered.Check(nil) == nil
}

func extensionOutcomesCovered(operation string, variants []string, outcomes []OperationOutcome) bool {
	if operation == "" || len(outcomes) == 0 {
		return false
	}
	hasSuccess, hasFailure := false, false
	for _, outcome := range outcomes {
		if outcome.Operation != operation || outcome.Verify() != nil {
			return false
		}
		hasSuccess = hasSuccess || outcome.Success
		hasFailure = hasFailure || !outcome.Success
	}
	return containsString(variants, "success") && containsString(variants, "typed_failure") &&
		hasSuccess && hasFailure
}

func boundedInputs(bounds map[string]string) bool {
	if len(bounds) == 0 {
		return false
	}
	for field, bound := range bounds {
		if field == "" || bound == "" {
			return false
		}
	}
	return true
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
