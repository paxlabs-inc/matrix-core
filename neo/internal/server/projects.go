// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
	Root      string    `json:"root"` // persisted privately; never returned by projectView
	Imported  bool      `json:"imported,omitempty"`
	Archived  bool      `json:"archived,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type projectView struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Archived  bool      `json:"archived,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
}

func viewProject(p project) projectView {
	return projectView{ID: p.ID, Name: p.Name, Archived: p.Archived, CreatedAt: p.CreatedAt}
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
	dir := filepath.Dir(pr.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".projects-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, pr.path); err != nil {
		return err
	}
	dh, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer dh.Close()
	return dh.Sync()
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
	if existing, ok := m[p.ID]; ok {
		// A directory imported from the legacy workbench may immediately be
		// adopted by an explicit create call. Once it has product metadata it is
		// an ordinary duplicate and remains a conflict.
		if !existing.Imported || filepath.Clean(existing.Root) != filepath.Clean(p.Root) {
			return fmt.Errorf("project %q already exists", p.ID)
		}
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
			_ = e.projects.importExisting(e.workspaceRoot)
		}
	})
	return e.projects
}

// importExisting performs the one compatibility migration from the historical
// directory-only workbench: safe top-level directories become explicit
// private registry records. Requests still resolve registry identities only;
// no request value is ever joined onto the workspace as a path.
func (pr *projectRegistry) importExisting(workspaceRoot string) error {
	pr.mu.Lock()
	defer pr.mu.Unlock()
	entries, err := os.ReadDir(workspaceRoot)
	if err != nil {
		return err
	}
	projects := pr.loadLocked()
	changed := false
	for _, entry := range entries {
		name := entry.Name()
		if !entry.IsDir() || name == ".neo" || projectDirSlug(name) != name {
			continue
		}
		if _, exists := projects[name]; exists {
			continue
		}
		info, err := entry.Info()
		if err != nil || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		projects[name] = project{ID: name, Name: name, Root: filepath.Join(workspaceRoot, name), Imported: true, CreatedAt: info.ModTime().UTC()}
		changed = true
	}
	if !changed {
		return nil
	}
	return pr.saveLocked(projects)
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

// ensureBuildProject resolves an existing project or creates its workspace
// directory and registry record under one registry lock. It is idempotent for
// replayed Build admission: the same normalized project name resolves to the
// same record instead of producing a second directory.
func (e *Engine) ensureBuildProject(name string) (project, bool, error) {
	name = strings.TrimSpace(name)
	id := projectDirSlug(name)
	if id == "" || id == defaultProjectID {
		return project{}, false, errors.New("could not derive a valid Build project name")
	}
	registry := e.projectsRegistry()
	if registry == nil {
		return project{}, false, errors.New("workspace is not configured on this daemon")
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	projects := registry.loadLocked()
	if existing, ok := projects[id]; ok {
		return existing, false, nil
	}
	root, err := filepath.Abs(filepath.Join(e.workspaceRoot, id))
	if err != nil {
		return project{}, false, err
	}
	createdDir := false
	if info, statErr := os.Lstat(root); statErr == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return project{}, false, errors.New("Build project path is not a real directory")
		}
	} else if !os.IsNotExist(statErr) {
		return project{}, false, statErr
	} else {
		if err := os.Mkdir(root, 0o755); err != nil {
			return project{}, false, err
		}
		createdDir = true
	}
	created := project{ID: id, Name: name, Root: root, CreatedAt: time.Now().UTC()}
	projects[id] = created
	if err := registry.saveLocked(projects); err != nil {
		if createdDir {
			_ = os.Remove(root)
		}
		return project{}, false, err
	}
	return created, true, nil
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

var projectForkSkip = map[string]bool{
	".git": true, ".neo": true, ".agentcore": true, "node_modules": true,
	".next": true, ".turbo": true, ".cache": true, "dist": true,
	"build": true, "target": true, "__pycache__": true, ".venv": true,
	"venv": true,
}

func projectForkSkipFile(name string) bool {
	return name == ".env" || strings.HasPrefix(name, ".env.")
}

// copyProjectTree creates a credential-clean, independent project copy. It
// deliberately omits source-control metadata, dependency/build caches,
// AgentCore state, and dotenv files. Symlinks and special files are refused so
// a fork cannot retain an out-of-project capability through an external link.
func copyProjectTree(source, destination string) (err error) {
	if err := os.Mkdir(destination, 0o755); err != nil {
		return err
	}
	complete := false
	defer func() {
		if !complete {
			_ = os.RemoveAll(destination)
		}
	}()
	err = filepath.WalkDir(source, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		if entry.IsDir() && projectForkSkip[entry.Name()] {
			return filepath.SkipDir
		}
		if projectForkSkipFile(entry.Name()) {
			return nil
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() && !info.IsDir() {
			return fmt.Errorf("fork refuses special file %q", rel)
		}
		target := filepath.Join(destination, rel)
		if info.IsDir() {
			return os.Mkdir(target, info.Mode().Perm()&0o755)
		}
		return copyVerifiedProjectFile(path, target, info.Mode().Perm())
	})
	if err != nil {
		return err
	}
	if err := syncProjectTree(destination); err != nil {
		return err
	}
	complete = true
	return nil
}

func copyVerifiedProjectFile(source, destination string, mode fs.FileMode) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode&0o755)
	if err != nil {
		return err
	}
	h := sha256.New()
	_, copyErr := io.Copy(io.MultiWriter(out, h), in)
	if copyErr == nil {
		copyErr = out.Sync()
	}
	if closeErr := out.Close(); copyErr == nil {
		copyErr = closeErr
	}
	if copyErr != nil {
		return copyErr
	}
	want := h.Sum(nil)
	check, err := os.Open(destination)
	if err != nil {
		return err
	}
	defer check.Close()
	actual := sha256.New()
	if _, err := io.Copy(actual, check); err != nil {
		return err
	}
	if string(want) != string(actual.Sum(nil)) {
		return fmt.Errorf("fork verification failed for %q", source)
	}
	return nil
}

