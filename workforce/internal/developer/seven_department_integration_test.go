package developer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/vault"

	"matrix/workforce/internal/approval"
	"matrix/workforce/internal/circuit"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/domainwork"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/knowledgework"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/lineage"
	"matrix/workforce/internal/mail"
	"matrix/workforce/internal/operationswork"
	"matrix/workforce/internal/policy"
	"matrix/workforce/internal/skills"
	"matrix/workforce/internal/workcompile"
)

func TestIntegration_SevenDepartmentWorkOrderUsesRealBoundaries(t *testing.T) {
	ctx := context.Background()
	tenant := "tenant:seven-department"
	organizationID := contracts.OrganizationID("organization:seven-department")
	ownerID := contracts.OwnerID("owner:seven-department")
	now := developerNow()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	rememberDeveloperVault(t, tenant, session.UserVault())
	ownerPublic, ownerPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	approvalStore, err := approval.New(
		developerPool, session.UserVault(), tenant, organizationID,
		ownerID, "key:seven-owner", ownerPublic, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	knowledgeService, err := knowledgework.New(developerNow)
	if err != nil {
		t.Fatal(err)
	}
	domainService, err := domainwork.New(developerNow, approvalStore)
	if err != nil {
		t.Fatal(err)
	}
	operationsService, err := operationswork.New(developerNow, approvalStore)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := dependency.New(developerPool, tenant, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := mail.New(
		developerPool, session.UserVault(), graph, tenant, knowledgeMailConfig(),
		developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	pack := sevenDepartmentPack(t)
	runner := buildKnowledgeSeatRunner(t)
	evidence := sevenDepartmentEvidence(now)

	steps := []struct {
		department contracts.DepartmentKind
		role       contracts.SeatRole
		skill      contracts.SkillID
	}{
		{contracts.DepartmentDeveloper, contracts.SeatLead, skills.DeveloperPlanSkill},
		{contracts.DepartmentExecutive, contracts.SeatLead, skills.PortfolioPlanningSkill},
		{contracts.DepartmentResearch, contracts.SeatExecutor, skills.ExperimentDesignSkill},
		{contracts.DepartmentMarketing, contracts.SeatExecutor, skills.PublicationGatesSkill},
		{contracts.DepartmentLegal, contracts.SeatLead, skills.IssueSpottingSkill},
		{contracts.DepartmentAccounting, contracts.SeatExecutor, skills.ReconciliationSkill},
		{contracts.DepartmentBackOffice, contracts.SeatLead, skills.AdministrativeWorkflowSkill},
	}
	seats := make([]knowledgeMailSeat, len(steps))
	nodes := make([]dependency.NodeID, len(steps))
	for index, step := range steps {
		label := strings.ReplaceAll(string(step.department), "_and_", "-")
		seats[index] = newKnowledgeMailSeat(
			t, organizationID,
			contracts.DepartmentID("department:"+label),
			contracts.SeatID("seat:"+label+":"+string(step.role)),
		)
		insertMultiwakeSeatAuthority(t, tenant, seats[index].address, now)
		if err := mailbox.PublishSeatKey(ctx, mail.SeatKey{
			Address: seats[index].address, KeyID: seats[index].keyID,
			PublicKey: seats[index].public, EffectiveAt: now,
		}); err != nil {
			t.Fatal(err)
		}
		nodes[index] = dependency.NodeID("intent:launch:" + label)
		seatID := seats[index].address.SeatID
		departmentID := seats[index].address.DepartmentID
		state := dependency.StatePending
		if index == 0 {
			state = dependency.StateEligible
		}
		if err := graph.PutNode(ctx, dependency.Node{
			ID: nodes[index], OrganizationID: organizationID,
			Kind: dependency.NodeIntent, OwnerSeatID: &seatID,
			OwnerDepartmentID: &departmentID,
			Title:             "Launch work for " + string(step.department),
			State:             state, BasePriority: 10,
			CreatedAt: now, UpdatedAt: now, Version: 1,
		}); err != nil {
			t.Fatal(err)
		}
		if index > 0 {
			expires := now.Add(time.Hour)
			sla := now.Add(30 * time.Minute)
			if err := graph.AddEdge(ctx, dependency.Edge{
				OrganizationID: organizationID,
				Prerequisite:   nodes[index-1], Dependent: nodes[index],
				Kind:                   dependency.EdgeDelegation,
				RequiredResponseSchema: "workforce.mail.handoff.v1",
				ExpiresAt:              &expires,
				TimeoutAction:          contracts.TimeoutEscalate,
				SLAAt:                  &sla,
				CreatedAt:              now,
			}); err != nil {
				t.Fatal(err)
			}
		}
	}

	var priorMessage *contracts.MessageEnvelope
	var priorArtifact contracts.ArtifactRef
	var executiveArtifact contracts.ArtifactRef
	var finalInput operationswork.Input
	var finalPacket contracts.WorkPacket
	var publicationApproval contracts.ApprovalID
	wakeIDs := make(map[contracts.WakeID]bool)
	for index, step := range steps {
		var inbox []contracts.MessageEnvelope
		var artifacts []contracts.ArtifactRef
		if priorMessage != nil {
			opened, duplicate, err := mailbox.Consume(ctx, mail.ConsumeRequest{
				OrganizationID: organizationID,
				SeatID:         seats[index].address.SeatID,
				MessageID:      priorMessage.ID,
				IdempotencyKey: "consume:" + string(priorMessage.ID),
			})
			if err != nil || duplicate {
				t.Fatalf("consume step %d duplicate=%v err=%v", index, duplicate, err)
			}
			inbox = []contracts.MessageEnvelope{opened}
			artifacts = []contracts.ArtifactRef{priorArtifact}
		}
		packet := knowledgeLoopPacket(
			t, organizationID, seats[index].address, step.department, step.role,
			contracts.WakeID("wake:launch:"+string(step.department)),
			contracts.LeaseID("lease:launch:"+string(step.department)),
			contracts.IntentID(nodes[index]), knowledgeSkillRef(t, pack, step.skill),
			inbox, artifacts, []contracts.EvidenceRef{evidence}, now,
		)
		output, err := runKnowledgeSeat(t, ctx, runner, packet)
		if err != nil {
			t.Fatal(err)
		}
		if wakeIDs[output.WakeID] {
			t.Fatal("department reused a prior wake identity")
		}
		wakeIDs[output.WakeID] = true
		artifact := sevenDepartmentArtifact(
			"developer-plan", output.PacketDigest, 1,
		)
		switch step.department {
		case contracts.DepartmentExecutive:
			result, err := knowledgeService.Execute(ctx, knowledgework.Input{
				SchemaVersion: contracts.SchemaVersionV1, OrganizationID: organizationID,
				Department: step.department, SeatID: packet.Seat.ID,
				IntentID: packet.Intent.ID, SkillID: step.skill,
				Objective:    "Prioritize a bounded launch",
				Constraints:  []string{"No approval or effect authority"},
				Evidence:     []contracts.EvidenceRef{evidence},
				SourceDigest: priorArtifact.Hash,
				Draft: knowledgework.Draft{
					Summary: "Prioritize evidence review before launch",
					Findings: []knowledgework.Finding{{
						Statement:   "Current launch evidence is bounded",
						EvidenceIDs: []contracts.EvidenceID{evidence.ID},
					}},
					Recommendations: []knowledgework.Recommendation{{
						Action:      "Request a bounded experiment",
						Rationale:   "Launch requires current evidence",
						EvidenceIDs: []contracts.EvidenceID{evidence.ID},
					}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			artifact = result.Artifact
			executiveArtifact = result.Artifact
		case contracts.DepartmentResearch:
			result, err := knowledgeService.Execute(ctx, knowledgework.Input{
				SchemaVersion: contracts.SchemaVersionV1, OrganizationID: organizationID,
				Department: step.department, SeatID: packet.Seat.ID,
				IntentID: packet.Intent.ID, SkillID: step.skill,
				Objective:    "Design an offline launch experiment",
				Constraints:  []string{"Human run only"},
				Evidence:     []contracts.EvidenceRef{evidence},
				SourceDigest: priorArtifact.Hash,
				Draft: knowledgework.Draft{
					Summary: "Bounded offline launch experiment",
					Experiment: &knowledgework.ExperimentDesign{
						Hypothesis:      "The launch message is understood",
						Method:          "Offline consented panel review",
						SuccessMetrics:  []string{"comprehension threshold"},
						StopConditions:  []string{"evidence quality degrades"},
						MaximumDuration: "P1D", RequiresHumanRun: true,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			correctionOf := executiveArtifact.Hash
			corrected, err := knowledgeService.Execute(ctx, knowledgework.Input{
				SchemaVersion: contracts.SchemaVersionV1, OrganizationID: organizationID,
				Department:   contracts.DepartmentExecutive,
				SeatID:       seats[1].address.SeatID,
				IntentID:     contracts.IntentID(nodes[1]),
				SkillID:      skills.PortfolioPlanningSkill,
				Objective:    "Correct launch priority using the bounded experiment design",
				Constraints:  []string{"No approval or effect authority"},
				Evidence:     []contracts.EvidenceRef{evidence},
				SourceDigest: result.Artifact.Hash, CorrectionOf: &correctionOf,
				Draft: knowledgework.Draft{
					Summary: "Prioritize the offline experiment before publication",
					Findings: []knowledgework.Finding{{
						Statement:   "The launch needs a bounded comprehension check",
						EvidenceIDs: []contracts.EvidenceID{evidence.ID},
					}},
					Recommendations: []knowledgework.Recommendation{{
						Action:      "Run the consented offline panel before publication",
						Rationale:   "The experiment corrects the original launch sequence",
						EvidenceIDs: []contracts.EvidenceID{evidence.ID},
					}},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			artifact = corrected.Artifact
		case contracts.DepartmentMarketing:
			publicationApproval = "approval:seven-publication"
			batch := approval.BatchApproval{
				SchemaVersion: contracts.SchemaVersionV1,
				BatchID:       publicationApproval, TenantID: tenant,
				OrganizationID:             organizationID,
				IntentIDs:                  []contracts.IntentID{packet.Intent.ID},
				AggregateCeilingMicrounits: 1,
				ExpiresAt:                  now.Add(30 * time.Minute), OwnerID: ownerID,
			}
			if err := approval.SignBatch(&batch, "key:seven-owner", ownerPrivate); err != nil {
				t.Fatal(err)
			}
			if err := approvalStore.PublishBatch(ctx, batch); err != nil {
				t.Fatal(err)
			}
			result, err := domainService.Execute(ctx, domainwork.Input{
				SchemaVersion: contracts.SchemaVersionV1, OrganizationID: organizationID,
				Department: step.department, SeatID: packet.Seat.ID,
				IntentID: packet.Intent.ID, SkillID: step.skill,
				Objective: "Gate factual owned-channel launch content",
				Evidence: []domainwork.ExpiringEvidence{{
					Reference: evidence, ExpiresAt: now.Add(time.Hour),
				}},
				SourceDigest: priorArtifact.Hash,
				Draft: domainwork.Draft{
					Summary: "Owner-approved factual launch content",
					Campaign: &domainwork.CampaignDraft{
						Audience:           "Existing consenting customers",
						Channels:           []string{"owned-web"},
						Content:            "Current factual launch update",
						PerformanceMetrics: []string{"qualified engagement"},
					},
					Publication: &domainwork.PublicationAuthorization{
						BatchID: publicationApproval, CostMicrounits: 1,
						IdempotencyKey: "consume:seven-publication",
					},
				},
			})
			if err != nil || result.Outcome != "approved_for_publication" {
				t.Fatalf("publication result=%#v err=%v", result, err)
			}
			artifact = result.Artifact
			runSevenDepartmentEffect(
				t, ctx, session.UserVault(), tenant, organizationID,
				packet, artifact,
			)
		case contracts.DepartmentLegal:
			result, err := domainService.Execute(ctx, domainwork.Input{
				SchemaVersion: contracts.SchemaVersionV1, OrganizationID: organizationID,
				Department: step.department, SeatID: packet.Seat.ID,
				IntentID: packet.Intent.ID, SkillID: step.skill,
				Objective: "Spot launch disclosure issues",
				Evidence: []domainwork.ExpiringEvidence{{
					Reference: evidence, ExpiresAt: now.Add(time.Hour),
				}},
				SourceDigest: priorArtifact.Hash,
				Draft: domainwork.Draft{
					Summary: "Qualified review required for final wording",
					Legal: &domainwork.LegalDraft{
						Jurisdictions: []string{"DE"},
						Issues:        []string{"consumer disclosure"},
						Analysis:      "Final wording may require jurisdiction review",
						Disclaimer:    "This is not final legal advice.",
						HumanSignoff:  true,
					},
				},
			})
			if err != nil || !result.RequiresHuman {
				t.Fatalf("legal result=%#v err=%v", result, err)
			}
			artifact = result.Artifact
		case contracts.DepartmentAccounting:
			result, err := operationsService.Execute(ctx, operationswork.Input{
				SchemaVersion: contracts.SchemaVersionV1, OrganizationID: organizationID,
				Department: step.department, SeatID: packet.Seat.ID,
				IntentID: packet.Intent.ID, SkillID: step.skill,
				Objective: "Reconcile the launch budget observation",
				Evidence: []operationswork.ExpiringEvidence{{
					Reference: evidence, ExpiresAt: now.Add(time.Hour),
				}},
				SourceDigest: priorArtifact.Hash,
				Draft: operationswork.Draft{
					Summary: "Launch budget observation reconciled",
					Accounting: &operationswork.AccountingDraft{
						Reconciliation: &operationswork.ReconciliationObservation{
							ExternalObservationID: "budget-observation:launch",
							ExpectedMinor:         0, ObservedMinor: 0,
							Currency: "USD", Disposition: "matched",
						},
					},
					CompletionChecks: []string{"external observation retained"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			artifact = result.Artifact
		case contracts.DepartmentBackOffice:
			sla := now.Add(time.Hour)
			finalInput = operationswork.Input{
				SchemaVersion: contracts.SchemaVersionV1, OrganizationID: organizationID,
				Department: step.department, SeatID: packet.Seat.ID,
				IntentID: packet.Intent.ID, SkillID: step.skill,
				Objective: "Close the launch coordination handoff",
				Evidence: []operationswork.ExpiringEvidence{{
					Reference: evidence, ExpiresAt: now.Add(time.Hour),
				}},
				SourceDigest: priorArtifact.Hash,
				Draft: operationswork.Draft{
					Summary: "Launch records and SLA are ready for human closure",
					BackOffice: &operationswork.BackOfficeDraft{
						Records: []string{"seven-department launch evidence"},
						Process: "human launch closure", SLAAt: &sla,
					},
					CompletionChecks: []string{"all department artifacts are present"},
				},
			}
			result, err := operationsService.Execute(ctx, finalInput)
			if err != nil {
				t.Fatal(err)
			}
			artifact = result.Artifact
			finalPacket = packet
		}
		expectedVersion := uint64(1)
		if index > 0 {
			expectedVersion = 2
		}
		if err := graph.Transition(
			ctx, organizationID, nodes[index], expectedVersion,
			dependency.StateCompleted, "",
		); err != nil {
			t.Fatal(err)
		}
		priorArtifact = artifact
		if index+1 < len(steps) {
			message := knowledgeEnvelope(
				t, seats[index], seats[index+1].address, contracts.MessageHandoff,
				contracts.MessageID("message:launch:"+string(step.department)),
				contracts.ThreadID("thread:launch:"+string(step.department)),
				nil, packet.Intent.ID,
				artifact, nil, []contracts.EvidenceRef{evidence},
				"Continue the bounded launch Work Order", now,
			)
			if _, err := mailbox.Send(ctx, message, mail.SendOptions{}); err != nil {
				t.Fatal(err)
			}
			if count, err := mailbox.Dispatch(ctx, organizationID, 10); err != nil || count != 1 {
				t.Fatalf("dispatch step %d = %d, %v", index, count, err)
			}
			priorMessage = &message
			if _, err := graph.Resolve(ctx, organizationID); err != nil {
				t.Fatal(err)
			}
		}
	}
	if len(wakeIDs) != 7 {
		t.Fatalf("fresh wake count = %d", len(wakeIDs))
	}
	snapshot, err := graph.Snapshot(ctx, organizationID)
	if err != nil {
		t.Fatal(err)
	}
	for _, node := range snapshot.Nodes {
		if node.State != dependency.StateCompleted {
			t.Fatalf("node %s state = %s", node.ID, node.State)
		}
	}
	publishSevenDepartmentReceipt(
		t, ctx, session.UserVault(), tenant, organizationID,
		ownerPrivate, pack, finalPacket, finalInput, priorArtifact,
		evidence, publicationApproval,
	)
}

func sevenDepartmentPack(t *testing.T) []skills.Contract {
	t.Helper()
	var result []skills.Contract
	for _, build := range []func() ([]skills.Contract, error){
		skills.DeveloperPack, skills.ExecutiveResearchPack,
		skills.MarketingLegalPack, skills.OperationsPack,
	} {
		pack, err := build()
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, pack...)
	}
	return result
}

func sevenDepartmentArtifact(
	label string,
	hash contracts.ContentHash,
	size uint64,
) contracts.ArtifactRef {
	return contracts.ArtifactRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.ArtifactID("artifact:" + label),
		Hash:          hash,
		MediaType:     "application/vnd.matrix.work-order+json",
		SizeBytes:     size,
	}
}

func sevenDepartmentEvidence(now time.Time) contracts.EvidenceRef {
	return contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "evidence:seven-department",
		Hash:          developerHash("seven-department-evidence"),
		Kind:          "work_order_observation",
		ObservedAt:    now,
	}
}

func runSevenDepartmentEffect(
	t *testing.T,
	ctx context.Context,
	userVault *vault.UserVault,
	tenant string,
	organizationID contracts.OrganizationID,
	packet contracts.WorkPacket,
	artifact contracts.ArtifactRef,
) {
	t.Helper()
	leaseStore, err := lease.New(developerPool, tenant, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	request := lease.Request{
		ID: packet.Lease.ID, WakeID: packet.Lease.WakeID,
		OrganizationID: organizationID, SeatID: "seat:publication-effect",
		NodeID:    dependency.NodeID(packet.Intent.ID),
		MandateID: "mandate:publication-effect", MandateVersion: 1,
		Policies: []contracts.PolicyRef{{
			ID: "policy:publication-effect", Version: 1,
			Hash: developerHash("publication-effect-policy"),
		}},
		IssuedAt: developerNow(), ExpiresAt: developerNow().Add(time.Hour),
	}
	request = insertDeveloperAuthority(t, tenant, request)
	grant, err := leaseStore.Acquire(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	registerDeveloperLease(t, tenant, grant)
	breakers, err := circuit.New(developerPool, tenant, circuit.Config{
		FailureThreshold: 3, SuccessThreshold: 1, Window: time.Hour,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	adapter, err := effect.NewCommandAdapter(
		"owned-channel", "/bin/sh",
		map[string][]string{
			"publish_launch": {"-c", `cat > "$1/$WORKFORCE_IDEMPOTENCY_KEY"; printf published`, "sh", directory},
		},
		map[string][]string{
			"publish_launch": {"-c", `printf '{"outcome":"unchanged","reason":"already observed"}'`},
		},
		[]string{"PATH=/usr/bin:/bin"}, directory, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := effect.New(
		developerPool, userVault, leaseStore,
		developerPolicyAuthority(t, tenant, organizationID).Store(), breakers,
		tenant, approval.Authority{}, developerNow, adapter,
	)
	if err != nil {
		t.Fatal(err)
	}
	input, _ := json.Marshal(artifact)
	effectProposal := effect.Proposal{
		ID: "proposal:launch-publication", OrganizationID: organizationID,
		IntentID: packet.Intent.ID, NodeID: dependency.NodeID(packet.Intent.ID),
		SeatID: request.SeatID, LeaseID: request.ID, Fence: grant.Fence,
		Provider: adapter.Name(), SkillID: skills.PublicationGatesSkill,
		EffectClass: skills.EffectReversible, Operation: "publish_launch",
		IdempotencyKey:  "seven-publication",
		SkillDigest:     packet.Skills[0].Digest,
		OperationDigest: developerHash("seven-publication-operation"),
		Input:           input, Deadline: developerNow().Add(30 * time.Minute),
	}
	if err := authorizeDeveloperProposal(ctx, tenant, userVault, effectProposal); err != nil {
		t.Fatal(err)
	}
	result, err := gateway.Execute(ctx, effectProposal)
	if err != nil || result.State != effect.StateSucceeded {
		t.Fatalf("real launch effect = %#v, %v", result, err)
	}
	if _, err := os.Stat(filepath.Join(directory, "seven-publication")); err != nil {
		t.Fatalf("provider effect artifact: %v", err)
	}
}

func publishSevenDepartmentReceipt(
	t *testing.T,
	ctx context.Context,
	userVault *vault.UserVault,
	tenant string,
	organizationID contracts.OrganizationID,
	privateKey ed25519.PrivateKey,
	pack []skills.Contract,
	packet contracts.WorkPacket,
	input operationswork.Input,
	artifact contracts.ArtifactRef,
	evidence contracts.EvidenceRef,
	approvalID contracts.ApprovalID,
) {
	t.Helper()
	publicKey := privateKey.Public().(ed25519.PublicKey)
	skillStore, err := skills.NewStore(
		developerPool, userVault, tenant, organizationID,
		"key:seven-owner", publicKey, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	var contract skills.Contract
	for _, candidate := range pack {
		if candidate.ID == input.SkillID {
			contract = candidate
			break
		}
	}
	signed := skills.SignedContract{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID, Contract: contract,
		EffectiveAt: developerNow(),
	}
	if err := skills.SignContract(&signed, "key:seven-owner", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := skillStore.Publish(ctx, signed); err != nil {
		t.Fatal(err)
	}
	// The compiler admits only genuinely signed authority bound to a live
	// runtime lease, exactly as the kernel presents a packet in production.
	if err := policy.SignMandate(&packet.Mandate, "key:seven-owner", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := policy.SignSeat(&packet.Seat, "key:seven-owner", privateKey); err != nil {
		t.Fatal(err)
	}
	compilerAuthority := publishSevenPacketAuthority(
		t, ctx, tenant, userVault, &packet, publicKey, privateKey,
	)
	leaseStore, err := lease.New(developerPool, tenant, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	runtimeGrant, err := leaseStore.Acquire(ctx, lease.Request{
		ID: packet.Lease.ID, WakeID: packet.Lease.WakeID,
		OrganizationID: organizationID, SeatID: packet.Seat.ID,
		NodeID:    dependency.NodeID(packet.Intent.ID),
		MandateID: packet.Mandate.ID, MandateVersion: packet.Mandate.Version,
		Policies: append([]contracts.PolicyRef(nil), packet.Policies...),
		IssuedAt: packet.Lease.IssuedAt, ExpiresAt: packet.Lease.ExpiresAt,
	})
	if err != nil {
		t.Fatal(err)
	}
	packet.Lease.Fence = runtimeGrant.Fence
	if err := policy.SignWakeLease(&packet.Lease, "key:seven-owner", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := compilerAuthority.RegisterLease(ctx, packet.Lease); err != nil {
		t.Fatal(err)
	}
	compiler, err := workcompile.New(
		developerPool, userVault, tenant, skillStore,
		compilerAuthority, leaseStore, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	encodedInput, err := json.Marshal(struct {
		operationswork.Input
		Grant lease.Grant `json:"grant"`
	}{Input: input, Grant: runtimeGrant})
	if err != nil {
		t.Fatal(err)
	}
	source := contracts.SourceState{
		RootDigest:      developerHash("seven-department-source"),
		GraphGeneration: 1, LedgerCursor: 1,
	}
	plan, err := compiler.Compile(ctx, packet, workcompile.Proposal{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "compile:seven-final", OrganizationID: organizationID,
		WakeID: packet.Lease.WakeID, IntentID: packet.Intent.ID,
		SeatID: packet.Seat.ID, Skill: packet.Skills[0],
		Operation: string(input.SkillID), Provider: "operations",
		IdempotencyKey: "compile:seven-final", Input: encodedInput,
		Deadline: developerNow().Add(30 * time.Minute),
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	lineageStore, err := lineage.New(
		developerPool, userVault, tenant, "lineage:seven", privateKey, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	modelEvidence, err := lineageStore.PutModelEvidence(ctx, lineage.ModelExchange{
		ID: "evidence:model:seven", OrganizationID: organizationID,
		WakeID: packet.Lease.WakeID, Model: packet.Lease.Model,
		MGS: packet.Lease.MGS, Runtime: packet.Lease.Runtime,
		Request: encodedInput, Response: []byte(`{"outcome":"proposed"}`),
		Output:         []byte(`{"outcome":"proposed"}`),
		ReplayRequired: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	receipt, err := lineageStore.BuildReceipt(lineage.ReceiptInput{
		ID: "receipt:seven-department", Packet: packet, Plan: plan,
		ModelEvidence: modelEvidence,
		Constraints:   []string{"No department exceeded its signed mandate"},
		Approvals:     []contracts.ApprovalID{approvalID},
		Artifacts:     []contracts.ArtifactRef{artifact},
		Evidence:      []contracts.EvidenceRef{evidence},
		CostMinor:     0, LatencyMillis: 1,
		Disposition:      contracts.DispositionGoalCompleted,
		OperationOutcome: "verified",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lineageStore.PublishReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	opened, err := lineageStore.OpenReceipt(ctx, organizationID, receipt.ID)
	if err != nil || opened.ContentHash != receipt.ContentHash ||
		opened.Disposition != contracts.DispositionGoalCompleted {
		t.Fatalf("sealed final receipt = %#v, %v", opened, err)
	}
}

func publishSevenPacketAuthority(
	t *testing.T,
	ctx context.Context,
	tenant string,
	userVault *vault.UserVault,
	packet *contracts.WorkPacket,
	publicKey ed25519.PublicKey,
	privateKey ed25519.PrivateKey,
) *policy.Store {
	t.Helper()
	store, err := policy.New(developerPool, userVault, policy.OwnerRoot{
		TenantID: tenant, OrganizationID: packet.Lease.OrganizationID,
		OwnerID: "owner:seven", KeyID: "key:seven-owner", PublicKey: publicKey,
	}, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	grant := policy.OwnerGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		TenantID:      tenant, OrganizationID: packet.Lease.OrganizationID,
		OwnerID: "owner:seven", KeyID: "key:seven-owner",
		Scope:     "authority:write",
		IssuedAt:  developerNow().Add(-time.Minute),
		ExpiresAt: developerNow().Add(time.Hour),
	}
	if err := policy.SignOwnerGrant(&grant, "key:seven-owner", privateKey); err != nil {
		t.Fatal(err)
	}
	runtimeAuthority := policy.RuntimeAuthority{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             policy.RuntimeAuthorityID("key:seven-owner"),
		Version:        1,
		OrganizationID: packet.Lease.OrganizationID,
		KeyID:          "key:seven-owner",
		PublicKey:      base64.RawURLEncoding.EncodeToString(publicKey),
		Purposes:       []string{policy.WakeLeaseSigningPurpose},
		EffectiveAt:    developerNow().Add(-time.Hour),
	}
	if err := policy.SignRuntimeAuthority(
		&runtimeAuthority, "key:seven-owner", privateKey,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishRuntimeAuthority(
		ctx, runtimeAuthority, grant,
	); err != nil {
		t.Fatal(err)
	}
	for index := range packet.Policies {
		value := contracts.Policy{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            packet.Policies[index].ID, Version: packet.Policies[index].Version,
			OrganizationID: packet.Lease.OrganizationID, Kind: "compiled_dispatch",
			EffectiveAt: developerNow().Add(-time.Hour),
			Rules: []contracts.PolicyRule{{
				ClauseID: "compiled-only", Outcome: "allow",
				Scope: "Only the exact compiled operation may dispatch",
			}},
		}
		if err := policy.SignPolicy(&value, "key:seven-owner", privateKey); err != nil {
			t.Fatal(err)
		}
		canonical, err := contracts.EncodeCanonical(&value)
		if err != nil {
			t.Fatal(err)
		}
		packet.Policies[index].Hash = developerHash(string(canonical))
		if err := store.PublishPolicy(ctx, value, grant); err != nil {
			t.Fatal(err)
		}
	}
	packet.Lease.Policies = append([]contracts.PolicyRef(nil), packet.Policies...)
	if err := store.PublishMandate(ctx, packet.Mandate, grant); err != nil {
		t.Fatal(err)
	}
	packet.Seat.Version = 2
	packet.Seat.EffectiveAt = developerNow()
	if err := policy.SignSeat(&packet.Seat, "key:seven-owner", privateKey); err != nil {
		t.Fatal(err)
	}
	if err := store.PublishSeat(ctx, packet.Seat, grant); err != nil {
		t.Fatal(err)
	}
	return store
}
