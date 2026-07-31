// Package controlapi owns the authenticated human Workforce control plane.
package controlapi

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
)

const SchemaVersion = "workforce.control.v1"

type Principal struct {
	TenantID       string
	OrganizationID contracts.OrganizationID
	OwnerID        contracts.OwnerID
}

type ResourceItem struct {
	ID        string         `json:"id"`
	Version   uint64         `json:"version"`
	UpdatedAt time.Time      `json:"updated_at"`
	Fields    map[string]any `json:"fields"`
}

type ResourcePage struct {
	SchemaVersion string         `json:"schema_version"`
	Resource      string         `json:"resource"`
	Items         []ResourceItem `json:"items"`
	NextCursor    string         `json:"next_cursor,omitempty"`
}

type LifecycleEvent struct {
	SchemaVersion      string                   `json:"schema_version"`
	Cursor             uint64                   `json:"cursor"`
	ID                 string                   `json:"event_id"`
	OrganizationID     contracts.OrganizationID `json:"organization_id"`
	Type               string                   `json:"event_type"`
	ResourceKind       string                   `json:"resource_kind"`
	ResourceID         string                   `json:"resource_id"`
	ResourceVersion    uint64                   `json:"resource_version"`
	VerifiedCompletion bool                     `json:"verified_completion"`
	ReceiptID          contracts.ReceiptID      `json:"receipt_id,omitempty"`
	Fields             map[string]any           `json:"fields"`
	CreatedAt          time.Time                `json:"created_at"`
}

type EventPage struct {
	SchemaVersion string           `json:"schema_version"`
	Events        []LifecycleEvent `json:"events"`
	NextCursor    uint64           `json:"next_cursor"`
}

type SignedCommand struct {
	SchemaVersion   string                   `json:"schema_version"`
	ID              string                   `json:"command_id"`
	OrganizationID  contracts.OrganizationID `json:"organization_id"`
	OwnerID         contracts.OwnerID        `json:"owner_id"`
	Action          string                   `json:"action"`
	ResourceKind    string                   `json:"resource_kind"`
	ResourceID      string                   `json:"resource_id"`
	ExpectedVersion uint64                   `json:"expected_version"`
	Change          json.RawMessage          `json:"change"`
	EffectiveAt     time.Time                `json:"effective_at"`
	Signature       contracts.Signature      `json:"signature"`
}

type CommandResult struct {
	SchemaVersion string `json:"schema_version"`
	CommandID     string `json:"command_id"`
	Version       uint64 `json:"version"`
	EventCursor   uint64 `json:"event_cursor"`
}

type ControlKeyRegistration struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
}

// ActivationPreviewRequest names the locally signed canonical organization
// draft the owner intends to activate.
type ActivationPreviewRequest struct {
	Name        string    `json:"name"`
	KeyID       string    `json:"key_id"`
	EffectiveAt time.Time `json:"effective_at"`
}

// ActivationPreview contains the exact canonical records the browser signs.
type ActivationPreview struct {
	SchemaVersion  string                  `json:"schema_version"`
	Seed           policy.Seed             `json:"seed"`
	SkillContracts []skills.SignedContract `json:"skill_contracts"`
}

type ActivationBundle struct {
	Seed           policy.Seed             `json:"seed"`
	SkillContracts []skills.SignedContract `json:"skill_contracts"`
}

// ActivationResult reports the durable organization projection created by an
// activation request.
type ActivationResult struct {
	SchemaVersion  string `json:"schema_version"`
	OrganizationID string `json:"organization_id"`
	Departments    int    `json:"departments"`
	Seats          int    `json:"seats"`
	Deduplicated   bool   `json:"deduplicated"`
	EventCursor    uint64 `json:"event_cursor,omitempty"`
}

func SignCommand(command *SignedCommand, keyID string, key ed25519.PrivateKey) error {
	if command == nil || len(key) != ed25519.PrivateKeySize || strings.TrimSpace(keyID) == "" {
		return fmt.Errorf("controlapi: command and signing authority are required")
	}
	command.Signature = signaturePlaceholder(keyID)
	payload, err := commandSigningBytes(*command)
	if err != nil {
		return err
	}
	command.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	return command.Validate()
}

func (command SignedCommand) Validate() error {
	if command.SchemaVersion != SchemaVersion || command.ID == "" ||
		command.OrganizationID == "" || command.OwnerID == "" ||
		command.ResourceKind == "" || command.ResourceID == "" ||
		!validAction(command.Action) || command.EffectiveAt.IsZero() ||
		command.EffectiveAt.Location() != time.UTC {
		return fmt.Errorf("controlapi: signed command is incomplete")
	}
	if len(command.Change) == 0 || len(command.Change) > 1<<20 ||
		!json.Valid(command.Change) || command.Signature.Validate() != nil {
		return fmt.Errorf("controlapi: signed command payload is invalid")
	}
	return nil
}

func verifyCommand(command SignedCommand, keyID string, key ed25519.PublicKey) error {
	if err := command.Validate(); err != nil {
		return err
	}
	if command.Signature.KeyID != keyID || len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("controlapi: signing authority mismatch")
	}
	signature, err := base64.RawURLEncoding.DecodeString(command.Signature.Value)
	if err != nil {
		return fmt.Errorf("controlapi: invalid command signature")
	}
	payload, err := commandSigningBytes(command)
	if err != nil || !ed25519.Verify(key, payload, signature) {
		return fmt.Errorf("controlapi: invalid command signature")
	}
	return nil
}

func commandSigningBytes(command SignedCommand) ([]byte, error) {
	command.Signature = signaturePlaceholder(command.Signature.KeyID)
	var change any
	decoder := json.NewDecoder(strings.NewReader(string(command.Change)))
	decoder.UseNumber()
	if err := decoder.Decode(&change); err != nil {
		return nil, err
	}
	command.Change, _ = json.Marshal(change)
	return json.Marshal(command)
}

func commandHash(command SignedCommand) (string, error) {
	payload, err := commandSigningBytes(command)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:]), nil
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func decodePublicKey(value string) (ed25519.PublicKey, error) {
	decoded, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(decoded) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("controlapi: public key must be a base64url Ed25519 key")
	}
	return ed25519.PublicKey(decoded), nil
}

func validAction(value string) bool {
	switch value {
	case "set_policy", "set_mandate", "set_autonomy", "set_schedule",
		"cancel_work", "force_wake", "approve_batch":
		return true
	default:
		return false
	}
}
