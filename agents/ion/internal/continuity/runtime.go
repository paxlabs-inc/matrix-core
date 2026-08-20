package continuity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Sidiora-Labs/centra-llm-agents/ion/internal/session"
	"github.com/Sidiora-Labs/centra-llm-agents/ion/internal/work"
	"github.com/Sidiora-Labs/centra-llm-agents/ion/pkg/types"
	"github.com/google/uuid"
)

const (
	defaultLeaseTTL = 90 * time.Second
	maxLeaseTTL     = 30 * time.Minute
	maxRetiredKeys  = 4096
)

// Runtime owns goal-bound authorization over durable continuation state. It
// writes approved plans, work-item transitions, and verified evidence through
// the production work service so no parallel execution rail exists.
type Runtime struct {
	mu    sync.Mutex
	store *session.Store
	clock types.Clock
	work  *work.Service
}

// New constructs the goal-bound continuity runtime.
func New(store *session.Store, clock types.Clock, workService *work.Service) (*Runtime, error) {
	if store == nil || clock == nil || workService == nil {
		return nil, fmt.Errorf("continuity: session store, clock, and work service are required")
	}
	return &Runtime{store: store, clock: clock, work: workService}, nil
}

// ExhaustionError reports frames closed by budget exhaustion with the honest
// parent disposition already applied.
type ExhaustionError struct {
	Closures []FrameClosure `json:"closed_frames"`
	Reason   string         `json:"reason"`
}

func (err *ExhaustionError) Error() string {
	return fmt.Sprintf("continuity: recovery budget exhausted: %s", err.Reason)
}

// Unwrap keeps ErrBudgetExhausted testable through wrapping.
func (err *ExhaustionError) Unwrap() error { return ErrBudgetExhausted }

func (runtime *Runtime) now() time.Time { return runtime.clock.Now().UTC() }

func (runtime *Runtime) rawState(
	ctx context.Context, actor uuid.UUID,
) (json.RawMessage, error) {
	if actor == uuid.Nil {
		return nil, fmt.Errorf("%w: actor is required", ErrInvalidInput)
	}
	return runtime.store.LoadLivingState(ctx, stateKind, actor.String())
}

func (runtime *Runtime) load(ctx context.Context, actor uuid.UUID) (State, error) {
	raw, err := runtime.rawState(ctx, actor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return State{}, runtime.failClosed(ctx, actor, nil, &StateFailure{
				Reason: "durable continuation state is absent",
			})
		}
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, runtime.failClosed(ctx, actor, raw, &StateFailure{
			Reason: "durable continuation state is not decodable",
		})
	}
	if err := validate(state, runtime.now()); err != nil {
		var failure *StateFailure
		if errors.As(err, &failure) {
			return State{}, runtime.failClosed(ctx, actor, raw, failure)
		}
		return State{}, err
	}
	return state, nil
}

// failClosed exposes the original work item as blocked or escalated without
// repairing or overwriting the latest durable continuation state.
func (runtime *Runtime) failClosed(
	ctx context.Context,
	actor uuid.UUID,
	raw json.RawMessage,
	failure *StateFailure,
) error {
	if failure.Disposition == "" {
		failure.Disposition = DispositionBlocked
	}
	var salvage struct {
		ContractID     uuid.UUID `json:"contract_id"`
		OpenWorkItemID string    `json:"open_work_item_id"`
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &salvage)
	}
	if failure.WorkItemID == "" {
		failure.WorkItemID = salvage.OpenWorkItemID
	}
	if failure.WorkItemID != "" && salvage.ContractID != uuid.Nil {
		note := "continuation state validation failed: " + failure.Reason
		if failure.Disposition == DispositionEscalated {
			note = "escalated for human decision: " + note
		}
		if _, err := runtime.work.UpdateWorkItem(ctx, actor, work.WorkItemUpdate{
			ContractID: salvage.ContractID,
			ItemID:     failure.WorkItemID,
			Status:     work.WorkItemBlocked,
			Note:       boundedText(note),
		}); err == nil {
			failure.Exposed = true
		}
	}
	return failure
}

func (runtime *Runtime) save(ctx context.Context, actor uuid.UUID, state State) (State, error) {
	state.Version = StateVersion
	state.ActorID = actor
	state.Revision++
	if len(state.Evidence) > maxEvidence {
		return State{}, fmt.Errorf("%w: evidence ledger is full", ErrInvalidInput)
	}
	state.Cursor = EvidenceCursor{
		Count:  len(state.Evidence),
		Digest: evidenceDigest(state.Evidence),
	}
	for index := len(state.Evidence) - 1; index >= 0; index-- {
		if state.Evidence[index].ArtifactID != uuid.Nil {
			last := state.Evidence[index].ArtifactID
			state.Cursor.LastArtifactID = &last
			break
		}
	}
	state.Actions, state.RetiredKeys = boundActions(state.Actions, state.RetiredKeys)
	if len(state.Fencing) > maxFencing {
		state.Fencing = state.Fencing[len(state.Fencing)-maxFencing:]
	}
	if len(state.Continuations) > maxContinuations {
		state.Continuations = state.Continuations[len(state.Continuations)-maxContinuations:]
	}
	if len(state.Proposals) > maxProposals {
		state.Proposals = state.Proposals[len(state.Proposals)-maxProposals:]
	}
	state.Checksum = stateChecksum(state)
	if err := validate(state, runtime.now()); err != nil {
		return State{}, fmt.Errorf("continuity: refusing to persist inconsistent state: %w", err)
	}
	raw, err := json.Marshal(state)
	if err != nil {
		return State{}, fmt.Errorf("continuity: encode state: %w", err)
	}
	if err := runtime.store.SaveLivingState(ctx, stateKind, actor.String(), raw); err != nil {
		return State{}, err
	}
	return state, nil
}

func boundActions(actions []ActionRecord, retired []string) ([]ActionRecord, []string) {
	if len(actions) <= maxActionRecords {
		return actions, retired
	}
	overflow := len(actions) - maxActionRecords
	dropped := 0
	kept := make([]ActionRecord, 0, maxActionRecords)
	for _, action := range actions {
		if dropped < overflow && action.Settled {
			retired = append(retired, action.IdempotencyKey)
			dropped++
			continue
		}
		kept = append(kept, action)
	}
	if len(retired) > maxRetiredKeys {
		retired = retired[len(retired)-maxRetiredKeys:]
	}
	return kept, retired
}

