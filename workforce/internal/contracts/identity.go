package contracts

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Organization is the immutable, owner-controlled topology at one version.
// Its canonical identity is ID plus Version; changes create a later version.
type Organization struct {
	SchemaVersion string         `json:"schema_version"`
	ID            OrganizationID `json:"organization_id"`
	OwnerID       OwnerID        `json:"owner_id"`
	Version       uint64         `json:"version"`
	Name          string         `json:"name"`
	Departments   []Department   `json:"departments"`
	EffectiveAt   time.Time      `json:"effective_at"`
	Signature     Signature      `json:"signature"`
}

// Validate enforces one owner, exactly seven departments, and exactly three
// unique seat roles in each department.
func (o Organization) Validate() error {
	if err := validateSchema(o.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("organization_id", string(o.ID)); err != nil {
		return err
	}
	if err := validateID("owner_id", string(o.OwnerID)); err != nil {
		return err
	}
	if o.Version == 0 {
		return fmt.Errorf("organization version must be positive")
	}
	if strings.TrimSpace(o.Name) == "" || len(o.Name) > 160 {
		return fmt.Errorf("organization name must contain 1 to 160 bytes")
	}
	if !isUTC(o.EffectiveAt) {
		return fmt.Errorf("organization effective_at must be a non-zero UTC timestamp")
	}
	if err := o.Signature.Validate(); err != nil {
		return fmt.Errorf("organization signature: %w", err)
	}
	if len(o.Departments) != len(AllDepartmentKinds()) {
		return fmt.Errorf("organization must define exactly seven departments")
	}
	seen := make(map[DepartmentKind]struct{}, len(o.Departments))
	for i := range o.Departments {
		department := o.Departments[i]
		if err := department.Validate(); err != nil {
			return fmt.Errorf("department %d: %w", i, err)
		}
		if department.OrganizationID != o.ID {
			return fmt.Errorf("department %d belongs to another organization", i)
		}
		if _, exists := seen[department.Kind]; exists {
			return fmt.Errorf("department %q is duplicated", department.Kind)
		}
		seen[department.Kind] = struct{}{}
	}
	return nil
}

// Department is one of the seven first-class organizational departments.
type Department struct {
	SchemaVersion  string         `json:"schema_version"`
	ID             DepartmentID   `json:"department_id"`
	OrganizationID OrganizationID `json:"organization_id"`
	Kind           DepartmentKind `json:"kind"`
	Seats          []Seat         `json:"seats"`
	Enabled        bool           `json:"enabled"`
}

// Validate enforces one Lead, Executor, and Auditor seat with no extras.
func (d Department) Validate() error {
	if err := validateSchema(d.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("department_id", string(d.ID)); err != nil {
		return err
	}
	if err := validateID("organization_id", string(d.OrganizationID)); err != nil {
		return err
	}
	if !d.Kind.Valid() {
		return fmt.Errorf("invalid department kind %q", d.Kind)
	}
	if len(d.Seats) != len(AllSeatRoles()) {
		return fmt.Errorf("department must define exactly three seats")
	}
	seen := make(map[SeatRole]struct{}, len(d.Seats))
	for i := range d.Seats {
		seat := d.Seats[i]
		if err := seat.Validate(); err != nil {
			return fmt.Errorf("seat %d: %w", i, err)
		}
		if seat.OrganizationID != d.OrganizationID || seat.DepartmentID != d.ID {
			return fmt.Errorf("seat %d belongs to another organization or department", i)
		}
		if _, exists := seen[seat.Role]; exists {
			return fmt.Errorf("seat role %q is duplicated", seat.Role)
		}
		seen[seat.Role] = struct{}{}
	}
	return nil
}

// Seat is a durable organizational address, never a persistent model session.
type Seat struct {
	SchemaVersion  string         `json:"schema_version"`
	ID             SeatID         `json:"seat_id"`
	Version        uint64         `json:"version"`
	DID            SeatDID        `json:"seat_did"`
	OrganizationID OrganizationID `json:"organization_id"`
	DepartmentID   DepartmentID   `json:"department_id"`
	Role           SeatRole       `json:"role"`
	MandateID      MandateID      `json:"mandate_id"`
	MandateVersion uint64         `json:"mandate_version"`
	BindingID      SeatBindingID  `json:"binding_id"`
	BindingVersion uint64         `json:"binding_version"`
	EffectiveAt    time.Time      `json:"effective_at"`
	Signature      Signature      `json:"signature"`
}

// Validate enforces a complete durable seat identity and version bindings.
func (s Seat) Validate() error {
	if err := validateSchema(s.SchemaVersion); err != nil {
		return err
	}
	for name, value := range map[string]string{
		"seat_id":         string(s.ID),
		"seat_did":        string(s.DID),
		"organization_id": string(s.OrganizationID),
		"department_id":   string(s.DepartmentID),
		"mandate_id":      string(s.MandateID),
		"binding_id":      string(s.BindingID),
	} {
		if err := validateID(name, value); err != nil {
			return err
		}
	}
	if !s.Role.Valid() {
		return fmt.Errorf("invalid seat role %q", s.Role)
	}
	if s.Version == 0 || s.MandateVersion == 0 || s.BindingVersion == 0 {
		return fmt.Errorf("seat, mandate, and binding versions must be positive")
	}
	if !isUTC(s.EffectiveAt) {
		return fmt.Errorf("seat effective_at must be a non-zero UTC timestamp")
	}
	return s.Signature.Validate()
}

// DataScope is a versioned named authorization scope, not a provider token.
type DataScope struct {
	Name           string         `json:"name"`
	Classification Classification `json:"classification"`
	Purpose        string         `json:"purpose"`
}

// Validate enforces a bounded, explicit authorization scope.
func (s DataScope) Validate() error {
	if err := validateID("data scope name", s.Name); err != nil {
		return err
	}
	if !s.Classification.Valid() {
		return fmt.Errorf("invalid data scope classification %q", s.Classification)
	}
	if strings.TrimSpace(s.Purpose) == "" || len(s.Purpose) > 512 {
		return fmt.Errorf("data scope purpose must contain 1 to 512 bytes")
	}
	return nil
}

// EscalationRule names a deterministic condition and owner-visible response.
type EscalationRule struct {
	Condition string `json:"condition"`
	Action    string `json:"action"`
}

// Validate rejects empty or oversized escalation clauses.
func (r EscalationRule) Validate() error {
	if strings.TrimSpace(r.Condition) == "" || len(r.Condition) > 512 {
		return fmt.Errorf("escalation condition must contain 1 to 512 bytes")
	}
	if strings.TrimSpace(r.Action) == "" || len(r.Action) > 512 {
		return fmt.Errorf("escalation action must contain 1 to 512 bytes")
	}
	return nil
}

// Prohibition names authority a seat can never acquire through delegation.
type Prohibition struct {
	ClauseID    string `json:"clause_id"`
	Description string `json:"description"`
}

// Validate rejects empty or oversized non-delegable prohibitions.
func (p Prohibition) Validate() error {
	if err := validateID("prohibition clause_id", p.ClauseID); err != nil {
		return err
	}
	if strings.TrimSpace(p.Description) == "" || len(p.Description) > 512 {
		return fmt.Errorf("prohibition description must contain 1 to 512 bytes")
	}
	return nil
}

// Mandate is an immutable, versioned authority contract for a seat.
type Mandate struct {
	SchemaVersion   string           `json:"schema_version"`
	ID              MandateID        `json:"mandate_id"`
	Version         uint64           `json:"version"`
	OrganizationID  OrganizationID   `json:"organization_id"`
	DepartmentKind  DepartmentKind   `json:"department_kind"`
	SeatRole        SeatRole         `json:"seat_role"`
	AllowedSkills   []SkillID        `json:"allowed_skills"`
	DataScopes      []DataScope      `json:"data_scopes"`
	EscalationRules []EscalationRule `json:"escalation_rules"`
	Prohibitions    []Prohibition    `json:"prohibitions"`
	EffectiveAt     time.Time        `json:"effective_at"`
	ExpiresAt       *time.Time       `json:"expires_at"`
	Signature       Signature        `json:"signature"`
}

// Validate enforces explicit authority, escalation, and non-delegable limits.
func (m Mandate) Validate() error {
	if err := validateSchema(m.SchemaVersion); err != nil {
		return err
	}
	if err := validateID("mandate_id", string(m.ID)); err != nil {
		return err
	}
	if err := validateID("organization_id", string(m.OrganizationID)); err != nil {
		return err
	}
	if m.Version == 0 {
		return fmt.Errorf("mandate version must be positive")
	}
	if !m.DepartmentKind.Valid() || !m.SeatRole.Valid() {
		return fmt.Errorf("mandate department and seat role must be valid")
	}
	if len(m.AllowedSkills) == 0 {
		return fmt.Errorf("mandate must allow at least one skill")
	}
	skillIDs := make([]string, len(m.AllowedSkills))
	for i, skillID := range m.AllowedSkills {
		if err := validateID("allowed skill", string(skillID)); err != nil {
			return err
		}
		skillIDs[i] = string(skillID)
	}
	if !slices.IsSorted(skillIDs) || hasAdjacentDuplicate(skillIDs) {
		return fmt.Errorf("allowed_skills must be sorted and unique")
	}
	if len(m.DataScopes) == 0 || len(m.EscalationRules) == 0 || len(m.Prohibitions) == 0 {
		return fmt.Errorf("mandate scopes, escalation rules, and prohibitions must be non-empty")
	}
	for i := range m.DataScopes {
		if err := m.DataScopes[i].Validate(); err != nil {
			return fmt.Errorf("data scope %d: %w", i, err)
		}
	}
	for i := range m.EscalationRules {
		if err := m.EscalationRules[i].Validate(); err != nil {
			return fmt.Errorf("escalation rule %d: %w", i, err)
		}
	}
	for i := range m.Prohibitions {
		if err := m.Prohibitions[i].Validate(); err != nil {
			return fmt.Errorf("prohibition %d: %w", i, err)
		}
	}
	if !isUTC(m.EffectiveAt) {
		return fmt.Errorf("mandate effective_at must be a non-zero UTC timestamp")
	}
	if m.ExpiresAt != nil {
		if !isUTC(*m.ExpiresAt) || !m.ExpiresAt.After(m.EffectiveAt) {
			return fmt.Errorf("mandate expires_at must be UTC and after effective_at")
		}
	}
	if err := m.Signature.Validate(); err != nil {
		return fmt.Errorf("mandate signature: %w", err)
	}
	return nil
}

func isUTC(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}

func hasAdjacentDuplicate(values []string) bool {
	for i := 1; i < len(values); i++ {
		if values[i] == values[i-1] {
			return true
		}
	}
	return false
}
