package organization

import (
	"crypto/ed25519"
	"fmt"
	"slices"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
)

const (
	MigrationManifestSchemaVersion   = "workforce.organization-migration-manifest.v1"
	MigrationActivationSchemaVersion = "workforce.organization-migration-activation.v1"
	MigrationRollbackSchemaVersion   = "workforce.organization-migration-rollback.v1"
	DefaultLegacyTemplateID          = TemplateID("organization-template:default-v1")
)

type MigrationID string

type MigrationState string

const (
	MigrationStaged     MigrationState = "staged"
	MigrationActivated  MigrationState = "activated"
	MigrationRolledBack MigrationState = "rolled_back"
)

func (value MigrationState) Valid() bool {
	return value == MigrationStaged || value == MigrationActivated || value == MigrationRolledBack
}

func RequiredPreservedFamilies() []string {
	return []string{
		"approval", "correction", "department", "effect", "evidence", "graph_node",
		"lease_history", "mail", "mandate", "organization", "policy", "project_brain",
		"receipt", "seat", "work_order",
	}
}

type MigrationManifest struct {
	SchemaVersion              string                   `json:"schema_version"`
	ID                         MigrationID              `json:"migration_id"`
	Version                    uint64                   `json:"version"`
	OrganizationID             contracts.OrganizationID `json:"organization_id"`
	OwnerID                    contracts.OwnerID        `json:"owner_id"`
	FromTemplateID             TemplateID               `json:"from_template_id"`
	FromTemplateVersion        uint64                   `json:"from_template_version"`
	FromOrganizationVersion    uint64                   `json:"from_organization_version"`
	LegacyOrganizationDigest   contracts.ContentHash    `json:"legacy_organization_digest"`
	LegacyAuthoritySetDigest   contracts.ContentHash    `json:"legacy_authority_set_digest"`
	LegacyReceiptSetDigest     contracts.ContentHash    `json:"legacy_receipt_set_digest"`
	ToTemplateID               TemplateID               `json:"to_template_id"`
	ToTemplateVersion          uint64                   `json:"to_template_version"`
	ToTemplateDigest           contracts.ContentHash    `json:"to_template_digest"`
	CapabilityRegistryDigest   contracts.ContentHash    `json:"capability_registry_digest"`
	PreservedRecordFamilies    []string                 `json:"preserved_record_families"`
	ReceiptSchemaVersions      []string                 `json:"receipt_schema_versions"`
	GrantedCapabilities        []CapabilityID           `json:"granted_capabilities"`
	TopologyDepartmentCount    uint16                   `json:"topology_department_count"`
	TopologySeatCount          uint16                   `json:"topology_seat_count"`
	ReversibleBeforeActivation bool                     `json:"reversible_before_activation"`
	PreparedAt                 time.Time                `json:"prepared_at"`
	ActivateNotBefore          time.Time                `json:"activate_not_before"`
	ExpiresAt                  time.Time                `json:"expires_at"`
	Signature                  contracts.Signature      `json:"signature"`
}

