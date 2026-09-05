#![forbid(unsafe_code)]

use std::fs::{self, File, OpenOptions};
use std::io;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU8, Ordering};
use std::sync::{Mutex, MutexGuard};
use std::time::Duration;

use keith_agent_types::{CURRENT_SCHEMA_VERSION, EntityId, Revision, SchemaVersion, UtcTimestamp};
use keith_state_store_core::{
    ActionRepository, AtomicStateRepository, AttentionRepository, CatalogRepository,
    ChannelOffsetRepository, ChildMessageRepository, ChildRepository, ClassifiedRepositoryError,
    Collection, CommitReceipt, CommitmentRepository, DeliveryRepository,
    EvolutionLedgerDataControlRepository, EvolutionLedgerErasureReport, EvolutionLedgerRepository,
    GenerationRepository, GoalRepository, InitiativeRepository, JobAttemptRepository,
    LeaseRepository, MigrationRepository, PlanRepository, ProfileRepository, RecordMutation,
    RefinementRepository, ResourceRepository, RouteRepository, ScheduleRepository,
    ToolExperienceRepository, VersionedRecord, WaitRepository, WritePrecondition,
};
use rusqlite::{Connection, OptionalExtension, Transaction, TransactionBehavior, params};
use thiserror::Error;

const STORE_SCHEMA_VERSION: u32 = 1;

pub trait BackupHook: Send + Sync {
    /// # Errors
    ///
    /// Returns an I/O error when the required pre-migration backup cannot be created.
    fn before_migration(
        &self,
        source: &Path,
        destination: &Path,
        from_version: u32,
        to_version: u32,
    ) -> io::Result<()>;
}

#[derive(Clone, Copy, Debug, Default)]
pub struct FileBackupHook;

impl BackupHook for FileBackupHook {
    fn before_migration(
        &self,
        source: &Path,
        destination: &Path,
        _from_version: u32,
        _to_version: u32,
    ) -> io::Result<()> {
        let mut source = File::open(source)?;
        let mut destination = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(destination)?;
        io::copy(&mut source, &mut destination)?;
        destination.sync_all()
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
#[repr(u8)]
pub enum FaultPoint {
    BeforeTransaction = 1,
    BeforeCommit = 2,
    AfterCommit = 3,
}

#[derive(Debug, Error)]
pub enum StoreError {
    #[error("SQLite state store failed: {0}")]
    Sqlite(#[from] rusqlite::Error),
    #[error("state store backup failed: {0}")]
    Backup(#[from] io::Error),
    #[error("migration from schema {from_version} requires a backup hook")]
    BackupRequired { from_version: u32 },
    #[error("state store schema {0} is newer than this binary")]
    UnsupportedSchema(u32),
    #[error("state store lock was poisoned")]
    LockPoisoned,
    #[error("record {id} in {collection:?} violated its write precondition")]
    Conflict {
        collection: Collection,
        id: EntityId,
    },
    #[error("record {id} in {collection:?} is corrupt: {reason}")]
    CorruptRecord {
        collection: Collection,
        id: String,
        reason: String,
    },
    #[error("numeric value is outside SQLite's signed integer range")]
    NumericRange,
    #[error("injected failure at {0:?}")]
    Injected(FaultPoint),
    #[error("transaction committed but acknowledgement was interrupted")]
    UnknownOutcome,
    #[error("collection {0:?} is append-only")]
    AppendOnlyViolation(Collection),
    #[error(
        "evolution ledger erasure left remnants: {remaining_records} records and {remaining_heads} heads"
    )]
    EvolutionLedgerErasureIncomplete {
        remaining_records: usize,
        remaining_heads: usize,
    },
}

impl ClassifiedRepositoryError for StoreError {
    fn is_conflict(&self) -> bool {
        matches!(self, Self::Conflict { .. })
    }
}

pub struct EmbeddedStore {
    connection: Mutex<Connection>,
    fault_once: AtomicU8,
}

impl EmbeddedStore {
    /// # Errors
    ///
    /// Returns an error if the in-memory database cannot be configured or migrated.
    pub fn open_in_memory() -> Result<Self, StoreError> {
        let mut connection = Connection::open_in_memory()?;
        configure(&connection)?;
        migrate(&mut connection, None, None)?;
        Ok(Self::from_connection(connection))
    }

    /// # Errors
    ///
    /// Returns an error for inaccessible, corrupt, incompatible, or unbacked legacy databases.
    pub fn open(path: &Path, backup_hook: Option<&dyn BackupHook>) -> Result<Self, StoreError> {
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)?;
        }
        let mut connection = Connection::open(path)?;
        configure(&connection)?;
        migrate(&mut connection, Some(path), backup_hook)?;
        Ok(Self::from_connection(connection))
    }

