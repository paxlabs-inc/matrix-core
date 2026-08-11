// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package o1

import (
	"encoding/json"
	"testing"
)

func TestOutcomeBuilderSuccess(t *testing.T) {
	o := NewOutcome("fs__read_file").Success().Evidence("content returned").MustBuild()
	if !o.Success {
		t.Fatal("expected success")
	}
	if o.FailureLayer != "" {
		t.Fatal("success should have no failure layer")
	}
}

func TestOutcomeBuilderFailureRequiresLayer(t *testing.T) {
	_, err := NewOutcome("exec__shell").Fail(LayerProcess, "crash").
		Retryable(true).
		Evidence("exit code 137").
		AllowTransition(TransitionRetry).
		Build()
	if err != nil {
		t.Fatalf("valid failed outcome should build: %v", err)
	}
}

func TestOutcomeBuilderFailsValidation(t *testing.T) {
	// Failed outcome without layer
	_, err := NewOutcome("test").Fail("", "").Build()
	if err == nil {
		t.Fatal("failed outcome without layer should fail validation")
	}

	// Retryable outcome without transitions
	_, err = NewOutcome("test").Fail(LayerTransport, "timeout").Retryable(true).Build()
	if err == nil {
		t.Fatal("retryable outcome without transitions should fail validation")
	}
}

func TestOutcomeAllowsTransition(t *testing.T) {
	o := NewOutcome("fetch__fetch").Fail(LayerTransport, "timeout").
		Retryable(true).Evidence("deadline exceeded").
		AllowTransition(TransitionRetry).AllowTransition(TransitionStop).MustBuild()
	if !o.AllowsTransition(TransitionRetry) {
		t.Fatal("should allow retry")
	}
	if !o.AllowsTransition(TransitionStop) {
		t.Fatal("should allow stop")
	}
	if o.AllowsTransition(TransitionRepair) {
		t.Fatal("should not allow repair")
	}
}

func TestOutcomeIsTerminal(t *testing.T) {
	success := NewOutcome("fs__read_file").Success().Evidence("ok").MustBuild()
	if !success.IsTerminal() {
		t.Fatal("success with no transitions is terminal")
	}
	blocked := NewOutcome("deus__pay").Fail(LayerPolicy, "no_leash").Evidence("refused").MustBuild()
	if !blocked.IsTerminal() {
		t.Fatal("policy failure with no transitions is terminal")
	}
	retriable := NewOutcome("fetch__fetch").Fail(LayerTransport, "timeout").
		Retryable(true).Evidence("deadline exceeded").AllowTransition(TransitionRetry).MustBuild()
	if retriable.IsTerminal() {
		t.Fatal("retriable outcome is not terminal")
	}
}

func TestOutcomeVerifyInvalidLayer(t *testing.T) {
	o := OperationOutcome{Operation: "test", Success: false, FailureLayer: "bogus"}
	if err := o.Verify(); err == nil {
		t.Fatal("invalid failure layer should fail verify")
	}
}

func TestOutcomeRenderProducesJSON(t *testing.T) {
	o := NewOutcome("exec__shell").Fail(LayerProcess, "nonzero_exit").
		Retryable(true).Evidence("exit code 1").AllowTransition(TransitionRetry).MustBuild()
	rendered := RenderOutcome(o)
	var parsed map[string]interface{}
	if err := json.Unmarshal([]byte(rendered), &parsed); err != nil {
		t.Fatalf("render should produce valid JSON: %v", err)
	}
	if parsed["operation"] != "exec__shell" {
		t.Fatal("rendered outcome should carry operation name")
	}
}

func TestOutcomeAllFailureLayers(t *testing.T) {
	layers := []FailureLayer{
		LayerTransport, LayerProcess, LayerProtocol, LayerHTTP, LayerApplication,
		LayerValidation, LayerPolicy, LayerAuthorization, LayerConflict,
		LayerCancellation, LayerInvocation,
	}
	for _, layer := range layers {
		o, err := NewOutcome("test").Fail(layer, "test_class").
			Retryable(false).Evidence("typed evidence").AllowTransition(TransitionStop).Build()
		if err != nil {
			t.Fatalf("layer %s should be valid: %v", layer, err)
		}
		if o.FailureLayer != layer {
			t.Fatalf("layer mismatch: got %s", o.FailureLayer)
		}
	}
}
