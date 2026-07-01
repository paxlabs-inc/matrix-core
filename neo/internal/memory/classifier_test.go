// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package memory

import (
	"context"
	"testing"
)

// These tests exercise the REAL deterministic financial classifier and its
// REAL wiring into RememberOpportunity (against a real cortex store). No stubs,
// no fakes.

func TestClassifyFinancialKeywords(t *testing.T) {
	financial := []string{
		"Swap 100 PAX for USDC at the best rate",
		"Buy more of the dip while it's low",
		"Sell half the position before earnings",
		"Pay the electric bill that's due Friday",
		"Send money to the landlord for rent",
		"Transfer funds to the savings account",
		"Stake the tokens for the higher yield",
		"Mint the NFT collection you sketched",
		"Trade ETH into a stablecoin",
		"Top up the wallet before the trip",
		"Settle the invoice from the contractor",
		"Make an on-chain write to register the name",
		"Withdraw the deposit from the exchange",
		"Spend the gift card before it expires",
		"It will cost about $250 to fix",
		// "transfer" is a spec-listed financial keyword: fail-closed treats even
		// a transfer-of-files task as financial (surfaced for approval, not auto-run).
		"Transfer the design files into the shared folder",
	}
	for _, s := range financial {
		if !ClassifyFinancial(s) {
			t.Errorf("ClassifyFinancial(%q) = false, want true (financial signal present)", s)
		}
	}

	nonFinancial := []string{
		"Draft the quarterly update doc you mentioned",
		"Summarize the API rate-limit thread",
		"Book the dentist appointment for next Tuesday",
		"Clean up the stale TODO comments",
		"Refactor the config loader",
		"Compile the migration notes into a checklist",
		"Send the meeting recap email to the team",
		"Outline the blog post about onboarding",
	}
	for _, s := range nonFinancial {
		if ClassifyFinancial(s) {
			t.Errorf("ClassifyFinancial(%q) = true, want false (no financial signal)", s)
		}
	}
}

func TestEligibleForAutonomyFailClosed(t *testing.T) {
	cases := []struct {
		name           string
		summary        string
		modelFinancial bool
		wantEligible   bool
	}{
		{"clearly non-financial, model agrees", "Draft the quarterly update doc", false, true},
		{"model flags financial, benign summary", "Organize the photo library", true, false},
		{"model misses it, keyword catches it", "Buy the domain you wanted", false, false},
		{"model misses it, phrase catches it", "Make an on-chain write for the alias", false, false},
		{"model misses it, symbol catches it", "Reserve the venue for about $500", false, false},
		{"both agree financial", "Swap PAX for USDC", true, false},
		{"spec keyword transfer fails closed even for files", "Transfer the slides to the new deck", false, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := EligibleForAutonomy(tc.summary, tc.modelFinancial); got != tc.wantEligible {
				t.Errorf("EligibleForAutonomy(%q, %v) = %v, want %v", tc.summary, tc.modelFinancial, got, tc.wantEligible)
			}
		})
	}
}

// TestRememberOpportunityHardensEligibility proves the re-check is the final
// word at capture: a model-said-non-financial opportunity whose summary trips
// the deterministic classifier is persisted ineligible and is therefore never
// returned by the autonomous picker (PendingOpportunities), while a genuinely
// non-financial one survives as eligible.
func TestRememberOpportunityHardensEligibility(t *testing.T) {
	p, err := Open(testCfg(t))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer p.Close()
	ctx := context.Background()

	// Model said non-financial (EligibleAutonomous=true) but the summary is
	// plainly financial — the re-check must flip it to ineligible.
	if _, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary:            "Swap your idle PAX into USDC while rates are good",
		EligibleAutonomous: true,
		Confidence:         0.9,
	}); err != nil {
		t.Fatalf("RememberOpportunity financial: %v", err)
	}
	// A genuinely non-financial opportunity stays eligible.
	if _, err := p.RememberOpportunity(ctx, OpportunitySpec{
		Summary:            "Draft the release notes for v2",
		EligibleAutonomous: true,
		Confidence:         0.8,
	}); err != nil {
		t.Fatalf("RememberOpportunity benign: %v", err)
	}

	pending, err := p.PendingOpportunities(ctx, 0)
	if err != nil {
		t.Fatalf("PendingOpportunities: %v", err)
	}
	if len(pending) != 1 {
		t.Fatalf("autonomous picker set = %d, want 1 (financial item must be filtered out)", len(pending))
	}
	if pending[0].Summary != "Draft the release notes for v2" {
		t.Errorf("unexpected eligible opportunity surfaced: %q", pending[0].Summary)
	}
}
