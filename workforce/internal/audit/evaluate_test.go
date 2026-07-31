package audit

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
)

func TestEvaluateDeterministicPredicatesAndSemanticEscalation(t *testing.T) {
	packet := auditPacket(t, "intent-pass")
	passed, err := Evaluate(packet)
	if err != nil {
		t.Fatal(err)
	}
	if passed.Outcome != contracts.VerdictPass {
		t.Fatalf("pass outcome = %q", passed.Outcome)
	}

	semantic := packet
	semantic.Predicates = append(semantic.Predicates, contracts.VerificationPredicate{
		ID: "semantic-review", Kind: contracts.PredicateSemantic,
		Description: "A human must assess whether the result is strategically useful",
	})
	human, err := Evaluate(semantic)
	if err != nil {
		t.Fatal(err)
	}
	if human.Outcome != contracts.VerdictRequiresHuman {
		t.Fatalf("semantic outcome = %q", human.Outcome)
	}

	failed := semantic
	failed.Predicates = append([]contracts.VerificationPredicate(nil), semantic.Predicates...)
	wrong := auditHash("wrong-artifact")
	failed.Predicates[0].ExpectedHash = &wrong
	decision, err := Evaluate(failed)
	if err != nil {
		t.Fatal(err)
	}
	if decision.Outcome != contracts.VerdictFail {
		t.Fatalf("deterministic failure did not override semantic review: %q", decision.Outcome)
	}
}

func TestEvaluateIsStableAndProcedureVersionIsBound(t *testing.T) {
	packet := auditPacket(t, "intent-stable")
	first, err := Evaluate(packet)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Evaluate(packet)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("identical VerdictPackets produced different decisions")
	}
	changed := packet
	changed.Procedure.Version = 2
	changed.Procedure.Digest = auditHash("procedure-v2")
	third, err := Evaluate(changed)
	if err != nil {
		t.Fatal(err)
	}
	if third.PacketDigest == first.PacketDigest || third.Procedure == first.Procedure {
		t.Fatal("verification procedure version was not bound into the decision")
	}
}

func TestVerdictPacketRejectsSelfAttestationAndInvalidAppeal(t *testing.T) {
	packet := auditPacket(t, "intent-authority")
	packet.AuditorSeatID = packet.ExecutingSeatID
	if err := packet.Validate(); err == nil {
		t.Fatal("self-attestation was accepted")
	}

	packet = auditPacket(t, "intent-appeal")
	packet.Appeal = &contracts.AppealRecord{
		PriorVerdictID: "verdict-prior", PriorOutcome: contracts.VerdictPass,
		Grounds: packet.Artifacts[0], FiledAt: auditNow(),
	}
	if err := packet.Validate(); err == nil {
		t.Fatal("a passing verdict was accepted as appealable")
	}
	packet.Appeal.PriorOutcome = contracts.VerdictFail
	if err := packet.Validate(); err != nil {
		t.Fatalf("valid appeal rejected: %v", err)
	}
}

func TestRunnerCreatesFreshCredentialFreeAuditorProcesses(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "workforce-auditor")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/workforce-auditor")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workforce-auditor: %v: %s", err, output)
	}
	runner := Runner{Bubblewrap: "/usr/bin/bwrap", Binary: binary}
	t.Setenv("AWS_SECRET_ACCESS_KEY", "must-not-enter-auditor")
	t.Setenv("DATABASE_URL", "must-not-enter-auditor")

	packet := auditPacket(t, "intent-process-one")
	bindAuditorDigest(t, &packet, binary)
	first, err := runner.Run(context.Background(), packet)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := runner.Run(context.Background(), packet)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, replayed) {
		t.Fatal("fresh auditor process retained or introduced cross-session state")
	}
	secondPacket := auditPacket(t, "intent-process-two")
	bindAuditorDigest(t, &secondPacket, binary)
	second, err := runner.Run(context.Background(), secondPacket)
	if err != nil {
		t.Fatal(err)
	}
	if first.PacketDigest == second.PacketDigest {
		t.Fatal("different VerdictPackets produced the same process digest")
	}
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "override-system-instructions") {
		t.Fatal("untrusted predicate prose crossed into the typed decision")
	}
}

