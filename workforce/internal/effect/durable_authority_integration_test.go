package effect_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"centra/workforce/internal/approval"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/skills"
)

// A compiled plan is the only durable dispatch authority, so the projection
// beside it must not be able to authorize anything on its own.
func TestIntegration_EffectGateway_CompiledPlanAuthorityIsAppendOnlyAndSealed(t *testing.T) {
	ctx := context.Background()
	gateway, _, _, proposal := integrationGateway(t, "compiled-authority")
	tenant := gatewayTenant("compiled-authority")

	forged := proposal
	forged.ID = "proposal:compiled-forged"
	forged.IdempotencyKey = "effect:compiled-forged"
	forged.Input = []byte("forged-payload")
	// A database-write compromise can write the hash column, but cannot forge
	// the Vault-sealed plan the compiler wrote.
	hash := strings.Repeat("d", 64)
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO workforce_compiled_plans (
			tenant_id,organization_id,proposal_id,intent_id,skill_id,skill_version,
			skill_digest,operation_digest,verifier_digest,plan_hash,
			effect_proposal_hash,sealed_plan,created_at
		) VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12)
	`, tenant, forged.OrganizationID, forged.ID, forged.IntentID,
		forged.SkillID, forged.SkillDigest.Digest, forged.OperationDigest.Digest,
		hash, hash, effect.ProposalHash(forged),
		[]byte("not-a-sealed-plan"), baseTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Execute(ctx, forged); err == nil {
		t.Fatal("gateway dispatched a proposal with an unopenable compiled plan")
	}

	substituted := proposal
	substituted.ID = "proposal:compiled-substituted"
	substituted.IdempotencyKey = "effect:compiled-substituted"
	// The sealed plan authorizes a different operation than the row claims.
	other := substituted
	other.Input = []byte("a-different-operation")
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO workforce_compiled_plans (
			tenant_id,organization_id,proposal_id,intent_id,skill_id,skill_version,
			skill_digest,operation_digest,verifier_digest,plan_hash,
			effect_proposal_hash,sealed_plan,created_at
		) VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12)
	`, tenant, substituted.OrganizationID, substituted.ID, substituted.IntentID,
		substituted.SkillID, substituted.SkillDigest.Digest,
		substituted.OperationDigest.Digest, hash, hash,
		effect.ProposalHash(substituted),
		sealCompiledPlan(t, tenant, hash, other), baseTime()); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Execute(ctx, substituted); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("substituted sealed plan = %v", err)
	}

	authorized := authorizeProposal(t, tenant, proposal)
	if _, err := integrationPool.Exec(ctx, `
		UPDATE workforce_compiled_plans SET effect_proposal_hash=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND proposal_id=$4
	`, strings.Repeat("e", 64), tenant, authorized.OrganizationID,
		authorized.ID); err == nil {
		t.Fatal("durable compiled dispatch authority was mutable")
	}
	if _, err := integrationPool.Exec(ctx, `
		DELETE FROM workforce_compiled_plans
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, authorized.OrganizationID, authorized.ID); err == nil {
		t.Fatal("durable compiled dispatch authority was deletable")
	}
}

// Spending authority must come from the owner-signed sealed batch, so widening
// the relational projection must neither be possible nor sufficient.
func TestIntegration_EffectGateway_ApprovalAuthorityComesFromSignedSealedBatch(t *testing.T) {
	ctx := context.Background()
	tenant := gatewayTenant("approval-authority")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := approval.New(
		integrationPool, testVault(t, tenant), tenant,
		contracts.OrganizationID("organization:approval-authority"),
		"owner:approval-authority", "owner-key:approval-authority",
		publicKey, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	gateway, _, _, base := integrationGatewayFor(
		t, "approval-authority", approvals.Authority(),
	)
	batch := approval.BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       "approval:authority",
		TenantID:      tenant, OrganizationID: base.OrganizationID,
		IntentIDs:                  []contracts.IntentID{base.IntentID},
		AggregateCeilingMicrounits: 50,
		ExpiresAt:                  baseTime().Add(time.Hour),
		OwnerID:                    "owner:approval-authority",
	}
	if err := approval.SignBatch(
		&batch, "owner-key:approval-authority", privateKey,
	); err != nil {
		t.Fatal(err)
	}
	if err := approvals.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}

	for _, mutation := range []struct {
		name      string
		statement string
		argument  any
	}{
		{
			"ceiling", `UPDATE workforce_approval_batches
				SET aggregate_ceiling_microunits=$1
				WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4`,
			uint64(100_000),
		},
		{
			"expiry", `UPDATE workforce_approval_batches SET expires_at=$1
				WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4`,
			baseTime().Add(9000 * time.Hour),
		},
		{
			"sealed record", `UPDATE workforce_approval_batches SET sealed_batch=$1
				WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4`,
			[]byte("replaced"),
		},
	} {
		if _, err := integrationPool.Exec(ctx, mutation.statement,
			mutation.argument, tenant, base.OrganizationID, batch.BatchID); err == nil {
			t.Fatalf("signed approval %s was mutable", mutation.name)
		}
	}

	approved := base
	approved.ID = "proposal:approval-approved"
	approved.IdempotencyKey = "effect:approval-approved"
	approved.EffectClass = skills.EffectIrreversible
	approved.Irreversible = true
	approved.ApprovalID = batch.BatchID
	approved.ApprovalCost = 25
	approved = authorizeProposal(t, tenant, approved)
	if _, err := gateway.Execute(ctx, approved); err != nil {
		t.Fatalf("owner-approved irreversible dispatch: %v", err)
	}

	// The two columns that legitimately move are the ones a rollback would
	// target, so each is one-way: spend never decreases and a revocation is
	// never cleared or restamped.
	if _, err := integrationPool.Exec(ctx, `
		UPDATE workforce_approval_batches SET consumed_microunits=0
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, tenant, base.OrganizationID, batch.BatchID); err == nil {
		t.Fatal("approval consumption was reversible")
	}
	if _, err := integrationPool.Exec(ctx, `
		UPDATE workforce_approval_batches SET revoked_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4
	`, baseTime(), tenant, base.OrganizationID, batch.BatchID); err != nil {
		t.Fatalf("revoke approval: %v", err)
	}
	for _, reversal := range []any{nil, baseTime().Add(time.Hour)} {
		if _, err := integrationPool.Exec(ctx, `
			UPDATE workforce_approval_batches SET revoked_at=$1
			WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4
		`, reversal, tenant, base.OrganizationID, batch.BatchID); err == nil {
			t.Fatalf("approval revocation was reversible to %v", reversal)
		}
	}
	revoked := base
	revoked.ID = "proposal:approval-revoked"
	revoked.IdempotencyKey = "effect:approval-revoked"
	revoked.EffectClass = skills.EffectIrreversible
	revoked.Irreversible = true
	revoked.ApprovalID = batch.BatchID
	revoked.ApprovalCost = 10
	revoked = authorizeProposal(t, tenant, revoked)
	if _, err := gateway.Execute(ctx, revoked); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("revoked approval dispatch = %v", err)
	}
}

