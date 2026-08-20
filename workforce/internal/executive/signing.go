package executive

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"

	"centra/workforce/internal/contracts"
)

// SignDelegationPolicy signs every canonical founder-policy field.
func SignDelegationPolicy(value *DelegationPolicy, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("executive: delegation policy is required")
	}
	payload, err := delegationPolicySigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

// VerifyDelegationPolicy verifies the founder signature over the complete policy.
func VerifyDelegationPolicy(value DelegationPolicy, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := delegationPolicySigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// SignDecisionRequest signs an exact Executive request with its policy-bound seat key.
func SignDecisionRequest(value *DecisionRequest, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("executive: decision request is required")
	}
	payload, err := decisionRequestSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

// VerifyDecisionRequest verifies an Executive request against its policy-bound key.
func VerifyDecisionRequest(value DecisionRequest, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := decisionRequestSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// SignReview signs one fresh independent review with its policy-bound Auditor key.
func SignReview(value *Review, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("executive: independent review is required")
	}
	payload, err := reviewSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

// VerifyReview verifies a complete independent review and its reviewer key.
func VerifyReview(value Review, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := reviewSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// SignPolicyRevocation signs an exact policy-version revocation with founder authority.
func SignPolicyRevocation(value *PolicyRevocation, keyID string, privateKey ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("executive: policy revocation is required")
	}
	payload, err := revocationSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

// VerifyPolicyRevocation verifies a complete founder-signed policy revocation.
func VerifyPolicyRevocation(value PolicyRevocation, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := revocationSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// VerifyDecision verifies a controller-signed Executive decision.
func VerifyDecision(value Decision, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := decisionSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// VerifyFounderDecisionRequest verifies a controller-signed founder escalation.
func VerifyFounderDecisionRequest(value FounderDecisionRequest, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := founderRequestSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// VerifyDecisionIncident verifies a controller-signed decision incident.
func VerifyDecisionIncident(value DecisionIncident, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := incidentSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

// VerifyDecisionConsumption verifies controller-signed one-use decision consumption.
func VerifyDecisionConsumption(value DecisionConsumption, keyID string, publicKey ed25519.PublicKey) error {
	if err := value.Validate(); err != nil {
		return err
	}
	payload, err := consumptionSigningBytes(value, keyID)
	return verifySignature(value.Signature, keyID, publicKey, payload, err)
}

func signDecision(value *Decision, keyID string, privateKey ed25519.PrivateKey) error {
	payload, err := decisionSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

func signFounderRequest(value *FounderDecisionRequest, keyID string, privateKey ed25519.PrivateKey) error {
	payload, err := founderRequestSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

func signIncident(value *DecisionIncident, keyID string, privateKey ed25519.PrivateKey) error {
	payload, err := incidentSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

func signConsumption(value *DecisionConsumption, keyID string, privateKey ed25519.PrivateKey) error {
	payload, err := consumptionSigningBytes(*value, keyID)
	return applySignature(&value.Signature, keyID, privateKey, payload, err)
}

func delegationPolicySigningBytes(value DelegationPolicy, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func decisionRequestSigningBytes(value DecisionRequest, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func reviewSigningBytes(value Review, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func decisionSigningBytes(value Decision, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func founderRequestSigningBytes(value FounderDecisionRequest, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func revocationSigningBytes(value PolicyRevocation, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func incidentSigningBytes(value DecisionIncident, keyID string) ([]byte, error) {
	value.Signature = signaturePlaceholder(keyID)
	return contracts.EncodeCanonical(&value)
}

func consumptionSigningBytes(value DecisionConsumption, keyID string) ([]byte, error) {
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
	if validateToken("signing key id", keyID) != nil || len(privateKey) != ed25519.PrivateKeySize {
		return fmt.Errorf("executive: Ed25519 signing authority is invalid")
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
		return fmt.Errorf("executive: signature authority is invalid")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(publicKey, payload, decoded) {
		return fmt.Errorf("executive: signature verification failed")
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
