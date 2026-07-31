package policy

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"time"

	"matrix/workforce/internal/contracts"
)

type organizationPayload struct {
	SchemaVersion string                   `json:"schema_version"`
	ID            contracts.OrganizationID `json:"organization_id"`
	OwnerID       contracts.OwnerID        `json:"owner_id"`
	Version       uint64                   `json:"version"`
	Name          string                   `json:"name"`
	Departments   []contracts.Department   `json:"departments"`
	EffectiveAt   interfaceTime            `json:"effective_at"`
}

type mandatePayload struct {
	SchemaVersion   string                     `json:"schema_version"`
	ID              contracts.MandateID        `json:"mandate_id"`
	Version         uint64                     `json:"version"`
	OrganizationID  contracts.OrganizationID   `json:"organization_id"`
	DepartmentKind  contracts.DepartmentKind   `json:"department_kind"`
	SeatRole        contracts.SeatRole         `json:"seat_role"`
	AllowedSkills   []contracts.SkillID        `json:"allowed_skills"`
	DataScopes      []contracts.DataScope      `json:"data_scopes"`
	EscalationRules []contracts.EscalationRule `json:"escalation_rules"`
	Prohibitions    []contracts.Prohibition    `json:"prohibitions"`
	EffectiveAt     interfaceTime              `json:"effective_at"`
	ExpiresAt       *interfaceTime             `json:"expires_at"`
}

type seatPayload struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             contracts.SeatID         `json:"seat_id"`
	Version        uint64                   `json:"version"`
	DID            contracts.SeatDID        `json:"seat_did"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	DepartmentID   contracts.DepartmentID   `json:"department_id"`
	Role           contracts.SeatRole       `json:"role"`
	MandateID      contracts.MandateID      `json:"mandate_id"`
	MandateVersion uint64                   `json:"mandate_version"`
	BindingID      contracts.SeatBindingID  `json:"binding_id"`
	BindingVersion uint64                   `json:"binding_version"`
	EffectiveAt    interfaceTime            `json:"effective_at"`
}

type policyPayload struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             contracts.PolicyID       `json:"policy_id"`
	Version        uint64                   `json:"version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Kind           string                   `json:"kind"`
	EffectiveAt    interfaceTime            `json:"effective_at"`
	ExpiresAt      *interfaceTime           `json:"expires_at"`
	Rules          []contracts.PolicyRule   `json:"rules"`
}

type runtimeAuthorityPayload struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"runtime_authority_id"`
	Version        uint64                   `json:"version"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	KeyID          string                   `json:"key_id"`
	PublicKey      string                   `json:"public_key"`
	Purposes       []string                 `json:"purposes"`
	EffectiveAt    interfaceTime            `json:"effective_at"`
	ExpiresAt      *interfaceTime           `json:"expires_at"`
}

type grantPayload struct {
	SchemaVersion  string                   `json:"schema_version"`
	TenantID       string                   `json:"tenant_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	OwnerID        contracts.OwnerID        `json:"owner_id"`
	KeyID          string                   `json:"key_id"`
	Scope          string                   `json:"scope"`
	IssuedAt       interfaceTime            `json:"issued_at"`
	ExpiresAt      interfaceTime            `json:"expires_at"`
}

type revocationPayload struct {
	SchemaVersion  string                   `json:"schema_version"`
	TenantID       string                   `json:"tenant_id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	Kind           Kind                     `json:"kind"`
	AuthorityID    string                   `json:"authority_id"`
	Version        uint64                   `json:"version"`
	OwnerID        contracts.OwnerID        `json:"owner_id"`
	KeyID          string                   `json:"key_id"`
	Reason         string                   `json:"reason"`
	RevokedAt      interfaceTime            `json:"revoked_at"`
}

type wakeLeasePayload struct {
	SchemaVersion      string                   `json:"schema_version"`
	ID                 contracts.LeaseID        `json:"lease_id"`
	WakeID             contracts.WakeID         `json:"wake_id"`
	OrganizationID     contracts.OrganizationID `json:"organization_id"`
	SeatID             contracts.SeatID         `json:"seat_id"`
	SeatDID            contracts.SeatDID        `json:"seat_did"`
	Reason             string                   `json:"reason"`
	MandateID          contracts.MandateID      `json:"mandate_id"`
	MandateVersion     uint64                   `json:"mandate_version"`
	Policies           []contracts.PolicyRef    `json:"policies"`
	GraphScope         []contracts.IntentID     `json:"graph_scope"`
	Model              contracts.ModelBinding   `json:"model"`
	MGS                contracts.MGSGenomeRef   `json:"mgs"`
	Runtime            contracts.RuntimeBinding `json:"runtime"`
	SkillCatalogDigest contracts.ContentHash    `json:"skill_catalog_digest"`
	Budget             contracts.WakeBudget     `json:"budget"`
	IssuedAt           interfaceTime            `json:"issued_at"`
	ExpiresAt          interfaceTime            `json:"expires_at"`
	Fence              contracts.FenceToken     `json:"fence"`
}

