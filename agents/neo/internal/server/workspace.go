// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"centra/executor/tool"
)

// The workbench's environment surface on the Neo daemon: a project-scoped view
// of the VM workspace (tree, file read, atomic file WRITE for the editable
// editor, git diff) plus a bounded command runner (the live terminal).
// Everything here operates on the REAL workspace — the same root Neo's fs/exec
// tools mutate — so what the user reviews and edits is the ground truth, never
// a mirror. Neo-owned routes, registered before the catch-all proxy; same
// single-tenant trust posture as /media.

// wsSkipDirs are never listed in a tree walk.
var wsSkipDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true, ".turbo": true,
	".neo": true,
}

const (
	// wsTreeMaxEntries caps one tree listing.
	wsTreeMaxEntries = 4000
	// wsFileReadCap caps one file read's returned content (bytes). The hash
	// always covers the FULL file so staleness detection stays honest even on
	// a truncated read.
	wsFileReadCap = 256 * 1024
	// wsFileWriteCap caps one editor save (bytes).
	wsFileWriteCap = 2 * 1024 * 1024
	// wsDiffOutputCap caps the diff body (bytes).
	wsDiffOutputCap = 512 * 1024
	// wsExecOutputCap caps one terminal command's output (bytes).
	wsExecOutputCap = 64 * 1024
	// wsExecDefaultTimeout bounds one terminal command.
	wsExecDefaultTimeout = 60 * time.Second
	// wsExecMaxTimeoutSecs is the largest client-requested exec bound.
	wsExecMaxTimeoutSecs = 600
)

// errWorkspaceEscape is returned by the path seam for any path that resolves
// outside the project root.
var errWorkspaceEscape = errors.New("path escapes the workspace")

// WorkspaceRoot resolves the VM workspace root: an explicit override wins,
// else the persisted /workspace (= /data/workspace in prod, matching the fs
// and exec tools' default), else "" (workbench surface disabled). The result
// is always symlink-resolved: prod's /workspace is a symlink to
// /data/workspace, and filepath.WalkDir does not follow a symlink root — an
// unresolved root makes every tree listing come back empty.
func WorkspaceRoot(override string) string {
	if override = strings.TrimSpace(override); override != "" {
		return resolveWorkspaceDir(strings.TrimRight(override, "/"))
	}
	for _, dir := range []string{"/workspace", "/data/workspace"} {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return resolveWorkspaceDir(dir)
		}
	}
	return ""
}

// resolveWorkspaceDir canonicalizes a workspace root through any symlinks; an
// unresolvable path passes through unchanged (dev-box overrides may not exist
// yet).
func resolveWorkspaceDir(dir string) string {
	if resolved, err := filepath.EvalSymlinks(dir); err == nil {
		return strings.TrimRight(resolved, "/")
	}
	return dir
}

// wsTreeEntry is one row of the workspace tree, depth-first.
type wsTreeEntry struct {
	Path string `json:"path"`
	Dir  bool   `json:"dir"`
	Size int64  `json:"size,omitempty"`
}

// resolveWorkspacePath is the ONE path seam every workspace handler routes
// through (the cody edit.Rel posture): it joins a workspace-relative OR
// absolute path against root and refuses anything that resolves outside it,
// so `../`, absolute out-of-root, and symlink-free traversal games never
// reach the filesystem beyond the project root.
func resolveWorkspacePath(root, path string) (string, error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	absRoot = filepath.Clean(absRoot)
	p := filepath.FromSlash(path)
	if !filepath.IsAbs(p) {
		p = filepath.Join(absRoot, p)
	}
	abs := filepath.Clean(p)
	if abs != absRoot && !strings.HasPrefix(abs, absRoot+string(filepath.Separator)) {
		return "", errWorkspaceEscape
	}
	return abs, nil
}

// workspaceRel normalizes path to the clean slash-separated workspace-relative
// form (an absolute in-root path and its relative form normalize identically),
// rejecting escapes via the same seam.
func workspaceRel(root, path string) (string, error) {
	abs, err := resolveWorkspacePath(root, path)
	if err != nil {
		return "", err
	}
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(filepath.Clean(absRoot), abs)
	if err != nil {
		return "", err
	}
	return filepath.ToSlash(rel), nil
}

// projectRoot resolves the request's project (a workspace subdirectory) to an
// absolute directory inside the workspace root, through the same escape seam.
// An empty project addresses the workspace root itself (the default project).
func (e *Engine) projectRoot(project string) (string, error) {
	if e.workspaceRoot == "" {
		return "", errors.New("workspace is not configured on this daemon")
	}
	record, err := e.resolveProjectRecord(strings.TrimSpace(project))
	if err != nil {
		return "", err
	}
	abs, err := resolveWorkspacePath(e.workspaceRoot, record.Root)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolveWorkspacePath(e.workspaceRoot, resolved)
}

