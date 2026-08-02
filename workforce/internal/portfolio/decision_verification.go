package portfolio

import (
	"crypto/ed25519"
	"fmt"
)

// VerifyDecision verifies every portfolio-decision field against its exact
// decision authority. A structurally valid receipt alone is not authority.
func VerifyDecision(
	value DecisionReceipt,
	keyID string,
	publicKey ed25519.PublicKey,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := decisionSigningBytes(value, keyID)
	if err := verifySignature(value.Signature, keyID, publicKey, payload, err); err != nil {
		return fmt.Errorf("portfolio: decision: %w", err)
	}
	return nil
}
