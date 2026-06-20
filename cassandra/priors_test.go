// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package cassandra

import "testing"

func TestScanPriors_Degenerate(t *testing.T) {
	for _, ev := range []string{"", "   ", "(no plan executed)", "(plan executed but produced no recorded output)"} {
		sig := ScanPriors(PriorInput{Evidence: ev})
		if !sig.DegenerateEvidence {
			t.Fatalf("expected degenerate for %q", ev)
		}
		if !sig.FlagsCompletionRisk() {
			t.Fatalf("degenerate evidence should flag completion risk for %q", ev)
		}
	}
}

func TestScanPriors_Truncation(t *testing.T) {
	sig := ScanPriors(PriorInput{Evidence: "TOOL read\n  -> big output ... (truncated to 200 chars)"})
	if !sig.TruncationSuspect {
		t.Fatal("expected truncation suspect")
	}
	// A clean, substantive digest fires nothing.
	clean := ScanPriors(PriorInput{Evidence: "TOOL chain_info\n  -> {\"blockNumber\": 42}"})
	if clean.FlagsCompletionRisk() {
		t.Fatalf("clean evidence should not flag risk: %#v", clean)
	}
}

func TestScanPriors_DegenerateSuppressesTruncation(t *testing.T) {
	// A "nothing ran" sentinel must not also be reported as a truncation.
	sig := ScanPriors(PriorInput{Evidence: "(no plan executed)"})
	if sig.TruncationSuspect {
		t.Fatal("degenerate sentinel should not be flagged as truncation")
	}
}

func TestScanPriors_SelfReportedGaps(t *testing.T) {
	sig := ScanPriors(PriorInput{Evidence: "STEP s1\n  -> I could not reach the RPC and was unable to read the balance"})
	if len(sig.SelfReportedGaps) == 0 {
		t.Fatal("expected self-reported gap phrases")
	}
	seen := map[string]bool{}
	for _, g := range sig.SelfReportedGaps {
		if seen[g] {
			t.Fatalf("duplicate gap phrase %q", g)
		}
		seen[g] = true
	}
	if !seen["could not"] || !seen["unable to"] {
		t.Fatalf("expected 'could not' and 'unable to', got %#v", sig.SelfReportedGaps)
	}
}

func TestScanPriors_FinishedByLength(t *testing.T) {
	if !ScanPriors(PriorInput{Evidence: "x", FinishReason: "length"}).FinishedByLength {
		t.Fatal("expected FinishedByLength for finish_reason=length")
	}
	if ScanPriors(PriorInput{Evidence: "x", FinishReason: "stop"}).FinishedByLength {
		t.Fatal("stop must not flag FinishedByLength")
	}
}

func TestScanPriors_UnderProduced(t *testing.T) {
	if !ScanPriors(PriorInput{Evidence: "x", RequestedDeliverables: 8, ProducedResults: 3}).UnderProduced {
		t.Fatal("expected UnderProduced when produced < requested")
	}
	if ScanPriors(PriorInput{Evidence: "x", RequestedDeliverables: 0, ProducedResults: 0}).UnderProduced {
		t.Fatal("unknown counts must not flag UnderProduced")
	}
	if ScanPriors(PriorInput{Evidence: "x", RequestedDeliverables: 3, ProducedResults: 3}).UnderProduced {
		t.Fatal("equal counts must not flag UnderProduced")
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
