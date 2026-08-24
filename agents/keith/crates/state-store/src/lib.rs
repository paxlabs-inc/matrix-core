#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io;
use std::path::{Path, PathBuf};
use std::sync::atomic::{AtomicU8, AtomicUsize, Ordering};
use std::sync::{Mutex, MutexGuard};
use std::time::Duration;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, Generation, ProfileId, Revision, RootTreeId, SchemaVersion,
    StableKey, UtcTimestamp, WorkerId,
};
use keith_state_store_core::{
    ActionRepository, AssignmentHandoffTransaction, AssignmentRepository, AssignmentTransition,
    AtomicStateRepository, AttentionRepository, AuthoritativeTransitionOutcome,
    CanonicalAppendOutcome, CanonicalConversationAppend, CanonicalConversationRepository,
    CanonicalDirectConversationBinding, CanonicalDirectConversationOutcome, CatalogRepository,
    ChannelOffsetRepository, ChildMessageRepository, ChildRepository, ClassifiedRepositoryError,
    CollaborationRoundRepository, CollaborationRoundTransition, Collection, CommitReceipt,
    CommitmentRepository, ConversationDeliveryFinalization,
    ConversationDeliveryFinalizationOutcome, ConversationDeliveryRepository,
    ConversationProjectionRebuild, DeliveryRepository, DirectConversationRepository,
    EvolutionLedgerDataControlRepository, EvolutionLedgerErasureReport, EvolutionLedgerRepository,
    GenerationRepository, GoalRepository, GroupMembershipRepository, GroupMembershipTransition,
    InitiativeRepository, JobAttemptRepository, LeaseRepository, MigrationRepository,
    PlanRepository, ProfileExecutionAdmissionOutcome, ProfileExecutionAdmissionRepository,
    ProfileExecutionAdmissionRequest, ProfileExecutionCloseRequest, ProfileExecutionFence,
    ProfileExecutionFenceSnapshot, ProfileExecutionFenceState, ProfileExecutionMutationStatus,
    ProfileExecutionPermit, ProfileExecutionRegistration, ProfileExecutionRegistrationState,
    ProfileExecutionReopenRequest, ProfileExecutionWorkerBinding, ProfileRepository,
    RecordMutation, RefinementRepository, ResourceRepository, RouteRepository, ScheduleRepository,
    SharedKnowledgeSpaceRepository, StateRecordRepository, ToolExperienceRepository,
    VersionedRecord, WaitRepository, WritePrecondition,
};
use rusqlite::{Connection, OptionalExtension, Transaction, TransactionBehavior, params};
use serde::Deserialize;
use thiserror::Error;

