use std::collections::BTreeMap;
use std::path::{Path, PathBuf};
use std::time::Duration;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, Generation, Revision, RootTreeId, SessionId, UtcTimestamp,
    WorkerId,
};
use keith_delivery::{DeliveryItem, DeliveryState};
use keith_scheduler::{JobAttempt, JobAttemptState, ScheduledJob};
use keith_session_store::{RecoveryReport, SessionStore, SessionStoreError};
use keith_state_store::{EmbeddedStore, FileBackupHook, StoreError};
use keith_state_store_core::{
    AtomicStateRepository, Collection, RecordMutation, VersionedRecord, WritePrecondition,
};
use keith_tool_core::Repeatability;
use keith_waiting::WaitingItem;
use keith_worker_runtime::{LeaseError, LeaseManager};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::{CatalogError, RootCatalog};

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StartupRecoveryStage {
    StateMigrated,
    StaleClaimsExpired,
    LazyCatalogRebuilt,
    SchedulesRestored,
    WaitsRestored,
    DeliveriesRestored,
    ChannelOffsetsRestored,
    PublicEndpointsReady,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct StartupRecoveryReport {
    pub stages: Vec<StartupRecoveryStage>,
    pub expired_scheduler_claims: usize,
    pub expired_delivery_claims: usize,
    pub schedules: usize,
    pub waits: usize,
    pub deliveries: usize,
    pub channel_offsets: usize,
}

impl StartupRecoveryReport {
    pub fn endpoints_ready(&self) -> bool {
        self.stages.last() == Some(&StartupRecoveryStage::PublicEndpointsReady)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum OperationKind {
    Provider,
    Tool,
    Process,
    Kernel,
    Channel,
    Refinement,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RecoveryProjectionState {
    Running,
    Waiting,
    Recovering,
    Interrupted,
    Incomplete,
    Failed,
    Cancelled,
    Completed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DurableBoundary {
    Started,
    EffectObserved,
    FinalCommitted,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RetryPolicy {
    Provider {
        retryable_error: bool,
        action_allows_retry: bool,
    },
    Tool(Repeatability),
    CheckStateFirst,
    NeverAutomatic,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DurableOperation {
    pub id: EntityId,
    pub stable_identity: String,
    pub kind: OperationKind,
    pub policy: RetryPolicy,
    pub boundary: DurableBoundary,
    pub state: RecoveryProjectionState,
    pub safe_detail: Option<String>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ExternalState {
    Applied,
    NotApplied,
    Unknown,
}

pub trait ExternalStateProbe {
    /// Queries the authoritative external system using the operation's stable identity.
    ///
    /// # Errors
    ///
    /// Returns a safe error when the external state cannot be queried.
    fn query(&self, operation: &DurableOperation) -> Result<ExternalState, String>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ReconciliationAction {
    RetrySameIdentity,
    AcceptObservedEffect,
    PauseForUser,
    NoAction,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReconciliationOutcome {
    pub operation: DurableOperation,
    pub action: ReconciliationAction,
}

pub struct OperationJournal {
    store: EmbeddedStore,
}

impl OperationJournal {
    /// Opens the same transactional state database used by startup recovery.
    ///
    /// # Errors
    ///
    /// Returns an error when the store cannot be opened or migrated.
    pub fn open(path: &Path) -> Result<Self, RecoveryError> {
        Ok(Self {
            store: EmbeddedStore::open(path, Some(&FileBackupHook))?,
        })
    }

    /// Persists an operation before execution. Replaying an identical stable identity is idempotent.
    ///
    /// # Errors
    ///
    /// Returns an error for identity conflicts or persistence failure.
    pub fn begin(
        &self,
        stable_identity: impl Into<String>,
        kind: OperationKind,
        policy: RetryPolicy,
        now: UtcTimestamp,
    ) -> Result<DurableOperation, RecoveryError> {
        let stable_identity = stable_identity.into();
        if stable_identity.trim().is_empty() {
            return Err(RecoveryError::InvalidOperation(
                "stable identity is empty".into(),
            ));
        }
        if let Some(existing) = self
            .operations()?
            .into_iter()
            .find(|operation| operation.stable_identity == stable_identity)
        {
            if existing.kind == kind && existing.policy == policy {
                return Ok(existing);
            }
            return Err(RecoveryError::IdentityConflict(stable_identity));
        }
        let operation = DurableOperation {
            id: EntityId::new(),
            stable_identity,
            kind,
            policy,
            boundary: DurableBoundary::Started,
            state: RecoveryProjectionState::Running,
            safe_detail: None,
            updated_at: now,
        };
        self.put_new(&operation)?;
        Ok(operation)
    }

    /// Commits a durable boundary before acknowledging it to the caller.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing operation, regression, or persistence failure.
    pub fn commit_boundary(
        &self,
        id: &EntityId,
        boundary: DurableBoundary,
        now: UtcTimestamp,
    ) -> Result<DurableOperation, RecoveryError> {
        let (mut operation, record) = self.required(id)?;
        if boundary_rank(boundary) < boundary_rank(operation.boundary) {
            return Err(RecoveryError::BoundaryRegression);
        }
        operation.boundary = boundary;
        operation.updated_at = now;
        if boundary == DurableBoundary::FinalCommitted {
            operation.state = RecoveryProjectionState::Completed;
            operation.safe_detail = None;
        }
        self.put_existing(&operation, &record)?;
        Ok(operation)
    }

    /// Reconciles all non-terminal operations without blindly repeating external effects.
    ///
    /// # Errors
    ///
    /// Returns an error when authoritative state cannot be read or persisted.
    pub fn reconcile(
        &self,
        probe: &dyn ExternalStateProbe,
        now: UtcTimestamp,
    ) -> Result<Vec<ReconciliationOutcome>, RecoveryError> {
        let mut outcomes = Vec::new();
        for operation in self.operations()?.into_iter().filter(|operation| {
            !matches!(
                operation.state,
                RecoveryProjectionState::Completed
                    | RecoveryProjectionState::Failed
                    | RecoveryProjectionState::Cancelled
            )
        }) {
            outcomes.push(self.reconcile_one(&operation.id, probe, now)?);
        }
        Ok(outcomes)
    }

    /// Lists every durable operation in stable identifier order.
    ///
    /// # Errors
    ///
    /// Returns an error when a record cannot be read or decoded.
    pub fn operations(&self) -> Result<Vec<DurableOperation>, RecoveryError> {
        self.store
            .list_records(Collection::ActiveOperations)?
            .into_iter()
            .map(|record| decode_operation(&record))
            .collect()
    }

    fn reconcile_one(
        &self,
        id: &EntityId,
        probe: &dyn ExternalStateProbe,
        now: UtcTimestamp,
    ) -> Result<ReconciliationOutcome, RecoveryError> {
        let (mut operation, record) = self.required(id)?;
        operation.state = RecoveryProjectionState::Recovering;
        operation.updated_at = now;
        self.put_existing(&operation, &record)?;
        let (mut operation, record) = self.required(id)?;
        let external =
            match operation.policy {
                RetryPolicy::Tool(Repeatability::CheckBeforeRetry)
                | RetryPolicy::CheckStateFirst => Some(probe.query(&operation).map_err(
                    |detail| RecoveryError::Probe {
                        identity: operation.stable_identity.clone(),
                        detail,
                    },
                )?),
                _ => None,
            };
        let (state, action, detail) = decide(&operation, external);
        operation.state = state;
        operation.safe_detail = Some(detail.to_owned());
        operation.updated_at = now;
        self.put_existing(&operation, &record)?;
        Ok(ReconciliationOutcome { operation, action })
    }

    fn required(
        &self,
        id: &EntityId,
    ) -> Result<(DurableOperation, VersionedRecord), RecoveryError> {
        let record = self
            .store
            .get_record(Collection::ActiveOperations, id)?
            .ok_or_else(|| RecoveryError::MissingOperation(id.clone()))?;
        Ok((decode_operation(&record)?, record))
    }

    fn put_new(&self, operation: &DurableOperation) -> Result<(), RecoveryError> {
        self.store.transact(&[RecordMutation::Put {
            collection: Collection::ActiveOperations,
            record: operation_record(operation, Revision::new(1))?,
            precondition: WritePrecondition::Missing,
        }])?;
        Ok(())
    }

    fn put_existing(
        &self,
        operation: &DurableOperation,
        current: &VersionedRecord,
    ) -> Result<(), RecoveryError> {
        let revision = current
            .revision
            .checked_next()
            .ok_or(RecoveryError::RevisionOverflow)?;
        self.store.transact(&[RecordMutation::Put {
            collection: Collection::ActiveOperations,
            record: operation_record(operation, revision)?,
            precondition: WritePrecondition::Exact(current.revision),
        }])?;
        Ok(())
    }
}

fn decide(
    operation: &DurableOperation,
    external: Option<ExternalState>,
) -> (RecoveryProjectionState, ReconciliationAction, &'static str) {
    if operation.boundary == DurableBoundary::FinalCommitted {
        return (
            RecoveryProjectionState::Completed,
            ReconciliationAction::NoAction,
            "final result was durably committed",
        );
    }
    match operation.policy {
        RetryPolicy::Provider {
            retryable_error: true,
            action_allows_retry: true,
        }
        | RetryPolicy::Tool(Repeatability::Safe) => (
            RecoveryProjectionState::Interrupted,
            ReconciliationAction::RetrySameIdentity,
            "interrupted operation is eligible for stable-identity retry",
        ),
        RetryPolicy::Provider { .. } => (
            RecoveryProjectionState::Interrupted,
            ReconciliationAction::NoAction,
            "provider request ended without a committed final",
        ),
        RetryPolicy::Tool(Repeatability::CheckBeforeRetry) | RetryPolicy::CheckStateFirst => {
            match external.unwrap_or(ExternalState::Unknown) {
                ExternalState::Applied => (
                    RecoveryProjectionState::Incomplete,
                    ReconciliationAction::AcceptObservedEffect,
                    "external effect exists but its local final is incomplete",
                ),
                ExternalState::NotApplied => (
                    RecoveryProjectionState::Interrupted,
                    ReconciliationAction::RetrySameIdentity,
                    "authoritative state confirms the effect was not applied",
                ),
                ExternalState::Unknown => (
                    RecoveryProjectionState::Waiting,
                    ReconciliationAction::PauseForUser,
                    "external effect is ambiguous and requires user decision",
                ),
            }
        }
        RetryPolicy::Tool(Repeatability::NeverAutomatic) | RetryPolicy::NeverAutomatic => (
            RecoveryProjectionState::Waiting,
            ReconciliationAction::PauseForUser,
            "operation is never retried automatically",
        ),
    }
}

fn boundary_rank(boundary: DurableBoundary) -> u8 {
    match boundary {
        DurableBoundary::Started => 0,
        DurableBoundary::EffectObserved => 1,
        DurableBoundary::FinalCommitted => 2,
    }
}

fn operation_record(
    operation: &DurableOperation,
    revision: Revision,
) -> Result<VersionedRecord, RecoveryError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: operation.id.clone(),
        revision,
        updated_at: operation.updated_at,
        payload: serde_json::to_value(operation)?,
    })
}

fn decode_operation(record: &VersionedRecord) -> Result<DurableOperation, RecoveryError> {
    serde_json::from_value(record.payload.clone()).map_err(RecoveryError::from)
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerRecoveryReport {
    pub generation: Generation,
    pub sessions: BTreeMap<SessionId, RecoveryReport>,
    pub goals: usize,
    pub actions: usize,
    pub children: usize,
    pub kernels: usize,
    pub reconciled_operations: Vec<ReconciliationOutcome>,
}

#[derive(Clone, Debug)]
pub struct WorkerRecoveryRequest {
    pub root_tree_id: RootTreeId,
    pub worker_id: WorkerId,
    pub lease_database: PathBuf,
    pub state_database: PathBuf,
    pub session_store_root: PathBuf,
    pub now: UtcTimestamp,
    pub lease_duration: Duration,
}

/// Acquires a fresh generation, repairs log tails, rebuilds indexes, restores durable state,
/// and reconciles active operations before the worker is made ready.
///
/// # Errors
///
/// Returns an error for lease, manifest, journal, state, or reconciliation failure.
pub fn recover_worker(
    request: &WorkerRecoveryRequest,
    probe: &dyn ExternalStateProbe,
) -> Result<WorkerRecoveryReport, RecoveryError> {
    let lease = LeaseManager::open(&request.lease_database)?.claim_at(
        &request.root_tree_id,
        request.worker_id.clone(),
        request.now,
        request.lease_duration,
    )?;
    let sessions = SessionStore::open(&request.session_store_root)?;
    let mut recovered = BTreeMap::new();
    for manifest in sessions
        .discover()?
        .into_iter()
        .filter(|manifest| manifest.root_tree_id == request.root_tree_id)
    {
        let report = sessions.recover(&manifest.session_id, request.now)?;
        sessions.load_index(&manifest.session_id)?;
        recovered.insert(manifest.session_id, report);
    }
    let state = EmbeddedStore::open(&request.state_database, Some(&FileBackupHook))?;
    let goals = state.list_records(Collection::Goals)?.len();
    let actions = state.list_records(Collection::PendingActions)?.len();
    let children = state.list_records(Collection::Children)?.len();
    let kernels = state.list_records(Collection::KernelMetadata)?.len();
    drop(state);
    let reconciled_operations =
        OperationJournal::open(&request.state_database)?.reconcile(probe, request.now)?;
    Ok(WorkerRecoveryReport {
        generation: lease.generation,
        sessions: recovered,
        goals,
        actions,
        children,
        kernels,
        reconciled_operations,
    })
}

pub(crate) fn recover_daemon_startup(
    data_root: &Path,
) -> Result<(RootCatalog, StartupRecoveryReport), RecoveryError> {
    let now = UtcTimestamp::now().map_err(|error| RecoveryError::Clock(error.to_string()))?;
    let state_path = data_root.join("state.sqlite");
    let store = EmbeddedStore::open(&state_path, Some(&FileBackupHook))?;
    let mut stages = vec![StartupRecoveryStage::StateMigrated];
    let (expired_scheduler_claims, expired_delivery_claims) = expire_stale_claims(&store, now)?;
    stages.push(StartupRecoveryStage::StaleClaimsExpired);
    let catalog = RootCatalog::discover(data_root)?;
    stages.push(StartupRecoveryStage::LazyCatalogRebuilt);
    let schedules = decode_count::<ScheduledJob>(&store, Collection::ScheduledJobs)?;
    stages.push(StartupRecoveryStage::SchedulesRestored);
    let waits = decode_count::<WaitingItem>(&store, Collection::WaitingConditions)?;
    stages.push(StartupRecoveryStage::WaitsRestored);
    let deliveries = decode_count::<DeliveryItem>(&store, Collection::Deliveries)?;
    stages.push(StartupRecoveryStage::DeliveriesRestored);
    let channel_offsets = store.list_records(Collection::ChannelOffsets)?.len();
    stages.push(StartupRecoveryStage::ChannelOffsetsRestored);
    stages.push(StartupRecoveryStage::PublicEndpointsReady);
    Ok((
        catalog,
        StartupRecoveryReport {
            stages,
            expired_scheduler_claims,
            expired_delivery_claims,
            schedules,
            waits,
            deliveries,
            channel_offsets,
        },
    ))
}

fn expire_stale_claims(
    store: &EmbeddedStore,
    now: UtcTimestamp,
) -> Result<(usize, usize), RecoveryError> {
    let mut mutations = Vec::new();
    let mut scheduler = 0;
    for record in store.list_records(Collection::JobAttempts)? {
        let mut attempt: JobAttempt = decode(&record)?;
        if attempt.state == JobAttemptState::Claimed && attempt.claim_expires <= now {
            attempt.state = JobAttemptState::RetryScheduled;
            attempt.retry_at = Some(now);
            attempt.safe_error = Some("expired scheduler claim recovered".into());
            attempt.updated_at = now;
            mutations.push(replace(&record, Collection::JobAttempts, &attempt, now)?);
            scheduler += 1;
        }
    }
    let mut deliveries = 0;
    for record in store.list_records(Collection::Deliveries)? {
        let mut delivery: DeliveryItem = decode(&record)?;
        if delivery.state == DeliveryState::Claimed
            && delivery
                .claim_expires_at
                .is_some_and(|expiry| expiry <= now)
        {
            delivery.state = DeliveryState::RetryScheduled;
            delivery.possible_duplicate |= !delivery.platform_idempotency;
            delivery.safe_error = Some("delivery claim expired before acknowledgement".into());
            delivery.claim_token = None;
            delivery.claim_expires_at = None;
            delivery.not_before = now;
            delivery.updated_at = now;
            delivery.revision = delivery
                .revision
                .checked_next()
                .ok_or(RecoveryError::RevisionOverflow)?;
            mutations.push(replace(&record, Collection::Deliveries, &delivery, now)?);
            deliveries += 1;
        }
    }
    if !mutations.is_empty() {
        store.transact(&mutations)?;
    }
    Ok((scheduler, deliveries))
}

fn decode_count<T>(store: &EmbeddedStore, collection: Collection) -> Result<usize, RecoveryError>
where
    T: for<'de> Deserialize<'de>,
{
    let records = store.list_records(collection)?;
    for record in &records {
        let _: T = decode(record)?;
    }
    Ok(records.len())
}

fn decode<T>(record: &VersionedRecord) -> Result<T, RecoveryError>
where
    T: for<'de> Deserialize<'de>,
{
    serde_json::from_value(record.payload.clone()).map_err(RecoveryError::from)
}

fn replace<T: Serialize>(
    record: &VersionedRecord,
    collection: Collection,
    payload: &T,
    now: UtcTimestamp,
) -> Result<RecordMutation, RecoveryError> {
    let revision = record
        .revision
        .checked_next()
        .ok_or(RecoveryError::RevisionOverflow)?;
    Ok(RecordMutation::Put {
        collection,
        record: VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: record.id.clone(),
            revision,
            updated_at: now,
            payload: serde_json::to_value(payload)?,
        },
        precondition: WritePrecondition::Exact(record.revision),
    })
}

#[derive(Debug, Error)]
pub enum RecoveryError {
    #[error(transparent)]
    Store(#[from] StoreError),
    #[error(transparent)]
    Catalog(#[from] CatalogError),
    #[error(transparent)]
    Session(#[from] SessionStoreError),
    #[error(transparent)]
    Lease(#[from] LeaseError),
    #[error("recovery serialization failed: {0}")]
    Serialization(#[from] serde_json::Error),
    #[error("operation {0} was not found")]
    MissingOperation(EntityId),
    #[error("stable operation identity conflicts: {0}")]
    IdentityConflict(String),
    #[error("invalid durable operation: {0}")]
    InvalidOperation(String),
    #[error("durable operation boundary cannot regress")]
    BoundaryRegression,
    #[error("durable record revision overflow")]
    RevisionOverflow,
    #[error("external state query failed for {identity}: {detail}")]
    Probe { identity: String, detail: String },
    #[error("clock failed: {0}")]
    Clock(String),
}

#[cfg(test)]
mod tests {
    use std::fs::{self, OpenOptions};
    #[cfg(unix)]
    use std::process::{Command, Stdio};
    use std::sync::atomic::{AtomicUsize, Ordering};
    #[cfg(unix)]
    use std::thread;

    use keith_agent_types::{DeliveryId, JobId, ProfileId, WorkspaceId};
    use keith_channel_core::ReplyRoute;
    use keith_delivery::DeliverySource;
    use keith_session_store::{NewSession, SessionKind};
    use tempfile::tempdir;

    use super::*;

    struct FileStateProbe {
        effects: PathBuf,
        queries: AtomicUsize,
    }

    impl FileStateProbe {
        fn new(effects: PathBuf) -> Self {
            Self {
                effects,
                queries: AtomicUsize::new(0),
            }
        }
    }

    impl ExternalStateProbe for FileStateProbe {
        fn query(&self, operation: &DurableOperation) -> Result<ExternalState, String> {
            self.queries.fetch_add(1, Ordering::SeqCst);
            Ok(if self.effects.join(&operation.stable_identity).exists() {
                ExternalState::Applied
            } else {
                ExternalState::NotApplied
            })
        }
    }

    fn record<T: Serialize>(id: EntityId, value: &T, at: UtcTimestamp) -> VersionedRecord {
        VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision: Revision::new(1),
            updated_at: at,
            payload: serde_json::to_value(value).unwrap(),
        }
    }

    #[test]
    fn startup_is_ordered_and_expires_ambiguous_claims_before_endpoints() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let store = EmbeddedStore::open(&path, Some(&FileBackupHook)).unwrap();
        let expired = UtcTimestamp::from_unix_millis(1);
        let attempt = JobAttempt {
            version: CURRENT_SCHEMA_VERSION,
            job_id: JobId::new(),
            attempt_id: EntityId::new(),
            ordinal: 1,
            scheduled_for: UtcTimestamp::UNIX_EPOCH,
            claimed_by: EntityId::new(),
            claim_expires: expired,
            state: JobAttemptState::Claimed,
            action_id: keith_agent_types::ActionId::new(),
            retry_count: 0,
            retry_at: None,
            safe_error: None,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        };
        let delivery = DeliveryItem {
            id: DeliveryId::new(),
            stable_key: "channel-send-1".into(),
            profile_id: ProfileId::new(),
            session_id: SessionId::new(),
            source: DeliverySource::Interactive(EntityId::new()),
            route: ReplyRoute {
                channel: "test".into(),
                external_account: "account".into(),
                conversation: "conversation".into(),
                thread: None,
                reply_to_message: None,
            },
            text: "durable delivery".into(),
            artifacts: Vec::new(),
            state: DeliveryState::Claimed,
            attempt_count: 1,
            not_before: UtcTimestamp::UNIX_EPOCH,
            safe_error: None,
            receipt: None,
            platform_idempotency: false,
            possible_duplicate: false,
            claim_token: Some(EntityId::new()),
            claim_expires_at: Some(expired),
            created_at: UtcTimestamp::UNIX_EPOCH,
            updated_at: UtcTimestamp::UNIX_EPOCH,
            revision: Revision::ZERO,
        };
        let offset_id = EntityId::new();
        store
            .transact(&[
                RecordMutation::Put {
                    collection: Collection::JobAttempts,
                    record: record(attempt.attempt_id.clone(), &attempt, expired),
                    precondition: WritePrecondition::Missing,
                },
                RecordMutation::Put {
                    collection: Collection::Deliveries,
                    record: record(EntityId::new(), &delivery, expired),
                    precondition: WritePrecondition::Missing,
                },
                RecordMutation::Put {
                    collection: Collection::ChannelOffsets,
                    record: record(offset_id, &serde_json::json!({"offset": 7}), expired),
                    precondition: WritePrecondition::Missing,
                },
            ])
            .unwrap();
        drop(store);

        let (_, report) = recover_daemon_startup(directory.path()).unwrap();
        assert_eq!(
            report.stages,
            vec![
                StartupRecoveryStage::StateMigrated,
                StartupRecoveryStage::StaleClaimsExpired,
                StartupRecoveryStage::LazyCatalogRebuilt,
                StartupRecoveryStage::SchedulesRestored,
                StartupRecoveryStage::WaitsRestored,
                StartupRecoveryStage::DeliveriesRestored,
                StartupRecoveryStage::ChannelOffsetsRestored,
                StartupRecoveryStage::PublicEndpointsReady,
            ]
        );
        assert!(report.endpoints_ready());
        assert_eq!(report.expired_scheduler_claims, 1);
        assert_eq!(report.expired_delivery_claims, 1);
        assert_eq!(report.deliveries, 1);
        assert_eq!(report.channel_offsets, 1);
        let recovered = EmbeddedStore::open(&path, Some(&FileBackupHook)).unwrap();
        let claim: JobAttempt =
            decode(&recovered.list_records(Collection::JobAttempts).unwrap()[0]).unwrap();
        assert_eq!(claim.state, JobAttemptState::RetryScheduled);
        let send: DeliveryItem =
            decode(&recovered.list_records(Collection::Deliveries).unwrap()[0]).unwrap();
        assert_eq!(send.state, DeliveryState::RetryScheduled);
        assert!(send.possible_duplicate);
        assert!(send.claim_token.is_none());
    }

    #[test]
    fn corrupt_restored_state_prevents_endpoint_readiness() {
        let directory = tempdir().unwrap();
        let store = EmbeddedStore::open(
            &directory.path().join("state.sqlite"),
            Some(&FileBackupHook),
        )
        .unwrap();
        store
            .transact(&[RecordMutation::Put {
                collection: Collection::WaitingConditions,
                record: record(
                    EntityId::new(),
                    &serde_json::json!({"not": "a waiting item"}),
                    UtcTimestamp::UNIX_EPOCH,
                ),
                precondition: WritePrecondition::Missing,
            }])
            .unwrap();
        drop(store);
        assert!(matches!(
            recover_daemon_startup(directory.path()),
            Err(RecoveryError::Serialization(_))
        ));
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn reconciliation_enforces_every_retry_class_and_truthful_state() {
        let directory = tempdir().unwrap();
        let effects = directory.path().join("effects");
        fs::create_dir(&effects).unwrap();
        let journal = OperationJournal::open(&directory.path().join("state.sqlite")).unwrap();
        let policies = [
            (
                "provider-retry",
                RetryPolicy::Provider {
                    retryable_error: true,
                    action_allows_retry: true,
                },
                ReconciliationAction::RetrySameIdentity,
                RecoveryProjectionState::Interrupted,
            ),
            (
                "provider-stop",
                RetryPolicy::Provider {
                    retryable_error: false,
                    action_allows_retry: true,
                },
                ReconciliationAction::NoAction,
                RecoveryProjectionState::Interrupted,
            ),
            (
                "safe-tool",
                RetryPolicy::Tool(Repeatability::Safe),
                ReconciliationAction::RetrySameIdentity,
                RecoveryProjectionState::Interrupted,
            ),
            (
                "check-tool",
                RetryPolicy::Tool(Repeatability::CheckBeforeRetry),
                ReconciliationAction::RetrySameIdentity,
                RecoveryProjectionState::Interrupted,
            ),
            (
                "never-tool",
                RetryPolicy::Tool(Repeatability::NeverAutomatic),
                ReconciliationAction::PauseForUser,
                RecoveryProjectionState::Waiting,
            ),
        ];
        for (identity, policy, _, _) in policies {
            journal
                .begin(
                    identity,
                    OperationKind::Tool,
                    policy,
                    UtcTimestamp::UNIX_EPOCH,
                )
                .unwrap();
        }
        let applied = journal
            .begin(
                "already-applied",
                OperationKind::Channel,
                RetryPolicy::CheckStateFirst,
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(effects.join(&applied.stable_identity))
            .unwrap();
        journal
            .commit_boundary(
                &applied.id,
                DurableBoundary::EffectObserved,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        let probe = FileStateProbe::new(effects);
        let outcomes = journal
            .reconcile(&probe, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        let by_identity = outcomes
            .into_iter()
            .map(|outcome| (outcome.operation.stable_identity.clone(), outcome))
            .collect::<BTreeMap<_, _>>();
        for (identity, _, action, state) in policies {
            assert_eq!(by_identity[identity].action, action);
            assert_eq!(by_identity[identity].operation.state, state);
        }
        assert_eq!(
            by_identity["already-applied"].action,
            ReconciliationAction::AcceptObservedEffect
        );
        assert_eq!(
            by_identity["already-applied"].operation.state,
            RecoveryProjectionState::Incomplete
        );
        assert_eq!(probe.queries.load(Ordering::SeqCst), 2);
        assert!(journal.operations().unwrap().iter().all(|operation| {
            !matches!(
                operation.state,
                RecoveryProjectionState::Running | RecoveryProjectionState::Recovering
            )
        }));
        let all_states = [
            RecoveryProjectionState::Running,
            RecoveryProjectionState::Waiting,
            RecoveryProjectionState::Recovering,
            RecoveryProjectionState::Interrupted,
            RecoveryProjectionState::Incomplete,
            RecoveryProjectionState::Failed,
            RecoveryProjectionState::Cancelled,
            RecoveryProjectionState::Completed,
        ];
        assert_eq!(all_states.len(), 8);
        assert_eq!(
            serde_json::to_value(all_states).unwrap(),
            serde_json::json!([
                "running",
                "waiting",
                "recovering",
                "interrupted",
                "incomplete",
                "failed",
                "cancelled",
                "completed"
            ])
        );
    }

    #[test]
    fn worker_recovery_advances_generation_repairs_tail_and_rebuilds_index() {
        let directory = tempdir().unwrap();
        let session_root = directory.path().join("session-store");
        let sessions = SessionStore::open(&session_root).unwrap();
        let root = RootTreeId::new();
        let session = SessionId::new();
        sessions
            .create(NewSession {
                kind: SessionKind::Root,
                session_id: session.clone(),
                root_tree_id: root.clone(),
                parent_session_id: None,
                profile_id: ProfileId::new(),
                workspace_id: WorkspaceId::new(),
                created_at: UtcTimestamp::UNIX_EPOCH,
                label: Some("recover me".into()),
                profile_snapshot: None,
            })
            .unwrap();
        let state_database = directory.path().join("state.sqlite");
        let journal = OperationJournal::open(&state_database).unwrap();
        journal
            .begin(
                "provider-turn-1",
                OperationKind::Provider,
                RetryPolicy::Provider {
                    retryable_error: false,
                    action_allows_retry: false,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        drop(journal);
        let request = WorkerRecoveryRequest {
            root_tree_id: root,
            worker_id: WorkerId::new(),
            lease_database: directory.path().join("leases.sqlite"),
            state_database,
            session_store_root: session_root,
            now: UtcTimestamp::from_unix_millis(10),
            lease_duration: Duration::from_millis(5),
        };
        let probe = FileStateProbe::new(directory.path().join("effects"));
        let first = recover_worker(&request, &probe).unwrap();
        assert_eq!(first.generation, Generation::new(1));
        assert_eq!(first.sessions[&session].entries, 0);
        assert_eq!(first.reconciled_operations.len(), 1);
        let mut second_request = request;
        second_request.worker_id = WorkerId::new();
        second_request.now = UtcTimestamp::from_unix_millis(20);
        let second = recover_worker(&second_request, &probe).unwrap();
        assert_eq!(second.generation, Generation::new(2));
        assert_eq!(second.sessions.len(), 1);
    }

    #[cfg(unix)]
    #[test]
    fn process_kills_around_all_durable_boundaries_leave_no_phantom_running_state() {
        let executable = std::env::current_exe().unwrap();
        let kinds = [
            OperationKind::Provider,
            OperationKind::Tool,
            OperationKind::Process,
            OperationKind::Kernel,
            OperationKind::Channel,
            OperationKind::Refinement,
        ];
        let points = [
            "before_started",
            "after_started",
            "before_effect",
            "after_effect",
            "before_final",
            "after_final",
        ];
        for kind in kinds {
            for point in points {
                let directory = tempdir().unwrap();
                let ready = directory.path().join("ready");
                let effects = directory.path().join("effects");
                fs::create_dir(&effects).unwrap();
                let mut child = Command::new(&executable)
                    .args([
                        "--exact",
                        "recovery::tests::recovery_fault_child",
                        "--nocapture",
                    ])
                    .env("KEITH_RECOVERY_CHILD", "1")
                    .env("KEITH_RECOVERY_ROOT", directory.path())
                    .env("KEITH_RECOVERY_KIND", format!("{kind:?}"))
                    .env("KEITH_RECOVERY_POINT", point)
                    .stdin(Stdio::null())
                    .stdout(Stdio::null())
                    .stderr(Stdio::null())
                    .spawn()
                    .unwrap();
                for _ in 0..500 {
                    if ready.exists() {
                        break;
                    }
                    assert!(
                        child.try_wait().unwrap().is_none(),
                        "child exited before {point}"
                    );
                    thread::sleep(Duration::from_millis(2));
                }
                assert!(ready.exists(), "child never reached {kind:?} {point}");
                child.kill().unwrap();
                child.wait().unwrap();

                let journal =
                    OperationJournal::open(&directory.path().join("state.sqlite")).unwrap();
                let probe = FileStateProbe::new(effects.clone());
                let outcomes = journal
                    .reconcile(&probe, UtcTimestamp::from_unix_millis(10))
                    .unwrap();
                let operations = journal.operations().unwrap();
                assert!(operations.iter().all(|operation| {
                    !matches!(
                        operation.state,
                        RecoveryProjectionState::Running | RecoveryProjectionState::Recovering
                    )
                }));
                assert!(fs::read_dir(&effects).unwrap().count() <= 1);
                if matches!(point, "after_effect" | "before_final")
                    && kind != OperationKind::Provider
                {
                    assert_eq!(
                        outcomes[0].action,
                        ReconciliationAction::AcceptObservedEffect
                    );
                    assert_eq!(
                        outcomes[0].operation.state,
                        RecoveryProjectionState::Incomplete
                    );
                }
                if point == "after_final" {
                    assert!(outcomes.is_empty());
                    assert_eq!(operations[0].state, RecoveryProjectionState::Completed);
                }
            }
        }
    }

    #[cfg(unix)]
    #[test]
    fn recovery_fault_child() {
        if std::env::var_os("KEITH_RECOVERY_CHILD").is_none() {
            return;
        }
        let root = PathBuf::from(std::env::var_os("KEITH_RECOVERY_ROOT").unwrap());
        let kind = match std::env::var("KEITH_RECOVERY_KIND").unwrap().as_str() {
            "Provider" => OperationKind::Provider,
            "Tool" => OperationKind::Tool,
            "Process" => OperationKind::Process,
            "Kernel" => OperationKind::Kernel,
            "Channel" => OperationKind::Channel,
            "Refinement" => OperationKind::Refinement,
            value => panic!("unknown operation kind {value}"),
        };
        let point = std::env::var("KEITH_RECOVERY_POINT").unwrap();
        let stop = |name: &str| {
            if point == name {
                fs::write(root.join("ready"), name).unwrap();
                loop {
                    thread::park();
                }
            }
        };
        stop("before_started");
        let journal = OperationJournal::open(&root.join("state.sqlite")).unwrap();
        let policy = if kind == OperationKind::Provider {
            RetryPolicy::Provider {
                retryable_error: true,
                action_allows_retry: true,
            }
        } else {
            RetryPolicy::CheckStateFirst
        };
        let operation = journal
            .begin("stable-operation", kind, policy, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        stop("after_started");
        stop("before_effect");
        OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(root.join("effects").join(&operation.stable_identity))
            .unwrap();
        journal
            .commit_boundary(
                &operation.id,
                DurableBoundary::EffectObserved,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        stop("after_effect");
        stop("before_final");
        journal
            .commit_boundary(
                &operation.id,
                DurableBoundary::FinalCommitted,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        stop("after_final");
    }
}
