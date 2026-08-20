package projectbrain

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"centra/packages/vault"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/dependency"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/ledger"
	"centra/workforce/internal/testauthority"
)

const projectBrainPostgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var projectBrainPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startProjectBrainPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "project brain integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	projectBrainPool, err = waitForProjectBrainPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "project brain integration setup:", err)
		os.Exit(1)
	}
	if err := ledger.ApplyMigrations(ctx, projectBrainPool, projectBrainNow()); err != nil {
		projectBrainPool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "project brain migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	projectBrainPool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_CodeGraphCapture_UsesRealIndexAndChangedFileFallback(t *testing.T) {
	executable, err := exec.LookPath("cg")
	if err != nil {
		t.Fatalf("locate real CodeGraph: %v", err)
	}
	root := t.TempDir()
	sourcePath := filepath.Join(root, "main.go")
	if err := os.WriteFile(
		filepath.Join(root, "go.mod"), []byte("module example\n\ngo 1.26\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sourcePath, []byte("package example\n\nfunc Value() int { return 1 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	database := filepath.Join(root, ".cg", "codegraph.db")
	output, err := exec.CommandContext(
		ctx, executable, "--db", database, "build", root,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("initialize real CodeGraph: %v: %s", err, output)
	}
	now := time.Now().UTC().Add(time.Second)
	graph, err := NewCodeGraph(executable, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := graph.Capture(ctx, root, "")
	if err != nil {
		t.Fatalf("capture fresh CodeGraph: %v", err)
	}
	if !fresh.Fresh || len(fresh.Files) != 2 {
		t.Fatalf("fresh capture = %#v", fresh)
	}
	indexedMain := false
	var freshMain contracts.ContentHash
	for _, file := range fresh.Files {
		if file.Path == "main.go" && file.Indexed {
			indexedMain = true
			freshMain = file.Hash
		}
	}
	if !indexedMain {
		t.Fatalf("fresh capture omitted indexed main.go: %#v", fresh.Files)
	}
	if err := os.WriteFile(sourcePath, []byte("package example\n\nfunc Value() int { return 2 }\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(sourcePath, now.Add(time.Second), now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(2 * time.Second)
	stale, err := graph.Capture(ctx, root, "")
	if err != nil {
		t.Fatalf("capture changed source: %v", err)
	}
	var staleMain contracts.ContentHash
	for _, file := range stale.Files {
		if file.Path == "main.go" {
			staleMain = file.Hash
		}
	}
	if stale.Fresh || len(stale.PendingFiles) != 1 ||
		stale.PendingFiles[0] != "main.go" || staleMain == freshMain {
		t.Fatalf("changed-file fallback = %#v", stale)
	}
	output, err = exec.CommandContext(
		ctx, executable, "--db", database, "build", root, "--incremental",
	).CombinedOutput()
	if err != nil {
		t.Fatalf("refresh real CodeGraph: %v: %s", err, output)
	}
	now = time.Now().UTC().Add(time.Second)
	refreshed, err := graph.Capture(ctx, root, "")
	if err != nil {
		t.Fatalf("capture refreshed CodeGraph: %v", err)
	}
	if !refreshed.Fresh || refreshed.GraphDigest == fresh.GraphDigest ||
		refreshed.RootDigest == fresh.RootDigest {
		t.Fatalf("refreshed CodeGraph = %#v", refreshed)
	}
	now = refreshed.IndexedAt.Add(-time.Second)
	if _, err := graph.Capture(ctx, root, ""); err == nil {
		t.Fatal("CodeGraph accepted capture clock before index generation")
	}
}

func TestIntegration_ProjectBrainCommitViewCorrectionAndVault_UsesRealPostgres(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	record, _, _, _, _ := signedRecord(t, "commit")
	record.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	bindRecordToWorkspace(t, &record, store.graph, root)
	resignRecord(t, &record)
	writeGrant := integrationWriteGrant(
		t, record, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	deduplicated, err := store.Commit(ctx, record, writeGrant)
	if err != nil || deduplicated {
		t.Fatalf("commit record: deduplicated=%v err=%v", deduplicated, err)
	}
	deduplicated, err = store.Commit(ctx, record, writeGrant)
	if err != nil || !deduplicated {
		t.Fatalf("deduplicate record: deduplicated=%v err=%v", deduplicated, err)
	}
	second := record
	second.Proposal.ID = RecordID("record-second-" + tenantID)
	second.Proposal.Kind = KindPlan
	second.Proposal.Content.Summary = "second current verified engineering record"
	second.Verification.RecordID = second.Proposal.ID
	resignRecord(t, &second)
	secondGrant := integrationWriteGrant(
		t, second, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, second, secondGrant); err != nil {
		t.Fatalf("commit second record: %v", err)
	}
	grant := integrationReadGrant(t, record, root, authorityPrivate, 1, nil)
	view, err := store.View(ctx, grant)
	if err != nil {
		t.Fatalf("open view: %v", err)
	}
	if len(view.Records) != 1 || view.NextCursor == nil {
		t.Fatalf("view records = %#v", view.Records)
	}
	nextGrant := integrationReadGrant(
		t, record, root, authorityPrivate, 1, view.NextCursor,
	)
	nextGrant.ID += "-next"
	if err := SignCapabilityGrant(&nextGrant, "kernel-authority", authorityPrivate); err != nil {
		t.Fatal(err)
	}
	nextView, err := store.View(ctx, nextGrant)
	if err != nil || len(nextView.Records) != 1 {
		t.Fatalf("next cursor view records=%#v err=%v", nextView.Records, err)
	}

	var sealed []byte
	if err := projectBrainPool.QueryRow(ctx, `
		SELECT sealed_record FROM workforce_project_brain_records
		WHERE tenant_id=$1 AND organization_id=$2 AND project_id=$3
		  AND workspace_id=$4 AND record_id=$5
	`, tenantID, record.Proposal.OrganizationID, record.Proposal.ProjectID,
		record.Proposal.WorkspaceID, record.Proposal.ID).Scan(&sealed); err != nil {
		t.Fatal(err)
	}
	if !vault.IsVault(sealed) || bytes.Contains(sealed, []byte(record.Proposal.Content.Summary)) {
		t.Fatal("project brain record is not confidentially Vault sealed")
	}

	correction := record
	correction.Proposal.ID = RecordID("correction-" + tenantID)
	correction.Proposal.Kind = KindCorrection
	correction.Proposal.Origin = OriginCorrection
	correction.Proposal.Version = 2
	correction.Proposal.Supersedes = nil
	correction.Proposal.Corrects = &record.Proposal.ID
	correction.Proposal.Content.Summary = "corrected verified engineering truth"
	correction.Proposal.CreatedAt = projectBrainNow()
	correction.Verification.RecordID = correction.Proposal.ID
	correction.Verification.VerifiedAt = projectBrainNow()
	resignRecord(t, &correction)
	correctionGrant := integrationWriteGrant(
		t, correction, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, correction, correctionGrant); err != nil {
		t.Fatalf("commit correction: %v", err)
	}
	sibling := correction
	sibling.Proposal.ID = RecordID("correction-sibling-" + tenantID)
	sibling.Verification.RecordID = sibling.Proposal.ID
	sibling.Proposal.Content.Summary = "competing correction"
	resignRecord(t, &sibling)
	siblingGrant := integrationWriteGrant(
		t, sibling, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(ctx, sibling, siblingGrant); !errors.Is(err, ErrConflict) {
		t.Fatalf("sibling correction error = %v, want ErrConflict", err)
	}
	grant = integrationReadGrant(t, record, root, authorityPrivate, 128, nil)
	view, err = store.View(ctx, grant)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Records) != 2 {
		t.Fatalf("correction view records = %#v", view.Records)
	}
	if err := os.WriteFile(
		filepath.Join(root, "main.go"),
		[]byte("package project\n\nfunc Current() string { return \"changed\" }\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(
		filepath.Join(root, "main.go"),
		projectBrainNow().Add(time.Second), projectBrainNow().Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	staleView, err := store.View(ctx, grant)
	if err != nil {
		t.Fatalf("open changed-source view: %v", err)
	}
	if len(staleView.Records) != 0 || len(staleView.StaleRecordIDs) != 2 {
		t.Fatalf("stale view records=%#v stale=%#v", staleView.Records, staleView.StaleRecordIDs)
	}
}

func TestIntegration_ProjectBrainCommitAcceptsLaterCaptureObservation(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	record, _, _, _, _ := signedRecord(t, "live-clock")
	record.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	store.graph.now = func() time.Time { return projectBrainNow().Add(time.Minute) }
	bindRecordToWorkspace(t, &record, store.graph, root)
	resignRecord(t, &record)
	store.graph.now = func() time.Time { return projectBrainNow().Add(2 * time.Minute) }
	store.now = func() time.Time { return projectBrainNow().Add(3 * time.Minute) }
	grant := integrationWriteGrant(
		t, record, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(context.Background(), record, grant); err != nil {
		t.Fatalf("unchanged source with later capture observation was rejected: %v", err)
	}
}

func TestIntegration_ProjectBrainPaginationAdvancesPastStaleRows(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	stale, _, _, _, _ := signedRecord(t, "page-stale")
	stale.Proposal.ID = RecordID("a-stale-" + tenantID)
	stale.Verification.RecordID = stale.Proposal.ID
	stale.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	stale.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	stale.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	bindRecordToWorkspace(t, &stale, store.graph, root)
	resignRecord(t, &stale)
	staleGrant := integrationWriteGrant(
		t, stale, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(context.Background(), stale, staleGrant); err != nil {
		t.Fatal(err)
	}

	current := stale
	current.Proposal.ID = RecordID("b-current-" + tenantID)
	current.Verification.RecordID = current.Proposal.ID
	current.Proposal.Kind = KindPlan
	current.Proposal.Content.Claims[0].Files = nil
	current.Proposal.Content.Claims[0].Evidence = append(
		[]contracts.EvidenceRef(nil),
		current.Verification.Evidence...,
	)
	resignRecord(t, &current)
	currentGrant := integrationWriteGrant(
		t, current, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	if _, err := store.Commit(context.Background(), current, currentGrant); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "main.go"),
		[]byte("package project\n\nfunc Current() string { return \"stale\" }\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(
		filepath.Join(root, "main.go"),
		projectBrainNow().Add(time.Second), projectBrainNow().Add(time.Second),
	); err != nil {
		t.Fatal(err)
	}
	firstGrant := integrationReadGrant(t, stale, root, authorityPrivate, 1, nil)
	first, err := store.View(context.Background(), firstGrant)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Records) != 0 || len(first.StaleRecordIDs) != 1 || first.NextCursor == nil {
		t.Fatalf("first stale page = %#v", first)
	}
	nextGrant := integrationReadGrant(
		t, stale, root, authorityPrivate, 1, first.NextCursor,
	)
	nextGrant.ID += "-current"
	if err := SignCapabilityGrant(&nextGrant, "kernel-authority", authorityPrivate); err != nil {
		t.Fatal(err)
	}
	next, err := store.View(context.Background(), nextGrant)
	if err != nil {
		t.Fatal(err)
	}
	if len(next.Records) != 1 || next.Records[0].Proposal.ID != current.Proposal.ID {
		t.Fatalf("current record after stale cursor = %#v", next)
	}
}

func TestIntegration_ProjectBrainConcurrentCommit_StoresOneImmutableRecord(t *testing.T) {
	store, tenantID, authorityPrivate, root, _ := integrationProjectBrainStore(t)
	record, _, _, _, _ := signedRecord(t, "concurrent")
	record.Proposal.OrganizationID = contracts.OrganizationID("org-" + tenantID)
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	bindRecordToWorkspace(t, &record, store.graph, root)
	resignRecord(t, &record)
	grant := integrationWriteGrant(
		t, record, root, authorityPrivate, recordAuthorPublic, recordVerifierPublic,
	)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const writers = 8
	var wait sync.WaitGroup
	errorsChannel := make(chan error, writers)
	for index := 0; index < writers; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, err := store.Commit(ctx, record, grant)
			errorsChannel <- err
		}()
	}
	wait.Wait()
	close(errorsChannel)
	for err := range errorsChannel {
		if err != nil {
			t.Fatalf("concurrent commit: %v", err)
		}
	}
	var count int
	if err := projectBrainPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_project_brain_records
		WHERE tenant_id=$1 AND organization_id=$2 AND project_id=$3
		  AND workspace_id=$4 AND record_id=$5
	`, tenantID, record.Proposal.OrganizationID, record.Proposal.ProjectID,
		record.Proposal.WorkspaceID, record.Proposal.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("concurrent record count = %d, want 1", count)
	}
}

func TestIntegration_ProjectBrainRejectsGrantAfterRequesterSeatRotation(t *testing.T) {
	store, tenantID, authorityPrivate, root, policyAuthority :=
		integrationProjectBrainStore(t)
	organizationID := contracts.OrganizationID("org-" + tenantID)
	record, _, _, _, _ := signedRecord(t, "seat-rotation")
	record.Proposal.OrganizationID = organizationID
	record.Proposal.ProjectID = contracts.ProjectID("project-" + tenantID)
	record.Proposal.WorkspaceID = contracts.WorkspaceID("workspace-" + tenantID)
	grant := integrationReadGrant(t, record, root, authorityPrivate, 16, nil)
	if _, err := store.View(context.Background(), grant); err != nil {
		t.Fatalf("current requester grant rejected: %v", err)
	}

	seat := integrationProjectBrainSeat(organizationID, grant.RequesterSeatID)
	seat.Version = 2
	seat.DID += ":rotated"
	seat.BindingVersion = 2
	seat.EffectiveAt = projectBrainNow()
	if err := policyAuthority.PublishSeat(context.Background(), seat); err != nil {
		t.Fatal(err)
	}
	if _, err := store.View(context.Background(), grant); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("stale pre-rotation requester grant error = %v", err)
	}
}

var (
	recordAuthorPrivate   ed25519.PrivateKey
	recordAuthorPublic    ed25519.PublicKey
	recordVerifierPrivate ed25519.PrivateKey
	recordVerifierPublic  ed25519.PublicKey
)

func signedRecord(
	t *testing.T,
	label string,
) (EngineeringRecord, ed25519.PrivateKey, ed25519.PrivateKey, ed25519.PublicKey, ed25519.PublicKey) {
	t.Helper()
	authorPublic, authorPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifierPublic, verifierPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := projectBrainNow()
	sourceHash := hashText("source-" + label)
	proposal := Proposal{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             RecordID("record-" + label),
		OrganizationID: contracts.OrganizationID("org-" + label),
		ProjectID:      contracts.ProjectID("project-" + label),
		WorkspaceID:    contracts.WorkspaceID("workspace-" + label),
		AuthorSeatID:   "developer-author",
		ParentIntentID: contracts.IntentID("intent-" + label),
		Kind:           KindDecision, Origin: OriginSource, Version: 1,
		Source: GraphSnapshot{
			SchemaVersion: contracts.SchemaVersionV1,
			RootDigest:    hashText("root-" + label), GraphDigest: hashText("graph-" + label),
			Generation: 1, IndexedAt: now.Add(-time.Minute), CapturedAt: now,
			Fresh: true,
			Files: []GraphFile{{
				Path: "main.go", Language: "go", NodeCount: 1, SizeBytes: 64,
				Hash: sourceHash, Indexed: true,
			}},
			NodeCount: 1, EdgeCount: 0,
		},
		Content: Content{
			Summary: "verified engineering decision " + label,
			Claims: []Claim{{
				Statement: "the current source establishes " + label,
				Files: []FileEvidence{{
					Path: "main.go", Hash: sourceHash, StartLine: 1, EndLine: 2,
				}},
			}},
		},
		CreatedAt: now,
	}
	if err := SignProposal(&proposal, "author-key", authorPrivate); err != nil {
		t.Fatal(err)
	}
	proposalDigest, err := proposalHash(proposal)
	if err != nil {
		t.Fatal(err)
	}
	verification := Verification{
		SchemaVersion: contracts.SchemaVersionV1,
		RecordID:      proposal.ID, VerifierSeatID: "developer-auditor",
		ProposalHash: proposalDigest, Accepted: true,
		Procedure: "source-and-contract-review.v1",
		Evidence: []contracts.EvidenceRef{{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.EvidenceID("evidence-" + label),
			Hash:          hashText("evidence-" + label), Kind: "source-review",
			ObservedAt: now,
		}},
		VerifiedAt: now,
	}
	if err := SignVerification(&verification, "verifier-key", verifierPrivate); err != nil {
		t.Fatal(err)
	}
	return EngineeringRecord{Proposal: proposal, Verification: verification},
		authorPrivate, verifierPrivate, authorPublic, verifierPublic
}

func resignRecord(t *testing.T, record *EngineeringRecord) {
	t.Helper()
	authorPublic, authorPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	verifierPublic, verifierPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignProposal(&record.Proposal, "author-key", authorPrivate); err != nil {
		t.Fatal(err)
	}
	record.Verification.ProposalHash, err = proposalHash(record.Proposal)
	if err != nil {
		t.Fatal(err)
	}
	if err := SignVerification(&record.Verification, "verifier-key", verifierPrivate); err != nil {
		t.Fatal(err)
	}
	recordAuthorPrivate, recordAuthorPublic = authorPrivate, authorPublic
	recordVerifierPrivate, recordVerifierPublic = verifierPrivate, verifierPublic
}

func integrationProjectBrainStore(
	t *testing.T,
) (*Store, string, ed25519.PrivateKey, string, *testauthority.Fixture) {
	t.Helper()
	sum := sha256.Sum256([]byte(t.Name()))
	tenantID := "tenant-" + hex.EncodeToString(sum[:6])
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: tenantID,
		KEKHex: hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef")),
	})
	if err != nil {
		t.Fatal(err)
	}
	executable, err := exec.LookPath("cg")
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(root, "go.mod"), []byte("module project\n\ngo 1.26\n"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "main.go"),
		[]byte("package project\n\nfunc Current() string { return \"verified\" }\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	output, err := exec.CommandContext(
		ctx, executable, "--db", filepath.Join(root, ".cg", "codegraph.db"),
		"build", root,
	).CombinedOutput()
	if err != nil {
		t.Fatalf("initialize Project Brain CodeGraph: %v: %s", err, output)
	}
	graph, err := NewCodeGraph(executable, projectBrainNow)
	if err != nil {
		t.Fatal(err)
	}
	authorityPublic, authorityPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	organizationID := contracts.OrganizationID("org-" + tenantID)
	policyAuthority, err := testauthority.New(
		projectBrainPool, session.UserVault(), tenantID, organizationID,
		"projectbrain:"+tenantID, projectBrainNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, seatID := range []contracts.SeatID{
		"developer-author", "developer-auditor", "developer-reader",
	} {
		publishIntegrationProjectBrainSeat(t, policyAuthority, organizationID, seatID)
	}
	store, err := New(
		projectBrainPool, session.UserVault(), tenantID,
		"kernel-authority", authorityPublic, policyAuthority.Store(),
		graph, projectBrainNow,
	)
	if err != nil {
		t.Fatal(err)
	}
	return store, tenantID, authorityPrivate, root, policyAuthority
}

func bindRecordToWorkspace(
	t *testing.T,
	record *EngineeringRecord,
	graph *CodeGraph,
	root string,
) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	source, err := graph.Capture(ctx, root, "")
	if err != nil {
		t.Fatal(err)
	}
	record.Proposal.Source = source
	var claimed GraphFile
	for _, file := range source.Files {
		if file.Indexed {
			claimed = file
			break
		}
	}
	if claimed.Path == "" {
		t.Fatal("Project Brain fixture has no indexed source")
	}
	record.Proposal.Content.Claims[0].Files = []FileEvidence{{
		Path: claimed.Path, Hash: claimed.Hash,
		StartLine: 1, EndLine: 3,
	}}
	record.Proposal.CreatedAt = projectBrainNow()
	record.Verification.VerifiedAt = projectBrainNow()
}

func integrationWriteGrant(
	t *testing.T,
	record EngineeringRecord,
	root string,
	authorityPrivate ed25519.PrivateKey,
	authorPublic, verifierPublic ed25519.PublicKey,
) CapabilityGrant {
	t.Helper()
	recordID := record.Proposal.ID
	requester := integrationProjectBrainSeat(
		record.Proposal.OrganizationID, record.Proposal.AuthorSeatID,
	)
	verifier := integrationProjectBrainSeat(
		record.Proposal.OrganizationID, record.Verification.VerifierSeatID,
	)
	grant := CapabilityGrant{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             "grant-write-" + string(recordID),
		TenantID:       grantTenantID(record),
		OrganizationID: record.Proposal.OrganizationID,
		ProjectID:      record.Proposal.ProjectID, WorkspaceID: record.Proposal.WorkspaceID,
		WorkspaceRoot: root, Operation: CapabilityWrite,
		RequesterSeatID:         requester.ID,
		RequesterSeatVersion:    requester.Version,
		RequesterSeatDID:        requester.DID,
		RequesterBindingID:      requester.BindingID,
		RequesterBindingVersion: requester.BindingVersion,
		RecordID:                &recordID,
		Author: &SeatKeyBinding{
			SeatID: requester.ID, SeatVersion: requester.Version,
			SeatDID: requester.DID, BindingID: requester.BindingID,
			BindingVersion: requester.BindingVersion,
			KeyID:          record.Proposal.Signature.KeyID,
			PublicKey:      base64.RawURLEncoding.EncodeToString(authorPublic),
		},
		Verifier: &SeatKeyBinding{
			SeatID: verifier.ID, SeatVersion: verifier.Version,
			SeatDID: verifier.DID, BindingID: verifier.BindingID,
			BindingVersion: verifier.BindingVersion,
			KeyID:          record.Verification.Signature.KeyID,
			PublicKey:      base64.RawURLEncoding.EncodeToString(verifierPublic),
		},
		Purpose:   "verified-engineering-write",
		IssuedAt:  projectBrainNow().Add(-time.Minute),
		ExpiresAt: projectBrainNow().Add(time.Hour),
	}
	if err := SignCapabilityGrant(&grant, "kernel-authority", authorityPrivate); err != nil {
		t.Fatal(err)
	}
	return grant
}

func integrationReadGrant(
	t *testing.T,
	record EngineeringRecord,
	root string,
	authorityPrivate ed25519.PrivateKey,
	maxRecords uint32,
	after *RecordID,
) CapabilityGrant {
	t.Helper()
	requester := integrationProjectBrainSeat(
		record.Proposal.OrganizationID, "developer-reader",
	)
	grant := CapabilityGrant{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             "grant-read-" + string(record.Proposal.ID),
		TenantID:       grantTenantID(record),
		OrganizationID: record.Proposal.OrganizationID,
		ProjectID:      record.Proposal.ProjectID, WorkspaceID: record.Proposal.WorkspaceID,
		WorkspaceRoot: root, Operation: CapabilityRead,
		RequesterSeatID:         requester.ID,
		RequesterSeatVersion:    requester.Version,
		RequesterSeatDID:        requester.DID,
		RequesterBindingID:      requester.BindingID,
		RequesterBindingVersion: requester.BindingVersion,
		Purpose:                 "implementation",
		AfterRecordID:           after, MaxRecords: maxRecords,
		IssuedAt:  projectBrainNow().Add(-time.Minute),
		ExpiresAt: projectBrainNow().Add(time.Hour),
	}
	if err := SignCapabilityGrant(&grant, "kernel-authority", authorityPrivate); err != nil {
		t.Fatal(err)
	}
	return grant
}

func integrationProjectBrainSeat(
	organizationID contracts.OrganizationID,
	seatID contracts.SeatID,
) contracts.Seat {
	return contracts.Seat{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            seatID, Version: 1,
		DID:            contracts.SeatDID("did:matrix:projectbrain:" + string(seatID)),
		OrganizationID: organizationID,
		DepartmentID:   "department:projectbrain",
		Role:           contracts.SeatExecutor,
		MandateID:      contracts.MandateID("mandate:projectbrain:" + string(seatID)),
		MandateVersion: 1,
		BindingID:      contracts.SeatBindingID("binding:projectbrain:" + string(seatID)),
		BindingVersion: 1,
		EffectiveAt:    projectBrainNow().Add(-time.Hour),
	}
}

func publishIntegrationProjectBrainSeat(
	t *testing.T,
	fixture *testauthority.Fixture,
	organizationID contracts.OrganizationID,
	seatID contracts.SeatID,
) {
	t.Helper()
	seat := integrationProjectBrainSeat(organizationID, seatID)
	request := lease.Request{
		ID:             contracts.LeaseID("lease:projectbrain:" + string(seatID)),
		WakeID:         contracts.WakeID("wake:projectbrain:" + string(seatID)),
		OrganizationID: organizationID,
		SeatID:         seatID,
		NodeID:         dependency.NodeID("intent:projectbrain:" + string(seatID)),
		MandateID:      seat.MandateID,
		MandateVersion: seat.MandateVersion,
		Policies: []contracts.PolicyRef{{
			ID:      contracts.PolicyID("policy:projectbrain:" + string(seatID)),
			Version: 1,
		}},
		IssuedAt:  projectBrainNow().Add(-time.Minute),
		ExpiresAt: projectBrainNow().Add(time.Hour),
	}
	mandate := contracts.Mandate{
		SchemaVersion: contracts.SchemaVersionV1,
		ID:            seat.MandateID, Version: seat.MandateVersion,
		OrganizationID: organizationID,
		DepartmentKind: contracts.DepartmentDeveloper,
		SeatRole:       seat.Role,
		AllowedSkills:  []contracts.SkillID{"projectbrain:test"},
		DataScopes: []contracts.DataScope{{
			Name: "projectbrain", Classification: contracts.ClassificationProject,
			Purpose: "Exercise current signed Project Brain authority",
		}},
		EscalationRules: []contracts.EscalationRule{{
			Condition: "Current authority is unavailable",
			Action:    "Fail closed",
		}},
		Prohibitions: []contracts.Prohibition{{
			ClauseID:    "no-stale-grants",
			Description: "Never accept a grant from a replaced seat",
		}},
		EffectiveAt: projectBrainNow().Add(-time.Hour),
	}
	if _, err := fixture.PublishExact(
		context.Background(), request, seat, mandate,
	); err != nil {
		t.Fatalf("publish Project Brain seat %s: %v", seatID, err)
	}
}

func grantTenantID(record EngineeringRecord) string {
	organizationID := string(record.Proposal.OrganizationID)
	if strings.HasPrefix(organizationID, "org-tenant-") {
		return strings.TrimPrefix(organizationID, "org-")
	}
	return "tenant-" + organizationID
}

func hashText(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func startProjectBrainPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", err
	}
	name := "workforce-projectbrain-" + hex.EncodeToString(suffix[:])
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "-d",
		"--name", name, "-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce", "-p", "127.0.0.1::5432",
		projectBrainPostgresImage,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("inspect PostgreSQL port: %w: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	_, port, found := strings.Cut(address, ":")
	if !found {
		return containerID, "", fmt.Errorf("unexpected PostgreSQL mapping %q", address)
	}
	return containerID,
		"postgres://postgres:workforce-test-password@127.0.0.1:" + port + "/workforce?sslmode=disable",
		nil
}

func waitForProjectBrainPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			if pingErr := pool.Ping(ctx); pingErr == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func projectBrainNow() time.Time {
	return projectBrainClock
}

var projectBrainClock = time.Now().UTC().Add(5 * time.Minute).Truncate(time.Microsecond)
