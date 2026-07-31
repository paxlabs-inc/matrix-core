package ledger

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"matrix/vault"

	"matrix/workforce/internal/contracts"
)

const postgresImage = "postgres@sha256:33f923b05f64ca54ac4401c01126a6b92afe839a0aa0a52bc5aeb5cc958e5f20"

var integrationPool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	containerID, databaseURL, err := startPostgres(ctx)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ledger integration setup:", err)
		os.Exit(1)
	}
	cleanup := func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer stopCancel()
		_ = exec.CommandContext(stopCtx, "docker", "rm", "-f", containerID).Run()
	}
	pool, err := waitForPostgres(ctx, databaseURL)
	if err != nil {
		cleanup()
		fmt.Fprintln(os.Stderr, "ledger integration setup:", err)
		os.Exit(1)
	}
	integrationPool = pool
	if err := ApplyMigrations(ctx, integrationPool, integrationNow()); err != nil {
		pool.Close()
		cleanup()
		fmt.Fprintln(os.Stderr, "ledger integration migrations:", err)
		os.Exit(1)
	}
	code := m.Run()
	pool.Close()
	cleanup()
	os.Exit(code)
}

func TestIntegration_Migrations_AreRestartableAndChecksummed(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	if err := ApplyMigrations(ctx, integrationPool, integrationNow()); err != nil {
		t.Fatalf("reapply migrations: %v", err)
	}
	var count int
	if err := integrationPool.QueryRow(ctx,
		`SELECT COUNT(*) FROM workforce_schema_migrations`,
	).Scan(&count); err != nil {
		t.Fatalf("count migrations: %v", err)
	}
	available, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if count != len(available) {
		t.Fatalf("applied migration count = %d, want %d", count, len(available))
	}
	if _, err := integrationPool.Exec(ctx, `
		UPDATE workforce_schema_migrations SET checksum = $2 WHERE version = $1
	`, available[0].version, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("simulate migration checksum drift: %v", err)
	}
	defer func() {
		_, err := integrationPool.Exec(context.Background(), `
			UPDATE workforce_schema_migrations SET checksum = $2 WHERE version = $1
		`, available[0].version, available[0].checksum)
		if err != nil {
			t.Errorf("restore migration checksum: %v", err)
		}
	}()
	if err := ApplyMigrations(ctx, integrationPool, integrationNow()); err == nil {
		t.Fatal("ApplyMigrations accepted checksum drift")
	}
}