// ProposeGoalRevision records a bounded revision that is not yet authoritative.
func (runtime *Runtime) ProposeGoalRevision(
	ctx context.Context, actor uuid.UUID, input ProposalInput,
) (GoalProposal, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if actor == uuid.Nil || input.ContractID == uuid.Nil {
		return GoalProposal{}, fmt.Errorf("%w: actor and contract are required", ErrInvalidInput)
	}
	if err := validateProposalInput(input); err != nil {
		return GoalProposal{}, err
	}
	state, err := runtime.loadForProposal(ctx, actor, input.ContractID)
	if err != nil {
		return GoalProposal{}, err
	}
	current, approved := state.latest()
	if approved && len(state.activeFrames()) > 0 {
		return GoalProposal{}, fmt.Errorf(
			"%w: a recovery frame cannot revise the approved goal", ErrFrameActive,
		)
	}
	version := uint64(1)
	if approved {
		version = current.Version + 1
	}
	now := runtime.now()
	proposal := GoalProposal{
		ID: uuid.New(), Version: version, Status: ProposalProposed,
		Origin: boundedText(input.Origin), Rationale: boundedText(input.Rationale),
		Goal: strings.TrimSpace(input.Goal), Deliverable: strings.TrimSpace(input.Deliverable),
		Constraints: trimUnique(input.Constraints), Verification: trimUnique(input.Verification),
		DoneCriteria: normalizeCriteria(input.DoneCriteria), Plan: normalizePlan(input.Plan),
		NextAction: strings.TrimSpace(input.NextAction), CreatedAt: now,
	}
	for index := range state.Proposals {
		if state.Proposals[index].Status == ProposalProposed {
			state.Proposals[index].Status = ProposalSuperseded
			state.Proposals[index].DecidedAt = &now
			state.Proposals[index].Decision = "superseded by a newer proposal"
		}
	}
	state.Proposals = append(state.Proposals, proposal)
	if _, err := runtime.saveProposalState(ctx, actor, state); err != nil {
		return GoalProposal{}, err
	}
	return proposal, nil
}

// loadForProposal tolerates absent state only for the first proposal.
func (runtime *Runtime) loadForProposal(
	ctx context.Context, actor, contractID uuid.UUID,
) (State, error) {
	raw, err := runtime.rawState(ctx, actor)
	if errors.Is(err, sql.ErrNoRows) {
		return State{Version: StateVersion, ActorID: actor, ContractID: contractID}, nil
	}
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, runtime.failClosed(ctx, actor, raw, &StateFailure{
			Reason: "durable continuation state is not decodable",
		})
	}
	if len(state.Approved) == 0 {
		state.Version, state.ActorID = StateVersion, actor
		if state.ContractID == uuid.Nil {
			state.ContractID = contractID
		}
		return state, nil
	}
	if err := validate(state, runtime.now()); err != nil {
		var failure *StateFailure
		if errors.As(err, &failure) {
			return State{}, runtime.failClosed(ctx, actor, raw, failure)
		}
		return State{}, err
	}
	if state.ContractID != contractID {
		return State{}, fmt.Errorf("%w: proposal targets another contract", ErrInvalidInput)
	}
	return state, nil
}

// saveProposalState persists proposals before the first approval exists.
func (runtime *Runtime) saveProposalState(
	ctx context.Context, actor uuid.UUID, state State,
) (State, error) {
	if len(state.Approved) > 0 {
		return runtime.save(ctx, actor, state)
	}
	state.Version, state.ActorID = StateVersion, actor
	state.Revision++
	state.Cursor = EvidenceCursor{Count: 0, Digest: evidenceDigest(nil)}
	state.Checksum = stateChecksum(state)
	raw, err := json.Marshal(state)
	if err != nil {
		return State{}, fmt.Errorf("continuity: encode state: %w", err)
	}
	if err := runtime.store.SaveLivingState(ctx, stateKind, actor.String(), raw); err != nil {
		return State{}, err
	}
	return state, nil
}

// ApproveGoalRevision makes one proposal the immutable authorization root at a
// strictly increased version and writes its plan through the work service.
func (runtime *Runtime) ApproveGoalRevision(
	ctx context.Context, actor, proposalID uuid.UUID, approver string,
) (GoalContract, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if invalidText(approver) {
		return GoalContract{}, fmt.Errorf("%w: explicit approver is required", ErrInvalidInput)
	}
	raw, err := runtime.rawState(ctx, actor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalContract{}, fmt.Errorf("%w: no proposal exists", ErrNotFound)
		}
		return GoalContract{}, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return GoalContract{}, runtime.failClosed(ctx, actor, raw, &StateFailure{
			Reason: "durable continuation state is not decodable",
		})
	}
	if len(state.Approved) > 0 {
		if err := validate(state, runtime.now()); err != nil {
			var failure *StateFailure
			if errors.As(err, &failure) {
				return GoalContract{}, runtime.failClosed(ctx, actor, raw, failure)
			}
			return GoalContract{}, err
		}
		if len(state.activeFrames()) > 0 {
			return GoalContract{}, fmt.Errorf(
				"%w: close recovery before revising the approved goal", ErrFrameActive,
			)
		}
	}
	index := -1
	for candidate := range state.Proposals {
		if state.Proposals[candidate].ID == proposalID {
			index = candidate
			break
		}
	}
	if index < 0 || state.Proposals[index].Status != ProposalProposed {
		return GoalContract{}, fmt.Errorf("%w: open proposal not found", ErrNotFound)
	}
	proposal := state.Proposals[index]
	current, approved := state.latest()
	if approved && proposal.Version <= current.Version {
		return GoalContract{}, fmt.Errorf(
			"%w: approval requires a strictly increased goal version", ErrInvalidInput,
		)
	}
	now := runtime.now()
	contract := GoalContract{
		ContractID: state.ContractID, Version: proposal.Version,
		Goal: proposal.Goal, Deliverable: proposal.Deliverable,
		Constraints: proposal.Constraints, Verification: proposal.Verification,
		DoneCriteria: proposal.DoneCriteria, Plan: proposal.Plan,
		NextAction: proposal.NextAction, ApprovedBy: boundedText(approver),
		ApprovedAt: now, ProposalID: proposal.ID,
	}
	contract.Hash = GoalHash(contract)
	if _, _, err := runtime.work.PutContractWithWorkItems(
		ctx, actor, workContractInput(contract), workItemInputs(contract),
	); err != nil {
		return GoalContract{}, err
	}
	state.Proposals[index].Status = ProposalApproved
	state.Proposals[index].DecidedAt = &now
	state.Proposals[index].Decision = "explicitly approved by " + boundedText(approver)
	state.Approved = append(state.Approved, contract)
	if len(state.Approved) > maxApproved {
		return GoalContract{}, fmt.Errorf("%w: approved version ledger is full", ErrInvalidInput)
	}
	if state.OpenWorkItemID != "" && !planContains(contract.Plan, state.OpenWorkItemID) {
		state.OpenWorkItemID, state.PlanNodeID = "", ""
	}
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return GoalContract{}, err
	}
	return contract, nil
}

// RejectGoalRevision records an explicit rejection without any authority.
func (runtime *Runtime) RejectGoalRevision(
	ctx context.Context, actor, proposalID uuid.UUID, reason string,
) (GoalProposal, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if invalidText(reason) {
		return GoalProposal{}, fmt.Errorf("%w: rejection reason is required", ErrInvalidInput)
	}
	raw, err := runtime.rawState(ctx, actor)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return GoalProposal{}, fmt.Errorf("%w: no proposal exists", ErrNotFound)
		}
		return GoalProposal{}, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return GoalProposal{}, runtime.failClosed(ctx, actor, raw, &StateFailure{
			Reason: "durable continuation state is not decodable",
		})
	}
	index := -1
	for candidate := range state.Proposals {
		if state.Proposals[candidate].ID == proposalID {
			index = candidate
			break
		}
	}
	if index < 0 || state.Proposals[index].Status != ProposalProposed {
		return GoalProposal{}, fmt.Errorf("%w: open proposal not found", ErrNotFound)
	}
	now := runtime.now()
	state.Proposals[index].Status = ProposalRejected
	state.Proposals[index].DecidedAt = &now
	state.Proposals[index].Decision = boundedText(reason)
	saved, err := runtime.saveProposalState(ctx, actor, state)
	if err != nil {
		return GoalProposal{}, err
	}
	return saved.Proposals[index], nil
}

