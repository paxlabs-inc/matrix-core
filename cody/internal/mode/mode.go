// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package mode defines Cody's modes — Prototype / Engineer / Architect — as
// POLICY TUPLES over one engine: planning depth, verification cadence,
// creativity temperature, autonomy level, and explanation register. There are
// no mode-specific loops; a mode only parameterizes the one orchestrator/
// worker engine. The constitution binds identically in every mode — nothing
// in a Policy can weaken the acceptance gate, which is why none of these
// fields reach it. Model policy is per role and mode-aware (the orchestrator
// pins a stronger model; Prototype workers run faster ones), all metered
// through the 'cody' gateway slot.
package mode

import (
	"fmt"
	"strings"

	mcllm "matrix/mcl/llm"
)

// Mode names one policy preset.
type Mode string

const (
	// Prototype is for vibe coders and spikes: terse inline plan, verify at
	// milestones and at done, creative opinionated defaults, outcome language.
	Prototype Mode = "prototype"
	// Engineer is the default: waved task list, verify after every task,
	// balanced rigor, technical register.
	Engineer Mode = "engineer"
	// Architect is for long-horizon work: durable in-workspace spec files
	// (EARS requirements, design, waved tasks), per-task verification with
	// property tests, resumable across sessions.
	Architect Mode = "architect"
)

// Parse resolves a mode name; empty defaults to Engineer.
func Parse(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "":
		return Engineer, nil
	case Prototype:
		return Prototype, nil
	case Engineer:
		return Engineer, nil
	case Architect:
		return Architect, nil
	}
	return "", fmt.Errorf("mode: unknown mode %q (want prototype|engineer|architect)", s)
}

// Planning depths.
const (
	PlanInline    = "inline"     // terse inline plan
	PlanWaved     = "waved"      // waved task list
	PlanSpecFiles = "spec_files" // durable in-workspace spec files
)

// Verification cadences. The acceptance gate itself never relaxes — cadence
// only shapes how often WORKERS verify mid-task and whether property tests
// are demanded.
const (
	VerifyMilestones      = "milestones"        // at milestones and at done
	VerifyPerTask         = "per_task"          // after every task
	VerifyPerTaskProperty = "per_task_property" // per task + property tests
)

// Explanation registers.
const (
	RegisterOutcome   = "outcome"   // "your site now has login"
	RegisterTechnical = "technical" // engineer-grade detail
)

// GatewaySlot is Cody's own metered gateway LLM slot. Never borrowed.
const GatewaySlot = "cody"

// Default per-role models. Operators override via config; the defaults stay
// on the cody slot whitelist. The orchestrator (planning + adjudication) pins
// the stronger model in every mode; Prototype workers run the fast router.
const (
	defaultOrchestratorModel = "deepseek-ai/DeepSeek-V4-Pro"
	defaultWorkerModel       = "nvidia/NVIDIA-Nemotron-3-Ultra-550B-A55B"
	defaultFastWorkerModel   = "accounts/fireworks/routers/glm-5p1-fast"
)

// Policy is one mode rendered as an explicit tuple. It parameterizes the one
// engine — it can dial ceremony, never the standard.
type Policy struct {
	Mode          Mode    `json:"mode"`
	PlanningDepth string  `json:"planning_depth"`
	VerifyCadence string  `json:"verify_cadence"`
	Creativity    float64 `json:"creativity"` // worker sampling temperature
	Autonomy      string  `json:"autonomy"`   // high | balanced | deliberate
	Register      string  `json:"register"`
	// Per-role models, mode-aware. Both route through GatewaySlot.
	OrchestratorModel string `json:"orchestrator_model"`
	WorkerModel       string `json:"worker_model"`
}

// For returns the policy tuple for a mode.
func For(m Mode) Policy {
	switch m {
	case Prototype:
		return Policy{
			Mode:              Prototype,
			PlanningDepth:     PlanInline,
			VerifyCadence:     VerifyMilestones,
			Creativity:        0.7,
			Autonomy:          "high",
			Register:          RegisterOutcome,
			OrchestratorModel: defaultOrchestratorModel,
			WorkerModel:       defaultFastWorkerModel,
		}
	case Architect:
		return Policy{
			Mode:              Architect,
			PlanningDepth:     PlanSpecFiles,
			VerifyCadence:     VerifyPerTaskProperty,
			Creativity:        0.2,
			Autonomy:          "deliberate",
			Register:          RegisterTechnical,
			OrchestratorModel: defaultOrchestratorModel,
			WorkerModel:       defaultWorkerModel,
		}
	default: // Engineer
		return Policy{
			Mode:              Engineer,
			PlanningDepth:     PlanWaved,
			VerifyCadence:     VerifyPerTask,
			Creativity:        0.3,
			Autonomy:          "balanced",
			Register:          RegisterTechnical,
			OrchestratorModel: defaultOrchestratorModel,
			WorkerModel:       defaultWorkerModel,
		}
	}
}

// OrchestratorLLM builds the orchestrator-role client config: the stronger
// model, cold temperature (planning + adjudication), the cody slot.
func (p Policy) OrchestratorLLM(gatewayURL, actorDID string) mcllm.Config {
	return mcllm.Config{
		Model:       p.OrchestratorModel,
		Temperature: 0.1,
		MaxTokens:   8192,
		GatewayURL:  gatewayURL,
		ActorDID:    actorDID,
		SlotLabel:   GatewaySlot,
	}
}

// WorkerLLM builds the worker-role client config: the mode's worker model at
// the mode's creativity temperature, the cody slot.
func (p Policy) WorkerLLM(gatewayURL, actorDID string) mcllm.Config {
	return mcllm.Config{
		Model:       p.WorkerModel,
		Temperature: p.Creativity,
		MaxTokens:   8192,
		GatewayURL:  gatewayURL,
		ActorDID:    actorDID,
		SlotLabel:   GatewaySlot,
	}
}

// Render produces the mode-policy prose carried by task sheets and prompts.
// It deliberately restates that the constitution is untouchable: a worker
// reading a Prototype sheet must never mistake speed for permission to fake.
func (p Policy) Render() string {
	var b strings.Builder
	fmt.Fprintf(&b, "Active mode: %s.\n", p.Mode)
	switch p.PlanningDepth {
	case PlanInline:
		b.WriteString("Planning: terse and inline — plan just enough to move, no ceremony.\n")
	case PlanSpecFiles:
		b.WriteString("Planning: durable spec files (EARS requirements, design, waved tasks) persisted in the workspace; work is resumable across sessions.\n")
	default:
		b.WriteString("Planning: a waved task list with explicit dependencies.\n")
	}
	switch p.VerifyCadence {
	case VerifyMilestones:
		b.WriteString("Verification cadence: run verification at meaningful milestones and always before turning in.\n")
	case VerifyPerTaskProperty:
		b.WriteString("Verification cadence: verify after every change set AND cover the acceptance criteria with property-style tests where they fit.\n")
	default:
		b.WriteString("Verification cadence: verify after every task.\n")
	}
	switch p.Register {
	case RegisterOutcome:
		b.WriteString("Explanation register: outcome language — say what the user can now do, not how the code does it.\n")
	default:
		b.WriteString("Explanation register: technical — precise engineering detail, file and seam names welcome.\n")
	}
	fmt.Fprintf(&b, "Autonomy: %s. Creativity: %.1f.\n", p.Autonomy, p.Creativity)
	b.WriteString("The constitution binds identically in every mode: modes dial ceremony, never the standard.")
	return b.String()
}
