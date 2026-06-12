// Package types holds the wire contracts shared across chronosd: the uniform
// response envelope, error codes, the agent-DID auth lane, and the alarm
// request/response shapes the MCP proxy (tools/chronos/chronos.mjs) speaks.
//
// The envelope mirrors tachyond / uwacd ({ok,data,error}) so the MCP proxy can
// branch on ok=false without bespoke parsing.
package types

import (
	"encoding/json"
	"time"
)

// Envelope is the uniform {ok,data,error} response shape.
type Envelope struct {
	Ok    bool   `json:"ok"`
	Data  any    `json:"data,omitempty"`
	Error *Error `json:"error,omitempty"`
}

// Error is a structured, machine-branchable failure.
type Error struct {
	Code      string `json:"code"`
	Message   string `json:"message"`
	Retryable bool   `json:"retryable,omitempty"`
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// Error codes (stable; the planner/agent may branch on these).
const (
	CodeInvalidRequest = "invalid_request"
	CodeUnauthorized   = "unauthorized" // transport/principal auth failed
	CodeNotFound       = "not_found"    // alarm id unknown / not owned
	CodeConflict       = "conflict"     // idempotency-key clash with a different alarm
	CodeInternal       = "internal"
)

// OK wraps a success payload.
func OK(data any) Envelope { return Envelope{Ok: true, Data: data} }

// Fail wraps an error.
func Fail(err *Error) Envelope { return Envelope{Ok: false, Error: err} }

// NewError constructs an *Error.
func NewError(code, message string, retryable bool) *Error {
	return &Error{Code: code, Message: message, Retryable: retryable}
}

// Alarm kinds. The three user cases collapse onto two: "in 10 minutes" + "at a
// specific time" = once; "every day/hour/N" = cron.
const (
	KindOnce = "once"
	KindCron = "cron"
)

// Alarm lifecycle statuses.
const (
	StatusActive    = "active"
	StatusFired     = "fired"     // a once alarm that has fired (retained for audit)
	StatusCancelled = "cancelled" // explicitly cancelled by the owner
	StatusFailed    = "failed"    // wake delivery exhausted max_failures (once only)
)

// Alarm is one scheduled wake — the durable timer. It maps 1:1 onto a row of
// the alarms table (see migrations/001_init.sql).
type Alarm struct {
	ID             string
	OwnerDID       string // full agent DID: did:matrix:<user_id>:<keyfp>
	UserID         string // Supabase user UUID from the DID label — the wake target
	Label          string
	Kind           string // once | cron
	FireAt         *time.Time
	CronExpr       string
	Timezone       string
	NextFireAt     time.Time
	ConversationID string
	WakeMessage    string
	Payload        json.RawMessage
	Status         string
	IdempotencyKey string
	MaxFailures    int
	FailureCount   int
	LastError      string
	ClaimedAt      *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
	LastFiredAt    *time.Time
}

// View is the JSON projection of an alarm returned to the agent. High-entropy
// fields (payload, wake_message, ids) are passed through verbatim (invariant i4).
type View struct {
	ID             string          `json:"id"`
	Label          string          `json:"label"`
	Kind           string          `json:"kind"`
	CronExpr       string          `json:"cron_expr,omitempty"`
	Timezone       string          `json:"timezone,omitempty"`
	NextFireAt     *time.Time      `json:"next_fire_at,omitempty"`
	ConversationID string          `json:"conversation_id,omitempty"`
	WakeMessage    string          `json:"wake_message"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	Status         string          `json:"status"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	MaxFailures    int             `json:"max_failures"`
	FailureCount   int             `json:"failure_count"`
	LastError      string          `json:"last_error,omitempty"`
	CreatedAt      time.Time       `json:"created_at"`
	LastFiredAt    *time.Time      `json:"last_fired_at,omitempty"`
}

// ViewOf projects an Alarm onto its JSON wire shape.
func ViewOf(a Alarm) View {
	v := View{
		ID:             a.ID,
		Label:          a.Label,
		Kind:           a.Kind,
		CronExpr:       a.CronExpr,
		Timezone:       a.Timezone,
		ConversationID: a.ConversationID,
		WakeMessage:    a.WakeMessage,
		Payload:        a.Payload,
		Status:         a.Status,
		IdempotencyKey: a.IdempotencyKey,
		MaxFailures:    a.MaxFailures,
		FailureCount:   a.FailureCount,
		LastError:      a.LastError,
		CreatedAt:      a.CreatedAt,
		LastFiredAt:    a.LastFiredAt,
	}
	if a.Status == StatusActive {
		nf := a.NextFireAt
		v.NextFireAt = &nf
	}
	return v
}

// CreateAlarmRequest is POST /v1/alarms (and the alarm_set MCP tool args).
type CreateAlarmRequest struct {
	Label string `json:"label"`
	Kind  string `json:"kind"` // once | cron
	// once: exactly one of DelaySeconds / FireAt.
	DelaySeconds int64  `json:"delay_seconds,omitempty"`
	FireAt       string `json:"fire_at,omitempty"` // RFC3339 absolute instant
	// cron: a 5-field expression, @descriptor, or @every Nm.
	CronExpr string `json:"cron_expr,omitempty"`
	Timezone string `json:"timezone,omitempty"` // IANA tz for cron (default UTC)

	ConversationID string          `json:"conversation_id,omitempty"`
	WakeMessage    string          `json:"wake_message"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	MaxFailures    int             `json:"max_failures,omitempty"`
}

// CreateAlarmResponse is returned on a successful create.
type CreateAlarmResponse struct {
	ID         string    `json:"id"`
	NextFireAt time.Time `json:"next_fire_at"`
	Status     string    `json:"status"`
}

// ChallengeRequest opens the agent-DID principal-auth lane.
type ChallengeRequest struct {
	DID string `json:"did"`
}

// ChallengeResponse carries the exact bytes the agent must ed25519-sign.
type ChallengeResponse struct {
	DID       string `json:"did"`
	Nonce     string `json:"nonce"`
	Message   string `json:"message"`
	ExpiresIn int    `json:"expires_in"`
}

// VerifyRequest proves possession of the DID's key over the challenge.
type VerifyRequest struct {
	DID       string `json:"did"`
	PublicKey string `json:"public_key"` // hex(ed25519 pubkey)
	Nonce     string `json:"nonce"`
	Signature string `json:"signature"` // hex(ed25519 signature)
}

// VerifyResponse returns a short-lived principal token bound to the owner.
type VerifyResponse struct {
	Token       string `json:"token"`
	OwnerUserID string `json:"owner_user_id"`
	ExpiresIn   int    `json:"expires_in"`
}
