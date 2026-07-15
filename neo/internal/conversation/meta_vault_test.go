package conversation

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"matrix/vault"
)

// TestVaultMetaSidecarRoundTrip proves the conversation meta sidecar is sealed
// on disk (no plaintext project tag) yet reads back through the store.
func TestVaultMetaSidecarRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := sealingStore(t, dir, "did:matrix:alice", 1000)
	convID := "conv-meta"

	s.SetProject(convID, "proj-secret-tag")

	raw, err := os.ReadFile(s.metaPathLocked(convID))
	if err != nil {
		t.Fatalf("read meta sidecar: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("meta sidecar is not sealed")
	}
	if strings.Contains(string(raw), "proj-secret-tag") {
		t.Fatal("plaintext leaked into the sealed meta sidecar")
	}
	if got := s.Project(convID); got != "proj-secret-tag" {
		t.Fatalf("Project = %q, want proj-secret-tag", got)
	}
	// Idempotent re-set on an already-sealed sidecar (read-modify-write path).
	s.SetProject(convID, "proj-secret-tag")
	if got := s.Project(convID); got != "proj-secret-tag" {
		t.Fatalf("Project after re-set = %q", got)
	}
}

// TestVaultMetaSidecarWrongUser proves another user's key cannot read the
// sealed sidecar — the tag resolves empty.
func TestVaultMetaSidecarWrongUser(t *testing.T) {
	dir := t.TempDir()
	s := sealingStore(t, dir, "did:matrix:alice", 1000)
	s.SetProject("conv-meta", "private-project")

	s2 := sealingStore(t, dir, "did:matrix:mallory", 1000)
	if got := s2.Project("conv-meta"); got != "" {
		t.Fatalf("wrong user read sealed meta: %q", got)
	}
}

// TestVaultMetaSidecarLegacyPlaintext proves a pre-vault plaintext sidecar
// stays readable and is re-sealed by the next write.
func TestVaultMetaSidecarLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	plain := openWithCap(dir, 1000)
	plain.SetProject("conv-old", "legacy-project")
	if b, _ := os.ReadFile(plain.metaPathLocked("conv-old")); !json.Valid(b) {
		t.Fatal("seed sidecar should be plaintext JSON")
	}

	s := sealingStore(t, dir, "did:matrix:alice", 1000)
	if got := s.Project("conv-old"); got != "legacy-project" {
		t.Fatalf("legacy plaintext unreadable: %q", got)
	}
	s.SetProject("conv-old", "migrated-project")
	raw, _ := os.ReadFile(s.metaPathLocked("conv-old"))
	if !vault.IsVault(raw) {
		t.Fatal("meta sidecar not re-sealed after a vault write")
	}
	if got := s.Project("conv-old"); got != "migrated-project" {
		t.Fatalf("Project after re-seal = %q", got)
	}
}
