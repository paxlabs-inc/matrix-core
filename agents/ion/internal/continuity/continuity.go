// Package continuity binds every continuation and recovery action to the
// latest explicitly approved goal contract. It owns the durable authorization
// root, bounded non-promotable RecoveryFrames, worker leases with fencing
// identity, and the evidence correlation that completion depends on, so that
// tool errors, restarts, provider changes, and worker replacement cannot
// silently replace delegated work or manufacture completion.
package continuity

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

const (
	// StateVersion is the durable continuation-state contract version.
	StateVersion = "ion.goal-bound-continuity.v1"
	stateKind    = "goal_bound_continuity_v1"

	// MaxFrameToolInvocations is the independent per-frame tool ceiling.
	MaxFrameToolInvocations = 32
	// MaxFrameModelTokens is the independent per-frame model-token ceiling.
	MaxFrameModelTokens = 131072
	// MaxFrameElapsedSeconds is the independent per-frame wall-clock ceiling.
	MaxFrameElapsedSeconds = 1800
	// MaxFrameSubsequentErrors is the independent per-frame error ceiling.
	MaxFrameSubsequentErrors = 8
	// MaxFrameRetriesPerOperation bounds retries of one failed operation.
	MaxFrameRetriesPerOperation = 3
	// MaxActiveFrames bounds simultaneously active frames, outermost included.
	MaxActiveFrames = 3

	maxTextBytes     = 4096
	maxStrategies    = 16
	maxActionRecords = 512
	maxEvidence      = 512
	maxFrames        = 64
	maxProposals     = 64
	maxApproved      = 64
	maxFencing       = 128
	maxContinuations = 128
	maxCriteria      = 32
	maxPlanNodes     = 256
)

var (
	// ErrInvalidInput rejects malformed callers before any durable effect.
	ErrInvalidInput = errors.New("continuity: invalid input")
	// ErrNoApprovedGoal reports that no explicit approval exists yet.
	ErrNoApprovedGoal = errors.New("continuity: no explicitly approved goal contract")
	// ErrGoalMismatch rejects authorization bound to another goal version.
	ErrGoalMismatch = errors.New("continuity: authorization is not bound to the approved goal")
	// ErrParent rejects authorization without exactly one open parent.
	ErrParent = errors.New("continuity: authorization requires exactly one open parent")
	// ErrEvidenceDelta rejects authorization without an expected evidence delta.
	ErrEvidenceDelta = errors.New("continuity: expected observable evidence delta is required")
	// ErrStrategy rejects a strategy change outside the frame allowlist.
	ErrStrategy = errors.New("continuity: strategy change is not allowlisted")
	// ErrStaleWorker rejects stale, duplicate, or expired worker authority.
	ErrStaleWorker = errors.New("continuity: worker lease or fencing identity is not current")
	// ErrDuplicateAction rejects a repeated idempotency key.
	ErrDuplicateAction = errors.New("continuity: duplicate action authorization")
	// ErrBudgetExhausted reports a closed frame after budget exhaustion.
	ErrBudgetExhausted = errors.New("continuity: recovery budget exhausted")
	// ErrFrameDepth rejects creation beyond the nesting maximum.
	ErrFrameDepth = errors.New("continuity: recovery nesting depth limit reached")
	// ErrFrameActive rejects contract revision while recovery is open.
	ErrFrameActive = errors.New("continuity: recovery frame is active")
	// ErrFrameClosed rejects action under an already closed frame.
	ErrFrameClosed = errors.New("continuity: recovery frame is closed")
	// ErrExitUnverified rejects closing a frame without exit evidence.
	ErrExitUnverified = errors.New("continuity: recovery exit condition is not verified")
	// ErrEvidenceMissing rejects completion without correlated evidence.
	ErrEvidenceMissing = errors.New("continuity: server-verified evidence does not cover every approved criterion")
	// ErrNotFound reports an unknown proposal, frame, or authorization.
	ErrNotFound = errors.New("continuity: record not found")
)

// Strategy is the closed vocabulary of execution-strategy changes a recovery
// frame may allowlist. No strategy can revise the approved goal, constraints,
// done criteria, or plan.
type Strategy string

const (
	StrategyRetryWithBackoff  Strategy = "retry_with_backoff"
	StrategyAlternateTool     Strategy = "alternate_tool"
	StrategyReReadInputs      Strategy = "re_read_inputs"
	StrategySmallerBatch      Strategy = "smaller_batch"
	StrategySwitchProvider    Strategy = "switch_provider"
	StrategyReduceConcurrency Strategy = "reduce_concurrency"
	StrategyRequestHumanHelp  Strategy = "request_human_help"
)

