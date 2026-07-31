// Package reconcile owns authoritative drift and ambiguous-effect resolution.
package reconcile

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"matrix/workforce/internal/circuit"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/skills"
)

// Event is one durable authoritative reconciliation projection.
type Event struct {
	OrganizationID contracts.OrganizationID
	ProposalID     string
	IntentID       contracts.IntentID
	NodeID         dependency.NodeID
	Outcome        skills.ProbeOutcome
	EffectState    effect.State
	EvidenceHash   contracts.ContentHash
	SafeReason     string
	ObservedAt     time.Time
}

// Summary is the complete deterministic sweep performed before work selection.
type Summary struct {
	OrganizationID contracts.OrganizationID
	Events         []Event
	Completed      uint32
	Blocked        uint32
	Unchanged      uint32
}

// Service coordinates the concrete effect gateway and global graph store.
type Service struct {
	pool     *pgxpool.Pool
	gateway  *effect.Gateway
	graph    *dependency.Store
	tenantID string
	now      func() time.Time
}

// New constructs one tenant-scoped reconciliation authority.
func New(
	pool *pgxpool.Pool,
	gateway *effect.Gateway,
	graph *dependency.Store,
	tenantID string,
	now func() time.Time,
) (*Service, error) {
	tenantID = strings.TrimSpace(tenantID)
	if pool == nil || gateway == nil || graph == nil || tenantID == "" || now == nil {
		return nil, fmt.Errorf("reconcile: pool, gateway, graph, tenant_id, and time source are required")
	}
	return &Service{
		pool: pool, gateway: gateway, graph: graph, tenantID: tenantID, now: now,
	}, nil
}

// Sweep resolves every pending external effect before any new work may be selected.
func (service *Service) Sweep(
	ctx context.Context,
	organizationID contracts.OrganizationID,
) (Summary, error) {
	if strings.TrimSpace(string(organizationID)) == "" {
		return Summary{}, fmt.Errorf("reconcile: organization_id is required")
	}
	now := service.now()
	if now.IsZero() || now.Location() != time.UTC {
		return Summary{}, fmt.Errorf("reconcile: time source must return UTC")
	}
	pending, err := service.gateway.Pending(ctx, organizationID)
	if err != nil {
		return Summary{}, err
	}
	sort.Slice(pending, func(left, right int) bool {
		return pending[left].ID < pending[right].ID
	})
	summary := Summary{OrganizationID: organizationID}
	byNode := make(map[dependency.NodeID][]skills.ProbeOutcome)
	for _, proposal := range pending {
		result, reconcileErr := service.gateway.Reconcile(ctx, proposal)
		outcome := result.ProbeOutcome
		if errors.Is(reconcileErr, circuit.ErrOpen) {
			outcome = skills.ProbeUnknown
			result = effect.Result{
				ProposalID: proposal.ID, State: effect.StateExternallyAmbiguous,
				ProbeOutcome: outcome, SafeErrorCode: "probe_circuit_open",
			}
		} else if reconcileErr != nil && !errors.Is(reconcileErr, effect.ErrAmbiguous) {
			return Summary{}, reconcileErr
		}
		if !outcome.Valid() {
			outcome = skills.ProbeUnknown
		}
		event := Event{
			OrganizationID: organizationID, ProposalID: proposal.ID,
			IntentID: proposal.IntentID, NodeID: proposal.NodeID, Outcome: outcome,
			EffectState: result.State, EvidenceHash: result.EvidenceHash,
			SafeReason: result.SafeErrorCode, ObservedAt: now,
		}
		if err := service.record(ctx, event); err != nil {
			return Summary{}, err
		}
		summary.Events = append(summary.Events, event)
		byNode[event.NodeID] = append(byNode[event.NodeID], outcome)
		switch outcome {
		case skills.ProbeCompletedOutOfBand:
			summary.Completed++
		case skills.ProbeUnchanged:
			summary.Unchanged++
		default:
			summary.Blocked++
		}
	}
	nodeIDs := make([]dependency.NodeID, 0, len(byNode))
	for nodeID := range byNode {
		nodeIDs = append(nodeIDs, nodeID)
	}
	sort.Slice(nodeIDs, func(left, right int) bool { return nodeIDs[left] < nodeIDs[right] })
	for _, nodeID := range nodeIDs {
		if err := service.applyIntent(ctx, organizationID, nodeID, byNode[nodeID]); err != nil {
			return Summary{}, err
		}
	}
	return summary, nil
}

func (service *Service) record(ctx context.Context, event Event) error {
	command, err := service.pool.Exec(ctx, `
		INSERT INTO workforce_reconciliation_events (
			tenant_id,organization_id,proposal_id,intent_id,outcome,effect_state,
			evidence_hash,safe_reason,observed_at
		) VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),NULLIF($8,''),$9)
		ON CONFLICT DO NOTHING
	`, service.tenantID, event.OrganizationID, event.ProposalID, event.IntentID,
		event.Outcome, event.EffectState, event.EvidenceHash.Digest,
		event.SafeReason, event.ObservedAt)
	if err != nil {
		return fmt.Errorf("reconcile: persist event: %w", err)
	}
	if command.RowsAffected() != 1 {
		var count int
		err := service.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM workforce_reconciliation_events
			WHERE tenant_id=$1 AND organization_id=$2 AND proposal_id=$3
			  AND intent_id=$4 AND outcome=$5 AND effect_state=$6
			  AND COALESCE(evidence_hash,'')=$7 AND COALESCE(safe_reason,'')=$8
			  AND observed_at=$9
		`, service.tenantID, event.OrganizationID, event.ProposalID, event.IntentID,
			event.Outcome, event.EffectState, event.EvidenceHash.Digest,
			event.SafeReason, event.ObservedAt).Scan(&count)
		if err != nil || count != 1 {
			return fmt.Errorf("reconcile: event identity conflict")
		}
	}
	return nil
}

func (service *Service) applyIntent(
	ctx context.Context,
	organizationID contracts.OrganizationID,
	nodeID dependency.NodeID,
	outcomes []skills.ProbeOutcome,
) error {
	snapshot, err := service.graph.Snapshot(ctx, organizationID)
	if err != nil {
		return err
	}
	var node dependency.Node
	found := false
	for _, candidate := range snapshot.Nodes {
		if candidate.ID == nodeID {
			node, found = candidate, true
			break
		}
	}
	if !found {
		return fmt.Errorf("reconcile: graph node %q is missing", nodeID)
	}
	if node.State == dependency.StateContested ||
		node.State == dependency.StateCancelled || node.State == dependency.StateFailed {
		return nil
	}
	target := aggregate(outcomes)
	if node.State == target || node.State == dependency.StateCompleted {
		return nil
	}
	return service.graph.Transition(ctx, organizationID, nodeID, node.Version, target, "")
}

func aggregate(outcomes []skills.ProbeOutcome) dependency.NodeState {
	for _, outcome := range outcomes {
		switch outcome {
		case skills.ProbeReversed, skills.ProbeDrifted,
			skills.ProbeConflicted, skills.ProbeUnknown:
			return dependency.StateContested
		case skills.ProbeUnchanged:
		case skills.ProbeCompletedOutOfBand:
		default:
			return dependency.StateContested
		}
	}
	// Authoritative provider reconciliation proves the effect outcome, but it
	// is not an independent audit receipt. Keep the graph waiting so the wake
	// runtime can verify, receipt, and close it through FinishWithReceipt.
	return dependency.StateWaiting
}
