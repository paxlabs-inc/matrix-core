package contracts

import (
	"fmt"
	"strings"
)

// OrganizationID identifies one tenant-owned Workforce organization.
type OrganizationID string

// OwnerID identifies the human authority that controls an organization.
type OwnerID string

// DepartmentID identifies one department within an organization.
type DepartmentID string

// SeatID identifies one durable organizational seat.
type SeatID string

// SeatDID identifies the cryptographic identity bound to a seat version.
type SeatDID string

// MandateID identifies a versioned seat mandate.
type MandateID string

// SeatBindingID identifies a versioned model and runtime binding.
type SeatBindingID string

// WorkOrderID identifies a human-signed root order.
type WorkOrderID string

// GoalID identifies a durable goal in the global work graph.
type GoalID string

// IntentID identifies a durable unit of work.
type IntentID string

// WakeID identifies one fresh worker wake.
type WakeID string

// LeaseID identifies one bounded authorization lease.
type LeaseID string

// RecordID identifies one immutable organizational record.
type RecordID string

// MessageID identifies one immutable Workforce Mail envelope.
type MessageID string

// ThreadID identifies one bounded Workforce Mail thread.
type ThreadID string

// ArtifactID identifies one content-addressed artifact.
type ArtifactID string

// EvidenceID identifies one content-addressed observation or evidence item.
type EvidenceID string

// PolicyID identifies one immutable policy version chain.
type PolicyID string

// SkillID identifies one versioned executable skill contract.
type SkillID string

// ReceiptID identifies one immutable execution receipt.
type ReceiptID string

// VerdictID identifies one immutable Auditor verdict.
type VerdictID string

// ProjectID identifies one Developer Project Brain project.
type ProjectID string

// WorkspaceID identifies one source workspace within a project.
type WorkspaceID string

// CorrectionID identifies one correction and its reconciliation lifecycle.
type CorrectionID string

// ApprovalID identifies one approval decision or exact-set batch.
type ApprovalID string

// ModelBindingID identifies a versioned model lineage binding.
type ModelBindingID string

func validateID(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if len(value) > 128 {
		return fmt.Errorf("%s exceeds 128 bytes", name)
	}
	for _, r := range value {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' || r == ':' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", name)
	}
	return nil
}

func validateSchema(version string) error {
	if version != SchemaVersionV1 {
		return fmt.Errorf("unsupported schema_version %q", version)
	}
	return nil
}
