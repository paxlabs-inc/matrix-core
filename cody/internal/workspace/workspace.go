// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

// Package workspace maintains Cody's persistent repo index over /workspace:
// the file tree, detected languages, frameworks, build targets, and entry
// points. The index is persisted on the volume (<root>/.cody/workspace.json)
// so it survives sessions and restarts; the orchestrator reads it during
// "understand" and grounds task sheets in it.
package workspace

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

// StateDir is the per-workspace directory Cody keeps its own state in. It is
// always excluded from the index and from worker edits.
const StateDir = ".cody"

const indexFile = "workspace.json"

// maxIndexedFiles bounds the persisted file list so a huge monorepo cannot
// balloon the index; language/framework detection still sees every file.
const maxIndexedFiles = 20000

// File is one indexed workspace file.
type File struct {
	Path string `json:"path"`
	Size int64  `json:"size"`
	Lang string `json:"lang,omitempty"`
}

// Model is the persistent repo index.
type Model struct {
	Root        string    `json:"root"`
	GeneratedAt time.Time `json:"generated_at"`
	FileCount   int       `json:"file_count"`
	Files       []File    `json:"files"`
	// Languages maps language name -> file count, descending significance.
	Languages map[string]int `json:"languages"`
	// Frameworks lists detected frameworks/runtimes (next, react, django, ...).
	Frameworks []string `json:"frameworks,omitempty"`
	// BuildTargets lists the project's own invocable targets
	// (package.json script names, Makefile targets, go/cargo defaults).
	BuildTargets []string `json:"build_targets,omitempty"`
	// EntryPoints lists likely program entry files.
	EntryPoints []string `json:"entry_points,omitempty"`
}

// Greenfield reports whether the workspace holds no existing codebase to build
// on: no source-language files and no invocable build targets. A fresh /workspace
// (or one seeded with only plain-text scaffolding) is greenfield — the signal
// that gates the Stack Decision Record before planning (req 8.1).
func (m *Model) Greenfield() bool {
	return len(m.Languages) == 0 && len(m.BuildTargets) == 0
}

// uiFrameworks are the detected frameworks that imply a user-facing surface.
var uiFrameworks = map[string]bool{
	"next": true, "react": true, "vue": true, "svelte": true,
	"astro": true, "remix": true,
}

// uiLangs are the languages that imply UI markup/styling files.
var uiLangs = map[string]bool{
	"css": true, "html": true, "vue": true, "svelte": true,
}

// UIBearing reports whether an existing workspace already carries a user-facing
// UI: a detected frontend framework, or CSS/HTML/component files. It is the
// signal (alongside the plan's own UI tasks) that gates the Design Language
// Record before the first UI task (req 9.1). A greenfield workspace has nothing
// to detect yet, so UI intent there is read from the authored plan instead.
func (m *Model) UIBearing() bool {
	for _, fw := range m.Frameworks {
		if uiFrameworks[fw] {
			return true
		}
	}
	for lang := range m.Languages {
		if uiLangs[lang] {
			return true
		}
	}
	// A .tsx/.jsx file is a React component even before a framework is detected.
	for _, f := range m.Files {
		switch strings.ToLower(filepath.Ext(f.Path)) {
		case ".tsx", ".jsx":
			return true
		}
	}
	return false
}

var ignoredDirs = map[string]bool{
	".git": true, "node_modules": true, "vendor": true, "dist": true,
	"build": true, "target": true, ".next": true, "__pycache__": true,
	".venv": true, "venv": true, ".cache": true, ".turbo": true,
	StateDir: true,
}

var langByExt = map[string]string{
	".go": "go", ".ts": "typescript", ".tsx": "typescript",
	".js": "javascript", ".jsx": "javascript", ".mjs": "javascript",
	".py": "python", ".rs": "rust", ".java": "java", ".kt": "kotlin",
	".rb": "ruby", ".php": "php", ".c": "c", ".h": "c", ".cpp": "cpp",
	".cc": "cpp", ".hpp": "cpp", ".cs": "csharp", ".swift": "swift",
	".dart": "dart", ".sh": "shell", ".sql": "sql", ".css": "css",
	".html": "html", ".vue": "vue", ".svelte": "svelte",
}

