package projectbrain

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"centra/workforce/internal/contracts"
)

func TestValidation_RejectsMalformedProjectBrainBoundaries(t *testing.T) {
	record, _, _, _, _ := signedRecord(t, "validation-table")
	tests := []struct {
		name   string
		mutate func(*EngineeringRecord)
	}{
		{"proposal schema", func(value *EngineeringRecord) { value.Proposal.SchemaVersion = "v2" }},
		{"record id", func(value *EngineeringRecord) { value.Proposal.ID = "" }},
		{"kind", func(value *EngineeringRecord) { value.Proposal.Kind = "raw_reasoning" }},
		{"origin", func(value *EngineeringRecord) { value.Proposal.Origin = "transcript" }},
		{"zero version", func(value *EngineeringRecord) { value.Proposal.Version = 0 }},
		{"empty summary", func(value *EngineeringRecord) { value.Proposal.Content.Summary = "" }},
		{"no claims", func(value *EngineeringRecord) { value.Proposal.Content.Claims = nil }},
		{"ungrounded claim", func(value *EngineeringRecord) {
			value.Proposal.Content.Claims[0].Files = nil
		}},
		{"absolute source", func(value *EngineeringRecord) {
			value.Proposal.Content.Claims[0].Files[0].Path = "/private/source.go"
		}},
		{"invalid lines", func(value *EngineeringRecord) {
			value.Proposal.Content.Claims[0].Files[0].StartLine = 0
		}},
		{"two replacement modes", func(value *EngineeringRecord) {
			parent := RecordID("parent")
			value.Proposal.Supersedes = &parent
			value.Proposal.Corrects = &parent
		}},
		{"correction without parent", func(value *EngineeringRecord) {
			value.Proposal.Kind = KindCorrection
		}},
		{"non-correction corrects", func(value *EngineeringRecord) {
			parent := RecordID("parent")
			value.Proposal.Corrects = &parent
		}},
		{"non UTC created", func(value *EngineeringRecord) {
			value.Proposal.CreatedAt = value.Proposal.CreatedAt.In(time.FixedZone("other", 3600))
		}},
		{"verification schema", func(value *EngineeringRecord) {
			value.Verification.SchemaVersion = "v2"
		}},
		{"verification rejected", func(value *EngineeringRecord) {
			value.Verification.Accepted = false
		}},
		{"verification procedure", func(value *EngineeringRecord) {
			value.Verification.Procedure = ""
		}},
		{"verification evidence", func(value *EngineeringRecord) {
			value.Verification.Evidence = nil
		}},
		{"verification time", func(value *EngineeringRecord) {
			value.Verification.VerifiedAt = time.Time{}
		}},
		{"verification record", func(value *EngineeringRecord) {
			value.Verification.RecordID = "other"
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := record
			invalid.Proposal.Content.Claims = append([]Claim(nil), record.Proposal.Content.Claims...)
			invalid.Proposal.Content.Claims[0].Files =
				append([]FileEvidence(nil), record.Proposal.Content.Claims[0].Files...)
			invalid.Verification.Evidence =
				append([]contracts.EvidenceRef(nil), record.Verification.Evidence...)
			test.mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("invalid %s accepted", test.name)
			}
		})
	}
}

