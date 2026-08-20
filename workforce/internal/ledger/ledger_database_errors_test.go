package ledger

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"centra/workforce/internal/contracts"
)

func TestIntegration_LedgerDatabaseErrorsFailClosed(t *testing.T) {
	store, _, scope := integrationStore(t, "database-errors")
	record := integrationRecord(scope, "record", contracts.RecordFinding, nil)
	prepared, err := store.prepareRecord(record)
	if err != nil {
		t.Fatalf("prepare record: %v", err)
	}
	request := CorrectionRequest{
		ID:             contracts.CorrectionID(scope + "-correction"),
		SourceRecordID: record.ID,
		CorrectionRecord: integrationRecord(scope, "correction", contracts.RecordCorrection, []contracts.RecordRef{{
			ID: record.ID, Kind: record.Kind, Hash: record.ContentHash,
		}}),
		IdempotencyKey: scope + "-correction-key",
	}
	reconcileRequest := ReconcileRequest{
		OrganizationID:   record.OrganizationID,
		CorrectionID:     request.ID,
		AffectedRecordID: record.ID,
		State:            ReconciliationApplied,
		IdempotencyKey:   scope + "-resolution-key",
	}
	metadata := recordMetadata{
		organizationID: record.OrganizationID,
		recordID:       record.ID,
	}
	targets := []correctionTarget{{recordID: record.ID, seatID: record.AuthorSeatID}}

	tests := []struct {
		name string
		call func(context.Context, pgx.Tx) error
	}{
		{name: "find metadata", call: func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.findMetadata(ctx, tx, record.OrganizationID, record.ID)
			return err
		}},
		{name: "insert denial", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.insertDenial(ctx, tx, OpenRequest{
				OrganizationID: record.OrganizationID,
				RecordID:       record.ID,
			}, integrationNow())
		}},
		{name: "commit denial", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.commitDenial(ctx, tx, OpenRequest{
				OrganizationID: record.OrganizationID,
				RecordID:       record.ID,
			}, integrationNow())
		}},
		{name: "insert access edge", call: func(ctx context.Context, tx pgx.Tx) error {
			return insertAccessEdge(
				ctx,
				tx,
				store.tenantID,
				metadata,
				nil,
				AccessDelivery,
				record.AuthorSeatID,
				record.Purpose,
				scope+"-access-key",
				integrationNow(),
			)
		}},
		{name: "lock append", call: func(ctx context.Context, tx pgx.Tx) error {
			return lockAppendIdentities(ctx, tx, store.tenantID, record, scope+"-append-key")
		}},
		{name: "append prepared", call: func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.appendPreparedTx(
				ctx, tx, prepared, scope+"-append-key", integrationNow(),
			)
			return err
		}},
		{name: "find append key", call: func(ctx context.Context, tx pgx.Tx) error {
			_, _, err := findAppendKey(
				ctx, tx, store.tenantID, record.OrganizationID, scope+"-append-key",
			)
			return err
		}},
		{name: "find record identity", call: func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.findRecordIdentity(ctx, tx, prepared)
			return err
		}},
		{name: "insert record", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.insertRecord(ctx, tx, prepared)
		}},
		{name: "insert provenance", call: func(ctx context.Context, tx pgx.Tx) error {
			withProvenance := record
			withProvenance.Provenance = []contracts.RecordRef{{
				ID: record.ID, Kind: record.Kind, Hash: record.ContentHash,
			}}
			return store.insertProvenance(ctx, tx, withProvenance, integrationNow())
		}},
		{name: "insert append key", call: func(ctx context.Context, tx pgx.Tx) error {
			return insertAppendKey(
				ctx,
				tx,
				store.tenantID,
				record,
				prepared,
				scope+"-append-key",
				integrationNow(),
			)
		}},
		{name: "lock correction", call: func(ctx context.Context, tx pgx.Tx) error {
			return lockCorrection(ctx, tx, store.tenantID, request)
		}},
		{name: "find correction", call: func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.findCorrection(ctx, tx, request)
			return err
		}},
		{name: "insert correction", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.insertCorrection(ctx, tx, request, integrationNow())
		}},
		{name: "insert correction targets", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.insertCorrectionTargets(ctx, tx, request, integrationNow())
		}},
		{name: "persist correction targets", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.persistCorrectionTargets(ctx, tx, request, targets, integrationNow())
		}},
		{name: "apply resolution", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.applyResolution(ctx, tx, reconcileRequest, integrationNow())
		}},
		{name: "verify resolution", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.verifyExistingResolution(ctx, tx, reconcileRequest)
		}},
		{name: "close correction", call: func(ctx context.Context, tx pgx.Tx) error {
			return store.closeResolvedCorrection(ctx, tx, reconcileRequest, integrationNow())
		}},
		{name: "correction status", call: func(ctx context.Context, tx pgx.Tx) error {
			_, err := store.correctionStatusTx(
				ctx, tx, record.OrganizationID, request.ID,
			)
			return err
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			tx, err := integrationPool.Begin(context.Background())
			if err != nil {
				t.Fatalf("begin real transaction: %v", err)
			}
			defer func() { _ = tx.Rollback(context.Background()) }()
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			if err := test.call(ctx, tx); err == nil {
				t.Fatal("database operation accepted a canceled context")
			}
		})
	}
}

