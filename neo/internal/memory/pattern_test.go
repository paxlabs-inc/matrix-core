// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"reflect"
	"strings"
	"testing"
)

func TestPatternSpecEncodeDecodeRoundTrip(t *testing.T) {
	spec := PatternSpec{
		Name:            "deploy ERC-20 on Paxeer",
		Trigger:         "user asks to launch a token",
		Preconditions:   []string{"FIREWORKS_API_KEY set", "agent wallet funded"},
		Steps:           []string{"tachyon_compile", "tachyon_test", "tachyon_deploy"},
		Gotchas:         []string{"pre-Cancun chain -> pin evm_version=shanghai"},
		SuccessCriteria: []string{"receipt status=1", "balanceOf>0"},
	}
	enc := spec.Encode()
	if !strings.HasPrefix(enc, patternEncPrefix) {
		t.Fatalf("encoded form should carry the version prefix: %q", enc)
	}
	got := DecodePatternSpec(enc)
	if !reflect.DeepEqual(got, spec) {
		t.Errorf("round-trip mismatch:\n got %+v\nwant %+v", got, spec)
	}
}

func TestDecodeLegacyPlainStatement(t *testing.T) {
	got := DecodePatternSpec("just a freeform recipe")
	if len(got.Steps) != 1 || got.Steps[0] != "just a freeform recipe" {
		t.Errorf("legacy plain statement should map to a single step: %+v", got)
	}
	if DecodePatternSpec("").IsEmpty() != true {
		t.Error("empty statement should decode to an empty spec")
	}
}

func TestDedupKeyPrecedence(t *testing.T) {
	if k := (PatternSpec{Name: "Deploy  ERC20", Trigger: "t", Steps: []string{"s"}}).dedupKey(); k != "deploy erc20" {
		t.Errorf("name should win and normalize: %q", k)
	}
	if k := (PatternSpec{Trigger: "When Launching", Steps: []string{"s"}}).dedupKey(); k != "when launching" {
		t.Errorf("trigger should be the fallback: %q", k)
	}
	if k := (PatternSpec{Steps: []string{"Compile", "Deploy"}}).dedupKey(); k != "compile deploy" {
		t.Errorf("steps should be the last resort: %q", k)
	}
	if !(PatternSpec{}).IsEmpty() {
		t.Error("zero spec must be empty")
	}
}

func TestPatternRender(t *testing.T) {
	p := Pattern{
		Spec: PatternSpec{
			Name:            "deploy ERC-20",
			Trigger:         "launch a token",
			Steps:           []string{"compile", "deploy"},
			Gotchas:         []string{"MCOPY revert on pre-Cancun"},
			SuccessCriteria: []string{"balanceOf>0"},
		},
		Coverage: 6,
	}
	got := p.Render()
	for _, want := range []string{"deploy ERC-20", "when: launch a token", "compile → deploy", "MCOPY revert", "balanceOf>0", "6× proven"} {
		if !strings.Contains(got, want) {
			t.Errorf("render missing %q:\n%s", want, got)
		}
	}
}
