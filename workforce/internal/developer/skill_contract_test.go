package developer_test

import (
	"testing"

	"matrix/workforce/internal/developer"
	"matrix/workforce/internal/skills"
)

func TestEveryDeclaredDeveloperSkillOperationHasExecutableAdapterSupport(t *testing.T) {
	pack, err := skills.DeveloperPack()
	if err != nil {
		t.Fatal(err)
	}
	declared := make(map[string]bool)
	for _, contract := range pack {
		for _, operation := range contract.Operations {
			if !developer.SupportsOperation(operation.Name) {
				t.Fatalf(
					"skill %s declares operation %q without executable adapter support",
					contract.ID, operation.Name,
				)
			}
			declared[operation.Name] = true
		}
	}
	for _, expected := range []string{
		"plan_change", "inspect_source", "apply_scoped_change",
		"restore_source_snapshot", "run_verification", "inspect_handoff",
		"publish_review_handoff", "inspect_project_brain",
		"propose_verified_record",
	} {
		if !declared[expected] {
			t.Fatalf("executable Developer operation %q is absent from skill contracts", expected)
		}
	}
	if developer.SupportsOperation("ambient_shell") {
		t.Fatal("adapter advertised undeclared ambient operation")
	}
}
