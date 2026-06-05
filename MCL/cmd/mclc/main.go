// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command mclc is the MatrixScript compiler CLI.
//
// It wires together the MCL runtime packages (lexer, parser, validator,
// canonical, interpreter) and drives the 6-stage compilation pipeline
// defined in core/pipeline.mtx.
//
// Usage:
//
//	mclc compile -skill <path> -prose "user's goal" [-verb V] [-grammar G]
//	mclc validate <path>             # validate a .mtx file
//	mclc hash <path>                 # print canonical AST hash (D11 mtx_digest)
//	mclc parse <path>                # parse and dump AST summary
//	mclc fmt <path>                  # (future) canonical formatting
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"matrix/mcl/ir"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/canonical"
	"matrix/mcl/mtx/interpreter"
	"matrix/mcl/mtx/parser"
	"matrix/mcl/mtx/validator"
)

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(1)
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	switch cmd {
	case "compile":
		cmdCompile(args)
	case "validate":
		cmdValidate(args)
	case "hash":
		cmdHash(args)
	case "parse":
		cmdParse(args)
	case "help", "-h", "--help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "mclc: unknown command %q\n", cmd)
		usage()
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprintf(os.Stderr, `mclc — MatrixScript compiler

Commands:
  compile   Compile a SKILL.mtx against user prose → Intent IR
  validate  Validate a .mtx file against spec §11 rules
  hash      Print canonical AST hash (D11 mtx_digest)
  parse     Parse a .mtx file and print AST summary

Compile flags:
  -skill <path>    Path to SKILL.mtx
  -prose "text"    User's natural language goal
  -verb <verb>     Pre-classified verb (skips stage 2)
  -grammar <id>    Grammar constraint ID (default: intent_frame@1)
  -confidence <f>  Current confidence (default: 1.0)
  -model <model>   Model string (default: fireworks deepseek-v4-flash)
  -seed <int>      Seed for D11 determinism (default: 42)
  -dry-run         Don't call LLM, just show interpolated prompts

Environment:
  FIREWORKS_API_KEY   API key for Fireworks AI
  TOGETHER_API_KEY    API key for Together AI
`)
}

// ---- compile ----