func TestIntegration_LedgerClosedPoolFailsEveryTransactionEntry(t *testing.T) {
	store, userVault, scope := integrationStore(t, "closed-pool")
	closedPool, err := pgxpool.New(
		context.Background(),
		integrationPool.Config().ConnString(),
	)
	if err != nil {
		t.Fatalf("construct pool: %v", err)
	}
	closedPool.Close()
	closedStore, err := New(closedPool, userVault, store.tenantID, integrationNow)
	if err != nil {
		t.Fatalf("construct store over closed pool: %v", err)
	}
	record := integrationRecord(scope, "record", contracts.RecordFinding, nil)
	correction := integrationRecord(scope, "correction", contracts.RecordCorrection, []contracts.RecordRef{{
		ID: record.ID, Kind: record.Kind, Hash: record.ContentHash,
	}})
	if _, err := closedStore.AppendRecord(context.Background(), AppendRequest{
		Record: record, IdempotencyKey: "key",
	}); err == nil {
		t.Fatal("append began on a closed pool")
	}
	if _, err := closedStore.OpenRecord(context.Background(), OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       record.ID,
		IdempotencyKey: "key",
	}); err == nil {
		t.Fatal("open began on a closed pool")
	}
	if err := closedStore.RecordAccess(context.Background(), AccessRequest{
		Action: AccessDelivery, IdempotencyKey: "key",
	}); err == nil {
		t.Fatal("access began on a closed pool")
	}
	if _, err := closedStore.CreateCorrection(context.Background(), CorrectionRequest{
		ID:               "correction",
		SourceRecordID:   record.ID,
		CorrectionRecord: correction,
		IdempotencyKey:   "key",
	}); err == nil {
		t.Fatal("correction began on a closed pool")
	}
	if _, err := closedStore.ReconcileCorrection(context.Background(), ReconcileRequest{
		OrganizationID:   record.OrganizationID,
		CorrectionID:     "correction",
		AffectedRecordID: record.ID,
		State:            ReconciliationApplied,
		IdempotencyKey:   "key",
	}); err == nil {
		t.Fatal("reconciliation began on a closed pool")
	}
	if _, err := closedStore.CorrectionStatus(
		context.Background(),
		record.OrganizationID,
		"correction",
	); err == nil {
		t.Fatal("status read began on a closed pool")
	}
	if err := ApplyMigrations(context.Background(), closedPool, integrationNow()); err == nil {
		t.Fatal("migration acquired a closed pool")
	}
}