// ApprovedGoal returns the sole authoritative contract.
func (runtime *Runtime) ApprovedGoal(ctx context.Context, actor uuid.UUID) (GoalContract, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return GoalContract{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return GoalContract{}, ErrNoApprovedGoal
	}
	return contract, nil
}

// Snapshot returns the authoritative replay projection, including the
// deterministic next eligible work item.
func (runtime *Runtime) Snapshot(ctx context.Context, actor uuid.UUID) (Snapshot, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return Snapshot{}, err
	}
	return runtime.snapshot(ctx, actor, state)
}

func (runtime *Runtime) snapshot(
	ctx context.Context, actor uuid.UUID, state State,
) (Snapshot, error) {
	contract, ok := state.latest()
	if !ok {
		return Snapshot{}, ErrNoApprovedGoal
	}
	portfolio, err := runtime.work.Get(ctx, actor)
	if err != nil {
		return Snapshot{}, err
	}
	snapshot := Snapshot{
		Version: state.Version, Revision: state.Revision, Contract: contract,
		PlanNodeID: state.PlanNodeID, OpenWorkItemID: state.OpenWorkItemID,
		Frames: append([]RecoveryFrame(nil), state.Frames...),
		Lease:  cloneLease(state.Lease), HighestFencing: state.HighestFencing,
		Cursor: state.Cursor, NextWorkItemID: nextWorkItem(portfolio, state, contract),
	}
	if frame, active := state.innermost(); active {
		snapshot.ReturnTo = frame.ReturnTo
	}
	return snapshot, nil
}

// AcquireWorker issues the first lease and fencing identity for a goal.
func (runtime *Runtime) AcquireWorker(
	ctx context.Context, actor uuid.UUID, workerID string, ttl time.Duration,
) (WorkerLease, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return WorkerLease{}, err
	}
	lease, state, err := runtime.issueLease(state, workerID, ttl, "initial worker lease")
	if err != nil {
		return WorkerLease{}, err
	}
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return WorkerLease{}, err
	}
	return lease, nil
}

func (runtime *Runtime) issueLease(
	state State, workerID string, ttl time.Duration, reason string,
) (WorkerLease, State, error) {
	workerID = strings.TrimSpace(workerID)
	if workerID == "" || len(workerID) > 256 {
		return WorkerLease{}, state, fmt.Errorf("%w: worker identity is required", ErrInvalidInput)
	}
	if ttl <= 0 {
		ttl = defaultLeaseTTL
	}
	if ttl > maxLeaseTTL {
		return WorkerLease{}, state, fmt.Errorf("%w: lease duration is out of bounds", ErrInvalidInput)
	}
	now := runtime.now()
	for index := range state.Fencing {
		if state.Fencing[index].RetiredAt == nil {
			retired := now
			state.Fencing[index].RetiredAt = &retired
			state.Fencing[index].Reason = boundedText(reason)
		}
	}
	token := state.HighestFencing + 1
	lease := WorkerLease{
		WorkerID: workerID, FencingToken: token, IssuedAt: now, ExpiresAt: now.Add(ttl),
	}
	state.HighestFencing = token
	state.Lease = &lease
	state.Fencing = append(state.Fencing, FencingRecord{
		WorkerID: workerID, FencingToken: token, IssuedAt: now,
	})
	return lease, state, nil
}

// Continue restores authoritative continuation state under a fresh worker
// identity and invalidates prior worker authority before any next action.
func (runtime *Runtime) Continue(
	ctx context.Context, actor uuid.UUID, input ContinueInput,
) (Continuation, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if _, ok := knownCauses[input.Cause]; !ok {
		return Continuation{}, fmt.Errorf("%w: unsupported continuation cause", ErrInvalidInput)
	}
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return Continuation{}, err
	}
	lease, state, err := runtime.issueLease(
		state, input.WorkerID, input.LeaseTTL, "invalidated by "+string(input.Cause),
	)
	if err != nil {
		return Continuation{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return Continuation{}, ErrNoApprovedGoal
	}
	record := ContinuationRecord{
		Cause: input.Cause, At: runtime.now(), WorkerID: lease.WorkerID,
		Provider: boundedText(input.Provider), FencingToken: lease.FencingToken,
		RestoredFrames: len(state.activeFrames()), OpenWorkItemID: state.OpenWorkItemID,
		GoalHash: contract.Hash, GoalVersion: contract.Version,
	}
	state.Continuations = append(state.Continuations, record)
	saved, err := runtime.save(ctx, actor, state)
	if err != nil {
		return Continuation{}, err
	}
	snapshot, err := runtime.snapshot(ctx, actor, saved)
	if err != nil {
		return Continuation{}, err
	}
	return Continuation{Snapshot: snapshot, Lease: lease, Record: record}, nil
}

// OpenWorkItem opens the deterministic next eligible plan node as the single
// open work item and transitions it through the production work service.
func (runtime *Runtime) OpenWorkItem(
	ctx context.Context, actor uuid.UUID, workerID string, token uint64, itemID string,
) (work.WorkItem, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return work.WorkItem{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return work.WorkItem{}, ErrNoApprovedGoal
	}
	if err := runtime.checkLease(state, workerID, token); err != nil {
		return work.WorkItem{}, err
	}
	if len(state.activeFrames()) > 0 {
		return work.WorkItem{}, fmt.Errorf(
			"%w: an active recovery frame already owns the open work item", ErrFrameActive,
		)
	}
	itemID = strings.TrimSpace(itemID)
	portfolio, err := runtime.work.Get(ctx, actor)
	if err != nil {
		return work.WorkItem{}, err
	}
	if itemID == "" || itemID != nextWorkItem(portfolio, state, contract) {
		return work.WorkItem{}, fmt.Errorf(
			"%w: only the next eligible plan node can be opened", ErrInvalidInput,
		)
	}
	item, err := runtime.work.UpdateWorkItem(ctx, actor, work.WorkItemUpdate{
		ContractID: state.ContractID, ItemID: itemID, Status: work.WorkItemRunning,
	})
	if err != nil {
		return work.WorkItem{}, err
	}
	state.OpenWorkItemID, state.PlanNodeID = itemID, itemID
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return work.WorkItem{}, err
	}
	return item, nil
}

func (runtime *Runtime) checkLease(state State, workerID string, token uint64) error {
	workerID = strings.TrimSpace(workerID)
	if state.Lease == nil {
		return fmt.Errorf("%w: no worker lease exists", ErrStaleWorker)
	}
	if state.Lease.WorkerID != workerID || state.Lease.FencingToken != token {
		return fmt.Errorf("%w: worker %q token %d is not current", ErrStaleWorker, workerID, token)
	}
	if !runtime.now().Before(state.Lease.ExpiresAt) {
		return fmt.Errorf("%w: worker lease expired", ErrStaleWorker)
	}
	return nil
}