var knownStrategies = map[Strategy]struct{}{
	StrategyRetryWithBackoff: {}, StrategyAlternateTool: {},
	StrategyReReadInputs: {}, StrategySmallerBatch: {},
	StrategySwitchProvider: {}, StrategyReduceConcurrency: {},
	StrategyRequestHumanHelp: {},
}

// Criterion is one independently provable approved completion condition.
type Criterion struct {
	ID          string `json:"id"`
	Description string `json:"description"`
}

// PlanNode is one approved plan unit. It is also the durable work item.
type PlanNode struct {
	ID        string   `json:"id"`
	Title     string   `json:"title"`
	Criteria  []string `json:"criteria"`
	DependsOn []string `json:"depends_on,omitempty"`
}

// GoalContract is one immutable explicitly approved version.
type GoalContract struct {
	ContractID   uuid.UUID   `json:"contract_id"`
	Version      uint64      `json:"version"`
	Hash         string      `json:"goal_hash"`
	Goal         string      `json:"goal"`
	Deliverable  string      `json:"deliverable"`
	Constraints  []string    `json:"constraints"`
	Verification []string    `json:"verification_required"`
	DoneCriteria []Criterion `json:"done_criteria"`
	Plan         []PlanNode  `json:"plan"`
	NextAction   string      `json:"next_action"`
	ApprovedBy   string      `json:"approved_by"`
	ApprovedAt   time.Time   `json:"approved_at"`
	ProposalID   uuid.UUID   `json:"proposal_id"`
}

// ProposalStatus is the closed revision lifecycle.
type ProposalStatus string

const (
	ProposalProposed   ProposalStatus = "proposed"
	ProposalApproved   ProposalStatus = "approved"
	ProposalRejected   ProposalStatus = "rejected"
	ProposalSuperseded ProposalStatus = "superseded"
)

// GoalProposal is a model- or user-originated revision that is authoritative
// only after explicit approval at a strictly increased version.
type GoalProposal struct {
	ID           uuid.UUID      `json:"id"`
	Version      uint64         `json:"version"`
	Status       ProposalStatus `json:"status"`
	Origin       string         `json:"origin"`
	Rationale    string         `json:"rationale"`
	Goal         string         `json:"goal"`
	Deliverable  string         `json:"deliverable"`
	Constraints  []string       `json:"constraints"`
	Verification []string       `json:"verification_required"`
	DoneCriteria []Criterion    `json:"done_criteria"`
	Plan         []PlanNode     `json:"plan"`
	NextAction   string         `json:"next_action"`
	CreatedAt    time.Time      `json:"created_at"`
	DecidedAt    *time.Time     `json:"decided_at,omitempty"`
	Decision     string         `json:"decision_reason,omitempty"`
}

// ProposalInput is the bounded revision request.
type ProposalInput struct {
	ContractID   uuid.UUID   `json:"contract_id"`
	Origin       string      `json:"origin"`
	Rationale    string      `json:"rationale"`
	Goal         string      `json:"goal"`
	Deliverable  string      `json:"deliverable"`
	Constraints  []string    `json:"constraints"`
	Verification []string    `json:"verification_required"`
	DoneCriteria []Criterion `json:"done_criteria"`
	Plan         []PlanNode  `json:"plan"`
	NextAction   string      `json:"next_action"`
}

// FrameStatus is the closed RecoveryFrame lifecycle.
type FrameStatus string

const (
	FrameActive         FrameStatus = "active"
	FrameClosedVerified FrameStatus = "closed_verified"
	FrameClosedExhaust  FrameStatus = "closed_exhausted"
	FrameClosedFailed   FrameStatus = "closed_terminal_failure"
)

// Disposition is the declared exhaustion outcome for the original work item.
type Disposition string

const (
	DispositionBlocked   Disposition = "blocked"
	DispositionEscalated Disposition = "escalated"
)

// FrameBudget is one frame's independent maxima.
type FrameBudget struct {
	ToolInvocations     int   `json:"tool_invocations"`
	ModelTokens         int64 `json:"model_tokens"`
	ElapsedSeconds      int   `json:"elapsed_seconds"`
	SubsequentErrors    int   `json:"subsequent_errors"`
	RetriesPerOperation int   `json:"retries_per_operation"`
}

// OperationRetry is deterministic per-operation retry accounting.
type OperationRetry struct {
	OperationID string `json:"operation_id"`
	Count       int    `json:"count"`
}

// FrameUsage is one frame's consumed budget.
type FrameUsage struct {
	ToolInvocations  int              `json:"tool_invocations"`
	ModelTokens      int64            `json:"model_tokens"`
	SubsequentErrors int              `json:"subsequent_errors"`
	Retries          []OperationRetry `json:"retries,omitempty"`
}

