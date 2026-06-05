// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_fs_routes.go — Forge HTTP filesystem surface (Session 34 / Forge
// Phase 1, matrix.kvx sess#34).
//
// Routes:
//
//	GET  /fs/tree?path=<abs>[&depth=N]    recursive directory listing (cap N=3)
//	GET  /fs/read?path=<abs>              read file contents (utf-8 + size capped)
//	POST /fs/write { path, content }      write file (Monaco onChange debounced)
//
// Auth: existing bearer-token requireAuth path (MATRIX_DAEMON_TOKEN).
// Enabled only when daemonState.forgeFS != nil (set by -forge-mode flag);
// the routes 404 unmounted otherwise so a non-Forge daemon doesn't expose
// even the existence of these surfaces.
//
// Policy: every path passes through NormalizePath → forgeFS.CheckRead /
// CheckWrite. Outside AllowRoots → 403. Inside a DenyPrefix → 403. File
// larger than MaxReadBytes → 413. Write body bigger than MaxWriteBytes
// → 413. Symlink escapes prevented via filepath.EvalSymlinks BEFORE the
// policy check so a symlink to /etc/passwd 403s the same as a direct path.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// fsTreeNode is one entry in a /fs/tree response. Directories carry
// Children (recursively, up to the requested depth); files carry Size +
// ModTime but no Children. Symlinks resolved and marked Symlink=true so
// the SPA can render a hint icon without re-statting.
type fsTreeNode struct {
	Name     string       `json:"name"`
	Path     string       `json:"path"`
	IsDir    bool         `json:"is_dir"`
	Size     int64        `json:"size,omitempty"`
	ModTime  string       `json:"mod_time,omitempty"`
	Symlink  bool         `json:"symlink,omitempty"`
	Denied   bool         `json:"denied,omitempty"` // matched DenyPrefix; surfaced but not descended
	ReadOnly bool         `json:"read_only,omitempty"`
	Children []fsTreeNode `json:"children,omitempty"`
}

// fsReadResponse carries the file contents + metadata. Encoding is "utf8"
// for text files; "base64" when the body fails utf-8 validation (Monaco
// can handle base64 for binary inspection in the future, but Phase 1 the
// SPA only renders utf-8).
type fsReadResponse struct {
	Path     string `json:"path"`
	Encoding string `json:"encoding"` // "utf8" | "base64"
	Content  string `json:"content"`
	Size     int64  `json:"size"`
	ModTime  string `json:"mod_time"`
	SHA256   string `json:"sha256,omitempty"`
}

// fsWriteRequest is the POST /fs/write body. Encoding defaults to utf8;
// base64 supported for future binary uploads. PrevSHA256, when non-empty,
// gates the write on optimistic-concurrency: if the on-disk file's hash
// differs, the route returns 409 Conflict so the SPA can re-fetch and
// merge. Empty PrevSHA256 disables the check (initial create, brand-new
// files).
type fsWriteRequest struct {
	Path       string `json:"path"`
	Content    string `json:"content"`
	Encoding   string `json:"encoding,omitempty"`
	PrevSHA256 string `json:"prev_sha256,omitempty"`
	CreateDirs bool   `json:"create_dirs,omitempty"`
}

type fsWriteResponse struct {
	Path    string `json:"path"`
	Bytes   int64  `json:"bytes"`
	SHA256  string `json:"sha256"`
	Created bool   `json:"created"` // true iff the file did not exist before this write
	ModTime string `json:"mod_time"`
}

// handleForgeFSRouter dispatches /fs/tree, /fs/read, /fs/write. Returns
// 404 when forge-mode is disabled so the routes effectively don't exist
// on non-Forge daemons.
func (d *daemonState) handleForgeFSRouter(w http.ResponseWriter, r *http.Request) {
	if d.forgeFS == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "forge filesystem routes disabled; restart daemon with -forge-mode",
		})
		return
	}
	if !d.requireAuth(w, r) {
		return
	}

	switch r.URL.Path {
	case "/fs/tree":
		d.handleForgeFSTree(w, r)
	case "/fs/read":
		d.handleForgeFSRead(w, r)
	case "/fs/write":
		d.handleForgeFSWrite(w, r)
	default:
		w.Header().Set("Allow", "GET, POST")
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("unknown forge fs route: %s", r.URL.Path),
		})
	}
}