// Authorize validates one complete action envelope before any effect. Rejected
// envelopes produce zero externally observable effects.
func (runtime *Runtime) Authorize(
	ctx context.Context, actor uuid.UUID, envelope ActionEnvelope,
) (Authorization, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return Authorization{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return Authorization{}, ErrNoApprovedGoal
	}
	if err := runtime.checkLease(state, envelope.WorkerID, envelope.FencingToken); err != nil {
		return Authorization{}, err
	}
	if envelope.GoalHash != contract.Hash || envelope.GoalVersion != contract.Version {
		return Authorization{}, fmt.Errorf(
			"%w: envelope carries goal %s v%d", ErrGoalMismatch,
			truncateHash(envelope.GoalHash), envelope.GoalVersion,
		)
	}
	if envelope.Kind != ActionTool && envelope.Kind != ActionModel {
		return Authorization{}, fmt.Errorf("%w: unsupported action kind", ErrInvalidInput)
	}
	if invalidText(envelope.OperationID) || invalidText(envelope.IdempotencyKey) {
		return Authorization{}, fmt.Errorf(
			"%w: operation and idempotency identity are required", ErrInvalidInput,
		)
	}
	if envelope.Cost.ToolInvocations < 0 || envelope.Cost.ModelTokens < 0 {
		return Authorization{}, fmt.Errorf("%w: action cost cannot be negative", ErrInvalidInput)
	}
	for _, action := range state.Actions {
		if action.IdempotencyKey == envelope.IdempotencyKey {
			return Authorization{}, fmt.Errorf(
				"%w: idempotency key %q already authorized", ErrDuplicateAction,
				envelope.IdempotencyKey,
			)
		}
	}
	for _, key := range state.RetiredKeys {
		if key == envelope.IdempotencyKey {
			return Authorization{}, fmt.Errorf(
				"%w: idempotency key %q already settled", ErrDuplicateAction,
				envelope.IdempotencyKey,
			)
		}
	}
	frame, hasFrame := state.innermost()
	var parentCriteria []string
	switch {
	case hasFrame:
		if envelope.FrameID != frame.ID || envelope.WorkItemID != "" {
			return Authorization{}, fmt.Errorf(
				"%w: the innermost active RecoveryFrame is the only parent", ErrParent,
			)
		}
		parentCriteria = frame.Exit.Criteria
	default:
		if envelope.FrameID != uuid.Nil {
			return Authorization{}, fmt.Errorf("%w: no RecoveryFrame is active", ErrParent)
		}
		if state.OpenWorkItemID == "" || envelope.WorkItemID != state.OpenWorkItemID {
			return Authorization{}, fmt.Errorf(
				"%w: exactly one open work item must parent the action", ErrParent,
			)
		}
		portfolio, err := runtime.work.Get(ctx, actor)
		if err != nil {
			return Authorization{}, err
		}
		item, found := findWorkItem(portfolio, state.ContractID, state.OpenWorkItemID)
		if !found || item.Status == work.WorkItemCompleted ||
			item.Status == work.WorkItemBlocked {
			return Authorization{}, fmt.Errorf(
				"%w: the declared parent work item is not open", ErrParent,
			)
		}
		parentCriteria = item.Criteria
	}
	if invalidText(envelope.Expected.Description) || len(envelope.Expected.Criteria) == 0 {
		return Authorization{}, fmt.Errorf(
			"%w: describe the observable evidence delta and its criteria", ErrEvidenceDelta,
		)
	}
	for _, criterion := range trimUnique(envelope.Expected.Criteria) {
		if !containsString(parentCriteria, criterion) {
			return Authorization{}, fmt.Errorf(
				"%w: criterion %q is not correlated to this parent", ErrEvidenceDelta, criterion,
			)
		}
	}
	if hasFrame {
		if envelope.Strategy == "" || !containsStrategy(frame.Allowlist, envelope.Strategy) {
			return Authorization{}, fmt.Errorf(
				"%w: strategy %q is outside the frame allowlist", ErrStrategy, envelope.Strategy,
			)
		}
		closures, exhausted := runtime.debitFrames(&state, frame.ID, envelope)
		if exhausted != nil {
			if _, err := runtime.save(ctx, actor, state); err != nil {
				return Authorization{}, err
			}
			if err := runtime.applyDispositions(ctx, actor, closures); err != nil {
				return Authorization{}, err
			}
			return Authorization{}, exhausted
		}
	}
	now := runtime.now()
	record := ActionRecord{
		ID: uuid.New(), GoalHash: contract.Hash, GoalVersion: contract.Version,
		Kind: envelope.Kind, Strategy: envelope.Strategy,
		OperationID:    boundedText(envelope.OperationID),
		IdempotencyKey: boundedText(envelope.IdempotencyKey), Cost: envelope.Cost,
		Expected: EvidenceDelta{
			Description: boundedText(envelope.Expected.Description),
			Criteria:    trimUnique(envelope.Expected.Criteria),
		},
		WorkerID: state.Lease.WorkerID, FencingToken: state.Lease.FencingToken,
		AuthorizedAt: now,
	}
	if hasFrame {
		frameID := frame.ID
		record.FrameID = &frameID
	} else {
		record.WorkItemID = state.OpenWorkItemID
	}
	state.Actions = append(state.Actions, record)
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return Authorization{}, err
	}
	return Authorization{
		ID: record.ID, GoalHash: record.GoalHash, GoalVersion: record.GoalVersion,
		WorkItemID: record.WorkItemID, FrameID: record.FrameID,
		WorkerID: record.WorkerID, FencingToken: record.FencingToken, AuthorizedAt: now,
	}, nil
}

// debitFrames atomically charges one action to the innermost frame and every
// ancestor, closing exhausted frames instead of permitting the action.
func (runtime *Runtime) debitFrames(
	state *State, frameID uuid.UUID, envelope ActionEnvelope,
) ([]FrameClosure, error) {
	chain := activeFrameChain(*state, frameID)
	now := runtime.now()
	offender, offense := -1, ""
	for _, index := range chain {
		frame := state.Frames[index]
		remaining := frame.Remaining(now)
		reason := ""
		switch {
		case remaining.ElapsedSeconds <= 0:
			reason = "elapsed seconds"
		case envelope.Cost.ToolInvocations > remaining.ToolInvocations:
			reason = "tool invocations"
		case envelope.Cost.ModelTokens > remaining.ModelTokens:
			reason = "model tokens"
		case envelope.Retry && frame.retries(envelope.OperationID)+1 > frame.Budget.RetriesPerOperation:
			reason = "retries for one failed operation"
		}
		if reason == "" {
			continue
		}
		if offender < 0 || frame.Depth < state.Frames[offender].Depth {
			offender, offense = index, reason
		}
	}
	if offender >= 0 {
		closures := runtime.closeFrameCascade(
			state, offender, FrameClosedExhaust, "budget exhausted: "+offense,
		)
		return closures, &ExhaustionError{Closures: closures, Reason: offense}
	}
	for _, index := range chain {
		frame := &state.Frames[index]
		frame.Consumed.ToolInvocations += envelope.Cost.ToolInvocations
		frame.Consumed.ModelTokens += envelope.Cost.ModelTokens
		if envelope.Retry {
			frame.Consumed.Retries = bumpRetry(frame.Consumed.Retries, envelope.OperationID)
		}
		if index == state.frameIndex(frameID) &&
			!containsStrategy(frame.Attempted, envelope.Strategy) && envelope.Strategy != "" {
			frame.Attempted = append(frame.Attempted, envelope.Strategy)
		}
	}
	return nil, nil
}

