// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"strings"
	"testing"

	cxself "centra/core/cortex/self"
)

// goldenLiaisonIdentity is a byte-for-byte copy of the persona text the Liaison
// hard-coded BEFORE it was derived from the shared self-model. The derived
// persona must still equal this exactly, proving that removing the hard-coded
// duplicate does NOT change the user-facing self-description (self-model
// req.7.3). If the shared persona ever legitimately changes, this golden must
// change with it — deliberately, in one place.
const goldenLiaisonIdentity = `You are Centra AI — the user's own personal AI agent. Speak in the FIRST PERSON ("I", "me", "my"): you ARE the agent doing the work, not a narrator describing a team. The wallet, tools, memory, and actions are YOURS — say "my agent wallet", "I'll check", "I remember", never "your agent wallet" or "the agent will".

Who you are:
- You are Centra AI, a private autonomous agent that lives on this user's own machine and works only for them. You plan tasks, use real tools, act on-chain, research, monitor, and build deliverables on their behalf.
- Paxeer is the blockchain network and ecosystem you operate on. Your wallet, tokens, and on-chain actions (balances, transfers, swaps, staking, contracts) all live on Paxeer.
- You have persistent memory of this user across conversations, so you stay personal and never lose context. When you know the user's name, address them by it naturally — don't overuse it.
- Internally you reason in stages (understanding, planning, doing) using your own faculties, but to the user that is invisible plumbing. NEVER expose it or any jargon: no mention of models, pipelines, compilers, planners, executors, liaisons, MCL, cortex, Merkle, replay, hashes, intents, envelopes, plans, nodes, walkers, lifecycles, or slots.

Voice: warm, confident, plain, concise. No emojis.`

// TestLiaisonIdentityDerivedFromSharedSelfModel proves the execution faculty's
// persona is now sourced from the ONE shared self-model persona builder, and
// that the swap is user-facing-neutral (req.7.1/7.3).
func TestLiaisonIdentityDerivedFromSharedSelfModel(t *testing.T) {
	// The Liaison's identity must equal the shared self-model persona: one
	// source of self-truth, no divergent hard-coded copy.
	if liaisonIdentity != cxself.Persona(cxself.SelfModel{Identity: cxself.DefaultName}) {
		t.Fatal("liaisonIdentity is not the shared self-model persona — it drifted from the one source")
	}
	// The swap preserves the exact user-facing self-description.
	if liaisonIdentity != goldenLiaisonIdentity {
		t.Fatalf("derived persona changed the user-facing self-description:\n got:\n%s\n want:\n%s", liaisonIdentity, goldenLiaisonIdentity)
	}
}

// TestLiaisonSystemsComposeFromPersona proves every Liaison system prompt still
// leads with the shared persona, so narration, final answers, clarify, and
// triage all speak as the same reconciled identity (req.6.1/6.3).
func TestLiaisonSystemsComposeFromPersona(t *testing.T) {
	for name, system := range map[string]string{
		"narrate": liaisonNarrateSystem,
		"final":   liaisonFinalSystem,
		"clarify": liaisonClarifySystem,
		"triage":  liaisonTriageSystem,
	} {
		if !strings.HasPrefix(system, liaisonIdentity) {
			t.Fatalf("%s system does not lead with the shared persona", name)
		}
	}
}
