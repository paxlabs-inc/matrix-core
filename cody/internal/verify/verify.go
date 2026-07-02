// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package verify detects and runs a project's OWN verification commands —
// build, test, lint, typecheck from package.json scripts, Makefile, go.mod,
// Cargo.toml, pytest — as first-class loop steps with structured results.
// The project's own truth is the only truth: task acceptance is gated on
// these commands being green, and oversized output spills to an overflow
// file so the full content is always retrievable (read-full discipline).
package verify

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// Kind classifies what a verification command proves.
type Kind string

const (
	KindBuild     Kind = "build"
	KindTest      Kind = "test"
	KindLint      Kind = "lint"
	KindTypecheck Kind = "typecheck"
	KindCustom    Kind = "custom"
)

// Command is one runnable verification step.
type Command struct {
	Kind Kind   `json:"kind"`
	Cmd  string `json:"cmd"` // executed via sh -c in the workspace root
}

// Result is the structured outcome of one verification run: real exit code,
// real output (excerpted, with the full copy on disk when oversized).
type Result struct {
	Command      Command       `json:"command"`
	Exit         int           `json:"exit"`
	Green        bool          `json:"green"`
	Output       string        `json:"output"`
	OverflowPath string        `json:"overflow_path,omitempty"`
	Duration     time.Duration `json:"duration"`
	TimedOut     bool          `json:"timed_out,omitempty"`
}

// DefaultTimeout bounds a single verification command.
const DefaultTimeout = 10 * time.Minute

// defaultExcerptLimit is the inline output cap before spilling to overflow.
const defaultExcerptLimit = 16 * 1024

// Runner executes verification commands in a workspace.
type Runner struct {
	Root string
	// Timeout per command; DefaultTimeout when zero.
	Timeout time.Duration
	// ExcerptLimit caps inline output bytes; defaultExcerptLimit when zero.
	ExcerptLimit int
	// OverflowDir receives full outputs that exceed the excerpt limit.
	// Defaults to <Root>/.cody/overflow.
	OverflowDir string
}

// NewRunner creates a runner for a workspace root.
func NewRunner(root string) (*Runner, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("verify runner root %q is not a directory", root)
	}
	return &Runner{Root: abs}, nil
}

// Detect discovers the project's own verification commands under root, in
// deterministic order (typecheck, lint, build, test per ecosystem).
func Detect(root string) ([]Command, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	var cmds []Command
	add := func(kind Kind, cmd string) {
		for _, c := range cmds {
			if c.Cmd == cmd {
				return
			}
		}
		cmds = append(cmds, Command{Kind: kind, Cmd: cmd})
	}

	if scripts := readScripts(filepath.Join(abs, "package.json")); len(scripts) > 0 {
		runner := "npm run"
		if _, err := os.Stat(filepath.Join(abs, "pnpm-lock.yaml")); err == nil {
			runner = "pnpm run"
		}
		ordered := []struct {
			name string
			kind Kind
		}{
			{"typecheck", KindTypecheck}, {"tsc", KindTypecheck},
			{"lint", KindLint},
			{"build", KindBuild},
			{"test", KindTest},
		}
		for _, o := range ordered {
			if _, ok := scripts[o.name]; ok {
				add(o.kind, runner+" "+o.name)
			}
		}
	}
	if exists(filepath.Join(abs, "go.mod")) {
		add(KindBuild, "go build ./...")
		add(KindLint, "go vet ./...")
		add(KindTest, "go test ./...")
	}
	if exists(filepath.Join(abs, "Cargo.toml")) {
		add(KindBuild, "cargo build --quiet")
		add(KindTest, "cargo test --quiet")
	}
	if exists(filepath.Join(abs, "pyproject.toml")) || exists(filepath.Join(abs, "pytest.ini")) || exists(filepath.Join(abs, "setup.py")) {
		add(KindTest, "pytest -q")
	}
	if len(cmds) == 0 {
		if targets := makefileTargets(filepath.Join(abs, "Makefile")); len(targets) > 0 {
			for _, name := range []string{"lint", "build", "test", "check"} {
				if targets[name] {
					add(kindOfTarget(name), "make "+name)
				}
			}
		}
	}
	return cmds, nil
}