// handleForgeFSTree returns a recursive listing of a directory. Depth
// default 1 (one level); cap 3 to keep payloads bounded for SPA-level
// "expand" interactions. Files matching a DenyPrefix are listed with
// Denied=true so the SPA can render a hint icon but cannot drill in.
func (d *daemonState) handleForgeFSTree(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	pathQ := r.URL.Query().Get("path")
	if pathQ == "" {
		// Default to the first AllowRoot — convenient root-listing for
		// the SPA's "open Forge" first paint.
		if len(d.forgeFS.AllowRoots) > 0 {
			pathQ = d.forgeFS.AllowRoots[0]
		} else {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
			return
		}
	}
	depthQ := r.URL.Query().Get("depth")
	depth := 1
	if depthQ != "" {
		n, err := strconv.Atoi(depthQ)
		if err != nil || n < 1 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "depth must be a positive integer"})
			return
		}
		depth = n
	}
	if depth > 3 {
		depth = 3
	}

	clean, err := NormalizePath(pathQ)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resolved, err := resolveAndCheckRead(d.forgeFS, clean)
	if err != nil {
		writeForgeFSError(w, err)
		return
	}

	info, err := os.Lstat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "path does not exist"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stat: " + err.Error()})
		return
	}
	if !info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "path is a file, not a directory; use /fs/read instead",
		})
		return
	}

	root := buildTree(d.forgeFS, resolved, info, depth)
	writeJSON(w, http.StatusOK, root)
}

// handleForgeFSRead returns the file's contents. UTF-8 by default;
// base64 fallback when the body fails utf-8 validation. SHA256 is
// always included so the SPA can use it as the PrevSHA256 on a
// subsequent write for optimistic-concurrency.
func (d *daemonState) handleForgeFSRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	pathQ := r.URL.Query().Get("path")
	clean, err := NormalizePath(pathQ)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resolved, err := resolveAndCheckRead(d.forgeFS, clean)
	if err != nil {
		writeForgeFSError(w, err)
		return
	}
	info, err := os.Stat(resolved)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "file does not exist"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stat: " + err.Error()})
		return
	}
	if info.IsDir() {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "path is a directory; use /fs/tree instead",
		})
		return
	}
	if info.Size() > d.forgeFS.MaxReadBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("file size %d exceeds MaxReadBytes %d", info.Size(), d.forgeFS.MaxReadBytes),
		})
		return
	}
	body, err := os.ReadFile(resolved)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "read: " + err.Error()})
		return
	}

	resp := fsReadResponse{
		Path:    clean,
		Size:    info.Size(),
		ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
		SHA256:  sha256HexBytes(body),
	}
	if utf8.Valid(body) {
		resp.Encoding = "utf8"
		resp.Content = string(body)
	} else {
		resp.Encoding = "base64"
		resp.Content = base64.StdEncoding.EncodeToString(body)
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleForgeFSWrite persists a file. Mandatory CheckWrite gate +
// MaxWriteBytes cap + optimistic-concurrency via PrevSHA256. Parent
// directories are created on demand when CreateDirs=true.
func (d *daemonState) handleForgeFSWrite(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, d.forgeFS.MaxWriteBytes+4096)
	var req fsWriteRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	clean, err := NormalizePath(req.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	resolved, err := resolveAndCheckWrite(d.forgeFS, clean)
	if err != nil {
		writeForgeFSError(w, err)
		return
	}

	var body []byte
	switch strings.ToLower(req.Encoding) {
	case "", "utf8", "utf-8":
		body = []byte(req.Content)
	case "base64":
		decoded, err := base64.StdEncoding.DecodeString(req.Content)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "base64 decode: " + err.Error()})
			return
		}
		body = decoded
	default:
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": fmt.Sprintf("unknown encoding %q (allowed: utf8, base64)", req.Encoding),
		})
		return
	}
	if int64(len(body)) > d.forgeFS.MaxWriteBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("content size %d exceeds MaxWriteBytes %d", len(body), d.forgeFS.MaxWriteBytes),
		})
		return
	}

	// Optimistic concurrency: if PrevSHA256 set, on-disk file MUST hash
	// to PrevSHA256 OR not exist (in which case PrevSHA256 must be "").
	existed := false
	if existing, err := os.ReadFile(resolved); err == nil {
		existed = true
		if req.PrevSHA256 != "" && sha256HexBytes(existing) != req.PrevSHA256 {
			writeJSON(w, http.StatusConflict, map[string]string{
				"error":          "prev_sha256 mismatch; refetch and retry",
				"current_sha256": sha256HexBytes(existing),
			})
			return
		}
	} else if !os.IsNotExist(err) {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "stat/read: " + err.Error()})
		return
	} else if req.PrevSHA256 != "" {
		// File doesn't exist but caller expected a specific prior hash.
		// Refuse to silently create — caller's mental model is stale.
		writeJSON(w, http.StatusConflict, map[string]string{
			"error": "file does not exist; pass prev_sha256=\"\" for creation",
		})
		return
	}

	if req.CreateDirs {
		parent := filepath.Dir(resolved)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "mkdir: " + err.Error()})
			return
		}
	}

	// Atomic write via tmpfile + rename so partial writes don't corrupt
	// in-flight reads from the SPA or other agents.
	tmp, err := os.CreateTemp(filepath.Dir(resolved), ".forge-write-*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "tmp: " + err.Error()})
		return
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "write tmp: " + err.Error()})
		return
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "close tmp: " + err.Error()})
		return
	}
	if err := os.Rename(tmpPath, resolved); err != nil {
		_ = os.Remove(tmpPath)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "rename: " + err.Error()})
		return
	}

	info, err := os.Stat(resolved)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "post-write stat: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, fsWriteResponse{
		Path:    clean,
		Bytes:   int64(len(body)),
		SHA256:  sha256HexBytes(body),
		Created: !existed,
		ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
	})
}

