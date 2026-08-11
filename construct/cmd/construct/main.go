// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Command construct is the Construct schema toolchain.
//
// Usage:
//
//	construct codegen [--out PATH] [--check]   emit (or drift-check) the client TS
//	construct validate [--file PATH]           validate a JSON surface (stdin if no file)
//	construct version                          print the schema version
//
// codegen writes the TypeScript mirror of the Go schema into the client repo;
// --check fails (exit 2) if the on-disk file differs from a fresh generation,
// for CI drift detection. validate parses a surface JSON and reports whether
// it satisfies the schema contract.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"matrix/construct/internal/codegen"
	"matrix/construct/schema"
)

// schemaVersion mirrors construct.frozen.kvx [meta].version.
const schemaVersion = "0.1.0"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	switch os.Args[1] {
	case "codegen":
		os.Exit(runCodegen(os.Args[2:]))
	case "validate":
		os.Exit(runValidate(os.Args[2:]))
	case "version":
		fmt.Println(schemaVersion)
	case "-h", "--help", "help":
		usage()
	default:
		fmt.Fprintf(os.Stderr, "construct: unknown command %q\n\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `construct — the Construct schema toolchain

Usage:
  construct codegen [--out PATH] [--check]   emit (or drift-check) the client TS
  construct validate [--file PATH]           validate a JSON surface (stdin if no file)
  construct version                          print the schema version
`)
}

// runCodegen emits or drift-checks the generated TypeScript.
func runCodegen(args []string) int {
	out := codegen.DefaultOutPath
	check := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--check", "-check":
			check = true
		case "--out", "-out":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "construct: --out requires a path")
				return 2
			}
			i++
			out = args[i]
		default:
			fmt.Fprintf(os.Stderr, "construct: unknown codegen flag %q\n", args[i])
			return 2
		}
	}

	gen, err := codegen.Generate()
	if err != nil {
		fmt.Fprintf(os.Stderr, "construct: codegen: %v\n", err)
		return 1
	}

	if check {
		have, err := os.ReadFile(out)
		if err != nil {
			fmt.Fprintf(os.Stderr, "construct: codegen --check: read %s: %v\n", out, err)
			return 2
		}
		if string(have) != gen {
			fmt.Fprintf(os.Stderr, "construct: codegen drift — %s is stale; run `construct codegen`\n", out)
			return 2
		}
		fmt.Printf("construct: codegen up to date (%s)\n", out)
		return 0
	}

	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "construct: codegen: mkdir %s: %v\n", filepath.Dir(out), err)
		return 1
	}
	if err := os.WriteFile(out, []byte(gen), 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "construct: codegen: write %s: %v\n", out, err)
		return 1
	}
	fmt.Printf("construct: wrote %s\n", out)
	return 0
}

// runValidate validates a surface JSON read from --file or stdin.
func runValidate(args []string) int {
	file := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--file", "-file":
			if i+1 >= len(args) {
				fmt.Fprintln(os.Stderr, "construct: --file requires a path")
				return 2
			}
			i++
			file = args[i]
		default:
			fmt.Fprintf(os.Stderr, "construct: unknown validate flag %q\n", args[i])
			return 2
		}
	}

	var (
		data []byte
		err  error
	)
	if file != "" {
		data, err = os.ReadFile(file)
	} else {
		data, err = io.ReadAll(os.Stdin)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "construct: validate: read: %v\n", err)
		return 1
	}

	s, err := schema.ParseValid(data)
	if err != nil {
		fmt.Fprintf(os.Stderr, "construct: invalid surface: %v\n", err)
		return 1
	}
	fmt.Printf("construct: valid %s surface %q\n", s.Kind, s.ID)
	return 0
}
