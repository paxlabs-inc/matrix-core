package developer

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"path/filepath"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/projectbrain"
)

func TestAuditPacketIsClosedStatelessAndContentAddressed(t *testing.T) {
	now := time.Now().UTC()
	before := testHash("before")
	after := testHash("after")
	packet := AuditPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:developer", WorkspaceID: "workspace:developer",
		ViewDigest: testHash("view"), AssembledAt: now,
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "intent:developer", OrganizationID: "organization:developer",
			GoalID: "goal:developer", OwnerSeatID: "seat:executor",
			Summary: "verify a bounded real source change", CreatedAt: now,
		},
		Graph: projectbrain.GraphSnapshot{
			SchemaVersion: contracts.SchemaVersionV1,
			RootDigest:    testHash("root"), GraphDigest: testHash("graph"),
			Generation: 1, IndexedAt: now.Add(-time.Minute), CapturedAt: now,
			Fresh: true,
			Files: []projectbrain.GraphFile{{
				Path: "main.go", Language: "go", NodeCount: 1,
				SizeBytes: 12, Hash: after, Indexed: true,
			}},
			NodeCount: 1,
		},
		ChangedSource: []ChangedFile{{
			Path: "main.go", BeforeHash: before, AfterHash: after,
		}},
		BlastRadius: []projectbrain.ImpactNode{{
			Name: "Value", Kind: "function", FilePath: "main.go", StartLine: 3,
		}},
		TestEvidence: []contracts.EvidenceRef{{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:test", Hash: testHash("test"),
			Kind: "test", ObservedAt: now,
		}},
		Verifier: contracts.VerificationProcedureRef{
			ID: "developer-auditor.v1", Version: 1, Digest: testHash("verifier"),
		},
	}
	first, err := packet.Digest()
	if err != nil {
		t.Fatal(err)
	}
	second, err := packet.Digest()
	if err != nil || first != second {
		t.Fatalf("stateless digest first=%#v second=%#v err=%v", first, second, err)
	}
	packet.ChangedSource[0].AfterHash = before
	if err := packet.Validate(); err == nil {
		t.Fatal("auditor accepted unchanged source as implementation evidence")
	}
}

func TestScopeValidationRejectsAuthorityWideningAndStaleEvidence(t *testing.T) {
	now := time.Now().UTC()
	request := ScopeRequest{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:test", WorkspaceID: "workspace:test",
		TaskNodeID: "task:test", WorkspaceRoot: t.TempDir(),
		Files: []string{"main.go"}, Symbols: []string{"Value"},
	}
	request.Capability = testScopeCapability(request, now)
	requestCases := []func(*ScopeRequest){
		func(value *ScopeRequest) { value.SchemaVersion = "v2" },
		func(value *ScopeRequest) { value.ProjectID = "" },
		func(value *ScopeRequest) { value.WorkspaceRoot = "relative" },
		func(value *ScopeRequest) { value.Files = nil },
		func(value *ScopeRequest) { value.Files = []string{"main.go", "main.go"} },
		func(value *ScopeRequest) { value.Symbols = []string{"Value", "Value"} },
		func(value *ScopeRequest) {
			invalid := projectbrain.RecordID("bad plan")
			value.CoordinationPlanID = &invalid
		},
	}
	for index, mutate := range requestCases {
		invalid := request
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid scope request case %d accepted", index)
		}
	}
	fileHash := testHash("file")
	resolved := ResolvedScope{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:test", WorkspaceID: "workspace:test",
		TaskNodeID: "task:test", WorkspaceRoot: request.WorkspaceRoot,
		Source: projectbrain.GraphSnapshot{
			SchemaVersion: contracts.SchemaVersionV1,
			RootDigest:    testHash("root"), GraphDigest: testHash("graph"),
			Generation: 1, IndexedAt: now.Add(-time.Minute), CapturedAt: now,
			Fresh: true,
			Files: []projectbrain.GraphFile{{
				Path: "main.go", Language: "go", NodeCount: 1,
				SizeBytes: 12, Hash: fileHash, Indexed: true,
			}},
			NodeCount: 1,
		},
		Files: []projectbrain.GraphFile{{
			Path: "main.go", Language: "go", NodeCount: 1,
			SizeBytes: 12, Hash: fileHash, Indexed: true,
		}},
		Symbols: []string{"Value"},
		BlastRadius: []projectbrain.ImpactNode{{
			Name: "Value", Kind: "function", FilePath: "main.go", StartLine: 1,
		}},
		ResolvedAt: now,
		Capability: request.Capability,
	}
	if err := resolved.Validate(); err != nil {
		t.Fatalf("valid resolved scope: %v", err)
	}
	resolvedCases := []func(*ResolvedScope){
		func(value *ResolvedScope) { value.SchemaVersion = "v2" },
		func(value *ResolvedScope) { value.WorkspaceID = "" },
		func(value *ResolvedScope) { value.WorkspaceRoot = "relative" },
		func(value *ResolvedScope) { value.Source.Generation = 0 },
		func(value *ResolvedScope) { value.Files = nil },
		func(value *ResolvedScope) { value.Files[0].Hash = testHash("stale") },
		func(value *ResolvedScope) { value.Symbols = []string{"Value", "Value"} },
		func(value *ResolvedScope) { value.BlastRadius[0].StartLine = 0 },
		func(value *ResolvedScope) { value.ResolvedAt = time.Time{} },
	}
	for index, mutate := range resolvedCases {
		invalid := resolved
		invalid.Files = append([]projectbrain.GraphFile(nil), resolved.Files...)
		invalid.Symbols = append([]string(nil), resolved.Symbols...)
		invalid.BlastRadius = append(
			[]projectbrain.ImpactNode(nil), resolved.BlastRadius...,
		)
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid resolved scope case %d accepted", index)
		}
	}
}