    fn from_connection(connection: Connection) -> Self {
        Self {
            connection: Mutex::new(connection),
            fault_once: AtomicU8::new(0),
        }
    }

    pub fn inject_fault_once(&self, point: FaultPoint) {
        self.fault_once.store(point as u8, Ordering::SeqCst);
    }

    /// # Errors
    ///
    /// Returns an error if the schema version cannot be read.
    pub fn schema_version(&self) -> Result<u32, StoreError> {
        let connection = self.lock()?;
        user_version(&connection)
    }

    /// # Errors
    ///
    /// Returns an error if a consistent `SQLite` backup cannot be created.
    pub fn backup_to(&self, destination: &Path) -> Result<(), StoreError> {
        if destination.exists() {
            return Err(StoreError::Backup(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "backup destination exists",
            )));
        }
        let destination = destination
            .to_str()
            .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "non-UTF-8 backup path"))?;
        let connection = self.lock()?;
        connection.execute("VACUUM INTO ?1", params![destination])?;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error if the database cannot be read or the record is corrupt.
    pub fn get_record(
        &self,
        collection: Collection,
        id: &EntityId,
    ) -> Result<Option<VersionedRecord>, StoreError> {
        let connection = self.lock()?;
        read_record(&connection, collection, id)
    }

    /// # Errors
    ///
    /// Returns an error if the database cannot be read or any record is corrupt.
    pub fn list_records(&self, collection: Collection) -> Result<Vec<VersionedRecord>, StoreError> {
        let connection = self.lock()?;
        let mut statement = connection.prepare(
            "SELECT id, schema_major, schema_minor, revision, updated_at, payload
             FROM records WHERE collection = ?1 ORDER BY id",
        )?;
        let rows = statement.query_map(params![collection.as_str()], |row| {
            Ok(RawRecord {
                id: row.get(0)?,
                schema_major: row.get(1)?,
                schema_minor: row.get(2)?,
                revision: row.get(3)?,
                updated_at: row.get(4)?,
                payload: row.get(5)?,
            })
        })?;
        rows.map(|row| {
            let raw = row?;
            decode_record(collection, &raw)
        })
        .collect()
    }

    fn transact_records(&self, mutations: &[RecordMutation]) -> Result<CommitReceipt, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        for mutation in mutations {
            apply_mutation(&transaction, mutation)?;
        }
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(CommitReceipt {
            applied_mutations: mutations.len(),
        })
    }

    fn put_record(
        &self,
        collection: Collection,
        record: VersionedRecord,
        precondition: WritePrecondition,
    ) -> Result<CommitReceipt, StoreError> {
        self.transact_records(&[RecordMutation::Put {
            collection,
            record,
            precondition,
        }])
    }

    fn delete_record(
        &self,
        collection: Collection,
        id: &EntityId,
        precondition: WritePrecondition,
    ) -> Result<CommitReceipt, StoreError> {
        self.transact_records(&[RecordMutation::Delete {
            collection,
            id: id.clone(),
            precondition,
        }])
    }

    fn lock(&self) -> Result<MutexGuard<'_, Connection>, StoreError> {
        self.connection.lock().map_err(|_| StoreError::LockPoisoned)
    }

    fn fail_if(&self, point: FaultPoint) -> Result<(), StoreError> {
        if self.take_fault(point) {
            Err(StoreError::Injected(point))
        } else {
            Ok(())
        }
    }

    fn take_fault(&self, point: FaultPoint) -> bool {
        self.fault_once
            .compare_exchange(point as u8, 0, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok()
    }
}

impl AtomicStateRepository for EmbeddedStore {
    type Error = StoreError;

    fn transact(&self, mutations: &[RecordMutation]) -> Result<CommitReceipt, Self::Error> {
        self.transact_records(mutations)
    }
}

impl EvolutionLedgerRepository for EmbeddedStore {
    type Error = StoreError;

    fn get_evolution_record(&self, id: &EntityId) -> Result<Option<VersionedRecord>, Self::Error> {
        self.get_record(Collection::EvolutionLedger, id)
    }

    fn list_evolution_records(&self) -> Result<Vec<VersionedRecord>, Self::Error> {
        self.list_records(Collection::EvolutionLedger)
    }

    fn get_evolution_head(&self) -> Result<Option<VersionedRecord>, Self::Error> {
        self.get_record(Collection::EvolutionLedgerHead, &EntityId::from_u128(0))
    }

    fn append_evolution_record(
        &self,
        record: VersionedRecord,
        head: VersionedRecord,
        head_precondition: WritePrecondition,
    ) -> Result<CommitReceipt, Self::Error> {
        self.transact_evolution_append(record, head, head_precondition)
    }
}

