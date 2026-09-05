#![forbid(unsafe_code)]

mod activation;
mod bindings;
pub use bindings::{BindingAliasCandidate, BindingAliasCandidates, BindingAssociationOrigin, BindingCorrectionDraft, BindingDraft, BindingEntityTarget, BindingError, BindingFreshness, BindingLookupRequest, BindingMutation, BindingQuery, BindingResolution, BindingResolutionReason, BindingSourceSpan, BindingUsePolicy, BindingWriteReceipt, RequiredBindingResolution, ResolvedBinding};
mod causal;
mod ingestion;
mod observatory;
mod recall;
mod relationship;
mod semantic;
#[cfg(test)]
mod test_sources;
mod unified;

pub use activation::{
    ACTIVATION_SELECTOR_VERSION, ActivationError, ActivationPolicy, ActivationRequest,
    select_activation, validate_activation,
};
pub use causal::{
    EVIDENCE_CAUSAL_VERSION, EvidenceCausalMetadata, EvidenceEffectiveInterval,
    EvidenceMetadataError, EvidenceSourceRoot, SourceLineageGap, SourceLineageGapReason,
};
pub use ingestion::{CommittedIngestionReceipt, IngestionProjectionStatus};
pub use semantic::{
    CandidateEvidenceReference, SEMANTIC_CANDIDATE_VERSION, SemanticCandidate,
    SemanticCandidateBatch, SemanticCandidateError, SemanticCandidateLane, SemanticCandidateQuery,
    SemanticCandidateSource, SemanticDegradedReason, SemanticIndexIdentity,
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
    CURRENT_SCHEMA_VERSION, EntityId, EntryId, ProfileId, SchemaVersion, SessionId, UtcTimestamp,
    canonical_json_bytes,
};
use keith_session_store::{
    CompactionEmission, MemoryKind, RetentionClass, Sensitivity, SessionEntryPayload,
};
use keith_workspace::{EditOutcome, PersonalWorkspace, WorkspaceActor};
use serde::{Deserialize, Serialize};
use thiserror::Error;

