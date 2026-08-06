// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package converge

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestControllerPersistsFingerprintCountsAndAppliesPolicyTable(t *testing.T) {
	state := State{}
	controller := Controller{}
	limits := ForPosture(Exploration)
	base := Fingerprint{Operation: "read_text_file", NormalizedArguments: json.RawMessage(`{"path":"missing"}`), Phase: "tool", FailureLayer: "tool", NormalizedCause: "not found", EffectStatus: "completed"}
	first := controller.Observe(&state, Failure{Kind: FailureTransient, Fingerprint: base}, limits)
	second := controller.Observe(&state, Failure{Kind: FailureTransient, Fingerprint: base}, limits)
	third := controller.Observe(&state, Failure{Kind: FailureTransient, Fingerprint: base}, limits)
	if !first.Retry || !second.Retry || third.Retry || third.Action != ActionDegrade || third.Count != 3 {
		t.Fatalf("transient policy first=%+v second=%+v third=%+v", first, second, third)
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	var resumed State
	if err := json.Unmarshal(encoded, &resumed); err != nil {
		t.Fatal(err)
	}
	fourth := controller.Observe(&resumed, Failure{Kind: FailureTransient, Fingerprint: base}, limits)
	if fourth.Count != 4 || fourth.Retry {
		t.Fatalf("respawn reset persisted controller: %+v", fourth)
	}

	cases := []struct {
		kind FailureKind
		want Action
	}{
		{FailureDeterministic, ActionStrategyChange},
		{FailureUnknownEffect, ActionReconcile},
		{FailureRepeatedSemantic, ActionStrategyChange},
		{FailureProviderCorruption, ActionRetryProvider},
		{FailureProcessUnusable, ActionResumeTurn},
		{FailureDelivery, ActionRetryDelivery},
	}
	for index, test := range cases {
		fingerprint := base
		fingerprint.NormalizedCause = string(test.kind)
		decision := controller.Observe(&resumed, Failure{Kind: test.kind, Fingerprint: fingerprint}, limits)
		if decision.Action != test.want {
			t.Fatalf("case %d %s action=%s want=%s", index, test.kind, decision.Action, test.want)
		}
	}
}

func TestCeilingReportUsesExactTypedAttemptCounts(t *testing.T) {
	report := CeilingReport(AttemptFacts{
		GenerationAttempts: 1, ToolAttempts: 1, ResultsPreserved: 1,
		UnresolvedIssue: "provider response was corrupt",
		RetryClass:      FailureProviderCorruption, NextRecovery: ActionRetryProvider,
	})
	if !strings.Contains(report, "after 1 generation and 1 tool attempt") ||
		strings.Contains(strings.ToLower(report), "several") {
		t.Fatalf("inaccurate ceiling report: %q", report)
	}
}
