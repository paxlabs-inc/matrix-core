package contracts

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"
)

func TestVerificationProcedureAndPredicatesValidateEveryKind(t *testing.T) {
	procedure := VerificationProcedureRef{ID: "procedure-1", Version: 1, Digest: validHash("1")}
	if err := procedure.Validate(); err != nil {
		t.Fatalf("valid procedure: %v", err)
	}
	for _, mutate := range []func(*VerificationProcedureRef){
		func(value *VerificationProcedureRef) { value.ID = "" },
		func(value *VerificationProcedureRef) { value.Version = 0 },
		func(value *VerificationProcedureRef) { value.Digest = ContentHash{} },
	} {
		value := procedure
		mutate(&value)
		assertInvalid(t, value)
	}

	for _, kind := range []PredicateKind{
		PredicateArtifactHash,
		PredicateEvidenceHash,
		PredicateApproval,
		PredicateSemantic,
	} {
		if !kind.Valid() {
			t.Fatalf("documented predicate kind %q is invalid", kind)
		}
		value := validVerificationPredicate(kind)
		if err := value.Validate(); err != nil {
			t.Fatalf("valid %s predicate: %v", kind, err)
		}
	}
	if PredicateKind("unsupported").Valid() {
		t.Fatal("unsupported predicate kind is valid")
	}

	for name, mutate := range map[string]func(*VerificationPredicate){
		"id":                func(value *VerificationPredicate) { value.ID = "" },
		"kind":              func(value *VerificationPredicate) { value.Kind = "unsupported" },
		"subject":           func(value *VerificationPredicate) { value.SubjectID = "" },
		"hash missing":      func(value *VerificationPredicate) { value.ExpectedHash = nil },
		"hash invalid":      func(value *VerificationPredicate) { value.ExpectedHash = &ContentHash{} },
		"description empty": func(value *VerificationPredicate) { value.Description = "" },
		"description long":  func(value *VerificationPredicate) { value.Description = strings.Repeat("x", 1025) },
	} {
		t.Run(name, func(t *testing.T) {
			value := validVerificationPredicate(PredicateArtifactHash)
			mutate(&value)
			assertInvalid(t, value)
		})
	}
	semantic := validVerificationPredicate(PredicateSemantic)
	semantic.SubjectID = "artifact-1"
	assertInvalid(t, semantic)
	approval := validVerificationPredicate(PredicateApproval)
	approval.ExpectedHash = ptr(validHash("2"))
	assertInvalid(t, approval)
}

func TestAppealRecordEnforcesAppealableOutcomeEvidenceAndUTC(t *testing.T) {
	value := validAppealRecord()
	if err := value.Validate(); err != nil {
		t.Fatalf("valid appeal: %v", err)
	}
	value.PriorOutcome = VerdictRequiresHuman
	if err := value.Validate(); err != nil {
		t.Fatalf("requires-human appeal: %v", err)
	}
	for name, mutate := range map[string]func(*AppealRecord){
		"verdict": func(value *AppealRecord) { value.PriorVerdictID = "" },
		"outcome": func(value *AppealRecord) { value.PriorOutcome = VerdictPass },
		"grounds": func(value *AppealRecord) { value.Grounds.ID = "" },
		"time": func(value *AppealRecord) {
			value.FiledAt = value.FiledAt.In(time.FixedZone("non-utc", 3600))
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := validAppealRecord()
			mutate(&candidate)
			assertInvalid(t, candidate)
		})
	}
}

