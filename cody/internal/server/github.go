// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// GitHub integration: link a token, browse repos, clone one onto the machine
// as a project, or create a repo and wire it as a project's origin remote.
//
// The token lives ONLY on the user's machine volume (DataDir/github.json,
// 0600) — the environment IS the user's private machine, exactly like their
// code and media. It is never echoed back in full; status returns the login it
// authenticates as. Git runs on the user's machine against their workspace
// (the same posture as /workspace/diff and /workspace/exec); the token rides
// an ephemeral credential helper per command, never the remote URL, so it can
// never land in .git/config or process listings.

// githubAPI is a var so tests point it at a real httptest server (the same
// posture as the llmtest gateway: stub the external boundary, never our code).
var githubAPI = "https://api.github.com"

// githubHTTPTimeout bounds one GitHub API call.
const githubHTTPTimeout = 20 * time.Second

// gitOpTimeout bounds one git clone/remote operation.
const gitOpTimeout = 5 * time.Minute

// githubLink is the durable record at DataDir/github.json.
type githubLink struct {
	Token string `json:"token"`
	Login string `json:"login"`
}

func (e *Engine) githubPath() string { return filepath.Join(e.opts.DataDir, "github.json") }

func (e *Engine) loadGithub() (githubLink, bool) {
	data, err := os.ReadFile(e.githubPath())
	if err != nil {
		return githubLink{}, false
	}
	var l githubLink
	if json.Unmarshal(data, &l) != nil || strings.TrimSpace(l.Token) == "" {
		return githubLink{}, false
	}
	return l, true
}

func (e *Engine) saveGithub(l githubLink) error {
	if err := os.MkdirAll(filepath.Dir(e.githubPath()), 0o755); err != nil {
		return err
	}
	data, err := json.Marshal(l)
	if err != nil {
		return err
	}
	tmp := e.githubPath() + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, e.githubPath())
}

// githubUser validates a token and returns the login it authenticates as.
func githubUser(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, githubAPI+"/user", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusUnauthorized || res.StatusCode == http.StatusForbidden {
		return "", fmt.Errorf("github rejected the token (status %d)", res.StatusCode)
	}
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("github /user: status %d", res.StatusCode)
	}
	var u struct {
		Login string `json:"login"`
	}
	if err := json.NewDecoder(res.Body).Decode(&u); err != nil || u.Login == "" {
		return "", fmt.Errorf("github /user: unreadable response")
	}
	return u.Login, nil
}

// scrubToken removes the token from any surfaced text (defense in depth; the
// credential helper keeps it out of git's own output already).
func scrubToken(s, token string) string {
	if token == "" {
		return s
	}
	return strings.ReplaceAll(s, token, "***")
}

// gitWithToken runs one git command in dir with an ephemeral credential helper
// supplying the token, so it never appears in remote URLs or git config.
func gitWithToken(ctx context.Context, dir, token string, args ...string) (string, error) {
	helper := fmt.Sprintf(`!f() { echo "username=x-access-token"; echo "password=%s"; }; f`, token)
	full := append([]string{"-c", "credential.helper=" + helper}, args...)
	cmd := exec.CommandContext(ctx, "git", full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	out, err := cmd.CombinedOutput()
	return scrubToken(string(out), token), err
}

// --- routes -----------------------------------------------------------------

// handleGithub routes the /github/* surface.
func (s *Server) handleGithub(w http.ResponseWriter, r *http.Request) {
	switch strings.TrimPrefix(r.URL.Path, "/github/") {
	case "link":
		s.handleGithubLink(w, r)
	case "status":
		s.handleGithubStatus(w, r)
	case "repos":
		s.handleGithubRepos(w, r)
	case "clone":
		s.handleGithubClone(w, r)
	case "create":
		s.handleGithubCreate(w, r)
	default:
		http.Error(w, "not found", http.StatusNotFound)
	}
}

// handleGithubLink stores (POST) or removes (DELETE) the user's token. POST
// validates it against GET /user before persisting.
func (s *Server) handleGithubLink(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodPost:
		var req struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Token) == "" {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "token is required"})
			return
		}
		token := strings.TrimSpace(req.Token)
		ctx, cancel := context.WithTimeout(r.Context(), githubHTTPTimeout)
		defer cancel()
		login, err := githubUser(ctx, token)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": scrubToken(err.Error(), token)})
			return
		}
		if err := s.engine.saveGithub(githubLink{Token: token, Login: login}); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "store token: " + scrubToken(err.Error(), token)})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"linked": true, "login": login})
	case http.MethodDelete:
		if err := os.Remove(s.engine.githubPath()); err != nil && !os.IsNotExist(err) {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "unlink: " + err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{"linked": false})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleGithubStatus reports whether a token is linked and as whom (GET).