func cmdCompile(args []string) {
	var (
		skillPath  string
		prose      string
		verb       string
		grammar          = "intent_frame@1"
		confidence       = 1.0
		model            = ""
		seed       int64 = 42
		dryRun           = false
		slotValues       = map[string]string{}
	)

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-skill":
			i++
			if i < len(args) {
				skillPath = args[i]
			}
		case "-prose":
			i++
			if i < len(args) {
				prose = args[i]
			}
		case "-verb":
			i++
			if i < len(args) {
				verb = args[i]
			}
		case "-grammar":
			i++
			if i < len(args) {
				grammar = args[i]
			}
		case "-confidence":
			i++
			if i < len(args) {
				if _, err := fmt.Sscanf(args[i], "%f", &confidence); err != nil {
					fmt.Fprintf(os.Stderr, "mclc compile: bad -confidence %q: %v\n", args[i], err)
					os.Exit(1)
				}
			}
		case "-model":
			i++
			if i < len(args) {
				model = args[i]
			}
		case "-seed":
			i++
			if i < len(args) {
				if _, err := fmt.Sscanf(args[i], "%d", &seed); err != nil {
					fmt.Fprintf(os.Stderr, "mclc compile: bad -seed %q: %v\n", args[i], err)
					os.Exit(1)
				}
			}
		case "-dry-run":
			dryRun = true
		default:
			// slot=value pairs
			if strings.Contains(args[i], "=") {
				parts := strings.SplitN(args[i], "=", 2)
				slotValues[parts[0]] = parts[1]
			}
		}
	}

	if skillPath == "" {
		fmt.Fprintln(os.Stderr, "mclc compile: -skill required")
		os.Exit(1)
	}
	if prose == "" {
		fmt.Fprintln(os.Stderr, "mclc compile: -prose required")
		os.Exit(1)
	}

	// Read and parse SKILL.mtx
	src, err := os.ReadFile(skillPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "mclc compile: read %s: %v\n", skillPath, err)
		os.Exit(1)
	}

	p := parser.New(src)
	file, errs := p.Parse()
	if len(errs) > 0 {
		for _, e := range errs {
			fmt.Fprintf(os.Stderr, "  parse: %s\n", e)
		}
		os.Exit(1)
	}

	// Validate
	verrs := validator.ValidateSkill(file)
	if len(verrs) > 0 {
		for _, e := range verrs {
			fmt.Fprintf(os.Stderr, "  validate: %s\n", e)
		}
		os.Exit(1)
	}

	// Compute mtx_digest
	digest := canonical.Hash(file)

	// Set up interpreter
	var llmClient interpreter.LLM
	if !dryRun {
		cfg := llm.DefaultCompilerModel()
		if model != "" {
			cfg.Model = model
		}
		cfg.Seed = seed
		client, err := llm.New(&cfg)
		if err != nil {
			// Fall back to dry-run if no API key available
			fmt.Fprintf(os.Stderr, "mclc compile: LLM unavailable (%v), running in dry-run mode\n", err)
		} else {
			llmClient = client
		}
	}

	interp := interpreter.New(file, llmClient, nil)

	result, err := interp.Run(context.Background(), &interpreter.RunInput{
		Prose:      prose,
		Verb:       verb,
		Grammar:    grammar,
		Confidence: confidence,
		SlotValues: slotValues,
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "mclc compile: %v\n", err)
		os.Exit(1)
	}

	// Build output
	output := compileOutput{
		MtxDigest:        digest,
		MatchedCondition: result.MatchedCondition,
		Executed:         result.Executed,
	}

	if len(result.PromptMessages) > 0 {
		for _, m := range result.PromptMessages {
			output.PromptMessages = append(output.PromptMessages, promptMsg{
				Role:    m.Role,
				Content: m.Content,
			})
		}
	}

	if result.FrameJSON != "" {
		output.FrameJSON = result.FrameJSON
	}

	for name, slot := range result.Slots {
		output.Slots = append(output.Slots, slotOut{
			Name:   name,
			Value:  slot.Value,
			Status: statusName(slot.Status),
			Type:   slot.TypeName,
		})
	}

	for _, u := range result.Unknowns {
		output.Unknowns = append(output.Unknowns, unknownOut{
			SlotName: u.SlotName,
			Severity: u.Severity,
			Reason:   u.Reason,
		})
	}

	for _, q := range result.ClarifyQuestions {
		output.ClarifyQuestions = append(output.ClarifyQuestions, clarifyOut{
			SlotName: q.SlotName,
			Prompt:   q.Prompt,
			Type:     q.TypeName,
			Required: q.Required,
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(output); err != nil {
		fmt.Fprintf(os.Stderr, "mclc compile: encode output: %v\n", err)
		os.Exit(1)
	}
}

type compileOutput struct {
	MtxDigest        string       `json:"mtx_digest"`
	MatchedCondition string       `json:"matched_condition"`
	Executed         bool         `json:"executed"`
	FrameJSON        string       `json:"frame_json,omitempty"`
	PromptMessages   []promptMsg  `json:"prompt_messages,omitempty"`
	Slots            []slotOut    `json:"slots,omitempty"`
	Unknowns         []unknownOut `json:"unknowns,omitempty"`
	ClarifyQuestions []clarifyOut `json:"clarify_questions,omitempty"`
}

type promptMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type slotOut struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Status string `json:"status"`
	Type   string `json:"type"`
}

type unknownOut struct {
	SlotName string `json:"slot_name"`
	Severity string `json:"severity"`
	Reason   string `json:"reason"`
}

type clarifyOut struct {
	SlotName string `json:"slot_name"`
	Prompt   string `json:"prompt"`
	Type     string `json:"type"`
	Required bool   `json:"required"`
}

func statusName(s interpreter.SlotStatus) string {
	switch s {
	case interpreter.SlotEmpty:
		return "empty"
	case interpreter.SlotRaw:
		return "raw"
	case interpreter.SlotResolved:
		return "resolved"
	case interpreter.SlotDefault:
		return "default"
	default:
		return "unknown"
	}
}

// ---- validate ----

func cmdValidate(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "mclc validate: path required")
		os.Exit(1)
	}

	for _, path := range args {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}

		p := parser.New(src)
		file, perrs := p.Parse()
		if len(perrs) > 0 {
			for _, e := range perrs {
				fmt.Fprintf(os.Stderr, "%s: parse: %s\n", path, e)
			}
			os.Exit(1)
		}

		// Determine if this is a SKILL.mtx or core .mtx
		isSkill := false
		for _, sec := range file.Sections {
			if sec.Name == "SKILL" {
				isSkill = true
				break
			}
		}

		var verrs []*validator.Error
		if isSkill {
			verrs = validator.ValidateSkill(file)
		} else {
			verrs = validator.ValidateCore(file)
		}

		if len(verrs) > 0 {
			for _, e := range verrs {
				fmt.Fprintf(os.Stderr, "%s: %s\n", path, e)
			}
			os.Exit(1)
		}

		fmt.Printf("%s: ok\n", path)
	}
}

// ---- hash ----

func cmdHash(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "mclc hash: path required")
		os.Exit(1)
	}

	for _, path := range args {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}

		p := parser.New(src)
		file, perrs := p.Parse()
		if len(perrs) > 0 {
			for _, e := range perrs {
				fmt.Fprintf(os.Stderr, "%s: parse: %s\n", path, e)
			}
			os.Exit(1)
		}

		digest := canonical.Hash(file)
		fmt.Printf("%s  %s\n", digest, path)
	}
}

// ---- parse ----

func cmdParse(args []string) {
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "mclc parse: path required")
		os.Exit(1)
	}

	for _, path := range args {
		src, err := os.ReadFile(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", path, err)
			os.Exit(1)
		}

		p := parser.New(src)
		file, perrs := p.Parse()
		if len(perrs) > 0 {
			for _, e := range perrs {
				fmt.Fprintf(os.Stderr, "%s: parse: %s\n", path, e)
			}
			os.Exit(1)
		}

		fmt.Printf("%s: %d sections\n", path, len(file.Sections))
		for _, sec := range file.Sections {
			fmt.Printf("  §%s: %d entries\n", sec.Name, len(sec.Entries))
		}
	}
}

// Ensure ir package is linked (used in compile output types)
var _ = ir.VerbBuild

// Copyright © 2026 Paxlabs Inc. All rights reserved.
