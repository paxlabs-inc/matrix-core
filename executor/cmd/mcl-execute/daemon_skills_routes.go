// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package main

// daemon_skills_routes.go — /skills/* route surface (sess#27).
//
// Routes:
//
//   GET  /skills                         list all skills (paginated, filterable)
//   GET  /skills/:slug@:version          full detail (parsed SKILL.mtx + .md)
//   GET  /skills/by-verb/:verb           filter by D7 verb
//   GET  /skills/index                   raw INDEX.json (when present)
//   POST /skills/suggest                 embedding similarity ranking (stub v1)
//
// All resolution goes through runtime.SkillLoader so the daemon's
// route shape matches what the existing executor walk path consumes.
//
// Skill corpus is at d.skillsRoot (default: /opt/matrix/skills/, set
// to /root/matrix/skills/ in dev). Each subdirectory MUST contain a
// SKILL.mtx; companion SKILL.md is optional.

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"matrix/executor/runtime"
)

// skillSummaryDTO is the wire shape for a skill in list views.
type skillSummaryDTO struct {
	URI           string   `json:"uri"`
	Slug          string   `json:"slug"`
	Version       string   `json:"version"`
	Display       string   `json:"display,omitempty"`
	Description   string   `json:"description,omitempty"`
	Author        string   `json:"author,omitempty"`
	MclVerbs      []string `json:"mcl_verbs,omitempty"`
	CanonicalHash string   `json:"canonical_hash,omitempty"`
	HasMD         bool     `json:"has_md"`
	ToolsNone     bool     `json:"tools_none"`
	SubSkillsNone bool     `json:"sub_skills_none"`
	DeclaredTools int      `json:"declared_tools_n"`
	DeclaredSubs  int      `json:"declared_sub_skills_n"`
}

// skillDetailDTO extends summary with full body bytes for /skills/:uri.
type skillDetailDTO struct {
	skillSummaryDTO
	MtxPath           string   `json:"mtx_path,omitempty"`
	MdPath            string   `json:"md_path,omitempty"`
	MtxBytes          string   `json:"mtx_text,omitempty"` // raw SKILL.mtx text
	MdBytes           string   `json:"md_text,omitempty"`  // raw SKILL.md text
	DeclaredTools     []string `json:"declared_tools,omitempty"`
	DeclaredSubSkills []string `json:"declared_sub_skills,omitempty"`
}

// skillCatalog is the daemon-wide cached skill index. Built lazily on
// first /skills hit and refreshed when any SKILL.mtx mtime changes.
type skillCatalog struct {
	mu       sync.RWMutex
	root     string
	loader   *runtime.SkillLoader
	entries  []skillSummaryDTO
	bySlug   map[string][]skillSummaryDTO // slug -> versions
	byVerb   map[string][]skillSummaryDTO
	byURI    map[string]skillSummaryDTO
	loadedAt time.Time
	rootHash string // fingerprint of corpus dir mtimes
}

func newSkillCatalog(root string) *skillCatalog {
	return &skillCatalog{
		root:   root,
		loader: runtime.NewSkillLoader(root),
	}
}

// ensureLoaded scans the corpus when stale (>30s old or rootHash
// changed). The hash combines per-skill SKILL.mtx mtimes so catalog
// updates without a daemon restart still reflect on the next call.
func (c *skillCatalog) ensureLoaded() error {
	c.mu.RLock()
	stale := time.Since(c.loadedAt) > 30*time.Second
	c.mu.RUnlock()
	if !stale && c.entries != nil {
		// Fast path: still warm.
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	// Re-check stale under write lock.
	if c.entries != nil && time.Since(c.loadedAt) <= 30*time.Second {
		return nil
	}
	hash, err := corpusFingerprint(c.root)
	if err != nil {
		return err
	}
	if hash == c.rootHash && c.entries != nil {
		c.loadedAt = time.Now()
		return nil
	}
	entries, bySlug, byVerb, byURI, err := scanSkillCorpus(c.root, c.loader)
	if err != nil {
		return err
	}
	c.entries = entries
	c.bySlug = bySlug
	c.byVerb = byVerb
	c.byURI = byURI
	c.loadedAt = time.Now()
	c.rootHash = hash
	return nil
}

// corpusFingerprint hashes (slug, mtime) pairs across all skill dirs.
func corpusFingerprint(root string) (string, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	h := sha256.New()
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		mtxPath := filepath.Join(root, e.Name(), "SKILL.mtx")
		st, err := os.Stat(mtxPath)
		if err != nil {
			continue
		}
		fmt.Fprintf(h, "%s|%d\n", e.Name(), st.ModTime().UnixNano())
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// scanSkillCorpus walks every skill dir and returns the typed indexes.
func scanSkillCorpus(root string, loader *runtime.SkillLoader) ([]skillSummaryDTO, map[string][]skillSummaryDTO, map[string][]skillSummaryDTO, map[string]skillSummaryDTO, error) {
	dirEntries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil, nil, nil
		}
		return nil, nil, nil, nil, err
	}
	entries := make([]skillSummaryDTO, 0, len(dirEntries))
	bySlug := make(map[string][]skillSummaryDTO)
	byVerb := make(map[string][]skillSummaryDTO)
	byURI := make(map[string]skillSummaryDTO)
	for _, e := range dirEntries {
		if !e.IsDir() {
			continue
		}
		slug := e.Name()
		mtxPath := filepath.Join(root, slug, "SKILL.mtx")
		if _, err := os.Stat(mtxPath); err != nil {
			continue
		}
		// Best-effort: try to load each skill. Bad SKILL.mtx files are
		// reported in catalog scan but don't fail the whole list.
		// Compute the URI by reading just the version field via a
		// quick parse, then load fully via the loader for metadata.
		ver, slugFromFile, err := readSkillVersionAndID(mtxPath)
		if err != nil || ver == "" || slugFromFile == "" {
			continue
		}
		uri := fmt.Sprintf("matrix://skill/%s@%s", slug, ver)
		ls, err := loader.Load(uri)
		if err != nil {
			continue
		}
		dto := loadedSkillToSummary(ls)
		entries = append(entries, dto)
		bySlug[ls.Slug] = append(bySlug[ls.Slug], dto)
		byURI[ls.URI] = dto
		for _, v := range ls.MclVerbs {
			byVerb[v] = append(byVerb[v], dto)
		}
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Slug < entries[j].Slug })
	return entries, bySlug, byVerb, byURI, nil
}