func TestIntegration_MigrationExecutionAndChecksumFailures(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	connection, err := integrationPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire connection: %v", err)
	}
	defer connection.Release()
	schema := strings.ReplaceAll(testScope(t, "migration-errors"), "-", "_")
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	defer func() {
		_, _ = connection.Exec(context.Background(), `RESET search_path`)
		_, _ = connection.Exec(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
		_ = connection.Conn().Close(context.Background())
	}()
	if _, err := connection.Exec(ctx, `SET search_path TO `+schema); err != nil {
		t.Fatalf("set isolated search path: %v", err)
	}

	item := migration{
		version:  900001,
		name:     "900001_test.sql",
		checksum: strings.Repeat("a", 64),
		sql:      `CREATE TABLE migration_probe(id INTEGER PRIMARY KEY)`,
	}
	if err := applyMigration(ctx, connection, item, integrationNow()); err == nil {
		t.Fatal("applyMigration accepted a missing migration table")
	}
	if _, err := connection.Exec(ctx, `
		CREATE TABLE workforce_schema_migrations (
			version INTEGER PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			checksum CHAR(64) NOT NULL,
			applied_at TIMESTAMPTZ NOT NULL
		)
	`); err != nil {
		t.Fatalf("create isolated migration table: %v", err)
	}
	if err := applyMigration(ctx, connection, item, integrationNow()); err != nil {
		t.Fatalf("apply valid migration: %v", err)
	}
	if err := applyMigration(ctx, connection, item, integrationNow()); err != nil {
		t.Fatalf("reapply matching migration: %v", err)
	}
	drift := item
	drift.checksum = strings.Repeat("b", 64)
	if err := applyMigration(ctx, connection, drift, integrationNow()); err == nil {
		t.Fatal("applyMigration accepted checksum drift")
	}
	invalid := migration{
		version:  900002,
		name:     "900002_invalid.sql",
		checksum: strings.Repeat("c", 64),
		sql:      `not valid SQL`,
	}
	if err := applyMigration(ctx, connection, invalid, integrationNow()); err == nil {
		t.Fatal("applyMigration accepted invalid SQL")
	}
	if _, err := connection.Exec(ctx, `
		INSERT INTO workforce_schema_migrations(version, name, checksum, applied_at)
		VALUES (899999, '900004_name_conflict.sql', $1, $2)
	`, strings.Repeat("e", 64), integrationNow()); err != nil {
		t.Fatalf("seed migration name conflict: %v", err)
	}
	nameConflict := migration{
		version:  900004,
		name:     "900004_name_conflict.sql",
		checksum: strings.Repeat("f", 64),
		sql:      `CREATE TABLE migration_name_conflict_probe(id INTEGER PRIMARY KEY)`,
	}
	if err := applyMigration(ctx, connection, nameConflict, integrationNow()); err == nil {
		t.Fatal("applyMigration accepted a duplicate migration name")
	}
	deferredFailure := migration{
		version:  900005,
		name:     "900005_deferred_failure.sql",
		checksum: strings.Repeat("1", 64),
		sql: `
			CREATE TABLE migration_parent(id INTEGER PRIMARY KEY);
			CREATE TABLE migration_child(
				parent_id INTEGER REFERENCES migration_parent(id)
					DEFERRABLE INITIALLY DEFERRED
			);
			INSERT INTO migration_child(parent_id) VALUES (1);
		`,
	}
	if err := applyMigration(ctx, connection, deferredFailure, integrationNow()); err == nil {
		t.Fatal("applyMigration committed a deferred constraint violation")
	}
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err := applyMigration(canceled, connection, migration{
		version:  900003,
		name:     "900003_canceled.sql",
		checksum: strings.Repeat("d", 64),
		sql:      `SELECT 1`,
	}, integrationNow()); err == nil {
		t.Fatal("applyMigration began with canceled context")
	}
}

