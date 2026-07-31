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

func TestOperationsPackMatchesMandatesAndCannotMoveFunds(t *testing.T) {
	pack, err := OperationsPack()
	if err != nil {
		t.Fatal(err)
	}
	if len(pack) != 10 {
		t.Fatalf("operations pack contains %d contracts, want 10", len(pack))
	}
	catalog, err := NewCatalog(pack)
	if err != nil {
		t.Fatal(err)
	}
	for _, ids := range [][]contracts.SkillID{
		AccountingSkillIDs(), BackOfficeSkillIDs(),
	} {
		if refs, err := catalog.Resolve(ids); err != nil || len(refs) != len(ids) {
			t.Fatalf("resolve mandate skills %#v = %#v, %v", ids, refs, err)
		}
	}
	for _, contract := range pack {
		if contract.Resources.EffectCalls != 0 ||
			contract.Operations[0].EffectClass != EffectRead {
			t.Fatalf("%s acquired an external effect", contract.ID)
		}
		if contract.ID == PaymentProposalSkill {
			if strings.Join(contract.Approvals, "|") != "human_payment_approval" {
				t.Fatalf("payment approvals = %#v", contract.Approvals)
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
		"organization:operations", "owner:operations", "Operations Org",
		time.Now().UTC(), "owner-key", privateKey,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected := map[contracts.DepartmentKind][]contracts.SkillID{
		contracts.DepartmentAccounting: AccountingSkillIDs(),
		contracts.DepartmentBackOffice: BackOfficeSkillIDs(),
	}
	for _, mandate := range seed.Mandates {
		want, relevant := expected[mandate.DepartmentKind]
		if relevant && strings.Join(skillStrings(mandate.AllowedSkills), "|") !=
			strings.Join(skillStrings(want), "|") {
			t.Fatalf("%s mandate skills do not match operations pack", mandate.DepartmentKind)
		}
	}
}
