package organization

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"centra/workforce/internal/contracts"
)

func SignCapabilityDefinition(value *CapabilityDefinition, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("organization: capability definition is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifyCapabilityDefinition(
	value CapabilityDefinition,
	keyID string,
	key ed25519.PublicKey,
) error {
	return verifyCanonical(value.Signature, keyID, key, func() ([]byte, error) {
		value.Signature = signaturePreimage(keyID)
		return contracts.EncodeCanonical(&value)
	})
}

func SignSeatMandate(value *SeatMandate, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("organization: seat mandate is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifySeatMandate(value SeatMandate, keyID string, key ed25519.PublicKey) error {
	return verifyCanonical(value.Signature, keyID, key, func() ([]byte, error) {
		value.Signature = signaturePreimage(keyID)
		return contracts.EncodeCanonical(&value)
	})
}

func SignOrganizationTemplate(
	value *OrganizationTemplate,
	keyID string,
	key ed25519.PrivateKey,
) error {
	if value == nil {
		return fmt.Errorf("organization: template is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifyOrganizationTemplate(
	value OrganizationTemplate,
	keyID string,
	key ed25519.PublicKey,
) error {
	return verifyCanonical(value.Signature, keyID, key, func() ([]byte, error) {
		value.Signature = signaturePreimage(keyID)
		return contracts.EncodeCanonical(&value)
	})
}

func SignTemplateActivation(
	value *TemplateActivation,
	keyID string,
	key ed25519.PrivateKey,
) error {
	if value == nil {
		return fmt.Errorf("organization: template activation is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifyTemplateActivation(
	value TemplateActivation,
	keyID string,
	key ed25519.PublicKey,
) error {
	return verifyCanonical(value.Signature, keyID, key, func() ([]byte, error) {
		value.Signature = signaturePreimage(keyID)
		return contracts.EncodeCanonical(&value)
	})
}

func CapabilityDigest(value CapabilityDefinition) (contracts.ContentHash, error) {
	return hashCanonical(&value)
}

func SeatMandateDigest(value SeatMandate) (contracts.ContentHash, error) {
	return hashCanonical(&value)
}

func TemplateDigest(value OrganizationTemplate) (contracts.ContentHash, error) {
	return hashCanonical(&value)
}

func hashCanonical[T contracts.Validatable](value T) (contracts.ContentHash, error) {
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func sign(target *contracts.Signature, keyID string, key ed25519.PrivateKey, canonical []byte) error {
	if keyID == "" || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("organization: Ed25519 signing authority is invalid")
	}
	*target = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, canonical)),
	}
	return nil
}

func verifyCanonical(
	signature contracts.Signature,
	keyID string,
	key ed25519.PublicKey,
	canonical func() ([]byte, error),
) error {
	if err := signature.Validate(); err != nil {
		return err
	}
	if signature.KeyID != keyID || len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("organization: signature authority does not match")
	}
	payload, err := canonical()
	if err != nil {
		return err
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(key, payload, decoded) {
		return fmt.Errorf("organization: Ed25519 signature verification failed")
	}
	return nil
}

func signaturePreimage(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}
