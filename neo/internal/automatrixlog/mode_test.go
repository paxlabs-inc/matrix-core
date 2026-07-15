package automatrixlog

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreDirAndFileModes proves the Automatrix inbox log creates its directory
// 0700 and the log file 0600 on a really-created path.
func TestStoreDirAndFileModes(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "automatrixlog")
	s := Open(dir)
	if _, err := s.Append(Record{OpportunitySummary: "found a task", ResultSummary: "done", ConversationID: "conv-1"}); err != nil {
		t.Fatalf("append: %v", err)
	}

	if got := permOf(t, dir); got != 0o700 {
		t.Fatalf("dir mode = %o, want 0700", got)
	}
	assertFilePerms(t, dir, 0o600)
}

func permOf(t *testing.T, p string) os.FileMode {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatalf("stat %s: %v", p, err)
	}
	return fi.Mode().Perm()
}

func assertFilePerms(t *testing.T, dir string, want os.FileMode) {
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
		if got := permOf(t, filepath.Join(dir, e.Name())); got != want {
			t.Fatalf("file %s mode = %o, want %o", e.Name(), got, want)
		}
	}
	if seen == 0 {
		t.Fatal("no files created to assert modes on")
	}
}