func loadedSkillToSummary(ls *runtime.LoadedSkill) skillSummaryDTO {
	return skillSummaryDTO{
		URI:           ls.URI,
		Slug:          ls.Slug,
		Version:       ls.Version,
		Display:       ls.Display,
		Description:   ls.Description,
		Author:        ls.Author,
		MclVerbs:      append([]string(nil), ls.MclVerbs...),
		CanonicalHash: ls.CanonicalHash,
		HasMD:         len(ls.MdBytes) > 0,
		ToolsNone:     ls.ToolsNone,
		SubSkillsNone: ls.SubSkillsNone,
		DeclaredTools: len(ls.DeclaredTools),
		DeclaredSubs:  len(ls.DeclaredSubSkills),
	}
}

// readSkillVersionAndID extracts §SKILL.id and §SKILL.version from a
// SKILL.mtx file via a simple line scan. Avoids the full parser
// allocation when only the URI is needed.
func readSkillVersionAndID(path string) (version string, id string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", err
	}
	scan := strings.NewReader(string(raw))
	buf := make([]byte, 0, 1024)
	tmp := make([]byte, 1024)
	inSkill := false
	for {
		n, _ := scan.Read(tmp)
		if n == 0 {
			break
		}
		buf = append(buf, tmp[:n]...)
	}
	for _, line := range strings.Split(string(buf), "\n") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "§SKILL") {
			inSkill = true
			continue
		}
		if strings.HasPrefix(trim, "§") && !strings.HasPrefix(trim, "§SKILL") {
			inSkill = false
			continue
		}
		if !inSkill {
			continue
		}
		switch {
		case strings.HasPrefix(trim, "id="):
			id = strings.TrimSpace(strings.TrimPrefix(trim, "id="))
		case strings.HasPrefix(trim, "version="):
			version = strings.TrimSpace(strings.TrimPrefix(trim, "version="))
		}
	}
	return version, id, nil
}

// ---- handlers ----

// handleSkillsList serves GET /skills.
func (d *daemonState) handleSkillsList(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.skillCatalog == nil {
		d.skillCatalog = newSkillCatalog(d.skillsRoot)
	}
	if err := d.skillCatalog.ensureLoaded(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "skills scan: " + err.Error(),
		})
		return
	}
	verb := queryString(r, "verb", "")
	search := strings.ToLower(queryString(r, "search", ""))
	d.skillCatalog.mu.RLock()
	defer d.skillCatalog.mu.RUnlock()
	pool := d.skillCatalog.entries
	if verb != "" {
		pool = d.skillCatalog.byVerb[verb]
	}
	if search != "" {
		filtered := make([]skillSummaryDTO, 0, len(pool))
		for _, s := range pool {
			if strings.Contains(strings.ToLower(s.Slug), search) ||
				strings.Contains(strings.ToLower(s.Display), search) ||
				strings.Contains(strings.ToLower(s.Description), search) {
				filtered = append(filtered, s)
			}
		}
		pool = filtered
	}
	_, limit, ok := pageParams(w, r, 50, 200)
	if !ok {
		return
	}
	if len(pool) > limit {
		pool = pool[:limit]
	}
	writePaged(w, pool, "", len(pool))
}

// handleSkillsByVerb serves GET /skills/by-verb/:verb.
func (d *daemonState) handleSkillsByVerb(w http.ResponseWriter, r *http.Request, verb string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if d.skillCatalog == nil {
		d.skillCatalog = newSkillCatalog(d.skillsRoot)
	}
	if err := d.skillCatalog.ensureLoaded(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	d.skillCatalog.mu.RLock()
	defer d.skillCatalog.mu.RUnlock()
	items := d.skillCatalog.byVerb[verb]
	writePaged(w, items, "", len(items))
}

// handleSkillsIndex serves GET /skills/index.
func (d *daemonState) handleSkillsIndex(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	path := filepath.Join(d.skillsRoot, "INDEX.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "INDEX.json not present"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	var doc interface{}
	if err := json.Unmarshal(raw, &doc); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{
			"error": "INDEX.json parse: " + err.Error(),
		})
		return
	}
	writeJSON(w, http.StatusOK, doc)
}