pub const STORE_SCHEMA_VERSION: u32 = 1;
pub const TEAMMATE_MIGRATION_MARKER_KIND: &str = "keith_teammates";

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VerifiedStoreBackup {
    pub schema_version: u32,
    pub record_count: usize,
}

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
    #[error("injected failure before migration write {0}")]
    InjectedWrite(usize),
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
    #[error("teammate migration backup verification failed: {0}")]
    BackupVerification(String),
    #[error("teammate migration version must not be empty")]
    InvalidMigrationVersion,
    #[error("teammate migration marker is corrupt: {0}")]
    InvalidMigrationMarker(String),
    #[error("teammate migration version was replayed with a different canonical plan")]
    MigrationReplayMismatch,
    #[error("system clock failed while recording teammate migration: {0}")]
    Clock(String),
    #[error("invalid canonical conversation append: {0}")]
    InvalidCanonicalAppend(&'static str),
    #[error("canonical direct conversation binding is invalid: {0}")]
    InvalidDirectConversation(&'static str),
    #[error("conversation delivery finalization is invalid: {0}")]
    InvalidDeliveryFinalization(&'static str),
    #[error("authoritative collaboration transition is invalid: {0}")]
    InvalidAuthoritativeTransition(&'static str),
    #[error("stable conversation publication key collides with different event bytes")]
    StableKeyCollision,
    #[error("profile {profile_id} is not enabled at the required revision")]
    ProfileExecutionProfileNotEnabled { profile_id: ProfileId },
    #[error("profile execution admission was rejected for {profile_id}: {reason}")]
    ProfileExecutionRejected {
        profile_id: ProfileId,
        reason: &'static str,
    },
    #[error("profile execution admission record is invalid: {0}")]
    InvalidProfileExecution(&'static str),
    #[error("profile execution counter overflowed")]
    ProfileExecutionOverflow,
    #[error("collection {0:?} requires the profile execution admission capability")]
    ProfileExecutionProtected(Collection),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TeammateMigrationOutcome {
    Applied(CommitReceipt),
    AlreadyApplied,
}

impl ClassifiedRepositoryError for StoreError {
    fn is_conflict(&self) -> bool {
        matches!(
            self,
            Self::Conflict { .. }
                | Self::ProfileExecutionProfileNotEnabled { .. }
                | Self::ProfileExecutionRejected { .. }
        )
    }
}

pub struct EmbeddedStore {
    connection: Mutex<Connection>,
    fault_once: AtomicU8,
    fault_write_once: AtomicUsize,
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
            fault_write_once: AtomicUsize::new(usize::MAX),
        }
    }

    pub fn inject_fault_once(&self, point: FaultPoint) {
        self.fault_once.store(point as u8, Ordering::SeqCst);
    }

    pub fn inject_fault_before_migration_write_once(&self, index: usize) {
        self.fault_write_once.store(index, Ordering::SeqCst);
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

    /// Verifies that a store backup is integral and exactly schema-compatible with this binary.
    ///
    /// # Errors
    ///
    /// Returns an error for a corrupt, unreadable, or incompatible backup.
    pub fn verify_backup(path: &Path) -> Result<VerifiedStoreBackup, StoreError> {
        let connection = open_verified_backup(path)?;
        let schema_version = user_version(&connection)?;
        if schema_version != STORE_SCHEMA_VERSION {
            return Err(StoreError::UnsupportedSchema(schema_version));
        }
        let record_count = raw_store_snapshot(&connection)?.len();
        Ok(VerifiedStoreBackup {
            schema_version,
            record_count,
        })
    }

    /// Copies a verified backup to a new data-root path without overwriting existing state.
    ///
    /// # Errors
    ///
    /// Returns an error unless the source verifies, the destination is new, and the restored
    /// snapshot is byte-for-byte equivalent at the logical record boundary.
    pub fn restore_backup_to(
        backup: &Path,
        destination: &Path,
    ) -> Result<VerifiedStoreBackup, StoreError> {
        let verified = Self::verify_backup(backup)?;
        if destination.exists() {
            return Err(StoreError::Backup(io::Error::new(
                io::ErrorKind::AlreadyExists,
                "restore destination exists",
            )));
        }
        if let Some(parent) = destination.parent() {
            fs::create_dir_all(parent)?;
        }
        let mut source = File::open(backup)?;
        let mut target = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(destination)?;
        io::copy(&mut source, &mut target)?;
        target.sync_all()?;
        let source = open_verified_backup(backup)?;
        let restored = open_verified_backup(destination)?;
        if raw_store_snapshot(&source)? != raw_store_snapshot(&restored)?
            || user_version(&source)? != user_version(&restored)?
        {
            return Err(StoreError::BackupVerification(
                "restored backup differs from verified source".into(),
            ));
        }
        Ok(verified)
    }

    /// Applies one versioned teammate migration after creating and verifying a restorable backup.
    /// A committed version is a replay barrier; an interrupted transaction remains safely retryable.
    ///
    /// # Errors
    /// Returns an error before mutation when backup creation or verification fails, or rolls the
    /// entire batch back when a mutation or commit fails.
    pub fn migrate_teammates(
        &self,
        version: &str,
        backup: &Path,
        mutations: &[RecordMutation],
    ) -> Result<TeammateMigrationOutcome, StoreError> {
        if !valid_migration_version(version) {
            return Err(StoreError::InvalidMigrationVersion);
        }
        let mutation_bytes = keith_agent_types::canonical_json_bytes(&mutations)
            .map_err(|error| StoreError::InvalidMigrationMarker(error.to_string()))?;
        let mutation_digest = hex_sha256(&mutation_bytes);
        let marker_id = migration_marker_id(version);
        if let Some(marker) = self.get_record(Collection::SchemaMigrations, &marker_id)? {
            validate_migration_marker(&marker, version, &mutation_digest)?;
            return Ok(TeammateMigrationOutcome::AlreadyApplied);
        }

        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        backup_connection(&connection, backup)?;
        verify_equivalent_backup(&connection, backup)?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        if let Some(marker) = read_record(&transaction, Collection::SchemaMigrations, &marker_id)? {
            validate_migration_marker(&marker, version, &mutation_digest)?;
            transaction.rollback()?;
            return Ok(TeammateMigrationOutcome::AlreadyApplied);
        }
        validate_teammate_uniqueness(&transaction, mutations)?;
        validate_migration_direct_conversation_bindings(mutations)?;
        for (index, mutation) in mutations.iter().enumerate() {
            self.fail_before_migration_write(index)?;
            apply_mutation_inner(&transaction, mutation, MutationAuthority::TeammateMigration)?;
        }
        let marker = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: marker_id,
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::now()
                .map_err(|error| StoreError::Clock(error.to_string()))?,
            payload: serde_json::json!({
                "kind": TEAMMATE_MIGRATION_MARKER_KIND,
                "version": version,
                "store_schema_version": STORE_SCHEMA_VERSION,
                "mutations_sha256": mutation_digest,
            }),
        };
        self.fail_before_migration_write(mutations.len())?;
        apply_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::SchemaMigrations,
                record: marker,
                precondition: WritePrecondition::Missing,
            },
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        let receipt = CommitReceipt {
            applied_mutations: mutations.len() + 1,
        };
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(TeammateMigrationOutcome::Applied(receipt))
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

    fn fail_before_migration_write(&self, index: usize) -> Result<(), StoreError> {
        if self
            .fault_write_once
            .compare_exchange(index, usize::MAX, Ordering::SeqCst, Ordering::SeqCst)
            .is_ok()
        {
            Err(StoreError::InjectedWrite(index))
        } else {
            Ok(())
        }
    }
}

impl AtomicStateRepository for EmbeddedStore {
    type Error = StoreError;

    fn transact(&self, mutations: &[RecordMutation]) -> Result<CommitReceipt, Self::Error> {
        self.transact_records(mutations)
    }
}

impl StateRecordRepository for EmbeddedStore {
    fn get_record(
        &self,
        collection: Collection,
        id: &EntityId,
    ) -> Result<Option<VersionedRecord>, Self::Error> {
        EmbeddedStore::get_record(self, collection, id)
    }

    fn list_records(&self, collection: Collection) -> Result<Vec<VersionedRecord>, Self::Error> {
        EmbeddedStore::list_records(self, collection)
    }
}

impl ProfileExecutionAdmissionRepository for EmbeddedStore {
    fn initialize_profile_execution_fence(
        &self,
        profile_id: &ProfileId,
        expected_profile_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error> {
        self.transact_initialize_profile_execution_fence(profile_id, expected_profile_revision, now)
    }

    fn profile_execution_snapshot(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error> {
        self.read_profile_execution_snapshot(profile_id)
    }

    fn admit_profile_execution(
        &self,
        request: &ProfileExecutionAdmissionRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionAdmissionOutcome, Self::Error> {
        self.transact_admit_profile_execution(request, now)
    }

    fn renew_profile_execution(
        &self,
        permit: &ProfileExecutionPermit,
        lease_expires_at: UtcTimestamp,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionPermit, Self::Error> {
        self.transact_renew_profile_execution(permit, lease_expires_at, now)
    }

    fn close_profile_execution_fence(
        &self,
        request: &ProfileExecutionCloseRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error> {
        self.transact_close_profile_execution_fence(request, now)
    }

    fn complete_profile_execution(
        &self,
        permit: &ProfileExecutionPermit,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error> {
        self.transact_complete_profile_execution(permit, now)
    }

    fn reclaim_profile_executions(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error> {
        self.transact_reclaim_profile_executions(profile_id, now)
    }

    fn reopen_profile_execution_fence(
        &self,
        request: &ProfileExecutionReopenRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, Self::Error> {
        self.transact_reopen_profile_execution_fence(request, now)
    }

    fn transact_profile_execution_commit(
        &self,
        permit: &ProfileExecutionPermit,
        mutations: &[RecordMutation],
        now: UtcTimestamp,
    ) -> Result<CommitReceipt, Self::Error> {
        self.transact_profile_execution_mutations(permit, mutations, now)
    }
}

impl CanonicalConversationRepository for EmbeddedStore {
    fn append_canonical_conversation(
        &self,
        append: &CanonicalConversationAppend,
    ) -> Result<CanonicalAppendOutcome, Self::Error> {
        self.transact_canonical_conversation_append(append)
    }

    fn rebuild_conversation_projections(
        &self,
        rebuild: &ConversationProjectionRebuild,
    ) -> Result<CommitReceipt, Self::Error> {
        self.transact_conversation_projection_rebuild(rebuild)
    }
}

impl DirectConversationRepository for EmbeddedStore {
    fn bind_direct_conversation(
        &self,
        binding: &CanonicalDirectConversationBinding,
    ) -> Result<CanonicalDirectConversationOutcome, Self::Error> {
        self.transact_direct_conversation_binding(binding)
    }
}

impl ConversationDeliveryRepository for EmbeddedStore {
    fn finalize_conversation_delivery(
        &self,
        finalization: &ConversationDeliveryFinalization,
    ) -> Result<ConversationDeliveryFinalizationOutcome, Self::Error> {
        self.transact_conversation_delivery_finalization(finalization)
    }
}

impl GroupMembershipRepository for EmbeddedStore {
    fn transition_group_membership(
        &self,
        transition: &GroupMembershipTransition,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error> {
        self.transact_group_membership(transition)
    }
}

impl CollaborationRoundRepository for EmbeddedStore {
    fn transition_collaboration_round(
        &self,
        transition: &CollaborationRoundTransition,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error> {
        self.transact_collaboration_round(transition)
    }
}

impl AssignmentRepository for EmbeddedStore {
    fn transition_assignment(
        &self,
        transition: &AssignmentTransition,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error> {
        self.transact_assignment(transition)
    }

    fn handoff_assignment(
        &self,
        handoff: &AssignmentHandoffTransaction,
    ) -> Result<AuthoritativeTransitionOutcome, Self::Error> {
        self.transact_assignment_handoff(handoff)
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
    fn transact_initialize_profile_execution_fence(
        &self,
        profile_id: &ProfileId,
        expected_profile_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        require_enabled_profile(&transaction, profile_id, expected_profile_revision)?;
        if read_record(
            &transaction,
            Collection::ProfileExecutionFences,
            profile_id.as_entity_id(),
        )?
        .is_some()
        {
            let snapshot = profile_execution_snapshot_in_transaction(
                &transaction,
                profile_id,
                ProfileExecutionMutationStatus::Replay,
            )?;
            transaction.rollback()?;
            return Ok(snapshot);
        }
        let fence = ProfileExecutionFence {
            version: CURRENT_SCHEMA_VERSION,
            profile_id: profile_id.clone(),
            state: ProfileExecutionFenceState::Open,
            epoch: 0,
            revision: Revision::ZERO,
            cancellation_requested_at: None,
            updated_at: now,
        };
        apply_profile_execution_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ProfileExecutionFences,
                record: encode_profile_execution_fence(&fence)?,
                precondition: WritePrecondition::Missing,
            },
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(ProfileExecutionFenceSnapshot {
            status: ProfileExecutionMutationStatus::Applied,
            fence,
            active: Vec::new(),
        })
    }

    fn read_profile_execution_snapshot(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
        let connection = self.lock()?;
        profile_execution_snapshot_in_connection(
            &connection,
            profile_id,
            ProfileExecutionMutationStatus::Replay,
        )
    }

    fn transact_admit_profile_execution(
        &self,
        request: &ProfileExecutionAdmissionRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionAdmissionOutcome, StoreError> {
        if request.lease_expires_at <= now {
            return Err(StoreError::InvalidProfileExecution(
                "execution lease must expire after admission",
            ));
        }
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        require_enabled_profile(
            &transaction,
            &request.profile_id,
            request.expected_profile_revision,
        )?;
        require_profile_execution_session_binding(&transaction, request)?;
        let fence = required_profile_execution_fence(&transaction, &request.profile_id)?;
        if fence.state != ProfileExecutionFenceState::Open
            || fence.epoch != request.expected_fence_epoch
            || fence.revision != request.expected_fence_revision
        {
            return Err(profile_execution_rejected(
                &request.profile_id,
                "fence is not the expected open epoch",
            ));
        }
        if request.worker_binding.root_tree_id != request.root_tree_id
            || request.worker_binding.worker_id != request.worker_id
            || request.worker_lease_expires_at <= now
        {
            return Err(StoreError::InvalidProfileExecution(
                "validated worker binding is stale or inconsistent",
            ));
        }
        let worker = request.worker_binding.clone();
        let worker_lease_expires_at = request.worker_lease_expires_at;
        if request.lease_expires_at > worker_lease_expires_at {
            return Err(StoreError::InvalidProfileExecution(
                "execution lease exceeds worker lease",
            ));
        }
        if let Some(existing) = read_record(
            &transaction,
            Collection::ProfileExecutionRegistrations,
            &request.registration_id,
        )? {
            let registration = decode_profile_execution_registration(&existing)?;
            if registration_replays_request(&registration, request, &worker)
                && registration.state == ProfileExecutionRegistrationState::Active
                && registration.cancellation_requested_at.is_none()
                && registration.lease_expires_at > now
            {
                let permit = profile_execution_permit(&registration);
                transaction.rollback()?;
                return Ok(ProfileExecutionAdmissionOutcome {
                    status: ProfileExecutionMutationStatus::Replay,
                    permit,
                });
            }
            return Err(profile_execution_rejected(
                &request.profile_id,
                "registration identity replay conflict",
            ));
        }
        let registration =
            new_profile_execution_registration(&transaction, request, worker, fence.epoch, now)?;
        apply_profile_execution_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ProfileExecutionRegistrations,
                record: encode_profile_execution_registration(&registration)?,
                precondition: WritePrecondition::Missing,
            },
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        let permit = profile_execution_permit(&registration);
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(ProfileExecutionAdmissionOutcome {
            status: ProfileExecutionMutationStatus::Applied,
            permit,
        })
    }

    fn transact_renew_profile_execution(
        &self,
        permit: &ProfileExecutionPermit,
        lease_expires_at: UtcTimestamp,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionPermit, StoreError> {
        if lease_expires_at <= now {
            return Err(StoreError::InvalidProfileExecution(
                "renewed execution lease must be live",
            ));
        }
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let stored = read_record(
            &transaction,
            Collection::ProfileExecutionRegistrations,
            &permit.registration_id,
        )?
        .ok_or_else(|| profile_execution_rejected(&permit.profile_id, "registration is missing"))?;
        let mut registration = decode_profile_execution_registration(&stored)?;
        if registration.revision != permit.registration_revision {
            if registration_static_identity_matches(&registration, permit)
                && registration.state == ProfileExecutionRegistrationState::Active
                && registration.cancellation_requested_at.is_none()
                && registration.lease_expires_at == lease_expires_at
            {
                let replay = profile_execution_permit(&registration);
                transaction.rollback()?;
                return Ok(replay);
            }
            return Err(profile_execution_rejected(
                &permit.profile_id,
                "registration revision is stale",
            ));
        }
        validate_active_profile_execution_permit(&transaction, &registration, permit, now)?;
        if lease_expires_at <= registration.lease_expires_at {
            return Err(StoreError::InvalidProfileExecution(
                "renewal must advance the execution lease",
            ));
        }
        registration.lease_expires_at = lease_expires_at;
        registration.revision = registration
            .revision
            .checked_next()
            .ok_or(StoreError::ProfileExecutionOverflow)?;
        registration.updated_at = now;
        apply_profile_execution_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ProfileExecutionRegistrations,
                record: encode_profile_execution_registration(&registration)?,
                precondition: WritePrecondition::Exact(permit.registration_revision),
            },
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        let renewed = profile_execution_permit(&registration);
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(renewed)
    }

    fn transact_close_profile_execution_fence(
        &self,
        request: &ProfileExecutionCloseRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let mut fence = required_profile_execution_fence(&transaction, &request.profile_id)?;
        let closed_epoch = request
            .expected_epoch
            .checked_add(1)
            .ok_or(StoreError::ProfileExecutionOverflow)?;
        if fence.epoch == closed_epoch
            && matches!(
                fence.state,
                ProfileExecutionFenceState::Closing | ProfileExecutionFenceState::Closed
            )
        {
            let snapshot = profile_execution_snapshot_in_transaction(
                &transaction,
                &request.profile_id,
                ProfileExecutionMutationStatus::Replay,
            )?;
            transaction.rollback()?;
            return Ok(snapshot);
        }
        if fence.state != ProfileExecutionFenceState::Open
            || fence.epoch != request.expected_epoch
            || fence.revision != request.expected_revision
        {
            return Err(profile_execution_rejected(
                &request.profile_id,
                "close precondition is stale",
            ));
        }
        let mut active = active_profile_execution_registrations(&transaction, &request.profile_id)?;
        for registration in &mut active {
            registration.cancellation_requested_at = Some(now);
            let previous_revision = registration.revision;
            registration.revision = previous_revision
                .checked_next()
                .ok_or(StoreError::ProfileExecutionOverflow)?;
            registration.updated_at = now;
            apply_profile_execution_mutation(
                &transaction,
                &RecordMutation::Put {
                    collection: Collection::ProfileExecutionRegistrations,
                    record: encode_profile_execution_registration(registration)?,
                    precondition: WritePrecondition::Exact(previous_revision),
                },
            )?;
        }
        let previous_revision = fence.revision;
        fence.epoch = closed_epoch;
        fence.revision = previous_revision
            .checked_next()
            .ok_or(StoreError::ProfileExecutionOverflow)?;
        fence.state = if active.is_empty() {
            ProfileExecutionFenceState::Closed
        } else {
            ProfileExecutionFenceState::Closing
        };
        fence.cancellation_requested_at = Some(now);
        fence.updated_at = now;
        apply_profile_execution_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ProfileExecutionFences,
                record: encode_profile_execution_fence(&fence)?,
                precondition: WritePrecondition::Exact(previous_revision),
            },
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(ProfileExecutionFenceSnapshot {
            status: ProfileExecutionMutationStatus::Applied,
            fence,
            active,
        })
    }

    fn transact_complete_profile_execution(
        &self,
        permit: &ProfileExecutionPermit,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let stored = read_record(
            &transaction,
            Collection::ProfileExecutionRegistrations,
            &permit.registration_id,
        )?
        .ok_or_else(|| profile_execution_rejected(&permit.profile_id, "registration is missing"))?;
        let mut registration = decode_profile_execution_registration(&stored)?;
        if registration.state == ProfileExecutionRegistrationState::Completed
            && registration_static_identity_matches(&registration, permit)
            && registration.lease_expires_at == permit.lease_expires_at
        {
            let snapshot = profile_execution_snapshot_in_transaction(
                &transaction,
                &permit.profile_id,
                ProfileExecutionMutationStatus::Replay,
            )?;
            transaction.rollback()?;
            return Ok(snapshot);
        }
        if !registration_accepts_completion(&registration, permit)
            || registration.state != ProfileExecutionRegistrationState::Active
        {
            return Err(profile_execution_rejected(
                &permit.profile_id,
                "completion ownership is stale",
            ));
        }
        let previous_registration_revision = registration.revision;
        registration.state = ProfileExecutionRegistrationState::Completed;
        registration.terminal_at = Some(now);
        registration.revision = previous_registration_revision
            .checked_next()
            .ok_or(StoreError::ProfileExecutionOverflow)?;
        registration.updated_at = now;
        apply_profile_execution_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ProfileExecutionRegistrations,
                record: encode_profile_execution_registration(&registration)?,
                precondition: WritePrecondition::Exact(previous_registration_revision),
            },
        )?;
        seal_closing_fence_if_quiescent(&transaction, &permit.profile_id, now)?;
        let snapshot = profile_execution_snapshot_in_transaction(
            &transaction,
            &permit.profile_id,
            ProfileExecutionMutationStatus::Applied,
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(snapshot)
    }

    fn transact_reclaim_profile_executions(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        required_profile_execution_fence(&transaction, profile_id)?;
        let active = active_profile_execution_registrations(&transaction, profile_id)?;
        let mut reclaimed = 0_usize;
        for mut registration in active {
            if registration.lease_expires_at > now
                && worker_binding_is_live(&transaction, &registration.worker, now)?
            {
                continue;
            }
            let previous_revision = registration.revision;
            registration.state = ProfileExecutionRegistrationState::Reclaimed;
            registration.terminal_at = Some(now);
            registration.revision = previous_revision
                .checked_next()
                .ok_or(StoreError::ProfileExecutionOverflow)?;
            registration.updated_at = now;
            apply_profile_execution_mutation(
                &transaction,
                &RecordMutation::Put {
                    collection: Collection::ProfileExecutionRegistrations,
                    record: encode_profile_execution_registration(&registration)?,
                    precondition: WritePrecondition::Exact(previous_revision),
                },
            )?;
            reclaimed += 1;
        }
        let sealed = seal_closing_fence_if_quiescent(&transaction, profile_id, now)?;
        let snapshot = profile_execution_snapshot_in_transaction(
            &transaction,
            profile_id,
            if reclaimed == 0 && !sealed {
                ProfileExecutionMutationStatus::Replay
            } else {
                ProfileExecutionMutationStatus::Applied
            },
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(snapshot)
    }

    fn transact_reopen_profile_execution_fence(
        &self,
        request: &ProfileExecutionReopenRequest,
        now: UtcTimestamp,
    ) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        require_enabled_profile(
            &transaction,
            &request.profile_id,
            request.expected_profile_revision,
        )?;
        let mut fence = required_profile_execution_fence(&transaction, &request.profile_id)?;
        let open_epoch = request
            .expected_epoch
            .checked_add(1)
            .ok_or(StoreError::ProfileExecutionOverflow)?;
        if fence.state == ProfileExecutionFenceState::Open && fence.epoch == open_epoch {
            let snapshot = profile_execution_snapshot_in_transaction(
                &transaction,
                &request.profile_id,
                ProfileExecutionMutationStatus::Replay,
            )?;
            transaction.rollback()?;
            return Ok(snapshot);
        }
        if fence.state != ProfileExecutionFenceState::Closed
            || fence.epoch != request.expected_epoch
            || fence.revision != request.expected_revision
            || !active_profile_execution_registrations(&transaction, &request.profile_id)?
                .is_empty()
        {
            return Err(profile_execution_rejected(
                &request.profile_id,
                "reopen precondition is stale or not quiescent",
            ));
        }
        let previous_revision = fence.revision;
        fence.state = ProfileExecutionFenceState::Open;
        fence.epoch = open_epoch;
        fence.revision = previous_revision
            .checked_next()
            .ok_or(StoreError::ProfileExecutionOverflow)?;
        fence.cancellation_requested_at = None;
        fence.updated_at = now;
        apply_profile_execution_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ProfileExecutionFences,
                record: encode_profile_execution_fence(&fence)?,
                precondition: WritePrecondition::Exact(previous_revision),
            },
        )?;
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(ProfileExecutionFenceSnapshot {
            status: ProfileExecutionMutationStatus::Applied,
            fence,
            active: Vec::new(),
        })
    }

    fn transact_profile_execution_mutations(
        &self,
        permit: &ProfileExecutionPermit,
        mutations: &[RecordMutation],
        now: UtcTimestamp,
    ) -> Result<CommitReceipt, StoreError> {
        if mutations.len() > 4_096
            || mutations.iter().any(|mutation| {
                matches!(
                    mutation,
                    RecordMutation::Put {
                        collection: Collection::Profiles
                            | Collection::WorkerLeases
                            | Collection::WorkerGenerations
                            | Collection::ProfileExecutionFences
                            | Collection::ProfileExecutionRegistrations,
                        ..
                    } | RecordMutation::Delete {
                        collection: Collection::Profiles
                            | Collection::WorkerLeases
                            | Collection::WorkerGenerations
                            | Collection::ProfileExecutionFences
                            | Collection::ProfileExecutionRegistrations,
                        ..
                    }
                )
            })
        {
            return Err(StoreError::InvalidProfileExecution(
                "final commit contains a protected mutation",
            ));
        }
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let stored = read_record(
            &transaction,
            Collection::ProfileExecutionRegistrations,
            &permit.registration_id,
        )?
        .ok_or_else(|| profile_execution_rejected(&permit.profile_id, "registration is missing"))?;
        let registration = decode_profile_execution_registration(&stored)?;
        validate_active_profile_execution_permit(&transaction, &registration, permit, now)?;
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

    fn transact_group_membership(
        &self,
        transition: &GroupMembershipTransition,
    ) -> Result<AuthoritativeTransitionOutcome, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let append = &transition.canonical_append;
        let mut mutations =
            Vec::with_capacity(transition.participant_mutations.len() + append.intents.len() + 3);
        mutations.push(RecordMutation::Put {
            collection: Collection::ConversationEvents,
            record: append.event.clone(),
            precondition: WritePrecondition::Missing,
        });
        mutations.push(RecordMutation::Put {
            collection: Collection::ConversationStableKeys,
            record: append.stable_key.clone(),
            precondition: WritePrecondition::Missing,
        });
        mutations.push(RecordMutation::Put {
            collection: Collection::Conversations,
            record: append.head.clone(),
            precondition: WritePrecondition::Exact(append.expected_head_revision),
        });
        mutations.extend(append.intents.iter().cloned());
        mutations.extend(transition.participant_mutations.iter().cloned());
        if mutations_are_replayed(&transaction, &mutations)? {
            let record = append.head.clone();
            transaction.rollback()?;
            return Ok(AuthoritativeTransitionOutcome::Replay { record });
        }
        validate_group_membership_transition(&transaction, transition)?;
        commit_authoritative_mutations(self, transaction, mutations, append.head.clone())
    }

    fn transact_collaboration_round(
        &self,
        transition: &CollaborationRoundTransition,
    ) -> Result<AuthoritativeTransitionOutcome, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let current = read_record(
            &transaction,
            Collection::CollaborationRounds,
            &transition.round.id,
        )?;
        let mut side_effects = transition.delivery_mutations.clone();
        side_effects.extend(transition.supersessions.iter().cloned().map(|record| {
            RecordMutation::Put {
                collection: Collection::ConversationSupersessions,
                record,
                precondition: WritePrecondition::Missing,
            }
        }));
        if current.as_ref() == Some(&transition.round)
            && mutations_are_replayed(&transaction, &side_effects)?
        {
            let record = transition.round.clone();
            transaction.rollback()?;
            return Ok(AuthoritativeTransitionOutcome::Replay { record });
        }
        validate_collaboration_round_transition(&transaction, current.as_ref(), transition)?;
        let mut mutations = Vec::with_capacity(side_effects.len() + 1);
        mutations.push(RecordMutation::Put {
            collection: Collection::CollaborationRounds,
            record: transition.round.clone(),
            precondition: transition.precondition,
        });
        mutations.extend(side_effects);
        commit_authoritative_mutations(self, transaction, mutations, transition.round.clone())
    }

    fn transact_assignment(
        &self,
        transition: &AssignmentTransition,
    ) -> Result<AuthoritativeTransitionOutcome, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let current = read_record(
            &transaction,
            Collection::Assignments,
            &transition.assignment.id,
        )?;
        if current.as_ref() == Some(&transition.assignment) {
            let record = transition.assignment.clone();
            transaction.rollback()?;
            return Ok(AuthoritativeTransitionOutcome::Replay { record });
        }
        let required_precondition = current
            .as_ref()
            .map_or(WritePrecondition::Missing, |record| {
                WritePrecondition::Exact(record.revision)
            });
        if transition.precondition != required_precondition {
            return Err(StoreError::InvalidAuthoritativeTransition(
                "assignment transition must use exact durable CAS",
            ));
        }
        validate_assignment_transition(&transaction, current.as_ref(), &transition.assignment)?;
        commit_authoritative_mutations(
            self,
            transaction,
            vec![RecordMutation::Put {
                collection: Collection::Assignments,
                record: transition.assignment.clone(),
                precondition: transition.precondition,
            }],
            transition.assignment.clone(),
        )
    }

    fn transact_assignment_handoff(
        &self,
        handoff: &AssignmentHandoffTransaction,
    ) -> Result<AuthoritativeTransitionOutcome, StoreError> {
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let current = read_record(
            &transaction,
            Collection::Assignments,
            &handoff.assignment.id,
        )?;
        let mut side_effects = handoff.obsolete_delivery_mutations.clone();
        side_effects.push(RecordMutation::Put {
            collection: Collection::ConversationDeliveries,
            record: handoff.new_owner_delivery.clone(),
            precondition: WritePrecondition::Missing,
        });
        side_effects.push(RecordMutation::Put {
            collection: Collection::TeammateAudits,
            record: handoff.handoff_audit.clone(),
            precondition: WritePrecondition::Missing,
        });
        if current.as_ref() == Some(&handoff.assignment)
            && mutations_are_replayed(&transaction, &side_effects)?
        {
            let record = handoff.assignment.clone();
            transaction.rollback()?;
            return Ok(AuthoritativeTransitionOutcome::Replay { record });
        }
        validate_assignment_handoff(&transaction, current.as_ref(), handoff)?;
        let mut mutations = Vec::with_capacity(side_effects.len() + 1);
        mutations.push(RecordMutation::Put {
            collection: Collection::Assignments,
            record: handoff.assignment.clone(),
            precondition: WritePrecondition::Exact(handoff.expected_assignment_revision),
        });
        mutations.extend(side_effects);
        commit_authoritative_mutations(self, transaction, mutations, handoff.assignment.clone())
    }

    fn transact_direct_conversation_binding(
        &self,
        binding: &CanonicalDirectConversationBinding,
    ) -> Result<CanonicalDirectConversationOutcome, StoreError> {
        validate_direct_conversation_binding(binding)?;
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        if let Some(existing) =
            read_record(&transaction, Collection::DirectMessageKeys, &binding.key.id)?
        {
            if existing == binding.key
                && direct_conversation_replay_is_complete(&transaction, binding)?
            {
                let conversation = binding.conversation.clone();
                transaction.rollback()?;
                return Ok(CanonicalDirectConversationOutcome::Replay { conversation });
            }
            return Err(StoreError::InvalidDirectConversation(
                "direct-message key replay is partial or mismatched",
            ));
        }
        let mut mutations = Vec::with_capacity(binding.participants.len() + 2);
        mutations.push(RecordMutation::Put {
            collection: Collection::DirectMessageKeys,
            record: binding.key.clone(),
            precondition: WritePrecondition::Missing,
        });
        mutations.push(RecordMutation::Put {
            collection: Collection::Conversations,
            record: binding.conversation.clone(),
            precondition: WritePrecondition::Missing,
        });
        mutations.extend(
            binding
                .participants
                .iter()
                .cloned()
                .map(|record| RecordMutation::Put {
                    collection: Collection::ConversationParticipants,
                    record,
                    precondition: WritePrecondition::Missing,
                }),
        );
        validate_teammate_uniqueness(&transaction, &mutations)?;
        for mutation in &mutations {
            apply_mutation_inner(
                &transaction,
                mutation,
                MutationAuthority::DirectConversationBinding,
            )?;
        }
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(CanonicalDirectConversationOutcome::Applied {
            receipt: CommitReceipt {
                applied_mutations: mutations.len(),
            },
        })
    }

    fn transact_conversation_delivery_finalization(
        &self,
        finalization: &ConversationDeliveryFinalization,
    ) -> Result<ConversationDeliveryFinalizationOutcome, StoreError> {
        validate_conversation_delivery_finalization(finalization)?;
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        if delivery_finalization_is_replay(&transaction, finalization)? {
            let delivery = finalization.delivery.clone();
            transaction.rollback()?;
            return Ok(ConversationDeliveryFinalizationOutcome::Replay { delivery });
        }
        let mut mutations = Vec::with_capacity(4);
        mutations.push(RecordMutation::Put {
            collection: Collection::ConversationDeliveries,
            record: finalization.delivery.clone(),
            precondition: WritePrecondition::Exact(finalization.expected_delivery_revision),
        });
        mutations.push(RecordMutation::Put {
            collection: Collection::ConversationFinalizationIntents,
            record: finalization.finalization_intent.clone(),
            precondition: WritePrecondition::Missing,
        });
        if let Some(record) = &finalization.publication_outbox {
            mutations.push(RecordMutation::Put {
                collection: Collection::ConversationPublicationOutbox,
                record: record.clone(),
                precondition: WritePrecondition::Missing,
            });
        }
        if let Some(record) = &finalization.supersession {
            mutations.push(RecordMutation::Put {
                collection: Collection::ConversationSupersessions,
                record: record.clone(),
                precondition: WritePrecondition::Missing,
            });
        }
        validate_teammate_uniqueness(&transaction, &mutations)?;
        for mutation in &mutations {
            apply_mutation_inner(
                &transaction,
                mutation,
                MutationAuthority::ConversationDeliveryFinalization,
            )?;
        }
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(ConversationDeliveryFinalizationOutcome::Applied {
            receipt: CommitReceipt {
                applied_mutations: mutations.len(),
            },
        })
    }

    fn transact_canonical_conversation_append(
        &self,
        append: &CanonicalConversationAppend,
    ) -> Result<CanonicalAppendOutcome, StoreError> {
        validate_canonical_append(append)?;
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        if let Some(existing_key) = read_record(
            &transaction,
            Collection::ConversationStableKeys,
            &append.stable_key.id,
        )? {
            let existing_event = read_record(
                &transaction,
                Collection::ConversationEvents,
                &append.event.id,
            )?
            .ok_or(StoreError::StableKeyCollision)?;
            if existing_key == append.stable_key && existing_event == append.event {
                transaction.rollback()?;
                return Ok(CanonicalAppendOutcome::Replay {
                    event: existing_event,
                });
            }
            return Err(StoreError::StableKeyCollision);
        }
        if read_record(
            &transaction,
            Collection::ConversationEvents,
            &append.event.id,
        )?
        .is_some()
        {
            return Err(StoreError::StableKeyCollision);
        }
        apply_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ConversationStableKeys,
                record: append.stable_key.clone(),
                precondition: WritePrecondition::Missing,
            },
        )?;
        apply_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::ConversationEvents,
                record: append.event.clone(),
                precondition: WritePrecondition::Missing,
            },
        )?;
        apply_mutation(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::Conversations,
                record: append.head.clone(),
                precondition: WritePrecondition::Exact(append.expected_head_revision),
            },
        )?;
        for mutation in &append.intents {
            apply_mutation(&transaction, mutation)?;
        }
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        let receipt = CommitReceipt {
            applied_mutations: append.intents.len() + 3,
        };
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(CanonicalAppendOutcome::Applied { receipt })
    }

    fn transact_conversation_projection_rebuild(
        &self,
        rebuild: &ConversationProjectionRebuild,
    ) -> Result<CommitReceipt, StoreError> {
        if rebuild.mutations.len() > 4096
            || rebuild
                .mutations
                .iter()
                .any(|mutation| !is_projection_mutation(mutation))
        {
            return Err(StoreError::InvalidCanonicalAppend(
                "projection rebuild contains an invalid mutation",
            ));
        }
        self.fail_if(FaultPoint::BeforeTransaction)?;
        let mut connection = self.lock()?;
        let transaction = connection.transaction_with_behavior(TransactionBehavior::Immediate)?;
        let revision = current_revision(
            &transaction,
            Collection::Conversations,
            &rebuild.conversation_id,
        )?;
        check_precondition(
            Collection::Conversations,
            &rebuild.conversation_id,
            revision,
            WritePrecondition::Exact(rebuild.expected_head_revision),
        )?;
        for mutation in &rebuild.mutations {
            apply_mutation(&transaction, mutation)?;
        }
        self.fail_if(FaultPoint::BeforeCommit)?;
        transaction.commit()?;
        if self.take_fault(FaultPoint::AfterCommit) {
            return Err(StoreError::UnknownOutcome);
        }
        Ok(CommitReceipt {
            applied_mutations: rebuild.mutations.len(),
        })
    }

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
            MutationAuthority::EvolutionAppend,
        )?;
        apply_mutation_inner(
            &transaction,
            &RecordMutation::Put {
                collection: Collection::EvolutionLedgerHead,
                record: head,
                precondition: head_precondition,
            },
            MutationAuthority::EvolutionAppend,
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

fn migration_marker_id(version: &str) -> EntityId {
    let digest = keith_agent_types::canonical_json_bytes(&serde_json::json!({
        "migration": "keith_teammates",
        "version": version,
    }))
    .expect("a string-only migration marker is canonical");
    let digest = sha256(&digest);
    EntityId::from_u128(u128::from_be_bytes(
        digest[..16].try_into().expect("fixed digest"),
    ))
}

fn valid_migration_version(version: &str) -> bool {
    !version.is_empty()
        && version.len() <= 128
        && version
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'-' | b'_'))
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct TeammateMigrationMarker {
    kind: String,
    version: String,
    store_schema_version: u32,
    mutations_sha256: String,
}

fn validate_migration_marker(
    record: &VersionedRecord,
    version: &str,
    mutation_digest: &str,
) -> Result<(), StoreError> {
    let marker: TeammateMigrationMarker = serde_json::from_value(record.payload.clone())
        .map_err(|error| StoreError::InvalidMigrationMarker(error.to_string()))?;
    if record.version.major != CURRENT_SCHEMA_VERSION.major
        || record.revision != Revision::ZERO
        || marker.kind != TEAMMATE_MIGRATION_MARKER_KIND
        || marker.version != version
        || marker.store_schema_version != STORE_SCHEMA_VERSION
    {
        return Err(StoreError::InvalidMigrationMarker(
            "identity or schema compatibility mismatch".into(),
        ));
    }
    if marker.mutations_sha256 != mutation_digest {
        return Err(StoreError::MigrationReplayMismatch);
    }
    Ok(())
}

fn hex_sha256(bytes: &[u8]) -> String {
    let mut output = String::with_capacity(64);
    for byte in sha256(bytes) {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("writing to a String cannot fail");
    }
    output
}

fn backup_connection(connection: &Connection, destination: &Path) -> Result<(), StoreError> {
    if destination.exists() {
        return Err(StoreError::Backup(io::Error::new(
            io::ErrorKind::AlreadyExists,
            "backup destination exists",
        )));
    }
    let destination = destination
        .to_str()
        .ok_or_else(|| io::Error::new(io::ErrorKind::InvalidInput, "non-UTF-8 backup path"))?;
    connection.execute("VACUUM INTO ?1", params![destination])?;
    Ok(())
}

fn verify_equivalent_backup(source: &Connection, path: &Path) -> Result<(), StoreError> {
    let connection = open_verified_backup(path)?;
    if raw_store_snapshot(source)? != raw_store_snapshot(&connection)? {
        return Err(StoreError::BackupVerification(
            "restored backup bytes differ from source snapshot".into(),
        ));
    }
    Ok(())
}

fn open_verified_backup(path: &Path) -> Result<Connection, StoreError> {
    let metadata = fs::symlink_metadata(path)
        .map_err(|error| StoreError::BackupVerification(error.to_string()))?;
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        return Err(StoreError::BackupVerification(
            "backup must be a regular non-symlink file".into(),
        ));
    }
    let connection = Connection::open_with_flags(path, rusqlite::OpenFlags::SQLITE_OPEN_READ_ONLY)
        .map_err(|error| StoreError::BackupVerification(error.to_string()))?;
    let result: String = connection
        .query_row("PRAGMA integrity_check", [], |row| row.get(0))
        .map_err(|error| StoreError::BackupVerification(error.to_string()))?;
    if result != "ok" {
        return Err(StoreError::BackupVerification(result));
    }
    Ok(connection)
}

type RawStoreSnapshotRow = (String, String, i64, i64, i64, i64, Vec<u8>);

fn raw_store_snapshot(connection: &Connection) -> Result<Vec<RawStoreSnapshotRow>, StoreError> {
    let mut statement = connection.prepare(
        "SELECT collection, id, schema_major, schema_minor, revision, updated_at, payload
         FROM records ORDER BY collection, id",
    )?;
    let rows = statement.query_map([], |row| {
        Ok((
            row.get(0)?,
            row.get(1)?,
            row.get(2)?,
            row.get(3)?,
            row.get(4)?,
            row.get(5)?,
            row.get(6)?,
        ))
    })?;
    rows.collect::<Result<Vec<_>, _>>()
        .map_err(StoreError::from)
}

fn validate_teammate_uniqueness(
    transaction: &Transaction<'_>,
    mutations: &[RecordMutation],
) -> Result<(), StoreError> {
    let collections = mutations
        .iter()
        .map(|mutation| match mutation {
            RecordMutation::Put { collection, .. } | RecordMutation::Delete { collection, .. } => {
                *collection
            }
        })
        .filter(|collection| teammate_collection(*collection))
        .collect::<BTreeSet<_>>();
    for collection in collections {
        let mut keys = BTreeMap::<String, EntityId>::new();
        for record in list_records_in_transaction(transaction, collection)? {
            for key in domain_unique_keys(collection, &record.payload)? {
                keys.insert(key, record.id.clone());
            }
        }
        for mutation in mutations {
            match mutation {
                RecordMutation::Put {
                    collection: candidate,
                    record,
                    ..
                } if *candidate == collection => {
                    keys.retain(|_, id| id != &record.id);
                    for key in domain_unique_keys(collection, &record.payload)? {
                        if keys.insert(key, record.id.clone()).is_some() {
                            return Err(StoreError::Conflict {
                                collection,
                                id: record.id.clone(),
                            });
                        }
                    }
                }
                RecordMutation::Delete {
                    collection: candidate,
                    id,
                    ..
                } if *candidate == collection => {
                    keys.retain(|_, existing| existing != id);
                }
                _ => {}
            }
        }
    }
    Ok(())
}

fn teammate_collection(collection: Collection) -> bool {
    matches!(
        collection,
        Collection::SessionCatalog
            | Collection::Profiles
            | Collection::ProfileExecutionFences
            | Collection::ProfileExecutionRegistrations
            | Collection::PendingActions
            | Collection::Children
            | Collection::ChildMessages
            | Collection::Goals
            | Collection::Plans
            | Collection::Commitments
            | Collection::WaitingConditions
            | Collection::ScheduledJobs
            | Collection::JobAttempts
            | Collection::RoutingRules
            | Collection::ResourceGovernance
            | Collection::ChannelOffsets
            | Collection::Deliveries
            | Collection::AttentionCandidates
            | Collection::InitiativeHistory
            | Collection::ToolExperience
            | Collection::Conversations
            | Collection::ConversationParticipants
            | Collection::ConversationEvents
            | Collection::ConversationDeliveries
            | Collection::CollaborationRounds
            | Collection::Assignments
            | Collection::ReadReceipts
            | Collection::SharedKnowledgeGrants
            | Collection::ComputerRecords
            | Collection::TakeoverLeases
            | Collection::TeammateAudits
            | Collection::LegacySessions
            | Collection::MigrationProvenance
            | Collection::ConversationBindings
            | Collection::DirectMessageKeys
            | Collection::ConversationStableKeys
            | Collection::ConversationProjectionIntents
            | Collection::ConversationUnreadIntents
            | Collection::ConversationSearchIntents
            | Collection::ConversationPublicationIntents
            | Collection::ConversationPublicationOutbox
            | Collection::ConversationSupersessions
            | Collection::ConversationFinalizationIntents
            | Collection::AgentDeleteOperations
            | Collection::AgentDeleteReceipts
            | Collection::AgentDeleteAudits
            | Collection::ComputerAudits
            | Collection::AgentProvisionOperations
            | Collection::SharedKnowledgeSpaces
    )
}

fn domain_unique_keys(
    collection: Collection,
    payload: &serde_json::Value,
) -> Result<Vec<String>, StoreError> {
    let object = payload.as_object().ok_or_else(|| {
        StoreError::BackupVerification(format!("{collection:?} payload must be an object"))
    })?;
    let mut keys = Vec::new();
    for field in [
        "stable_key",
        "human_dm_profile_id",
        "agent_pair_key",
        "stable_source_key",
        "publication_key",
    ] {
        if let Some(value) = object.get(field) {
            let value = value.as_str().ok_or_else(|| {
                StoreError::BackupVerification(format!("{collection:?}.{field} must be a string"))
            })?;
            if value.is_empty() || value.len() > 512 {
                return Err(StoreError::BackupVerification(format!(
                    "{collection:?}.{field} is invalid"
                )));
            }
            keys.push(format!("{field}:{value}"));
        }
    }
    if collection == Collection::PendingActions
        && payload
            .pointer("/action/source/source")
            .and_then(serde_json::Value::as_str)
            == Some("peer_message")
    {
        let binding = payload
            .pointer("/action/source/binding")
            .and_then(serde_json::Value::as_object)
            .ok_or_else(|| {
                StoreError::BackupVerification(
                    "peer-message action binding must be an object".into(),
                )
            })?;
        let field = |name: &str| {
            binding
                .get(name)
                .and_then(serde_json::Value::as_str)
                .ok_or_else(|| {
                    StoreError::BackupVerification(format!(
                        "peer-message action binding.{name} must be a string"
                    ))
                })
        };
        let publication_key = field("publication_key")?;
        let conversation_id = field("conversation_id")?;
        let source_event_id = field("source_event_id")?;
        let destination_profile_id = field("destination_profile_id")?;
        if StableKey::parse(publication_key).is_err()
            || EntityId::parse(conversation_id).is_err()
            || EntityId::parse(source_event_id).is_err()
            || EntityId::parse(destination_profile_id).is_err()
        {
            return Err(StoreError::BackupVerification(
                "peer-message action binding contains a noncanonical key or ID".into(),
            ));
        }
        keys.push(format!("peer-publication:{publication_key}"));
        keys.push(format!(
            "peer-source:{conversation_id}:{source_event_id}:{destination_profile_id}"
        ));
    }
    if collection == Collection::ConversationSupersessions {
        let field = |name: &str| {
            object
                .get(name)
                .and_then(serde_json::Value::as_str)
                .ok_or_else(|| {
                    StoreError::BackupVerification(format!(
                        "conversation supersession.{name} must be a string"
                    ))
                })
        };
        let target_kind = field("target_kind")?;
        let target_id = field("target_id")?;
        let source_event_id = field("source_event_id")?;
        let context_revision = object
            .get("context_revision")
            .and_then(serde_json::Value::as_u64)
            .ok_or_else(|| {
                StoreError::BackupVerification(
                    "conversation supersession.context_revision must be an integer".into(),
                )
            })?;
        keys.push(format!(
            "supersession:{target_kind}:{target_id}:{source_event_id}:{context_revision}"
        ));
    }
    if matches!(
        collection,
        Collection::ComputerRecords | Collection::TakeoverLeases
    ) {
        let owner = object
            .get("owner_profile_id")
            .and_then(serde_json::Value::as_str)
            .ok_or_else(|| {
                StoreError::BackupVerification(format!(
                    "{collection:?}.owner_profile_id is required"
                ))
            })?;
        keys.push(format!("owner_profile_id:{owner}"));
    }
    Ok(keys)
}

fn list_records_in_transaction(
    transaction: &Transaction<'_>,
    collection: Collection,
) -> Result<Vec<VersionedRecord>, StoreError> {
    list_records_in_connection(transaction, collection)
}

fn list_records_in_connection(
    connection: &Connection,
    collection: Collection,
) -> Result<Vec<VersionedRecord>, StoreError> {
    let mut statement = connection.prepare("SELECT id, schema_major, schema_minor, revision, updated_at, payload FROM records WHERE collection = ?1 ORDER BY id")?;
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
    rows.map(|row| decode_record(collection, &row?)).collect()
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct StoredWorkerLease {
    root_tree_id: RootTreeId,
    worker_id: WorkerId,
    generation: Generation,
    authentication: EntityId,
    expires_at: UtcTimestamp,
}

#[derive(Deserialize)]
struct StoredSessionAuthority {
    session_id: keith_agent_types::SessionId,
    root_tree_id: RootTreeId,
    profile_id: ProfileId,
    archived: bool,
}

fn profile_execution_rejected(profile_id: &ProfileId, reason: &'static str) -> StoreError {
    StoreError::ProfileExecutionRejected {
        profile_id: profile_id.clone(),
        reason,
    }
}

fn require_enabled_profile(
    connection: &Connection,
    profile_id: &ProfileId,
    expected_revision: Revision,
) -> Result<(), StoreError> {
    let Some(record) = read_record(connection, Collection::Profiles, profile_id.as_entity_id())?
    else {
        return Err(StoreError::ProfileExecutionProfileNotEnabled {
            profile_id: profile_id.clone(),
        });
    };
    if record.revision != expected_revision {
        return Err(StoreError::Conflict {
            collection: Collection::Profiles,
            id: profile_id.as_entity_id().clone(),
        });
    }
    let enabled = record
        .payload
        .get("enabled")
        .and_then(serde_json::Value::as_bool);
    let lifecycle = record
        .payload
        .pointer("/teammate/presentation/lifecycle")
        .and_then(serde_json::Value::as_str)
        .or_else(|| {
            (!record
                .payload
                .as_object()
                .is_some_and(|payload| payload.contains_key("teammate"))
                && enabled == Some(true))
            .then_some("enabled")
        });
    let deletion = record.payload.pointer("/teammate/deletion");
    if enabled != Some(true)
        || lifecycle != Some("enabled")
        || deletion.is_some_and(|value| !value.is_null())
    {
        return Err(StoreError::ProfileExecutionProfileNotEnabled {
            profile_id: profile_id.clone(),
        });
    }
    Ok(())
}

fn require_profile_execution_session_binding(
    connection: &Connection,
    request: &ProfileExecutionAdmissionRequest,
) -> Result<(), StoreError> {
    let record = read_record(
        connection,
        Collection::SessionCatalog,
        request.session_id.as_entity_id(),
    )?
    .ok_or_else(|| {
        profile_execution_rejected(&request.profile_id, "session catalog entry is missing")
    })?;
    let authority: StoredSessionAuthority = serde_json::from_value(record.payload.clone())
        .map_err(|error| {
            corrupt(
                Collection::SessionCatalog,
                record.id.as_str(),
                format!("invalid session authority fields: {error}"),
            )
        })?;
    if record.id != *authority.session_id.as_entity_id() {
        return Err(corrupt(
            Collection::SessionCatalog,
            record.id.as_str(),
            "session catalog envelope ID mismatch".into(),
        ));
    }
    if authority.session_id != request.session_id
        || authority.profile_id != request.profile_id
        || authority.root_tree_id != request.root_tree_id
        || authority.archived
    {
        return Err(profile_execution_rejected(
            &request.profile_id,
            "session is archived or belongs to another profile or worker root",
        ));
    }
    Ok(())
}

fn encode_profile_execution_fence(
    fence: &ProfileExecutionFence,
) -> Result<VersionedRecord, StoreError> {
    validate_profile_execution_fence(fence)?;
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: fence.profile_id.as_entity_id().clone(),
        revision: fence.revision,
        updated_at: fence.updated_at,
        payload: serde_json::to_value(fence)
            .map_err(|_| StoreError::InvalidProfileExecution("fence canonical encoding failed"))?,
    })
}

fn decode_profile_execution_fence(
    record: &VersionedRecord,
) -> Result<ProfileExecutionFence, StoreError> {
    let fence: ProfileExecutionFence =
        serde_json::from_value(record.payload.clone()).map_err(|error| {
            corrupt(
                Collection::ProfileExecutionFences,
                record.id.as_str(),
                error.to_string(),
            )
        })?;
    if record.id != *fence.profile_id.as_entity_id()
        || record.revision != fence.revision
        || record.updated_at != fence.updated_at
    {
        return Err(corrupt(
            Collection::ProfileExecutionFences,
            record.id.as_str(),
            "fence envelope mismatch".into(),
        ));
    }
    validate_profile_execution_fence(&fence).map_err(|error| {
        corrupt(
            Collection::ProfileExecutionFences,
            record.id.as_str(),
            error.to_string(),
        )
    })?;
    Ok(fence)
}

fn validate_profile_execution_fence(fence: &ProfileExecutionFence) -> Result<(), StoreError> {
    if fence.version.major != CURRENT_SCHEMA_VERSION.major
        || fence.version.minor > CURRENT_SCHEMA_VERSION.minor
        || (fence.state == ProfileExecutionFenceState::Open
            && fence.cancellation_requested_at.is_some())
        || (fence.state != ProfileExecutionFenceState::Open
            && fence.cancellation_requested_at.is_none())
    {
        return Err(StoreError::InvalidProfileExecution(
            "fence schema or state is invalid",
        ));
    }
    Ok(())
}

fn encode_profile_execution_registration(
    registration: &ProfileExecutionRegistration,
) -> Result<VersionedRecord, StoreError> {
    validate_profile_execution_registration(registration)?;
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: registration.id.clone(),
        revision: registration.revision,
        updated_at: registration.updated_at,
        payload: serde_json::to_value(registration).map_err(|_| {
            StoreError::InvalidProfileExecution("registration canonical encoding failed")
        })?,
    })
}

