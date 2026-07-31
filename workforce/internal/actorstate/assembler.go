package actorstate

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/ledger"
	"matrix/workforce/internal/mail"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/projectbrain"
	"matrix/workforce/internal/skills"
)

type ProjectBrainReader interface {
	View(context.Context, projectbrain.CapabilityGrant) (projectbrain.View, error)
}

type Assembler struct {
	graph   *dependency.Store
	ledger  *ledger.Store
	mail    *mail.Store
	policy  *policy.Store
	leases  *lease.Store
	catalog *skills.Catalog
	brain   ProjectBrainReader
	now     func() time.Time
}

type AssemblyRequest struct {
	LeaseID        contracts.LeaseID
	Goal           contracts.Goal
	Intent         contracts.Intent
	RecordIDs      []contracts.RecordID
	Artifacts      []contracts.ArtifactRef
	Evidence       []contracts.EvidenceRef
	Tools          []contracts.ToolRef
	RequiredOutput []contracts.RequiredOutput
	ProjectBrain   *projectbrain.CapabilityGrant
	InboxLimit     uint32
}

func NewAssembler(
	graph *dependency.Store,
	records *ledger.Store,
	mailbox *mail.Store,
	authority *policy.Store,
	leases *lease.Store,
	catalog *skills.Catalog,
	brain ProjectBrainReader,
	now func() time.Time,
) (*Assembler, error) {
	if graph == nil || records == nil || mailbox == nil || authority == nil ||
		leases == nil || catalog == nil || now == nil {
		return nil, fmt.Errorf("actorstate: complete current-state sources are required")
	}
	return &Assembler{
		graph: graph, ledger: records, mail: mailbox, policy: authority,
		leases: leases, catalog: catalog, brain: brain, now: now,
	}, nil
}

func (assembler *Assembler) Assemble(
	ctx context.Context,
	request AssemblyRequest,
) (contracts.WorkPacket, error) {
	now := assembler.now()
	if now.IsZero() || now.Location() != time.UTC {
		return contracts.WorkPacket{}, fmt.Errorf("actorstate: time source must return UTC")
	}
	authorityLease, err := assembler.policy.LoadLease(ctx, request.LeaseID)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	if err := assembler.policy.AuthorizeLease(ctx, request.LeaseID); err != nil {
		return contracts.WorkPacket{}, err
	}
	runtimeLease, err := assembler.leases.Authorize(
		ctx, authorityLease.OrganizationID, request.LeaseID, authorityLease.Fence,
	)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	if runtimeLease.WakeID != authorityLease.WakeID ||
		runtimeLease.SeatID != authorityLease.SeatID ||
		runtimeLease.NodeID != dependency.NodeID(authorityLease.GraphScope[0]) ||
		runtimeLease.MandateID != authorityLease.MandateID ||
		runtimeLease.MandateVersion != authorityLease.MandateVersion {
		return contracts.WorkPacket{}, fmt.Errorf("actorstate: authority and runtime leases disagree")
	}
	seat, err := assembler.policy.LoadCurrentSeat(ctx, authorityLease.SeatID)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	mandate, err := assembler.policy.LoadMandate(
		ctx, authorityLease.MandateID, authorityLease.MandateVersion,
	)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	if seat.DID != authorityLease.SeatDID || seat.MandateID != mandate.ID ||
		seat.MandateVersion != mandate.Version {
		return contracts.WorkPacket{}, fmt.Errorf("actorstate: seat authority does not match lease mandate")
	}
	for _, reference := range authorityLease.Policies {
		if _, err := assembler.policy.LoadPolicy(ctx, reference.ID, reference.Version); err != nil {
			return contracts.WorkPacket{}, err
		}
	}
	if assembler.catalog.Digest() != authorityLease.SkillCatalogDigest {
		return contracts.WorkPacket{}, fmt.Errorf("actorstate: current skill catalog does not match lease")
	}
	skillRefs, err := assembler.catalog.Resolve(mandate.AllowedSkills)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	dependencies, err := assembler.graphSlice(ctx, authorityLease, seat, request)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	brain, err := assembler.openProjectBrain(ctx, authorityLease, seat, request.ProjectBrain)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	records, artifacts, err := assembler.openRecords(
		ctx, authorityLease, seat, mandate, request, brain,
	)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	inbox, mailArtifacts, evidence, err := assembler.openInbox(ctx, authorityLease, request.InboxLimit)
	if err != nil {
		return contracts.WorkPacket{}, err
	}
	artifacts = append(artifacts, request.Artifacts...)
	artifacts = append(artifacts, mailArtifacts...)
	artifacts = uniqueArtifacts(artifacts)
	evidence = append(evidence, request.Evidence...)
	evidence = uniqueEvidence(evidence)
	packet := contracts.WorkPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		Lease:         authorityLease, Seat: seat, Mandate: mandate,
		Goal: request.Goal, Intent: request.Intent,
		VerifiedState: records, Dependencies: dependencies,
		Artifacts: artifacts, Evidence: evidence, Inbox: inbox,
		Tools:  append([]contracts.ToolRef(nil), request.Tools...),
		Skills: skillRefs, Policies: append([]contracts.PolicyRef(nil), authorityLease.Policies...),
		RequiredOutputs: append([]contracts.RequiredOutput(nil), request.RequiredOutput...),
		ProjectBrain:    brain, AssembledAt: now,
	}
	if err := packet.Validate(); err != nil {
		return contracts.WorkPacket{}, err
	}
	return packet, nil
}

