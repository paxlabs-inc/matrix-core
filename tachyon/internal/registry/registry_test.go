package registry

import (
	"path/filepath"
	"testing"
)

func TestRegistryRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "registry.json")
	reg, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rec := ArtifactRecord{
		ProjectID: "proj1",
		Name:      "Create2",
		Bytecode:  "0x6000",
		ABI:       []byte(`[]`),
	}
	if err := reg.PutArtifact(rec); err != nil {
		t.Fatal(err)
	}
	got, ok := reg.GetArtifact("proj1", "Create2")
	if !ok || got.Name != "Create2" {
		t.Fatalf("artifact missing: %+v ok=%v", got, ok)
	}
	dep := DeploymentRecord{
		IdempotencyKey: "k1",
		ChainID:        "local-anvil",
		Contract:       "Create2",
		Address:        "0xabc",
		Confirmed:      true,
	}
	if err := reg.PutDeployment(dep); err != nil {
		t.Fatal(err)
	}
	d, ok := reg.GetDeployment("k1", "local-anvil")
	if !ok || d.Address != "0xabc" {
		t.Fatalf("deployment missing: %+v", d)
	}
}
