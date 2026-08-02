package companyruntime

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"matrix/workforce/internal/contracts"
)

func SignStartConfiguration(value *StartConfiguration, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || !validToken(keyID) || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("company runtime: founder signing authority is invalid")
	}
	payload, err := startSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func VerifyStartConfiguration(value StartConfiguration, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := startSigningBytes(value, keyID)
	if err != nil || value.Signature.KeyID != keyID || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("company runtime: founder signature authority is invalid")
	}
	signature, decodeErr := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if decodeErr != nil || !ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("company runtime: founder signature verification failed")
	}
	return nil
}

func signGateAuthorization(value *GateAuthorization, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || !validToken(keyID) || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("company runtime: controller signing authority is invalid")
	}
	payload, err := gateSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func verifyGateAuthorization(value GateAuthorization, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := gateSigningBytes(value, keyID)
	if err != nil || value.Signature.KeyID != keyID || len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("company runtime: gate authorization authority is invalid")
	}
	signature, decodeErr := base64.RawURLEncoding.DecodeString(value.Signature.Value)
	if decodeErr != nil || !ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("company runtime: gate authorization signature failed")
	}
	return nil
}

func startSigningBytes(value StartConfiguration, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func gateSigningBytes(value GateAuthorization, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}