func (assembler *Assembler) graphSlice(
	ctx context.Context,
	authority contracts.WakeLease,
	seat contracts.Seat,
	request AssemblyRequest,
) ([]contracts.IntentID, error) {
	if err := request.Goal.Validate(); err != nil {
		return nil, err
	}
	if err := request.Intent.Validate(); err != nil {
		return nil, err
	}
	if request.Goal.OrganizationID != authority.OrganizationID ||
		request.Intent.OrganizationID != authority.OrganizationID ||
		request.Intent.ID != authority.GraphScope[0] ||
		request.Intent.OwnerSeatID != seat.ID {
		return nil, fmt.Errorf("actorstate: requested work is outside lease graph authority")
	}
	snapshot, err := assembler.graph.Snapshot(ctx, authority.OrganizationID)
	if err != nil {
		return nil, err
	}
	selected := dependency.NodeID(request.Intent.ID)
	found := false
	for _, node := range snapshot.Nodes {
		if node.ID != selected {
			continue
		}
		found = true
		if node.State != dependency.StateEligible || node.Contested ||
			node.OwnerSeatID == nil || *node.OwnerSeatID != seat.ID {
			return nil, fmt.Errorf("actorstate: selected intent is not currently eligible for the seat")
		}
	}
	if !found {
		return nil, dependency.ErrNotFound
	}
	result := make([]contracts.IntentID, 0)
	for _, edge := range snapshot.Edges {
		if edge.Dependent == selected {
			result = append(result, contracts.IntentID(edge.Prerequisite))
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i] < result[j] })
	return result, nil
}

func (assembler *Assembler) openRecords(
	ctx context.Context,
	authority contracts.WakeLease,
	seat contracts.Seat,
	mandate contracts.Mandate,
	request AssemblyRequest,
	brain *contracts.ProjectBrainRef,
) ([]contracts.RecordRef, []contracts.ArtifactRef, error) {
	if len(request.RecordIDs) > 0 && len(mandate.DataScopes) == 0 {
		return nil, nil, fmt.Errorf(
			"actorstate: signed mandate grants no data scopes for requested records",
		)
	}
	records := make([]contracts.RecordRef, 0, len(request.RecordIDs))
	artifacts := make([]contracts.ArtifactRef, 0, len(request.RecordIDs))
	for _, id := range request.RecordIDs {
		var record contracts.Record
		var openErr error
		for _, scope := range mandate.DataScopes {
			grant := ledger.AccessGrant{
				OrganizationID: authority.OrganizationID,
				SeatID:         seat.ID,
				DepartmentID:   &seat.DepartmentID,
				Purpose:        scope.Purpose,
				Classifications: []contracts.Classification{
					scope.Classification,
				},
				Restricted: scope.Classification == contracts.ClassificationRestricted,
				ExpiresAt:  authority.ExpiresAt,
			}
			if brain != nil {
				projectID := brain.ProjectID
				grant.ProjectID = &projectID
			}
			record, openErr = assembler.ledger.OpenRecord(ctx, ledger.OpenRequest{
				OrganizationID: authority.OrganizationID,
				RecordID:       id,
				Grant:          grant,
				IdempotencyKey: "packet:" + string(authority.WakeID) + ":" +
					string(id) + ":" + scope.Name,
			})
			if openErr == nil {
				break
			}
			if !errors.Is(openErr, ledger.ErrNotFound) {
				return nil, nil, openErr
			}
		}
		if openErr != nil {
			return nil, nil, openErr
		}
		if record.Validity != contracts.ValidityActive ||
			record.ParentIntentID != request.Intent.ID {
			return nil, nil, fmt.Errorf("actorstate: record %q is not verified current intent state", id)
		}
		records = append(records, contracts.RecordRef{
			ID: record.ID, Kind: record.Kind, Hash: record.ContentHash,
		})
		artifacts = append(artifacts, record.Payload)
	}
	return records, artifacts, nil
}

