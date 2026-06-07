package engine

import (
	"context"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/paxlabs-inc/tachyon-tools/internal/config"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

// counterSource is a self-contained contract (no external imports) so the
// uploaded-source compile path can be exercised without the box's dependency
// corpus being present.
const counterSource = `// SPDX-License-Identifier: MIT
pragma solidity ^0.8.20;

contract Counter {
    uint256 public value;

    function set(uint256 v) external {
        value = v;
    }
}
`

// TestCompileFromUploadedSources verifies the shared-engine source-upload path:
// a self-contained `sources` map compiles in an ephemeral workdir, yields the
// deterministic ProjectID, and registers an artifact resolvable by that id.
// Skips when `forge` is not installed.
func TestCompileFromUploadedSources(t *testing.T) {
	if _, err := exec.LookPath("forge"); err != nil {
		t.Skip("forge not installed; skipping uploaded-source compile test")
	}

	root := t.TempDir()
	e, err := New(config.Config{
		ProjectRoot:  root,
		ArtifactsDir: "artifacts",
		RegistryPath: filepath.Join(root, "registry.json"),
		ForgePath:    "forge",
	})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}

	sources := map[string]string{"src/Counter.sol": counterSource}
	env := e.Compile(context.Background(), types.CompileRequest{Sources: sources})
	if !env.Ok {
		t.Fatalf("compile failed: %+v", env.Error)
	}

	wantID := sourcesProjectID(sources)
	if env.Data.ProjectID != wantID {
		t.Fatalf("project id = %q, want deterministic %q", env.Data.ProjectID, wantID)
	}

	var found bool
	for _, a := range env.Data.Artifacts {
		if a.Name == "Counter" && len(a.ABI) > 0 && a.Bytecode != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Counter artifact missing from compile response: %+v", env.Data.Artifacts)
	}

	// The artifact must be resolvable from the registry by the derived id,
	// which is exactly how a subsequent deploy/call locates it.
	got := e.ArtifactGet(types.ArtifactGetRequest{ProjectID: wantID, Name: "Counter"})
	if !got.Ok {
		t.Fatalf("artifact_get by project id failed: %+v", got.Error)
	}
}
