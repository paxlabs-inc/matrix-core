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

	"matrix/cody/internal/mode"
)

// Projects let a beta tester build more than one thing in one environment: each
// project is a /workspace/<dir> subtree with its own mode and its own runs,
// spec files, and snapshots (req 2.1-2.3). The bare /workspace is the default
// project, so a request that names no project behaves exactly as before.

// defaultProjectID names the bare /workspace project.
const defaultProjectID = "default"

// project is one registry entry: a named /workspace subtree with a mode.
type project struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Root      string    `json:"root"` // absolute workspace subtree
	Mode      string    `json:"mode"`
	CreatedAt time.Time `json:"created_at"`
}

// modeOr parses the project's mode, falling back to def.
func (p project) modeOr(def mode.Mode) mode.Mode {
	return parseModeOrDefault(p.Mode, def)
}

// projectRegistry persists the project map at /data/cody/projects.json. The
// default project is synthesized, never stored, so it always exists.
type projectRegistry struct {
	path string
	mu   sync.Mutex
}

func newProjectRegistry(path string) *projectRegistry { return &projectRegistry{path: path} }

func (pr *projectRegistry) loadLocked() map[string]project {
	m := map[string]project{}
	data, err := os.ReadFile(pr.path)
	if err != nil {
		return m
	}
	_ = json.Unmarshal(data, &m)
	return m
}

func (pr *projectRegistry) saveLocked(m map[string]project) error {
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

func (pr *projectRegistry) setMode(id string, md mode.Mode) (project, error) {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	m := pr.loadLocked()
	p, ok := m[id]
	if !ok {
		return project{}, fmt.Errorf("unknown project %q", id)
	}
	p.Mode = string(md)
	m[id] = p
	if err := pr.saveLocked(m); err != nil {
		return project{}, err
	}
	return p, nil
}

// --- engine seams ---------------------------------------------------------

// resolveProject resolves a project id to its record; empty / "default" is the
// bare /workspace project, synthesized from engine options.
func (e *Engine) resolveProject(id string) (project, error) {
	id = strings.TrimSpace(id)
	if id == "" || id == defaultProjectID {
		root, err := filepath.Abs(e.opts.WorkspaceRoot)
		if err != nil {
			return project{}, err
		}
		return project{ID: defaultProjectID, Name: "Workspace", Root: root, Mode: string(e.opts.DefaultMode)}, nil
	}
	p, ok := e.projects.get(id)
	if !ok {
		return project{}, fmt.Errorf("unknown project %q", id)
	}
	return p, nil
}

// reqProjectRoot resolves the workspace root for a workspace request from its
// ?project= (or ?project_id=) query param.
func (e *Engine) reqProjectRoot(r *http.Request) (string, error) {
	id := r.URL.Query().Get("project")
	if id == "" {
		id = r.URL.Query().Get("project_id")
	}
	p, err := e.resolveProject(id)
	if err != nil {
		return "", err
	}
	return p.Root, nil
}

// specFileCap bounds an ingested spec document (bytes) so a pathological file
// cannot blow up the planner window.
const specFileCap = 512 * 1024

// readSpecFile reads a workspace-relative spec document from the addressed
// project's root, traversal-guarded, for spec ingestion (req 11.1). It returns
// the document, its source label (the path), or an error.
func (e *Engine) readSpecFile(projectID, rel string) (string, string, error) {
	proj, err := e.resolveProject(projectID)
	if err != nil {
		return "", "", err
	}
	abs, err := resolveWorkspacePath(proj.Root, rel)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(abs)
	if err != nil || info.IsDir() {
		return "", "", fmt.Errorf("spec file %q not found", rel)
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return "", "", err
	}
	if len(data) > specFileCap {
		data = data[:specFileCap]
	}
	if strings.TrimSpace(string(data)) == "" {
		return "", "", fmt.Errorf("spec file %q is empty", rel)
	}
	return string(data), filepath.ToSlash(rel), nil
}

// projectDir slugifies a name/dir into a safe single-segment workspace subdir.
func projectDir(s string) string {
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
	switch r.Method {
	case http.MethodGet:
		def, _ := s.engine.resolveProject(defaultProjectID)
		out := append([]project{def}, s.engine.projects.list()...)
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
		Mode string `json:"mode"`
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
	m, err := mode.Parse(req.Mode)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	dir := projectDir(req.Dir)
	if dir == "" {
		dir = projectDir(req.Name)
	}
	if dir == "" || dir == defaultProjectID {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "could not derive a valid project directory from name/dir"})
		return
	}
	root, err := filepath.Abs(filepath.Join(s.engine.opts.WorkspaceRoot, dir))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "create workspace subtree: " + err.Error()})
		return
	}
	p := project{ID: dir, Name: strings.TrimSpace(req.Name), Root: root, Mode: string(m), CreatedAt: time.Now().UTC()}
	if err := s.engine.projects.create(p); err != nil {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// handleProject serves GET /projects/{id} and PATCH /projects/{id} (mode).
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/projects/"), "/")
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := s.engine.resolveProject(id)
		if err != nil {
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, p)
	case http.MethodPatch:
		if id == defaultProjectID {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "the default project's mode is fixed by the daemon's default mode"})
			return
		}
		var req struct {
			Mode string `json:"mode"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid body: " + err.Error()})
			return
		}
		m, err := mode.Parse(req.Mode)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
			return
		}
		p, err := s.engine.projects.setMode(id, m)
		if err != nil {
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, p)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
