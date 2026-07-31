package skills

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/policy"
)

func TestMarketingLegalPackMatchesMandatesAndPublicationIsOnlyAGate(t *testing.T) {
	knowledge, err := ExecutiveResearchPack()
	if err != nil {
		t.Fatal(err)
	}
	pack, err := MarketingLegalPack()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) != 9 {
		t.Fatalf("marketing/legal pack contains %d contracts, want 9", len(pack))
	}
	catalog, err := NewCatalog(append(knowledge, pack...))
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]contracts.SkillID{MarketingSkillIDs(), LegalSkillIDs()} {
		if refs, err := catalog.Resolve(ids); err != nil || len(refs) != len(ids) {
			t.Fatalf("resolve mandate skills %#v = %#v, %v", ids, refs, err)
		}
	}
	for _, contract := range pack {
		if contract.Resources.EffectCalls != 0 {
			t.Fatalf("%s directly owns public-effect calls", contract.ID)
		}
		for _, operation := range contract.Operations {
			if operation.EffectClass != EffectRead {
				t.Fatalf("%s operation %s bypasses the publication gate", contract.ID, operation.Name)
			}
		}
		if contract.ID == PublicationGatesSkill {
			if strings.Join(contract.Approvals, "|") != "human_publication_approval" {
				t.Fatalf("publication approvals = %#v", contract.Approvals)
			}
		} else if len(contract.Approvals) != 0 {
			t.Fatalf("%s unexpectedly consumes approval", contract.ID)
		}
	}

	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	seed, err := policy.BuildSeed(
		"organization:domain", "owner:domain", "Domain Org",
		time.Now().UTC(), "owner-key", privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[contracts.DepartmentKind][]contracts.SkillID{
		contracts.DepartmentMarketing: MarketingSkillIDs(),
		contracts.DepartmentLegal:     LegalSkillIDs(),
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
