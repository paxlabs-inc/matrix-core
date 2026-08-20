// Package mail owns durable typed Workforce Mail delivery and consumption.
package mail

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
)

var (
	// ErrConflict means an immutable message or consumption identity was reused.
	ErrConflict = errors.New("mail identity conflict")
	// ErrUnauthorized means a sender, recipient, signature, or delegation is invalid.
	ErrUnauthorized = errors.New("mail authority denied")
	// ErrQuota means a mailbox, thread, attachment, or automatic-reply bound was reached.
	ErrQuota = errors.New("mail protocol quota exceeded")
	// ErrNotReady means delivery or binding has not reached a consumable state.
	ErrNotReady = errors.New("mail delivery is not ready")
	// ErrExpired means the message can no longer be consumed.
	ErrExpired = errors.New("mail message expired")
	// ErrUncertain means durable mail state cannot be established and fails closed.
	ErrUncertain = errors.New("mail state is uncertain")
)

// DeliveryState is the closed per-recipient mailbox lifecycle.
type DeliveryState string

const (
	StateQueued       DeliveryState = "queued"
	StateDelivered    DeliveryState = "delivered"
	StateOpened       DeliveryState = "opened"
	StateAcknowledged DeliveryState = "acknowledged"
	StateReplied      DeliveryState = "replied"
	StateResolved     DeliveryState = "resolved"
	StateExpired      DeliveryState = "expired"
	StateRejected     DeliveryState = "rejected"
	StateCancelled    DeliveryState = "cancelled"
	StateCorrected    DeliveryState = "corrected"
)

// Valid reports whether the delivery state is recognized.
func (state DeliveryState) Valid() bool {
	switch state {
	case StateQueued, StateDelivered, StateOpened, StateAcknowledged,
		StateReplied, StateResolved, StateExpired, StateRejected,
		StateCancelled, StateCorrected:
		return true
	default:
		return false
	}
}

// Terminal reports whether no further recipient transition is allowed.
func (state DeliveryState) Terminal() bool {
	switch state {
	case StateResolved, StateExpired, StateRejected, StateCancelled, StateCorrected:
		return true
	default:
		return false
	}
}

// Config defines hard mail resource and loop bounds.
type Config struct {
	MaxMailboxMessages uint32
	MaxThreadMessages  uint32
	MaxThreadDepth     uint32
	MaxRecipients      uint32
	MaxAutoReplies     uint32
	MaxAttachmentBytes uint64
	MaxMessageLifetime time.Duration
}

// Validate rejects absent or unreasonable mail bounds.
func (config Config) Validate() error {
	if config.MaxMailboxMessages == 0 || config.MaxMailboxMessages > 100000 ||
		config.MaxThreadMessages == 0 || config.MaxThreadMessages > 10000 ||
		config.MaxThreadDepth == 0 || config.MaxThreadDepth > 256 ||
		config.MaxRecipients == 0 || config.MaxRecipients > 64 ||
		config.MaxAutoReplies == 0 || config.MaxAutoReplies > 1000 ||
		config.MaxAttachmentBytes == 0 || config.MaxAttachmentBytes > 1<<30 ||
		config.MaxMessageLifetime <= 0 || config.MaxMessageLifetime > 365*24*time.Hour {
		return fmt.Errorf("mail: configuration bounds are invalid")
	}
	return nil
}

// SeatKey binds one seat address to a real Ed25519 verification key.
type SeatKey struct {
	Address     contracts.SeatAddress
	KeyID       string
	PublicKey   ed25519.PublicKey
	EffectiveAt time.Time
}

// DelegationBinding is the exact graph mutation requested by a delegation message.
type DelegationBinding struct {
	Node dependency.Node `json:"node"`
	Edge dependency.Edge `json:"edge"`
}

// CorrectionBinding binds mail delivery to a durable ledger correction notice.
type CorrectionBinding struct {
	NoticeID        string                 `json:"notice_id"`
	CorrectionID    contracts.CorrectionID `json:"correction_id"`
	AffectedRecord  contracts.RecordID     `json:"affected_record_id"`
	RecipientSeatID contracts.SeatID       `json:"recipient_seat_id"`
}

// SendOptions are kernel-owned delivery controls outside the signed envelope.
type SendOptions struct {
	Automatic  bool
	Delegation *DelegationBinding
	Correction *CorrectionBinding
}

// SendResult is the durable immutable send projection.
type SendResult struct {
	MessageID    contracts.MessageID
	EnvelopeHash contracts.ContentHash
	Deduplicated bool
	BindingReady bool
}

// Delivery is one recipient-facing mailbox projection.
type Delivery struct {
	MessageID      contracts.MessageID
	ThreadID       contracts.ThreadID
	Recipient      contracts.SeatAddress
	State          DeliveryState
	Priority       int32
	CreatedAt      time.Time
	ExpiresAt      time.Time
	ConsumptionKey string
	BindingReady   bool
	Automatic      bool
	RequiredAction string
	TimeoutAction  contracts.TimeoutAction
	ParentIntentID contracts.IntentID
}

