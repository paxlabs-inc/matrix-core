// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"matrix/cody/internal/llmtest"
)

func postJSON(t *testing.T, base, path, body string) (int, map[string]interface{}) {
	t.Helper()
	resp, err := http.Post(base+path, "application/json", bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestProjectRegistryCRUD proves req 2.1/2.3: create a /workspace/<dir> project
// with a mode, list it beside the synthesized default, and change its mode
// between runs.
func TestProjectRegistryCRUD(t *testing.T) {
	workspaceRoot := t.TempDir()
	e := newEngine(t, workspaceRoot, t.TempDir(), "", openCortex(t, t.TempDir()))
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	code, proj := postJSON(t, srv.URL, "/projects", `{"name":"My Shop","mode":"prototype"}`)
	if code != http.StatusCreated {
		t.Fatalf("create project = %d (%v)", code, proj)
	}
	if proj["id"] != "my-shop" || proj["mode"] != "prototype" {
		t.Fatalf("created project = %v", proj)
	}
	wantRoot, _ := filepath.Abs(filepath.Join(workspaceRoot, "my-shop"))
	if proj["root"] != wantRoot {
		t.Fatalf("project root = %v, want %s", proj["root"], wantRoot)
	}
	if fi, err := os.Stat(wantRoot); err != nil || !fi.IsDir() {
		t.Fatalf("project subtree not created: %v", err)
	}

	// List includes the synthesized default + the new project.
	resp, err := http.Get(srv.URL + "/projects")
	if err != nil {
		t.Fatal(err)
	}
	var listOut struct {
		Projects []project `json:"projects"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&listOut)
	resp.Body.Close()
	if len(listOut.Projects) != 2 || listOut.Projects[0].ID != defaultProjectID || listOut.Projects[1].ID != "my-shop" {
		t.Fatalf("project list = %+v", listOut.Projects)
	}

	// Mode is changeable between runs (req 2.3).
	code, patched := postPatch(t, srv.URL+"/projects/my-shop", `{"mode":"architect"}`)
	if code != http.StatusOK || patched["mode"] != "architect" {
		t.Fatalf("patch mode = %d (%v)", code, patched)
	}
	// The default project's mode is fixed.
	if code, _ := postPatch(t, srv.URL+"/projects/default", `{"mode":"prototype"}`); code != http.StatusBadRequest {
		t.Fatalf("patching default mode = %d, want 400", code)
	}
}

func postPatch(t *testing.T, url, body string) (int, map[string]interface{}) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPatch, url, bytes.NewReader([]byte(body)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]interface{}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return resp.StatusCode, out
}

// TestProjectScopedRunLandsInSubtree proves req 2.2/2.3: a run addressed to a
// project executes in that project's /workspace subtree (not the bare root) and
// takes the PROJECT's mode, not the per-message default.
func TestProjectScopedRunLandsInSubtree(t *testing.T) {
	workspaceRoot := t.TempDir()
	gw := llmtest.NewServer(t, gatewayScript(t, nil))
	t.Cleanup(gw.Close)
	e := newEngine(t, workspaceRoot, t.TempDir(), gw.URL, openCortex(t, t.TempDir()))
	t.Cleanup(e.Close)
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	if code, out := postJSON(t, srv.URL, "/projects", `{"name":"Demo Two","mode":"prototype"}`); code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}

	out := postChat(t, srv.URL, `{"message":"seed it","conversation_id":"conv-proj","project_id":"demo-two"}`)
	runID := out["intent_id"].(string)
	if status := waitTerminal(t, srv.URL, runID); status != "completed" {
		t.Fatalf("terminal = %q", status)
	}

	// The deliverables landed in the project subtree, not the bare workspace.
	sub := filepath.Join(workspaceRoot, "demo-two")
	for file, want := range map[string]string{"greet.txt": "hello cody\n", "reply.txt": "hello back\n"} {
		data, err := os.ReadFile(filepath.Join(sub, file))
		if err != nil || string(data) != want {
			t.Fatalf("%s in subtree = %q, %v", file, data, err)
		}
		if _, err := os.Stat(filepath.Join(workspaceRoot, file)); !os.IsNotExist(err) {
			t.Fatalf("%s leaked into the bare workspace root", file)
		}
	}
	// The run took the project's mode (prototype), not the default (engineer).
	led, err := e.readLedger("conv-proj")
	if err != nil || led.Mode != "prototype" || led.ProjectID != "demo-two" {
		t.Fatalf("ledger = %+v, %v (want project mode prototype)", led, err)
	}
}

// TestWorkspaceRoutesScopedToProject proves the read surface honors ?project=:
// the tree/file of a project shows only that subtree.
func TestWorkspaceRoutesScopedToProject(t *testing.T) {
	workspaceRoot := t.TempDir()
	e := newEngine(t, workspaceRoot, t.TempDir(), "", openCortex(t, t.TempDir()))
	srv := httptest.NewServer(New(e).Handler())
	t.Cleanup(srv.Close)

	if code, out := postJSON(t, srv.URL, "/projects", `{"name":"Scoped"}`); code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}
	// A file only in the project subtree, and a different file in the bare root.
	if err := os.WriteFile(filepath.Join(workspaceRoot, "scoped", "inproj.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspaceRoot, "bareroot.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	resp, err := http.Get(srv.URL + "/workspace/tree?project=scoped")
	if err != nil {
		t.Fatal(err)
	}
	var tree struct {
		Entries []treeEntry `json:"entries"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&tree)
	resp.Body.Close()
	var sawInProj, sawBare bool
	for _, ent := range tree.Entries {
		if ent.Path == "inproj.txt" {
			sawInProj = true
		}
		if ent.Path == "bareroot.txt" {
			sawBare = true
		}
	}
	if !sawInProj || sawBare {
		t.Fatalf("scoped tree = %+v (want inproj.txt, not bareroot.txt)", tree.Entries)
	}

	// The file route reads the project-scoped file.
	resp2, err := http.Get(srv.URL + "/workspace/file?project=scoped&path=inproj.txt")
	if err != nil {
		t.Fatal(err)
	}
	var fileOut struct {
		Content string `json:"content"`
	}
	_ = json.NewDecoder(resp2.Body).Decode(&fileOut)
	resp2.Body.Close()
	if fileOut.Content != "x" {
		t.Fatalf("scoped file content = %q", fileOut.Content)
	}
}
