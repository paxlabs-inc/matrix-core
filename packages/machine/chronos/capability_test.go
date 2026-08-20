package chronos

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCapabilityStableAndFailClosed(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "chronos")
	first, err := EnsureCapability(directory)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EnsureCapability(directory)
	if err != nil || second != first {
		t.Fatalf("second=%q first=%q err=%v", second, first, err)
	}
	path := filepath.Join(directory, capabilityFile)
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := EnsureCapability(directory); err == nil {
		t.Fatal("weak capability permissions were accepted")
	}
}
