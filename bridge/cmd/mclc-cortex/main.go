// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command mclc-cortex is the bridged compiler: mclc against a live cortex.
//
// It mirrors `mclc compile` from matrix/mcl/cmd/mclc but additionally wires
// a Pebble-backed *cortex.Cortex via the bridge package so that on-block
// resolve statements (`cortex.find`, `cortex.resolve`, `cortex.context`) run
// against a real memory graph instead of being dry-run no-ops.
//
// Usage:
//
//	mclc-cortex -skill <SKILL.mtx> -prose "user goal" [flags]
//
// Flags mirror mclc compile plus cortex-specific knobs:
//
//	-skill <path>         Path to SKILL.mtx
//	-prose "text"         User natural language goal
//	-verb <verb>          Pre-classified verb (skips stage 2)
//	-grammar <id>         Grammar constraint ID (default: intent_frame@1)
//	-confidence <f>       Current confidence (default: 1.0)
//	-model <model>        LLM model id (overrides DefaultCompilerModel)
//	-seed <int>           D11 deterministic seed (default: 42)
//	-dry-run              Don't call LLM; collect interpolated prompts only
//	-cortex-root <dir>    Cortex data directory (defaults to ./.matrix-cortex)
//	-actor <name>         Cortex actor name (default: andrew)
//	-with-embedder        Start a hash-stub embedder so cortex.find(near=...)
//	                       can run. Use -with-fireworks-embedder for a real
//	                       768-dim nomic-embed-text-v1.5 client (requires
//	                       FIREWORKS_API_KEY).
//	-with-fireworks-embedder
//	                       Start the real APIEmbedder against Fireworks
//	                       /v1/embeddings; falls back to hash-stub on error.
//
// Environment:
//
//	FIREWORKS_API_KEY     Fireworks LLM + embedder
//	TOGETHER_API_KEY      Together LLM
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"matrix/bridge"
	"matrix/cortex"
	"matrix/cortex/embed"
	"matrix/cortex/store"
	"matrix/mcl/llm"
	"matrix/mcl/mtx/canonical"
	"matrix/mcl/mtx/interpreter"
	"matrix/mcl/mtx/parser"
	"matrix/mcl/mtx/validator"
)

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "mclc-cortex:", err)
		os.Exit(1)
	}
}

type flags struct {
	skillPath             string
	prose                 string
	verb                  string
	grammar               string
	confidence            float64
	model                 string
	seed                  int64
	dryRun                bool
	cortexRoot            string
	actor                 string
	withEmbedder          bool
	withFireworksEmbedder bool
	slotValues            map[string]string
}

