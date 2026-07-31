package developer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/vault"

	"matrix/workforce/internal/actorstate"
	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/dependency"
	"matrix/workforce/internal/knowledgework"
	"matrix/workforce/internal/mail"
	"matrix/workforce/internal/skills"
)

func TestIntegration_ExecutiveResearchLoopUsesFreshWakesTypedMailAndCorrection(t *testing.T) {
	ctx := context.Background()
	tenant := "tenant:knowledge-loop"
	organizationID := contracts.OrganizationID("organization:knowledge-loop")
	now := developerNow()
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
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
	executive := newKnowledgeMailSeat(
		t, organizationID, "department:executive", "seat:executive-lead",
	)
	research := newKnowledgeMailSeat(
		t, organizationID, "department:research", "seat:research-executor",
	)
	for _, seat := range []knowledgeMailSeat{executive, research} {
		insertMultiwakeSeatAuthority(t, tenant, seat.address, now)
		if err := mailbox.PublishSeatKey(ctx, mail.SeatKey{
			Address: seat.address, KeyID: seat.keyID,
			PublicKey: seat.public, EffectiveAt: now,
		}); err != nil {
			t.Fatal(err)
		}
	}

	pack, err := skills.ExecutiveResearchPack()
	if err != nil {
		t.Fatal(err)
	}
	service, err := knowledgework.New(developerNow)
	if err != nil {
		t.Fatal(err)
	}
	runner := buildKnowledgeSeatRunner(t)
	evidence := knowledgeLoopEvidence(now)

	executivePlanPacket := knowledgeLoopPacket(
		t, organizationID, executive.address, contracts.DepartmentExecutive,
		contracts.SeatLead, "wake:executive-plan", "lease:executive-plan",
		"intent:executive-plan", knowledgeSkillRef(t, pack, skills.PortfolioPlanningSkill),
		nil, nil, []contracts.EvidenceRef{evidence}, now,
	)
	executiveWake, err := runKnowledgeSeat(t, ctx, runner, executivePlanPacket)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := service.Execute(ctx, knowledgework.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentExecutive,
		SeatID:         executive.address.SeatID,
		IntentID:       executivePlanPacket.Intent.ID,
		SkillID:        skills.PortfolioPlanningSkill,
		Objective:      "Prioritize a bounded onboarding experiment",
		Constraints: []string{
			"Recommendations grant no approval, spending, publication, or effect authority",
		},
		Evidence:     []contracts.EvidenceRef{evidence},
		SourceDigest: executiveWake.PacketDigest,
		Draft: knowledgework.Draft{
			Summary: "Recommend evidence review before any human-authorized experiment",
			Findings: []knowledgework.Finding{{
				Statement:   "Current activation evidence has a measured baseline",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			Recommendations: []knowledgework.Recommendation{{
				Action:      "Ask Research and Development for a bounded experiment design",
				Rationale:   "The portfolio decision requires independent current evidence",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	handoffDraft, err := service.Execute(ctx, knowledgework.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentExecutive,
		SeatID:         executive.address.SeatID,
		IntentID:       "intent:executive-handoff",
		SkillID:        skills.TypedHandoffSkill,
		Objective:      "Request an evidence-bound experiment design",
		Constraints:    []string{"The handoff delegates work but grants no authority"},
		Evidence:       []contracts.EvidenceRef{evidence},
		SourceDigest:   plan.Artifact.Hash,
		Draft: knowledgework.Draft{
			Summary: "Typed research request linked to the current portfolio proposal",
			Findings: []knowledgework.Finding{{
				Statement:   "The proposed priority depends on bounded research",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			Handoff: &knowledgework.HandoffDraft{
				RecipientDepartment: contracts.DepartmentResearch,
				RecipientSeatID:     research.address.SeatID,
				Subject:             "Design bounded onboarding experiment",
				RequiredAction:      "Return a typed design and evidence review",
				TimeoutAction:       contracts.TimeoutEscalate,
				ExpiresAt:           now.Add(30 * time.Minute),
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	handoff := knowledgeEnvelope(
		t, executive, research.address, contracts.MessageHandoff,
		"message:executive-research", "thread:executive-research", nil,
		executivePlanPacket.Intent.ID, handoffDraft.Artifact,
		[]contracts.ArtifactRef{plan.Artifact}, []contracts.EvidenceRef{evidence},
		"Return a typed design and evidence review", now,
	)
	if _, err := mailbox.Send(ctx, handoff, mail.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if delivered, err := mailbox.Dispatch(
		ctx, organizationID, 10,
	); err != nil || delivered != 1 {
		t.Fatalf("dispatch Executive handoff = %d, %v", delivered, err)
	}
	openedHandoff, duplicate, err := mailbox.Consume(ctx, mail.ConsumeRequest{
		OrganizationID: organizationID,
		SeatID:         research.address.SeatID,
		MessageID:      handoff.ID,
		IdempotencyKey: "consume:executive-research",
	})
	if err != nil || duplicate {
		t.Fatalf("consume Executive handoff duplicate=%v err=%v", duplicate, err)
	}

	researchPacket := knowledgeLoopPacket(
		t, organizationID, research.address, contracts.DepartmentResearch,
		contracts.SeatExecutor, "wake:research-design", "lease:research-design",
		"intent:research-design", knowledgeSkillRef(t, pack, skills.ExperimentDesignSkill),
		[]contracts.MessageEnvelope{openedHandoff}, []contracts.ArtifactRef{plan.Artifact},
		[]contracts.EvidenceRef{evidence}, now,
	)
	researchWake, err := runKnowledgeSeat(t, ctx, runner, researchPacket)
	if err != nil {
		t.Fatal(err)
	}
	experiment, err := service.Execute(ctx, knowledgework.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentResearch,
		SeatID:         research.address.SeatID,
		IntentID:       researchPacket.Intent.ID,
		SkillID:        skills.ExperimentDesignSkill,
		Objective:      "Design the requested bounded onboarding experiment",
		Constraints:    []string{"No production run without human authorization"},
		Evidence:       []contracts.EvidenceRef{evidence},
		SourceDigest:   handoffDraft.Artifact.Hash,
		Draft: knowledgework.Draft{
			Summary: "Offline replay design with explicit success and stop conditions",
			Findings: []knowledgework.Finding{{
				Statement:   "The baseline supports an offline comparison",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			Experiment: &knowledgework.ExperimentDesign{
				Hypothesis:       "The onboarding change improves activation",
				Method:           "Replay consented historical events in an isolated dataset",
				SuccessMetrics:   []string{"activation delta above the declared threshold"},
				StopConditions:   []string{"source quality falls below the declared threshold"},
				MaximumDuration:  "P7D",
				RequiresHumanRun: true,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	review, err := service.Execute(ctx, knowledgework.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentResearch,
		SeatID:         research.address.SeatID,
		IntentID:       "intent:research-review",
		SkillID:        skills.EvidenceReviewSkill,
		Objective:      "Review evidence supporting the experiment proposal",
		Constraints:    []string{"Keep uncertainty and human-run requirements explicit"},
		Evidence:       []contracts.EvidenceRef{evidence},
		SourceDigest:   experiment.Artifact.Hash,
		Draft: knowledgework.Draft{
			Summary: "Evidence supports an offline test, not autonomous execution",
			Findings: []knowledgework.Finding{{
				Statement:   "The baseline is sufficient only for a bounded offline comparison",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			UnresolvedRisks: []string{"A human must authorize and run the experiment"},
			RequiresHuman:   true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	answer := knowledgeEnvelope(
		t, research, executive.address, contracts.MessageAnswer,
		"message:research-executive", handoff.ThreadID, &handoff.ID,
		researchPacket.Intent.ID, review.Artifact,
		[]contracts.ArtifactRef{experiment.Artifact}, []contracts.EvidenceRef{evidence},
		"Correct the portfolio proposal using the bounded research result", now,
	)
	if _, err := mailbox.Send(ctx, answer, mail.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if delivered, err := mailbox.Dispatch(
		ctx, organizationID, 10,
	); err != nil || delivered != 1 {
		t.Fatalf("dispatch R&D answer = %d, %v", delivered, err)
	}
	openedAnswer, duplicate, err := mailbox.Consume(ctx, mail.ConsumeRequest{
		OrganizationID: organizationID,
		SeatID:         executive.address.SeatID,
		MessageID:      answer.ID,
		IdempotencyKey: "consume:research-executive",
	})
	if err != nil || duplicate {
		t.Fatalf("consume R&D answer duplicate=%v err=%v", duplicate, err)
	}

	correctionPacket := knowledgeLoopPacket(
		t, organizationID, executive.address, contracts.DepartmentExecutive,
		contracts.SeatLead, "wake:executive-correction", "lease:executive-correction",
		"intent:executive-correction",
		knowledgeSkillRef(t, pack, skills.PortfolioPlanningSkill),
		[]contracts.MessageEnvelope{openedAnswer},
		[]contracts.ArtifactRef{plan.Artifact, experiment.Artifact, review.Artifact},
		[]contracts.EvidenceRef{evidence}, now,
	)
	correctionWake, err := runKnowledgeSeat(t, ctx, runner, correctionPacket)
	if err != nil {
		t.Fatal(err)
	}
	correctionOf := plan.Artifact.Hash
	corrected, err := service.Execute(ctx, knowledgework.Input{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: organizationID,
		Department:     contracts.DepartmentExecutive,
		SeatID:         executive.address.SeatID,
		IntentID:       correctionPacket.Intent.ID,
		SkillID:        skills.PortfolioPlanningSkill,
		Objective:      "Correct the portfolio proposal using returned research",
		Constraints:    []string{"The corrected proposal remains advisory and human-run"},
		Evidence:       []contracts.EvidenceRef{evidence},
		SourceDigest:   review.Artifact.Hash,
		CorrectionOf:   &correctionOf,
		Draft: knowledgework.Draft{
			Summary: "Proceed only with a human-authorized offline experiment",
			Findings: []knowledgework.Finding{{
				Statement:   "Research supports only a bounded offline comparison",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			Recommendations: []knowledgework.Recommendation{{
				Action:      "Seek human authorization for the offline experiment",
				Rationale:   "The returned design explicitly requires a human run",
				EvidenceIDs: []contracts.EvidenceID{evidence.ID},
			}},
			RequiresHuman: true,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if corrected.Artifact.Hash == plan.Artifact.Hash ||
		!corrected.RequiresHuman || experiment.Outcome != "requires_human" ||
		review.Outcome != "requires_human" {
		t.Fatalf(
			"correction/result state plan=%s corrected=%s experiment=%s review=%s",
			plan.Artifact.Hash.Digest, corrected.Artifact.Hash.Digest,
			experiment.Outcome, review.Outcome,
		)
	}
	if executiveWake.WakeID == researchWake.WakeID ||
		executiveWake.WakeID == correctionWake.WakeID ||
		researchWake.WakeID == correctionWake.WakeID ||
		researchWake.InputCounts.Inbox != 1 ||
		correctionWake.InputCounts.Inbox != 1 {
		t.Fatalf(
			"fresh wake identities/counts executive=%#v research=%#v correction=%#v",
			executiveWake, researchWake, correctionWake,
		)
	}
}

func buildKnowledgeSeatRunner(t *testing.T) actorstate.Runner {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "workforce-seat")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/workforce-seat")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workforce-seat: %v: %s", err, output)
	}
	return actorstate.Runner{Bubblewrap: "/usr/bin/bwrap", Binary: binary}
}

func runKnowledgeSeat(
	t *testing.T,
	ctx context.Context,
	runner actorstate.Runner,
	packet contracts.WorkPacket,
) (actorstate.SeatOutput, error) {
	t.Helper()
	content, err := os.ReadFile(runner.Binary)
	if err != nil {
		return actorstate.SeatOutput{}, err
	}
	sum := sha256.Sum256(content)
	packet.Lease.Runtime.BuildDigest = contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
	return runner.Run(ctx, packet)
}

type knowledgeMailSeat struct {
	address contracts.SeatAddress
	keyID   string
	public  ed25519.PublicKey
	private ed25519.PrivateKey
}

func newKnowledgeMailSeat(
	t *testing.T,
	organizationID contracts.OrganizationID,
	departmentID contracts.DepartmentID,
	seatID contracts.SeatID,
) knowledgeMailSeat {
	t.Helper()
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return knowledgeMailSeat{
		address: contracts.SeatAddress{
			OrganizationID: organizationID,
			DepartmentID:   departmentID,
			SeatID:         seatID,
		},
		keyID:  "key:" + string(seatID),
		public: publicKey, private: privateKey,
	}
}

func knowledgeMailConfig() mail.Config {
	return mail.Config{
		MaxMailboxMessages: 100,
		MaxThreadMessages:  100,
		MaxThreadDepth:     32,
		MaxRecipients:      32,
		MaxAutoReplies:     8,
		MaxAttachmentBytes: 64 << 20,
		MaxMessageLifetime: 30 * 24 * time.Hour,
	}
}

func knowledgeSkillRef(
	t *testing.T,
	pack []skills.Contract,
	id contracts.SkillID,
) contracts.SkillRef {
	t.Helper()
	for _, contract := range pack {
		if contract.ID == id {
			return contracts.SkillRef{
				ID: contract.ID, Version: contract.Version, Digest: contract.Digest,
			}
		}
	}
	t.Fatalf("knowledge skill %q is absent", id)
	return contracts.SkillRef{}
}

func knowledgeLoopPacket(
	t *testing.T,
	organizationID contracts.OrganizationID,
	address contracts.SeatAddress,
	department contracts.DepartmentKind,
	role contracts.SeatRole,
	wakeID contracts.WakeID,
	leaseID contracts.LeaseID,
	intentID contracts.IntentID,
	skill contracts.SkillRef,
	inbox []contracts.MessageEnvelope,
	artifacts []contracts.ArtifactRef,
	evidence []contracts.EvidenceRef,
	now time.Time,
) contracts.WorkPacket {
	t.Helper()
	hash := func(character string) contracts.ContentHash {
		return contracts.ContentHash{
			Algorithm: "sha256", Digest: strings.Repeat(character, 64),
		}
	}
	signature := knowledgeLoopSignature(t, string(wakeID))
	policyRef := contracts.PolicyRef{
		ID: "policy:knowledge-loop", Version: 1, Hash: hash("1"),
	}
	mandateID := contracts.MandateID(
		"mandate:" + string(department) + ":" + string(role),
	)
	seat := contracts.Seat{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             address.SeatID,
		Version:        1,
		DID:            contracts.SeatDID("did:matrix:" + string(address.SeatID)),
		OrganizationID: organizationID,
		DepartmentID:   address.DepartmentID,
		Role:           role,
		MandateID:      mandateID,
		MandateVersion: 1,
		BindingID:      contracts.SeatBindingID("binding:" + string(address.SeatID)),
		BindingVersion: 1,
		EffectiveAt:    now.Add(-time.Hour),
		Signature:      signature,
	}
	mandate := contracts.Mandate{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             mandateID,
		Version:        1,
		OrganizationID: organizationID,
		DepartmentKind: department,
		SeatRole:       role,
		AllowedSkills:  append([]contracts.SkillID(nil), skill.ID),
		DataScopes: []contracts.DataScope{{
			Name:           "department.evidence",
			Classification: contracts.ClassificationDepartment,
			Purpose:        "Produce bounded evidence-backed proposals",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "human authorization or effect is required",
			Action:    "escalate to the human owner",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID: "no-effect-authority",
			Description: "Cannot approve, spend, publish, deploy, mutate control plane, " +
				"or execute external effects",
		}},
		EffectiveAt: now.Add(-time.Hour),
		Signature:   signature,
	}
	lease := contracts.WakeLease{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             leaseID,
		WakeID:         wakeID,
		OrganizationID: organizationID,
		SeatID:         seat.ID,
		SeatDID:        seat.DID,
		Reason:         "eligible_work",
		MandateID:      mandate.ID,
		MandateVersion: mandate.Version,
		Policies:       []contracts.PolicyRef{policyRef},
		GraphScope:     []contracts.IntentID{intentID},
		Model: contracts.ModelBinding{
			SchemaVersion:  contracts.SchemaVersionV1,
			ID:             contracts.ModelBindingID("model:" + string(address.SeatID)),
			Provider:       "mimo",
			ModelID:        "mimo-v2.5-pro",
			ModelVersion:   "mimo-v2.5-pro",
			SamplingDigest: hash("2"),
		},
		MGS: contracts.MGSGenomeRef{
			Reference: "mgs:knowledge-loop", Digest: hash("3"),
		},
		Runtime: contracts.RuntimeBinding{
			BuildDigest: hash("4"), AuditorBuildDigest: hash("a"),
			OperationRegistryDigest: hash("5"),
		},
		SkillCatalogDigest: hash("6"),
		Budget: contracts.WakeBudget{
			MaxDurationMillis: uint64((20 * time.Minute) / time.Millisecond),
			MaxSteps:          12,
			MaxModelCalls:     4,
			MaxToolCalls:      0,
			MaxCostMinor:      200,
			Currency:          "USD",
			MaxOutputBytes:    1 << 20,
		},
		IssuedAt:  now,
		ExpiresAt: now.Add(time.Hour),
		Fence:     1,
		Signature: signature,
	}
	packet := contracts.WorkPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		Lease:         lease,
		Seat:          seat,
		Mandate:       mandate,
		Goal: contracts.Goal{
			SchemaVersion:   contracts.SchemaVersionV1,
			ID:              "goal:knowledge-loop",
			OrganizationID:  organizationID,
			WorkOrderID:     "order:knowledge-loop",
			Title:           "Evaluate a bounded onboarding experiment",
			SuccessCriteria: []string{"Return evidence-backed advisory work"},
			CreatedAt:       now.Add(-time.Hour),
		},
		Intent: contracts.Intent{
			SchemaVersion:  contracts.SchemaVersionV1,
			ID:             intentID,
			OrganizationID: organizationID,
			GoalID:         "goal:knowledge-loop",
			OwnerSeatID:    seat.ID,
			Summary:        "Produce current evidence-backed knowledge work",
			Priority:       10,
			CreatedAt:      now.Add(-time.Hour),
		},
		Artifacts: artifacts,
		Evidence:  evidence,
		Inbox:     inbox,
		Skills:    []contracts.SkillRef{skill},
		Policies:  []contracts.PolicyRef{policyRef},
		RequiredOutputs: []contracts.RequiredOutput{{
			Kind:             "knowledge_proposal",
			SuccessPredicate: "typed evidence references and mandate remain valid",
		}},
		AssembledAt: now.Add(time.Minute),
	}
	if err := packet.Validate(); err != nil {
		t.Fatal(err)
	}
	return packet
}

func knowledgeEnvelope(
	t *testing.T,
	sender knowledgeMailSeat,
	recipient contracts.SeatAddress,
	kind contracts.MessageKind,
	messageID contracts.MessageID,
	threadID contracts.ThreadID,
	inReplyTo *contracts.MessageID,
	parentIntentID contracts.IntentID,
	payload contracts.ArtifactRef,
	artifacts []contracts.ArtifactRef,
	evidence []contracts.EvidenceRef,
	requiredAction string,
	now time.Time,
) contracts.MessageEnvelope {
	t.Helper()
	envelope := contracts.MessageEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            messageID,
		ThreadID:      threadID,
		InReplyTo:     inReplyTo,
		From:          sender.address,
		To:            []contracts.SeatAddress{recipient},
		Kind:          kind,
		Subject:       "Evidence-bound cross-department work",
		Payload: contracts.MessagePayloadRef{
			SchemaID: "workforce.mail." + string(kind) + ".v1",
			Artifact: payload,
		},
		ParentIntentID: parentIntentID,
		RequiredAction: requiredAction,
		Artifacts:      artifacts,
		Evidence:       evidence,
		Priority:       10,
		TimeoutAction:  contracts.TimeoutEscalate,
		Classification: contracts.ClassificationDepartment,
		IdempotencyKey: "send:" + string(messageID),
		CreatedAt:      now,
		ExpiresAt:      now.Add(30 * time.Minute),
	}
	deadline := now.Add(20 * time.Minute)
	envelope.Deadline = &deadline
	if err := mail.SignEnvelope(&envelope, sender.keyID, sender.private); err != nil {
		t.Fatal(err)
	}
	return envelope
}

func knowledgeLoopEvidence(now time.Time) contracts.EvidenceRef {
	return contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "evidence:activation-baseline",
		Hash: contracts.ContentHash{
			Algorithm: "sha256", Digest: strings.Repeat("7", 64),
		},
		Kind:       "measurement",
		ObservedAt: now,
	}
}

func knowledgeLoopSignature(t *testing.T, label string) contracts.Signature {
	t.Helper()
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     "key:" + strings.ReplaceAll(label, ":", "-"),
		Value: base64.RawURLEncoding.EncodeToString(
			ed25519.Sign(privateKey, []byte(label)),
		),
	}
}
