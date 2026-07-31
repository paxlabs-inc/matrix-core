package ledger

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"matrix/workforce/internal/contracts"
)

func TestIntegration_LedgerPublicValidationAndAccessPaths(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "public-paths")
	record := integrationRecord(scope, "source", contracts.RecordFinding, nil)
	consumerOne := integrationRecord(scope, "consumer-one", contracts.RecordDecision, nil)
	consumerTwo := integrationRecord(scope, "consumer-two", contracts.RecordDecision, nil)
	appendRecords(t, ctx, store, scope, record, consumerOne, consumerTwo)

	for _, request := range []AppendRequest{
		{Record: record},
		{Record: record, IdempotencyKey: "not/valid"},
	} {
		if _, err := store.AppendRecord(ctx, request); err == nil {
			t.Fatalf("AppendRecord accepted invalid request %#v", request)
		}
	}
	invalidRecord := record
	invalidRecord.ID = ""
	if _, err := store.AppendRecord(ctx, AppendRequest{
		Record: invalidRecord, IdempotencyKey: scope + "-invalid-record",
	}); err == nil {
		t.Fatal("AppendRecord accepted an invalid record")
	}

	for _, request := range []OpenRequest{
		{OrganizationID: "", RecordID: record.ID, IdempotencyKey: "key"},
		{OrganizationID: record.OrganizationID, RecordID: "", IdempotencyKey: "key"},
		{OrganizationID: record.OrganizationID, RecordID: record.ID},
	} {
		if _, err := store.OpenRecord(ctx, request); err == nil {
			t.Fatalf("OpenRecord accepted invalid request %#v", request)
		}
	}
	if _, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       "missing",
		Grant:          integrationGrant(scope, record),
		IdempotencyKey: scope + "-missing",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing open error = %v, want ErrNotFound", err)
	}
	expired := integrationGrant(scope, record)
	expired.ExpiresAt = integrationNow()
	if _, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       record.ID,
		Grant:          expired,
		IdempotencyKey: scope + "-expired",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("expired open error = %v, want ErrNotFound", err)
	}
	grant := integrationGrant(scope, record)
	if _, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       record.ID,
		Grant:          grant,
		IdempotencyKey: scope + "-open-conflict",
	}); err != nil {
		t.Fatalf("first open: %v", err)
	}
	if _, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: consumerOne.OrganizationID,
		RecordID:       consumerOne.ID,
		Grant:          integrationGrant(scope, consumerOne),
		IdempotencyKey: scope + "-open-conflict",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("open idempotency conflict = %v, want ErrConflict", err)
	}

	for _, request := range []AccessRequest{
		{Action: AccessOpen, IdempotencyKey: "key"},
		{Action: AccessAction("unknown"), IdempotencyKey: "key"},
		{Action: AccessDelivery},
	} {
		if err := store.RecordAccess(ctx, request); err == nil {
			t.Fatalf("RecordAccess accepted invalid request %#v", request)
		}
	}
	missingRequest := AccessRequest{
		OrganizationID: record.OrganizationID,
		SourceRecordID: "missing",
		Action:         AccessDelivery,
		Grant:          grant,
		IdempotencyKey: scope + "-missing-access",
	}
	if err := store.RecordAccess(ctx, missingRequest); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing access source error = %v, want ErrNotFound", err)
	}
	denied := AccessRequest{
		OrganizationID: record.OrganizationID,
		SourceRecordID: record.ID,
		Action:         AccessDelivery,
		Grant:          AccessGrant{},
		IdempotencyKey: scope + "-denied-access",
	}
	if err := store.RecordAccess(ctx, denied); !errors.Is(err, ErrNotFound) {
		t.Fatalf("denied access error = %v, want ErrNotFound", err)
	}
	for _, action := range []AccessAction{AccessCitation, AccessDerivation} {
		if err := store.RecordAccess(ctx, AccessRequest{
			OrganizationID: record.OrganizationID,
			SourceRecordID: record.ID,
			Action:         action,
			Grant:          grant,
			IdempotencyKey: scope + "-missing-consumer-" + string(action),
		}); err == nil {
			t.Fatalf("%s accepted no consumer", action)
		}
	}
	missingConsumer := contracts.RecordID("missing-consumer")
	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID:   record.OrganizationID,
		SourceRecordID:   record.ID,
		ConsumerRecordID: &missingConsumer,
		Action:           AccessCitation,
		Grant:            grant,
		IdempotencyKey:   scope + "-missing-consumer",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing consumer error = %v, want ErrNotFound", err)
	}
	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID: record.OrganizationID,
		SourceRecordID: record.ID,
		Action:         AccessDelivery,
		Grant:          grant,
		IdempotencyKey: scope + "-delivery",
	}); err != nil {
		t.Fatalf("delivery access: %v", err)
	}
	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID: record.OrganizationID,
		SourceRecordID: record.ID,
		Action:         AccessDelivery,
		Grant:          grant,
		IdempotencyKey: scope + "-delivery",
	}); err != nil {
		t.Fatalf("repeat delivery access: %v", err)
	}
	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID:   record.OrganizationID,
		SourceRecordID:   record.ID,
		ConsumerRecordID: &consumerOne.ID,
		Action:           AccessDerivation,
		Grant:            grant,
		IdempotencyKey:   scope + "-derivation",
	}); err != nil {
		t.Fatalf("derivation access: %v", err)
	}
	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID:   record.OrganizationID,
		SourceRecordID:   record.ID,
		ConsumerRecordID: &consumerTwo.ID,
		Action:           AccessDerivation,
		Grant:            grant,
		IdempotencyKey:   scope + "-derivation",
	}); !errors.Is(err, ErrConflict) {
		t.Fatalf("access idempotency conflict = %v, want ErrConflict", err)
	}
}