// resolveAndCheckRead normalises + symlink-resolves the path, then calls
// the policy CheckRead gate. The symlink resolution step is critical:
// without it a symlink at /root/matrix/foo → /etc/passwd would 200 the
// read because the textual prefix matches the allowlist.
//
// For non-existent paths (the common write-new-file case) symlink
// resolution falls back to filepath.Clean of the input — there's no
// link to follow yet, and the caller's CheckWrite enforces the policy
// against the textual path.
func resolveAndCheckRead(p *ForgeFSPolicy, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if os.IsNotExist(err) {
			return path, p.CheckRead(path)
		}
		return "", fmt.Errorf("forge_policy: resolve symlinks: %w", err)
	}
	return resolved, p.CheckRead(resolved)
}

// resolveAndCheckWrite mirrors resolveAndCheckRead but applies CheckWrite.
// For non-existent paths the policy check runs against the cleaned textual
// path AND, additionally, against the parent directory (so writing
// /root/matrix/cortex/store/new-file is rejected because the parent is
// in DenyPrefixes even though the file itself doesn't yet exist).
func resolveAndCheckWrite(p *ForgeFSPolicy, path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		if !os.IsNotExist(err) {
			return "", fmt.Errorf("forge_policy: resolve symlinks: %w", err)
		}
		// Path doesn't exist yet — check the textual path AND the
		// nearest existing ancestor to defend against denied-parent
		// creates.
		if err := p.CheckWrite(path); err != nil {
			return "", err
		}
		// Walk up to find the nearest existing ancestor and assert
		// it's reachable too. Defensive: a TOCTOU race where someone
		// drops a symlink between the check and the rename would still
		// land in the resolved path's denylist via CheckWrite of the
		// ancestor.
		if anc, ok := nearestExistingAncestor(path); ok {
			if err := p.CheckRead(anc); err != nil {
				return "", err
			}
		}
		return path, nil
	}
	return resolved, p.CheckWrite(resolved)
}

// nearestExistingAncestor walks up the path until os.Stat succeeds or
// the root is reached. Returns (ancestor, true) when found, ("", false)
// when no parent of the path exists (shouldn't happen for absolute paths
// since `/` always exists, but defensive).
func nearestExistingAncestor(path string) (string, bool) {
	cur := path
	for {
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		if _, err := os.Stat(parent); err == nil {
			return parent, true
		}
		cur = parent
	}
}

// buildTree assembles an fsTreeNode rooted at path with the given
// remaining depth. Directories matching a DenyPrefix render with
// Denied=true and no Children so the SPA can render a hint icon.
// Sorted by name for determinism (replay-friendly UI snapshots).
func buildTree(p *ForgeFSPolicy, path string, info os.FileInfo, depth int) fsTreeNode {
	node := fsTreeNode{
		Name:    filepath.Base(path),
		Path:    path,
		IsDir:   info.IsDir(),
		ModTime: info.ModTime().UTC().Format(time.RFC3339Nano),
	}
	if info.Mode()&os.ModeSymlink != 0 {
		node.Symlink = true
	}
	if !info.IsDir() {
		node.Size = info.Size()
		return node
	}
	if p.underDenyPrefix(path) {
		node.Denied = true
		return node
	}
	if p.underReadOnlyPrefix(path) {
		node.ReadOnly = true
	}
	if depth <= 0 {
		return node
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		// Surface as empty-but-listed; the SPA can render an indicator
		// from the absence of Children. Hard 500 would block the whole
		// tree fetch over one unreadable subdir.
		return node
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name() < entries[j].Name() })
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".forge-write-") {
			// Hide in-flight write tmpfiles; they're transient.
			continue
		}
		child := filepath.Join(path, e.Name())
		ci, err := e.Info()
		if err != nil {
			continue
		}
		node.Children = append(node.Children, buildTree(p, child, ci, depth-1))
	}
	return node
}

// writeForgeFSError maps policy sentinels to HTTP status codes.
func writeForgeFSError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrPathOutsideAllowlist):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrPathDenied):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrPathReadOnly):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrPathNotAbsolute):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// sha256HexBytes returns the hex SHA-256 of body. Used for optimistic-
// concurrency PrevSHA256 round-tripping between SPA writes. Distinct
// from the identity.go sha256Hex(string) helper because Forge writes
// arbitrary binary payloads — coercing through string would lossy-
// encode any non-utf-8 bytes that survive past CheckWrite.
func sha256HexBytes(body []byte) string {
	sum := sha256.Sum256(body)
	return hex.EncodeToString(sum[:])
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