fn decode_profile_execution_registration(
    record: &VersionedRecord,
) -> Result<ProfileExecutionRegistration, StoreError> {
    let registration: ProfileExecutionRegistration = serde_json::from_value(record.payload.clone())
        .map_err(|error| {
            corrupt(
                Collection::ProfileExecutionRegistrations,
                record.id.as_str(),
                error.to_string(),
            )
        })?;
    if record.id != registration.id
        || record.revision != registration.revision
        || record.updated_at != registration.updated_at
    {
        return Err(corrupt(
            Collection::ProfileExecutionRegistrations,
            record.id.as_str(),
            "registration envelope mismatch".into(),
        ));
    }
    validate_profile_execution_registration(&registration).map_err(|error| {
        corrupt(
            Collection::ProfileExecutionRegistrations,
            record.id.as_str(),
            error.to_string(),
        )
    })?;
    Ok(registration)
}

fn validate_profile_execution_registration(
    registration: &ProfileExecutionRegistration,
) -> Result<(), StoreError> {
    let terminal = registration.state != ProfileExecutionRegistrationState::Active;
    if registration.version.major != CURRENT_SCHEMA_VERSION.major
        || registration.version.minor > CURRENT_SCHEMA_VERSION.minor
        || registration.lease_expires_at <= registration.admitted_at
        || registration.updated_at < registration.admitted_at
        || terminal != registration.terminal_at.is_some()
        || registration
            .terminal_at
            .is_some_and(|terminal_at| terminal_at < registration.admitted_at)
    {
        return Err(StoreError::InvalidProfileExecution(
            "registration schema, lease, or terminal state is invalid",
        ));
    }
    Ok(())
}