// ExitCondition is an evidence-verifiable frame exit declaration.
type ExitCondition struct {
	Description string   `json:"description"`
	Criteria    []string `json:"criteria"`
}

// RecoveryFrame is a durable non-promotable recovery scope attached to the
// original failed work item.
type RecoveryFrame struct {
	ID                 uuid.UUID     `json:"id"`
	Depth              int           `json:"depth"`
	ParentFrameID      *uuid.UUID    `json:"parent_frame_id,omitempty"`
	OriginalWorkItemID string        `json:"original_work_item_id"`
	ReturnTo           string        `json:"return_to"`
	Cause              string        `json:"cause"`
	Exit               ExitCondition `json:"exit_condition"`
	Disposition        Disposition   `json:"exhaustion_disposition"`
	Allowlist          []Strategy    `json:"strategy_allowlist"`
	Budget             FrameBudget   `json:"budget"`
	Consumed           FrameUsage    `json:"consumed"`
	Status             FrameStatus   `json:"status"`
	Attempted          []Strategy    `json:"attempted_strategies,omitempty"`
	OpenedAt           time.Time     `json:"opened_at"`
	ClosedAt           *time.Time    `json:"closed_at,omitempty"`
	ClosureReason      string        `json:"closure_reason,omitempty"`
	UnmetExit          string        `json:"unmet_exit_condition,omitempty"`
	AppliedDisposition Disposition   `json:"applied_disposition,omitempty"`
}

// Remaining reports how much of each budget dimension is left.
func (frame RecoveryFrame) Remaining(now time.Time) FrameBudget {
	elapsed := int(now.Sub(frame.OpenedAt).Seconds())
	if elapsed < 0 {
		elapsed = 0
	}
	return FrameBudget{
		ToolInvocations:     maxInt(frame.Budget.ToolInvocations-frame.Consumed.ToolInvocations, 0),
		ModelTokens:         maxInt64(frame.Budget.ModelTokens-frame.Consumed.ModelTokens, 0),
		ElapsedSeconds:      maxInt(frame.Budget.ElapsedSeconds-elapsed, 0),
		SubsequentErrors:    maxInt(frame.Budget.SubsequentErrors-frame.Consumed.SubsequentErrors, 0),
		RetriesPerOperation: frame.Budget.RetriesPerOperation,
	}
}

func (frame RecoveryFrame) retries(operationID string) int {
	for _, entry := range frame.Consumed.Retries {
		if entry.OperationID == operationID {
			return entry.Count
		}
	}
	return 0
}

// OpenFrameInput creates one recovery frame for a failed work item.
type OpenFrameInput struct {
	WorkerID     string        `json:"worker_id"`
	FencingToken uint64        `json:"fencing_token"`
	GoalHash     string        `json:"goal_hash"`
	GoalVersion  uint64        `json:"goal_version"`
	WorkItemID   string        `json:"work_item_id"`
	Cause        string        `json:"cause"`
	Exit         ExitCondition `json:"exit_condition"`
	Disposition  Disposition   `json:"exhaustion_disposition"`
	Allowlist    []Strategy    `json:"strategy_allowlist"`
	Budget       FrameBudget   `json:"budget"`
}

// ActionKind is the closed set of authorized action kinds.
type ActionKind string

const (
	ActionTool  ActionKind = "tool"
	ActionModel ActionKind = "model"
)

// ActionCost is the declared consumption of one authorized action.
type ActionCost struct {
	ToolInvocations int   `json:"tool_invocations"`
	ModelTokens     int64 `json:"model_tokens"`
}

// EvidenceDelta is the expected observable change for one parent.
type EvidenceDelta struct {
	Description string   `json:"description"`
	Criteria    []string `json:"criteria"`
}

// ActionEnvelope is the complete authorization request. Every field is
// validated before any effect is permitted.
type ActionEnvelope struct {
	GoalHash       string        `json:"goal_hash"`
	GoalVersion    uint64        `json:"goal_version"`
	WorkItemID     string        `json:"work_item_id,omitempty"`
	FrameID        uuid.UUID     `json:"frame_id,omitempty"`
	Expected       EvidenceDelta `json:"expected_evidence_delta"`
	WorkerID       string        `json:"worker_id"`
	FencingToken   uint64        `json:"fencing_token"`
	Kind           ActionKind    `json:"kind"`
	Strategy       Strategy      `json:"strategy,omitempty"`
	OperationID    string        `json:"operation_id"`
	IdempotencyKey string        `json:"idempotency_key"`
	Retry          bool          `json:"retry"`
	Cost           ActionCost    `json:"cost"`
}

