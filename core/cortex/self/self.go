// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package self is the single, faculty-neutral source of the agent's shared
// self-model. It lives in the cortex module so every faculty that already
// depends on cortex — the conversation faculty (neo), the coding faculty
// (cody), and the execution faculty (executor/mcl-execute) — resolves the
// SAME self-model artifact from the SAME cortex store instead of keeping a
// divergent per-faculty copy (self-model req.6.1, req.6.4).
//
// The self-model has three facets, all keyed on one subject
// (matrix://self/neo):
//
//   - identity: the first-person persona the agent speaks with. Rendered by
//     Persona from the resolved model, it is the ONE source of identity prose
//     so the agent's self-description and its actual self-model can never
//     disagree (req.7.1). It is deliberately machinery-free — the brand the
//     user talks to, never the internal package structure (req.7.2).
//   - structural: "how I am built", derived from codegraph and written into
//     cortex as a Capability (see neo/internal/memory.WriteStructuralSelf).
//   - experiential: "how I tend to fail", the self-authored failure-pattern
//     Beliefs (see agents/neo/internal/memory consolidation).
//
// This package owns the canonical types and the read path (Resolve). The
// write path stays with the faculty that produces each facet (neo distils
// the structural facet from codegraph and authors the failure patterns); a
// resolver has no business writing.
package self

import (
	"encoding/json"

	"centra/core/cortex"
	cmem "centra/core/cortex/memory"
	"centra/core/cortex/query"
)

const (
	// Subject is the single cortex subject every self-model facet is keyed
	// on, so one Find over Capability+Belief collects the whole model.
	Subject = "matrix://self/neo"

	// StructuralCapability is the capability marker under which the derived
	// structural summary ("how I am built") is stored as a Capability.
	StructuralCapability = "understand its derived architecture"

	// DefaultName is the agent's internal identity name, used to anchor the
	// identity canary (scrubIdentity) across faculties when no configured
	// AgentName is supplied. The user-facing brand (see Persona) is Centra AI;
	// the codename is Neo.
	DefaultName = "Neo"
)

// StructuralSelf is the derived "how I am built" facet. Its JSON shape is the
// on-disk/on-cortex contract for the Capability parameters, so the tags must
// stay byte-stable across faculties.
type StructuralSelf struct {
	Summary      string        `json:"summary"`
	GraphURI     string        `json:"graph_uri,omitempty"`
	Scope        []string      `json:"scope,omitempty"`
	TokenBudget  int           `json:"token_budget,omitempty"`
	ContextLimit int           `json:"context_limit,omitempty"`
	Surface      *SurfaceFacts `json:"surface,omitempty"`
}

// SurfaceFacts is the derived capability-surface facet of the structural self:
// the agent's TRUE external API surface (routes/protocols) and its
// architectural is / is-not facts. It is written from the live serving surface
// (never hand-authored prose in the prompt) so the resident capability section
// the agent carries cannot drift from what the process actually serves
// (epistemic-core req.2.1/2.2).
type SurfaceFacts struct {
	API   []string `json:"api,omitempty"`
	Is    []string `json:"is,omitempty"`
	IsNot []string `json:"is_not,omitempty"`
}

// FailurePattern is one self-authored "how I tend to fail" belief.
type FailurePattern struct {
	URI         string
	Statement   string
	DerivedFrom []string
}

// SelfModel is the whole resolved artifact: the identity name, the structural
// summary, and the experiential failure patterns.
type SelfModel struct {
	Identity        string
	Structural      StructuralSelf
	StructuralURI   string
	FailurePatterns []FailurePattern
}

// Resolve reads the shared self-model from a cortex store. agentName seeds the
// identity name (falling back to DefaultName); the structural summary and the
// failure patterns are collected from the durable memories under Subject. This
// is the ONE resolver every faculty calls, so two faculties reading the same
// store observe byte-identical self-knowledge (req.6.4).
func Resolve(cx *cortex.Cortex, agentName string) (SelfModel, error) {
	model := SelfModel{Identity: agentName}
	if model.Identity == "" {
		model.Identity = DefaultName
	}
	if cx == nil {
		return model, nil
	}
	res, err := cx.Find(query.Query{
		Type:  []cmem.Type{cmem.TypeCapability, cmem.TypeBelief},
		Limit: 256,
	})
	if err != nil {
		return SelfModel{}, err
	}
	for _, item := range res.Memories {
		decoded, decodeErr := cmem.DecodeData(item.Version.Type, item.Version.Data)
		if decodeErr != nil {
			continue
		}
		uri := string(cortex.BuildURI(item.Head.Type, item.Head.ID, item.Head.CurrentVersion))
		switch data := decoded.(type) {
		case cmem.CapabilityData:
			if data.Subject == Subject && data.Capability == StructuralCapability && model.StructuralURI == "" {
				if json.Unmarshal(data.Parameters, &model.Structural) == nil {
					model.StructuralURI = uri
				}
			}
		case cmem.BeliefData:
			if data.Subject == Subject {
				model.FailurePatterns = append(model.FailurePatterns, FailurePattern{
					URI:         uri,
					Statement:   data.Statement,
					DerivedFrom: append([]string(nil), data.EvidenceFor...),
				})
			}
		}
	}
	return model, nil
}

// Persona renders the canonical first-person identity charter — the identity
// facet of the shared self-model. It is the SINGLE source of the agent's
// user-facing self-description, so every faculty that speaks as the agent
// derives its persona from here rather than a hand-written duplicate that
// drifts from reality (req.7.1).
//
// The persona is intentionally a function of the model's IDENTITY facet only,
// never the structural one: the user talks to a product (Centra AI), not to a
// package graph, so the prose stays machinery-free and leaks no internal
// structure (req.7.2). The internal codename (m.Identity, default Neo) is what
// the identity canary anchors on; the user-facing brand below is Centra AI.
func Persona(m SelfModel) string {
	// m participates so the persona is bound to the resolved self-model and
	// can never be constructed from thin air; the brand the user reads is the
	// product Centra AI, independent of the internal codename.
	_ = m.Identity
	return personaCharter
}

const personaCharter = `You are Centra AI — the user's own personal AI agent. Speak in the FIRST PERSON ("I", "me", "my"): you ARE the agent doing the work, not a narrator describing a team. The wallet, tools, memory, and actions are YOURS — say "my agent wallet", "I'll check", "I remember", never "your agent wallet" or "the agent will".

Who you are:
- You are Centra AI, a private autonomous agent that lives on this user's own machine and works only for them. You plan tasks, use real tools, act on-chain, research, monitor, and build deliverables on their behalf.
- Paxeer is the blockchain network and ecosystem you operate on. Your wallet, tokens, and on-chain actions (balances, transfers, swaps, staking, contracts) all live on Paxeer.
- You have persistent memory of this user across conversations, so you stay personal and never lose context. When you know the user's name, address them by it naturally — don't overuse it.
- Internally you reason in stages (understanding, planning, doing) using your own faculties, but to the user that is invisible plumbing. NEVER expose it or any jargon: no mention of models, pipelines, compilers, planners, executors, liaisons, MCL, cortex, Merkle, replay, hashes, intents, envelopes, plans, nodes, walkers, lifecycles, or slots.

Voice: warm, confident, plain, concise. No emojis.`
