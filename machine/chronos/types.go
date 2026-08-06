// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package chronos

import (
	"context"
	"encoding/json"
	"errors"
	"time"
)

var (
	ErrClosed   = errors.New("local chronos store is closed")
	ErrNotFound = errors.New("local chronos alarm not found")
	ErrConflict = errors.New("local chronos idempotency conflict")
	ErrLease    = errors.New("local chronos lease does not match")
)

type Status string

const (
	StatusScheduled Status = "scheduled"
	StatusLeased    Status = "leased"
	StatusCompleted Status = "completed"
	StatusCanceled  Status = "canceled"
	StatusSkipped   Status = "skipped"
	StatusFailed    Status = "failed"
)

type MisfirePolicy string

const (
	MisfireFireOnce MisfirePolicy = "fire_once"
	MisfireSkip     MisfirePolicy = "skip"
	MisfireCoalesce MisfirePolicy = "coalesce"
)

type Body struct {
	Payload        json.RawMessage `json:"payload"`
	WakeMessage    string          `json:"wake_message"`
	ConversationID string          `json:"conversation_id,omitempty"`
	Label          string          `json:"label,omitempty"`
	Target         string          `json:"target,omitempty"`
	MaxFailures    int             `json:"max_failures,omitempty"`
}

type CreateRequest struct {
	ID             string
	IdempotencyKey string
	NextFire       time.Time
	Interval       time.Duration
	CronExpr       string
	Timezone       string
	MisfirePolicy  MisfirePolicy
	Body           Body
}

type Alarm struct {
	ID               string
	IdempotencyKey   string
	NextFire         time.Time
	Interval         time.Duration
	CronExpr         string
	Timezone         string
	MisfirePolicy    MisfirePolicy
	Status           Status
	Body             Body
	LeaseUntil       time.Time
	DeliveryAttempts int
	LastError        string
	LastFiredAt      time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
}

type Claim struct {
	Alarm        Alarm
	LeaseToken   string
	ScheduledFor time.Time
}

type Recovery struct {
	RecoveredLeases int
	Skipped         int
	Coalesced       int
}

type Delivery struct {
	Alarm        Alarm
	ScheduledFor time.Time
}

type Target interface {
	Deliver(context.Context, Delivery) error
}
