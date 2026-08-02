package controlapi

import (
	"context"
	"errors"
	"fmt"
	"time"

	"matrix/workforce/internal/productcapability"
	"matrix/workforce/internal/projectbrain"
)

// ProductRecordCommitResult is the durable record and live-event identity
// returned after both current seat signatures and independent verification
// have passed the product capability store.
type ProductRecordCommitResult struct {
	SchemaVersion string `json:"schema_version"`
	RecordID      string `json:"record_id"`
	Version       uint64 `json:"version"`
	Kind          string `json:"kind"`
	Deduplicated  bool   `json:"deduplicated"`
	EventCursor   uint64 `json:"event_cursor"`
}

// CommitProductRecord accepts only a fully author-signed and independently
// verified record. The bearer session does not replace either seat authority.
func (service *Service) CommitProductRecord(
	ctx context.Context,
	principal Principal,
	value productcapability.VerifiedRecord,
) (ProductRecordCommitResult, error) {
	store, err := service.productStore(principal)
	if err != nil || value.Record.Body.OrganizationID != principal.OrganizationID {
		return ProductRecordCommitResult{}, ErrUnauthorized
	}
	deduplicated, err := store.Commit(ctx, value)
	if err != nil {
		switch {
		case errors.Is(err, productcapability.ErrUnauthorized):
			return ProductRecordCommitResult{}, ErrUnauthorized
		case errors.Is(err, productcapability.ErrConflict):
			return ProductRecordCommitResult{}, ErrConflict
		default:
			return ProductRecordCommitResult{}, err
		}
	}
	body := value.Record.Body
	fields := map[string]any{
		"kind":            body.Kind,
		"initiative_id":   body.InitiativeID,
		"project_id":      body.ProjectID,
		"workspace_id":    body.WorkspaceID,
		"verification_id": value.Verification.ID,
		"outcome":         value.Verification.Outcome,
		"fresh_until":     body.FreshUntil,
	}
	if body.Engineering != nil {
		launch, launchErr := productcapability.EvaluateLaunch(
			*body.Engineering,
			value.Verification.VerifiedAt,
		)
		if launchErr != nil {
			return ProductRecordCommitResult{}, fmt.Errorf(
				"controlapi: evaluate committed launch evidence: %w",
				launchErr,
			)
		}
		fields["launch_state"] = launch.State
		fields["launch_missing"] = launch.Missing
	}
	event, err := service.Publish(ctx, principal, LifecycleEvent{
		ID:                 "event:product-record:" + string(body.ID),
		OrganizationID:     principal.OrganizationID,
		Type:               "product.record.verified",
		ResourceKind:       "product-record",
		ResourceID:         string(body.ID),
		ResourceVersion:    body.Version,
		VerifiedCompletion: false,
		Fields:             fields,
	})
	if err != nil {
		return ProductRecordCommitResult{}, err
	}
	return ProductRecordCommitResult{
		SchemaVersion: SchemaVersion,
		RecordID:      string(body.ID),
		Version:       body.Version,
		Kind:          string(body.Kind),
		Deduplicated:  deduplicated,
		EventCursor:   event.Cursor,
	}, nil
}

func (service *Service) listProductRecords(
	ctx context.Context,
	principal Principal,
	cursor string,
	limit int,
) (ResourcePage, error) {
	const resource = "product-records"
	store, err := service.productStore(principal)
	if err != nil {
		return ResourcePage{}, err
	}
	offset, err := decodePageCursor(resource, cursor)
	if err != nil {
		return ResourcePage{}, err
	}
	values, hasMore, err := store.ListCurrent(ctx, offset, limit)
	if err != nil {
		return ResourcePage{}, err
	}
	now, err := service.currentTime()
	if err != nil {
		return ResourcePage{}, err
	}
	page := ResourcePage{
		SchemaVersion: SchemaVersion,
		Resource:      resource,
		Items:         make([]ResourceItem, 0, len(values)),
	}
	for _, value := range values {
		page.Items = append(page.Items, projectProductRecord(value, now))
	}
	if hasMore {
		page.NextCursor = encodePageCursor(resource, offset+uint64(len(page.Items)))
	}
	return page, nil
}

func (service *Service) productStore(principal Principal) (*productcapability.Store, error) {
	if service.vault == nil || principal.TenantID == "" || principal.OrganizationID == "" ||
		service.vault.User() != principal.TenantID {
		return nil, ErrUnauthorized
	}
	store, err := productcapability.NewStore(
		service.pool,
		service.vault,
		principal.TenantID,
		principal.OrganizationID,
		service.now,
	)
	if err != nil {
		return nil, fmt.Errorf("controlapi: open product capability store: %w", err)
	}
	return store, nil
}