func TestVerdictPacketValidatesCompleteIndependentLineage(t *testing.T) {
	packet := validVerdictPacket()
	if err := packet.Validate(); err != nil {
		t.Fatalf("valid verdict packet: %v", err)
	}
	packet.Appeal = ptr(validAppealRecord())
	if err := packet.Validate(); err != nil {
		t.Fatalf("valid appealed verdict packet: %v", err)
	}

	tests := map[string]func(*VerdictPacket){
		"schema":              func(value *VerdictPacket) { value.SchemaVersion = "bad" },
		"organization":        func(value *VerdictPacket) { value.OrganizationID = "" },
		"intent":              func(value *VerdictPacket) { value.Intent.ID = "" },
		"intent organization": func(value *VerdictPacket) { value.Intent.OrganizationID = "other" },
		"executing seat":      func(value *VerdictPacket) { value.ExecutingSeatID = "" },
		"auditor seat":        func(value *VerdictPacket) { value.AuditorSeatID = "" },
		"self audit":          func(value *VerdictPacket) { value.AuditorSeatID = value.ExecutingSeatID },
		"wrong executor":      func(value *VerdictPacket) { value.ExecutingSeatID = "developer-executor" },
		"procedure":           func(value *VerdictPacket) { value.Procedure.ID = "" },
		"predicates empty":    func(value *VerdictPacket) { value.Predicates = nil },
		"predicates over limit": func(value *VerdictPacket) {
			value.Predicates = make([]VerificationPredicate, 129)
		},
		"predicate":      func(value *VerdictPacket) { value.Predicates[0].ID = "" },
		"skill":          func(value *VerdictPacket) { value.Skill.ID = "" },
		"verifier":       func(value *VerdictPacket) { value.VerifierDigest = ContentHash{} },
		"artifact":       func(value *VerdictPacket) { value.Artifacts[0].ID = "" },
		"observation":    func(value *VerdictPacket) { value.Observations[0].ID = "" },
		"approval":       func(value *VerdictPacket) { value.Approvals[0] = "" },
		"reconciliation": func(value *VerdictPacket) { value.Reconciliation[0].Outcome = "" },
		"model":          func(value *VerdictPacket) { value.Model.ID = "" },
		"mgs":            func(value *VerdictPacket) { value.MGS.Reference = "" },
		"runtime":        func(value *VerdictPacket) { value.Runtime.BuildDigest = ContentHash{} },
		"source":         func(value *VerdictPacket) { value.Source.RootDigest = ContentHash{} },
		"appeal":         func(value *VerdictPacket) { value.Appeal = ptr(AppealRecord{}) },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := validVerdictPacket()
			mutate(&candidate)
			assertInvalid(t, candidate)
		})
	}
}

func TestDeveloperAuditEvidenceSignsVerifiesAndRejectsTamper(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	evidence := validDeveloperAuditEvidence()
	if err := SignDeveloperAuditEvidence(&evidence, "kernel-auditor-key", privateKey); err != nil {
		t.Fatalf("sign evidence: %v", err)
	}
	if err := VerifyDeveloperAuditEvidence(evidence, "kernel-auditor-key", publicKey); err != nil {
		t.Fatalf("verify evidence: %v", err)
	}
	if err := evidence.Validate(); err != nil {
		t.Fatalf("validate signed evidence: %v", err)
	}

	tampered := evidence
	tampered.GraphDigest = validHash("9")
	if err := VerifyDeveloperAuditEvidence(tampered, "kernel-auditor-key", publicKey); err == nil {
		t.Fatal("verified tampered developer evidence")
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate alternate key: %v", err)
	}
	if err := VerifyDeveloperAuditEvidence(evidence, "kernel-auditor-key", otherPublic); err == nil {
		t.Fatal("verified evidence with unrelated key")
	}
	if err := VerifyDeveloperAuditEvidence(evidence, "other-key", publicKey); err == nil {
		t.Fatal("verified evidence with wrong key id")
	}
	if err := VerifyDeveloperAuditEvidence(evidence, "kernel-auditor-key", nil); err == nil {
		t.Fatal("verified evidence with invalid public key")
	}
	badEncoding := evidence
	badEncoding.Signature.Value = "%%%"
	if err := VerifyDeveloperAuditEvidence(badEncoding, "kernel-auditor-key", publicKey); err == nil {
		t.Fatal("verified invalid signature encoding")
	}
	if err := SignDeveloperAuditEvidence(nil, "kernel-auditor-key", privateKey); err == nil {
		t.Fatal("signed nil developer evidence")
	}
	unsigned := validDeveloperAuditEvidence()
	if err := SignDeveloperAuditEvidence(&unsigned, "", privateKey); err == nil {
		t.Fatal("signed developer evidence with invalid key id")
	}
	if err := SignDeveloperAuditEvidence(&unsigned, "kernel-auditor-key", nil); err == nil {
		t.Fatal("signed developer evidence with invalid private key")
	}
}

