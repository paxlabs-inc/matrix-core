// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func wsServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &Server{engine: &Engine{workspaceRoot: root}}, root
}

func wsDo(t *testing.T, s *Server, method, target string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rd)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestWorkspaceReadWriteRoundTrip proves the editable-editor backend against a
// real workspace directory: an atomic write lands the exact bytes on disk and
// returns a version hash, a read returns the same content + hash, a save
// against the current hash succeeds, and a save against a stale hash is a 409
// that leaves the newer bytes untouched (concurrent-write staleness is
// DETECTABLE, req 4.3).
func TestWorkspaceReadWriteRoundTrip(t *testing.T) {
	s, root := wsServer(t)

	code, out := wsDo(t, s, http.MethodPut, "/workspace/file?project=app", map[string]string{
		"path": "src/main.go", "content": "package main\n",
	})
	if code != http.StatusOK {
		t.Fatalf("write = %d (%v), want 200", code, out)
	}
	h1, _ := out["hash"].(string)
	if h1 == "" {
		t.Fatal("write returned no hash")
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "app", "src", "main.go"))
	if err != nil || string(onDisk) != "package main\n" {
		t.Fatalf("disk = %q (%v), want the written bytes", onDisk, err)
	}

	code, out = wsDo(t, s, http.MethodGet, "/workspace/file?project=app&path=src/main.go", nil)
	if code != http.StatusOK {
		t.Fatalf("read = %d (%v), want 200", code, out)
	}
	if out["content"] != "package main\n" || out["hash"] != h1 {
		t.Fatalf("read = %v, want content+hash of the write", out)
	}
	if out["path"] != "src/main.go" {
		t.Fatalf("read path = %v, want normalized rel path", out["path"])
	}

	// Save against the current version: accepted, new hash.
	code, out = wsDo(t, s, http.MethodPut, "/workspace/file?project=app", map[string]string{
		"path": "src/main.go", "content": "package main // v2\n", "base_hash": h1,
	})
	if code != http.StatusOK {
		t.Fatalf("versioned write = %d (%v), want 200", code, out)
	}
	h2, _ := out["hash"].(string)
	if h2 == "" || h2 == h1 {
		t.Fatalf("versioned write hash = %q, want a new version", h2)
	}

	// Save against the now-stale version: 409 carrying the CURRENT hash, and
	// the newer bytes stay on disk (no silent clobber).
	code, out = wsDo(t, s, http.MethodPut, "/workspace/file?project=app", map[string]string{
		"path": "src/main.go", "content": "old buffer\n", "base_hash": h1,
	})
	if code != http.StatusConflict || out["stale"] != true || out["hash"] != h2 {
		t.Fatalf("stale write = %d (%v), want 409 stale with current hash", code, out)
	}
	onDisk, _ = os.ReadFile(filepath.Join(root, "app", "src", "main.go"))
	if string(onDisk) != "package main // v2\n" {
		t.Fatalf("disk after stale write = %q, want v2 untouched", onDisk)
	}

	// Tree lists the real structure.
	code, out = wsDo(t, s, http.MethodGet, "/workspace/tree?project=app", nil)
	if code != http.StatusOK {
		t.Fatalf("tree = %d, want 200", code)
	}
	b, _ := json.Marshal(out["entries"])
	if !strings.Contains(string(b), `"src/main.go"`) || !strings.Contains(string(b), `"src"`) {
		t.Fatalf("tree = %s, want src + src/main.go", b)
	}
}

