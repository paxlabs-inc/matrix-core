// Package incremental provides file-level content hashing for incremental graph builds.
// Instead of rebuilding the entire graph on every invocation, it detects which files
// changed since the last build and only re-extracts affected packages.
package incremental

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"centra/protocol/codegraph-ultra/internal/model"
)

// FileManifest tracks file hashes for incremental builds.
type FileManifest struct {
	Files map[string]string `json:"files"` // path -> sha256 hex
}

// NewFileManifest creates an empty manifest.
func NewFileManifest() *FileManifest {
	return &FileManifest{Files: make(map[string]string)}
}

// ComputeFileHash returns the sha256 hex of a file's content.
func ComputeFileHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:]), nil
}

// ScanDirectory walks a directory and computes hashes for all source files
// matching the given language extensions.
func ScanDirectory(root string, exts map[string]string) (*FileManifest, error) {
	m := NewFileManifest()
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // Skip errors
		}
		if info.IsDir() {
			base := filepath.Base(path)
			if base == ".git" || base == "node_modules" || base == "vendor" ||
				base == "__pycache__" || base == ".venv" || base == "dist" ||
				base == "build" || base == ".next" || base == "testdata" {
				return filepath.SkipDir
			}
			return nil
		}
		ext := filepath.Ext(path)
		if _, ok := exts[ext]; !ok {
			return nil
		}
		hash, err := ComputeFileHash(path)
		if err != nil {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		m.Files[rel] = hash
		return nil
	})
	return m, err
}

// Diff compares two manifests and returns added, removed, and changed files.
func (m *FileManifest) Diff(old *FileManifest) (added, removed, changed []string) {
	if old == nil {
		for path := range m.Files {
			added = append(added, path)
		}
		sort.Strings(added)
		return
	}

	for path, hash := range m.Files {
		oldHash, exists := old.Files[path]
		if !exists {
			added = append(added, path)
		} else if oldHash != hash {
			changed = append(changed, path)
		}
	}
	for path := range old.Files {
		if _, exists := m.Files[path]; !exists {
			removed = append(removed, path)
		}
	}

	sort.Strings(added)
	sort.Strings(removed)
	sort.Strings(changed)
	return
}

// AffectedPackages returns the set of package paths affected by the given file changes.
// A package is affected if any file in it was added, changed, or removed.
func AffectedPackages(files []string) []string {
	pkgSet := make(map[string]bool)
	for _, f := range files {
		dir := filepath.Dir(f)
		if dir == "." {
			dir = ""
		}
		pkgSet[dir] = true
	}
	pkgs := make([]string, 0, len(pkgSet))
	for pkg := range pkgSet {
		pkgs = append(pkgs, pkg)
	}
	sort.Strings(pkgs)
	return pkgs
}

// IsSourceFile checks if a file path is a supported source file.
func IsSourceFile(path string) bool {
	ext := filepath.Ext(path)
	_, ok := model.LanguageFromExt[ext]
	return ok
}

// FilterSourceFiles returns only source files from a list of paths.
func FilterSourceFiles(files []string) []string {
	var out []string
	for _, f := range files {
		if IsSourceFile(f) {
			out = append(out, f)
		}
	}
	return out
}

// StoreManifest persists a manifest to a file as simple key=value lines.
func StoreManifest(m *FileManifest, path string) error {
	var lines []string
	paths := make([]string, 0, len(m.Files))
	for p := range m.Files {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		lines = append(lines, p+"="+m.Files[p])
	}
	return os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// LoadManifest reads a manifest from a file.
func LoadManifest(path string) (*FileManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	m := NewFileManifest()
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) == 2 {
			m.Files[parts[0]] = parts[1]
		}
	}
	return m, nil
}
