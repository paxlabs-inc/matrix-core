// Package dependency owns the deterministic global work graph and its durable
// PostgreSQL projection.
package dependency

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

var (
	// ErrConflict reports optimistic-concurrency or immutable graph conflicts.
	ErrConflict = errors.New("work graph conflict")
	// ErrCycle reports an edge that would make the dependency graph cyclic.
	ErrCycle = errors.New("work graph cycle")
	// ErrNotFound reports a missing graph object without exposing another tenant.
	ErrNotFound = errors.New("work graph object not found")
)

// NodeID identifies one typed object in the global work graph.
type NodeID string

// NodeKind is a closed graph object family.
type NodeKind string

const (
	// NodeGoal is a durable desired outcome.
	NodeGoal NodeKind = "goal"
	// NodeIntent is a bounded executable unit of work.
	NodeIntent NodeKind = "intent"
	// NodeDelegation is a typed cross-seat obligation.
	NodeDelegation NodeKind = "delegation"
	// NodeHandoff is a typed transfer of work state.
	NodeHandoff NodeKind = "handoff"
	// NodeArtifact is an immutable required artifact.
	NodeArtifact NodeKind = "artifact"
	// NodeApproval is an approval predicate.
	NodeApproval NodeKind = "approval"
	// NodeTerminalOutcome is receipt-backed terminal truth.
	NodeTerminalOutcome NodeKind = "terminal_outcome"
)

// Valid reports whether the node kind is executable by this release.
func (kind NodeKind) Valid() bool {
	switch kind {
	case NodeGoal, NodeIntent, NodeDelegation, NodeHandoff, NodeArtifact,
		NodeApproval, NodeTerminalOutcome:
		return true
	default:
		return false
	}
}

// NodeState is the closed lifecycle of a graph node.
type NodeState string

const (
	// StatePending has unresolved prerequisites.
	StatePending NodeState = "pending"
	// StateEligible is resolver-ready.
	StateEligible NodeState = "eligible"
	// StateLeased has one active holder.
	StateLeased NodeState = "leased"
	// StateWaiting is explicitly waiting for a non-graph condition.
	StateWaiting NodeState = "waiting"
	// StateCompleted is backed by committed verified truth.
	StateCompleted NodeState = "completed"
	// StateCancelled is terminal by deterministic propagation or owner action.
	StateCancelled NodeState = "cancelled"
	// StateFailed is a receipt-backed terminal failure.
	StateFailed NodeState = "failed"
	// StateContested is paused by a correction.
	StateContested NodeState = "contested"
)

// Valid reports whether the node state is recognized.
func (state NodeState) Valid() bool {
	switch state {
	case StatePending, StateEligible, StateLeased, StateWaiting, StateCompleted,
		StateCancelled, StateFailed, StateContested:
		return true
	default:
		return false
	}
}

// Terminal reports whether the node can no longer become eligible.
func (state NodeState) Terminal() bool {
	return state == StateCompleted || state == StateCancelled || state == StateFailed
}

// EdgeKind is a closed dependency relationship.
type EdgeKind string

const (
	// EdgeDependency is a general prerequisite.
	EdgeDependency EdgeKind = "dependency"
	// EdgeDelegation is a bounded cross-seat dependency.
	EdgeDelegation EdgeKind = "delegation"
	// EdgeHandoff requires a typed handoff.
	EdgeHandoff EdgeKind = "handoff"
	// EdgeArtifact requires an immutable artifact.
	EdgeArtifact EdgeKind = "artifact"
	// EdgeApproval requires an approval result.
	EdgeApproval EdgeKind = "approval"
	// EdgeCorrection blocks on correction reconciliation.
	EdgeCorrection EdgeKind = "correction"
)

// Valid reports whether the edge kind is recognized.
func (kind EdgeKind) Valid() bool {
	switch kind {
	case EdgeDependency, EdgeDelegation, EdgeHandoff, EdgeArtifact,
		EdgeApproval, EdgeCorrection:
		return true
	default:
		return false
	}
}

// Node is one durable typed graph object.
type Node struct {
	ID                 NodeID
	OrganizationID     contracts.OrganizationID
	Kind               NodeKind
	OwnerSeatID        *contracts.SeatID
	OwnerDepartmentID  *contracts.DepartmentID
	Title              string
	State              NodeState
	BasePriority       int32
	CreatedAt          time.Time
	UpdatedAt          time.Time
	Deadline           *time.Time
	Contested          bool
	CancellationReason string
	TerminalRecordID   *contracts.RecordID
	Version            uint64
}