func TestIntegration_LedgerClassificationReads(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "classification-reads")
	department := contracts.DepartmentID("department-developer")
	seat := contracts.SeatID(scope + "-reader")
	project := contracts.ProjectID("project-" + scope)
	classifications := []contracts.Classification{
		contracts.ClassificationOrganization,
		contracts.ClassificationDepartment,
		contracts.ClassificationSeat,
		contracts.ClassificationProject,
		contracts.ClassificationRestricted,
	}
	for index, classification := range classifications {
		record := integrationRecord(
			scope,
			"class-"+string(classification),
			contracts.RecordFinding,
			nil,
		)
		record.Classification = classification
		record.DepartmentID = &department
		record.AccessSeatID = &seat
		record.ProjectID = &project
		if _, err := store.AppendRecord(ctx, AppendRequest{
			Record: record, IdempotencyKey: scope + "-class-append-" + string(classification),
		}); err != nil {
			t.Fatalf("append %s record: %v", classification, err)
		}
		grant := AccessGrant{
			OrganizationID:  record.OrganizationID,
			SeatID:          seat,
			DepartmentID:    &department,
			ProjectID:       &project,
			Purpose:         record.Purpose,
			Classifications: []contracts.Classification{classification},
			Restricted:      true,
			ExpiresAt:       integrationNow().Add(time.Hour),
		}
		if _, err := store.OpenRecord(ctx, OpenRequest{
			OrganizationID: record.OrganizationID,
			RecordID:       record.ID,
			Grant:          grant,
			IdempotencyKey: scope + "-class-open-" + string(rune('a'+index)),
		}); err != nil {
			t.Fatalf("open %s record: %v", classification, err)
		}
	}
}

func TestIntegration_LedgerOpenRejectsTamperedStoredIntegrity(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "stored-integrity")
	record := integrationRecord(scope, "record", contracts.RecordFinding, nil)
	appendRecords(t, ctx, store, scope, record)
	if _, err := integrationPool.Exec(
		ctx,
		`ALTER TABLE workforce_records DISABLE TRIGGER workforce_records_immutable`,
	); err != nil {
		t.Fatalf("disable immutable trigger for tamper simulation: %v", err)
	}
	defer func() {
		_, err := integrationPool.Exec(
			context.Background(),
			`ALTER TABLE workforce_records ENABLE TRIGGER workforce_records_immutable`,
		)
		if err != nil {
			t.Errorf("restore immutable trigger: %v", err)
		}
	}()
	if _, err := integrationPool.Exec(ctx, `
		UPDATE workforce_records
		SET canonical_hash = $4
		WHERE tenant_id = $1 AND organization_id = $2 AND record_id = $3
	`, store.tenantID, record.OrganizationID, record.ID, strings.Repeat("0", 64)); err != nil {
		t.Fatalf("simulate stored hash tamper: %v", err)
	}
	if _, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       record.ID,
		Grant:          integrationGrant(scope, record),
		IdempotencyKey: scope + "-tampered-open",
	}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered stored record error = %v, want ErrIntegrity", err)
	}
}