func (value MigrationManifest) Validate() error {
	if value.SchemaVersion != MigrationManifestSchemaVersion || value.Version == 0 ||
		value.OrganizationID == "" || value.OwnerID == "" {
		return fmt.Errorf("organization: migration manifest identity is incomplete")
	}
	if err := validateID("migration_id", string(value.ID)); err != nil {
		return err
	}
	if value.FromTemplateID != DefaultLegacyTemplateID || value.FromTemplateVersion != 1 ||
		value.FromOrganizationVersion == 0 {
		return fmt.Errorf("organization: migration source is not the canonical v1 topology")
	}
	if err := value.LegacyOrganizationDigest.Validate(); err != nil {
		return err
	}
	if err := value.LegacyAuthoritySetDigest.Validate(); err != nil {
		return err
	}
	if err := value.LegacyReceiptSetDigest.Validate(); err != nil {
		return err
	}
	if err := validateID("to_template_id", string(value.ToTemplateID)); err != nil {
		return err
	}
	if value.ToTemplateVersion == 0 {
		return fmt.Errorf("organization: migration target template version must be positive")
	}
	if err := value.ToTemplateDigest.Validate(); err != nil {
		return err
	}
	if err := value.CapabilityRegistryDigest.Validate(); err != nil {
		return err
	}
	if err := validateSortedUnique(
		"preserved record families", value.PreservedRecordFamilies,
		len(RequiredPreservedFamilies()), 64,
	); err != nil {
		return err
	}
	for _, required := range RequiredPreservedFamilies() {
		if !containsString(value.PreservedRecordFamilies, required) {
			return fmt.Errorf("organization: migration omits preserved record family %q", required)
		}
	}
	if err := validateSortedUnique(
		"receipt schema versions", value.ReceiptSchemaVersions, 1, 16,
	); err != nil {
		return err
	}
	if !containsString(value.ReceiptSchemaVersions, contracts.SchemaVersionV1) {
		return fmt.Errorf("organization: v1 migration must preserve Workforce v1 receipts")
	}
	if value.GrantedCapabilities == nil || len(value.GrantedCapabilities) != 0 {
		return fmt.Errorf("organization: v1 migration cannot grant capabilities")
	}
	if value.TopologyDepartmentCount < MinimumDepartments ||
		value.TopologyDepartmentCount > MaximumDepartments ||
		value.TopologySeatCount != value.TopologyDepartmentCount*3 ||
		value.TopologySeatCount > MaximumDurableSeats {
		return fmt.Errorf("organization: migration topology counts are invalid")
	}
	if !value.ReversibleBeforeActivation || !validUTC(value.PreparedAt) ||
		!validUTC(value.ActivateNotBefore) || !validUTC(value.ExpiresAt) ||
		value.ActivateNotBefore.Before(value.PreparedAt) || !value.ExpiresAt.After(value.ActivateNotBefore) {
		return fmt.Errorf("organization: migration timing or rollback boundary is invalid")
	}
	return value.Signature.Validate()
}