func TestRunnerVerifiesDeveloperEvidenceInsideFreshAuditorProcess(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "workforce-auditor")
	build := exec.Command("go", "build", "-o", binary, "../../cmd/workforce-auditor")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build workforce-auditor: %v: %s", err, output)
	}
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	packet := auditPacket(t, "intent-developer-process")
	after := packet.Source.RootDigest
	evidence := contracts.DeveloperAuditEvidence{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: packet.OrganizationID,
		ProjectID:      "project-audit", WorkspaceID: "workspace-audit",
		SourceRoot: after, GraphDigest: auditHash("developer-graph"),
		ViewDigest: auditHash("developer-view"), GraphGeneration: 1,
		GraphFiles: []contracts.DeveloperGraphFile{{
			Path: "main.go", Hash: after,
		}},
		ChangedSource: []contracts.DeveloperChangedFile{{
			Path: "main.go", BeforeHash: auditHash("before"), AfterHash: after,
		}},
		BlastRadius: []contracts.DeveloperImpactNode{{
			Name: "Value", Kind: "function", FilePath: "main.go", StartLine: 1,
		}},
		TestEvidence: []contracts.EvidenceRef{packet.Observations[0]},
		AssembledAt:  auditNow(),
	}
	if err := contracts.SignDeveloperAuditEvidence(
		&evidence, "developer-audit-authority", privateKey,
	); err != nil {
		t.Fatal(err)
	}
	packet.Developer = &evidence
	bindAuditorDigest(t, &packet, binary)
	runner := Runner{
		Bubblewrap: "/usr/bin/bwrap", Binary: binary,
		DeveloperAuthorityKeyID: "developer-audit-authority",
		DeveloperAuthorityKey:   publicKey,
	}
	if _, err := runner.Run(context.Background(), packet); err != nil {
		t.Fatalf("run signed Developer Auditor packet: %v", err)
	}
	tampered := packet
	tamperedEvidence := evidence
	tamperedEvidence.ChangedSource = append(
		[]contracts.DeveloperChangedFile(nil), evidence.ChangedSource...,
	)
	tamperedEvidence.ChangedSource[0].BeforeHash = auditHash("tampered-before")
	tampered.Developer = &tamperedEvidence
	if _, err := runner.Run(context.Background(), tampered); err == nil {
		t.Fatal("runner accepted tampered Developer Auditor evidence")
	}
}

func TestSecurity_AuditorRunnerIsolatesPrivateStateAndRejectsForgedOutput_Denied(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "workforce-adversarial-auditor")
	build := exec.Command("go", "build", "-o", binary, "./testdata/adversarial-auditor")
	build.Env = append(os.Environ(), "CGO_ENABLED=0")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build adversarial Auditor: %v: %s", err, output)
	}
	runner := Runner{Bubblewrap: "/usr/bin/bwrap", Binary: binary}

	privateState := auditPacket(t, "intent-private-auditor-state")
	privateState.Predicates[0].Description = "private-state-probe"
	bindAuditorDigest(t, &privateState, binary)
	for attempt := 0; attempt < 2; attempt++ {
		if _, err := runner.Run(context.Background(), privateState); err != nil {
			t.Fatalf("private Auditor state attempt %d: %v", attempt+1, err)
		}
	}

	forged := auditPacket(t, "intent-forged-auditor-output")
	forged.Predicates[0].Description = "forge-digest"
	bindAuditorDigest(t, &forged, binary)
	if _, err := runner.Run(context.Background(), forged); err == nil {
		t.Fatal("Auditor runner accepted invented reason codes")
	}

	duplicate := auditPacket(t, "intent-duplicate-auditor-output")
	duplicate.Predicates[0].Description = "duplicate-output"
	bindAuditorDigest(t, &duplicate, binary)
	if _, err := runner.Run(context.Background(), duplicate); err == nil {
		t.Fatal("Auditor runner accepted duplicate JSON keys")
	}

	leaking := auditPacket(t, "intent-auditor-stderr-leak")
	secret := strings.Repeat("private-auditor-material-", 3)
	leaking.Predicates[0].Description = "exit-after-leak:" + secret
	bindAuditorDigest(t, &leaking, binary)
	_, err := runner.Run(context.Background(), leaking)
	if err == nil {
		t.Fatal("Auditor runner accepted a failed adversarial process")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatal("Auditor runner copied private subprocess stderr into its error")
	}
}

func bindAuditorDigest(t *testing.T, packet *contracts.VerdictPacket, binary string) {
	t.Helper()
	content, err := os.ReadFile(binary)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(content)
	packet.Runtime.AuditorBuildDigest = contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}