// ActionRecord is the durable authorization ledger entry.
type ActionRecord struct {
	ID             uuid.UUID     `json:"id"`
	GoalHash       string        `json:"goal_hash"`
	GoalVersion    uint64        `json:"goal_version"`
	WorkItemID     string        `json:"work_item_id,omitempty"`
	FrameID        *uuid.UUID    `json:"frame_id,omitempty"`
	Kind           ActionKind    `json:"kind"`
	Strategy       Strategy      `json:"strategy,omitempty"`
	OperationID    string        `json:"operation_id"`
	IdempotencyKey string        `json:"idempotency_key"`
	Cost           ActionCost    `json:"cost"`
	Expected       EvidenceDelta `json:"expected_evidence_delta"`
	WorkerID       string        `json:"worker_id"`
	FencingToken   uint64        `json:"fencing_token"`
	AuthorizedAt   time.Time     `json:"authorized_at"`
	Settled        bool          `json:"settled"`
	Succeeded      bool          `json:"succeeded"`
	Error          string        `json:"error,omitempty"`
	SettledAt      *time.Time    `json:"settled_at,omitempty"`
}

// Authorization is the caller's proof that one action may produce effects.
type Authorization struct {
	ID           uuid.UUID  `json:"id"`
	GoalHash     string     `json:"goal_hash"`
	GoalVersion  uint64     `json:"goal_version"`
	WorkItemID   string     `json:"work_item_id,omitempty"`
	FrameID      *uuid.UUID `json:"frame_id,omitempty"`
	WorkerID     string     `json:"worker_id"`
	FencingToken uint64     `json:"fencing_token"`
	AuthorizedAt time.Time  `json:"authorized_at"`
}

// ActionOutcome settles one authorized action honestly.
type ActionOutcome struct {
	Succeeded   bool   `json:"succeeded"`
	Error       string `json:"error,omitempty"`
	ModelTokens int64  `json:"model_tokens,omitempty"`
}

// Settlement reports the durable consequence of settling an action.
type Settlement struct {
	Record        ActionRecord   `json:"record"`
	ClosedFrames  []FrameClosure `json:"closed_frames,omitempty"`
	ActiveFrameID *uuid.UUID     `json:"active_frame_id,omitempty"`
}

// EvidenceScope separates approved-criterion evidence from evidence that only
// shows a recovery strategy ran.
type EvidenceScope string

const (
	ScopeCriterion        EvidenceScope = "approved_criterion"
	ScopeRecoveryStrategy EvidenceScope = "recovery_strategy"
)

// EvidenceInput registers and server-verifies one workspace artifact.
type EvidenceInput struct {
	WorkerID     string        `json:"worker_id"`
	FencingToken uint64        `json:"fencing_token"`
	Scope        EvidenceScope `json:"scope"`
	Kind         string        `json:"kind"`
	Title        string        `json:"title"`
	Reference    string        `json:"reference"`
	Criteria     []string      `json:"criteria"`
	FrameID      uuid.UUID     `json:"frame_id,omitempty"`
}

// EvidenceRecord correlates one server-verified artifact to the approved goal.
type EvidenceRecord struct {
	ArtifactID   uuid.UUID     `json:"artifact_id"`
	Scope        EvidenceScope `json:"scope"`
	Criteria     []string      `json:"criteria"`
	GoalHash     string        `json:"goal_hash"`
	GoalVersion  uint64        `json:"goal_version"`
	SHA256       string        `json:"sha256"`
	SizeBytes    int64         `json:"size_bytes"`
	Verification string        `json:"verification"`
	VerifiedAt   time.Time     `json:"verified_at"`
	WorkerID     string        `json:"worker_id"`
	FencingToken uint64        `json:"fencing_token"`
	FrameID      *uuid.UUID    `json:"frame_id,omitempty"`
}

// EvidenceCursor is the deterministic replay position over evidence.
type EvidenceCursor struct {
	Count          int        `json:"count"`
	Digest         string     `json:"digest"`
	LastArtifactID *uuid.UUID `json:"last_artifact_id,omitempty"`
}

// WorkerLease is the current exclusive execution authority.
type WorkerLease struct {
	WorkerID     string    `json:"worker_id"`
	FencingToken uint64    `json:"fencing_token"`
	IssuedAt     time.Time `json:"issued_at"`
	ExpiresAt    time.Time `json:"expires_at"`
}

// FencingRecord retires prior worker authority durably.
type FencingRecord struct {
	WorkerID     string     `json:"worker_id"`
	FencingToken uint64     `json:"fencing_token"`
	IssuedAt     time.Time  `json:"issued_at"`
	RetiredAt    *time.Time `json:"retired_at,omitempty"`
	Reason       string     `json:"reason,omitempty"`
}