type MigrationActivation struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        string                   `json:"activation_id"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	OwnerID                   contracts.OwnerID        `json:"owner_id"`
	MigrationID               MigrationID              `json:"migration_id"`
	MigrationVersion          uint64                   `json:"migration_version"`
	ManifestDigest            contracts.ContentHash    `json:"manifest_digest"`
	ExpectedProjectionVersion uint64                   `json:"expected_projection_version"`
	ActivatedAt               time.Time                `json:"activated_at"`
	Signature                 contracts.Signature      `json:"signature"`
}

func (value MigrationActivation) Validate() error {
	if value.SchemaVersion != MigrationActivationSchemaVersion || value.OrganizationID == "" ||
		value.OwnerID == "" || value.MigrationVersion == 0 || value.ExpectedProjectionVersion == 0 {
		return fmt.Errorf("organization: migration activation identity is incomplete")
	}
	if err := validateID("activation_id", value.ID); err != nil {
		return err
	}
	if err := validateID("migration_id", string(value.MigrationID)); err != nil {
		return err
	}
	if err := value.ManifestDigest.Validate(); err != nil {
		return err
	}
	if !validUTC(value.ActivatedAt) {
		return fmt.Errorf("organization: migration activation time must be UTC")
	}
	return value.Signature.Validate()
}

type MigrationRollback struct {
	SchemaVersion    string                   `json:"schema_version"`
	ID               string                   `json:"rollback_id"`
	OrganizationID   contracts.OrganizationID `json:"organization_id"`
	OwnerID          contracts.OwnerID        `json:"owner_id"`
	MigrationID      MigrationID              `json:"migration_id"`
	MigrationVersion uint64                   `json:"migration_version"`
	ManifestDigest   contracts.ContentHash    `json:"manifest_digest"`
	Reason           string                   `json:"reason"`
	RolledBackAt     time.Time                `json:"rolled_back_at"`
	Signature        contracts.Signature      `json:"signature"`
}

func (value MigrationRollback) Validate() error {
	if value.SchemaVersion != MigrationRollbackSchemaVersion || value.OrganizationID == "" ||
		value.OwnerID == "" || value.MigrationVersion == 0 {
		return fmt.Errorf("organization: migration rollback identity is incomplete")
	}
	if err := validateID("rollback_id", value.ID); err != nil {
		return err
	}
	if err := validateID("migration_id", string(value.MigrationID)); err != nil {
		return err
	}
	if err := value.ManifestDigest.Validate(); err != nil {
		return err
	}
	if err := validateText("rollback reason", value.Reason, 1024); err != nil {
		return err
	}
	if !validUTC(value.RolledBackAt) {
		return fmt.Errorf("organization: migration rollback time must be UTC")
	}
	return value.Signature.Validate()
}

type MigrationPreview struct {
	MigrationID              MigrationID
	FromTemplate             string
	ToTemplate               string
	Departments              uint16
	DurableSeats             uint16
	CapabilityRegistryDigest contracts.ContentHash
	PreservedRecordFamilies  []string
	ReceiptSchemaVersions    []string
	AuthorityWidening        bool
	RollbackPoint            string
	IrreversibleConsequence  string
}

func PreviewMigration(value MigrationManifest) (MigrationPreview, error) {
	if err := value.Validate(); err != nil {
		return MigrationPreview{}, err
	}
	return MigrationPreview{
		MigrationID:              value.ID,
		FromTemplate:             fmt.Sprintf("%s@%d", value.FromTemplateID, value.FromTemplateVersion),
		ToTemplate:               fmt.Sprintf("%s@%d", value.ToTemplateID, value.ToTemplateVersion),
		Departments:              value.TopologyDepartmentCount,
		DurableSeats:             value.TopologySeatCount,
		CapabilityRegistryDigest: value.CapabilityRegistryDigest,
		PreservedRecordFamilies:  append([]string(nil), value.PreservedRecordFamilies...),
		ReceiptSchemaVersions:    append([]string(nil), value.ReceiptSchemaVersions...),
		AuthorityWidening:        len(value.GrantedCapabilities) != 0,
		RollbackPoint:            "staged migration may be owner-signed rolled back until atomic activation",
		IrreversibleConsequence:  "activation supersedes the executable topology; rollback requires a later owner-signed migration",
	}, nil
}

func MigrationManifestDigest(value MigrationManifest) (contracts.ContentHash, error) {
	return hashCanonical(&value)
}

func SignMigrationManifest(value *MigrationManifest, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("organization: migration manifest is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifyMigrationManifest(value MigrationManifest, keyID string, key ed25519.PublicKey) error {
	return verifyCanonical(value.Signature, keyID, key, func() ([]byte, error) {
		value.Signature = signaturePreimage(keyID)
		return contracts.EncodeCanonical(&value)
	})
}

func SignMigrationActivation(value *MigrationActivation, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("organization: migration activation is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifyMigrationActivation(value MigrationActivation, keyID string, key ed25519.PublicKey) error {
	return verifyCanonical(value.Signature, keyID, key, func() ([]byte, error) {
		value.Signature = signaturePreimage(keyID)
		return contracts.EncodeCanonical(&value)
	})
}

func SignMigrationRollback(value *MigrationRollback, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("organization: migration rollback is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifyMigrationRollback(value MigrationRollback, keyID string, key ed25519.PublicKey) error {
	return verifyCanonical(value.Signature, keyID, key, func() ([]byte, error) {
		value.Signature = signaturePreimage(keyID)
		return contracts.EncodeCanonical(&value)
	})
}

func normalizedPreservedFamilies(values []string) []string {
	result := append([]string(nil), values...)
	slices.Sort(result)
	return slices.Compact(result)
}

func migrationIdentity(value MigrationID, version uint64) string {
	return strings.Join([]string{string(value), fmt.Sprint(version)}, "@")
}
