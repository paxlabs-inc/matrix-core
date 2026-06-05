// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command gideon-ingest builds Gideon's cortex knowledge graph directly from
// the on-disk ops corpus and the HyperPax-OS source tree.
//
// It opens an actor's cortex store in-process (no daemon, no HTTP) and writes
// typed memory nodes plus typed edges:
//
//	Ops corpus (priority) — knowledge/core_chats/
//	  RUNBOOK.md      -> Pattern (failure modes) + Capability (recovery
//	                     procedures) + Event (incident history), with
//	                     caused-by and resolved-by edges.
//	  9 issue/fix logs -> Event (incident) + Pattern (symptom->diagnosis->fix)
//	                     with derived-from and caused-by edges.
//	Source graph — knowledge/HyperPax-OS/{x/*,precompiles/*,app,rpc,indexer}
//	  each module -> Fact, each keeper -> Capability, with depends-on
//	  (references) and keeper-of (part-of) edges. Fix Patterns are linked to
//	  the module Facts they name.
//
// Parsing is deterministic and dependency-free: markdown heading/section
// splitting and a directory walk; no LLM is consulted. Re-runs are idempotent
// — every node carries a stable `gideon:key:<key>` tag, so a second run
// Updates changed nodes and skips unchanged ones rather than duplicating.
//
// Usage:
//
//	gideon-ingest -cortex-root /path/to/cortex [-cortex-actor gideon]
//	              [-knowledge knowledge] [-dry-run]
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"matrix/cortex"
	"matrix/cortex/store"
)

func main() {
	cortexRoot := flag.String("cortex-root", "", "cortex store root directory (required)")
	cortexActor := flag.String("cortex-actor", "gideon", "cortex actor name (subdir under -cortex-root)")
	knowledge := flag.String("knowledge", "knowledge", "path to the knowledge corpus directory")
	dryRun := flag.Bool("dry-run", false, "parse and report counts without writing to cortex")
	flag.Parse()

	if *cortexRoot == "" {
		fmt.Fprintln(os.Stderr, "gideon-ingest: -cortex-root is required")
		flag.Usage()
		os.Exit(2)
	}
	if err := run(*cortexRoot, *cortexActor, *knowledge, *dryRun); err != nil {
		fmt.Fprintf(os.Stderr, "gideon-ingest: %v\n", err)
		os.Exit(1)
	}
}

func run(root, actor, knowledge string, dryRun bool) error {
	coreChats := filepath.Join(knowledge, "core_chats")
	runbook := filepath.Join(coreChats, "RUNBOOK.md")
	hyperpax := filepath.Join(knowledge, "HyperPax-OS")

	if _, err := os.Stat(coreChats); err != nil {
		return fmt.Errorf("knowledge corpus not found: %s: %w", coreChats, err)
	}

	ig := newIngester(actor, dryRun)

	if !dryRun {
		if err := os.MkdirAll(root, 0o755); err != nil {
			return fmt.Errorf("mkdir cortex-root: %w", err)
		}
		s, err := store.Open(root, actor, nil)
		if err != nil {
			return fmt.Errorf("store.Open: %w", err)
		}
		defer s.Close()
		ig.cx = cortex.New(s)
	}

	// Ops corpus first (priority): the RUNBOOK and chat logs cover ~99% of
	// expected issues, and chat incidents link back to RUNBOOK failure modes.
	if err := ig.ingestRunbook(runbook); err != nil {
		return fmt.Errorf("ingest runbook: %w", err)
	}
	if err := ig.ingestChats(coreChats); err != nil {
		return fmt.Errorf("ingest chats: %w", err)
	}
	if err := ig.ingestModules(hyperpax); err != nil {
		return fmt.Errorf("ingest modules: %w", err)
	}
	if err := ig.crossLinkPatternsToModules(); err != nil {
		return fmt.Errorf("cross-link patterns: %w", err)
	}

	ig.report(os.Stdout, root, actor, dryRun)
	return nil
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