impl EvolutionLedgerDataControlRepository for EmbeddedStore {
    type Error = StoreError;

    fn erase_evolution_ledger_for_data_control(
        &self,
    ) -> Result<EvolutionLedgerErasureReport, Self::Error> {
        self.transact_evolution_ledger_erasure()
    }
}

impl EmbeddedStore {
    fn transact_evolution_ledger_erasure(
        &self,
    ) -> Result<EvolutionLedgerErasureReport, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let deleted_records = transaction.execute(
            "DELETE FROM records WHERE collection = ?1",
            params![Collection::EvolutionLedger.as_str()],
        )?;
        let deleted_heads = transaction.execute(
            "DELETE FROM records WHERE collection = ?1",
            params![Collection::EvolutionLedgerHead.as_str()],
        )?;
        let remaining_records = collection_record_count(&transaction, Collection::EvolutionLedger)?;
        let remaining_heads =
            collection_record_count(&transaction, Collection::EvolutionLedgerHead)?;
        if remaining_records != 0 || remaining_heads != 0 {
            return Err(StoreError::EvolutionLedgerErasureIncomplete {
                remaining_records,
                remaining_heads,
            });
        }
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(EvolutionLedgerErasureReport {
            deleted_records,
            deleted_heads,
            remaining_records,
            remaining_heads,
        })
    }

    fn transact_evolution_append(
        &self,
        record: VersionedRecord,
        head: VersionedRecord,
        head_precondition: WritePrecondition,
    ) -> Result<CommitReceipt, StoreError> {
        if head.id != EntityId::from_u128(0) {
            return Err(StoreError::AppendOnlyViolation(
                Collection::EvolutionLedgerHead,
            ));
        }
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        apply_mutation_inner(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::EvolutionLedger,
                record,
                precondition: WritePrecondition::Missing,
            },
            true,
        )?;
        apply_mutation_inner(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::EvolutionLedgerHead,
                record: head,
                precondition: head_precondition,
            },
            true,
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(CommitReceipt {
            applied_mutations: 2,
        })
    }
}

fn collection_record_count(
    transaction: &Transaction<'_>,
    collection: Collection,
) -> Result<usize, StoreError> {
    let count = transaction.query_row(
        "SELECT COUNT(*) FROM records WHERE collection = ?1",
        params![collection.as_str()],
        |row| row.get::<_, i64>(0),
    )?;
    usize::try_from(count).map_err(|_| StoreError::NumericRange)
}

fn configure(connection: &Connection) -> Result<(), StoreError> {
    connection.busy_timeout(Duration::from_secs(5))?;
    connection.pragma_update(None, "foreign_keys", true)?;
    Ok(())
}

fn user_version(connection: &Connection) -> Result<u32, StoreError> {
    connection
        .pragma_query_value(None, "user_version", |row| row.get(0))
        .map_err(StoreError::from)
}

fn migrate(
    connection: &mut Connection,
    path: Option<&Path>,
    backup_hook: Option<&dyn BackupHook>,
) -> Result<(), StoreError> {
    let from_version = user_version(connection)?;
    if from_version > STORE_SCHEMA_VERSION {
        return Err(StoreError::UnsupportedSchema(from_version));
    }
    if from_version == STORE_SCHEMA_VERSION {
        return Ok(());
    }
    let has_existing_state: bool = connection.query_row(
        "SELECT EXISTS(
             SELECT 1 FROM sqlite_master
             WHERE type = 'table' AND name NOT LIKE 'sqlite_%'
         )",
        [],
        |row| row.get(0),
    )?;
    if has_existing_state {
        let source = path.ok_or(StoreError::BackupRequired { from_version })?;
        let hook = backup_hook.ok_or(StoreError::BackupRequired { from_version })?;
        connection.execute_batch("PRAGMA wal_checkpoint(FULL)")?;
        let destination = migration_backup_path(source, from_version, STORE_SCHEMA_VERSION);
        hook.before_migration(source, &destination, from_version, STORE_SCHEMA_VERSION)?;
    }
    let transaction = connection.transaction_with_behavior(TransactionBehavior::Exclusive)?;
    transaction.execute_batch(
        "CREATE TABLE IF NOT EXISTS records (
             collection TEXT NOT NULL,
             id TEXT NOT NULL,
             schema_major INTEGER NOT NULL,
             schema_minor INTEGER NOT NULL,
             revision INTEGER NOT NULL,
             updated_at INTEGER NOT NULL,
             payload BLOB NOT NULL,
             PRIMARY KEY (collection, id)
         ) WITHOUT ROWID;
         CREATE INDEX IF NOT EXISTS records_updated_at
             ON records(collection, updated_at, id);",
    )?;
    transaction.pragma_update(None, "user_version", STORE_SCHEMA_VERSION)?;
    transaction.commit()?;
    Ok(())
}

