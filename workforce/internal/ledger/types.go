package ledger

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
)

var (
	// ErrConflict means an immutable identity or idempotency key was reused with
	// different canonical content.
	ErrConflict = errors.New("ledger immutable conflict")
	// ErrNotFound intentionally combines missing and unauthorized reads so the
	// caller cannot use the ledger as a record-existence oracle.
	ErrNotFound = errors.New("ledger record not found")
	// ErrIntegrity means sealed or canonical record bytes failed authentication.
	ErrIntegrity = errors.New("ledger record integrity failure")
	// ErrCorrectionClosed means a reconciliation tried to mutate a terminal correction.
	ErrCorrectionClosed = errors.New("ledger correction is closed")
)

// Store owns PostgreSQL transactions for one tenant and requires an encrypting
// UserVault. The caller owns and closes the supplied pool.
type Store struct {
	pool     *pgxpool.Pool
	vault    *vault.UserVault
	tenantID string
	now      func() time.Time
}

// New constructs a tenant-scoped ledger. It fails closed unless Vault is
// encrypting for the same tenant identity used in PostgreSQL keys and AAD.
func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	tenantID string,
	now func() time.Time,
) (*Store, error) {
	tenantID = strings.TrimSpace(tenantID)
	switch {
	case pool == nil:
		return nil, fmt.Errorf("ledger: pool is required")
	case userVault == nil:
		return nil, fmt.Errorf("ledger: encrypting user vault is required")
	case tenantID == "":
		return nil, fmt.Errorf("ledger: tenant_id is required")
	case userVault.User() != tenantID:
		return nil, fmt.Errorf("ledger: vault user does not match tenant")
	case now == nil:
		return nil, fmt.Errorf("ledger: time source is required")
	default:
		return &Store{pool: pool, vault: userVault, tenantID: tenantID, now: now}, nil
	}
}

// AppendRequest atomically stores one immutable typed record and its declared
// derivation edges. IdempotencyKey is scoped to tenant and organization.
type AppendRequest struct {
	Record         contracts.Record
	IdempotencyKey string
}

// AppendResult describes whether the immutable append created or reused truth.
type AppendResult struct {
	RecordID      contracts.RecordID
	CanonicalHash contracts.ContentHash
	Deduplicated  bool
}

// AccessAction is an immutable provenance-producing read or delivery action.
type AccessAction string

const (
	// AccessDelivery records durable delivery to a consumer.
	AccessDelivery AccessAction = "delivery"
	// AccessOpen records opening canonical record content.
	AccessOpen AccessAction = "open"
	// AccessCitation records evidence citation by a derived consumer.
	AccessCitation AccessAction = "citation"
	// AccessDerivation records creation of a record derived from a source.
	AccessDerivation AccessAction = "derivation"
)

// Valid reports whether the access action is executable by this release.
func (a AccessAction) Valid() bool {
	switch a {
	case AccessDelivery, AccessOpen, AccessCitation, AccessDerivation:
		return true
	default:
		return false
	}
}

// AccessGrant is a caller-verified, expiring organizational read projection.
// The ledger still enforces its scope before opening sealed bytes.
type AccessGrant struct {
	OrganizationID  contracts.OrganizationID
	SeatID          contracts.SeatID
	DepartmentID    *contracts.DepartmentID
	ProjectID       *contracts.ProjectID
	Purpose         string
	Classifications []contracts.Classification
	Restricted      bool
	ExpiresAt       time.Time
}

// OpenRequest identifies one authorized read and its idempotent access record.
type OpenRequest struct {
	OrganizationID contracts.OrganizationID
	RecordID       contracts.RecordID
	Grant          AccessGrant
	IdempotencyKey string
}

// AccessRequest records delivery, citation, or derivation without opening bytes.
type AccessRequest struct {
	OrganizationID   contracts.OrganizationID
	SourceRecordID   contracts.RecordID
	ConsumerRecordID *contracts.RecordID
	Action           AccessAction
	Grant            AccessGrant
	IdempotencyKey   string
}

// CorrectionRequest atomically appends a typed Correction record, computes all
// transitive consumers, creates mandatory notices, and pauses unsafe targets.
type CorrectionRequest struct {
	ID               contracts.CorrectionID
	SourceRecordID   contracts.RecordID
	CorrectionRecord contracts.Record
	IdempotencyKey   string
	MateriallyUnsafe bool
}

// ReconciliationState is a consumer's closed response to a correction.
type ReconciliationState string

const (
	// ReconciliationApplied accepts and applies the corrected truth.
	ReconciliationApplied ReconciliationState = "applied"
	// ReconciliationRejected rejects a correction with authoritative evidence.
	ReconciliationRejected ReconciliationState = "rejected"
	// ReconciliationEscalated transfers unresolved correction authority to a human.
	ReconciliationEscalated ReconciliationState = "escalated"
)

// Valid reports whether the correction response is closed and executable.
func (s ReconciliationState) Valid() bool {
	switch s {
	case ReconciliationApplied, ReconciliationRejected, ReconciliationEscalated:
		return true
	default:
		return false
	}
}

// ReconcileRequest resolves one affected record's mandatory correction target.
type ReconcileRequest struct {
	OrganizationID   contracts.OrganizationID
	CorrectionID     contracts.CorrectionID
	AffectedRecordID contracts.RecordID
	State            ReconciliationState
	EvidenceRecordID *contracts.RecordID
	IdempotencyKey   string
}

// CorrectionStatus is the durable correction closure projection.
type CorrectionStatus struct {
	ID        contracts.CorrectionID
	Status    string
	Pending   int
	Applied   int
	Rejected  int
	Escalated int
	Paused    int
}

func validateToken(name, value string) error {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 bytes", name)
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

func (s *Store) currentTime() (time.Time, error) {
	now := s.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("ledger: time source returned a non-UTC timestamp")
	}
	return now, nil
}
