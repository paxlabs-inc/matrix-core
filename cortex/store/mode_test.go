package store

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStoreDirMode proves the cortex Pebble store directory is created 0700 on a
// really-created path (Pebble owns the file modes inside; volume encryption is
// the documented complement for the values themselves).
func TestStoreDirMode(t *testing.T) {
	root := t.TempDir()
	s, err := Open(root, "alice", nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()

	dbDir := filepath.Join(root, "alice", "store")
	fi, err := os.Stat(dbDir)
	if err != nil {
		t.Fatalf("stat %s: %v", dbDir, err)
	}
	if got := fi.Mode().Perm(); got != 0o700 {
		t.Fatalf("cortex store dir mode = %o, want 0700", got)
	}
}