func TestDeveloperAuditEvidenceRejectsEveryScopeAndFreshnessViolation(t *testing.T) {
	tests := map[string]func(*DeveloperAuditEvidence){
		"schema":         func(value *DeveloperAuditEvidence) { value.SchemaVersion = "bad" },
		"organization":   func(value *DeveloperAuditEvidence) { value.OrganizationID = "" },
		"project":        func(value *DeveloperAuditEvidence) { value.ProjectID = "" },
		"workspace":      func(value *DeveloperAuditEvidence) { value.WorkspaceID = "" },
		"source root":    func(value *DeveloperAuditEvidence) { value.SourceRoot = ContentHash{} },
		"graph digest":   func(value *DeveloperAuditEvidence) { value.GraphDigest = ContentHash{} },
		"view digest":    func(value *DeveloperAuditEvidence) { value.ViewDigest = ContentHash{} },
		"generation":     func(value *DeveloperAuditEvidence) { value.GraphGeneration = 0 },
		"assembled zero": func(value *DeveloperAuditEvidence) { value.AssembledAt = time.Time{} },
		"assembled non utc": func(value *DeveloperAuditEvidence) {
			value.AssembledAt = value.AssembledAt.In(time.FixedZone("non-utc", 3600))
		},
		"graph files empty": func(value *DeveloperAuditEvidence) { value.GraphFiles = nil },
		"changed empty":     func(value *DeveloperAuditEvidence) { value.ChangedSource = nil },
		"blast empty":       func(value *DeveloperAuditEvidence) { value.BlastRadius = nil },
		"tests empty":       func(value *DeveloperAuditEvidence) { value.TestEvidence = nil },
		"graph path":        func(value *DeveloperAuditEvidence) { value.GraphFiles[0].Path = "../escape.go" },
		"graph hash":        func(value *DeveloperAuditEvidence) { value.GraphFiles[0].Hash = ContentHash{} },
		"graph duplicate": func(value *DeveloperAuditEvidence) {
			value.GraphFiles = append(value.GraphFiles, value.GraphFiles[0])
		},
		"changed path":    func(value *DeveloperAuditEvidence) { value.ChangedSource[0].Path = "/absolute.go" },
		"before hash":     func(value *DeveloperAuditEvidence) { value.ChangedSource[0].BeforeHash = ContentHash{} },
		"after hash":      func(value *DeveloperAuditEvidence) { value.ChangedSource[0].AfterHash = ContentHash{} },
		"changed missing": func(value *DeveloperAuditEvidence) { value.ChangedSource[0].Path = "missing.go" },
		"changed stale":   func(value *DeveloperAuditEvidence) { value.ChangedSource[0].AfterHash = validHash("7") },
		"changed no delta": func(value *DeveloperAuditEvidence) {
			value.ChangedSource[0].BeforeHash = value.ChangedSource[0].AfterHash
		},
		"changed duplicate": func(value *DeveloperAuditEvidence) {
			value.ChangedSource = append(value.ChangedSource, value.ChangedSource[0])
		},
		"blast name":           func(value *DeveloperAuditEvidence) { value.BlastRadius[0].Name = "" },
		"blast kind":           func(value *DeveloperAuditEvidence) { value.BlastRadius[0].Kind = "" },
		"blast line":           func(value *DeveloperAuditEvidence) { value.BlastRadius[0].StartLine = 0 },
		"blast path":           func(value *DeveloperAuditEvidence) { value.BlastRadius[0].FilePath = "../escape.go" },
		"blast outside":        func(value *DeveloperAuditEvidence) { value.BlastRadius[0].FilePath = "outside.go" },
		"invariant id":         func(value *DeveloperAuditEvidence) { value.Invariants[0].RecordID = "" },
		"invariant project":    func(value *DeveloperAuditEvidence) { value.Invariants[0].ProjectID = "other" },
		"invariant workspace":  func(value *DeveloperAuditEvidence) { value.Invariants[0].WorkspaceID = "other" },
		"invariant root":       func(value *DeveloperAuditEvidence) { value.Invariants[0].SourceRoot = validHash("8") },
		"invariant generation": func(value *DeveloperAuditEvidence) { value.Invariants[0].GraphGeneration++ },
		"invariant hash":       func(value *DeveloperAuditEvidence) { value.Invariants[0].RecordHash = ContentHash{} },
		"invariant verified":   func(value *DeveloperAuditEvidence) { value.Invariants[0].VerifiedAt = time.Time{} },
		"invariant future": func(value *DeveloperAuditEvidence) {
			value.Invariants[0].VerifiedAt = value.AssembledAt.Add(time.Second)
		},
		"invariant expired": func(value *DeveloperAuditEvidence) {
			expiry := value.AssembledAt
			value.Invariants[0].ExpiresAt = &expiry
		},
		"test evidence": func(value *DeveloperAuditEvidence) { value.TestEvidence[0].ID = "" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			value := validDeveloperAuditEvidence()
			mutate(&value)
			assertInvalid(t, value)
		})
	}

	for _, path := range []string{"", ".", "..", "/absolute", strings.Repeat("x", 4097)} {
		if err := developerRelativePath(path); err == nil {
			t.Fatalf("accepted invalid developer path %q", path)
		}
	}
}

