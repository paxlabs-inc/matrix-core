package learning

import (
	"crypto/ed25519"
	"encoding/base64"
	"fmt"
	"time"

	"centra/workforce/internal/contracts"
)

func SignHypothesis(value *Hypothesis, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("learning: hypothesis is required")
	}
	return sign(value, &value.Signature, keyID, key, value.Validate)
}

func VerifyHypothesis(value Hypothesis, key ed25519.PublicKey) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, value.Validate)
}

func SignObservation(value *Observation, keyID string, key ed25519.PrivateKey, now time.Time) error {
	if value == nil {
		return fmt.Errorf("learning: observation is required")
	}
	return sign(value, &value.Signature, keyID, key, func() error { return value.ValidateAt(now) })
}

func VerifyObservation(value Observation, key ed25519.PublicKey, now time.Time) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, func() error { return value.ValidateAt(now) })
}

func SignEvaluation(value *Evaluation, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("learning: evaluation is required")
	}
	return sign(value, &value.Signature, keyID, key, value.Validate)
}

func VerifyEvaluation(value Evaluation, key ed25519.PublicKey) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, value.Validate)
}

func SignReview(value *IndependentReview, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("learning: review is required")
	}
	return sign(value, &value.Signature, keyID, key, value.Validate)
}

func VerifyReview(value IndependentReview, key ed25519.PublicKey) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, value.Validate)
}

func SignConclusion(value *Conclusion, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("learning: conclusion is required")
	}
	return sign(value, &value.Signature, keyID, key, value.Validate)
}

func VerifyConclusion(value Conclusion, key ed25519.PublicKey) error {
	signature := value.Signature
	value.Signature = placeholder(signature.KeyID)
	return verify(&value, signature, key, value.Validate)
}

func sign(
	value contracts.Validatable,
	signature *contracts.Signature,
	keyID string,
	key ed25519.PrivateKey,
	validate func() error,
) error {
	if token(keyID) != nil || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("learning: signing authority is invalid")
	}
	*signature = placeholder(keyID)
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	payload, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	signature.Value = base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, payload))
	if validate != nil {
		return validate()
	}
	return signature.Validate()
}

func verify(
	value contracts.Validatable,
	signature contracts.Signature,
	key ed25519.PublicKey,
	validate func() error,
) error {
	if len(key) != ed25519.PublicKeySize || signature.Validate() != nil {
		return fmt.Errorf("learning: verification authority is invalid")
	}
	if validate != nil {
		if err := validate(); err != nil {
			return err
		}
	}
	payload, err := contracts.EncodeCanonical(value)
	decoded, decodeErr := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || decodeErr != nil || !ed25519.Verify(key, payload, decoded) {
		return fmt.Errorf("learning: signature verification failed")
	}
	return nil
}

func placeholder(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}
