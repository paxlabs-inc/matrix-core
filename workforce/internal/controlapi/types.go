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

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/mission"
	"centra/workforce/internal/policy"
	"centra/workforce/internal/skills"
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
	Name        string                  `json:"name"`
	KeyID       string                  `json:"key_id"`
	EffectiveAt time.Time               `json:"effective_at"`
	Authority   mission.ActivationDraft `json:"authority"`
}

// MigrationPreviewRequest names the founder authority to add above a current
// qualified v1 organization without re-signing or reinterpreting v1 records.
type MigrationPreviewRequest struct {
	KeyID       string                  `json:"key_id"`
	EffectiveAt time.Time               `json:"effective_at"`
	Authority   mission.ActivationDraft `json:"authority"`
}

// MigrationPreview contains the exact company authority records signed by the
// browser during a v1-to-v2 migration.
type MigrationPreview struct {
	SchemaVersion             string                      `json:"schema_version"`
	Authority                 mission.ActivationAuthority `json:"authority"`
	LegacyOrganizationVersion uint64                      `json:"legacy_organization_version"`
	Impact                    MigrationImpactPreview      `json:"impact"`
}

// MigrationImpactPreview is the exact, server-observed compatibility and
// consequence report shown before the owner signs a v1-to-v2 migration.
type MigrationImpactPreview struct {
	CurrentDepartments        int64    `json:"current_departments"`
	CurrentSeats              int64    `json:"current_seats"`
	CurrentWorkNodes          int64    `json:"current_work_nodes"`
	CurrentMailRecords        int64    `json:"current_mail_records"`
	CurrentReceipts           int64    `json:"current_receipts"`
	CurrentSkillContracts     int64    `json:"current_skill_contracts"`
	ActiveAuthorityLeases     int64    `json:"active_authority_leases"`
	TargetTemplateID          string   `json:"target_template_id"`
	TargetTemplateVersion     uint64   `json:"target_template_version"`
	NewAuthorityKinds         []string `json:"new_authority_kinds"`
	StartingMicrounits        uint64   `json:"starting_microunits"`
	SpendCeilingMicrounits    uint64   `json:"spend_ceiling_microunits"`
	ExposureCeilingMicrounits uint64   `json:"exposure_ceiling_microunits"`
	Compatibility             string   `json:"compatibility"`
	Effect                    string   `json:"effect"`
	RollbackPoint             string   `json:"rollback_point"`
	IrreversibleConsequences  []string `json:"irreversible_consequences"`
}

// MigrationBundle is the exact locally signed migration preview.
type MigrationBundle struct {
	Authority                 mission.ActivationAuthority `json:"authority"`
	LegacyOrganizationVersion uint64                      `json:"legacy_organization_version"`
}

// AuthorityChangePreviewRequest describes a complete founder-reviewed material
// change; partial unsigned field mutation is intentionally unsupported.
type AuthorityChangePreviewRequest struct {
	KeyID           string                  `json:"key_id"`
	ExpectedVersion uint64                  `json:"expected_version"`
	EffectiveAt     time.Time               `json:"effective_at"`
	Authority       mission.ActivationDraft `json:"authority"`
}

// AuthorityChangePreview contains the exact next canonical authority version.
type AuthorityChangePreview struct {
	SchemaVersion   string                       `json:"schema_version"`
	ExpectedVersion uint64                       `json:"expected_version"`
	Authority       mission.ActivationAuthority  `json:"authority"`
	Impact          AuthorityChangeImpactPreview `json:"impact"`
}

