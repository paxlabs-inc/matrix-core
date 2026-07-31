package contracts

import (
	"fmt"
	"strings"
	"time"
)

// SeatAddress is a durable Workforce Mail address scoped to one organization.
type SeatAddress struct {
	OrganizationID OrganizationID `json:"organization_id"`
	DepartmentID   DepartmentID   `json:"department_id"`
	SeatID         SeatID         `json:"seat_id"`
}

// Validate enforces a complete organizational address.
func (a SeatAddress) Validate() error {
	if err := validateID("address organization_id", string(a.OrganizationID)); err != nil {
		return err
	}
	if err := validateID("address department_id", string(a.DepartmentID)); err != nil {
		return err
	}
	return validateID("address seat_id", string(a.SeatID))
}

// MessagePayloadRef identifies canonical typed payload bytes for a message kind.
type MessagePayloadRef struct {
	SchemaID string      `json:"schema_id"`
	Artifact ArtifactRef `json:"artifact"`
}

// Validate enforces a named schema and content-addressed payload.
func (p MessagePayloadRef) Validate() error {
	if err := validateID("message payload schema_id", p.SchemaID); err != nil {
		return err
	}
	return p.Artifact.Validate()
}

// MessageEnvelope is one signed, bounded Workforce Mail protocol message.
type MessageEnvelope struct {
	SchemaVersion  string            `json:"schema_version"`
	ID             MessageID         `json:"message_id"`
	ThreadID       ThreadID          `json:"thread_id"`
	InReplyTo      *MessageID        `json:"in_reply_to"`
	From           SeatAddress       `json:"from"`
	To             []SeatAddress     `json:"to"`
	CC             []SeatAddress     `json:"cc"`
	Kind           MessageKind       `json:"kind"`
	Subject        string            `json:"subject"`
	Payload        MessagePayloadRef `json:"payload"`
	ParentIntentID IntentID          `json:"parent_intent_id"`
	RequiredAction string            `json:"required_action"`
	Artifacts      []ArtifactRef     `json:"artifacts"`
	Evidence       []EvidenceRef     `json:"evidence"`
	Priority       int32             `json:"priority"`
	Deadline       *time.Time        `json:"deadline"`
	TimeoutAction  TimeoutAction     `json:"timeout_action"`
	Classification Classification    `json:"classification"`
	IdempotencyKey string            `json:"idempotency_key"`
	CreatedAt      time.Time         `json:"created_at"`
	ExpiresAt      time.Time         `json:"expires_at"`
	Signature      Signature         `json:"signature"`
}

