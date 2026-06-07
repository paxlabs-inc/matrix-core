package compiler

import (
	"path/filepath"
	"testing"

	"github.com/paxlabs-inc/tachyon-tools/internal/registry"
	"github.com/paxlabs-inc/tachyon-tools/pkg/types"
)

func TestCompileCreate2(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	regPath := filepath.Join(t.TempDir(), "registry.json")
	reg, err := registry.Open(regPath)
	if err != nil {
		t.Fatal(err)
	}
	c := &Compiler{ForgePath: "forge", ArtifactsDir: "artifacts"}
	resp, apiErr := c.Compile(types.CompileRequest{
		ProjectRoot: root,
		Targets:     []string{"Create2"},
	}, reg)
	if apiErr != nil {
		t.Fatalf("compile error: %s", apiErr.Message)
	}
	if len(resp.Artifacts) == 0 {
		t.Fatal("expected artifacts")
	}
	found := false
	for _, a := range resp.Artifacts {
		if a.Name == "Create2" {
			found = true
		}
	}
	if !found {
		t.Fatalf("Create2 not in artifacts: %d total", len(resp.Artifacts))
	}
}