func TestValidation_RejectsOversizedAndCorruptEvidence(t *testing.T) {
	record, _, _, _, _ := signedRecord(t, "evidence-bounds")
	claim := record.Proposal.Content.Claims[0]
	invalidClaim := claim
	invalidClaim.Statement = strings.Repeat("x", 4097)
	if err := invalidClaim.validate(); err == nil {
		t.Fatal("claim accepted oversized statement")
	}
	invalidClaim = claim
	invalidClaim.Files = make([]FileEvidence, 129)
	if err := invalidClaim.validate(); err == nil {
		t.Fatal("claim accepted excessive file evidence")
	}
	invalidClaim = claim
	invalidClaim.Files[0].Hash = contracts.ContentHash{}
	if err := invalidClaim.validate(); err == nil {
		t.Fatal("claim accepted invalid file hash")
	}
	invalidClaim = claim
	invalidClaim.Files = nil
	invalidClaim.Evidence = []contracts.EvidenceRef{{}}
	if err := invalidClaim.validate(); err == nil {
		t.Fatal("claim accepted invalid authoritative evidence")
	}

	content := record.Proposal.Content
	content.Summary = strings.Repeat("x", 8193)
	if err := content.validate(); err == nil {
		t.Fatal("content accepted oversized summary")
	}
	content = record.Proposal.Content
	content.Claims = make([]Claim, 257)
	if err := content.validate(); err == nil {
		t.Fatal("content accepted excessive claims")
	}
	content = record.Proposal.Content
	content.Artifacts = []contracts.ArtifactRef{{}}
	if err := content.validate(); err == nil {
		t.Fatal("content accepted invalid artifact")
	}
	content = record.Proposal.Content
	nonUTC := projectBrainNow().In(time.FixedZone("other", 3600))
	content.ExpiresAt = &nonUTC
	if err := content.validate(); err == nil {
		t.Fatal("content accepted non-UTC expiry")
	}

	verification := record.Verification
	verification.ProposalHash = contracts.ContentHash{}
	if err := verification.Validate(); err == nil {
		t.Fatal("verification accepted invalid proposal hash")
	}
	verification = record.Verification
	verification.VerifierSeatID = ""
	if err := verification.Validate(); err == nil {
		t.Fatal("verification accepted empty verifier")
	}
	verification = record.Verification
	verification.Evidence[0] = contracts.EvidenceRef{}
	if err := verification.Validate(); err == nil {
		t.Fatal("verification accepted malformed evidence")
	}
}

func TestCapabilityGrant_RejectsAuthorityWidening(t *testing.T) {
	_, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	record, _, _, authorPublic, verifierPublic := signedRecord(t, "grant")
	root := t.TempDir()
	valid := integrationWriteGrant(
		t, record, root, authorityPrivate, authorPublic, verifierPublic,
	)
	tests := []struct {
		name   string
		mutate func(*CapabilityGrant)
	}{
		{"schema", func(value *CapabilityGrant) { value.SchemaVersion = "v2" }},
		{"grant id", func(value *CapabilityGrant) { value.ID = "" }},
		{"tenant id", func(value *CapabilityGrant) { value.TenantID = "" }},
		{"requester", func(value *CapabilityGrant) { value.RequesterSeatID = "" }},
		{"requester version", func(value *CapabilityGrant) { value.RequesterSeatVersion = 0 }},
		{"requester did", func(value *CapabilityGrant) { value.RequesterSeatDID = "" }},
		{"requester binding", func(value *CapabilityGrant) { value.RequesterBindingID = "" }},
		{"requester binding version", func(value *CapabilityGrant) {
			value.RequesterBindingVersion = 0
		}},
		{"workspace", func(value *CapabilityGrant) { value.WorkspaceRoot = "relative" }},
		{"filter", func(value *CapabilityGrant) { value.Filter = "../escape" }},
		{"purpose", func(value *CapabilityGrant) { value.Purpose = "" }},
		{"times", func(value *CapabilityGrant) { value.ExpiresAt = value.IssuedAt }},
		{"operation", func(value *CapabilityGrant) { value.Operation = "admin" }},
		{"missing record", func(value *CapabilityGrant) { value.RecordID = nil }},
		{"write page size", func(value *CapabilityGrant) { value.MaxRecords = 1 }},
		{"write cursor", func(value *CapabilityGrant) {
			cursor := RecordID("cursor")
			value.AfterRecordID = &cursor
		}},
		{"same seat", func(value *CapabilityGrant) {
			value.Verifier.SeatID = value.Author.SeatID
		}},
		{"bad author seat", func(value *CapabilityGrant) { value.Author.SeatID = "" }},
		{"bad author key", func(value *CapabilityGrant) { value.Author.PublicKey = "bad" }},
		{"bad verifier key", func(value *CapabilityGrant) { value.Verifier.PublicKey = "bad" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := valid
			author := *valid.Author
			verifier := *valid.Verifier
			invalid.Author, invalid.Verifier = &author, &verifier
			test.mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("invalid capability %s accepted", test.name)
			}
		})
	}
	read := CapabilityGrant{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            "read-grant", TenantID: "tenant-grant",
		OrganizationID: record.Proposal.OrganizationID,
		ProjectID:      record.Proposal.ProjectID, WorkspaceID: record.Proposal.WorkspaceID,
		WorkspaceRoot: root, Operation: CapabilityRead, RequesterSeatID: "reader",
		RequesterSeatVersion:    1,
		RequesterSeatDID:        "did:matrix:reader",
		RequesterBindingID:      "binding:reader",
		RequesterBindingVersion: 1,
		Purpose:                 "read", MaxRecords: 10,
		IssuedAt: projectBrainNow(), ExpiresAt: projectBrainNow().Add(time.Hour),
	}
	if err := SignCapabilityGrant(&read, "kernel", authorityPrivate); err != nil {
		t.Fatal(err)
	}
	read.MaxRecords = 0
	if err := read.Validate(); err == nil {
		t.Fatal("read capability accepted zero page bound")
	}
	read.MaxRecords = 1025
	if err := read.Validate(); err == nil {
		t.Fatal("read capability accepted excessive page bound")
	}
	read.MaxRecords = 10
	read.Author = valid.Author
	if err := read.Validate(); err == nil {
		t.Fatal("read capability accepted write authority")
	}
}

