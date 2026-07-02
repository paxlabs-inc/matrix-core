// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// The client coding workspace's environment surface: a read-only view of
// /workspace (tree, file, git diff) plus a bounded command runner (the live
// terminal). Everything here operates on the REAL workspace — the same root
// the workers mutate — so what the user reviews is the ground truth, never a
// mirror.

// treeSkipDirs are never listed (mirrors the gate's baseline scan).
var treeSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true, ".turbo": true,
	".cody": true,
}

const (
	// treeMaxEntries caps one tree listing.
	treeMaxEntries = 4000
	// fileReadCap caps one file read (bytes).
	fileReadCap = 256 * 1024
	// diffOutputCap caps the diff body (bytes).
	diffOutputCap = 512 * 1024
	// execOutputCap caps one terminal command's output (bytes).
	termOutputCap = 64 * 1024
	// termDefaultTimeout bounds one terminal command.
	termDefaultTimeout = 60 * time.Second
)

// treeEntry is one row of the workspace tree, depth-first.
type treeEntry struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

// resolveWorkspacePath joins a workspace-relative path against the root and
// refuses traversal outside it.
func (e *Engine) resolveWorkspacePath(rel string) (string, error) {
	root, err := filepath.Abs(e.opts.WorkspaceRoot)
	if err != nil {
		return "", err
	}
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	return abs, nil
}

// handleWorkspace routes GET /workspace/tree|file|diff + POST /workspace/exec.
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/workspace/") {
	case "tree":
		s.handleWorkspaceTree(w, r)
	case "file":
		s.handleWorkspaceFile(w, r)
	case "diff":
		s.handleWorkspaceDiff(w, r)
	case "exec":
		s.handleWorkspaceExec(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleWorkspaceTree serves the depth-first workspace listing.
func (s *Server) handleWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, err := filepath.Abs(s.engine.opts.WorkspaceRoot)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	entries := []treeEntry{}
	truncated := false
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		if d.IsDir() && treeSkipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if len(entries) >= treeMaxEntries {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		entry := treeEntry{Path: filepath.ToSlash(rel), Dir: d.IsDir()}
		if !d.IsDir() {
			if info, err := d.Info(); err == nil {
				entry.Size = info.Size()
			}
		}
		entries = append(entries, entry)
		return nil
	})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"entries": entries, "truncated": truncated,
	})
}

// handleWorkspaceFile serves one file's content (capped, read-only).
func (s *Server) handleWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	abs, err := s.engine.resolveWorkspacePath(rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	buf := make([]byte, fileReadCap)
	n, _ := f.Read(buf)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":      filepath.ToSlash(rel),
		"content":   string(buf[:n]),
		"size":      info.Size(),
		"truncated": info.Size() > int64(n),
	})
}

// handleWorkspaceDiff serves the real `git diff` over the workspace —
// read-only inspection, the diff-review pane's ground truth. A non-git
// workspace returns an empty diff, not an error.
func (s *Server) handleWorkspaceDiff(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root := s.engine.opts.WorkspaceRoot
	if _, err := os.Stat(filepath.Join(root, ".git")); err != nil {
		writeJSON(w, http.StatusOK, map[string]interface{}{"git": false, "diff": "", "untracked": []string{}})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	run := func(args ...string) string {
		cmd := exec.CommandContext(ctx, "git", args...)
		cmd.Dir = root
		out, _ := cmd.CombinedOutput()
		if len(out) > diffOutputCap {
			out = append(out[:diffOutputCap], []byte("\n[diff truncated]")...)
		}
		return string(out)
	}
	diff := run("diff", "--no-color")
	staged := run("diff", "--no-color", "--cached")
	if strings.TrimSpace(staged) != "" {
		diff += staged
	}
	untracked := []string{}
	for _, line := range strings.Split(run("ls-files", "--others", "--exclude-standard"), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			untracked = append(untracked, line)
		}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"git": true, "diff": diff, "untracked": untracked,
	})
}

// handleWorkspaceExec runs one bounded shell command in the workspace root —
// the live terminal. Output is capped; the exit code is always reported
// honestly.
func (s *Server) handleWorkspaceExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Cmd         string `json:"cmd"`
		TimeoutSecs int    `json:"timeout_secs"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Cmd) == "" {
		http.Error(w, "cmd is required", http.StatusBadRequest)
		return
	}
	timeout := termDefaultTimeout
	if req.TimeoutSecs > 0 && req.TimeoutSecs <= 600 {
		timeout = time.Duration(req.TimeoutSecs) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Cmd)
	cmd.Dir = s.engine.opts.WorkspaceRoot
	out, err := cmd.CombinedOutput()
	exit := 0
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			exit = ee.ExitCode()
		} else {
			exit = -1
			out = append(out, []byte(err.Error())...)
		}
	}
	timedOut := ctx.Err() == context.DeadlineExceeded
	if len(out) > termOutputCap {
		out = append(out[:termOutputCap], []byte(fmt.Sprintf("\n[output truncated to %d bytes]", termOutputCap))...)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cmd": req.Cmd, "exit": exit, "output": string(out), "timed_out": timedOut,
	})
}
