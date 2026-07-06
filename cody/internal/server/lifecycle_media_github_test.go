// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func doJSON(t *testing.T, method, url, body string) (int, map[string]interface{}) {
	t.Helper()
	var rd io.Reader
	if body != "" {
		rd = bytes.NewReader([]byte(body))
	}
	req, err := http.NewRequest(method, url, rd)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestProjectRenameArchiveDelete proves the project lifecycle surface: PATCH
// renames and archives a registry project, DELETE removes it (files kept by
// default, purged with ?purge=true), and the guards hold (empty patch, unknown
// project, purge stays inside the workspace).
func TestProjectRenameArchiveDelete(t *testing.T) {
	workspaceRoot := t.TempDir()
	e := newEngine(t, workspaceRoot, t.TempDir(), "", openCortex(t, t.TempDir()))
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	if code, out := postJSON(t, srv.URL, "/projects", `{"name":"Keep Me","mode":"engineer"}`); code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}
	sub := filepath.Join(workspaceRoot, "keep-me")
	if err := os.WriteFile(filepath.Join(sub, "code.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Rename.
	code, out := doJSON(t, http.MethodPatch, srv.URL+"/projects/keep-me", `{"name":"Kept"}`)
	if code != http.StatusOK || out["name"] != "Kept" {
		t.Fatalf("rename = %d (%v)", code, out)
	}
	// Archive; the flag persists through the list.
	code, out = doJSON(t, http.MethodPatch, srv.URL+"/projects/keep-me", `{"archived":true}`)
	if code != http.StatusOK || out["archived"] != true {
		t.Fatalf("archive = %d (%v)", code, out)
	}
	if p, ok := e.projects.get("keep-me"); !ok || !p.Archived || p.Name != "Kept" {
		t.Fatalf("registry after patch = %+v, %v", p, ok)
	}
	// Un-archive.
	if code, out = doJSON(t, http.MethodPatch, srv.URL+"/projects/keep-me", `{"archived":false}`); code != http.StatusOK || out["archived"] == true {
		t.Fatalf("unarchive = %d (%v)", code, out)
	}
	// An empty patch is rejected honestly.
	if code, _ = doJSON(t, http.MethodPatch, srv.URL+"/projects/keep-me", `{}`); code != http.StatusBadRequest {
		t.Fatalf("empty patch = %d, want 400", code)
	}

	// Delete WITHOUT purge: registry entry gone, files stay.
	code, out = doJSON(t, http.MethodDelete, srv.URL+"/projects/keep-me", "")
	if code != http.StatusOK || out["deleted"] != true || out["purged"] != false {
		t.Fatalf("delete = %d (%v)", code, out)
	}
	if _, ok := e.projects.get("keep-me"); ok {
		t.Fatal("registry entry survived the delete")
	}
	if _, err := os.Stat(filepath.Join(sub, "code.txt")); err != nil {
		t.Fatalf("files should survive a non-purge delete: %v", err)
	}

	// Delete WITH purge removes the subtree.
	if code, out = postJSON(t, srv.URL, "/projects", `{"name":"Purge Me"}`); code != http.StatusCreated {
		t.Fatalf("create purge-me = %d (%v)", code, out)
	}
	purgeSub := filepath.Join(workspaceRoot, "purge-me")
	if err := os.WriteFile(filepath.Join(purgeSub, "junk.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, out = doJSON(t, http.MethodDelete, srv.URL+"/projects/purge-me?purge=true", ""); code != http.StatusOK || out["purged"] != true {
		t.Fatalf("purge delete = %d (%v)", code, out)
	}
	if _, err := os.Stat(purgeSub); !os.IsNotExist(err) {
		t.Fatalf("purged subtree still exists: %v", err)
	}

	// Unknown project (and the never-stored default) 404 on delete.
	if code, _ = doJSON(t, http.MethodDelete, srv.URL+"/projects/nope", ""); code != http.StatusNotFound {
		t.Fatalf("delete unknown = %d, want 404", code)
	}
	if code, _ = doJSON(t, http.MethodDelete, srv.URL+"/projects/default", ""); code != http.StatusNotFound {
		t.Fatalf("delete default = %d, want 404", code)
	}
}

// TestPurgeProjectRootGuard proves the purge path refuses anything that is not
// a strict workspace subdirectory (the whole-volume protection).
func TestPurgeProjectRootGuard(t *testing.T) {
	workspaceRoot := t.TempDir()
	e := newEngine(t, workspaceRoot, t.TempDir(), "", openCortex(t, t.TempDir()))
	for _, root := range []string{workspaceRoot, filepath.Dir(workspaceRoot), "/", filepath.Join(workspaceRoot, "..", "outside")} {
		if err := e.purgeProjectRoot(root); err == nil {
			t.Fatalf("purgeProjectRoot(%q) accepted a non-subdirectory", root)
		}
	}
	sub := filepath.Join(workspaceRoot, "ok")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := e.purgeProjectRoot(sub); err != nil {
		t.Fatalf("purgeProjectRoot(subdir) = %v", err)
	}
}

// TestUploadAndServeMedia proves the media plane round-trip: a multipart PNG
// upload lands on the machine volume and streams back from /media/<name> with
// the right content type; unsupported kinds and traversal names are refused.
func TestUploadAndServeMedia(t *testing.T) {
	e := newEngine(t, t.TempDir(), t.TempDir(), "", openCortex(t, t.TempDir()))
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	png := []byte{0x89, 'P', 'N', 'G', 0x0d, 0x0a, 0x1a, 0x0a, 1, 2, 3}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	fw, err := mw.CreateFormFile("file", "shot.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fw.Write(png); err != nil {
		t.Fatal(err)
	}
	mw.Close()

	resp, err := http.Post(srv.URL+"/upload", mw.FormDataContentType(), &buf)
	if err != nil {
		t.Fatal(err)
	}
	var up struct {
		URL   string `json:"url"`
		Kind  string `json:"kind"`
		Mime  string `json:"mime"`
		Bytes int64  `json:"bytes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&up); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || up.Kind != "image" || up.Mime != "image/png" || up.Bytes != int64(len(png)) {
		t.Fatalf("upload = %d %+v", resp.StatusCode, up)
	}
	if !strings.HasPrefix(up.URL, "/media/") {
		t.Fatalf("upload url = %q", up.URL)
	}

	// The bytes stream back with the right type.
	got, err := http.Get(srv.URL + up.URL)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(got.Body)
	got.Body.Close()
	if got.StatusCode != http.StatusOK || !bytes.Equal(body, png) || got.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("serve = %d type=%q len=%d", got.StatusCode, got.Header.Get("Content-Type"), len(body))
	}

	// The file landed on the derived <volume>/media plane.
	name := strings.TrimPrefix(up.URL, "/media/")
	if _, err := os.Stat(filepath.Join(e.mediaDir(), name)); err != nil {
		t.Fatalf("upload not on the media plane: %v", err)
	}

	// Unsupported kind is refused honestly.
	var buf2 bytes.Buffer
	mw2 := multipart.NewWriter(&buf2)
	fw2, _ := mw2.CreateFormFile("file", "evil.exe")
	_, _ = fw2.Write([]byte("MZ"))
	mw2.Close()
	resp2, err := http.Post(srv.URL+"/upload", mw2.FormDataContentType(), &buf2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("exe upload = %d, want 415", resp2.StatusCode)
	}

	// Traversal names never resolve.
	tr, err := http.Get(srv.URL + "/media/..%2Fgithub.json")
	if err != nil {
		t.Fatal(err)
	}
	tr.Body.Close()
	if tr.StatusCode != http.StatusBadRequest && tr.StatusCode != http.StatusNotFound {
		t.Fatalf("traversal read = %d", tr.StatusCode)
	}
}

// TestGithubLinkStatusRepos proves the token surface against a real HTTP
// boundary: link validates via /user and persists 0600 on the volume; status
// reports the login without the token; repos lists; unlink clears.
func TestGithubLinkStatusRepos(t *testing.T) {
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok-ok" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/user":
			_ = json.NewEncoder(w).Encode(map[string]string{"login": "octocat"})
		case "/user/repos":
			_ = json.NewEncoder(w).Encode([]map[string]interface{}{
				{"full_name": "octocat/hello", "name": "hello", "private": false, "default_branch": "main",
					"clone_url": "https://github.com/octocat/hello.git"},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(gh.Close)
	oldAPI := githubAPI
	githubAPI = gh.URL
	t.Cleanup(func() { githubAPI = oldAPI })

	dataDir := t.TempDir()
	e := newEngine(t, t.TempDir(), dataDir, "", openCortex(t, t.TempDir()))
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	// A bad token is rejected and nothing persists.
	if code, out := postJSON(t, srv.URL, "/github/link", `{"token":"bad"}`); code != http.StatusBadRequest {
		t.Fatalf("bad link = %d (%v)", code, out)
	}
	if _, ok := e.loadGithub(); ok {
		t.Fatal("bad token persisted")
	}

	// A valid token links, is stored 0600, and never echoes back.
	code, out := postJSON(t, srv.URL, "/github/link", `{"token":"tok-ok"}`)
	if code != http.StatusOK || out["login"] != "octocat" {
		t.Fatalf("link = %d (%v)", code, out)
	}
	info, err := os.Stat(filepath.Join(dataDir, "github.json"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("token file mode = %v, %v", info, err)
	}
	code, out = doJSON(t, http.MethodGet, srv.URL+"/github/status", "")
	if code != http.StatusOK || out["linked"] != true || out["login"] != "octocat" {
		t.Fatalf("status = %d (%v)", code, out)
	}
	if _, hasToken := out["token"]; hasToken {
		t.Fatal("status leaked the token")
	}

	// Repos list flows through.
	code, out = doJSON(t, http.MethodGet, srv.URL+"/github/repos", "")
	if code != http.StatusOK {
		t.Fatalf("repos = %d (%v)", code, out)
	}
	repos, _ := out["repos"].([]interface{})
	if len(repos) != 1 {
		t.Fatalf("repos = %v", out)
	}

	// Unlink clears the record; repos then demands a link.
	if code, _ = doJSON(t, http.MethodDelete, srv.URL+"/github/link", ""); code != http.StatusOK {
		t.Fatalf("unlink = %d", code)
	}
	if code, _ = doJSON(t, http.MethodGet, srv.URL+"/github/repos", ""); code != http.StatusPreconditionFailed {
		t.Fatalf("repos after unlink = %d, want 412", code)
	}
}

// TestResolveCloneTarget pins the clone-target derivation (owner/repo,
// https URL forms, rejects non-github and malformed inputs).
func TestResolveCloneTarget(t *testing.T) {
	cases := []struct {
		fullName, url string
		wantURL       string
		wantName      string
		wantErr       bool
	}{
		{fullName: "octocat/hello", wantURL: "https://github.com/octocat/hello.git", wantName: "hello"},
		{url: "https://github.com/octocat/hello", wantURL: "https://github.com/octocat/hello.git", wantName: "hello"},
		{url: "https://github.com/octocat/hello.git", wantURL: "https://github.com/octocat/hello.git", wantName: "hello"},
		{fullName: "justname", wantErr: true},
		{url: "http://github.com/a/b", wantErr: true},
		{url: "https://evil.com/a/b", wantErr: true},
		{wantErr: true},
	}
	for _, c := range cases {
		gotURL, gotName, err := resolveCloneTarget(c.fullName, c.url)
		if c.wantErr {
			if err == nil {
				t.Fatalf("resolveCloneTarget(%q,%q) accepted", c.fullName, c.url)
			}
			continue
		}
		if err != nil || gotURL != c.wantURL || gotName != c.wantName {
			t.Fatalf("resolveCloneTarget(%q,%q) = %q,%q,%v", c.fullName, c.url, gotURL, gotName, err)
		}
	}
}

// TestScrubToken pins the defense-in-depth scrub.
func TestScrubToken(t *testing.T) {
	if got := scrubToken("fatal: auth tok-secret failed", "tok-secret"); strings.Contains(got, "tok-secret") {
		t.Fatalf("scrub failed: %q", got)
	}
	if got := scrubToken("plain", ""); got != "plain" {
		t.Fatalf("empty-token scrub = %q", got)
	}
}
