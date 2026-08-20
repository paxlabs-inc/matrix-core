package codingworker

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

type SessionID string
type StoredSessionID string
type RequestID string

func (id SessionID) Valid() bool       { return strings.TrimSpace(string(id)) != "" }
func (id StoredSessionID) Valid() bool { return strings.TrimSpace(string(id)) != "" }

type RPCError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *RPCError) Error() string {
	if e == nil {
		return ""
	}
	if e.Code == 0 {
		return e.Message
	}
	return fmt.Sprintf("agentcore rpc %d: %s", e.Code, e.Message)
}

type Event struct {
	Type      string          `json:"type"`
	SessionID SessionID       `json:"session_id,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"`
}

type Session struct {
	RuntimeID    SessionID       `json:"session_id"`
	StoredID     StoredSessionID `json:"stored_session_id,omitempty"`
	ResumedID    StoredSessionID `json:"resumed,omitempty"`
	SessionKey   StoredSessionID `json:"session_key,omitempty"`
	Status       string          `json:"status,omitempty"`
	Running      bool            `json:"running,omitempty"`
	MessageCount int             `json:"message_count,omitempty"`
	Messages     json.RawMessage `json:"messages,omitempty"`
	Info         json.RawMessage `json:"info,omitempty"`
}

func (s Session) DurableID() StoredSessionID {
	for _, id := range []StoredSessionID{s.StoredID, s.SessionKey, s.ResumedID} {
		if id.Valid() {
			return id
		}
	}
	return ""
}

func (s Session) validate() error {
	if !s.RuntimeID.Valid() {
		return errors.New("agentcore returned an empty runtime session id")
	}
	return nil
}

type CreateOptions struct {
	CWD               string          `json:"cwd"`
	Title             string          `json:"title,omitempty"`
	Source            string          `json:"source,omitempty"`
	Model             string          `json:"model,omitempty"`
	Provider          string          `json:"provider,omitempty"`
	ReasoningEffort   string          `json:"reasoning_effort,omitempty"`
	Fast              *bool           `json:"fast,omitempty"`
	CloseOnDisconnect bool            `json:"close_on_disconnect"`
	Messages          json.RawMessage `json:"messages,omitempty"`
}

type ResumeOptions struct {
	CWD               string `json:"cwd,omitempty"`
	Source            string `json:"source,omitempty"`
	CloseOnDisconnect bool   `json:"close_on_disconnect"`
}

type BranchOptions struct {
	Name  string `json:"name,omitempty"`
	Count int    `json:"count,omitempty"`
}

type PromptResult struct {
	Status string `json:"status"`
}

type ApprovalResult struct {
	Resolved bool `json:"resolved"`
}

type ApprovalChoice string

const (
	ApproveOnce    ApprovalChoice = "once"
	ApproveSession ApprovalChoice = "session"
	ApproveAlways  ApprovalChoice = "always"
	DenyApproval   ApprovalChoice = "deny"
)

func (choice ApprovalChoice) Valid() bool {
	switch choice {
	case ApproveOnce, ApproveSession, ApproveAlways, DenyApproval:
		return true
	default:
		return false
	}
}

type InterruptResult struct {
	Status string `json:"status"`
}

type CloseResult struct {
	Closed bool `json:"closed"`
}

type Verification struct {
	Status   string          `json:"status"`
	Evidence json.RawMessage `json:"evidence,omitempty"`
}

type VerificationResult struct {
	Verification Verification `json:"verification"`
}

type Health struct {
	OK           bool   `json:"ok"`
	Version      string `json:"version"`
	AuthRequired bool   `json:"auth_required"`
}

type ClientConfig struct {
	Endpoint       string
	Token          []byte
	ConnectTimeout time.Duration
	RequestTimeout time.Duration
	HealthTimeout  time.Duration
	EventBuffer    int
	MaxFrameBytes  int64
}

func (c ClientConfig) normalized() ClientConfig {
	if c.ConnectTimeout <= 0 {
		c.ConnectTimeout = 15 * time.Second
	}
	if c.RequestTimeout <= 0 {
		c.RequestTimeout = 2 * time.Minute
	}
	if c.HealthTimeout <= 0 {
		c.HealthTimeout = 5 * time.Second
	}
	if c.EventBuffer <= 0 {
		c.EventBuffer = 1024
	}
	if c.MaxFrameBytes <= 0 {
		c.MaxFrameBytes = 16 << 20
	}
	return c
}

var (
	ErrClosed            = errors.New("agentcore connection closed")
	ErrEventBackpressure = errors.New("agentcore event consumer fell behind")
	ErrSessionMismatch   = errors.New("agentcore resumed a different durable session")
)
