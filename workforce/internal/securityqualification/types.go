// Package securityqualification records real integrated threat evidence and
// enforces the two-review release gate for every critical authority boundary.
package securityqualification

import (
	"fmt"
	"slices"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
)

const (
	ModelSchemaVersion         = "workforce.security-threat-model.v1"
	ReviewSchemaVersion        = "workforce.security-boundary-review.v1"
	QualificationSchemaVersion = "workforce.security-qualification.v1"
)

type Boundary string

const (
	BoundaryPolicy       Boundary = "policy"
	BoundarySignature    Boundary = "signature"
	BoundaryMigration    Boundary = "migration"
	BoundaryEffect       Boundary = "effect"
	BoundaryCustomerData Boundary = "customer_data"
	BoundaryFinancial    Boundary = "financial"
	BoundaryBackup       Boundary = "backup"
	BoundaryRecovery     Boundary = "recovery"
)

var allBoundaries = []Boundary{
	BoundaryBackup, BoundaryCustomerData, BoundaryEffect, BoundaryFinancial,
	BoundaryMigration, BoundaryPolicy, BoundaryRecovery, BoundarySignature,
}

func AllBoundaries() []Boundary {
	return append([]Boundary(nil), allBoundaries...)
}

func (value Boundary) Valid() bool { return slices.Contains(allBoundaries, value) }

type Severity string

const (
	SeverityCritical Severity = "critical"
	SeverityHigh     Severity = "high"
	SeverityMedium   Severity = "medium"
	SeverityLow      Severity = "low"
)

func (value Severity) Valid() bool {
	return value == SeverityCritical || value == SeverityHigh ||
		value == SeverityMedium || value == SeverityLow
}

type HazardState string

const (
	HazardOpen          HazardState = "open"
	HazardMitigated     HazardState = "mitigated"
	HazardRequiresHuman HazardState = "requires_human"
)

type Hazard struct {
	ID           string                  `json:"hazard_id"`
	Boundary     Boundary                `json:"boundary"`
	Severity     Severity                `json:"severity"`
	AttackClass  string                  `json:"attack_class"`
	Target       string                  `json:"target"`
	ControlIDs   []string                `json:"control_ids"`
	Evidence     []contracts.EvidenceRef `json:"evidence"`
	State        HazardState             `json:"state"`
	ResidualRisk string                  `json:"residual_risk"`
}

func (value Hazard) Validate() error {
	if token(value.ID) != nil || !value.Boundary.Valid() || !value.Severity.Valid() ||
		token(value.AttackClass) != nil || token(value.Target) != nil ||
		tokenSet(value.ControlIDs, 1, 64) != nil || len(value.Evidence) == 0 ||
		len(value.Evidence) > 128 ||
		(value.State != HazardOpen && value.State != HazardMitigated &&
			value.State != HazardRequiresHuman) ||
		strings.TrimSpace(value.ResidualRisk) == "" || len(value.ResidualRisk) > 2048 {
		return fmt.Errorf("security qualification: hazard is invalid")
	}
	for _, evidence := range value.Evidence {
		if evidence.Validate() != nil {
			return fmt.Errorf("security qualification: hazard evidence is invalid")
		}
	}
	return nil
}

type ThreatModel struct {
	SchemaVersion         string                   `json:"schema_version"`
	ID                    string                   `json:"threat_model_id"`
	Version               uint64                   `json:"version"`
	OrganizationID        contracts.OrganizationID `json:"organization_id"`
	BuildHash             contracts.ContentHash    `json:"build_hash"`
	MigrationSetHash      contracts.ContentHash    `json:"migration_set_hash"`
	OperationRegistryHash contracts.ContentHash    `json:"operation_registry_hash"`
	AuthorSeatID          contracts.SeatID         `json:"author_seat_id"`
	Hazards               []Hazard                 `json:"hazards"`
	CompatibilityEvidence []contracts.EvidenceRef  `json:"compatibility_evidence"`
	CreatedAt             time.Time                `json:"created_at"`
	ExpiresAt             time.Time                `json:"expires_at"`
	Signature             contracts.Signature      `json:"signature"`
}

func (value ThreatModel) Validate() error {
	if value.SchemaVersion != ModelSchemaVersion || token(value.ID) != nil ||
		value.Version == 0 || token(string(value.OrganizationID)) != nil ||
		value.BuildHash.Validate() != nil || value.MigrationSetHash.Validate() != nil ||
		value.OperationRegistryHash.Validate() != nil ||
		token(string(value.AuthorSeatID)) != nil || len(value.Hazards) == 0 ||
		len(value.Hazards) > 512 || len(value.CompatibilityEvidence) == 0 ||
		len(value.CompatibilityEvidence) > 128 || !utc(value.CreatedAt) ||
		!utc(value.ExpiresAt) || !value.ExpiresAt.After(value.CreatedAt) {
		return fmt.Errorf("security qualification: threat model is incomplete")
	}
	coverage := make(map[Boundary]bool, len(allBoundaries))
	previous := ""
	for _, hazard := range value.Hazards {
		if hazard.Validate() != nil || hazard.ID <= previous {
			return fmt.Errorf("security qualification: hazards are invalid or non-canonical")
		}
		coverage[hazard.Boundary] = true
		previous = hazard.ID
	}
	for _, boundary := range allBoundaries {
		if !coverage[boundary] {
			return fmt.Errorf("security qualification: threat model omits boundary %q", boundary)
		}
	}
	for _, evidence := range value.CompatibilityEvidence {
		if evidence.Validate() != nil {
			return fmt.Errorf("security qualification: compatibility evidence is invalid")
		}
	}
	return value.Signature.Validate()
}