// AuthorityChangeImpactPreview is the server-observed exact mutation radius
// presented before the founder signs the next authority version.
type AuthorityChangeImpactPreview struct {
	CurrentVersion             uint64   `json:"current_version"`
	ProposedVersion            uint64   `json:"proposed_version"`
	AffectedAuthorityKinds     []string `json:"affected_authority_kinds"`
	ActiveAuthorityLeases      int64    `json:"active_authority_leases"`
	ActiveRuntimeLeases        int64    `json:"active_runtime_leases"`
	QueuedWakes                int64    `json:"queued_wakes"`
	DispatchedWakes            int64    `json:"dispatched_wakes"`
	UnsettledEffects           int64    `json:"unsettled_effects"`
	InvalidatedLeaseCount      int64    `json:"invalidated_lease_count"`
	CompanyStateAfterCommit    string   `json:"company_state_after_commit"`
	NewInitiationAfterCommit   string   `json:"new_initiation_after_commit"`
	ExternalEffectsAfterCommit string   `json:"external_effects_after_commit"`
}

// AuthorityChangeBundle is the locally signed exact change preview.
type AuthorityChangeBundle struct {
	ExpectedVersion uint64                      `json:"expected_version"`
	Authority       mission.ActivationAuthority `json:"authority"`
}

// FounderKeyRotation is the dual-signed, owner-reserved proof that moves the
// live company authority root from one Ed25519 key to another. Both signature
// placeholders are part of the canonical payload so neither signer can be
// substituted after either proof is made.
type FounderKeyRotation struct {
	SchemaVersion   string                   `json:"schema_version"`
	OrganizationID  contracts.OrganizationID `json:"organization_id"`
	OwnerID         contracts.OwnerID        `json:"owner_id"`
	ExpectedVersion uint64                   `json:"expected_version"`
	EffectiveAt     time.Time                `json:"effective_at"`
	OldKeyID        string                   `json:"old_key_id"`
	NewKeyID        string                   `json:"new_key_id"`
	NewPublicKey    string                   `json:"new_public_key"`
	OldSignature    contracts.Signature      `json:"old_signature"`
	NewSignature    contracts.Signature      `json:"new_signature"`
}

type FounderKeyRotationPreviewRequest struct {
	OldKeyID        string                  `json:"old_key_id"`
	NewKeyID        string                  `json:"new_key_id"`
	NewPublicKey    string                  `json:"new_public_key"`
	ExpectedVersion uint64                  `json:"expected_version"`
	EffectiveAt     time.Time               `json:"effective_at"`
	Authority       mission.ActivationDraft `json:"authority"`
}

type FounderKeyRotationPreview struct {
	SchemaVersion string                      `json:"schema_version"`
	Rotation      FounderKeyRotation          `json:"rotation"`
	Authority     mission.ActivationAuthority `json:"authority"`
}

type FounderKeyRotationBundle struct {
	Rotation  FounderKeyRotation          `json:"rotation"`
	Authority mission.ActivationAuthority `json:"authority"`
}

// ActivationPreview contains the exact canonical records the browser signs.
type ActivationPreview struct {
	SchemaVersion  string                      `json:"schema_version"`
	Seed           policy.Seed                 `json:"seed"`
	Authority      mission.ActivationAuthority `json:"authority"`
	SkillContracts []skills.SignedContract     `json:"skill_contracts"`
}

type ActivationBundle struct {
	Seed           policy.Seed                 `json:"seed"`
	Authority      mission.ActivationAuthority `json:"authority"`
	SkillContracts []skills.SignedContract     `json:"skill_contracts"`
}