func TestViewValidation_RejectsUnboundedOrInvalidProjection(t *testing.T) {
	record, _, _, _, _ := signedRecord(t, "view-validation")
	view := View{
		SchemaVersion:  contracts.SchemaVersionV1,
		OrganizationID: record.Proposal.OrganizationID,
		ProjectID:      record.Proposal.ProjectID, WorkspaceID: record.Proposal.WorkspaceID,
		Source: record.Proposal.Source, Records: []EngineeringRecord{record},
		Digest: hashText("view"), ExpiresAt: projectBrainNow().Add(time.Hour),
	}
	if err := view.Validate(); err != nil {
		t.Fatalf("valid view rejected: %v", err)
	}
	tests := []struct {
		name   string
		mutate func(*View)
	}{
		{"schema", func(value *View) { value.SchemaVersion = "v2" }},
		{"organization", func(value *View) { value.OrganizationID = "" }},
		{"source", func(value *View) { value.Source.Generation = 0 }},
		{"records", func(value *View) { value.Records = make([]EngineeringRecord, 1025) }},
		{"invalid record", func(value *View) { value.Records[0].Proposal.ID = "" }},
		{"stale id", func(value *View) { value.StaleRecordIDs = []RecordID{""} }},
		{"cursor", func(value *View) {
			cursor := RecordID("")
			value.NextCursor = &cursor
		}},
		{"digest", func(value *View) { value.Digest = contracts.ContentHash{} }},
		{"expiry", func(value *View) { value.ExpiresAt = time.Time{} }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := view
			invalid.Records = append([]EngineeringRecord(nil), view.Records...)
			test.mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("invalid view %s accepted", test.name)
			}
		})
	}
}

func TestGraphSnapshot_RejectsAmbiguousFreshnessAndInvalidFiles(t *testing.T) {
	record, _, _, _, _ := signedRecord(t, "graph-validation")
	valid := record.Proposal.Source
	tests := []struct {
		name   string
		mutate func(*GraphSnapshot)
	}{
		{"schema", func(value *GraphSnapshot) { value.SchemaVersion = "v2" }},
		{"root", func(value *GraphSnapshot) { value.RootDigest = contracts.ContentHash{} }},
		{"graph", func(value *GraphSnapshot) { value.GraphDigest = contracts.ContentHash{} }},
		{"generation", func(value *GraphSnapshot) { value.Generation = 0 }},
		{"capture before index", func(value *GraphSnapshot) {
			value.CapturedAt = value.IndexedAt.Add(-time.Second)
		}},
		{"fresh pending", func(value *GraphSnapshot) { value.PendingFiles = []string{"main.go"} }},
		{"stale no pending", func(value *GraphSnapshot) { value.Fresh = false }},
		{"absolute file", func(value *GraphSnapshot) { value.Files[0].Path = "/main.go" }},
		{"duplicate file", func(value *GraphSnapshot) {
			value.Files = append(value.Files, value.Files[0])
		}},
		{"invalid pending", func(value *GraphSnapshot) {
			value.Fresh = false
			value.PendingFiles = []string{"../main.go"}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			invalid := valid
			invalid.Files = append([]GraphFile(nil), valid.Files...)
			test.mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatalf("invalid %s accepted", test.name)
			}
		})
	}
}