fn migration_backup_path(source: &Path, from_version: u32, to_version: u32) -> PathBuf {
    let extension = format!(
        "pre-v{from_version}-to-v{to_version}-{}.sqlite",
        EntityId::new()
    );
    source.with_extension(extension)
}

fn apply_mutation(
    transaction: &Transaction<'_>,
    mutation: &RecordMutation,
) -> Result<(), StoreError> {
    apply_mutation_inner(transaction, mutation, false)
}

fn apply_mutation_inner(
    transaction: &Transaction<'_>,
    mutation: &RecordMutation,
    evolution_append: bool,
) -> Result<(), StoreError> {
    #[allow(clippy::match_same_arms)]
    match mutation {
        RecordMutation::Put {
            collection: Collection::EvolutionLedger,
            precondition: WritePrecondition::Missing,
            ..
        } if evolution_append => {}
        RecordMutation::Put {
            collection: Collection::EvolutionLedgerHead,
            precondition: WritePrecondition::Missing | WritePrecondition::Exact(_),
            ..
        } if evolution_append => {}
        RecordMutation::Put {
            collection: Collection::EvolutionLedger | Collection::EvolutionLedgerHead,
            ..
        }
        | RecordMutation::Delete {
            collection: Collection::EvolutionLedger | Collection::EvolutionLedgerHead,
            ..
        } => return Err(StoreError::AppendOnlyViolation(Collection::EvolutionLedger)),
        _ => {}
    }
    match mutation {
        RecordMutation::Put {
            collection,
            record,
            precondition,
        } => {
            validate_record(*collection, record)?;
            let current = current_revision(transaction, *collection, &record.id)?;
            check_precondition(*collection, &record.id, current, *precondition)?;
            if current.is_some_and(|revision| record.revision <= revision) {
                return Err(StoreError::Conflict {
                    collection: *collection,
                    id: record.id.clone(),
                });
            }
            let revision =
                i64::try_from(record.revision.get()).map_err(|_| StoreError::NumericRange)?;
            let payload = keith_agent_types::canonical_json_bytes(&record.payload)
                .map_err(|error| corrupt(*collection, record.id.as_str(), error.to_string()))?;
            transaction.execute(
                "INSERT INTO records(
                     collection, id, schema_major, schema_minor, revision, updated_at, payload
                 ) VALUES (?1, ?2, ?3, ?4, ?5, ?6, ?7)
                 ON CONFLICT(collection, id) DO UPDATE SET
                     schema_major = excluded.schema_major,
                     schema_minor = excluded.schema_minor,
                     revision = excluded.revision,
                     updated_at = excluded.updated_at,
                     payload = excluded.payload",
                params![
                    collection.as_str(),
                    record.id.as_str(),
                    record.version.major,
                    record.version.minor,
                    revision,
                    record.updated_at.unix_millis(),
                    payload,
                ],
            )?;
        }
        RecordMutation::Delete {
            collection,
            id,
            precondition,
        } => {
            let current = current_revision(transaction, *collection, id)?;
            check_precondition(*collection, id, current, *precondition)?;
            transaction.execute(
                "DELETE FROM records WHERE collection = ?1 AND id = ?2",
                params![collection.as_str(), id.as_str()],
            )?;
        }
    }
    Ok(())
}

fn validate_record(collection: Collection, record: &VersionedRecord) -> Result<(), StoreError> {
    if record.version.major != CURRENT_SCHEMA_VERSION.major
        || record.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(corrupt(
            collection,
            record.id.as_str(),
            format!("unsupported record schema {}", record.version),
        ));
    }
    Ok(())
}

fn current_revision(
    transaction: &Transaction<'_>,
    collection: Collection,
    id: &EntityId,
) -> Result<Option<Revision>, StoreError> {
    let revision = transaction
        .query_row(
            "SELECT revision FROM records WHERE collection = ?1 AND id = ?2",
            params![collection.as_str(), id.as_str()],
            |row| row.get::<_, i64>(0),
        )
        .optional()?;
    revision
        .map(|value| {
            u64::try_from(value)
                .map(Revision::new)
                .map_err(|_| corrupt(collection, id.as_str(), "negative revision".into()))
        })
        .transpose()
}

fn check_precondition(
    collection: Collection,
    id: &EntityId,
    current: Option<Revision>,
    precondition: WritePrecondition,
) -> Result<(), StoreError> {
    let accepted = match precondition {
        WritePrecondition::Any => true,
        WritePrecondition::Missing => current.is_none(),
        WritePrecondition::Exact(expected) => current == Some(expected),
    };
    if accepted {
        Ok(())
    } else {
        Err(StoreError::Conflict {
            collection,
            id: id.clone(),
        })
    }
}

