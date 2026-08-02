package securityqualification

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"matrix/workforce/internal/contracts"
)

func SignThreatModel(value *ThreatModel, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("security qualification: threat model is required")
	}
	return sign(value, &value.Signature, keyID, key, value.Validate)
}

func VerifyThreatModel(value ThreatModel, key ed25519.PublicKey) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, value.Validate)
}

func SignReview(value *BoundaryReview, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("security qualification: review is required")
	}
	return sign(value, &value.Signature, keyID, key, value.Validate)
}

func VerifyReview(value BoundaryReview, key ed25519.PublicKey) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, value.Validate)
}

func SignQualification(value *Qualification, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("security qualification: qualification is required")
	}
	return sign(value, &value.Signature, keyID, key, value.Validate)
}

func VerifyQualification(value Qualification, key ed25519.PublicKey) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, value.Validate)
}

func sign(
	value contracts.Validatable,
	signature *contracts.Signature,
	keyID string,
	key ed25519.PrivateKey,
	validate func() error,
) error {
	if token(keyID) != nil || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("security qualification: signing authority is invalid")
	}
	*signature = placeholder(keyID)
	if err := validate(); err != nil {
		return err
	}
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	return validate()
}

func verify(
	value contracts.Validatable,
	signature contracts.Signature,
	key ed25519.PublicKey,
	validate func() error,
) error {
	if len(key) != ed25519.PublicKeySize || signature.Validate() != nil {
		return fmt.Errorf("security qualification: verification authority is invalid")
	}
	if err := validate(); err != nil {
		return err
	}
	payload, err := contracts.EncodeCanonical(value)
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || decodeErr != nil || !ed25519.Verify(key, payload, decoded) {
		return fmt.Errorf("security qualification: signature verification failed")
	}
	return nil
}

func placeholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}
