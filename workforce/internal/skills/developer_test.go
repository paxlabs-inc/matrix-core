package skills

import (
	"testing"

	"matrix/workforce/internal/contracts"
)

func TestDeveloperPackIsCompleteVersionedAndExecutable(t *testing.T) {
	pack, err := DeveloperPack()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) != 5 {
		t.Fatalf("developer pack contains %d skills, want 5", len(pack))
	}
	catalog, err := NewCatalog(pack)
	if err != nil {
		t.Fatal(err)
	}
	ids := []contracts.SkillID{
		DeveloperPlanSkill, DeveloperImplementSkill, DeveloperVerifySkill,
		DeveloperReviewHandoffSkill, DeveloperBrainUpdateSkill,
	}
	refs, err := catalog.Resolve(ids)
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != len(ids) || catalog.Digest().Digest == "" {
		t.Fatalf("developer catalog refs=%#v digest=%#v", refs, catalog.Digest())
	}
	for _, contract := range pack {
		if err := contract.Validate(); err != nil {
			t.Fatalf("%s: %v", contract.ID, err)
		}
		if err := contract.AuthorizeUnattended(true); err != nil {
			t.Fatalf("%s is drift-blind: %v", contract.ID, err)
		}
	}
}
