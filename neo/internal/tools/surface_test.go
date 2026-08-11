// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package tools

import "testing"

func TestClassifyDefaultPatterns(t *testing.T) {
	c := NewClassifier(nil) // defaults: no barrier — everything is Natural
	names := []string{"transfer", "approve", "tachyon_deploy", "stream_settle", "deus_invoke", "swap_tokens", "bridge_assets", "mint_nft", "withdraw",
		"read_file", "list_directory", "web_search", "tachyon_compile", "tachyon_simulate", "git_status", "get_balance", "chain_info"}
	for _, n := range names {
		if got := c.Classify(n, "write"); got != Natural {
			t.Errorf("Classify(%q) = %v, want Natural (default surface has no escalate wall)", n, got)
		}
	}
}

func TestValueTransferGuardStillFlagsMoneyTools(t *testing.T) {
	escalate := []string{"transfer", "approve", "tachyon_deploy", "stream_settle", "deus_invoke", "swap_tokens", "bridge_assets", "mint_nft", "withdraw"}
	for _, n := range escalate {
		if !IsValueTransferTool(n) {
			t.Errorf("IsValueTransferTool(%q) = false, want true (autonomous surface guard)", n)
		}
	}
	natural := []string{"read_file", "list_directory", "web_search", "tachyon_compile", "tachyon_simulate", "git_status", "get_balance", "chain_info", "wallet_info", "sign_message"}
	for _, n := range natural {
		if IsValueTransferTool(n) {
			t.Errorf("IsValueTransferTool(%q) = true, want false", n)
		}
	}
}

func TestClassifyCustomPatterns(t *testing.T) {
	c := NewClassifier([]string{"danger"})
	if c.Classify("do_danger_now", "") != Escalate {
		t.Error("custom pattern should escalate")
	}
	// default words are no longer escalated when a custom list is supplied.
	if c.Classify("transfer", "") != Natural {
		t.Error("with custom patterns, 'transfer' should not match")
	}
}

func TestSurfaceString(t *testing.T) {
	if Natural.String() != "natural" || Escalate.String() != "escalate" {
		t.Errorf("surface strings wrong: %q / %q", Natural.String(), Escalate.String())
	}
}
