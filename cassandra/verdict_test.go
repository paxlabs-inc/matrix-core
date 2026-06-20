// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import "testing"

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

func TestNormalize_CoverageInferredWhenBlank(t *testing.T) {
	full := &Verdict{Coverage: "", Missing: nil}
	full.Normalize()
	if full.Coverage != CoverageFull {
		t.Fatalf("blank coverage with no missing should infer full, got %q", full.Coverage)
	}
	partial := &Verdict{Coverage: "", Missing: []string{"x"}}
	partial.Normalize()
	if partial.Coverage != CoveragePartial {
		t.Fatalf("blank coverage with missing should infer partial, got %q", partial.Coverage)
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

func TestCheckCitations_G3_PhantomEvidenceUnGrounds(t *testing.T) {
	evidence := "TOOL chain_info\n  -> {\"blockNumber\": 998877}\n"
	v := &Verdict{Grounded: true, Coverage: CoverageFull}
	v.Normalize()
	phantom := v.CheckCitations([]string{"998877", "0xdeadbeef"}, evidence)
	if len(phantom) != 1 || phantom[0] != "0xdeadbeef" {
		t.Fatalf("expected phantom [0xdeadbeef], got %#v", phantom)
	}
	if v.Grounded {
		t.Fatal("g3: grounded must be forced false when a cited ref is absent from evidence")
	}

	v2 := &Verdict{Grounded: true, Coverage: CoverageFull}
	v2.Normalize()
	if got := v2.CheckCitations([]string{"998877"}, evidence); len(got) != 0 {
		t.Fatalf("expected no phantom refs, got %#v", got)
	}
	if !v2.Grounded {
		t.Fatal("g3: grounded must stay true when all cited refs are present")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
