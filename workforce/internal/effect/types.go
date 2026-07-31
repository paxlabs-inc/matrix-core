// Package effect owns the only credential-bearing external mutation gateway.
package effect

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/skills"
)

var (
	// ErrConflict means an idempotency identity was reused for different work.
	ErrConflict = errors.New("effect proposal conflicts with durable identity")
	// ErrAmbiguous means dispatch may have happened and must reconcile before retry.
	ErrAmbiguous = errors.New("effect outcome is externally ambiguous")
	// ErrRejected means preflight or a definitely-before-send failure rejected work.
	ErrRejected = errors.New("effect was rejected before external dispatch")
	// ErrUncertain means the gateway cannot prove durable state and fails closed.
	ErrUncertain = errors.New("effect gateway state is uncertain")
)

// State is the closed durable effect lifecycle.
type State string

const (
	// StatePrepared is durable but has not started provider dispatch.
	StatePrepared State = "prepared"
	// StateDispatching is durable before provider invocation.
	StateDispatching State = "dispatching"
	// StateSucceeded has authoritative evidence.
	StateSucceeded State = "succeeded"
	// StateFailed definitely did not dispatch or authoritatively failed.
	StateFailed State = "failed"
	// StateExternallyAmbiguous requires reconciliation and forbids blind retry.
	StateExternallyAmbiguous State = "externally_ambiguous"
)

// Valid reports whether state is recognized.
func (state State) Valid() bool {
	return state == StatePrepared || state == StateDispatching ||
		state == StateSucceeded || state == StateFailed ||
		state == StateExternallyAmbiguous
}

// Proposal is a fully bound external operation proposed by a seat.
type Proposal struct {
	ID              string
	OrganizationID  contracts.OrganizationID
	IntentID        contracts.IntentID
	NodeID          dependency.NodeID
	SeatID          contracts.SeatID
	LeaseID         contracts.LeaseID
	Fence           contracts.FenceToken
	Provider        string
	SkillID         contracts.SkillID
	EffectClass     skills.EffectClass
	Irreversible    bool
	Operation       string
	IdempotencyKey  string
	SkillDigest     contracts.ContentHash
	OperationDigest contracts.ContentHash
	ApprovalID      contracts.ApprovalID
	ApprovalCost    uint64
	Input           []byte
	Deadline        time.Time
}

// Validate enforces the complete preflight identity and bounded input.
func (proposal Proposal) Validate() error {
	for name, value := range map[string]string{
		"proposal_id": proposal.ID, "organization_id": string(proposal.OrganizationID),
		"intent_id": string(proposal.IntentID), "seat_id": string(proposal.SeatID),
		"node_id":  string(proposal.NodeID),
		"lease_id": string(proposal.LeaseID), "provider": proposal.Provider,
		"skill_id":  string(proposal.SkillID),
		"operation": proposal.Operation, "idempotency_key": proposal.IdempotencyKey,
	} {
		if err := validateToken(name, value); err != nil {
			return err
		}
	}
	if err := proposal.Fence.Validate(); err != nil {
		return err
	}
	if !proposal.EffectClass.Valid() {
		return fmt.Errorf("effect class is invalid")
	}
	if proposal.Irreversible != (proposal.EffectClass == skills.EffectIrreversible) {
		return fmt.Errorf("irreversible flag must match effect class")
	}
	if proposal.Irreversible {
		if err := validateToken("approval_id", string(proposal.ApprovalID)); err != nil ||
			proposal.ApprovalCost == 0 {
			return fmt.Errorf("irreversible effect requires exact approval authority")
		}
	} else if proposal.ApprovalID != "" || proposal.ApprovalCost != 0 {
		return fmt.Errorf("reversible effect cannot consume approval authority")
	}
	if err := proposal.SkillDigest.Validate(); err != nil {
		return fmt.Errorf("skill digest: %w", err)
	}
	if err := proposal.OperationDigest.Validate(); err != nil {
		return fmt.Errorf("operation digest: %w", err)
	}
	if len(proposal.Input) == 0 || len(proposal.Input) > 256<<10 {
		return fmt.Errorf("effect input must contain 1 to 262144 bytes")
	}
	if proposal.Deadline.IsZero() || proposal.Deadline.Location() != time.UTC {
		return fmt.Errorf("effect deadline must be non-zero UTC")
	}
	return nil
}

// DispatchResult states whether provider dispatch began and carries observation.
type DispatchResult struct {
	Started     bool
	ExternalID  string
	Observation []byte
	ObservedAt  time.Time
}

// ProbeResult carries a closed authoritative drift outcome and any observation.
type ProbeResult struct {
	Outcome  skills.ProbeOutcome
	Dispatch DispatchResult
	Reason   string
}

// Result is the durable effect projection safe to return to a seat.
type Result struct {
	ProposalID    string
	State         State
	ExternalID    string
	EvidenceHash  contracts.ContentHash
	ObservedAt    time.Time
	SafeErrorCode string
	ProbeOutcome  skills.ProbeOutcome
	Deduplicated  bool
}

// Adapter is a real provider boundary. Implementations own credentials and must
// propagate the supplied idempotency identity where supported.
type Adapter interface {
	Name() string
	Dispatch(context.Context, Operation) (DispatchResult, error)
	Probe(context.Context, Operation) (ProbeResult, error)
}

// Operation is the immutable provider request after gateway preflight.
type Operation struct {
	OrganizationID contracts.OrganizationID
	SeatID         contracts.SeatID
	LeaseID        contracts.LeaseID
	Fence          contracts.FenceToken
	Name           string
	IdempotencyKey string
	Input          []byte
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", name)
	}
	return nil
}