// SettleAction records the honest outcome of one authorized action and applies
// error budgets to the owning frame chain.
func (runtime *Runtime) SettleAction(
	ctx context.Context, actor, authorizationID uuid.UUID, outcome ActionOutcome,
) (Settlement, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return Settlement{}, err
	}
	index := -1
	for candidate := range state.Actions {
		if state.Actions[candidate].ID == authorizationID {
			index = candidate
			break
		}
	}
	if index < 0 {
		return Settlement{}, fmt.Errorf("%w: authorization not found", ErrNotFound)
	}
	if state.Actions[index].Settled {
		return Settlement{}, fmt.Errorf(
			"%w: authorization %s is already settled", ErrDuplicateAction, authorizationID,
		)
	}
	if !outcome.Succeeded && invalidText(outcome.Error) {
		return Settlement{}, fmt.Errorf("%w: failed actions require an error", ErrInvalidInput)
	}
	now := runtime.now()
	action := &state.Actions[index]
	action.Settled, action.Succeeded, action.SettledAt = true, outcome.Succeeded, &now
	action.Error = boundedText(outcome.Error)
	var closures []FrameClosure
	if action.FrameID != nil {
		chain := activeFrameChain(state, *action.FrameID)
		for _, frameIndex := range chain {
			frame := &state.Frames[frameIndex]
			if outcome.Succeeded {
				frame.Consumed.SubsequentErrors = 0
				continue
			}
			frame.Consumed.SubsequentErrors++
		}
		if outcome.ModelTokens > 0 {
			for _, frameIndex := range chain {
				frame := &state.Frames[frameIndex]
				frame.Consumed.ModelTokens = minInt64(
					frame.Consumed.ModelTokens+outcome.ModelTokens, frame.Budget.ModelTokens,
				)
			}
		}
		if !outcome.Succeeded {
			shallowest := -1
			for _, frameIndex := range chain {
				if state.Frames[frameIndex].Consumed.SubsequentErrors <
					state.Frames[frameIndex].Budget.SubsequentErrors {
					continue
				}
				if shallowest < 0 ||
					state.Frames[frameIndex].Depth < state.Frames[shallowest].Depth {
					shallowest = frameIndex
				}
			}
			if shallowest >= 0 {
				closures = runtime.closeFrameCascade(
					&state, shallowest, FrameClosedExhaust,
					"budget exhausted: subsequent errors",
				)
			}
		}
	}
	saved, err := runtime.save(ctx, actor, state)
	if err != nil {
		return Settlement{}, err
	}
	if err := runtime.applyDispositions(ctx, actor, closures); err != nil {
		return Settlement{}, err
	}
	settlement := Settlement{Record: saved.Actions[index], ClosedFrames: closures}
	if frame, active := saved.innermost(); active {
		id := frame.ID
		settlement.ActiveFrameID = &id
	}
	return settlement, nil
}

// OpenRecoveryFrame creates exactly one non-promotable frame for the open work
// item, or one nested frame inside the innermost active frame.
func (runtime *Runtime) OpenRecoveryFrame(
	ctx context.Context, actor uuid.UUID, input OpenFrameInput,
) (RecoveryFrame, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return RecoveryFrame{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return RecoveryFrame{}, ErrNoApprovedGoal
	}
	if err := runtime.checkLease(state, input.WorkerID, input.FencingToken); err != nil {
		return RecoveryFrame{}, err
	}
	if input.GoalHash != contract.Hash || input.GoalVersion != contract.Version {
		return RecoveryFrame{}, fmt.Errorf("%w: frame is not bound to the approved goal", ErrGoalMismatch)
	}
	itemID := strings.TrimSpace(input.WorkItemID)
	if itemID == "" || state.OpenWorkItemID == "" || itemID != state.OpenWorkItemID {
		return RecoveryFrame{}, fmt.Errorf(
			"%w: a recovery frame must attach to the open original work item", ErrParent,
		)
	}
	if invalidText(input.Cause) || invalidText(input.Exit.Description) ||
		len(input.Exit.Criteria) == 0 {
		return RecoveryFrame{}, fmt.Errorf(
			"%w: cause and evidence-verifiable exit condition are required", ErrInvalidInput,
		)
	}
	if input.Disposition != DispositionBlocked && input.Disposition != DispositionEscalated {
		return RecoveryFrame{}, fmt.Errorf("%w: declare a blocked or escalated disposition", ErrInvalidInput)
	}
	allowlist := uniqueStrategies(input.Allowlist)
	if len(allowlist) == 0 || len(allowlist) > maxStrategies {
		return RecoveryFrame{}, fmt.Errorf("%w: declare an allowlist of strategy changes", ErrInvalidInput)
	}
	for _, strategy := range allowlist {
		if _, known := knownStrategies[strategy]; !known {
			return RecoveryFrame{}, fmt.Errorf(
				"%w: strategy %q is not an execution-strategy change", ErrStrategy, strategy,
			)
		}
	}
	approved := make(map[string]struct{}, len(contract.DoneCriteria))
	for _, criterion := range contract.DoneCriteria {
		approved[criterion.ID] = struct{}{}
	}
	exitCriteria := trimUnique(input.Exit.Criteria)
	for _, criterion := range exitCriteria {
		if _, ok := approved[criterion]; !ok {
			return RecoveryFrame{}, fmt.Errorf(
				"%w: exit criterion %q is not an approved done criterion", ErrInvalidInput, criterion,
			)
		}
	}
	now := runtime.now()
	budget := boundedBudget(input.Budget)
	active := state.activeFrames()
	depth := len(active) + 1
	if depth > MaxActiveFrames {
		return RecoveryFrame{}, fmt.Errorf(
			"%w: %d frames are already active", ErrFrameDepth, len(active),
		)
	}
	frame := RecoveryFrame{
		ID: uuid.New(), Depth: depth, OriginalWorkItemID: itemID, ReturnTo: itemID,
		Cause: boundedText(input.Cause),
		Exit: ExitCondition{
			Description: boundedText(input.Exit.Description), Criteria: exitCriteria,
		},
		Disposition: input.Disposition, Allowlist: allowlist, Budget: budget,
		Status: FrameActive, OpenedAt: now,
	}
	if depth > 1 {
		parent := active[len(active)-1]
		if parent.OriginalWorkItemID != itemID || parent.ReturnTo != itemID {
			return RecoveryFrame{}, fmt.Errorf(
				"%w: nested frame must share the original work item", ErrParent,
			)
		}
		remaining := parent.Remaining(now)
		if remaining.ToolInvocations <= 0 || remaining.ModelTokens <= 0 ||
			remaining.ElapsedSeconds <= 0 || remaining.SubsequentErrors <= 0 {
			return RecoveryFrame{}, &ExhaustionError{
				Reason: "parent frame has no remaining budget for a nested frame",
			}
		}
		frame.Budget = clampToRemaining(budget, remaining)
		parentID := parent.ID
		frame.ParentFrameID = &parentID
		frame.ReturnTo = parent.ReturnTo
	}
	if len(state.Frames) >= maxFrames {
		return RecoveryFrame{}, fmt.Errorf("%w: recovery frame ledger is full", ErrInvalidInput)
	}
	state.Frames = append(state.Frames, frame)
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return RecoveryFrame{}, err
	}
	return frame, nil
}

