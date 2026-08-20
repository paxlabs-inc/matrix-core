// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package work

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"centra/core/cortexclient"
	"centra/agents/neo/internal/runtime/loop"
	"centra/agents/neo/internal/runtime/protocol"
	"centra/agents/neo/internal/runtime/turnstate"

	"github.com/google/uuid"
)

const (
	MaxParallelSpecialists = 32
	maxTasks               = 128
	maxCriteria            = 32
	maxSteeringRecords     = 64
	maxTextBytes           = 16 << 10
)

var (
	ErrAuthorityDenied  = errors.New("runtime work: authority denied")
	ErrBudgetProjection = errors.New("runtime work: budget projection cannot finish")
	ErrEvidenceRequired = errors.New("runtime work: verified evidence required")
)

type Specialist string

const (
	SpecialistDiscovery      Specialist = "discovery"
	SpecialistExploration    Specialist = "exploration"
	SpecialistImplementation Specialist = "implementation"
	SpecialistTest           Specialist = "test"
	SpecialistSecurity       Specialist = "security"
	SpecialistData           Specialist = "data"
	SpecialistFrontend       Specialist = "frontend"
	SpecialistPerformance    Specialist = "performance"
	SpecialistOperations     Specialist = "operations"
	SpecialistReview         Specialist = "review"
)

type RunStatus string

const (
	RunQueued         RunStatus = "queued"
	RunWorking        RunStatus = "working"
	RunWaiting        RunStatus = "waiting"
	RunBlocked        RunStatus = "blocked"
	RunPaused         RunStatus = "paused"
	RunCancelled      RunStatus = "cancelled"
	RunCompleted      RunStatus = "completed"
	RunOutcomeUnknown RunStatus = "outcome_unknown"
)

type TaskStatus string

const (
	TaskPending         TaskStatus = "pending"
	TaskReady           TaskStatus = "ready"
	TaskRunning         TaskStatus = "running"
	TaskWaitingEvidence TaskStatus = "waiting_evidence"
	TaskRetrying        TaskStatus = "retrying"
	TaskBlocked         TaskStatus = "blocked"
	TaskCancelled       TaskStatus = "cancelled"
	TaskCompleted       TaskStatus = "completed"
	TaskOutcomeUnknown  TaskStatus = "outcome_unknown"
)

type AuthorityScope struct {
	ReadFiles       []string `json:"read_files,omitempty"`
	WriteFiles      []string `json:"write_files,omitempty"`
	Services        []string `json:"services,omitempty"`
	EnvironmentKeys []string `json:"environment_keys,omitempty"`
	NetworkHosts    []string `json:"network_hosts,omitempty"`
	ExternalEffects bool     `json:"external_effects"`
}

type SpecialistBudget struct {
	MaxTokens             int64 `json:"max_tokens"`
	MaxModelMicrocents    int64 `json:"max_model_microcents"`
	MaxProviderMicrocents int64 `json:"max_provider_microcents"`
	MaxToolCalls          int   `json:"max_tool_calls"`
	MaxWallSeconds        int   `json:"max_wall_seconds"`
	MaxProcesses          int   `json:"max_processes"`
	MaxStorageBytes       int64 `json:"max_storage_bytes"`
	MaxNetworkBytes       int64 `json:"max_network_bytes"`
	MaxRetries            int   `json:"max_retries"`
}

type SupervisorBudget struct {
	SpecialistBudget
	MaxParallel int `json:"max_parallel"`
}

type BudgetUsage struct {
	Tokens             int64 `json:"tokens"`
	ModelMicrocents    int64 `json:"model_microcents"`
	ModelCostKnown     bool  `json:"model_cost_known"`
	ProviderMicrocents int64 `json:"provider_microcents"`
	ProviderSpendKnown bool  `json:"provider_spend_known"`
	ToolCalls          int   `json:"tool_calls"`
	WallSeconds        int   `json:"wall_seconds"`
	Processes          int   `json:"processes"`
	StorageBytes       int64 `json:"storage_bytes"`
	NetworkBytes       int64 `json:"network_bytes"`
	Retries            int   `json:"retries"`
}