fn required_profile_execution_fence(
    connection: &Connection,
    profile_id: &ProfileId,
) -> Result<ProfileExecutionFence, StoreError> {
    let record = read_record(
        connection,
        Collection::ProfileExecutionFences,
        profile_id.as_entity_id(),
    )?
    .ok_or_else(|| profile_execution_rejected(profile_id, "fence is missing"))?;
    decode_profile_execution_fence(&record)
}

fn profile_execution_registrations(
    connection: &Connection,
    profile_id: &ProfileId,
) -> Result<Vec<ProfileExecutionRegistration>, StoreError> {
    let mut registrations =
        list_records_in_connection(connection, Collection::ProfileExecutionRegistrations)?
            .into_iter()
            .map(|record| decode_profile_execution_registration(&record))
            .collect::<Result<Vec<_>, _>>()?;
    registrations.retain(|registration| &registration.profile_id == profile_id);
    registrations.sort_by(|left, right| left.id.cmp(&right.id));
    Ok(registrations)
}

fn active_profile_execution_registrations(
    connection: &Connection,
    profile_id: &ProfileId,
) -> Result<Vec<ProfileExecutionRegistration>, StoreError> {
    Ok(profile_execution_registrations(connection, profile_id)?
        .into_iter()
        .filter(|registration| registration.state == ProfileExecutionRegistrationState::Active)
        .collect())
}

fn profile_execution_snapshot_in_connection(
    connection: &Connection,
    profile_id: &ProfileId,
    status: ProfileExecutionMutationStatus,
) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
    let fence = required_profile_execution_fence(connection, profile_id)?;
    let active = active_profile_execution_registrations(connection, profile_id)?;
    if (fence.state == ProfileExecutionFenceState::Closed && !active.is_empty())
        || (fence.state == ProfileExecutionFenceState::Open
            && active
                .iter()
                .any(|registration| registration.fence_epoch != fence.epoch))
        || (fence.state == ProfileExecutionFenceState::Closing
            && active
                .iter()
                .any(|registration| registration.fence_epoch >= fence.epoch))
    {
        return Err(StoreError::InvalidProfileExecution(
            "fence and active registrations disagree",
        ));
    }
    Ok(ProfileExecutionFenceSnapshot {
        status,
        fence,
        active,
    })
}

fn profile_execution_snapshot_in_transaction(
    transaction: &Transaction<'_>,
    profile_id: &ProfileId,
    status: ProfileExecutionMutationStatus,
) -> Result<ProfileExecutionFenceSnapshot, StoreError> {
    profile_execution_snapshot_in_connection(transaction, profile_id, status)
}

fn profile_execution_permit(registration: &ProfileExecutionRegistration) -> ProfileExecutionPermit {
    ProfileExecutionPermit {
        registration_id: registration.id.clone(),
        profile_id: registration.profile_id.clone(),
        profile_revision: registration.profile_revision,
        session_id: registration.session_id.clone(),
        worker: registration.worker.clone(),
        owner_instance: registration.owner_instance.clone(),
        token: registration.token.clone(),
        fence_epoch: registration.fence_epoch,
        registration_revision: registration.revision,
        lease_expires_at: registration.lease_expires_at,
    }
}

fn registration_static_identity_matches(
    registration: &ProfileExecutionRegistration,
    permit: &ProfileExecutionPermit,
) -> bool {
    registration.id == permit.registration_id
        && registration.profile_id == permit.profile_id
        && registration.profile_revision == permit.profile_revision
        && registration.session_id == permit.session_id
        && registration.worker == permit.worker
        && registration.owner_instance == permit.owner_instance
        && registration.token == permit.token
        && registration.fence_epoch == permit.fence_epoch
}

fn registration_matches_permit(
    registration: &ProfileExecutionRegistration,
    permit: &ProfileExecutionPermit,
) -> bool {
    registration_static_identity_matches(registration, permit)
        && registration.revision == permit.registration_revision
        && registration.lease_expires_at == permit.lease_expires_at
}

fn registration_accepts_completion(
    registration: &ProfileExecutionRegistration,
    permit: &ProfileExecutionPermit,
) -> bool {
    if registration_matches_permit(registration, permit) {
        return true;
    }
    registration_static_identity_matches(registration, permit)
        && registration.lease_expires_at == permit.lease_expires_at
        && registration.cancellation_requested_at.is_some()
        && permit
            .registration_revision
            .checked_next()
            .is_some_and(|revision| revision == registration.revision)
}

fn registration_replays_request(
    registration: &ProfileExecutionRegistration,
    request: &ProfileExecutionAdmissionRequest,
    worker: &ProfileExecutionWorkerBinding,
) -> bool {
    registration.id == request.registration_id
        && registration.profile_id == request.profile_id
        && registration.profile_revision == request.expected_profile_revision
        && registration.session_id == request.session_id
        && &registration.worker == worker
        && registration.owner_instance == request.owner_instance
        && registration.token == request.token
        && registration.fence_epoch == request.expected_fence_epoch
        && registration.lease_expires_at == request.lease_expires_at
}

fn new_profile_execution_registration(
    transaction: &Transaction<'_>,
    request: &ProfileExecutionAdmissionRequest,
    worker: ProfileExecutionWorkerBinding,
    fence_epoch: u64,
    now: UtcTimestamp,
) -> Result<ProfileExecutionRegistration, StoreError> {
    let active = active_profile_execution_registrations(transaction, &request.profile_id)?;
    if active.len() >= 4_096 {
        return Err(StoreError::InvalidProfileExecution(
            "profile has too many active executions",
        ));
    }
    if active.iter().any(|registration| {
        registration.session_id == request.session_id || registration.token == request.token
    }) {
        return Err(profile_execution_rejected(
            &request.profile_id,
            "session or token already has an active registration",
        ));
    }
    Ok(ProfileExecutionRegistration {
        version: CURRENT_SCHEMA_VERSION,
        id: request.registration_id.clone(),
        profile_id: request.profile_id.clone(),
        profile_revision: request.expected_profile_revision,
        session_id: request.session_id.clone(),
        worker,
        owner_instance: request.owner_instance.clone(),
        token: request.token.clone(),
        fence_epoch,
        lease_expires_at: request.lease_expires_at,
        state: ProfileExecutionRegistrationState::Active,
        cancellation_requested_at: None,
        admitted_at: now,
        terminal_at: None,
        revision: Revision::ZERO,
        updated_at: now,
    })
}

fn current_profile_execution_worker_binding(
    connection: &Connection,
    root_tree_id: &RootTreeId,
    worker_id: &WorkerId,
    now: UtcTimestamp,
) -> Result<(ProfileExecutionWorkerBinding, UtcTimestamp), StoreError> {
    let generation_record = read_record(
        connection,
        Collection::WorkerGenerations,
        root_tree_id.as_entity_id(),
    )?
    .ok_or(StoreError::InvalidProfileExecution(
        "worker generation is missing",
    ))?;
    let generation: Generation = serde_json::from_value(generation_record.payload.clone())
        .map_err(|error| {
            corrupt(
                Collection::WorkerGenerations,
                generation_record.id.as_str(),
                error.to_string(),
            )
        })?;
    let lease_record = read_record(
        connection,
        Collection::WorkerLeases,
        root_tree_id.as_entity_id(),
    )?
    .ok_or(StoreError::InvalidProfileExecution(
        "worker lease is missing",
    ))?;
    let lease: StoredWorkerLease =
        serde_json::from_value(lease_record.payload.clone()).map_err(|error| {
            corrupt(
                Collection::WorkerLeases,
                lease_record.id.as_str(),
                error.to_string(),
            )
        })?;
    if lease.root_tree_id != *root_tree_id
        || lease.worker_id != *worker_id
        || lease.generation != generation
        || lease.expires_at <= now
    {
        return Err(StoreError::InvalidProfileExecution(
            "worker generation or lease ownership is stale",
        ));
    }
    Ok((
        ProfileExecutionWorkerBinding {
            root_tree_id: root_tree_id.clone(),
            worker_id: worker_id.clone(),
            generation,
            lease_authentication: lease.authentication,
        },
        lease.expires_at,
    ))
}

fn worker_binding_is_live(
    connection: &Connection,
    binding: &ProfileExecutionWorkerBinding,
    now: UtcTimestamp,
) -> Result<bool, StoreError> {
    let Some(generation_record) = read_record(
        connection,
        Collection::WorkerGenerations,
        binding.root_tree_id.as_entity_id(),
    )?
    else {
        return Ok(false);
    };
    let generation: Generation =
        serde_json::from_value(generation_record.payload).map_err(|error| {
            corrupt(
                Collection::WorkerGenerations,
                generation_record.id.as_str(),
                error.to_string(),
            )
        })?;
    let Some(lease_record) = read_record(
        connection,
        Collection::WorkerLeases,
        binding.root_tree_id.as_entity_id(),
    )?
    else {
        return Ok(false);
    };
    let lease: StoredWorkerLease =
        serde_json::from_value(lease_record.payload).map_err(|error| {
            corrupt(
                Collection::WorkerLeases,
                lease_record.id.as_str(),
                error.to_string(),
            )
        })?;
    Ok(generation == binding.generation
        && lease.root_tree_id == binding.root_tree_id
        && lease.worker_id == binding.worker_id
        && lease.generation == binding.generation
        && lease.authentication == binding.lease_authentication
        && lease.expires_at > now)
}

fn require_live_worker_binding(
    connection: &Connection,
    binding: &ProfileExecutionWorkerBinding,
    now: UtcTimestamp,
) -> Result<UtcTimestamp, StoreError> {
    if !worker_binding_is_live(connection, binding, now)? {
        return Err(StoreError::InvalidProfileExecution(
            "worker generation or lease ownership is stale",
        ));
    }
    let record = read_record(
        connection,
        Collection::WorkerLeases,
        binding.root_tree_id.as_entity_id(),
    )?
    .ok_or(StoreError::InvalidProfileExecution(
        "worker lease is missing",
    ))?;
    let lease: StoredWorkerLease = serde_json::from_value(record.payload).map_err(|error| {
        corrupt(
            Collection::WorkerLeases,
            record.id.as_str(),
            error.to_string(),
        )
    })?;
    Ok(lease.expires_at)
}

fn validate_active_profile_execution_permit(
    transaction: &Transaction<'_>,
    registration: &ProfileExecutionRegistration,
    permit: &ProfileExecutionPermit,
    now: UtcTimestamp,
) -> Result<(), StoreError> {
    if !registration_matches_permit(registration, permit)
        || registration.state != ProfileExecutionRegistrationState::Active
        || registration.cancellation_requested_at.is_some()
        || registration.lease_expires_at <= now
    {
        return Err(profile_execution_rejected(
            &permit.profile_id,
            "execution permit is stale, cancelled, or expired",
        ));
    }
    let fence = required_profile_execution_fence(transaction, &permit.profile_id)?;
    if fence.state != ProfileExecutionFenceState::Open || fence.epoch != permit.fence_epoch {
        return Err(profile_execution_rejected(
            &permit.profile_id,
            "execution fence epoch is stale",
        ));
    }
    require_enabled_profile(transaction, &permit.profile_id, permit.profile_revision)?;
    Ok(())
}

fn apply_profile_execution_mutation(
    transaction: &Transaction<'_>,
    mutation: &RecordMutation,
) -> Result<(), StoreError> {
    apply_mutation_inner(
        transaction,
        mutation,
        MutationAuthority::ProfileExecutionAdmission,
    )
}

fn seal_closing_fence_if_quiescent(
    transaction: &Transaction<'_>,
    profile_id: &ProfileId,
    now: UtcTimestamp,
) -> Result<bool, StoreError> {
    let mut fence = required_profile_execution_fence(transaction, profile_id)?;
    let active = active_profile_execution_registrations(transaction, profile_id)?;
    if fence.state == ProfileExecutionFenceState::Closed && !active.is_empty() {
        return Err(StoreError::InvalidProfileExecution(
            "closed fence retained active registrations",
        ));
    }
    if fence.state != ProfileExecutionFenceState::Closing || !active.is_empty() {
        return Ok(false);
    }
    let previous_revision = fence.revision;
    fence.state = ProfileExecutionFenceState::Closed;
    fence.revision = previous_revision
        .checked_next()
        .ok_or(StoreError::ProfileExecutionOverflow)?;
    fence.updated_at = now;
    apply_profile_execution_mutation(
        transaction,
        &RecordMutation::Put {
            collection: Collection::ProfileExecutionFences,
            record: encode_profile_execution_fence(&fence)?,
            precondition: WritePrecondition::Exact(previous_revision),
        },
    )?;
    Ok(true)
}

#[allow(clippy::many_single_char_names, clippy::too_many_lines)]
fn sha256(input: &[u8]) -> [u8; 32] {
    const K: [u32; 64] = [
        0x428a_2f98,
        0x7137_4491,
        0xb5c0_fbcf,
        0xe9b5_dba5,
        0x3956_c25b,
        0x59f1_11f1,
        0x923f_82a4,
        0xab1c_5ed5,
        0xd807_aa98,
        0x1283_5b01,
        0x2431_85be,
        0x550c_7dc3,
        0x72be_5d74,
        0x80de_b1fe,
        0x9bdc_06a7,
        0xc19b_f174,
        0xe49b_69c1,
        0xefbe_4786,
        0x0fc1_9dc6,
        0x240c_a1cc,
        0x2de9_2c6f,
        0x4a74_84aa,
        0x5cb0_a9dc,
        0x76f9_88da,
        0x983e_5152,
        0xa831_c66d,
        0xb003_27c8,
        0xbf59_7fc7,
        0xc6e0_0bf3,
        0xd5a7_9147,
        0x06ca_6351,
        0x1429_2967,
        0x27b7_0a85,
        0x2e1b_2138,
        0x4d2c_6dfc,
        0x5338_0d13,
        0x650a_7354,
        0x766a_0abb,
        0x81c2_c92e,
        0x9272_2c85,
        0xa2bf_e8a1,
        0xa81a_664b,
        0xc24b_8b70,
        0xc76c_51a3,
        0xd192_e819,
        0xd699_0624,
        0xf40e_3585,
        0x106a_a070,
        0x19a4_c116,
        0x1e37_6c08,
        0x2748_774c,
        0x34b0_bcb5,
        0x391c_0cb3,
        0x4ed8_aa4a,
        0x5b9c_ca4f,
        0x682e_6ff3,
        0x748f_82ee,
        0x78a5_636f,
        0x84c8_7814,
        0x8cc7_0208,
        0x90be_fffa,
        0xa450_6ceb,
        0xbef9_a3f7,
        0xc671_78f2,
    ];
    let mut data = input.to_vec();
    let bit_len = (data.len() as u64).wrapping_mul(8);
    data.push(0x80);
    while data.len() % 64 != 56 {
        data.push(0);
    }
    data.extend_from_slice(&bit_len.to_be_bytes());
    let mut h: [u32; 8] = [
        0x6a09_e667,
        0xbb67_ae85,
        0x3c6e_f372,
        0xa54f_f53a,
        0x510e_527f,
        0x9b05_688c,
        0x1f83_d9ab,
        0x5be0_cd19,
    ];
    for chunk in data.chunks_exact(64) {
        let mut w = [0_u32; 64];
        for (index, word) in chunk.chunks_exact(4).enumerate() {
            w[index] = u32::from_be_bytes(word.try_into().expect("four-byte word"));
        }
        for index in 16..64 {
            let s0 = w[index - 15].rotate_right(7)
                ^ w[index - 15].rotate_right(18)
                ^ (w[index - 15] >> 3);
            let s1 = w[index - 2].rotate_right(17)
                ^ w[index - 2].rotate_right(19)
                ^ (w[index - 2] >> 10);
            w[index] = w[index - 16]
                .wrapping_add(s0)
                .wrapping_add(w[index - 7])
                .wrapping_add(s1);
        }
        let [mut a, mut b, mut c, mut d, mut e, mut f, mut g, mut hh] = h;
        for index in 0..64 {
            let s1 = e.rotate_right(6) ^ e.rotate_right(11) ^ e.rotate_right(25);
            let ch = (e & f) ^ (!e & g);
            let t1 = hh
                .wrapping_add(s1)
                .wrapping_add(ch)
                .wrapping_add(K[index])
                .wrapping_add(w[index]);
            let s0 = a.rotate_right(2) ^ a.rotate_right(13) ^ a.rotate_right(22);
            let maj = (a & b) ^ (a & c) ^ (b & c);
            let t2 = s0.wrapping_add(maj);
            hh = g;
            g = f;
            f = e;
            e = d.wrapping_add(t1);
            d = c;
            c = b;
            b = a;
            a = t1.wrapping_add(t2);
        }
        for (value, addition) in h.iter_mut().zip([a, b, c, d, e, f, g, hh]) {
            *value = (*value).wrapping_add(addition);
        }
    }
    let mut output = [0_u8; 32];
    for (chunk, value) in output.chunks_exact_mut(4).zip(h) {
        chunk.copy_from_slice(&value.to_be_bytes());
    }
    output
}

fn apply_mutation(
    transaction: &Transaction<'_>,
    mutation: &RecordMutation,
) -> Result<(), StoreError> {
    apply_mutation_inner(transaction, mutation, MutationAuthority::Generic)
}

fn commit_authoritative_mutations(
    store: &EmbeddedStore,
    transaction: Transaction<'_>,
    mutations: Vec<RecordMutation>,
    _record: VersionedRecord,
) -> Result<AuthoritativeTransitionOutcome, StoreError> {
    validate_teammate_uniqueness(&transaction, &mutations)?;
    for mutation in &mutations {
        apply_mutation_inner(
            &transaction,
            mutation,
            MutationAuthority::AuthoritativeCollaboration,
        )?;
    }
    store.fail_if(FaultPoint::BeforeCommit)?;
    transaction.commit()?;
    if store.take_fault(FaultPoint::AfterCommit) {
        return Err(StoreError::UnknownOutcome);
    }
    Ok(AuthoritativeTransitionOutcome::Applied {
        receipt: CommitReceipt {
            applied_mutations: mutations.len(),
        },
    })
}

fn mutations_are_replayed(
    transaction: &Transaction<'_>,
    mutations: &[RecordMutation],
) -> Result<bool, StoreError> {
    for mutation in mutations {
        let replayed = match mutation {
            RecordMutation::Put {
                collection, record, ..
            } => read_record(transaction, *collection, &record.id)?.as_ref() == Some(record),
            RecordMutation::Delete { collection, id, .. } => {
                read_record(transaction, *collection, id)?.is_none()
            }
        };
        if !replayed {
            return Ok(false);
        }
    }
    Ok(true)
}