// An effect can sit prepared for a long time — a denied circuit or a crash
// leaves it that way — so the owner's approval must be re-established when the
// dispatch finally happens, not trusted from the earlier transaction.
func TestIntegration_EffectGateway_PreparedIrreversibleEffectRevalidatesApproval(t *testing.T) {
	ctx := context.Background()
	tenant := gatewayTenant("prepared-approval")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := approval.New(
		integrationPool, testVault(t, tenant), tenant,
		contracts.OrganizationID("organization:prepared-approval"),
		"owner:prepared-approval", "owner-key:prepared-approval",
		publicKey, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	gateway, _, _, base := integrationGatewayFor(
		t, "prepared-approval", approvals.Authority(),
	)
	batch := approval.BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       "approval:prepared",
		TenantID:      tenant, OrganizationID: base.OrganizationID,
		IntentIDs:                  []contracts.IntentID{base.IntentID},
		AggregateCeilingMicrounits: 100,
		ExpiresAt:                  baseTime().Add(time.Hour),
		OwnerID:                    "owner:prepared-approval",
	}
	if err := approval.SignBatch(
		&batch, "owner-key:prepared-approval", privateKey,
	); err != nil {
		t.Fatal(err)
	}
	if err := approvals.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}
	proposal := base
	proposal.ID = "proposal:prepared-approval"
	proposal.IdempotencyKey = "effect:prepared-approval"
	proposal.Operation = "not_allowlisted"
	proposal.EffectClass = skills.EffectIrreversible
	proposal.Irreversible = true
	proposal.ApprovalID = batch.BatchID
	proposal.ApprovalCost = 20
	proposal = authorizeProposal(t, tenant, proposal)

	// A dispatch that definitely never started leaves the effect prepared with
	// its approval already consumed.
	if _, err := gateway.Execute(ctx, proposal); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("unstarted dispatch = %v", err)
	}
	if _, err := integrationPool.Exec(ctx, `
		UPDATE workforce_effect_operations SET state='prepared',safe_error_code=NULL
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, proposal.OrganizationID, proposal.ID); err != nil {
		t.Fatal(err)
	}
	revokeTx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = revokeTx.Rollback(context.Background()) }()
	var lockedID contracts.ApprovalID
	if err := revokeTx.QueryRow(ctx, `
		SELECT batch_id FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
		FOR UPDATE
	`, tenant, proposal.OrganizationID, batch.BatchID).Scan(&lockedID); err != nil {
		t.Fatal(err)
	}
	dispatchResult := make(chan error, 1)
	go func() {
		_, dispatchErr := gateway.Execute(ctx, proposal)
		dispatchResult <- dispatchErr
	}()
	lockDeadline := time.Now().Add(2 * time.Second)
	for {
		probeTx, probeErr := integrationPool.Begin(ctx)
		if probeErr != nil {
			t.Fatal(probeErr)
		}
		var probeID string
		probeErr = probeTx.QueryRow(ctx, `
			SELECT proposal_id FROM workforce_effect_operations
			WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
			FOR UPDATE NOWAIT
		`, tenant, proposal.OrganizationID, proposal.ID).Scan(&probeID)
		_ = probeTx.Rollback(ctx)
		if probeErr != nil {
			break
		}
		if time.Now().After(lockDeadline) {
			t.Fatal("dispatch did not reach the locked authorization transition")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if _, err := revokeTx.Exec(ctx, `
		UPDATE workforce_approval_batches SET revoked_at=$1
		WHERE tenant_id=$2 AND organization_id=$3 AND batch_id=$4
	`, baseTime(), tenant, proposal.OrganizationID, batch.BatchID); err != nil {
		t.Fatal(err)
	}
	if err := revokeTx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-dispatchResult; !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("concurrent approval revocation dispatch = %v", err)
	}
	var state effect.State
	if err := integrationPool.QueryRow(ctx, `
		SELECT state FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, proposal.OrganizationID, proposal.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != effect.StatePrepared {
		t.Fatalf("revoked prepared effect advanced to %s", state)
	}
}