func TestIntegration_LedgerAppendOpenAndVault_EnforcesImmutableTenantScope(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, userVault, scope := integrationStore(t, "append-open")
	source := integrationRecord(scope, "source", contracts.RecordFinding, nil)

	first, err := store.AppendRecord(ctx, AppendRequest{
		Record: source, IdempotencyKey: scope + "-append-source",
	})
	if err != nil {
		t.Fatalf("append source: %v", err)
	}
	if first.Deduplicated {
		t.Fatal("first append reported deduplication")
	}
	second, err := store.AppendRecord(ctx, AppendRequest{
		Record: source, IdempotencyKey: scope + "-append-source",
	})
	if err != nil {
		t.Fatalf("repeat append: %v", err)
	}
	if !second.Deduplicated || second.CanonicalHash != first.CanonicalHash {
		t.Fatalf("repeat append = %#v, want same hash and deduplicated", second)
	}
	third, err := store.AppendRecord(ctx, AppendRequest{
		Record: source, IdempotencyKey: scope + "-append-source-alias",
	})
	if err != nil {
		t.Fatalf("append same record under new key: %v", err)
	}
	if !third.Deduplicated || third.CanonicalHash != first.CanonicalHash {
		t.Fatalf("record-identity deduplication = %#v, want same hash and deduplicated", third)
	}

	grant := integrationGrant(scope, source)
	opened, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: source.OrganizationID,
		RecordID:       source.ID,
		Grant:          grant,
		IdempotencyKey: scope + "-open-source",
	})
	if err != nil {
		t.Fatalf("open source: %v", err)
	}
	if opened.ID != source.ID || opened.ContentHash != source.ContentHash {
		t.Fatalf("opened record changed identity: %#v", opened)
	}

	sealed := storedCiphertext(t, ctx, store, source)
	if !vault.IsVault(sealed) {
		t.Fatal("stored record is not Vault sealed")
	}
	if bytes.Contains(sealed, []byte(source.Purpose)) {
		t.Fatal("stored ciphertext exposes canonical record purpose")
	}
	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := userVault.OpenRecord(store.recordAD(source), tampered); err == nil {
		t.Fatal("Vault accepted tampered ledger ciphertext")
	}
	wrongAD := store.recordAD(source)
	wrongAD.Stream += "-wrong-project"
	if _, err := userVault.OpenRecord(wrongAD, sealed); err == nil {
		t.Fatal("Vault accepted ledger ciphertext under different associated data")
	}

	wrongGrant := grant
	wrongProject := contracts.ProjectID("project-other")
	wrongGrant.ProjectID = &wrongProject
	if _, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: source.OrganizationID,
		RecordID:       source.ID,
		Grant:          wrongGrant,
		IdempotencyKey: scope + "-open-denied",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cross-project read error = %v, want ErrNotFound", err)
	}
	var denials int
	if err := integrationPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_access_denials
		WHERE tenant_id = $1 AND organization_id = $2
	`, store.tenantID, source.OrganizationID).Scan(&denials); err != nil {
		t.Fatalf("count denials: %v", err)
	}
	if denials != 1 {
		t.Fatalf("denial audit count = %d, want 1", denials)
	}

	if _, err := integrationPool.Exec(ctx, `
		UPDATE workforce_records SET purpose = 'mutated'
		WHERE tenant_id = $1 AND organization_id = $2 AND record_id = $3
	`, store.tenantID, source.OrganizationID, source.ID); err == nil {
		t.Fatal("immutable record accepted an update")
	}
}

func TestIntegration_LedgerAppend_RollsBackMissingProvenanceAndRejectsConflicts(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "rollback")
	missing := integrationRecord(scope, "missing-source", contracts.RecordFinding, nil)
	derived := integrationRecord(scope, "derived", contracts.RecordDecision, []contracts.RecordRef{{
		ID: missing.ID, Kind: missing.Kind, Hash: missing.ContentHash,
	}})
	_, err := store.AppendRecord(ctx, AppendRequest{
		Record: derived, IdempotencyKey: scope + "-missing-provenance",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("append with missing provenance = %v, want ErrNotFound", err)
	}
	assertRecordCount(t, ctx, store, derived.OrganizationID, derived.ID, 0)

	source := integrationRecord(scope, "source", contracts.RecordFinding, nil)
	if _, err := store.AppendRecord(ctx, AppendRequest{
		Record: source, IdempotencyKey: scope + "-source",
	}); err != nil {
		t.Fatalf("append source: %v", err)
	}
	changed := source
	changed.Purpose = "different-purpose"
	if _, err := store.AppendRecord(ctx, AppendRequest{
		Record: changed, IdempotencyKey: scope + "-source",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("idempotency conflict = %v, want ErrConflict", err)
	}
	if _, err := store.AppendRecord(ctx, AppendRequest{
		Record: changed, IdempotencyKey: scope + "-different-key",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("record identity conflict = %v, want ErrConflict", err)
	}
}

func TestIntegration_LedgerAppend_ConcurrentWritersCommitOneCanonicalRecord(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "concurrent")
	record := integrationRecord(scope, "shared", contracts.RecordFinding, nil)

	const writers = 16
	results := make(chan AppendResult, writers)
	failures := make(chan error, writers)
	var group sync.WaitGroup
	for index := 0; index < writers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			result, err := store.AppendRecord(ctx, AppendRequest{
				Record: record, IdempotencyKey: scope + "-shared-key",
			})
			if err != nil {
				failures <- err
				return
			}
			results <- result
		}()
	}
	group.Wait()
	close(results)
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent append: %v", err)
	}
	created := 0
	deduplicated := 0
	for result := range results {
		if result.Deduplicated {
			deduplicated++
		} else {
			created++
		}
	}
	if created != 1 || deduplicated != writers-1 {
		t.Fatalf("created=%d deduplicated=%d, want 1/%d", created, deduplicated, writers-1)
	}
	assertRecordCount(t, ctx, store, record.OrganizationID, record.ID, 1)
}

func TestIntegration_ProvenanceAccessAndCorrection_ReachesEveryTransitiveConsumer(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "correction")
	source := integrationRecord(scope, "source", contracts.RecordFinding, nil)
	middle := integrationRecord(scope, "middle", contracts.RecordDecision, []contracts.RecordRef{{
		ID: source.ID, Kind: source.Kind, Hash: source.ContentHash,
	}})
	leaf := integrationRecord(scope, "leaf", contracts.RecordArtifact, []contracts.RecordRef{{
		ID: middle.ID, Kind: middle.Kind, Hash: middle.ContentHash,
	}})
	evidence := integrationRecord(scope, "evidence", contracts.RecordAttestation, nil)
	appendRecords(t, ctx, store, scope, source, middle, leaf, evidence)

	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID:   source.OrganizationID,
		SourceRecordID:   source.ID,
		ConsumerRecordID: &leaf.ID,
		Action:           AccessCitation,
		Grant:            integrationGrant(scope, source),
		IdempotencyKey:   scope + "-citation",
	}); err != nil {
		t.Fatalf("record citation: %v", err)
	}
	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID:   source.OrganizationID,
		SourceRecordID:   source.ID,
		ConsumerRecordID: &leaf.ID,
		Action:           AccessCitation,
		Grant:            integrationGrant(scope, source),
		IdempotencyKey:   scope + "-citation",
	}); err != nil {
		t.Fatalf("repeat citation: %v", err)
	}

	correction := integrationRecord(scope, "correction", contracts.RecordCorrection, []contracts.RecordRef{{
		ID: source.ID, Kind: source.Kind, Hash: source.ContentHash,
	}})
	status, err := store.CreateCorrection(ctx, CorrectionRequest{
		ID:               contracts.CorrectionID(scope + "-correction"),
		SourceRecordID:   source.ID,
		CorrectionRecord: correction,
		IdempotencyKey:   scope + "-append-correction",
		MateriallyUnsafe: true,
	})
	if err != nil {
		t.Fatalf("create correction: %v", err)
	}
	if status.Status != "open" || status.Pending != 3 || status.Paused != 3 {
		t.Fatalf("initial correction status = %#v, want 3 pending and paused", status)
	}
	var notices int
	if err := integrationPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_correction_notices
		WHERE tenant_id = $1 AND organization_id = $2 AND correction_id = $3
	`, store.tenantID, source.OrganizationID, status.ID).Scan(&notices); err != nil {
		t.Fatalf("count correction notices: %v", err)
	}
	if notices != 3 {
		t.Fatalf("correction notices = %d, want 3", notices)
	}

	status = reconcile(t, ctx, store, status.ID, source, ReconciliationApplied, nil, scope+"-resolve-source")
	if status.Status != "open" || status.Pending != 2 || status.Paused != 2 {
		t.Fatalf("status after source apply = %#v", status)
	}
	status = reconcile(t, ctx, store, status.ID, middle, ReconciliationRejected, &evidence.ID, scope+"-resolve-middle")
	if status.Status != "open" || status.Pending != 1 || status.Paused != 1 {
		t.Fatalf("status after middle reject = %#v", status)
	}
	status = reconcile(t, ctx, store, status.ID, leaf, ReconciliationEscalated, &evidence.ID, scope+"-resolve-leaf")
	if status.Status != "escalated" || status.Pending != 0 || status.Paused != 0 ||
		status.Applied != 1 || status.Rejected != 1 || status.Escalated != 1 {
		t.Fatalf("terminal correction status = %#v", status)
	}
	repeated := reconcile(t, ctx, store, status.ID, leaf, ReconciliationEscalated, &evidence.ID, scope+"-resolve-leaf")
	if repeated != status {
		t.Fatalf("idempotent reconciliation changed status: %#v != %#v", repeated, status)
	}
	if _, err := store.ReconcileCorrection(ctx, ReconcileRequest{
		OrganizationID:   source.OrganizationID,
		CorrectionID:     status.ID,
		AffectedRecordID: leaf.ID,
		State:            ReconciliationApplied,
		IdempotencyKey:   scope + "-different-resolution",
	}); !errors.Is(err, ErrCorrectionClosed) {
		t.Fatalf("changed terminal resolution = %v, want ErrCorrectionClosed", err)
	}
}

