package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	machineidentity "centra/packages/machine/identity"
)

func TestRunEnsuresAndReusesIdentity(t *testing.T) {
	root := t.TempDir()
	first := runCommand(t, root)
	second := runCommand(t, root)
	if first != second {
		t.Fatalf("identity changed across invocations: first=%+v second=%+v", first, second)
	}
}

func TestRunRefusesCorruptIdentity(t *testing.T) {
	root := t.TempDir()
	_ = runCommand(t, root)
	genesis := filepath.Join(machineidentity.RuntimeConfig(root).Dir, "state", "genesis.json")
	if err := os.WriteFile(genesis, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-data-root", root, "ensure"}, &stdout, &stderr); code != 1 {
		t.Fatalf("exit code=%d, want 1; stderr=%s", code, stderr.String())
	}
	if stdout.Len() != 0 {
		t.Fatalf("corrupt state returned output: %s", stdout.String())
	}
}

func runCommand(t *testing.T, root string) machineidentity.Descriptor {
	t.Helper()
	var stdout, stderr bytes.Buffer
	if code := run(context.Background(), []string{"-data-root", root, "ensure"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit code=%d; stderr=%s", code, stderr.String())
	}
	var descriptor machineidentity.Descriptor
	if err := json.Unmarshal(stdout.Bytes(), &descriptor); err != nil {
		t.Fatal(err)
	}
	return descriptor
}
