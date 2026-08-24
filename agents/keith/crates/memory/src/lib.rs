#![forbid(unsafe_code)]

mod activation;
mod observatory;
mod recall;
mod relationship;
mod unified;

pub use activation::{
    ACTIVATION_SELECTOR_VERSION, ActivationError, ActivationPolicy, ActivationRequest,
    select_activation, validate_activation,
};

pub use observatory::{
    AtlasCatalog, AtlasComparison, AtlasCoverage, AtlasEdge, AtlasNode, AtlasNodeKind,
    AtlasRelation, AtlasSearchRequest, AtlasSearchResult, AtlasTimelineRequest, EvidenceAuthority,
    EvidenceFacet, EvidenceFacetKind, EvidenceRecord, EvidenceSourceKind, EvidenceValidity,
    MemoryObservatory, ObservatoryError, ObservatoryHealth, ObservatoryLimits, ObservatoryMutation,
};
pub use recall::{
    MemoryScoutFinding, RECALL_SELECTOR_VERSION, RecallCapsule, RecallClaim, RecallContradiction,
    RecallCoverage, RecallError, RecallRequest, RecallService,
};
pub use relationship::{
    PreferredName, RelationshipError, RelationshipService, RelationshipStage,
    RelationshipTurnContext,
};
pub use unified::{
    AgentMemoryKind, MEMORY_CONTEXT_SELECTOR_VERSION, MemoryContextBundle, MemoryContradiction,
    MemoryCorrectRequest, MemoryCreateRequest, MemoryForgetRequest, MemoryWriteSource,
};

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, EntityId, EntryId, ProfileId, Revision, SchemaVersion, SessionId,
    UtcTimestamp, canonical_json_bytes,
};
use keith_session_store::{
    CompactionEmission, MemoryKind, RetentionClass, Sensitivity, SessionEntryPayload,
};
use keith_workspace::{EditOutcome, PersonalWorkspace, WorkspaceActor};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const LEDGER_PATH: &str = ".keith/memory-ledger.json";

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileMemoryProvision {
    pub profile_id: ProfileId,
    pub ledger_path: PathBuf,
    pub created: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileMemoryDisposition {
    pub profile_id: ProfileId,
    pub revision: Revision,
    pub stable_key: String,
    pub records: usize,
    pub managed_paths: BTreeSet<PathBuf>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileMemoryLeakScan {
    pub profile_id: ProfileId,
    pub ledger_present: bool,
    pub materialized_paths: BTreeSet<PathBuf>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileMemoryEraseReport {
    pub profile_id: ProfileId,
    pub ledger_removed: bool,
    pub retained_materialized_paths: BTreeSet<PathBuf>,
}

/// Idempotently persists the empty profile-owned memory ledger before profile enablement.
/// # Errors
/// Returns an error for incompatible existing state or durable-write failure.
pub fn provision_profile_memory(
    workspace: &PersonalWorkspace,
    profile_id: &ProfileId,
) -> Result<ProfileMemoryProvision, MemoryError> {
    let path = workspace.layout().root.join(LEDGER_PATH);
    if path.exists() {
        let ledger: MemoryLedger = serde_json::from_slice(&fs::read(&path)?)?;
        if &ledger.profile_id != profile_id || ledger.version != CURRENT_SCHEMA_VERSION {
            return Err(MemoryError::IncompatibleLedger);
        }
        return Ok(ProfileMemoryProvision {
            profile_id: profile_id.clone(),
            ledger_path: path,
            created: false,
        });
    }
    persist_ledger(
        &workspace.layout().root,
        &MemoryLedger::new(profile_id.clone()),
    )?;
    Ok(ProfileMemoryProvision {
        profile_id: profile_id.clone(),
        ledger_path: path,
        created: true,
    })
}

/// # Errors
/// Removes only a newly-created matching ledger during lifecycle rollback.
pub fn rollback_profile_memory(provision: &ProfileMemoryProvision) -> Result<(), MemoryError> {
    if !provision.created || !provision.ledger_path.ends_with(LEDGER_PATH) {
        return Err(MemoryError::IncompatibleLedger);
    }
    if provision.ledger_path.exists() {
        let ledger: MemoryLedger = serde_json::from_slice(&fs::read(&provision.ledger_path)?)?;
        if ledger.profile_id != provision.profile_id {
            return Err(MemoryError::IncompatibleLedger);
        }
        fs::remove_file(&provision.ledger_path)?;
    }
    Ok(())
}

/// # Errors
/// Returns an error when the durable ledger is missing, corrupt, or belongs to another profile.
pub fn inspect_profile_memory_disposition(
    workspace: &PersonalWorkspace,
    profile_id: &ProfileId,
) -> Result<ProfileMemoryDisposition, MemoryError> {
    let path = workspace.layout().root.join(LEDGER_PATH);
    let bytes = fs::read(path)?;
    let ledger: MemoryLedger = serde_json::from_slice(&bytes)?;
    if &ledger.profile_id != profile_id || ledger.version != CURRENT_SCHEMA_VERSION {
        return Err(MemoryError::IncompatibleLedger);
    }
    Ok(ProfileMemoryDisposition {
        profile_id: profile_id.clone(),
        revision: Revision::new(
            u64::try_from(
                ledger
                    .records
                    .len()
                    .saturating_add(ledger.processed_boundaries.len()),
            )
            .map_err(|_| MemoryError::InvalidRequest)?,
        ),
        stable_key: format!("memory-delete:{}", memory_hex(&Sha256::digest(&bytes))),
        records: ledger.records.len(),
        managed_paths: ledger.managed_paths,
    })
}

/// Erases the profile-private ledger after revalidating the exact disposition.
/// Materialized human-readable workspace files are reported as remnants for the workspace plan.
/// # Errors
/// Returns an error when the disposition is stale or belongs to another profile.
pub fn erase_profile_memory(
    workspace: &PersonalWorkspace,
    disposition: &ProfileMemoryDisposition,
) -> Result<ProfileMemoryEraseReport, MemoryError> {
    let path = workspace.layout().root.join(LEDGER_PATH);
    if !path.exists() {
        return Ok(ProfileMemoryEraseReport {
            profile_id: disposition.profile_id.clone(),
            ledger_removed: false,
            retained_materialized_paths: disposition.managed_paths.clone(),
        });
    }
    let current = inspect_profile_memory_disposition(workspace, &disposition.profile_id)?;
    if &current != disposition {
        return Err(MemoryError::Changed);
    }
    fs::remove_file(path)?;
    Ok(ProfileMemoryEraseReport {
        profile_id: disposition.profile_id.clone(),
        ledger_removed: true,
        retained_materialized_paths: disposition.managed_paths.clone(),
    })
}

/// Reports every surviving memory-owned ledger or materialized path after erasure.
pub fn scan_profile_memory_leaks(
    workspace: &PersonalWorkspace,
    disposition: &ProfileMemoryDisposition,
) -> ProfileMemoryLeakScan {
    let root = workspace.layout().root;
    ProfileMemoryLeakScan {
        profile_id: disposition.profile_id.clone(),
        ledger_present: root.join(LEDGER_PATH).exists(),
        materialized_paths: disposition
            .managed_paths
            .iter()
            .filter(|path| root.join(path).exists())
            .cloned()
            .collect(),
    }
}

fn memory_hex(bytes: &[u8]) -> String {
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut value, byte| {
            use std::fmt::Write as _;
            let _ = write!(value, "{byte:02x}");
            value
        })
}

#[cfg(test)]
mod agent_lifecycle_resource_tests {
    use super::*;
    use keith_agent_types::EntityId;

    #[test]
    fn agent_lifecycle_memory_provision_replays_and_rollback_checks_owner() {
        let directory = tempfile::tempdir().unwrap();
        let workspace = PersonalWorkspace::open(
            directory.path().join("workspace"),
            keith_workspace::PersonalWorkspaceLimits::default(),
            UtcTimestamp(1),
        )
        .unwrap();
        let profile = ProfileId::from(EntityId::from_u128(1));
        let created = provision_profile_memory(&workspace, &profile).unwrap();
        let replay = provision_profile_memory(&workspace, &profile).unwrap();
        assert!(created.created);
        assert!(!replay.created);
        assert_eq!(
            inspect_profile_memory_disposition(&workspace, &profile)
                .unwrap()
                .records,
            0
        );
        let disposition = inspect_profile_memory_disposition(&workspace, &profile).unwrap();
        let erased = erase_profile_memory(&workspace, &disposition).unwrap();
        assert!(erased.ledger_removed);
        assert!(!scan_profile_memory_leaks(&workspace, &disposition).ledger_present);
        assert!(
            !erase_profile_memory(&workspace, &disposition)
                .unwrap()
                .ledger_removed
        );
        assert!(rollback_profile_memory(&created).is_ok());
        assert!(!created.ledger_path.exists());
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct MemoryPolicy {
    pub max_automatic_sensitivity: Sensitivity,
    pub retain_secret_candidates: bool,
    pub max_current_state_bytes: usize,
    pub max_daily_bytes: usize,
    pub max_durable_bytes: usize,
}

impl Default for MemoryPolicy {
    fn default() -> Self {
        Self {
            max_automatic_sensitivity: Sensitivity::Personal,
            retain_secret_candidates: false,
            max_current_state_bytes: 32 * 1_024,
            max_daily_bytes: 64 * 1_024,
            max_durable_bytes: 256 * 1_024,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MemoryRecordState {
    Proposed,
    Active,
    Superseded,
    Deleted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryRecord {
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub kind: MemoryKind,
    pub text: String,
    pub source_session: SessionId,
    pub source_entries: Vec<EntryId>,
    pub source_boundary: EntryId,
    pub proposed_at: UtcTimestamp,
    pub sensitivity: Sensitivity,
    pub retention: RetentionClass,
    pub state: MemoryRecordState,
    pub day: Option<String>,
    pub supersedes: Option<EntityId>,
    pub superseded_by: Option<EntityId>,
    pub deleted_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConsolidationOutcome {
    pub boundary: EntryId,
    pub active: Vec<EntityId>,
    pub proposed: Vec<EntityId>,
    pub skipped: Vec<EntityId>,
    pub duplicate: bool,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct MemoryLedger {
    version: SchemaVersion,
    profile_id: ProfileId,
    records: BTreeMap<EntityId, MemoryRecord>,
    processed_boundaries: BTreeSet<EntryId>,
    managed_paths: BTreeSet<PathBuf>,
}

impl MemoryLedger {
    fn new(profile_id: ProfileId) -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            profile_id,
            records: BTreeMap::new(),
            processed_boundaries: BTreeSet::new(),
            managed_paths: BTreeSet::new(),
        }
    }
}

#[derive(Debug, Error)]
pub enum MemoryError {
    #[error("memory I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("memory JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("workspace update failed: {0}")]
    Workspace(#[from] keith_workspace::PersonalWorkspaceError),
    #[error("memory observatory failed: {0}")]
    Observatory(#[from] ObservatoryError),
    #[error("memory recall failed: {0}")]
    Recall(String),
    #[error("relationship memory failed: {0}")]
    Relationship(#[from] RelationshipError),
    #[error("memory clock failed: {0}")]
    Clock(String),
    #[error("memory ledger belongs to another profile or unsupported schema")]
    IncompatibleLedger,
    #[error("compaction emission does not contain a committed compaction boundary")]
    InvalidEmission,
    #[error("memory policy contains a zero output bound")]
    InvalidPolicy,
    #[error("memory record was not found or is no longer mutable")]
    MissingRecord,
    #[error("memory record text must be non-empty")]
    EmptyText,
    #[error("human and memory-service edits conflict at {0}")]
    Conflict(PathBuf),
    #[error("memory state lock was poisoned")]
    LockPoisoned,
    #[error("workspace rollback failed after: {cause}; rollback: {rollback}")]
    Rollback { cause: String, rollback: String },
    #[error("memory request is invalid or exceeds a hard bound")]
    InvalidRequest,
    #[error("memory write does not cite exact committed evidence")]
    InvalidEvidenceQuote,
    #[error("memory authority changed while assembling context")]
    Changed,
    #[error("memory identity serialization failed: {0}")]
    Identity(String),
}

pub struct MemoryService {
    profile_id: ProfileId,
    workspace: PersonalWorkspace,
    policy: MemoryPolicy,
    ledger: Mutex<MemoryLedger>,
    observatory: MemoryObservatory,
    recall: RecallService,
    relationship: Option<RelationshipService>,
    pending_ingestion: Mutex<unified::PendingIngestionQueue>,
    hot_cache: Mutex<unified::HotMemoryCache>,
}

impl MemoryService {
    /// Opens the profile-scoped consolidation ledger and readable workspace destinations.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid policy, corrupt metadata, or a profile/schema mismatch.
    pub fn open(
        workspace: PersonalWorkspace,
        profile_id: &ProfileId,
        policy: MemoryPolicy,
    ) -> Result<Self, MemoryError> {
        validate_policy(policy)?;
        let ledger_path = workspace.layout().root.join(LEDGER_PATH);
        let ledger = if ledger_path.exists() {
            serde_json::from_slice::<MemoryLedger>(&fs::read(ledger_path)?)?
        } else {
            MemoryLedger::new(profile_id.clone())
        };
        if &ledger.profile_id != profile_id
            || ledger.version.major != CURRENT_SCHEMA_VERSION.major
            || ledger.version.minor > CURRENT_SCHEMA_VERSION.minor
        {
            return Err(MemoryError::IncompatibleLedger);
        }
        let now = UtcTimestamp::now().map_err(|error| MemoryError::Clock(error.to_string()))?;
        let observatory = MemoryObservatory::open(
            &workspace.layout().root,
            profile_id,
            ObservatoryLimits::default(),
            now,
        )?;
        observatory.sync_memory_records(ledger.records.values(), now)?;
        let recall = RecallService::open(&workspace.layout().root, profile_id)
            .map_err(|error| MemoryError::Recall(error.to_string()))?;
        let relationship = RelationshipService::open(&workspace.layout().root, profile_id).ok();
        if let Some(relationship) = &relationship {
            let _ = relationship.sync_evidence(&observatory, now);
        }
        Ok(Self {
            profile_id: profile_id.clone(),
            workspace,
            policy,
            ledger: Mutex::new(ledger),
            observatory,
            recall,
            relationship,
            pending_ingestion: Mutex::new(unified::PendingIngestionQueue::default()),
            hot_cache: Mutex::new(unified::HotMemoryCache::default()),
        })
    }

    /// Applies exactly one structured summarization result after its compaction boundary committed.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid emission, conflicting human edit, or persistence failure.
    pub fn apply_compaction(
        &self,
        session_id: &SessionId,
        emission: CompactionEmission,
        now: UtcTimestamp,
    ) -> Result<ConsolidationOutcome, MemoryError> {
        let compacted_through = match &emission.boundary.payload {
            SessionEntryPayload::Compaction {
                compacted_through, ..
            }
            | SessionEntryPayload::CompactionCheckpoint {
                compacted_through, ..
            } => compacted_through.clone(),
            _ => return Err(MemoryError::InvalidEmission),
        };
        let boundary = emission.boundary.id.clone();
        let mut ledger = self.lock()?;
        if ledger.processed_boundaries.contains(&boundary) {
            return Ok(ConsolidationOutcome {
                boundary,
                active: Vec::new(),
                proposed: Vec::new(),
                skipped: Vec::new(),
                duplicate: true,
            });
        }
        let mut next = ledger.clone();
        let mut outcome = ConsolidationOutcome {
            boundary: boundary.clone(),
            active: Vec::new(),
            proposed: Vec::new(),
            skipped: Vec::new(),
            duplicate: false,
        };
        admit_emission(
            &mut next,
            emission,
            AdmissionContext {
                session_id,
                compacted_through: &compacted_through,
                boundary: &boundary,
                now,
            },
            self.policy,
            &mut outcome,
        );
        next.processed_boundaries.insert(boundary);
        self.commit_ledger(&mut next, now)?;
        let _ = self
            .observatory
            .sync_memory_records(next.records.values(), now);
        *ledger = next;
        Ok(outcome)
    }

    /// Replaces an active or proposed record while retaining its source and supersession chain.
    ///
    /// # Errors
    ///
    /// Returns an error for empty text, a missing record, conflict, or persistence failure.
    pub fn correct(
        &self,
        record_id: &EntityId,
        replacement: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<MemoryRecord, MemoryError> {
        let replacement = replacement.into();
        if replacement.trim().is_empty() {
            return Err(MemoryError::EmptyText);
        }
        let mut ledger = self.lock()?;
        let mut next = ledger.clone();
        let original = next
            .records
            .get(record_id)
            .filter(|record| {
                matches!(
                    record.state,
                    MemoryRecordState::Active | MemoryRecordState::Proposed
                )
            })
            .cloned()
            .ok_or(MemoryError::MissingRecord)?;
        let mut corrected = original.clone();
        corrected.id = EntityId::new();
        corrected.text = replacement;
        corrected.proposed_at = now;
        corrected.supersedes = Some(original.id.clone());
        corrected.superseded_by = None;
        corrected.deleted_at = None;
        let previous = next
            .records
            .get_mut(record_id)
            .ok_or(MemoryError::MissingRecord)?;
        previous.state = MemoryRecordState::Superseded;
        previous.superseded_by = Some(corrected.id.clone());
        next.records.insert(corrected.id.clone(), corrected.clone());
        self.commit_ledger(&mut next, now)?;
        let _ = self
            .observatory
            .sync_memory_records(next.records.values(), now);
        *ledger = next;
        Ok(corrected)
    }

    /// Tombstones a record so it is removed from future materialization and retrieval sources.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing record, conflict, or persistence failure.
    pub fn delete(&self, record_id: &EntityId, now: UtcTimestamp) -> Result<(), MemoryError> {
        let mut ledger = self.lock()?;
        let mut next = ledger.clone();
        let record = next
            .records
            .get_mut(record_id)
            .filter(|record| {
                matches!(
                    record.state,
                    MemoryRecordState::Active | MemoryRecordState::Proposed
                )
            })
            .ok_or(MemoryError::MissingRecord)?;
        record.state = MemoryRecordState::Deleted;
        record.deleted_at = Some(now);
        self.commit_ledger(&mut next, now)?;
        let _ = self
            .observatory
            .sync_memory_records(next.records.values(), now);
        *ledger = next;
        Ok(())
    }

    /// Returns every source-linked record, including superseded and deleted metadata.
    ///
    /// # Errors
    ///
    /// Returns an error when the memory state lock is poisoned.
    pub fn records(&self) -> Result<Vec<MemoryRecord>, MemoryError> {
        Ok(self.lock()?.records.values().cloned().collect())
    }

    /// Returns the profile-scoped evidence vault and rebuildable atlas.
    pub const fn observatory(&self) -> &MemoryObservatory {
        &self.observatory
    }

    /// Returns the strongest sensitivity the service may expose automatically.
    pub const fn max_automatic_sensitivity(&self) -> Sensitivity {
        self.policy.max_automatic_sensitivity
    }

    /// Returns the bounded deliberate-recall service for this profile.
    pub const fn recall(&self) -> &RecallService {
        &self.recall
    }

    /// Advances relationship onboarding for one exact user-ingress source.
    ///
    /// The caller may treat errors as optional-context failures. A successfully appended
    /// relationship transition remains durable even if its evidence projection must retry later.
    ///
    /// # Errors
    ///
    /// Returns an error for a non-user/corrupt entry or relationship-log persistence failure.
    pub fn prepare_relationship_turn(
        &self,
        session_id: &SessionId,
        entry: &keith_session_store::SessionEntry,
        user_text: &str,
        now: UtcTimestamp,
    ) -> Result<RelationshipTurnContext, MemoryError> {
        entry
            .verify()
            .map_err(|_| MemoryError::Relationship(RelationshipError::Invalid))?;
        if !matches!(entry.payload, SessionEntryPayload::UserMessage { .. }) {
            return Err(MemoryError::Relationship(RelationshipError::Invalid));
        }
        let relationship = self
            .relationship
            .as_ref()
            .ok_or(MemoryError::Relationship(RelationshipError::Invalid))?;
        let context =
            relationship.prepare_turn(session_id, &entry.id, &entry.checksum, user_text, now)?;
        let _ = relationship.sync_evidence(&self.observatory, now);
        Ok(context)
    }

    /// Returns the profile-scoped durable relationship state service.
    pub const fn relationship(&self) -> Option<&RelationshipService> {
        self.relationship.as_ref()
    }

    /// Projects committed session evidence without making the atlas authoritative.
    ///
    /// # Errors
    ///
    /// Returns an error when source evidence is invalid or the append-only vault cannot persist.
    pub fn ingest_session_entries(
        &self,
        session_id: &SessionId,
        entries: &[keith_session_store::SessionEntry],
        now: UtcTimestamp,
    ) -> Result<u64, MemoryError> {
        self.observatory
            .ingest_session_entries(session_id, entries, now)
            .map_err(Into::into)
    }

    fn commit_ledger(&self, next: &mut MemoryLedger, now: UtcTimestamp) -> Result<(), MemoryError> {
        self.workspace.scan_external_changes(now)?;
        let snapshot = self
            .workspace
            .create_snapshot("before memory consolidation", now)?;
        let result = self.materialize(next, now).and_then(|paths| {
            next.managed_paths = paths;
            persist_ledger(&self.workspace.layout().root, next)
        });
        if let Err(cause) = result {
            if let Err(rollback) =
                self.workspace
                    .restore_snapshot(WorkspaceActor::System, &snapshot.id, now)
            {
                return Err(MemoryError::Rollback {
                    cause: cause.to_string(),
                    rollback: rollback.to_string(),
                });
            }
            return Err(cause);
        }
        Ok(())
    }

    fn materialize(
        &self,
        ledger: &MemoryLedger,
        now: UtcTimestamp,
    ) -> Result<BTreeSet<PathBuf>, MemoryError> {
        let rendered = render_destinations(ledger, self.policy);
        let mut paths = ledger.managed_paths.clone();
        paths.extend(rendered.keys().cloned());
        let mut retained = BTreeSet::new();
        for path in paths {
            let generated = rendered.get(&path).map_or("", String::as_str);
            let absolute = self.workspace.layout().root.join(&path);
            let existing = match fs::read_to_string(&absolute) {
                Ok(existing) => existing,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => String::new(),
                Err(error) => return Err(error.into()),
            };
            let replacement = replace_managed(&existing, generated);
            if replacement.as_bytes() == existing.as_bytes() {
                if !generated.is_empty() {
                    retained.insert(path);
                }
                continue;
            }
            let expected = self.workspace.token(&path)?;
            match self.workspace.edit(
                WorkspaceActor::MemoryTool,
                &path,
                &expected,
                replacement.as_bytes(),
                now,
            )? {
                EditOutcome::Written(_) => {
                    if !generated.is_empty() {
                        retained.insert(path);
                    }
                }
                EditOutcome::Conflict(_) => return Err(MemoryError::Conflict(path)),
            }
        }
        Ok(retained)
    }

    fn lock(&self) -> Result<MutexGuard<'_, MemoryLedger>, MemoryError> {
        self.ledger.lock().map_err(|_| MemoryError::LockPoisoned)
    }
}

fn validate_policy(policy: MemoryPolicy) -> Result<(), MemoryError> {
    if policy.max_current_state_bytes == 0
        || policy.max_daily_bytes == 0
        || policy.max_durable_bytes == 0
    {
        Err(MemoryError::InvalidPolicy)
    } else {
        Ok(())
    }
}

#[derive(Clone, Copy)]
struct AdmissionContext<'a> {
    session_id: &'a SessionId,
    compacted_through: &'a EntryId,
    boundary: &'a EntryId,
    now: UtcTimestamp,
}

fn admit_emission(
    ledger: &mut MemoryLedger,
    emission: CompactionEmission,
    context: AdmissionContext<'_>,
    policy: MemoryPolicy,
    outcome: &mut ConsolidationOutcome,
) {
    for candidate in emission.memory_candidates {
        let record = MemoryRecord {
            id: candidate.id,
            profile_id: ledger.profile_id.clone(),
            kind: candidate.kind,
            text: candidate.text,
            source_session: context.session_id.clone(),
            source_entries: candidate.source_entries,
            source_boundary: context.boundary.clone(),
            proposed_at: context.now,
            sensitivity: candidate.sensitivity,
            retention: candidate.retention,
            state: MemoryRecordState::Proposed,
            day: day_for_retention(candidate.retention, context.now),
            supersedes: None,
            superseded_by: None,
            deleted_at: None,
        };
        admit_record(ledger, record, policy, outcome);
    }
    if let Some(text) = emission.daily_entry {
        admit_record(
            ledger,
            derived_record(
                MemoryKind::DailySummary,
                text,
                RetentionClass::Daily,
                context.session_id,
                context.compacted_through,
                context.boundary,
                context.now,
            ),
            policy,
            outcome,
        );
    }
    for commitment in emission.open_commitments {
        let record = MemoryRecord {
            id: commitment.id,
            profile_id: ledger.profile_id.clone(),
            kind: MemoryKind::Commitment,
            text: commitment.description,
            source_session: context.session_id.clone(),
            source_entries: commitment.source_entries,
            source_boundary: context.boundary.clone(),
            proposed_at: context.now,
            sensitivity: Sensitivity::Personal,
            retention: RetentionClass::CurrentState,
            state: MemoryRecordState::Active,
            day: None,
            supersedes: None,
            superseded_by: None,
            deleted_at: None,
        };
        admit_record(ledger, record, policy, outcome);
    }
    for text in emission.unresolved_items {
        admit_record(
            ledger,
            derived_record(
                MemoryKind::ProjectContext,
                text,
                RetentionClass::CurrentState,
                context.session_id,
                context.compacted_through,
                context.boundary,
                context.now,
            ),
            policy,
            outcome,
        );
    }
}

fn derived_record(
    kind: MemoryKind,
    text: String,
    retention: RetentionClass,
    session_id: &SessionId,
    compacted_through: &EntryId,
    boundary: &EntryId,
    now: UtcTimestamp,
) -> MemoryRecord {
    MemoryRecord {
        id: EntityId::new(),
        profile_id: ProfileId::new(),
        kind,
        text,
        source_session: session_id.clone(),
        source_entries: vec![compacted_through.clone()],
        source_boundary: boundary.clone(),
        proposed_at: now,
        sensitivity: Sensitivity::Personal,
        retention,
        state: MemoryRecordState::Active,
        day: day_for_retention(retention, now),
        supersedes: None,
        superseded_by: None,
        deleted_at: None,
    }
}

fn admit_record(
    ledger: &mut MemoryLedger,
    mut record: MemoryRecord,
    policy: MemoryPolicy,
    outcome: &mut ConsolidationOutcome,
) {
    record.profile_id = ledger.profile_id.clone();
    if record.retention == RetentionClass::DoNotStore
        || (record.sensitivity == Sensitivity::Secret && !policy.retain_secret_candidates)
    {
        outcome.skipped.push(record.id);
        return;
    }
    record.state = if record.retention == RetentionClass::Durable
        && sensitivity_rank(record.sensitivity) > sensitivity_rank(policy.max_automatic_sensitivity)
    {
        MemoryRecordState::Proposed
    } else {
        MemoryRecordState::Active
    };
    match record.state {
        MemoryRecordState::Active => outcome.active.push(record.id.clone()),
        MemoryRecordState::Proposed => outcome.proposed.push(record.id.clone()),
        MemoryRecordState::Superseded | MemoryRecordState::Deleted => {}
    }
    ledger.records.insert(record.id.clone(), record);
}

const fn sensitivity_rank(sensitivity: Sensitivity) -> u8 {
    match sensitivity {
        Sensitivity::Public => 0,
        Sensitivity::Personal => 1,
        Sensitivity::Sensitive => 2,
        Sensitivity::Secret => 3,
    }
}

fn day_for_retention(retention: RetentionClass, now: UtcTimestamp) -> Option<String> {
    (retention == RetentionClass::Daily).then(|| utc_day(now))
}

fn utc_day(now: UtcTimestamp) -> String {
    let mut days = now.unix_millis().div_euclid(86_400_000) + 719_468;
    let era = days.div_euclid(146_097);
    days -= era * 146_097;
    let year_of_era = (days - days / 1_460 + days / 36_524 - days / 146_096) / 365;
    let mut year = year_of_era + era * 400;
    let day_of_year = days - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let month_prime = (5 * day_of_year + 2) / 153;
    let day = day_of_year - (153 * month_prime + 2) / 5 + 1;
    let month = month_prime + if month_prime < 10 { 3 } else { -9 };
    year += i64::from(month <= 2);
    format!("{year:04}-{month:02}-{day:02}")
}

fn render_destinations(ledger: &MemoryLedger, policy: MemoryPolicy) -> BTreeMap<PathBuf, String> {
    let mut durable = Vec::new();
    let mut current = Vec::new();
    let mut commitments = Vec::new();
    let mut daily = BTreeMap::<String, Vec<&MemoryRecord>>::new();
    for record in ledger
        .records
        .values()
        .filter(|record| record.state == MemoryRecordState::Active)
    {
        match record.retention {
            RetentionClass::Durable => durable.push(record),
            RetentionClass::CurrentState if record.kind == MemoryKind::Commitment => {
                commitments.push(record);
            }
            RetentionClass::CurrentState => current.push(record),
            RetentionClass::Daily => {
                if let Some(day) = &record.day {
                    daily.entry(day.clone()).or_default().push(record);
                }
            }
            RetentionClass::DoNotStore => {}
        }
    }
    durable.sort_by_key(|record| record.proposed_at);
    current.sort_by_key(|record| record.proposed_at);
    commitments.sort_by_key(|record| record.proposed_at);
    let mut rendered = BTreeMap::new();
    if !durable.is_empty() {
        rendered.insert(
            PathBuf::from("MEMORY.md"),
            render_records(&durable, policy.max_durable_bytes),
        );
    }
    if !current.is_empty() {
        rendered.insert(
            PathBuf::from("state/now.md"),
            render_records(&current, policy.max_current_state_bytes),
        );
    }
    if !commitments.is_empty() {
        rendered.insert(
            PathBuf::from("state/commitments.toml"),
            render_commitments(&commitments, policy.max_current_state_bytes),
        );
    }
    for (day, mut records) in daily {
        records.sort_by_key(|record| record.proposed_at);
        rendered.insert(
            PathBuf::from(format!("memory/daily/{day}.md")),
            render_records(&records, policy.max_daily_bytes),
        );
    }
    rendered
}

fn render_records(records: &[&MemoryRecord], limit: usize) -> String {
    let mut output = String::new();
    for record in records.iter().rev() {
        let line = format!("- {}\n", one_line(&record.text));
        if output.len().saturating_add(line.len()) > limit {
            break;
        }
        output.insert_str(0, &line);
    }
    output
}

fn render_commitments(records: &[&MemoryRecord], limit: usize) -> String {
    let mut output = String::new();
    for record in records {
        let text = one_line(&record.text).replace('"', "\\\"");
        let line = format!(
            "[[commitment]]\nid = \"{}\"\ndescription = \"{text}\"\n\n",
            record.id
        );
        if output.len().saturating_add(line.len()) > limit {
            break;
        }
        output.push_str(&line);
    }
    output
}

fn one_line(text: &str) -> String {
    text.split_whitespace().collect::<Vec<_>>().join(" ")
}

fn replace_managed(existing: &str, generated: &str) -> String {
    const START: &str = "<!-- keith:managed-memory:start -->";
    const END: &str = "<!-- keith:managed-memory:end -->";
    let managed = if generated.is_empty() {
        String::new()
    } else {
        format!("{START}\n{generated}{END}\n")
    };
    if let Some(start) = existing.find(START)
        && let Some(relative_end) = existing[start..].find(END)
    {
        let end = start + relative_end + END.len();
        let suffix = existing[end..]
            .strip_prefix('\n')
            .unwrap_or(&existing[end..]);
        let mut output = String::with_capacity(existing.len() + managed.len());
        output.push_str(existing[..start].trim_end_matches('\n'));
        if !output.is_empty() && !managed.is_empty() {
            output.push_str("\n\n");
        }
        output.push_str(&managed);
        if !suffix.is_empty() {
            if !output.is_empty() && !output.ends_with('\n') {
                output.push('\n');
            }
            output.push_str(suffix);
        }
        return output;
    }
    let mut output = existing.trim_end_matches('\n').to_owned();
    if !output.is_empty() && !managed.is_empty() {
        output.push_str("\n\n");
    }
    output.push_str(&managed);
    output
}

fn persist_ledger(root: &Path, ledger: &MemoryLedger) -> Result<(), MemoryError> {
    let path = root.join(LEDGER_PATH);
    let temporary = path.with_extension(format!("{}.tmp", EntityId::new()));
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&canonical_json_bytes(ledger)?)?;
    file.sync_all()?;
    keith_platform::replace_file(&temporary, &path)?;
    File::open(path.parent().ok_or(MemoryError::IncompatibleLedger)?)?.sync_all()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use keith_agent_types::{ActionId, Generation, RootTreeId, TurnId, WorkerId, WorkspaceId};
    use keith_session_store::{
        CommitmentDraft, CompactionOutput, CompactionPolicy, CompactionTrigger,
        MemoryCandidateDraft, NewSession, SessionKind, SessionStore, StoredMessage,
        TurnTerminalStatus, WriterIdentity,
    };
    use tempfile::tempdir;

    use super::*;

    #[allow(clippy::too_many_lines)]
    fn committed_emission(root: &Path, profile_id: ProfileId) -> (SessionId, CompactionEmission) {
        let store = SessionStore::open(root).unwrap();
        let session_id = SessionId::new();
        store
            .create(NewSession {
                kind: SessionKind::Root,
                session_id: session_id.clone(),
                root_tree_id: RootTreeId::new(),
                parent_session_id: None,
                profile_id,
                workspace_id: WorkspaceId::new(),
                created_at: UtcTimestamp::UNIX_EPOCH,
                label: None,
                profile_snapshot: None,
            })
            .unwrap();
        let mut writer = store
            .acquire_writer(
                &session_id,
                WriterIdentity {
                    worker_id: WorkerId::new(),
                    owner_instance: EntityId::new(),
                    generation: Generation::new(1),
                    acquired_at: UtcTimestamp::UNIX_EPOCH,
                },
            )
            .unwrap();
        let entry = writer
            .append(
                None,
                UtcTimestamp::UNIX_EPOCH,
                SessionEntryPayload::UserMessage {
                    message: StoredMessage {
                        role: keith_session_store::MessageRole::User,
                        content: vec![keith_session_store::ContentBlock::Text {
                            text: "Remember my preference and task".into(),
                        }],
                        provider_metadata: BTreeMap::new(),
                    },
                },
            )
            .unwrap();
        let first_turn = TurnId::new();
        let first_action = ActionId::new();
        writer
            .accept_turn(
                UtcTimestamp::UNIX_EPOCH,
                first_action.clone(),
                first_turn.clone(),
                entry.id.clone(),
            )
            .unwrap();
        writer
            .append_final_candidate(
                UtcTimestamp::UNIX_EPOCH,
                first_turn.clone(),
                StoredMessage {
                    role: keith_session_store::MessageRole::Assistant,
                    content: vec![keith_session_store::ContentBlock::Text {
                        text: "First answer".into(),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
                10,
                2,
                0,
            )
            .unwrap();
        writer
            .append_finalized_turn(
                UtcTimestamp::UNIX_EPOCH,
                &first_turn,
                StoredMessage {
                    role: keith_session_store::MessageRole::Assistant,
                    content: vec![keith_session_store::ContentBlock::Text {
                        text: "unused fallback".into(),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
                TurnTerminalStatus::Failed,
                false,
                true,
                Some(first_action),
                Vec::new(),
                None,
            )
            .unwrap();
        let second_user = writer
            .append(
                writer.manifest().active_leaf.clone(),
                UtcTimestamp::from_unix_millis(1),
                SessionEntryPayload::UserMessage {
                    message: StoredMessage {
                        role: keith_session_store::MessageRole::User,
                        content: vec![keith_session_store::ContentBlock::Text {
                            text: "Continue with the implementation".into(),
                        }],
                        provider_metadata: BTreeMap::new(),
                    },
                },
            )
            .unwrap();
        let second_turn = TurnId::new();
        let second_action = ActionId::new();
        writer
            .accept_turn(
                UtcTimestamp::from_unix_millis(1),
                second_action.clone(),
                second_turn.clone(),
                second_user.id,
            )
            .unwrap();
        writer
            .append_finalized_turn(
                UtcTimestamp::from_unix_millis(1),
                &second_turn,
                StoredMessage {
                    role: keith_session_store::MessageRole::Assistant,
                    content: vec![keith_session_store::ContentBlock::Text {
                        text: "Second answer".into(),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
                TurnTerminalStatus::Completed,
                true,
                true,
                Some(second_action),
                Vec::new(),
                None,
            )
            .unwrap();
        let request = writer
            .request_compaction(
                10_000,
                CompactionPolicy {
                    trigger_tokens: 2,
                    target_tokens: 1,
                    ..CompactionPolicy::default()
                },
                None,
                CompactionTrigger::Pressure,
            )
            .unwrap()
            .unwrap();
        let request = writer
            .begin_compaction(request, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        let output = CompactionOutput {
            request_id: request.id.clone(),
            session_summary: "summary".into(),
            raw_provider_output: "summary".into(),
            provider: Some("test-provider".into()),
            model: Some("test-model".into()),
            max_output_tokens: 128,
            input_tokens: 100,
            output_tokens: 10,
            cached_input_tokens: 0,
            memory_candidates: vec![
                MemoryCandidateDraft {
                    id: EntityId::new(),
                    kind: MemoryKind::Preference,
                    text: "Prefers concise updates".into(),
                    source_entries: vec![entry.id.clone()],
                    sensitivity: Sensitivity::Personal,
                    retention: RetentionClass::Durable,
                },
                MemoryCandidateDraft {
                    id: EntityId::new(),
                    kind: MemoryKind::PersonalFact,
                    text: "Highly sensitive candidate".into(),
                    source_entries: vec![entry.id.clone()],
                    sensitivity: Sensitivity::Sensitive,
                    retention: RetentionClass::Durable,
                },
            ],
            daily_entry: Some("Completed the workspace setup".into()),
            open_commitments: vec![CommitmentDraft {
                id: EntityId::new(),
                description: "Finish the implementation".into(),
                source_entries: vec![entry.id],
            }],
            unresolved_items: vec!["Verify restart behavior".into()],
        };
        let emission = writer
            .commit_compaction(&request, output, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        (session_id, emission)
    }

    #[test]
    fn committed_output_routes_separately_and_survives_restart() {
        let directory = tempdir().unwrap();
        let workspace_root = directory.path().join("workspace");
        let session_root = directory.path().join("sessions");
        let profile_id = ProfileId::new();
        let workspace = PersonalWorkspace::open(
            &workspace_root,
            keith_workspace::PersonalWorkspaceLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        let (session_id, emission) = committed_emission(&session_root, profile_id.clone());
        let service =
            MemoryService::open(workspace.clone(), &profile_id, MemoryPolicy::default()).unwrap();
        let outcome = service
            .apply_compaction(&session_id, emission.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert_eq!(outcome.active.len(), 4);
        assert_eq!(outcome.proposed.len(), 1);
        assert!(
            fs::read_to_string(workspace_root.join("MEMORY.md"))
                .unwrap()
                .contains("Prefers concise updates")
        );
        assert!(
            !fs::read_to_string(workspace_root.join("MEMORY.md"))
                .unwrap()
                .contains("Highly sensitive candidate")
        );
        assert!(
            fs::read_to_string(workspace_root.join("memory/daily/1970-01-01.md"))
                .unwrap()
                .contains("Completed the workspace setup")
        );
        assert!(
            fs::read_to_string(workspace_root.join("state/now.md"))
                .unwrap()
                .contains("Verify restart behavior")
        );
        assert!(
            fs::read_to_string(workspace_root.join("state/commitments.toml"))
                .unwrap()
                .contains("Finish the implementation")
        );
        drop(service);
        let reopened =
            MemoryService::open(workspace, &profile_id, MemoryPolicy::default()).unwrap();
        assert_eq!(reopened.records().unwrap().len(), 5);
        assert!(
            reopened
                .apply_compaction(&session_id, emission, UtcTimestamp::from_unix_millis(2))
                .unwrap()
                .duplicate
        );
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn correction_and_deletion_preserve_auditable_metadata_and_bounds() {
        let directory = tempdir().unwrap();
        let workspace_root = directory.path().join("workspace");
        let session_root = directory.path().join("sessions");
        let profile_id = ProfileId::new();
        let workspace = PersonalWorkspace::open(
            &workspace_root,
            keith_workspace::PersonalWorkspaceLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        let (session_id, emission) = committed_emission(&session_root, profile_id.clone());
        let service = MemoryService::open(
            workspace,
            &profile_id,
            MemoryPolicy {
                max_current_state_bytes: 30,
                ..MemoryPolicy::default()
            },
        )
        .unwrap();
        service
            .apply_compaction(&session_id, emission, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let original = service
            .records()
            .unwrap()
            .into_iter()
            .find(|record| record.text == "Prefers concise updates")
            .unwrap();
        let corrected = service
            .correct(
                &original.id,
                "Prefers short, concrete updates",
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        let memory = fs::read_to_string(workspace_root.join("MEMORY.md")).unwrap();
        assert!(memory.contains("Prefers short, concrete updates"));
        assert!(!memory.contains("Prefers concise updates"));
        let (old_results, _) = service
            .observatory()
            .search(&AtlasSearchRequest {
                query: "concise updates".into(),
                limit: 8,
                max_sensitivity: Sensitivity::Personal,
                include_disputed: false,
            })
            .unwrap();
        assert!(
            old_results
                .iter()
                .all(|result| result.evidence.text != "Prefers concise updates")
        );
        let (corrected_results, _) = service
            .observatory()
            .search(&AtlasSearchRequest {
                query: "short concrete updates".into(),
                limit: 8,
                max_sensitivity: Sensitivity::Personal,
                include_disputed: false,
            })
            .unwrap();
        assert!(
            corrected_results
                .iter()
                .any(|result| result.evidence.text == "Prefers short, concrete updates")
        );
        service
            .delete(&corrected.id, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert!(
            !fs::read_to_string(workspace_root.join("MEMORY.md"))
                .unwrap()
                .contains("Prefers short, concrete updates")
        );
        let (deleted_results, _) = service
            .observatory()
            .search(&AtlasSearchRequest {
                query: "short concrete updates".into(),
                limit: 8,
                max_sensitivity: Sensitivity::Personal,
                include_disputed: true,
            })
            .unwrap();
        assert!(
            deleted_results
                .iter()
                .all(|result| result.evidence.text != "Prefers short, concrete updates")
        );
        let records = service.records().unwrap();
        assert!(records.iter().any(|record| {
            record.id == original.id
                && record.state == MemoryRecordState::Superseded
                && record.superseded_by.as_ref() == Some(&corrected.id)
        }));
        assert!(records.iter().any(|record| {
            record.id == corrected.id && record.state == MemoryRecordState::Deleted
        }));
        let now = fs::read_to_string(workspace_root.join("state/now.md")).unwrap();
        let managed = now
            .split("<!-- keith:managed-memory:start -->")
            .nth(1)
            .unwrap_or("");
        assert!(managed.len() <= 80);
    }

    #[test]
    fn corrupt_relationship_state_fails_open_without_disabling_memory() {
        let directory = tempdir().unwrap();
        let workspace_root = directory.path().join("workspace");
        let profile_id = ProfileId::new();
        let workspace = PersonalWorkspace::open(
            &workspace_root,
            keith_workspace::PersonalWorkspaceLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        fs::create_dir_all(workspace_root.join(".keith")).unwrap();
        fs::write(
            workspace_root.join(".keith/relationship-events.jsonl"),
            b"{\"complete_but_invalid\":true}\n",
        )
        .unwrap();

        let service = MemoryService::open(workspace, &profile_id, MemoryPolicy::default()).unwrap();
        assert!(service.relationship().is_none());
        assert_eq!(service.observatory().revision().unwrap(), 0);
        let entry = keith_session_store::SessionEntry::new(
            EntryId::new(),
            None,
            UtcTimestamp::UNIX_EPOCH,
            SessionEntryPayload::UserMessage {
                message: StoredMessage {
                    role: keith_session_store::MessageRole::User,
                    content: vec![keith_session_store::ContentBlock::Text {
                        text: "hello".into(),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
            },
        )
        .unwrap();
        assert!(
            service
                .prepare_relationship_turn(
                    &SessionId::new(),
                    &entry,
                    "hello",
                    UtcTimestamp::UNIX_EPOCH,
                )
                .is_err()
        );
        service
            .ingest_session_entries(&SessionId::new(), &[entry], UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert_eq!(service.observatory().revision().unwrap(), 1);
    }
}
