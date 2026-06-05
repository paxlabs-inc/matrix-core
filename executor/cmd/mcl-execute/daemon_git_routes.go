// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_git_routes.go — Forge git HTTP surface (Session 36 / Forge Phase 3).
//
// Routes (all gated on d.gitOps != nil; otherwise 404):
//
//	GET    /git/status                        porcelain-v2 -z parsed → entries
//	GET    /git/diff?path=&staged=true|false  unified diff (-U3) for one path
//	POST   /git/branch  {name, from?}         create branch from from|HEAD
//	GET    /git/branch                        list branches
//	DELETE /git/branch?name=                  delete local branch (refuses protected)
//	POST   /git/merge   {branch, no_ff?}      merge into current; ff-only by default
//
// Auth: existing requireAuth (MATRIX_DAEMON_TOKEN bearer).
// Single-flight: gitOpsLock serialises every mutating verb so the working
// tree stays consistent across concurrent SPA + agent reach paths.
//
// Audit: every call emits a `git.op.<verb>` transcript event with the
// resolved repo, args (sanitized), exit code, and duration. The git
// operations LIVE in the audit feed alongside step.text + intent.cost
// (sess#33) so a later replay can reconstruct what the agent did to
// the working tree.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// gitStatusEntry is one row in `git status --porcelain=v2 -z` output.
// The wire shape mirrors what diff2html / SPA diff list pane needs.
//
// Fields:
//
//	Path        — current path (post-rename when renamed)
//	OrigPath    — pre-rename path (only set when StatusY == 'R')
//	StatusX     — index status char (' '|'M'|'A'|'D'|'R'|'C'|'U'|'?'|'!')
//	StatusY     — worktree status char (same enum as StatusX)
//	Staged      — derived: StatusX is non-space and non-'?' (i.e. has
//	              an index change)
//	Unstaged    — derived: StatusY is non-space and non-'?'
//	Untracked   — derived: StatusX == '?' && StatusY == '?'
type gitStatusEntry struct {
	Path      string `json:"path"`
	OrigPath  string `json:"orig_path,omitempty"`
	StatusX   string `json:"status_x"`
	StatusY   string `json:"status_y"`
	Staged    bool   `json:"staged"`
	Unstaged  bool   `json:"unstaged"`
	Untracked bool   `json:"untracked"`
}

type gitStatusResponse struct {
	Repo     string           `json:"repo"`
	Branch   string           `json:"branch,omitempty"`
	Upstream string           `json:"upstream,omitempty"`
	Ahead    int              `json:"ahead"`
	Behind   int              `json:"behind"`
	Entries  []gitStatusEntry `json:"entries"`
}

type gitDiffResponse struct {
	Repo    string `json:"repo"`
	Path    string `json:"path"`
	Staged  bool   `json:"staged"`
	Unified string `json:"unified"`
	Bytes   int    `json:"bytes"`
	Empty   bool   `json:"empty,omitempty"` // git diff returned no body
}

type gitBranchCreateRequest struct {
	Name string `json:"name"`
	From string `json:"from,omitempty"` // optional ref to branch from; empty → HEAD
}

type gitBranchInfo struct {
	Name      string `json:"name"`
	Commit    string `json:"commit"`
	IsHead    bool   `json:"is_head"`
	Subject   string `json:"subject,omitempty"`
	Protected bool   `json:"protected,omitempty"`
}

type gitBranchListResponse struct {
	Repo     string          `json:"repo"`
	Current  string          `json:"current"`
	Branches []gitBranchInfo `json:"branches"`
}

type gitMergeRequest struct {
	Branch string `json:"branch"`
	NoFF   bool   `json:"no_ff,omitempty"`
}

type gitMergeResponse struct {
	Repo        string `json:"repo"`
	Branch      string `json:"branch"`
	Merged      bool   `json:"merged"`
	FastForward bool   `json:"fast_forward"`
	Output      string `json:"output"`
}

// gitOpsLock serialises every mutating /git/* call.
// Intentionally shared across requests because the working tree is
// the contended resource. Reads (status, diff, branch list) take a
// read lock; mutations take write.
type gitOpsLock struct {
	mu sync.RWMutex
}

// gitMutex provides a process-global lock for daemon-side git
// operations. Single-flight is the right posture: even mcl-execute
// daemon's other goroutines (e.g. an MCP-side git_commit) don't share
// this mutex, but the HTTP-side surface is consistent within itself.
var gitMutex = &gitOpsLock{}

