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

	"matrix/cody/internal/checkpoint"
	"matrix/cody/internal/preview"
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

// resolveWorkspacePath joins a workspace-relative path against a project root
// and refuses traversal outside it.
func resolveWorkspacePath(projectRoot, rel string) (string, error) {
	root, err := filepath.Abs(projectRoot)
	if err != nil {
		return "", err
	}
	abs := filepath.Clean(filepath.Join(root, filepath.FromSlash(rel)))
	if abs != root && !strings.HasPrefix(abs, root+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	return abs, nil
}

// handleWorkspace routes the read-only environment surface plus the terminal
// and the snapshot/undo actions.
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
	case "snapshot":
		s.handleWorkspaceSnapshot(w, r)
	case "snapshots":
		s.handleWorkspaceSnapshots(w, r)
	case "restore":
		s.handleWorkspaceRestore(w, r)
	case "preview":
		s.handleWorkspacePreview(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleWorkspacePreview is the single-action re-preview (req 7.3): POST it to
// (re)provision the project's on-demand sandbox preview. It returns 202 and the
// lifecycle unfolds on preview.pending/ready/failed events; the client renders
// those. A run need not be active — the user can ask for a preview of the
// current project state at any time.
func (s *Server) handleWorkspacePreview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.engine.preview.Enabled() {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{
			"error": "preview is not configured in this environment",
		})
		return
	}
	var req struct {
		ConversationID string `json:"conversation_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	convID := strings.TrimSpace(req.ConversationID)
	if convID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "conversation_id is required"})
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// Provision in the background so the POST returns immediately; the client
	// follows preview.* on the conversation's live topic.
	go s.engine.preview.Provision(context.Background(), preview.Request{ConvID: convID, Root: root})
	writeJSON(w, http.StatusAccepted, map[string]interface{}{"preview": "provisioning"})
}

// handleWorkspaceSnapshot takes a named workspace snapshot (POST). One-button
// undo in Prototype and named checkpoints in Engineer/Architect both land here
// (req 14.1). The snapshot.created event is best-effort emitted on the addressed
// conversation's run so it appears in the durable timeline.
func (s *Server) handleWorkspaceSnapshot(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		Label          string `json:"label"`
		ConversationID string `json:"conversation_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	snap, err := checkpoint.NewSnapshotter(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	id, err := snap.Snapshot(strings.TrimSpace(req.Label))
	if err != nil {
		http.Error(w, "snapshot: "+err.Error(), http.StatusInternalServerError)
		return
	}
	s.engine.publishToConversation(req.ConversationID, "snapshot.created",
		map[string]interface{}{"id": id, "label": strings.TrimSpace(req.Label)})
	writeJSON(w, http.StatusCreated, map[string]interface{}{"id": id, "label": strings.TrimSpace(req.Label)})
}

// handleWorkspaceSnapshots lists the project's snapshots, oldest first (GET).
func (s *Server) handleWorkspaceSnapshots(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	snap, err := checkpoint.NewSnapshotter(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	list, err := snap.List()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if list == nil {
		list = []checkpoint.Info{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"snapshots": list})
}

// handleWorkspaceRestore rolls the workspace back to a snapshot (POST). It
// REFUSES while a worker is alive on the project (req 14.1): a restore mid-run
// would pull the ground out from under a live worker. The snapshot.restored
// event is best-effort emitted on the addressed conversation's run.
func (s *Server) handleWorkspaceRestore(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		ID             string `json:"id"`
		ConversationID string `json:"conversation_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.ID) == "" {
		http.Error(w, "id is required", http.StatusBadRequest)
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.engine.projectHasLiveRun(root) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "a run is live in this project; stop it before restoring"})
		return
	}
	snap, err := checkpoint.NewSnapshotter(root)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := snap.Restore(strings.TrimSpace(req.ID)); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "restore: " + err.Error()})
		return
	}
	s.engine.publishToConversation(req.ConversationID, "snapshot.restored", map[string]interface{}{"id": strings.TrimSpace(req.ID)})
	writeJSON(w, http.StatusOK, map[string]interface{}{"restored": true, "id": strings.TrimSpace(req.ID)})
}

// handleWorkspaceTree serves the depth-first workspace listing.
func (s *Server) handleWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	projectRoot, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	root, err := filepath.Abs(projectRoot)
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
	projectRoot, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	abs, err := resolveWorkspacePath(projectRoot, rel)
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
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
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
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	timeout := termDefaultTimeout
	if req.TimeoutSecs > 0 && req.TimeoutSecs <= 600 {
		timeout = time.Duration(req.TimeoutSecs) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Cmd)
	cmd.Dir = root
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