func TestIntegration_LedgerCommitAndNoticeFailuresRollBack(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "commit-failures")
	record := integrationRecord(scope, "record", contracts.RecordFinding, nil)

	tx, err := integrationPool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin denial commit transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TEMP TABLE denial_commit_parent(id INTEGER PRIMARY KEY);
		CREATE TEMP TABLE denial_commit_child(
			parent_id INTEGER REFERENCES denial_commit_parent(id)
				DEFERRABLE INITIALLY DEFERRED
		);
		INSERT INTO denial_commit_child(parent_id) VALUES (1);
	`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed deferred denial commit failure: %v", err)
	}
	if err := store.commitDenial(ctx, tx, OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       record.ID,
	}, integrationNow()); err == nil {
		t.Fatal("denial audit committed through a deferred constraint violation")
	}

	connection, err := integrationPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire notice failure connection: %v", err)
	}
	defer connection.Release()
	schema := strings.ReplaceAll(testScope(t, "notice-errors"), "-", "_")
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create notice failure schema: %v", err)
	}
	defer func() {
		_, _ = connection.Exec(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
		_ = connection.Conn().Close(context.Background())
	}()
	tx, err = connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin notice failure transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		t.Fatalf("set notice failure search path: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE workforce_correction_targets (
			tenant_id TEXT NOT NULL,
			organization_id TEXT NOT NULL,
			correction_id TEXT NOT NULL,
			affected_record_id TEXT NOT NULL,
			consumer_seat_id TEXT NOT NULL,
			state TEXT NOT NULL,
			materially_unsafe BOOLEAN NOT NULL,
			paused BOOLEAN NOT NULL
		)
	`); err != nil {
		t.Fatalf("create isolated correction target table: %v", err)
	}
	request := CorrectionRequest{
		ID:               contracts.CorrectionID(scope + "-correction"),
		CorrectionRecord: record,
		MateriallyUnsafe: true,
	}
	if err := store.persistCorrectionTargets(
		ctx,
		tx,
		request,
		[]correctionTarget{{recordID: record.ID, seatID: record.AuthorSeatID}},
		integrationNow(),
	); err == nil {
		t.Fatal("correction target persisted without its mandatory notice")
	}
}

