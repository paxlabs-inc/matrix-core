// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package capabilityhub

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestCapabilityLifecyclePersistsAndRollsBack(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	source := corpusSkill(t, "brainstorming")
	first, err := store.ImportDirectory(ctx, ImportRequest{SourceDir: source, SourceType: SourceLibrary, SourceRef: "matrix-library/brainstorming"})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StateQuarantine || first.Digest == "" || first.CanonicalHash == "" {
		t.Fatalf("unexpected quarantined capability: %+v", first)
	}
	first, err = store.Verify(ctx, first.Slug, first.Version, Verification{})
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StateVerified || first.VerifiedAt == nil {
		t.Fatalf("unexpected verified capability: %+v", first)
	}
	first, err = store.Activate(ctx, first.Slug, first.Version)
	if err != nil {
		t.Fatal(err)
	}
	if first.State != StateActive {
		t.Fatalf("state = %s", first.State)
	}

	secondSource := t.TempDir()
	if err := copyPackage(source, secondSource); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(secondSource, "SKILL.mtx")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	manifest = bytes.Replace(manifest, []byte("version=0.1.0"), []byte("version=0.2.0"), 1)
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := store.ImportDirectory(ctx, ImportRequest{SourceDir: secondSource, SourceType: SourceAuthored, SourceRef: "proposal/session-2"})
	if err != nil {
		t.Fatal(err)
	}
	second, err = store.Verify(ctx, second.Slug, second.Version, Verification{})
	if err != nil {
		t.Fatal(err)
	}
	second, err = store.Activate(ctx, second.Slug, second.Version)
	if err != nil {
		t.Fatal(err)
	}
	if second.Version != "0.2.0" || second.State != StateActive {
		t.Fatalf("unexpected second activation: %+v", second)
	}
	rolledBack, err := store.Rollback(ctx, second.Slug)
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Version != "0.1.0" || rolledBack.State != StateActive {
		t.Fatalf("unexpected rollback: %+v", rolledBack)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	versions, err := reopened.Versions(ctx, "brainstorming")
	if err != nil {
		t.Fatal(err)
	}
	if len(versions) != 2 {
		t.Fatalf("versions = %d", len(versions))
	}
	var active string
	for _, capability := range versions {
		if capability.State == StateActive {
			active = capability.Version
		}
	}
	if active != "0.1.0" {
		t.Fatalf("active after reopen = %q", active)
	}
}

func TestCapabilityVerificationRequiresExactGrantsAndAvailableTools(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	capability, err := store.ImportDirectory(ctx, ImportRequest{
		SourceDir: corpusSkill(t, "using-the-desktop"), SourceType: SourceLibrary, SourceRef: "matrix-library/using-the-desktop",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(capability.DeclaredTools) == 0 {
		t.Fatal("expected declared tools")
	}
	if _, err := store.Verify(ctx, capability.Slug, capability.Version, Verification{}); !errors.Is(err, ErrGrantRequired) {
		t.Fatalf("verify without grants: %v", err)
	}
	capability, err = store.Grant(ctx, capability.Slug, capability.Version, capability.DeclaredTools)
	if err != nil {
		t.Fatal(err)
	}
	if len(capability.Granted) != len(capability.DeclaredTools) {
		t.Fatalf("grants = %d tools = %d", len(capability.Granted), len(capability.DeclaredTools))
	}
	if _, err := store.Verify(ctx, capability.Slug, capability.Version, Verification{AvailableTools: map[string]string{}}); !errors.Is(err, ErrToolUnavailable) {
		t.Fatalf("verify unavailable tools: %v", err)
	}
	available := make(map[string]string, len(capability.DeclaredTools))
	for _, uri := range capability.DeclaredTools {
		available[toolName(uri)] = toolName(uri)
	}
	if _, err := store.Verify(ctx, capability.Slug, capability.Version, Verification{AvailableTools: available}); !errors.Is(err, ErrVerificationRequired) {
		t.Fatalf("verify without real tests: %v", err)
	}
	if _, err := store.Grant(ctx, capability.Slug, capability.Version, []string{"matrix://tool/mcp/unknown/root@1.0.0"}); !errors.Is(err, ErrGrantRequired) {
		t.Fatalf("undeclared grant: %v", err)
	}
}

func TestCapabilityVerificationExecutesDeclaredReadOnlyTool(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	packageDir := t.TempDir()
	if err := copyPackage(corpusSkill(t, "brainstorming"), packageDir); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(packageDir, "SKILL.mtx")
	manifest, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	toolURI := "matrix://tool/mcp/native/read_text_file@0.1.0"
	manifest = bytes.Replace(manifest, []byte("§TOOLS\nnone"), []byte("§TOOLS\n"+toolURI), 1)
	if err := os.WriteFile(manifestPath, manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	testSuite := `{"schema_version":1,"cases":[{"name":"reads its validated manifest","tool":"` + toolURI + `","arguments":{"path":"` + manifestPath + `"},"expect_contains":"id=brainstorming"}]}`
	if err := os.WriteFile(filepath.Join(packageDir, "CAPABILITY_TESTS.json"), []byte(testSuite), 0o600); err != nil {
		t.Fatal(err)
	}
	capability, err := store.ImportDirectory(ctx, ImportRequest{SourceDir: packageDir, SourceType: SourceAuthored, SourceRef: "proposal/real-read-test"})
	if err != nil {
		t.Fatal(err)
	}
	capability, err = store.Grant(ctx, capability.Slug, capability.Version, capability.DeclaredTools)
	if err != nil {
		t.Fatal(err)
	}
	runs := 0
	verified, err := store.Verify(ctx, capability.Slug, capability.Version, Verification{
		AvailableTools: map[string]string{"read_text_file": "read_text_file"},
		ReadOnlyTools:  map[string]bool{"read_text_file": true},
		RunTool: func(name string, arguments map[string]any) (ToolTestResult, error) {
			runs++
			path, _ := arguments["path"].(string)
			body, readErr := os.ReadFile(path)
			return ToolTestResult{Content: string(body), IsError: readErr != nil}, readErr
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if verified.State != StateVerified || runs != 1 {
		t.Fatalf("verified=%+v runs=%d", verified, runs)
	}
}

func TestCapabilityImportRejectsSecretsSymlinksAndArchiveTraversal(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	secretPackage := t.TempDir()
	if err := copyPackage(corpusSkill(t, "brainstorming"), secretPackage); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(secretPackage, ".env"), []byte("API_KEY=abcdefghijklmnop"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportDirectory(ctx, ImportRequest{SourceDir: secretPackage, SourceType: SourceURL, SourceRef: "https://example.test/skill"}); !errors.Is(err, ErrUnsafePackage) {
		t.Fatalf("secret import: %v", err)
	}

	symlinkPackage := t.TempDir()
	if err := copyPackage(corpusSkill(t, "brainstorming"), symlinkPackage); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("SKILL.mtx", filepath.Join(symlinkPackage, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportDirectory(ctx, ImportRequest{SourceDir: symlinkPackage, SourceType: SourceLibrary, SourceRef: "test"}); !errors.Is(err, ErrUnsafePackage) {
		t.Fatalf("symlink import: %v", err)
	}

	var archive bytes.Buffer
	writer := zip.NewWriter(&archive)
	entry, err := writer.Create("../SKILL.mtx")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = entry.Write([]byte("unsafe"))
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := extractZip(archive.Bytes(), t.TempDir()); !errors.Is(err, ErrUnsafePackage) {
		t.Fatalf("archive traversal: %v", err)
	}
}

func TestCapabilitySameVersionCannotChangeContent(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	source := corpusSkill(t, "brainstorming")
	if _, err := store.ImportDirectory(ctx, ImportRequest{SourceDir: source, SourceType: SourceLibrary, SourceRef: "library"}); err != nil {
		t.Fatal(err)
	}
	changed := t.TempDir()
	if err := copyPackage(source, changed); err != nil {
		t.Fatal(err)
	}
	prose := filepath.Join(changed, "SKILL.md")
	body, err := os.ReadFile(prose)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(prose, append(body, []byte("\nChanged without a version bump.\n")...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportDirectory(ctx, ImportRequest{SourceDir: changed, SourceType: SourceAuthored, SourceRef: "changed"}); !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("same-version import: %v", err)
	}
}

func corpusSkill(t *testing.T, slug string) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime caller unavailable")
	}
	path := filepath.Clean(filepath.Join(filepath.Dir(file), "..", "..", "..", "skills", slug))
	if _, err := os.Stat(filepath.Join(path, "SKILL.mtx")); err != nil {
		t.Fatalf("real corpus skill %s unavailable: %v", slug, err)
	}
	return path
}

func TestToolName(t *testing.T) {
	if got := toolName("matrix://tool/mcp/git/git_status@2026.1.14"); got != "git_status" {
		t.Fatalf("toolName = %q", got)
	}
	if strings.TrimSpace(toolName("")) != "" {
		t.Fatal("empty URI produced a tool name")
	}
}

func TestCapabilityProvenanceQueueSurvivesUntilProjected(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ImportDirectory(ctx, ImportRequest{
		SourceDir: corpusSkill(t, "brainstorming"), SourceType: SourceLibrary, SourceRef: "matrix-library/brainstorming",
	}); err != nil {
		t.Fatal(err)
	}
	pending, err := store.PendingProvenance(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].Action != "import.quarantine" {
		t.Fatalf("pending provenance: %+v", pending)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(ctx, root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	pending, err = reopened.PendingProvenance(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 {
		t.Fatalf("pending after reopen = %d", len(pending))
	}
	if err := reopened.MarkProvenanceProjected(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}
	pending, err = reopened.PendingProvenance(ctx, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending after projection = %d", len(pending))
	}
}