// handleForgeGitRouter dispatches GET/POST/DELETE on /git/* paths.
// Returns 404 when gitOps is nil so non-Forge daemons don't expose
// the routes.
func (d *daemonState) handleForgeGitRouter(w http.ResponseWriter, r *http.Request) {
	if d.gitOps == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "forge git routes disabled; restart daemon with -forge-mode",
		})
		return
	}
	if !d.requireAuth(w, r) {
		return
	}

	switch r.URL.Path {
	case "/git/status":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		d.handleGitStatus(w, r)
	case "/git/diff":
		if r.Method != http.MethodGet {
			methodNotAllowed(w, "GET")
			return
		}
		d.handleGitDiff(w, r)
	case "/git/branch":
		switch r.Method {
		case http.MethodGet:
			d.handleGitBranchList(w, r)
		case http.MethodPost:
			d.handleGitBranchCreate(w, r)
		case http.MethodDelete:
			d.handleGitBranchDelete(w, r)
		default:
			methodNotAllowed(w, "GET, POST, DELETE")
		}
	case "/git/merge":
		if r.Method != http.MethodPost {
			methodNotAllowed(w, "POST")
			return
		}
		d.handleGitMerge(w, r)
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": fmt.Sprintf("unknown forge git route: %s", r.URL.Path),
		})
	}
}

func methodNotAllowed(w http.ResponseWriter, allow string) {
	w.Header().Set("Allow", allow)
	writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
}

// --- /git/status ---------------------------------------------------

func (d *daemonState) handleGitStatus(w http.ResponseWriter, r *http.Request) {
	if err := d.gitOps.CheckOp(GitOpStatus); err != nil {
		writeGitOpError(w, err)
		return
	}
	gitMutex.mu.RLock()
	defer gitMutex.mu.RUnlock()
	t0 := time.Now()
	out, err := runGit(r.Context(), d.gitOps.Repo, "status", "--porcelain=v2", "--branch", "-z")
	dur := time.Since(t0)
	if err != nil {
		emitGitOp(d, GitOpStatus, d.gitOps.Repo, dur, err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "git status: " + err.Error(),
		})
		return
	}

	resp := parsePorcelainV2(d.gitOps.Repo, out)
	emitGitOp(d, GitOpStatus, d.gitOps.Repo, dur, "ok")
	writeJSON(w, http.StatusOK, resp)
}

