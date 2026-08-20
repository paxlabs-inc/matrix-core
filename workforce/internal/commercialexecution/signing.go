package commercialexecution

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"centra/workforce/internal/contracts"
)

func SignPlan(value *Plan, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("commercial execution: plan is required")
	}
	payload, err := planSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyPlan(value Plan, keyID string, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("commercial execution: plan is invalid")
	}
	payload, err := planSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func SignEvidence(value *Evidence, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("commercial execution: evidence is required")
	}
	payload, err := evidenceSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyEvidence(value Evidence, keyID string, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("commercial execution: evidence is invalid")
	}
	payload, err := evidenceSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func SignCorrection(value *Correction, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("commercial execution: correction is required")
	}
	payload, err := correctionSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyCorrection(value Correction, keyID string, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("commercial execution: correction is invalid")
	}
	payload, err := correctionSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func SignRecovery(value *Recovery, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("commercial execution: recovery is required")
	}
	payload, err := recoverySigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyRecovery(value Recovery, keyID string, publicKey ed25519.PublicKey) error {
	if value.Validate() != nil {
		return fmt.Errorf("commercial execution: recovery is invalid")
	}
	payload, err := recoverySigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func PlanHash(value Plan) (contracts.ContentHash, error) {
	return contracts.HashCanonical(&value)
}

func EvidenceHash(value Evidence) (contracts.ContentHash, error) {
	return contracts.HashCanonical(&value)
}

func CorrectionHash(value Correction) (contracts.ContentHash, error) {
	return contracts.HashCanonical(&value)
}

func RecoveryHash(value Recovery) (contracts.ContentHash, error) {
	return contracts.HashCanonical(&value)
}

func planSigningBytes(value Plan, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func evidenceSigningBytes(value Evidence, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func correctionSigningBytes(value Correction, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func recoverySigningBytes(value Recovery, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func signPayload(keyID string, privateKey ed25519.PrivateKey, payload []byte) (contracts.Signature, error) {
	if token("signing key id", keyID) != nil || len(privateKey) != ed25519.PrivateKeySize {
		return contracts.Signature{}, fmt.Errorf("commercial execution: Ed25519 signing authority is invalid")
	}
	return contracts.Signature{Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))}, nil
}

func verifyPayload(signature contracts.Signature, keyID string, publicKey ed25519.PublicKey, payload []byte) error {
	if signature.Algorithm != "ed25519" || signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("commercial execution: signature authority mismatch")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("commercial execution: signature verification failed")
	}
	return nil
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))}
}