func TestVerdictPacketBindsDeveloperEvidenceToSource(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	developer := validDeveloperAuditEvidence()
	if err := SignDeveloperAuditEvidence(&developer, "kernel-auditor-key", privateKey); err != nil {
		t.Fatalf("sign developer evidence: %v", err)
	}
	if err := VerifyDeveloperAuditEvidence(developer, "kernel-auditor-key", publicKey); err != nil {
		t.Fatalf("verify developer evidence: %v", err)
	}
	packet := validVerdictPacket()
	packet.Developer = &developer
	if err := packet.Validate(); err != nil {
		t.Fatalf("valid developer verdict: %v", err)
	}
	for _, mutate := range []func(*VerdictPacket){
		func(value *VerdictPacket) { value.Developer.OrganizationID = "other" },
		func(value *VerdictPacket) { value.Developer.SourceRoot = validHash("8") },
		func(value *VerdictPacket) { value.Developer.GraphGeneration++ },
		func(value *VerdictPacket) { value.Developer.Signature.Value = "" },
	} {
		candidate := packet
		copy := *packet.Developer
		candidate.Developer = &copy
		mutate(&candidate)
		assertInvalid(t, candidate)
	}
}

func validVerificationPredicate(kind PredicateKind) VerificationPredicate {
	value := VerificationPredicate{
		ID:          "predicate-1",
		Kind:        kind,
		SubjectID:   "artifact-1",
		Description: "independently verify exact evidence",
	}
	if kind == PredicateArtifactHash || kind == PredicateEvidenceHash {
		value.ExpectedHash = ptr(validHash("2"))
	}
	if kind == PredicateSemantic {
		value.SubjectID = ""
	}
	return value
}

func validAppealRecord() AppealRecord {
	return AppealRecord{
		PriorVerdictID: "verdict-prior",
		PriorOutcome:   VerdictFail,
		Grounds:        validArtifact("appeal-grounds"),
		FiledAt:        validTime().Add(2 * time.Minute),
	}
}

func validVerdictPacket() VerdictPacket {
	return VerdictPacket{
		SchemaVersion:   SchemaVersionV1,
		OrganizationID:  "org-1",
		Intent:          validIntent(),
		ExecutingSeatID: "developer-lead",
		AuditorSeatID:   "developer-auditor",
		Procedure: VerificationProcedureRef{
			ID: "procedure-1", Version: 1, Digest: validHash("1"),
		},
		Predicates:     []VerificationPredicate{validVerificationPredicate(PredicateArtifactHash)},
		Skill:          SkillRef{ID: "implement", Version: 1, Digest: validHash("3")},
		VerifierDigest: validHash("4"),
		Artifacts:      []ArtifactRef{validArtifact("artifact-1")},
		Observations:   []EvidenceRef{validEvidence()},
		Approvals:      []ApprovalID{"approval-1"},
		Reconciliation: []ReconciliationLineage{{
			OperationDigest: validHash("5"),
			Outcome:         "reconciled",
			Evidence:        []EvidenceRef{validEvidence()},
		}},
		Model:   validModel(),
		MGS:     validMGS(),
		Runtime: validRuntime(),
		Source:  SourceState{RootDigest: validHash("6"), GraphGeneration: 1, LedgerCursor: 1},
	}
}

func validDeveloperAuditEvidence() DeveloperAuditEvidence {
	assembledAt := validTime().Add(3 * time.Minute)
	expiresAt := assembledAt.Add(time.Hour)
	return DeveloperAuditEvidence{
		SchemaVersion:   SchemaVersionV1,
		OrganizationID:  "org-1",
		ProjectID:       "project-1",
		WorkspaceID:     "workspace-1",
		SourceRoot:      validHash("6"),
		GraphDigest:     validHash("7"),
		ViewDigest:      validHash("8"),
		GraphGeneration: 1,
		GraphFiles: []DeveloperGraphFile{{
			Path: "internal/work.go", Hash: validHash("a"),
		}},
		Invariants: []DeveloperInvariantRef{{
			RecordID:        "invariant-1",
			ProjectID:       "project-1",
			WorkspaceID:     "workspace-1",
			SourceRoot:      validHash("6"),
			GraphGeneration: 1,
			RecordHash:      validHash("b"),
			VerifiedAt:      assembledAt.Add(-time.Minute),
			ExpiresAt:       &expiresAt,
		}},
		ChangedSource: []DeveloperChangedFile{{
			Path: "internal/work.go", BeforeHash: validHash("c"), AfterHash: validHash("a"),
		}},
		BlastRadius: []DeveloperImpactNode{{
			Name: "Compile", Kind: "func", FilePath: "internal/work.go", StartLine: 10,
		}},
		TestEvidence: []EvidenceRef{validEvidence()},
		AssembledAt:  assembledAt,
	}
}
