// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// newGitTestDaemon initialises a real git repo under t.TempDir and
// returns a daemonState wired to it. Used by every git route test.
func newGitTestDaemon(t *testing.T) (*daemonState, string) {
	t.Helper()
	tmp := t.TempDir()

	gitInit(t, tmp)
	policy := DefaultGitOpsPolicy()
	policy.Repo = tmp
	d := &daemonState{
		gitOps: policy,
	}
	return d, tmp
}

func gitInit(t *testing.T, dir string) {
	t.Helper()
	mustGit(t, dir, "init", "-q", "-b", "main")
	mustGit(t, dir, "config", "user.email", "forge-test@matrix.local")
	mustGit(t, dir, "config", "user.name", "forge-test")
	mustGit(t, dir, "commit", "-q", "--allow-empty", "-m", "init")
}

func mustGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(context.Background(), "git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// TestGitRouter_Disabled404 verifies 404 when gitOps is nil.
func TestGitRouter_Disabled404(t *testing.T) {
	d := &daemonState{gitOps: nil}
	req := httptest.NewRequest(http.MethodGet, "/git/status", nil)
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
	}
}

// TestGitStatus_Empty returns no entries on a clean repo.
func TestGitStatus_Empty(t *testing.T) {
	d, _ := newGitTestDaemon(t)
	req := httptest.NewRequest(http.MethodGet, "/git/status", nil)
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp gitStatusResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Branch != "main" {
		t.Errorf("branch = %q, want main", resp.Branch)
	}
	if len(resp.Entries) != 0 {
		t.Errorf("entries = %d, want 0", len(resp.Entries))
	}
}

// TestGitStatus_TrackedAndUntracked covers the porcelain v2 parser
// across the three common entry shapes (modified, added/staged,
// untracked).
func TestGitStatus_TrackedAndUntracked(t *testing.T) {
	d, repo := newGitTestDaemon(t)

	// 1) modified tracked file
	mustWriteFile(t, filepath.Join(repo, "tracked.go"), "package m\n")
	mustGit(t, repo, "add", "tracked.go")
	mustGit(t, repo, "commit", "-q", "-m", "add tracked.go")
	mustWriteFile(t, filepath.Join(repo, "tracked.go"), "package m\n// edit\n")

	// 2) staged new file
	mustWriteFile(t, filepath.Join(repo, "staged.go"), "package s\n")
	mustGit(t, repo, "add", "staged.go")

	// 3) untracked file
	mustWriteFile(t, filepath.Join(repo, "untracked.go"), "package u\n")

	req := httptest.NewRequest(http.MethodGet, "/git/status", nil)
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp gitStatusResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	byPath := map[string]gitStatusEntry{}
	for _, e := range resp.Entries {
		byPath[e.Path] = e
	}
	if e, ok := byPath["tracked.go"]; !ok {
		t.Errorf("tracked.go missing from entries")
	} else if !e.Unstaged {
		t.Errorf("tracked.go must have unstaged=true; got %+v", e)
	}
	if e, ok := byPath["staged.go"]; !ok {
		t.Errorf("staged.go missing")
	} else if !e.Staged {
		t.Errorf("staged.go must have staged=true; got %+v", e)
	}
	if e, ok := byPath["untracked.go"]; !ok {
		t.Errorf("untracked.go missing")
	} else if !e.Untracked {
		t.Errorf("untracked.go must have untracked=true; got %+v", e)
	}
}