// syncProjectTree makes Save explicit: regular files are flushed before their
// containing directories, then the project root itself is synced.
func syncProjectTree(root string) error {
	directories := []string{}
	if err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			directories = append(directories, path)
			return nil
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		file, err := os.OpenFile(path, os.O_RDONLY, 0)
		if err != nil {
			return err
		}
		err = file.Sync()
		closeErr := file.Close()
		if err != nil {
			return err
		}
		return closeErr
	}); err != nil {
		return err
	}
	for index := len(directories) - 1; index >= 0; index-- {
		dir, err := os.Open(directories[index])
		if err != nil {
			return err
		}
		err = dir.Sync()
		closeErr := dir.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
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
		out := []projectView{viewProject(def)}
		if pr := s.engine.projectsRegistry(); pr != nil {
			for _, p := range pr.list() {
				out = append(out, viewProject(p))
			}
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
	pr := s.engine.projectsRegistry()
	if pr == nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "workspace is not configured on this daemon"})
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
	if info, statErr := os.Lstat(root); statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "project directory must be a real workspace directory"})
		return
	}
	p := project{ID: dir, Name: strings.TrimSpace(req.Name), Root: root, CreatedAt: time.Now().UTC()}
	if err := pr.create(p); err != nil {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, viewProject(p))
}

// handleProject serves GET / PATCH (rename, archive) / DELETE on
// /projects/{id}.
func (s *Server) handleProject(w http.ResponseWriter, r *http.Request) {
	path := strings.Trim(strings.TrimPrefix(r.URL.Path, "/projects/"), "/")
	parts := strings.Split(path, "/")
	if path == "" || len(parts) > 2 {
		http.Error(w, "not found", http.StatusNotFound)
		return
	}
	id := parts[0]
	if len(parts) == 2 {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		switch parts[1] {
		case "fork":
			s.handleForkProject(w, r, id)
		case "save":
			s.handleSaveProject(w, r, id)
		case "reopen":
			s.handleReopenProject(w, r, id)
		default:
			http.Error(w, "not found", http.StatusNotFound)
		}
		return
	}
	switch r.Method {
	case http.MethodGet:
		p, err := s.engine.resolveProjectRecord(id)
		if err != nil {
			http.Error(w, "unknown project", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, viewProject(p))
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

func (s *Server) handleSaveProject(w http.ResponseWriter, _ *http.Request, id string) {
	p, err := s.engine.resolveProjectRecord(id)
	if err != nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	if err := syncProjectTree(p.Root); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "save project: " + err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"saved": true, "project": viewProject(p), "saved_at": time.Now().UTC(),
	})
}

func (s *Server) handleReopenProject(w http.ResponseWriter, _ *http.Request, id string) {
	p, err := s.engine.resolveProjectRecord(id)
	if err != nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	info, err := os.Stat(p.Root)
	if err != nil || !info.IsDir() {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "project directory is unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"project": viewProject(p), "reopened": true})
}

func (s *Server) handleForkProject(w http.ResponseWriter, r *http.Request, sourceID string) {
	if sourceID == defaultProjectID {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "the default workspace cannot be forked"})
		return
	}
	source, err := s.engine.resolveProjectRecord(sourceID)
	if err != nil {
		http.Error(w, "unknown project", http.StatusNotFound)
		return
	}
	var request struct {
		Name string `json:"name"`
		Dir  string `json:"dir"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&request); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid body: " + err.Error()})
		return
	}
	name := strings.TrimSpace(request.Name)
	if name == "" {
		name = source.Name + " Copy"
	}
	id := projectDirSlug(request.Dir)
	if id == "" {
		id = projectDirSlug(name)
	}
	if id == "" || id == defaultProjectID || id == sourceID {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "a distinct valid fork directory is required"})
		return
	}
	registry := s.engine.projectsRegistry()
	if registry == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]interface{}{"error": "project registry is unavailable"})
		return
	}
	if _, exists := registry.get(id); exists {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "project already exists"})
		return
	}
	destination, err := resolveWorkspacePath(s.engine.workspaceRoot, id)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid fork directory"})
		return
	}
	if _, err := os.Lstat(destination); !os.IsNotExist(err) {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": "fork directory already exists"})
		return
	}
	if err := copyProjectTree(source.Root, destination); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "fork project: " + err.Error()})
		return
	}
	fork := project{ID: id, Name: name, Root: destination, CreatedAt: time.Now().UTC()}
	if err := registry.create(fork); err != nil {
		_ = os.RemoveAll(destination)
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"project": viewProject(fork), "source_project_id": sourceID, "independent": true,
	})
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
	writeJSON(w, http.StatusOK, viewProject(p))
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
