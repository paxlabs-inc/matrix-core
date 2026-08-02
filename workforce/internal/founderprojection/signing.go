package founderprojection

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

type receiptSigningPayload struct {
	SchemaVersion  string          `json:"schema_version"`
	ReceiptID      string          `json:"receipt_id"`
	Version        uint64          `json:"version"`
	OrganizationID string          `json:"organization_id"`
	InitiativeID   string          `json:"initiative_id"`
	Authoritative  Snapshot        `json:"authoritative_snapshot"`
	Rendered       Snapshot        `json:"rendered_snapshot"`
	Process        ProcessIdentity `json:"process"`
	Evidence       RenderEvidence  `json:"evidence"`
	RenderedAt     string          `json:"rendered_at"`
	ExpiresAt      string          `json:"expires_at"`
	CreatedAt      string          `json:"created_at"`
}

func receiptSigningBytes(value Receipt) ([]byte, error) {
	return json.Marshal(receiptSigningPayload{
		SchemaVersion: value.SchemaVersion, ReceiptID: value.ID, Version: value.Version,
		OrganizationID: string(value.OrganizationID), InitiativeID: value.InitiativeID,
		Authoritative: value.Authoritative, Rendered: value.Rendered,
		Process: value.Process, Evidence: value.Evidence,
		RenderedAt: value.RenderedAt.Format(timeFormat),
		ExpiresAt:  value.ExpiresAt.Format(timeFormat),
		CreatedAt:  value.CreatedAt.Format(timeFormat),
	})
}

func signReceipt(value *Receipt, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil || token(keyID) != nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("founder projection: signing authority is invalid")
	}
	payload, err := receiptSigningBytes(*value)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(payload)
	value.CanonicalHash.Algorithm = "sha256"
	value.CanonicalHash.Digest = hex.EncodeToString(sum[:])
	value.SignerKeyID = keyID
	value.Signature = ed25519.Sign(privateKey, payload)
	return nil
}

func verifyReceipt(value Receipt, publicKey ed25519.PublicKey) error {
	payload, err := receiptSigningBytes(value)
	if err != nil || len(publicKey) != ed25519.PublicKeySize ||
		len(value.Signature) != ed25519.SignatureSize ||
		!ed25519.Verify(publicKey, payload, value.Signature) {
		return fmt.Errorf("founder projection: receipt signature is invalid")
	}
	sum := sha256.Sum256(payload)
	if value.CanonicalHash.Algorithm != "sha256" ||
		value.CanonicalHash.Digest != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("founder projection: receipt canonical hash is invalid")
	}
	return nil
}