// interfaceTime aliases time.Time without introducing a custom JSON encoding.
type interfaceTime = time.Time

func (organizationPayload) Validate() error { return nil }
func (mandatePayload) Validate() error      { return nil }
func (seatPayload) Validate() error         { return nil }
func (policyPayload) Validate() error       { return nil }
func (runtimeAuthorityPayload) Validate() error {
	return nil
}
func (grantPayload) Validate() error      { return nil }
func (revocationPayload) Validate() error { return nil }
func (wakeLeasePayload) Validate() error  { return nil }

func SignOrganization(value *contracts.Organization, keyID string, key ed25519.PrivateKey) error {
	payload, err := organizationSigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

func SignMandate(value *contracts.Mandate, keyID string, key ed25519.PrivateKey) error {
	payload, err := mandateSigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

func SignSeat(value *contracts.Seat, keyID string, key ed25519.PrivateKey) error {
	payload, err := seatSigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

func SignPolicy(value *contracts.Policy, keyID string, key ed25519.PrivateKey) error {
	payload, err := policySigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

func SignRuntimeAuthority(
	value *RuntimeAuthority,
	keyID string,
	key ed25519.PrivateKey,
) error {
	if value == nil {
		return fmt.Errorf("policy: runtime authority is required")
	}
	payload, err := runtimeAuthoritySigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

func SignOwnerGrant(value *OwnerGrant, keyID string, key ed25519.PrivateKey) error {
	value.KeyID = keyID
	payload, err := grantSigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

func SignRevocation(value *Revocation, keyID string, key ed25519.PrivateKey) error {
	value.KeyID = keyID
	payload, err := revocationSigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

// SignWakeLease binds every executable wake-authority field to the trusted
// organizational authority key before the lease can be registered.
func SignWakeLease(value *contracts.WakeLease, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("policy: wake lease is required")
	}
	payload, err := wakeLeaseSigningBytes(*value)
	return signValue(&value.Signature, keyID, key, payload, err)
}

func signValue(
	target *contracts.Signature,
	keyID string,
	key ed25519.PrivateKey,
	payload []byte,
	err error,
) error {
	if err != nil {
		return err
	}
	if len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("policy: Ed25519 private key is invalid")
	}
	*target = contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value: base64.RawURLEncoding.EncodeToString(
			ed25519.Sign(key, payload),
		),
	}
	return nil
}

func verifyValue(
	publicKey ed25519.PublicKey,
	expectedKeyID string,
	signature contracts.Signature,
	payload []byte,
	err error,
) error {
	if err != nil {
		return err
	}
	if signature.Algorithm != "ed25519" || signature.KeyID != expectedKeyID {
		return fmt.Errorf("policy: signature does not match owner root")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("policy: owner signature verification failed")
	}
	return nil
}

func verifyOrganization(
	publicKey ed25519.PublicKey,
	keyID string,
	value contracts.Organization,
) error {
	payload, err := organizationSigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

func verifyMandate(
	publicKey ed25519.PublicKey,
	keyID string,
	value contracts.Mandate,
) error {
	payload, err := mandateSigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

func verifySeat(
	publicKey ed25519.PublicKey,
	keyID string,
	value contracts.Seat,
) error {
	payload, err := seatSigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

// VerifySeatAuthority verifies a seat against the configured organizational
// authority without exposing signing payload details to other kernel packages.
func VerifySeatAuthority(
	value contracts.Seat,
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	return verifySeat(publicKey, keyID, value)
}

// VerifyMandateAuthority verifies a mandate against the configured
// organizational authority.
func VerifyMandateAuthority(
	value contracts.Mandate,
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	return verifyMandate(publicKey, keyID, value)
}

// VerifyWakeLeaseAuthority verifies a wake lease against the configured
// organizational authority.
func VerifyWakeLeaseAuthority(
	value contracts.WakeLease,
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	return verifyWakeLease(publicKey, keyID, value)
}

func verifyPolicy(
	publicKey ed25519.PublicKey,
	keyID string,
	value contracts.Policy,
) error {
	payload, err := policySigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

func verifyRuntimeAuthority(
	publicKey ed25519.PublicKey,
	keyID string,
	value RuntimeAuthority,
) error {
	payload, err := runtimeAuthoritySigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

func verifyGrant(
	publicKey ed25519.PublicKey,
	keyID string,
	value OwnerGrant,
) error {
	payload, err := grantSigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

func verifyRevocation(
	publicKey ed25519.PublicKey,
	keyID string,
	value Revocation,
) error {
	payload, err := revocationSigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

func verifyWakeLease(
	publicKey ed25519.PublicKey,
	keyID string,
	value contracts.WakeLease,
) error {
	payload, err := wakeLeaseSigningBytes(value)
	return verifyValue(publicKey, keyID, value.Signature, payload, err)
}

func organizationSigningBytes(value contracts.Organization) ([]byte, error) {
	return contracts.EncodeCanonical(&organizationPayload{
		SchemaVersion: value.SchemaVersion,
		ID:            value.ID, OwnerID: value.OwnerID, Version: value.Version,
		Name: value.Name, Departments: value.Departments, EffectiveAt: value.EffectiveAt,
	})
}

func mandateSigningBytes(value contracts.Mandate) ([]byte, error) {
	var expiresAt *interfaceTime
	if value.ExpiresAt != nil {
		converted := interfaceTime(*value.ExpiresAt)
		expiresAt = &converted
	}
	return contracts.EncodeCanonical(&mandatePayload{
		SchemaVersion: value.SchemaVersion, ID: value.ID, Version: value.Version,
		OrganizationID: value.OrganizationID, DepartmentKind: value.DepartmentKind,
		SeatRole: value.SeatRole, AllowedSkills: value.AllowedSkills,
		DataScopes: value.DataScopes, EscalationRules: value.EscalationRules,
		Prohibitions: value.Prohibitions, EffectiveAt: value.EffectiveAt,
		ExpiresAt: expiresAt,
	})
}

func seatSigningBytes(value contracts.Seat) ([]byte, error) {
	return contracts.EncodeCanonical(&seatPayload{
		SchemaVersion: value.SchemaVersion, ID: value.ID, Version: value.Version,
		DID: value.DID, OrganizationID: value.OrganizationID,
		DepartmentID: value.DepartmentID, Role: value.Role,
		MandateID: value.MandateID, MandateVersion: value.MandateVersion,
		BindingID: value.BindingID, BindingVersion: value.BindingVersion,
		EffectiveAt: value.EffectiveAt,
	})
}

func wakeLeaseSigningBytes(value contracts.WakeLease) ([]byte, error) {
	return contracts.EncodeCanonical(&wakeLeasePayload{
		SchemaVersion: value.SchemaVersion,
		ID:            value.ID, WakeID: value.WakeID, OrganizationID: value.OrganizationID,
		SeatID: value.SeatID, SeatDID: value.SeatDID, Reason: value.Reason,
		MandateID: value.MandateID, MandateVersion: value.MandateVersion,
		Policies:   append([]contracts.PolicyRef(nil), value.Policies...),
		GraphScope: append([]contracts.IntentID(nil), value.GraphScope...),
		Model:      value.Model, MGS: value.MGS, Runtime: value.Runtime,
		SkillCatalogDigest: value.SkillCatalogDigest, Budget: value.Budget,
		IssuedAt: value.IssuedAt, ExpiresAt: value.ExpiresAt, Fence: value.Fence,
	})
}

func policySigningBytes(value contracts.Policy) ([]byte, error) {
	var expiresAt *interfaceTime
	if value.ExpiresAt != nil {
		converted := interfaceTime(*value.ExpiresAt)
		expiresAt = &converted
	}
	return contracts.EncodeCanonical(&policyPayload{
		SchemaVersion: value.SchemaVersion, ID: value.ID, Version: value.Version,
		OrganizationID: value.OrganizationID, Kind: value.Kind,
		EffectiveAt: value.EffectiveAt, ExpiresAt: expiresAt, Rules: value.Rules,
	})
}

func runtimeAuthoritySigningBytes(value RuntimeAuthority) ([]byte, error) {
	var expiresAt *interfaceTime
	if value.ExpiresAt != nil {
		converted := interfaceTime(*value.ExpiresAt)
		expiresAt = &converted
	}
	return contracts.EncodeCanonical(&runtimeAuthorityPayload{
		SchemaVersion: value.SchemaVersion,
		ID:            value.ID, Version: value.Version,
		OrganizationID: value.OrganizationID,
		KeyID:          value.KeyID, PublicKey: value.PublicKey,
		Purposes:    append([]string(nil), value.Purposes...),
		EffectiveAt: value.EffectiveAt, ExpiresAt: expiresAt,
	})
}

func grantSigningBytes(value OwnerGrant) ([]byte, error) {
	return contracts.EncodeCanonical(&grantPayload{
		SchemaVersion: value.SchemaVersion, TenantID: value.TenantID,
		OrganizationID: value.OrganizationID, OwnerID: value.OwnerID,
		KeyID: value.KeyID, Scope: value.Scope,
		IssuedAt: value.IssuedAt, ExpiresAt: value.ExpiresAt,
	})
}

func revocationSigningBytes(value Revocation) ([]byte, error) {
	return contracts.EncodeCanonical(&revocationPayload{
		SchemaVersion: value.SchemaVersion, TenantID: value.TenantID,
		OrganizationID: value.OrganizationID, Kind: value.Kind,
		AuthorityID: value.AuthorityID, Version: value.Version,
		OwnerID: value.OwnerID, KeyID: value.KeyID,
		Reason: value.Reason, RevokedAt: value.RevokedAt,
	})
}