func testScopeCapability(scope ScopeRequest, now time.Time) projectbrain.CapabilityGrant {
	return projectbrain.CapabilityGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "grant:test", TenantID: "tenant:test",
		OrganizationID: "organization:test",
		ProjectID:      scope.ProjectID, WorkspaceID: scope.WorkspaceID,
		WorkspaceRoot:           scope.WorkspaceRoot,
		Operation:               projectbrain.CapabilityChangeScope,
		RequesterSeatID:         "seat:test",
		RequesterSeatVersion:    1,
		RequesterSeatDID:        "did:matrix:seat:test",
		RequesterBindingID:      "binding:seat:test",
		RequesterBindingVersion: 1,
		Purpose:                 "developer_change_scope:" + string(scope.TaskNodeID),
		IssuedAt:                now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour),
		Signature: contracts.Signature{
			Algorithm: "ed25519", KeyID: "kernel",
			Value: base64.RawURLEncoding.EncodeToString(make([]byte, 64)),
		},
	}
}

func TestAuditPacketRejectsMemoryAndEvidenceBoundaryViolations(t *testing.T) {
	now := time.Now().UTC()
	packet := AuditPacket{
		SchemaVersion: contracts.SchemaVersionV1,
		ProjectID:     "project:audit", WorkspaceID: "workspace:audit",
		ViewDigest: testHash("view"), AssembledAt: now,
		Intent: contracts.Intent{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "intent:audit", OrganizationID: "organization:audit",
			GoalID: "goal:audit", OwnerSeatID: "seat:executor",
			Summary: "audit current source only", CreatedAt: now,
		},
		Graph: projectbrain.GraphSnapshot{
			SchemaVersion: contracts.SchemaVersionV1,
			RootDigest:    testHash("root"), GraphDigest: testHash("graph"),
			Generation: 1, IndexedAt: now.Add(-time.Minute), CapturedAt: now,
			Fresh: true,
			Files: []projectbrain.GraphFile{{
				Path: "main.go", Hash: testHash("after"), Indexed: true,
			}},
		},
		ChangedSource: []ChangedFile{{
			Path: "main.go", BeforeHash: testHash("before"), AfterHash: testHash("after"),
		}},
		BlastRadius: []projectbrain.ImpactNode{{
			Name: "Value", Kind: "function", FilePath: "main.go", StartLine: 1,
		}},
		TestEvidence: []contracts.EvidenceRef{{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            "evidence:audit", Hash: testHash("evidence"),
			Kind: "test", ObservedAt: now,
		}},
		Verifier: contracts.VerificationProcedureRef{
			ID: "developer-auditor.v1", Version: 1, Digest: testHash("verifier"),
		},
	}
	cases := []func(*AuditPacket){
		func(value *AuditPacket) { value.SchemaVersion = "v2" },
		func(value *AuditPacket) { value.Intent.ID = "" },
		func(value *AuditPacket) { value.Graph.Generation = 0 },
		func(value *AuditPacket) { value.ProjectID = "" },
		func(value *AuditPacket) { value.ViewDigest = contracts.ContentHash{} },
		func(value *AuditPacket) { value.ChangedSource = nil },
		func(value *AuditPacket) { value.ChangedSource[0].Path = filepath.Join("..", "secret") },
		func(value *AuditPacket) { value.ChangedSource[0].BeforeHash = contracts.ContentHash{} },
		func(value *AuditPacket) { value.TestEvidence[0] = contracts.EvidenceRef{} },
		func(value *AuditPacket) { value.Verifier.Version = 0 },
		func(value *AuditPacket) {
			value.Invariants = []projectbrain.EngineeringRecord{{
				Proposal: projectbrain.Proposal{Kind: projectbrain.KindDecision},
			}}
		},
	}
	for index, mutate := range cases {
		invalid := packet
		invalid.ChangedSource = append([]ChangedFile(nil), packet.ChangedSource...)
		invalid.TestEvidence = append(
			[]contracts.EvidenceRef(nil), packet.TestEvidence...,
		)
		mutate(&invalid)
		if err := invalid.Validate(); err == nil {
			t.Fatalf("invalid audit packet case %d accepted", index)
		}
	}
}

