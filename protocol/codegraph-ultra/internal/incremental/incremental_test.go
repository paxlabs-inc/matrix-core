package incremental

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewFileManifest(t *testing.T) {
	m := NewFileManifest()
	if m.Files == nil {
		t.Error("Files map is nil")
	}
	if len(m.Files) != 0 {
		t.Errorf("Files len = %d, want 0", len(m.Files))
	}
}

func TestComputeFileHash(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "test.txt")
	os.WriteFile(path, []byte("hello world"), 0o644)

	hash, err := ComputeFileHash(path)
	if err != nil {
		t.Fatalf("ComputeFileHash: %v", err)
	}
	if hash == "" {
		t.Error("hash is empty")
	}

	// Same content should produce same hash
	hash2, _ := ComputeFileHash(path)
	if hash != hash2 {
		t.Error("same file produced different hashes")
	}

	// Different content should produce different hash
	path2 := filepath.Join(tmpDir, "test2.txt")
	os.WriteFile(path2, []byte("goodbye world"), 0o644)
	hash3, _ := ComputeFileHash(path2)
	if hash == hash3 {
		t.Error("different files produced same hash")
	}
}

func TestScanDirectory(t *testing.T) {
	tmpDir := t.TempDir()
	os.WriteFile(filepath.Join(tmpDir, "main.go"), []byte("package main"), 0o644)
	os.MkdirAll(filepath.Join(tmpDir, "sub"), 0o755)
	os.WriteFile(filepath.Join(tmpDir, "sub", "lib.go"), []byte("package lib"), 0o644)
	os.WriteFile(filepath.Join(tmpDir, "readme.txt"), []byte("readme"), 0o644)

	exts := map[string]string{".go": "go"}
	m, err := ScanDirectory(tmpDir, exts)
	if err != nil {
		t.Fatalf("ScanDirectory: %v", err)
	}

	if len(m.Files) != 2 {
		t.Errorf("Files len = %d, want 2", len(m.Files))
	}
}

func TestDiffEmpty(t *testing.T) {
	m := NewFileManifest()
	m.Files["a.go"] = "hash1"
	m.Files["b.go"] = "hash2"

	added, removed, changed := m.Diff(nil)
	if len(added) != 2 {
		t.Errorf("added = %d, want 2", len(added))
	}
	if len(removed) != 0 {
		t.Errorf("removed = %d, want 0", len(removed))
	}
	if len(changed) != 0 {
		t.Errorf("changed = %d, want 0", len(changed))
	}
}

func TestDiffChanges(t *testing.T) {
	old := NewFileManifest()
	old.Files["a.go"] = "hash1"
	old.Files["b.go"] = "hash2"
	old.Files["c.go"] = "hash3"

	newManifest := NewFileManifest()
	newManifest.Files["a.go"] = "hash1"      // unchanged
	newManifest.Files["b.go"] = "hash2_new"  // changed
	newManifest.Files["d.go"] = "hash4"      // added

	added, removed, changed := newManifest.Diff(old)
	if len(added) != 1 || added[0] != "d.go" {
		t.Errorf("added = %v, want [d.go]", added)
	}
	if len(removed) != 1 || removed[0] != "c.go" {
		t.Errorf("removed = %v, want [c.go]", removed)
	}
	if len(changed) != 1 || changed[0] != "b.go" {
		t.Errorf("changed = %v, want [b.go]", changed)
	}
}

func TestAffectedPackages(t *testing.T) {
	files := []string{
		"pkg/a.go",
		"pkg/b.go",
		"internal/c.go",
		"main.go",
	}
	pkgs := AffectedPackages(files)
	if len(pkgs) != 3 {
		t.Errorf("AffectedPackages = %d, want 3", len(pkgs))
	}
}

func TestIsSourceFile(t *testing.T) {
	tests := []struct {
		path string
		want bool
	}{
		{"main.go", true},
		{"app.py", true},
		{"index.ts", true},
		{"readme.md", false},
		{"image.png", false},
	}
	for _, tt := range tests {
		got := IsSourceFile(tt.path)
		if got != tt.want {
			t.Errorf("IsSourceFile(%q) = %v, want %v", tt.path, got, tt.want)
		}
	}
}

func TestFilterSourceFiles(t *testing.T) {
	files := []string{"main.go", "readme.md", "app.py", "image.png"}
	filtered := FilterSourceFiles(files)
	if len(filtered) != 2 {
		t.Errorf("FilterSourceFiles = %d, want 2", len(filtered))
	}
}

func TestStoreAndLoadManifest(t *testing.T) {
	tmpDir := t.TempDir()
	path := filepath.Join(tmpDir, "manifest.kvx")

	m := NewFileManifest()
	m.Files["a.go"] = "hash1"
	m.Files["b.go"] = "hash2"

	if err := StoreManifest(m, path); err != nil {
		t.Fatalf("StoreManifest: %v", err)
	}

	loaded, err := LoadManifest(path)
	if err != nil {
		t.Fatalf("LoadManifest: %v", err)
	}
	if len(loaded.Files) != 2 {
		t.Errorf("loaded Files len = %d, want 2", len(loaded.Files))
	}
	if loaded.Files["a.go"] != "hash1" {
		t.Errorf("loaded Files[a.go] = %q, want %q", loaded.Files["a.go"], "hash1")
	}
}
