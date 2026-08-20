// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func wsServer(t *testing.T) (*Server, string) {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	engine := &Engine{workspaceRoot: root}
	registry := engine.projectsRegistry()
	if registry == nil {
		t.Fatal("project registry unavailable")
	}
	if err := registry.create(project{ID: "app", Name: "App", Root: filepath.Join(root, "app")}); err != nil {
		t.Fatal(err)
	}
	return &Server{engine: engine}, root
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
	if err := s.engine.projectsRegistry().create(project{ID: "plain", Name: "Plain", Root: filepath.Join(root, "plain")}); err != nil {
		t.Fatal(err)
	}
	code, out = wsDo(t, s, http.MethodGet, "/workspace/diff?project=plain", nil)
	if code != http.StatusOK || out["git"] != false {
		t.Fatalf("non-git diff = %d (%v), want 200 git:false", code, out)
	}

	// Exec: real command, real exit code, cwd = the project root, and an exact
	// child environment that excludes protected and unknown parent values.
	t.Setenv("TAVILY_API_KEY", "workbench-hidden-sentinel")
	t.Setenv("UNREVIEWED_TOKEN", "workbench-unknown-sentinel")
	t.Setenv("MATRIX_USER_ID", "workbench-visible-sentinel")
	code, out = wsDo(t, s, http.MethodPost, "/workspace/exec?project=app", map[string]interface{}{
		"cmd": "pwd; cat a.txt; env; exit 3",
	})
	if code != http.StatusOK {
		t.Fatalf("exec = %d, want 200", code)
	}
	output, _ := out["output"].(string)
	if !strings.Contains(output, "new") {
		t.Fatalf("exec output = %q, want the project file content", output)
	}
	if !strings.Contains(output, "MATRIX_USER_ID=workbench-visible-sentinel") ||
		strings.Contains(output, "workbench-hidden-sentinel") ||
		strings.Contains(output, "workbench-unknown-sentinel") {
		t.Fatalf("exec environment policy failed: %q", output)
	}
	if out["exit"] != float64(3) || out["timed_out"] != false {
		t.Fatalf("exec exit = %v timed_out = %v, want 3/false", out["exit"], out["timed_out"])
	}
}

// TestWorkspaceDisabled proves an unconfigured workspace root answers 400
// (surface disabled) instead of touching the filesystem.
// TestWorkspaceTreeThroughSymlinkRoot reproduces the prod layout — /workspace
// is a symlink to the persisted /data/workspace — and proves the tree listing
// still sees the real files. filepath.WalkDir does not follow a symlink root,
// so an unresolved root walked zero children and prod's Files page showed an
// empty workspace over real data.
func TestWorkspaceTreeThroughSymlinkRoot(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "data", "workspace")
	if err := os.MkdirAll(filepath.Join(real, "app"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(real, "app", "main.go"), []byte("package main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "workspace")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlink unsupported here: %v", err)
	}

	root := WorkspaceRoot(link)
	if resolved, err := filepath.EvalSymlinks(real); err != nil || root != resolved {
		t.Fatalf("WorkspaceRoot(%q) = %q (%v), want the symlink-resolved %q", link, root, err, resolved)
	}

	s := &Server{engine: &Engine{workspaceRoot: root}}
	code, out := wsDo(t, s, http.MethodGet, "/workspace/tree?project=", nil)
	if code != http.StatusOK {
		t.Fatalf("tree = %d (%v), want 200", code, out)
	}
	entries, _ := out["entries"].([]interface{})
	paths := make([]string, 0, len(entries))
	for _, e := range entries {
		m, _ := e.(map[string]interface{})
		p, _ := m["path"].(string)
		paths = append(paths, p)
	}
	sort.Strings(paths)
	if len(paths) != 2 || paths[0] != "app" || paths[1] != "app/main.go" {
		t.Fatalf("tree through symlink root = %v, want [app app/main.go]", paths)
	}
	if out["truncated"] != false {
		t.Fatalf("truncated = %v, want false", out["truncated"])
	}
}

func TestWorkspaceDisabled(t *testing.T) {
	s := &Server{engine: &Engine{}}
	code, _ := wsDo(t, s, http.MethodGet, "/workspace/tree", nil)
	if code != http.StatusBadRequest {
		t.Fatalf("tree without workspace = %d, want 400", code)
	}
}

