package effect_test

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/approval"
	"centra/workforce/internal/circuit"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/ledger"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/skills"
	"centra/workforce/internal/testauthority"
)

const postgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "effect integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	integrationPool, err = waitForPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "effect integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, integrationPool, baseTime()); err != nil {
		integrationPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "effect integration migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	integrationPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_EffectGateway_IdempotentSuccessAndConflict(t *testing.T) {
	gateway, leaseStore, grant, proposal := integrationGateway(t, "success")
	result, err := gateway.Execute(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if result.State != effect.StateSucceeded || result.EvidenceHash.Digest == "" {
		t.Fatalf("success result = %+v", result)
	}
	loaded, err := gateway.LoadResult(context.Background(), proposal)
	if err != nil || loaded.State != effect.StateSucceeded ||
		loaded.EvidenceHash != result.EvidenceHash ||
		!loaded.ObservedAt.Equal(result.ObservedAt) {
		t.Fatalf("durable result = %+v, %v; initial=%+v", loaded, err, result)
	}
	duplicate, err := gateway.Execute(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate.Deduplicated || duplicate.EvidenceHash != result.EvidenceHash {
		t.Fatalf("duplicate result = %+v, want %+v", duplicate, result)
	}
	conflict := proposal
	conflict.Input = []byte("different")
	if _, err := gateway.Execute(context.Background(), conflict); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("conflicting replay = %v", err)
	}
	otherIdentity := proposal
	otherIdentity.ID = "proposal:other"
	otherIdentity = authorizeProposal(t, gatewayTenant("success"), otherIdentity)
	if _, err := gateway.Execute(context.Background(), otherIdentity); !errors.Is(err, effect.ErrConflict) {
		t.Fatalf("idempotency alias = %v", err)
	}
	uncompiled := proposal
	uncompiled.ID = "proposal:uncompiled"
	uncompiled.IdempotencyKey = "effect:uncompiled"
	if _, err := gateway.Execute(context.Background(), uncompiled); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("uncompiled proposal = %v", err)
	}
	if err := leaseStore.Cancel(context.Background(), proposal.OrganizationID,
		proposal.LeaseID, grant.Fence, "rotate"); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_EffectGateway_IrreversibleDispatchConsumesExactApprovalAtomically(t *testing.T) {
	tenant := gatewayTenant("irreversible-approval")
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	organizationID := contracts.OrganizationID("organization:irreversible-approval")
	approvals, err := approval.New(
		integrationPool, testVault(t, tenant), tenant, organizationID,
		"owner:irreversible", "owner-key:irreversible", publicKey, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	gateway, _, _, proposal := integrationGatewayFor(
		t, "irreversible-approval", approvals.Authority(),
	)
	batch := approval.BatchApproval{
		SchemaVersion: contracts.SchemaVersionV1,
		BatchID:       "approval:irreversible",
		TenantID:      tenant, OrganizationID: proposal.OrganizationID,
		IntentIDs:                  []contracts.IntentID{proposal.IntentID},
		AggregateCeilingMicrounits: 100,
		ExpiresAt:                  baseTime().Add(time.Hour),
		OwnerID:                    "owner:irreversible",
	}
	if err := approval.SignBatch(&batch, "owner-key:irreversible", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := approvals.PublishBatch(context.Background(), batch); err != nil {
		t.Fatal(err)
	}
	// A compiled plan is immutable, so the irreversible dispatch is its own
	// compiled identity rather than a mutation of the reversible one.
	proposal.ID = "proposal:irreversible"
	proposal.IdempotencyKey = "effect:irreversible"
	proposal.EffectClass = skills.EffectIrreversible
	proposal.Irreversible = true
	proposal.ApprovalID = batch.BatchID
	proposal.ApprovalCost = 75
	proposal = authorizeProposal(t, tenant, proposal)
	if _, err := gateway.Execute(context.Background(), proposal); err != nil {
		t.Fatal(err)
	}
	var consumed uint64
	if err := integrationPool.QueryRow(context.Background(), `
		SELECT consumed_microunits FROM workforce_approval_batches
		WHERE tenant_id=$1 AND organization_id=$2 AND batch_id=$3
	`, tenant, proposal.OrganizationID, batch.BatchID).Scan(&consumed); err != nil {
		t.Fatal(err)
	}
	if consumed != proposal.ApprovalCost {
		t.Fatalf("consumed approval = %d, want %d", consumed, proposal.ApprovalCost)
	}

	overCeiling := proposal
	overCeiling.ID = "proposal:irreversible-over-ceiling"
	overCeiling.IdempotencyKey = "effect:irreversible-over-ceiling"
	overCeiling.ApprovalCost = 50
	overCeiling = authorizeProposal(t, tenant, overCeiling)
	if _, err := gateway.Execute(context.Background(), overCeiling); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("over-ceiling irreversible dispatch = %v", err)
	}
	var operations int
	if err := integrationPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, overCeiling.OrganizationID, overCeiling.ID).Scan(&operations); err != nil {
		t.Fatal(err)
	}
	if operations != 0 {
		t.Fatal("rejected approval left a prepared effect operation")
	}
}

func TestIntegration_EffectGateway_OpenCircuitCannotBeBypassed(t *testing.T) {
	gateway, _, _, proposal := integrationGateway(t, "circuit-denial")
	tenant := gatewayTenant("circuit-denial")
	breakers := testCircuit(t, tenant, baseTime)
	keys, err := circuit.Keys(proposal.OrganizationID, proposal.Provider,
		string(proposal.SkillID), string(proposal.EffectClass))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 3; attempt++ {
		permit, err := breakers.Admit(context.Background(), keys, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := breakers.Fail(context.Background(), permit, "provider_storm"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, circuit.ErrOpen) {
		t.Fatalf("gateway bypassed open circuit: %v", err)
	}
	var state effect.State
	if err := integrationPool.QueryRow(context.Background(), `
		SELECT state FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, proposal.OrganizationID, proposal.ID).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if state != effect.StatePrepared {
		t.Fatalf("denied effect state = %s", state)
	}
	var evidence int
	if err := integrationPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_effect_evidence
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, proposal.OrganizationID, proposal.ID).Scan(&evidence); err != nil {
		t.Fatal(err)
	}
	if evidence != 0 {
		t.Fatalf("open-circuit effect wrote %d evidence rows", evidence)
	}
	reconcileGateway, _, _, reconcileProposal := integrationGateway(t, "circuit-reconcile-denial")
	reconcileProposal.Operation = "write_then_fail"
	reconcileProposal = authorizeProposal(t, gatewayTenant("circuit-reconcile-denial"), reconcileProposal)
	if _, err := reconcileGateway.Execute(context.Background(), reconcileProposal); !errors.Is(err, effect.ErrAmbiguous) {
		t.Fatal(err)
	}
	reconcileTenant := gatewayTenant("circuit-reconcile-denial")
	reconcileBreakers := testCircuit(t, reconcileTenant, baseTime)
	reconcileKeys, err := circuit.Keys(reconcileProposal.OrganizationID,
		reconcileProposal.Provider, string(reconcileProposal.SkillID),
		string(reconcileProposal.EffectClass))
	if err != nil {
		t.Fatal(err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		permit, err := reconcileBreakers.Admit(context.Background(), reconcileKeys, false)
		if err != nil {
			t.Fatal(err)
		}
		if err := reconcileBreakers.Fail(context.Background(), permit, "provider_storm"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reconcileGateway.Reconcile(context.Background(), reconcileProposal); !errors.Is(err, circuit.ErrOpen) {
		t.Fatalf("reconciliation bypassed open circuit: %v", err)
	}
}

func TestIntegration_EffectGateway_AmbiguousEffectReconcilesWithoutReplay(t *testing.T) {
	gateway, _, _, proposal := integrationGateway(t, "ambiguous")
	proposal.Operation = "write_then_fail"
	proposal.IdempotencyKey = "effect:ambiguous"
	proposal = authorizeProposal(t, gatewayTenant("ambiguous"), proposal)
	result, err := gateway.Execute(context.Background(), proposal)
	if !errors.Is(err, effect.ErrAmbiguous) ||
		result.State != effect.StateExternallyAmbiguous {
		t.Fatalf("ambiguous dispatch = %+v, %v", result, err)
	}
	if _, err := integrationPool.Exec(context.Background(), `
		UPDATE workforce_effect_operations SET state='dispatching'
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, gatewayTenant("ambiguous"), proposal.OrganizationID, proposal.ID); err != nil {
		t.Fatal(err)
	}
	retry, err := gateway.Execute(context.Background(), proposal)
	if !errors.Is(err, effect.ErrAmbiguous) || !retry.Deduplicated {
		t.Fatalf("blind retry was not blocked: %+v, %v", retry, err)
	}
	alternate, err := effect.NewCommandAdapter(
		"filesystem", "/bin/sh",
		map[string][]string{"other": {"-c", "true"}},
		map[string][]string{"other": {"-c", "true"}},
		[]string{"PATH=/usr/bin:/bin"}, t.TempDir(), baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	tenant := gatewayTenant("ambiguous")
	leaseStore, err := lease.New(integrationPool, tenant, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	inconclusiveGateway, err := effect.New(
		integrationPool, testVault(t, tenant), leaseStore, gatewayPolicyStore(t, tenant),
		testCircuit(t, tenant, baseTime), tenant, approval.Authority{}, baseTime, alternate,
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := inconclusiveGateway.Reconcile(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
		t.Fatalf("not-started probe = %v", err)
	}
	reconciled, err := gateway.Reconcile(context.Background(), proposal)
	if err != nil {
		t.Fatal(err)
	}
	if reconciled.State != effect.StateSucceeded ||
		reconciled.ExternalID != result.ExternalID {
		t.Fatalf("reconciled result = %+v, initial=%+v", reconciled, result)
	}
	terminal, err := gateway.Reconcile(context.Background(), proposal)
	if err != nil || !terminal.Deduplicated {
		t.Fatalf("terminal reconciliation = %+v, %v", terminal, err)
	}
	changed := proposal
	changed.Input = []byte("changed")
	if _, err := gateway.Reconcile(context.Background(), changed); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("changed reconciliation = %v", err)
	}
	missing := proposal
	missing.ID = "proposal:missing"
	if _, err := gateway.Reconcile(context.Background(), missing); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("missing reconciliation = %v", err)
	}
}

func TestIntegration_EffectGateway_PendingSweepPreservesAllProbeOutcomes(t *testing.T) {
	for _, outcome := range []skills.ProbeOutcome{
		skills.ProbeUnchanged, skills.ProbeReversed, skills.ProbeDrifted,
		skills.ProbeConflicted, skills.ProbeUnknown,
	} {
		t.Run(string(outcome), func(t *testing.T) {
			gateway, _, _, proposal := integrationGateway(t, "probe-"+string(outcome))
			proposal.Operation = "probe_" + string(outcome)
			proposal.IdempotencyKey = "effect:probe:" + string(outcome)
			proposal = authorizeProposal(t, gatewayTenant("probe-"+string(outcome)), proposal)
			initial, err := gateway.Execute(context.Background(), proposal)
			if !errors.Is(err, effect.ErrAmbiguous) ||
				initial.State != effect.StateExternallyAmbiguous {
				t.Fatalf("ambiguous setup = %+v, %v", initial, err)
			}
			pending, err := gateway.Pending(context.Background(), proposal.OrganizationID)
			if err != nil || len(pending) != 1 || pending[0].ID != proposal.ID ||
				string(pending[0].Input) != string(proposal.Input) ||
				pending[0].NodeID != proposal.NodeID {
				t.Fatalf("pending proposal = %+v, %v", pending, err)
			}
			result, err := gateway.Reconcile(context.Background(), pending[0])
			if !errors.Is(err, effect.ErrAmbiguous) ||
				result.ProbeOutcome != outcome ||
				result.SafeErrorCode != "probe_"+string(outcome) {
				t.Fatalf("probe result = %+v, %v", result, err)
			}
		})
	}
}

func TestIntegration_EffectGateway_PendingRejectsTamperAndStorageFailure(t *testing.T) {
	gateway, _, _, proposal := integrationGateway(t, "pending-tamper")
	proposal.Operation = "write_then_fail"
	proposal = authorizeProposal(t, gatewayTenant("pending-tamper"), proposal)
	if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
		t.Fatal(err)
	}
	if _, err := gateway.Pending(context.Background(), ""); err == nil {
		t.Fatal("empty pending organization accepted")
	}
	tenant := gatewayTenant("pending-tamper")
	var sealed []byte
	if err := integrationPool.QueryRow(context.Background(), `
		SELECT proposal_sealed FROM workforce_effect_operations
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, proposal.OrganizationID, proposal.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if _, err := integrationPool.Exec(context.Background(), `
		UPDATE workforce_effect_operations SET proposal_sealed=NULL
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, proposal.OrganizationID, proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Pending(context.Background(), proposal.OrganizationID); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("missing sealed proposal error = %v", err)
	}
	if _, err := integrationPool.Exec(context.Background(), `
		UPDATE workforce_effect_operations SET proposal_sealed=$1,graph_node_id=NULL
		WHERE tenant_id=$2 AND organization_id=$3 AND proposal_id=$4
	`, sealed, tenant, proposal.OrganizationID, proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Pending(context.Background(), proposal.OrganizationID); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("invalid pending row error = %v", err)
	}
	if _, err := integrationPool.Exec(context.Background(), `
		UPDATE workforce_effect_operations SET graph_node_id=$1,effect_class='invalid'
		WHERE tenant_id=$2 AND organization_id=$3 AND proposal_id=$4
	`, proposal.NodeID, tenant, proposal.OrganizationID, proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Pending(context.Background(), proposal.OrganizationID); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("invalid pending proposal error = %v", err)
	}
	if _, err := integrationPool.Exec(context.Background(), `
		UPDATE workforce_effect_operations SET proposal_sealed=$1,effect_class=$2
		WHERE tenant_id=$3 AND organization_id=$4 AND proposal_id=$5
	`, []byte("tampered"), proposal.EffectClass, tenant, proposal.OrganizationID, proposal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Pending(context.Background(), proposal.OrganizationID); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("tampered pending proposal error = %v", err)
	}
	closed, err := pgxpool.New(context.Background(),
		"postgres://postgres:password@127.0.0.1:1/database?sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}
	closed.Close()
	leaseStore, err := lease.New(integrationPool, tenant, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	closedGateway, err := effect.New(
		closed, testVault(t, tenant), leaseStore, gatewayPolicyStore(t, tenant),
		testCircuit(t, tenant, baseTime), tenant, approval.Authority{}, baseTime,
		testAdapter(t, "pending-closed"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := closedGateway.Pending(context.Background(), proposal.OrganizationID); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("closed pending query error = %v", err)
	}
}

func TestIntegration_EffectGateway_ProbePreflightRejectsInvalidClockAndProvider(t *testing.T) {
	gateway, leaseStore, _, proposal := integrationGateway(t, "probe-preflight")
	if _, err := gateway.Reconcile(context.Background(), effect.Proposal{}); err == nil {
		t.Fatal("invalid reconciliation proposal accepted")
	}
	unknown := proposal
	unknown.Provider = "missing"
	if _, err := gateway.Reconcile(context.Background(), unknown); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("unknown reconciliation provider error = %v", err)
	}
	tenant := gatewayTenant("probe-preflight")
	badClock, err := effect.New(
		integrationPool, testVault(t, tenant), leaseStore, gatewayPolicyStore(t, tenant),
		testCircuit(t, tenant, baseTime), tenant, approval.Authority{}, time.Now,
		testAdapter(t, "probe-preflight-clock"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badClock.Reconcile(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("non-UTC probe clock error = %v", err)
	}
}

func TestIntegration_EffectGateway_CircuitPersistenceFailuresFailClosed(t *testing.T) {
	for _, test := range []struct {
		label     string
		operation string
		want      error
	}{
		{"circuit-started-failure", "write_then_fail", circuit.ErrUncertain},
		{"circuit-before-failure", "not_allowlisted", circuit.ErrUncertain},
		{"circuit-success-failure", "write", circuit.ErrUncertain},
	} {
		t.Run(test.label, func(t *testing.T) {
			gateway, _, _, proposal := integrationGateway(t, test.label)
			proposal.Operation = test.operation
			proposal = authorizeProposal(t, gatewayTenant(test.label), proposal)
			installCircuitUpdateFailure(t, gatewayTenant(test.label), test.label)
			if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, test.want) {
				t.Fatalf("circuit persistence error = %v", err)
			}
		})
	}
	t.Run("probe failure record", func(t *testing.T) {
		gateway, _, _, proposal := integrationGateway(t, "circuit-probe-failure")
		proposal.Operation = "write_probe_fail"
		proposal = authorizeProposal(t, gatewayTenant("circuit-probe-failure"), proposal)
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
			t.Fatal(err)
		}
		installCircuitUpdateFailure(t, gatewayTenant("circuit-probe-failure"), "circuit-probe-failure")
		if _, err := gateway.Reconcile(context.Background(), proposal); !errors.Is(err, circuit.ErrUncertain) {
			t.Fatalf("probe failure breaker error = %v", err)
		}
	})
	t.Run("probe success record", func(t *testing.T) {
		gateway, _, _, proposal := integrationGateway(t, "circuit-probe-success")
		proposal.Operation = "probe_unchanged"
		proposal = authorizeProposal(t, gatewayTenant("circuit-probe-success"), proposal)
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
			t.Fatal(err)
		}
		installCircuitUpdateFailure(t, gatewayTenant("circuit-probe-success"), "circuit-probe-success")
		if _, err := gateway.Reconcile(context.Background(), proposal); !errors.Is(err, circuit.ErrUncertain) {
			t.Fatalf("probe success breaker error = %v", err)
		}
	})
}

func TestIntegration_EffectGateway_ReconcilePersistenceFailuresFailClosed(t *testing.T) {
	t.Run("probe update", func(t *testing.T) {
		gateway, _, _, proposal := integrationGateway(t, "probe-update-failure")
		proposal.Operation = "probe_unchanged"
		proposal = authorizeProposal(t, gatewayTenant("probe-update-failure"), proposal)
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
			t.Fatal(err)
		}
		installEffectUpdateFailure(t, gatewayTenant("probe-update-failure"),
			"probe-update-failure", "NEW.last_probe_outcome IS NOT NULL")
		if _, err := gateway.Reconcile(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
			t.Fatalf("probe update error = %v", err)
		}
	})
	t.Run("terminal probe outcome", func(t *testing.T) {
		gateway, _, _, proposal := integrationGateway(t, "probe-outcome-failure")
		proposal.Operation = "write_then_fail"
		proposal = authorizeProposal(t, gatewayTenant("probe-outcome-failure"), proposal)
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
			t.Fatal(err)
		}
		installEffectUpdateFailure(t, gatewayTenant("probe-outcome-failure"),
			"probe-outcome-failure", "OLD.state='succeeded' AND NEW.last_probe_outcome IS NOT NULL")
		if _, err := gateway.Reconcile(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
			t.Fatalf("terminal probe outcome error = %v", err)
		}
	})
	t.Run("probe evidence", func(t *testing.T) {
		gateway, _, _, proposal := integrationGateway(t, "probe-evidence-failure")
		proposal.Operation = "write_then_fail"
		proposal = authorizeProposal(t, gatewayTenant("probe-evidence-failure"), proposal)
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
			t.Fatal(err)
		}
		installEffectEvidenceFailure(t, gatewayTenant("probe-evidence-failure"), "probe-evidence-failure")
		if _, err := gateway.Reconcile(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
			t.Fatalf("probe evidence error = %v", err)
		}
	})
}

func TestIntegration_EffectGateway_BeforeSendStaleFenceAndPolicyDriftFailClosed(t *testing.T) {
	gateway, leaseStore, grant, proposal := integrationGateway(t, "failures")
	proposal.Operation = "not_allowlisted"
	proposal.IdempotencyKey = "effect:not-started"
	proposal = authorizeProposal(t, gatewayTenant("failures"), proposal)
	failed, err := gateway.Execute(context.Background(), proposal)
	if !errors.Is(err, effect.ErrRejected) || failed.State != effect.StateFailed {
		t.Fatalf("before-send failure = %+v, %v", failed, err)
	}
	terminal, err := gateway.Reconcile(context.Background(), proposal)
	if err != nil || !terminal.Deduplicated || terminal.State != effect.StateFailed {
		t.Fatalf("failed terminal reconciliation = %+v, %v", terminal, err)
	}
	stale := proposal
	stale.ID = "proposal:stale"
	stale.IdempotencyKey = "effect:stale"
	stale.Fence = grant.Fence + 1
	if _, err := gateway.Execute(context.Background(), stale); !errors.Is(err, lease.ErrStaleFence) {
		t.Fatalf("stale fence dispatch = %v", err)
	}
	if err := leaseStore.Cancel(context.Background(), proposal.OrganizationID,
		proposal.LeaseID, grant.Fence, "cancel"); err != nil {
		t.Fatal(err)
	}
	driftGateway, _, driftGrant, driftProposal := integrationGateway(t, "policy-drift")
	requestDrift := leaseRequest("policy-drift", baseTime())
	insertAuthority(t, gatewayTenant("policy-drift"), requestDrift, 2)
	driftProposal.Fence = driftGrant.Fence
	if _, err := driftGateway.Execute(context.Background(), driftProposal); !errors.Is(err, lease.ErrPolicyMismatch) {
		t.Fatalf("policy drift dispatch = %v", err)
	}
}

func TestIntegration_EffectGateway_PreflightAndProbeFailuresFailClosed(t *testing.T) {
	gateway, leaseStore, _, proposal := integrationGateway(t, "preflight")
	if _, err := gateway.Execute(context.Background(), effect.Proposal{}); err == nil {
		t.Fatal("invalid proposal accepted")
	}
	expired := proposal
	expired.ID = "proposal:expired"
	expired.IdempotencyKey = "effect:expired"
	expired.Deadline = baseTime()
	if _, err := gateway.Execute(context.Background(), expired); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("expired proposal = %v", err)
	}
	unknownProvider := proposal
	unknownProvider.ID = "proposal:provider"
	unknownProvider.IdempotencyKey = "effect:provider"
	unknownProvider.Provider = "missing"
	if _, err := gateway.Execute(context.Background(), unknownProvider); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("unknown provider = %v", err)
	}
	if _, err := gateway.Reconcile(context.Background(), expired); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("unpersisted expired reconciliation = %v", err)
	}
	wrongSeat := proposal
	wrongSeat.ID = "proposal:seat"
	wrongSeat.IdempotencyKey = "effect:seat"
	wrongSeat.SeatID = "seat:other"
	if _, err := gateway.Execute(context.Background(), wrongSeat); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("wrong seat = %v", err)
	}
	tenant := gatewayTenant("preflight")
	badClock, err := effect.New(
		integrationPool, testVault(t, tenant), leaseStore, gatewayPolicyStore(t, tenant),
		testCircuit(t, tenant, baseTime), tenant, approval.Authority{},
		time.Now, testAdapter(t, "preflight-clock"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := badClock.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
		t.Fatalf("non-UTC gateway clock = %v", err)
	}

	probeFailure := proposal
	probeFailure.ID = "proposal:probe-failure"
	probeFailure.Operation = "write_probe_fail"
	probeFailure.IdempotencyKey = "effect:probe-failure"
	probeFailure = authorizeProposal(t, tenant, probeFailure)
	first, err := gateway.Execute(context.Background(), probeFailure)
	if !errors.Is(err, effect.ErrAmbiguous) || first.State != effect.StateExternallyAmbiguous {
		t.Fatalf("probe setup = %+v, %v", first, err)
	}
	if duplicate, err := gateway.Execute(context.Background(), probeFailure); !errors.Is(err, effect.ErrAmbiguous) ||
		!duplicate.Deduplicated {
		t.Fatalf("direct ambiguous replay = %+v, %v", duplicate, err)
	}
	inconclusive, err := gateway.Reconcile(context.Background(), probeFailure)
	if !errors.Is(err, effect.ErrAmbiguous) ||
		inconclusive.SafeErrorCode != "probe_inconclusive" {
		t.Fatalf("inconclusive probe = %+v, %v", inconclusive, err)
	}
	if _, err := integrationPool.Exec(context.Background(), `
		UPDATE workforce_effect_operations SET state='prepared'
		WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
	`, tenant, probeFailure.OrganizationID, probeFailure.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := gateway.Reconcile(context.Background(), probeFailure); !errors.Is(err, effect.ErrRejected) {
		t.Fatalf("prepared reconciliation = %v", err)
	}
}

func TestIntegration_EffectGateway_RealPersistenceFailuresFailClosed(t *testing.T) {
	t.Run("closed prepare", func(t *testing.T) {
		_, leaseStore, _, proposal := integrationGateway(t, "db-closed")
		closed, err := pgxpool.New(context.Background(),
			"postgres://postgres:password@127.0.0.1:1/database?sslmode=disable")
		if err != nil {
			t.Fatal(err)
		}
		closed.Close()
		tenant := gatewayTenant("db-closed")
		gateway, err := effect.New(
			closed, testVault(t, tenant), leaseStore, gatewayPolicyStore(t, tenant),
			testCircuit(t, tenant, baseTime), tenant, approval.Authority{}, baseTime,
			testAdapter(t, "db-closed-adapter"),
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
			t.Fatalf("closed prepare = %v", err)
		}
		if _, err := gateway.Reconcile(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
			t.Fatalf("closed lookup = %v", err)
		}
	})
	for _, failure := range []struct {
		label, table, timing, condition string
	}{
		{"db-prepare", "workforce_effect_operations", "INSERT", ""},
		{"db-state", "workforce_effect_operations", "UPDATE", ""},
		{"db-evidence", "workforce_effect_evidence", "INSERT", ""},
		{"db-commit", "workforce_effect_operations", "UPDATE", " AND NEW.state='succeeded'"},
	} {
		t.Run(failure.label, func(t *testing.T) {
			gateway, _, _, proposal := integrationGateway(t, failure.label)
			functionName := strings.ReplaceAll(failure.label, "-", "_")
			statement := fmt.Sprintf(`
				CREATE FUNCTION reject_%s() RETURNS trigger AS $$
				BEGIN RAISE EXCEPTION 'rejected'; END;
				$$ LANGUAGE plpgsql;
				CREATE TRIGGER reject_%s
				BEFORE %s ON %s
				FOR EACH ROW WHEN (NEW.tenant_id='%s'%s)
				EXECUTE FUNCTION reject_%s()
			`, functionName, functionName, failure.timing, failure.table,
				gatewayTenant(failure.label), failure.condition, functionName)
			if _, err := integrationPool.Exec(context.Background(), statement); err != nil {
				t.Fatal(err)
			}
			if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
				t.Fatalf("%s failure = %v", failure.label, err)
			}
		})
	}
	for _, failure := range []struct {
		label, operation, state string
	}{
		{"db-ambiguous-state", "write_then_fail", "externally_ambiguous"},
		{"db-failed-state", "not_allowlisted", "failed"},
	} {
		t.Run(failure.label, func(t *testing.T) {
			gateway, _, _, proposal := integrationGateway(t, failure.label)
			proposal.Operation = failure.operation
			proposal = authorizeProposal(t, gatewayTenant(failure.label), proposal)
			functionName := strings.ReplaceAll(failure.label, "-", "_")
			statement := fmt.Sprintf(`
				CREATE FUNCTION reject_%s() RETURNS trigger AS $$
				BEGIN RAISE EXCEPTION 'rejected'; END;
				$$ LANGUAGE plpgsql;
				CREATE TRIGGER reject_%s
				BEFORE UPDATE ON workforce_effect_operations
				FOR EACH ROW WHEN (NEW.tenant_id='%s' AND NEW.state='%s')
				EXECUTE FUNCTION reject_%s()
			`, functionName, functionName, gatewayTenant(failure.label),
				failure.state, functionName)
			if _, err := integrationPool.Exec(context.Background(), statement); err != nil {
				t.Fatal(err)
			}
			if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrUncertain) {
				t.Fatalf("%s failure = %v", failure.label, err)
			}
		})
	}
	t.Run("observation conflict", func(t *testing.T) {
		gateway, _, _, proposal := integrationGateway(t, "db-observation-conflict")
		tenant := gatewayTenant("db-observation-conflict")
		if _, err := integrationPool.Exec(context.Background(), fmt.Sprintf(`
			CREATE FUNCTION force_effect_failed() RETURNS trigger AS $$
			BEGIN
				UPDATE workforce_effect_operations SET state='failed'
				WHERE tenant_id=NEW.tenant_id AND organization_id=NEW.organization_id
				  AND proposal_id=NEW.proposal_id;
				RETURN NEW;
			END;
			$$ LANGUAGE plpgsql;
			CREATE TRIGGER force_effect_failed
			BEFORE INSERT ON workforce_effect_evidence
			FOR EACH ROW WHEN (NEW.tenant_id='%s')
			EXECUTE FUNCTION force_effect_failed()
		`, tenant)); err != nil {
			t.Fatal(err)
		}
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrConflict) {
			t.Fatalf("observation state conflict = %v", err)
		}
	})
	t.Run("invalid observation", func(t *testing.T) {
		_, leaseStore, _, proposal := integrationGateway(t, "invalid-observation")
		tenant := gatewayTenant("invalid-observation")
		adapter, err := effect.NewCommandAdapter(
			"filesystem", "/bin/sh",
			map[string][]string{"write": {"-c", "printf observed"}},
			map[string][]string{"write": {"-c", "printf observed"}},
			[]string{"PATH=/usr/bin:/bin"}, t.TempDir(), time.Now,
		)
		if err != nil {
			t.Fatal(err)
		}
		gateway, err := effect.New(
			integrationPool, testVault(t, tenant), leaseStore, gatewayPolicyStore(t, tenant),
			testCircuit(t, tenant, baseTime), tenant, approval.Authority{},
			baseTime, adapter,
		)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := gateway.Execute(context.Background(), proposal); !errors.Is(err, effect.ErrAmbiguous) {
			t.Fatalf("invalid observation = %v", err)
		}
	})
}

