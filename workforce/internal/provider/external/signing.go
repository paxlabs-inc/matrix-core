package external

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"matrix/workforce/internal/contracts"
)

func SignConnection(value *Connection, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("external adapter: connection is required")
	}
	payload, err := connectionSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	if len(privateKey) != ed25519.PrivateKeySize || token("signing key id", keyID) != nil {
		return fmt.Errorf("external adapter: Ed25519 signing authority is invalid")
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
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
	return verifySignature(value.Signature, keyID, publicKey, payload)
}

func SignConnectionRevocation(
	value *ConnectionRevocation,
	keyID string,
	privateKey ed25519.PrivateKey,
) error {
	if value == nil {
		return fmt.Errorf("external adapter: connection revocation is required")
	}
	payload, err := revocationSigningBytes(*value, keyID)
	if err != nil {
		return err
	}
	if len(privateKey) != ed25519.PrivateKeySize || token("signing key id", keyID) != nil {
		return fmt.Errorf("external adapter: Ed25519 signing authority is invalid")
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func VerifyConnectionRevocation(
	value ConnectionRevocation,
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := revocationSigningBytes(value, keyID)
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

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func verifySignature(
	signature contracts.Signature,
	keyID string,
	publicKey ed25519.PublicKey,
	payload []byte,
) error {
	if signature.Algorithm != "ed25519" || signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("external adapter: signature authority mismatch")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("external adapter: signature verification failed")
	}
	return nil
}
