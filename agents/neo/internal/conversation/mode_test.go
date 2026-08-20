package conversation

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreDirAndFileModes proves the conversation store creates its data
// directory 0700 and every record file 0600 on a really-created path.
func TestStoreDirAndFileModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "conversations")
	s := Open(dir)
	s.AppendUser("conv-1", "intent-1", "hello sealed world")
	s.SetProject("conv-1", "proj-1")

	if got := statPerm(t, dir); got != 0o700 {
		t.Fatalf("dir mode = %o, want 0700", got)
	}
	assertFilesPerm(t, dir, 0o600)
}

func statPerm(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode().Perm()
}

func assertFilesPerm(t *testing.T, dir string, want os.FileMode) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir %s: %v", dir, err)
	}
	seen := 0
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		seen++
		if got := statPerm(t, filepath.Join(dir, e.Name())); got != want {
			t.Fatalf("file %s mode = %o, want %o", e.Name(), got, want)
		}
	}
	if seen == 0 {
		t.Fatal("no files created to assert modes on")
	}
}