// VerifyFrameExit closes only the innermost active frame when its exit
// condition is verified by current-goal-correlated evidence.
func (runtime *Runtime) VerifyFrameExit(
	ctx context.Context, actor, frameID uuid.UUID,
) (FrameClosure, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return FrameClosure{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return FrameClosure{}, ErrNoApprovedGoal
	}
	frame, active := state.innermost()
	if !active || frame.ID != frameID {
		return FrameClosure{}, fmt.Errorf("%w: only the innermost active frame can close", ErrFrameClosed)
	}
	covered := coverage(state, contract)
	for _, criterion := range frame.Exit.Criteria {
		if _, ok := covered[criterion]; !ok {
			return FrameClosure{}, fmt.Errorf(
				"%w: criterion %q has no current-goal server-verified evidence",
				ErrExitUnverified, criterion,
			)
		}
	}
	index := state.frameIndex(frameID)
	now := runtime.now()
	state.Frames[index].Status = FrameClosedVerified
	state.Frames[index].ClosedAt = &now
	state.Frames[index].ClosureReason = "exit condition verified by server-verified evidence"
	closure := FrameClosure{
		FrameID: frame.ID, Depth: frame.Depth, Status: FrameClosedVerified,
		OriginalWorkItemID: frame.OriginalWorkItemID, ReturnTo: frame.ReturnTo,
		Consumed: state.Frames[index].Consumed,
		Reason:   state.Frames[index].ClosureReason,
	}
	if parent, ok := state.innermost(); ok {
		id := parent.ID
		closure.ResumedFrameID = &id
	} else {
		closure.ResumedWorkItemID = frame.ReturnTo
		state.OpenWorkItemID, state.PlanNodeID = frame.ReturnTo, frame.ReturnTo
	}
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return FrameClosure{}, err
	}
	return closure, nil
}

// FailRecoveryFrame closes a frame at its declared terminal failure condition
// and applies the declared disposition to the original work item.
func (runtime *Runtime) FailRecoveryFrame(
	ctx context.Context, actor, frameID uuid.UUID, reason string,
) (FrameClosure, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if invalidText(reason) {
		return FrameClosure{}, fmt.Errorf("%w: terminal failure reason is required", ErrInvalidInput)
	}
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return FrameClosure{}, err
	}
	frame, active := state.innermost()
	if !active || frame.ID != frameID {
		return FrameClosure{}, fmt.Errorf("%w: only the innermost active frame can close", ErrFrameClosed)
	}
	closures := runtime.closeFrameCascade(
		&state, state.frameIndex(frameID), FrameClosedFailed,
		"terminal failure: "+boundedText(reason),
	)
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return FrameClosure{}, err
	}
	if err := runtime.applyDispositions(ctx, actor, closures); err != nil {
		return FrameClosure{}, err
	}
	return closures[0], nil
}

func (runtime *Runtime) closeFrameCascade(
	state *State, index int, status FrameStatus, reason string,
) []FrameClosure {
	now := runtime.now()
	depth := state.Frames[index].Depth
	closures := make([]FrameClosure, 0, MaxActiveFrames)
	for candidate := len(state.Frames) - 1; candidate >= 0; candidate-- {
		frame := &state.Frames[candidate]
		if frame.Status != FrameActive || frame.Depth < depth {
			continue
		}
		frame.Status = status
		frame.ClosedAt = &now
		frame.ClosureReason = boundedText(reason)
		frame.UnmetExit = frame.Exit.Description
		frame.AppliedDisposition = frame.Disposition
		closures = append(closures, FrameClosure{
			FrameID: frame.ID, Depth: frame.Depth, Status: status,
			OriginalWorkItemID: frame.OriginalWorkItemID, ReturnTo: frame.ReturnTo,
			UnmetExit: frame.Exit.Description, Attempted: frame.Attempted,
			Consumed: frame.Consumed, Disposition: frame.Disposition, Reason: frame.ClosureReason,
		})
	}
	for _, closure := range closures {
		state.Dispositions = append(state.Dispositions, DispositionRecord{
			WorkItemID: closure.OriginalWorkItemID, FrameID: closure.FrameID,
			Disposition: closure.Disposition, Reason: closure.Reason,
			UnmetExit: closure.UnmetExit, Attempted: closure.Attempted,
			Consumed: closure.Consumed, At: now,
		})
	}
	if len(state.Dispositions) > maxContinuations {
		state.Dispositions = state.Dispositions[len(state.Dispositions)-maxContinuations:]
	}
	return closures
}

// applyDispositions writes the honest blocked or escalated parent outcome
// through the production work service.
func (runtime *Runtime) applyDispositions(
	ctx context.Context, actor uuid.UUID, closures []FrameClosure,
) error {
	if len(closures) == 0 {
		return nil
	}
	disposition, itemID, reason, unmet := DispositionBlocked, "", "", ""
	for _, closure := range closures {
		itemID, reason, unmet = closure.OriginalWorkItemID, closure.Reason, closure.UnmetExit
		if closure.Disposition == DispositionEscalated {
			disposition = DispositionEscalated
		}
	}
	if itemID == "" {
		return nil
	}
	state, err := runtime.loadUnlocked(ctx, actor)
	if err != nil {
		return err
	}
	note := fmt.Sprintf(
		"recovery closed (%s); unmet exit condition: %s", reason, unmet,
	)
	if disposition == DispositionEscalated {
		note = "escalated for human decision: " + note
	}
	if _, err := runtime.work.UpdateWorkItem(ctx, actor, work.WorkItemUpdate{
		ContractID: state.ContractID, ItemID: itemID,
		Status: work.WorkItemBlocked, Note: boundedText(note),
	}); err != nil {
		return err
	}
	return nil
}

func (runtime *Runtime) loadUnlocked(ctx context.Context, actor uuid.UUID) (State, error) {
	raw, err := runtime.rawState(ctx, actor)
	if err != nil {
		return State{}, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return State{}, &StateFailure{Reason: "durable continuation state is not decodable"}
	}
	return state, nil
}