func TestCodeGraphHelpers_ClassifyAndDiscoverCurrentSource(t *testing.T) {
	for _, path := range []string{"main.go", "view.tsx", "schema.sql", "worker.py"} {
		if !isSourceFile(path) || sourceLanguage(path) == "other" {
			t.Fatalf("source classification failed for %s", path)
		}
	}
	if isSourceFile("README.md") || sourceLanguage("unit.rs") != "rust" ||
		sourceLanguage("native.c") != "other" {
		t.Fatal("source classification accepted unsupported content")
	}
	for _, directory := range []string{".git", ".cg", ".codegraph", "node_modules", "vendor"} {
		if !skipSourceDirectory(directory) {
			t.Fatalf("source directory %q was not excluded", directory)
		}
	}
	if skipSourceDirectory("internal") {
		t.Fatal("ordinary source directory was excluded")
	}
	if err := validateFilter("../escape"); err == nil {
		t.Fatal("escaping CodeGraph filter accepted")
	}

	root := t.TempDir()
	indexedPath := filepath.Join(root, "indexed.go")
	addedPath := filepath.Join(root, "added.go")
	if err := os.WriteFile(indexedPath, []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(addedPath, []byte("package sample\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	indexedHash, _, err := hashWorkspaceFile(root, indexedPath)
	if err != nil {
		t.Fatal(err)
	}
	files, pending, err := captureFiles(root, "", []indexedFile{{
		Path: "indexed.go", Language: "go", NodeCount: 1,
		Digest: "sha256:" + indexedHash.Digest[:16],
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || len(pending) != 1 || pending[0] != "added.go" {
		t.Fatalf("current source discovery files=%#v pending=%#v", files, pending)
	}
	_, pending, err = captureFiles(root, "", []indexedFile{{
		Path: "removed.go", Language: "go", NodeCount: 1,
		Digest: "sha256:0000000000000000",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) == 0 || pending[len(pending)-1] != "removed.go" {
		t.Fatalf("removed indexed source was not pending: %#v", pending)
	}
	outside := filepath.Join(t.TempDir(), "outside.go")
	if err := os.WriteFile(outside, []byte("package outside\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link.go")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hashWorkspaceFile(root, link); err == nil {
		t.Fatal("workspace source hash followed a symlink")
	}
}

func TestCodeGraph_RejectsInvalidConstructionAndWorkspace(t *testing.T) {
	if _, err := NewCodeGraph("", projectBrainNow); err == nil {
		t.Fatal("CodeGraph accepted empty executable")
	}
	if _, err := NewCodeGraph("/bin/false", projectBrainNow); err == nil {
		t.Fatal("CodeGraph accepted executable with wrong identity")
	}
	if _, err := NewCodeGraph("cg", projectBrainNow); err == nil {
		t.Fatal("CodeGraph accepted a relative executable")
	}
	executable, err := exec.LookPath("cg")
	if err != nil {
		t.Fatal(err)
	}
	graph, err := NewCodeGraph(executable, projectBrainNow)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(graph.environment, "\x00") !=
		"PATH=/usr/bin:/bin\x00HOME=/nonexistent\x00NO_COLOR=1" {
		t.Fatalf("CodeGraph environment is not exact: %#v", graph.environment)
	}
	if _, err := graph.Impact(
		context.Background(), t.TempDir(), "--output=/tmp/escape", 1,
	); err == nil {
		t.Fatal("CodeGraph accepted an option-shaped symbol")
	}
	if validateRelativePath("-output") == nil ||
		validateRelativePath("source\nfile.go") == nil {
		t.Fatal("CodeGraph accepted an option-shaped or control-bearing path")
	}
	if _, err := graph.Capture(context.Background(), t.TempDir(), "../escape"); err == nil {
		t.Fatal("CodeGraph accepted escaping filter")
	}
	if _, err := graph.Capture(context.Background(), t.TempDir(), ""); err == nil {
		t.Fatal("CodeGraph accepted uninitialized workspace")
	}
	symlinkRoot := t.TempDir()
	if err := os.Mkdir(filepath.Join(symlinkRoot, ".cg"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(
		"/dev/null", filepath.Join(symlinkRoot, ".cg", "codegraph.db"),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := graph.Capture(context.Background(), symlinkRoot, ""); err == nil {
		t.Fatal("CodeGraph accepted a symlink database")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	indexedRoot := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(indexedRoot, "go.mod"),
		[]byte("module indexed\n\ngo 1.26\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(indexedRoot, "main.go"), []byte("package indexed\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if output, err := exec.Command(
		executable, "--db", filepath.Join(indexedRoot, ".cg", "codegraph.db"),
		"build", indexedRoot,
	).CombinedOutput(); err != nil {
		t.Fatalf("build cancellation index: %v: %s", err, output)
	}
	if _, _, _, err := graph.exportGraph(cancelled, indexedRoot); err == nil {
		t.Fatal("CodeGraph command ignored cancellation")
	}
}

func TestSigningAndSorting_RejectInvalidAuthorityAndOrderDeterministically(t *testing.T) {
	if err := SignProposal(nil, "key", make(ed25519.PrivateKey, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("proposal signing accepted nil proposal")
	}
	if err := SignVerification(nil, "key", make(ed25519.PrivateKey, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("verification signing accepted nil verification")
	}
	if err := SignCapabilityGrant(nil, "key", make(ed25519.PrivateKey, ed25519.PrivateKeySize)); err == nil {
		t.Fatal("capability signing accepted nil grant")
	}
	first, _, _, _, _ := signedRecord(t, "sort-z")
	second, _, _, _, _ := signedRecord(t, "sort-a")
	second.Proposal.Kind = KindDecision
	second.Proposal.Version = 2
	first.Proposal.Kind = KindVerification
	records := []EngineeringRecord{first, second}
	sortRecords(records)
	if records[0].Proposal.ID != second.Proposal.ID {
		t.Fatalf("record ordering = %#v", records)
	}
	record, _, _, authorPublic, verifierPublic := signedRecord(t, "bad-signature")
	record.Verification.Signature.Value = "invalid"
	if err := verifyRecordSignatures(record, authorPublic, verifierPublic); err == nil {
		t.Fatal("verification accepted malformed signature")
	}
	record, _, _, authorPublic, _ = signedRecord(t, "wrong-verifier")
	wrongVerifier, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := verifyRecordSignatures(record, authorPublic, wrongVerifier); err == nil {
		t.Fatal("verification accepted wrong verifier authority")
	}
	third := second
	third.Proposal.ID = "record-sort-0"
	records = []EngineeringRecord{second, third}
	sortRecords(records)
	if records[0].Proposal.ID != third.Proposal.ID {
		t.Fatalf("same-version record ordering = %#v", records)
	}
	if err := (proposalSigningPayload{
		Proposal: first.Proposal, KeyID: "different",
	}).Validate(); err == nil {
		t.Fatal("proposal signing payload accepted key mismatch")
	}
	if err := (verificationSigningPayload{
		Verification: first.Verification, KeyID: "different",
	}).Validate(); err == nil {
		t.Fatal("verification signing payload accepted key mismatch")
	}
	grant := CapabilityGrant{Signature: placeholderSignature("one")}
	if err := (grantSigningPayload{Grant: grant, KeyID: "different"}).Validate(); err == nil {
		t.Fatal("grant signing payload accepted key mismatch")
	}
	if _, err := (SeatKeyBinding{}).publicKey(); err == nil {
		t.Fatal("seat key binding opened invalid authority")
	}
	if denialReason(errors.New("other")) != "invalid" {
		t.Fatal("unexpected denial reason classification")
	}
}

func TestStore_RejectsInvalidConstructionAndAuthorization(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	if _, err := New(
		nil, store.vault, tenantID, store.authorityKeyID,
		store.authorityKey, store.seatAuthority, store.graph, projectBrainNow,
	); err == nil {
		t.Fatal("store accepted nil PostgreSQL")
	}
	if _, err := New(
		projectBrainPool, nil, tenantID, store.authorityKeyID,
		store.authorityKey, store.seatAuthority, store.graph, projectBrainNow,
	); err == nil {
		t.Fatal("store accepted nil Vault")
	}
	if _, err := New(
		projectBrainPool, store.vault, "other-tenant", store.authorityKeyID,
		store.authorityKey, store.seatAuthority, store.graph, projectBrainNow,
	); err == nil {
		t.Fatal("store accepted mismatched Vault tenant")
	}
	if _, err := New(
		projectBrainPool, store.vault, "", store.authorityKeyID,
		store.authorityKey, store.seatAuthority, store.graph, projectBrainNow,
	); err == nil {
		t.Fatal("store accepted empty tenant")
	}
	if _, err := New(
		projectBrainPool, store.vault, tenantID, store.authorityKeyID,
		store.authorityKey, store.seatAuthority, nil, projectBrainNow,
	); err == nil {
		t.Fatal("store accepted nil CodeGraph")
	}
	if _, err := New(
		projectBrainPool, store.vault, tenantID, store.authorityKeyID,
		store.authorityKey, store.seatAuthority, store.graph, nil,
	); err == nil {
		t.Fatal("store accepted nil time source")
	}
	if _, err := New(
		projectBrainPool, store.vault, tenantID, store.authorityKeyID,
		store.authorityKey, nil, store.graph, projectBrainNow,
	); err == nil {
		t.Fatal("store accepted nil current seat authority")
	}
	record, _, _, _, _ := signedRecord(t, "authorization")
	record.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	grant := integrationReadGrant(t, record, root, authorityPrivate, 10, nil)
	grant.ExpiresAt = projectBrainNow()
	if _, err := store.View(context.Background(), grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("expired access grant error = %v", err)
	}
	var denials int
	if err := projectBrainPool.QueryRow(context.Background(), `
		SELECT COUNT(*) FROM workforce_project_brain_access_denials
		WHERE tenant_id=$1 AND grant_id=$2
	`, tenantID, grant.ID).Scan(&denials); err != nil || denials != 1 {
		t.Fatalf("denied read audit count=%d err=%v", denials, err)
	}
	validGrant := integrationReadGrant(t, record, root, authorityPrivate, 10, nil)
	validGrant.ID += "-tampered"
	validGrant.Purpose = "tampered-after-signing"
	if _, err := store.View(context.Background(), validGrant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("tampered capability error = %v", err)
	}
}

func TestStore_RejectsWrongSignatureConflictAndBrokenVersionChain(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	record, _, _, _, _ := signedRecord(t, "store-rejections")
	record.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	bindRecordToWorkspace(t, &record, store.graph, root)
	resignRecord(t, &record)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	forgedSource := record
	forgedSource.Proposal.ID = RecordID("forged-source-" + tenantID)
	forgedSource.Proposal.Source.RootDigest = hashText("fabricated-source")
	forgedSource.Verification.RecordID = forgedSource.Proposal.ID
	resignRecord(t, &forgedSource)
	forgedGrant := integrationWriteGrant(
		t, forgedSource, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, forgedSource, forgedGrant); err == nil {
		t.Fatal("store accepted caller-fabricated source snapshot")
	}
	resignRecord(t, &record)

	wrongPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	grant := integrationWriteGrant(
		t, record, root, authorityPrivate, wrongPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, record, grant); err == nil {
		t.Fatal("store accepted wrong author signature")
	}
	grant = integrationWriteGrant(
		t, record, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, record, grant); err != nil {
		t.Fatal(err)
	}

	conflict := record
	conflict.Proposal.Content.Summary = "different canonical truth"
	resignRecord(t, &conflict)
	conflictGrant := integrationWriteGrant(
		t, conflict, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, conflict, conflictGrant); !errors.Is(err, ErrConflict) {
		t.Fatalf("immutable conflict error = %v", err)
	}

	missing := record
	missing.Proposal.ID = RecordID("missing-parent-" + tenantID)
	missing.Proposal.Version = 2
	parent := RecordID("does-not-exist")
	missing.Proposal.Supersedes = &parent
	missing.Verification.RecordID = missing.Proposal.ID
	resignRecord(t, &missing)
	missingGrant := integrationWriteGrant(
		t, missing, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, missing, missingGrant); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing supersession parent error = %v", err)
	}

	future := record
	future.Proposal.ID = RecordID("future-" + tenantID)
	future.Proposal.CreatedAt = projectBrainNow().Add(time.Minute)
	future.Verification.RecordID = future.Proposal.ID
	future.Verification.VerifiedAt = projectBrainNow().Add(2 * time.Minute)
	resignRecord(t, &future)
	futureGrant := integrationWriteGrant(
		t, future, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, future, futureGrant); err == nil {
		t.Fatal("store accepted future record chronology")
	}

	wrongVersion := record
	wrongVersion.Proposal.ID = RecordID("wrong-version-" + tenantID)
	wrongVersion.Proposal.Version = 3
	wrongVersion.Proposal.Supersedes = &record.Proposal.ID
	wrongVersion.Verification.RecordID = wrongVersion.Proposal.ID
	resignRecord(t, &wrongVersion)
	wrongVersionGrant := integrationWriteGrant(
		t, wrongVersion, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, wrongVersion, wrongVersionGrant); err == nil {
		t.Fatal("store accepted broken version chain")
	}

	wrongKind := record
	wrongKind.Proposal.ID = RecordID("wrong-kind-" + tenantID)
	wrongKind.Proposal.Version = 2
	wrongKind.Proposal.Kind = KindPlan
	wrongKind.Proposal.Supersedes = &record.Proposal.ID
	wrongKind.Verification.RecordID = wrongKind.Proposal.ID
	resignRecord(t, &wrongKind)
	wrongKindGrant := integrationWriteGrant(
		t, wrongKind, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, wrongKind, wrongKindGrant); err == nil {
		t.Fatal("store accepted supersession that changes kind")
	}
}

func TestStore_ViewOmitsExpiredEvidenceAndFailsClosedOnInvalidClock(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	record, _, _, _, _ := signedRecord(t, "expired-view")
	record.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	bindRecordToWorkspace(t, &record, store.graph, root)
	expiredAt := projectBrainNow()
	record.Proposal.Content.ExpiresAt = &expiredAt
	resignRecord(t, &record)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	writeGrant := integrationWriteGrant(
		t, record, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, record, writeGrant); err != nil {
		t.Fatal(err)
	}
	grant := integrationReadGrant(t, record, root, authorityPrivate, 10, nil)
	view, err := store.View(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Records) != 0 {
		t.Fatalf("expired records remained visible: %#v", view.Records)
	}
	store.now = func() time.Time { return projectBrainNow().In(time.FixedZone("other", 3600)) }
	if _, err := store.View(ctx, grant); err == nil {
		t.Fatal("view accepted non-UTC authority clock")
	}
}

func TestStore_RejectsCrossTenantCapabilityReplayAndAuditsWriteIntegrity(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	record, _, _, _, _ := signedRecord(t, "tenant-replay")
	record.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	bindRecordToWorkspace(t, &record, store.graph, root)
	resignRecord(t, &record)
	writeGrant := integrationWriteGrant(
		t, record, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(context.Background(), record, writeGrant); err != nil {
		t.Fatal(err)
	}

	replay := integrationReadGrant(t, record, root, authorityPrivate, 10, nil)
	replay.TenantID = "tenant-other"
	if err := SignCapabilityGrant(&replay, "kernel-authority", authorityPrivate); err != nil {
		t.Fatal(err)
	}
	if _, err := store.View(context.Background(), replay); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("cross-tenant replay error = %v", err)
	}
	wrongProject := integrationReadGrant(t, record, root, authorityPrivate, 10, nil)
	wrongProject.ID += "-other-project"
	wrongProject.ProjectID = "project-other"
	if err := SignCapabilityGrant(
		&wrongProject, "kernel-authority", authorityPrivate,
	); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(context.Background(), wrongProject)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Records) != 0 {
		t.Fatalf("unauthorized Project Brain records crossed project scope: %#v", view.Records)
	}

	forged := record
	forged.Proposal.Source.RootDigest = hashText("forged-root")
	resignRecord(t, &forged)
	forgedGrant := integrationWriteGrant(
		t, forged, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(context.Background(), forged, forgedGrant); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("forged source error = %v", err)
	}
	var replayEvents, integrityEvents int
	if err := projectBrainPool.QueryRow(context.Background(), `
		SELECT
			COUNT(*) FILTER (WHERE grant_id=$2 AND operation='read'),
			COUNT(*) FILTER (WHERE grant_id=$3 AND operation='write' AND reason_code='source_mismatch')
		FROM workforce_project_brain_access_denials
		WHERE tenant_id=$1
	`, tenantID, replay.ID, forgedGrant.ID).Scan(&replayEvents, &integrityEvents); err != nil {
		t.Fatal(err)
	}
	if replayEvents != 1 || integrityEvents != 1 {
		t.Fatalf("security audit replay=%d integrity=%d", replayEvents, integrityEvents)
	}
}

func TestHashWorkspaceFileRejectsOversizeAndRevalidationDetectsMutation(t *testing.T) {
	root := t.TempDir()
	oversize := filepath.Join(root, "oversize.go")
	file, err := os.Create(oversize)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(maxSourceFileBytes + 1); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := hashWorkspaceFile(root, oversize); err == nil {
		t.Fatal("source larger than one GiB was prefix-hashed")
	}

	first := filepath.Join(root, "first.go")
	second := filepath.Join(root, "second.go")
	if err := os.WriteFile(first, []byte("package p\nconst First = 1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("package p\nconst Second = 2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	files := make([]GraphFile, 0, 2)
	for _, name := range []string{"first.go", "second.go"} {
		hash, info, err := hashWorkspaceFile(root, filepath.Join(root, name))
		if err != nil {
			t.Fatal(err)
		}
		files = append(files, GraphFile{
			Path: name, SizeBytes: uint64(info.Size()), Hash: hash,
		})
	}
	if err := os.WriteFile(first, []byte("package p\nconst First = 3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := revalidateGraphFiles(root, "", files); err == nil {
		t.Fatal("cross-file mutation after initial hash was not detected")
	}
	hash, info, err := hashWorkspaceFile(root, first)
	if err != nil {
		t.Fatal(err)
	}
	files[0].Hash = hash
	files[0].SizeBytes = uint64(info.Size())
	if err := os.WriteFile(
		filepath.Join(root, "new.go"),
		[]byte("package p\nconst New = 4\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := revalidateGraphFiles(root, "", files); err == nil {
		t.Fatal("new source path after initial discovery was not detected")
	}
}

func TestBoundedOutputCancelsAndCapsBytes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	output := boundedOutput{limit: 4, cancel: cancel}
	written, err := output.Write([]byte("123456"))
	if err != nil || written != 6 {
		t.Fatalf("bounded write = %d, %v", written, err)
	}
	if !output.exceeded || string(output.Bytes()) != "1234" {
		t.Fatalf("bounded output = %q exceeded=%v", output.Bytes(), output.exceeded)
	}
	select {
	case <-ctx.Done():
	default:
		t.Fatal("output overflow did not terminate the subprocess context")
	}
}