func reconcile(
	t *testing.T,
	ctx context.Context,
	store *Store,
	correctionID contracts.CorrectionID,
	record contracts.Record,
	state ReconciliationState,
	evidenceID *contracts.RecordID,
	idempotencyKey string,
) CorrectionStatus {
	t.Helper()
	status, err := store.ReconcileCorrection(ctx, ReconcileRequest{
		OrganizationID:   record.OrganizationID,
		CorrectionID:     correctionID,
		AffectedRecordID: record.ID,
		State:            state,
		EvidenceRecordID: evidenceID,
		IdempotencyKey:   idempotencyKey,
	})
	if err != nil {
		t.Fatalf("reconcile %s as %s: %v", record.ID, state, err)
	}
	return status
}

func appendRecords(
	t *testing.T,
	ctx context.Context,
	store *Store,
	scope string,
	records ...contracts.Record,
) {
	t.Helper()
	for index, record := range records {
		if _, err := store.AppendRecord(ctx, AppendRequest{
			Record:         record,
			IdempotencyKey: scope + "-append-" + strconv.Itoa(index),
		}); err != nil {
			t.Fatalf("append %s: %v", record.ID, err)
		}
	}
}

func integrationStore(t *testing.T, label string) (*Store, *vault.UserVault, string) {
	t.Helper()
	scope := testScope(t, label)
	tenantID := "tenant-" + scope
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	session, err := vault.Boot(context.Background(), vault.Config{
		Required: true,
		DataDir:  t.TempDir(),
		UserDID:  tenantID,
		KEKHex:   kek,
	})
	if err != nil {
		t.Fatalf("boot real Vault: %v", err)
	}
	if !session.Encrypting() || session.UserVault() == nil {
		t.Fatal("real Vault session is not encrypting")
	}
	store, err := New(integrationPool, session.UserVault(), tenantID, integrationNow)
	if err != nil {
		t.Fatalf("construct ledger: %v", err)
	}
	return store, session.UserVault(), scope
}

