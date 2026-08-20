// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package agent

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"centra/agents/neo/internal/config"
	"centra/agents/neo/internal/llm"
	"centra/agents/neo/internal/tools"
)

// This file proves PROPERTY 4 (task 6.4): the proof-of-work completion gate is
// fully RETIRED from Neo with no collateral. Real code paths only:
//
//   - task_complete is absent from Neo's advertised schema set (both the
//     top-level and sub-agent surfaces);
//   - a work-touched bare-answer turn finishes WITHOUT a gate;
//   - the neo module source does not import centra/core/cassandra;
//   - (build/vet/test green and cody + executor unchanged are proven by the
//     module-level `go build/vet/test ./...`, which this package is part of).

// TestTaskCompleteAbsentFromSchemas asserts the retired synthetic tool is gone
// from every advertised surface, so the model can no longer end a turn via a
// task_complete object.
func TestTaskCompleteAbsentFromSchemas(t *testing.T) {
	mgr := &tools.Manager{}
	for _, set := range [][]llm.Tool{mgr.Schemas(), mgr.SubagentSchemas()} {
		for _, tl := range set {
			if strings.Contains(strings.ToLower(tl.Function.Name), "task_complete") {
				t.Fatalf("task_complete must be retired from the advertised schema set, found %q", tl.Function.Name)
			}
		}
	}
	// The full agent surface (Schemas + read_overflow) must also be clean.
	a := New(Options{Config: config.Default(), Tools: mgr})
	for _, tl := range a.schemas {
		if strings.Contains(strings.ToLower(tl.Function.Name), "task_complete") {
			t.Fatalf("task_complete leaked into the agent schema set: %q", tl.Function.Name)
		}
	}
}

// TestWorkTouchedBareAnswerFinishesWithoutGate drives the REAL loop: the model
// does real tool work, then gives a bare final answer. With the gate retired this
// must FINISH (nil error) — no task_complete object, no adjudication, no strict
// routing.
func TestWorkTouchedBareAnswerFinishesWithoutGate(t *testing.T) {
	steps := []scriptStep{
		{content: "Reading the file to answer.", toolName: "fs__read_file", toolArgs: `{"path":"/x"}`},
		{content: "Based on the file, the answer is 42."}, // bare-answer close after real work
	}
	a, err := runScripted(t, steps, true, "read the file and tell me the value")
	if err != nil {
		t.Fatalf("a work-touched bare answer must finish without a gate, got: %v", err)
	}
	if len(a.turn.casRecord) != 0 {
		t.Errorf("a clean two-step answer must not be modded, got %d mods", len(a.turn.casRecord))
	}
}

// TestNeoDoesNotImportMatrixCassandra walks the whole neo module source and
// asserts NO Go file imports centra/core/cassandra — the 2.0 controller severed that
// dependency (req.1.4). A comment mentioning the retired package is fine; an
// actual import is the failure.
func TestNeoDoesNotImportMatrixCassandra(t *testing.T) {
	// Walk up from this package to the neo module root (the dir holding go.mod).
	root, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(root, "go.mod")); statErr == nil {
			break
		}
		parent := filepath.Dir(root)
		if parent == root {
			t.Fatal("could not locate the neo module root (go.mod)")
		}
		root = parent
	}

	fset := token.NewFileSet()
	var offenders []string
	walkErr := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == "vendor" || info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			return nil // unparseable non-source (fixtures/testdata) — not our concern
		}
		for _, imp := range f.Imports {
			p := strings.Trim(imp.Path.Value, `"`)
			if p == "centra/core/cassandra" || strings.HasPrefix(p, "centra/core/cassandra/") {
				rel, _ := filepath.Rel(root, path)
				offenders = append(offenders, rel)
			}
		}
		return nil
	})
	if walkErr != nil {
		t.Fatal(walkErr)
	}
	if len(offenders) != 0 {
		t.Fatalf("neo must not import centra/core/cassandra (Cassandra 2.0 severed it); offenders: %v", offenders)
	}
}