func projectProductRecord(
	value productcapability.VerifiedRecord,
	now time.Time,
) ResourceItem {
	body := value.Record.Body
	verification := value.Verification
	fresh := body.FreshUntil.After(now) && verification.ExpiresAt.After(now)
	fields := map[string]any{
		"chain_id":                body.ChainID,
		"kind":                    body.Kind,
		"initiative_id":           body.InitiativeID,
		"project_id":              body.ProjectID,
		"workspace_id":            body.WorkspaceID,
		"author_seat_id":          body.AuthorSeatID,
		"author_key_id":           value.Record.Signature.KeyID,
		"verifier_seat_id":        verification.VerifierSeatID,
		"verifier_key_id":         verification.Signature.KeyID,
		"verification_id":         verification.ID,
		"verification_state":      verification.Outcome,
		"procedure_id":            verification.ProcedureID,
		"procedure_version":       verification.ProcedureVersion,
		"procedure_digest":        verification.ProcedureDigest,
		"record_hash":             verification.RecordHash,
		"verification_evidence":   verification.Evidence,
		"created_at":              body.CreatedAt,
		"effective_at":            body.EffectiveAt,
		"fresh_until":             body.FreshUntil,
		"verified_at":             verification.VerifiedAt,
		"verification_expires_at": verification.ExpiresAt,
		"fresh":                   fresh,
		"uncertainty":             projectionUncertainty(value, now),
	}
	if body.Supersedes != nil {
		fields["supersedes"] = *body.Supersedes
	}
	switch body.Kind {
	case productcapability.RecordProductDesignHandoff:
		handoff := body.Handoff
		fields["handoff_id"] = handoff.ID
		fields["developer_intent_id"] = handoff.DeveloperIntentID
		fields["product_state"] = handoff.ProductState
		fields["target_segment_state"] = handoff.TargetSegmentState
		fields["value_proposition_state"] = handoff.ValuePropositionState
		fields["acceptance_criteria"] = handoff.AcceptanceCriteria
		fields["experiment_ids"] = handoff.ExperimentIDs
		fields["handoff_expires_at"] = handoff.ExpiresAt
		fields["artifacts"] = projectArtifacts(handoff.Artifacts)
	case productcapability.RecordEngineeringResult:
		result := body.Engineering
		fields["handoff_id"] = result.HandoffID
		fields["developer_intent_id"] = result.DeveloperIntentID
		fields["lease_id"] = result.LeaseID
		fields["fence"] = result.Fence
		fields["project_brain_view_digest"] = result.BrainViewDigest
		fields["source"] = projectSource(result.Source)
		fields["completed_at"] = result.CompletedAt
		fields["artifacts"] = projectArtifacts(result.Artifacts)
		launch, err := productcapability.EvaluateLaunch(*result, verification.VerifiedAt)
		if err == nil {
			fields["launch_state"] = launch.State
			fields["launch_missing"] = launch.Missing
			fields["launch_evidence_hash"] = launch.EvidenceHash
		} else {
			fields["launch_state"] = productcapability.LaunchRequiresHuman
			fields["launch_evaluation_error"] = "verified evidence could not be evaluated"
		}
	case productcapability.RecordMetricDefinition:
		fields["metric"] = body.Metric
	case productcapability.RecordReliabilityIncident:
		fields["incident"] = body.Incident
	}
	return ResourceItem{
		ID:        string(body.ID),
		Version:   body.Version,
		UpdatedAt: verification.VerifiedAt,
		Fields:    fields,
	}
}

func projectArtifacts(values []productcapability.Artifact) []map[string]any {
	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		item := map[string]any{
			"id":             value.ID,
			"kind":           value.Kind,
			"author_seat_id": value.AuthorSeatID,
			"summary":        value.Summary,
			"artifact":       value.Artifact,
			"evidence":       value.Evidence,
			"data_scopes":    value.DataScopes,
			"observed_at":    value.ObservedAt,
			"effective_at":   value.EffectiveAt,
			"fresh_until":    value.FreshUntil,
		}
		if value.Source != nil {
			item["source"] = projectSource(*value.Source)
		}
		result = append(result, item)
	}
	return result
}

func projectSource(value projectbrain.GraphSnapshot) map[string]any {
	return map[string]any{
		"schema_version":     value.SchemaVersion,
		"root_digest":        value.RootDigest,
		"graph_digest":       value.GraphDigest,
		"generation":         value.Generation,
		"indexed_at":         value.IndexedAt,
		"captured_at":        value.CapturedAt,
		"fresh":              value.Fresh,
		"pending_file_count": len(value.PendingFiles),
		"file_count":         len(value.Files),
		"node_count":         value.NodeCount,
		"edge_count":         value.EdgeCount,
	}
}

func projectionUncertainty(
	value productcapability.VerifiedRecord,
	now time.Time,
) []string {
	result := make([]string, 0, 3)
	if !value.Record.Body.FreshUntil.After(now) {
		result = append(result, "record_evidence_expired")
	}
	if !value.Verification.ExpiresAt.After(now) {
		result = append(result, "independent_verification_expired")
	}
	if value.Record.Body.Engineering != nil && !value.Record.Body.Engineering.Source.Fresh {
		result = append(result, "source_snapshot_stale")
	}
	return result
}