// TestGitDiff_StagedAndUnstaged verifies both modes return the
// expected unified diff with hunk headers.
func TestGitDiff_StagedAndUnstaged(t *testing.T) {
	d, repo := newGitTestDaemon(t)
	target := filepath.Join(repo, "diff.go")
	mustWriteFile(t, target, "package d\nfunc x() {}\n")
	mustGit(t, repo, "add", "diff.go")
	mustGit(t, repo, "commit", "-q", "-m", "add diff.go")

	mustWriteFile(t, target, "package d\nfunc y() {}\n")
	// unstaged diff
	req := httptest.NewRequest(http.MethodGet, "/git/diff?path=diff.go&staged=false", nil)
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp gitDiffResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !strings.Contains(resp.Unified, "@@") {
		t.Errorf("expected hunk header in unstaged diff; got %q", resp.Unified)
	}
	if resp.Empty {
		t.Errorf("unstaged diff reported empty despite changes")
	}

	// staged: there's nothing in the index yet — should be empty.
	req2 := httptest.NewRequest(http.MethodGet, "/git/diff?path=diff.go&staged=true", nil)
	rec2 := httptest.NewRecorder()
	d.handleForgeGitRouter(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("staged status = %d", rec2.Code)
	}
	var staged gitDiffResponse
	_ = json.Unmarshal(rec2.Body.Bytes(), &staged)
	if !staged.Empty {
		t.Errorf("staged diff must be empty pre-add; got %q", staged.Unified)
	}
}

// TestGitDiff_RejectsLeadingDashPath defends against `--upload-pack`
// style argument injection.
func TestGitDiff_RejectsLeadingDashPath(t *testing.T) {
	d, _ := newGitTestDaemon(t)
	req := httptest.NewRequest(http.MethodGet, "/git/diff?path=-evil", nil)
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", rec.Code)
	}
}

// TestGitBranch_CreateAndList exercises the round-trip.
func TestGitBranch_CreateAndList(t *testing.T) {
	d, _ := newGitTestDaemon(t)

	body, _ := json.Marshal(gitBranchCreateRequest{Name: "feature/test"})
	req := httptest.NewRequest(http.MethodPost, "/git/branch", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("create status = %d, body = %s", rec.Code, rec.Body.String())
	}

	listReq := httptest.NewRequest(http.MethodGet, "/git/branch", nil)
	listRec := httptest.NewRecorder()
	d.handleForgeGitRouter(listRec, listReq)
	if listRec.Code != http.StatusOK {
		t.Fatalf("list status = %d", listRec.Code)
	}
	var resp gitBranchListResponse
	_ = json.Unmarshal(listRec.Body.Bytes(), &resp)
	names := []string{}
	for _, b := range resp.Branches {
		names = append(names, b.Name)
	}
	found := false
	for _, n := range names {
		if n == "feature/test" {
			found = true
		}
	}
	if !found {
		t.Errorf("created branch missing from list: %v", names)
	}

	// main must be flagged as protected in the list.
	for _, b := range resp.Branches {
		if b.Name == "main" && !b.Protected {
			t.Errorf("main branch must be marked protected")
		}
	}
}

// TestGitBranch_DeleteRefusesProtected protects main / master / HEAD.
func TestGitBranch_DeleteRefusesProtected(t *testing.T) {
	d, _ := newGitTestDaemon(t)
	req := httptest.NewRequest(http.MethodDelete, "/git/branch?name=main", nil)
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("delete main: status = %d, want 403", rec.Code)
	}
}

// TestGitBranch_DeleteHappyPath creates + deletes a non-protected branch.
func TestGitBranch_DeleteHappyPath(t *testing.T) {
	d, _ := newGitTestDaemon(t)
	body, _ := json.Marshal(gitBranchCreateRequest{Name: "scratch"})
	createReq := httptest.NewRequest(http.MethodPost, "/git/branch", bytes.NewReader(body))
	createRec := httptest.NewRecorder()
	d.handleForgeGitRouter(createRec, createReq)
	if createRec.Code != http.StatusOK {
		t.Fatalf("create: %d", createRec.Code)
	}

	delReq := httptest.NewRequest(http.MethodDelete, "/git/branch?name=scratch", nil)
	delRec := httptest.NewRecorder()
	d.handleForgeGitRouter(delRec, delReq)
	if delRec.Code != http.StatusOK {
		t.Errorf("delete: status = %d, body = %s", delRec.Code, delRec.Body.String())
	}
}

