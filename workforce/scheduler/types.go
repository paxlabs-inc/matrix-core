package scheduler

import (
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrConflict     = errors.New("scheduler: conflict")
	ErrUnauthorized = errors.New("scheduler: unauthorized")
	ErrUncertain    = errors.New("scheduler: state is uncertain")
	ErrNotReady     = errors.New("scheduler: no wake is dispatchable")
)

type TriggerKind string

const (
	TriggerOnce       TriggerKind = "once"
	TriggerRecurring  TriggerKind = "recurring"
	TriggerEvent      TriggerKind = "event"
	TriggerDeadline   TriggerKind = "deadline"
	TriggerApproval   TriggerKind = "approval"
	TriggerDependency TriggerKind = "dependency"
	TriggerCorrection TriggerKind = "correction"
	TriggerRetry      TriggerKind = "retry"
	TriggerForce      TriggerKind = "force"
)

func (kind TriggerKind) Valid() bool {
	switch kind {
	case TriggerOnce, TriggerRecurring, TriggerEvent, TriggerDeadline,
		TriggerApproval, TriggerDependency, TriggerCorrection, TriggerRetry,
		TriggerForce:
		return true
	default:
		return false
	}
}

type Budget struct {
	MaxTasks           uint32 `json:"max_tasks"`
	MaxSpendMicrounits uint64 `json:"max_spend_microunits"`
}

type ModelBinding struct {
	Provider string `json:"provider"`
	ModelID  string `json:"model_id"`
}

type MGSBinding struct {
	Reference string `json:"reference"`
	Digest    string `json:"digest"`
}

type WakeEnvelope struct {
	SchemaVersion  string       `json:"schema_version"`
	WakeID         string       `json:"wake_id"`
	ScheduleID     string       `json:"schedule_id"`
	TenantID       string       `json:"tenant_id"`
	OrganizationID string       `json:"organization_id"`
	SeatID         string       `json:"seat_id"`
	MandateID      string       `json:"mandate_id"`
	MandateVersion uint64       `json:"mandate_version"`
	Trigger        TriggerKind  `json:"trigger"`
	Reason         string       `json:"reason"`
	ScheduledAt    time.Time    `json:"scheduled_at"`
	Budget         Budget       `json:"budget"`
	Model          ModelBinding `json:"model"`
	MGS            MGSBinding   `json:"mgs"`
	IdempotencyKey string       `json:"idempotency_key"`
	CoalesceKey    string       `json:"coalesce_key"`
	GraphScope     string       `json:"graph_scope"`
}

func (wake WakeEnvelope) Validate() error {
	for name, value := range map[string]string{
		"schema_version": wake.SchemaVersion,
		"wake_id":        wake.WakeID, "schedule_id": wake.ScheduleID,
		"tenant_id": wake.TenantID, "organization_id": wake.OrganizationID,
		"seat_id": wake.SeatID, "mandate_id": wake.MandateID,
		"reason": wake.Reason, "model_provider": wake.Model.Provider,
		"model_id": wake.Model.ModelID, "mgs_reference": wake.MGS.Reference,
		"idempotency_key": wake.IdempotencyKey,
		"coalesce_key":    wake.CoalesceKey, "graph_scope": wake.GraphScope,
	} {
		if err := validText(name, value, 512); err != nil {
			return err
		}
	}
	if wake.SchemaVersion != "workforce.wake.v1" {
		return fmt.Errorf("scheduler: unsupported schema_version")
	}
	if !wake.Trigger.Valid() || wake.MandateVersion == 0 ||
		wake.Budget.MaxTasks == 0 || wake.ScheduledAt.IsZero() ||
		wake.ScheduledAt.Location() != time.UTC {
		return fmt.Errorf("scheduler: trigger, mandate, budget, and UTC schedule are required")
	}
	if len(wake.MGS.Digest) != 64 {
		return fmt.Errorf("scheduler: MGS digest must be 64 lowercase hexadecimal characters")
	}
	for _, char := range wake.MGS.Digest {
		if !strings.ContainsRune("0123456789abcdef", char) {
			return fmt.Errorf("scheduler: MGS digest must be 64 lowercase hexadecimal characters")
		}
	}
	return nil
}

type Config struct {
	MaxOrganizationConcurrency uint32
	MaxSeatConcurrency         uint32
	DailyTaskLimit             uint32
	DailySpendMicrounits       uint64
	QuietHoursStartUTC         int
	QuietHoursEndUTC           int
	ClaimLease                 time.Duration
}

func (config Config) Validate() error {
	if config.MaxOrganizationConcurrency == 0 || config.MaxSeatConcurrency == 0 ||
		config.MaxSeatConcurrency > config.MaxOrganizationConcurrency ||
		config.DailyTaskLimit == 0 || config.ClaimLease <= 0 ||
		config.QuietHoursStartUTC < 0 || config.QuietHoursStartUTC > 23 ||
		config.QuietHoursEndUTC < 0 || config.QuietHoursEndUTC > 23 {
		return fmt.Errorf("scheduler: invalid limits, quiet hours, or claim lease")
	}
	return nil
}

type EnqueueResult struct {
	WakeID       string
	Deduplicated bool
	Coalesced    bool
}

type Claim struct {
	Envelope  WakeEnvelope
	ClaimedAt time.Time
}

func validText(name, value string, max int) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > max {
		return fmt.Errorf("scheduler: %s must contain 1 to %d bytes", name, max)
	}
	return nil
}