fn read_record(
    connection: &Connection,
    collection: Collection,
    id: &EntityId,
) -> Result<Option<VersionedRecord>, StoreError> {
    let raw = connection
        .query_row(
            "SELECT id, schema_major, schema_minor, revision, updated_at, payload
             FROM records WHERE collection = ?1 AND id = ?2",
            params![collection.as_str(), id.as_str()],
            |row| {
                Ok(RawRecord {
                    id: row.get(0)?,
                    schema_major: row.get(1)?,
                    schema_minor: row.get(2)?,
                    revision: row.get(3)?,
                    updated_at: row.get(4)?,
                    payload: row.get(5)?,
                })
            },
        )
        .optional()?;
    raw.as_ref()
        .map(|raw| decode_record(collection, raw))
        .transpose()
}

struct RawRecord {
    id: String,
    schema_major: i64,
    schema_minor: i64,
    revision: i64,
    updated_at: i64,
    payload: Vec<u8>,
}

fn decode_record(collection: Collection, raw: &RawRecord) -> Result<VersionedRecord, StoreError> {
    let id = EntityId::parse(raw.id.clone())
        .map_err(|error| corrupt(collection, &raw.id, error.to_string()))?;
    let schema_major = u16::try_from(raw.schema_major)
        .map_err(|_| corrupt(collection, &raw.id, "invalid schema major".into()))?;
    let schema_minor = u16::try_from(raw.schema_minor)
        .map_err(|_| corrupt(collection, &raw.id, "invalid schema minor".into()))?;
    let revision = u64::try_from(raw.revision)
        .map(Revision::new)
        .map_err(|_| corrupt(collection, &raw.id, "invalid revision".into()))?;
    let payload = serde_json::from_slice(&raw.payload)
        .map_err(|error| corrupt(collection, &raw.id, error.to_string()))?;
    let record = VersionedRecord {
        version: SchemaVersion::new(schema_major, schema_minor),
        id,
        revision,
        updated_at: UtcTimestamp::from_unix_millis(raw.updated_at),
        payload,
    };
    validate_record(collection, &record)?;
    Ok(record)
}

fn corrupt(collection: Collection, id: &str, reason: String) -> StoreError {
    StoreError::CorruptRecord {
        collection,
        id: id.to_owned(),
        reason,
    }
}

macro_rules! implement_repository {
    (
        $trait_name:ident,
        $collection:expr,
        $get:ident,
        $list:ident,
        $put:ident,
        $delete:ident
    ) => {
        impl $trait_name for EmbeddedStore {
            type Error = StoreError;

            fn $get(&self, id: &EntityId) -> Result<Option<VersionedRecord>, Self::Error> {
                self.get_record($collection, id)
            }

            fn $list(&self) -> Result<Vec<VersionedRecord>, Self::Error> {
                self.list_records($collection)
            }

            fn $put(
                &self,
                record: VersionedRecord,
                precondition: WritePrecondition,
            ) -> Result<CommitReceipt, Self::Error> {
                self.put_record($collection, record, precondition)
            }

            fn $delete(
                &self,
                id: &EntityId,
                precondition: WritePrecondition,
            ) -> Result<CommitReceipt, Self::Error> {
                self.delete_record($collection, id, precondition)
            }
        }
    };
}