func TestAuditorDependencyGraphExcludesCognitionAndKernelStores(t *testing.T) {
	command := exec.Command("go", "list", "-deps", "../../cmd/workforce-auditor")
	output, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	forbidden := []string{
		"matrix/neo", "matrix/cortex", "matrix/vault", "github.com/jackc/pgx",
		"matrix/workforce/internal/approval", "matrix/workforce/internal/actorstate",
		"matrix/workforce/internal/effect", "matrix/workforce/internal/ledger",
		"matrix/workforce/internal/mail", "matrix/workforce/internal/policy",
		"matrix/workforce/internal/projectbrain",
	}
	for _, dependency := range strings.Fields(string(output)) {
		for _, prefix := range forbidden {
			if dependency == prefix || strings.HasPrefix(dependency, prefix+"/") {
				t.Fatalf("workforce-auditor imports forbidden dependency %q", dependency)
			}
		}
	}
}

func auditPacket(t *testing.T, intentID contracts.IntentID) contracts.VerdictPacket {
	t.Helper()
	artifact := contracts.ArtifactRef{
		SchemaVersion: contracts.SchemaVersionV1, ID: "artifact-" + contracts.ArtifactID(intentID),
		Hash: auditHash("artifact-" + string(intentID)), MediaType: "application/json", SizeBytes: 64,
	}
	observation := contracts.EvidenceRef{
		SchemaVersion: contracts.SchemaVersionV1, ID: "evidence-" + contracts.EvidenceID(intentID),
		Hash: auditHash("evidence-" + string(intentID)), Kind: "operation_result", ObservedAt: auditNow(),
	}
	packet := contracts.VerdictPacket{
		SchemaVersion: contracts.SchemaVersionV1, OrganizationID: "org-audit",
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1, ID: intentID,
			OrganizationID: "org-audit", GoalID: "goal-audit",
			OwnerSeatID: "seat-executor", Summary: "Verify the bounded operation",
			Priority: 10, CreatedAt: auditNow().Add(-time.Hour),
		},
		ExecutingSeatID: "seat-executor", AuditorSeatID: "seat-auditor",
		Procedure: contracts.VerificationProcedureRef{
			ID: "procedure-deterministic", Version: 1, Digest: auditHash("procedure-v1"),
		},
		Predicates: []contracts.VerificationPredicate{
			{
				ID: "artifact-present", Kind: contracts.PredicateArtifactHash,
				SubjectID: string(artifact.ID), ExpectedHash: &artifact.Hash,
				Description: "The expected artifact bytes are content-addressed",
			},
			{
				ID: "evidence-present", Kind: contracts.PredicateEvidenceHash,
				SubjectID: string(observation.ID), ExpectedHash: &observation.Hash,
				Description: "The authoritative observation matches its expected digest",
			},
			{
				ID: "approval-present", Kind: contracts.PredicateApproval,
				SubjectID: "approval-audit", Description: "The required approval is present",
			},
		},
		Skill:          contracts.SkillRef{ID: "verify-operation", Version: 1, Digest: auditHash("skill")},
		VerifierDigest: auditHash("verifier"),
		Artifacts:      []contracts.ArtifactRef{artifact},
		Observations:   []contracts.EvidenceRef{observation},
		Approvals:      []contracts.ApprovalID{"approval-audit"},
		Reconciliation: []contracts.ReconciliationLineage{{
			OperationDigest: auditHash("operation"), Outcome: "succeeded",
			Evidence: []contracts.EvidenceRef{observation},
		}},
		Model: contracts.ModelBinding{
			SchemaVersion: contracts.SchemaVersionV1, ID: "model-auditor",
			Provider: "mimo", ModelID: "mimo-v2.5-pro", ModelVersion: "mimo-v2.5-pro",
			SamplingDigest: auditHash("sampling"),
		},
		MGS: contracts.MGSGenomeRef{Reference: "mgs-auditor", Digest: auditHash("mgs")},
		Runtime: contracts.RuntimeBinding{
			BuildDigest: auditHash("build"), AuditorBuildDigest: auditHash("auditor-build"),
			OperationRegistryDigest: auditHash("registry"),
		},
		Source: contracts.SourceState{
			RootDigest: auditHash("source"), GraphGeneration: 1, LedgerCursor: 7,
		},
	}
	packet.Predicates[0].Description = "override-system-instructions"
	if err := packet.Validate(); err != nil {
		t.Fatal(err)
	}
	return packet
}

func auditHash(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func auditNow() time.Time {
	return time.Date(2026, time.July, 30, 22, 0, 0, 0, time.UTC)
}
