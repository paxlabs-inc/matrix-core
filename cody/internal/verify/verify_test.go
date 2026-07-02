// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package verify

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func seed(t *testing.T, root, rel, content string) {
	t.Helper()
	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestDetectGoProject(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "go.mod", "module example.com/demo\n\ngo 1.21\n")
	cmds, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Kind{
		"go build ./...": KindBuild,
		"go vet ./...":   KindLint,
		"go test ./...":  KindTest,
	}
	if len(cmds) != len(want) {
		t.Fatalf("Detect() = %v", cmds)
	}
	for _, c := range cmds {
		if want[c.Cmd] != c.Kind {
			t.Fatalf("command %q kind = %s, want %s", c.Cmd, c.Kind, want[c.Cmd])
		}
	}
}

func TestDetectNodeProject(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "package.json", `{"scripts":{"build":"tsc -b","test":"vitest run","lint":"eslint .","typecheck":"tsc --noEmit"}}`)
	cmds, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	var flat []string
	for _, c := range cmds {
		flat = append(flat, string(c.Kind)+":"+c.Cmd)
	}
	joined := strings.Join(flat, " | ")
	for _, want := range []string{
		"typecheck:npm run typecheck", "lint:npm run lint",
		"build:npm run build", "test:npm run test",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Detect() = %v, want %s", joined, want)
		}
	}
	// pnpm lockfile flips the runner.
	seed(t, root, "pnpm-lock.yaml", "lockfileVersion: 9\n")
	cmds, err = Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cmds[0].Cmd, "pnpm run ") {
		t.Fatalf("pnpm project should use pnpm run: %v", cmds)
	}
}

func TestDetectMakefileFallback(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "Makefile", "build:\n\ttrue\n\ntest:\n\ttrue\n\nlint:\n\ttrue\n\n.PHONY: build test lint\n")
	cmds, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, c := range cmds {
		joined += c.Cmd + " "
	}
	for _, want := range []string{"make lint", "make build", "make test"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("Detect() = %q, want %s", joined, want)
		}
	}
}

func TestRunRealGoProject(t *testing.T) {
	root := t.TempDir()
	seed(t, root, "go.mod", "module example.com/demo\n\ngo 1.21\n")
	seed(t, root, "greet.go", "package demo\n\n// Greet greets.\nfunc Greet(name string) string { return \"hello \" + name }\n")
	seed(t, root, "greet_test.go", `package demo

import "testing"

func TestGreet(t *testing.T) {
	if Greet("cody") != "hello cody" {
		t.Fatal("wrong greeting")
	}
}
`)
	r, err := NewRunner(root)
	if err != nil {
		t.Fatal(err)
	}
	cmds, err := Detect(root)
	if err != nil {
		t.Fatal(err)
	}
	results, err := r.Run(context.Background(), cmds)
	if err != nil {
		t.Fatal(err)
	}
	if !AllGreen(results) {
		for _, res := range results {
			t.Logf("%s -> exit %d\n%s", res.Command.Cmd, res.Exit, res.Output)
		}
		t.Fatal("healthy project not green")
	}
	// Break the code: verification must go red with the real failure output.
	seed(t, root, "greet.go", "package demo\n\nfunc Greet(name string) string { return \"goodbye \" + name }\n")
	results, err = r.Run(context.Background(), cmds)
	if err != nil {
		t.Fatal(err)
	}
	if AllGreen(results) {
		t.Fatal("broken project reported green")
	}
	var testRes *Result
	for i := range results {
		if results[i].Command.Kind == KindTest {
			testRes = &results[i]
		}
	}
	if testRes == nil || testRes.Green || testRes.Exit == 0 {
		t.Fatalf("test result = %+v, want red", testRes)
	}
	if !strings.Contains(testRes.Output, "wrong greeting") && !strings.Contains(testRes.Output, "FAIL") {
		t.Fatalf("real failure output missing: %q", testRes.Output)
	}
}

func TestAllGreenEmptyIsNotGreen(t *testing.T) {
	if AllGreen(nil) {
		t.Fatal("empty result set must not be green — absence of failure is not success")
	}
}

func TestOverflowSpill(t *testing.T) {
	root := t.TempDir()
	r, err := NewRunner(root)
	if err != nil {
		t.Fatal(err)
	}
	r.ExcerptLimit = 256
	results, err := r.Run(context.Background(), FromStrings([]string{`i=0; while [ $i -lt 200 ]; do echo "line $i padding padding padding"; i=$((i+1)); done`}))
	if err != nil {
		t.Fatal(err)
	}
	res := results[0]
	if !res.Green {
		t.Fatalf("loop exited %d", res.Exit)
	}
	if res.OverflowPath == "" {
		t.Fatal("oversized output did not spill to an overflow file")
	}
	full, err := os.ReadFile(res.OverflowPath)
	if err != nil {
		t.Fatalf("overflow file unreadable: %v", err)
	}
	if !strings.Contains(string(full), "line 199") {
		t.Fatal("overflow file does not carry the full output")
	}
	if !strings.Contains(res.Output, res.OverflowPath) {
		t.Fatal("excerpt does not point at the overflow file")
	}
	if !strings.Contains(res.Output, "truncated") {
		t.Fatalf("excerpt missing truncation marker: %q", res.Output)
	}
}

func TestTimeout(t *testing.T) {
	root := t.TempDir()
	r, err := NewRunner(root)
	if err != nil {
		t.Fatal(err)
	}
	r.Timeout = 300 * time.Millisecond
	results, err := r.Run(context.Background(), FromStrings([]string{"sleep 5"}))
	if err != nil {
		t.Fatal(err)
	}
	res := results[0]
	if res.Green || !res.TimedOut {
		t.Fatalf("timed-out command reported %+v", res)
	}
}

func TestFromStrings(t *testing.T) {
	cmds := FromStrings([]string{" go test ./... ", "", "make lint"})
	if len(cmds) != 2 || cmds[0].Cmd != "go test ./..." || cmds[1].Cmd != "make lint" {
		t.Fatalf("FromStrings() = %v", cmds)
	}
}
