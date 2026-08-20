package securityqualification

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"time"

	"centra/workforce/internal/contracts"
)

func Qualify(
	model ThreatModel,
	reviews []BoundaryReview,
	runtimeKeyID string,
	runtimeKey ed25519.PrivateKey,
	now time.Time,
	validFor time.Duration,
) (Qualification, error) {
	if model.Validate() != nil || !utc(now) || now.Before(model.CreatedAt) ||
		!model.ExpiresAt.After(now) || validFor <= 0 || validFor > 90*24*time.Hour ||
		token(runtimeKeyID) != nil || len(runtimeKey) != ed25519.PrivateKeySize {
		return Qualification{}, fmt.Errorf("security qualification: qualification inputs are invalid")
	}
	modelHash, err := contracts.HashCanonical(&model)
	if err != nil {
		return Qualification{}, err
	}
	for _, hazard := range model.Hazards {
		if (hazard.Severity == SeverityCritical || hazard.Severity == SeverityHigh) &&
			hazard.State != HazardMitigated {
			return Qualification{}, fmt.Errorf(
				"security qualification: hazard %q is not mitigated", hazard.ID,
			)
		}
	}
	type reviewRef struct {
		value BoundaryReview
		hash  contracts.ContentHash
	}
	refs := make([]reviewRef, 0, len(reviews))
	coverage := make(map[Boundary]map[contracts.SeatID]contracts.DepartmentID, len(allBoundaries))
	for _, review := range reviews {
		if review.Validate() != nil || review.OrganizationID != model.OrganizationID ||
			review.ThreatModelID != model.ID || review.ThreatModelHash != modelHash ||
			review.Outcome != ReviewApproved || review.ReviewerSeatID == model.AuthorSeatID ||
			review.ReviewedAt.Before(model.CreatedAt) || review.ReviewedAt.After(now) {
			return Qualification{}, fmt.Errorf("security qualification: review is invalid or non-independent")
		}
		hash, err := contracts.HashCanonical(&review)
		if err != nil {
			return Qualification{}, err
		}
		refs = append(refs, reviewRef{value: review, hash: hash})
		for _, boundary := range review.Boundaries {
			if coverage[boundary] == nil {
				coverage[boundary] = make(map[contracts.SeatID]contracts.DepartmentID)
			}
			coverage[boundary][review.ReviewerSeatID] = review.ReviewerDepartmentID
		}
	}
	for _, boundary := range allBoundaries {
		departments := make(map[contracts.DepartmentID]bool)
		for _, departmentID := range coverage[boundary] {
			departments[departmentID] = true
		}
		if len(coverage[boundary]) < 2 || len(departments) < 2 {
			return Qualification{}, fmt.Errorf(
				"security qualification: boundary %q lacks two independent reviews", boundary,
			)
		}
	}
	sort.Slice(refs, func(left, right int) bool { return refs[left].value.ID < refs[right].value.ID })
	reviewIDs := make([]string, len(refs))
	reviewHashes := make([]contracts.ContentHash, len(refs))
	for index := range refs {
		reviewIDs[index], reviewHashes[index] = refs[index].value.ID, refs[index].hash
		if index > 0 && reviewIDs[index] == reviewIDs[index-1] {
			return Qualification{}, fmt.Errorf("security qualification: duplicate review identity")
		}
	}
	expiresAt := now.Add(validFor)
	if expiresAt.After(model.ExpiresAt) {
		expiresAt = model.ExpiresAt
	}
	qualification := Qualification{
		SchemaVersion:  QualificationSchemaVersion,
		ID:             derivedID("security-qualification", model.ID, modelHash.Digest),
		OrganizationID: model.OrganizationID, ThreatModelID: model.ID,
		ThreatModelHash: modelHash, ReviewIDs: reviewIDs, ReviewHashes: reviewHashes,
		QualifiedBoundaries: AllBoundaries(), QualifiedAt: now, ExpiresAt: expiresAt,
	}
	if err := SignQualification(&qualification, runtimeKeyID, runtimeKey); err != nil {
		return Qualification{}, err
	}
	return qualification, nil
}

func derivedID(kind string, values ...string) string {
	hash := sha256.New()
	for _, value := range values {
		_, _ = hash.Write([]byte(value))
		_, _ = hash.Write([]byte{0})
	}
	return kind + ":" + hex.EncodeToString(hash.Sum(nil))
}
