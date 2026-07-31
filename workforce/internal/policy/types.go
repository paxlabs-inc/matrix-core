// Package policy owns human authority roots, signed organizational versions,
// revocation, and material-change invalidation of already issued leases.
package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

type Kind string

const (
	KindOrganization Kind = "organization"
	KindMandate      Kind = "mandate"
	KindSeat         Kind = "seat"
	KindPolicy       Kind = "policy"
	KindRuntime      Kind = "runtime_authority"
)

func (kind Kind) Valid() bool {
	switch kind {
	case KindOrganization, KindMandate, KindSeat, KindPolicy, KindRuntime:
		return true
	default:
		return false
	}
}

const WakeLeaseSigningPurpose = "wake_lease_signing"

func RuntimeAuthorityID(keyID string) string {
	return "runtime-authority:" + keyID
}

// RuntimeAuthority is the owner's bounded delegation to the deterministic
// kernel. The delegated private key remains outside every seat process.
type RuntimeAuthority struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"runtime_authority_id"`
	Version        uint64                   `json:"version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	KeyID          string                   `json:"key_id"`
	PublicKey      string                   `json:"public_key"`
	Purposes       []string                 `json:"purposes"`
	EffectiveAt    time.Time                `json:"effective_at"`
	ExpiresAt      *time.Time               `json:"expires_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (authority RuntimeAuthority) Validate() error {
	if authority.SchemaVersion != contracts.SchemaVersionV1 ||
		strings.TrimSpace(authority.ID) == "" ||
		authority.Version == 0 ||
		authority.OrganizationID == "" ||
		strings.TrimSpace(authority.KeyID) == "" {
		return fmt.Errorf("policy: runtime authority identity is incomplete")
	}
	if authority.ID != RuntimeAuthorityID(authority.KeyID) {
		return fmt.Errorf("policy: runtime authority ID does not bind its key")
	}
	publicKey, err := base64.RawURLEncoding.DecodeString(authority.PublicKey)
	if err != nil || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("policy: runtime authority requires an Ed25519 public key")
	}
	if len(authority.Purposes) != 1 ||
		authority.Purposes[0] != WakeLeaseSigningPurpose {
		return fmt.Errorf("policy: runtime authority purpose is invalid")
	}
	if authority.EffectiveAt.IsZero() ||
		authority.EffectiveAt.Location() != time.UTC {
		return fmt.Errorf("policy: runtime authority effective_at must be UTC")
	}
	if authority.ExpiresAt != nil &&
		(authority.ExpiresAt.Location() != time.UTC ||
			!authority.ExpiresAt.After(authority.EffectiveAt)) {
		return fmt.Errorf("policy: runtime authority expiry is invalid")
	}
	return authority.Signature.Validate()
}

type OwnerRoot struct {
	TenantID       string
	OrganizationID contracts.OrganizationID
	OwnerID        contracts.OwnerID
	KeyID          string
	PublicKey      ed25519.PublicKey
}

func (root OwnerRoot) Validate() error {
	if strings.TrimSpace(root.TenantID) == "" ||
		root.OrganizationID == "" ||
		root.OwnerID == "" ||
		strings.TrimSpace(root.KeyID) == "" {
		return fmt.Errorf("policy: owner root identity is incomplete")
	}
	if len(root.PublicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("policy: owner root requires an Ed25519 public key")
	}
	return nil
}

type OwnerGrant struct {
	SchemaVersion  string                   `json:"schema_version"`
	TenantID       string                   `json:"tenant_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	OwnerID        contracts.OwnerID        `json:"owner_id"`
	KeyID          string                   `json:"key_id"`
	Scope          string                   `json:"scope"`
	IssuedAt       time.Time                `json:"issued_at"`
	ExpiresAt      time.Time                `json:"expires_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (grant OwnerGrant) Validate() error {
	if grant.SchemaVersion != contracts.SchemaVersionV1 ||
		strings.TrimSpace(grant.TenantID) == "" ||
		grant.OrganizationID == "" ||
		grant.OwnerID == "" ||
		strings.TrimSpace(grant.KeyID) == "" ||
		strings.TrimSpace(grant.Scope) == "" {
		return fmt.Errorf("policy: owner grant identity is incomplete")
	}
	if grant.IssuedAt.IsZero() || grant.IssuedAt.Location() != time.UTC ||
		grant.ExpiresAt.Location() != time.UTC ||
		!grant.ExpiresAt.After(grant.IssuedAt) {
		return fmt.Errorf("policy: owner grant times are invalid")
	}
	return grant.Signature.Validate()
}

type Revocation struct {
	SchemaVersion  string                   `json:"schema_version"`
	TenantID       string                   `json:"tenant_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Kind           Kind                     `json:"kind"`
	AuthorityID    string                   `json:"authority_id"`
	Version        uint64                   `json:"version"`
	OwnerID        contracts.OwnerID        `json:"owner_id"`
	KeyID          string                   `json:"key_id"`
	Reason         string                   `json:"reason"`
	RevokedAt      time.Time                `json:"revoked_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (revocation Revocation) Validate() error {
	if revocation.SchemaVersion != contracts.SchemaVersionV1 ||
		strings.TrimSpace(revocation.TenantID) == "" ||
		revocation.OrganizationID == "" ||
		!revocation.Kind.Valid() ||
		strings.TrimSpace(revocation.AuthorityID) == "" ||
		revocation.Version == 0 ||
		revocation.OwnerID == "" ||
		strings.TrimSpace(revocation.KeyID) == "" ||
		strings.TrimSpace(revocation.Reason) == "" {
		return fmt.Errorf("policy: revocation is incomplete")
	}
	if revocation.RevokedAt.IsZero() || revocation.RevokedAt.Location() != time.UTC {
		return fmt.Errorf("policy: revocation time must be UTC")
	}
	return revocation.Signature.Validate()
}

type Store struct {
	pool  *pgxpool.Pool
	vault *vault.UserVault
	root  OwnerRoot
	now   func() time.Time
}

func New(
	pool *pgxpool.Pool,
	userVault *vault.UserVault,
	root OwnerRoot,
	now func() time.Time,
) (*Store, error) {
	if pool == nil || userVault == nil || now == nil {
		return nil, fmt.Errorf("policy: pool, Vault, and time source are required")
	}
	if err := root.Validate(); err != nil {
		return nil, err
	}
	if userVault.User() != root.TenantID {
		return nil, fmt.Errorf("policy: Vault user does not match owner root tenant")
	}
	return &Store{pool: pool, vault: userVault, root: root, now: now}, nil
}

func (store *Store) currentTime() (time.Time, error) {
	now := store.now()
	if now.IsZero() || now.Location() != time.UTC {
		return time.Time{}, fmt.Errorf("policy: time source must return UTC")
	}
	return now, nil
}

type Seed struct {
	Organization     contracts.Organization `json:"organization"`
	Mandates         []contracts.Mandate    `json:"mandates"`
	RuntimeAuthority RuntimeAuthority       `json:"runtime_authority"`
	Policies         []contracts.Policy     `json:"policies"`
}

// SeedPublishResult reports whether activation committed new authority or
// matched the already committed canonical seed.
type SeedPublishResult struct {
	Deduplicated bool
}

// Validate enforces the exact canonical topology and one matching mandate for
// every durable seat.
func (seed Seed) Validate() error {
	if err := seed.Organization.Validate(); err != nil {
		return err
	}
	if len(seed.Mandates) != 21 {
		return fmt.Errorf("policy: seed requires exactly twenty-one mandates")
	}
	if err := seed.RuntimeAuthority.Validate(); err != nil {
		return err
	}
	if seed.RuntimeAuthority.OrganizationID != seed.Organization.ID {
		return fmt.Errorf("policy: runtime authority belongs to another organization")
	}
	if len(seed.Policies) != 1 {
		return fmt.Errorf("policy: seed requires exactly one baseline policy")
	}
	if err := seed.Policies[0].Validate(); err != nil {
		return fmt.Errorf("policy: baseline policy: %w", err)
	}
	if seed.Policies[0].OrganizationID != seed.Organization.ID {
		return fmt.Errorf("policy: baseline policy belongs to another organization")
	}
	mandates := make(map[contracts.MandateID]contracts.Mandate, len(seed.Mandates))
	for index := range seed.Mandates {
		mandate := seed.Mandates[index]
		if err := mandate.Validate(); err != nil {
			return fmt.Errorf("policy: mandate %d: %w", index, err)
		}
		if mandate.OrganizationID != seed.Organization.ID {
			return fmt.Errorf("policy: mandate belongs to another organization")
		}
		if _, exists := mandates[mandate.ID]; exists {
			return fmt.Errorf("policy: duplicate mandate %q", mandate.ID)
		}
		mandates[mandate.ID] = mandate
	}
	for _, department := range seed.Organization.Departments {
		for _, seat := range department.Seats {
			mandate, exists := mandates[seat.MandateID]
			if !exists || mandate.Version != seat.MandateVersion ||
				mandate.DepartmentKind != department.Kind ||
				mandate.SeatRole != seat.Role {
				return fmt.Errorf("policy: seat %q mandate binding is invalid", seat.ID)
			}
		}
	}
	return nil
}