// wsUpload posts a real multipart upload (field "file" per payload entry,
// optional "dir" field) through the workspace router.
func wsUpload(t *testing.T, s *Server, target, dir string, payload map[string][]byte) (int, map[string]interface{}) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if dir != "" {
		if err := mw.WriteField("dir", dir); err != nil {
			t.Fatal(err)
		}
	}
	names := make([]string, 0, len(payload))
	for name := range payload {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		part, err := mw.CreateFormFile("file", name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := part.Write(payload[name]); err != nil {
			t.Fatal(err)
		}
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, target, &buf)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestWorkspaceUploadAndRawDownload proves the Files-page backend end to end
// against a real workspace directory: a multipart upload lands the EXACT bytes
// (binary-safe, non-UTF-8) atomically in the requested subdirectory, and the
// raw route streams those exact bytes back — attachment + nosniff for
// non-media (stored-XSS posture), inline for images.
func TestWorkspaceUploadAndRawDownload(t *testing.T) {
	s, root := wsServer(t)
	blob := []byte{0x00, 0xff, 0xfe, 'n', 'e', 'o', 0x00, 0x89, 'P', 'N', 'G'}

	code, out := wsUpload(t, s, "/workspace/upload?project=app", "docs/in", map[string][]byte{
		"report.bin": blob,
		"pic.png":    []byte("not-really-png-but-bytes"),
	})
	if code != http.StatusCreated {
		t.Fatalf("upload = %d (%v), want 201", code, out)
	}
	b, _ := json.Marshal(out["files"])
	for _, want := range []string{`"docs/in/pic.png"`, `"docs/in/report.bin"`} {
		if !strings.Contains(string(b), want) {
			t.Fatalf("upload response = %s, want %s", b, want)
		}
	}
	onDisk, err := os.ReadFile(filepath.Join(root, "app", "docs", "in", "report.bin"))
	if err != nil || !bytes.Equal(onDisk, blob) {
		t.Fatalf("disk = %x (%v), want the exact uploaded bytes", onDisk, err)
	}
	// No temp residue from the atomic write path.
	entries, _ := os.ReadDir(filepath.Join(root, "app", "docs", "in"))
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".neo-upload-") {
			t.Fatalf("temp residue %q left behind", e.Name())
		}
	}

	// Raw download: exact bytes back, attachment + nosniff for a .bin.
	req := httptest.NewRequest(http.MethodGet, "/workspace/raw?project=app&path=docs/in/report.bin", nil)
	rec := httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusOK || !bytes.Equal(rec.Body.Bytes(), blob) {
		t.Fatalf("raw = %d body %x, want 200 with the exact bytes", rec.Code, rec.Body.Bytes())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, `attachment; filename="report.bin"`) {
		t.Fatalf("raw disposition = %q, want attachment", cd)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("raw .bin response missing nosniff")
	}

	// An image serves inline (thumbnails) with its real content type.
	req = httptest.NewRequest(http.MethodGet, "/workspace/raw?project=app&path=docs/in/pic.png", nil)
	rec = httptest.NewRecorder()
	s.handleWorkspace(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("raw png = %d type %q, want 200 image/png", rec.Code, rec.Header().Get("Content-Type"))
	}
	if rec.Header().Get("Content-Disposition") != "" {
		t.Fatalf("raw png disposition = %q, want inline (none)", rec.Header().Get("Content-Disposition"))
	}

	// A directory is not downloadable.
	if code, _ := wsDo(t, s, http.MethodGet, "/workspace/raw?project=app&path=docs", nil); code != http.StatusNotFound {
		t.Fatalf("raw dir = %d, want 404", code)
	}
}

// TestWorkspaceUploadAndRawEscapesRejected proves the new routes ride the same
// escape seam as every other workspace handler: traversal via ?path=, the
// upload dir field, and the uploaded filename itself never reach outside the
// project root.
func TestWorkspaceUploadAndRawEscapesRejected(t *testing.T) {
	s, root := wsServer(t)
	if err := os.WriteFile(filepath.Join(root, "secret.txt"), []byte("outside"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, rel := range []string{"../secret.txt", "a/../../secret.txt", "/etc/passwd"} {
		if code, _ := wsDo(t, s, http.MethodGet, "/workspace/raw?project=app&path="+rel, nil); code != http.StatusBadRequest {
			t.Errorf("raw %q = %d, want 400", rel, code)
		}
	}
	if code, _ := wsUpload(t, s, "/workspace/upload?project=app", "../", map[string][]byte{"x.txt": []byte("x")}); code != http.StatusBadRequest {
		t.Errorf("upload dir=../ = %d, want 400", code)
	}
	// A traversal-shaped filename is sanitized to its base name — it can only
	// ever land inside the target directory.
	code, out := wsUpload(t, s, "/workspace/upload?project=app", "", map[string][]byte{"../evil.txt": []byte("x")})
	if code != http.StatusCreated {
		t.Fatalf("upload base-named file = %d (%v), want 201", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "evil.txt")); !os.IsNotExist(err) {
		t.Fatal("traversal filename escaped the project root")
	}
	if _, err := os.Stat(filepath.Join(root, "app", "evil.txt")); err != nil {
		t.Fatalf("sanitized upload missing from the project root: %v", err)
	}
	// A pure-dot filename cannot be sanitized into anything — rejected.
	if code, _ := wsUpload(t, s, "/workspace/upload?project=app", "", map[string][]byte{"..": []byte("x")}); code != http.StatusBadRequest {
		t.Errorf("upload filename '..' = %d, want 400", code)
	}
}