// ConsumeRequest identifies one idempotent message opening.
type ConsumeRequest struct {
	OrganizationID contracts.OrganizationID
	SeatID         contracts.SeatID
	MessageID      contracts.MessageID
	IdempotencyKey string
}

// TransitionRequest identifies one authorized recipient lifecycle transition.
type TransitionRequest struct {
	OrganizationID contracts.OrganizationID
	SeatID         contracts.SeatID
	MessageID      contracts.MessageID
	State          DeliveryState
	IdempotencyKey string
}

// SignEnvelope signs the canonical unsigned envelope with one seat key.
func SignEnvelope(
	envelope *contracts.MessageEnvelope,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	if envelope == nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("mail: envelope and Ed25519 private key are required")
	}
	if err := validateToken("key_id", keyID); err != nil {
		return err
	}
	payload, err := SigningBytes(*envelope)
	if err != nil {
		return err
	}
	envelope.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return envelope.Validate()
}

// SigningBytes returns the canonical unsigned message bytes.
func SigningBytes(envelope contracts.MessageEnvelope) ([]byte, error) {
	copyEnvelope := envelope
	copyEnvelope.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: "unsigned",
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
	if err := copyEnvelope.Validate(); err != nil {
		return nil, err
	}
	payload := unsignedEnvelope{
		SchemaVersion: envelope.SchemaVersion, ID: envelope.ID,
		ThreadID: envelope.ThreadID, InReplyTo: envelope.InReplyTo,
		From: envelope.From, To: envelope.To, CC: envelope.CC, Kind: envelope.Kind,
		Subject: envelope.Subject, Payload: envelope.Payload,
		ParentIntentID: envelope.ParentIntentID, RequiredAction: envelope.RequiredAction,
		Artifacts: envelope.Artifacts, Evidence: envelope.Evidence, Priority: envelope.Priority,
		Deadline: envelope.Deadline, TimeoutAction: envelope.TimeoutAction,
		Classification: envelope.Classification, IdempotencyKey: envelope.IdempotencyKey,
		CreatedAt: envelope.CreatedAt, ExpiresAt: envelope.ExpiresAt,
	}
	return json.Marshal(payload)
}

type unsignedEnvelope struct {
	SchemaVersion  string                      `json:"schema_version"`
	ID             contracts.MessageID         `json:"message_id"`
	ThreadID       contracts.ThreadID          `json:"thread_id"`
	InReplyTo      *contracts.MessageID        `json:"in_reply_to"`
	From           contracts.SeatAddress       `json:"from"`
	To             []contracts.SeatAddress     `json:"to"`
	CC             []contracts.SeatAddress     `json:"cc"`
	Kind           contracts.MessageKind       `json:"kind"`
	Subject        string                      `json:"subject"`
	Payload        contracts.MessagePayloadRef `json:"payload"`
	ParentIntentID contracts.IntentID          `json:"parent_intent_id"`
	RequiredAction string                      `json:"required_action"`
	Artifacts      []contracts.ArtifactRef     `json:"artifacts"`
	Evidence       []contracts.EvidenceRef     `json:"evidence"`
	Priority       int32                       `json:"priority"`
	Deadline       *time.Time                  `json:"deadline"`
	TimeoutAction  contracts.TimeoutAction     `json:"timeout_action"`
	Classification contracts.Classification    `json:"classification"`
	IdempotencyKey string                      `json:"idempotency_key"`
	CreatedAt      time.Time                   `json:"created_at"`
	ExpiresAt      time.Time                   `json:"expires_at"`
}

type bindingEnvelope struct {
	Kind       contracts.MessageKind `json:"kind"`
	Delegation *DelegationBinding    `json:"delegation,omitempty"`
	Correction *CorrectionBinding    `json:"correction,omitempty"`
}

func (options SendOptions) binding(kind contracts.MessageKind) (bindingEnvelope, error) {
	switch kind {
	case contracts.MessageDelegation:
		if options.Delegation == nil || options.Correction != nil {
			return bindingEnvelope{}, fmt.Errorf("mail: delegation binding is required")
		}
		return bindingEnvelope{Kind: kind, Delegation: options.Delegation}, nil
	case contracts.MessageCorrection:
		if options.Correction == nil || options.Delegation != nil {
			return bindingEnvelope{}, fmt.Errorf("mail: correction binding is required")
		}
		return bindingEnvelope{Kind: kind, Correction: options.Correction}, nil
	default:
		if options.Delegation != nil || options.Correction != nil {
			return bindingEnvelope{}, fmt.Errorf("mail: binding is forbidden for %q", kind)
		}
		return bindingEnvelope{Kind: kind}, nil
	}
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("mail: %s must contain 1 to 128 bytes", name)
	}
	for _, char := range value {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' ||
			char >= '0' && char <= '9' || char == '-' || char == '_' ||
			char == '.' || char == ':' {
			continue
		}
		return fmt.Errorf("mail: %s contains an invalid character", name)
	}
	return nil
}