fn validate_group_membership_transition(
    transaction: &Transaction<'_>,
    transition: &GroupMembershipTransition,
) -> Result<(), StoreError> {
    validate_canonical_append(&transition.canonical_append)?;
    let append = &transition.canonical_append;
    let current = read_record(
        transaction,
        Collection::Conversations,
        &append.conversation_id,
    )?
    .ok_or(StoreError::InvalidAuthoritativeTransition(
        "membership conversation is missing",
    ))?;
    let next_participant_revision = transition
        .expected_participant_revision
        .checked_next()
        .ok_or(StoreError::InvalidAuthoritativeTransition(
            "participant revision overflow",
        ))?;
    if current.revision != append.expected_head_revision
        || current
            .payload
            .get("participant_revision")
            .and_then(serde_json::Value::as_u64)
            != Some(transition.expected_participant_revision.get())
        || append
            .head
            .payload
            .get("participant_revision")
            .and_then(serde_json::Value::as_u64)
            != Some(next_participant_revision.get())
        || transition.participant_mutations.is_empty()
        || transition.participant_mutations.len() > 4_096
    {
        return Err(StoreError::InvalidAuthoritativeTransition(
            "membership head or mutation batch is invalid",
        ));
    }
    for mutation in &transition.participant_mutations {
        let linked = match mutation {
            RecordMutation::Put {
                collection: Collection::ConversationParticipants,
                record,
                precondition: WritePrecondition::Missing | WritePrecondition::Exact(_),
            } => {
                record
                    .payload
                    .get("conversation_id")
                    .and_then(serde_json::Value::as_str)
                    == Some(append.conversation_id.as_str())
            }
            RecordMutation::Delete {
                collection: Collection::ConversationParticipants,
                id,
                precondition: WritePrecondition::Exact(_),
            } => read_record(transaction, Collection::ConversationParticipants, id)?.is_some_and(
                |record| {
                    record
                        .payload
                        .get("conversation_id")
                        .and_then(serde_json::Value::as_str)
                        == Some(append.conversation_id.as_str())
                },
            ),
            _ => false,
        };
        if !linked {
            return Err(StoreError::InvalidAuthoritativeTransition(
                "membership mutation is unscoped or non-CAS",
            ));
        }
    }
    Ok(())
}

fn validate_collaboration_round_transition(
    transaction: &Transaction<'_>,
    current: Option<&VersionedRecord>,
    transition: &CollaborationRoundTransition,
) -> Result<(), StoreError> {
    let next = &transition.round;
    let state = next
        .payload
        .get("state")
        .and_then(serde_json::Value::as_str)
        .ok_or(StoreError::InvalidAuthoritativeTransition(
            "round state is missing",
        ))?;
    if let Some(current) = current {
        const IMMUTABLE: [&str; 8] = [
            "stable_key",
            "conversation_id",
            "trigger_event_id",
            "eligible_participants",
            "mention_policy",
            "max_depth",
            "max_turns",
            "id",
        ];
        let previous_state = current
            .payload
            .get("state")
            .and_then(serde_json::Value::as_str)
            .unwrap_or("");
        let legal = match previous_state {
            "open" => matches!(
                state,
                "open" | "quiet" | "blocked" | "converged" | "budget_closed" | "cancelled"
            ),
            "quiet" => matches!(
                state,
                "quiet" | "open" | "blocked" | "converged" | "budget_closed" | "cancelled"
            ),
            "blocked" => matches!(
                state,
                "blocked" | "open" | "quiet" | "converged" | "budget_closed" | "cancelled"
            ),
            "converged" | "budget_closed" | "cancelled" => current == next,
            _ => false,
        };
        let budget_increased = ["remaining_depth", "remaining_turns"].iter().any(|field| {
            next.payload.get(*field).and_then(serde_json::Value::as_u64)
                > current
                    .payload
                    .get(*field)
                    .and_then(serde_json::Value::as_u64)
        });
        if transition.precondition != WritePrecondition::Exact(current.revision)
            || current.revision.checked_next() != Some(next.revision)
            || IMMUTABLE
                .iter()
                .any(|field| current.payload.get(*field) != next.payload.get(*field))
            || budget_increased
            || !legal
        {
            return Err(StoreError::InvalidAuthoritativeTransition(
                "round revision, scope, budget, or state transition is invalid",
            ));
        }
    } else if transition.precondition != WritePrecondition::Missing
        || next.revision != Revision::ZERO
    {
        return Err(StoreError::InvalidAuthoritativeTransition(
            "new round must use revision zero and Missing",
        ));
    }
    for delivery_id in next
        .payload
        .get("active_deliveries")
        .and_then(serde_json::Value::as_array)
        .into_iter()
        .flatten()
    {
        let id = delivery_id
            .as_str()
            .and_then(|id| EntityId::parse(id).ok())
            .ok_or(StoreError::InvalidAuthoritativeTransition(
                "active delivery ID is invalid",
            ))?;
        let delivery = read_record(transaction, Collection::ConversationDeliveries, &id)?.ok_or(
            StoreError::InvalidAuthoritativeTransition("active delivery is missing"),
        )?;
        if delivery.payload.get("conversation_id") != next.payload.get("conversation_id") {
            return Err(StoreError::InvalidAuthoritativeTransition(
                "active delivery belongs to another conversation",
            ));
        }
    }
    if transition.delivery_mutations.iter().any(|mutation| {
        !matches!(
            mutation,
            RecordMutation::Put {
                collection: Collection::ConversationDeliveries,
                precondition: WritePrecondition::Missing | WritePrecondition::Exact(_),
                ..
            } | RecordMutation::Delete {
                collection: Collection::ConversationDeliveries,
                precondition: WritePrecondition::Exact(_),
                ..
            }
        )
    }) || transition
        .supersessions
        .iter()
        .any(|record| record.revision != Revision::ZERO)
    {
        return Err(StoreError::InvalidAuthoritativeTransition(
            "round delivery or supersession mutation is invalid",
        ));
    }
    Ok(())
}

fn validate_assignment_transition(
    transaction: &Transaction<'_>,
    current: Option<&VersionedRecord>,
    next: &VersionedRecord,
) -> Result<(), StoreError> {
    if let Some(current) = current {
        const IMMUTABLE: [&str; 6] = [
            "stable_key",
            "conversation_id",
            "objective",
            "creator_profile_id",
            "dependencies",
            "source_event_id",
        ];
        let history = next
            .payload
            .get("ownership_history")
            .and_then(serde_json::Value::as_array);
        let previous_history = current
            .payload
            .get("ownership_history")
            .and_then(serde_json::Value::as_array);
        if current.revision.checked_next() != Some(next.revision)
            || IMMUTABLE
                .iter()
                .any(|field| current.payload.get(*field) != next.payload.get(*field))
            || previous_history
                .zip(history)
                .is_none_or(|(previous, history)| {
                    history.len() < previous.len() || !history.starts_with(previous)
                })
        {
            return Err(StoreError::InvalidAuthoritativeTransition(
                "assignment identity, revision, or ownership history is invalid",
            ));
        }
    } else if next.revision != Revision::ZERO {
        return Err(StoreError::InvalidAuthoritativeTransition(
            "new assignment must use revision zero",
        ));
    }
    if let Some(claim) = next
        .payload
        .get("claim")
        .and_then(serde_json::Value::as_object)
        && claim.get("fence").and_then(serde_json::Value::as_u64) != Some(next.revision.get())
    {
        return Err(StoreError::InvalidAuthoritativeTransition(
            "assignment claim fence must equal its durable revision",
        ));
    }
    if next
        .payload
        .get("claim")
        .is_some_and(|claim| !claim.is_null())
    {
        for dependency in next
            .payload
            .get("dependencies")
            .and_then(serde_json::Value::as_array)
            .into_iter()
            .flatten()
        {
            let id = dependency
                .as_str()
                .and_then(|id| EntityId::parse(id).ok())
                .ok_or(StoreError::InvalidAuthoritativeTransition(
                    "assignment dependency ID is invalid",
                ))?;
            let ready = read_record(transaction, Collection::Assignments, &id)?
                .and_then(|record| record.payload.get("state").cloned())
                .and_then(|state| state.as_str().map(str::to_owned))
                .is_some_and(|state| state == "completed");
            if !ready {
                return Err(StoreError::InvalidAuthoritativeTransition(
                    "assignment dependency is not completed",
                ));
            }
        }
    }
    Ok(())
}

fn validate_assignment_handoff(
    transaction: &Transaction<'_>,
    current: Option<&VersionedRecord>,
    handoff: &AssignmentHandoffTransaction,
) -> Result<(), StoreError> {
    let current = current.ok_or(StoreError::InvalidAuthoritativeTransition(
        "handoff assignment is missing",
    ))?;
    if current.revision != handoff.expected_assignment_revision
        || handoff.assignment.revision
            != current
                .revision
                .checked_next()
                .ok_or(StoreError::InvalidAuthoritativeTransition(
                    "assignment revision overflow",
                ))?
        || current.payload.get("owner_profile_id")
            == handoff.assignment.payload.get("owner_profile_id")
        || handoff.handoff_audit.revision != Revision::ZERO
        || handoff.new_owner_delivery.revision != Revision::ZERO
        || handoff.obsolete_delivery_mutations.iter().any(|mutation| {
            !matches!(
                mutation,
                RecordMutation::Put {
                    collection: Collection::ConversationDeliveries,
                    precondition: WritePrecondition::Exact(_),
                    ..
                }
            )
        })
    {
        return Err(StoreError::InvalidAuthoritativeTransition(
            "assignment handoff CAS, ownership, delivery, or audit is invalid",
        ));
    }
    validate_assignment_transition(transaction, Some(current), &handoff.assignment)
}

fn validate_direct_conversation_binding(
    binding: &CanonicalDirectConversationBinding,
) -> Result<(), StoreError> {
    let stable_key = binding
        .key
        .payload
        .get("stable_key")
        .and_then(serde_json::Value::as_str);
    let human_profile = binding
        .key
        .payload
        .get("profile_id")
        .and_then(serde_json::Value::as_str);
    let first_profile = binding
        .key
        .payload
        .get("first_profile_id")
        .and_then(serde_json::Value::as_str);
    let second_profile = binding
        .key
        .payload
        .get("second_profile_id")
        .and_then(serde_json::Value::as_str);
    let typed_key_is_valid = stable_key.is_some_and(|key| StableKey::parse(key).is_ok())
        && match (human_profile, first_profile, second_profile) {
            (Some(profile_id), None, None) => profile_id.parse::<ProfileId>().is_ok(),
            (None, Some(first), Some(second)) => {
                first.parse::<ProfileId>().is_ok()
                    && second.parse::<ProfileId>().is_ok()
                    && first != second
            }
            _ => false,
        };
    let legacy_human_key_is_valid = binding
        .key
        .payload
        .get("human_dm_profile_id")
        .and_then(serde_json::Value::as_str)
        .is_some_and(|profile_id| profile_id.parse::<ProfileId>().is_ok());
    let legacy_pair_key_is_valid = binding
        .key
        .payload
        .get("agent_pair_key")
        .and_then(serde_json::Value::as_str)
        .is_some_and(|pair| StableKey::parse(pair).is_ok());
    let key_is_valid = typed_key_is_valid
        || (stable_key.is_none()
            && human_profile.is_none()
            && first_profile.is_none()
            && second_profile.is_none()
            && (legacy_human_key_is_valid ^ legacy_pair_key_is_valid));
    if !key_is_valid
        || binding.key.revision != Revision::ZERO
        || binding.participants.len() != 2
        || binding
            .participants
            .iter()
            .any(|participant| participant.revision != Revision::ZERO)
        || binding.participants[0].id == binding.participants[1].id
        || binding
            .key
            .payload
            .get("conversation_id")
            .and_then(serde_json::Value::as_str)
            != Some(binding.conversation.id.as_str())
        || binding.participants.iter().any(|participant| {
            participant
                .payload
                .get("conversation_id")
                .and_then(serde_json::Value::as_str)
                != Some(binding.conversation.id.as_str())
        })
    {
        return Err(StoreError::InvalidDirectConversation(
            "key, conversation, or participant identity is invalid",
        ));
    }
    Ok(())
}

fn validate_migration_direct_conversation_bindings(
    mutations: &[RecordMutation],
) -> Result<(), StoreError> {
    for key in mutations.iter().filter_map(|mutation| match mutation {
        RecordMutation::Put {
            collection: Collection::DirectMessageKeys,
            record,
            precondition: WritePrecondition::Missing,
        } => Some(record),
        _ => None,
    }) {
        let conversation_id = key
            .payload
            .get("conversation_id")
            .and_then(serde_json::Value::as_str)
            .ok_or(StoreError::InvalidDirectConversation(
                "migration direct-message key has no conversation identity",
            ))?;
        let conversation = mutations
            .iter()
            .find_map(|mutation| match mutation {
                RecordMutation::Put {
                    collection: Collection::Conversations,
                    record,
                    precondition: WritePrecondition::Missing,
                } if record.id.as_str() == conversation_id => Some(record.clone()),
                _ => None,
            })
            .ok_or(StoreError::InvalidDirectConversation(
                "migration direct-message conversation is missing",
            ))?;
        let participants = mutations
            .iter()
            .filter_map(|mutation| match mutation {
                RecordMutation::Put {
                    collection: Collection::ConversationParticipants,
                    record,
                    precondition: WritePrecondition::Missing,
                } if record
                    .payload
                    .get("conversation_id")
                    .and_then(serde_json::Value::as_str)
                    == Some(conversation_id) =>
                {
                    Some(record.clone())
                }
                _ => None,
            })
            .collect();
        validate_direct_conversation_binding(&CanonicalDirectConversationBinding {
            key: key.clone(),
            conversation,
            participants,
        })?;
    }
    Ok(())
}

fn direct_conversation_replay_is_complete(
    transaction: &Transaction<'_>,
    binding: &CanonicalDirectConversationBinding,
) -> Result<bool, StoreError> {
    if read_record(
        transaction,
        Collection::Conversations,
        &binding.conversation.id,
    )?
    .as_ref()
        != Some(&binding.conversation)
    {
        return Ok(false);
    }
    let mut stored =
        list_records_in_transaction(transaction, Collection::ConversationParticipants)?
            .into_iter()
            .filter(|record| {
                record
                    .payload
                    .get("conversation_id")
                    .and_then(serde_json::Value::as_str)
                    == Some(binding.conversation.id.as_str())
            })
            .collect::<Vec<_>>();
    let mut expected = binding.participants.clone();
    stored.sort_by(|left, right| left.id.cmp(&right.id));
    expected.sort_by(|left, right| left.id.cmp(&right.id));
    Ok(stored == expected)
}

fn validate_conversation_delivery_finalization(
    finalization: &ConversationDeliveryFinalization,
) -> Result<(), StoreError> {
    let next_revision = finalization
        .expected_delivery_revision
        .checked_next()
        .ok_or(StoreError::InvalidDeliveryFinalization(
            "delivery revision overflow",
        ))?;
    if finalization.delivery.revision != next_revision
        || finalization.finalization_intent.revision != Revision::ZERO
        || finalization
            .finalization_intent
            .payload
            .get("delivery_id")
            .and_then(serde_json::Value::as_str)
            != Some(finalization.delivery.id.as_str())
    {
        return Err(StoreError::InvalidDeliveryFinalization(
            "delivery and finalization intent linkage is invalid",
        ));
    }
    if let Some(outbox) = &finalization.publication_outbox {
        let publication_key = outbox
            .payload
            .get("publication_key")
            .and_then(serde_json::Value::as_str);
        let digest = outbox
            .payload
            .get("payload_digest")
            .and_then(serde_json::Value::as_str);
        if outbox.revision != Revision::ZERO
            || outbox
                .payload
                .get("delivery_id")
                .and_then(serde_json::Value::as_str)
                != Some(finalization.delivery.id.as_str())
            || publication_key.is_none_or(|key| StableKey::parse(key).is_err())
            || digest.is_none_or(|value| {
                value.len() != 64 || !value.bytes().all(|byte| byte.is_ascii_hexdigit())
            })
            || outbox.payload.get("source_event_id").is_none()
            || outbox.payload.get("source_profile_id").is_none()
            || outbox.payload.get("participant_session_revision").is_none()
            || outbox.payload.get("state").is_none()
        {
            return Err(StoreError::InvalidDeliveryFinalization(
                "publication outbox identity or digest is invalid",
            ));
        }
    }
    if let Some(supersession) = &finalization.supersession {
        let target_kind = supersession
            .payload
            .get("target_kind")
            .and_then(serde_json::Value::as_str);
        let target_id = supersession
            .payload
            .get("target_id")
            .and_then(serde_json::Value::as_str);
        let source_event_id = supersession
            .payload
            .get("source_event_id")
            .and_then(serde_json::Value::as_str);
        if supersession.revision != Revision::ZERO
            || target_kind.is_none_or(|kind| kind.is_empty() || kind.len() > 64)
            || target_id.is_none_or(|id| EntityId::parse(id).is_err())
            || source_event_id.is_none_or(|id| EntityId::parse(id).is_err())
            || supersession
                .payload
                .get("context_revision")
                .and_then(serde_json::Value::as_u64)
                .is_none()
        {
            return Err(StoreError::InvalidDeliveryFinalization(
                "targeted supersession identity is invalid",
            ));
        }
    }
    if finalization
        .delivery
        .payload
        .get("state")
        .and_then(serde_json::Value::as_str)
        == Some("dead_letter")
        && finalization
            .delivery
            .payload
            .get("safe_error")
            .and_then(serde_json::Value::as_str)
            .is_none_or(|reason| reason.is_empty() || reason.len() > 1_024)
    {
        return Err(StoreError::InvalidDeliveryFinalization(
            "dead-letter safe error is missing or unbounded",
        ));
    }
    Ok(())
}

fn delivery_finalization_is_replay(
    transaction: &Transaction<'_>,
    finalization: &ConversationDeliveryFinalization,
) -> Result<bool, StoreError> {
    if read_record(
        transaction,
        Collection::ConversationDeliveries,
        &finalization.delivery.id,
    )?
    .as_ref()
        != Some(&finalization.delivery)
    {
        return Ok(false);
    }
    if read_record(
        transaction,
        Collection::ConversationFinalizationIntents,
        &finalization.finalization_intent.id,
    )?
    .as_ref()
        != Some(&finalization.finalization_intent)
    {
        return Ok(false);
    }
    for (collection, expected) in [
        (
            Collection::ConversationPublicationOutbox,
            finalization.publication_outbox.as_ref(),
        ),
        (
            Collection::ConversationSupersessions,
            finalization.supersession.as_ref(),
        ),
    ] {
        if let Some(expected) = expected
            && read_record(transaction, collection, &expected.id)?.as_ref() != Some(expected)
        {
            return Ok(false);
        }
    }
    Ok(true)
}

