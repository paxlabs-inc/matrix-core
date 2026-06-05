// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command mcl-execute is the Session 23 end-to-end driver:
//
//	skill_loader → bridge.Adapter + compiler LLM → Intent
//	  → executor LLM (plan_tree@1) → PlanTree
//	  → mcp.Manager + tool.Registry → runtime.Walker
//	  → cortex Event memory per step → envelope.IntentAttest
//
// All four sess#23 surfaces wired:
//
//   - runtime.SkillLoader    (matrix://skill/<slug>@<v> → SKILL.mtx AST)
//   - runtime.Walker         (DFS plan walk with pluggable handlers)
//   - materiality.Classify   (D9 §18.1 classifier — `classify` subcommand)
//   - cmd/mcl-execute        (this binary)
//
// Production-grade defaults: executor LLM is REQUIRED for the `walk`
// subcommand (no hand-authored-plan fallback); compiler LLM is REQUIRED
// unless -intent <file> pre-loads a compiled Intent. The CLI exits with
// a clear error when API keys are missing.
//
// Citations:
//   - matrix.kvx executor_locked_design Q4/Q12/Q13/Q14/Q16/Q17/Q22
//   - research/02-protocol.md §18.1 (materiality)
//   - research/06-agents.md §5.2 (plan synthesis runs under executor)
//   - MCL/ir/plan.go:3-8 (PlanTree IR shared producer+consumer)
//
// Three subcommands:
//
//	walk      Full pipeline: compile → synthesise plan → walk → attest.
//	classify  Run §18.1 materiality classifier on two plans + intents.
//	loader    Smoke-check the SkillLoader against the 159-skill corpus.

package main

import (
	"fmt"
	"os"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "walk":
		runWalk(os.Args[2:])
	case "classify":
		runClassify(os.Args[2:])
	case "loader":
		runLoader(os.Args[2:])
	case "daemon":
		runDaemon(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mcl-execute: unknown subcommand %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `mcl-execute — Matrix executor end-to-end driver (sess#23)

Subcommands:
  walk      Compile prose → synthesise plan via executor LLM → walk → attest.
  classify  Run §18.1 materiality classifier on two plans + intents.
  loader    Load a skill via matrix://skill/<slug>@<v> and dump metadata.
  daemon    Long-running HTTP+SSE server, single-user; reuses one infra
            (cortex+MCP+registry) across many messages (sess#24).

Run a subcommand with -h for its specific flags.
`)
}

func fatalf(format string, args ...interface{}) {
	fmt.Fprintf(os.Stderr, "mcl-execute: "+format+"\n", args...)
	os.Exit(1)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
