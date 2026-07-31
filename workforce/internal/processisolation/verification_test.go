package processisolation

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerificationCommandExecutesWithClosedReadOnlyWorkspace(t *testing.T) {
	workspace := t.TempDir()
	command, err := VerificationCommand(context.Background(), VerificationSpec{
		Bubblewrap: "/usr/bin/bwrap",
		Executable: "/usr/bin/env",
		Workspace:  workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	command.Stdout = &output
	command.Stderr = &output
	if err := command.Run(); err != nil {
		t.Fatalf("run isolated environment: %v: %s", err, output.String())
	}
	environment := output.String()
	for _, required := range []string{
		"PATH=/usr/local/go/bin:/usr/bin",
		"HOME=/tmp",
		"GOCACHE=/cache",
		"GOTOOLCHAIN=local",
		"CGO_ENABLED=0",
	} {
		if !strings.Contains(environment, required+"\n") {
			t.Fatalf("verification environment omits %q: %q", required, environment)
		}
	}
	if strings.Contains(environment, "HOST_SECRET") {
		t.Fatalf("verification inherited host environment: %q", environment)
	}

	command, err = VerificationCommand(context.Background(), VerificationSpec{
		Bubblewrap: "/usr/bin/bwrap",
		Executable: "/usr/bin/touch",
		Arguments:  []string{"/workspace/forbidden"},
		Workspace:  workspace,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Run(); err == nil {
		t.Fatal("verification wrote through the read-only workspace mount")
	}
	if _, err := os.Stat(filepath.Join(workspace, "forbidden")); !os.IsNotExist(err) {
		t.Fatalf("verification changed host workspace: %v", err)
	}
}

func TestVerificationCommandRejectsReplaceableInputs(t *testing.T) {
	workspace := t.TempDir()
	executable := filepath.Join(t.TempDir(), "verifier")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := VerificationCommand(context.Background(), VerificationSpec{
		Bubblewrap: "/usr/bin/bwrap",
		Executable: executable,
		Workspace:  workspace,
	}); err == nil {
		t.Fatal("verification accepted an executable outside the trusted image")
	}
	link := filepath.Join(t.TempDir(), "workspace")
	if err := os.Symlink(workspace, link); err != nil {
		t.Fatal(err)
	}
	if _, err := VerificationCommand(context.Background(), VerificationSpec{
		Bubblewrap: "/usr/bin/bwrap",
		Executable: "/usr/bin/true",
		Workspace:  link,
	}); err == nil {
		t.Fatal("verification accepted a symlink workspace")
	}
}
