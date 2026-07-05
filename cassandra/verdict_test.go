// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import "testing"

// TestSoundComposesCoverageAndGrounded pins the canonical acceptance predicate
// (X1, req 6.1): Sound is exactly CoverageComplete AND IsGrounded, and
// IsGrounded is exactly grounded AND no unverified claims. Neo's verdictAccepts
// and Cody's gate.Adjudicate decide through these same methods, so this table
// is the shared truth the three consumers cannot drift from (req 6.3).
func TestSoundComposesCoverageAndGrounded(t *testing.T) {
	cases := []struct {
		name string
		v    *Verdict
	}{
		{"grounded_full", &Verdict{Grounded: true, Coverage: CoverageFull}},
		{"ungrounded_full", &Verdict{Grounded: false, Coverage: CoverageFull}},
		{"grounded_but_unverified", &Verdict{Grounded: true, Coverage: CoverageFull, UnverifiedClaims: []string{"the deploy succeeded"}}},
		{"grounded_partial", &Verdict{Grounded: true, Coverage: CoveragePartial, Missing: []string{"criterion 2 unexercised"}}},
		{"full_with_missing", &Verdict{Grounded: true, Coverage: CoverageFull, Missing: []string{"the refund was never sent"}}},
	}
	for _, tc := range cases {
		tc.v.Normalize()
		wantGrounded := tc.v.Grounded && len(tc.v.UnverifiedClaims) == 0
		if got := tc.v.IsGrounded(); got != wantGrounded {
			t.Errorf("%s: IsGrounded()=%v, want %v", tc.name, got, wantGrounded)
		}
		if got := tc.v.Sound(); got != (tc.v.CoverageComplete() && tc.v.IsGrounded()) {
			t.Errorf("%s: Sound() must equal CoverageComplete && IsGrounded, got %v", tc.name, got)
		}
	}
}

func TestNormalize_G1_FullWithMissingForcesPartial(t *testing.T) {
	v := &Verdict{Coverage: CoverageFull, Missing: []string{"deploy the contract"}}
	v.Normalize()
	if v.Coverage != CoveragePartial {
		t.Fatalf("g1: expected coverage partial, got %q", v.Coverage)
	}
	if v.CoverageComplete() {
		t.Fatal("g1: CoverageComplete must be false when items remain missing")
	}
}

func TestNormalize_G2_GroundedWithUnverifiedForcesFalse(t *testing.T) {
	v := &Verdict{Grounded: true, Coverage: CoverageFull, UnverifiedClaims: []string{"block height is 123456"}}
	v.Normalize()
	if v.Grounded {
		t.Fatal("g2: expected grounded false when unverified claims remain")
	}
	if v.Sound() {
		t.Fatal("g2: Sound must be false when grounded was forced false")
	}
}

func TestNormalize_BlankEntriesDropped(t *testing.T) {
	v := &Verdict{
		Coverage:     CoveragePartial,
		Missing:      []string{"  ", "real item", ""},
		OpenUnknowns: []string{"\t", "unknown x"},
	}
	v.Normalize()
	if len(v.Missing) != 1 || v.Missing[0] != "real item" {
		t.Fatalf("blank missing entries not cleaned: %#v", v.Missing)
	}
	if len(v.OpenUnknowns) != 1 || v.OpenUnknowns[0] != "unknown x" {
		t.Fatalf("blank unknowns not cleaned: %#v", v.OpenUnknowns)
	}
}

func TestNormalize_BlankCoverageFailsTowardRefusal(t *testing.T) {
	// g4 / req 7.1: a blank coverage carries NO affirmative completeness
	// signal, so it fails TOWARD REFUSAL (partial) — an empty missing list is
	// silence, never a claim of completion. Completeness is only ever asserted
	// by an explicit coverage=full.
	noSignal := &Verdict{Coverage: "", Missing: nil}
	noSignal.Normalize()
	if noSignal.Coverage != CoveragePartial {
		t.Fatalf("blank coverage with no missing must fail toward refusal (partial), got %q", noSignal.Coverage)
	}
	if noSignal.CoverageComplete() {
		t.Fatal("a signal-less verdict must not report CoverageComplete")
	}
	withMissing := &Verdict{Coverage: "", Missing: []string{"x"}}
	withMissing.Normalize()
	if withMissing.Coverage != CoveragePartial {
		t.Fatalf("blank coverage with missing should be partial, got %q", withMissing.Coverage)
	}
}

func TestNormalize_CertaintyClamped(t *testing.T) {
	for _, tc := range []struct{ in, want float64 }{{-0.5, 0}, {1.7, 1}, {0.3, 0.3}} {
		v := &Verdict{Certainty: tc.in}
		v.Normalize()
		if v.Certainty != tc.want {
			t.Fatalf("certainty %v clamped to %v, want %v", tc.in, v.Certainty, tc.want)
		}
	}
}

func TestCoverageCompleteAndSound(t *testing.T) {
	complete := &Verdict{Grounded: true, Coverage: CoverageFull}
	complete.Normalize()
	if !complete.CoverageComplete() {
		t.Fatal("expected CoverageComplete true for full/no-missing verdict")
	}
	if !complete.Sound() {
		t.Fatal("expected Sound true for grounded full verdict")
	}

	covOnly := &Verdict{Grounded: false, Coverage: CoverageFull}
	covOnly.Normalize()
	if !covOnly.CoverageComplete() {
		t.Fatal("CoverageComplete must ignore grounding")
	}
	if covOnly.Sound() {
		t.Fatal("Sound must require grounding")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