// handleSkillsDetail serves GET /skills/<slug>@<version>.
//
// The path component immediately after /skills/ is the URI's slug+version
// pair (e.g. "brainstorming@0.1.0"). URL-encoded characters are decoded
// before parsing.
func (d *daemonState) handleSkillsDetail(w http.ResponseWriter, r *http.Request, slugAtVer string) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	if !strings.Contains(slugAtVer, "@") {
		writeJSON(w, http.StatusBadRequest, map[string]string{
			"error": "skill ref must be slug@version",
		})
		return
	}
	uri := "matrix://skill/" + slugAtVer
	loader := runtime.NewSkillLoader(d.skillsRoot)
	ls, err := loader.Load(uri)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{
			"error": "skill load: " + err.Error(),
		})
		return
	}
	out := skillDetailDTO{
		skillSummaryDTO:   loadedSkillToSummary(ls),
		MtxPath:           ls.MtxPath,
		MdPath:            ls.MdPath,
		MtxBytes:          string(ls.MtxBytes),
		MdBytes:           string(ls.MdBytes),
		DeclaredTools:     append([]string(nil), ls.DeclaredTools...),
		DeclaredSubSkills: append([]string(nil), ls.DeclaredSubSkills...),
	}
	writeJSON(w, http.StatusOK, out)
}

// handleSkillsSuggest serves POST /skills/suggest.
//
// v1 stub: ranks by simple substring match against the prose; the
// embedding-based ranker lands when the daemon's embedder is
// authorised for ad-hoc query embedding (Phase 4 deferral). The route
// is in place so the frontend can build against it.
func (d *daemonState) handleSkillsSuggest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeJSON(w, http.StatusMethodNotAllowed, map[string]string{"error": "method not allowed"})
		return
	}
	if _, ok := d.requireAuthPolicy(w, r, authAny); !ok {
		return
	}
	var req struct {
		Prose string `json:"prose"`
		TopK  int    `json:"top_k"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "decode: " + err.Error()})
		return
	}
	if req.Prose == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "prose required"})
		return
	}
	if req.TopK <= 0 {
		req.TopK = 3
	}
	if req.TopK > 20 {
		req.TopK = 20
	}
	if d.skillCatalog == nil {
		d.skillCatalog = newSkillCatalog(d.skillsRoot)
	}
	if err := d.skillCatalog.ensureLoaded(); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
		return
	}
	prose := strings.ToLower(req.Prose)
	type scored struct {
		skillSummaryDTO
		Score float64 `json:"score"`
		Why   string  `json:"why,omitempty"`
	}
	d.skillCatalog.mu.RLock()
	defer d.skillCatalog.mu.RUnlock()
	results := make([]scored, 0, len(d.skillCatalog.entries))
	for _, s := range d.skillCatalog.entries {
		score := 0.0
		why := ""
		// Heuristic 1: slug substring match
		if strings.Contains(prose, strings.ToLower(s.Slug)) {
			score += 0.6
			why = "slug match"
		}
		// Heuristic 2: display words present in prose
		for _, w := range strings.Fields(strings.ToLower(s.Display)) {
			if len(w) >= 4 && strings.Contains(prose, w) {
				score += 0.15
			}
		}
		// Heuristic 3: description words
		for _, w := range strings.Fields(strings.ToLower(s.Description)) {
			if len(w) >= 5 && strings.Contains(prose, w) {
				score += 0.05
			}
		}
		if score > 0 {
			results = append(results, scored{
				skillSummaryDTO: s,
				Score:           score,
				Why:             why,
			})
		}
	}
	sort.Slice(results, func(i, j int) bool { return results[i].Score > results[j].Score })
	if len(results) > req.TopK {
		results = results[:req.TopK]
	}
	writeJSON(w, http.StatusOK, map[string]interface{}{
		"prose":   req.Prose,
		"top_k":   req.TopK,
		"items":   results,
		"ranking": "substring_v1",
	})
}

// handleSkillsRouter dispatches every /skills/* request.
func (d *daemonState) handleSkillsRouter(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/skills")
	path = strings.TrimPrefix(path, "/")
	switch {
	case path == "":
		d.handleSkillsList(w, r)
	case path == "index":
		d.handleSkillsIndex(w, r)
	case path == "suggest":
		d.handleSkillsSuggest(w, r)
	case strings.HasPrefix(path, "by-verb/"):
		verb := strings.TrimPrefix(path, "by-verb/")
		d.handleSkillsByVerb(w, r, verb)
	default:
		d.handleSkillsDetail(w, r, path)
	}
}

// Copyright © 2026 Paxlabs Inc. All rights reserved.