// ActivationResult reports the durable organization projection created by an
// activation request.
type ActivationResult struct {
	SchemaVersion       string `json:"schema_version"`
	OrganizationID      string `json:"organization_id"`
	Departments         int    `json:"departments"`
	Seats               int    `json:"seats"`
	MissionVersion      uint64 `json:"mission_version"`
	ConstitutionVersion uint64 `json:"constitution_version"`
	OrganizationVersion uint64 `json:"organization_version"`
	OrganizationSchema  string `json:"organization_schema"`
	Deduplicated        bool   `json:"deduplicated"`
	EventCursor         uint64 `json:"event_cursor,omitempty"`
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

func SignFounderKeyRotation(
	rotation *FounderKeyRotation,
	oldPrivateKey, newPrivateKey ed25519.PrivateKey,
) error {
	if rotation == nil || len(oldPrivateKey) != ed25519.PrivateKeySize ||
		len(newPrivateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("controlapi: rotation and both signing authorities are required")
	}
	rotation.OldSignature = signaturePlaceholder(rotation.OldKeyID)
	rotation.NewSignature = signaturePlaceholder(rotation.NewKeyID)
	payload, err := founderKeyRotationSigningBytes(*rotation)
	if err != nil {
		return err
	}
	rotation.OldSignature.Value = base64.RawURLEncoding.EncodeToString(
		ed25519.Sign(oldPrivateKey, payload),
	)
	rotation.NewSignature.Value = base64.RawURLEncoding.EncodeToString(
		ed25519.Sign(newPrivateKey, payload),
	)
	return rotation.Validate()
}

func (rotation FounderKeyRotation) Validate() error {
	if rotation.SchemaVersion != SchemaVersion || rotation.OrganizationID == "" ||
		rotation.OwnerID == "" || rotation.ExpectedVersion == 0 ||
		rotation.EffectiveAt.IsZero() || rotation.EffectiveAt.Location() != time.UTC ||
		strings.TrimSpace(rotation.OldKeyID) == "" ||
		strings.TrimSpace(rotation.NewKeyID) == "" ||
		rotation.OldKeyID == rotation.NewKeyID ||
		rotation.OldSignature.KeyID != rotation.OldKeyID ||
		rotation.NewSignature.KeyID != rotation.NewKeyID ||
		rotation.OldSignature.Validate() != nil ||
		rotation.NewSignature.Validate() != nil {
		return fmt.Errorf("controlapi: founder key rotation is incomplete")
	}
	if _, err := decodePublicKey(rotation.NewPublicKey); err != nil {
		return err
	}
	return nil
}

func verifyFounderKeyRotation(
	rotation FounderKeyRotation,
	oldPublicKey, newPublicKey ed25519.PublicKey,
) error {
	if err := rotation.Validate(); err != nil {
		return err
	}
	decodedNew, err := decodePublicKey(rotation.NewPublicKey)
	if err != nil || !decodedNew.Equal(newPublicKey) {
		return fmt.Errorf("controlapi: founder rotation new key mismatch")
	}
	payload, err := founderKeyRotationSigningBytes(rotation)
	if err != nil {
		return err
	}
	oldSignature, oldErr := base64.RawURLEncoding.DecodeString(rotation.OldSignature.Value)
	newSignature, newErr := base64.RawURLEncoding.DecodeString(rotation.NewSignature.Value)
	if oldErr != nil || newErr != nil ||
		!ed25519.Verify(oldPublicKey, payload, oldSignature) ||
		!ed25519.Verify(newPublicKey, payload, newSignature) {
		return fmt.Errorf("controlapi: founder key rotation proof is invalid")
	}
	return nil
}

func founderKeyRotationSigningBytes(rotation FounderKeyRotation) ([]byte, error) {
	rotation.OldSignature = signaturePlaceholder(rotation.OldKeyID)
	rotation.NewSignature = signaturePlaceholder(rotation.NewKeyID)
	return json.Marshal(rotation)
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
		"set_budget", "set_capital", "set_model", "set_capability",
		"set_channel", "set_counterparty", "set_jurisdiction",
		"cancel_work", "force_wake", "approve_batch", "pause_company",
		"resume_company", "emergency_stop", "revoke_company_issuer",
		"pause_portfolio", "resume_portfolio", "terminate_portfolio",
		"pause_initiative", "resume_initiative", "terminate_initiative",
		"pause_department", "resume_department", "pause_squad", "resume_squad",
		"pause_seat", "resume_seat":
		return true
	default:
		return false
	}
}
