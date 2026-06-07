// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import "testing"

func TestRouteToTachyonEngineer(t *testing.T) {
	cases := []struct {
		name  string
		prose string
		want  bool
	}{
		// The failing task: full ERC-20 lifecycle. MUST route.
		{"erc20 lifecycle", "Build and deploy an ERC-20 token to Paxeer Mainnet, write a clean Solidity contract using OpenZeppelin's ERC20 and a Forge test suite.", true},
		{"solidity only", "Write a Solidity vault contract and deploy it.", true},
		{"sol extension", "Save the token to src/MFT.sol and compile it.", true},
		{"smart contract phrase", "Author a smart contract and run its tests.", true},
		{"tachyon explicit", "Use tachyon to compile and deploy my token.", true},
		{"weak paired", "Compile the contract and report the ABI.", true},

		// Must NOT route — these belong on the catch-all default skill.
		{"native transfer", "Transfer 5 PAX to 0xdead and show the tx hash.", false},
		{"balance read", "Show my agent wallet address and its PAX balance.", false},
		{"swap", "Swap 100 USDC for PAX on the DEX with 1% slippage.", false},
		{"deploy a service", "Build a price bot and deploy it as a service.", false},
		{"research", "Research the latest Paxeer TVL and summarize it.", false},
		{"empty", "", false},
	}
	for _, c := range cases {
		if got := routeToTachyonEngineer(c.prose); got != c.want {
			t.Errorf("%s: routeToTachyonEngineer(%q) = %v, want %v", c.name, c.prose, got, c.want)
		}
	}
}

func TestSelectSkill_FallsBackToDefault(t *testing.T) {
	d := &daemonState{defaultSkillURI: "matrix://skill/paxeer-assistant@0.1.0"}
	if got := d.selectSkill("check my PAX balance", ""); got != d.defaultSkillURI {
		t.Errorf("non-contract prose = %q, want default %q", got, d.defaultSkillURI)
	}
	if got := d.selectSkill("deploy a Solidity ERC-20 token", ""); got != tachyonEngineerSkillURI {
		t.Errorf("contract prose = %q, want %q", got, tachyonEngineerSkillURI)
	}
}
