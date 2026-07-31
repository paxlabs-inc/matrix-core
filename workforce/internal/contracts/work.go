package contracts

import (
	"fmt"
	"strings"
	"time"
)

// FenceToken is a monotonically increasing authorization generation.
type FenceToken uint64

// Validate rejects the zero token, which never authorizes execution.
func (f FenceToken) Validate() error {
	if f == 0 {
		return fmt.Errorf("fence token must be positive")
	}
	return nil
}

// Goal is a durable desired outcome in the global work graph.
type Goal struct {
	SchemaVersion   string         `json:"schema_version"`
	ID              GoalID         `json:"goal_id"`
	OrganizationID  OrganizationID `json:"organization_id"`
	WorkOrderID     WorkOrderID    `json:"work_order_id"`
	Title           string         `json:"title"`
	SuccessCriteria []string       `json:"success_criteria"`
	CreatedAt       time.Time      `json:"created_at"`
}

// Validate enforces a bounded goal with explicit success criteria.
func (g Goal) Validate() error {
	if err := validateSchema(g.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("goal_id", string(g.ID)); err != nil {
		return err
	}
	if err := validateID("organization_id", string(g.OrganizationID)); err != nil {
		return err
	}
	if err := validateID("work_order_id", string(g.WorkOrderID)); err != nil {
		return err
	}
	if strings.TrimSpace(g.Title) == "" || len(g.Title) > 512 {
		return fmt.Errorf("goal title must contain 1 to 512 bytes")
	}
	if len(g.SuccessCriteria) == 0 || len(g.SuccessCriteria) > 64 {
		return fmt.Errorf("goal must contain 1 to 64 success criteria")
	}
	for i, criterion := range g.SuccessCriteria {
		if strings.TrimSpace(criterion) == "" || len(criterion) > 2048 {
			return fmt.Errorf("success criterion %d must contain 1 to 2048 bytes", i)
		}
	}
	if !isUTC(g.CreatedAt) {
		return fmt.Errorf("goal created_at must be a non-zero UTC timestamp")
	}
	return nil
}

// Intent is a versioned, bounded unit of work selected by the dependency resolver.
type Intent struct {
	SchemaVersion  string         `json:"schema_version"`
	ID             IntentID       `json:"intent_id"`
	OrganizationID OrganizationID `json:"organization_id"`
	GoalID         GoalID         `json:"goal_id"`
	ParentIntentID *IntentID      `json:"parent_intent_id"`
	OwnerSeatID    SeatID         `json:"owner_seat_id"`
	Summary        string         `json:"summary"`
	Priority       int32          `json:"priority"`
	CreatedAt      time.Time      `json:"created_at"`
	Deadline       *time.Time     `json:"deadline"`
}

// Validate enforces the intent identity, owner, bounds, and UTC lifecycle times.
func (i Intent) Validate() error {
	if err := validateSchema(i.SchemaVersion); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"intent_id", string(i.ID)},
		{"organization_id", string(i.OrganizationID)},
		{"goal_id", string(i.GoalID)},
		{"owner_seat_id", string(i.OwnerSeatID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if i.ParentIntentID != nil {
		if err := validateID("parent_intent_id", string(*i.ParentIntentID)); err != nil {
			return err
		}
		if *i.ParentIntentID == i.ID {
			return fmt.Errorf("intent cannot parent itself")
		}
	}
	if strings.TrimSpace(i.Summary) == "" || len(i.Summary) > 2048 {
		return fmt.Errorf("intent summary must contain 1 to 2048 bytes")
	}
	if i.Priority < -1000 || i.Priority > 1000 {
		return fmt.Errorf("intent priority must be between -1000 and 1000")
	}
	if !isUTC(i.CreatedAt) {
		return fmt.Errorf("intent created_at must be a non-zero UTC timestamp")
	}
	if i.Deadline != nil && (!isUTC(*i.Deadline) || !i.Deadline.After(i.CreatedAt)) {
		return fmt.Errorf("intent deadline must be UTC and after created_at")
	}
	return nil
}

// RecordRef points to a content-addressed immutable organizational record.
type RecordRef struct {
	ID   RecordID    `json:"record_id"`
	Kind RecordKind  `json:"kind"`
	Hash ContentHash `json:"hash"`
}

// Validate enforces a typed, content-addressed record reference.
func (r RecordRef) Validate() error {
	if err := validateID("record_id", string(r.ID)); err != nil {
		return err
	}
	if !r.Kind.Valid() {
		return fmt.Errorf("invalid record kind %q", r.Kind)
	}
	return r.Hash.Validate()
}

// ArtifactRef points to immutable artifact bytes outside oversized envelopes.
type ArtifactRef struct {
	SchemaVersion string      `json:"schema_version"`
	ID            ArtifactID  `json:"artifact_id"`
	Hash          ContentHash `json:"hash"`
	MediaType     string      `json:"media_type"`
	SizeBytes     uint64      `json:"size_bytes"`
}

// Validate enforces a non-empty content-addressed artifact within the hard limit.
func (a ArtifactRef) Validate() error {
	if err := validateSchema(a.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("artifact_id", string(a.ID)); err != nil {
		return err
	}
	if err := a.Hash.Validate(); err != nil {
		return fmt.Errorf("artifact hash: %w", err)
	}
	if strings.TrimSpace(a.MediaType) == "" || len(a.MediaType) > 255 {
		return fmt.Errorf("artifact media_type must contain 1 to 255 bytes")
	}
	if a.SizeBytes == 0 {
		return fmt.Errorf("artifact size_bytes must be positive")
	}
	return nil
}

// EvidenceRef points to immutable authoritative observation bytes.
type EvidenceRef struct {
	SchemaVersion string      `json:"schema_version"`
	ID            EvidenceID  `json:"evidence_id"`
	Hash          ContentHash `json:"hash"`
	Kind          string      `json:"kind"`
	ObservedAt    time.Time   `json:"observed_at"`
}

// Validate enforces content addressability and an authoritative observation time.
func (e EvidenceRef) Validate() error {
	if err := validateSchema(e.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("evidence_id", string(e.ID)); err != nil {
		return err
	}
	if err := e.Hash.Validate(); err != nil {
		return fmt.Errorf("evidence hash: %w", err)
	}
	if err := validateID("evidence kind", e.Kind); err != nil {
		return err
	}
	if !isUTC(e.ObservedAt) {
		return fmt.Errorf("evidence observed_at must be a non-zero UTC timestamp")
	}
	return nil
}

// Record is the canonical immutable metadata envelope for typed organizational truth.
// Payload identifies canonical typed bytes whose schema is fixed by Kind.
type Record struct {
	SchemaVersion  string         `json:"schema_version"`
	ID             RecordID       `json:"record_id"`
	OrganizationID OrganizationID `json:"organization_id"`
	Kind           RecordKind     `json:"kind"`
	AuthorSeatID   SeatID         `json:"author_seat_id"`
	DepartmentID   *DepartmentID  `json:"department_id"`
	AccessSeatID   *SeatID        `json:"access_seat_id"`
	ProjectID      *ProjectID     `json:"project_id"`
	Purpose        string         `json:"purpose"`
	ParentIntentID IntentID       `json:"parent_intent_id"`
	CreatedAt      time.Time      `json:"created_at"`
	EffectiveAt    time.Time      `json:"effective_at"`
	Validity       Validity       `json:"validity"`
	PayloadSchema  string         `json:"payload_schema"`
	Payload        ArtifactRef    `json:"payload"`
	ContentHash    ContentHash    `json:"content_hash"`
	Provenance     []RecordRef    `json:"provenance"`
	Classification Classification `json:"classification"`
	Supersedes     *RecordID      `json:"supersedes"`
	Retracts       *RecordID      `json:"retracts"`
	Signature      Signature      `json:"signature"`
}

// Validate enforces immutable record metadata and explicit provenance.
func (r Record) Validate() error {
	if err := validateSchema(r.SchemaVersion); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{"record_id", string(r.ID)},
		{"organization_id", string(r.OrganizationID)},
		{"author_seat_id", string(r.AuthorSeatID)},
		{"parent_intent_id", string(r.ParentIntentID)},
	} {
		if err := validateID(field.name, field.value); err != nil {
			return err
		}
	}
	if !r.Kind.Valid() || !r.Validity.Valid() || !r.Classification.Valid() {
		return fmt.Errorf("record kind, validity, and classification must be valid")
	}
	if r.DepartmentID != nil {
		if err := validateID("record department_id", string(*r.DepartmentID)); err != nil {
			return err
		}
	}
	if r.AccessSeatID != nil {
		if err := validateID("record access_seat_id", string(*r.AccessSeatID)); err != nil {
			return err
		}
	}
	if r.ProjectID != nil {
		if err := validateID("record project_id", string(*r.ProjectID)); err != nil {
			return err
		}
	}
	if strings.TrimSpace(r.Purpose) == "" || len(r.Purpose) > 512 {
		return fmt.Errorf("record purpose must contain 1 to 512 bytes")
	}
	switch r.Classification {
	case ClassificationDepartment:
		if r.DepartmentID == nil {
			return fmt.Errorf("department-classified record requires department_id")
		}
	case ClassificationSeat:
		if r.AccessSeatID == nil {
			return fmt.Errorf("seat-classified record requires access_seat_id")
		}
	case ClassificationProject:
		if r.ProjectID == nil {
			return fmt.Errorf("project-classified record requires project_id")
		}
	}
	if r.PayloadSchema != "workforce.record."+string(r.Kind)+".v1" {
		return fmt.Errorf("record payload_schema does not match kind %q", r.Kind)
	}
	if !isUTC(r.CreatedAt) || !isUTC(r.EffectiveAt) {
		return fmt.Errorf("record times must be non-zero UTC timestamps")
	}
	if err := r.Payload.Validate(); err != nil {
		return fmt.Errorf("record payload: %w", err)
	}
	if err := r.ContentHash.Validate(); err != nil {
		return fmt.Errorf("record content_hash: %w", err)
	}
	for i := range r.Provenance {
		if err := r.Provenance[i].Validate(); err != nil {
			return fmt.Errorf("record provenance %d: %w", i, err)
		}
	}
	if r.Supersedes != nil {
		if err := validateID("supersedes", string(*r.Supersedes)); err != nil {
			return err
		}
	}
	if r.Retracts != nil {
		if err := validateID("retracts", string(*r.Retracts)); err != nil {
			return err
		}
	}
	if r.Supersedes != nil && r.Retracts != nil {
		return fmt.Errorf("record cannot both supersede and retract")
	}
	if err := r.Signature.Validate(); err != nil {
		return fmt.Errorf("record signature: %w", err)
	}
	return nil
}

// PolicyRef identifies the exact immutable policy version used by an operation.
type PolicyRef struct {
	ID      PolicyID    `json:"policy_id"`
	Version uint64      `json:"version"`
	Hash    ContentHash `json:"hash"`
}

// Validate rejects missing or unversioned policy authority.
func (p PolicyRef) Validate() error {
	if err := validateID("policy_id", string(p.ID)); err != nil {
		return err
	}
	if p.Version == 0 {
		return fmt.Errorf("policy version must be positive")
	}
	return p.Hash.Validate()
}

// Policy is an immutable, human-signed authorization record.
type Policy struct {
	SchemaVersion  string         `json:"schema_version"`
	ID             PolicyID       `json:"policy_id"`
	Version        uint64         `json:"version"`
	OrganizationID OrganizationID `json:"organization_id"`
	Kind           string         `json:"kind"`
	EffectiveAt    time.Time      `json:"effective_at"`
	ExpiresAt      *time.Time     `json:"expires_at"`
	Rules          []PolicyRule   `json:"rules"`
	Signature      Signature      `json:"signature"`
}

// PolicyRule is one deterministic deny, allow, require-review, or escalate clause.
type PolicyRule struct {
	ClauseID string `json:"clause_id"`
	Outcome  string `json:"outcome"`
	Scope    string `json:"scope"`
}

// Validate enforces a complete deterministic rule.
func (r PolicyRule) Validate() error {
	if err := validateID("policy clause_id", r.ClauseID); err != nil {
		return err
	}
	switch r.Outcome {
	case "deny", "allow", "require_review", "escalate":
	default:
		return fmt.Errorf("invalid policy outcome %q", r.Outcome)
	}
	if strings.TrimSpace(r.Scope) == "" || len(r.Scope) > 1024 {
		return fmt.Errorf("policy scope must contain 1 to 1024 bytes")
	}
	return nil
}

// Validate enforces immutable versioned policy authority and bounded rules.
func (p Policy) Validate() error {
	if err := validateSchema(p.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("policy_id", string(p.ID)); err != nil {
		return err
	}
	if err := validateID("organization_id", string(p.OrganizationID)); err != nil {
		return err
	}
	if p.Version == 0 {
		return fmt.Errorf("policy version must be positive")
	}
	if err := validateID("policy kind", p.Kind); err != nil {
		return err
	}
	if !isUTC(p.EffectiveAt) {
		return fmt.Errorf("policy effective_at must be a non-zero UTC timestamp")
	}
	if p.ExpiresAt != nil && (!isUTC(*p.ExpiresAt) || !p.ExpiresAt.After(p.EffectiveAt)) {
		return fmt.Errorf("policy expires_at must be UTC and after effective_at")
	}
	if len(p.Rules) == 0 || len(p.Rules) > 256 {
		return fmt.Errorf("policy must contain 1 to 256 rules")
	}
	for i := range p.Rules {
		if err := p.Rules[i].Validate(); err != nil {
			return fmt.Errorf("policy rule %d: %w", i, err)
		}
	}
	return p.Signature.Validate()
}