// Cause is the closed set of continuation causes.
type Cause string

const (
	CauseWorkerReplacement   Cause = "worker_replacement"
	CauseWorkerDeath         Cause = "worker_death"
	CauseProcessRestart      Cause = "process_restart"
	CauseContextCompression  Cause = "context_compression"
	CauseProviderChange      Cause = "provider_change"
	CauseWatchdogContinuaton Cause = "watchdog_continuation"
)

var knownCauses = map[Cause]struct{}{
	CauseWorkerReplacement: {}, CauseWorkerDeath: {}, CauseProcessRestart: {},
	CauseContextCompression: {}, CauseProviderChange: {},
	CauseWatchdogContinuaton: {},
}

// ContinueInput requests restoration under a new worker identity.
type ContinueInput struct {
	Cause    Cause         `json:"cause"`
	WorkerID string        `json:"worker_id"`
	Provider string        `json:"provider,omitempty"`
	LeaseTTL time.Duration `json:"lease_ttl,omitempty"`
}

// ContinuationRecord is the durable audit of one restoration.
type ContinuationRecord struct {
	Cause          Cause     `json:"cause"`
	At             time.Time `json:"at"`
	WorkerID       string    `json:"worker_id"`
	Provider       string    `json:"provider,omitempty"`
	FencingToken   uint64    `json:"fencing_token"`
	RestoredFrames int       `json:"restored_frames"`
	OpenWorkItemID string    `json:"open_work_item_id,omitempty"`
	GoalHash       string    `json:"goal_hash"`
	GoalVersion    uint64    `json:"goal_version"`
}

// FrameClosure is the inspectable consequence of closing a frame.
type FrameClosure struct {
	FrameID            uuid.UUID   `json:"frame_id"`
	Depth              int         `json:"depth"`
	Status             FrameStatus `json:"status"`
	OriginalWorkItemID string      `json:"original_work_item_id"`
	ReturnTo           string      `json:"return_to"`
	UnmetExit          string      `json:"unmet_exit_condition,omitempty"`
	Attempted          []Strategy  `json:"attempted_strategies,omitempty"`
	Consumed           FrameUsage  `json:"consumed_budget"`
	Disposition        Disposition `json:"applied_disposition,omitempty"`
	ResumedFrameID     *uuid.UUID  `json:"resumed_frame_id,omitempty"`
	ResumedWorkItemID  string      `json:"resumed_work_item_id,omitempty"`
	Reason             string      `json:"reason,omitempty"`
}

// DispositionRecord exposes the honest parent outcome after recovery failure.
type DispositionRecord struct {
	WorkItemID  string      `json:"work_item_id"`
	FrameID     uuid.UUID   `json:"frame_id"`
	Disposition Disposition `json:"disposition"`
	Reason      string      `json:"reason"`
	UnmetExit   string      `json:"unmet_exit_condition,omitempty"`
	Attempted   []Strategy  `json:"attempted_strategies,omitempty"`
	Consumed    FrameUsage  `json:"consumed_budget"`
	At          time.Time   `json:"at"`
}

// State is the complete durable continuation state.
type State struct {
	Version        string               `json:"version"`
	Revision       uint64               `json:"revision"`
	ActorID        uuid.UUID            `json:"actor_id"`
	ContractID     uuid.UUID            `json:"contract_id"`
	Approved       []GoalContract       `json:"approved"`
	Proposals      []GoalProposal       `json:"proposals,omitempty"`
	PlanNodeID     string               `json:"current_plan_node_id,omitempty"`
	OpenWorkItemID string               `json:"open_work_item_id,omitempty"`
	Frames         []RecoveryFrame      `json:"frames,omitempty"`
	Lease          *WorkerLease         `json:"lease,omitempty"`
	Fencing        []FencingRecord      `json:"fencing,omitempty"`
	Evidence       []EvidenceRecord     `json:"evidence,omitempty"`
	Cursor         EvidenceCursor       `json:"evidence_cursor"`
	Actions        []ActionRecord       `json:"actions,omitempty"`
	Continuations  []ContinuationRecord `json:"continuations,omitempty"`
	Dispositions   []DispositionRecord  `json:"dispositions,omitempty"`
	RetiredKeys    []string             `json:"retired_idempotency_keys,omitempty"`
	HighestFencing uint64               `json:"highest_fencing_token"`
	Checksum       string               `json:"checksum"`
}

