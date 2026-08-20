package financial

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"centra/workforce/internal/contracts"
)

func SignConnection(value *Connection, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("financial adapter: connection is required")
	}
	payload, err := connectionSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyConnection(value Connection, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := connectionSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func SignConnectionRevocation(value *ConnectionRevocation, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("financial adapter: connection revocation is required")
	}
	payload, err := revocationSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyConnectionRevocation(value ConnectionRevocation, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := revocationSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func SignValuationSnapshot(value *ValuationSnapshot, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("financial adapter: valuation snapshot is required")
	}
	payload, err := valuationSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyValuationSnapshot(value ValuationSnapshot, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := valuationSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func SignRiskSnapshot(value *RiskSnapshot, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("financial adapter: risk snapshot is required")
	}
	payload, err := riskSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyRiskSnapshot(value RiskSnapshot, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := riskSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func SignFounderReservation(value *FounderReservation, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("financial adapter: founder reservation is required")
	}
	payload, err := founderReservationSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	value.Signature, err = signPayload(keyID, privateKey, payload)
	if err != nil {
		return err
	}
	return value.Validate()
}

func VerifyFounderReservation(value FounderReservation, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := founderReservationSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifyPayload(value.Signature, keyID, publicKey, payload)
}

func connectionSigningBytes(value Connection, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func revocationSigningBytes(value ConnectionRevocation, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func valuationSigningBytes(value ValuationSnapshot, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func riskSigningBytes(value RiskSnapshot, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func founderReservationSigningBytes(value FounderReservation, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func signPayload(keyID string, privateKey ed25519.PrivateKey, payload []byte) (contracts.Signature, error) {
	if token("signing key id", keyID) != nil || len(privateKey) != ed25519.PrivateKeySize {
		return contracts.Signature{}, fmt.Errorf("financial adapter: Ed25519 signing authority is invalid")
	}
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}, nil
}

func verifyPayload(signature contracts.Signature, keyID string, publicKey ed25519.PublicKey, payload []byte) error {
	if signature.Algorithm != "ed25519" || signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("financial adapter: signature authority mismatch")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("financial adapter: signature verification failed")
	}
	return nil
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}