func run(args []string) error {
	f := flags{
		grammar:    "intent_frame@1",
		confidence: 1.0,
		seed:       42,
		cortexRoot: ".matrix-cortex",
		actor:      "andrew",
		slotValues: map[string]string{},
	}

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-skill":
			i++
			if i < len(args) {
				f.skillPath = args[i]
			}
		case "-prose":
			i++
			if i < len(args) {
				f.prose = args[i]
			}
		case "-verb":
			i++
			if i < len(args) {
				f.verb = args[i]
			}
		case "-grammar":
			i++
			if i < len(args) {
				f.grammar = args[i]
			}
		case "-confidence":
			i++
			if i < len(args) {
				if _, err := fmt.Sscanf(args[i], "%f", &f.confidence); err != nil {
					return fmt.Errorf("bad -confidence %q: %w", args[i], err)
				}
			}
		case "-model":
			i++
			if i < len(args) {
				f.model = args[i]
			}
		case "-seed":
			i++
			if i < len(args) {
				if _, err := fmt.Sscanf(args[i], "%d", &f.seed); err != nil {
					return fmt.Errorf("bad -seed %q: %w", args[i], err)
				}
			}
		case "-dry-run":
			f.dryRun = true
		case "-cortex-root":
			i++
			if i < len(args) {
				f.cortexRoot = args[i]
			}
		case "-actor":
			i++
			if i < len(args) {
				f.actor = args[i]
			}
		case "-with-embedder":
			f.withEmbedder = true
		case "-with-fireworks-embedder":
			f.withFireworksEmbedder = true
		case "-h", "--help", "help":
			usage()
			return nil
		default:
			if strings.Contains(args[i], "=") {
				parts := strings.SplitN(args[i], "=", 2)
				f.slotValues[parts[0]] = parts[1]
			}
		}
	}

	if f.skillPath == "" {
		usage()
		return fmt.Errorf("-skill required")
	}
	if f.prose == "" {
		usage()
		return fmt.Errorf("-prose required")
	}

	// Parse + validate SKILL.mtx.
	src, err := os.ReadFile(f.skillPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", f.skillPath, err)
	}
	file, perrs := parser.New(src).Parse()
	if len(perrs) > 0 {
		for _, e := range perrs {
			fmt.Fprintf(os.Stderr, "  parse: %s\n", e)
		}
		return fmt.Errorf("parse failed")
	}
	if verrs := validator.ValidateSkill(file); len(verrs) > 0 {
		for _, e := range verrs {
			fmt.Fprintf(os.Stderr, "  validate: %s\n", e)
		}
		return fmt.Errorf("validation failed")
	}

	// Open cortex.
	if err := os.MkdirAll(f.cortexRoot, 0o755); err != nil {
		return fmt.Errorf("mkdir cortex-root: %w", err)
	}
	s, err := store.Open(f.cortexRoot, f.actor, nil)
	if err != nil {
		return fmt.Errorf("store.Open: %w", err)
	}
	defer s.Close()
	c := cortex.New(s)

	// Start embedder if requested.
	if f.withFireworksEmbedder || f.withEmbedder {
		if err := startEmbedder(c, f.withFireworksEmbedder); err != nil {
			fmt.Fprintf(os.Stderr, "embedder: %v (continuing without)\n", err)
		} else {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := c.DrainEmbedder(ctx); err != nil {
				fmt.Fprintf(os.Stderr, "embedder drain: %v\n", err)
			}
			defer func() {
				if err := c.StopEmbedder(); err != nil {
					fmt.Fprintf(os.Stderr, "embedder stop: %v\n", err)
				}
			}()
		}
	}

	// LLM client (optional).
	var llmClient interpreter.LLM
	if !f.dryRun {
		cfg := llm.DefaultCompilerModel()
		if f.model != "" {
			cfg.Model = f.model
		}
		cfg.Seed = f.seed
		client, err := llm.New(&cfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "LLM unavailable (%v); running dry-run\n", err)
		} else {
			llmClient = client
		}
	}

	// Cortex bridge.
	cortexAdapter := bridge.New(c)

	// Compute mtx_digest for audit.
	digest := canonical.Hash(file)

	// Run the interpreter.
	interp := interpreter.New(file, llmClient, cortexAdapter)
	result, err := interp.Run(context.Background(), &interpreter.RunInput{
		Prose:      f.prose,
		Verb:       f.verb,
		Grammar:    f.grammar,
		Confidence: f.confidence,
		SlotValues: f.slotValues,
	})
	if err != nil {
		return fmt.Errorf("interpret: %w", err)
	}

	// Emit JSON output mirroring mclc compile's shape.
	rootBytes, rootErr := c.OverallRoot()
	rootHex := ""
	if rootErr == nil {
		rootHex = fmt.Sprintf("%x", rootBytes)
	}
	out := compileOutput{
		MtxDigest:        digest,
		MatchedCondition: result.MatchedCondition,
		Executed:         result.Executed,
		OverallRoot:      rootHex,
		FrameJSON:        result.FrameJSON,
	}
	for _, m := range result.PromptMessages {
		out.PromptMessages = append(out.PromptMessages, promptMsg{Role: m.Role, Content: m.Content})
	}
	for name, slot := range result.Slots {
		out.Slots = append(out.Slots, slotOut{
			Name:   name,
			Value:  slot.Value,
			Status: statusName(slot.Status),
			Type:   slot.TypeName,
		})
	}
	for _, u := range result.Unknowns {
		out.Unknowns = append(out.Unknowns, unknownOut{
			SlotName: u.SlotName,
			Severity: u.Severity,
			Reason:   u.Reason,
		})
	}
	for _, q := range result.ClarifyQuestions {
		out.ClarifyQuestions = append(out.ClarifyQuestions, clarifyOut{
			SlotName: q.SlotName,
			Prompt:   q.Prompt,
			Type:     q.TypeName,
			Required: q.Required,
		})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func startEmbedder(c *cortex.Cortex, useFireworks bool) error {
	var emb embed.Embedder
	if useFireworks {
		client, err := embed.NewAPIEmbedder(embed.APIEmbedderConfig{})
		if err != nil {
			fmt.Fprintf(os.Stderr, "fireworks embedder unavailable (%v); falling back to hash-stub\n", err)
			emb = embed.NewHashEmbedder()
		} else {
			emb = client
		}
	} else {
		emb = embed.NewHashEmbedder()
	}
	return c.StartEmbedder(cortex.EmbedderOptions{Embedder: emb})
}

// ----- JSON output shapes (mirror mclc compile) ---------------------------

type compileOutput struct {
	MtxDigest        string       `json:"mtx_digest"`
	MatchedCondition string       `json:"matched_condition"`
	Executed         bool         `json:"executed"`
	OverallRoot      string       `json:"cortex_overall_root"`
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

func usage() {
	fmt.Fprintf(os.Stderr, `mclc-cortex — MatrixScript compiler bridged to a live cortex

Usage:
  mclc-cortex -skill <SKILL.mtx> -prose "user goal" [flags]

Flags:
  -skill <path>              Path to SKILL.mtx
  -prose "text"              User natural-language goal
  -verb <verb>               Pre-classified verb (skips stage 2)
  -grammar <id>              Grammar constraint ID (default: intent_frame@1)
  -confidence <f>            Current confidence (default: 1.0)
  -model <model>             LLM model id (overrides DefaultCompilerModel)
  -seed <int>                D11 deterministic seed (default: 42)
  -dry-run                   Don't call LLM; collect interpolated prompts only
  -cortex-root <dir>         Cortex data dir (default: ./.matrix-cortex)
  -actor <name>              Cortex actor name (default: andrew)
  -with-embedder             Start hash-stub embedder for cortex.find(near=...)
  -with-fireworks-embedder   Start real Fireworks nomic embedder (req. FIREWORKS_API_KEY)
  slot=value                 Pre-fill named slots

Environment:
  FIREWORKS_API_KEY          Fireworks LLM + embedder
  TOGETHER_API_KEY           Together LLM
`)
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