func TestIntegration_CorrectionValidationSafeClosureAndConflicts(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "correction-paths")
	source := integrationRecord(scope, "source", contracts.RecordFinding, nil)
	evidence := integrationRecord(scope, "evidence", contracts.RecordAttestation, nil)
	appendRecords(t, ctx, store, scope, source, evidence)
	correction := integrationRecord(scope, "correction", contracts.RecordCorrection, []contracts.RecordRef{{
		ID: source.ID, Kind: source.Kind, Hash: source.ContentHash,
	}})
	valid := CorrectionRequest{
		ID:               contracts.CorrectionID(scope + "-correction"),
		SourceRecordID:   source.ID,
		CorrectionRecord: correction,
		IdempotencyKey:   scope + "-correction-append",
	}
	for _, mutate := range []func(*CorrectionRequest){
		func(value *CorrectionRequest) { value.ID = "" },
		func(value *CorrectionRequest) { value.SourceRecordID = "" },
		func(value *CorrectionRequest) { value.IdempotencyKey = "" },
		func(value *CorrectionRequest) { value.CorrectionRecord.Kind = contracts.RecordFinding },
		func(value *CorrectionRequest) { value.CorrectionRecord.Provenance = nil },
	} {
		invalid := valid
		mutate(&invalid)
		if _, err := store.CreateCorrection(ctx, invalid); err == nil {
			t.Fatalf("CreateCorrection accepted invalid request %#v", invalid)
		}
	}
	missingSource := integrationRecord(
		scope,
		"missing-correction",
		contracts.RecordCorrection,
		[]contracts.RecordRef{{
			ID:   "missing-source",
			Kind: contracts.RecordFinding,
			Hash: hashFor("missing-source"),
		}},
	)
	if _, err := store.CreateCorrection(ctx, CorrectionRequest{
		ID:               contracts.CorrectionID(scope + "-missing-correction"),
		SourceRecordID:   "missing-source",
		CorrectionRecord: missingSource,
		IdempotencyKey:   scope + "-missing-correction-key",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing correction source error = %v, want ErrNotFound", err)
	}

	status, err := store.CreateCorrection(ctx, valid)
	if err != nil {
		t.Fatalf("create safe correction: %v", err)
	}
	if status.Pending != 1 || status.Paused != 0 || status.Status != "open" {
		t.Fatalf("safe correction status = %#v", status)
	}
	repeated, err := store.CreateCorrection(ctx, valid)
	if err != nil || repeated != status {
		t.Fatalf("repeat correction = %#v, %v; want %#v", repeated, err, status)
	}
	tx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin target failure transaction: %v", err)
	}
	missingTargetRequest := valid
	missingTargetRequest.SourceRecordID = "absent-source"
	if err := store.insertCorrectionTargets(
		ctx,
		tx,
		missingTargetRequest,
		integrationNow(),
	); !errors.Is(err, ErrNotFound) {
		_ = tx.Rollback(ctx)
		t.Fatalf("empty correction target set error = %v, want ErrNotFound", err)
	}
	_ = tx.Rollback(ctx)
	tx, err = integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin duplicate target transaction: %v", err)
	}
	if err := store.insertCorrectionTargets(
		ctx,
		tx,
		valid,
		integrationNow(),
	); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("duplicate correction targets were inserted")
	}
	_ = tx.Rollback(ctx)
	conflict := valid
	conflict.MateriallyUnsafe = true
	if _, err := store.CreateCorrection(ctx, conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("correction identity conflict = %v, want ErrConflict", err)
	}

	for _, mutate := range []func(*ReconcileRequest){
		func(value *ReconcileRequest) { value.OrganizationID = "" },
		func(value *ReconcileRequest) { value.CorrectionID = "" },
		func(value *ReconcileRequest) { value.AffectedRecordID = "" },
		func(value *ReconcileRequest) { value.IdempotencyKey = "" },
		func(value *ReconcileRequest) { value.State = "unknown" },
		func(value *ReconcileRequest) {
			value.State = ReconciliationRejected
			value.EvidenceRecordID = nil
		},
	} {
		invalid := ReconcileRequest{
			OrganizationID:   source.OrganizationID,
			CorrectionID:     status.ID,
			AffectedRecordID: source.ID,
			State:            ReconciliationApplied,
			IdempotencyKey:   scope + "-resolution",
		}
		mutate(&invalid)
		if _, err := store.ReconcileCorrection(ctx, invalid); err == nil {
			t.Fatalf("ReconcileCorrection accepted invalid request %#v", invalid)
		}
	}
	missingEvidence := contracts.RecordID("missing-evidence")
	if _, err := store.ReconcileCorrection(ctx, ReconcileRequest{
		OrganizationID:   source.OrganizationID,
		CorrectionID:     status.ID,
		AffectedRecordID: source.ID,
		State:            ReconciliationRejected,
		EvidenceRecordID: &missingEvidence,
		IdempotencyKey:   scope + "-missing-evidence",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing evidence error = %v, want ErrNotFound", err)
	}
	if _, err := store.ReconcileCorrection(ctx, ReconcileRequest{
		OrganizationID:   source.OrganizationID,
		CorrectionID:     status.ID,
		AffectedRecordID: "not-a-target",
		State:            ReconciliationApplied,
		IdempotencyKey:   scope + "-missing-target",
	}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing target error = %v, want ErrNotFound", err)
	}
	status, err = store.ReconcileCorrection(ctx, ReconcileRequest{
		OrganizationID:   source.OrganizationID,
		CorrectionID:     status.ID,
		AffectedRecordID: source.ID,
		State:            ReconciliationApplied,
		IdempotencyKey:   scope + "-apply",
	})
	if err != nil {
		t.Fatalf("apply correction: %v", err)
	}
	if status.Status != "closed" || status.Applied != 1 || status.Pending != 0 {
		t.Fatalf("closed correction status = %#v", status)
	}
	projected, err := store.CorrectionStatus(ctx, source.OrganizationID, status.ID)
	if err != nil || projected != status {
		t.Fatalf("CorrectionStatus = %#v, %v; want %#v", projected, err, status)
	}
	if _, err := store.CorrectionStatus(
		ctx,
		source.OrganizationID,
		contracts.CorrectionID(scope+"-missing"),
	); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing correction status error = %v, want ErrNotFound", err)
	}
}

