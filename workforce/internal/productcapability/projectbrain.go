package productcapability

import (
	"fmt"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/projectbrain"
)

// NewProjectBrainProposal projects a verified Product/Design handoff or
// Developer result into the Developer-only engineering continuity boundary.
// The returned proposal is intentionally unsigned; the fresh Developer seat
// must sign it with projectbrain.SignProposal and a separate current seat must
// independently verify it before Project Brain accepts it.
func NewProjectBrainProposal(
	value VerifiedRecord,
	recordID projectbrain.RecordID,
	authorSeatID contracts.SeatID,
	parentIntentID contracts.IntentID,
	source projectbrain.GraphSnapshot,
	createdAt time.Time,
) (projectbrain.Proposal, error) {
	if err := value.ValidateAt(createdAt); err != nil {
		return projectbrain.Proposal{}, err
	}
	if err := source.Validate(); err != nil {
		return projectbrain.Proposal{}, fmt.Errorf("product capability: Project Brain source: %w", err)
	}
	if !source.Fresh || !validUTC(createdAt) || source.CapturedAt.After(createdAt) {
		return projectbrain.Proposal{}, fmt.Errorf("product capability: Project Brain source is stale")
	}
	if err := validateToken("project brain record id", string(recordID)); err != nil {
		return projectbrain.Proposal{}, err
	}
	if err := validateToken("project brain author seat id", string(authorSeatID)); err != nil {
		return projectbrain.Proposal{}, err
	}
	if err := validateToken("project brain parent intent id", string(parentIntentID)); err != nil {
		return projectbrain.Proposal{}, err
	}
	body := value.Record.Body
	if body.Kind != RecordProductDesignHandoff && body.Kind != RecordEngineeringResult {
		return projectbrain.Proposal{}, fmt.Errorf("product capability: record kind cannot enter Project Brain")
	}
	verificationHash, err := canonicalHash(value.Verification)
	if err != nil {
		return projectbrain.Proposal{}, err
	}
	verificationEvidence := contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.EvidenceID(value.Verification.ID),
		Hash:          verificationHash,
		Kind:          string(body.Kind),
		ObservedAt:    value.Verification.VerifiedAt,
	}
	var stateEvidence []contracts.EvidenceRef
	if body.Handoff != nil {
		stateEvidence = companyStateEvidence(*body.Handoff, createdAt)
	}
	claims := make([]projectbrain.Claim, 0, len(body.artifacts()))
	artifacts := make([]contracts.ArtifactRef, 0, len(body.artifacts()))
	for _, artifact := range body.artifacts() {
		evidence := append([]contracts.EvidenceRef{verificationEvidence}, stateEvidence...)
		evidence = append(evidence, artifact.Evidence...)
		claims = append(claims, projectbrain.Claim{
			Statement: fmt.Sprintf(
				"Verified %s %s from capability record %s",
				artifact.Kind, handoffIdentity(body), body.ID,
			),
			Evidence: evidence,
		})
		artifacts = append(artifacts, artifact.Artifact)
	}
	expiresAt := body.FreshUntil
	kind := projectbrain.KindHandoff
	if body.Kind == RecordEngineeringResult {
		kind = projectbrain.KindOutcome
	}
	return projectbrain.Proposal{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             recordID,
		OrganizationID: body.OrganizationID,
		ProjectID:      body.ProjectID,
		WorkspaceID:    body.WorkspaceID,
		AuthorSeatID:   authorSeatID,
		ParentIntentID: parentIntentID,
		Kind:           kind,
		Origin:         projectbrain.OriginReceipt,
		Version:        1,
		Source:         source,
		Content: projectbrain.Content{
			Summary: fmt.Sprintf(
				"Verified %s %s for initiative %s",
				body.Kind, handoffIdentity(body), body.InitiativeID,
			),
			Claims: claims, Artifacts: artifacts, ExpiresAt: &expiresAt,
		},
		CreatedAt: createdAt,
	}, nil
}

func companyStateEvidence(value ProductDesignHandoff, observedAt time.Time) []contracts.EvidenceRef {
	references := []struct {
		value CompanyStateBinding
	}{
		{value.ProductState},
		{value.TargetSegmentState},
		{value.ValuePropositionState},
	}
	result := make([]contracts.EvidenceRef, 0, len(references))
	for _, reference := range references {
		result = append(result, contracts.EvidenceRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID: contracts.EvidenceID(fmt.Sprintf(
				"company-state:%s:%s", reference.value.Kind,
				reference.value.Reference.ContentHash.Digest[:32],
			)),
			Hash: reference.value.Reference.ContentHash, Kind: string(reference.value.Kind),
			ObservedAt: observedAt,
		})
	}
	return result
}

func handoffIdentity(value RecordBody) string {
	if value.Handoff != nil {
		return string(value.Handoff.ID)
	}
	if value.Engineering != nil {
		return string(value.Engineering.HandoffID)
	}
	return string(value.ID)
}