func TestIntegration_LedgerLateTransactionalFailuresRollBack(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "late-failures")
	record := integrationRecord(scope, "source", contracts.RecordFinding, nil)
	consumerID := contracts.RecordID(scope + "-consumer")
	schema := strings.ReplaceAll(testScope(t, "late-schema"), "-", "_")
	connection, err := integrationPool.Acquire(ctx)
	if err != nil {
		t.Fatalf("acquire isolated connection: %v", err)
	}
	defer connection.Release()
	if _, err := connection.Exec(ctx, `CREATE SCHEMA `+schema); err != nil {
		t.Fatalf("create isolated schema: %v", err)
	}
	defer func() {
		_, _ = connection.Exec(
			context.Background(),
			`DROP SCHEMA IF EXISTS `+schema+` CASCADE`,
		)
		_ = connection.Conn().Close(context.Background())
	}()

	tx, err := connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin provenance failure transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("set provenance failure search path: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE workforce_records (
			tenant_id TEXT NOT NULL,
			organization_id TEXT NOT NULL,
			record_id TEXT NOT NULL,
			kind TEXT NOT NULL,
			author_seat_id TEXT NOT NULL,
			department_id TEXT,
			access_seat_id TEXT,
			project_id TEXT,
			purpose TEXT NOT NULL,
			classification TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			canonical_hash TEXT NOT NULL,
			sealed_record BYTEA NOT NULL
		)
	`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create isolated records table: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE workforce_access_edges (
			tenant_id TEXT NOT NULL,
			organization_id TEXT NOT NULL,
			source_record_id TEXT NOT NULL,
			consumer_record_id TEXT,
			consumer_seat_id TEXT NOT NULL,
			action TEXT NOT NULL,
			purpose TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			created_at TIMESTAMPTZ NOT NULL,
			UNIQUE (tenant_id, organization_id, idempotency_key)
		)
	`); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("create isolated access table: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_records (
			tenant_id, organization_id, record_id, kind, author_seat_id,
			purpose, classification, schema_version, canonical_hash, sealed_record
		) VALUES ($1, $2, $3, 'finding', $4, $5, 'organization', 'v1', $6, $7)
	`, store.tenantID, record.OrganizationID, consumerID, record.AuthorSeatID,
		record.Purpose, strings.Repeat("0", 64), []byte("sealed")); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatalf("seed isolated access tables: %v", err)
	}
	if err := store.recordAccessTx(ctx, tx, recordMetadata{
		organizationID: record.OrganizationID,
		recordID:       record.ID,
	}, AccessRequest{
		OrganizationID:   record.OrganizationID,
		ConsumerRecordID: &consumerID,
		Action:           AccessDerivation,
		Grant: AccessGrant{
			SeatID:  record.AuthorSeatID,
			Purpose: record.Purpose,
		},
		IdempotencyKey: scope + "-access",
	}, integrationNow()); err == nil {
		_ = tx.Rollback(ctx)
		t.Fatal("access committed without its provenance table")
	}
	_ = tx.Rollback(ctx)

	tx, err = connection.Begin(ctx)
	if err != nil {
		t.Fatalf("begin correction close failure transaction: %v", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	if _, err := tx.Exec(ctx, `SET LOCAL search_path TO `+schema); err != nil {
		t.Fatalf("set correction failure search path: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		CREATE TABLE workforce_correction_targets (
			tenant_id TEXT NOT NULL,
			organization_id TEXT NOT NULL,
			correction_id TEXT NOT NULL,
			state TEXT NOT NULL
		)
	`); err != nil {
		t.Fatalf("create isolated correction targets: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO workforce_correction_targets(
			tenant_id, organization_id, correction_id, state
		) VALUES ($1, $2, $3, 'applied')
	`, store.tenantID, record.OrganizationID, scope+"-correction"); err != nil {
		t.Fatalf("seed isolated correction targets: %v", err)
	}
	if err := store.closeResolvedCorrection(ctx, tx, ReconcileRequest{
		OrganizationID: record.OrganizationID,
		CorrectionID:   contracts.CorrectionID(scope + "-correction"),
	}, integrationNow()); err == nil {
		t.Fatal("correction closed without its durable correction row")
	}
}

func TestIntegration_ProvenanceHashMismatchRollsBack(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "provenance-hash")
	source := integrationRecord(scope, "source", contracts.RecordFinding, nil)
	appendRecords(t, ctx, store, scope, source)
	derived := integrationRecord(scope, "derived", contracts.RecordDecision, []contracts.RecordRef{{
		ID: source.ID, Kind: source.Kind, Hash: hashFor("wrong-hash"),
	}})
	if _, err := store.AppendRecord(ctx, AppendRequest{
		Record: derived, IdempotencyKey: scope + "-derived",
	}); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("provenance hash mismatch error = %v, want ErrIntegrity", err)
	}
	assertRecordCount(t, ctx, store, derived.OrganizationID, derived.ID, 0)
}

func TestIntegration_OpenCommitFailureDoesNotReturnRecord(t *testing.T) {
	ctx, cancel := testContext(t)
	defer cancel()
	store, _, scope := integrationStore(t, "open-commit-failure")
	record := integrationRecord(scope, "record", contracts.RecordFinding, nil)
	appendRecords(t, ctx, store, scope, record)
	functionName := strings.ReplaceAll(scope, "-", "_") + "_fail_commit"
	triggerName := strings.ReplaceAll(scope, "-", "_") + "_deferred_failure"
	idempotencyKey := scope + "-open"
	if _, err := integrationPool.Exec(ctx, `
		CREATE FUNCTION `+functionName+`() RETURNS TRIGGER AS $$
		BEGIN
			RAISE EXCEPTION 'forced deferred access failure';
		END;
		$$ LANGUAGE plpgsql;
		CREATE CONSTRAINT TRIGGER `+triggerName+`
		AFTER INSERT ON workforce_access_edges
		DEFERRABLE INITIALLY DEFERRED
		FOR EACH ROW
		WHEN (NEW.idempotency_key = '`+idempotencyKey+`')
		EXECUTE FUNCTION `+functionName+`()
	`); err != nil {
		t.Fatalf("create deferred access failure trigger: %v", err)
	}
	defer func() {
		_, _ = integrationPool.Exec(
			context.Background(),
			`DROP TRIGGER IF EXISTS `+triggerName+` ON workforce_access_edges`,
		)
		_, _ = integrationPool.Exec(
			context.Background(),
			`DROP FUNCTION IF EXISTS `+functionName+`()`,
		)
	}()
	if _, err := store.OpenRecord(ctx, OpenRequest{
		OrganizationID: record.OrganizationID,
		RecordID:       record.ID,
		Grant:          integrationGrant(scope, record),
		IdempotencyKey: idempotencyKey,
	}); err == nil {
		t.Fatal("OpenRecord returned plaintext before its access edge committed")
	}
	if err := store.RecordAccess(ctx, AccessRequest{
		OrganizationID: record.OrganizationID,
		SourceRecordID: record.ID,
		Action:         AccessDelivery,
		Grant:          integrationGrant(scope, record),
		IdempotencyKey: idempotencyKey,
	}); err == nil {
		t.Fatal("RecordAccess returned before its access edge committed")
	}
}
