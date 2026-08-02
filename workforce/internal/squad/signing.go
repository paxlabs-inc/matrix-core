package squad

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/organization"
)

func SignSeatRuntimeState(
	value *SeatRuntimeState,
	keyID string,
	key ed25519.PrivateKey,
) error {
	if value == nil {
		return fmt.Errorf("squad: runtime seat state is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifySeatRuntimeState(
	value SeatRuntimeState,
	keyID string,
	key ed25519.PublicKey,
) error {
	signature := value.Signature
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return err
	}
	return verify(signature, keyID, key, canonical)
}

func BuildSignedAssignment(
	requirement Requirement,
	template organization.OrganizationTemplate,
	selection Selection,
	keyID string,
	key ed25519.PrivateKey,
) (Assignment, error) {
	if err := requirement.Validate(); err != nil {
		return Assignment{}, err
	}
	if len(selection.Members) < 2 || len(selection.SatisfiedRuleIDs) == 0 {
		return Assignment{}, fmt.Errorf("squad: completed selection is required")
	}
	requirementDigest, err := contracts.HashCanonical(&requirement)
	if err != nil {
		return Assignment{}, err
	}
	assignment := Assignment{
		SchemaVersion: AssignmentSchemaVersion,
		ID:            requirement.ID, OrganizationID: requirement.OrganizationID,
		InitiativeID: requirement.InitiativeID, LifecycleStage: requirement.LifecycleStage,
		GraphScopes:     append([]string(nil), requirement.GraphScopes...),
		ConflictDomains: append([]string(nil), requirement.ConflictDomains...),
		TemplateID:      requirement.TemplateID, TemplateVersion: requirement.TemplateVersion,
		TemplateDigest: requirement.TemplateDigest, RequirementDigest: requirementDigest,
		Members:               copyMembers(selection.Members),
		SatisfiedRuleIDs:      append([]string(nil), selection.SatisfiedRuleIDs...),
		ReceiptSchemaVersions: append([]string(nil), requirement.ReceiptSchemaVersions...),
		AuthorityEffect:       AuthorityEffectNone,
		IssuedAt:              requirement.IssuedAt, ExpiresAt: requirement.ExpiresAt,
		Signature: signaturePreimage(keyID),
	}
	if err := SignAssignment(&assignment, keyID, key); err != nil {
		return Assignment{}, err
	}
	return assignment, nil
}

func SignAssignment(value *Assignment, keyID string, key ed25519.PrivateKey) error {
	if value == nil {
		return fmt.Errorf("squad: assignment is required")
	}
	value.Signature = signaturePreimage(keyID)
	canonical, err := contracts.EncodeCanonical(value)
	if err != nil {
		return err
	}
	return sign(&value.Signature, keyID, key, canonical)
}

func VerifyAssignment(
	value Assignment,
	requirement Requirement,
	template organization.OrganizationTemplate,
	registry *organization.Registry,
	candidates []Candidate,
	keyID string,
	key ed25519.PublicKey,
) error {
	actualCanonical, err := assignmentSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	if err := verify(value.Signature, keyID, key, actualCanonical); err != nil {
		return err
	}
	selection, err := SelectSmallest(requirement, template, registry, candidates)
	if err != nil {
		return err
	}
	requirementDigest, err := contracts.HashCanonical(&requirement)
	if err != nil {
		return err
	}
	expected := Assignment{
		SchemaVersion: AssignmentSchemaVersion,
		ID:            requirement.ID, OrganizationID: requirement.OrganizationID,
		InitiativeID: requirement.InitiativeID, LifecycleStage: requirement.LifecycleStage,
		GraphScopes:     append([]string(nil), requirement.GraphScopes...),
		ConflictDomains: append([]string(nil), requirement.ConflictDomains...),
		TemplateID:      requirement.TemplateID, TemplateVersion: requirement.TemplateVersion,
		TemplateDigest: requirement.TemplateDigest, RequirementDigest: requirementDigest,
		Members:               copyMembers(selection.Members),
		SatisfiedRuleIDs:      append([]string(nil), selection.SatisfiedRuleIDs...),
		ReceiptSchemaVersions: append([]string(nil), requirement.ReceiptSchemaVersions...),
		AuthorityEffect:       AuthorityEffectNone,
		IssuedAt:              requirement.IssuedAt, ExpiresAt: requirement.ExpiresAt,
		Signature: signaturePreimage(keyID),
	}
	expectedCanonical, err := contracts.EncodeCanonical(&expected)
	if err != nil {
		return err
	}
	if !bytes.Equal(actualCanonical, expectedCanonical) {
		return fmt.Errorf("squad: assignment does not match deterministic smallest-qualified selection")
	}
	return nil
}

func VerifyAssignmentSignature(
	value Assignment,
	keyID string,
	key ed25519.PublicKey,
) error {
	canonical, err := assignmentSigningBytes(value, keyID)
	if err != nil {
		return err
	}
	return verify(value.Signature, keyID, key, canonical)
}

func AssignmentDigest(value Assignment) (contracts.ContentHash, error) {
	canonical, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(canonical)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func assignmentSigningBytes(value Assignment, keyID string) ([]byte, error) {
	value.Signature = signaturePreimage(keyID)
	return contracts.EncodeCanonical(&value)
}

func sign(target *contracts.Signature, keyID string, key ed25519.PrivateKey, canonical []byte) error {
	if keyID == "" || len(key) != ed25519.PrivateKeySize {
		return fmt.Errorf("squad: assignment signing authority is invalid")
	}
	*target = contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(ed25519.Sign(key, canonical)),
	}
	return nil
}

func verify(
	signature contracts.Signature,
	keyID string,
	key ed25519.PublicKey,
	canonical []byte,
) error {
	if err := signature.Validate(); err != nil {
		return err
	}
	if signature.KeyID != keyID || len(key) != ed25519.PublicKeySize {
		return fmt.Errorf("squad: signature authority does not match")
	}
	decoded, err := base64.RawURLEncoding.DecodeString(signature.Value)
	if err != nil || !ed25519.Verify(key, canonical, decoded) {
		return fmt.Errorf("squad: Ed25519 signature verification failed")
	}
	return nil
}

func signaturePreimage(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519", KeyID: keyID,
		Value: base64.RawURLEncoding.EncodeToString(make([]byte, ed25519.SignatureSize)),
	}
}

func copyMembers(values []AssignmentMember) []AssignmentMember {
	result := append([]AssignmentMember(nil), values...)
	for index := range result {
		result[index].NeedIDs = append([]string(nil), result[index].NeedIDs...)
	}
	return result
}