func TestScopeHelpersAreDeterministicAndBounded(t *testing.T) {
	values := sortedUnique([]string{"b", "a", "b"})
	if len(values) != 2 || values[0] != "a" || values[1] != "b" {
		t.Fatalf("sorted unique = %#v", values)
	}
	if err := relativePath("/absolute"); err == nil {
		t.Fatal("absolute scope path accepted")
	}
	if err := token("task", "bad value"); err == nil {
		t.Fatal("invalid task token accepted")
	}
	if err := uniqueBounded(
		"file", []string{"a.go", "a.go"}, true,
	); err == nil {
		t.Fatal("duplicate file claims accepted")
	}
}

func TestScopeClaimsUnifyDirectAndBlastRadiusResources(t *testing.T) {
	base := ResolvedScope{
		ProjectID: "project:test", WorkspaceID: "workspace:test",
		TaskNodeID: "task:first",
		Files:      []projectbrain.GraphFile{{Path: "a.go"}},
		Symbols:    []string{"Alpha"},
		BlastRadius: []projectbrain.ImpactNode{{
			Name: "Beta", Kind: "function", FilePath: "b.go", StartLine: 1,
		}},
		AffectedTests: []string{"b_test.go"},
	}
	other := base
	other.TaskNodeID = "task:second"
	other.Files = []projectbrain.GraphFile{{Path: "b.go"}}
	other.Symbols = []string{"Beta"}
	rightResources := make(map[string]bool)
	for _, value := range scopeClaims(other) {
		rightResources[value.resource] = true
	}
	var fileConflict, symbolConflict bool
	for _, value := range scopeClaims(base) {
		if !rightResources[value.resource] {
			continue
		}
		fileConflict = fileConflict || value.kind == "file"
		symbolConflict = symbolConflict || value.kind == "symbol"
	}
	if !fileConflict || !symbolConflict {
		t.Fatalf(
			"cross-kind claims did not converge file=%v symbol=%v",
			fileConflict, symbolConflict,
		)
	}
}

func testHash(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{
		Algorithm: "sha256", Digest: hex.EncodeToString(sum[:]),
	}
}