func (s *Server) handleGithubStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	l, ok := s.engine.loadGithub()
	if !ok {
		writeJSON(w, http.StatusOK, map[string]interface{}{"linked": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"linked": true, "login": l.Login})
}

// githubRepo is the trimmed repo shape the client lists.
type githubRepo struct {
	FullName      string `json:"full_name"`
	Name          string `json:"name"`
	Private       bool   `json:"private"`
	Description   string `json:"description"`
	DefaultBranch string `json:"default_branch"`
	CloneURL      string `json:"clone_url"`
	PushedAt      string `json:"pushed_at"`
}

// handleGithubRepos lists the linked user's repos, most recently pushed first
// (GET, one page of up to 100).
func (s *Server) handleGithubRepos(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	l, ok := s.engine.loadGithub()
	if !ok {
		writeJSON(w, http.StatusPreconditionFailed, map[string]interface{}{"error": "no GitHub account linked"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), githubHTTPTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		githubAPI+"/user/repos?per_page=100&sort=pushed&direction=desc", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	req.Header.Set("Authorization", "Bearer "+l.Token)
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": "github unreachable: " + scrubToken(err.Error(), l.Token)})
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": fmt.Sprintf("github /user/repos: status %d", res.StatusCode)})
		return
	}
	var repos []githubRepo
	if err := json.NewDecoder(res.Body).Decode(&repos); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": "github /user/repos: unreadable response"})
		return
	}
	if repos == nil {
		repos = []githubRepo{}
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{"repos": repos})
}

// handleGithubClone clones a repo into a NEW /workspace/<dir> project (POST).
// Body: {full_name | url, name?, mode?}. The clone lands on the user's machine
// and registers as a project so runs, snapshots, and previews address it.
func (s *Server) handleGithubClone(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	l, ok := s.engine.loadGithub()
	if !ok {
		writeJSON(w, http.StatusPreconditionFailed, map[string]interface{}{"error": "no GitHub account linked"})
		return
	}
	var req struct {
		FullName string `json:"full_name"`
		URL      string `json:"url"`
		Name     string `json:"name"`
		Mode     string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "invalid body: " + err.Error()})
		return
	}
	cloneURL, repoName, err := resolveCloneTarget(req.FullName, req.URL)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": err.Error()})
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = repoName
	}
	md := parseModeOrDefault(req.Mode, s.engine.opts.DefaultMode)
	dir := projectDir(name)
	if dir == "" || dir == defaultProjectID {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "could not derive a valid project directory from the repo name"})
		return
	}
	root, err := filepath.Abs(filepath.Join(s.engine.opts.WorkspaceRoot, dir))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	if _, ok := s.engine.projects.get(dir); ok {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("project %q already exists", dir)})
		return
	}
	if entries, err := os.ReadDir(root); err == nil && len(entries) > 0 {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": fmt.Sprintf("workspace directory %q already exists and is not empty", dir)})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), gitOpTimeout)
	defer cancel()
	if out, err := gitWithToken(ctx, s.engine.opts.WorkspaceRoot, l.Token, "clone", cloneURL, root); err != nil {
		_ = os.RemoveAll(root)
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": "git clone failed: " + strings.TrimSpace(out)})
		return
	}
	p := project{ID: dir, Name: name, Root: root, Mode: string(md), CreatedAt: time.Now().UTC()}
	if err := s.engine.projects.create(p); err != nil {
		writeJSON(w, http.StatusConflict, map[string]interface{}{"error": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, p)
}

// resolveCloneTarget derives the https clone URL + repo name from a full_name
// ("owner/repo") or an explicit github.com URL.
func resolveCloneTarget(fullName, rawURL string) (cloneURL, repoName string, err error) {
	if fn := strings.TrimSpace(fullName); fn != "" {
		parts := strings.Split(fn, "/")
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			return "", "", fmt.Errorf("full_name must be owner/repo")
		}
		return "https://github.com/" + fn + ".git", strings.TrimSuffix(parts[1], ".git"), nil
	}
	raw := strings.TrimSpace(rawURL)
	if raw == "" {
		return "", "", fmt.Errorf("full_name or url is required")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" {
		return "", "", fmt.Errorf("url must be an https://github.com repository URL")
	}
	path := strings.Trim(u.Path, "/")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("url must name owner/repo")
	}
	repo := strings.TrimSuffix(parts[1], ".git")
	return "https://github.com/" + parts[0] + "/" + repo + ".git", repo, nil
}