// Scan walks the workspace and builds a fresh Model. It does not persist;
// callers use Save (or Refresh which does both).
func Scan(root string) (*Model, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if fi, err := os.Stat(abs); err != nil || !fi.IsDir() {
		return nil, fmt.Errorf("workspace root %q is not a directory", root)
	}
	m := &Model{Root: abs, GeneratedAt: time.Now().UTC(), Languages: map[string]int{}}
	err = filepath.WalkDir(abs, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entries are skipped, not fatal
		}
		if d.IsDir() {
			if path != abs && ignoredDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		rel, err := filepath.Rel(abs, path)
		if err != nil {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		lang := langByExt[strings.ToLower(filepath.Ext(rel))]
		if lang != "" {
			m.Languages[lang]++
		}
		m.FileCount++
		if len(m.Files) < maxIndexedFiles {
			m.Files = append(m.Files, File{Path: rel, Size: info.Size(), Lang: lang})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(m.Files, func(i, j int) bool { return m.Files[i].Path < m.Files[j].Path })
	detectFrameworks(m)
	detectBuildTargets(m)
	detectEntryPoints(m)
	return m, nil
}

// Refresh rescans the workspace and persists the fresh index.
func Refresh(root string) (*Model, error) {
	m, err := Scan(root)
	if err != nil {
		return nil, err
	}
	if err := m.Save(); err != nil {
		return nil, err
	}
	return m, nil
}

// Load reads the persisted index for a workspace root. It fails if no index
// has been persisted yet.
func Load(root string) (*Model, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(abs, StateDir, indexFile))
	if err != nil {
		return nil, err
	}
	var m Model
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("workspace index corrupt: %w", err)
	}
	m.Root = abs
	return &m, nil
}

// LoadOrScan loads the persisted index when present, otherwise scans and
// persists a fresh one — the cross-session entry point.
func LoadOrScan(root string) (*Model, error) {
	if m, err := Load(root); err == nil {
		return m, nil
	}
	return Refresh(root)
}

// Save persists the index under <root>/.cody/workspace.json atomically.
func (m *Model) Save() error {
	dir := filepath.Join(m.Root, StateDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, indexFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// PrimaryLanguages returns the detected languages ordered by file count
// descending — the stack the rules injection keys off.
func (m *Model) PrimaryLanguages() []string {
	langs := make([]string, 0, len(m.Languages))
	for l := range m.Languages {
		langs = append(langs, l)
	}
	sort.Slice(langs, func(i, j int) bool {
		if m.Languages[langs[i]] != m.Languages[langs[j]] {
			return m.Languages[langs[i]] > m.Languages[langs[j]]
		}
		return langs[i] < langs[j]
	})
	return langs
}

// Summary renders a compact human/model-readable digest of the index for the
// orchestrator's understand step and worker grounding bundles.
func (m *Model) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "workspace %s: %d files", m.Root, m.FileCount)
	if langs := m.PrimaryLanguages(); len(langs) > 0 {
		fmt.Fprintf(&b, "; languages: %s", strings.Join(langs, ", "))
	}
	if len(m.Frameworks) > 0 {
		fmt.Fprintf(&b, "; frameworks: %s", strings.Join(m.Frameworks, ", "))
	}
	if len(m.BuildTargets) > 0 {
		fmt.Fprintf(&b, "; build targets: %s", strings.Join(m.BuildTargets, ", "))
	}
	if len(m.EntryPoints) > 0 {
		fmt.Fprintf(&b, "; entry points: %s", strings.Join(m.EntryPoints, ", "))
	}
	return b.String()
}

// has reports whether an indexed file exists at rel (exact match).
func (m *Model) has(rel string) bool {
	i := sort.Search(len(m.Files), func(i int) bool { return m.Files[i].Path >= rel })
	return i < len(m.Files) && m.Files[i].Path == rel
}

var knownJSFrameworks = []string{
	"next", "react", "vue", "svelte", "express", "fastify", "astro",
	"remix", "nest", "vite", "vitest", "jest",
}

var knownPyFrameworks = []string{"django", "flask", "fastapi", "pytest"}

func detectFrameworks(m *Model) {
	seen := map[string]bool{}
	add := func(name string) {
		if name != "" && !seen[name] {
			seen[name] = true
			m.Frameworks = append(m.Frameworks, name)
		}
	}
	if deps := readPackageJSONDeps(filepath.Join(m.Root, "package.json")); deps != nil {
		for _, fw := range knownJSFrameworks {
			if _, ok := deps[fw]; ok {
				add(fw)
			}
		}
		if _, ok := deps["@nestjs/core"]; ok {
			add("nest")
		}
	}
	if m.has("go.mod") {
		add("go-module")
	}
	if m.has("Cargo.toml") {
		add("cargo")
	}
	if m.has("pyproject.toml") || m.has("setup.py") {
		add("python-project")
		if data, err := os.ReadFile(filepath.Join(m.Root, "pyproject.toml")); err == nil {
			low := strings.ToLower(string(data))
			for _, fw := range knownPyFrameworks {
				if strings.Contains(low, fw) {
					add(fw)
				}
			}
		}
	}
	sort.Strings(m.Frameworks)
}

var makefileTarget = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_./-]*):(?:[^=]|$)`)

func detectBuildTargets(m *Model) {
	seen := map[string]bool{}
	add := func(t string) {
		if t != "" && !seen[t] {
			seen[t] = true
			m.BuildTargets = append(m.BuildTargets, t)
		}
	}
	if scripts := readPackageJSONScripts(filepath.Join(m.Root, "package.json")); len(scripts) > 0 {
		names := make([]string, 0, len(scripts))
		for name := range scripts {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			add("npm:" + name)
		}
	}
	if f, err := os.Open(filepath.Join(m.Root, "Makefile")); err == nil {
		sc := bufio.NewScanner(f)
		var targets []string
		for sc.Scan() {
			if mt := makefileTarget.FindStringSubmatch(sc.Text()); mt != nil && mt[1] != ".PHONY" {
				targets = append(targets, "make:"+mt[1])
			}
		}
		f.Close()
		sort.Strings(targets)
		for _, t := range targets {
			add(t)
		}
	}
	if m.has("go.mod") {
		add("go:build")
		add("go:test")
	}
	if m.has("Cargo.toml") {
		add("cargo:build")
		add("cargo:test")
	}
	if m.has("pyproject.toml") || m.has("pytest.ini") || m.has("setup.py") {
		add("pytest")
	}
}

func detectEntryPoints(m *Model) {
	candidates := []string{
		"main.go", "src/index.ts", "src/index.tsx", "src/main.ts",
		"src/main.tsx", "src/index.js", "index.js", "app/page.tsx",
		"src/app/page.tsx", "main.py", "app.py", "src/main.rs", "src/lib.rs",
	}
	for _, c := range candidates {
		if m.has(filepath.FromSlash(c)) {
			m.EntryPoints = append(m.EntryPoints, c)
		}
	}
	for _, f := range m.Files {
		p := filepath.ToSlash(f.Path)
		if strings.HasPrefix(p, "cmd/") && strings.HasSuffix(p, "/main.go") {
			m.EntryPoints = append(m.EntryPoints, p)
		}
	}
	if pkgMain := readPackageJSONMain(filepath.Join(m.Root, "package.json")); pkgMain != "" {
		if m.has(filepath.FromSlash(pkgMain)) {
			m.EntryPoints = append(m.EntryPoints, pkgMain)
		}
	}
	sort.Strings(m.EntryPoints)
	m.EntryPoints = dedupe(m.EntryPoints)
}

func dedupe(in []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

type packageJSON struct {
	Main            string            `json:"main"`
	Scripts         map[string]string `json:"scripts"`
	Dependencies    map[string]string `json:"dependencies"`
	DevDependencies map[string]string `json:"devDependencies"`
}

func readPackageJSON(path string) *packageJSON {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var pkg packageJSON
	if json.Unmarshal(data, &pkg) != nil {
		return nil
	}
	return &pkg
}

func readPackageJSONDeps(path string) map[string]string {
	pkg := readPackageJSON(path)
	if pkg == nil {
		return nil
	}
	deps := map[string]string{}
	for k, v := range pkg.Dependencies {
		deps[k] = v
	}
	for k, v := range pkg.DevDependencies {
		deps[k] = v
	}
	return deps
}

func readPackageJSONScripts(path string) map[string]string {
	if pkg := readPackageJSON(path); pkg != nil {
		return pkg.Scripts
	}
	return nil
}

func readPackageJSONMain(path string) string {
	if pkg := readPackageJSON(path); pkg != nil {
		return pkg.Main
	}
	return ""
}