// VerifyEvidence records a workspace artifact, has the server hash it, and
// correlates it to the current approved goal hash and version.
func (runtime *Runtime) VerifyEvidence(
	ctx context.Context, actor uuid.UUID, input EvidenceInput,
) (EvidenceRecord, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return EvidenceRecord{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return EvidenceRecord{}, ErrNoApprovedGoal
	}
	if err := runtime.checkLease(state, input.WorkerID, input.FencingToken); err != nil {
		return EvidenceRecord{}, err
	}
	if input.Scope != ScopeCriterion && input.Scope != ScopeRecoveryStrategy {
		return EvidenceRecord{}, fmt.Errorf("%w: declare the evidence scope", ErrInvalidInput)
	}
	criteria := trimUnique(input.Criteria)
	if len(criteria) == 0 || len(criteria) > maxCriteria {
		return EvidenceRecord{}, fmt.Errorf("%w: evidence must cover criteria", ErrInvalidInput)
	}
	approved := make(map[string]struct{}, len(contract.DoneCriteria))
	for _, criterion := range contract.DoneCriteria {
		approved[criterion.ID] = struct{}{}
	}
	for _, criterion := range criteria {
		if _, ok := approved[criterion]; !ok {
			return EvidenceRecord{}, fmt.Errorf(
				"%w: criterion %q is not approved", ErrInvalidInput, criterion,
			)
		}
	}
	record := EvidenceRecord{
		Scope: input.Scope, Criteria: criteria, GoalHash: contract.Hash,
		GoalVersion: contract.Version, Verification: "server_sha256",
		VerifiedAt: runtime.now(), WorkerID: state.Lease.WorkerID,
		FencingToken: state.Lease.FencingToken,
	}
	switch input.Scope {
	case ScopeCriterion:
		artifact, err := runtime.work.RecordArtifact(ctx, actor, work.ArtifactInput{
			ContractID: state.ContractID, Kind: strings.TrimSpace(input.Kind),
			Title: strings.TrimSpace(input.Title), Reference: strings.TrimSpace(input.Reference),
			CriteriaCovered: criteria,
		})
		if err != nil {
			return EvidenceRecord{}, err
		}
		verified, err := runtime.work.VerifyArtifact(ctx, actor, artifact.ID)
		if err != nil {
			return EvidenceRecord{}, err
		}
		if verified.VerifiedAt == nil || verified.Verification != "server_sha256" ||
			len(verified.SHA256) != 64 {
			return EvidenceRecord{}, fmt.Errorf(
				"%w: artifact was not server verified", ErrEvidenceMissing,
			)
		}
		record.ArtifactID, record.SHA256 = verified.ID, verified.SHA256
		record.SizeBytes, record.VerifiedAt = verified.SizeBytes, *verified.VerifiedAt
	default:
		digest, size, err := runtime.work.HashWorkspaceFile(ctx, strings.TrimSpace(input.Reference))
		if err != nil {
			return EvidenceRecord{}, err
		}
		record.SHA256, record.SizeBytes = digest, size
	}
	if input.FrameID != uuid.Nil {
		if state.frameIndex(input.FrameID) < 0 {
			return EvidenceRecord{}, fmt.Errorf("%w: frame not found", ErrNotFound)
		}
		frameID := input.FrameID
		record.FrameID = &frameID
	}
	state.Evidence = append(state.Evidence, record)
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return EvidenceRecord{}, err
	}
	return record, nil
}

// CompleteWorkItem transitions the open original work item to completed only
// from current-goal-correlated server-verified evidence.
func (runtime *Runtime) CompleteWorkItem(
	ctx context.Context, actor uuid.UUID, itemID string,
) (work.WorkItem, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return work.WorkItem{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return work.WorkItem{}, ErrNoApprovedGoal
	}
	itemID = strings.TrimSpace(itemID)
	if itemID == "" || itemID != state.OpenWorkItemID {
		return work.WorkItem{}, fmt.Errorf("%w: only the open work item can complete", ErrParent)
	}
	if len(state.activeFrames()) > 0 {
		return work.WorkItem{}, fmt.Errorf(
			"%w: close recovery before completing the work item", ErrFrameActive,
		)
	}
	portfolio, err := runtime.work.Get(ctx, actor)
	if err != nil {
		return work.WorkItem{}, err
	}
	item, found := findWorkItem(portfolio, state.ContractID, itemID)
	if !found {
		return work.WorkItem{}, fmt.Errorf("%w: work item not found", ErrNotFound)
	}
	covered := coverage(state, contract)
	for _, criterion := range item.Criteria {
		if _, ok := covered[criterion]; !ok {
			return work.WorkItem{}, fmt.Errorf(
				"%w: criterion %q lacks current-goal verified evidence", ErrEvidenceMissing, criterion,
			)
		}
	}
	completed, err := runtime.work.UpdateWorkItem(ctx, actor, work.WorkItemUpdate{
		ContractID: state.ContractID, ItemID: itemID, Status: work.WorkItemCompleted,
	})
	if err != nil {
		return work.WorkItem{}, err
	}
	state.OpenWorkItemID = ""
	if _, err := runtime.save(ctx, actor, state); err != nil {
		return work.WorkItem{}, err
	}
	return completed, nil
}

// CompleteGoal closes the approved contract only when every original approved
// criterion has current-goal-correlated server-verified evidence.
func (runtime *Runtime) CompleteGoal(
	ctx context.Context, actor uuid.UUID,
) (work.OutcomeContract, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return work.OutcomeContract{}, err
	}
	contract, ok := state.latest()
	if !ok {
		return work.OutcomeContract{}, ErrNoApprovedGoal
	}
	if len(state.activeFrames()) > 0 {
		return work.OutcomeContract{}, fmt.Errorf(
			"%w: recovery must close before completion", ErrFrameActive,
		)
	}
	covered := coverage(state, contract)
	for _, criterion := range contract.DoneCriteria {
		if _, ok := covered[criterion.ID]; !ok {
			return work.OutcomeContract{}, fmt.Errorf(
				"%w: criterion %q lacks current-goal verified evidence",
				ErrEvidenceMissing, criterion.ID,
			)
		}
	}
	return runtime.work.CompleteContract(ctx, actor, state.ContractID)
}

// ApprovedVersions exposes the immutable approved version ledger.
func (runtime *Runtime) ApprovedVersions(
	ctx context.Context, actor uuid.UUID,
) ([]GoalContract, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return nil, err
	}
	return append([]GoalContract(nil), state.Approved...), nil
}

// Actions exposes the durable authorization ledger.
func (runtime *Runtime) Actions(ctx context.Context, actor uuid.UUID) ([]ActionRecord, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return nil, err
	}
	return append([]ActionRecord(nil), state.Actions...), nil
}

// Frames exposes the ordered RecoveryFrame ledger.
func (runtime *Runtime) Frames(ctx context.Context, actor uuid.UUID) ([]RecoveryFrame, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return nil, err
	}
	return append([]RecoveryFrame(nil), state.Frames...), nil
}

// Dispositions exposes the honest recovery outcomes recorded for an actor.
func (runtime *Runtime) Dispositions(
	ctx context.Context, actor uuid.UUID,
) ([]DispositionRecord, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return nil, err
	}
	return append([]DispositionRecord(nil), state.Dispositions...), nil
}

// Continuations exposes the durable restoration audit for an actor.
func (runtime *Runtime) Continuations(
	ctx context.Context, actor uuid.UUID,
) ([]ContinuationRecord, error) {
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	state, err := runtime.load(ctx, actor)
	if err != nil {
		return nil, err
	}
	return append([]ContinuationRecord(nil), state.Continuations...), nil
}

func coverage(state State, contract GoalContract) map[string]struct{} {
	covered := make(map[string]struct{})
	for _, record := range state.Evidence {
		if record.Scope != ScopeCriterion || record.GoalHash != contract.Hash ||
			record.GoalVersion != contract.Version ||
			record.Verification != "server_sha256" || len(record.SHA256) != 64 {
			continue
		}
		for _, criterion := range record.Criteria {
			covered[criterion] = struct{}{}
		}
	}
	return covered
}

func activeFrameChain(state State, frameID uuid.UUID) []int {
	chain := make([]int, 0, MaxActiveFrames)
	for _, index := range frameChain(state, frameID) {
		if state.Frames[index].Status == FrameActive {
			chain = append(chain, index)
		}
	}
	return chain
}