implement_repository!(
    LeaseRepository,
    Collection::WorkerLeases,
    get_lease,
    list_leases,
    put_lease,
    delete_lease
);
implement_repository!(
    GenerationRepository,
    Collection::WorkerGenerations,
    get_generation,
    list_generations,
    put_generation,
    delete_generation
);
implement_repository!(
    CatalogRepository,
    Collection::SessionCatalog,
    get_catalog_entry,
    list_catalog_entries,
    put_catalog_entry,
    delete_catalog_entry
);
implement_repository!(
    ActionRepository,
    Collection::PendingActions,
    get_action,
    list_actions,
    put_action,
    delete_action
);
implement_repository!(
    ProfileRepository,
    Collection::Profiles,
    get_profile,
    list_profiles,
    put_profile,
    delete_profile
);
implement_repository!(
    ChildRepository,
    Collection::Children,
    get_child,
    list_children,
    put_child,
    delete_child
);
implement_repository!(
    ChildMessageRepository,
    Collection::ChildMessages,
    get_child_message,
    list_child_messages,
    put_child_message,
    delete_child_message
);
implement_repository!(
    GoalRepository,
    Collection::Goals,
    get_goal,
    list_goals,
    put_goal,
    delete_goal
);
implement_repository!(
    PlanRepository,
    Collection::Plans,
    get_plan,
    list_plans,
    put_plan,
    delete_plan
);
implement_repository!(
    CommitmentRepository,
    Collection::Commitments,
    get_commitment,
    list_commitments,
    put_commitment,
    delete_commitment
);
implement_repository!(
    WaitRepository,
    Collection::WaitingConditions,
    get_wait,
    list_waits,
    put_wait,
    delete_wait
);
implement_repository!(
    ScheduleRepository,
    Collection::ScheduledJobs,
    get_schedule,
    list_schedules,
    put_schedule,
    delete_schedule
);
implement_repository!(
    JobAttemptRepository,
    Collection::JobAttempts,
    get_job_attempt,
    list_job_attempts,
    put_job_attempt,
    delete_job_attempt
);
implement_repository!(
    RouteRepository,
    Collection::RoutingRules,
    get_route,
    list_routes,
    put_route,
    delete_route
);
implement_repository!(
    ResourceRepository,
    Collection::ResourceGovernance,
    get_resource_record,
    list_resource_records,
    put_resource_record,
    delete_resource_record
);
implement_repository!(
    ChannelOffsetRepository,
    Collection::ChannelOffsets,
    get_channel_offset,
    list_channel_offsets,
    put_channel_offset,
    delete_channel_offset
);
implement_repository!(
    DeliveryRepository,
    Collection::Deliveries,
    get_delivery,
    list_deliveries,
    put_delivery,
    delete_delivery
);
implement_repository!(
    AttentionRepository,
    Collection::AttentionCandidates,
    get_attention_candidate,
    list_attention_candidates,
    put_attention_candidate,
    delete_attention_candidate
);
implement_repository!(
    InitiativeRepository,
    Collection::InitiativeHistory,
    get_initiative,
    list_initiatives,
    put_initiative,
    delete_initiative
);
implement_repository!(
    RefinementRepository,
    Collection::EvolutionTransactions,
    get_refinement,
    list_refinements,
    put_refinement,
    delete_refinement
);
implement_repository!(
    ToolExperienceRepository,
    Collection::ToolExperience,
    get_tool_experience,
    list_tool_experience,
    put_tool_experience,
    delete_tool_experience
);
implement_repository!(
    MigrationRepository,
    Collection::SchemaMigrations,
    get_migration,
    list_migrations,
    put_migration,
    delete_migration
);

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::thread;

    use keith_state_store_core::{ActionRepository, LeaseRepository};
    use serde_json::json;
    use tempfile::tempdir;

    use super::*;

    fn record(id: EntityId, revision: u64, value: usize) -> VersionedRecord {
        VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision: Revision::new(revision),
            updated_at: UtcTimestamp::from_unix_millis(i64::try_from(value).unwrap()),
            payload: json!({"value": value}),
        }
    }

    fn append_evolution_pair(store: &EmbeddedStore, value: usize, head_revision: u64) {
        store
            .append_evolution_record(
                record(EntityId::new(), 0, value),
                record(EntityId::from_u128(0), head_revision, value),
                if head_revision == 0 {
                    WritePrecondition::Missing
                } else {
                    WritePrecondition::Exact(Revision::new(head_revision - 1))
                },
            )
            .unwrap();
    }

    #[test]
    fn data_control_erasure_atomically_removes_ledger_and_authenticated_head() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        append_evolution_pair(&store, 1, 0);
        append_evolution_pair(&store, 2, 1);

        let report = store.erase_evolution_ledger_for_data_control().unwrap();

        assert_eq!(
            report,
            EvolutionLedgerErasureReport {
                deleted_records: 2,
                deleted_heads: 1,
                remaining_records: 0,
                remaining_heads: 0,
            }
        );
        assert!(store.list_evolution_records().unwrap().is_empty());
        assert!(store.get_evolution_head().unwrap().is_none());
    }

    #[test]
    fn failed_data_control_erasure_rolls_back_both_append_only_collections() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        append_evolution_pair(&store, 1, 0);
        store.inject_fault_once(FaultPoint::BeforeCommit);

        assert!(matches!(
            store.erase_evolution_ledger_for_data_control(),
            Err(StoreError::Injected(FaultPoint::BeforeCommit))
        ));
        assert_eq!(store.list_evolution_records().unwrap().len(), 1);
        assert!(store.get_evolution_head().unwrap().is_some());
    }

    #[test]
    fn generic_mutations_cannot_use_the_data_control_erasure_boundary() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        append_evolution_pair(&store, 1, 0);

        for collection in [Collection::EvolutionLedger, Collection::EvolutionLedgerHead] {
            let error = store
                .transact(&[RecordMutation::Delete {
                    collection,
                    id: EntityId::from_u128(0),
                    precondition: WritePrecondition::Any,
                }])
                .unwrap_err();
            assert!(matches!(error, StoreError::AppendOnlyViolation(_)));
        }
        assert_eq!(store.list_evolution_records().unwrap().len(), 1);
        assert!(store.get_evolution_head().unwrap().is_some());
    }

    #[test]
    fn atomic_batches_commit_or_roll_back_every_collection() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let lease_id = EntityId::new();
        let action_id = EntityId::new();
        let mutations = vec![
            RecordMutation::Put {
                collection: Collection::WorkerLeases,
                record: record(lease_id.clone(), 0, 1),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::PendingActions,
                record: record(action_id.clone(), 0, 2),
                precondition: WritePrecondition::Missing,
            },
        ];
        let receipt = store.transact(&mutations).unwrap();
        assert_eq!(receipt.applied_mutations, 2);
        assert!(store.get_lease(&lease_id).unwrap().is_some());
        assert!(store.get_action(&action_id).unwrap().is_some());

        let second_lease = EntityId::new();
        let second_action = EntityId::new();
        store.inject_fault_once(FaultPoint::BeforeCommit);
        let error = store
            .transact(&[
                RecordMutation::Put {
                    collection: Collection::WorkerLeases,
                    record: record(second_lease.clone(), 0, 3),
                    precondition: WritePrecondition::Missing,
                },
                RecordMutation::Put {
                    collection: Collection::PendingActions,
                    record: record(second_action.clone(), 0, 4),
                    precondition: WritePrecondition::Missing,
                },
            ])
            .unwrap_err();
        assert!(matches!(
            error,
            StoreError::Injected(FaultPoint::BeforeCommit)
        ));
        assert!(store.get_lease(&second_lease).unwrap().is_none());
        assert!(store.get_action(&second_action).unwrap().is_none());
    }

    #[test]
    fn preconditions_and_revisions_reject_stale_writers() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let id = EntityId::new();
        store
            .put_lease(record(id.clone(), 0, 1), WritePrecondition::Missing)
            .unwrap();
        let error = store
            .put_lease(
                record(id.clone(), 1, 2),
                WritePrecondition::Exact(Revision::new(9)),
            )
            .unwrap_err();
        assert!(matches!(error, StoreError::Conflict { .. }));
        store
            .put_lease(
                record(id.clone(), 1, 2),
                WritePrecondition::Exact(Revision::ZERO),
            )
            .unwrap();
        assert_eq!(
            store.get_lease(&id).unwrap().unwrap().revision,
            Revision::new(1)
        );
    }

    #[test]
    fn external_service_collections_round_trip_restart_and_exact_deletion() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("integrations.sqlite");
        let collections = [
            Collection::ChannelAccounts,
            Collection::AcpSessions,
            Collection::PluginRegistry,
            Collection::ConnectedApps,
            Collection::ComputerSessions,
            Collection::ControlLeases,
            Collection::Demonstrations,
            Collection::TaskRecipes,
            Collection::HarnessRepairs,
            Collection::IntegrationOperations,
            Collection::IntegrationAudit,
        ];
        let ids = collections
            .iter()
            .enumerate()
            .map(|(index, collection)| {
                let id = EntityId::new();
                let value = index.saturating_add(1);
                (id, *collection, value)
            })
            .collect::<Vec<_>>();
        {
            let store = EmbeddedStore::open(&path, Some(&FileBackupHook)).unwrap();
            for (id, collection, value) in &ids {
                store
                    .transact(&[RecordMutation::Put {
                        collection: *collection,
                        record: record(id.clone(), 0, *value),
                        precondition: WritePrecondition::Missing,
                    }])
                    .unwrap();
            }
        }
        let reopened = EmbeddedStore::open(&path, Some(&FileBackupHook)).unwrap();
        for (id, collection, value) in &ids {
            assert_eq!(
                reopened
                    .get_record(*collection, id)
                    .unwrap()
                    .unwrap()
                    .payload,
                json!({"value": value})
            );
            reopened
                .transact(&[RecordMutation::Delete {
                    collection: *collection,
                    id: id.clone(),
                    precondition: WritePrecondition::Exact(Revision::ZERO),
                }])
                .unwrap();
            assert!(reopened.get_record(*collection, id).unwrap().is_none());
        }
    }

    #[test]
    fn concurrent_readers_and_writers_preserve_every_record() {
        let store = Arc::new(EmbeddedStore::open_in_memory().unwrap());
        let mut threads = Vec::new();
        for worker in 0..8 {
            let store = Arc::clone(&store);
            threads.push(thread::spawn(move || {
                for item in 0..25 {
                    let id = EntityId::new();
                    let value = worker * 25 + item;
                    store
                        .put_lease(record(id.clone(), 0, value), WritePrecondition::Missing)
                        .unwrap();
                    assert_eq!(
                        store.get_lease(&id).unwrap().unwrap().payload,
                        json!({"value": value})
                    );
                }
            }));
        }
        for thread in threads {
            thread.join().unwrap();
        }
        assert_eq!(store.list_leases().unwrap().len(), 200);
    }

    #[test]
    fn faults_around_commit_recover_with_explicit_outcomes() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let first_id = EntityId::new();
        let second_id = EntityId::new();
        {
            let store = EmbeddedStore::open(&path, None).unwrap();
            store.inject_fault_once(FaultPoint::BeforeCommit);
            assert!(matches!(
                store.put_lease(record(first_id.clone(), 0, 1), WritePrecondition::Missing),
                Err(StoreError::Injected(FaultPoint::BeforeCommit))
            ));
            store.inject_fault_once(FaultPoint::AfterCommit);
            assert!(matches!(
                store.put_lease(record(second_id.clone(), 0, 2), WritePrecondition::Missing),
                Err(StoreError::UnknownOutcome)
            ));
        }
        let recovered = EmbeddedStore::open(&path, None).unwrap();
        assert!(recovered.get_lease(&first_id).unwrap().is_none());
        assert!(recovered.get_lease(&second_id).unwrap().is_some());
    }

    #[test]
    fn migration_creates_real_backup_and_retains_legacy_data() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        {
            let connection = Connection::open(&path).unwrap();
            connection
                .execute_batch(
                    "CREATE TABLE legacy(value TEXT); INSERT INTO legacy VALUES ('kept');",
                )
                .unwrap();
        }
        let store = EmbeddedStore::open(&path, Some(&FileBackupHook)).unwrap();
        assert_eq!(store.schema_version().unwrap(), STORE_SCHEMA_VERSION);
        let connection = store.lock().unwrap();
        let value: String = connection
            .query_row("SELECT value FROM legacy", [], |row| row.get(0))
            .unwrap();
        assert_eq!(value, "kept");
        drop(connection);
        assert!(fs::read_dir(directory.path()).unwrap().any(|entry| {
            entry
                .unwrap()
                .file_name()
                .to_string_lossy()
                .contains("pre-v0-to-v1")
        }));
    }

    struct RejectBackup;

    impl BackupHook for RejectBackup {
        fn before_migration(
            &self,
            _source: &Path,
            _destination: &Path,
            _from_version: u32,
            _to_version: u32,
        ) -> io::Result<()> {
            Err(io::Error::other("injected backup failure"))
        }
    }

    #[test]
    fn failed_backup_prevents_migration() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        {
            let connection = Connection::open(&path).unwrap();
            connection
                .execute_batch("CREATE TABLE legacy(value TEXT)")
                .unwrap();
        }
        assert!(matches!(
            EmbeddedStore::open(&path, Some(&RejectBackup)),
            Err(StoreError::Backup(_))
        ));
        let connection = Connection::open(&path).unwrap();
        assert_eq!(user_version(&connection).unwrap(), 0);
        let exists: bool = connection
            .query_row(
                "SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name = 'legacy')",
                [],
                |row| row.get(0),
            )
            .unwrap();
        assert!(exists);
    }

    #[test]
    fn corrupt_database_is_rejected_instead_of_reinitialized() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        fs::write(&path, b"not a sqlite database").unwrap();
        assert!(matches!(
            EmbeddedStore::open(&path, None),
            Err(StoreError::Sqlite(_))
        ));
        assert_eq!(fs::read(&path).unwrap(), b"not a sqlite database");
    }

    #[test]
    fn embedded_backend_implements_every_repository_contract() {
        fn assert_contracts<T>()
        where
            T: AtomicStateRepository<Error = StoreError>
                + LeaseRepository<Error = StoreError>
                + GenerationRepository<Error = StoreError>
                + CatalogRepository<Error = StoreError>
                + ActionRepository<Error = StoreError>
                + ProfileRepository<Error = StoreError>
                + ChildRepository<Error = StoreError>
                + ChildMessageRepository<Error = StoreError>
                + GoalRepository<Error = StoreError>
                + PlanRepository<Error = StoreError>
                + CommitmentRepository<Error = StoreError>
                + WaitRepository<Error = StoreError>
                + ScheduleRepository<Error = StoreError>
                + JobAttemptRepository<Error = StoreError>
                + RouteRepository<Error = StoreError>
                + ResourceRepository<Error = StoreError>
                + ChannelOffsetRepository<Error = StoreError>
                + DeliveryRepository<Error = StoreError>
                + AttentionRepository<Error = StoreError>
                + InitiativeRepository<Error = StoreError>
                + RefinementRepository<Error = StoreError>
                + ToolExperienceRepository<Error = StoreError>
                + MigrationRepository<Error = StoreError>,
        {
        }
        assert_contracts::<EmbeddedStore>();
    }
}
