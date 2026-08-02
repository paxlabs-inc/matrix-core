package founderprojection

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const SchemaVersion = "workforce.founder-ui-projection-receipt.v1"
const timeFormat = time.RFC3339Nano

var (
	ErrConflict     = errors.New("founder projection: conflicting receipt")
	ErrNotFound     = errors.New("founder projection: receipt not found")
	ErrUnauthorized = errors.New("founder projection: unauthorized")
)

type Snapshot struct {
	Hash             contracts.ContentHash `json:"hash"`
	Cursor           string                `json:"cursor"`
	ResourceCounts   map[string]uint64     `json:"resource_counts"`
	ResourceVersions map[string]uint64     `json:"resource_versions"`
}

func (value Snapshot) Validate() error {
	if value.Hash.Validate() != nil || digest(value.Cursor) != nil ||
		len(value.ResourceCounts) == 0 ||
		len(value.ResourceCounts) != len(value.ResourceVersions) {
		return fmt.Errorf("founder projection: snapshot is invalid")
	}
	for resource := range value.ResourceCounts {
		if token(resource) != nil {
			return fmt.Errorf("founder projection: snapshot resource is invalid")
		}
		if _, exists := value.ResourceVersions[resource]; !exists {
			return fmt.Errorf("founder projection: snapshot resource set is inconsistent")
		}
	}
	return nil
}

func (value Snapshot) Equal(other Snapshot) bool {
	if value.Hash != other.Hash || value.Cursor != other.Cursor ||
		len(value.ResourceCounts) != len(other.ResourceCounts) ||
		len(value.ResourceVersions) != len(other.ResourceVersions) {
		return false
	}
	for resource, count := range value.ResourceCounts {
		if other.ResourceCounts[resource] != count ||
			other.ResourceVersions[resource] != value.ResourceVersions[resource] {
			return false
		}
	}
	return true
}

type ProcessIdentity struct {
	ProcessID    string            `json:"process_id"`
	WakeID       string            `json:"wake_id"`
	OwnerID      contracts.OwnerID `json:"owner_id"`
	Runtime      string            `json:"runtime"`
	Role         string            `json:"role"`
	FreshProcess bool              `json:"fresh_process"`
}

func (value ProcessIdentity) Validate() error {
	if token(value.ProcessID) != nil || token(value.WakeID) != nil ||
		token(string(value.OwnerID)) != nil || value.Runtime != "browser" ||
		value.Role != "founder_renderer" || !value.FreshProcess {
		return fmt.Errorf("founder projection: renderer process identity is invalid")
	}
	return nil
}

type RenderEvidence struct {
	ID         string                `json:"evidence_id"`
	Hash       contracts.ContentHash `json:"evidence_hash"`
	ObservedAt time.Time             `json:"observed_at"`
	FreshUntil time.Time             `json:"fresh_until"`
}

func (value RenderEvidence) Validate() error {
	if token(value.ID) != nil || value.Hash.Validate() != nil ||
		!utc(value.ObservedAt) || !utc(value.FreshUntil) ||
		!value.FreshUntil.After(value.ObservedAt) {
		return fmt.Errorf("founder projection: render evidence is invalid")
	}
	return nil
}

type CaptureDraft struct {
	InitiativeID  string          `json:"initiative_id"`
	Authoritative Snapshot        `json:"authoritative_snapshot"`
	Rendered      Snapshot        `json:"rendered_snapshot"`
	Process       ProcessIdentity `json:"process"`
	Evidence      RenderEvidence  `json:"evidence"`
	RenderedAt    time.Time       `json:"rendered_at"`
	ExpiresAt     time.Time       `json:"expires_at"`
}

func (value CaptureDraft) Validate() error {
	if token(value.InitiativeID) != nil || value.Authoritative.Validate() != nil ||
		value.Rendered.Validate() != nil || !value.Authoritative.Equal(value.Rendered) ||
		value.Process.Validate() != nil || value.Evidence.Validate() != nil ||
		!utc(value.RenderedAt) || !utc(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.RenderedAt) ||
		!value.Evidence.ObservedAt.Equal(value.RenderedAt) ||
		!value.Evidence.FreshUntil.Equal(value.ExpiresAt) ||
		value.Evidence.Hash != value.Rendered.Hash {
		return fmt.Errorf("founder projection: capture draft is invalid")
	}
	return nil
}

type Receipt struct {
	SchemaVersion  string
	ID             string
	Version        uint64
	OrganizationID contracts.OrganizationID
	InitiativeID   string
	Authoritative  Snapshot
	Rendered       Snapshot
	Process        ProcessIdentity
	Evidence       RenderEvidence
	RenderedAt     time.Time
	ExpiresAt      time.Time
	CreatedAt      time.Time
	CanonicalHash  contracts.ContentHash
	SignerKeyID    string
	Signature      []byte
}

type CurrentReceipt struct {
	SchemaVersion    string                   `json:"schema_version"`
	ReceiptID        string                   `json:"receipt_id"`
	RecordID         string                   `json:"record_id"`
	Version          uint64                   `json:"version"`
	RecordVersion    uint64                   `json:"record_version"`
	OrganizationID   contracts.OrganizationID `json:"organization_id"`
	InitiativeID     string                   `json:"initiative_id"`
	SnapshotHash     contracts.ContentHash    `json:"snapshot_hash"`
	SnapshotCursor   string                   `json:"snapshot_cursor"`
	ResourceCounts   map[string]uint64        `json:"resource_counts"`
	ResourceVersions map[string]uint64        `json:"resource_versions"`
	Process          ProcessIdentity          `json:"process"`
	RenderEvidence   RenderEvidence           `json:"render_evidence"`
	RecordHash       contracts.ContentHash    `json:"record_hash"`
	SourceState      string                   `json:"source_state"`
	ObservedAt       time.Time                `json:"observed_at"`
	FreshUntil       time.Time                `json:"fresh_until"`
	RenderedAt       time.Time                `json:"rendered_at"`
	ExpiresAt        time.Time                `json:"expires_at"`
	CreatedAt        time.Time                `json:"created_at"`
}

func (value Receipt) Current() CurrentReceipt {
	return CurrentReceipt{
		SchemaVersion: SchemaVersion, ReceiptID: value.ID, RecordID: value.ID,
		Version: value.Version, RecordVersion: value.Version,
		OrganizationID: value.OrganizationID, InitiativeID: value.InitiativeID,
		SnapshotHash: value.Authoritative.Hash, SnapshotCursor: value.Authoritative.Cursor,
		ResourceCounts:   cloneMap(value.Authoritative.ResourceCounts),
		ResourceVersions: cloneMap(value.Authoritative.ResourceVersions),
		Process:          value.Process, RenderEvidence: value.Evidence,
		RecordHash: value.CanonicalHash, SourceState: "current",
		ObservedAt: value.RenderedAt, FreshUntil: value.ExpiresAt,
		RenderedAt: value.RenderedAt, ExpiresAt: value.ExpiresAt, CreatedAt: value.CreatedAt,
	}
}

func cloneMap(value map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(value))
	for key, item := range value {
		result[key] = item
	}
	return result
}

func token(value string) error {
	if value == "" || value != strings.TrimSpace(value) || len(value) > 512 {
		return fmt.Errorf("invalid token")
	}
	for _, character := range value {
		if character < 0x20 || character == 0x7f {
			return fmt.Errorf("invalid token")
		}
	}
	return nil
}

func digest(value string) error {
	return contracts.ContentHash{Algorithm: "sha256", Digest: value}.Validate()
}

func utc(value time.Time) bool {
	return !value.IsZero() && value.Location() == time.UTC
}