// Snapshot is the authoritative replayable projection compared across
// replacement, restart, compression, provider change, and continuation.
type Snapshot struct {
	Version        string          `json:"version"`
	Revision       uint64          `json:"revision"`
	Contract       GoalContract    `json:"approved_contract"`
	PlanNodeID     string          `json:"current_plan_node_id,omitempty"`
	OpenWorkItemID string          `json:"open_work_item_id,omitempty"`
	Frames         []RecoveryFrame `json:"frames,omitempty"`
	ReturnTo       string          `json:"return_to,omitempty"`
	Lease          *WorkerLease    `json:"lease,omitempty"`
	HighestFencing uint64          `json:"highest_fencing_token"`
	Cursor         EvidenceCursor  `json:"evidence_cursor"`
	NextWorkItemID string          `json:"next_work_item_id,omitempty"`
}

// Continuation is the restored authority handed to a fresh worker.
type Continuation struct {
	Snapshot Snapshot           `json:"snapshot"`
	Lease    WorkerLease        `json:"lease"`
	Record   ContinuationRecord `json:"record"`
}

// StateFailure reports fail-closed durable-state validation.
type StateFailure struct {
	Reason      string      `json:"reason"`
	WorkItemID  string      `json:"work_item_id,omitempty"`
	Disposition Disposition `json:"disposition"`
	Exposed     bool        `json:"exposed"`
}

func (failure *StateFailure) Error() string {
	return fmt.Sprintf(
		"continuity: durable continuation state failed validation: %s", failure.Reason,
	)
}

// Unwrap keeps callers able to test for fail-closed state validation.
func (failure *StateFailure) Unwrap() error { return ErrCorruptState }

// ErrCorruptState is the sentinel behind every StateFailure.
var ErrCorruptState = errors.New("continuity: durable continuation state is invalid")

type goalHashPayload struct {
	ContractID  string      `json:"contract_id"`
	Goal        string      `json:"goal"`
	Constraints []string    `json:"constraints"`
	Criteria    []Criterion `json:"done_criteria"`
	Plan        []PlanNode  `json:"plan"`
}

