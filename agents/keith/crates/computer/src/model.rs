#![allow(clippy::missing_errors_doc, clippy::too_many_lines)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Display;
use std::sync::RwLock;

use keith_agent_types::{
    AuditId, CURRENT_SCHEMA_VERSION, ComputerId, ProfileId, Revision, SchemaVersion, StableKey,
    TakeoverLeaseId, UtcTimestamp, canonical_json_bytes,
};
use keith_state_store_core::{
    Collection, RecordMutation, StateRecordRepository, VersionedRecord,
    WritePrecondition as StorePrecondition,
};
use serde::de::DeserializeOwned;
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const COMPUTER_SCHEMA_VERSION: SchemaVersion = CURRENT_SCHEMA_VERSION;
const MAX_PATH_BYTES: usize = 4_096;
const MAX_SCREEN_BYTES: usize = 128;
const MAX_TASK_KEY_BYTES: usize = 256;
const MAX_AUDIT_TEXT_BYTES: usize = 2_048;
const MAX_ORIGIN_BYTES: usize = 2_048;
const TOKEN_DIGEST_HEX_BYTES: usize = 64;

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum ComputerError {
    #[error("unsupported computer schema version {0}")]
    UnsupportedVersion(SchemaVersion),
    #[error("malformed computer record: {0}")]
    Malformed(&'static str),
    #[error("computer already exists for profile {0}")]
    DuplicateComputer(ProfileId),
    #[error("computer is missing for profile {0}")]
    MissingComputer(ProfileId),
    #[error("takeover lease already exists for profile {0}")]
    DuplicateLease(ProfileId),
    #[error("takeover lease is missing for profile {0}")]
    MissingLease(ProfileId),
    #[error("expected revision {expected:?}, found {actual:?}")]
    RevisionConflict {
        expected: Revision,
        actual: Revision,
    },
    #[error("audit sequence or stable key conflicts with existing history")]
    AuditConflict,
    #[error("computer repository lock was poisoned")]
    LockPoisoned,
    #[error("computer record encoding failed: {0}")]
    Encoding(String),
    #[error("computer revision overflowed")]
    RevisionOverflow,
    #[error("new computer provisioning cannot be safely rolled back: {0}")]
    UnsafeProvisionRollback(&'static str),
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerState {
    Provisioning,
    Ready,
    Quarantined,
    Disabled,
    Tombstoned,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ControlState {
    Idle,
    Agent,
    UserTakeover,
    Paused,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerRecord {
    pub version: SchemaVersion,
    pub computer_id: ComputerId,
    pub owner_profile_id: ProfileId,
    pub browser_profile_root: String,
    pub screen_key: StableKey,
    pub state: ComputerState,
    pub control_state: ControlState,
    pub current_task_key: Option<StableKey>,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub revision: Revision,
}

impl ComputerRecord {
    pub fn stable_key(&self) -> String {
        format!("computer:{}", self.owner_profile_id)
    }

    pub fn validate(&self) -> Result<(), ComputerError> {
        version(self.version)?;
        bounded_nonempty(
            &self.browser_profile_root,
            MAX_PATH_BYTES,
            "browser profile root",
        )?;
        if self.screen_key.as_str().len() > MAX_SCREEN_BYTES {
            return Err(ComputerError::Malformed("screen key"));
        }
        if self.browser_profile_root.contains('\0') {
            return Err(ComputerError::Malformed(
                "computer paths may not contain NUL",
            ));
        }
        if self
            .current_task_key
            .as_ref()
            .is_some_and(|task| task.as_str().len() > MAX_TASK_KEY_BYTES)
        {
            return Err(ComputerError::Malformed("current task key"));
        }
        if self.created_at > self.updated_at {
            return Err(ComputerError::Malformed(
                "created timestamp follows updated timestamp",
            ));
        }
        if matches!(
            self.state,
            ComputerState::Disabled | ComputerState::Tombstoned
        ) && (self.control_state != ControlState::Idle || self.current_task_key.is_some())
        {
            return Err(ComputerError::Malformed(
                "inactive computer retains active control or task",
            ));
        }
        Ok(())
    }

    pub fn to_canonical_json(&self) -> Result<Vec<u8>, ComputerError> {
        canonical(self)
    }

    pub fn from_canonical_json(bytes: &[u8]) -> Result<Self, ComputerError> {
        decode(bytes, Self::validate)
    }
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum TakeoverState {
    Active,
    HandedBack,
    Expired,
    Revoked,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct TakeoverLease {
    pub version: SchemaVersion,
    pub takeover_lease_id: TakeoverLeaseId,
    pub computer_id: ComputerId,
    pub owner_profile_id: ProfileId,
    pub task_key: StableKey,
    pub token_digest_hex: String,
    pub fencing_token: u64,
    pub acquired_at: UtcTimestamp,
    pub renewed_at: UtcTimestamp,
    pub expires_at: UtcTimestamp,
    pub state: TakeoverState,
    pub revision: Revision,
}

impl TakeoverLease {
    pub fn stable_key(&self) -> String {
        format!("takeover:{}", self.owner_profile_id)
    }

    pub fn validate(&self) -> Result<(), ComputerError> {
        version(self.version)?;
        if self.task_key.as_str().len() > MAX_TASK_KEY_BYTES {
            return Err(ComputerError::Malformed("takeover task key"));
        }
        if self.fencing_token == 0 {
            return Err(ComputerError::Malformed("fencing token must be nonzero"));
        }
        if self.token_digest_hex.len() != TOKEN_DIGEST_HEX_BYTES
            || !self
                .token_digest_hex
                .bytes()
                .all(|byte| byte.is_ascii_digit() || matches!(byte, b'a'..=b'f'))
        {
            return Err(ComputerError::Malformed(
                "takeover token digest must be 64 hexadecimal bytes",
            ));
        }
        if self.acquired_at > self.renewed_at || self.renewed_at > self.expires_at {
            return Err(ComputerError::Malformed(
                "takeover lease timestamps are not ordered",
            ));
        }
        if matches!(self.state, TakeoverState::Active) && self.renewed_at == self.expires_at {
            return Err(ComputerError::Malformed(
                "active takeover lease must have remaining duration",
            ));
        }
        Ok(())
    }

    pub fn to_canonical_json(&self) -> Result<Vec<u8>, ComputerError> {
        canonical(self)
    }

    pub fn from_canonical_json(bytes: &[u8]) -> Result<Self, ComputerError> {
        decode(bytes, Self::validate)
    }
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(
    rename_all = "snake_case",
    tag = "kind",
    content = "profile_id",
    deny_unknown_fields
)]
pub enum AuditActor {
    Owner,
    Profile(ProfileId),
    System,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerAuditKind {
    Provisioned,
    Reconciled,
    Disabled,
    Archived,
    Duplicated,
    Deleted,
    LeaseRevoked,
    StateChanged,
    TaskChanged,
    TakeoverAcquired,
    TakeoverRenewed,
    TakeoverHandedBack,
    TakeoverExpired,
    PolicyDecision,
    Recovery,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ControlTransition {
    pub from: ControlState,
    pub to: ControlState,
}

#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "snake_case")]
pub enum PolicyDecision {
    Allowed,
    Denied,
    ApprovalRequired,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct TakeoverTransferMetadata {
    pub lease_id: TakeoverLeaseId,
    pub fencing_token: u64,
    pub from: ControlState,
    pub to: ControlState,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerAudit {
    pub version: SchemaVersion,
    pub audit_id: AuditId,
    pub computer_id: ComputerId,
    pub owner_profile_id: ProfileId,
    pub sequence: u64,
    pub stable_key: StableKey,
    pub actor: AuditActor,
    pub kind: ComputerAuditKind,
    pub task_key: Option<StableKey>,
    pub navigation_origin: Option<String>,
    pub control_transition: Option<ControlTransition>,
    pub policy_decision: Option<PolicyDecision>,
    pub side_effect_summary: Option<String>,
    pub transfer: Option<TakeoverTransferMetadata>,
    pub safe_failure: Option<String>,
    pub recovery_correlation: Option<StableKey>,
    pub safe_summary: String,
    pub occurred_at: UtcTimestamp,
    pub computer_revision: Revision,
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerProvisionRollback {
    pub version: SchemaVersion,
    pub rollback_id: AuditId,
    pub owner_profile_id: ProfileId,
    pub removed_computer_id: ComputerId,
    pub removed_browser_profile_root: String,
    pub removed_screen_key: StableKey,
    pub provision_operation_key: StableKey,
    pub rollback_operation_key: StableKey,
    pub rolled_back_at: UtcTimestamp,
}

impl ComputerProvisionRollback {
    pub fn validate(&self) -> Result<(), ComputerError> {
        version(self.version)?;
        bounded_nonempty(
            &self.removed_browser_profile_root,
            MAX_PATH_BYTES,
            "rollback browser profile root",
        )?;
        if self.removed_screen_key.as_str().len() > MAX_SCREEN_BYTES
            || self.provision_operation_key == self.rollback_operation_key
        {
            return Err(ComputerError::Malformed("provision rollback identity"));
        }
        Ok(())
    }
}

impl ComputerAudit {
    pub fn validate(&self) -> Result<(), ComputerError> {
        version(self.version)?;
        if self.sequence == 0 {
            return Err(ComputerError::Malformed("audit sequence must be nonzero"));
        }
        if self.stable_key.as_str().len() > MAX_TASK_KEY_BYTES {
            return Err(ComputerError::Malformed("audit stable key"));
        }
        bounded(&self.safe_summary, MAX_AUDIT_TEXT_BYTES, "audit summary")?;
        if self
            .task_key
            .as_ref()
            .is_some_and(|task| task.as_str().len() > MAX_TASK_KEY_BYTES)
        {
            return Err(ComputerError::Malformed("audit task key"));
        }
        if self.navigation_origin.as_deref().is_some_and(|origin| {
            !bounded_safe(origin, MAX_ORIGIN_BYTES) || !origin.contains("://")
        }) {
            return Err(ComputerError::Malformed("audit navigation origin"));
        }
        for value in [&self.side_effect_summary, &self.safe_failure]
            .into_iter()
            .flatten()
        {
            if !bounded_safe(value, MAX_AUDIT_TEXT_BYTES) {
                return Err(ComputerError::Malformed("audit detail"));
            }
        }
        if self.transfer.as_ref().is_some_and(|transfer| {
            transfer.fencing_token == 0
                || (transfer.from == transfer.to && self.kind != ComputerAuditKind::TakeoverRenewed)
                || (self.kind == ComputerAuditKind::TakeoverRenewed
                    && (transfer.from != ControlState::UserTakeover
                        || transfer.to != ControlState::UserTakeover))
        }) {
            return Err(ComputerError::Malformed("audit takeover transfer"));
        }
        let takeover = matches!(
            self.kind,
            ComputerAuditKind::TakeoverAcquired
                | ComputerAuditKind::TakeoverRenewed
                | ComputerAuditKind::TakeoverHandedBack
                | ComputerAuditKind::TakeoverExpired
        );
        if takeover != self.transfer.is_some() {
            return Err(ComputerError::Malformed("audit takeover metadata"));
        }
        if matches!(self.kind, ComputerAuditKind::PolicyDecision) != self.policy_decision.is_some()
        {
            return Err(ComputerError::Malformed("audit policy decision"));
        }
        if matches!(self.kind, ComputerAuditKind::Recovery) != self.recovery_correlation.is_some() {
            return Err(ComputerError::Malformed("audit recovery correlation"));
        }
        Ok(())
    }

    pub fn to_canonical_json(&self) -> Result<Vec<u8>, ComputerError> {
        canonical(self)
    }

    pub fn from_canonical_json(bytes: &[u8]) -> Result<Self, ComputerError> {
        decode(bytes, Self::validate)
    }
}

#[derive(Clone, Debug)]
pub enum ComputerRepositoryBatch {
    InsertComputer(ComputerRecord),
    ReplaceComputer {
        expected_revision: Revision,
        record: ComputerRecord,
    },
    PutLease {
        expected_revision: Option<Revision>,
        lease: TakeoverLease,
    },
    RemoveLease {
        owner_profile_id: ProfileId,
        expected_revision: Revision,
    },
    AppendAudit(ComputerAudit),
    RollbackNewProvision(ComputerProvisionRollback),
}

pub trait ComputerRepository: Send + Sync {
    fn computer(&self, owner: &ProfileId) -> Result<Option<ComputerRecord>, ComputerError>;
    fn lease(&self, owner: &ProfileId) -> Result<Option<TakeoverLease>, ComputerError>;
    fn audit(&self, owner: &ProfileId) -> Result<Vec<ComputerAudit>, ComputerError>;
    fn provision_rollback(
        &self,
        owner: &ProfileId,
    ) -> Result<Option<ComputerProvisionRollback>, ComputerError>;
    fn transact(&self, changes: &[ComputerRepositoryBatch]) -> Result<(), ComputerError>;
}

#[derive(Clone, Debug, Default)]
struct RepositoryState {
    computers: BTreeMap<ProfileId, Vec<u8>>,
    computer_ids: BTreeSet<ComputerId>,
    leases: BTreeMap<ProfileId, Vec<u8>>,
    lease_tombstones: BTreeMap<ProfileId, LeaseTombstone>,
    lease_ids: BTreeSet<TakeoverLeaseId>,
    audits: BTreeMap<ProfileId, Vec<Vec<u8>>>,
    audit_ids: BTreeSet<AuditId>,
    audit_keys: BTreeSet<String>,
    provision_rollbacks: BTreeMap<ProfileId, ComputerProvisionRollback>,
}

#[derive(Debug, Default)]
pub struct InMemoryComputerRepository {
    state: RwLock<RepositoryState>,
}

pub struct DurableComputerRepository<R> {
    repository: R,
}

impl<R> DurableComputerRepository<R> {
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    pub const fn repository(&self) -> &R {
        &self.repository
    }
}

impl<R> DurableComputerRepository<R>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    /// Validates a computer-domain batch against current durable state and returns the exact
    /// store mutations without committing them, so a composition root can include them in a
    /// larger atomic transaction.
    pub fn plan_mutations(
        &self,
        changes: &[ComputerRepositoryBatch],
    ) -> Result<Vec<RecordMutation>, ComputerError> {
        let mut candidate = load_durable_state(&self.repository)?;
        let mut mutations = Vec::new();
        for change in changes {
            let before = candidate.clone();
            candidate.apply(change.clone())?;
            mutations.extend(durable_mutations(change, &before, &candidate)?);
        }
        candidate.validate_all()?;
        Ok(mutations)
    }
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(
    rename_all = "snake_case",
    tag = "record",
    content = "value",
    deny_unknown_fields
)]
enum StoredTakeoverLease {
    Active(TakeoverLease),
    Tombstone {
        owner_profile_id: ProfileId,
        takeover_lease_id: TakeoverLeaseId,
        revision: Revision,
        fencing_token: u64,
    },
}

#[derive(Clone, Debug, Deserialize, Serialize)]
#[serde(untagged)]
enum StoredComputerAudit {
    Event(ComputerAudit),
    ProvisionRollback(ComputerProvisionRollback),
}

#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
struct LeaseTombstone {
    takeover_lease_id: TakeoverLeaseId,
    revision: Revision,
    fencing_token: u64,
}

#[cfg(test)]
impl InMemoryComputerRepository {
    fn corrupt_computer(&self, owner: &ProfileId, bytes: Vec<u8>) {
        self.state
            .write()
            .unwrap()
            .computers
            .insert(owner.clone(), bytes);
    }
}

impl ComputerRepository for InMemoryComputerRepository {
    fn computer(&self, owner: &ProfileId) -> Result<Option<ComputerRecord>, ComputerError> {
        self.state
            .read()
            .map_err(|_| ComputerError::LockPoisoned)?
            .computers
            .get(owner)
            .map(|bytes| decode(bytes, ComputerRecord::validate))
            .transpose()
    }

    fn lease(&self, owner: &ProfileId) -> Result<Option<TakeoverLease>, ComputerError> {
        self.state
            .read()
            .map_err(|_| ComputerError::LockPoisoned)?
            .leases
            .get(owner)
            .map(|bytes| decode(bytes, TakeoverLease::validate))
            .transpose()
    }

    fn audit(&self, owner: &ProfileId) -> Result<Vec<ComputerAudit>, ComputerError> {
        self.state
            .read()
            .map_err(|_| ComputerError::LockPoisoned)?
            .audits
            .get(owner)
            .into_iter()
            .flatten()
            .map(|bytes| decode(bytes, ComputerAudit::validate))
            .collect()
    }

    fn provision_rollback(
        &self,
        owner: &ProfileId,
    ) -> Result<Option<ComputerProvisionRollback>, ComputerError> {
        Ok(self
            .state
            .read()
            .map_err(|_| ComputerError::LockPoisoned)?
            .provision_rollbacks
            .get(owner)
            .cloned())
    }

    fn transact(&self, changes: &[ComputerRepositoryBatch]) -> Result<(), ComputerError> {
        let mut state = self
            .state
            .write()
            .map_err(|_| ComputerError::LockPoisoned)?;
        let mut candidate = state.clone();
        for change in changes {
            candidate.apply(change.clone())?;
        }
        candidate.validate_all()?;
        *state = candidate;
        Ok(())
    }
}

impl<R> ComputerRepository for DurableComputerRepository<R>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    fn computer(&self, owner: &ProfileId) -> Result<Option<ComputerRecord>, ComputerError> {
        let state = load_durable_state(&self.repository)?;
        state
            .computers
            .get(owner)
            .map(|bytes| decode(bytes, ComputerRecord::validate))
            .transpose()
    }

    fn lease(&self, owner: &ProfileId) -> Result<Option<TakeoverLease>, ComputerError> {
        let state = load_durable_state(&self.repository)?;
        state
            .leases
            .get(owner)
            .map(|bytes| decode(bytes, TakeoverLease::validate))
            .transpose()
    }

    fn audit(&self, owner: &ProfileId) -> Result<Vec<ComputerAudit>, ComputerError> {
        let state = load_durable_state(&self.repository)?;
        state
            .audits
            .get(owner)
            .into_iter()
            .flatten()
            .map(|bytes| decode(bytes, ComputerAudit::validate))
            .collect()
    }

    fn provision_rollback(
        &self,
        owner: &ProfileId,
    ) -> Result<Option<ComputerProvisionRollback>, ComputerError> {
        Ok(load_durable_state(&self.repository)?
            .provision_rollbacks
            .get(owner)
            .cloned())
    }

    fn transact(&self, changes: &[ComputerRepositoryBatch]) -> Result<(), ComputerError> {
        let mut candidate = load_durable_state(&self.repository)?;
        let mut mutations = Vec::with_capacity(changes.len());
        for change in changes {
            let before_change = candidate.clone();
            candidate.apply(change.clone())?;
            mutations.extend(durable_mutations(change, &before_change, &candidate)?);
        }
        candidate.validate_all()?;
        self.repository
            .transact(&mutations)
            .map_err(repository_error)?;
        Ok(())
    }
}

fn durable_mutations(
    change: &ComputerRepositoryBatch,
    before: &RepositoryState,
    after: &RepositoryState,
) -> Result<Vec<RecordMutation>, ComputerError> {
    match change {
        ComputerRepositoryBatch::InsertComputer(record) => Ok(vec![RecordMutation::Put {
            collection: Collection::ComputerRecords,
            record: durable_record(
                record.computer_id.as_entity_id().clone(),
                record.revision,
                record.updated_at,
                record,
            )?,
            precondition: StorePrecondition::Missing,
        }]),
        ComputerRepositoryBatch::ReplaceComputer {
            expected_revision,
            record,
        } => Ok(vec![RecordMutation::Put {
            collection: Collection::ComputerRecords,
            record: durable_record(
                record.computer_id.as_entity_id().clone(),
                record.revision,
                record.updated_at,
                record,
            )?,
            precondition: StorePrecondition::Exact(*expected_revision),
        }]),
        ComputerRepositoryBatch::PutLease {
            expected_revision,
            lease,
        } => Ok(vec![RecordMutation::Put {
            collection: Collection::TakeoverLeases,
            record: durable_record(
                lease.takeover_lease_id.as_entity_id().clone(),
                lease.revision,
                lease.renewed_at,
                &StoredTakeoverLease::Active(lease.clone()),
            )?,
            precondition: expected_revision
                .map_or(StorePrecondition::Missing, StorePrecondition::Exact),
        }]),
        ComputerRepositoryBatch::RemoveLease {
            owner_profile_id,
            expected_revision,
        } => {
            let lease: TakeoverLease = decode(
                before
                    .leases
                    .get(owner_profile_id)
                    .ok_or_else(|| ComputerError::MissingLease(owner_profile_id.clone()))?,
                TakeoverLease::validate,
            )?;
            let revision = next_revision(*expected_revision)?;
            let tombstone = StoredTakeoverLease::Tombstone {
                owner_profile_id: owner_profile_id.clone(),
                takeover_lease_id: lease.takeover_lease_id.clone(),
                revision,
                fencing_token: lease.fencing_token,
            };
            Ok(vec![RecordMutation::Put {
                collection: Collection::TakeoverLeases,
                record: durable_record(
                    lease.takeover_lease_id.as_entity_id().clone(),
                    revision,
                    lease.expires_at,
                    &tombstone,
                )?,
                precondition: StorePrecondition::Exact(*expected_revision),
            }])
        }
        ComputerRepositoryBatch::AppendAudit(event) => {
            let stored = after
                .audits
                .get(&event.owner_profile_id)
                .and_then(|events| events.last())
                .ok_or(ComputerError::AuditConflict)?;
            let event: ComputerAudit = decode(stored, ComputerAudit::validate)?;
            Ok(vec![RecordMutation::Put {
                collection: Collection::ComputerAudits,
                record: durable_record(
                    event.audit_id.as_entity_id().clone(),
                    event.computer_revision,
                    event.occurred_at,
                    &StoredComputerAudit::Event(event),
                )?,
                precondition: StorePrecondition::Missing,
            }])
        }
        ComputerRepositoryBatch::RollbackNewProvision(rollback) => {
            let audit = before
                .audits
                .get(&rollback.owner_profile_id)
                .and_then(|events| events.first())
                .ok_or(ComputerError::AuditConflict)
                .and_then(|bytes| decode(bytes, ComputerAudit::validate))?;
            Ok(vec![
                RecordMutation::Delete {
                    collection: Collection::ComputerRecords,
                    id: rollback.removed_computer_id.as_entity_id().clone(),
                    precondition: StorePrecondition::Exact(Revision::ZERO),
                },
                RecordMutation::Delete {
                    collection: Collection::ComputerAudits,
                    id: audit.audit_id.as_entity_id().clone(),
                    precondition: StorePrecondition::Exact(Revision::ZERO),
                },
                RecordMutation::Put {
                    collection: Collection::ComputerAudits,
                    record: durable_record(
                        rollback.rollback_id.as_entity_id().clone(),
                        Revision::ZERO,
                        rollback.rolled_back_at,
                        &StoredComputerAudit::ProvisionRollback(rollback.clone()),
                    )?,
                    precondition: StorePrecondition::Missing,
                },
            ])
        }
    }
}

fn durable_record<T: Serialize>(
    id: keith_agent_types::EntityId,
    revision: Revision,
    updated_at: UtcTimestamp,
    value: &T,
) -> Result<VersionedRecord, ComputerError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id,
        revision,
        updated_at,
        payload: serde_json::to_value(value)
            .map_err(|error| ComputerError::Encoding(error.to_string()))?,
    })
}

fn payload<T: DeserializeOwned + Serialize>(value: &serde_json::Value) -> Result<T, ComputerError> {
    let bytes =
        canonical_json_bytes(&value).map_err(|error| ComputerError::Encoding(error.to_string()))?;
    serde_json::from_slice(&bytes).map_err(|error| ComputerError::Encoding(error.to_string()))
}

fn repository_error(error: impl Display) -> ComputerError {
    ComputerError::Encoding(error.to_string())
}

fn load_durable_state<R>(repository: &R) -> Result<RepositoryState, ComputerError>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    let mut state = RepositoryState::default();
    for record in repository
        .list_records(Collection::ComputerRecords)
        .map_err(repository_error)?
    {
        let value: ComputerRecord = payload(&record.payload)?;
        value.validate()?;
        if value.computer_id.as_entity_id() != &record.id || value.revision != record.revision {
            return Err(ComputerError::Malformed("computer envelope mismatch"));
        }
        let owner = value.owner_profile_id.clone();
        if !state.computer_ids.insert(value.computer_id.clone())
            || state.computers.insert(owner, canonical(&value)?).is_some()
        {
            return Err(ComputerError::Malformed("duplicate durable computer"));
        }
    }
    for record in repository
        .list_records(Collection::TakeoverLeases)
        .map_err(repository_error)?
    {
        let stored: StoredTakeoverLease = payload(&record.payload)?;
        match stored {
            StoredTakeoverLease::Active(value) => {
                value.validate()?;
                if value.takeover_lease_id.as_entity_id() != &record.id
                    || value.revision != record.revision
                    || !state.lease_ids.insert(value.takeover_lease_id.clone())
                    || state
                        .leases
                        .insert(value.owner_profile_id.clone(), canonical(&value)?)
                        .is_some()
                {
                    return Err(ComputerError::Malformed("duplicate durable takeover lease"));
                }
            }
            StoredTakeoverLease::Tombstone {
                owner_profile_id,
                takeover_lease_id,
                revision,
                fencing_token,
            } => {
                if takeover_lease_id.as_entity_id() != &record.id
                    || revision != record.revision
                    || fencing_token == 0
                    || !state.lease_ids.insert(takeover_lease_id)
                    || state
                        .lease_tombstones
                        .insert(
                            owner_profile_id,
                            LeaseTombstone {
                                takeover_lease_id: TakeoverLeaseId::from(record.id.clone()),
                                revision,
                                fencing_token,
                            },
                        )
                        .is_some()
                {
                    return Err(ComputerError::Malformed("invalid takeover tombstone"));
                }
            }
        }
    }
    for record in repository
        .list_records(Collection::ComputerAudits)
        .map_err(repository_error)?
    {
        match payload::<StoredComputerAudit>(&record.payload)? {
            StoredComputerAudit::Event(value) => {
                value.validate()?;
                if value.audit_id.as_entity_id() != &record.id
                    || value.computer_revision != record.revision
                    || !state.audit_ids.insert(value.audit_id.clone())
                    || !state.audit_keys.insert(value.stable_key.to_string())
                {
                    return Err(ComputerError::AuditConflict);
                }
                state
                    .audits
                    .entry(value.owner_profile_id.clone())
                    .or_default()
                    .push(canonical(&value)?);
            }
            StoredComputerAudit::ProvisionRollback(value) => {
                value.validate()?;
                if value.rollback_id.as_entity_id() != &record.id
                    || record.revision != Revision::ZERO
                    || state
                        .provision_rollbacks
                        .insert(value.owner_profile_id.clone(), value)
                        .is_some()
                {
                    return Err(ComputerError::AuditConflict);
                }
            }
        }
    }
    for events in state.audits.values_mut() {
        events.sort_by_key(|bytes| {
            decode::<ComputerAudit>(bytes, ComputerAudit::validate)
                .map(|event| event.sequence)
                .unwrap_or(u64::MAX)
        });
    }
    state.validate_all()?;
    Ok(state)
}

impl RepositoryState {
    fn validate_all(&self) -> Result<(), ComputerError> {
        for record in self.computers.values() {
            let _: ComputerRecord = decode(record, ComputerRecord::validate)?;
        }
        for lease in self.leases.values() {
            let lease: TakeoverLease = decode(lease, TakeoverLease::validate)?;
            let computer = self
                .computers
                .get(&lease.owner_profile_id)
                .ok_or_else(|| ComputerError::MissingComputer(lease.owner_profile_id.clone()))?;
            let computer: ComputerRecord = decode(computer, ComputerRecord::validate)?;
            if lease.computer_id != computer.computer_id {
                return Err(ComputerError::Malformed(
                    "takeover lease computer does not match owner",
                ));
            }
        }
        for (owner, events) in &self.audits {
            for (index, event) in events.iter().enumerate() {
                let event: ComputerAudit = decode(event, ComputerAudit::validate)?;
                let expected = u64::try_from(index)
                    .map_err(|_| ComputerError::AuditConflict)?
                    .checked_add(1)
                    .ok_or(ComputerError::AuditConflict)?;
                let computer = self
                    .computers
                    .get(owner)
                    .ok_or(ComputerError::AuditConflict)?;
                let computer: ComputerRecord = decode(computer, ComputerRecord::validate)?;
                if event.sequence != expected
                    || event.owner_profile_id != *owner
                    || event.computer_id != computer.computer_id
                {
                    return Err(ComputerError::AuditConflict);
                }
            }
        }
        Ok(())
    }

    fn apply(&mut self, change: ComputerRepositoryBatch) -> Result<(), ComputerError> {
        match change {
            ComputerRepositoryBatch::InsertComputer(record) => {
                record.validate()?;
                if record.revision != Revision::ZERO {
                    return Err(ComputerError::Malformed(
                        "new computer revision must be zero",
                    ));
                }
                let owner = record.owner_profile_id.clone();
                if self
                    .provision_rollbacks
                    .get(&owner)
                    .is_some_and(|rollback| {
                        rollback.removed_computer_id == record.computer_id
                            || rollback.removed_browser_profile_root == record.browser_profile_root
                            || rollback.removed_screen_key == record.screen_key
                    })
                {
                    return Err(ComputerError::UnsafeProvisionRollback(
                        "retry reused rolled-back identity or resources",
                    ));
                }
                if !self.computer_ids.insert(record.computer_id.clone()) {
                    return Err(ComputerError::DuplicateComputer(owner));
                }
                if self
                    .computers
                    .insert(owner.clone(), canonical(&record)?)
                    .is_some()
                {
                    return Err(ComputerError::DuplicateComputer(owner));
                }
            }
            ComputerRepositoryBatch::ReplaceComputer {
                expected_revision,
                record,
            } => {
                record.validate()?;
                let owner = record.owner_profile_id.clone();
                let current: ComputerRecord = decode(
                    self.computers
                        .get(&owner)
                        .ok_or_else(|| ComputerError::MissingComputer(owner.clone()))?,
                    ComputerRecord::validate,
                )?;
                check_revision(expected_revision, current.revision)?;
                if record.computer_id != current.computer_id {
                    return Err(ComputerError::Malformed("computer identity is immutable"));
                }
                if record.revision != next_revision(expected_revision)? {
                    return Err(ComputerError::Malformed(
                        "replacement revision must increment by one",
                    ));
                }
                self.computers.insert(owner, canonical(&record)?);
            }
            ComputerRepositoryBatch::PutLease {
                expected_revision,
                lease,
            } => {
                lease.validate()?;
                let owner = lease.owner_profile_id.clone();
                let computer: ComputerRecord = decode(
                    self.computers
                        .get(&owner)
                        .ok_or_else(|| ComputerError::MissingComputer(owner.clone()))?,
                    ComputerRecord::validate,
                )?;
                if lease.computer_id != computer.computer_id {
                    return Err(ComputerError::Malformed(
                        "takeover lease computer does not match owner",
                    ));
                }
                if computer.state != ComputerState::Ready {
                    return Err(ComputerError::Malformed(
                        "takeover lease requires a ready computer",
                    ));
                }
                match (self.leases.get(&owner), expected_revision) {
                    (None, None) => {
                        if self.lease_tombstones.get(&owner).is_some_and(|tombstone| {
                            tombstone.takeover_lease_id == lease.takeover_lease_id
                        }) {
                            return Err(ComputerError::DuplicateLease(owner));
                        }
                        let (required_revision, required_fence) = self
                            .lease_tombstones
                            .get(&owner)
                            .map_or(Ok((Revision::ZERO, 1)), |tombstone| {
                                Ok((
                                    next_revision(tombstone.revision)?,
                                    next_fence(tombstone.fencing_token)?,
                                ))
                            })?;
                        if lease.revision != required_revision
                            || lease.fencing_token != required_fence
                        {
                            return Err(ComputerError::Malformed(
                                "new lease must advance the retained revision and fence",
                            ));
                        }
                        if !self.lease_ids.insert(lease.takeover_lease_id.clone()) {
                            return Err(ComputerError::DuplicateLease(owner));
                        }
                        self.leases.insert(owner, canonical(&lease)?);
                    }
                    (Some(current_bytes), Some(expected)) => {
                        let current: TakeoverLease =
                            decode(current_bytes, TakeoverLease::validate)?;
                        check_revision(expected, current.revision)?;
                        if lease.takeover_lease_id != current.takeover_lease_id {
                            return Err(ComputerError::Malformed(
                                "takeover lease identity is immutable",
                            ));
                        }
                        if lease.revision != next_revision(expected)? {
                            return Err(ComputerError::Malformed(
                                "lease revision must increment by one",
                            ));
                        }
                        if lease.fencing_token != next_fence(current.fencing_token)?
                            || lease.token_digest_hex == current.token_digest_hex
                            || lease.acquired_at != current.acquired_at
                            || lease.renewed_at < current.renewed_at
                            || lease.expires_at <= lease.renewed_at
                            || lease.task_key != current.task_key
                        {
                            return Err(ComputerError::Malformed(
                                "lease renewal must rotate token and advance its fence",
                            ));
                        }
                        self.leases.insert(owner, canonical(&lease)?);
                    }
                    (Some(_), None) => return Err(ComputerError::DuplicateLease(owner)),
                    (None, Some(_)) => return Err(ComputerError::MissingLease(owner)),
                }
            }
            ComputerRepositoryBatch::RemoveLease {
                owner_profile_id,
                expected_revision,
            } => {
                let current: TakeoverLease = decode(
                    self.leases
                        .get(&owner_profile_id)
                        .ok_or_else(|| ComputerError::MissingLease(owner_profile_id.clone()))?,
                    TakeoverLease::validate,
                )?;
                check_revision(expected_revision, current.revision)?;
                self.leases.remove(&owner_profile_id);
                self.lease_tombstones.insert(
                    owner_profile_id,
                    LeaseTombstone {
                        takeover_lease_id: current.takeover_lease_id,
                        revision: next_revision(current.revision)?,
                        fencing_token: current.fencing_token,
                    },
                );
            }
            ComputerRepositoryBatch::AppendAudit(event) => {
                event.validate()?;
                let computer = self
                    .computers
                    .get(&event.owner_profile_id)
                    .map(|bytes| decode(bytes, ComputerRecord::validate))
                    .transpose()?;
                if computer.is_none_or(|record| record.computer_id != event.computer_id)
                    || !self.audit_ids.insert(event.audit_id.clone())
                    || !self.audit_keys.insert(event.stable_key.to_string())
                {
                    return Err(ComputerError::AuditConflict);
                }
                let history = self
                    .audits
                    .entry(event.owner_profile_id.clone())
                    .or_default();
                let expected = u64::try_from(history.len())
                    .unwrap_or(u64::MAX)
                    .saturating_add(1);
                if event.sequence != expected {
                    return Err(ComputerError::AuditConflict);
                }
                history.push(canonical(&event)?);
            }
            ComputerRepositoryBatch::RollbackNewProvision(rollback) => {
                rollback.validate()?;
                if self
                    .provision_rollbacks
                    .contains_key(&rollback.owner_profile_id)
                {
                    return Err(ComputerError::UnsafeProvisionRollback(
                        "profile already has a rollback sentinel",
                    ));
                }
                let current: ComputerRecord = decode(
                    self.computers
                        .get(&rollback.owner_profile_id)
                        .ok_or_else(|| {
                            ComputerError::MissingComputer(rollback.owner_profile_id.clone())
                        })?,
                    ComputerRecord::validate,
                )?;
                let history = self.audits.get(&rollback.owner_profile_id).ok_or(
                    ComputerError::UnsafeProvisionRollback("provision audit is missing"),
                )?;
                if current.revision != Revision::ZERO
                    || current.computer_id != rollback.removed_computer_id
                    || current.browser_profile_root != rollback.removed_browser_profile_root
                    || current.screen_key != rollback.removed_screen_key
                    || self.leases.contains_key(&rollback.owner_profile_id)
                    || history.len() != 1
                {
                    return Err(ComputerError::UnsafeProvisionRollback(
                        "computer was used or changed after provisioning",
                    ));
                }
                let provision: ComputerAudit = decode(&history[0], ComputerAudit::validate)?;
                if provision.kind != ComputerAuditKind::Provisioned
                    || provision.stable_key != rollback.provision_operation_key
                    || provision.computer_revision != Revision::ZERO
                {
                    return Err(ComputerError::UnsafeProvisionRollback(
                        "provision operation does not match",
                    ));
                }
                self.computers.remove(&rollback.owner_profile_id);
                self.computer_ids.remove(&current.computer_id);
                self.audits.remove(&rollback.owner_profile_id);
                self.audit_ids.remove(&provision.audit_id);
                self.audit_keys.remove(provision.stable_key.as_str());
                self.provision_rollbacks
                    .insert(rollback.owner_profile_id.clone(), rollback);
            }
        }
        Ok(())
    }
}

fn check_revision(expected: Revision, actual: Revision) -> Result<(), ComputerError> {
    if expected == actual {
        Ok(())
    } else {
        Err(ComputerError::RevisionConflict { expected, actual })
    }
}

fn next_revision(value: Revision) -> Result<Revision, ComputerError> {
    value.checked_next().ok_or(ComputerError::RevisionOverflow)
}

fn next_fence(value: u64) -> Result<u64, ComputerError> {
    value.checked_add(1).ok_or(ComputerError::RevisionOverflow)
}

fn version(value: SchemaVersion) -> Result<(), ComputerError> {
    if value == COMPUTER_SCHEMA_VERSION {
        Ok(())
    } else {
        Err(ComputerError::UnsupportedVersion(value))
    }
}

fn bounded(value: &str, max: usize, field: &'static str) -> Result<(), ComputerError> {
    if value.len() <= max {
        Ok(())
    } else {
        Err(ComputerError::Malformed(field))
    }
}

fn bounded_nonempty(value: &str, max: usize, field: &'static str) -> Result<(), ComputerError> {
    if !bounded_safe(value, max) {
        return Err(ComputerError::Malformed(field));
    }
    Ok(())
}

fn bounded_safe(value: &str, max: usize) -> bool {
    !value.trim().is_empty() && value.len() <= max && !value.contains('\0')
}

fn canonical<T: Serialize>(record: &T) -> Result<Vec<u8>, ComputerError> {
    canonical_json_bytes(record).map_err(|error| ComputerError::Encoding(error.to_string()))
}

fn decode<T: DeserializeOwned + Serialize>(
    bytes: &[u8],
    validate: fn(&T) -> Result<(), ComputerError>,
) -> Result<T, ComputerError> {
    let record: T = serde_json::from_slice(bytes)
        .map_err(|error| ComputerError::Encoding(error.to_string()))?;
    validate(&record)?;
    if canonical(&record)? != bytes {
        return Err(ComputerError::Malformed(
            "record is not canonically encoded",
        ));
    }
    Ok(record)
}

#[cfg(test)]
mod tests {
    use super::*;
    use keith_agent_types::EntityId;
    use keith_state_store::EmbeddedStore;

    fn profile(value: u128) -> ProfileId {
        ProfileId::from(EntityId::from_u128(value))
    }

    fn key(value: &str) -> StableKey {
        StableKey::parse(value).unwrap()
    }

    fn computer(owner: ProfileId, id: u128) -> ComputerRecord {
        ComputerRecord {
            version: CURRENT_SCHEMA_VERSION,
            computer_id: ComputerId::from(EntityId::from_u128(id)),
            owner_profile_id: owner,
            browser_profile_root: "/data/browser".into(),
            screen_key: key("display-1"),
            state: ComputerState::Ready,
            control_state: ControlState::Idle,
            current_task_key: None,
            created_at: UtcTimestamp(10),
            updated_at: UtcTimestamp(10),
            revision: Revision::ZERO,
        }
    }

    fn audit(owner: ProfileId, key: &str) -> ComputerAudit {
        ComputerAudit {
            version: CURRENT_SCHEMA_VERSION,
            audit_id: AuditId::from(EntityId::from_u128(200)),
            computer_id: ComputerId::from(EntityId::from_u128(100)),
            owner_profile_id: owner,
            sequence: 1,
            stable_key: StableKey::parse(key).unwrap(),
            actor: AuditActor::Owner,
            kind: ComputerAuditKind::Provisioned,
            task_key: None,
            navigation_origin: None,
            control_transition: None,
            policy_decision: None,
            side_effect_summary: None,
            transfer: None,
            safe_failure: None,
            recovery_correlation: None,
            safe_summary: "created".into(),
            occurred_at: UtcTimestamp(10),
            computer_revision: Revision::ZERO,
        }
    }

    fn lease(owner: ProfileId, id: u128, digest: char) -> TakeoverLease {
        TakeoverLease {
            version: CURRENT_SCHEMA_VERSION,
            takeover_lease_id: TakeoverLeaseId::from(EntityId::from_u128(id)),
            computer_id: ComputerId::from(EntityId::from_u128(100)),
            owner_profile_id: owner,
            task_key: key("task"),
            token_digest_hex: digest.to_string().repeat(TOKEN_DIGEST_HEX_BYTES),
            fencing_token: 1,
            acquired_at: UtcTimestamp(10),
            renewed_at: UtcTimestamp(10),
            expires_at: UtcTimestamp(20),
            state: TakeoverState::Active,
            revision: Revision::ZERO,
        }
    }

    #[test]
    fn domain_round_trips_only_canonical_strict_records() {
        let record = computer(profile(1), 101);
        let encoded = record.to_canonical_json().unwrap();
        assert_eq!(
            ComputerRecord::from_canonical_json(&encoded).unwrap(),
            record
        );
        let mut unknown = encoded;
        unknown.pop();
        unknown.extend_from_slice(b",\"owner_id\":\"intruder\"}");
        assert!(ComputerRecord::from_canonical_json(&unknown).is_err());
        let pretty = serde_json::to_vec_pretty(&record).unwrap();
        assert_eq!(
            ComputerRecord::from_canonical_json(&pretty),
            Err(ComputerError::Malformed(
                "record is not canonically encoded"
            ))
        );
    }

    #[test]
    fn domain_rejects_malformed_and_unbounded_records() {
        let mut record = computer(profile(2), 102);
        record.browser_profile_root = "x".repeat(MAX_PATH_BYTES + 1);
        assert!(record.validate().is_err());
        let lease = TakeoverLease {
            version: CURRENT_SCHEMA_VERSION,
            takeover_lease_id: TakeoverLeaseId::from(EntityId::from_u128(300)),
            computer_id: ComputerId::from(EntityId::from_u128(100)),
            owner_profile_id: profile(2),
            task_key: key("task"),
            token_digest_hex: "not-a-digest".into(),
            fencing_token: 0,
            acquired_at: UtcTimestamp(3),
            renewed_at: UtcTimestamp(2),
            expires_at: UtcTimestamp(1),
            state: TakeoverState::Active,
            revision: Revision::ZERO,
        };
        assert!(lease.validate().is_err());
    }

    #[test]
    fn domain_transaction_rolls_back_all_changes_on_conflict() {
        let repository = InMemoryComputerRepository::default();
        let owner = profile(3);
        repository
            .transact(&[ComputerRepositoryBatch::InsertComputer(computer(
                owner.clone(),
                100,
            ))])
            .unwrap();
        let other = profile(4);
        let result = repository.transact(&[
            ComputerRepositoryBatch::InsertComputer(computer(other.clone(), 104)),
            ComputerRepositoryBatch::AppendAudit(audit(owner.clone(), "provision:3")),
            ComputerRepositoryBatch::InsertComputer(computer(owner.clone(), 105)),
        ]);
        assert_eq!(result, Err(ComputerError::DuplicateComputer(owner.clone())));
        assert!(repository.computer(&other).unwrap().is_none());
        assert!(repository.audit(&owner).unwrap().is_empty());
    }

    #[test]
    fn domain_enforces_owner_uniqueness_revisions_and_audit_keys() {
        let repository = InMemoryComputerRepository::default();
        let owner = profile(5);
        repository
            .transact(&[
                ComputerRepositoryBatch::InsertComputer(computer(owner.clone(), 100)),
                ComputerRepositoryBatch::AppendAudit(audit(owner.clone(), "provision:5")),
            ])
            .unwrap();
        assert_eq!(repository.audit(&owner).unwrap().len(), 1);
        let mut replacement = computer(owner.clone(), 100);
        replacement.revision = Revision::new(1);
        replacement.updated_at = UtcTimestamp(11);
        repository
            .transact(&[ComputerRepositoryBatch::ReplaceComputer {
                expected_revision: Revision::ZERO,
                record: replacement,
            }])
            .unwrap();
        assert!(matches!(
            repository.transact(&[ComputerRepositoryBatch::InsertComputer(computer(
                owner.clone(),
                106,
            ))]),
            Err(ComputerError::DuplicateComputer(_))
        ));
        assert!(matches!(
            repository.transact(&[ComputerRepositoryBatch::AppendAudit(audit(
                owner,
                "provision:5"
            ))]),
            Err(ComputerError::AuditConflict)
        ));
    }

    #[test]
    fn domain_lease_renewal_rotates_token_advances_fence_and_rejects_reuse() {
        let repository = InMemoryComputerRepository::default();
        let owner = profile(6);
        let original = lease(owner.clone(), 300, 'a');
        repository
            .transact(&[
                ComputerRepositoryBatch::InsertComputer(computer(owner.clone(), 100)),
                ComputerRepositoryBatch::PutLease {
                    expected_revision: None,
                    lease: original.clone(),
                },
            ])
            .unwrap();
        let mut stale = original.clone();
        stale.revision = Revision::new(1);
        stale.renewed_at = UtcTimestamp(11);
        stale.expires_at = UtcTimestamp(21);
        assert!(
            repository
                .transact(&[ComputerRepositoryBatch::PutLease {
                    expected_revision: Some(Revision::ZERO),
                    lease: stale,
                }])
                .is_err()
        );
        let mut renewed = original;
        renewed.revision = Revision::new(1);
        renewed.fencing_token = 2;
        renewed.token_digest_hex = "b".repeat(TOKEN_DIGEST_HEX_BYTES);
        renewed.renewed_at = UtcTimestamp(11);
        renewed.expires_at = UtcTimestamp(21);
        repository
            .transact(&[ComputerRepositoryBatch::PutLease {
                expected_revision: Some(Revision::ZERO),
                lease: renewed,
            }])
            .unwrap();
        repository
            .transact(&[ComputerRepositoryBatch::RemoveLease {
                owner_profile_id: owner.clone(),
                expected_revision: Revision::new(1),
            }])
            .unwrap();
        assert!(matches!(
            repository.transact(&[ComputerRepositoryBatch::PutLease {
                expected_revision: None,
                lease: lease(owner, 300, 'c'),
            }]),
            Err(ComputerError::DuplicateLease(_))
        ));
    }

    #[test]
    fn domain_repository_rejects_corrupt_bytes_and_preserves_atomic_state() {
        let repository = InMemoryComputerRepository::default();
        let owner = profile(7);
        repository
            .transact(&[ComputerRepositoryBatch::InsertComputer(computer(
                owner.clone(),
                100,
            ))])
            .unwrap();
        repository.corrupt_computer(&owner, b"{".to_vec());
        assert!(matches!(
            repository.computer(&owner),
            Err(ComputerError::Encoding(_))
        ));
        let other = profile(8);
        assert!(
            repository
                .transact(&[ComputerRepositoryBatch::InsertComputer(computer(
                    other.clone(),
                    108,
                ))])
                .is_err()
        );
        assert!(repository.computer(&other).unwrap().is_none());
    }

    #[test]
    fn domain_rejects_revision_overflow_and_noncanonical_token_digest() {
        assert_eq!(
            next_revision(Revision::new(u64::MAX)),
            Err(ComputerError::RevisionOverflow)
        );
        let mut value = lease(profile(9), 301, 'A');
        assert!(value.validate().is_err());
        value.token_digest_hex = "a".repeat(TOKEN_DIGEST_HEX_BYTES);
        value.revision = Revision::new(1);
        let repository = InMemoryComputerRepository::default();
        assert!(
            repository
                .transact(&[ComputerRepositoryBatch::PutLease {
                    expected_revision: None,
                    lease: value,
                }])
                .is_err()
        );
    }

    #[test]
    fn domain_durable_repository_survives_restart_and_preserves_lease_tombstones() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("computer.sqlite3");
        let owner = profile(10);
        {
            let repository =
                DurableComputerRepository::new(EmbeddedStore::open(&path, None).unwrap());
            repository
                .transact(&[
                    ComputerRepositoryBatch::InsertComputer(computer(owner.clone(), 100)),
                    ComputerRepositoryBatch::PutLease {
                        expected_revision: None,
                        lease: lease(owner.clone(), 302, 'a'),
                    },
                    ComputerRepositoryBatch::AppendAudit(audit(owner.clone(), "provision:10")),
                ])
                .unwrap();
        }
        {
            let repository =
                DurableComputerRepository::new(EmbeddedStore::open(&path, None).unwrap());
            assert!(repository.computer(&owner).unwrap().is_some());
            assert!(repository.lease(&owner).unwrap().is_some());
            assert_eq!(repository.audit(&owner).unwrap().len(), 1);
            repository
                .transact(&[ComputerRepositoryBatch::RemoveLease {
                    owner_profile_id: owner.clone(),
                    expected_revision: Revision::ZERO,
                }])
                .unwrap();
        }
        let repository = DurableComputerRepository::new(EmbeddedStore::open(&path, None).unwrap());
        assert!(repository.lease(&owner).unwrap().is_none());
        assert!(matches!(
            repository.transact(&[ComputerRepositoryBatch::PutLease {
                expected_revision: None,
                lease: lease(owner, 302, 'b'),
            }]),
            Err(ComputerError::DuplicateLease(_))
        ));
    }
}
