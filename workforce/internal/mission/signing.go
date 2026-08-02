package mission

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"matrix/workforce/internal/contracts"
)

// SignFounderMission signs every canonical Mission field with the local
// founder key.
func SignFounderMission(value *FounderMission, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("mission: founder mission is required")
	}
	payload, err := missionSigningBytes(*value, keyID)
	return sign(&value.Signature, keyID, key, payload, err)
}

// SignCompanyConstitution signs every canonical Constitution field with the
// local founder key.
func SignCompanyConstitution(value *CompanyConstitution, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("mission: company constitution is required")
	}
	payload, err := constitutionSigningBytes(*value, keyID)
	return sign(&value.Signature, keyID, key, payload, err)
}

// SignCapitalEnvelope signs the exact delegated capital limits.
func SignCapitalEnvelope(value *CapitalEnvelope, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("mission: capital envelope is required")
	}
	payload, err := capitalSigningBytes(*value, keyID)
	return sign(&value.Signature, keyID, key, payload, err)
}

// SignCompanyIssuerPolicy signs the controller's bounded issuance authority.
func SignCompanyIssuerPolicy(value *CompanyIssuerPolicy, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("mission: company issuer policy is required")
	}
	payload, err := issuerSigningBytes(*value, keyID)
	return sign(&value.Signature, keyID, key, payload, err)
}

// SignOrganizationV2 signs the exact default-template projection and its
// bindings to the founder company-authority records.
func SignOrganizationV2(value *OrganizationV2, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("mission: organization-v2 authority is required")
	}
	payload, err := organizationV2SigningBytes(*value, keyID)
	return sign(&value.Signature, keyID, key, payload, err)
}

// VerifyActivationAuthority verifies every record against the same founder
// authority and rejects cross-record inconsistencies.
func VerifyActivationAuthority(value ActivationAuthority, keyID string, key ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := missionSigningBytes(value.Mission, keyID)
	if err := verifyPrepared(value.Mission.Signature, keyID, key, payload, err); err != nil {
		return fmt.Errorf("mission: founder mission: %w", err)
	}
	payload, err = constitutionSigningBytes(value.Constitution, keyID)
	if err := verifyPrepared(value.Constitution.Signature, keyID, key, payload, err); err != nil {
		return fmt.Errorf("mission: company constitution: %w", err)
	}
	payload, err = capitalSigningBytes(value.Capital, keyID)
	if err := verifyPrepared(value.Capital.Signature, keyID, key, payload, err); err != nil {
		return fmt.Errorf("mission: capital envelope: %w", err)
	}
	payload, err = issuerSigningBytes(value.IssuerPolicy, keyID)
	if err := verifyPrepared(value.IssuerPolicy.Signature, keyID, key, payload, err); err != nil {
		return fmt.Errorf("mission: company issuer policy: %w", err)
	}
	payload, err = organizationV2SigningBytes(value.Organization, keyID)
	if err := verifyPrepared(value.Organization.Signature, keyID, key, payload, err); err != nil {
		return fmt.Errorf("mission: organization-v2 authority: %w", err)
	}
	return nil
}

func verifyPrepared(signature contracts.Signature, keyID string, key ed25519.PublicKey, payload []byte, err error) error {
	if err != nil {
		return err
	}
	return verify(signature, keyID, key, payload)
}

func sign(target *contracts.Signature, keyID string, key ed25519.PrivateKey, payload []byte, err error) error {
	if err != nil {
		return err
	}
	if len(key) != ed25519.PrivateKeySize || keyID == "" {
		return fmt.Errorf("mission: Ed25519 signing authority is invalid")
	}
	*target = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload)),
	}
	return nil
}

func verify(signature contracts.Signature, keyID string, key ed25519.PublicKey, payload []byte) error {
	if signature.Algorithm != "ed25519" || signature.KeyID != keyID ||
		len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("signature does not match founder authority")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(key, payload, decoded) {
		return fmt.Errorf("founder signature verification failed")
	}
	return nil
}

func missionSigningBytes(value FounderMission, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func constitutionSigningBytes(value CompanyConstitution, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func capitalSigningBytes(value CapitalEnvelope, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func issuerSigningBytes(value CompanyIssuerPolicy, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func organizationV2SigningBytes(value OrganizationV2, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}
