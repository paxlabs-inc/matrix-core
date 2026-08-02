package companystate

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"matrix/workforce/internal/contracts"
)

func SignRecord(record *Record, keyID string, privateKey ed25519.PrivateKey) error {
	if record == nil {
		return fmt.Errorf("company state: record is required")
	}
	if err := record.Body.Validate(); err != nil {
		return err
	}
	if err := validateSigningAuthority(keyID, privateKey); err != nil {
		return err
	}
	contentHash, err := hashBody(record.Body)
	if err != nil {
		return err
	}
	record.ContentHash = contentHash
	record.Signature = signaturePlaceholder(keyID)
	payload, err := contracts.EncodeCanonical(record)
	if err != nil {
		return fmt.Errorf("company state: canonical signing record: %w", err)
	}
	record.Signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload))
	return nil
}

func VerifyRecord(record Record, publicKey ed25519.PublicKey) error {
	if err := record.Validate(); err != nil {
		return err
	}
	if len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("company state: Ed25519 verification key is invalid")
	}
	expected, err := hashBody(record.Body)
	if err != nil {
		return err
	}
	if expected != record.ContentHash {
		return fmt.Errorf("company state: content hash mismatch")
	}
	signature, err := base64.RawURLEncoding.DecodeString(record.Signature.Value)
	if err != nil || len(signature) != ed25519.SignatureSize {
		return fmt.Errorf("company state: Ed25519 signature encoding is invalid")
	}
	prepared := record
	prepared.Signature = signaturePlaceholder(record.Signature.KeyID)
	payload, err := contracts.EncodeCanonical(&prepared)
	if err != nil {
		return fmt.Errorf("company state: canonical verification record: %w", err)
	}
	if !ed25519.Verify(publicKey, payload, signature) {
		return fmt.Errorf("company state: Ed25519 signature verification failed")
	}
	return nil
}

func hashBody(body RecordBody) (contracts.ContentHash, error) {
	canonical, err := contracts.EncodeCanonical(&body)
	if err != nil {
		return contracts.ContentHash{}, fmt.Errorf("company state: canonical record body: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func validateSigningAuthority(keyID string, privateKey ed25519.PrivateKey) error {
	if err := validateID("signature key_id", keyID); err != nil {
		return err
	}
	if len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("company state: Ed25519 signing key is invalid")
	}
	return nil
}