const LEDGER_PATH: &str = ".keith/memory-ledger.json";

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
    #[error("binding failed: {0}")]
    Binding(#[from] BindingError),
    #[error("memory ingestion is busy; retry the same committed source")]
    IngestionBusy,
    #[error("memory ingestion checkpoint or committed-source scope is invalid")]
    InvalidIngestion,
    #[error("memory ingestion cursor changed; reload it before replay")]
    IngestionCursorChanged,
    #[error("memory ingestion exceeds its pending-source or checkpoint bound")]
    IngestionLimit,
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
        let source_entries = match &emission.boundary.payload {
            SessionEntryPayload::Compaction {
                compacted_through, ..
            } => vec![compacted_through.clone()],
            SessionEntryPayload::CompactionCheckpoint { source_entries, .. } => {
                source_entries.clone()
            }
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
                source_entries: &source_entries,
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
    source_entries: &'a [EntryId],
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
                context.source_entries,
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
                context.source_entries,
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
    source_entries: &[EntryId],
    boundary: &EntryId,
    now: UtcTimestamp,
) -> MemoryRecord {
    MemoryRecord {
        id: EntityId::new(),
        profile_id: ProfileId::new(),
        kind,
        text,
        source_session: session_id.clone(),
        source_entries: source_entries.to_vec(),
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

    fn ingest_history(
        service: &MemoryService,
        root: &Path,
        profile: &ProfileId,
        session: &SessionId,
    ) {
        let store = SessionStore::open(root).unwrap();
        loop {
            let cursor = service.committed_source_cursor(session).unwrap();
            let page = store
                .committed_source_page(
                    profile,
                    session,
                    cursor.as_ref(),
                    keith_session_store::CommittedSourceLimits::default(),
                )
                .unwrap();
            service
                .ingest_committed_page(&page, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            if page.caught_up() {
                break;
            }
        }
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
        ingest_history(&service, &session_root, &profile_id, &session_id);
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
    fn committed_compaction_and_consolidation_keep_context_roots_without_fact_promotion() {
        let root = tempdir().unwrap();
        let profile = ProfileId::new();
        let sessions = root.path().join("sessions");
        let (session, emission) = committed_emission(&sessions, profile.clone());
        let source_entries = match &emission.boundary.payload {
            SessionEntryPayload::CompactionCheckpoint { source_entries, .. } => {
                source_entries.clone()
            }
            _ => panic!("fixture must use the real committed checkpoint"),
        };
        let service = MemoryService::open(
            PersonalWorkspace::open(
                root.path().join("workspace"),
                keith_workspace::PersonalWorkspaceLimits::default(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap(),
            &profile,
            MemoryPolicy::default(),
        )
        .unwrap();
        // The durable ledger accepts the already committed emission; evidence
        // projection waits for original source intake rather than making digests.
        service
            .apply_compaction(&session, emission, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(
            service
                .observatory()
                .evidence_snapshot()
                .unwrap()
                .is_empty()
        );
        ingest_history(&service, &sessions, &profile, &session);
        let snapshot = service.observatory().evidence_snapshot().unwrap();
        let summary = snapshot
            .values()
            .find(|record| record.source_kind == EvidenceSourceKind::CompactionSummary)
            .unwrap();
        let expected = snapshot
            .values()
            .filter(|record| {
                record.source_entries.len() == 1
                    && source_entries.contains(&record.source_entries[0])
                    && record.source_identity.starts_with("session:")
            })
            .flat_map(|record| record.causal.as_ref().unwrap().source_roots.clone())
            .collect::<BTreeSet<_>>();
        assert!(!expected.is_empty());
        assert!(snapshot.values().any(|record| {
            record.authority == EvidenceAuthority::AssistantGenerated
                && expected
                    .iter()
                    .any(|root| record.source_entries.contains(&root.source_entry))
        }));
        assert_eq!(
            summary
                .causal
                .as_ref()
                .unwrap()
                .source_roots
                .iter()
                .cloned()
                .collect::<BTreeSet<_>>(),
            expected
        );
        assert_eq!(summary.authority, EvidenceAuthority::DerivedInference);
        let daily_memory = service
            .records()
            .unwrap()
            .into_iter()
            .find(|record| record.kind == MemoryKind::DailySummary)
            .unwrap();
        assert_eq!(daily_memory.source_entries, source_entries);
        let daily = snapshot
            .values()
            .find(|record| record.source_identity == format!("memory:{}", daily_memory.id))
            .unwrap();
        assert_eq!(daily.authority, EvidenceAuthority::DerivedInference);
        assert_eq!(
            daily
                .causal
                .as_ref()
                .unwrap()
                .source_roots
                .iter()
                .cloned()
                .collect::<BTreeSet<_>>(),
            expected
        );
        assert!(
            daily
                .source_digests
                .iter()
                .all(|digest| crate::causal::valid_digest(digest))
        );
        let quoted = service
            .memory_create(
                MemoryCreateRequest {
                    source: MemoryWriteSource {
                        evidence_id: Some(summary.id.clone()),
                        source_entry_id: summary.source_entries[0].clone(),
                        evidence_quote: summary.text.clone(),
                    },
                    text: summary.text.clone(),
                    kind: AgentMemoryKind::ProjectContext,
                    facets: vec![],
                    sensitivity: Sensitivity::Personal,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_eq!(quoted.authority, EvidenceAuthority::DerivedInference);
        assert_eq!(
            quoted
                .causal
                .unwrap()
                .source_roots
                .iter()
                .cloned()
                .collect::<BTreeSet<_>>(),
            expected
        );
        let store = SessionStore::open(&sessions).unwrap();
        let page = store
            .committed_source_page(
                &profile,
                &session,
                None,
                keith_session_store::CommittedSourceLimits::default(),
            )
            .unwrap();
        let checkpoint_id = page
            .entries()
            .iter()
            .find(|entry| {
                matches!(
                    entry.payload,
                    SessionEntryPayload::CompactionCheckpoint { .. }
                )
            })
            .unwrap()
            .id
            .clone();
        let checkpoint = store
            .committed_source_entry(
                &profile,
                &session,
                &checkpoint_id,
                keith_session_store::CommittedSourceLimits::default(),
            )
            .unwrap();
        let user_id = page
            .entries()
            .iter()
            .find(|entry| matches!(entry.payload, SessionEntryPayload::UserMessage { .. }))
            .unwrap()
            .id
            .clone();
        let user = store
            .committed_source_entry(
                &profile,
                &session,
                &user_id,
                keith_session_store::CommittedSourceLimits::default(),
            )
            .unwrap();
        let fork = SessionId::new();
        store
            .create(NewSession {
                kind: SessionKind::Root,
                session_id: fork.clone(),
                root_tree_id: RootTreeId::new(),
                parent_session_id: None,
                profile_id: profile.clone(),
                workspace_id: page.workspace_id().clone(),
                created_at: UtcTimestamp::UNIX_EPOCH,
                label: None,
                profile_snapshot: None,
            })
            .unwrap();
        let mut writer = store
            .acquire_writer(
                &fork,
                WriterIdentity {
                    worker_id: WorkerId::new(),
                    owner_instance: EntityId::new(),
                    generation: Generation::new(1),
                    acquired_at: UtcTimestamp::UNIX_EPOCH,
                },
            )
            .unwrap();
        let copied_user = writer.append_source_copy(None, &user).unwrap().unwrap();
        let copied_checkpoint = writer
            .append_source_copy(Some(copied_user.entry().id.clone()), &checkpoint)
            .unwrap()
            .unwrap();
        service
            .ingest_committed_entry(&copied_user, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        service
            .ingest_committed_entry(&copied_checkpoint, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let after_copy = service.observatory().evidence_snapshot().unwrap();
        let copied =
            crate::ingestion::direct_source(&after_copy, &fork, &copied_checkpoint.entry().id)
                .unwrap();
        assert_eq!(copied.authority, EvidenceAuthority::DerivedInference);
        assert_eq!(
            copied
                .causal
                .as_ref()
                .unwrap()
                .source_roots
                .iter()
                .cloned()
                .collect::<BTreeSet<_>>(),
            expected
        );
        // These are context parents, not independently assessed claim supports.
    }

    #[test]
    fn legacy_summary_identity_is_repaired_in_place_from_actual_committed_history() {
        let root = tempdir().unwrap();
        let profile = ProfileId::new();
        let sessions = root.path().join("sessions");
        let (session, _) = committed_emission(&sessions, profile.clone());
        let store = SessionStore::open(&sessions).unwrap();
        let page = store
            .committed_source_page(
                &profile,
                &session,
                None,
                keith_session_store::CommittedSourceLimits::default(),
            )
            .unwrap();
        let summary_entry = page
            .entries()
            .iter()
            .find(|entry| matches!(entry.payload, SessionEntryPayload::CompactionSummary { .. }))
            .unwrap();
        let SessionEntryPayload::CompactionSummary {
            summary,
            source_entries,
            ..
        } = &summary_entry.payload
        else {
            unreachable!()
        };
        let legacy = EvidenceRecord::new(
            profile.clone(),
            session.clone(),
            source_entries.clone(),
            source_entries
                .iter()
                .map(|id| format!("{}:{id}", summary_entry.checksum))
                .collect(),
            format!("session:{session}:compaction-summary:{}", summary_entry.id),
            summary_entry.parent_id.clone(),
            EvidenceSourceKind::CompactionSummary,
            EvidenceAuthority::DerivedInference,
            summary.clone(),
            summary_entry.timestamp,
            Sensitivity::Personal,
            RetentionClass::CurrentState,
            vec![],
        );
        let legacy_id = legacy.id.clone();
        let workspace = root.path().join("workspace");
        let service = MemoryService::open(
            PersonalWorkspace::open(
                &workspace,
                keith_workspace::PersonalWorkspaceLimits::default(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap(),
            &profile,
            MemoryPolicy::default(),
        )
        .unwrap();
        service
            .observatory()
            .apply(
                vec![ObservatoryMutation::Observe(legacy)],
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let old_bytes = fs::read(workspace.join(".keith/memory-vault.jsonl")).unwrap();
        ingest_history(&service, &sessions, &profile, &session);
        let snapshot = service.observatory().evidence_snapshot().unwrap();
        let repaired = snapshot.get(&legacy_id).unwrap();
        assert_eq!(repaired.authority, EvidenceAuthority::DerivedInference);
        assert!(!repaired.causal.as_ref().unwrap().source_roots.is_empty());
        assert!(
            repaired
                .causal
                .as_ref()
                .unwrap()
                .source_roots
                .iter()
                .all(|root| root.source_entry != summary_entry.id)
        );
        assert!(!snapshot.values().any(|record| record.source_identity
            == format!("session:{session}:entry:{}", summary_entry.id)));
        assert!(
            fs::read(workspace.join(".keith/memory-vault.jsonl"))
                .unwrap()
                .starts_with(&old_bytes)
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
        ingest_history(&service, &session_root, &profile_id, &session_id);
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
        crate::test_sources::ingest(
            &workspace_root,
            &profile_id,
            &SessionId::new(),
            &[entry],
            UtcTimestamp::UNIX_EPOCH,
        );
        assert_eq!(service.observatory().evidence_snapshot().unwrap().len(), 1);
        assert_eq!(service.observatory().revision().unwrap(), 2);
    }
}