type ParentPlan struct {
	ID       string           `json:"id"`
	Accepted bool             `json:"accepted"`
	Scope    AuthorityScope   `json:"scope"`
	Tools    []string         `json:"tools"`
	Budget   SupervisorBudget `json:"budget"`
}

type TaskSpec struct {
	ID             string           `json:"id"`
	Title          string           `json:"title"`
	Prompt         string           `json:"prompt"`
	Criteria       []string         `json:"criteria"`
	DependsOn      []string         `json:"depends_on,omitempty"`
	Specialist     Specialist       `json:"specialist,omitempty"`
	Scope          AuthorityScope   `json:"scope"`
	Tools          []string         `json:"tools"`
	Budget         SpecialistBudget `json:"budget"`
	ProjectedUsage BudgetUsage      `json:"projected_usage"`
}

type TaskPacket struct {
	ID             string            `json:"id"`
	Digest         [32]byte          `json:"digest"`
	SupervisorID   uuid.UUID         `json:"supervisor_id"`
	ActorID        string            `json:"actor_id"`
	ConversationID string            `json:"conversation_id"`
	ParentPlanID   string            `json:"parent_plan_id"`
	Title          string            `json:"title"`
	Prompt         string            `json:"prompt"`
	Criteria       []string          `json:"criteria"`
	DependsOn      []string          `json:"depends_on,omitempty"`
	Specialist     Specialist        `json:"specialist"`
	Scope          AuthorityScope    `json:"scope"`
	Tools          []string          `json:"tools"`
	ToolClasses    map[string]string `json:"tool_classes"`
	Budget         SpecialistBudget  `json:"budget"`
	ProjectedUsage BudgetUsage       `json:"projected_usage"`
}

type EvidenceRef struct {
	Criterion string                         `json:"criterion"`
	Citation  cortexclient.ToolEventCitation `json:"citation"`
}

type ExternalEffect struct {
	ToolName       string `json:"tool_name"`
	Target         string `json:"target,omitempty"`
	IdempotencyKey string `json:"idempotency_key"`
	State          string `json:"state"`
}

type Finding struct {
	Kind       string   `json:"kind"`
	Summary    string   `json:"summary"`
	Evidence   []string `json:"evidence,omitempty"`
	Confidence string   `json:"confidence,omitempty"`
}

type WorkerResult struct {
	AttemptID       uuid.UUID        `json:"attempt_id"`
	WorkerID        string           `json:"worker_id,omitempty"`
	Status          TaskStatus       `json:"status"`
	Progress        int              `json:"progress"`
	Summary         string           `json:"summary"`
	Usage           BudgetUsage      `json:"usage"`
	Evidence        []EvidenceRef    `json:"evidence,omitempty"`
	Findings        []Finding        `json:"findings,omitempty"`
	ExternalEffects []ExternalEffect `json:"external_effects,omitempty"`
}

type Attempt struct {
	ID               uuid.UUID        `json:"id"`
	Number           int              `json:"number"`
	Status           TaskStatus       `json:"status"`
	StartedAt        time.Time        `json:"started_at"`
	FinishedAt       *time.Time       `json:"finished_at,omitempty"`
	WorkerID         string           `json:"worker_id,omitempty"`
	Summary          string           `json:"summary,omitempty"`
	Error            string           `json:"error,omitempty"`
	Usage            BudgetUsage      `json:"usage"`
	Evidence         []EvidenceRef    `json:"evidence,omitempty"`
	Findings         []Finding        `json:"findings,omitempty"`
	ExternalEffects  []ExternalEffect `json:"external_effects,omitempty"`
	ResumeCheckpoint bool             `json:"resume_checkpoint,omitempty"`
}

