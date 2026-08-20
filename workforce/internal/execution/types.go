package execution

import (
	"fmt"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

type Stage string

const (
	StageLease     Stage = "lease"
	StageReconcile Stage = "reconcile"
	StageOrient    Stage = "orient"
	StageSelect    Stage = "select"
	StagePropose   Stage = "propose"
	StageCompile   Stage = "compile"
	StagePreflight Stage = "preflight"
	StageExecute   Stage = "execute"
	StageObserve   Stage = "observe"
	StageVerify    Stage = "verify"
	StageCommit    Stage = "commit"
	StageYield     Stage = "yield"
	StageSleep     Stage = "sleep"
)

var orderedStages = []Stage{
	StageLease, StageReconcile, StageOrient, StageSelect, StagePropose,
	StageCompile, StagePreflight, StageExecute, StageObserve, StageVerify,
	StageCommit, StageYield, StageSleep,
}

func (stage Stage) Valid() bool {
	for _, current := range orderedStages {
		if stage == current {
			return true
		}
	}
	return false
}

type Decision string

const (
	DecisionAdvance            Decision = "advance"
	DecisionDispatch           Decision = "dispatch"
	DecisionObserved           Decision = "observed"
	DecisionEffectAmbiguous    Decision = "effect_ambiguous"
	DecisionReconcileUnchanged Decision = "reconcile_unchanged"
	DecisionReconcileCompleted Decision = "reconcile_completed"
	DecisionWaitDependency     Decision = "wait_dependency"
	DecisionWaitApproval       Decision = "wait_approval"
	DecisionBlock              Decision = "block"
	DecisionComplete           Decision = "complete"
	DecisionExhaustBudget      Decision = "exhaust_budget"
	DecisionExpireLease        Decision = "expire_lease"
	DecisionCancel             Decision = "cancel"
	DecisionFail               Decision = "fail"
)

func (decision Decision) Valid() bool {
	switch decision {
	case DecisionAdvance, DecisionDispatch, DecisionObserved,
		DecisionEffectAmbiguous, DecisionReconcileUnchanged,
		DecisionReconcileCompleted, DecisionWaitDependency,
		DecisionWaitApproval, DecisionBlock, DecisionComplete,
		DecisionExhaustBudget, DecisionExpireLease, DecisionCancel,
		DecisionFail:
		return true
	default:
		return false
	}
}

type Usage struct {
	ModelCalls uint32 `json:"model_calls"`
	ToolCalls  uint32 `json:"tool_calls"`
	CostMinor  int64  `json:"cost_minor"`
}

type State struct {
	SchemaVersion   string                    `json:"schema_version"`
	OrganizationID  contracts.OrganizationID  `json:"organization_id"`
	WakeID          contracts.WakeID          `json:"wake_id"`
	LeaseID         contracts.LeaseID         `json:"lease_id"`
	SeatID          contracts.SeatID          `json:"seat_id"`
	IntentID        contracts.IntentID        `json:"intent_id"`
	Stage           Stage                     `json:"stage"`
	ResumeStage     Stage                     `json:"resume_stage"`
	Version         uint64                    `json:"version"`
	Steps           uint32                    `json:"steps"`
	Usage           Usage                     `json:"usage"`
	Budget          contracts.WakeBudget      `json:"budget"`
	StartedAt       time.Time                 `json:"started_at"`
	LeaseExpiresAt  time.Time                 `json:"lease_expires_at"`
	PacketDigest    contracts.ContentHash     `json:"packet_digest"`
	PendingEffectID string                    `json:"pending_effect_id"`
	ReceiptID       contracts.ReceiptID       `json:"receipt_id"`
	Disposition     contracts.WakeDisposition `json:"disposition"`
	ReasonCode      string                    `json:"reason_code"`
	UpdatedAt       time.Time                 `json:"updated_at"`
}

func (state State) Validate() error {
	if state.SchemaVersion != contracts.SchemaVersionV1 ||
		state.OrganizationID == "" || state.WakeID == "" || state.LeaseID == "" ||
		state.SeatID == "" || state.IntentID == "" {
		return fmt.Errorf("execution: checkpoint identity is incomplete")
	}
	if !state.Stage.Valid() || state.Version == 0 {
		return fmt.Errorf("execution: checkpoint stage or version is invalid")
	}
	if state.ResumeStage != "" && state.ResumeStage != StageExecute {
		return fmt.Errorf("execution: invalid resume stage %q", state.ResumeStage)
	}
	if err := state.Budget.Validate(); err != nil {
		return err
	}
	if err := state.PacketDigest.Validate(); err != nil {
		return err
	}
	if state.StartedAt.IsZero() || state.StartedAt.Location() != time.UTC ||
		state.LeaseExpiresAt.IsZero() || state.LeaseExpiresAt.Location() != time.UTC ||
		!state.LeaseExpiresAt.After(state.StartedAt) ||
		state.UpdatedAt.IsZero() || state.UpdatedAt.Location() != time.UTC {
		return fmt.Errorf("execution: checkpoint times are invalid")
	}
	if state.Usage.CostMinor < 0 {
		return fmt.Errorf("execution: cost cannot be negative")
	}
	if state.Disposition != "" && (!state.Disposition.Valid() || state.Stage != StageSleep) {
		return fmt.Errorf("execution: only sleeping checkpoints may have a disposition")
	}
	if state.Stage == StageSleep && state.Disposition == "" {
		return fmt.Errorf("execution: sleeping checkpoint requires a disposition")
	}
	if len(state.ReasonCode) > 128 || strings.ContainsAny(state.ReasonCode, "\r\n") {
		return fmt.Errorf("execution: reason code is invalid")
	}
	if state.PendingEffectID != "" && len(state.PendingEffectID) > 128 {
		return fmt.Errorf("execution: effect identity is invalid")
	}
	if state.ReceiptID != "" && len(state.ReceiptID) > 128 {
		return fmt.Errorf("execution: receipt identity is invalid")
	}
	return nil
}

type AdvanceRequest struct {
	OrganizationID   contracts.OrganizationID
	WakeID           contracts.WakeID
	ExpectedVersion  uint64
	Decision         Decision
	IdempotencyKey   string
	Usage            Usage
	EffectID         string
	ReceiptID        contracts.ReceiptID
	FinalDisposition contracts.WakeDisposition
	ReasonCode       string
}
