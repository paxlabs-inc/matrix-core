package developer

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"centra/packages/vault"

	"centra/workforce/internal/actorstate"
	"centra/workforce/internal/approval"
	"centra/workforce/internal/audit"
	"centra/workforce/internal/circuit"
	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/execution"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/lineage"
	"centra/workforce/internal/mail"
	"centra/workforce/internal/projectbrain"
	"centra/workforce/internal/skills"
	"centra/workforce/internal/workcompile"
)

func TestIntegration_MultiWakeDeveloperLoopUsesOnlyDurableCurrentTruth(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	codegraphExecutable, err := exec.LookPath("cg")
	if err != nil {
		t.Fatal(err)
	}
	goExecutable, err := exec.LookPath("go")
	if err != nil {
		t.Fatal(err)
	}
	bubblewrap, err := exec.LookPath("bwrap")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	writeDeveloperRepository(t, root)
	database := filepath.Join(root, ".cg", "codegraph.db")
	if output, err := exec.CommandContext(
		ctx, codegraphExecutable, "--db", database, "build", root,
	).CombinedOutput(); err != nil {
		t.Fatalf("initialize multi-wake CodeGraph: %v: %s", err, output)
	}
	graph, err := projectbrain.NewCodeGraph(codegraphExecutable, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	scope := strings.ReplaceAll(t.Name(), "/", ":")
	tenant := "tenant:multiwake:" + scope
	organizationID := contracts.OrganizationID("organization:multiwake")
	session, err := vault.Boot(ctx, vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenant,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	rememberDeveloperVault(t, tenant, session.UserVault())
	policyAuthority := developerPolicyAuthority(t, tenant, organizationID)
	kernelPublic, kernelPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	brain, err := projectbrain.New(
		developerPool, session.UserVault(), tenant, "kernel-multiwake",
		kernelPublic, policyAuthority.Store(), graph, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	leaseStore, err := lease.New(developerPool, tenant, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := NewAuthority(
		developerPool, leaseStore, graph, brain, tenant, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	leadRequest := developerLeaseRequest("multiwake-lead", organizationID, "task:multiwake-lead")
	executorRequest := developerLeaseRequest(
		"multiwake-executor", organizationID, "task:multiwake-executor",
	)
	requests := []lease.Request{leadRequest, executorRequest}
	roles := []contracts.SeatRole{contracts.SeatLead, contracts.SeatExecutor}
	labels := []string{"lead", "executor"}
	for index, request := range requests {
		insertDeveloperTask(t, tenant, organizationID, request.NodeID)
		requests[index] = insertMultiwakeDeveloperAuthority(
			t, tenant, request, labels[index], roles[index],
		)
	}
	leadRequest, executorRequest = requests[0], requests[1]
	leadScope := ScopeRequest{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:multiwake", WorkspaceID: "workspace:multiwake",
		TaskNodeID: leadRequest.NodeID, WorkspaceRoot: root,
		Files: []string{"c.go"}, Symbols: []string{"Gamma"},
	}
	bindDeveloperCapability(t, &leadScope, tenant, leadRequest, kernelPrivate)
	if err := projectbrain.SignCapabilityGrant(
		&leadScope.Capability, "kernel-multiwake", kernelPrivate,
	); err != nil {
		t.Fatal(err)
	}
	leadGrant, err := authority.Acquire(ctx, leadRequest, leadScope)
	if err != nil {
		t.Fatal(err)
	}
	registerDeveloperLease(t, tenant, leadGrant.Lease)
	executorScope := ScopeRequest{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:multiwake", WorkspaceID: "workspace:multiwake",
		TaskNodeID: executorRequest.NodeID, WorkspaceRoot: root,
		Files: []string{"d.go"}, Symbols: []string{"Delta"},
	}
	bindDeveloperCapability(t, &executorScope, tenant, executorRequest, kernelPrivate)
	if err := projectbrain.SignCapabilityGrant(
		&executorScope.Capability, "kernel-multiwake", kernelPrivate,
	); err != nil {
		t.Fatal(err)
	}
	executorGrant, err := authority.Acquire(ctx, executorRequest, executorScope)
	if err != nil {
		t.Fatal(err)
	}
	registerDeveloperLease(t, tenant, executorGrant.Lease)
	developerAdapter, err := NewAdapter(
		authority, brain, []VerificationCommand{{
			ID: "go_test", Bubblewrap: bubblewrap, Executable: goExecutable,
			Arguments: []string{"test", "./..."},
			Timeout:   time.Minute,
		}}, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	breakers, err := circuit.New(developerPool, tenant, circuit.Config{
		FailureThreshold: 3, SuccessThreshold: 1, Window: time.Minute,
		OpenDuration: time.Minute, HalfOpenLimit: 1, TrialDuration: time.Minute,
	}, developerNow)
	if err != nil {
		t.Fatal(err)
	}
	gateway, err := effect.New(
		developerPool, session.UserVault(), leaseStore,
		developerPolicyAuthority(t, tenant, organizationID).Store(), breakers, tenant,
		approval.Authority{}, developerNow, developerAdapter,
	)
	if err != nil {
		t.Fatal(err)
	}

	skillPublic, skillPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := skills.DeveloperPack()
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := skills.NewCatalog(pack)
	if err != nil {
		t.Fatal(err)
	}
	skillStore, err := skills.NewStore(
		developerPool, session.UserVault(), tenant, organizationID,
		"owner-multiwake", skillPublic, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, contract := range pack {
		published := skills.SignedContract{
			SchemaVersion:  contracts.SchemaVersionV1,
			OrganizationID: organizationID, Contract: contract,
			EffectiveAt: developerNow().Add(-time.Minute),
		}
		if err := skills.SignContract(
			&published, "owner-multiwake", skillPrivate,
		); err != nil {
			t.Fatal(err)
		}
		if err := skillStore.Publish(ctx, published); err != nil {
			t.Fatal(err)
		}
	}
	implementSkill := multiwakeSkillRef(t, pack, skills.DeveloperImplementSkill)
	verifySkill := multiwakeSkillRef(t, pack, skills.DeveloperVerifySkill)

	initialRecord := commitMultiwakeRecord(
		t, ctx, brain, graph, root, tenant, organizationID, kernelPrivate,
		projectbrain.KindPlan, "plan:multiwake",
		"Lead changes Gamma, then Executor follows the verified correction for Delta",
		"task:multiwake-lead", "c.go",
	)
	initialView := readMultiwakeView(
		t, ctx, brain, root, tenant, organizationID, kernelPrivate,
		leadRequest.SeatID, "view:lead", initialRecord.Proposal.ID,
	)

	seatBinary := filepath.Join(t.TempDir(), "workforce-seat")
	buildSeat := exec.CommandContext(
		ctx, goExecutable, "build", "-o", seatBinary, "../../cmd/workforce-seat",
	)
	buildSeat.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := buildSeat.CombinedOutput(); err != nil {
		t.Fatalf("build real workforce-seat: %v: %s", err, output)
	}
	seatRunner := actorstate.Runner{Bubblewrap: bubblewrap, Binary: seatBinary}
	leadPacket := multiwakePacket(
		t, "lead", organizationID, leadRequest, contracts.SeatLead,
		catalog.Digest(), []contracts.SkillRef{implementSkill, verifySkill},
		initialView, nil,
	)
	leadOutput, err := runKnowledgeSeat(t, ctx, seatRunner, leadPacket)
	if err != nil {
		t.Fatalf("fresh Lead wake: %v", err)
	}
	if leadOutput.InputCounts.Inbox != 0 ||
		leadOutput.PacketDigest == (contracts.ContentHash{}) {
		t.Fatalf("unexpected first wake projection: %#v", leadOutput)
	}

	leadChange := []SourceChange{{
		Path: "c.go", BeforeHash: leadGrant.Scope.Files[0].Hash,
		Content: []byte("package scoped\n\nfunc Gamma() int { return 4 }\n"),
	}}
	leadEffect := executeMultiwakeOperation(
		t, ctx, gateway, tenant, session.UserVault(), leadGrant, skills.DeveloperImplementSkill,
		skills.EffectReversible, "apply_scoped_change", "lead-change",
		leadChange, "",
	)
	leadVerification := executeMultiwakeOperation(
		t, ctx, gateway, tenant, session.UserVault(), leadGrant, skills.DeveloperVerifySkill,
		skills.EffectRead, "run_verification", "lead-verification",
		nil, "go_test",
	)
	if output, err := exec.CommandContext(
		ctx, codegraphExecutable, "--db", database, "build", root, "--incremental",
	).CombinedOutput(); err != nil {
		t.Fatalf("sync after Lead wake: %v: %s", err, output)
	}

	correctionRecord := commitMultiwakeRecord(
		t, ctx, brain, graph, root, tenant, organizationID, kernelPrivate,
		projectbrain.KindCorrection, "correction:multiwake",
		"Executor must apply the current-source Delta correction and re-run verification",
		"task:multiwake-executor", "d.go",
	)
	mailbox, handoff := sendMultiwakeHandoff(
		t, ctx, session.UserVault(), tenant, organizationID,
		leadRequest.SeatID, executorRequest.SeatID,
		correctionRecord, developerNow,
	)
	executorView := readMultiwakeView(
		t, ctx, brain, root, tenant, organizationID, kernelPrivate,
		executorRequest.SeatID, "view:executor", correctionRecord.Proposal.ID,
	)
	executorPacket := multiwakePacket(
		t, "executor", organizationID, executorRequest, contracts.SeatExecutor,
		catalog.Digest(), []contracts.SkillRef{implementSkill, verifySkill},
		executorView,
		[]contracts.MessageEnvelope{handoff},
	)
	executorOutput, err := runKnowledgeSeat(t, ctx, seatRunner, executorPacket)
	if err != nil {
		t.Fatalf("fresh Executor wake: %v", err)
	}
	if executorOutput.InputCounts.Inbox != 1 ||
		executorOutput.PacketDigest == leadOutput.PacketDigest {
		t.Fatalf("fresh Executor did not receive only current durable inputs: %#v", executorOutput)
	}
	if err := mailbox.Transition(ctx, mail.TransitionRequest{
		OrganizationID: organizationID, SeatID: executorRequest.SeatID,
		MessageID: handoff.ID, State: mail.StateAcknowledged,
		IdempotencyKey: "ack:multiwake-handoff",
	}); err != nil {
		t.Fatal(err)
	}

	executorChange := []SourceChange{{
		Path: "d.go", BeforeHash: executorGrant.Scope.Files[0].Hash,
		Content: []byte("package scoped\n\nfunc Delta() int { return 5 }\n"),
	}}
	executorEffect := executeMultiwakeOperation(
		t, ctx, gateway, tenant, session.UserVault(), executorGrant, skills.DeveloperImplementSkill,
		skills.EffectReversible, "apply_scoped_change", "executor-change",
		executorChange, "",
	)
	executorVerification := executeMultiwakeOperation(
		t, ctx, gateway, tenant, session.UserVault(), executorGrant, skills.DeveloperVerifySkill,
		skills.EffectRead, "run_verification", "executor-verification",
		nil, "go_test",
	)
	if output, err := exec.CommandContext(
		ctx, codegraphExecutable, "--db", database, "build", root, "--incremental",
	).CombinedOutput(); err != nil {
		t.Fatalf("sync after Executor wake: %v: %s", err, output)
	}
	finalRecord := commitMultiwakeRecord(
		t, ctx, brain, graph, root, tenant, organizationID, kernelPrivate,
		projectbrain.KindOutcome, "outcome:multiwake",
		"Both scoped changes and the correction passed current repository verification",
		"task:multiwake-executor", "d.go",
	)
	finalView := readMultiwakeView(
		t, ctx, brain, root, tenant, organizationID, kernelPrivate,
		leadRequest.SeatID, "view:restart", finalRecord.Proposal.ID,
	)
	if len(finalView.Records) == 0 ||
		finalView.Source.RootDigest == initialView.Source.RootDigest {
		t.Fatal("Project Brain did not advance from current source across wakes")
	}

	compiler, err := workcompile.New(
		developerPool, session.UserVault(), tenant, skillStore,
		developerPolicyAuthority(t, tenant, organizationID).Store(),
		leaseStore, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	lineageStore, err := lineage.New(
		developerPool, session.UserVault(), tenant, "lineage-multiwake",
		kernelPrivate, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	executionStore, err := execution.New(
		developerPool, session.UserVault(), tenant, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	leadReceipt := completeMultiwakeWake(
		t, ctx, tenant, executionStore, compiler, lineageStore,
		leadPacket, leadOutput, leadGrant, verifySkill,
		leadEffect.ExternalID, leadVerification.EvidenceHash, "lead",
	)
	// A fresh store instance is the restart boundary: it must recover the prior
	// committed wake before accepting the next fresh process.
	executionStore, err = execution.New(
		developerPool, session.UserVault(), tenant, developerNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := executionStore.Load(
		ctx, organizationID, leadPacket.Lease.WakeID,
	)
	if err != nil || recovered.ReceiptID != leadReceipt.ID ||
		recovered.Disposition != contracts.DispositionProgressed {
		t.Fatalf("restart recovery = %#v, %v", recovered, err)
	}
	executorReceipt := completeMultiwakeWake(
		t, ctx, tenant, executionStore, compiler, lineageStore,
		executorPacket, executorOutput, executorGrant, verifySkill,
		executorEffect.ExternalID, executorVerification.EvidenceHash, "executor",
	)
	openedReceipt, err := lineageStore.OpenReceipt(
		ctx, organizationID, executorReceipt.ID,
	)
	if err != nil || openedReceipt.ContentHash != executorReceipt.ContentHash ||
		openedReceipt.Source.GraphGeneration == 0 ||
		openedReceipt.ModelRequestHash == (contracts.ContentHash{}) {
		t.Fatalf("durable complete lineage receipt = %#v, %v", openedReceipt, err)
	}

	runMultiwakeDeveloperAudit(
		t, ctx, bubblewrap, goExecutable, graph, root, finalView,
		executorPacket, executorGrant, executorChange,
		executorVerification.EvidenceHash, implementSkill, kernelPrivate,
	)
}

func insertMultiwakeDeveloperAuthority(
	t *testing.T,
	tenant string,
	request lease.Request,
	label string,
	role contracts.SeatRole,
) lease.Request {
	t.Helper()
	fixture := developerPolicyAuthority(t, tenant, request.OrganizationID)
	seat := contracts.Seat{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.SeatID, Version: 1,
		DID:            contracts.SeatDID("did:matrix:developer:" + label),
		OrganizationID: request.OrganizationID,
		DepartmentID:   "department:developer", Role: role,
		MandateID: request.MandateID, MandateVersion: request.MandateVersion,
		BindingID: contracts.SeatBindingID("binding:" + label), BindingVersion: 1,
		EffectiveAt: request.IssuedAt.Add(-time.Hour),
	}
	mandate := contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            request.MandateID, Version: request.MandateVersion,
		OrganizationID: request.OrganizationID,
		DepartmentKind: contracts.DepartmentDeveloper, SeatRole: role,
		AllowedSkills: []contracts.SkillID{
			skills.DeveloperImplementSkill, skills.DeveloperVerifySkill,
		},
		DataScopes: []contracts.DataScope{{
			Name: "source", Classification: contracts.ClassificationProject,
			Purpose: "Implement only the current fenced Developer task",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "Current durable evidence is insufficient",
			Action:    "Escalate to the human owner",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID:    "no-ambient-authority",
			Description: "No effect credentials or prior-session memory",
		}},
		EffectiveAt: request.IssuedAt.Add(-time.Hour),
	}
	published, err := fixture.PublishExact(
		context.Background(), request, seat, mandate,
	)
	if err != nil {
		t.Fatal(err)
	}
	developerAuthorityMutex.Lock()
	developerPublishedLeases[tenant+"|"+string(request.ID)] = published
	developerAuthorityMutex.Unlock()
	return published.Request
}

func multiwakeSkillRef(
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
	t.Fatalf("Developer skill %q is absent", id)
	return contracts.SkillRef{}
}

func compilerSkillOperation(
	skill contracts.SkillRef,
	name string,
) (skills.Operation, error) {
	pack, err := skills.DeveloperPack()
	if err != nil {
		return skills.Operation{}, err
	}
	for _, contract := range pack {
		if contract.ID != skill.ID || contract.Version != skill.Version ||
			contract.Digest != skill.Digest {
			continue
		}
		for _, operation := range contract.Operations {
			if operation.Name == name {
				return operation, nil
			}
		}
	}
	return skills.Operation{}, errors.New("Developer operation contract is absent")
}

func commitMultiwakeRecord(
	t *testing.T,
	ctx context.Context,
	brain *projectbrain.Store,
	graph *projectbrain.CodeGraph,
	root, tenant string,
	organizationID contracts.OrganizationID,
	kernelPrivate ed25519.PrivateKey,
	kind projectbrain.Kind,
	id projectbrain.RecordID,
	summary, statement, filePath string,
) projectbrain.EngineeringRecord {
	t.Helper()
	source, err := graph.Capture(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	var sourceFile projectbrain.GraphFile
	for _, file := range source.Files {
		if file.Path == filePath {
			sourceFile = file
			break
		}
	}
	if sourceFile.Path == "" {
		t.Fatalf("current graph omitted %s", filePath)
	}
	authorPublic, authorPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifierPublic, verifierPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	proposal := projectbrain.Proposal{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             id,
		OrganizationID: organizationID,
		ProjectID:      "project:multiwake",
		WorkspaceID:    "workspace:multiwake",
		AuthorSeatID:   "seat:multiwake-author:" + contracts.SeatID(string(id)),
		ParentIntentID: "intent:multiwake",
		Kind:           kind,
		Origin:         projectbrain.OriginSource,
		Version:        1,
		Source:         source,
		Content: projectbrain.Content{
			Summary: summary,
			Claims: []projectbrain.Claim{{
				Statement: statement,
				Files: []projectbrain.FileEvidence{{
					Path: filePath, Hash: sourceFile.Hash,
					StartLine: 1, EndLine: 3,
				}},
			}},
		},
		CreatedAt: developerNow(),
	}
	if kind == projectbrain.KindCorrection {
		corrects := projectbrain.RecordID("plan:multiwake")
		proposal.Corrects = &corrects
		proposal.Version = 2
	}
	if err := projectbrain.SignProposal(
		&proposal, "author:"+string(id), authorPrivate,
	); err != nil {
		t.Fatal(err)
	}
	proposalHash, err := projectbrain.ProposalHash(proposal)
	if err != nil {
		t.Fatal(err)
	}
	verification := projectbrain.Verification{
		SchemaVersion: contracts.SchemaVersionV1,
		RecordID:      id,
		VerifierSeatID: "seat:multiwake-verifier:" +
			contracts.SeatID(string(id)),
		ProposalHash: proposalHash,
		Accepted:     true,
		Procedure:    "multiwake-independent-source-review.v1",
		Evidence: []contracts.EvidenceRef{{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:" + contracts.EvidenceID(string(id)),
			Hash:          developerHash("evidence:" + string(id)),
			Kind:          "independent_review",
			ObservedAt:    developerNow(),
		}},
		VerifiedAt: developerNow(),
	}
	if err := projectbrain.SignVerification(
		&verification, "verifier:"+string(id), verifierPrivate,
	); err != nil {
		t.Fatal(err)
	}
	record := projectbrain.EngineeringRecord{
		Proposal: proposal, Verification: verification,
	}
	authorSeat := publishDeveloperProjectBrainSeat(
		t, tenant, organizationID, proposal.AuthorSeatID,
	)
	verifierSeat := publishDeveloperProjectBrainSeat(
		t, tenant, organizationID, verification.VerifierSeatID,
	)
	recordID := id
	grant := projectbrain.CapabilityGrant{
		SchemaVersion:           contracts.SchemaVersionV1,
		ID:                      "grant:write:" + string(id),
		TenantID:                tenant,
		OrganizationID:          organizationID,
		ProjectID:               proposal.ProjectID,
		WorkspaceID:             proposal.WorkspaceID,
		WorkspaceRoot:           root,
		Operation:               projectbrain.CapabilityWrite,
		RequesterSeatID:         authorSeat.ID,
		RequesterSeatVersion:    authorSeat.Version,
		RequesterSeatDID:        authorSeat.DID,
		RequesterBindingID:      authorSeat.BindingID,
		RequesterBindingVersion: authorSeat.BindingVersion,
		RecordID:                &recordID,
		Author: developerProjectBrainKeyBinding(
			authorSeat, proposal.Signature.KeyID, authorPublic,
		),
		Verifier: developerProjectBrainKeyBinding(
			verifierSeat, verification.Signature.KeyID, verifierPublic,
		),
		Purpose:   "verified-multiwake-engineering-record",
		IssuedAt:  developerNow().Add(-time.Minute),
		ExpiresAt: developerNow().Add(time.Hour),
	}
	if err := projectbrain.SignCapabilityGrant(
		&grant, "kernel-multiwake", kernelPrivate,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := brain.Commit(ctx, record, grant); err != nil {
		t.Fatal(err)
	}
	return record
}

func readMultiwakeView(
	t *testing.T,
	ctx context.Context,
	brain *projectbrain.Store,
	root, tenant string,
	organizationID contracts.OrganizationID,
	kernelPrivate ed25519.PrivateKey,
	seatID contracts.SeatID,
	grantID string,
	expected projectbrain.RecordID,
) projectbrain.View {
	t.Helper()
	seat := developerCurrentSeat(t, tenant, organizationID, seatID)
	grant := projectbrain.CapabilityGrant{
		SchemaVersion:           contracts.SchemaVersionV1,
		ID:                      grantID,
		TenantID:                tenant,
		OrganizationID:          organizationID,
		ProjectID:               "project:multiwake",
		WorkspaceID:             "workspace:multiwake",
		WorkspaceRoot:           root,
		Operation:               projectbrain.CapabilityRead,
		RequesterSeatID:         seat.ID,
		RequesterSeatVersion:    seat.Version,
		RequesterSeatDID:        seat.DID,
		RequesterBindingID:      seat.BindingID,
		RequesterBindingVersion: seat.BindingVersion,
		Purpose:                 "multiwake-current-state",
		MaxRecords:              64,
		IssuedAt:                developerNow().Add(-time.Minute),
		ExpiresAt:               developerNow().Add(time.Hour),
	}
	if err := projectbrain.SignCapabilityGrant(
		&grant, "kernel-multiwake", kernelPrivate,
	); err != nil {
		t.Fatal(err)
	}
	view, err := brain.View(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	for _, record := range view.Records {
		if record.Proposal.ID == expected {
			return view
		}
	}
	t.Fatalf("current Project Brain view omitted %q: %#v", expected, view)
	return projectbrain.View{}
}

func multiwakePacket(
	t *testing.T,
	label string,
	organizationID contracts.OrganizationID,
	request lease.Request,
	role contracts.SeatRole,
	catalogDigest contracts.ContentHash,
	skillRefs []contracts.SkillRef,
	view projectbrain.View,
	inbox []contracts.MessageEnvelope,
) contracts.WorkPacket {
	t.Helper()
	sortedSkills := append([]contracts.SkillRef(nil), skillRefs...)
	sort.Slice(sortedSkills, func(left, right int) bool {
		return sortedSkills[left].ID < sortedSkills[right].ID
	})
	allowed := make([]contracts.SkillID, len(sortedSkills))
	for index, skill := range sortedSkills {
		allowed[index] = skill.ID
	}
	seatDID := contracts.SeatDID(
		"did:matrix:developer:" + label,
	)
	seat := contracts.Seat{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             request.SeatID,
		Version:        1,
		DID:            seatDID,
		OrganizationID: organizationID,
		DepartmentID:   "department:developer",
		Role:           role,
		MandateID:      request.MandateID,
		MandateVersion: request.MandateVersion,
		BindingID:      contracts.SeatBindingID("binding:" + label),
		BindingVersion: 1,
		EffectiveAt:    request.IssuedAt.Add(-time.Hour),
		Signature:      multiwakeSignature("seat:" + label),
	}
	mandate := contracts.Mandate{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             request.MandateID,
		Version:        request.MandateVersion,
		OrganizationID: organizationID,
		DepartmentKind: contracts.DepartmentDeveloper,
		SeatRole:       role,
		AllowedSkills:  allowed,
		DataScopes: []contracts.DataScope{{
			Name: "source", Classification: contracts.ClassificationProject,
			Purpose: "Implement only the current fenced Developer task",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "Current durable evidence is insufficient",
			Action:    "Escalate to the human owner",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID:    "no-ambient-authority",
			Description: "No effect credentials or prior-session memory",
		}},
		EffectiveAt: request.IssuedAt.Add(-time.Hour),
		Signature:   multiwakeSignature("mandate:" + label),
	}
	intentID := contracts.IntentID(request.NodeID)
	goalID := contracts.GoalID("goal:multiwake:" + label)
	brainRef := contracts.ProjectBrainRef{
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
	}
	packet := contracts.WorkPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		Lease: contracts.WakeLease{
			SchemaVersion:  contracts.SchemaVersionV1,
			ID:             request.ID,
			WakeID:         request.WakeID,
			OrganizationID: organizationID,
			SeatID:         request.SeatID,
			SeatDID:        seatDID,
			Reason:         "eligible_work",
			MandateID:      request.MandateID,
			MandateVersion: request.MandateVersion,
			Policies:       append([]contracts.PolicyRef(nil), request.Policies...),
			GraphScope:     []contracts.IntentID{intentID},
			Model: contracts.ModelBinding{
				SchemaVersion: contracts.SchemaVersionV1,
				ID:            contracts.ModelBindingID("model:multiwake:" + label),
				Provider:      "mimo",
				ModelID:       "mimo-v2.5-pro",
				ModelVersion:  "mimo-v2.5-pro",
				SamplingDigest: developerHash(
					"sampling:" + label,
				),
			},
			MGS: contracts.MGSGenomeRef{
				Reference: "mgs:multiwake:" + label,
				Digest:    developerHash("mgs:" + label),
			},
			Runtime: contracts.RuntimeBinding{
				BuildDigest: developerHash("runtime:" + label),
				AuditorBuildDigest: developerHash(
					"auditor-runtime:" + label,
				),
				OperationRegistryDigest: developerHash(
					"registry:" + label,
				),
			},
			SkillCatalogDigest: catalogDigest,
			Budget: contracts.WakeBudget{
				MaxDurationMillis: uint64((30 * time.Minute) / time.Millisecond),
				MaxSteps:          32, MaxModelCalls: 8, MaxToolCalls: 32,
				MaxCostMinor: 1000, Currency: "USD", MaxOutputBytes: 1 << 20,
			},
			IssuedAt:  request.IssuedAt,
			ExpiresAt: request.ExpiresAt,
			Fence:     1,
			Signature: multiwakeSignature("lease:" + label),
		},
		Seat:    seat,
		Mandate: mandate,
		Goal: contracts.Goal{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            goalID, OrganizationID: organizationID,
			WorkOrderID: "work-order:multiwake",
			Title:       "Complete the bounded multi-wake Developer change",
			SuccessCriteria: []string{
				"Current repository tests and independent review pass",
			},
			CreatedAt: request.IssuedAt.Add(-time.Hour),
		},
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            intentID, OrganizationID: organizationID,
			GoalID: goalID, OwnerSeatID: request.SeatID,
			Summary:  "Execute only the current Project Brain-backed change scope",
			Priority: 10, CreatedAt: request.IssuedAt.Add(-time.Hour),
		},
		Artifacts: []contracts.ArtifactRef{{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.ArtifactID("artifact:project-brain:" + label),
			Hash:          view.Digest,
			MediaType:     "application/vnd.matrix.project-brain+json",
			SizeBytes:     1,
		}},
		Inbox:  append([]contracts.MessageEnvelope(nil), inbox...),
		Tools:  []contracts.ToolRef{{Name: "codegraph", SchemaDigest: view.Source.GraphDigest}},
		Skills: sortedSkills,
		Policies: append(
			[]contracts.PolicyRef(nil), request.Policies...,
		),
		RequiredOutputs: []contracts.RequiredOutput{{
			Kind:             "source_change",
			SuccessPredicate: "Affected repository tests pass and evidence is receipted",
		}},
		ProjectBrain: &brainRef,
		AssembledAt:  developerNow(),
	}
	if err := packet.Validate(); err != nil {
		t.Fatalf("multi-wake WorkPacket %s: %v", label, err)
	}
	return packet
}

func multiwakeSignature(keyID string) contracts.Signature {
	return contracts.Signature{
		Algorithm: "ed25519",
		KeyID:     keyID,
		Value: base64.RawURLEncoding.EncodeToString(
			make([]byte, ed25519.SignatureSize),
		),
	}
}

func sendMultiwakeHandoff(
	t *testing.T,
	ctx context.Context,
	userVault *vault.UserVault,
	tenant string,
	organizationID contracts.OrganizationID,
	senderID, recipientID contracts.SeatID,
	record projectbrain.EngineeringRecord,
	now func() time.Time,
) (*mail.Store, contracts.MessageEnvelope) {
	t.Helper()
	graphStore, err := dependency.New(developerPool, tenant, now)
	if err != nil {
		t.Fatal(err)
	}
	mailbox, err := mail.New(
		developerPool, userVault, graphStore, tenant, mail.Config{
			MaxMailboxMessages: 100, MaxThreadMessages: 100,
			MaxThreadDepth: 32, MaxRecipients: 8, MaxAutoReplies: 4,
			MaxAttachmentBytes: 8 << 20, MaxMessageLifetime: 24 * time.Hour,
		}, now,
	)
	if err != nil {
		t.Fatal(err)
	}
	departmentID := contracts.DepartmentID("department:developer")
	sender := contracts.SeatAddress{
		OrganizationID: organizationID,
		DepartmentID:   departmentID, SeatID: senderID,
	}
	recipient := contracts.SeatAddress{
		OrganizationID: organizationID,
		DepartmentID:   departmentID, SeatID: recipientID,
	}
	senderPublic, senderPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	recipientPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []struct {
		address contracts.SeatAddress
		public  ed25519.PublicKey
	}{
		{sender, senderPublic}, {recipient, recipientPublic},
	} {
		insertMultiwakeSeatAuthority(t, tenant, value.address, now())
		if err := mailbox.PublishSeatKey(ctx, mail.SeatKey{
			Address:     value.address,
			KeyID:       "mail-key:" + string(value.address.SeatID),
			PublicKey:   value.public,
			EffectiveAt: now(),
		}); err != nil {
			t.Fatal(err)
		}
	}
	recordHash, err := contracts.HashCanonical(&record)
	if err != nil {
		t.Fatal(err)
	}
	envelope := contracts.MessageEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "message:multiwake-handoff",
		ThreadID:      "thread:multiwake",
		From:          sender,
		To:            []contracts.SeatAddress{recipient},
		Kind:          contracts.MessageHandoff,
		Subject:       "Verified Developer correction handoff",
		Payload: contracts.MessagePayloadRef{
			SchemaID: "workforce.mail.handoff.v1",
			Artifact: contracts.ArtifactRef{
				SchemaVersion: contracts.SchemaVersionV1,
				ID:            "artifact:multiwake-correction",
				Hash:          recordHash,
				MediaType:     "application/vnd.matrix.project-brain+json",
				SizeBytes:     1,
			},
		},
		ParentIntentID: "intent:multiwake",
		RequiredAction: "Apply the current-source correction and return test evidence",
		Priority:       10,
		TimeoutAction:  contracts.TimeoutEscalate,
		Classification: contracts.ClassificationDepartment,
		IdempotencyKey: "mail:multiwake-handoff",
		CreatedAt:      now(),
		ExpiresAt:      now().Add(30 * time.Minute),
	}
	deadline := now().Add(20 * time.Minute)
	envelope.Deadline = &deadline
	if err := mail.SignEnvelope(
		&envelope, "mail-key:"+string(senderID), senderPrivate,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := mailbox.Send(ctx, envelope, mail.SendOptions{}); err != nil {
		t.Fatal(err)
	}
	if count, err := mailbox.Dispatch(ctx, organizationID, 10); err != nil || count != 1 {
		t.Fatalf("dispatch multi-wake handoff = %d, %v", count, err)
	}
	opened, duplicate, err := mailbox.Consume(ctx, mail.ConsumeRequest{
		OrganizationID: organizationID,
		SeatID:         recipientID, MessageID: envelope.ID,
		IdempotencyKey: "consume:multiwake-handoff",
	})
	if err != nil || duplicate {
		t.Fatalf("consume multi-wake handoff duplicate=%v err=%v", duplicate, err)
	}
	return mailbox, opened
}

func insertMultiwakeSeatAuthority(
	t *testing.T,
	tenant string,
	address contracts.SeatAddress,
	now time.Time,
) {
	t.Helper()
	hash := developerHash("seat-authority:" + string(address.SeatID)).Digest
	if _, err := developerPool.Exec(context.Background(), `
		INSERT INTO workforce_authority_records (
			tenant_id,organization_id,authority_kind,authority_id,version,
			owner_id,key_id,effective_at,canonical_hash,sealed_record,
			material_change,created_at
		) VALUES ($1,$2,'seat',$3,1,'owner','owner-key',$4,$5,$6,FALSE,$4)
		ON CONFLICT DO NOTHING
	`, tenant, address.OrganizationID, address.SeatID, now, hash, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if _, err := developerPool.Exec(context.Background(), `
		INSERT INTO workforce_authority_heads (
			tenant_id,organization_id,authority_kind,authority_id,
			latest_version,updated_at
		) VALUES ($1,$2,'seat',$3,1,$4)
		ON CONFLICT DO NOTHING
	`, tenant, address.OrganizationID, address.SeatID, now); err != nil {
		t.Fatal(err)
	}
}

func executeMultiwakeOperation(
	t *testing.T,
	ctx context.Context,
	gateway *effect.Gateway,
	tenant string,
	userVault *vault.UserVault,
	grant Grant,
	skillID contracts.SkillID,
	class skills.EffectClass,
	operation, label string,
	changes []SourceChange,
	verification string,
) effect.Result {
	t.Helper()
	input, err := json.Marshal(OperationEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		Grant:         grant, Changes: changes, Verification: verification,
	})
	if err != nil {
		t.Fatal(err)
	}
	proposal := effect.Proposal{
		ID:              "proposal:" + label,
		OrganizationID:  grant.Lease.OrganizationID,
		IntentID:        contracts.IntentID(grant.Lease.NodeID),
		NodeID:          grant.Lease.NodeID,
		SeatID:          grant.Lease.SeatID,
		LeaseID:         grant.Lease.ID,
		Fence:           grant.Lease.Fence,
		Provider:        developerAdapterName,
		SkillID:         skillID,
		EffectClass:     class,
		Operation:       operation,
		IdempotencyKey:  "multiwake:" + label,
		SkillDigest:     developerHash("skill:" + label),
		OperationDigest: developerHash("operation:" + label),
		Input:           input,
		Deadline:        developerNow().Add(30 * time.Minute),
	}
	if err := authorizeDeveloperProposal(ctx, tenant, userVault, proposal); err != nil {
		t.Fatal(err)
	}
	result, err := gateway.Execute(ctx, proposal)
	if err != nil || result.State != effect.StateSucceeded {
		t.Fatalf("%s result=%#v err=%v", label, result, err)
	}
	return result
}

func completeMultiwakeWake(
	t *testing.T,
	ctx context.Context,
	tenant string,
	executionStore *execution.Store,
	compiler *workcompile.Compiler,
	lineageStore *lineage.Store,
	packet contracts.WorkPacket,
	output actorstate.SeatOutput,
	grant Grant,
	verifySkill contracts.SkillRef,
	effectID string,
	verificationHash contracts.ContentHash,
	label string,
) contracts.Receipt {
	t.Helper()
	input, err := json.Marshal(OperationEnvelope{
		SchemaVersion: contracts.SchemaVersionV1,
		Grant:         grant, Verification: "go_test",
	})
	if err != nil {
		t.Fatal(err)
	}
	published, err := compilerSkillOperation(
		verifySkill, "run_verification",
	)
	if err != nil {
		t.Fatal(err)
	}
	if !workcompile.InputConforms(published.InputSchema, input) {
		t.Fatalf("Developer verification input does not satisfy its contract: %s", input)
	}
	// The compiler admits only genuinely signed authority bound to the live
	// runtime fence, exactly as the kernel presents a packet in production.
	authority, signedLease := developerPublishedAuthority(
		t, tenant, packet.Lease.ID,
	)
	packet.Seat = authority.Seat
	packet.Mandate = authority.Mandate
	packet.Lease = signedLease
	packet.Policies = append([]contracts.PolicyRef(nil), signedLease.Policies...)
	source := packet.ProjectBrain.Source
	plan, err := compiler.Compile(ctx, packet, workcompile.Proposal{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             "compile:" + label,
		OrganizationID: packet.Lease.OrganizationID,
		WakeID:         packet.Lease.WakeID,
		IntentID:       packet.Intent.ID,
		SeatID:         packet.Seat.ID,
		Skill:          verifySkill,
		Operation:      "run_verification",
		Provider:       developerAdapterName,
		IdempotencyKey: "compiled-verification:" + label,
		Input:          input,
		Deadline:       developerNow().Add(30 * time.Minute),
	}, source)
	if err != nil {
		t.Fatal(err)
	}
	packetBytes, err := json.Marshal(packet)
	if err != nil {
		t.Fatal(err)
	}
	outputBytes, err := json.Marshal(output)
	if err != nil {
		t.Fatal(err)
	}
	modelEvidence, err := lineageStore.PutModelEvidence(ctx, lineage.ModelExchange{
		ID:             contracts.EvidenceID("model-evidence:" + label),
		OrganizationID: packet.Lease.OrganizationID,
		WakeID:         packet.Lease.WakeID,
		Model:          packet.Lease.Model,
		MGS:            packet.Lease.MGS,
		Runtime:        packet.Lease.Runtime,
		Request:        packetBytes,
		Response:       outputBytes,
		Output:         outputBytes,
		ReplayRequired: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	testEvidence := contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            contracts.EvidenceID("verification:" + label),
		Hash:          verificationHash,
		Kind:          "test_result",
		ObservedAt:    developerNow(),
	}
	receipt, err := lineageStore.BuildReceipt(lineage.ReceiptInput{
		ID:            contracts.ReceiptID("receipt:" + label),
		Packet:        packet,
		Plan:          plan,
		ModelEvidence: modelEvidence,
		Constraints: []string{
			"fresh process, current Project Brain, exact change scope, and live fence",
		},
		Artifacts: packet.Artifacts,
		Evidence:  []contracts.EvidenceRef{testEvidence},
		CostMinor: 1, LatencyMillis: 1,
		Disposition:      contracts.DispositionProgressed,
		OperationOutcome: "succeeded",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := lineageStore.PublishReceipt(ctx, receipt); err != nil {
		t.Fatal(err)
	}
	state, err := executionStore.Start(ctx, packet)
	if err != nil {
		t.Fatal(err)
	}
	sequence := 0
	advance := func(request execution.AdvanceRequest) {
		t.Helper()
		sequence++
		request.OrganizationID = packet.Lease.OrganizationID
		request.WakeID = packet.Lease.WakeID
		request.ExpectedVersion = state.Version
		request.IdempotencyKey = "transition:" + label + ":" +
			strconv.Itoa(sequence)
		state, err = executionStore.Advance(ctx, request)
		if err != nil {
			t.Fatalf("advance %s %d: %v", label, sequence, err)
		}
	}
	for state.Stage != execution.StageExecute {
		advance(execution.AdvanceRequest{Decision: execution.DecisionAdvance})
	}
	advance(execution.AdvanceRequest{
		Decision: execution.DecisionDispatch, EffectID: effectID,
	})
	advance(execution.AdvanceRequest{
		Decision: execution.DecisionObserved, EffectID: effectID,
	})
	advance(execution.AdvanceRequest{Decision: execution.DecisionAdvance})
	advance(execution.AdvanceRequest{Decision: execution.DecisionAdvance})
	advance(execution.AdvanceRequest{
		Decision: execution.DecisionAdvance, ReceiptID: receipt.ID,
	})
	advance(execution.AdvanceRequest{
		Decision:         execution.DecisionAdvance,
		FinalDisposition: contracts.DispositionProgressed,
		ReasonCode:       "verified_progress",
	})
	if state.Stage != execution.StageSleep ||
		state.ReceiptID != receipt.ID ||
		state.Disposition != contracts.DispositionProgressed {
		t.Fatalf("terminal multi-wake checkpoint = %#v", state)
	}
	return receipt
}

func runMultiwakeDeveloperAudit(
	t *testing.T,
	ctx context.Context,
	bubblewrap, goExecutable string,
	graph *projectbrain.CodeGraph,
	root string,
	view projectbrain.View,
	packet contracts.WorkPacket,
	grant Grant,
	changes []SourceChange,
	verificationHash contracts.ContentHash,
	skill contracts.SkillRef,
	kernelPrivate ed25519.PrivateKey,
) {
	t.Helper()
	current, err := graph.Capture(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	changed := make([]ChangedFile, 0, len(changes))
	for _, change := range changes {
		changed = append(changed, ChangedFile{
			Path: change.Path, BeforeHash: change.BeforeHash,
			AfterHash: developerHash(string(change.Content)),
		})
	}
	testEvidence := contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "evidence:multiwake-audit-test",
		Hash:          verificationHash,
		Kind:          "test_result",
		ObservedAt:    developerNow(),
	}
	procedure := contracts.VerificationProcedureRef{
		ID: "developer-multiwake-auditor.v1", Version: 1,
		Digest: developerHash("developer-multiwake-auditor.v1"),
	}
	auditPacket := AuditPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		Intent:        packet.Intent,
		ProjectID:     view.ProjectID,
		WorkspaceID:   view.WorkspaceID,
		ViewDigest:    view.Digest,
		Graph:         current,
		ChangedSource: changed,
		BlastRadius: append(
			[]projectbrain.ImpactNode(nil), grant.Scope.BlastRadius...,
		),
		TestEvidence: []contracts.EvidenceRef{testEvidence},
		Verifier:     procedure,
		AssembledAt:  developerNow(),
	}
	source := contracts.SourceState{
		RootDigest: current.RootDigest, GraphGeneration: current.Generation,
		LedgerCursor: uint64(len(view.Records) + len(view.StaleRecordIDs) + 1),
	}
	base := contracts.VerdictPacket{
		SchemaVersion:   contracts.SchemaVersionV1,
		OrganizationID:  packet.Lease.OrganizationID,
		Intent:          packet.Intent,
		ExecutingSeatID: packet.Seat.ID,
		AuditorSeatID:   "seat:developer-auditor",
		Procedure:       procedure,
		Predicates: []contracts.VerificationPredicate{{
			ID:           "predicate:multiwake-tests",
			Kind:         contracts.PredicateEvidenceHash,
			SubjectID:    string(testEvidence.ID),
			ExpectedHash: &testEvidence.Hash,
			Description:  "Current affected repository tests must pass",
		}},
		Skill:          skill,
		VerifierDigest: procedure.Digest,
		Artifacts:      append([]contracts.ArtifactRef(nil), packet.Artifacts...),
		Observations:   []contracts.EvidenceRef{testEvidence},
		Model:          packet.Lease.Model,
		MGS:            packet.Lease.MGS,
		Runtime:        packet.Lease.Runtime,
		Source:         source,
	}
	auditorBinary := filepath.Join(t.TempDir(), "workforce-auditor")
	build := exec.CommandContext(
		ctx, goExecutable, "build", "-o", auditorBinary,
		"../../cmd/workforce-auditor",
	)
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build real workforce-auditor: %v: %s", err, output)
	}
	publicKey := kernelPrivate.Public().(ed25519.PublicKey)
	runner := audit.Runner{
		Bubblewrap:              bubblewrap,
		Binary:                  auditorBinary,
		DeveloperAuthorityKeyID: "kernel-multiwake",
		DeveloperAuthorityKey:   publicKey,
	}
	dispatcher, err := NewAuditor(
		runner, "kernel-multiwake", kernelPrivate,
	)
	if err != nil {
		t.Fatal(err)
	}
	auditorContent, err := os.ReadFile(auditorBinary)
	if err != nil {
		t.Fatal(err)
	}
	auditorSum := sha256.Sum256(auditorContent)
	base.Runtime.AuditorBuildDigest = contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(auditorSum[:]),
	}
	decision, err := dispatcher.Run(ctx, base, auditPacket)
	if err != nil {
		t.Fatalf("fresh memoryless Developer Auditor: %v", err)
	}
	if decision.Outcome != contracts.VerdictPass ||
		len(decision.Evidence) != 1 {
		t.Fatalf("Developer Auditor decision = %#v", decision)
	}
}