func integrationRecord(
	scope string,
	label string,
	kind contracts.RecordKind,
	provenance []contracts.RecordRef,
) contracts.Record {
	organizationID := contracts.OrganizationID("org-" + scope)
	departmentID := contracts.DepartmentID("department-developer")
	projectID := contracts.ProjectID("project-" + scope)
	recordID := contracts.RecordID(scope + "-" + label)
	return contracts.Record{
		SchemaVersion:  contracts.SchemaVersionV1,
		ID:             recordID,
		OrganizationID: organizationID,
		Kind:           kind,
		AuthorSeatID:   contracts.SeatID(scope + "-developer"),
		DepartmentID:   &departmentID,
		ProjectID:      &projectID,
		Purpose:        "ledger-" + scope,
		ParentIntentID: contracts.IntentID(scope + "-intent"),
		CreatedAt:      integrationNow(),
		EffectiveAt:    integrationNow(),
		Validity:       contracts.ValidityActive,
		PayloadSchema:  "workforce.record." + string(kind) + ".v1",
		Payload: contracts.ArtifactRef{
			SchemaVersion: contracts.SchemaVersionV1,
			ID:            contracts.ArtifactID(recordID + "-payload"),
			Hash:          hashFor(string(recordID) + "-payload"),
			MediaType:     "application/json",
			SizeBytes:     128,
		},
		ContentHash:    hashFor(string(recordID) + "-content"),
		Provenance:     provenance,
		Classification: contracts.ClassificationProject,
		Signature: contracts.Signature{
			Algorithm: "ed25519",
			KeyID:     scope + "-owner-key",
			Value: base64.RawURLEncoding.EncodeToString(
				make([]byte, ed25519.SignatureSize),
			),
		},
	}
}