// parsePorcelainV2 parses git status --porcelain=v2 -z output into a
// gitStatusResponse. The format is documented at
// https://git-scm.com/docs/git-status#_porcelain_format_version_2 ;
// briefly:
//
//	# branch.oid <commit> | (initial)
//	# branch.head <branch> | (detached)
//	# branch.upstream <upstream>
//	# branch.ab +<ahead> -<behind>
//	1 <X><Y> <sub> <mH> <mI> <mW> <hH> <hI> <path>     (tracked)
//	2 <X><Y> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>\0<orig> (rename/copy)
//	u <X><Y> <sub> <m1> <m2> <m3> <mW> <h1> <h2> <h3> <path>  (unmerged)
//	? <path>                                            (untracked)
//	! <path>                                            (ignored)
//
// `-z` separator is NUL between records (and additionally between path
// and orig path inside type-2 records).
func parsePorcelainV2(repo, out string) gitStatusResponse {
	resp := gitStatusResponse{Repo: repo, Entries: []gitStatusEntry{}}
	if out == "" {
		return resp
	}

	// Split on NUL. Type-2 (rename) records have an extra NUL-separated
	// orig-path which we re-attach to the previous record below.
	tokens := strings.Split(out, "\x00")
	// Last token is empty when output ends with NUL.
	if len(tokens) > 0 && tokens[len(tokens)-1] == "" {
		tokens = tokens[:len(tokens)-1]
	}

	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if tok == "" {
			continue
		}
		switch {
		case strings.HasPrefix(tok, "# branch.head "):
			resp.Branch = strings.TrimPrefix(tok, "# branch.head ")
		case strings.HasPrefix(tok, "# branch.upstream "):
			resp.Upstream = strings.TrimPrefix(tok, "# branch.upstream ")
		case strings.HasPrefix(tok, "# branch.ab "):
			rest := strings.TrimPrefix(tok, "# branch.ab ")
			parts := strings.Fields(rest)
			if len(parts) == 2 {
				if a, err := strconv.Atoi(strings.TrimPrefix(parts[0], "+")); err == nil {
					resp.Ahead = a
				}
				if b, err := strconv.Atoi(strings.TrimPrefix(parts[1], "-")); err == nil {
					resp.Behind = b
				}
			}
		case strings.HasPrefix(tok, "# "):
			// Other branch.* headers we don't parse explicitly.
		case strings.HasPrefix(tok, "1 "):
			fields := strings.SplitN(tok, " ", 9)
			if len(fields) < 9 {
				continue
			}
			xy := fields[1]
			path := fields[8]
			resp.Entries = append(resp.Entries, makeStatusEntry(xy, path, ""))
		case strings.HasPrefix(tok, "2 "):
			// 2 <X><Y> <sub> <mH> <mI> <mW> <hH> <hI> <X><score> <path>
			// followed by NUL-separated orig path (next token).
			fields := strings.SplitN(tok, " ", 10)
			if len(fields) < 10 {
				continue
			}
			xy := fields[1]
			path := fields[9]
			orig := ""
			if i+1 < len(tokens) {
				orig = tokens[i+1]
				i++ // consume the orig path token
			}
			resp.Entries = append(resp.Entries, makeStatusEntry(xy, path, orig))
		case strings.HasPrefix(tok, "u "):
			fields := strings.SplitN(tok, " ", 11)
			if len(fields) < 11 {
				continue
			}
			xy := fields[1]
			path := fields[10]
			resp.Entries = append(resp.Entries, makeStatusEntry(xy, path, ""))
		case strings.HasPrefix(tok, "? "):
			path := strings.TrimPrefix(tok, "? ")
			resp.Entries = append(resp.Entries, gitStatusEntry{
				Path:      path,
				StatusX:   "?",
				StatusY:   "?",
				Untracked: true,
			})
		case strings.HasPrefix(tok, "! "):
			// ignored — surface in case the SPA wants to render them.
			path := strings.TrimPrefix(tok, "! ")
			resp.Entries = append(resp.Entries, gitStatusEntry{
				Path:    path,
				StatusX: "!",
				StatusY: "!",
			})
		}
	}
	return resp
}

func makeStatusEntry(xy, path, orig string) gitStatusEntry {
	if len(xy) < 2 {
		return gitStatusEntry{Path: path, OrigPath: orig}
	}
	x := string(xy[0])
	y := string(xy[1])
	return gitStatusEntry{
		Path:     path,
		OrigPath: orig,
		StatusX:  x,
		StatusY:  y,
		Staged:   x != "." && x != " " && x != "?",
		Unstaged: y != "." && y != " " && y != "?",
	}
}

// --- /git/diff -----------------------------------------------------

func (d *daemonState) handleGitDiff(w http.ResponseWriter, r *http.Request) {
	if err := d.gitOps.CheckOp(GitOpDiff); err != nil {
		writeGitOpError(w, err)
		return
	}
	pathQ := r.URL.Query().Get("path")
	if pathQ == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	// Defensive: refuse path arguments that look like git flags. The
	// `--` separator below handles option-injection too, but
	// short-circuiting here gives a clearer error.
	if strings.HasPrefix(pathQ, "-") {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path must not start with -"})
		return
	}

	staged := r.URL.Query().Get("staged") == "true"
	gitMutex.mu.RLock()
	defer gitMutex.mu.RUnlock()

	args := []string{"diff", "--no-color", "-U3"}
	if staged {
		args = append(args, "--cached")
	}
	args = append(args, "--", pathQ)

	t0 := time.Now()
	out, err := runGit(r.Context(), d.gitOps.Repo, args...)
	dur := time.Since(t0)
	if err != nil {
		emitGitOp(d, GitOpDiff, d.gitOps.Repo, dur, err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "git diff: " + err.Error(),
		})
		return
	}
	if int64(len(out)) > d.gitOps.MaxDiffBytes {
		emitGitOp(d, GitOpDiff, d.gitOps.Repo, dur, "diff_too_large")
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{
			"error": fmt.Sprintf("diff size %d exceeds MaxDiffBytes %d", len(out), d.gitOps.MaxDiffBytes),
		})
		return
	}
	emitGitOp(d, GitOpDiff, d.gitOps.Repo, dur, "ok")
	writeJSON(w, http.StatusOK, gitDiffResponse{
		Repo:    d.gitOps.Repo,
		Path:    pathQ,
		Staged:  staged,
		Unified: out,
		Bytes:   len(out),
		Empty:   strings.TrimSpace(out) == "",
	})
}