// Validate rejects incomplete, ambiguous, or out-of-bounds graph nodes.
func (node Node) Validate() error {
	if err := validateToken("node_id", string(node.ID)); err != nil {
		return err
	}
	if err := validateToken("organization_id", string(node.OrganizationID)); err != nil {
		return err
	}
	if !node.Kind.Valid() || !node.State.Valid() {
		return fmt.Errorf("node kind and state must be valid")
	}
	if strings.TrimSpace(node.Title) == "" || len(node.Title) > 512 {
		return fmt.Errorf("node title must contain 1 to 512 bytes")
	}
	if node.BasePriority < -1000 || node.BasePriority > 1000 {
		return fmt.Errorf("node base priority must be between -1000 and 1000")
	}
	if !validUTC(node.CreatedAt) || !validUTC(node.UpdatedAt) ||
		node.UpdatedAt.Before(node.CreatedAt) {
		return fmt.Errorf("node times must be non-zero UTC and ordered")
	}
	if node.Deadline != nil && (!validUTC(*node.Deadline) || !node.Deadline.After(node.CreatedAt)) {
		return fmt.Errorf("node deadline must be UTC and follow creation")
	}
	if (node.State == StateCancelled) != (strings.TrimSpace(node.CancellationReason) != "") {
		return fmt.Errorf("cancelled nodes require a reason and other nodes forbid one")
	}
	if node.TerminalRecordID != nil {
		if err := validateToken(
			"terminal_record_id", string(*node.TerminalRecordID),
		); err != nil {
			return err
		}
	}
	if node.Version == 0 {
		return fmt.Errorf("node version must be positive")
	}
	return nil
}

// Edge is a directed prerequisite-to-dependent graph relationship.
type Edge struct {
	OrganizationID         contracts.OrganizationID
	Prerequisite           NodeID
	Dependent              NodeID
	Kind                   EdgeKind
	RequiredResponseSchema string
	ExpiresAt              *time.Time
	TimeoutAction          contracts.TimeoutAction
	SLAAt                  *time.Time
	CreatedAt              time.Time
}

// Validate enforces a bounded edge and complete delegation contract.
func (edge Edge) Validate() error {
	if err := validateToken("organization_id", string(edge.OrganizationID)); err != nil {
		return err
	}
	if err := validateToken("prerequisite", string(edge.Prerequisite)); err != nil {
		return err
	}
	if err := validateToken("dependent", string(edge.Dependent)); err != nil {
		return err
	}
	if edge.Prerequisite == edge.Dependent {
		return fmt.Errorf("edge cannot depend on itself")
	}
	if !edge.Kind.Valid() || !validUTC(edge.CreatedAt) {
		return fmt.Errorf("edge kind and created_at must be valid")
	}
	if edge.Kind == EdgeDelegation {
		if strings.TrimSpace(edge.RequiredResponseSchema) == "" ||
			len(edge.RequiredResponseSchema) > 512 {
			return fmt.Errorf("delegation requires a bounded response schema")
		}
		if edge.ExpiresAt == nil || edge.SLAAt == nil ||
			!validUTC(*edge.ExpiresAt) || !validUTC(*edge.SLAAt) ||
			!edge.ExpiresAt.After(edge.CreatedAt) ||
			edge.SLAAt.Before(edge.CreatedAt) || edge.SLAAt.After(*edge.ExpiresAt) ||
			!edge.TimeoutAction.Valid() {
			return fmt.Errorf("delegation requires ordered expiry, SLA, and timeout action")
		}
	} else if edge.RequiredResponseSchema != "" || edge.ExpiresAt != nil ||
		edge.SLAAt != nil || edge.TimeoutAction != "" {
		return fmt.Errorf("delegation fields are forbidden on %q edge", edge.Kind)
	}
	return nil
}

// IncidentKind is an owner-visible graph failure.
type IncidentKind string

const (
	// IncidentDeadlock reports pending nodes with no external unblock path.
	IncidentDeadlock IncidentKind = "deadlock"
	// IncidentOrphan reports non-root work with no predecessor or parent path.
	IncidentOrphan IncidentKind = "orphan"
	// IncidentSLABreach reports an unresolved delegation beyond its SLA.
	IncidentSLABreach IncidentKind = "sla_breach"
	// IncidentDelegationExpired reports an unresolved expired delegation.
	IncidentDelegationExpired IncidentKind = "delegation_expired"
)

// Incident is a deterministic dashboard-facing explanation.
type Incident struct {
	ID             string
	OrganizationID contracts.OrganizationID
	Kind           IncidentKind
	NodeIDs        []NodeID
	Explanation    string
	CreatedAt      time.Time
}

// Projection is the authorized deterministic slice exposed to one seat.
type Projection struct {
	Nodes     []Node
	Edges     []Edge
	Eligible  []Node
	Incidents []Incident
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 bytes", name)
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", name)
	}
	return nil
}

func validUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