func integrationGrant(scope string, record contracts.Record) AccessGrant {
	return AccessGrant{
		OrganizationID: record.OrganizationID,
		SeatID:         contracts.SeatID(scope + "-reader"),
		ProjectID:      record.ProjectID,
		Purpose:        record.Purpose,
		Classifications: []contracts.Classification{
			contracts.ClassificationProject,
		},
		ExpiresAt: integrationNow().Add(time.Hour),
	}
}

func hashFor(value string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(value))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func storedCiphertext(
	t *testing.T,
	ctx context.Context,
	store *Store,
	record contracts.Record,
) []byte {
	t.Helper()
	var sealed []byte
	if err := integrationPool.QueryRow(ctx, `
		SELECT sealed_record FROM workforce_records
		WHERE tenant_id = $1 AND organization_id = $2 AND record_id = $3
	`, store.tenantID, record.OrganizationID, record.ID).Scan(&sealed); err != nil {
		t.Fatalf("read sealed record: %v", err)
	}
	return sealed
}

func assertRecordCount(
	t *testing.T,
	ctx context.Context,
	store *Store,
	organizationID contracts.OrganizationID,
	recordID contracts.RecordID,
	want int,
) {
	t.Helper()
	var count int
	if err := integrationPool.QueryRow(ctx, `
		SELECT COUNT(*) FROM workforce_records
		WHERE tenant_id = $1 AND organization_id = $2 AND record_id = $3
	`, store.tenantID, organizationID, recordID).Scan(&count); err != nil {
		t.Fatalf("count records: %v", err)
	}
	if count != want {
		t.Fatalf("record count = %d, want %d", count, want)
	}
}

func startPostgres(ctx context.Context) (string, string, error) {
	var suffix [6]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return "", "", fmt.Errorf("random container suffix: %w", err)
	}
	name := "workforce-ledger-" + hex.EncodeToString(suffix[:])
	command := exec.CommandContext(ctx, "docker", "run", "--rm", "-d",
		"--name", name,
		"-e", "POSTGRES_PASSWORD=workforce-test-password",
		"-e", "POSTGRES_DB=workforce",
		"-p", "127.0.0.1::5432",
		postgresImage,
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return "", "", fmt.Errorf("start PostgreSQL container: %w: %s", err, output)
	}
	containerID := strings.TrimSpace(string(output))
	portOutput, err := exec.CommandContext(ctx, "docker", "port", containerID, "5432/tcp").CombinedOutput()
	if err != nil {
		return containerID, "", fmt.Errorf("inspect PostgreSQL port: %w: %s", err, portOutput)
	}
	address := strings.TrimSpace(string(portOutput))
	_, port, found := strings.Cut(address, ":")
	if !found || port == "" {
		return containerID, "", fmt.Errorf("unexpected PostgreSQL port mapping %q", address)
	}
	return containerID,
		"postgres://postgres:workforce-test-password@127.0.0.1:" + port + "/workforce?sslmode=disable",
		nil
}

func waitForPostgres(ctx context.Context, databaseURL string) (*pgxpool.Pool, error) {
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		pool, err := pgxpool.New(ctx, databaseURL)
		if err == nil {
			pingErr := pool.Ping(ctx)
			if pingErr == nil {
				return pool, nil
			}
			pool.Close()
		}
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("wait for PostgreSQL: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func testContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	return context.WithTimeout(context.Background(), 30*time.Second)
}

func testScope(t *testing.T, label string) string {
	t.Helper()
	sum := sha256.Sum256([]byte(t.Name() + "|" + label))
	return label + "-" + hex.EncodeToString(sum[:6])
}

func integrationNow() time.Time {
	return time.Date(2026, time.July, 30, 15, 0, 0, 0, time.UTC)
}