func frameChain(state State, frameID uuid.UUID) []int {
	chain := make([]int, 0, MaxActiveFrames)
	current := frameID
	for depth := 0; depth < MaxActiveFrames+1; depth++ {
		index := -1
		for candidate := range state.Frames {
			if state.Frames[candidate].ID == current {
				index = candidate
				break
			}
		}
		if index < 0 {
			break
		}
		chain = append(chain, index)
		if state.Frames[index].ParentFrameID == nil {
			break
		}
		current = *state.Frames[index].ParentFrameID
	}
	return chain
}

func bumpRetry(retries []OperationRetry, operationID string) []OperationRetry {
	for index := range retries {
		if retries[index].OperationID == operationID {
			retries[index].Count++
			return retries
		}
	}
	return append(retries, OperationRetry{OperationID: operationID, Count: 1})
}

func nextWorkItem(portfolio work.Portfolio, state State, contract GoalContract) string {
	if frame, active := state.innermost(); active {
		return frame.ReturnTo
	}
	if state.OpenWorkItemID != "" {
		if item, found := findWorkItem(portfolio, state.ContractID, state.OpenWorkItemID); found &&
			item.Status != work.WorkItemCompleted {
			return state.OpenWorkItemID
		}
	}
	status := make(map[string]work.WorkItemStatus, len(portfolio.WorkItems))
	items := make(map[string]work.WorkItem, len(portfolio.WorkItems))
	for _, item := range portfolio.WorkItems {
		if item.ContractID != state.ContractID {
			continue
		}
		status[item.ID], items[item.ID] = item.Status, item
	}
	order := make([]string, 0, len(contract.Plan))
	for _, node := range contract.Plan {
		order = append(order, node.ID)
	}
	remaining := make([]string, 0, len(items))
	for id := range items {
		if !containsString(order, id) {
			remaining = append(remaining, id)
		}
	}
	sort.Strings(remaining)
	order = append(order, remaining...)
	for _, id := range order {
		item, found := items[id]
		if !found || item.Status == work.WorkItemCompleted {
			continue
		}
		ready := true
		for _, dependency := range item.DependsOn {
			if status[dependency] != work.WorkItemCompleted {
				ready = false
				break
			}
		}
		if ready {
			return id
		}
	}
	return ""
}

func findWorkItem(
	portfolio work.Portfolio, contractID uuid.UUID, itemID string,
) (work.WorkItem, bool) {
	for _, item := range portfolio.WorkItems {
		if item.ContractID == contractID && item.ID == itemID {
			return item, true
		}
	}
	return work.WorkItem{}, false
}

func workContractInput(contract GoalContract) work.ContractInput {
	criteria := make([]work.Criterion, 0, len(contract.DoneCriteria))
	for _, criterion := range contract.DoneCriteria {
		criteria = append(criteria, work.Criterion{
			ID: criterion.ID, Description: criterion.Description,
		})
	}
	return work.ContractInput{
		ID: contract.ContractID, Goal: contract.Goal, Deliverable: contract.Deliverable,
		DoneCriteria: criteria, VerificationRequired: contract.Verification,
		NextAction: contract.NextAction, Status: work.StatusActive,
	}
}

func workItemInputs(contract GoalContract) []work.WorkItemInput {
	items := make([]work.WorkItemInput, 0, len(contract.Plan))
	for _, node := range contract.Plan {
		items = append(items, work.WorkItemInput{
			ID: node.ID, Title: node.Title, Criteria: node.Criteria, DependsOn: node.DependsOn,
		})
	}
	return items
}

func planContains(plan []PlanNode, id string) bool {
	for _, node := range plan {
		if node.ID == id {
			return true
		}
	}
	return false
}

func containsString(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func containsStrategy(values []Strategy, wanted Strategy) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func cloneLease(lease *WorkerLease) *WorkerLease {
	if lease == nil {
		return nil
	}
	cloned := *lease
	return &cloned
}

func normalizeCriteria(criteria []Criterion) []Criterion {
	result := make([]Criterion, 0, len(criteria))
	for _, criterion := range criteria {
		result = append(result, Criterion{
			ID: strings.TrimSpace(criterion.ID), Description: strings.TrimSpace(criterion.Description),
		})
	}
	return result
}

func normalizePlan(plan []PlanNode) []PlanNode {
	result := make([]PlanNode, 0, len(plan))
	for _, node := range plan {
		result = append(result, PlanNode{
			ID: strings.TrimSpace(node.ID), Title: strings.TrimSpace(node.Title),
			Criteria: trimUnique(node.Criteria), DependsOn: trimUnique(node.DependsOn),
		})
	}
	return result
}

func validateProposalInput(input ProposalInput) error {
	if invalidText(input.Goal) || invalidText(input.Deliverable) ||
		invalidText(input.NextAction) || invalidText(input.Rationale) ||
		invalidText(input.Origin) {
		return fmt.Errorf(
			"%w: goal, deliverable, next action, rationale, and origin are required", ErrInvalidInput,
		)
	}
	criteria := normalizeCriteria(input.DoneCriteria)
	if len(criteria) == 0 || len(criteria) > maxCriteria {
		return fmt.Errorf("%w: bounded done criteria are required", ErrInvalidInput)
	}
	known := make(map[string]struct{}, len(criteria))
	for _, criterion := range criteria {
		if criterion.ID == "" || invalidText(criterion.Description) {
			return fmt.Errorf("%w: criterion identity and description are required", ErrInvalidInput)
		}
		if _, exists := known[criterion.ID]; exists {
			return fmt.Errorf("%w: criterion identities must be unique", ErrInvalidInput)
		}
		known[criterion.ID] = struct{}{}
	}
	plan := normalizePlan(input.Plan)
	if len(plan) == 0 || len(plan) > maxPlanNodes {
		return fmt.Errorf("%w: a bounded plan is required", ErrInvalidInput)
	}
	nodes := make(map[string]struct{}, len(plan))
	for _, node := range plan {
		if node.ID == "" || invalidText(node.Title) || len(node.Criteria) == 0 {
			return fmt.Errorf("%w: plan node identity, title, and criteria are required", ErrInvalidInput)
		}
		if _, exists := nodes[node.ID]; exists {
			return fmt.Errorf("%w: plan node identities must be unique", ErrInvalidInput)
		}
		nodes[node.ID] = struct{}{}
		for _, criterion := range node.Criteria {
			if _, ok := known[criterion]; !ok {
				return fmt.Errorf("%w: plan node cites unknown criterion %q", ErrInvalidInput, criterion)
			}
		}
	}
	for _, node := range plan {
		for _, dependency := range node.DependsOn {
			if dependency == node.ID {
				return fmt.Errorf("%w: plan node cannot depend on itself", ErrInvalidInput)
			}
			if _, ok := nodes[dependency]; !ok {
				return fmt.Errorf("%w: plan node cites unknown dependency %q", ErrInvalidInput, dependency)
			}
		}
	}
	if len(input.Verification) == 0 || len(input.Verification) > maxCriteria {
		return fmt.Errorf("%w: bounded verification requirements are required", ErrInvalidInput)
	}
	covered := make(map[string]struct{}, len(known))
	for _, node := range plan {
		for _, criterion := range node.Criteria {
			covered[criterion] = struct{}{}
		}
	}
	for id := range known {
		if _, ok := covered[id]; !ok {
			return fmt.Errorf("%w: criterion %q is not covered by the plan", ErrInvalidInput, id)
		}
	}
	return nil
}

func truncateHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
