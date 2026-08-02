package portfolio

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"matrix/workforce/internal/contracts"
)

// SignOpportunity signs the complete canonical opportunity with its author key.
func SignOpportunity(value *Opportunity, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("portfolio: opportunity is required")
	}
	payload, err := opportunitySigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

// VerifyOpportunity verifies author identity and every canonical opportunity field.
func VerifyOpportunity(value Opportunity, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := opportunitySigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// SignProcedure signs the deterministic portfolio procedure with founder authority.
func SignProcedure(value *DecisionProcedure, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("portfolio: decision procedure is required")
	}
	payload, err := procedureSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

// VerifyProcedure verifies the current founder-signed scoring and limit policy.
func VerifyProcedure(value DecisionProcedure, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := procedureSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// SignCadence signs a recurring company cadence with founder authority.
func SignCadence(value *Cadence, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("portfolio: cadence is required")
	}
	payload, err := cadenceSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

// VerifyCadence verifies every immutable recurrence and authority field.
func VerifyCadence(value Cadence, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := cadenceSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

func signDecision(value *DecisionReceipt, keyID string, privateKey ed25519.PrivateKey) error {
	payload, err := decisionSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

func opportunitySigningBytes(value Opportunity, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func procedureSigningBytes(value DecisionProcedure, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func decisionSigningBytes(value DecisionReceipt, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func cadenceSigningBytes(value Cadence, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func applySignature(
	target *contracts.Signature,
	keyID string,
	privateKey ed25519.PrivateKey,
	payload []byte,
	payloadErr error,
) error {
	if payloadErr != nil {
		return payloadErr
	}
	if keyID == "" || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("portfolio: Ed25519 signing authority is invalid")
	}
	*target = contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(ed25519.Sign(privateKey, payload)),
	}
	return nil
}

func verifySignature(
	signature contracts.Signature,
	keyID string,
	publicKey ed25519.PublicKey,
	payload []byte,
	payloadErr error,
) error {
	if payloadErr != nil {
		return payloadErr
	}
	if signature.Algorithm != "ed25519" || signature.KeyID != keyID ||
		len(publicKey) != ed25519.PublicKeySize {
		return fmt.Errorf("portfolio: signature authority is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("portfolio: signature verification failed")
	}
	return nil
}

func signaturePlaceholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value:     base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}
