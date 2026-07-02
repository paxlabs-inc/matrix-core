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

	"matrix/cody/internal/llmtest"
)

func workspaceServer(t *testing.T, workspaceRoot string) *httptest.Server {
	t.Helper()
	gw := llmtest.NewServer(t, func(step int, req llmtest.Request) llmtest.Turn {
		return llmtest.Say("unused")
	})
	t.Cleanup(gw.Close)
	engine := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(engine.Close)
	srv := httptest.NewServer(New(engine).Handler())
	t.Cleanup(srv.Close)
	return srv
}

func getJSON(t *testing.T, url string, out interface{}) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusOK {
		if err := json.NewDecoder(resp.Body).Decode(out); err != nil {
			t.Fatal(err)
		}
	}
	return resp.StatusCode
}

// TestWorkspaceTreeAndFile proves the coding workspace's environment surface
// serves the REAL workspace: the tree lists real files (skipping noise dirs),
// and the file endpoint returns real content while refusing traversal.
func TestWorkspaceTreeAndFile(t *testing.T) {
	root := t.TempDir()
	for rel, content := range map[string]string{
		"main.go":            "package main\n",
		"web/app.ts":         "export {}\n",
		".cody/private.json": "{}",
	} {
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srv := workspaceServer(t, root)

	var tree struct {
		Entries []struct {
			Path string `json:"path"`
			Dir  bool   `json:"dir"`
		} `json:"entries"`
	}
	if code := getJSON(t, srv.URL+"/workspace/tree", &tree); code != 200 {
		t.Fatalf("tree = %d", code)
	}
	paths := map[string]bool{}
	for _, e := range tree.Entries {
		paths[e.Path] = true
	}
	if !paths["main.go"] || !paths["web"] || !paths["web/app.ts"] {
		t.Fatalf("tree = %+v", tree.Entries)
	}
	if paths[".cody"] || paths[".cody/private.json"] {
		t.Fatalf("internal dirs leaked into the tree: %+v", tree.Entries)
	}

	var file struct {
		Content   string `json:"content"`
		Truncated bool   `json:"truncated"`
	}
	if code := getJSON(t, srv.URL+"/workspace/file?path=main.go", &file); code != 200 {
		t.Fatalf("file = %d", code)
	}
	if file.Content != "package main\n" || file.Truncated {
		t.Fatalf("file = %+v", file)
	}

	// Traversal is refused.
	resp, err := http.Get(srv.URL + "/workspace/file?path=../../etc/passwd")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("traversal = %d, want 400", resp.StatusCode)
	}
}

// TestWorkspaceDiffAndExec proves the diff pane serves the real `git diff`
// (with untracked files listed) and the terminal endpoint runs real commands
// with honest exit codes.
func TestWorkspaceDiffAndExec(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("one\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-q"},
		{"-c", "user.name=seed", "-c", "user.email=seed@test", "add", "."},
		{"-c", "user.name=seed", "-c", "user.email=seed@test", "commit", "-q", "-m", "seed"},
	} {
		cmd := exec.Command("git", args...)
		cmd.Dir = root
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	// A real modification + a real untracked file.
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("two\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "new.txt"), []byte("fresh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	srv := workspaceServer(t, root)

	var diff struct {
		Git       bool     `json:"git"`
		Diff      string   `json:"diff"`
		Untracked []string `json:"untracked"`
	}
	if code := getJSON(t, srv.URL+"/workspace/diff", &diff); code != 200 {
		t.Fatalf("diff = %d", code)
	}
	if !diff.Git || !strings.Contains(diff.Diff, "-one") || !strings.Contains(diff.Diff, "+two") {
		t.Fatalf("diff = %+v", diff)
	}
	if len(diff.Untracked) != 1 || diff.Untracked[0] != "new.txt" {
		t.Fatalf("untracked = %v", diff.Untracked)
	}

	// The terminal runs a real command in the workspace root.
	body, _ := json.Marshal(map[string]interface{}{"cmd": "cat a.txt; exit 3"})
	resp, err := http.Post(srv.URL+"/workspace/exec", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var run struct {
		Exit   int    `json:"exit"`
		Output string `json:"output"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if run.Exit != 3 || !strings.Contains(run.Output, "two") {
		t.Fatalf("exec = %+v", run)
	}
}