func TestLedgerNonUTCTimeFailsClosedAtEveryEntryPoint(t *testing.T) {
	store, _, scope := integrationStore(t, "non-utc")
	store.now = func() time.Time {
		return integrationNow().In(time.FixedZone("non-utc", 3600))
	}
	record := integrationRecord(scope, "record", contracts.RecordFinding, nil)
	correction := integrationRecord(scope, "correction", contracts.RecordCorrection, []contracts.RecordRef{{
		ID: record.ID, Kind: record.Kind, Hash: record.ContentHash,
	}})
	if _, err := store.AppendRecord(context.Background(), AppendRequest{
		Record: record, IdempotencyKey: "key",
	}); err == nil {
		t.Fatal("AppendRecord accepted non-UTC time")
	}
	if _, err := store.OpenRecord(context.Background(), OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       record.ID,
		IdempotencyKey: "key",
	}); err == nil {
		t.Fatal("OpenRecord accepted non-UTC time")
	}
	if err := store.RecordAccess(context.Background(), AccessRequest{
		Action: AccessDelivery, IdempotencyKey: "key",
	}); err == nil {
		t.Fatal("RecordAccess accepted non-UTC time")
	}
	if _, err := store.CreateCorrection(context.Background(), CorrectionRequest{
		ID:               "correction",
		SourceRecordID:   record.ID,
		CorrectionRecord: correction,
		IdempotencyKey:   "key",
	}); err == nil {
		t.Fatal("CreateCorrection accepted non-UTC time")
	}
	if _, err := store.ReconcileCorrection(context.Background(), ReconcileRequest{
		OrganizationID:   record.OrganizationID,
		CorrectionID:     "correction",
		AffectedRecordID: record.ID,
		State:            ReconciliationApplied,
		IdempotencyKey:   "key",
	}); err == nil {
		t.Fatal("ReconcileCorrection accepted non-UTC time")
	}
}

