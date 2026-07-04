// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// diagTimeout bounds one post-edit diagnostic check — these are fast syntax
// probes, never full builds.
const diagTimeout = 10 * time.Second

// diagMaxBytes caps the diagnostics appended to a tool result.
const diagMaxBytes = 2 * 1024

// diagnose runs a cheap per-language syntax check on a just-written file and
// returns a diagnostics note to append to the fs_write/fs_edit tool result —
// so the model learns "you broke the file" in the same step, without waiting
// for a full verify_run. Best-effort: a missing checker or an unknown
// extension returns "". It never replaces verification.
func (w *Worker) diagnose(rel string) string {
	abs := filepath.Join(w.opts.Root, filepath.FromSlash(rel))
	var out string
	switch strings.ToLower(filepath.Ext(rel)) {
	case ".go":
		out = runDiag(w.opts.Root, "gofmt", "-e", "-l", abs)
	case ".js", ".mjs", ".cjs":
		out = runDiag(w.opts.Root, "node", "--check", abs)
	case ".py":
		out = runDiag(w.opts.Root, "python3", "-m", "py_compile", abs)
	case ".sh", ".bash":
		out = runDiag(w.opts.Root, "bash", "-n", abs)
	case ".json":
		data, err := os.ReadFile(abs)
		if err == nil && !json.Valid(data) {
			out = "invalid JSON: " + jsonSyntaxDetail(data)
		}
	default:
		return ""
	}
	out = strings.TrimSpace(out)
	if out == "" {
		return ""
	}
	if len(out) > diagMaxBytes {
		out = out[:diagMaxBytes] + "..."
	}
	return "\n[diagnostics] " + out
}

// runDiag executes one checker and returns its combined output ONLY when it
// failed (non-zero exit). A missing binary or a clean pass returns "".
// gofmt -e -l is special-cased: it exits 0 even on syntax errors, reporting
// them on stderr, so any stderr output counts as a failure there.
func runDiag(dir, bin string, args ...string) string {
	if _, err := exec.LookPath(bin); err != nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), diagTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stderr strings.Builder
	cmd.Stderr = &stderr
	err := cmd.Run()
	if bin == "gofmt" {
		return stderr.String()
	}
	if err == nil {
		return ""
	}
	return stderr.String()
}

// jsonSyntaxDetail locates the first JSON syntax error for the note.
func jsonSyntaxDetail(data []byte) string {
	var v interface{}
	err := json.Unmarshal(data, &v)
	if err == nil {
		return "unparseable"
	}
	if se, ok := err.(*json.SyntaxError); ok {
		line := 1 + strings.Count(string(data[:min(int(se.Offset), len(data))]), "\n")
		return fmt.Sprintf("%s (around line %d)", se.Error(), line)
	}
	return err.Error()
}
