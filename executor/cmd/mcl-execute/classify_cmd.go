// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// classify_cmd.go — `classify` subcommand: standalone D9 §18.1
// materiality classifier. Reads two intents + two plans (original and
// candidate) from JSON files, runs materiality.Classify, prints the
// result.
//
// Usage:
//
//   mcl-execute classify \
//       -orig-intent intent_v1.json -orig-plan plan_v1.json \
//       -new-intent  intent_v2.json -new-plan  plan_v2.json \
//       [-orig-anchor=true] [-new-anchor=true]
//
// Output: JSON document on stdout with shape:
//
//   {"material": true,
//    "reasons": [{"rule": "budget_delta", "detail": "..."}, ...]}

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"matrix/executor/materiality"
	"matrix/mcl/ir"
)

func runClassify(args []string) {
	fs := flag.NewFlagSet("classify", flag.ExitOnError)
	var (
		origIntent = fs.String("orig-intent", "", "path to original Intent JSON")
		origPlan   = fs.String("orig-plan", "", "path to original PlanTree JSON")
		newIntent  = fs.String("new-intent", "", "path to candidate Intent JSON")
		newPlan    = fs.String("new-plan", "", "path to candidate PlanTree JSON")
		origAnchor = fs.Bool("orig-anchor", false, "anchor flag on original intent.accept")
		newAnchor  = fs.Bool("new-anchor", false, "anchor flag on candidate intent.accept")
		emitPretty = fs.Bool("pretty", true, "indent classification JSON output")
	)
	fs.Parse(args)

	in := materiality.Inputs{
		OriginalIntent: loadIntent(*origIntent),
		OriginalPlan:   loadPlan(*origPlan),
		NewIntent:      loadIntent(*newIntent),
		NewPlan:        loadPlan(*newPlan),
		OriginalAnchor: *origAnchor,
		NewAnchor:      *newAnchor,
	}

	cls := materiality.Classify(in)

	out := map[string]interface{}{
		"material": cls.Material,
		"reasons":  cls.Reasons,
	}
	var buf []byte
	var err error
	if *emitPretty {
		buf, err = json.MarshalIndent(out, "", "  ")
	} else {
		buf, err = json.Marshal(out)
	}
	if err != nil {
		fatalf("classify: marshal: %v", err)
	}
	fmt.Println(string(buf))

	if cls.Material {
		// Non-zero exit signals "material — caller should rewind".
		// CI scripts can rely on this for automated re-accept flows.
		os.Exit(2)
	}
}

func loadIntent(path string) *ir.Intent {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fatalf("classify: read %s: %v", path, err)
	}
	var out ir.Intent
	if err := json.Unmarshal(b, &out); err != nil {
		fatalf("classify: unmarshal %s: %v", path, err)
	}
	return &out
}

func loadPlan(path string) *ir.PlanTree {
	if path == "" {
		return nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		fatalf("classify: read %s: %v", path, err)
	}
	var out ir.PlanTree
	if err := json.Unmarshal(b, &out); err != nil {
		fatalf("classify: unmarshal %s: %v", path, err)
	}
	return &out
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
