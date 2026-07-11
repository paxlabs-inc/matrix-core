// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// Projects (NEO-WORKBENCH task 1.3): each project is a workspace subdirectory
// with a minimal registry entry on the Neo daemon — replacing codyd's project
// routes. The bare workspace root is the synthesized "default" project, so a
// request that names no project behaves exactly like the fs/exec tools'
// default posture. Conversations are tagged with a project id (see
// conversation.Store.SetProject) so Workspace and History scope per project.

// defaultProjectID names the bare workspace-root project.
const defaultProjectID = "default"

// project is one registry entry: a named workspace subtree.
type project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Root      string    `json:"root"` // absolute workspace subtree
	Archived  bool      `json:"archived,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// projectRegistry persists the project map as JSON under the workspace root's
// .neo dir (self-contained on the machine volume; .neo is tree-skipped). The
// default project is synthesized, never stored, so it always exists.
type projectRegistry struct {
	path string
	mu   sync.Mutex
}

// projectsRegistryPath places the registry file for a workspace root.
func projectsRegistryPath(workspaceRoot string) string {
	if workspaceRoot == "" {
		return ""
	}
	return filepath.Join(workspaceRoot, ".neo", "projects.json")
}

func (pr *projectRegistry) loadLocked() map[string]project {
	m := map[string]project{}
	if pr.path == "" {
		return m
	}
	data, err := os.ReadFile(pr.path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func (pr *projectRegistry) saveLocked(m map[string]project) error {
	if pr.path == "" {
		return fmt.Errorf("projects registry is not configured")
	}
	if err := os.MkdirAll(filepath.Dir(pr.path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := pr.path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, pr.path)
}

func (pr *projectRegistry) get(id string) (project, bool) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	p, ok := pr.loadLocked()[id]
	return p, ok
}

func (pr *projectRegistry) list() []project {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	m := pr.loadLocked()
	out := make([]project, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (pr *projectRegistry) create(p project) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	m := pr.loadLocked()
	if _, ok := m[p.ID]; ok {
		return fmt.Errorf("project %q already exists", p.ID)
	}
	m[p.ID] = p
	return pr.saveLocked(m)
}

// patch applies fn to an existing project record and persists the result.
func (pr *projectRegistry) patch(id string, fn func(*project)) (project, error) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	m := pr.loadLocked()
	p, ok := m[id]
	if !ok {
		return project{}, fmt.Errorf("unknown project %q", id)
	}
	fn(&p)
	m[id] = p
	if err := pr.saveLocked(m); err != nil {
		return project{}, err
	}
	return p, nil
}

func (pr *projectRegistry) remove(id string) (project, error) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	m := pr.loadLocked()
	p, ok := m[id]
	if !ok {
		return project{}, fmt.Errorf("unknown project %q", id)
	}
	delete(m, id)
	if err := pr.saveLocked(m); err != nil {
		return project{}, err
	}
	return p, nil
}

// --- engine seams ---------------------------------------------------------

// projectsRegistry lazily builds the registry rooted at the engine's
// workspace root (nil when the workbench surface is disabled).
func (e *Engine) projectsRegistry() *projectRegistry {
	e.projectsOnce.Do(func() {
		if e.workspaceRoot != "" {
			e.projects = &projectRegistry{path: projectsRegistryPath(e.workspaceRoot)}
		}
	})
	return e.projects
}

// resolveProjectRecord resolves a project id; empty / "default" synthesizes
// the bare workspace-root project.
func (e *Engine) resolveProjectRecord(id string) (project, error) {
	if e.workspaceRoot == "" {
		return project{}, fmt.Errorf("workspace is not configured on this daemon")
	}
	id = strings.TrimSpace(id)
	if id == "" || id == defaultProjectID {
		root, err := filepath.Abs(e.workspaceRoot)
		if err != nil {
			return project{}, err
		}
		return project{ID: defaultProjectID, Name: "Workspace", Root: root}, nil
	}
	pr := e.projectsRegistry()
	if pr == nil {
		return project{}, fmt.Errorf("unknown project %q", id)
	}
	p, ok := pr.get(id)
	if !ok {
		return project{}, fmt.Errorf("unknown project %q", id)
	}
	return p, nil
}

// projectHasLiveRun reports whether any in-flight run belongs to a
// conversation tagged with this project (delete/purge guard).
func (e *Engine) projectHasLiveRun(projectID string) bool {
	e.mu.Lock()
	convs := make([]string, 0, len(e.runs))
	for _, r := range e.runs {
		convs = append(convs, r.convID)
	}
	e.mu.Unlock()
	for _, c := range convs {
		if e.conv.Project(c) == projectID {
			return true
		}
	}
	return false
}

// purgeProjectRoot removes a project's workspace subtree — guarded to a
// STRICT subdirectory of the workspace root so a mis-rooted record can never
// delete the whole volume.
func (e *Engine) purgeProjectRoot(root string) error {
	wsRoot, err := filepath.Abs(e.workspaceRoot)
	if err != nil {
		return err
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	rel, err := filepath.Rel(wsRoot, abs)
	if err != nil || rel == "." || rel == "" || strings.HasPrefix(rel, "..") {
		return fmt.Errorf("project root %q is not a workspace subdirectory", root)
	}
	return os.RemoveAll(abs)
}

// projectDirSlug slugifies a name/dir into a safe single-segment workspace
// subdir.
func projectDirSlug(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '-' || r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if len(out) > 48 {
		out = out[:48]
	}
	return out
}

// --- routes ---------------------------------------------------------------

// handleProjects serves GET /projects (list) and POST /projects (create).
func (s *Server) handleProjects(w http.ResponseWriter, r *http.Request) {
	if s.engine.workspaceRoot == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "workspace is not configured on this daemon"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		def, _ := s.engine.resolveProjectRecord(defaultProjectID)
		out := []project{def}
		if pr := s.engine.projectsRegistry(); pr != nil {
			out = append(out, pr.list()...)
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"projects": out})
	case http.MethodPost:
		s.handleCreateProject(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleCreateProject(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid body: " + err.Error()})
		return
	}
	if strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "name is required"})
		return
	}
	dir := projectDirSlug(req.Dir)
	if dir == "" {
		dir = projectDirSlug(req.Name)
	}
	if dir == "" || dir == defaultProjectID {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "could not derive a valid project directory from name/dir"})
		return
	}
	root, err := filepath.Abs(filepath.Join(s.engine.workspaceRoot, dir))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "create workspace subtree: " + err.Error()})
		return
	}
	p := project{ID: dir, Name: strings.TrimSpace(req.Name), Root: root, CreatedAt: time.Now().UTC()}
	pr := s.engine.projectsRegistry()
	if pr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "workspace is not configured on this daemon"})
		return
	}
	if err := pr.create(p); err != nil {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handleProject serves GET / PATCH (rename, archive) / DELETE on
// /projects/{id}.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/projects/"), "/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := s.engine.resolveProjectRecord(id)
		if err != nil {
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodPatch:
		if id == defaultProjectID {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "the default project is synthesized and cannot be edited"})
			return
		}
		s.handlePatchProject(w, r, id)
	case http.MethodDelete:
		s.handleDeleteProject(w, r, id)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handlePatchProject updates any subset of {name, archived}.
func (s *Server) handlePatchProject(w http.ResponseWriter, r *http.Request, id string) {
	var req struct {
		Name     *string `json:"name"`
		Archived *bool   `json:"archived"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid body: " + err.Error()})
		return
	}
	if req.Name == nil && req.Archived == nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "nothing to change: provide name or archived"})
		return
	}
	if req.Name != nil && strings.TrimSpace(*req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "name cannot be empty"})
		return
	}
	pr := s.engine.projectsRegistry()
	if pr == nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	p, err := pr.patch(id, func(p *project) {
		if req.Name != nil {
			p.Name = strings.TrimSpace(*req.Name)
		}
		if req.Archived != nil {
			p.Archived = *req.Archived
		}
	})
	if err != nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, p)
}

// handleDeleteProject removes a project from the registry. It REFUSES while a
// run is live in that project. With ?purge=true the workspace subtree is also
// removed (strict-subdirectory guarded).
func (s *Server) handleDeleteProject(w http.ResponseWriter, r *http.Request, id string) {
	pr := s.engine.projectsRegistry()
	if pr == nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	p, ok := pr.get(id)
	if !ok {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	if s.engine.projectHasLiveRun(id) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "a run is live in this project; stop it before deleting"})
		return
	}
	purge := r.URL.Query().Get("purge") == "true"
	if purge {
		if err := s.engine.purgeProjectRoot(p.Root); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "purge workspace subtree: " + err.Error()})
			return
		}
	}
	if _, err := pr.remove(id); err != nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"deleted": true, "id": id, "purged": purge})
}