type ReviewOutcome string

const (
	ReviewApproved      ReviewOutcome = "approved"
	ReviewRejected      ReviewOutcome = "rejected"
	ReviewRequiresHuman ReviewOutcome = "requires_human"
)

type BoundaryReview struct {
	SchemaVersion        string                   `json:"schema_version"`
	ID                   string                   `json:"review_id"`
	OrganizationID       contracts.OrganizationID `json:"organization_id"`
	ThreatModelID        string                   `json:"threat_model_id"`
	ThreatModelHash      contracts.ContentHash    `json:"threat_model_hash"`
	ReviewerSeatID       contracts.SeatID         `json:"reviewer_seat_id"`
	ReviewerDepartmentID contracts.DepartmentID   `json:"reviewer_department_id"`
	Boundaries           []Boundary               `json:"boundaries"`
	Outcome              ReviewOutcome            `json:"outcome"`
	Evidence             []contracts.EvidenceRef  `json:"evidence"`
	ReasonCode           string                   `json:"reason_code"`
	ReviewedAt           time.Time                `json:"reviewed_at"`
	Signature            contracts.Signature      `json:"signature"`
}

func (value BoundaryReview) Validate() error {
	if value.SchemaVersion != ReviewSchemaVersion || token(value.ID) != nil ||
		token(string(value.OrganizationID)) != nil || token(value.ThreatModelID) != nil ||
		value.ThreatModelHash.Validate() != nil || token(string(value.ReviewerSeatID)) != nil ||
		token(string(value.ReviewerDepartmentID)) != nil || len(value.Boundaries) == 0 ||
		len(value.Boundaries) > len(allBoundaries) || !slices.IsSorted(value.Boundaries) ||
		(value.Outcome != ReviewApproved && value.Outcome != ReviewRejected &&
			value.Outcome != ReviewRequiresHuman) || len(value.Evidence) == 0 ||
		len(value.Evidence) > 256 || token(value.ReasonCode) != nil || !utc(value.ReviewedAt) {
		return fmt.Errorf("security qualification: boundary review is invalid")
	}
	for index, boundary := range value.Boundaries {
		if !boundary.Valid() || index > 0 && boundary == value.Boundaries[index-1] {
			return fmt.Errorf("security qualification: review boundary set is invalid")
		}
	}
	for _, evidence := range value.Evidence {
		if evidence.Validate() != nil {
			return fmt.Errorf("security qualification: review evidence is invalid")
		}
	}
	return value.Signature.Validate()
}

type Qualification struct {
	SchemaVersion       string                   `json:"schema_version"`
	ID                  string                   `json:"qualification_id"`
	OrganizationID      contracts.OrganizationID `json:"organization_id"`
	ThreatModelID       string                   `json:"threat_model_id"`
	ThreatModelHash     contracts.ContentHash    `json:"threat_model_hash"`
	ReviewIDs           []string                 `json:"review_ids"`
	ReviewHashes        []contracts.ContentHash  `json:"review_hashes"`
	QualifiedBoundaries []Boundary               `json:"qualified_boundaries"`
	QualifiedAt         time.Time                `json:"qualified_at"`
	ExpiresAt           time.Time                `json:"expires_at"`
	Signature           contracts.Signature      `json:"signature"`
}

func (value Qualification) Validate() error {
	if value.SchemaVersion != QualificationSchemaVersion || token(value.ID) != nil ||
		token(string(value.OrganizationID)) != nil || token(value.ThreatModelID) != nil ||
		value.ThreatModelHash.Validate() != nil || tokenSet(value.ReviewIDs, 2, 64) != nil ||
		len(value.ReviewHashes) != len(value.ReviewIDs) ||
		!slices.Equal(value.QualifiedBoundaries, allBoundaries) || !utc(value.QualifiedAt) ||
		!utc(value.ExpiresAt) || !value.ExpiresAt.After(value.QualifiedAt) {
		return fmt.Errorf("security qualification: qualification is incomplete")
	}
	for _, hash := range value.ReviewHashes {
		if hash.Validate() != nil {
			return fmt.Errorf("security qualification: review hash is invalid")
		}
	}
	return value.Signature.Validate()
}

func token(value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("security qualification: identity is invalid")
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || strings.ContainsRune("-_.:", character) {
			continue
		}
		return fmt.Errorf("security qualification: identity is invalid")
	}
	return nil
}

func tokenSet(values []string, minimum, maximum int) error {
	if len(values) < minimum || len(values) > maximum || !slices.IsSorted(values) {
		return fmt.Errorf("security qualification: identity set is invalid")
	}
	for index, value := range values {
		if token(value) != nil || index > 0 && value == values[index-1] {
			return fmt.Errorf("security qualification: identity set is invalid")
		}
	}
	return nil
}

func utc(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