// handleGithubCreate creates a repo on GitHub and wires it as the addressed
// project's origin remote (POST). Body: {name, private?, project_id?}. It does
// NOT push — the user drives commits from the git surface.
func (s *Server) handleGithubCreate(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	l, ok := s.engine.loadGithub()
	if !ok {
		writeJSON(w, http.StatusPreconditionFailed, map[string]interface{}{"error": "no GitHub account linked"})
		return
	}
	var req struct {
		Name      string `json:"name"`
		Private   *bool  `json:"private"`
		ProjectID string `json:"project_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.Name) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{"error": "name is required"})
		return
	}
	private := true
	if req.Private != nil {
		private = *req.Private
	}
	proj, err := s.engine.resolveProject(req.ProjectID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{"error": err.Error()})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), githubHTTPTimeout)
	defer cancel()
	body, _ := json.Marshal(map[string]interface{}{"name": strings.TrimSpace(req.Name), "private": private})
	apiReq, err := http.NewRequestWithContext(ctx, http.MethodPost, githubAPI+"/user/repos", strings.NewReader(string(body)))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": err.Error()})
		return
	}
	apiReq.Header.Set("Authorization", "Bearer "+l.Token)
	apiReq.Header.Set("Accept", "application/vnd.github+json")
	apiReq.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(apiReq)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": "github unreachable: " + scrubToken(err.Error(), l.Token)})
		return
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		var ghErr struct {
			Message string `json:"message"`
		}
		_ = json.NewDecoder(res.Body).Decode(&ghErr)
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{
			"error": fmt.Sprintf("github create repo: status %d %s", res.StatusCode, ghErr.Message),
		})
		return
	}
	var created struct {
		FullName string `json:"full_name"`
		CloneURL string `json:"clone_url"`
		HTMLURL  string `json:"html_url"`
	}
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil || created.CloneURL == "" {
		writeJSON(w, http.StatusBadGateway, map[string]interface{}{"error": "github create repo: unreadable response"})
		return
	}

	// Wire the project: init if needed, then point origin at the new repo.
	gctx, gcancel := context.WithTimeout(r.Context(), gitOpTimeout)
	defer gcancel()
	if _, err := os.Stat(filepath.Join(proj.Root, ".git")); err != nil {
		if out, err := gitWithToken(gctx, proj.Root, l.Token, "init"); err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "git init failed: " + strings.TrimSpace(out)})
			return
		}
	}
	if out, err := gitWithToken(gctx, proj.Root, l.Token, "remote", "add", "origin", created.CloneURL); err != nil {
		if strings.Contains(out, "already exists") {
			if out2, err2 := gitWithToken(gctx, proj.Root, l.Token, "remote", "set-url", "origin", created.CloneURL); err2 != nil {
				writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "git remote set-url failed: " + strings.TrimSpace(out2)})
				return
			}
		} else {
			writeJSON(w, http.StatusInternalServerError, map[string]interface{}{"error": "git remote add failed: " + strings.TrimSpace(out)})
			return
		}
	}
	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"full_name": created.FullName,
		"url":       created.HTMLURL,
		"clone_url": created.CloneURL,
		"project":   proj.ID,
		"remote":    "origin",
	})
}
