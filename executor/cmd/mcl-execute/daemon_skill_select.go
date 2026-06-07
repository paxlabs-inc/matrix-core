// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import "strings"

// tachyonEngineerSkillURI is the focused Solidity/EVM skill the selector routes
// smart-contract authoring/deploy intents to. It is tachyon-only (plus the
// read-only wallet/balance reads), which keeps the planner from blending the
// catch-all paxeer-assistant's three overlapping contract paths (fs file writes
// + exec.shell forge/cast + paxeer-net contract_read/write + tachyon) into one
// incoherent plan.
const tachyonEngineerSkillURI = "matrix://skill/tachyon-engineer@0.1.0"

// contractStrongSignals are substrings that, on their own, identify a request
// as smart-contract engineering (author/compile/test/deploy/interact Solidity).
// Matched case-insensitively against the prose.
var contractStrongSignals = []string{
	"solidity",
	"openzeppelin",
	"tachyon",
	"smart contract",
	"erc-20", "erc20",
	"erc-721", "erc721",
	"erc-1155", "erc1155",
	"foundry",
	"forge test",
	".sol",
	"bytecode",
	"constructor_args", "constructor args",
}

// contractWeakSignals only count when paired with a contract noun — they are
// too generic to route on alone (e.g. "deploy a service", "compile the app").
var contractWeakSignals = []string{"compile", "deploy", "abi", "mint function", "onlyowner"}

// selectSkill chooses the skill URI for an incoming user message when the
// client did NOT explicitly pin one. Strong Solidity/contract signals route to
// the focused tachyon-engineer skill; everything else keeps the configured
// catch-all default. The check is deterministic (no LLM) and conservative:
// generic build/deploy verbs only route when paired with a contract noun, so
// chain reads, swaps, transfers, and software tasks stay on the default skill.
//
// verb is accepted for future verb-aware routing but currently unused: skill
// selection happens before the compiler classifies the verb, so req.Verb is
// almost always empty here.
func (d *daemonState) selectSkill(prose, verb string) string {
	_ = verb
	if routeToTachyonEngineer(prose) {
		return tachyonEngineerSkillURI
	}
	return d.defaultSkillURI
}

// routeToTachyonEngineer reports whether prose is a smart-contract engineering
// request. Split out (and package-level) so it is unit-testable without a
// daemonState.
func routeToTachyonEngineer(prose string) bool {
	p := strings.ToLower(prose)
	for _, s := range contractStrongSignals {
		if strings.Contains(p, s) {
			return true
		}
	}
	// Weak signals require a contract noun to avoid routing "deploy a
	// service" / "compile the app" away from the default skill.
	if strings.Contains(p, "contract") {
		for _, s := range contractWeakSignals {
			if strings.Contains(p, s) {
				return true
			}
		}
	}
	return false
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