// reqProjectRoot resolves ?project= to the project root and requires it to be
// an existing directory (writes create files, never projects).
func (e *Engine) reqProjectRoot(r *http.Request) (string, error) {
	root, err := e.projectRoot(r.URL.Query().Get("project"))
	if err != nil {
		return "", err
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return "", errors.New("project not found")
	}
	return root, nil
}

// hashBytes is the file version stamp: sha256 hex over the exact bytes.
func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// hashFile stamps the file's CURRENT full content; ("", os.ErrNotExist) for a
// missing file.
func hashFile(abs string) (string, error) {
	f, err := os.Open(abs)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// handleWorkspace routes the workbench environment surface.
func (s *Server) handleWorkspace(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/workspace/") {
	case "tree":
		s.handleWorkspaceTree(w, r)
	case "file":
		switch r.Method {
		case http.MethodGet:
			s.handleWorkspaceFileRead(w, r)
		case http.MethodPut, http.MethodPost:
			s.handleWorkspaceFileWrite(w, r)
		default:
			w.Header().Set("Allow", "GET, PUT")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	case "raw":
		s.handleWorkspaceRaw(w, r)
	case "upload":
		s.handleWorkspaceUpload(w, r)
	case "diff":
		s.handleWorkspaceDiff(w, r)
	case "exec":
		s.handleWorkspaceExec(w, r)
	case "preview":
		s.handleWorkspacePreview(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleWorkspaceTree serves the depth-first project listing.
func (s *Server) handleWorkspaceTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	entries := []wsTreeEntry{}
	truncated := false
	err = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if path == root {
			return nil
		}
		if d.IsDir() && wsSkipDirs[d.Name()] {
			return filepath.SkipDir
		}
		if len(entries) >= wsTreeMaxEntries {
			truncated = true
			return filepath.SkipAll
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return nil
		}
		entry := wsTreeEntry{Path: filepath.ToSlash(rel), Dir: d.IsDir()}
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

// handleWorkspaceFileRead serves one file's content (capped) plus its full
// version hash — the base the editable editor saves against.
func (s *Server) handleWorkspaceFileRead(w http.ResponseWriter, r *http.Request) {
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	abs, err := resolveWorkspacePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	hash, err := hashFile(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	buf := make([]byte, wsFileReadCap)
	n, _ := io.ReadFull(f, buf)
	relOut, _ := workspaceRel(root, rel)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path":      relOut,
		"content":   string(buf[:n]),
		"size":      info.Size(),
		"hash":      hash,
		"truncated": info.Size() > int64(n),
	})
}

// handleWorkspaceRaw streams one workspace file's exact bytes — the download
// path of the Files page. Unlike the JSON editor read it is binary-safe and
// uncapped. Images / video / audio serve inline (thumbnails, previews);
// everything else downloads as an attachment so user-uploaded markup can
// never execute on this origin (the /media stored-XSS posture).
func (s *Server) handleWorkspaceRaw(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rel := r.URL.Query().Get("path")
	if rel == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	abs, err := resolveWorkspacePath(root, rel)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	f, err := os.Open(abs)
	if err != nil {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.IsDir() {
		http.Error(w, "not a file", http.StatusNotFound)
		return
	}
	name := filepath.Base(abs)
	m := mimeForName(name)
	w.Header().Set("Content-Type", m)
	if kindForMIME(m) == "file" {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", name))
	}
	http.ServeContent(w, r, name, info.ModTime(), f)
}

// handleWorkspaceUpload receives user files onto the REAL workspace — the
// same root Neo's fs and exec tools mutate (POST multipart, one or more
// "file" parts, optional "dir" field targeting a workspace subdirectory).
// Each file lands atomically (temp + rename, the editor-save posture) under
// its own base name; the response reports the normalized workspace-relative
// paths so the client can show exactly what Neo will see.
func (s *Server) handleWorkspaceUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, uploadMaxBytes+(1<<20))
	if err := r.ParseMultipartForm(16 << 20); err != nil {
		http.Error(w, "multipart form required (field 'file')", http.StatusBadRequest)
		return
	}
	defer func() {
		if r.MultipartForm != nil {
			_ = r.MultipartForm.RemoveAll()
		}
	}()
	parts := r.MultipartForm.File["file"]
	if len(parts) == 0 {
		http.Error(w, "a 'file' part is required", http.StatusBadRequest)
		return
	}
	dirAbs, err := resolveWorkspacePath(root, strings.TrimSpace(r.FormValue("dir")))
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := os.MkdirAll(dirAbs, 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	type uploaded struct {
		Path string `json:"path"`
		Name string `json:"name"`
		Size int64  `json:"size"`
	}
	files := []uploaded{}
	for _, hdr := range parts {
		name := safeMediaName(filepath.Base(filepath.FromSlash(hdr.Filename)))
		if name == "" {
			http.Error(w, fmt.Sprintf("invalid filename %q", hdr.Filename), http.StatusBadRequest)
			return
		}
		if hdr.Size > uploadMaxBytes {
			http.Error(w, fmt.Sprintf("%s exceeds the %d-byte upload cap", name, int64(uploadMaxBytes)), http.StatusRequestEntityTooLarge)
			return
		}
		src, err := hdr.Open()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tmp, err := os.CreateTemp(dirAbs, ".neo-upload-*")
		if err != nil {
			src.Close()
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		tmpName := tmp.Name()
		written, copyErr := io.Copy(tmp, src)
		src.Close()
		closeErr := tmp.Close()
		if copyErr != nil || closeErr != nil {
			os.Remove(tmpName)
			http.Error(w, "failed to write upload", http.StatusInternalServerError)
			return
		}
		_ = os.Chmod(tmpName, 0o644)
		dst := filepath.Join(dirAbs, name)
		if err := os.Rename(tmpName, dst); err != nil {
			os.Remove(tmpName)
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		relOut, _ := workspaceRel(root, dst)
		files = append(files, uploaded{Path: relOut, Name: name, Size: written})
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{"files": files})
}

// workspaceWriteRequest is the editable editor's save body. BaseHash, when
// set, is the version the buffer was loaded from: a mismatching current file
// is a 409 (stale) so a concurrent Neo write is DETECTED, never clobbered
// silently.
type workspaceWriteRequest struct {
	Path     string `json:"path"`
	Content  string `json:"content"`
	BaseHash string `json:"base_hash,omitempty"`
}

// handleWorkspaceFileWrite is the atomic editor save: write-then-rename in the
// target dir, returning the resulting version hash.
func (s *Server) handleWorkspaceFileWrite(w http.ResponseWriter, r *http.Request) {
	var req workspaceWriteRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, wsFileWriteCap+64*1024)).Decode(&req); err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(req.Path) == "" {
		http.Error(w, "path is required", http.StatusBadRequest)
		return
	}
	if len(req.Content) > wsFileWriteCap {
		http.Error(w, fmt.Sprintf("content exceeds the %d-byte write cap", wsFileWriteCap), http.StatusRequestEntityTooLarge)
		return
	}
	root, err := s.engine.reqProjectRoot(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	abs, err := resolveWorkspacePath(root, req.Path)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if info, err := os.Stat(abs); err == nil && info.IsDir() {
		http.Error(w, "path is a directory", http.StatusBadRequest)
		return
	}
	if req.BaseHash != "" {
		current, err := hashFile(abs)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		if current != req.BaseHash {
			writeJSON(w, http.StatusConflict, map[string]interface{}{
				"stale": true, "hash": current,
			})
			return
		}
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmp, err := os.CreateTemp(filepath.Dir(abs), ".neo-write-*")
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.WriteString(req.Content); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Chmod(tmpName, 0o644)
	if err := os.Rename(tmpName, abs); err != nil {
		os.Remove(tmpName)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	relOut, _ := workspaceRel(root, req.Path)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"path": relOut,
		"hash": hashBytes([]byte(req.Content)),
		"size": len(req.Content),
	})
}

// handleWorkspaceDiff serves the real `git diff` over the project — read-only
// inspection, the diff pane's ground truth. A non-git project returns an empty
// diff, not an error.
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
		cmd.Env = tool.AgentEnvironment(os.Environ())
		if err := tool.ConfigureAgentCommand(cmd); err != nil {
			return ""
		}
		out, _ := cmd.CombinedOutput()
		if len(out) > wsDiffOutputCap {
			out = append(out[:wsDiffOutputCap], []byte("\n[diff truncated]")...)
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

// handleWorkspaceExec runs one bounded shell command in the project root — the
// live terminal. Output is capped; the exit code is always reported honestly.
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
	timeout := wsExecDefaultTimeout
	if req.TimeoutSecs > 0 && req.TimeoutSecs <= wsExecMaxTimeoutSecs {
		timeout = time.Duration(req.TimeoutSecs) * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sh", "-c", req.Cmd)
	cmd.Dir = root
	cmd.Env = tool.AgentEnvironment(os.Environ())
	cmd.Env = append(cmd.Env, "PWD="+root)
	if err := tool.ConfigureAgentCommand(cmd); err != nil {
		http.Error(w, "workspace execution identity unavailable", http.StatusInternalServerError)
		return
	}
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
	if len(out) > wsExecOutputCap {
		out = append(out[:wsExecOutputCap], []byte(fmt.Sprintf("\n[output truncated to %d bytes]", wsExecOutputCap))...)
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"cmd": req.Cmd, "exit": exit, "output": string(out), "timed_out": timedOut,
	})
}
