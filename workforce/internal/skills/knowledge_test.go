package skills

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/policy"
)

func TestExecutiveResearchPackMatchesSignedMandatesAndLeaksNoAuthority(t *testing.T) {
	pack, err := ExecutiveResearchPack()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) != 6 {
		t.Fatalf("knowledge pack contains %d contracts, want 6", len(pack))
	}
	catalog, err := NewCatalog(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]contracts.SkillID{
		ExecutiveSkillIDs(), ResearchSkillIDs(),
	} {
		refs, err := catalog.Resolve(ids)
		if err != nil || len(refs) != len(ids) {
			t.Fatalf("resolve mandate skills %#v = %#v, %v", ids, refs, err)
		}
	}
	for _, contract := range pack {
		if len(contract.Approvals) != 0 || contract.Resources.EffectCalls != 0 {
			t.Fatalf("%s acquired approvals or effect calls", contract.ID)
		}
		for _, operation := range contract.Operations {
			if operation.EffectClass != EffectRead {
				t.Fatalf("%s operation %s is effectful", contract.ID, operation.Name)
			}
			lower := strings.ToLower(operation.Capability)
			if strings.Contains(lower, "approve") ||
				strings.Contains(lower, "effect") ||
				strings.Contains(lower, "publish") ||
				strings.Contains(lower, "spend") {
				t.Fatalf("%s leaks authority through %q", contract.ID, operation.Capability)
			}
		}
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	seed, err := policy.BuildSeed(
		"organization:knowledge", "owner:knowledge", "Knowledge Org",
		now, "owner-key", privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[contracts.DepartmentKind][]contracts.SkillID{
		contracts.DepartmentExecutive: ExecutiveSkillIDs(),
		contracts.DepartmentResearch:  ResearchSkillIDs(),
	}
	for _, mandate := range seed.Mandates {
		want, relevant := expected[mandate.DepartmentKind]
		if !relevant {
			continue
		}
		if strings.Join(skillStrings(mandate.AllowedSkills), "|") !=
			strings.Join(skillStrings(want), "|") {
			t.Fatalf(
				"%s mandate skills %#v do not match pack %#v",
				mandate.DepartmentKind, mandate.AllowedSkills, want,
			)
		}
	}
}

func skillStrings(values []contracts.SkillID) []string {
	result := make([]string, len(values))
	for index, value := range values {
		result[index] = string(value)
	}
	return result
}
