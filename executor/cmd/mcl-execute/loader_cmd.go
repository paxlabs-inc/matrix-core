// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// loader_cmd.go — `loader` subcommand: smoke-check the SkillLoader
// against the 159-skill corpus.
//
// Resolves a matrix://skill/<slug>@<version> URI, parses + validates
// the SKILL.mtx, and dumps the resolved metadata + canonical hash.
//
// Usage:
//
//   mcl-execute loader -skill matrix://skill/writing-plans@0.1.0
//   mcl-execute loader -skill matrix://skill/writing-plans@0.1.0 -dump-md
//   mcl-execute loader -list-corpus  # quick walk of skills/ root

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"matrix/executor/runtime"
)

func runLoader(args []string) {
	fs := flag.NewFlagSet("loader", flag.ExitOnError)
	var (
		skillURI   = fs.String("skill", "", "matrix://skill/<slug>@<version> URI")
		skillsRoot = fs.String("skills-root", "/root/matrix/skills", "skill repository root")
		dumpMd     = fs.Bool("dump-md", false, "also dump SKILL.md body when present")
		listCorpus = fs.Bool("list-corpus", false, "list every <slug>/SKILL.mtx in -skills-root")
	)
	fs.Parse(args)

	if *listCorpus {
		listCorpusDirs(*skillsRoot)
		return
	}

	if *skillURI == "" {
		fatalf("loader: -skill is required (or use -list-corpus)")
	}

	loader := runtime.NewSkillLoader(*skillsRoot)
	skill, err := loader.Load(*skillURI)
	if err != nil {
		fatalf("loader: %v", err)
	}

	// Compose a stable JSON dump for the human + automation paths.
	out := map[string]interface{}{
		"uri":            skill.URI,
		"slug":           skill.Slug,
		"id":             skill.ID,
		"version":        skill.Version,
		"display":        skill.Display,
		"author":         skill.Author,
		"description":    skill.Description,
		"mcl_verbs":      skill.MclVerbs,
		"canonical_hash": skill.CanonicalHash,
		"mtx_path":       skill.MtxPath,
		"md_path":        skill.MdPath,
		"sections":       sectionList(skill),
	}
	if *dumpMd {
		out["md_body"] = string(skill.MdBytes)
	}
	js, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		fatalf("loader: marshal: %v", err)
	}
	fmt.Println(string(js))
}

// sectionList returns the §SECTION names in declaration order. Provides
// a quick sanity check for the loader pipeline without re-parsing.
func sectionList(skill *runtime.LoadedSkill) []string {
	if skill == nil || skill.File == nil {
		return nil
	}
	out := make([]string, 0, len(skill.File.Sections))
	for _, s := range skill.File.Sections {
		out = append(out, s.Name)
	}
	return out
}

// listCorpusDirs walks <root>/<slug>/SKILL.mtx and prints one line per
// resolved skill. Useful as a pre-flight before running the full
// loader against any skill in the 159-skill corpus.
func listCorpusDirs(root string) {
	entries, err := os.ReadDir(root)
	if err != nil {
		fatalf("loader: read corpus root %s: %v", root, err)
	}
	rows := make([]string, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mtxPath := filepath.Join(root, e.Name(), "SKILL.mtx")
		if _, err := os.Stat(mtxPath); err != nil {
			continue
		}
		mdPath := filepath.Join(root, e.Name(), "SKILL.md")
		mdMark := " "
		if _, err := os.Stat(mdPath); err == nil {
			mdMark = "+"
		}
		rows = append(rows, fmt.Sprintf("  %s%s  %s", mdMark, strings.Repeat(" ", 30-min(30, len(e.Name()))), e.Name()))
	}
	sort.Strings(rows)
	fmt.Fprintf(os.Stderr, "skill corpus at %s — %d skills:\n", root, len(rows))
	for _, r := range rows {
		fmt.Println(r)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