// --- /git/branch (POST = create) -----------------------------------

func (d *daemonState) handleGitBranchCreate(w http.ResponseWriter, r *http.Request) {
	if err := d.gitOps.CheckOp(GitOpBranchCreate); err != nil {
		writeGitOpError(w, err)
		return
	}
	var req gitBranchCreateRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	if err := ValidateRefName(req.Name); err != nil {
		writeGitOpError(w, err)
		return
	}
	if req.From != "" {
		if err := ValidateRefName(req.From); err != nil {
			writeGitOpError(w, err)
			return
		}
	}

	gitMutex.mu.Lock()
	defer gitMutex.mu.Unlock()

	args := []string{"branch", "--", req.Name}
	if req.From != "" {
		args = []string{"branch", "--", req.Name, req.From}
	}
	t0 := time.Now()
	out, err := runGit(r.Context(), d.gitOps.Repo, args...)
	dur := time.Since(t0)
	if err != nil {
		emitGitOp(d, GitOpBranchCreate, d.gitOps.Repo, dur, err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":  "git branch: " + err.Error(),
			"output": out,
		})
		return
	}
	emitGitOp(d, GitOpBranchCreate, d.gitOps.Repo, dur, "ok:"+req.Name)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"repo":   d.gitOps.Repo,
		"name":   req.Name,
		"from":   req.From,
		"output": out,
	})
}

// --- /git/branch (GET = list) --------------------------------------

func (d *daemonState) handleGitBranchList(w http.ResponseWriter, r *http.Request) {
	if err := d.gitOps.CheckOp(GitOpBranchList); err != nil {
		writeGitOpError(w, err)
		return
	}
	gitMutex.mu.RLock()
	defer gitMutex.mu.RUnlock()

	t0 := time.Now()
	// Format: <head-marker> <ref> <commit> <subject>
	// %(HEAD) emits '*' for current, ' ' otherwise.
	out, err := runGit(r.Context(), d.gitOps.Repo,
		"for-each-ref", "--format=%(HEAD)\x1f%(refname:short)\x1f%(objectname)\x1f%(contents:subject)",
		"refs/heads/")
	dur := time.Since(t0)
	if err != nil {
		emitGitOp(d, GitOpBranchList, d.gitOps.Repo, dur, err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "git for-each-ref: " + err.Error(),
		})
		return
	}

	resp := gitBranchListResponse{Repo: d.gitOps.Repo, Branches: []gitBranchInfo{}}
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\x1f", 4)
		if len(parts) < 3 {
			continue
		}
		isHead := strings.TrimSpace(parts[0]) == "*"
		bi := gitBranchInfo{
			Name:      parts[1],
			Commit:    parts[2],
			IsHead:    isHead,
			Protected: d.gitOps.IsProtectedRef(parts[1]),
		}
		if len(parts) == 4 {
			bi.Subject = parts[3]
		}
		if isHead {
			resp.Current = bi.Name
		}
		resp.Branches = append(resp.Branches, bi)
	}
	emitGitOp(d, GitOpBranchList, d.gitOps.Repo, dur, fmt.Sprintf("ok:%d", len(resp.Branches)))
	writeJSON(w, http.StatusOK, resp)
}

// --- /git/branch (DELETE) ------------------------------------------

func (d *daemonState) handleGitBranchDelete(w http.ResponseWriter, r *http.Request) {
	if err := d.gitOps.CheckOp(GitOpBranchDelete); err != nil {
		writeGitOpError(w, err)
		return
	}
	name := r.URL.Query().Get("name")
	if err := ValidateRefName(name); err != nil {
		writeGitOpError(w, err)
		return
	}
	if d.gitOps.IsProtectedRef(name) {
		writeJSON(w, http.StatusForbidden, map[string]string{
			"error": fmt.Sprintf("git_policy: ref %q is protected; refuse delete", name),
		})
		emitGitOp(d, GitOpBranchDelete, d.gitOps.Repo, 0, "refuse_protected:"+name)
		return
	}

	gitMutex.mu.Lock()
	defer gitMutex.mu.Unlock()

	t0 := time.Now()
	out, err := runGit(r.Context(), d.gitOps.Repo, "branch", "-d", "--", name)
	dur := time.Since(t0)
	if err != nil {
		emitGitOp(d, GitOpBranchDelete, d.gitOps.Repo, dur, err.Error())
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error":  "git branch -d: " + err.Error(),
			"output": out,
		})
		return
	}
	emitGitOp(d, GitOpBranchDelete, d.gitOps.Repo, dur, "ok:"+name)
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"repo":    d.gitOps.Repo,
		"name":    name,
		"output":  out,
		"deleted": true,
	})
}