type SpecialistTask struct {
	Packet         TaskPacket `json:"packet"`
	Status         TaskStatus `json:"status"`
	Progress       int        `json:"progress"`
	Attempts       []Attempt  `json:"attempts,omitempty"`
	BlockingReason string     `json:"blocking_reason,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type ScopeLease struct {
	ID        uuid.UUID `json:"id"`
	TaskID    string    `json:"task_id"`
	Resource  string    `json:"resource"`
	Mode      string    `json:"mode"`
	ExpiresAt time.Time `json:"expires_at"`
}

type SteeringRecord struct {
	At          time.Time `json:"at"`
	Instruction string    `json:"instruction"`
}

type Reconciliation struct {
	At             time.Time `json:"at"`
	RecoveredTasks int       `json:"recovered_tasks"`
	UncertainTasks int       `json:"uncertain_tasks"`
}

type SupervisorRun struct {
	ID              uuid.UUID        `json:"id"`
	ActorID         string           `json:"actor_id"`
	ConversationID  string           `json:"conversation_id"`
	ParentPlanID    string           `json:"parent_plan_id"`
	Status          RunStatus        `json:"status"`
	Revision        uint64           `json:"revision"`
	Tasks           []SpecialistTask `json:"tasks"`
	Leases          []ScopeLease     `json:"leases,omitempty"`
	Budget          SupervisorBudget `json:"budget"`
	Usage           BudgetUsage      `json:"usage"`
	Projection      BudgetUsage      `json:"projected_usage"`
	Steering        []SteeringRecord `json:"steering,omitempty"`
	Reconciliations []Reconciliation `json:"reconciliations,omitempty"`
	Synthesis       []Finding        `json:"synthesis,omitempty"`
	CreatedAt       time.Time        `json:"created_at"`
	UpdatedAt       time.Time        `json:"updated_at"`
	FinishedAt      *time.Time       `json:"finished_at,omitempty"`
}

type StartInput struct {
	ConversationID string     `json:"conversation_id"`
	Plan           ParentPlan `json:"plan"`
	Tasks          []TaskSpec `json:"tasks"`
}

type Executor interface {
	Surface(context.Context) []protocol.ToolDefinition
	Execute(
		context.Context,
		TaskPacket,
		uuid.UUID,
		<-chan SteeringRecord,
	) (WorkerResult, error)
}

type CitationVerifier interface {
	VerifyToolEventCitation(
		cortexclient.ToolEventCitation,
	) (cortexclient.ToolEventPayload, error)
}

type ToolMetadata interface {
	SideEffectClass(string) (string, bool)
}

type EffectReconciler interface {
	ReconcileEffect(context.Context, string) (loop.ReconcileResult, error)
}

type RunStore interface {
	SaveSupervisorRun(context.Context, turnstate.SupervisorRecord) error
	LoadSupervisorRun(context.Context, string) (turnstate.SupervisorRecord, error)
	ListSupervisorRuns(context.Context, string) ([]turnstate.SupervisorRecord, error)
	ListAllSupervisorRuns(context.Context) ([]turnstate.SupervisorRecord, error)
}

type ToolManager interface {
	Surface(context.Context) []protocol.ToolDefinition
	Execute(
		context.Context,
		protocol.NormalizedToolCall,
		string,
	) (loop.ToolResult, error)
	Reconcile(context.Context, string) (loop.ReconcileResult, error)
}

func terminalRun(status RunStatus) bool {
	return status == RunCancelled || status == RunCompleted
}

func boundedText(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > maxTextBytes {
		return value[:maxTextBytes]
	}
	return value
}

func cloneRun(run SupervisorRun) SupervisorRun {
	raw, _ := json.Marshal(run)
	var result SupervisorRun
	_ = json.Unmarshal(raw, &result)
	return result
}

func validateRun(run SupervisorRun) error {
	if run.ID == uuid.Nil || strings.TrimSpace(run.ActorID) == "" ||
		strings.TrimSpace(run.ConversationID) == "" ||
		strings.TrimSpace(run.ParentPlanID) == "" ||
		len(run.Tasks) == 0 || !run.Status.valid() ||
		run.CreatedAt.IsZero() || run.UpdatedAt.IsZero() {
		return fmt.Errorf("runtime work: invalid supervisor run")
	}
	for _, task := range run.Tasks {
		if err := verifyPacketDigest(task.Packet); err != nil {
			return err
		}
	}
	return nil
}

func (status RunStatus) valid() bool {
	switch status {
	case RunQueued, RunWorking, RunWaiting, RunBlocked, RunPaused,
		RunCancelled, RunCompleted, RunOutcomeUnknown:
		return true
	default:
		return false
	}
}