func (assembler *Assembler) openProjectBrain(
	ctx context.Context,
	authority contracts.WakeLease,
	seat contracts.Seat,
	grant *projectbrain.CapabilityGrant,
) (*contracts.ProjectBrainRef, error) {
	if grant == nil {
		return nil, nil
	}
	if assembler.brain == nil {
		return nil, fmt.Errorf("actorstate: Project Brain authority is unavailable")
	}
	if grant.OrganizationID != authority.OrganizationID ||
		grant.RequesterSeatID != seat.ID ||
		grant.Operation != projectbrain.CapabilityRead ||
		!grant.ExpiresAt.After(assembler.now()) ||
		grant.ExpiresAt.After(authority.ExpiresAt) {
		return nil, fmt.Errorf("actorstate: Project Brain grant is outside wake authority")
	}
	view, err := assembler.brain.View(ctx, *grant)
	if err != nil {
		return nil, err
	}
	if view.OrganizationID != authority.OrganizationID ||
		view.ProjectID != grant.ProjectID || view.WorkspaceID != grant.WorkspaceID ||
		view.ExpiresAt != grant.ExpiresAt {
		return nil, fmt.Errorf("actorstate: Project Brain view does not match signed grant")
	}
	return &contracts.ProjectBrainRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     view.ProjectID,
		WorkspaceID:   view.WorkspaceID,
		Source: contracts.SourceState{
			RootDigest:      view.Source.RootDigest,
			GraphGeneration: view.Source.Generation,
			LedgerCursor:    uint64(len(view.Records) + len(view.StaleRecordIDs) + 1),
		},
		ViewDigest:   view.Digest,
		Fresh:        view.Source.Fresh,
		PendingFiles: append([]string(nil), view.Source.PendingFiles...),
		ExpiresAt:    view.ExpiresAt,
	}, nil
}

func (assembler *Assembler) openInbox(
	ctx context.Context,
	authority contracts.WakeLease,
	limit uint32,
) ([]contracts.MessageEnvelope, []contracts.ArtifactRef, []contracts.EvidenceRef, error) {
	if limit == 0 {
		limit = 100
	}
	deliveries, err := assembler.mail.Inbox(ctx, authority.OrganizationID, authority.SeatID, limit)
	if err != nil {
		return nil, nil, nil, err
	}
	keyPrefix := "wake:" + string(authority.WakeID) + ":"
	inbox := make([]contracts.MessageEnvelope, 0, len(deliveries))
	artifacts := make([]contracts.ArtifactRef, 0)
	evidence := make([]contracts.EvidenceRef, 0)
	for _, delivery := range deliveries {
		if !delivery.BindingReady ||
			(delivery.State != mail.StateDelivered &&
				!(delivery.State == mail.StateOpened && delivery.ConsumptionKey == keyPrefix+string(delivery.MessageID))) {
			continue
		}
		envelope, _, err := assembler.mail.Consume(ctx, mail.ConsumeRequest{
			OrganizationID: authority.OrganizationID,
			SeatID:         authority.SeatID,
			MessageID:      delivery.MessageID,
			IdempotencyKey: keyPrefix + string(delivery.MessageID),
		})
		if err != nil {
			return nil, nil, nil, err
		}
		inbox = append(inbox, envelope)
		artifacts = append(artifacts, envelope.Artifacts...)
		if envelope.Payload.Artifact.ID != "" {
			artifacts = append(artifacts, envelope.Payload.Artifact)
		}
		evidence = append(evidence, envelope.Evidence...)
	}
	return inbox, artifacts, uniqueEvidence(evidence), nil
}

func uniqueArtifacts(values []contracts.ArtifactRef) []contracts.ArtifactRef {
	seen := make(map[contracts.ArtifactID]bool, len(values))
	result := make([]contracts.ArtifactRef, 0, len(values))
	for _, value := range values {
		if !seen[value.ID] {
			seen[value.ID] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func uniqueEvidence(values []contracts.EvidenceRef) []contracts.EvidenceRef {
	seen := make(map[contracts.EvidenceID]bool, len(values))
	result := make([]contracts.EvidenceRef, 0, len(values))
	for _, value := range values {
		if !seen[value.ID] {
			seen[value.ID] = true
			result = append(result, value)
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
