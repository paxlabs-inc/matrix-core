package customer

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"centra/workforce/internal/contracts"
)

func SignConnection(value *Connection, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("customer adapter: connection is required")
	}
	payload, err := connectionSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	return applySignature(&value.Signature, keyID, privateKey, payload, value.Validate)
}

func VerifyConnection(value Connection, keyID string, publicKey ed25519.PublicKey) error {
	payload, err := connectionSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifySignature(value.Signature, keyID, publicKey, payload)
}

func SignConnectionRevocation(value *ConnectionRevocation, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("customer adapter: connection revocation is required")
	}
	payload, err := revocationSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	return applySignature(&value.Signature, keyID, privateKey, payload, value.Validate)
}

func VerifyConnectionRevocation(value ConnectionRevocation, keyID string, publicKey ed25519.PublicKey) error {
	payload, err := revocationSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifySignature(value.Signature, keyID, publicKey, payload)
}

func SignCustomerScope(value *CustomerScope, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("customer adapter: customer scope is required")
	}
	payload, err := customerSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	return applySignature(&value.Signature, keyID, privateKey, payload, value.Validate)
}

func VerifyCustomerScope(value CustomerScope, keyID string, publicKey ed25519.PublicKey) error {
	payload, err := customerSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifySignature(value.Signature, keyID, publicKey, payload)
}

func SignConsentRecord(value *ConsentRecord, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("customer adapter: consent record is required")
	}
	payload, err := consentSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	return applySignature(&value.Signature, keyID, privateKey, payload, value.Validate)
}

func VerifyConsentRecord(value ConsentRecord, keyID string, publicKey ed25519.PublicKey) error {
	payload, err := consentSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verifySignature(value.Signature, keyID, publicKey, payload)
}

func connectionSigningBytes(value Connection, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return contracts.EncodeCanonical(&value)
}

func revocationSigningBytes(value ConnectionRevocation, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return contracts.EncodeCanonical(&value)
}

func customerSigningBytes(value CustomerScope, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return contracts.EncodeCanonical(&value)
}

func consentSigningBytes(value ConsentRecord, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	if err := value.Validate(); err != nil {
		return nil, err
	}
	return contracts.EncodeCanonical(&value)
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func applySignature(
	destination *contracts.Signature,
	keyID string,
	privateKey ed25519.PrivateKey,
	payload []byte,
	validate func() error,
) error {
	if destination == nil || token("signing key id", keyID) != nil ||
		len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("customer adapter: Ed25519 signing authority is invalid")
	}
	*destination = contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return validate()
}

func verifySignature(
	signature contracts.Signature,
	keyID string,
	publicKey ed25519.PublicKey,
	payload []byte,
) error {
	if signature.Algorithm != "ed25519" || signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("customer adapter: signature authority mismatch")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || len(decoded) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("customer adapter: signature verification failed")
	}
	return nil
}