// GoalHash binds a contract's root goal, constraints, done criteria, and plan.
func GoalHash(contract GoalContract) string {
	payload := goalHashPayload{
		ContractID:  contract.ContractID.String(),
		Goal:        contract.Goal,
		Constraints: contract.Constraints,
		Criteria:    contract.DoneCriteria,
		Plan:        contract.Plan,
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func stateChecksum(state State) string {
	state.Checksum = ""
	raw, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(raw)
	return hex.EncodeToString(digest[:])
}

func evidenceDigest(records []EvidenceRecord) string {
	chain := sha256.New()
	for _, record := range records {
		raw, err := json.Marshal(record)
		if err != nil {
			return ""
		}
		chain.Write(raw)
	}
	return hex.EncodeToString(chain.Sum(nil))
}

func (state State) latest() (GoalContract, bool) {
	if len(state.Approved) == 0 {
		return GoalContract{}, false
	}
	return state.Approved[len(state.Approved)-1], true
}

func (state State) activeFrames() []RecoveryFrame {
	active := make([]RecoveryFrame, 0, MaxActiveFrames)
	for _, frame := range state.Frames {
		if frame.Status == FrameActive {
			active = append(active, frame)
		}
	}
	return active
}

func (state State) innermost() (RecoveryFrame, bool) {
	active := state.activeFrames()
	if len(active) == 0 {
		return RecoveryFrame{}, false
	}
	return active[len(active)-1], true
}

func (state *State) frameIndex(id uuid.UUID) int {
	for index := range state.Frames {
		if state.Frames[index].ID == id {
			return index
		}
	}
	return -1
}

// validate is the fail-closed durable-state contract. Any absent, corrupt,
// incomplete, or internally inconsistent field denies continuation.
func validate(state State, now time.Time) error {
	if state.Version != StateVersion {
		return &StateFailure{Reason: "unsupported state version"}
	}
	if state.Revision == 0 {
		return &StateFailure{Reason: "missing state revision"}
	}
	if state.ActorID == uuid.Nil || state.ContractID == uuid.Nil {
		return &StateFailure{Reason: "missing actor or contract identity"}
	}
	if state.Checksum != stateChecksum(state) {
		return &StateFailure{
			Reason:     "state checksum does not match content",
			WorkItemID: state.OpenWorkItemID,
		}
	}
	if len(state.Approved) == 0 || len(state.Approved) > maxApproved {
		return &StateFailure{Reason: "no explicitly approved goal contract"}
	}
	previous := uint64(0)
	for _, contract := range state.Approved {
		if contract.Version <= previous {
			return &StateFailure{Reason: "approved versions are not strictly increasing"}
		}
		previous = contract.Version
		if contract.ContractID != state.ContractID {
			return &StateFailure{Reason: "approved contract identity is inconsistent"}
		}
		if contract.Hash == "" || contract.Hash != GoalHash(contract) {
			return &StateFailure{
				Reason:     "approved goal hash does not bind its goal, constraints, criteria, and plan",
				WorkItemID: state.OpenWorkItemID,
			}
		}
	}
	current, _ := state.latest()
	for _, proposal := range state.Proposals {
		switch proposal.Status {
		case ProposalProposed:
			if proposal.Version <= current.Version {
				return &StateFailure{Reason: "open proposal does not strictly increase the goal version"}
			}
		case ProposalApproved, ProposalRejected, ProposalSuperseded:
		default:
			return &StateFailure{Reason: "unknown proposal status"}
		}
	}
	if err := validateFrames(state, current, now); err != nil {
		return err
	}
	if state.Cursor.Count != len(state.Evidence) ||
		state.Cursor.Digest != evidenceDigest(state.Evidence) {
		return &StateFailure{
			Reason:     "evidence cursor is inconsistent with durable evidence",
			WorkItemID: state.OpenWorkItemID,
		}
	}
	for _, record := range state.Evidence {
		if record.Verification != "server_sha256" || len(record.SHA256) != 64 {
			return &StateFailure{Reason: "evidence is not server verified"}
		}
	}
	if state.Lease != nil {
		if strings.TrimSpace(state.Lease.WorkerID) == "" ||
			state.Lease.FencingToken == 0 ||
			state.Lease.FencingToken != state.HighestFencing {
			return &StateFailure{
				Reason:     "worker lease and fencing records disagree",
				WorkItemID: state.OpenWorkItemID,
			}
		}
	}
	highest := uint64(0)
	for _, record := range state.Fencing {
		if record.FencingToken == 0 {
			return &StateFailure{Reason: "fencing record is incomplete"}
		}
		if record.FencingToken > highest {
			highest = record.FencingToken
		}
	}
	if highest != state.HighestFencing {
		return &StateFailure{
			Reason:     "highest fencing token is inconsistent",
			WorkItemID: state.OpenWorkItemID,
		}
	}
	for _, action := range state.Actions {
		if action.GoalHash == "" || action.IdempotencyKey == "" || action.WorkerID == "" {
			return &StateFailure{Reason: "authorization ledger entry is incomplete"}
		}
		if (action.WorkItemID == "") == (action.FrameID == nil) {
			return &StateFailure{Reason: "authorization does not have exactly one parent"}
		}
	}
	return nil
}

func validateFrames(state State, current GoalContract, now time.Time) error {
	if len(state.Frames) > maxFrames {
		return &StateFailure{Reason: "recovery frame ledger exceeds bounds"}
	}
	approved := make(map[string]struct{}, len(current.DoneCriteria))
	for _, criterion := range current.DoneCriteria {
		approved[criterion.ID] = struct{}{}
	}
	active := 0
	var previous *RecoveryFrame
	for index := range state.Frames {
		frame := state.Frames[index]
		if frame.ID == uuid.Nil || frame.OriginalWorkItemID == "" {
			return &StateFailure{Reason: "recovery frame is incomplete"}
		}
		if frame.ReturnTo != frame.OriginalWorkItemID {
			return &StateFailure{
				Reason:     "recovery frame return_to is not the original work item",
				WorkItemID: frame.OriginalWorkItemID,
			}
		}
		if frame.Disposition != DispositionBlocked && frame.Disposition != DispositionEscalated {
			return &StateFailure{Reason: "recovery frame disposition is undeclared"}
		}
		if len(frame.Exit.Criteria) == 0 || strings.TrimSpace(frame.Exit.Description) == "" {
			return &StateFailure{Reason: "recovery frame exit condition is not evidence verifiable"}
		}
		for _, criterion := range frame.Exit.Criteria {
			if _, ok := approved[criterion]; !ok {
				return &StateFailure{
					Reason:     "recovery exit condition references an unapproved criterion",
					WorkItemID: frame.OriginalWorkItemID,
				}
			}
		}
		if len(frame.Allowlist) == 0 || len(frame.Allowlist) > maxStrategies {
			return &StateFailure{Reason: "recovery frame strategy allowlist is invalid"}
		}
		for _, strategy := range frame.Allowlist {
			if _, ok := knownStrategies[strategy]; !ok {
				return &StateFailure{Reason: "recovery frame allowlists an unknown strategy"}
			}
		}
		if err := validateFrameBudget(frame); err != nil {
			return err
		}
		switch frame.Status {
		case FrameActive:
			active++
			if active > MaxActiveFrames {
				return &StateFailure{Reason: "more active frames than the nesting maximum"}
			}
			if frame.Depth != active {
				return &StateFailure{Reason: "active recovery frames are not an ordered stack"}
			}
			if active == 1 && frame.ParentFrameID != nil {
				return &StateFailure{Reason: "outermost active frame declares a parent"}
			}
			if active > 1 {
				if frame.ParentFrameID == nil || previous == nil ||
					*frame.ParentFrameID != previous.ID {
					return &StateFailure{Reason: "nested recovery frame is not attached to its parent"}
				}
			}
			if state.OpenWorkItemID != frame.OriginalWorkItemID {
				return &StateFailure{
					Reason:     "active recovery frame is detached from the open work item",
					WorkItemID: frame.OriginalWorkItemID,
				}
			}
			copied := frame
			previous = &copied
			_ = now
		case FrameClosedVerified, FrameClosedExhaust, FrameClosedFailed:
			if frame.ClosedAt == nil {
				return &StateFailure{Reason: "closed recovery frame has no closure time"}
			}
		default:
			return &StateFailure{Reason: "unknown recovery frame status"}
		}
	}
	return nil
}

func validateFrameBudget(frame RecoveryFrame) error {
	budget := frame.Budget
	if budget.ToolInvocations < 0 || budget.ToolInvocations > MaxFrameToolInvocations ||
		budget.ModelTokens < 0 || budget.ModelTokens > MaxFrameModelTokens ||
		budget.ElapsedSeconds < 0 || budget.ElapsedSeconds > MaxFrameElapsedSeconds ||
		budget.SubsequentErrors < 0 || budget.SubsequentErrors > MaxFrameSubsequentErrors ||
		budget.RetriesPerOperation < 0 ||
		budget.RetriesPerOperation > MaxFrameRetriesPerOperation {
		return &StateFailure{
			Reason:     "recovery frame budget exceeds the declared maxima",
			WorkItemID: frame.OriginalWorkItemID,
		}
	}
	if frame.Consumed.ToolInvocations > budget.ToolInvocations ||
		frame.Consumed.ModelTokens > budget.ModelTokens ||
		frame.Consumed.SubsequentErrors > budget.SubsequentErrors {
		return &StateFailure{
			Reason:     "recovery frame consumed more than its budget",
			WorkItemID: frame.OriginalWorkItemID,
		}
	}
	for _, retry := range frame.Consumed.Retries {
		if retry.Count > budget.RetriesPerOperation {
			return &StateFailure{
				Reason:     "recovery frame exceeded retries for one operation",
				WorkItemID: frame.OriginalWorkItemID,
			}
		}
	}
	return nil
}

func boundedBudget(requested FrameBudget) FrameBudget {
	if requested.ToolInvocations <= 0 || requested.ToolInvocations > MaxFrameToolInvocations {
		requested.ToolInvocations = MaxFrameToolInvocations
	}
	if requested.ModelTokens <= 0 || requested.ModelTokens > MaxFrameModelTokens {
		requested.ModelTokens = MaxFrameModelTokens
	}
	if requested.ElapsedSeconds <= 0 || requested.ElapsedSeconds > MaxFrameElapsedSeconds {
		requested.ElapsedSeconds = MaxFrameElapsedSeconds
	}
	if requested.SubsequentErrors <= 0 || requested.SubsequentErrors > MaxFrameSubsequentErrors {
		requested.SubsequentErrors = MaxFrameSubsequentErrors
	}
	if requested.RetriesPerOperation <= 0 ||
		requested.RetriesPerOperation > MaxFrameRetriesPerOperation {
		requested.RetriesPerOperation = MaxFrameRetriesPerOperation
	}
	return requested
}

func clampToRemaining(requested, remaining FrameBudget) FrameBudget {
	return FrameBudget{
		ToolInvocations:     minInt(requested.ToolInvocations, remaining.ToolInvocations),
		ModelTokens:         minInt64(requested.ModelTokens, remaining.ModelTokens),
		ElapsedSeconds:      minInt(requested.ElapsedSeconds, remaining.ElapsedSeconds),
		SubsequentErrors:    minInt(requested.SubsequentErrors, remaining.SubsequentErrors),
		RetriesPerOperation: minInt(requested.RetriesPerOperation, remaining.RetriesPerOperation),
	}
}

func trimUnique(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func uniqueStrategies(values []Strategy) []Strategy {
	seen := make(map[Strategy]struct{}, len(values))
	result := make([]Strategy, 0, len(values))
	for _, value := range values {
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Slice(result, func(left, right int) bool { return result[left] < result[right] })
	return result
}

func invalidText(value string) bool {
	trimmed := strings.TrimSpace(value)
	return trimmed == "" || len(trimmed) > maxTextBytes
}

func boundedText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxTextBytes {
		return value[:maxTextBytes]
	}
	return value
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func minInt64(left, right int64) int64 {
	if left < right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}