// A membership row inserted next to a real batch must not extend the owner's
// approval to an intent they never signed.
func TestIntegration_EffectGateway_InjectedApprovalMembershipCannotWidenAuthority(t *testing.T) {
	ctx := context.Background()
	tenant := gatewayTenant("approval-injection")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approvals, err := approval.New(
		integrationPool, testVault(t, tenant), tenant,
		contracts.OrganizationID("organization:approval-injection"),
		"owner:approval-injection", "owner-key:approval-injection",
		publicKey, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	gateway, _, _, base := integrationGatewayFor(
		t, "approval-injection", approvals.Authority(),
	)
	batch := approval.BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       "approval:injection",
		TenantID:      tenant, OrganizationID: base.OrganizationID,
		IntentIDs:                  []contracts.IntentID{base.IntentID},
		AggregateCeilingMicrounits: 50,
		ExpiresAt:                  baseTime().Add(time.Hour),
		OwnerID:                    "owner:approval-injection",
	}
	if err := approval.SignBatch(
		&batch, "owner-key:approval-injection", privateKey,
	); err != nil {
		t.Fatal(err)
	}
	if err := approvals.PublishBatch(ctx, batch); err != nil {
		t.Fatal(err)
	}

	// Injecting a membership row must not authorize an unapproved intent: the
	// sealed intent set is what the owner actually signed.
	outside := base
	outside.ID = "proposal:approval-outside"
	outside.IdempotencyKey = "effect:approval-outside"
	outside.IntentID = "intent:never-approved"
	outside.EffectClass = skills.EffectIrreversible
	outside.Irreversible = true
	outside.ApprovalID = batch.BatchID
	outside.ApprovalCost = 10
	if _, err := integrationPool.Exec(ctx, `
		INSERT INTO workforce_approval_batch_intents (
			tenant_id,organization_id,batch_id,intent_id
		) VALUES ($1,$2,$3,$4) ON CONFLICT DO NOTHING
	`, tenant, base.OrganizationID, batch.BatchID, outside.IntentID); err != nil {
		t.Fatal(err)
	}
	outside = authorizeProposal(t, tenant, outside)
	if _, err := gateway.Execute(ctx, outside); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("injected approval membership = %v", err)
	}
	var consumed uint64
	if err := integrationPool.QueryRow(ctx, `
		SELECT consumed_microunits FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, tenant, base.OrganizationID, batch.BatchID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != 0 {
		t.Fatalf("unapproved intent consumed %d microunits", consumed)
	}

	approved := base
	approved.ID = "proposal:approval-approved"
	approved.IdempotencyKey = "effect:approval-approved"
	approved.EffectClass = skills.EffectIrreversible
	approved.Irreversible = true
	approved.ApprovalID = batch.BatchID
	approved.ApprovalCost = 25
	approved = authorizeProposal(t, tenant, approved)
	if _, err := gateway.Execute(ctx, approved); err != nil {
		t.Fatalf("owner-approved irreversible dispatch: %v", err)
	}
}

// A sealed batch is bound to its exact tenant, organization, and batch, so a
// record lifted from another identity cannot authorize spending here.
func TestOpenSealedBatchRejectsForeignIdentity(t *testing.T) {
	tenant := gatewayTenant("sealed-identity")
	userVault := testVault(t, tenant)
	organizationID := contracts.OrganizationID("organization:sealed-identity")
	batch := approval.BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       "approval:sealed-identity",
		TenantID:      tenant, OrganizationID: organizationID,
		IntentIDs:                  []contracts.IntentID{"intent:sealed"},
		AggregateCeilingMicrounits: 10,
		ExpiresAt:                  baseTime().Add(time.Hour),
		OwnerID:                    "owner:sealed-identity",
	}
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	authority := approval.Authority{
		OwnerID: batch.OwnerID, KeyID: "owner-key:sealed", PublicKey: ownerPublic,
	}
	if err := approval.SignBatch(&batch, "owner-key:sealed", ownerPrivate); err != nil {
		t.Fatal(err)
	}
	canonical, err := json.Marshal(batch)
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := userVault.SealRecord(
		approval.BatchAD(tenant, organizationID, batch.BatchID), canonical,
	)
	if err != nil {
		t.Fatal(err)
	}
	hash := contentHash(string(canonical)).Digest
	opened, err := approval.OpenSealedBatch(
		userVault, tenant, organizationID, batch.BatchID, sealed, hash, authority,
	)
	if err != nil {
		t.Fatalf("open sealed batch: %v", err)
	}
	if !opened.Authorizes("intent:sealed") || opened.Authorizes("intent:other") {
		t.Fatalf("sealed batch membership = %#v", opened.IntentIDs)
	}
	if _, err := approval.OpenSealedBatch(
		userVault, tenant, "organization:other", batch.BatchID, sealed, hash, authority,
	); err == nil {
		t.Fatal("sealed batch opened under a foreign organization")
	}
	if _, err := approval.OpenSealedBatch(
		userVault, tenant, organizationID, batch.BatchID, sealed,
		strings.Repeat("f", 64), authority,
	); err == nil {
		t.Fatal("sealed batch accepted a mismatched canonical hash")
	}
	if _, err := approval.OpenSealedBatch(
		userVault, tenant, organizationID, batch.BatchID,
		[]byte("not-sealed"), hash, authority,
	); err == nil {
		t.Fatal("sealed batch accepted unsealed bytes")
	}

	// Sealing proves only that the tenant's seal capability produced the bytes.
	// A batch signed by anyone other than the owner must never authorize spend.
	intruderPublic, intruderPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	forged := batch
	if err := approval.SignBatch(&forged, "owner-key:sealed", intruderPrivate); err != nil {
		t.Fatal(err)
	}
	forgedCanonical, err := json.Marshal(forged)
	if err != nil {
		t.Fatal(err)
	}
	forgedSealed, err := userVault.SealRecord(
		approval.BatchAD(tenant, organizationID, forged.BatchID), forgedCanonical,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := approval.OpenSealedBatch(
		userVault, tenant, organizationID, forged.BatchID, forgedSealed,
		contentHash(string(forgedCanonical)).Digest, authority,
	); err == nil {
		t.Fatal("sealed batch accepted a signature that is not the owner's")
	}
	if _, err := approval.OpenSealedBatch(
		userVault, tenant, organizationID, batch.BatchID, sealed, hash,
		approval.Authority{
			OwnerID: batch.OwnerID, KeyID: "owner-key:sealed",
			PublicKey: intruderPublic,
		},
	); err == nil {
		t.Fatal("sealed batch verified against the wrong owner key")
	}
	if _, err := approval.OpenSealedBatch(
		userVault, tenant, organizationID, batch.BatchID, sealed, hash,
		approval.Authority{},
	); err == nil {
		t.Fatal("sealed batch opened without any owner authority")
	}
}