fn validate_canonical_append(append: &CanonicalConversationAppend) -> Result<(), StoreError> {
    let intent_collections: BTreeSet<_> = append
        .intents
        .iter()
        .map(|mutation| match mutation {
            RecordMutation::Put { collection, .. } | RecordMutation::Delete { collection, .. } => {
                *collection
            }
        })
        .collect();
    let required_intents = [
        Collection::ConversationProjectionIntents,
        Collection::ConversationUnreadIntents,
        Collection::ConversationSearchIntents,
        Collection::ConversationPublicationIntents,
    ];
    if append.expected_next_sequence == 0
        || append.conversation_id != append.head.id
        || append.head.revision
            != append
                .expected_head_revision
                .checked_next()
                .ok_or(StoreError::InvalidCanonicalAppend("head revision overflow"))?
        || append.event.revision != Revision::ZERO
        || append.stable_key.revision != Revision::ZERO
        || append.intents.is_empty()
        || append.intents.len() > 4096
        || required_intents
            .iter()
            .any(|collection| !intent_collections.contains(collection))
        || append
            .intents
            .iter()
            .any(|mutation| !is_canonical_append_mutation(append, mutation))
    {
        return Err(StoreError::InvalidCanonicalAppend(
            "identity, revision, sequence, or intent invariant failed",
        ));
    }
    let event_head = append
        .head
        .payload
        .get("event_head")
        .and_then(serde_json::Value::as_object)
        .ok_or(StoreError::InvalidCanonicalAppend(
            "head payload lacks event_head",
        ))?;
    if event_head
        .get("sequence")
        .and_then(serde_json::Value::as_u64)
        != Some(append.expected_next_sequence)
        || event_head
            .get("event_id")
            .and_then(serde_json::Value::as_str)
            != Some(append.event.id.as_str())
    {
        return Err(StoreError::InvalidCanonicalAppend(
            "head does not identify the appended event",
        ));
    }
    let digest = record_digest(&append.event)?;
    if append
        .stable_key
        .payload
        .get("event_id")
        .and_then(serde_json::Value::as_str)
        != Some(append.event.id.as_str())
        || append
            .stable_key
            .payload
            .get("event_digest")
            .and_then(serde_json::Value::as_str)
            != Some(digest.as_str())
    {
        return Err(StoreError::InvalidCanonicalAppend(
            "stable key does not authenticate the event",
        ));
    }
    Ok(())
}

fn is_canonical_append_mutation(
    append: &CanonicalConversationAppend,
    mutation: &RecordMutation,
) -> bool {
    if is_projection_mutation(mutation) {
        return true;
    }
    matches!(
        mutation,
        RecordMutation::Put {
            collection: Collection::ConversationDeliveries,
            record,
            precondition: WritePrecondition::Missing,
        } if record.revision == Revision::ZERO
            && record.payload.get("conversation_id").and_then(serde_json::Value::as_str)
                == Some(append.conversation_id.as_str())
            && record.payload.get("source_event_id").and_then(serde_json::Value::as_str)
                == Some(append.event.id.as_str())
            && record.payload.get("state").and_then(serde_json::Value::as_str) == Some("pending")
    )
}

fn is_projection_mutation(mutation: &RecordMutation) -> bool {
    let collection = match mutation {
        RecordMutation::Put { collection, .. } | RecordMutation::Delete { collection, .. } => {
            *collection
        }
    };
    matches!(
        collection,
        Collection::ConversationParticipants
            | Collection::ReadReceipts
            | Collection::ConversationProjectionIntents
            | Collection::ConversationUnreadIntents
            | Collection::ConversationSearchIntents
            | Collection::ConversationPublicationIntents
    ) && !matches!(
        mutation,
        RecordMutation::Put {
            precondition: WritePrecondition::Any,
            ..
        } | RecordMutation::Delete {
            precondition: WritePrecondition::Any,
            ..
        }
    )
}