func kindOfTarget(name string) Kind {
	switch name {
	case "build":
		return KindBuild
	case "test", "check":
		return KindTest
	case "lint":
		return KindLint
	}
	return KindCustom
}

// Run executes the commands sequentially in the workspace root, stopping at
// nothing: every command runs so the caller sees the full verification
// picture, not just the first failure.
func (r *Runner) Run(ctx context.Context, cmds []Command) ([]Result, error) {
	results := make([]Result, 0, len(cmds))
	for _, c := range cmds {
		res, err := r.runOne(ctx, c)
		if err != nil {
			return results, err
		}
		results = append(results, res)
	}
	return results, nil
}

// AllGreen reports whether every result passed. An empty set is NOT green.
func AllGreen(results []Result) bool {
	if len(results) == 0 {
		return false
	}
	for _, res := range results {
		if !res.Green {
			return false
		}
	}
	return true
}

func (r *Runner) runOne(ctx context.Context, c Command) (Result, error) {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, "sh", "-c", c.Cmd)
	cmd.Dir = r.Root
	start := time.Now()
	out, err := cmd.CombinedOutput()
	res := Result{Command: c, Duration: time.Since(start)}
	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
	}
	switch {
	case err == nil:
		res.Exit = 0
		res.Green = true
	default:
		if ee, ok := err.(*exec.ExitError); ok {
			res.Exit = ee.ExitCode()
		} else {
			res.Exit = -1
			out = append(out, []byte("\nrunner error: "+err.Error())...)
		}
	}
	if res.TimedOut {
		res.Green = false
		if res.Exit == 0 {
			res.Exit = -1
		}
	}
	res.Output, res.OverflowPath = r.excerpt(c, out)
	return res, nil
}

// excerpt truncates oversized output head+tail and spills the full bytes to
// an overflow file, so nothing is ever lost to truncation.
func (r *Runner) excerpt(c Command, out []byte) (string, string) {
	limit := r.ExcerptLimit
	if limit <= 0 {
		limit = defaultExcerptLimit
	}
	if len(out) <= limit {
		return string(out), ""
	}
	dir := r.OverflowDir
	if dir == "" {
		dir = filepath.Join(r.Root, ".cody", "overflow")
	}
	overflow := ""
	if err := os.MkdirAll(dir, 0o755); err == nil {
		sum := sha256.Sum256([]byte(c.Cmd + time.Now().String()))
		overflow = filepath.Join(dir, "verify-"+hex.EncodeToString(sum[:8])+".log")
		if err := os.WriteFile(overflow, out, 0o644); err != nil {
			overflow = ""
		}
	}
	half := limit / 2
	head := out[:half]
	tail := out[len(out)-half:]
	marker := fmt.Sprintf("\n... [%d bytes truncated; full output: %s] ...\n", len(out)-limit, overflow)
	return string(head) + marker + string(tail), overflow
}

func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func readScripts(path string) map[string]string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	return pkg.Scripts
}

var makefileTargetRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_./-]*):(?:[^=]|$)`)

func makefileTargets(path string) map[string]bool {
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()
	targets := map[string]bool{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if m := makefileTargetRe.FindStringSubmatch(sc.Text()); m != nil && m[1] != ".PHONY" {
			targets[m[1]] = true
		}
	}
	return targets
}

// Describe renders commands for prompts and sheets.
func Describe(cmds []Command) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Cmd)
	}
	sort.Strings(out)
	return out
}

// FromStrings wraps raw command strings (e.g. a sheet's verify.commands) as
// custom commands for the runner.
func FromStrings(cmds []string) []Command {
	out := make([]Command, 0, len(cmds))
	for _, c := range cmds {
		c = strings.TrimSpace(c)
		if c == "" {
			continue
		}
		out = append(out, Command{Kind: KindCustom, Cmd: c})
	}
	return out
}