// Validate enforces typed addressing, authority-neutral payloads, and protocol bounds.
func (m MessageEnvelope) Validate() error {
	if err := validateSchema(m.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("message_id", string(m.ID)); err != nil {
		return err
	}
	if err := validateID("thread_id", string(m.ThreadID)); err != nil {
		return err
	}
	if m.InReplyTo != nil {
		if err := validateID("in_reply_to", string(*m.InReplyTo)); err != nil {
			return err
		}
		if *m.InReplyTo == m.ID {
			return fmt.Errorf("message cannot reply to itself")
		}
	}
	if err := m.From.Validate(); err != nil {
		return fmt.Errorf("message from: %w", err)
	}
	if len(m.To) == 0 || len(m.To) > 32 || len(m.CC) > 32 {
		return fmt.Errorf("message recipients exceed protocol bounds")
	}
	for i := range m.To {
		if err := m.To[i].Validate(); err != nil {
			return fmt.Errorf("message to recipient %d: %w", i, err)
		}
	}
	for i := range m.CC {
		if err := m.CC[i].Validate(); err != nil {
			return fmt.Errorf("message cc recipient %d: %w", i, err)
		}
	}
	if !m.Kind.Valid() {
		return fmt.Errorf("invalid message kind %q", m.Kind)
	}
	if strings.TrimSpace(m.Subject) == "" || len(m.Subject) > 512 {
		return fmt.Errorf("message subject must contain 1 to 512 bytes")
	}
	if err := m.Payload.Validate(); err != nil {
		return fmt.Errorf("message payload: %w", err)
	}
	if m.Payload.SchemaID != "workforce.mail."+string(m.Kind)+".v1" {
		return fmt.Errorf("message payload schema does not match kind %q", m.Kind)
	}
	if err := validateID("parent_intent_id", string(m.ParentIntentID)); err != nil {
		return err
	}
	if strings.TrimSpace(m.RequiredAction) == "" || len(m.RequiredAction) > 1024 {
		return fmt.Errorf("required_action must contain 1 to 1024 bytes")
	}
	for i := range m.Artifacts {
		if err := m.Artifacts[i].Validate(); err != nil {
			return fmt.Errorf("message artifact %d: %w", i, err)
		}
	}
	for i := range m.Evidence {
		if err := m.Evidence[i].Validate(); err != nil {
			return fmt.Errorf("message evidence %d: %w", i, err)
		}
	}
	if m.Priority < -1000 || m.Priority > 1000 {
		return fmt.Errorf("message priority must be between -1000 and 1000")
	}
	if m.Deadline != nil && (!isUTC(*m.Deadline) || !m.Deadline.After(m.CreatedAt)) {
		return fmt.Errorf("message deadline must be UTC and after created_at")
	}
	if !m.TimeoutAction.Valid() || !m.Classification.Valid() {
		return fmt.Errorf("message timeout_action and classification must be valid")
	}
	if err := validateID("message idempotency_key", m.IdempotencyKey); err != nil {
		return err
	}
	if !isUTC(m.CreatedAt) || !isUTC(m.ExpiresAt) || !m.ExpiresAt.After(m.CreatedAt) {
		return fmt.Errorf("message times must be UTC and expires_at must follow created_at")
	}
	return m.Signature.Validate()
}

type OperationLineage struct {
	Name        string      `json:"name"`
	EffectClass string      `json:"effect_class"`
	Digest      ContentHash `json:"digest"`
	Outcome     string      `json:"outcome"`
}

func (value OperationLineage) Validate() error {
	if err := validateID("operation name", value.Name); err != nil {
		return err
	}
	if err := validateID("operation effect_class", value.EffectClass); err != nil {
		return err
	}
	if err := value.Digest.Validate(); err != nil {
		return err
	}
	return validateID("operation outcome", value.Outcome)
}

type ReconciliationLineage struct {
	OperationDigest ContentHash   `json:"operation_digest"`
	Outcome         string        `json:"outcome"`
	Evidence        []EvidenceRef `json:"evidence"`
}

func (value ReconciliationLineage) Validate() error {
	if err := value.OperationDigest.Validate(); err != nil {
		return err
	}
	if err := validateID("reconciliation outcome", value.Outcome); err != nil {
		return err
	}
	for i := range value.Evidence {
		if err := value.Evidence[i].Validate(); err != nil {
			return fmt.Errorf("reconciliation evidence %d: %w", i, err)
		}
	}
	return nil
}

// Receipt is immutable, content-addressed proof of one terminal or partial outcome.
type Receipt struct {
	SchemaVersion     string                  `json:"schema_version"`
	ID                ReceiptID               `json:"receipt_id"`
	OrganizationID    OrganizationID          `json:"organization_id"`
	DepartmentID      DepartmentID            `json:"department_id"`
	WakeID            WakeID                  `json:"wake_id"`
	LeaseID           LeaseID                 `json:"lease_id"`
	SeatID            SeatID                  `json:"seat_id"`
	SeatDID           SeatDID                 `json:"seat_did"`
	MandateID         MandateID               `json:"mandate_id"`
	MandateVersion    uint64                  `json:"mandate_version"`
	ParentIntentID    IntentID                `json:"parent_intent_id"`
	ChildIntentIDs    []IntentID              `json:"child_intent_ids"`
	Inputs            []RecordRef             `json:"inputs"`
	Constraints       []string                `json:"constraints"`
	Approvals         []ApprovalID            `json:"approvals"`
	Policies          []PolicyRef             `json:"policies"`
	Operations        []OperationLineage      `json:"operations"`
	Artifacts         []ArtifactRef           `json:"artifacts"`
	Evidence          []EvidenceRef           `json:"evidence"`
	Reconciliation    []ReconciliationLineage `json:"reconciliation"`
	Model             ModelBinding            `json:"model"`
	MGS               MGSGenomeRef            `json:"mgs"`
	Runtime           RuntimeBinding          `json:"runtime"`
	Source            SourceState             `json:"source"`
	Skill             SkillRef                `json:"skill"`
	VerifierDigest    ContentHash             `json:"verifier_digest"`
	ModelRequestHash  ContentHash             `json:"model_request_hash"`
	ModelResponseHash ContentHash             `json:"model_response_hash"`
	VerdictID         *VerdictID              `json:"verdict_id"`
	CostMinor         int64                   `json:"cost_minor"`
	Currency          string                  `json:"currency"`
	LatencyMillis     uint64                  `json:"latency_millis"`
	Disposition       WakeDisposition         `json:"disposition"`
	UnresolvedRisk    string                  `json:"unresolved_risk"`
	CreatedAt         time.Time               `json:"created_at"`
	ContentHash       ContentHash             `json:"content_hash"`
	Signature         Signature               `json:"signature"`
}

// Validate enforces complete lineage and honest closed terminal state.
func (r Receipt) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"receipt_id", string(r.ID)},
		{"organization_id", string(r.OrganizationID)},
		{"department_id", string(r.DepartmentID)},
		{"wake_id", string(r.WakeID)},
		{"lease_id", string(r.LeaseID)},
		{"seat_id", string(r.SeatID)},
		{"seat_did", string(r.SeatDID)},
		{"mandate_id", string(r.MandateID)},
		{"parent_intent_id", string(r.ParentIntentID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if r.MandateVersion == 0 {
		return fmt.Errorf("receipt mandate_version must be positive")
	}
	for _, childID := range r.ChildIntentIDs {
		if err := validateID("child_intent_id", string(childID)); err != nil {
			return err
		}
	}
	for i := range r.Inputs {
		if err := r.Inputs[i].Validate(); err != nil {
			return fmt.Errorf("receipt input %d: %w", i, err)
		}
	}
	for i := range r.Policies {
		if err := r.Policies[i].Validate(); err != nil {
			return fmt.Errorf("receipt policy %d: %w", i, err)
		}
	}
	if len(r.Constraints) == 0 || len(r.Constraints) > 128 {
		return fmt.Errorf("receipt constraints must contain 1 to 128 entries")
	}
	for i, constraint := range r.Constraints {
		if strings.TrimSpace(constraint) == "" || len(constraint) > 2048 {
			return fmt.Errorf("receipt constraint %d is invalid", i)
		}
	}
	for _, approvalID := range r.Approvals {
		if err := validateID("approval_id", string(approvalID)); err != nil {
			return err
		}
	}
	for i := range r.Operations {
		if err := r.Operations[i].Validate(); err != nil {
			return fmt.Errorf("receipt operation %d: %w", i, err)
		}
	}
	for i := range r.Artifacts {
		if err := r.Artifacts[i].Validate(); err != nil {
			return fmt.Errorf("receipt artifact %d: %w", i, err)
		}
	}
	for i := range r.Evidence {
		if err := r.Evidence[i].Validate(); err != nil {
			return fmt.Errorf("receipt evidence %d: %w", i, err)
		}
	}
	for i := range r.Reconciliation {
		if err := r.Reconciliation[i].Validate(); err != nil {
			return fmt.Errorf("receipt reconciliation %d: %w", i, err)
		}
	}
	if err := r.Model.Validate(); err != nil {
		return fmt.Errorf("receipt model: %w", err)
	}
	if err := r.MGS.Validate(); err != nil {
		return fmt.Errorf("receipt MGS: %w", err)
	}
	if err := r.Runtime.Validate(); err != nil {
		return fmt.Errorf("receipt runtime: %w", err)
	}
	if err := r.Source.Validate(); err != nil {
		return fmt.Errorf("receipt source: %w", err)
	}
	if err := r.Skill.Validate(); err != nil {
		return fmt.Errorf("receipt skill: %w", err)
	}
	if err := r.VerifierDigest.Validate(); err != nil {
		return fmt.Errorf("receipt verifier_digest: %w", err)
	}
	if err := r.ModelRequestHash.Validate(); err != nil {
		return fmt.Errorf("receipt model_request_hash: %w", err)
	}
	if err := r.ModelResponseHash.Validate(); err != nil {
		return fmt.Errorf("receipt model_response_hash: %w", err)
	}
	if r.VerdictID != nil {
		if err := validateID("verdict_id", string(*r.VerdictID)); err != nil {
			return err
		}
	}
	if r.CostMinor < 0 {
		return fmt.Errorf("receipt cost_minor cannot be negative")
	}
	if err := validateID("receipt currency", r.Currency); err != nil {
		return err
	}
	if !r.Disposition.Valid() {
		return fmt.Errorf("invalid receipt disposition %q", r.Disposition)
	}
	if len(r.UnresolvedRisk) > 2048 {
		return fmt.Errorf("receipt unresolved_risk exceeds 2048 bytes")
	}
	if !isUTC(r.CreatedAt) {
		return fmt.Errorf("receipt created_at must be a non-zero UTC timestamp")
	}
	if err := r.ContentHash.Validate(); err != nil {
		return fmt.Errorf("receipt content_hash: %w", err)
	}
	return r.Signature.Validate()
}

// Verdict is a memoryless Auditor's immutable result over a closed packet.
type Verdict struct {
	SchemaVersion  string         `json:"schema_version"`
	ID             VerdictID      `json:"verdict_id"`
	OrganizationID OrganizationID `json:"organization_id"`
	IntentID       IntentID       `json:"intent_id"`
	AuditorSeatID  SeatID         `json:"auditor_seat_id"`
	Outcome        VerdictOutcome `json:"outcome"`
	VerifierDigest ContentHash    `json:"verifier_digest"`
	Evidence       []EvidenceRef  `json:"evidence"`
	ReasonCode     string         `json:"reason_code"`
	CreatedAt      time.Time      `json:"created_at"`
	Signature      Signature      `json:"signature"`
}

// Validate enforces independent identity, closed outcome, and verifier lineage.
func (v Verdict) Validate() error {
	if err := validateSchema(v.SchemaVersion); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"verdict_id", string(v.ID)},
		{"organization_id", string(v.OrganizationID)},
		{"intent_id", string(v.IntentID)},
		{"auditor_seat_id", string(v.AuditorSeatID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if !v.Outcome.Valid() {
		return fmt.Errorf("invalid verdict outcome %q", v.Outcome)
	}
	if err := v.VerifierDigest.Validate(); err != nil {
		return fmt.Errorf("verdict verifier_digest: %w", err)
	}
	for i := range v.Evidence {
		if err := v.Evidence[i].Validate(); err != nil {
			return fmt.Errorf("verdict evidence %d: %w", i, err)
		}
	}
	if err := validateID("verdict reason_code", v.ReasonCode); err != nil {
		return err
	}
	if !isUTC(v.CreatedAt) {
		return fmt.Errorf("verdict created_at must be a non-zero UTC timestamp")
	}
	return v.Signature.Validate()
}

// ProjectBrainRef is a read-only, project-scoped engineering context snapshot.
type ProjectBrainRef struct {
	SchemaVersion string      `json:"schema_version"`
	ProjectID     ProjectID   `json:"project_id"`
	WorkspaceID   WorkspaceID `json:"workspace_id"`
	Source        SourceState `json:"source"`
	ViewDigest    ContentHash `json:"view_digest"`
	Fresh         bool        `json:"fresh"`
	PendingFiles  []string    `json:"pending_files"`
	ExpiresAt     time.Time   `json:"expires_at"`
}

// Validate enforces project/workspace scope and explicit source freshness.
func (r ProjectBrainRef) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("project_id", string(r.ProjectID)); err != nil {
		return err
	}
	if err := validateID("workspace_id", string(r.WorkspaceID)); err != nil {
		return err
	}
	if err := r.Source.Validate(); err != nil {
		return err
	}
	if err := r.ViewDigest.Validate(); err != nil {
		return fmt.Errorf("project brain view_digest: %w", err)
	}
	if r.Fresh && len(r.PendingFiles) != 0 {
		return fmt.Errorf("fresh project brain view cannot contain pending files")
	}
	if !r.Fresh && len(r.PendingFiles) == 0 {
		return fmt.Errorf("stale project brain view must identify pending files")
	}
	for i, path := range r.PendingFiles {
		if strings.TrimSpace(path) == "" || len(path) > 4096 {
			return fmt.Errorf("pending file %d is empty or oversized", i)
		}
	}
	if !isUTC(r.ExpiresAt) {
		return fmt.Errorf("project brain expires_at must be a non-zero UTC timestamp")
	}
	return nil
}