// TestWorkspaceEscapeRejected proves the single path seam refuses every
// workspace escape — relative traversal, absolute out-of-root, project-param
// traversal — while an absolute IN-root path normalizes fine (req 4.2).
func TestWorkspaceEscapeRejected(t *testing.T) {
	s, root := wsServer(t)
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"../secret.txt", "a/../../secret.txt", "/etc/passwd"} {
		code, _ := wsDo(t, s, http.MethodGet, "/workspace/file?project=app&path="+rel, nil)
		if code != http.StatusBadRequest {
			t.Errorf("read %q = %d, want 400", rel, code)
		}
		code, _ = wsDo(t, s, http.MethodPut, "/workspace/file?project=app", map[string]string{
			"path": rel, "content": "x",
		})
		if code != http.StatusBadRequest {
			t.Errorf("write %q = %d, want 400", rel, code)
		}
	}
	// Project-param traversal never resolves outside the workspace root.
	if code, _ := wsDo(t, s, http.MethodGet, "/workspace/tree?project=../", nil); code != http.StatusBadRequest {
		t.Errorf("tree project=../ = %d, want 400", code)
	}
	// Absolute in-root path normalizes to the same file as its relative form.
	abs := filepath.Join(root, "app", "in.txt")
	code, out := wsDo(t, s, http.MethodPut, "/workspace/file?project=app", map[string]string{
		"path": abs, "content": "in-root",
	})
	if code != http.StatusOK || out["path"] != "in.txt" {
		t.Fatalf("absolute in-root write = %d (%v), want 200 with rel path", code, out)
	}
}

// TestWorkspaceDiffAndExec proves the diff pane serves the real `git diff`
// over a real repo and the terminal runs a real bounded command with an honest
// exit code (req 4.1, 4.4). Runs in a hermetic temp dir.
func TestWorkspaceDiffAndExec(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	s, root := wsServer(t)
	proj := filepath.Join(root, "app")
	git := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = proj
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(proj, "a.txt"), []byte("old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("init", "-q")
	git("add", ".")
	git("commit", "-q", "-m", "seed")
	if err := os.WriteFile(filepath.Join(proj, "a.txt"), []byte("new\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(proj, "fresh.txt"), []byte("hi\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, out := wsDo(t, s, http.MethodGet, "/workspace/diff?project=app", nil)
	if code != http.StatusOK || out["git"] != true {
		t.Fatalf("diff = %d (%v), want 200 git:true", code, out)
	}
	diff, _ := out["diff"].(string)
	if !strings.Contains(diff, "-old") || !strings.Contains(diff, "+new") {
		t.Fatalf("diff = %q, want the real change", diff)
	}
	ub, _ := json.Marshal(out["untracked"])
	if !strings.Contains(string(ub), "fresh.txt") {
		t.Fatalf("untracked = %s, want fresh.txt", ub)
	}

	// Non-git project degrades to an empty diff, not an error.
	if err := os.MkdirAll(filepath.Join(root, "plain"), 0o755); err != nil {
		t.Fatal(err)
	}
	code, out = wsDo(t, s, http.MethodGet, "/workspace/diff?project=plain", nil)
	if code != http.StatusOK || out["git"] != false {
		t.Fatalf("non-git diff = %d (%v), want 200 git:false", code, out)
	}

	// Exec: real command, real exit code, cwd = the project root.
	code, out = wsDo(t, s, http.MethodPost, "/workspace/exec?project=app", map[string]interface{}{
		"cmd": "pwd; cat a.txt; exit 3",
	})
	if code != http.StatusOK {
		t.Fatalf("exec = %d, want 200", code)
	}
	output, _ := out["output"].(string)
	if !strings.Contains(output, "new") {
		t.Fatalf("exec output = %q, want the project file content", output)
	}
	if out["exit"] != float64(3) || out["timed_out"] != false {
		t.Fatalf("exec exit = %v timed_out = %v, want 3/false", out["exit"], out["timed_out"])
	}
}

// TestWorkspaceDisabled proves an unconfigured workspace root answers 400
// (surface disabled) instead of touching the filesystem.
func TestWorkspaceDisabled(t *testing.T) {
	s := &Server{engine: &Engine{}}
	code, _ := wsDo(t, s, http.MethodGet, "/workspace/tree", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("tree without workspace = %d, want 400", code)
	}
}
