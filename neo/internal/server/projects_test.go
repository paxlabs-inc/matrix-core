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
)

// NEO-WORKBENCH task 1.3 (req 8.1, 1.3): the minimal project registry over
// real workspace subdirectories, and the server-backed per-project
// conversation history. Everything real: real handlers, a real workspace
// tree on disk, the real registry JSON, the real conversation store.

func projDo(t *testing.T, s *Server, method, target string, body interface{}) (int, map[string]interface{}) {
	t.Helper()
	var rd *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	} else {
		rd = bytes.NewReader(nil)
	}
	req := httptest.NewRequest(method, target, rd)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	out := map[string]interface{}{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func projServer(t *testing.T, root string) *Server {
	t.Helper()
	e := NewEngine(EngineOptions{
		ConversationDir: t.TempDir(),
		WorkspaceDir:    root,
		BackendURL:      "http://127.0.0.1:1",
	})
	t.Cleanup(e.Close)
	s, err := New(e, "http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestProjectsRegistryRoundTrip proves create → list → get → rename →
// archive → delete(+purge) against the real registry and real workspace
// directories.
func TestProjectsRegistryRoundTrip(t *testing.T) {
	root := t.TempDir()
	s := projServer(t, root)

	// Create: derives a slug dir, makes the real subtree.
	code, out := projDo(t, s, http.MethodPost, "/projects", map[string]string{"name": "My Shop App"})
	if code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, out)
	}
	id, _ := out["id"].(string)
	if id != "my-shop-app" {
		t.Fatalf("id = %q, want my-shop-app", id)
	}
	if _, exposed := out["root"]; exposed {
		t.Fatal("project response exposed its private absolute root")
	}
	if info, err := os.Stat(filepath.Join(root, id)); err != nil || !info.IsDir() {
		t.Fatalf("project subtree missing: %v", err)
	}

	// Duplicate create conflicts.
	if code, _ := projDo(t, s, http.MethodPost, "/projects", map[string]string{"name": "My Shop App"}); code != http.StatusConflict {
		t.Fatalf("duplicate create = %d, want 409", code)
	}

	// List: default project first, then the created one.
	code, out = projDo(t, s, http.MethodGet, "/projects", nil)
	if code != http.StatusOK {
		t.Fatalf("list = %d", code)
	}
	lb, _ := json.Marshal(out["projects"])
	var list []map[string]interface{}
	_ = json.Unmarshal(lb, &list)
	if len(list) != 2 || list[0]["id"] != "default" || list[1]["id"] != "my-shop-app" {
		t.Fatalf("list = %s", lb)
	}

	// Rename + archive via PATCH.
	code, out = projDo(t, s, http.MethodPatch, "/projects/my-shop-app", map[string]interface{}{
		"name": "Shop", "archived": true,
	})
	if code != http.StatusOK || out["name"] != "Shop" || out["archived"] != true {
		t.Fatalf("patch = %d (%v)", code, out)
	}
	// Registry survives across a fresh engine (durable JSON).
	s2 := projServer(t, root)
	code, out = projDo(t, s2, http.MethodGet, "/projects/my-shop-app", nil)
	if code != http.StatusOK || out["name"] != "Shop" {
		t.Fatalf("reload get = %d (%v)", code, out)
	}

	// Delete with purge removes the record and the real subtree.
	if err := os.WriteFile(filepath.Join(root, "my-shop-app", "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out = projDo(t, s2, http.MethodDelete, "/projects/my-shop-app?purge=true", nil)
	if code != http.StatusOK || out["purged"] != true {
		t.Fatalf("delete = %d (%v)", code, out)
	}
	if _, err := os.Stat(filepath.Join(root, "my-shop-app")); !os.IsNotExist(err) {
		t.Fatal("purged subtree still exists")
	}
	// The default project is synthesized and not deletable.
	if code, _ := projDo(t, s2, http.MethodDelete, "/projects/default", nil); code != http.StatusNotFound {
		t.Fatalf("delete default = %d, want 404", code)
	}
}

func TestProjectSaveForkAndReopen(t *testing.T) {
	root := t.TempDir()
	s := projServer(t, root)
	code, created := projDo(t, s, http.MethodPost, "/projects", map[string]string{"name": "Source App"})
	if code != http.StatusCreated {
		t.Fatalf("create = %d (%v)", code, created)
	}
	source := filepath.Join(root, "source-app")
	if err := os.MkdirAll(filepath.Join(source, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, "src", "main.ts"), []byte("export const value = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(source, ".env"), []byte("PRIVATE=value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	code, saved := projDo(t, s, http.MethodPost, "/projects/source-app/save", nil)
	if code != http.StatusOK || saved["saved"] != true {
		t.Fatalf("save = %d (%v)", code, saved)
	}
	code, forked := projDo(t, s, http.MethodPost, "/projects/source-app/fork", map[string]string{
		"name": "Independent App", "dir": "independent-app",
	})
	if code != http.StatusCreated || forked["independent"] != true {
		t.Fatalf("fork = %d (%v)", code, forked)
	}
	forkRoot := filepath.Join(root, "independent-app")
	content, err := os.ReadFile(filepath.Join(forkRoot, "src", "main.ts"))
	if err != nil || string(content) != "export const value = 1\n" {
		t.Fatalf("fork content = %q, %v", content, err)
	}
	if _, err := os.Stat(filepath.Join(forkRoot, ".env")); !os.IsNotExist(err) {
		t.Fatal("fork copied a dotenv credential file")
	}
	if err := os.WriteFile(filepath.Join(forkRoot, "src", "main.ts"), []byte("fork only\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(filepath.Join(source, "src", "main.ts"))
	if err != nil || string(original) != "export const value = 1\n" {
		t.Fatalf("source changed after fork edit: %q, %v", original, err)
	}
	code, reopened := projDo(t, s, http.MethodPost, "/projects/independent-app/reopen", nil)
	if code != http.StatusOK || reopened["reopened"] != true {
		t.Fatalf("reopen = %d (%v)", code, reopened)
	}
	projectJSON, _ := json.Marshal(reopened["project"])
	if bytes.Contains(projectJSON, []byte(root)) || bytes.Contains(projectJSON, []byte(`"root"`)) {
		t.Fatalf("reopen exposed private root: %s", projectJSON)
	}
	registryInfo, err := os.Stat(filepath.Join(root, ".neo", "projects.json"))
	if err != nil {
		t.Fatal(err)
	}
	if registryInfo.Mode().Perm() != 0o600 {
		t.Fatalf("registry mode = %o, want 600", registryInfo.Mode().Perm())
	}
}

// TestConversationsFilterByProject proves the server-backed per-project
// history: real stored conversations with project tags, listed via the real
// GET /conversations?project= handler.
func TestConversationsFilterByProject(t *testing.T) {
	root := t.TempDir()
	s := projServer(t, root)
	e := s.engine

	e.conv.AppendUser("conv_a", "run_a", "build the shop")
	e.conv.SetProject("conv_a", "shop")
	e.conv.AppendUser("conv_b", "run_b", "fix the blog")
	e.conv.SetProject("conv_b", "blog")
	e.conv.AppendUser("conv_c", "run_c", "dashboard chat with no project")

	get := func(target string) []map[string]interface{} {
		t.Helper()
		code, out := projDo(t, s, http.MethodGet, target, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s = %d", target, code)
		}
		b, _ := json.Marshal(out["items"])
		var items []map[string]interface{}
		_ = json.Unmarshal(b, &items)
		return items
	}

	all := get("/conversations")
	if len(all) != 3 {
		t.Fatalf("unfiltered list = %d items, want 3", len(all))
	}
	shop := get("/conversations?project=shop")
	if len(shop) != 1 || shop[0]["conversation_id"] != "conv_a" || shop[0]["project"] != "shop" {
		t.Fatalf("shop filter = %v", shop)
	}
	blog := get("/conversations?project=blog")
	if len(blog) != 1 || blog[0]["conversation_id"] != "conv_b" {
		t.Fatalf("blog filter = %v", blog)
	}
	if none := get("/conversations?project=nothing"); len(none) != 0 {
		t.Fatalf("unknown project filter = %v, want empty", none)
	}
}