func TestEffectGateway_RejectsInvalidConstructionAndPreflight(t *testing.T) {
	tenant := "tenant:constructor"
	userVault := testVault(t, tenant)
	leaseStore, err := lease.New(integrationPool, tenant, baseTime)
	if err != nil {
		t.Fatal(err)
	}
	adapter := testAdapter(t, "constructor")
	breakers := testCircuit(t, tenant, baseTime)
	_, authority := prepareGatewayPolicyAuthority(
		t, tenant, leaseRequest("constructor", baseTime()), "constructor",
	)
	cases := []struct {
		pool      *pgxpool.Pool
		userVault *vault.UserVault
		leases    *lease.Store
		policy    *policy.Store
		breakers  *circuit.Store
		tenant    string
		now       func() time.Time
		adapters  []effect.Adapter
	}{
		{nil, userVault, leaseStore, authority.store, breakers, tenant, baseTime, []effect.Adapter{adapter}},
		{integrationPool, nil, leaseStore, authority.store, breakers, tenant, baseTime, []effect.Adapter{adapter}},
		{integrationPool, userVault, nil, authority.store, breakers, tenant, baseTime, []effect.Adapter{adapter}},
		{integrationPool, userVault, leaseStore, nil, breakers, tenant, baseTime, []effect.Adapter{adapter}},
		{integrationPool, userVault, leaseStore, authority.store, nil, tenant, baseTime, []effect.Adapter{adapter}},
		{integrationPool, userVault, leaseStore, authority.store, breakers, "", baseTime, []effect.Adapter{adapter}},
		{integrationPool, userVault, leaseStore, authority.store, breakers, tenant, nil, []effect.Adapter{adapter}},
		{integrationPool, userVault, leaseStore, authority.store, breakers, tenant, baseTime, nil},
		{integrationPool, userVault, leaseStore, authority.store, breakers, tenant, baseTime, []effect.Adapter{nil}},
		{integrationPool, userVault, leaseStore, authority.store, breakers, tenant, baseTime, []effect.Adapter{adapter, adapter}},
	}
	for index, candidate := range cases {
		if _, err := effect.New(candidate.pool, candidate.userVault, candidate.leases,
			candidate.policy, candidate.breakers, candidate.tenant, approval.Authority{},
			candidate.now, candidate.adapters...); err == nil {
			t.Fatalf("constructor case %d accepted", index)
		}
	}
	otherVault := testVault(t, "tenant:other")
	if _, err := effect.New(integrationPool, otherVault, leaseStore,
		authority.store, breakers, tenant, approval.Authority{}, baseTime, adapter); err == nil {
		t.Fatal("cross-tenant Vault accepted")
	}
}