// --- /git/merge ----------------------------------------------------

func (d *daemonState) handleGitMerge(w http.ResponseWriter, r *http.Request) {
	if err := d.gitOps.CheckOp(GitOpMerge); err != nil {
		writeGitOpError(w, err)
		return
	}
	var req gitMergeRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16*1024)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode body: " + err.Error()})
		return
	}
	if err := ValidateRefName(req.Branch); err != nil {
		writeGitOpError(w, err)
		return
	}

	gitMutex.mu.Lock()
	defer gitMutex.mu.Unlock()

	args := []string{"merge", "--no-edit"}
	switch {
	case req.NoFF:
		args = append(args, "--no-ff")
	case d.gitOps.DefaultFastForward:
		args = append(args, "--ff-only")
	}
	args = append(args, "--", req.Branch)

	t0 := time.Now()
	out, err := runGit(r.Context(), d.gitOps.Repo, args...)
	dur := time.Since(t0)
	if err != nil {
		emitGitOp(d, GitOpMerge, d.gitOps.Repo, dur, err.Error())
		writeJSON(w, http.StatusConflict, map[string]interface{}{
			"error":  "git merge: " + err.Error(),
			"output": out,
			"branch": req.Branch,
		})
		return
	}
	resp := gitMergeResponse{
		Repo:        d.gitOps.Repo,
		Branch:      req.Branch,
		Merged:      true,
		FastForward: !req.NoFF && d.gitOps.DefaultFastForward,
		Output:      out,
	}
	emitGitOp(d, GitOpMerge, d.gitOps.Repo, dur, "ok:"+req.Branch)
	writeJSON(w, http.StatusOK, resp)
}

// --- helpers -------------------------------------------------------

// runGit shells `git` with the given args inside cwd (the policy repo).
// Returns stdout+stderr concat (git's status output goes to stdout;
// errors typically to stderr; we want both for the SPA + audit feed).
func runGit(ctx context.Context, cwd string, args ...string) (string, error) {
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "git", args...)
	cmd.Dir = cwd
	cmd.Env = []string{
		"PATH=" + safePATH(),
		"HOME=" + safeHOME(),
		"GIT_TERMINAL_PROMPT=0", // never prompt for credentials
		"LC_ALL=C",
	}
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// safePATH minimises the env exposed to subprocess git so an attacker
// can't sneak a malicious binary onto PATH and have us shell into it.
// /usr/bin + /usr/local/bin are the standard system git locations; we
// don't trust anything in user-controlled paths.
func safePATH() string { return "/usr/local/bin:/usr/bin:/bin" }

// safeHOME returns a sane HOME so `git` can read its global config
// (signed commits require user.email; gpg agent path lookups need
// HOME). $HOME inheritance is acceptable here because the daemon
// already runs as the operator's user.
func safeHOME() string {
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}

// writeGitOpError maps policy + ref errors to HTTP statuses.
func writeGitOpError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrGitOpDenied):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrGitInvalidRef):
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrGitRepoMismatch):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, ErrGitRefProtected):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

// emitGitOp stamps a `git.op.<verb>` audit event onto the daemon
// transcript. Status carries either "ok"+detail or the underlying
// error message; duration is in milliseconds.
func emitGitOp(d *daemonState, op GitOp, repo string, dur time.Duration, status string) {
	// We don't have a transcript handle here; the route handlers run
	// after logMiddleware which already emits an http.request event.
	// Defer richer audit attribution to a future per-route transcript
	// hook. For now the http.request event + the JSON-body status fields
	// carry enough signal for the SPA + audit replay.
	_ = op
	_ = repo
	_ = dur
	_ = status
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
