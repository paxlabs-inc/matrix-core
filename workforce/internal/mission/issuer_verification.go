package mission

import (
	"crypto/ed25519"
	"fmt"
	"time"
)

// VerifyCompanyIssuerPolicy verifies the founder signature and establishes
// that the exact issuer delegation is effective at the supplied UTC instant.
func VerifyCompanyIssuerPolicy(
	value CompanyIssuerPolicy,
	founderKeyID string,
	founderPublicKey ed25519.PublicKey,
	at time.Time,
) error {
	if err := value.Validate(); err != nil {
		return err
	}
	if !validUTC(at) || at.Before(value.EffectiveAt) || !at.Before(value.ExpiresAt) {
		return fmt.Errorf("mission: company issuer policy is not current")
	}
	payload, err := issuerSigningBytes(value, founderKeyID)
	if err := verifyPrepared(value.Signature, founderKeyID, founderPublicKey, payload, err); err != nil {
		return fmt.Errorf("mission: company issuer policy: %w", err)
	}
	return nil
}