func TestLedgerMigrationValidationAndLoading(t *testing.T) {
	if err := ApplyMigrations(context.Background(), nil, integrationNow()); err == nil {
		t.Fatal("ApplyMigrations accepted nil pool")
	}
	for _, now := range []time.Time{
		{},
		integrationNow().In(time.FixedZone("non-utc", 3600)),
	} {
		if err := ApplyMigrations(context.Background(), integrationPool, now); err == nil {
			t.Fatalf("ApplyMigrations accepted invalid time %v", now)
		}
	}
	loaded, err := loadMigrations()
	if err != nil {
		t.Fatalf("load migrations: %v", err)
	}
	if len(loaded) == 0 || loaded[0].version <= 0 ||
		!strings.HasSuffix(loaded[0].name, ".sql") ||
		len(loaded[0].checksum) != 64 || loaded[0].sql == "" {
		t.Fatalf("invalid loaded migration %#v", loaded)
	}
}

func TestLedgerMigrationLoaderRejectsInvalidRealFiles(t *testing.T) {
	root := t.TempDir()
	migrations := filepath.Join(root, "migrations")
	if err := os.Mkdir(migrations, 0o700); err != nil {
		t.Fatalf("create migrations directory: %v", err)
	}
	write := func(name, content string) {
		t.Helper()
		if err := os.WriteFile(
			filepath.Join(migrations, name),
			[]byte(content),
			0o600,
		); err != nil {
			t.Fatalf("write migration %s: %v", name, err)
		}
	}
	write("002_second.sql", "SELECT 2")
	write("001_first.sql", "SELECT 1")
	write("README.txt", "ignored")
	if err := os.Mkdir(filepath.Join(migrations, "003_directory.sql"), 0o700); err != nil {
		t.Fatalf("create ignored directory: %v", err)
	}
	loaded, err := loadMigrationsFrom(os.DirFS(root), "migrations")
	if err != nil {
		t.Fatalf("load real migration files: %v", err)
	}
	if len(loaded) != 2 || loaded[0].version != 1 || loaded[1].version != 2 {
		t.Fatalf("migration order = %#v", loaded)
	}
	write("invalid.sql", "SELECT 3")
	if _, err := loadMigrationsFrom(os.DirFS(root), "migrations"); err == nil {
		t.Fatal("loader accepted an invalid migration file name")
	}
	if err := os.Remove(filepath.Join(migrations, "invalid.sql")); err != nil {
		t.Fatalf("remove invalid migration: %v", err)
	}
	write("001_duplicate.sql", "SELECT 3")
	if _, err := loadMigrationsFrom(os.DirFS(root), "migrations"); err == nil {
		t.Fatal("loader accepted a duplicate migration version")
	}
	if err := os.Remove(filepath.Join(migrations, "001_duplicate.sql")); err != nil {
		t.Fatalf("remove duplicate migration: %v", err)
	}
	if err := os.Symlink(
		"missing-target",
		filepath.Join(migrations, "003_unreadable.sql"),
	); err != nil {
		t.Fatalf("create broken migration symlink: %v", err)
	}
	if _, err := loadMigrationsFrom(os.DirFS(root), "migrations"); err == nil {
		t.Fatal("loader accepted an unreadable migration")
	}
	if _, err := loadMigrationsFrom(os.DirFS(root), "missing"); err == nil {
		t.Fatal("loader accepted a missing migration directory")
	}
}