func integrationGateway(
	t *testing.T,
	label string,
) (*effect.Gateway, *lease.Store, lease.Grant, effect.Proposal) {
	t.Helper()
	// Most gateways under test dispatch only reversible effects, so they are
	// built without owner approval authority and refuse irreversible work.
	return integrationGatewayFor(t, label, approval.Authority{})
}

func integrationGatewayFor(
	t *testing.T,
	label string,
	ownerAuthority approval.Authority,
) (*effect.Gateway, *lease.Store, lease.Grant, effect.Proposal) {
	t.Helper()
	tenant := gatewayTenant(label)
	now := baseTime()
	leaseStore, err := lease.New(integrationPool, tenant, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	request := leaseRequest(label, now)
	request, policyFixture := prepareGatewayPolicyAuthority(t, tenant, request, label)
	grant, err := leaseStore.Acquire(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	registerGatewayWakeLease(t, policyFixture, request, grant.Fence, label)
	adapter := testAdapter(t, label)
	gateway, err := effect.New(
		integrationPool, testVault(t, tenant), leaseStore, policyFixture.store,
		testCircuit(t, tenant, func() time.Time { return now }), tenant, ownerAuthority,
		func() time.Time { return now }, adapter,
	)
	if err != nil {
		t.Fatal(err)
	}
	hash := contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("a", 64)}
	proposal := effect.Proposal{
		ID: "proposal:" + label, OrganizationID: request.OrganizationID,
		IntentID: contracts.IntentID("intent:" + label), NodeID: request.NodeID,
		SeatID:  request.SeatID,
		LeaseID: request.ID, Fence: grant.Fence, Provider: adapter.Name(),
		SkillID:     "skill:" + contracts.SkillID(label),
		EffectClass: skills.EffectReversible,
		Operation:   "write", IdempotencyKey: "effect:" + label,
		SkillDigest: hash, OperationDigest: hash, Input: []byte("payload-" + label),
		Deadline: now.Add(30 * time.Minute),
	}
	proposal = authorizeProposal(t, tenant, proposal)
	return gateway, leaseStore, grant, proposal
}

// authorizeProposal records the durable compiled plan that a prior real
// compilation would have written for this exact proposal. The plan is sealed
// with the same tenant Vault the gateway opens, so dispatch authority is
// established the same way it is in production.
func authorizeProposal(
	t *testing.T,
	tenant string,
	proposal effect.Proposal,
) effect.Proposal {
	t.Helper()
	// A compiled plan is immutable, so every distinct operation shape compiles
	// to its own proposal identity rather than rewriting an existing plan.
	sum := sha256.Sum256([]byte(
		proposal.Operation + "\x00" + proposal.IdempotencyKey + "\x00" +
			string(proposal.Input) + "\x00" + string(proposal.ApprovalID) + "\x00" +
			strconv.FormatUint(proposal.ApprovalCost, 10),
	))
	proposal.ID += ":" + hex.EncodeToString(sum[:4])
	hash := strings.Repeat("d", 64)
	sealed := sealCompiledPlan(t, tenant, hash, proposal)
	if _, err := integrationPool.Exec(context.Background(), `
		INSERT INTO workforce_compiled_plans (
			tenant_id,organization_id,proposal_id,intent_id,skill_id,skill_version,
			skill_digest,operation_digest,verifier_digest,plan_hash,
			effect_proposal_hash,sealed_plan,created_at
		) VALUES ($1,$2,$3,$4,$5,1,$6,$7,$8,$9,$10,$11,$12)
		ON CONFLICT DO NOTHING
	`, tenant, proposal.OrganizationID, proposal.ID, proposal.IntentID,
		proposal.SkillID, proposal.SkillDigest.Digest, proposal.OperationDigest.Digest,
		hash, hash, effect.ProposalHash(proposal), sealed, baseTime()); err != nil {
		t.Fatalf("authorize compiled effect proposal: %v", err)
	}
	return proposal
}

func sealCompiledPlan(
	t *testing.T,
	tenant string,
	planHash string,
	proposal effect.Proposal,
) []byte {
	t.Helper()
	encoded, err := json.Marshal(struct {
		PlanHash  contracts.ContentHash `json:"plan_hash"`
		Operation effect.Proposal       `json:"operation"`
	}{
		PlanHash:  contracts.ContentHash{Algorithm: "sha256", Digest: planHash},
		Operation: proposal,
	})
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := testVault(t, tenant).SealRecord(vault.AD{
		User: tenant, Store: "workforce.compiled.plan",
		Stream: string(proposal.OrganizationID) + "/" + proposal.ID,
		Schema: contracts.SchemaVersionV1,
	}, encoded)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}

func testAdapter(t *testing.T, label string) *effect.CommandAdapter {
	t.Helper()
	directory := t.TempDir()
	writeScript := `file="$1/$WORKFORCE_IDEMPOTENCY_KEY"; if [ ! -e "$file" ]; then cat >"$file"; fi; cat "$file"`
	failScript := `file="$1/$WORKFORCE_IDEMPOTENCY_KEY"; if [ ! -e "$file" ]; then cat >"$file"; fi; cat "$file"; exit 7`
	probeScript := `file="$1/$WORKFORCE_IDEMPOTENCY_KEY"; test -e "$file" || exit 8; printf '{"outcome":"completed_out_of_band","observation":"'; cat "$file"; printf '"}'`
	outcomeScript := func(outcome skills.ProbeOutcome) string {
		return `printf '{"outcome":"` + string(outcome) + `","reason":"observed"}'`
	}
	adapter, err := effect.NewCommandAdapter(
		"filesystem", "/bin/sh",
		map[string][]string{
			"write":            {"-c", writeScript, "sh", directory},
			"write_then_fail":  {"-c", failScript, "sh", directory},
			"write_probe_fail": {"-c", failScript, "sh", directory},
			"probe_unchanged":  {"-c", failScript, "sh", directory},
			"probe_reversed":   {"-c", failScript, "sh", directory},
			"probe_drifted":    {"-c", failScript, "sh", directory},
			"probe_conflicted": {"-c", failScript, "sh", directory},
			"probe_unknown":    {"-c", failScript, "sh", directory},
		},
		map[string][]string{
			"write":            {"-c", probeScript, "sh", directory},
			"write_then_fail":  {"-c", probeScript, "sh", directory},
			"write_probe_fail": {"-c", `exit 9`, "sh", directory},
			"probe_unchanged":  {"-c", outcomeScript(skills.ProbeUnchanged)},
			"probe_reversed":   {"-c", outcomeScript(skills.ProbeReversed)},
			"probe_drifted":    {"-c", outcomeScript(skills.ProbeDrifted)},
			"probe_conflicted": {"-c", outcomeScript(skills.ProbeConflicted)},
			"probe_unknown":    {"-c", outcomeScript(skills.ProbeUnknown)},
		},
		[]string{"PATH=/usr/bin:/bin"}, directory, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func testCircuit(t *testing.T, tenant string, now func() time.Time) *circuit.Store {
	t.Helper()
	store, err := circuit.New(integrationPool, tenant, circuit.Config{
		FailureThreshold: 3, SuccessThreshold: 1, Window: time.Minute,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func installCircuitUpdateFailure(t *testing.T, tenant, label string) {
	t.Helper()
	name := strings.ReplaceAll(label, "-", "_")
	function := "force_circuit_" + name
	trigger := "force_circuit_" + name
	_, err := integrationPool.Exec(context.Background(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced circuit failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER %s
		BEFORE UPDATE ON workforce_circuit_breakers
		FOR EACH ROW WHEN (OLD.tenant_id='%s')
		EXECUTE FUNCTION %s()
	`, function, trigger, tenant, function))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(context.Background(), fmt.Sprintf(`
			DROP TRIGGER IF EXISTS %s ON workforce_circuit_breakers;
			DROP FUNCTION IF EXISTS %s()
		`, trigger, function))
	})
}

func installEffectUpdateFailure(t *testing.T, tenant, label, condition string) {
	t.Helper()
	name := strings.ReplaceAll(label, "-", "_")
	function := "force_effect_update_" + name
	trigger := "force_effect_update_" + name
	_, err := integrationPool.Exec(context.Background(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced effect update failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER %s
		BEFORE UPDATE ON workforce_effect_operations
		FOR EACH ROW WHEN (OLD.tenant_id='%s' AND (%s))
		EXECUTE FUNCTION %s()
	`, function, trigger, tenant, condition, function))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(context.Background(), fmt.Sprintf(`
			DROP TRIGGER IF EXISTS %s ON workforce_effect_operations;
			DROP FUNCTION IF EXISTS %s()
		`, trigger, function))
	})
}

func installEffectEvidenceFailure(t *testing.T, tenant, label string) {
	t.Helper()
	name := strings.ReplaceAll(label, "-", "_")
	function := "force_effect_evidence_" + name
	trigger := "force_effect_evidence_" + name
	_, err := integrationPool.Exec(context.Background(), fmt.Sprintf(`
		CREATE FUNCTION %s() RETURNS trigger AS $$
		BEGIN
			RAISE EXCEPTION 'forced effect evidence failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE TRIGGER %s
		BEFORE INSERT ON workforce_effect_evidence
		FOR EACH ROW WHEN (NEW.tenant_id='%s')
		EXECUTE FUNCTION %s()
	`, function, trigger, tenant, function))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = integrationPool.Exec(context.Background(), fmt.Sprintf(`
			DROP TRIGGER IF EXISTS %s ON workforce_effect_evidence;
			DROP FUNCTION IF EXISTS %s()
		`, trigger, function))
	})
}

func leaseRequest(label string, now time.Time) lease.Request {
	return lease.Request{
		ID: contracts.LeaseID("lease:" + label), WakeID: contracts.WakeID("wake:" + label),
		OrganizationID: contracts.OrganizationID("organization:" + label),
		SeatID:         contracts.SeatID("seat:" + label), NodeID: dependency.NodeID("node:" + label),
		MandateID: contracts.MandateID("mandate:" + label), MandateVersion: 1,
		Policies: []contracts.PolicyRef{{
			ID: contracts.PolicyID("policy:" + label), Version: 1,
			Hash: contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("a", 64)},
		}},
		IssuedAt: now, ExpiresAt: now.Add(time.Hour),
	}
}

func insertAuthority(t *testing.T, tenant string, request lease.Request, version uint64) {
	t.Helper()
	for _, record := range []struct {
		kind, id, hash string
	}{
		{"mandate", string(request.MandateID), strings.Repeat("b", 64)},
		{"policy", string(request.Policies[0].ID), request.Policies[0].Hash.Digest},
	} {
		_, err := integrationPool.Exec(context.Background(), `
			INSERT INTO workforce_authority_records (
				tenant_id,organization_id,authority_kind,authority_id,version,
				owner_id,key_id,effective_at,canonical_hash,sealed_record,
				material_change,created_at
			) VALUES ($1,$2,$3,$4,$5,'owner','key',$6,$7,$8,FALSE,$6)
			ON CONFLICT DO NOTHING
		`, tenant, request.OrganizationID, record.kind, record.id, version,
			baseTime(), record.hash, []byte{1})
		if err != nil {
			t.Fatal(err)
		}
	}
}

type gatewayPolicyAuthority struct {
	store    *policy.Store
	keyID    string
	private  ed25519.PrivateKey
	grant    policy.OwnerGrant
	seatDID  contracts.SeatDID
	mandate  contracts.Mandate
	seat     contracts.Seat
	policies []contracts.Policy
}

func prepareGatewayPolicyAuthority(
	t *testing.T,
	tenant string,
	request lease.Request,
	label string,
) (lease.Request, gatewayPolicyAuthority) {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	keyID := "policy-owner-key:" + label
	ownerID := contracts.OwnerID("policy-owner:" + label)
	store, err := policy.New(
		integrationPool, testVault(t, tenant), policy.OwnerRoot{
			TenantID: tenant, OrganizationID: request.OrganizationID,
			OwnerID: ownerID, KeyID: keyID, PublicKey: publicKey,
		}, baseTime,
	)
	if err != nil {
		t.Fatal(err)
	}
	grant := policy.OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      tenant, OrganizationID: request.OrganizationID,
		OwnerID: ownerID, KeyID: keyID, Scope: "authority:write",
		IssuedAt: baseTime().Add(-time.Minute), ExpiresAt: baseTime().Add(time.Hour),
	}
	if err := policy.SignOwnerGrant(&grant, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	if err := testauthority.PublishRuntimeAuthority(
		context.Background(), store, request.OrganizationID,
		keyID, publicKey, privateKey, grant, baseTime().Add(-time.Hour),
	); err != nil {
		t.Fatal(err)
	}
	mandate := contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.MandateID, Version: request.MandateVersion,
		OrganizationID: request.OrganizationID,
		DepartmentKind: contracts.DepartmentDeveloper,
		SeatRole:       contracts.SeatExecutor,
		AllowedSkills:  []contracts.SkillID{contracts.SkillID("skill:" + label)},
		DataScopes: []contracts.DataScope{{
			Name: "effects", Classification: contracts.ClassificationOrganization,
			Purpose: "Dispatch only a compiled provider operation",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "Provider evidence is insufficient",
			Action:    "Escalate to the human owner",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID: "no-uncompiled-effects", Description: "Never dispatch an uncompiled operation",
		}},
		EffectiveAt: baseTime().Add(-time.Hour),
	}
	if err := policy.SignMandate(&mandate, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	seatDID := contracts.SeatDID("did:matrix:effect:" + label)
	seat := contracts.Seat{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.SeatID, Version: 1, DID: seatDID,
		OrganizationID: request.OrganizationID,
		DepartmentID:   contracts.DepartmentID("department:" + label),
		Role:           contracts.SeatExecutor,
		MandateID:      request.MandateID, MandateVersion: request.MandateVersion,
		BindingID: contracts.SeatBindingID("binding:" + label), BindingVersion: 1,
		EffectiveAt: baseTime().Add(-time.Hour),
	}
	if err := policy.SignSeat(&seat, keyID, privateKey); err != nil {
		t.Fatal(err)
	}
	policies := make([]contracts.Policy, len(request.Policies))
	for index := range request.Policies {
		value := contracts.Policy{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            request.Policies[index].ID, Version: request.Policies[index].Version,
			OrganizationID: request.OrganizationID, Kind: "effect_dispatch",
			EffectiveAt: baseTime().Add(-time.Hour),
			Rules: []contracts.PolicyRule{{
				ClauseID: "compiled-only", Outcome: "allow",
				Scope: "Exact compiled proposal under a current signed lease",
			}},
		}
		if err := policy.SignPolicy(&value, keyID, privateKey); err != nil {
			t.Fatal(err)
		}
		canonical, err := contracts.EncodeCanonical(&value)
		if err != nil {
			t.Fatal(err)
		}
		sum := sha256.Sum256(canonical)
		request.Policies[index].Hash = contracts.ContentHash{
			Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
		}
		policies[index] = value
	}
	if err := store.PublishMandate(context.Background(), mandate, grant); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishSeat(context.Background(), seat, grant); err != nil {
		t.Fatal(err)
	}
	for _, value := range policies {
		if err := store.PublishPolicy(context.Background(), value, grant); err != nil {
			t.Fatal(err)
		}
	}
	gatewayPolicyMutex.Lock()
	gatewayPolicyAuthorities[tenant] = store
	gatewayPolicyMutex.Unlock()
	return request, gatewayPolicyAuthority{
		store: store, keyID: keyID, private: privateKey, grant: grant, seatDID: seatDID,
		mandate: mandate, seat: seat, policies: policies,
	}
}

func registerGatewayWakeLease(
	t *testing.T,
	authority gatewayPolicyAuthority,
	request lease.Request,
	fence contracts.FenceToken,
	label string,
) contracts.WakeLease {
	t.Helper()
	value := contracts.WakeLease{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.ID, WakeID: request.WakeID, OrganizationID: request.OrganizationID,
		SeatID: request.SeatID, SeatDID: authority.seatDID, Reason: "eligible_work",
		MandateID: request.MandateID, MandateVersion: request.MandateVersion,
		Policies:   append([]contracts.PolicyRef(nil), request.Policies...),
		GraphScope: []contracts.IntentID{contracts.IntentID(request.NodeID)},
		Model: contracts.ModelBinding{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.ModelBindingID("model:" + label),
			Provider:      "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
			SamplingDigest: contentHash("gateway-sampling:" + label),
		},
		MGS: contracts.MGSGenomeRef{
			Reference: "mgs:" + label, Digest: contentHash("gateway-mgs:" + label),
		},
		Runtime: contracts.RuntimeBinding{
			BuildDigest:             contentHash("gateway-runtime:" + label),
			AuditorBuildDigest:      contentHash("gateway-auditor-runtime:" + label),
			OperationRegistryDigest: contentHash("gateway-registry:" + label),
		},
		SkillCatalogDigest: contentHash("gateway-catalog:" + label),
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64(time.Hour / time.Millisecond),
			MaxSteps:          50, MaxModelCalls: 8, MaxToolCalls: 32,
			MaxCostMinor: 1000, Currency: "USD", MaxOutputBytes: 1 << 20,
		},
		IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt, Fence: fence,
	}
	if err := policy.SignWakeLease(&value, authority.keyID, authority.private); err != nil {
		t.Fatal(err)
	}
	if err := authority.store.RegisterLease(context.Background(), value); err != nil {
		t.Fatal(err)
	}
	return value
}