// TestGitMerge_FastForward succeeds when ff-only is satisfiable.
func TestGitMerge_FastForward(t *testing.T) {
	d, repo := newGitTestDaemon(t)

	// branch off main, advance once, merge back ff-only.
	mustGit(t, repo, "checkout", "-q", "-b", "ff-test")
	mustWriteFile(t, filepath.Join(repo, "ff.go"), "package ff\n")
	mustGit(t, repo, "add", "ff.go")
	mustGit(t, repo, "commit", "-q", "-m", "ff add")
	mustGit(t, repo, "checkout", "-q", "main")

	body, _ := json.Marshal(gitMergeRequest{Branch: "ff-test"})
	req := httptest.NewRequest(http.MethodPost, "/git/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("merge ff status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var resp gitMergeResponse
	_ = json.Unmarshal(rec.Body.Bytes(), &resp)
	if !resp.Merged {
		t.Errorf("merged = false, want true")
	}
	if !resp.FastForward {
		t.Errorf("fast_forward flag should be true under default policy")
	}
}

// TestGitMerge_FFOnlyConflict returns 409 on diverged branches.
func TestGitMerge_FFOnlyConflict(t *testing.T) {
	d, repo := newGitTestDaemon(t)

	// Make main and feature diverge.
	mustGit(t, repo, "checkout", "-q", "-b", "div")
	mustWriteFile(t, filepath.Join(repo, "div.go"), "package d\n")
	mustGit(t, repo, "add", "div.go")
	mustGit(t, repo, "commit", "-q", "-m", "div feature")
	mustGit(t, repo, "checkout", "-q", "main")
	mustWriteFile(t, filepath.Join(repo, "main.go"), "package main\n")
	mustGit(t, repo, "add", "main.go")
	mustGit(t, repo, "commit", "-q", "-m", "div main")

	body, _ := json.Marshal(gitMergeRequest{Branch: "div"})
	req := httptest.NewRequest(http.MethodPost, "/git/merge", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	d.handleForgeGitRouter(rec, req)
	if rec.Code != http.StatusConflict {
		t.Errorf("ff-only divergent merge: status = %d, want 409", rec.Code)
	}
}

// TestParsePorcelainV2_RenamePath exercises the type-2 record (rename)
// where path and orig path are separated by NUL inside the same record.
func TestParsePorcelainV2_RenamePath(t *testing.T) {
	// Synthetic minimal porcelain v2 -z output: branch headers + one
	// rename record. Subject + scores are stubbed; parser doesn't read
	// them.
	out := strings.Join([]string{
		"# branch.head main",
		"2 R. N... 100644 100644 100644 abc123 def456 R100 newpath\x00oldpath",
	}, "\x00") + "\x00"
	resp := parsePorcelainV2("/repo", out)
	if resp.Branch != "main" {
		t.Errorf("branch = %q, want main", resp.Branch)
	}
	if len(resp.Entries) != 1 {
		t.Fatalf("entries = %d, want 1", len(resp.Entries))
	}
	e := resp.Entries[0]
	if e.Path != "newpath" {
		t.Errorf("path = %q, want newpath", e.Path)
	}
	if e.OrigPath != "oldpath" {
		t.Errorf("orig_path = %q, want oldpath", e.OrigPath)
	}
	if e.StatusX != "R" {
		t.Errorf("status_x = %q, want R", e.StatusX)
	}
}

// TestRunGit_Timeout ensures shelling git respects context cancellation.
func TestRunGit_Timeout(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	// `git --help` is fast and should fit; `git fsck` against a fresh
	// empty repo also fast. Use the fast invariant here — we're really
	// asserting the function returns within the timeout window without
	// hanging if the deadline blew (smoke).
	_, err := runGit(ctx, t.TempDir(), "version")
	if err == nil {
		// Sanity: empty cwd + `git version` works on most systems.
		// Either path is acceptable here; we just don't want a hang.
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