fn record_digest(record: &VersionedRecord) -> Result<String, StoreError> {
    let bytes = keith_agent_types::canonical_json_bytes(record).map_err(|error| {
        corrupt(
            Collection::ConversationEvents,
            record.id.as_str(),
            error.to_string(),
        )
    })?;
    let mut output = String::with_capacity(64);
    for byte in sha256(&bytes) {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("writing to String cannot fail");
    }
    Ok(output)
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum MutationAuthority {
    Generic,
    TeammateMigration,
    EvolutionAppend,
    ProfileExecutionAdmission,
    DirectConversationBinding,
    ConversationDeliveryFinalization,
    AuthoritativeCollaboration,
}

fn validate_mutation_authority(
    mutation: &RecordMutation,
    authority: MutationAuthority,
) -> Result<(), StoreError> {
    #[allow(clippy::match_same_arms)]
    match mutation {
        RecordMutation::Put {
            collection: Collection::EvolutionLedger,
            precondition: WritePrecondition::Missing,
            ..
        } if authority == MutationAuthority::EvolutionAppend => {}
        RecordMutation::Put {
            collection: Collection::EvolutionLedgerHead,
            precondition: WritePrecondition::Missing | WritePrecondition::Exact(_),
            ..
        } if authority == MutationAuthority::EvolutionAppend => {}
        RecordMutation::Put {
            collection: Collection::EvolutionLedger | Collection::EvolutionLedgerHead,
            ..
        }
        | RecordMutation::Delete {
            collection: Collection::EvolutionLedger | Collection::EvolutionLedgerHead,
            ..
        } => return Err(StoreError::AppendOnlyViolation(Collection::EvolutionLedger)),
        RecordMutation::Put {
            collection:
                Collection::ProfileExecutionFences | Collection::ProfileExecutionRegistrations,
            ..
        }
        | RecordMutation::Delete {
            collection:
                Collection::ProfileExecutionFences | Collection::ProfileExecutionRegistrations,
            ..
        } if authority != MutationAuthority::ProfileExecutionAdmission => {
            let collection = match mutation {
                RecordMutation::Put { collection, .. }
                | RecordMutation::Delete { collection, .. } => *collection,
            };
            return Err(StoreError::ProfileExecutionProtected(collection));
        }
        RecordMutation::Put {
            collection: Collection::DirectMessageKeys,
            precondition: WritePrecondition::Missing,
            ..
        } if matches!(
            authority,
            MutationAuthority::DirectConversationBinding | MutationAuthority::TeammateMigration
        ) => {}
        RecordMutation::Put {
            collection: Collection::DirectMessageKeys,
            ..
        }
        | RecordMutation::Delete {
            collection: Collection::DirectMessageKeys,
            ..
        } => {
            return Err(StoreError::AppendOnlyViolation(
                Collection::DirectMessageKeys,
            ));
        }
        RecordMutation::Put {
            collection: Collection::ConversationSupersessions,
            precondition: WritePrecondition::Missing,
            ..
        } if authority == MutationAuthority::AuthoritativeCollaboration => {}
        RecordMutation::Put {
            collection:
                Collection::ConversationFinalizationIntents | Collection::ConversationSupersessions,
            precondition: WritePrecondition::Missing,
            ..
        } if authority == MutationAuthority::ConversationDeliveryFinalization => {}
        RecordMutation::Put {
            collection:
                Collection::ConversationFinalizationIntents | Collection::ConversationSupersessions,
            ..
        }
        | RecordMutation::Delete {
            collection:
                Collection::ConversationFinalizationIntents | Collection::ConversationSupersessions,
            ..
        } => {
            let collection = match mutation {
                RecordMutation::Put { collection, .. }
                | RecordMutation::Delete { collection, .. } => *collection,
            };
            return Err(StoreError::AppendOnlyViolation(collection));
        }
        RecordMutation::Put {
            collection: Collection::ConversationPublicationOutbox,
            precondition: WritePrecondition::Missing,
            ..
        } if authority == MutationAuthority::ConversationDeliveryFinalization => {}
        RecordMutation::Put {
            collection: Collection::ConversationPublicationOutbox,
            precondition: WritePrecondition::Exact(_),
            ..
        } => {}
        RecordMutation::Put {
            collection: Collection::ConversationPublicationOutbox,
            ..
        }
        | RecordMutation::Delete {
            collection: Collection::ConversationPublicationOutbox,
            ..
        } => {
            return Err(StoreError::AppendOnlyViolation(
                Collection::ConversationPublicationOutbox,
            ));
        }
        RecordMutation::Put {
            collection: Collection::ConversationEvents | Collection::ConversationStableKeys,
            precondition: WritePrecondition::Missing,
            ..
        } => {}
        RecordMutation::Put {
            collection: Collection::ConversationEvents | Collection::ConversationStableKeys,
            ..
        }
        | RecordMutation::Delete {
            collection: Collection::ConversationEvents | Collection::ConversationStableKeys,
            ..
        } => {
            return Err(StoreError::AppendOnlyViolation(
                Collection::ConversationEvents,
            ));
        }
        _ => {}
    }
    Ok(())
}

fn apply_mutation_inner(
    transaction: &Transaction<'_>,
    mutation: &RecordMutation,
    authority: MutationAuthority,
) -> Result<(), StoreError> {
    validate_mutation_authority(mutation, authority)?;
    match mutation {
        RecordMutation::Put {
            collection,
            record,
            precondition,
        } => {
            validate_record(*collection, record)?;
            validate_collection_transition(transaction, *collection, record)?;
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

fn validate_collection_transition(
    transaction: &Transaction<'_>,
    collection: Collection,
    record: &VersionedRecord,
) -> Result<(), StoreError> {
    let Some(current) = read_record(transaction, collection, &record.id)? else {
        return Ok(());
    };
    if collection == Collection::ConversationDeliveries {
        const IMMUTABLE: [&str; 7] = [
            "stable_source_key",
            "conversation_id",
            "source_event_id",
            "source_profile_id",
            "destination_profile_id",
            "participant_session_id",
            "policy_snapshot_key",
        ];
        let identity_changed = IMMUTABLE
            .iter()
            .any(|field| current.payload.get(*field) != record.payload.get(*field));
        let counter_regressed = ["attempt_count", "last_claim_fence"].iter().any(|field| {
            let previous = current
                .payload
                .get(*field)
                .and_then(serde_json::Value::as_u64)
                .unwrap_or(0);
            record
                .payload
                .get(*field)
                .and_then(serde_json::Value::as_u64)
                .is_none_or(|next| next < previous)
        });
        let claim_invalid = match (
            current
                .payload
                .get("claim")
                .and_then(serde_json::Value::as_object),
            record
                .payload
                .get("claim")
                .and_then(serde_json::Value::as_object),
        ) {
            (Some(previous), Some(next)) => {
                let previous_fence = previous.get("fence").and_then(serde_json::Value::as_u64);
                let next_fence = next.get("fence").and_then(serde_json::Value::as_u64);
                previous_fence.is_none()
                    || next_fence.is_none()
                    || next_fence < previous_fence
                    || (next_fence == previous_fence
                        && ["token", "owner_profile_id", "attempt"]
                            .iter()
                            .any(|field| previous.get(*field) != next.get(*field)))
            }
            (None, Some(next)) => {
                let previous_fence = current
                    .payload
                    .get("last_claim_fence")
                    .and_then(serde_json::Value::as_u64)
                    .unwrap_or(0);
                next.get("fence")
                    .and_then(serde_json::Value::as_u64)
                    .is_none_or(|fence| fence <= previous_fence)
            }
            _ => false,
        };
        if identity_changed
            || counter_regressed
            || claim_invalid
            || current.revision.checked_next() != Some(record.revision)
        {
            return Err(StoreError::InvalidDeliveryFinalization(
                "delivery identity, claim fence, attempt, or revision transition is invalid",
            ));
        }
    }
    if collection == Collection::ConversationPublicationOutbox {
        const IMMUTABLE: [&str; 6] = [
            "publication_key",
            "delivery_id",
            "source_event_id",
            "source_profile_id",
            "participant_session_revision",
            "payload_digest",
        ];
        if IMMUTABLE
            .iter()
            .any(|field| current.payload.get(*field) != record.payload.get(*field))
            || current.revision.checked_next() != Some(record.revision)
        {
            return Err(StoreError::InvalidDeliveryFinalization(
                "publication outbox identity or revision transition is invalid",
            ));
        }
    }
    if collection == Collection::PendingActions {
        let previous = current.payload.pointer("/action/source");
        let next = record.payload.pointer("/action/source");
        if previous
            .and_then(|source| source.get("source"))
            .and_then(serde_json::Value::as_str)
            == Some("peer_message")
            && previous != next
        {
            return Err(StoreError::InvalidDeliveryFinalization(
                "peer-message receipt source authority is immutable",
            ));
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
implement_repository!(
    SharedKnowledgeSpaceRepository,
    Collection::SharedKnowledgeSpaces,
    get_shared_knowledge_space,
    list_shared_knowledge_spaces,
    put_shared_knowledge_space,
    delete_shared_knowledge_space
);

#[cfg(test)]
mod tests {
    use std::sync::{Arc, Barrier};
    use std::thread;

    use keith_agent_types::{SessionId, StableKey, WorkspaceId};
    use keith_session_store::{SessionKind, SessionManifest};
    use keith_state_store_core::{
        ActionRepository, LeaseRepository, SharedKnowledgeSpaceRepository,
    };
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

    #[derive(Clone)]
    struct ProfileExecutionFixture {
        profile_id: ProfileId,
        profile_revision: Revision,
        worker: ProfileExecutionWorkerBinding,
    }

    fn seed_profile_execution_fixture(store: &EmbeddedStore) -> ProfileExecutionFixture {
        let profile_id = ProfileId::new();
        let profile_revision = Revision::ZERO;
        let root_tree_id = RootTreeId::new();
        let worker_id = WorkerId::new();
        let generation = Generation::new(7);
        let authentication = EntityId::new();
        store
            .transact(&[
                RecordMutation::Put {
                    collection: Collection::Profiles,
                    record: VersionedRecord {
                        version: CURRENT_SCHEMA_VERSION,
                        id: profile_id.as_entity_id().clone(),
                        revision: profile_revision,
                        updated_at: UtcTimestamp::from_unix_millis(1),
                        payload: json!({
                            "enabled": true,
                            "teammate": {
                                "presentation": {"lifecycle": "enabled"},
                                "deletion": null
                            }
                        }),
                    },
                    precondition: WritePrecondition::Missing,
                },
                RecordMutation::Put {
                    collection: Collection::WorkerGenerations,
                    record: VersionedRecord {
                        version: CURRENT_SCHEMA_VERSION,
                        id: root_tree_id.as_entity_id().clone(),
                        revision: Revision::ZERO,
                        updated_at: UtcTimestamp::from_unix_millis(1),
                        payload: serde_json::to_value(generation).unwrap(),
                    },
                    precondition: WritePrecondition::Missing,
                },
                RecordMutation::Put {
                    collection: Collection::WorkerLeases,
                    record: VersionedRecord {
                        version: CURRENT_SCHEMA_VERSION,
                        id: root_tree_id.as_entity_id().clone(),
                        revision: Revision::ZERO,
                        updated_at: UtcTimestamp::from_unix_millis(1),
                        payload: json!({
                            "root_tree_id": root_tree_id,
                            "worker_id": worker_id,
                            "generation": generation,
                            "authentication": authentication,
                            "expires_at": UtcTimestamp::from_unix_millis(1_000),
                        }),
                    },
                    precondition: WritePrecondition::Missing,
                },
            ])
            .unwrap();
        ProfileExecutionFixture {
            profile_id,
            profile_revision,
            worker: ProfileExecutionWorkerBinding {
                root_tree_id,
                worker_id,
                generation,
                lease_authentication: authentication,
            },
        }
    }

    fn initialize_profile_execution(
        store: &EmbeddedStore,
        fixture: &ProfileExecutionFixture,
    ) -> ProfileExecutionFenceSnapshot {
        store
            .initialize_profile_execution_fence(
                &fixture.profile_id,
                fixture.profile_revision,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap()
    }

    fn profile_execution_request(
        store: &EmbeddedStore,
        fixture: &ProfileExecutionFixture,
        fence: &ProfileExecutionFence,
    ) -> ProfileExecutionAdmissionRequest {
        profile_execution_request_with_catalog_binding(
            store,
            fixture,
            fence,
            &fixture.profile_id,
            &fixture.worker.root_tree_id,
        )
    }

    fn profile_execution_request_with_catalog_binding(
        store: &EmbeddedStore,
        fixture: &ProfileExecutionFixture,
        fence: &ProfileExecutionFence,
        catalog_profile_id: &ProfileId,
        catalog_root_tree_id: &RootTreeId,
    ) -> ProfileExecutionAdmissionRequest {
        let request = ProfileExecutionAdmissionRequest {
            registration_id: EntityId::new(),
            profile_id: fixture.profile_id.clone(),
            expected_profile_revision: fixture.profile_revision,
            expected_fence_epoch: fence.epoch,
            expected_fence_revision: fence.revision,
            session_id: SessionId::new(),
            root_tree_id: fixture.worker.root_tree_id.clone(),
            worker_id: fixture.worker.worker_id.clone(),
            worker_binding: fixture.worker.clone(),
            worker_lease_expires_at: UtcTimestamp::from_unix_millis(1_000),
            owner_instance: EntityId::new(),
            token: StableKey::parse(format!("execution:{}", EntityId::new())).unwrap(),
            lease_expires_at: UtcTimestamp::from_unix_millis(500),
        };
        seed_session_catalog(
            store,
            &request.session_id,
            catalog_profile_id,
            catalog_root_tree_id,
        );
        request
    }

    fn seed_session_catalog(
        store: &EmbeddedStore,
        session_id: &SessionId,
        profile_id: &ProfileId,
        root_tree_id: &RootTreeId,
    ) {
        let manifest = SessionManifest {
            version: CURRENT_SCHEMA_VERSION,
            kind: SessionKind::Root,
            session_id: session_id.clone(),
            root_tree_id: root_tree_id.clone(),
            parent_session_id: None,
            profile_id: profile_id.clone(),
            workspace_id: WorkspaceId::new(),
            created_at: UtcTimestamp::from_unix_millis(3),
            active_leaf: None,
            compaction_generation: 0,
            label: None,
            profile_snapshot: None,
            branch_labels: BTreeMap::new(),
            archived: false,
        };
        store
            .transact(&[RecordMutation::Put {
                collection: Collection::SessionCatalog,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: session_id.as_entity_id().clone(),
                    revision: Revision::ZERO,
                    updated_at: UtcTimestamp::from_unix_millis(3),
                    payload: serde_json::to_value(manifest).unwrap(),
                },
                precondition: WritePrecondition::Missing,
            }])
            .unwrap();
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

    fn canonical_append(
        conversation_id: EntityId,
        event_id: &EntityId,
        stable_id: EntityId,
        sequence: u64,
        expected_revision: u64,
        value: usize,
    ) -> CanonicalConversationAppend {
        let event = record(event_id.clone(), 0, value);
        let stable_key = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: stable_id,
            revision: Revision::ZERO,
            updated_at: event.updated_at,
            payload: json!({
                "event_id": event_id.as_str(),
                "event_digest": record_digest(&event).unwrap(),
            }),
        };
        CanonicalConversationAppend {
            conversation_id: conversation_id.clone(),
            expected_head_revision: Revision::new(expected_revision),
            expected_next_sequence: sequence,
            event,
            head: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: conversation_id,
                revision: Revision::new(expected_revision + 1),
                updated_at: UtcTimestamp::from_unix_millis(i64::try_from(value).unwrap()),
                payload: json!({"event_head": {"sequence": sequence, "event_id": event_id.as_str()}}),
            },
            stable_key,
            intents: [
                Collection::ConversationProjectionIntents,
                Collection::ConversationUnreadIntents,
                Collection::ConversationSearchIntents,
                Collection::ConversationPublicationIntents,
            ]
            .into_iter()
            .map(|collection| RecordMutation::Put {
                collection,
                record: record(EntityId::new(), 0, value),
                precondition: WritePrecondition::Missing,
            })
            .collect(),
        }
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
    fn teammates_migration_is_backup_required_atomic_restartable_and_replay_safe() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let backup = directory.path().join("before-teammates.sqlite");
        let profile_id = EntityId::new();
        let conversation_id = EntityId::new();
        let store = EmbeddedStore::open(&path, None).unwrap();
        store
            .put_profile(record(profile_id.clone(), 0, 7), WritePrecondition::Missing)
            .unwrap();
        let migration = [RecordMutation::Put {
            collection: Collection::Conversations,
            record: record(conversation_id.clone(), 0, 8),
            precondition: WritePrecondition::Missing,
        }];

        store.inject_fault_once(FaultPoint::BeforeCommit);
        assert!(matches!(
            store.migrate_teammates("1.0.0", &backup, &migration),
            Err(StoreError::Injected(FaultPoint::BeforeCommit))
        ));
        assert!(
            store
                .get_record(Collection::Conversations, &conversation_id)
                .unwrap()
                .is_none()
        );
        assert!(backup.exists());

        // The verified backup is durable across interruption; retry uses a fresh destination and
        // commits the domain batch and version marker together.
        let retry_backup = directory.path().join("before-teammates-retry.sqlite");
        assert!(matches!(
            store
                .migrate_teammates("1.0.0", &retry_backup, &migration)
                .unwrap(),
            TeammateMigrationOutcome::Applied(CommitReceipt {
                applied_mutations: 2
            })
        ));
        assert!(
            store
                .get_record(Collection::Conversations, &conversation_id)
                .unwrap()
                .is_some()
        );
        assert_eq!(
            store.get_profile(&profile_id).unwrap().unwrap().payload,
            json!({"value": 7})
        );

        let no_extra_backup = directory.path().join("replay-must-not-write.sqlite");
        assert_eq!(
            store
                .migrate_teammates("1.0.0", &no_extra_backup, &migration)
                .unwrap(),
            TeammateMigrationOutcome::AlreadyApplied
        );
        assert!(!no_extra_backup.exists());
        assert_eq!(
            store.list_records(Collection::Conversations).unwrap().len(),
            1
        );
    }

    #[test]
    fn teammates_migration_rolls_back_at_every_write_and_recovers_unknown_commit() {
        for failing_index in 0..=2 {
            let directory = tempdir().unwrap();
            let path = directory.path().join("runtime.sqlite");
            let store = EmbeddedStore::open(&path, None).unwrap();
            let records = [
                RecordMutation::Put {
                    collection: Collection::LegacySessions,
                    record: record(EntityId::new(), 0, 1),
                    precondition: WritePrecondition::Missing,
                },
                RecordMutation::Put {
                    collection: Collection::MigrationProvenance,
                    record: record(EntityId::new(), 0, 2),
                    precondition: WritePrecondition::Missing,
                },
            ];
            store.inject_fault_before_migration_write_once(failing_index);
            assert!(
                matches!(store.migrate_teammates("v1", &directory.path().join("backup.sqlite"), &records), Err(StoreError::InjectedWrite(index)) if index == failing_index)
            );
            assert!(
                store
                    .list_records(Collection::LegacySessions)
                    .unwrap()
                    .is_empty()
            );
            assert!(
                store
                    .list_records(Collection::MigrationProvenance)
                    .unwrap()
                    .is_empty()
            );
            assert!(store.list_migrations().unwrap().is_empty());
        }

        let directory = tempdir().unwrap();
        let store = EmbeddedStore::open(&directory.path().join("runtime.sqlite"), None).unwrap();
        let id = EntityId::new();
        let records = [RecordMutation::Put {
            collection: Collection::LegacySessions,
            record: record(id.clone(), 0, 1),
            precondition: WritePrecondition::Missing,
        }];
        store.inject_fault_once(FaultPoint::AfterCommit);
        assert!(matches!(
            store.migrate_teammates("v1", &directory.path().join("backup.sqlite"), &records),
            Err(StoreError::UnknownOutcome)
        ));
        assert_eq!(
            store
                .migrate_teammates("v1", &directory.path().join("unused.sqlite"), &records)
                .unwrap(),
            TeammateMigrationOutcome::AlreadyApplied
        );
        assert!(
            store
                .get_record(Collection::LegacySessions, &id)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn teammates_migration_enforces_canonical_version_and_domain_keys() {
        let directory = tempdir().unwrap();
        let store = EmbeddedStore::open(&directory.path().join("runtime.sqlite"), None).unwrap();
        assert!(matches!(
            store.migrate_teammates(" v1 ", &directory.path().join("bad.sqlite"), &[]),
            Err(StoreError::InvalidMigrationVersion)
        ));
        let owner = EntityId::new().to_string();
        let mutations = [1, 2].map(|value| RecordMutation::Put {
            collection: Collection::ComputerRecords,
            record: VersionedRecord {
                payload: serde_json::json!({"owner_profile_id": owner, "value": value}),
                ..record(EntityId::new(), 0, value)
            },
            precondition: WritePrecondition::Missing,
        });
        assert!(matches!(
            store.migrate_teammates("v1", &directory.path().join("backup.sqlite"), &mutations),
            Err(StoreError::Conflict {
                collection: Collection::ComputerRecords,
                ..
            })
        ));
        assert!(
            store
                .list_records(Collection::ComputerRecords)
                .unwrap()
                .is_empty()
        );
        let digest = sha256(b"abc")
            .iter()
            .fold(String::new(), |mut output, byte| {
                use std::fmt::Write as _;
                write!(&mut output, "{byte:02x}").unwrap();
                output
            });
        assert_eq!(
            digest,
            "ba7816bf8f01cfea414140de5dae2223b00361a396177a9cb410ff61f20015ad"
        );
    }

    #[test]
    fn teammates_backup_failure_prevents_every_migration_write() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let backup = directory.path().join("already-exists.sqlite");
        fs::write(&backup, b"do not overwrite").unwrap();
        let store = EmbeddedStore::open(&path, None).unwrap();
        let id = EntityId::new();
        assert!(matches!(
            store.migrate_teammates(
                "1.0.0",
                &backup,
                &[RecordMutation::Put {
                    collection: Collection::LegacySessions,
                    record: record(id.clone(), 0, 1),
                    precondition: WritePrecondition::Missing,
                }]
            ),
            Err(StoreError::Backup(_))
        ));
        assert!(
            store
                .get_record(Collection::LegacySessions, &id)
                .unwrap()
                .is_none()
        );
        assert!(store.list_migrations().unwrap().is_empty());
        assert_eq!(fs::read(backup).unwrap(), b"do not overwrite");
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn agent_saga_records_are_isolated_from_computer_and_migration_decoders_after_restart() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("delete-isolation.sqlite");
        let backup = directory.path().join("delete-isolation-backup.sqlite");
        let profile_id = EntityId::new().to_string();
        let operation_id = EntityId::new().to_string();
        let computer_id = EntityId::new().to_string();
        let knowledge_space_id = EntityId::new().to_string();
        let make = |payload| VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: EntityId::new(),
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::UNIX_EPOCH,
            payload,
        };
        let store = EmbeddedStore::open(&path, None).unwrap();
        let mutations = vec![
            RecordMutation::Put {
                collection: Collection::ComputerRecords,
                record: make(
                    json!({"owner_profile_id": profile_id.clone(), "computer_state": "ready"}),
                ),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::ComputerAudits,
                record: make(json!({"computer_id": computer_id.clone(), "action": "started"})),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::AgentProvisionOperations,
                record: make(json!({
                    "operation_id": operation_id.clone(),
                    "profile_id": profile_id.clone(),
                    "phase": "resources_provisioned"
                })),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::SharedKnowledgeSpaces,
                record: make(json!({
                    "space_id": knowledge_space_id.clone(),
                    "owner_profile_id": profile_id.clone(),
                    "permission_revision": 3
                })),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::MigrationProvenance,
                record: make(json!({"migration_version": "v1", "source_session": "legacy-root"})),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::AgentDeleteOperations,
                record: make(
                    json!({"operation_id": operation_id.clone(), "profile_id": profile_id.clone()}),
                ),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::AgentDeleteReceipts,
                record: make(
                    json!({"operation_id": operation_id.clone(), "profile_id": profile_id.clone()}),
                ),
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::AgentDeleteAudits,
                record: make(
                    json!({"operation_id": operation_id.clone(), "profile_id": profile_id.clone()}),
                ),
                precondition: WritePrecondition::Missing,
            },
        ];
        assert!(matches!(
            store
                .migrate_teammates("delete-collections-v1", &backup, &mutations)
                .unwrap(),
            TeammateMigrationOutcome::Applied(_)
        ));
        drop(store);

        let reopened = EmbeddedStore::open(&path, None).unwrap();
        let computer = reopened
            .list_records(Collection::ComputerRecords)
            .unwrap()
            .remove(0)
            .payload;
        assert_eq!(computer.as_object().unwrap().len(), 2);
        assert_eq!(computer["owner_profile_id"], profile_id);
        assert_eq!(computer["computer_state"], "ready");
        let computer_audit = reopened
            .list_records(Collection::ComputerAudits)
            .unwrap()
            .remove(0)
            .payload;
        assert_eq!(computer_audit.as_object().unwrap().len(), 2);
        assert_eq!(computer_audit["computer_id"], computer_id);
        assert_eq!(computer_audit["action"], "started");
        let provision = reopened
            .list_records(Collection::AgentProvisionOperations)
            .unwrap()
            .remove(0)
            .payload;
        assert_eq!(provision.as_object().unwrap().len(), 3);
        assert_eq!(provision["operation_id"], operation_id);
        assert_eq!(provision["profile_id"], profile_id);
        assert_eq!(provision["phase"], "resources_provisioned");
        let knowledge_space = reopened
            .list_records(Collection::SharedKnowledgeSpaces)
            .unwrap()
            .remove(0)
            .payload;
        assert_eq!(knowledge_space.as_object().unwrap().len(), 3);
        assert_eq!(knowledge_space["space_id"], knowledge_space_id);
        assert_eq!(knowledge_space["owner_profile_id"], profile_id);
        assert_eq!(knowledge_space["permission_revision"], 3);
        let migration = reopened
            .list_records(Collection::MigrationProvenance)
            .unwrap()
            .remove(0)
            .payload;
        assert_eq!(migration.as_object().unwrap().len(), 2);
        assert_eq!(migration["migration_version"], "v1");
        assert_eq!(migration["source_session"], "legacy-root");
        for collection in [
            Collection::AgentDeleteOperations,
            Collection::AgentDeleteReceipts,
            Collection::AgentDeleteAudits,
        ] {
            let deletion = reopened.list_records(collection).unwrap().remove(0).payload;
            assert_eq!(deletion.as_object().unwrap().len(), 2);
            assert_eq!(deletion["operation_id"], operation_id);
            assert_eq!(deletion["profile_id"], profile_id);
        }
        assert_eq!(
            reopened
                .list_records(Collection::ComputerRecords)
                .unwrap()
                .len(),
            1
        );
        assert_eq!(
            reopened
                .list_records(Collection::MigrationProvenance)
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn shared_knowledge_space_and_grant_are_atomic_and_restartable() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("shared-knowledge.sqlite");
        let space_id = EntityId::new();
        let owner_id = EntityId::new();
        let grant_id = EntityId::new();
        let mutations = [
            RecordMutation::Put {
                collection: Collection::SharedKnowledgeSpaces,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: space_id.clone(),
                    revision: Revision::ZERO,
                    updated_at: UtcTimestamp::UNIX_EPOCH,
                    payload: json!({
                        "id": space_id.as_str(),
                        "owner": owner_id.as_str(),
                        "members": {},
                        "permission_revision": 0,
                        "source_conversation_id": null,
                        "source_event_ids": [],
                        "deleted": false
                    }),
                },
                precondition: WritePrecondition::Missing,
            },
            RecordMutation::Put {
                collection: Collection::SharedKnowledgeGrants,
                record: record(grant_id.clone(), 0, 1),
                precondition: WritePrecondition::Missing,
            },
        ];
        let store = EmbeddedStore::open(&path, None).unwrap();
        store.inject_fault_once(FaultPoint::BeforeCommit);
        assert!(matches!(
            store.transact(&mutations),
            Err(StoreError::Injected(FaultPoint::BeforeCommit))
        ));
        assert!(
            store
                .get_record(Collection::SharedKnowledgeSpaces, &space_id)
                .unwrap()
                .is_none()
        );
        assert!(
            store
                .get_record(Collection::SharedKnowledgeGrants, &grant_id)
                .unwrap()
                .is_none()
        );
        store.transact(&mutations).unwrap();
        drop(store);
        let reopened = EmbeddedStore::open(&path, None).unwrap();
        let persisted = reopened
            .get_shared_knowledge_space(&space_id)
            .unwrap()
            .unwrap();
        let mut next = persisted.clone();
        next.revision = Revision::new(1);
        next.updated_at = UtcTimestamp(1);
        next.payload["permission_revision"] = json!(1);
        reopened
            .put_shared_knowledge_space(next.clone(), WritePrecondition::Exact(Revision::ZERO))
            .unwrap();
        assert_eq!(
            reopened.list_shared_knowledge_spaces().unwrap(),
            vec![next.clone()]
        );
        assert!(matches!(
            reopened.put_shared_knowledge_space(next, WritePrecondition::Exact(Revision::ZERO)),
            Err(StoreError::Conflict { .. })
        ));
        assert!(
            reopened
                .get_record(Collection::SharedKnowledgeGrants, &grant_id)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn canonical_append_is_atomic_immutable_replay_safe_and_restartable() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("canonical.sqlite");
        let conversation_id = EntityId::new();
        let event_id = EntityId::new();
        let stable_id = EntityId::new();
        let append = canonical_append(conversation_id.clone(), &event_id, stable_id, 1, 0, 10);
        {
            let store = EmbeddedStore::open(&path, None).unwrap();
            store
                .transact(&[RecordMutation::Put {
                    collection: Collection::Conversations,
                    record: VersionedRecord {
                        version: CURRENT_SCHEMA_VERSION,
                        id: conversation_id.clone(),
                        revision: Revision::ZERO,
                        updated_at: UtcTimestamp::UNIX_EPOCH,
                        payload: json!({"event_head": null}),
                    },
                    precondition: WritePrecondition::Missing,
                }])
                .unwrap();
            store.inject_fault_once(FaultPoint::BeforeCommit);
            assert!(matches!(
                store.append_canonical_conversation(&append),
                Err(StoreError::Injected(FaultPoint::BeforeCommit))
            ));
            assert!(
                store
                    .get_record(Collection::ConversationEvents, &event_id)
                    .unwrap()
                    .is_none()
            );
            assert!(matches!(
                store.append_canonical_conversation(&append).unwrap(),
                CanonicalAppendOutcome::Applied { .. }
            ));
            assert!(matches!(
                store.append_canonical_conversation(&append).unwrap(),
                CanonicalAppendOutcome::Replay { .. }
            ));
            assert!(matches!(
                store.transact(&[RecordMutation::Delete {
                    collection: Collection::ConversationEvents,
                    id: event_id.clone(),
                    precondition: WritePrecondition::Any,
                }]),
                Err(StoreError::AppendOnlyViolation(_))
            ));
        }
        let reopened = EmbeddedStore::open(&path, None).unwrap();
        assert!(matches!(
            reopened.append_canonical_conversation(&append).unwrap(),
            CanonicalAppendOutcome::Replay { .. }
        ));
        assert_eq!(
            reopened
                .get_record(Collection::Conversations, &conversation_id)
                .unwrap()
                .unwrap()
                .revision,
            Revision::new(1)
        );
    }

    #[test]
    fn concurrent_canonical_sequence_allocation_has_one_winner() {
        use std::sync::Barrier;
        let directory = tempdir().unwrap();
        let path = directory.path().join("concurrent-canonical.sqlite");
        let conversation_id = EntityId::new();
        let seed = EmbeddedStore::open(&path, None).unwrap();
        seed.transact(&[RecordMutation::Put {
            collection: Collection::Conversations,
            record: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: conversation_id.clone(),
                revision: Revision::ZERO,
                updated_at: UtcTimestamp::UNIX_EPOCH,
                payload: json!({"event_head": null}),
            },
            precondition: WritePrecondition::Missing,
        }])
        .unwrap();
        drop(seed);
        let barrier = Arc::new(Barrier::new(2));
        let mut workers = Vec::new();
        for value in [21, 22] {
            let path = path.clone();
            let barrier = Arc::clone(&barrier);
            let conversation_id = conversation_id.clone();
            workers.push(thread::spawn(move || {
                let store = EmbeddedStore::open(&path, None).unwrap();
                let append = canonical_append(
                    conversation_id,
                    &EntityId::new(),
                    EntityId::new(),
                    1,
                    0,
                    value,
                );
                barrier.wait();
                store.append_canonical_conversation(&append)
            }));
        }
        let results: Vec<_> = workers
            .into_iter()
            .map(|worker| worker.join().unwrap())
            .collect();
        assert_eq!(results.iter().filter(|result| result.is_ok()).count(), 1);
        assert_eq!(
            EmbeddedStore::open(&path, None)
                .unwrap()
                .list_records(Collection::ConversationEvents)
                .unwrap()
                .len(),
            1
        );
    }

    #[test]
    fn projection_rebuild_is_head_guarded_and_cannot_rewrite_canonical_rows() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let conversation_id = EntityId::new();
        store
            .transact(&[RecordMutation::Put {
                collection: Collection::Conversations,
                record: record(conversation_id.clone(), 0, 1),
                precondition: WritePrecondition::Missing,
            }])
            .unwrap();
        let projection_id = EntityId::new();
        let rebuild = ConversationProjectionRebuild {
            conversation_id: conversation_id.clone(),
            expected_head_revision: Revision::ZERO,
            mutations: vec![RecordMutation::Put {
                collection: Collection::ConversationSearchIntents,
                record: record(projection_id.clone(), 0, 2),
                precondition: WritePrecondition::Missing,
            }],
        };
        assert_eq!(
            store
                .rebuild_conversation_projections(&rebuild)
                .unwrap()
                .applied_mutations,
            1
        );
        let forbidden = ConversationProjectionRebuild {
            conversation_id,
            expected_head_revision: Revision::ZERO,
            mutations: vec![RecordMutation::Delete {
                collection: Collection::ConversationEvents,
                id: EntityId::new(),
                precondition: WritePrecondition::Missing,
            }],
        };
        assert!(matches!(
            store.rebuild_conversation_projections(&forbidden),
            Err(StoreError::InvalidCanonicalAppend(_))
        ));
    }

    #[test]
    fn profile_execution_close_cancellation_can_be_acknowledged_with_the_issued_permit() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let fixture = seed_profile_execution_fixture(&store);
        let initialized = initialize_profile_execution(&store, &fixture);
        assert_eq!(
            initialize_profile_execution(&store, &fixture).status,
            ProfileExecutionMutationStatus::Replay
        );
        let request = profile_execution_request(&store, &fixture, &initialized.fence);
        let admission = store
            .admit_profile_execution(&request, UtcTimestamp::from_unix_millis(10))
            .unwrap();
        assert_eq!(admission.status, ProfileExecutionMutationStatus::Applied);
        assert_eq!(
            store
                .admit_profile_execution(&request, UtcTimestamp::from_unix_millis(10))
                .unwrap(),
            ProfileExecutionAdmissionOutcome {
                status: ProfileExecutionMutationStatus::Replay,
                permit: admission.permit.clone(),
            }
        );

        let close = store
            .close_profile_execution_fence(
                &ProfileExecutionCloseRequest {
                    profile_id: fixture.profile_id.clone(),
                    expected_epoch: initialized.fence.epoch,
                    expected_revision: initialized.fence.revision,
                },
                UtcTimestamp::from_unix_millis(20),
            )
            .unwrap();
        assert_eq!(close.fence.state, ProfileExecutionFenceState::Closing);
        assert_eq!(close.fence.epoch, initialized.fence.epoch + 1);
        assert_eq!(close.active.len(), 1);
        assert_eq!(
            close.active[0].cancellation_requested_at,
            Some(UtcTimestamp::from_unix_millis(20))
        );
        assert_eq!(
            close.active[0].revision,
            admission
                .permit
                .registration_revision
                .checked_next()
                .unwrap()
        );
        assert!(
            store
                .renew_profile_execution(
                    &admission.permit,
                    UtcTimestamp::from_unix_millis(600),
                    UtcTimestamp::from_unix_millis(21),
                )
                .is_err()
        );

        let completed = store
            .complete_profile_execution(&admission.permit, UtcTimestamp::from_unix_millis(22))
            .unwrap();
        assert_eq!(completed.fence.state, ProfileExecutionFenceState::Closed);
        assert!(completed.active.is_empty());
        assert_eq!(
            store
                .complete_profile_execution(&admission.permit, UtcTimestamp::from_unix_millis(23))
                .unwrap()
                .status,
            ProfileExecutionMutationStatus::Replay
        );
    }

    #[test]
    fn profile_execution_rejects_cross_profile_and_cross_root_session_catalog_bindings() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let fixture = seed_profile_execution_fixture(&store);
        let initialized = initialize_profile_execution(&store, &fixture);
        let cross_profile = profile_execution_request_with_catalog_binding(
            &store,
            &fixture,
            &initialized.fence,
            &ProfileId::new(),
            &fixture.worker.root_tree_id,
        );
        assert!(matches!(
            store.admit_profile_execution(&cross_profile, UtcTimestamp::from_unix_millis(10)),
            Err(StoreError::ProfileExecutionRejected { .. })
        ));

        let cross_root = profile_execution_request_with_catalog_binding(
            &store,
            &fixture,
            &initialized.fence,
            &fixture.profile_id,
            &RootTreeId::new(),
        );
        assert!(matches!(
            store.admit_profile_execution(&cross_root, UtcTimestamp::from_unix_millis(10)),
            Err(StoreError::ProfileExecutionRejected { .. })
        ));
        assert!(
            store
                .profile_execution_snapshot(&fixture.profile_id)
                .unwrap()
                .active
                .is_empty()
        );
    }

    #[test]
    fn profile_execution_cross_instance_close_vs_admit_has_a_serializable_winner() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("profile-admission-race.sqlite");
        let seed = EmbeddedStore::open(&path, None).unwrap();
        let fixture = seed_profile_execution_fixture(&seed);
        let initialized = initialize_profile_execution(&seed, &fixture);
        let request = profile_execution_request(&seed, &fixture, &initialized.fence);
        let close_request = ProfileExecutionCloseRequest {
            profile_id: fixture.profile_id.clone(),
            expected_epoch: initialized.fence.epoch,
            expected_revision: initialized.fence.revision,
        };
        drop(seed);

        let barrier = Arc::new(Barrier::new(3));
        let admitter = {
            let path = path.clone();
            let barrier = Arc::clone(&barrier);
            let request = request.clone();
            thread::spawn(move || {
                let store = EmbeddedStore::open(&path, None).unwrap();
                barrier.wait();
                store.admit_profile_execution(&request, UtcTimestamp::from_unix_millis(10))
            })
        };
        let closer = {
            let path = path.clone();
            let barrier = Arc::clone(&barrier);
            thread::spawn(move || {
                let store = EmbeddedStore::open(&path, None).unwrap();
                barrier.wait();
                store.close_profile_execution_fence(
                    &close_request,
                    UtcTimestamp::from_unix_millis(11),
                )
            })
        };
        barrier.wait();
        let admission = admitter.join().unwrap();
        let close = closer.join().unwrap().unwrap();
        let reopened = EmbeddedStore::open(&path, None).unwrap();
        let snapshot = reopened
            .profile_execution_snapshot(&fixture.profile_id)
            .unwrap();

        match admission {
            Ok(outcome) => {
                assert_eq!(close.fence.state, ProfileExecutionFenceState::Closing);
                assert_eq!(snapshot.active.len(), 1);
                assert_eq!(snapshot.active[0].id, outcome.permit.registration_id);
                reopened
                    .complete_profile_execution(&outcome.permit, UtcTimestamp::from_unix_millis(12))
                    .unwrap();
            }
            Err(StoreError::ProfileExecutionRejected { .. }) => {
                assert_eq!(close.fence.state, ProfileExecutionFenceState::Closed);
                assert!(snapshot.active.is_empty());
            }
            Err(error) => panic!("unexpected admission race result: {error}"),
        }
        assert_eq!(
            reopened
                .profile_execution_snapshot(&fixture.profile_id)
                .unwrap()
                .fence
                .state,
            ProfileExecutionFenceState::Closed
        );
    }

    #[test]
    fn profile_execution_crash_is_reclaimed_and_fence_reopens_after_restart() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("profile-admission-recovery.sqlite");
        let (fixture, permit) = {
            let store = EmbeddedStore::open(&path, None).unwrap();
            let fixture = seed_profile_execution_fixture(&store);
            let initialized = initialize_profile_execution(&store, &fixture);
            let permit = store
                .admit_profile_execution(
                    &profile_execution_request(&store, &fixture, &initialized.fence),
                    UtcTimestamp::from_unix_millis(10),
                )
                .unwrap()
                .permit;
            store
                .close_profile_execution_fence(
                    &ProfileExecutionCloseRequest {
                        profile_id: fixture.profile_id.clone(),
                        expected_epoch: initialized.fence.epoch,
                        expected_revision: initialized.fence.revision,
                    },
                    UtcTimestamp::from_unix_millis(20),
                )
                .unwrap();
            (fixture, permit)
        };

        let reopened = EmbeddedStore::open(&path, None).unwrap();
        let before_reclaim = reopened
            .profile_execution_snapshot(&fixture.profile_id)
            .unwrap();
        assert_eq!(
            before_reclaim.fence.state,
            ProfileExecutionFenceState::Closing
        );
        assert_eq!(before_reclaim.active.len(), 1);
        let reclaimed = reopened
            .reclaim_profile_executions(&fixture.profile_id, UtcTimestamp::from_unix_millis(600))
            .unwrap();
        assert_eq!(reclaimed.status, ProfileExecutionMutationStatus::Applied);
        assert_eq!(reclaimed.fence.state, ProfileExecutionFenceState::Closed);
        assert!(reclaimed.active.is_empty());
        let durable_registration = reopened
            .get_record(
                Collection::ProfileExecutionRegistrations,
                &permit.registration_id,
            )
            .unwrap()
            .unwrap();
        assert_eq!(
            decode_profile_execution_registration(&durable_registration)
                .unwrap()
                .state,
            ProfileExecutionRegistrationState::Reclaimed
        );
        assert!(
            reopened
                .complete_profile_execution(&permit, UtcTimestamp::from_unix_millis(601))
                .is_err()
        );
        assert_eq!(
            reopened
                .reclaim_profile_executions(
                    &fixture.profile_id,
                    UtcTimestamp::from_unix_millis(602)
                )
                .unwrap()
                .status,
            ProfileExecutionMutationStatus::Replay
        );

        let reopened_fence = reopened
            .reopen_profile_execution_fence(
                &ProfileExecutionReopenRequest {
                    profile_id: fixture.profile_id.clone(),
                    expected_profile_revision: fixture.profile_revision,
                    expected_epoch: reclaimed.fence.epoch,
                    expected_revision: reclaimed.fence.revision,
                },
                UtcTimestamp::from_unix_millis(603),
            )
            .unwrap();
        assert_eq!(reopened_fence.fence.state, ProfileExecutionFenceState::Open);
        assert!(reopened_fence.fence.epoch > reclaimed.fence.epoch);
    }

    #[test]
    fn profile_execution_stale_epoch_cannot_commit_after_close_or_reopen() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let fixture = seed_profile_execution_fixture(&store);
        let initialized = initialize_profile_execution(&store, &fixture);
        let permit = store
            .admit_profile_execution(
                &profile_execution_request(&store, &fixture, &initialized.fence),
                UtcTimestamp::from_unix_millis(10),
            )
            .unwrap()
            .permit;
        let before_close_id = EntityId::new();
        store
            .transact_profile_execution_commit(
                &permit,
                &[RecordMutation::Put {
                    collection: Collection::PendingActions,
                    record: record(before_close_id.clone(), 0, 12),
                    precondition: WritePrecondition::Missing,
                }],
                UtcTimestamp::from_unix_millis(12),
            )
            .unwrap();
        let closed = store
            .close_profile_execution_fence(
                &ProfileExecutionCloseRequest {
                    profile_id: fixture.profile_id.clone(),
                    expected_epoch: initialized.fence.epoch,
                    expected_revision: initialized.fence.revision,
                },
                UtcTimestamp::from_unix_millis(20),
            )
            .unwrap();
        let stale_write_id = EntityId::new();
        assert!(matches!(
            store.transact_profile_execution_commit(
                &permit,
                &[RecordMutation::Put {
                    collection: Collection::PendingActions,
                    record: record(stale_write_id.clone(), 0, 21),
                    precondition: WritePrecondition::Missing,
                }],
                UtcTimestamp::from_unix_millis(21),
            ),
            Err(StoreError::ProfileExecutionRejected { .. })
        ));
        assert!(
            store
                .get_record(Collection::PendingActions, &stale_write_id)
                .unwrap()
                .is_none()
        );
        let completed = store
            .complete_profile_execution(&permit, UtcTimestamp::from_unix_millis(22))
            .unwrap();
        assert_eq!(completed.fence.state, ProfileExecutionFenceState::Closed);
        let reopened = store
            .reopen_profile_execution_fence(
                &ProfileExecutionReopenRequest {
                    profile_id: fixture.profile_id.clone(),
                    expected_profile_revision: fixture.profile_revision,
                    expected_epoch: closed.fence.epoch,
                    expected_revision: completed.fence.revision,
                },
                UtcTimestamp::from_unix_millis(23),
            )
            .unwrap();
        assert_eq!(reopened.fence.state, ProfileExecutionFenceState::Open);
        assert!(matches!(
            store.transact_profile_execution_commit(
                &permit,
                &[RecordMutation::Put {
                    collection: Collection::PendingActions,
                    record: record(EntityId::new(), 0, 24),
                    precondition: WritePrecondition::Missing,
                }],
                UtcTimestamp::from_unix_millis(24),
            ),
            Err(StoreError::ProfileExecutionRejected { .. })
        ));
        assert!(
            store
                .get_record(Collection::PendingActions, &before_close_id)
                .unwrap()
                .is_some()
        );
    }

    #[test]
    fn profile_execution_collections_are_protected_and_corruption_is_rejected() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        assert!(matches!(
            store.transact(&[RecordMutation::Put {
                collection: Collection::ProfileExecutionFences,
                record: record(EntityId::new(), 0, 1),
                precondition: WritePrecondition::Missing,
            }]),
            Err(StoreError::ProfileExecutionProtected(
                Collection::ProfileExecutionFences
            ))
        ));

        let fixture = seed_profile_execution_fixture(&store);
        initialize_profile_execution(&store, &fixture);
        {
            let connection = store.lock().unwrap();
            connection
                .execute(
                    "UPDATE records SET payload = ?1 WHERE collection = ?2 AND id = ?3",
                    rusqlite::params![
                        br#"{"unexpected":true}"#.as_slice(),
                        Collection::ProfileExecutionFences.as_str(),
                        fixture.profile_id.as_entity_id().as_str(),
                    ],
                )
                .unwrap();
        }
        assert!(matches!(
            store.profile_execution_snapshot(&fixture.profile_id),
            Err(StoreError::CorruptRecord {
                collection: Collection::ProfileExecutionFences,
                ..
            })
        ));
    }

    #[test]
    fn direct_message_keys_are_atomic_unique_cross_instance_and_replay_safe() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("direct-message-keys.sqlite");
        let profile_id = ProfileId::new();
        let make_binding =
            |key_id: EntityId, conversation_id: EntityId| CanonicalDirectConversationBinding {
                key: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: key_id,
                    revision: Revision::ZERO,
                    updated_at: UtcTimestamp::from_unix_millis(10),
                    payload: json!({
                        "human_dm_profile_id": profile_id,
                        "conversation_id": conversation_id,
                    }),
                },
                conversation: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: conversation_id.clone(),
                    revision: Revision::ZERO,
                    updated_at: UtcTimestamp::from_unix_millis(10),
                    payload: json!({"kind": "human_agent_dm"}),
                },
                participants: ["human", "agent"]
                    .into_iter()
                    .map(|principal| VersionedRecord {
                        version: CURRENT_SCHEMA_VERSION,
                        id: EntityId::new(),
                        revision: Revision::ZERO,
                        updated_at: UtcTimestamp::from_unix_millis(10),
                        payload: json!({
                            "conversation_id": conversation_id,
                            "principal": principal,
                        }),
                    })
                    .collect(),
            };
        let first = make_binding(EntityId::new(), EntityId::new());
        let store = EmbeddedStore::open(&path, None).unwrap();
        store.inject_fault_once(FaultPoint::BeforeCommit);
        assert!(matches!(
            store.bind_direct_conversation(&first),
            Err(StoreError::Injected(FaultPoint::BeforeCommit))
        ));
        assert!(
            store
                .get_record(Collection::DirectMessageKeys, &first.key.id)
                .unwrap()
                .is_none()
        );
        assert!(matches!(
            store.bind_direct_conversation(&first).unwrap(),
            CanonicalDirectConversationOutcome::Applied { .. }
        ));
        drop(store);
        assert!(matches!(
            EmbeddedStore::open(&path, None)
                .unwrap()
                .bind_direct_conversation(&first)
                .unwrap(),
            CanonicalDirectConversationOutcome::Replay { .. }
        ));

        let barrier = Arc::new(Barrier::new(3));
        let mut workers = Vec::new();
        for binding in [
            make_binding(EntityId::new(), EntityId::new()),
            make_binding(EntityId::new(), EntityId::new()),
        ] {
            let path = path.clone();
            let barrier = Arc::clone(&barrier);
            workers.push(thread::spawn(move || {
                let store = EmbeddedStore::open(&path, None).unwrap();
                barrier.wait();
                store.bind_direct_conversation(&binding)
            }));
        }
        barrier.wait();
        let successes = workers
            .into_iter()
            .map(|worker| worker.join().unwrap())
            .filter(Result::is_ok)
            .count();
        assert_eq!(
            successes, 0,
            "the first durable profile key already owns uniqueness"
        );
    }

    #[test]
    fn delivery_finalization_is_atomic_restartable_and_unknown_outcome_replay_safe() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("delivery-finalization.sqlite");
        let delivery_id = EntityId::new();
        let initial = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: delivery_id.clone(),
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::from_unix_millis(1),
            payload: json!({"stable_source_key": "delivery/source/1", "state": "claimed"}),
        };
        let store = EmbeddedStore::open(&path, None).unwrap();
        store
            .transact(&[RecordMutation::Put {
                collection: Collection::ConversationDeliveries,
                record: initial,
                precondition: WritePrecondition::Missing,
            }])
            .unwrap();
        let finalization = ConversationDeliveryFinalization {
            delivery: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: delivery_id.clone(),
                revision: Revision::new(1),
                updated_at: UtcTimestamp::from_unix_millis(2),
                payload: json!({"stable_source_key": "delivery/source/1", "state": "delivered"}),
            },
            expected_delivery_revision: Revision::ZERO,
            finalization_intent: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: EntityId::new(),
                revision: Revision::ZERO,
                updated_at: UtcTimestamp::from_unix_millis(2),
                payload: json!({"delivery_id": delivery_id}),
            },
            publication_outbox: Some(VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: EntityId::new(),
                revision: Revision::ZERO,
                updated_at: UtcTimestamp::from_unix_millis(2),
                payload: json!({
                    "delivery_id": delivery_id,
                    "publication_key": "publication/delivery/1",
                    "source_event_id": EntityId::new(),
                    "source_profile_id": ProfileId::new(),
                    "participant_session_revision": 4,
                    "payload_digest": "00".repeat(32),
                    "state": "pending",
                }),
            }),
            supersession: Some(VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: EntityId::new(),
                revision: Revision::ZERO,
                updated_at: UtcTimestamp::from_unix_millis(2),
                payload: json!({
                    "target_kind": "message",
                    "target_id": EntityId::new(),
                    "source_event_id": EntityId::new(),
                    "context_revision": 3,
                }),
            }),
        };
        store.inject_fault_once(FaultPoint::BeforeCommit);
        assert!(matches!(
            store.finalize_conversation_delivery(&finalization),
            Err(StoreError::Injected(FaultPoint::BeforeCommit))
        ));
        assert!(
            store
                .get_record(
                    Collection::ConversationFinalizationIntents,
                    &finalization.finalization_intent.id,
                )
                .unwrap()
                .is_none()
        );
        store.inject_fault_once(FaultPoint::AfterCommit);
        assert!(matches!(
            store.finalize_conversation_delivery(&finalization),
            Err(StoreError::UnknownOutcome)
        ));
        drop(store);
        assert!(matches!(
            EmbeddedStore::open(&path, None)
                .unwrap()
                .finalize_conversation_delivery(&finalization)
                .unwrap(),
            ConversationDeliveryFinalizationOutcome::Replay { .. }
        ));
    }

    #[test]
    fn peer_message_source_and_publication_keys_are_unique_cross_instance() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("peer-message-keys.sqlite");
        EmbeddedStore::open(&path, None).unwrap();
        let conversation_id = EntityId::new();
        let source_event_id = EntityId::new();
        let destination_profile_id = ProfileId::new();
        let barrier = Arc::new(Barrier::new(3));
        let mut workers = Vec::new();
        for value in 1..=2 {
            let path = path.clone();
            let barrier = Arc::clone(&barrier);
            let conversation_id = conversation_id.clone();
            let source_event_id = source_event_id.clone();
            let destination_profile_id = destination_profile_id.clone();
            workers.push(thread::spawn(move || {
                let store = EmbeddedStore::open(&path, None).unwrap();
                let action = VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: EntityId::new(),
                    revision: Revision::ZERO,
                    updated_at: UtcTimestamp::from_unix_millis(value),
                    payload: json!({
                        "action": {
                            "source": {
                                "source": "peer_message",
                                "binding": {
                                    "publication_key": "peer/publication/1",
                                    "conversation_id": conversation_id,
                                    "source_event_id": source_event_id,
                                    "destination_profile_id": destination_profile_id,
                                }
                            }
                        }
                    }),
                };
                barrier.wait();
                store.transact(&[RecordMutation::Put {
                    collection: Collection::PendingActions,
                    record: action,
                    precondition: WritePrecondition::Missing,
                }])
            }));
        }
        barrier.wait();
        assert_eq!(
            workers
                .into_iter()
                .map(|worker| worker.join().unwrap())
                .filter(Result::is_ok)
                .count(),
            1
        );
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
                + MigrationRepository<Error = StoreError>
                + SharedKnowledgeSpaceRepository<Error = StoreError>
                + ProfileExecutionAdmissionRepository<Error = StoreError>
                + DirectConversationRepository<Error = StoreError>
                + ConversationDeliveryRepository<Error = StoreError>,
        {
        }
        assert_contracts::<EmbeddedStore>();
    }
}
