package effect_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/workcompile"
)

// A WorkPacket is transported, so the compiler must treat it as a claim rather
// than as authority. These are the exact widenings a caller holding one valid
// lease would otherwise attempt.
func TestIntegration_CompilerRejectsFabricatedAuthorityAndWidenedOperations(t *testing.T) {
	ctx := context.Background()
	label := "compiler-authority"
	tenant := gatewayTenant(label)
	now := baseTime()
	leaseStore, err := lease.New(integrationPool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := leaseRequest(label, now)
	request, policyFixture := prepareGatewayPolicyAuthority(t, tenant, request, label)
	grant, err := leaseStore.Acquire(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "owner-key:" + label
	skillStore, err := skills.NewStore(
		integrationPool, testVault(t, tenant), tenant, request.OrganizationID,
		keyID, ownerPublic, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	contract := compiledDispatchSkill(t, label)
	published := skills.SignedContract{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: request.OrganizationID, Contract: contract,
		EffectiveAt: now.Add(-time.Minute),
	}
	if err := skills.SignContract(&published, keyID, ownerPrivate); err != nil {
		t.Fatal(err)
	}
	if err := skillStore.Publish(ctx, published); err != nil {
		t.Fatal(err)
	}
	skillRef := contracts.SkillRef{
		ID: contract.ID, Version: contract.Version, Digest: contract.Digest,
	}
	catalog, err := skills.NewCatalog([]skills.Contract{contract})
	if err != nil {
		t.Fatal(err)
	}
	compiler, err := workcompile.New(
		integrationPool, testVault(t, tenant), tenant, skillStore,
		policyFixture.store, leaseStore, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	signed := compiledDispatchPacket(
		t, label, request, grant.Fence, catalog.Digest(), skillRef,
	)
	signed.Seat = policyFixture.seat
	signed.Mandate = policyFixture.mandate
	if err := policy.SignWakeLease(
		&signed.Lease, policyFixture.keyID, policyFixture.private,
	); err != nil {
		t.Fatal(err)
	}
	source := contracts.SourceState{
		RootDigest: contract.Digest, GraphGeneration: 1, LedgerCursor: 1,
	}
	baseProposal := workcompile.Proposal{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             "proposal:" + label,
		OrganizationID: request.OrganizationID,
		WakeID:         request.WakeID,
		IntentID:       contracts.IntentID(request.NodeID),
		SeatID:         request.SeatID,
		Skill:          skillRef, Operation: "write", Provider: "filesystem",
		IdempotencyKey: "effect:" + label,
		ApprovalID:     contracts.ApprovalID("approval:" + label),
		ApprovalCost:   contract.Operations[0].ApprovalCost(),
		Input:          json.RawMessage(`{"value":"compiled"}`),
		Deadline:       now.Add(30 * time.Minute),
	}
	if _, err := compiler.Compile(ctx, signed, baseProposal, source); err == nil {
		t.Fatal("compiler accepted a signed but never-registered WakeLease")
	}
	if err := policyFixture.store.RegisterLease(ctx, signed.Lease); err != nil {
		t.Fatal(err)
	}
	if _, err := compiler.Compile(ctx, signed, baseProposal, source); err != nil {
		t.Fatalf("correctly signed packet failed to compile: %v", err)
	}

	// An unsigned packet is the baseline forgery: placeholder signatures are
	// what a caller can always produce for itself.
	unsigned := compiledDispatchPacket(
		t, label, request, grant.Fence, catalog.Digest(), skillRef,
	)
	unsignedProposal := baseProposal
	unsignedProposal.ID = "proposal:" + label + ":unsigned"
	unsignedProposal.IdempotencyKey = "effect:" + label + ":unsigned"
	if _, err := compiler.Compile(ctx, unsigned, unsignedProposal, source); err == nil {
		t.Fatal("compiler accepted a packet with placeholder authority")
	}

	// A caller-fabricated mandate is the escalation the signature check exists
	// to stop: a real lease with a mandate of the caller's own making.
	_, intruderKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	fabricated := signed
	fabricated.Mandate.AllowedSkills = append(
		append([]contracts.SkillID(nil), fabricated.Mandate.AllowedSkills...),
		"skill:never-granted",
	)
	if err := policy.SignMandate(&fabricated.Mandate, keyID, intruderKey); err != nil {
		t.Fatal(err)
	}
	fabricatedProposal := baseProposal
	fabricatedProposal.ID = "proposal:" + label + ":fabricated"
	fabricatedProposal.IdempotencyKey = "effect:" + label + ":fabricated"
	if _, err := compiler.Compile(ctx, fabricated, fabricatedProposal, source); err == nil {
		t.Fatal("compiler accepted a self-signed mandate")
	}

	for _, attempt := range []struct {
		name   string
		mutate func(*workcompile.Proposal)
	}{
		{"unbound provider", func(value *workcompile.Proposal) {
			value.Provider = "another-credentialed-adapter"
		}},
		{"understated approval cost", func(value *workcompile.Proposal) {
			value.ApprovalCost = 1
		}},
		{"overstated approval cost", func(value *workcompile.Proposal) {
			value.ApprovalCost = contract.Operations[0].ApprovalCost() + 1
		}},
	} {
		proposal := baseProposal
		proposal.ID = "proposal:" + label + ":" + attempt.name
		proposal.IdempotencyKey = "effect:" + label + ":" + attempt.name
		attempt.mutate(&proposal)
		if _, err := compiler.Compile(ctx, signed, proposal, source); err == nil {
			t.Fatalf("compiler accepted %s", attempt.name)
		}
	}

	// A stale fence means the lease is no longer live, however well signed.
	stale := signed
	stale.Lease.Fence++
	if err := policy.SignWakeLease(&stale.Lease, keyID, ownerPrivate); err != nil {
		t.Fatal(err)
	}
	staleProposal := baseProposal
	staleProposal.ID = "proposal:" + label + ":stale"
	staleProposal.IdempotencyKey = "effect:" + label + ":stale"
	if _, err := compiler.Compile(ctx, stale, staleProposal, source); err == nil {
		t.Fatal("compiler accepted a stale runtime fence")
	}
}