func gatewayPolicyStore(t *testing.T, tenant string) *policy.Store {
	t.Helper()
	gatewayPolicyMutex.Lock()
	defer gatewayPolicyMutex.Unlock()
	store := gatewayPolicyAuthorities[tenant]
	if store == nil {
		t.Fatalf("no signed lease authority registered for %s", tenant)
	}
	return store
}

var (
	testVaultMutex           sync.Mutex
	testVaults               = map[string]*vault.UserVault{}
	gatewayPolicyMutex       sync.Mutex
	gatewayPolicyAuthorities = map[string]*policy.Store{}
)

// testVault returns one stable Vault per tenant for the whole test binary.
// Sealing and opening must use the same user key, exactly as a single running
// kernel would.
func testVault(t *testing.T, tenant string) *vault.UserVault {
	t.Helper()
	testVaultMutex.Lock()
	defer testVaultMutex.Unlock()
	if existing, found := testVaults[tenant]; found {
		return existing
	}
	directory, err := os.MkdirTemp("", "workforce-effect-vault-")
	if err != nil {
		t.Fatal(err)
	}
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: directory, UserDID: tenant, KEKHex: kek,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !session.Encrypting() || session.UserVault() == nil {
		t.Fatal("Vault did not start encrypting")
	}
	testVaults[tenant] = session.UserVault()
	return session.UserVault()
}

func gatewayTenant(label string) string { return "tenant:effect:" + label }

func baseTime() time.Time {
	return time.Date(2026, time.July, 30, 18, 0, 0, 0, time.UTC)
}

func startPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", fmt.Errorf("random container suffix: %w", err)
	}
	name := "workforce-effect-" + hex.EncodeToString(suffix[:])
	output, err := exec.CommandContext(ctx, "docker", "run", "--rm", "-d",
		"--name", name, "-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432", postgresImage,
	).CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL container: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("inspect PostgreSQL port: %w: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	_, port, found := strings.Cut(address, ":")
	if !found || port == "" {
		return containerID, "", fmt.Errorf("unexpected PostgreSQL port mapping %q", address)
	}
	return containerID,
		"postgres://postgres:workforce-test-password@127.0.0.1:" + port +
			"/workforce?sslmode=disable", nil
}

func waitForPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for PostgreSQL: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}
