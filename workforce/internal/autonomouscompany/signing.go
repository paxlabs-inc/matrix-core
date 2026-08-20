package autonomouscompany

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"

	"centra/workforce/internal/contracts"
)

func signPropertyRecord(value *PropertyRecord, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || token(keyID) != nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("autonomous company: property signing authority is invalid")
	}
	value.ContentHash = markerHash()
	value.Signature = markerSignature(keyID)
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.ContentHash = digest(payload)
	value.Signature = markerSignature(keyID)
	payload, err = contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func verifyPropertyRecord(value PropertyRecord, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil || value.Signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return ErrIntegrity
	}
	payload, err := propertySigningPayload(value)
	if err != nil || verifySignature(value.Signature, payload, publicKey) != nil {
		return ErrIntegrity
	}
	hashing := value
	hashing.ContentHash = markerHash()
	hashing.Signature = markerSignature(keyID)
	payload, err = contracts.EncodeCanonical(&hashing)
	if err != nil || digest(payload) != value.ContentHash {
		return ErrIntegrity
	}
	return nil
}

func propertySigningPayload(value PropertyRecord) ([]byte, error) {
	signing := value
	signing.Signature = markerSignature(value.Signature.KeyID)
	return contracts.EncodeCanonical(&signing)
}

func signNextCyclePlan(value *NextCyclePlan, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || token(keyID) != nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("autonomous company: next-cycle plan signing authority is invalid")
	}
	value.ContentHash = markerHash()
	value.Signature = markerSignature(keyID)
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.ContentHash = digest(payload)
	value.Signature = markerSignature(keyID)
	payload, err = contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func verifyNextCyclePlan(value NextCyclePlan, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil || value.Signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return ErrIntegrity
	}
	signing := value
	signing.Signature = markerSignature(keyID)
	payload, err := contracts.EncodeCanonical(&signing)
	if err != nil || verifySignature(value.Signature, payload, publicKey) != nil {
		return ErrIntegrity
	}
	hashing := value
	hashing.ContentHash = markerHash()
	hashing.Signature = markerSignature(keyID)
	payload, err = contracts.EncodeCanonical(&hashing)
	if err != nil || digest(payload) != value.ContentHash {
		return ErrIntegrity
	}
	return nil
}

func signNextCycleEvent(value *NextCycleEvent, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || token(keyID) != nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("autonomous company: next-cycle event signing authority is invalid")
	}
	value.ContentHash = markerHash()
	value.Signature = markerSignature(keyID)
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.ContentHash = digest(payload)
	value.Signature = markerSignature(keyID)
	payload, err = contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	value.Signature = contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return value.Validate()
}

func verifyNextCycleEvent(value NextCycleEvent, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil || value.Signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return ErrIntegrity
	}
	signing := value
	signing.Signature = markerSignature(keyID)
	payload, err := contracts.EncodeCanonical(&signing)
	if err != nil || verifySignature(value.Signature, payload, publicKey) != nil {
		return ErrIntegrity
	}
	hashing := value
	hashing.ContentHash = markerHash()
	hashing.Signature = markerSignature(keyID)
	payload, err = contracts.EncodeCanonical(&hashing)
	if err != nil || digest(payload) != value.ContentHash {
		return ErrIntegrity
	}
	return nil
}

func verifySignature(
	signature contracts.Signature,
	payload []byte,
	publicKey ed25519.PublicKey,
) error {
	if signature.Algorithm != "ed25519" {
		return ErrIntegrity
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || len(decoded) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, payload, decoded) {
		return ErrIntegrity
	}
	return nil
}

func markerHash() contracts.ContentHash {
	return contracts.ContentHash{Algorithm: "sha256", Digest: strings.Repeat("0", 64)}
}

func markerSignature(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func digest(value []byte) contracts.ContentHash {
	hash := sha256.Sum256(value)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(hash[:])}
}
