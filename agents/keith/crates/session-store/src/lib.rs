#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs::{self, File, OpenOptions};
use std::io::{Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};

use fs2::FileExt;
use keith_agent_types::{
    ActionId, ArtifactId, CURRENT_SCHEMA_VERSION, ChildId, EntityId, EntryId, Generation, GoalId,
    ProfileId, Revision, RootTreeId, SchemaVersion, SessionId, ToolCallId, ToolFailure, TurnId,
    UtcTimestamp, WorkerId, WorkspaceId, canonical_json_bytes,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const MANIFEST_FILE: &str = "manifest.json";
const HISTORY_FILE: &str = "history.jsonl";
const WRITER_LOCK_FILE: &str = ".writer.lock";
const QUARANTINE_FILE: &str = "quarantine.json";

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SessionKind {
    Root,
    DurableChild,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NewSession {
    pub kind: SessionKind,
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub parent_session_id: Option<SessionId>,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub created_at: UtcTimestamp,
    pub label: Option<String>,
    pub profile_snapshot: Option<ProfileSnapshotMetadata>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileSnapshotMetadata {
    pub version: SchemaVersion,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub revision: Revision,
    pub captured_at: UtcTimestamp,
    pub digest: String,
    pub snapshot: serde_json::Value,
}

impl ProfileSnapshotMetadata {
    /// Creates metadata with a digest over the canonical structured snapshot.
    ///
    /// # Errors
    ///
    /// Returns an error when the snapshot cannot be serialized canonically.
    pub fn new(
        profile_id: ProfileId,
        workspace_id: WorkspaceId,
        revision: Revision,
        captured_at: UtcTimestamp,
        snapshot: serde_json::Value,
    ) -> Result<Self, SessionStoreError> {
        let digest = snapshot_digest(&snapshot)?;
        Ok(Self {
            version: CURRENT_SCHEMA_VERSION,
            profile_id,
            workspace_id,
            revision,
            captured_at,
            digest,
            snapshot,
        })
    }

    /// # Errors
    ///
    /// Returns an error when the stored digest or schema version is invalid.
    pub fn verify(&self) -> Result<(), SessionStoreError> {
        if self.version.major != CURRENT_SCHEMA_VERSION.major
            || self.version.minor > CURRENT_SCHEMA_VERSION.minor
            || self.digest != snapshot_digest(&self.snapshot)?
        {
            return Err(SessionStoreError::InvalidProfileSnapshot(
                "schema version or digest mismatch".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SessionManifest {
    pub version: SchemaVersion,
    pub kind: SessionKind,
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub parent_session_id: Option<SessionId>,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub created_at: UtcTimestamp,
    pub active_leaf: Option<EntryId>,
    #[serde(default)]
    pub compaction_generation: u64,
    pub label: Option<String>,
    #[serde(default)]
    pub profile_snapshot: Option<ProfileSnapshotMetadata>,
    pub branch_labels: BTreeMap<String, EntryId>,
    pub archived: bool,
}

impl From<NewSession> for SessionManifest {
    fn from(value: NewSession) -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            kind: value.kind,
            session_id: value.session_id,
            root_tree_id: value.root_tree_id,
            parent_session_id: value.parent_session_id,
            profile_id: value.profile_id,
            workspace_id: value.workspace_id,
            created_at: value.created_at,
            active_leaf: None,
            compaction_generation: 0,
            label: value.label,
            profile_snapshot: value.profile_snapshot,
            branch_labels: BTreeMap::new(),
            archived: false,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MessageRole {
    User,
    Assistant,
    Tool,
    System,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReasoningVisibility {
    Hidden,
    Summary,
    Visible,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "content")]
pub enum ContentBlock {
    Text {
        text: String,
    },
    Reasoning {
        text: String,
        visibility: ReasoningVisibility,
    },
    Artifact {
        artifact_id: ArtifactId,
        media_type: String,
    },
    Resource {
        uri: String,
        title: Option<String>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct StoredMessage {
    pub role: MessageRole,
    pub content: Vec<ContentBlock>,
    pub provider_metadata: BTreeMap<String, String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TurnTerminalStatus {
    Completed,
    Failed,
    Cancelled,
    Exhausted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state", content = "detail")]
pub enum TurnObligationState {
    Accepted,
    Running {
        step: u32,
    },
    FinalizationPending {
        candidate_id: Option<EntryId>,
    },
    Finalized {
        final_id: EntryId,
        terminal_id: EntryId,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StepBoundaryState {
    Started,
    Completed,
    Failed,
    Cancelled,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CompactionTrigger {
    Pressure,
    ProviderOverflow,
    Manual,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CompactionFailureStage {
    Selection,
    Summary,
    Changed,
    Commit,
    Persistence,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "outcome", content = "detail")]
pub enum CompactionOutcome {
    Success,
    Failed {
        stage: CompactionFailureStage,
        error: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct AuthoritativeTurnSnapshot {
    pub session_id: SessionId,
    pub turn_id: TurnId,
    pub final_id: EntryId,
    pub delivery_outbox_id: EntryId,
    pub terminal_id: EntryId,
    pub status: TurnTerminalStatus,
    pub execution_succeeded: bool,
    pub final_created: bool,
    pub artifacts_persisted: bool,
    pub delivery_enqueued: bool,
    pub delivery_acknowledged: bool,
    pub action_id: Option<ActionId>,
    pub artifact_ids: Vec<ArtifactId>,
    pub assistant_final: StoredMessage,
    pub detail: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "payload")]
pub enum SessionEntryPayload {
    UserMessage {
        message: StoredMessage,
    },
    AssistantMessage {
        message: StoredMessage,
    },
    AssistantActivity {
        turn_id: TurnId,
        message: StoredMessage,
    },
    AssistantFinalCandidate {
        turn_id: TurnId,
        message: StoredMessage,
        input_tokens: u64,
        output_tokens: u64,
        cached_input_tokens: u64,
    },
    AssistantFinal {
        turn_id: TurnId,
        message: StoredMessage,
    },
    ControllerGuidance {
        turn_id: TurnId,
        source_id: String,
        text: String,
    },
    TurnObligation {
        action_id: ActionId,
        turn_id: TurnId,
        user_entry_id: EntryId,
        state: TurnObligationState,
    },
    StepBoundary {
        turn_id: TurnId,
        step: u32,
        provider_request_id: EntityId,
        state: StepBoundaryState,
        detail: Option<String>,
    },
    ToolCall {
        call_id: ToolCallId,
        name: String,
        arguments: serde_json::Value,
    },
    ToolResult {
        call_id: ToolCallId,
        content: Vec<ContentBlock>,
        is_error: bool,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        failure: Option<ToolFailure>,
    },
    TurnDeliveryOutbox {
        turn_id: TurnId,
        final_id: EntryId,
        action_id: Option<ActionId>,
        artifact_ids: Vec<ArtifactId>,
    },
    TerminalTurn {
        turn_id: TurnId,
        final_id: EntryId,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        delivery_outbox_id: Option<EntryId>,
        #[serde(default, skip_serializing_if = "Option::is_none")]
        authoritative_snapshot_id: Option<EntryId>,
        status: TurnTerminalStatus,
        execution_succeeded: bool,
        final_created: bool,
        artifacts_persisted: bool,
        delivery_enqueued: bool,
        detail: Option<String>,
    },
    AuthoritativeSnapshot {
        snapshot: AuthoritativeTurnSnapshot,
    },
    ModelChanged {
        provider: String,
        model: String,
    },
    ThinkingChanged {
        level: String,
    },
    Compaction {
        summary: String,
        compacted_through: EntryId,
    },
    CompactionStarted {
        compaction_id: EntityId,
        base_leaf: EntryId,
        range_start: EntryId,
        range_end: EntryId,
        source_entries: Vec<EntryId>,
        surface_generation: u64,
        trigger: CompactionTrigger,
    },
    CompactionSummary {
        compaction_id: EntityId,
        summary: String,
        raw_provider_output: String,
        source_entries: Vec<EntryId>,
        provider: Option<String>,
        model: Option<String>,
        max_output_tokens: u32,
        input_tokens: u64,
        output_tokens: u64,
        cached_input_tokens: u64,
        estimated_source_tokens: u64,
        estimated_summary_tokens: u64,
    },
    CompactionCheckpoint {
        compaction_id: EntityId,
        summary_id: EntryId,
        source_entries: Vec<EntryId>,
        compacted_through: EntryId,
        summary: String,
    },
    CompactionEnded {
        compaction_id: EntityId,
        outcome: CompactionOutcome,
    },
    MaintenanceFailure {
        turn_id: Option<TurnId>,
        subsystem: String,
        detail: String,
    },
    BranchSummary {
        summary: String,
    },
    GoalChanged {
        goal_id: GoalId,
        state: String,
    },
    PlanChanged {
        plan_id: EntityId,
        revision: u64,
    },
    ChildLinked {
        child_id: ChildId,
        child_session_id: SessionId,
    },
    Usage {
        input_tokens: u64,
        output_tokens: u64,
        cost_micros: Option<u64>,
    },
    Lifecycle {
        state: String,
        detail: Option<String>,
    },
    Custom {
        kind: String,
        value: serde_json::Value,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SessionEntry {
    pub version: SchemaVersion,
    pub id: EntryId,
    pub parent_id: Option<EntryId>,
    pub timestamp: UtcTimestamp,
    pub payload: SessionEntryPayload,
    pub checksum: String,
}

#[derive(Serialize)]
struct ChecksumInput<'a> {
    version: SchemaVersion,
    id: &'a EntryId,
    parent_id: &'a Option<EntryId>,
    timestamp: UtcTimestamp,
    payload: &'a SessionEntryPayload,
}

impl SessionEntry {
    /// # Errors
    ///
    /// Returns an error when the entry cannot be represented canonically as JSON.
    pub fn new(
        id: EntryId,
        parent_id: Option<EntryId>,
        timestamp: UtcTimestamp,
        payload: SessionEntryPayload,
    ) -> Result<Self, SessionStoreError> {
        let mut entry = Self {
            version: CURRENT_SCHEMA_VERSION,
            id,
            parent_id,
            timestamp,
            payload,
            checksum: String::new(),
        };
        entry.checksum = entry.expected_checksum()?;
        Ok(entry)
    }

    /// # Errors
    ///
    /// Returns an error when canonical serialization fails or the checksum differs.
    pub fn verify(&self) -> Result<(), SessionStoreError> {
        if self.version.major != CURRENT_SCHEMA_VERSION.major
            || self.version.minor > CURRENT_SCHEMA_VERSION.minor
        {
            return Err(SessionStoreError::UnsupportedVersion(self.version));
        }
        let expected = self.expected_checksum()?;
        if self.checksum == expected {
            Ok(())
        } else {
            Err(SessionStoreError::ChecksumMismatch(self.id.clone()))
        }
    }

    fn expected_checksum(&self) -> Result<String, SessionStoreError> {
        let bytes = canonical_json_bytes(&ChecksumInput {
            version: self.version,
            id: &self.id,
            parent_id: &self.parent_id,
            timestamp: self.timestamp,
            payload: &self.payload,
        })?;
        let digest = Sha256::digest(bytes);
        let mut checksum = String::with_capacity(64);
        for byte in digest {
            write!(&mut checksum, "{byte:02x}").map_err(|_| SessionStoreError::CorruptHistory {
                line: 0,
                reason: "checksum formatting failed".into(),
            })?;
        }
        Ok(checksum)
    }
}

#[derive(Clone, Debug, Default, PartialEq)]
pub struct SessionIndex {
    entries: BTreeMap<EntryId, SessionEntry>,
    children: BTreeMap<Option<EntryId>, Vec<EntryId>>,
}

impl SessionIndex {
    pub fn len(&self) -> usize {
        self.entries.len()
    }

    pub fn is_empty(&self) -> bool {
        self.entries.is_empty()
    }

    pub fn get(&self, id: &EntryId) -> Option<&SessionEntry> {
        self.entries.get(id)
    }

    pub fn children_of(&self, id: Option<&EntryId>) -> &[EntryId] {
        self.children.get(&id.cloned()).map_or(&[], Vec::as_slice)
    }

    /// # Errors
    ///
    /// Returns an error if the leaf is missing or its ancestry contains a cycle.
    pub fn ancestry(&self, leaf: &EntryId) -> Result<Vec<SessionEntry>, SessionStoreError> {
        let mut current = Some(leaf.clone());
        let mut seen = BTreeSet::new();
        let mut entries = Vec::new();
        while let Some(id) = current.take() {
            if !seen.insert(id.clone()) {
                return Err(SessionStoreError::AncestryCycle(id));
            }
            let entry = self
                .entries
                .get(&id)
                .ok_or_else(|| SessionStoreError::MissingEntry(id.clone()))?;
            entries.push(entry.clone());
            current.clone_from(&entry.parent_id);
        }
        entries.reverse();
        Ok(entries)
    }

    /// # Errors
    ///
    /// Returns an error when the selected branch cannot be reconstructed.
    pub fn compaction_request(
        &self,
        manifest: &SessionManifest,
        estimated_tokens: u64,
        policy: CompactionPolicy,
        protected_user_entry_id: Option<&EntryId>,
        trigger: CompactionTrigger,
    ) -> Result<Option<CompactionRequest>, SessionStoreError> {
        validate_compaction_policy(policy)?;
        if estimated_tokens < policy.trigger_tokens {
            return Ok(None);
        }
        let Some(selected_leaf) = &manifest.active_leaf else {
            return Ok(None);
        };
        let ancestry = self.ancestry(selected_leaf)?;
        let ended_compactions = ancestry
            .iter()
            .filter_map(|entry| match &entry.payload {
                SessionEntryPayload::CompactionEnded { compaction_id, .. } => {
                    Some(compaction_id.clone())
                }
                _ => None,
            })
            .collect::<BTreeSet<_>>();
        if ancestry.iter().any(|entry| {
            matches!(
                &entry.payload,
                SessionEntryPayload::CompactionStarted { compaction_id, .. }
                    if !ended_compactions.contains(compaction_id)
            )
        }) {
            return Err(SessionStoreError::CompactionBusy);
        }
        let previous_index = ancestry.iter().rposition(|entry| {
            matches!(
                entry.payload,
                SessionEntryPayload::Compaction { .. }
                    | SessionEntryPayload::CompactionEnded {
                        outcome: CompactionOutcome::Success,
                        ..
                    }
            )
        });
        let range_index = previous_index.map_or(0, |index| index + 1);
        let protected_index = protected_user_entry_id
            .and_then(|id| ancestry.iter().position(|entry| &entry.id == id))
            .unwrap_or(ancestry.len());
        if range_index >= protected_index {
            return Ok(None);
        }
        let mut retained_tokens = 0_u64;
        let mut cutoff = protected_index;
        while cutoff > range_index && retained_tokens < policy.target_tokens {
            cutoff -= 1;
            retained_tokens =
                retained_tokens.saturating_add(entry_estimated_tokens(&ancestry[cutoff]));
        }
        let end_index = ancestry[range_index..cutoff]
            .iter()
            .rposition(|entry| matches!(entry.payload, SessionEntryPayload::TerminalTurn { .. }))
            .map(|offset| range_index + offset);
        let Some(end_index) = end_index else {
            return Ok(None);
        };
        let range_start = &ancestry[range_index];
        let range_end = &ancestry[end_index];
        let source_entries = ancestry[range_index..=end_index]
            .iter()
            .map(|entry| entry.id.clone())
            .collect::<Vec<_>>();
        validate_balanced_compaction_range(&ancestry[range_index..=end_index])?;
        let estimated_source_tokens = ancestry[range_index..=end_index]
            .iter()
            .fold(0_u64, |total, entry| {
                total.saturating_add(entry_estimated_tokens(entry))
            });
        if estimated_source_tokens <= 1 {
            return Ok(None);
        }
        Ok(Some(CompactionRequest {
            id: EntityId::new(),
            session_id: manifest.session_id.clone(),
            selected_leaf: selected_leaf.clone(),
            started_entry_id: None,
            range_start: range_start.id.clone(),
            range_end: range_end.id.clone(),
            source_entries,
            previous_boundary: previous_index.map(|index| ancestry[index].id.clone()),
            surface_generation: manifest.compaction_generation,
            trigger,
            estimated_source_tokens,
            target_tokens: policy.target_tokens,
            max_summary_bytes: policy.max_summary_bytes,
            max_candidates: policy.max_candidates,
            max_candidate_bytes: policy.max_candidate_bytes,
        }))
    }

    /// # Errors
    ///
    /// Returns an error when the selected branch or a compaction boundary is inconsistent.
    pub fn reconstruct_context(
        &self,
        leaf: &EntryId,
    ) -> Result<ReconstructedContext, SessionStoreError> {
        let ancestry = self.ancestry(leaf)?;
        let mut model = None;
        let mut thinking_level = None;
        let mut compaction_summary = None;
        let mut boundary_index = None;
        for (index, entry) in ancestry.iter().enumerate() {
            match &entry.payload {
                SessionEntryPayload::ModelChanged {
                    provider,
                    model: selected_model,
                } => model = Some((provider.clone(), selected_model.clone())),
                SessionEntryPayload::ThinkingChanged { level } => {
                    thinking_level = Some(level.clone());
                }
                SessionEntryPayload::Compaction {
                    summary,
                    compacted_through,
                } => {
                    if !ancestry[..index]
                        .iter()
                        .any(|candidate| candidate.id == *compacted_through)
                    {
                        return Err(SessionStoreError::InvalidCompaction(
                            "compaction boundary is not on the selected ancestry".into(),
                        ));
                    }
                    compaction_summary = Some(summary.clone());
                    boundary_index = Some(index);
                }
                SessionEntryPayload::CompactionCheckpoint {
                    summary,
                    compacted_through,
                    source_entries,
                    ..
                } => {
                    if source_entries.last() != Some(compacted_through)
                        || !source_entries.iter().all(|source| {
                            ancestry[..index]
                                .iter()
                                .any(|candidate| candidate.id == *source)
                        })
                    {
                        return Err(SessionStoreError::InvalidCompaction(
                            "compaction checkpoint sources are not on the selected ancestry".into(),
                        ));
                    }
                    compaction_summary = Some(summary.clone());
                    boundary_index = Some(index);
                }
                SessionEntryPayload::UserMessage { .. }
                | SessionEntryPayload::AssistantMessage { .. }
                | SessionEntryPayload::AssistantActivity { .. }
                | SessionEntryPayload::AssistantFinalCandidate { .. }
                | SessionEntryPayload::AssistantFinal { .. }
                | SessionEntryPayload::ControllerGuidance { .. }
                | SessionEntryPayload::TurnObligation { .. }
                | SessionEntryPayload::StepBoundary { .. }
                | SessionEntryPayload::ToolCall { .. }
                | SessionEntryPayload::ToolResult { .. }
                | SessionEntryPayload::TurnDeliveryOutbox { .. }
                | SessionEntryPayload::TerminalTurn { .. }
                | SessionEntryPayload::AuthoritativeSnapshot { .. }
                | SessionEntryPayload::CompactionStarted { .. }
                | SessionEntryPayload::CompactionSummary { .. }
                | SessionEntryPayload::CompactionEnded { .. }
                | SessionEntryPayload::MaintenanceFailure { .. }
                | SessionEntryPayload::BranchSummary { .. }
                | SessionEntryPayload::GoalChanged { .. }
                | SessionEntryPayload::PlanChanged { .. }
                | SessionEntryPayload::ChildLinked { .. }
                | SessionEntryPayload::Usage { .. }
                | SessionEntryPayload::Lifecycle { .. }
                | SessionEntryPayload::Custom { .. } => {}
            }
        }
        let entries = boundary_index.map_or_else(
            || ancestry.clone(),
            |index| reconstructed_surface_tail(&ancestry, index),
        );
        Ok(ReconstructedContext {
            selected_leaf: leaf.clone(),
            compaction_summary,
            model,
            thinking_level,
            entries,
        })
    }

    fn insert(&mut self, entry: SessionEntry) -> Result<(), SessionStoreError> {
        if self.entries.contains_key(&entry.id) {
            return Err(SessionStoreError::DuplicateEntry(entry.id));
        }
        if let Some(parent_id) = &entry.parent_id {
            if !self.entries.contains_key(parent_id) {
                return Err(SessionStoreError::MissingParent {
                    entry: entry.id,
                    parent: parent_id.clone(),
                });
            }
        } else if !self.entries.is_empty() {
            return Err(SessionStoreError::MultipleRoots(entry.id));
        }
        self.children
            .entry(entry.parent_id.clone())
            .or_default()
            .push(entry.id.clone());
        self.entries.insert(entry.id.clone(), entry);
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct WriterIdentity {
    pub worker_id: WorkerId,
    pub owner_instance: EntityId,
    pub generation: Generation,
    pub acquired_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QuarantineRecord {
    pub session_id: SessionId,
    pub detected_at: UtcTimestamp,
    pub line: usize,
    pub reason: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RecoveryReport {
    pub entries: usize,
    pub discarded_tail_bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RepairIssue {
    pub line: usize,
    pub reason: String,
    pub final_unterminated: bool,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SessionExport {
    pub version: SchemaVersion,
    pub manifest: SessionManifest,
    pub entries: Vec<SessionEntry>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct LegacySessionExportLimits {
    pub max_manifest_bytes: usize,
    pub max_history_bytes: usize,
    pub max_entries: usize,
}

impl Default for LegacySessionExportLimits {
    fn default() -> Self {
        Self {
            max_manifest_bytes: 64 * 1_024,
            max_history_bytes: 256 * 1_024 * 1_024,
            max_entries: 1_000_000,
        }
    }
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacySessionManifestV0 {
    schema_version: u16,
    kind: SessionKind,
    session_id: SessionId,
    root_tree_id: RootTreeId,
    parent_session_id: Option<SessionId>,
    profile_id: ProfileId,
    workspace_id: WorkspaceId,
    created_at: UtcTimestamp,
    active_leaf: Option<EntryId>,
    label: Option<String>,
    branch_labels: BTreeMap<String, EntryId>,
    archived: bool,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacySessionEntryV0 {
    id: EntryId,
    parent_id: Option<EntryId>,
    timestamp: UtcTimestamp,
    payload: SessionEntryPayload,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct CompactionPolicy {
    pub trigger_tokens: u64,
    pub target_tokens: u64,
    pub max_summary_bytes: usize,
    pub max_candidates: usize,
    pub max_candidate_bytes: usize,
}

impl Default for CompactionPolicy {
    fn default() -> Self {
        Self {
            trigger_tokens: 96_000,
            target_tokens: 32_000,
            max_summary_bytes: 256 * 1_024,
            max_candidates: 128,
            max_candidate_bytes: 16 * 1_024,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CompactionRequest {
    pub id: EntityId,
    pub session_id: SessionId,
    pub selected_leaf: EntryId,
    pub started_entry_id: Option<EntryId>,
    pub range_start: EntryId,
    pub range_end: EntryId,
    pub source_entries: Vec<EntryId>,
    pub previous_boundary: Option<EntryId>,
    pub surface_generation: u64,
    pub trigger: CompactionTrigger,
    pub estimated_source_tokens: u64,
    pub target_tokens: u64,
    pub max_summary_bytes: usize,
    pub max_candidates: usize,
    pub max_candidate_bytes: usize,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MemoryKind {
    Preference,
    PersonalFact,
    ProjectContext,
    Routine,
    Relationship,
    Commitment,
    Procedure,
    DailySummary,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryRecallLink {
    pub query_identity: String,
    pub archive_revision: u64,
    pub source_entries: Vec<EntryId>,
    pub result_id: EntityId,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MemoryActivationKind {
    ConfirmedAnchor,
    ActiveWork,
    Correction,
    RelevantEvidence,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryActivationEvidence {
    pub kind: MemoryActivationKind,
    pub evidence_id: EntityId,
    pub source_entries: Vec<EntryId>,
    pub source_digests: Vec<String>,
    pub source_identity: String,
    pub content_digest: String,
    pub authority: String,
    pub validity: String,
    pub text: String,
    pub token_price: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryActivationCoverage {
    pub examined: usize,
    pub matched: usize,
    pub eligible: usize,
    pub selected: usize,
    pub excluded_current_thread: usize,
    pub deduplicated: usize,
    pub truncated: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryActivationManifest {
    pub manifest_id: String,
    pub selector_version: String,
    pub query_identity: String,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub archive_revision: u64,
    pub evidence: Vec<MemoryActivationEvidence>,
    pub coverage: MemoryActivationCoverage,
    pub token_price: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Sensitivity {
    Public,
    Personal,
    Sensitive,
    Secret,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RetentionClass {
    CurrentState,
    Daily,
    Durable,
    DoNotStore,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryCandidateDraft {
    pub id: EntityId,
    pub kind: MemoryKind,
    pub text: String,
    pub source_entries: Vec<EntryId>,
    pub sensitivity: Sensitivity,
    pub retention: RetentionClass,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommitmentDraft {
    pub id: EntityId,
    pub description: String,
    pub source_entries: Vec<EntryId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CompactionOutput {
    pub request_id: EntityId,
    pub session_summary: String,
    pub raw_provider_output: String,
    pub provider: Option<String>,
    pub model: Option<String>,
    pub max_output_tokens: u32,
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cached_input_tokens: u64,
    pub memory_candidates: Vec<MemoryCandidateDraft>,
    pub daily_entry: Option<String>,
    pub open_commitments: Vec<CommitmentDraft>,
    pub unresolved_items: Vec<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CompactionEmission {
    pub boundary: SessionEntry,
    pub memory_candidates: Vec<MemoryCandidateDraft>,
    pub daily_entry: Option<String>,
    pub open_commitments: Vec<CommitmentDraft>,
    pub unresolved_items: Vec<String>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct ReconstructedContext {
    pub selected_leaf: EntryId,
    pub compaction_summary: Option<String>,
    pub model: Option<(String, String)>,
    pub thinking_level: Option<String>,
    pub entries: Vec<SessionEntry>,
}

#[derive(Debug, Error)]
pub enum SessionStoreError {
    #[error("session storage I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("session JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("session {0} already exists")]
    AlreadyExists(SessionId),
    #[error("session {0} was not found")]
    NotFound(SessionId),
    #[error("session {0} has an active local writer")]
    WriterLocked(SessionId),
    #[error("session path escaped the canonical session root")]
    PathEscape,
    #[error("session {0} is archived")]
    Archived(SessionId),
    #[error("session {0} must be archived before deletion")]
    NotArchived(SessionId),
    #[error("session directory contains an unexpected entry")]
    UnexpectedEntry,
    #[error("session {0} is quarantined")]
    Quarantined(SessionId),
    #[error("unsupported session schema {0}")]
    UnsupportedVersion(SchemaVersion),
    #[error("checksum mismatch for entry {0}")]
    ChecksumMismatch(EntryId),
    #[error("duplicate entry {0}")]
    DuplicateEntry(EntryId),
    #[error("entry {entry} references missing parent {parent}")]
    MissingParent { entry: EntryId, parent: EntryId },
    #[error("entry {0} introduced a second history root")]
    MultipleRoots(EntryId),
    #[error("entry {0} was not found")]
    MissingEntry(EntryId),
    #[error("entry ancestry contains a cycle at {0}")]
    AncestryCycle(EntryId),
    #[error("branch label is empty or already exists")]
    InvalidLabel,
    #[error("history corruption at line {line}: {reason}")]
    CorruptHistory { line: usize, reason: String },
    #[error("compaction configuration or output is invalid: {0}")]
    InvalidCompaction(String),
    #[error("terminal turn is invalid: {0}")]
    InvalidTerminalTurn(String),
    #[error("compaction selected leaf changed before commit")]
    StaleCompaction,
    #[error("session has an unmatched compaction start")]
    CompactionBusy,
    #[error("profile snapshot is invalid: {0}")]
    InvalidProfileSnapshot(String),
    #[error("profile snapshot changed before the deliberate update")]
    StaleProfileSnapshot,
    #[error("legacy session export exceeded its configured bound")]
    LegacyExportLimit,
}

#[derive(Clone, Debug)]
pub struct SessionStore {
    root: PathBuf,
    sessions: PathBuf,
}

impl SessionStore {
    /// # Errors
    ///
    /// Returns an error when the storage root cannot be created or canonicalized.
    pub fn open(root: impl AsRef<Path>) -> Result<Self, SessionStoreError> {
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        let sessions = root.join("sessions");
        fs::create_dir_all(&sessions)?;
        let sessions = fs::canonicalize(sessions)?;
        if sessions.parent() != Some(root.as_path()) {
            return Err(SessionStoreError::PathEscape);
        }
        Ok(Self { root, sessions })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// # Errors
    ///
    /// Returns an error when the session exists or its durable files cannot be created.
    pub fn create(&self, session: NewSession) -> Result<SessionManifest, SessionStoreError> {
        validate_new_session(&session)?;
        let session_id = session.session_id.clone();
        let directory = self.sessions.join(session_id.to_string());
        match fs::create_dir(&directory) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => {
                return Err(SessionStoreError::AlreadyExists(session_id));
            }
            Err(error) => return Err(error.into()),
        }
        OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(directory.join(HISTORY_FILE))?
            .sync_all()?;
        let manifest = SessionManifest::from(session);
        write_manifest(&directory, &manifest)?;
        sync_directory(&directory)?;
        Ok(manifest)
    }

    /// # Errors
    ///
    /// Returns an error when a manifest cannot be read or decoded.
    pub fn manifest(&self, session_id: &SessionId) -> Result<SessionManifest, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        read_manifest(&directory)
    }

    /// # Errors
    ///
    /// Returns an error when any discovered manifest is corrupt or inaccessible.
    pub fn discover(&self) -> Result<Vec<SessionManifest>, SessionStoreError> {
        let mut manifests = Vec::new();
        for entry in fs::read_dir(&self.sessions)? {
            let entry = entry?;
            if entry.file_type()?.is_symlink() || !entry.file_type()?.is_dir() {
                continue;
            }
            if !entry.path().join(MANIFEST_FILE).is_file() {
                continue;
            }
            manifests.push(read_manifest(&entry.path())?);
        }
        manifests.sort_by(|left, right| left.session_id.cmp(&right.session_id));
        Ok(manifests)
    }

    /// # Errors
    ///
    /// Returns an error when the session is missing, quarantined, escaped, or already locked.
    pub fn acquire_writer(
        &self,
        session_id: &SessionId,
        identity: WriterIdentity,
    ) -> Result<SessionWriter, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        if directory.join(QUARANTINE_FILE).exists() {
            return Err(SessionStoreError::Quarantined(session_id.clone()));
        }
        let mut lock_file = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(directory.join(WRITER_LOCK_FILE))?;
        lock_file
            .try_lock_exclusive()
            .map_err(|_| SessionStoreError::WriterLocked(session_id.clone()))?;
        lock_file.set_len(0)?;
        lock_file.seek(SeekFrom::Start(0))?;
        lock_file.write_all(&canonical_json_bytes(&identity)?)?;
        lock_file.sync_all()?;
        let manifest = read_manifest(&directory)?;
        if manifest.archived {
            return Err(SessionStoreError::Archived(session_id.clone()));
        }
        Ok(SessionWriter {
            directory,
            manifest,
            lock_file,
            identity,
        })
    }

    /// # Errors
    ///
    /// Returns an error when durable history is corrupt or inaccessible.
    pub fn load_index(&self, session_id: &SessionId) -> Result<SessionIndex, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        if directory.join(QUARANTINE_FILE).exists() {
            return Err(SessionStoreError::Quarantined(session_id.clone()));
        }
        parse_complete_history(&directory.join(HISTORY_FILE))
    }

    /// # Errors
    ///
    /// Returns an error when the history cannot be inspected, truncated, or quarantined.
    pub fn recover(
        &self,
        session_id: &SessionId,
        detected_at: UtcTimestamp,
    ) -> Result<RecoveryReport, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        if directory.join(QUARANTINE_FILE).exists() {
            return Err(SessionStoreError::Quarantined(session_id.clone()));
        }
        let history_path = directory.join(HISTORY_FILE);
        let bytes = fs::read(&history_path)?;
        let inspection = inspect_bytes(&bytes);
        if let Some(issue) = inspection.issues.first() {
            if issue.final_unterminated {
                let discarded = u64::try_from(bytes.len().saturating_sub(inspection.valid_bytes))
                    .map_err(|_| SessionStoreError::CorruptHistory {
                    line: issue.line,
                    reason: "history length exceeds supported range".into(),
                })?;
                let file = OpenOptions::new().write(true).open(&history_path)?;
                file.set_len(u64::try_from(inspection.valid_bytes).map_err(|_| {
                    SessionStoreError::CorruptHistory {
                        line: issue.line,
                        reason: "history length exceeds supported range".into(),
                    }
                })?)?;
                file.sync_all()?;
                reconcile_manifest_tail(&directory, &inspection.index)?;
                return Ok(RecoveryReport {
                    entries: inspection.index.len(),
                    discarded_tail_bytes: discarded,
                });
            }
            let quarantine = QuarantineRecord {
                session_id: session_id.clone(),
                detected_at,
                line: issue.line,
                reason: issue.reason.clone(),
            };
            atomic_write_json(&directory, QUARANTINE_FILE, &quarantine)?;
            return Err(SessionStoreError::Quarantined(session_id.clone()));
        }
        reconcile_manifest_tail(&directory, &inspection.index)?;
        Ok(RecoveryReport {
            entries: inspection.index.len(),
            discarded_tail_bytes: 0,
        })
    }

    /// # Errors
    ///
    /// Returns an error when raw history cannot be read.
    pub fn inspect_repair(
        &self,
        session_id: &SessionId,
    ) -> Result<Vec<RepairIssue>, SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        Ok(inspect_bytes(&fs::read(directory.join(HISTORY_FILE))?).issues)
    }

    /// # Errors
    ///
    /// Returns an error when a quarantined or corrupt session cannot be exported.
    pub fn export(&self, session_id: &SessionId) -> Result<SessionExport, SessionStoreError> {
        let manifest = self.manifest(session_id)?;
        let index = self.load_index(session_id)?;
        let mut entries = index.entries.into_values().collect::<Vec<_>>();
        entries.sort_by(|left, right| left.timestamp.cmp(&right.timestamp));
        Ok(SessionExport {
            version: CURRENT_SCHEMA_VERSION,
            manifest,
            entries,
        })
    }

    /// # Errors
    ///
    /// Returns an error when raw session files cannot be read.
    pub fn export_raw(
        &self,
        session_id: &SessionId,
    ) -> Result<(Vec<u8>, Vec<u8>), SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        Ok((
            fs::read(directory.join(MANIFEST_FILE))?,
            fs::read(directory.join(HISTORY_FILE))?,
        ))
    }

    /// Converts the supported v0 manifest and JSONL history into a current standalone export.
    ///
    /// This function never writes either input and is deliberately separate from ordinary startup.
    ///
    /// # Errors
    ///
    /// Returns an error for unsupported versions, configured bounds, malformed JSONL, or invalid
    /// ancestry and manifest references.
    pub fn migrate_legacy_export(
        manifest_bytes: &[u8],
        history_bytes: &[u8],
        limits: LegacySessionExportLimits,
    ) -> Result<SessionExport, SessionStoreError> {
        if limits.max_manifest_bytes == 0
            || limits.max_history_bytes == 0
            || limits.max_entries == 0
            || manifest_bytes.len() > limits.max_manifest_bytes
            || history_bytes.len() > limits.max_history_bytes
        {
            return Err(SessionStoreError::LegacyExportLimit);
        }
        let legacy: LegacySessionManifestV0 = serde_json::from_slice(manifest_bytes)?;
        if legacy.schema_version != 0 {
            return Err(SessionStoreError::UnsupportedVersion(SchemaVersion::new(
                legacy.schema_version,
                0,
            )));
        }
        validate_new_session(&NewSession {
            kind: legacy.kind,
            session_id: legacy.session_id.clone(),
            root_tree_id: legacy.root_tree_id.clone(),
            parent_session_id: legacy.parent_session_id.clone(),
            profile_id: legacy.profile_id.clone(),
            workspace_id: legacy.workspace_id.clone(),
            created_at: legacy.created_at,
            label: legacy.label.clone(),
            profile_snapshot: None,
        })?;
        let mut index = SessionIndex::default();
        for (offset, line) in history_bytes.split(|byte| *byte == b'\n').enumerate() {
            if line.is_empty() {
                continue;
            }
            if index.len() >= limits.max_entries {
                return Err(SessionStoreError::LegacyExportLimit);
            }
            let old: LegacySessionEntryV0 = serde_json::from_slice(line).map_err(|error| {
                SessionStoreError::CorruptHistory {
                    line: offset + 1,
                    reason: error.to_string(),
                }
            })?;
            index.insert(SessionEntry::new(
                old.id,
                old.parent_id,
                old.timestamp,
                old.payload,
            )?)?;
        }
        let known = |id: &EntryId| index.get(id).is_some();
        if legacy.active_leaf.as_ref().is_some_and(|id| !known(id))
            || legacy.branch_labels.values().any(|id| !known(id))
        {
            return Err(SessionStoreError::CorruptHistory {
                line: 0,
                reason: "legacy manifest references an unknown history entry".into(),
            });
        }
        let mut entries = index.entries.into_values().collect::<Vec<_>>();
        entries.sort_by(|left, right| left.timestamp.cmp(&right.timestamp));
        Ok(SessionExport {
            version: CURRENT_SCHEMA_VERSION,
            manifest: SessionManifest {
                version: CURRENT_SCHEMA_VERSION,
                kind: legacy.kind,
                session_id: legacy.session_id,
                root_tree_id: legacy.root_tree_id,
                parent_session_id: legacy.parent_session_id,
                profile_id: legacy.profile_id,
                workspace_id: legacy.workspace_id,
                created_at: legacy.created_at,
                active_leaf: legacy.active_leaf,
                compaction_generation: 0,
                label: legacy.label,
                profile_snapshot: None,
                branch_labels: legacy.branch_labels,
                archived: legacy.archived,
            },
            entries,
        })
    }

    /// Archives a session while holding its ordinary writer lease.
    ///
    /// # Errors
    ///
    /// Returns an error when the writer lease or durable manifest update fails.
    pub fn archive_session(
        &self,
        session_id: &SessionId,
        identity: WriterIdentity,
    ) -> Result<(), SessionStoreError> {
        let mut writer = self.acquire_writer(session_id, identity)?;
        writer.archive()
    }

    /// Permanently deletes the known files of an explicitly archived session.
    ///
    /// # Errors
    ///
    /// Returns an error for live sessions, active writers, unexpected entries, or I/O failure.
    pub fn delete_archived(&self, session_id: &SessionId) -> Result<(), SessionStoreError> {
        let directory = self.session_directory(session_id)?;
        let manifest = read_manifest(&directory)?;
        if !manifest.archived {
            return Err(SessionStoreError::NotArchived(session_id.clone()));
        }
        let lock_path = directory.join(WRITER_LOCK_FILE);
        let lock = OpenOptions::new()
            .create(true)
            .truncate(false)
            .read(true)
            .write(true)
            .open(&lock_path)?;
        lock.try_lock_exclusive()
            .map_err(|_| SessionStoreError::WriterLocked(session_id.clone()))?;
        let mut removable = Vec::new();
        for entry in fs::read_dir(&directory)? {
            let entry = entry?;
            let metadata = entry.file_type()?;
            let name = entry.file_name();
            let known = matches!(
                name.to_str(),
                Some(MANIFEST_FILE | HISTORY_FILE | WRITER_LOCK_FILE | QUARANTINE_FILE)
            );
            if !known || metadata.is_dir() || metadata.is_symlink() {
                return Err(SessionStoreError::UnexpectedEntry);
            }
            if name != WRITER_LOCK_FILE {
                removable.push(entry.path());
            }
        }
        for path in removable {
            fs::remove_file(path)?;
        }
        drop(lock);
        fs::remove_file(lock_path)?;
        fs::remove_dir(directory)?;
        sync_directory(&self.sessions)
    }

    fn session_directory(&self, session_id: &SessionId) -> Result<PathBuf, SessionStoreError> {
        let candidate = self.sessions.join(session_id.to_string());
        let metadata = match fs::symlink_metadata(&candidate) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Err(SessionStoreError::NotFound(session_id.clone()));
            }
            Err(error) => return Err(error.into()),
        };
        if metadata.file_type().is_symlink() || !metadata.is_dir() {
            return Err(SessionStoreError::PathEscape);
        }
        let canonical = fs::canonicalize(candidate)?;
        if canonical.parent() != Some(self.sessions.as_path()) {
            return Err(SessionStoreError::PathEscape);
        }
        Ok(canonical)
    }
}

fn reconcile_manifest_tail(
    directory: &Path,
    index: &SessionIndex,
) -> Result<(), SessionStoreError> {
    let mut manifest = read_manifest(directory)?;
    let mut leaf = manifest.active_leaf.clone();
    let mut generation = manifest.compaction_generation;
    loop {
        let children = index.children.get(&leaf).map_or(&[][..], Vec::as_slice);
        if children.len() != 1 {
            break;
        }
        let child = children[0].clone();
        if let Some(entry) = index.entries.get(&child)
            && let SessionEntryPayload::CompactionEnded {
                compaction_id,
                outcome: CompactionOutcome::Success,
            } = &entry.payload
            && let Some(surface_generation) =
                index
                    .entries
                    .values()
                    .find_map(|candidate| match &candidate.payload {
                        SessionEntryPayload::CompactionStarted {
                            compaction_id: started_id,
                            surface_generation,
                            ..
                        } if started_id == compaction_id => Some(*surface_generation),
                        _ => None,
                    })
        {
            generation = generation.max(surface_generation.saturating_add(1));
        }
        leaf = Some(child);
    }
    if leaf != manifest.active_leaf || generation != manifest.compaction_generation {
        manifest.active_leaf = leaf;
        manifest.compaction_generation = generation;
        write_manifest(directory, &manifest)?;
    }
    Ok(())
}

pub struct SessionWriter {
    directory: PathBuf,
    manifest: SessionManifest,
    lock_file: File,
    identity: WriterIdentity,
}

impl SessionWriter {
    pub fn manifest(&self) -> &SessionManifest {
        &self.manifest
    }

    pub fn identity(&self) -> &WriterIdentity {
        &self.identity
    }

    /// Durably records the obligation created by accepted ingress work.
    ///
    /// # Errors
    ///
    /// Returns an error when the ingress entry is missing or the turn was already accepted differently.
    pub fn accept_turn(
        &mut self,
        timestamp: UtcTimestamp,
        action_id: ActionId,
        turn_id: TurnId,
        user_entry_id: EntryId,
    ) -> Result<SessionEntry, SessionStoreError> {
        self.ensure_writable()?;
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        let ingress_entry = index
            .entries
            .get(&user_entry_id)
            .ok_or_else(|| SessionStoreError::MissingEntry(user_entry_id.clone()))?;
        if !matches!(
            ingress_entry.payload,
            SessionEntryPayload::UserMessage { .. }
                | SessionEntryPayload::ControllerGuidance { .. }
        ) {
            return Err(SessionStoreError::InvalidTerminalTurn(
                "turn obligation does not reference accepted ingress".into(),
            ));
        }
        if let Some(existing) = index.entries.values().find(|entry| {
            matches!(
                &entry.payload,
                SessionEntryPayload::TurnObligation {
                    turn_id: existing_turn,
                    ..
                } if existing_turn == &turn_id
            )
        }) {
            if matches!(
                &existing.payload,
                SessionEntryPayload::TurnObligation {
                    action_id: existing_action,
                    user_entry_id: existing_user,
                    ..
                } if existing_action == &action_id && existing_user == &user_entry_id
            ) {
                return Ok(existing.clone());
            }
            return Err(SessionStoreError::InvalidTerminalTurn(
                "turn obligation conflicts with an existing accepted turn".into(),
            ));
        }
        self.append(
            self.manifest.active_leaf.clone(),
            timestamp,
            SessionEntryPayload::TurnObligation {
                action_id,
                turn_id,
                user_entry_id,
                state: TurnObligationState::Accepted,
            },
        )
    }

    /// Persists a complete provider-authored final candidate before completion is projected.
    ///
    /// # Errors
    ///
    /// Returns an error for incomplete content or a conflicting candidate for the same turn.
    pub fn append_final_candidate(
        &mut self,
        timestamp: UtcTimestamp,
        turn_id: TurnId,
        message: StoredMessage,
        input_tokens: u64,
        output_tokens: u64,
        cached_input_tokens: u64,
    ) -> Result<SessionEntry, SessionStoreError> {
        self.ensure_writable()?;
        validate_assistant_candidate(&message)?;
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        if let Some(existing) = index.entries.values().find(|entry| {
            matches!(
                &entry.payload,
                SessionEntryPayload::AssistantFinalCandidate {
                    turn_id: existing_turn,
                    ..
                } if existing_turn == &turn_id
            )
        }) {
            if matches!(
                &existing.payload,
                SessionEntryPayload::AssistantFinalCandidate {
                    message: existing_message,
                    input_tokens: existing_input,
                    output_tokens: existing_output,
                    cached_input_tokens: existing_cached,
                    ..
                } if existing_message == &message
                    && *existing_input == input_tokens
                    && *existing_output == output_tokens
                    && *existing_cached == cached_input_tokens
            ) {
                return Ok(existing.clone());
            }
            return Err(SessionStoreError::InvalidTerminalTurn(
                "turn has conflicting final candidates".into(),
            ));
        }
        self.append(
            self.manifest.active_leaf.clone(),
            timestamp,
            SessionEntryPayload::AssistantFinalCandidate {
                turn_id,
                message,
                input_tokens,
                output_tokens,
                cached_input_tokens,
            },
        )
    }

    /// # Errors
    ///
    /// Returns an error when the parent is invalid or the append cannot be made durable.
    pub fn append(
        &mut self,
        parent_id: Option<EntryId>,
        timestamp: UtcTimestamp,
        payload: SessionEntryPayload,
    ) -> Result<SessionEntry, SessionStoreError> {
        self.ensure_writable()?;
        let history_path = self.directory.join(HISTORY_FILE);
        let mut index = parse_complete_history(&history_path)?;
        if index.is_empty()
            && let Some(parent) = &parent_id
        {
            return Err(SessionStoreError::MissingEntry(parent.clone()));
        }
        if !index.is_empty() {
            let parent = parent_id
                .as_ref()
                .ok_or_else(|| SessionStoreError::MultipleRoots(EntryId::new()))?;
            if !index.entries.contains_key(parent) {
                return Err(SessionStoreError::MissingEntry(parent.clone()));
            }
        }
        let entry = SessionEntry::new(EntryId::new(), parent_id, timestamp, payload)?;
        index.insert(entry.clone())?;
        let mut bytes = canonical_json_bytes(&entry)?;
        bytes.push(b'\n');
        let mut history = OpenOptions::new().append(true).open(&history_path)?;
        history.write_all(&bytes)?;
        history.sync_all()?;
        let mut next_manifest = self.manifest.clone();
        next_manifest.active_leaf = Some(entry.id.clone());
        write_manifest(&self.directory, &next_manifest)?;
        self.manifest = next_manifest;
        Ok(entry)
    }

    /// Atomically appends the final, delivery intent, authoritative snapshot, and terminal.
    /// Repeating the same turn finalization repairs publication and returns the committed pair.
    ///
    /// # Errors
    ///
    /// Returns an error when the history is inconsistent or the pair cannot be durably appended.
    #[allow(clippy::too_many_arguments, clippy::too_many_lines)]
    pub fn append_finalized_turn(
        &mut self,
        timestamp: UtcTimestamp,
        turn_id: &TurnId,
        fallback_message: StoredMessage,
        fallback_status: TurnTerminalStatus,
        fallback_execution_succeeded: bool,
        artifacts_persisted: bool,
        action_id: Option<ActionId>,
        artifact_ids: Vec<ArtifactId>,
        detail: Option<String>,
    ) -> Result<(SessionEntry, SessionEntry), SessionStoreError> {
        self.ensure_writable()?;
        let history_path = self.directory.join(HISTORY_FILE);
        let mut index = parse_history_for_finalization(&history_path)?;
        let candidate = index
            .entries
            .values()
            .find_map(|entry| match &entry.payload {
                SessionEntryPayload::AssistantFinalCandidate {
                    turn_id: candidate_turn,
                    message,
                    ..
                } if candidate_turn == turn_id => Some((entry.id.clone(), message.clone())),
                _ => None,
            });
        let (message, status, execution_succeeded, detail) = if let Some((_, message)) = &candidate
        {
            (message.clone(), TurnTerminalStatus::Completed, true, None)
        } else {
            (
                fallback_message,
                fallback_status,
                fallback_execution_succeeded,
                detail,
            )
        };
        if let Some(terminal) = index
            .entries
            .values()
            .find(|entry| {
                matches!(
                    &entry.payload,
                    SessionEntryPayload::TerminalTurn {
                        turn_id: existing,
                        ..
                    } if existing == turn_id
                )
            })
            .cloned()
        {
            let SessionEntryPayload::TerminalTurn {
                final_id,
                delivery_outbox_id,
                authoritative_snapshot_id,
                ..
            } = &terminal.payload
            else {
                unreachable!("terminal predicate selected a terminal payload")
            };
            let final_entry = index
                .entries
                .get(final_id)
                .ok_or_else(|| SessionStoreError::MissingEntry(final_id.clone()))?;
            if !matches!(
                &final_entry.payload,
                SessionEntryPayload::AssistantFinal {
                    turn_id: final_turn,
                    message,
                } if final_turn == turn_id
                    && candidate.as_ref().is_none_or(|(_, candidate_message)| candidate_message == message)
            ) {
                return Err(SessionStoreError::InvalidTerminalTurn(
                    "terminal final_id does not reference its turn's assistant final".into(),
                ));
            }
            let outbox_id = delivery_outbox_id.as_ref().ok_or_else(|| {
                SessionStoreError::InvalidTerminalTurn(
                    "terminal does not reference a durable delivery outbox record".into(),
                )
            })?;
            let outbox = index
                .entries
                .get(outbox_id)
                .ok_or_else(|| SessionStoreError::MissingEntry(outbox_id.clone()))?;
            if !matches!(
                &outbox.payload,
                SessionEntryPayload::TurnDeliveryOutbox {
                    turn_id: outbox_turn,
                    final_id: outbox_final,
                    ..
                } if outbox_turn == turn_id && outbox_final == final_id
            ) {
                return Err(SessionStoreError::InvalidTerminalTurn(
                    "terminal outbox does not reference its turn and assistant final".into(),
                ));
            }
            let authoritative_snapshot = authoritative_turn_snapshot(
                &self.manifest.session_id,
                final_entry,
                outbox,
                &terminal,
            )?;
            let published_leaf = if let Some(snapshot_id) = authoritative_snapshot_id {
                let snapshot_entry = index
                    .entries
                    .get(snapshot_id)
                    .ok_or_else(|| SessionStoreError::MissingEntry(snapshot_id.clone()))?;
                if !matches!(
                    &snapshot_entry.payload,
                    SessionEntryPayload::AuthoritativeSnapshot { snapshot }
                        if snapshot == &authoritative_snapshot
                ) {
                    return Err(SessionStoreError::InvalidTerminalTurn(
                        "terminal authoritative snapshot does not match its finalized turn".into(),
                    ));
                }
                terminal.id.clone()
            } else if let Some(snapshot_entry) = index.entries.values().find(|entry| {
                matches!(
                    &entry.payload,
                    SessionEntryPayload::AuthoritativeSnapshot { snapshot }
                        if snapshot.terminal_id == terminal.id
                )
            }) {
                if !matches!(
                    &snapshot_entry.payload,
                    SessionEntryPayload::AuthoritativeSnapshot { snapshot }
                        if snapshot == &authoritative_snapshot
                ) {
                    return Err(SessionStoreError::InvalidTerminalTurn(
                        "repaired authoritative snapshot does not match its finalized turn".into(),
                    ));
                }
                snapshot_entry.id.clone()
            } else {
                self.append(
                    Some(terminal.id.clone()),
                    timestamp,
                    SessionEntryPayload::AuthoritativeSnapshot {
                        snapshot: authoritative_snapshot,
                    },
                )?
                .id
            };
            if self.manifest.active_leaf.as_ref() != Some(&published_leaf) {
                let mut next_manifest = self.manifest.clone();
                next_manifest.active_leaf = Some(published_leaf);
                write_manifest(&self.directory, &next_manifest)?;
                self.manifest = next_manifest;
            }
            return Ok((final_entry.clone(), terminal.clone()));
        }
        let existing_final = index.entries.values().find(|entry| {
            matches!(
                &entry.payload,
                SessionEntryPayload::AssistantFinal {
                    turn_id: existing,
                    ..
                } if existing == turn_id
            )
        });
        let mut pending = Vec::new();
        let final_entry = if let Some(existing) = existing_final {
            if !matches!(
                &existing.payload,
                SessionEntryPayload::AssistantFinal { message: existing_message, .. }
                    if candidate.as_ref().is_none_or(|(_, candidate_message)| candidate_message == existing_message)
            ) {
                return Err(SessionStoreError::InvalidTerminalTurn(
                    "stored final does not match the durable provider candidate".into(),
                ));
            }
            existing.clone()
        } else {
            let entry = SessionEntry::new(
                EntryId::new(),
                self.manifest.active_leaf.clone(),
                timestamp,
                SessionEntryPayload::AssistantFinal {
                    turn_id: turn_id.clone(),
                    message,
                },
            )?;
            index.insert(entry.clone())?;
            pending.push(entry.clone());
            entry
        };
        let existing_outbox = index.entries.values().find(|entry| {
            matches!(
                &entry.payload,
                SessionEntryPayload::TurnDeliveryOutbox {
                    turn_id: existing,
                    final_id,
                    ..
                } if existing == turn_id && final_id == &final_entry.id
            )
        });
        let outbox_entry = if let Some(existing) = existing_outbox {
            existing.clone()
        } else {
            let entry = SessionEntry::new(
                EntryId::new(),
                Some(final_entry.id.clone()),
                timestamp,
                SessionEntryPayload::TurnDeliveryOutbox {
                    turn_id: turn_id.clone(),
                    final_id: final_entry.id.clone(),
                    action_id,
                    artifact_ids,
                },
            )?;
            index.insert(entry.clone())?;
            pending.push(entry.clone());
            entry
        };
        let authoritative_snapshot_id = EntryId::new();
        let finalized_obligation_id = EntryId::new();
        let terminal_id = EntryId::new();
        let terminal_entry = SessionEntry::new(
            terminal_id.clone(),
            Some(finalized_obligation_id.clone()),
            timestamp,
            SessionEntryPayload::TerminalTurn {
                turn_id: turn_id.clone(),
                final_id: final_entry.id.clone(),
                delivery_outbox_id: Some(outbox_entry.id.clone()),
                authoritative_snapshot_id: Some(authoritative_snapshot_id.clone()),
                status,
                execution_succeeded,
                final_created: true,
                artifacts_persisted,
                delivery_enqueued: true,
                detail: detail.clone(),
            },
        )?;
        let authoritative_snapshot_entry = SessionEntry::new(
            authoritative_snapshot_id.clone(),
            Some(outbox_entry.id.clone()),
            timestamp,
            SessionEntryPayload::AuthoritativeSnapshot {
                snapshot: authoritative_turn_snapshot(
                    &self.manifest.session_id,
                    &final_entry,
                    &outbox_entry,
                    &terminal_entry,
                )?,
            },
        )?;
        index.insert(authoritative_snapshot_entry.clone())?;
        pending.push(authoritative_snapshot_entry);
        if let Some((accepted_action_id, user_entry_id)) =
            index
                .entries
                .values()
                .find_map(|entry| match &entry.payload {
                    SessionEntryPayload::TurnObligation {
                        action_id,
                        turn_id: obligation_turn,
                        user_entry_id,
                        state:
                            TurnObligationState::Accepted
                            | TurnObligationState::Running { .. }
                            | TurnObligationState::FinalizationPending { .. },
                    } if obligation_turn == turn_id => {
                        Some((action_id.clone(), user_entry_id.clone()))
                    }
                    _ => None,
                })
        {
            let obligation = SessionEntry::new(
                finalized_obligation_id,
                Some(authoritative_snapshot_id.clone()),
                timestamp,
                SessionEntryPayload::TurnObligation {
                    action_id: accepted_action_id,
                    turn_id: turn_id.clone(),
                    user_entry_id,
                    state: TurnObligationState::Finalized {
                        final_id: final_entry.id.clone(),
                        terminal_id: terminal_id.clone(),
                    },
                },
            )?;
            index.insert(obligation.clone())?;
            pending.push(obligation);
        } else {
            return Err(SessionStoreError::InvalidTerminalTurn(
                "finalization requires a durable turn obligation".into(),
            ));
        }
        index.insert(terminal_entry.clone())?;
        pending.push(terminal_entry.clone());
        let mut bytes = Vec::new();
        for entry in pending {
            bytes.extend(canonical_json_bytes(&entry)?);
            bytes.push(b'\n');
        }
        let mut history = OpenOptions::new().append(true).open(&history_path)?;
        history.write_all(&bytes)?;
        history.sync_all()?;
        let mut next_manifest = self.manifest.clone();
        next_manifest.active_leaf = Some(terminal_entry.id.clone());
        write_manifest(&self.directory, &next_manifest)?;
        self.manifest = next_manifest;
        Ok((final_entry, terminal_entry))
    }

    /// # Errors
    ///
    /// Returns an error when the active ancestry is corrupt or the policy is invalid.
    pub fn request_compaction(
        &self,
        estimated_tokens: u64,
        policy: CompactionPolicy,
        protected_user_entry_id: Option<&EntryId>,
        trigger: CompactionTrigger,
    ) -> Result<Option<CompactionRequest>, SessionStoreError> {
        self.ensure_writable()?;
        parse_complete_history(&self.directory.join(HISTORY_FILE))?.compaction_request(
            &self.manifest,
            estimated_tokens,
            policy,
            protected_user_entry_id,
            trigger,
        )
    }

    /// Durably opens a compaction transaction and returns the locked request.
    ///
    /// # Errors
    ///
    /// Returns an error when the selected surface changed before the lock was appended.
    pub fn begin_compaction(
        &mut self,
        mut request: CompactionRequest,
        timestamp: UtcTimestamp,
    ) -> Result<CompactionRequest, SessionStoreError> {
        self.ensure_writable()?;
        if request.session_id != self.manifest.session_id
            || request.started_entry_id.is_some()
            || request.surface_generation != self.manifest.compaction_generation
            || self.manifest.active_leaf.as_ref() != Some(&request.selected_leaf)
        {
            return Err(SessionStoreError::StaleCompaction);
        }
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        validate_compaction_selection(&request, &index)?;
        let started = self.append(
            Some(request.selected_leaf.clone()),
            timestamp,
            SessionEntryPayload::CompactionStarted {
                compaction_id: request.id.clone(),
                base_leaf: request.selected_leaf.clone(),
                range_start: request.range_start.clone(),
                range_end: request.range_end.clone(),
                source_entries: request.source_entries.clone(),
                surface_generation: request.surface_generation,
                trigger: request.trigger,
            },
        )?;
        request.started_entry_id = Some(started.id);
        Ok(request)
    }

    /// Closes an opened compaction as failed without advancing the surface generation.
    ///
    /// # Errors
    ///
    /// Returns an error when the opened transaction is no longer the active leaf.
    pub fn fail_compaction(
        &mut self,
        request: &CompactionRequest,
        stage: CompactionFailureStage,
        error: impl Into<String>,
        timestamp: UtcTimestamp,
    ) -> Result<SessionEntry, SessionStoreError> {
        self.ensure_writable()?;
        let started = request
            .started_entry_id
            .as_ref()
            .ok_or(SessionStoreError::StaleCompaction)?;
        if self.manifest.active_leaf.as_ref() != Some(started)
            || self.manifest.compaction_generation != request.surface_generation
        {
            return Err(SessionStoreError::StaleCompaction);
        }
        self.append(
            Some(started.clone()),
            timestamp,
            SessionEntryPayload::CompactionEnded {
                compaction_id: request.id.clone(),
                outcome: CompactionOutcome::Failed {
                    stage,
                    error: bounded_failure_detail(error.into()),
                },
            },
        )
    }

    /// # Errors
    ///
    /// Returns an error for stale selection, invalid structured output, or a failed durable commit.
    pub fn commit_compaction(
        &mut self,
        request: &CompactionRequest,
        output: CompactionOutput,
        timestamp: UtcTimestamp,
    ) -> Result<CompactionEmission, SessionStoreError> {
        self.ensure_writable()?;
        let started = request
            .started_entry_id
            .as_ref()
            .ok_or(SessionStoreError::StaleCompaction)?;
        if request.session_id != self.manifest.session_id
            || self.manifest.active_leaf.as_ref() != Some(started)
            || self.manifest.compaction_generation != request.surface_generation
        {
            return Err(SessionStoreError::StaleCompaction);
        }
        let history_path = self.directory.join(HISTORY_FILE);
        let mut index = parse_complete_history(&history_path)?;
        validate_compaction_output(request, &output, &index)?;
        let estimated_summary_tokens = estimated_text_tokens(&output.session_summary);
        let summary_entry = SessionEntry::new(
            EntryId::new(),
            Some(started.clone()),
            timestamp,
            SessionEntryPayload::CompactionSummary {
                compaction_id: request.id.clone(),
                summary: output.session_summary.clone(),
                raw_provider_output: output.raw_provider_output.clone(),
                source_entries: request.source_entries.clone(),
                provider: output.provider.clone(),
                model: output.model.clone(),
                max_output_tokens: output.max_output_tokens,
                input_tokens: output.input_tokens,
                output_tokens: output.output_tokens,
                cached_input_tokens: output.cached_input_tokens,
                estimated_source_tokens: request.estimated_source_tokens,
                estimated_summary_tokens,
            },
        )?;
        index.insert(summary_entry.clone())?;
        let boundary = SessionEntry::new(
            EntryId::new(),
            Some(summary_entry.id.clone()),
            timestamp,
            SessionEntryPayload::CompactionCheckpoint {
                compaction_id: request.id.clone(),
                summary_id: summary_entry.id.clone(),
                source_entries: request.source_entries.clone(),
                compacted_through: request.range_end.clone(),
                summary: output.session_summary,
            },
        )?;
        index.insert(boundary.clone())?;
        let ended = SessionEntry::new(
            EntryId::new(),
            Some(boundary.id.clone()),
            timestamp,
            SessionEntryPayload::CompactionEnded {
                compaction_id: request.id.clone(),
                outcome: CompactionOutcome::Success,
            },
        )?;
        index.insert(ended.clone())?;
        let mut bytes = Vec::new();
        for entry in [&summary_entry, &boundary, &ended] {
            bytes.extend(canonical_json_bytes(entry)?);
            bytes.push(b'\n');
        }
        let mut history = OpenOptions::new().append(true).open(&history_path)?;
        history.write_all(&bytes)?;
        history.sync_all()?;
        let mut next_manifest = self.manifest.clone();
        next_manifest.active_leaf = Some(ended.id);
        next_manifest.compaction_generation = next_manifest
            .compaction_generation
            .checked_add(1)
            .ok_or_else(|| {
                SessionStoreError::InvalidCompaction("surface generation overflow".into())
            })?;
        write_manifest(&self.directory, &next_manifest)?;
        self.manifest = next_manifest;
        Ok(CompactionEmission {
            boundary,
            memory_candidates: output.memory_candidates,
            daily_entry: output.daily_entry,
            open_commitments: output.open_commitments,
            unresolved_items: output.unresolved_items,
        })
    }

    /// # Errors
    ///
    /// Returns an error when the selected entry is missing or the manifest update fails.
    pub fn select_leaf(&mut self, leaf: &EntryId) -> Result<(), SessionStoreError> {
        self.ensure_writable()?;
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        if !index.entries.contains_key(leaf) {
            return Err(SessionStoreError::MissingEntry(leaf.clone()));
        }
        let mut next_manifest = self.manifest.clone();
        next_manifest.active_leaf = Some(leaf.clone());
        write_manifest(&self.directory, &next_manifest)?;
        self.manifest = next_manifest;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error for an invalid label, missing entry, or failed manifest update.
    pub fn label_branch(
        &mut self,
        label: impl Into<String>,
        leaf: &EntryId,
    ) -> Result<(), SessionStoreError> {
        self.ensure_writable()?;
        let label = label.into();
        if label.trim().is_empty() || self.manifest.branch_labels.contains_key(&label) {
            return Err(SessionStoreError::InvalidLabel);
        }
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        if !index.entries.contains_key(leaf) {
            return Err(SessionStoreError::MissingEntry(leaf.clone()));
        }
        let mut next_manifest = self.manifest.clone();
        next_manifest.branch_labels.insert(label, leaf.clone());
        write_manifest(&self.directory, &next_manifest)?;
        self.manifest = next_manifest;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when the archived manifest cannot be made durable.
    pub fn archive(&mut self) -> Result<(), SessionStoreError> {
        self.ensure_writable()?;
        let mut next_manifest = self.manifest.clone();
        next_manifest.archived = true;
        write_manifest(&self.directory, &next_manifest)?;
        self.manifest = next_manifest;
        Ok(())
    }

    /// Deliberately replaces the pinned route/profile snapshot using an optimistic digest.
    ///
    /// # Errors
    ///
    /// Returns an error when the expected snapshot is stale or the replacement is invalid.
    pub fn update_profile_snapshot(
        &mut self,
        expected_digest: Option<&str>,
        replacement: ProfileSnapshotMetadata,
    ) -> Result<(), SessionStoreError> {
        self.ensure_writable()?;
        let current_digest = self
            .manifest
            .profile_snapshot
            .as_ref()
            .map(|snapshot| snapshot.digest.as_str());
        if current_digest != expected_digest {
            return Err(SessionStoreError::StaleProfileSnapshot);
        }
        replacement.verify()?;
        if replacement.profile_id != self.manifest.profile_id
            || replacement.workspace_id != self.manifest.workspace_id
        {
            return Err(SessionStoreError::InvalidProfileSnapshot(
                "profile or workspace identity differs from the session".into(),
            ));
        }
        let mut next_manifest = self.manifest.clone();
        next_manifest.profile_snapshot = Some(replacement);
        write_manifest(&self.directory, &next_manifest)?;
        self.manifest = next_manifest;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when history cannot be reconstructed.
    pub fn active_ancestry(&self) -> Result<Vec<SessionEntry>, SessionStoreError> {
        let Some(leaf) = &self.manifest.active_leaf else {
            return Ok(Vec::new());
        };
        parse_complete_history(&self.directory.join(HISTORY_FILE))?.ancestry(leaf)
    }

    fn ensure_writable(&self) -> Result<(), SessionStoreError> {
        if self.directory.join(QUARANTINE_FILE).exists() {
            Err(SessionStoreError::Quarantined(
                self.manifest.session_id.clone(),
            ))
        } else if self.manifest.archived {
            Err(SessionStoreError::Archived(
                self.manifest.session_id.clone(),
            ))
        } else {
            Ok(())
        }
    }
}

fn authoritative_turn_snapshot(
    session_id: &SessionId,
    final_entry: &SessionEntry,
    outbox_entry: &SessionEntry,
    terminal_entry: &SessionEntry,
) -> Result<AuthoritativeTurnSnapshot, SessionStoreError> {
    let SessionEntryPayload::AssistantFinal {
        turn_id: final_turn,
        message,
    } = &final_entry.payload
    else {
        return Err(SessionStoreError::InvalidTerminalTurn(
            "authoritative snapshot final is not an assistant final".into(),
        ));
    };
    let SessionEntryPayload::TurnDeliveryOutbox {
        turn_id: outbox_turn,
        final_id: outbox_final,
        action_id,
        artifact_ids,
    } = &outbox_entry.payload
    else {
        return Err(SessionStoreError::InvalidTerminalTurn(
            "authoritative snapshot outbox is not a turn delivery outbox".into(),
        ));
    };
    let SessionEntryPayload::TerminalTurn {
        turn_id,
        final_id,
        delivery_outbox_id,
        status,
        execution_succeeded,
        final_created,
        artifacts_persisted,
        delivery_enqueued,
        detail,
        ..
    } = &terminal_entry.payload
    else {
        return Err(SessionStoreError::InvalidTerminalTurn(
            "authoritative snapshot terminal is not a terminal turn".into(),
        ));
    };
    if final_turn != turn_id
        || outbox_turn != turn_id
        || final_id != &final_entry.id
        || outbox_final != final_id
        || delivery_outbox_id.as_ref() != Some(&outbox_entry.id)
    {
        return Err(SessionStoreError::InvalidTerminalTurn(
            "authoritative snapshot records do not describe one finalized turn".into(),
        ));
    }
    Ok(AuthoritativeTurnSnapshot {
        session_id: session_id.clone(),
        turn_id: turn_id.clone(),
        final_id: final_id.clone(),
        delivery_outbox_id: outbox_entry.id.clone(),
        terminal_id: terminal_entry.id.clone(),
        status: *status,
        execution_succeeded: *execution_succeeded,
        final_created: *final_created,
        artifacts_persisted: *artifacts_persisted,
        delivery_enqueued: *delivery_enqueued,
        delivery_acknowledged: false,
        action_id: action_id.clone(),
        artifact_ids: artifact_ids.clone(),
        assistant_final: message.clone(),
        detail: detail.clone(),
    })
}

impl Drop for SessionWriter {
    fn drop(&mut self) {
        let _ = FileExt::unlock(&self.lock_file);
    }
}

struct Inspection {
    index: SessionIndex,
    valid_bytes: usize,
    issues: Vec<RepairIssue>,
}

fn inspect_bytes(bytes: &[u8]) -> Inspection {
    let mut index = SessionIndex::default();
    let mut offset = 0;
    let mut line = 0;
    while offset < bytes.len() {
        line += 1;
        let remaining = &bytes[offset..];
        let Some(relative_newline) = remaining.iter().position(|byte| *byte == b'\n') else {
            return Inspection {
                index,
                valid_bytes: offset,
                issues: vec![RepairIssue {
                    line,
                    reason: "unterminated final JSONL record".into(),
                    final_unterminated: true,
                }],
            };
        };
        let end = offset + relative_newline;
        let raw = &bytes[offset..end];
        let parsed = serde_json::from_slice::<SessionEntry>(raw)
            .map_err(SessionStoreError::from)
            .and_then(|entry| {
                entry.verify()?;
                index.insert(entry)
            });
        if let Err(error) = parsed {
            return Inspection {
                index,
                valid_bytes: offset,
                issues: vec![RepairIssue {
                    line,
                    reason: error.to_string(),
                    final_unterminated: false,
                }],
            };
        }
        offset = end + 1;
    }
    Inspection {
        index,
        valid_bytes: offset,
        issues: Vec::new(),
    }
}

fn parse_complete_history(path: &Path) -> Result<SessionIndex, SessionStoreError> {
    let bytes = fs::read(path)?;
    let inspection = inspect_bytes(&bytes);
    if let Some(issue) = inspection.issues.first() {
        return Err(SessionStoreError::CorruptHistory {
            line: issue.line,
            reason: issue.reason.clone(),
        });
    }
    Ok(inspection.index)
}

fn parse_history_for_finalization(path: &Path) -> Result<SessionIndex, SessionStoreError> {
    let bytes = fs::read(path)?;
    let inspection = inspect_bytes(&bytes);
    if inspection.issues.is_empty() {
        return Ok(inspection.index);
    }
    if inspection.issues.len() == 1 && inspection.issues[0].final_unterminated {
        let history = OpenOptions::new().write(true).open(path)?;
        history.set_len(u64::try_from(inspection.valid_bytes).map_err(|_| {
            SessionStoreError::CorruptHistory {
                line: inspection.issues[0].line,
                reason: "history length exceeds supported range".into(),
            }
        })?)?;
        history.sync_all()?;
        return Ok(inspection.index);
    }
    let issue = &inspection.issues[0];
    Err(SessionStoreError::CorruptHistory {
        line: issue.line,
        reason: issue.reason.clone(),
    })
}

fn validate_compaction_policy(policy: CompactionPolicy) -> Result<(), SessionStoreError> {
    if policy.target_tokens == 0
        || policy.trigger_tokens <= policy.target_tokens
        || policy.max_summary_bytes == 0
        || policy.max_candidates == 0
        || policy.max_candidate_bytes == 0
    {
        Err(SessionStoreError::InvalidCompaction(
            "threshold, target, summary, and candidate limits must be consistent".into(),
        ))
    } else {
        Ok(())
    }
}

fn validate_compaction_output(
    request: &CompactionRequest,
    output: &CompactionOutput,
    index: &SessionIndex,
) -> Result<(), SessionStoreError> {
    validate_compaction_selection(request, index)?;
    if output.request_id != request.id {
        return Err(SessionStoreError::InvalidCompaction(
            "structured output belongs to another request".into(),
        ));
    }
    let estimated_summary_tokens = estimated_text_tokens(&output.session_summary);
    if output.session_summary.trim().is_empty()
        || output.session_summary.len() > request.max_summary_bytes
        || output.raw_provider_output.trim().is_empty()
        || estimated_summary_tokens >= request.estimated_source_tokens
    {
        return Err(SessionStoreError::InvalidCompaction(
            "session summary is empty, oversized, missing raw output, or does not shrink the selected span".into(),
        ));
    }
    let candidate_count = output
        .memory_candidates
        .len()
        .saturating_add(output.open_commitments.len())
        .saturating_add(output.unresolved_items.len());
    if candidate_count > request.max_candidates {
        return Err(SessionStoreError::InvalidCompaction(
            "structured output has too many candidates".into(),
        ));
    }
    let ancestry = index.ancestry(&request.selected_leaf)?;
    let start = ancestry
        .iter()
        .position(|entry| entry.id == request.range_start)
        .ok_or_else(|| {
            SessionStoreError::InvalidCompaction("range start is outside selected ancestry".into())
        })?;
    let end = ancestry
        .iter()
        .position(|entry| entry.id == request.range_end)
        .ok_or_else(|| {
            SessionStoreError::InvalidCompaction("range end is outside selected ancestry".into())
        })?;
    if start > end {
        return Err(SessionStoreError::InvalidCompaction(
            "selected compaction range is inconsistent".into(),
        ));
    }
    if request.previous_boundary.as_ref()
        != start.checked_sub(1).map(|previous| &ancestry[previous].id)
    {
        return Err(SessionStoreError::InvalidCompaction(
            "previous boundary does not precede selected range".into(),
        ));
    }
    let selected_ids = ancestry[start..=end]
        .iter()
        .map(|entry| entry.id.clone())
        .collect::<BTreeSet<_>>();
    for candidate in &output.memory_candidates {
        validate_candidate_text(&candidate.text, request.max_candidate_bytes)?;
        validate_sources(&candidate.source_entries, &selected_ids)?;
    }
    for commitment in &output.open_commitments {
        validate_candidate_text(&commitment.description, request.max_candidate_bytes)?;
        validate_sources(&commitment.source_entries, &selected_ids)?;
    }
    if output
        .daily_entry
        .as_ref()
        .is_some_and(|text| text.trim().is_empty() || text.len() > request.max_candidate_bytes)
        || output
            .unresolved_items
            .iter()
            .any(|text| text.trim().is_empty() || text.len() > request.max_candidate_bytes)
    {
        return Err(SessionStoreError::InvalidCompaction(
            "daily or unresolved text is empty or oversized".into(),
        ));
    }
    Ok(())
}

fn validate_compaction_selection(
    request: &CompactionRequest,
    index: &SessionIndex,
) -> Result<(), SessionStoreError> {
    let ancestry = index.ancestry(&request.selected_leaf)?;
    let start = ancestry
        .iter()
        .position(|entry| entry.id == request.range_start)
        .ok_or_else(|| {
            SessionStoreError::InvalidCompaction("range start is outside selected ancestry".into())
        })?;
    let end = ancestry
        .iter()
        .position(|entry| entry.id == request.range_end)
        .ok_or_else(|| {
            SessionStoreError::InvalidCompaction("range end is outside selected ancestry".into())
        })?;
    if start > end
        || request.previous_boundary.as_ref()
            != start.checked_sub(1).map(|previous| &ancestry[previous].id)
    {
        return Err(SessionStoreError::InvalidCompaction(
            "selected range does not follow its declared prior boundary".into(),
        ));
    }
    let actual_sources = ancestry[start..=end]
        .iter()
        .map(|entry| entry.id.clone())
        .collect::<Vec<_>>();
    if actual_sources != request.source_entries {
        return Err(SessionStoreError::StaleCompaction);
    }
    validate_balanced_compaction_range(&ancestry[start..=end])?;
    let estimated = ancestry[start..=end].iter().fold(0_u64, |total, entry| {
        total.saturating_add(entry_estimated_tokens(entry))
    });
    if estimated != request.estimated_source_tokens {
        return Err(SessionStoreError::StaleCompaction);
    }
    Ok(())
}

fn validate_balanced_compaction_range(entries: &[SessionEntry]) -> Result<(), SessionStoreError> {
    if !matches!(
        entries.last().map(|entry| &entry.payload),
        Some(SessionEntryPayload::TerminalTurn { .. })
    ) {
        return Err(SessionStoreError::InvalidCompaction(
            "compaction range must end at a terminal turn boundary".into(),
        ));
    }
    let calls = entries
        .iter()
        .filter_map(|entry| match &entry.payload {
            SessionEntryPayload::ToolCall { call_id, .. } => Some(call_id.clone()),
            _ => None,
        })
        .collect::<BTreeSet<_>>();
    let results = entries
        .iter()
        .filter_map(|entry| match &entry.payload {
            SessionEntryPayload::ToolResult { call_id, .. } => Some(call_id.clone()),
            _ => None,
        })
        .collect::<BTreeSet<_>>();
    if calls != results {
        return Err(SessionStoreError::InvalidCompaction(
            "compaction range splits a tool-call/tool-result pair".into(),
        ));
    }
    Ok(())
}

fn entry_estimated_tokens(entry: &SessionEntry) -> u64 {
    canonical_json_bytes(entry)
        .ok()
        .and_then(|bytes| u64::try_from(bytes.len().saturating_add(3) / 4).ok())
        .unwrap_or(1)
        .max(1)
}

fn reconstructed_surface_tail(
    ancestry: &[SessionEntry],
    boundary_index: usize,
) -> Vec<SessionEntry> {
    let SessionEntryPayload::CompactionCheckpoint {
        compaction_id,
        compacted_through,
        ..
    } = &ancestry[boundary_index].payload
    else {
        return ancestry.iter().skip(boundary_index + 1).cloned().collect();
    };
    let compacted_end = ancestry[..boundary_index]
        .iter()
        .position(|entry| &entry.id == compacted_through)
        .unwrap_or(boundary_index);
    let started = ancestry[compacted_end.saturating_add(1)..boundary_index]
        .iter()
        .position(|entry| {
            matches!(
                &entry.payload,
                SessionEntryPayload::CompactionStarted {
                    compaction_id: started_id,
                    ..
                } if started_id == compaction_id
            )
        })
        .map_or(boundary_index, |offset| compacted_end + 1 + offset);
    ancestry[compacted_end.saturating_add(1)..started]
        .iter()
        .chain(ancestry.iter().skip(boundary_index + 1))
        .filter(|entry| {
            !matches!(
                entry.payload,
                SessionEntryPayload::CompactionStarted { .. }
                    | SessionEntryPayload::CompactionSummary { .. }
                    | SessionEntryPayload::CompactionEnded { .. }
            )
        })
        .cloned()
        .collect()
}

fn estimated_text_tokens(text: &str) -> u64 {
    u64::try_from(text.len().saturating_add(3) / 4)
        .unwrap_or(u64::MAX)
        .max(1)
}

fn bounded_failure_detail(mut detail: String) -> String {
    const MAX_BYTES: usize = 4 * 1_024;
    if detail.len() <= MAX_BYTES {
        return detail;
    }
    let mut end = MAX_BYTES;
    while end > 0 && !detail.is_char_boundary(end) {
        end -= 1;
    }
    detail.truncate(end);
    detail
}

fn validate_assistant_candidate(message: &StoredMessage) -> Result<(), SessionStoreError> {
    let substantive = message.role == MessageRole::Assistant
        && message.content.iter().any(|block| match block {
            ContentBlock::Text { text } | ContentBlock::Reasoning { text, .. } => {
                !text.trim().is_empty()
            }
            ContentBlock::Artifact { .. } | ContentBlock::Resource { .. } => true,
        });
    if substantive {
        Ok(())
    } else {
        Err(SessionStoreError::InvalidTerminalTurn(
            "assistant final candidate is incomplete or empty".into(),
        ))
    }
}

fn validate_candidate_text(text: &str, max_bytes: usize) -> Result<(), SessionStoreError> {
    if text.trim().is_empty() || text.len() > max_bytes {
        Err(SessionStoreError::InvalidCompaction(
            "candidate text is empty or oversized".into(),
        ))
    } else {
        Ok(())
    }
}

fn validate_sources(
    source_entries: &[EntryId],
    selected_ids: &BTreeSet<EntryId>,
) -> Result<(), SessionStoreError> {
    if source_entries.is_empty()
        || source_entries
            .iter()
            .any(|entry| !selected_ids.contains(entry))
    {
        Err(SessionStoreError::InvalidCompaction(
            "candidate sources must belong to the selected range".into(),
        ))
    } else {
        Ok(())
    }
}

fn validate_new_session(session: &NewSession) -> Result<(), SessionStoreError> {
    let parent_valid = match session.kind {
        SessionKind::Root => session.parent_session_id.is_none(),
        SessionKind::DurableChild => session.parent_session_id.is_some(),
    };
    if !parent_valid {
        return Err(SessionStoreError::CorruptHistory {
            line: 0,
            reason: "session kind and parent identity disagree".into(),
        });
    }
    if let Some(snapshot) = &session.profile_snapshot {
        snapshot.verify()?;
        if snapshot.profile_id != session.profile_id
            || snapshot.workspace_id != session.workspace_id
        {
            return Err(SessionStoreError::InvalidProfileSnapshot(
                "profile or workspace identity differs from the new session".into(),
            ));
        }
    }
    Ok(())
}

fn read_manifest(directory: &Path) -> Result<SessionManifest, SessionStoreError> {
    let manifest: SessionManifest =
        serde_json::from_slice(&fs::read(directory.join(MANIFEST_FILE))?)?;
    if manifest.version.major != CURRENT_SCHEMA_VERSION.major
        || manifest.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(SessionStoreError::UnsupportedVersion(manifest.version));
    }
    if let Some(snapshot) = &manifest.profile_snapshot {
        snapshot.verify()?;
        if snapshot.profile_id != manifest.profile_id
            || snapshot.workspace_id != manifest.workspace_id
        {
            return Err(SessionStoreError::InvalidProfileSnapshot(
                "profile or workspace identity differs from the manifest".into(),
            ));
        }
    }
    Ok(manifest)
}

fn snapshot_digest(snapshot: &serde_json::Value) -> Result<String, SessionStoreError> {
    let digest = Sha256::digest(canonical_json_bytes(snapshot)?);
    let mut encoded = String::with_capacity(64);
    for byte in digest {
        write!(&mut encoded, "{byte:02x}").map_err(|_| {
            SessionStoreError::InvalidProfileSnapshot("digest formatting failed".into())
        })?;
    }
    Ok(encoded)
}

fn write_manifest(directory: &Path, manifest: &SessionManifest) -> Result<(), SessionStoreError> {
    atomic_write_json(directory, MANIFEST_FILE, manifest)
}

fn atomic_write_json(
    directory: &Path,
    name: &str,
    value: &impl Serialize,
) -> Result<(), SessionStoreError> {
    let path = directory.join(name);
    let temporary = directory.join(format!(".{name}.{}.tmp", EntityId::new()));
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&canonical_json_bytes(value)?)?;
    file.sync_all()?;
    keith_platform::replace_file(&temporary, &path)?;
    sync_directory(directory)
}

fn sync_directory(directory: &Path) -> Result<(), SessionStoreError> {
    File::open(directory)?.sync_all()?;
    Ok(())
}

#[cfg(test)]
mod tests {
    use std::io::Write;

    use proptest::prelude::*;
    use tempfile::tempdir;

    use super::*;

    fn new_session(session_id: SessionId) -> NewSession {
        NewSession {
            kind: SessionKind::Root,
            session_id,
            root_tree_id: RootTreeId::new(),
            parent_session_id: None,
            profile_id: ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            created_at: UtcTimestamp::UNIX_EPOCH,
            label: Some("test".into()),
            profile_snapshot: None,
        }
    }

    fn identity(generation: u64) -> WriterIdentity {
        WriterIdentity {
            worker_id: WorkerId::new(),
            owner_instance: EntityId::new(),
            generation: Generation::new(generation),
            acquired_at: UtcTimestamp::UNIX_EPOCH,
        }
    }

    fn message(text: &str) -> SessionEntryPayload {
        SessionEntryPayload::UserMessage {
            message: StoredMessage {
                role: MessageRole::User,
                content: vec![ContentBlock::Text { text: text.into() }],
                provider_metadata: BTreeMap::new(),
            },
        }
    }

    fn compaction_output(
        request: &CompactionRequest,
        source: &EntryId,
        summary: &str,
    ) -> CompactionOutput {
        CompactionOutput {
            request_id: request.id.clone(),
            session_summary: summary.into(),
            raw_provider_output: summary.into(),
            provider: Some("test-provider".into()),
            model: Some("test-model".into()),
            max_output_tokens: 128,
            input_tokens: 100,
            output_tokens: 10,
            cached_input_tokens: 0,
            memory_candidates: vec![MemoryCandidateDraft {
                id: EntityId::new(),
                kind: MemoryKind::ProjectContext,
                text: "durable candidate".into(),
                source_entries: vec![source.clone()],
                sensitivity: Sensitivity::Personal,
                retention: RetentionClass::Durable,
            }],
            daily_entry: Some("daily entry".into()),
            open_commitments: vec![CommitmentDraft {
                id: EntityId::new(),
                description: "finish the task".into(),
                source_entries: vec![source.clone()],
            }],
            unresolved_items: vec!["confirm the result".into()],
        }
    }

    fn append_completed_turn(
        writer: &mut SessionWriter,
        text: &str,
        millis: i64,
    ) -> (SessionEntry, SessionEntry) {
        let timestamp = UtcTimestamp::from_unix_millis(millis);
        let user = writer
            .append(
                writer.manifest().active_leaf.clone(),
                timestamp,
                message(text),
            )
            .unwrap();
        let action_id = ActionId::new();
        let turn_id = TurnId::new();
        writer
            .accept_turn(
                timestamp,
                action_id.clone(),
                turn_id.clone(),
                user.id.clone(),
            )
            .unwrap();
        writer
            .append_final_candidate(
                timestamp,
                turn_id.clone(),
                StoredMessage {
                    role: MessageRole::Assistant,
                    content: vec![ContentBlock::Text {
                        text: format!("answer for {text}"),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
                10,
                5,
                0,
            )
            .unwrap();
        let (_, terminal) = writer
            .append_finalized_turn(
                timestamp,
                &turn_id,
                StoredMessage {
                    role: MessageRole::Assistant,
                    content: vec![ContentBlock::Text {
                        text: "unused fallback".into(),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
                TurnTerminalStatus::Failed,
                false,
                true,
                Some(action_id),
                Vec::new(),
                None,
            )
            .unwrap();
        (user, terminal)
    }

    #[test]
    fn branches_reconstruct_without_rewriting_history() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let root = writer
            .append(None, UtcTimestamp::UNIX_EPOCH, message("root"))
            .unwrap();
        let left = writer
            .append(
                Some(root.id.clone()),
                UtcTimestamp::from_unix_millis(1),
                message("left"),
            )
            .unwrap();
        let right = writer
            .append(
                Some(root.id.clone()),
                UtcTimestamp::from_unix_millis(2),
                message("right"),
            )
            .unwrap();
        writer.label_branch("left", &left.id).unwrap();
        writer.select_leaf(&right.id).unwrap();
        let ancestry = writer.active_ancestry().unwrap();
        assert_eq!(
            ancestry
                .iter()
                .map(|entry| entry.id.clone())
                .collect::<Vec<_>>(),
            vec![root.id.clone(), right.id.clone()]
        );
        let index = store.load_index(&session_id).unwrap();
        assert_eq!(index.children_of(Some(&root.id)).len(), 2);
        assert_eq!(index.len(), 3);
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn terminal_finalization_is_exactly_once_and_references_its_final() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let accepted = writer
            .append(
                None,
                UtcTimestamp::UNIX_EPOCH,
                message("accepted user input"),
            )
            .unwrap();
        let turn_id = TurnId::new();
        let action_id = ActionId::new();
        writer
            .accept_turn(
                UtcTimestamp::UNIX_EPOCH,
                action_id.clone(),
                turn_id.clone(),
                accepted.id,
            )
            .unwrap();
        let final_message = StoredMessage {
            role: MessageRole::Assistant,
            content: vec![ContentBlock::Text {
                text: "final answer".into(),
            }],
            provider_metadata: BTreeMap::new(),
        };
        let first = writer
            .append_finalized_turn(
                UtcTimestamp::UNIX_EPOCH,
                &turn_id,
                final_message.clone(),
                TurnTerminalStatus::Completed,
                true,
                true,
                Some(action_id.clone()),
                Vec::new(),
                None,
            )
            .unwrap();
        let replay = writer
            .append_finalized_turn(
                UtcTimestamp::UNIX_EPOCH,
                &turn_id,
                final_message,
                TurnTerminalStatus::Completed,
                true,
                true,
                Some(action_id),
                Vec::new(),
                None,
            )
            .unwrap();
        assert_eq!(first, replay);
        let ancestry = writer.active_ancestry().unwrap();
        let finals = ancestry
            .iter()
            .filter(|entry| {
                matches!(
                    &entry.payload,
                    SessionEntryPayload::AssistantFinal {
                        turn_id: existing,
                        ..
                    } if existing == &turn_id
                )
            })
            .collect::<Vec<_>>();
        let terminals = ancestry
            .iter()
            .filter(|entry| {
                matches!(
                    &entry.payload,
                    SessionEntryPayload::TerminalTurn {
                        turn_id: existing,
                        ..
                    } if existing == &turn_id
                )
            })
            .collect::<Vec<_>>();
        let outboxes = ancestry
            .iter()
            .filter(|entry| {
                matches!(
                    &entry.payload,
                    SessionEntryPayload::TurnDeliveryOutbox {
                        turn_id: existing,
                        ..
                    } if existing == &turn_id
                )
            })
            .collect::<Vec<_>>();
        assert_eq!(finals.len(), 1);
        assert_eq!(outboxes.len(), 1);
        assert_eq!(terminals.len(), 1);
        assert!(matches!(
            &terminals[0].payload,
            SessionEntryPayload::TerminalTurn {
                final_id,
                delivery_outbox_id: Some(outbox_id),
                delivery_enqueued: true,
                ..
            } if final_id == &finals[0].id && outbox_id == &outboxes[0].id
        ));
    }

    #[test]
    fn durable_provider_candidate_cannot_be_replaced_by_later_maintenance_failure() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let accepted = writer
            .append(None, UtcTimestamp::UNIX_EPOCH, message("accepted work"))
            .unwrap();
        let action_id = ActionId::new();
        let turn_id = TurnId::new();
        writer
            .accept_turn(
                UtcTimestamp::UNIX_EPOCH,
                action_id.clone(),
                turn_id.clone(),
                accepted.id,
            )
            .unwrap();
        writer
            .append_final_candidate(
                UtcTimestamp::from_unix_millis(1),
                turn_id.clone(),
                StoredMessage {
                    role: MessageRole::Assistant,
                    content: vec![ContentBlock::Text {
                        text: "the complete provider answer".into(),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
                100,
                20,
                0,
            )
            .unwrap();
        writer
            .append(
                writer.manifest().active_leaf.clone(),
                UtcTimestamp::from_unix_millis(2),
                SessionEntryPayload::MaintenanceFailure {
                    turn_id: Some(turn_id.clone()),
                    subsystem: "memory_projection".into(),
                    detail: "projection failed after the candidate committed".into(),
                },
            )
            .unwrap();
        let (final_entry, terminal) = writer
            .append_finalized_turn(
                UtcTimestamp::from_unix_millis(3),
                &turn_id,
                StoredMessage {
                    role: MessageRole::Assistant,
                    content: vec![ContentBlock::Text {
                        text: "replacement failure text".into(),
                    }],
                    provider_metadata: BTreeMap::new(),
                },
                TurnTerminalStatus::Failed,
                false,
                false,
                Some(action_id),
                Vec::new(),
                Some("late failure".into()),
            )
            .unwrap();
        assert!(matches!(
            final_entry.payload,
            SessionEntryPayload::AssistantFinal { message, .. }
                if message.content == vec![ContentBlock::Text {
                    text: "the complete provider answer".into()
                }]
        ));
        assert!(matches!(
            terminal.payload,
            SessionEntryPayload::TerminalTurn {
                status: TurnTerminalStatus::Completed,
                execution_succeeded: true,
                detail: None,
                ..
            }
        ));
    }

    #[test]
    fn legacy_successful_tool_result_serialization_does_not_gain_a_failure_field() {
        let payload = SessionEntryPayload::ToolResult {
            call_id: ToolCallId::new(),
            content: vec![ContentBlock::Text {
                text: "succeeded".into(),
            }],
            is_error: false,
            failure: None,
        };
        let encoded = serde_json::to_value(&payload).unwrap();
        assert!(encoded.get("failure").is_none());
        let decoded: SessionEntryPayload = serde_json::from_value(encoded).unwrap();
        assert!(matches!(
            decoded,
            SessionEntryPayload::ToolResult { failure: None, .. }
        ));
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn finalization_repairs_an_unterminated_batch_without_duplicate_final() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let accepted = writer
            .append(
                None,
                UtcTimestamp::UNIX_EPOCH,
                message("accepted user input"),
            )
            .unwrap();
        let turn_id = TurnId::new();
        let action_id = ActionId::new();
        let obligation = writer
            .accept_turn(
                UtcTimestamp::UNIX_EPOCH,
                action_id.clone(),
                turn_id.clone(),
                accepted.id,
            )
            .unwrap();
        let final_message = StoredMessage {
            role: MessageRole::Assistant,
            content: vec![ContentBlock::Text {
                text: "recovered final".into(),
            }],
            provider_metadata: BTreeMap::new(),
        };
        let final_entry = SessionEntry::new(
            EntryId::new(),
            Some(obligation.id),
            UtcTimestamp::UNIX_EPOCH,
            SessionEntryPayload::AssistantFinal {
                turn_id: turn_id.clone(),
                message: final_message.clone(),
            },
        )
        .unwrap();
        let incomplete_outbox = SessionEntry::new(
            EntryId::new(),
            Some(final_entry.id.clone()),
            UtcTimestamp::UNIX_EPOCH,
            SessionEntryPayload::TurnDeliveryOutbox {
                turn_id: turn_id.clone(),
                final_id: final_entry.id.clone(),
                action_id: None,
                artifact_ids: Vec::new(),
            },
        )
        .unwrap();
        let mut bytes = canonical_json_bytes(&final_entry).unwrap();
        bytes.push(b'\n');
        let partial = canonical_json_bytes(&incomplete_outbox).unwrap();
        bytes.extend(&partial[..partial.len() / 2]);
        let mut history = OpenOptions::new()
            .append(true)
            .open(writer.directory.join(HISTORY_FILE))
            .unwrap();
        history.write_all(&bytes).unwrap();
        history.sync_all().unwrap();

        writer
            .append_finalized_turn(
                UtcTimestamp::UNIX_EPOCH,
                &turn_id,
                final_message,
                TurnTerminalStatus::Failed,
                false,
                true,
                Some(action_id),
                Vec::new(),
                Some("recovered after interrupted finalization".into()),
            )
            .unwrap();
        let ancestry = writer.active_ancestry().unwrap();
        assert_eq!(
            ancestry
                .iter()
                .filter(|entry| matches!(
                    &entry.payload,
                    SessionEntryPayload::AssistantFinal {
                        turn_id: existing,
                        ..
                    } if existing == &turn_id
                ))
                .count(),
            1
        );
        assert_eq!(
            ancestry
                .iter()
                .filter(|entry| matches!(
                    &entry.payload,
                    SessionEntryPayload::TurnDeliveryOutbox {
                        turn_id: existing,
                        ..
                    } if existing == &turn_id
                ))
                .count(),
            1
        );
        assert_eq!(
            ancestry
                .iter()
                .filter(|entry| matches!(
                    &entry.payload,
                    SessionEntryPayload::TerminalTurn {
                        turn_id: existing,
                        ..
                    } if existing == &turn_id
                ))
                .count(),
            1
        );
    }

    #[test]
    fn finalization_republishes_a_terminal_hidden_by_a_stale_manifest() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let (accepted_id, action_id, turn_id, final_message, terminal_id) = {
            let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
            let accepted = writer
                .append(
                    None,
                    UtcTimestamp::UNIX_EPOCH,
                    message("accepted user input"),
                )
                .unwrap();
            let turn_id = TurnId::new();
            let action_id = ActionId::new();
            let obligation = writer
                .accept_turn(
                    UtcTimestamp::UNIX_EPOCH,
                    action_id.clone(),
                    turn_id.clone(),
                    accepted.id,
                )
                .unwrap();
            let final_message = StoredMessage {
                role: MessageRole::Assistant,
                content: vec![ContentBlock::Text {
                    text: "durable final".into(),
                }],
                provider_metadata: BTreeMap::new(),
            };
            let (_, terminal) = writer
                .append_finalized_turn(
                    UtcTimestamp::UNIX_EPOCH,
                    &turn_id,
                    final_message.clone(),
                    TurnTerminalStatus::Completed,
                    true,
                    true,
                    Some(action_id.clone()),
                    Vec::new(),
                    None,
                )
                .unwrap();
            (
                obligation.id,
                action_id,
                turn_id,
                final_message,
                terminal.id,
            )
        };
        let session_directory = store.session_directory(&session_id).unwrap();
        let mut stale = read_manifest(&session_directory).unwrap();
        stale.active_leaf = Some(accepted_id);
        write_manifest(&session_directory, &stale).unwrap();

        let mut writer = store.acquire_writer(&session_id, identity(2)).unwrap();
        writer
            .append_finalized_turn(
                UtcTimestamp::UNIX_EPOCH,
                &turn_id,
                final_message,
                TurnTerminalStatus::Completed,
                true,
                true,
                Some(action_id),
                Vec::new(),
                None,
            )
            .unwrap();
        assert_eq!(writer.manifest().active_leaf.as_ref(), Some(&terminal_id));
        assert_eq!(
            store.manifest(&session_id).unwrap().active_leaf.as_ref(),
            Some(&terminal_id)
        );
    }

    #[test]
    fn writer_lease_excludes_concurrent_and_allows_replacement() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let first = store.acquire_writer(&session_id, identity(1)).unwrap();
        assert!(matches!(
            store.acquire_writer(&session_id, identity(2)),
            Err(SessionStoreError::WriterLocked(_))
        ));
        drop(first);
        let replacement = store.acquire_writer(&session_id, identity(2)).unwrap();
        assert_eq!(replacement.identity().generation, Generation::new(2));
    }

    #[test]
    fn only_an_unterminated_final_line_is_discarded() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        {
            let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
            writer
                .append(None, UtcTimestamp::UNIX_EPOCH, message("safe"))
                .unwrap();
        }
        let history = store
            .session_directory(&session_id)
            .unwrap()
            .join(HISTORY_FILE);
        OpenOptions::new()
            .append(true)
            .open(&history)
            .unwrap()
            .write_all(b"{\"partial\":")
            .unwrap();
        let report = store
            .recover(&session_id, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        assert_eq!(report.entries, 1);
        assert!(report.discarded_tail_bytes > 0);
        assert_eq!(store.load_index(&session_id).unwrap().len(), 1);
    }

    #[test]
    fn complete_corruption_is_quarantined_and_remains_exportable_raw() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let history = store
            .session_directory(&session_id)
            .unwrap()
            .join(HISTORY_FILE);
        OpenOptions::new()
            .append(true)
            .open(&history)
            .unwrap()
            .write_all(b"not-json\n")
            .unwrap();
        assert!(matches!(
            store.recover(&session_id, UtcTimestamp::from_unix_millis(1)),
            Err(SessionStoreError::Quarantined(_))
        ));
        assert!(matches!(
            store.acquire_writer(&session_id, identity(2)),
            Err(SessionStoreError::Quarantined(_))
        ));
        assert_eq!(store.inspect_repair(&session_id).unwrap()[0].line, 1);
        assert!(
            store
                .export_raw(&session_id)
                .unwrap()
                .1
                .ends_with(b"not-json\n")
        );
    }

    #[test]
    fn checksums_detect_payload_changes() {
        let mut entry = SessionEntry::new(
            EntryId::new(),
            None,
            UtcTimestamp::UNIX_EPOCH,
            message("original"),
        )
        .unwrap();
        entry.payload = message("changed");
        assert!(matches!(
            entry.verify(),
            Err(SessionStoreError::ChecksumMismatch(_))
        ));
    }

    #[test]
    fn discovery_archive_and_export_follow_the_manifest() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        writer
            .append(None, UtcTimestamp::UNIX_EPOCH, message("entry"))
            .unwrap();
        writer.archive().unwrap();
        drop(writer);
        assert!(store.discover().unwrap()[0].archived);
        assert_eq!(store.export(&session_id).unwrap().entries.len(), 1);
        assert!(matches!(
            store.acquire_writer(&session_id, identity(2)),
            Err(SessionStoreError::Archived(_))
        ));
    }

    #[test]
    fn compaction_threshold_commit_and_context_rebuild_follow_selected_ancestry() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let model = writer
            .append(
                None,
                UtcTimestamp::UNIX_EPOCH,
                SessionEntryPayload::ModelChanged {
                    provider: "provider-a".into(),
                    model: "model-a".into(),
                },
            )
            .unwrap();
        let (message_entry, _) = append_completed_turn(&mut writer, "long context", 1);
        let (retained_user, _) = append_completed_turn(&mut writer, "retained context", 2);
        let policy = CompactionPolicy {
            trigger_tokens: 100,
            target_tokens: 40,
            ..CompactionPolicy::default()
        };
        assert!(
            writer
                .request_compaction(99, policy, None, CompactionTrigger::Pressure)
                .unwrap()
                .is_none()
        );
        let request = writer
            .request_compaction(100, policy, None, CompactionTrigger::Pressure)
            .unwrap()
            .unwrap();
        assert!(request.source_entries.contains(&model.id));
        assert!(!request.source_entries.contains(&retained_user.id));
        let request = writer
            .begin_compaction(request, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        let emission = writer
            .commit_compaction(
                &request,
                compaction_output(&request, &message_entry.id, "first summary"),
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(emission.memory_candidates.len(), 1);
        assert_eq!(writer.manifest().compaction_generation, 1);
        assert_ne!(
            writer.manifest().active_leaf,
            Some(emission.boundary.id.clone())
        );
        let continuation = writer
            .append(
                writer.manifest().active_leaf.clone(),
                UtcTimestamp::from_unix_millis(5),
                message("after boundary"),
            )
            .unwrap();
        let context = store
            .load_index(&session_id)
            .unwrap()
            .reconstruct_context(&continuation.id)
            .unwrap();
        assert_eq!(context.compaction_summary.as_deref(), Some("first summary"));
        assert_eq!(context.model, Some(("provider-a".into(), "model-a".into())));
        assert!(
            context
                .entries
                .iter()
                .any(|entry| entry.id == retained_user.id)
        );
        assert!(
            context
                .entries
                .iter()
                .any(|entry| entry.id == continuation.id)
        );
        assert!(
            !context
                .entries
                .iter()
                .any(|entry| entry.id == message_entry.id)
        );
    }

    #[test]
    fn recovery_republishes_a_fsynced_compaction_body_after_manifest_crash() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let (started, committed_leaf) = {
            let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
            let (source, _) = append_completed_turn(&mut writer, "source turn", 0);
            append_completed_turn(&mut writer, "retained turn", 1);
            let request = writer
                .request_compaction(
                    100_000,
                    CompactionPolicy {
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
            let started = request.started_entry_id.clone().unwrap();
            assert!(matches!(
                writer.request_compaction(
                    100_000,
                    CompactionPolicy {
                        target_tokens: 1,
                        ..CompactionPolicy::default()
                    },
                    None,
                    CompactionTrigger::Pressure,
                ),
                Err(SessionStoreError::CompactionBusy)
            ));
            writer
                .commit_compaction(
                    &request,
                    compaction_output(&request, &source.id, "durable summary"),
                    UtcTimestamp::from_unix_millis(3),
                )
                .unwrap();
            (started, writer.manifest().active_leaf.clone().unwrap())
        };
        let session_directory = store.session_directory(&session_id).unwrap();
        let mut stale = read_manifest(&session_directory).unwrap();
        stale.active_leaf = Some(started);
        stale.compaction_generation = 0;
        write_manifest(&session_directory, &stale).unwrap();
        store
            .recover(&session_id, UtcTimestamp::from_unix_millis(4))
            .unwrap();
        let recovered = store.manifest(&session_id).unwrap();
        assert_eq!(recovered.active_leaf, Some(committed_leaf));
        assert_eq!(recovered.compaction_generation, 1);
    }

    #[test]
    fn invalid_and_stale_compactions_leave_the_previous_leaf_active() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let (source, _) = append_completed_turn(&mut writer, "source turn", 0);
        append_completed_turn(&mut writer, "retained turn", 1);
        let request = writer
            .request_compaction(
                100_000,
                CompactionPolicy {
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
        let mut invalid = compaction_output(&request, &source.id, "");
        invalid.session_summary.clear();
        assert!(matches!(
            writer.commit_compaction(&request, invalid, UtcTimestamp::from_unix_millis(3)),
            Err(SessionStoreError::InvalidCompaction(_))
        ));
        assert_eq!(writer.manifest().compaction_generation, 0);
        writer
            .fail_compaction(
                &request,
                CompactionFailureStage::Summary,
                "invalid summary",
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        let stale = writer
            .request_compaction(
                100_000,
                CompactionPolicy {
                    target_tokens: 1,
                    ..CompactionPolicy::default()
                },
                None,
                CompactionTrigger::Pressure,
            )
            .unwrap()
            .unwrap();
        writer
            .append(
                writer.manifest().active_leaf.clone(),
                UtcTimestamp::from_unix_millis(5),
                message("surface changed"),
            )
            .unwrap();
        assert!(matches!(
            writer.begin_compaction(stale, UtcTimestamp::from_unix_millis(6)),
            Err(SessionStoreError::StaleCompaction)
        ));
        assert_eq!(writer.manifest().compaction_generation, 0);
        assert!(writer.active_ancestry().unwrap().iter().all(|entry| {
            !matches!(
                entry.payload,
                SessionEntryPayload::CompactionCheckpoint { .. }
            )
        }));
    }

    #[test]
    fn append_failure_exposes_no_candidates_and_keeps_the_manifest_leaf() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let (source, _) = append_completed_turn(&mut writer, "source turn", 0);
        append_completed_turn(&mut writer, "retained turn", 1);
        let request = writer
            .request_compaction(
                100_000,
                CompactionPolicy {
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
        let started = request.started_entry_id.clone().unwrap();
        fs::remove_file(writer.directory.join(HISTORY_FILE)).unwrap();
        assert!(
            writer
                .commit_compaction(
                    &request,
                    compaction_output(&request, &source.id, "summary"),
                    UtcTimestamp::from_unix_millis(3),
                )
                .is_err()
        );
        assert_eq!(writer.manifest().active_leaf, Some(started.clone()));
        assert_eq!(
            store.manifest(&session_id).unwrap().active_leaf,
            Some(started)
        );
        assert_eq!(writer.manifest().compaction_generation, 0);
    }

    #[test]
    fn repeated_compaction_advances_ranges_without_sibling_drift() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        for round in 0..3 {
            let (source, _) =
                append_completed_turn(&mut writer, &format!("source turn {round}"), round * 10);
            append_completed_turn(
                &mut writer,
                &format!("retained turn {round}"),
                round * 10 + 1,
            );
            let request = writer
                .request_compaction(
                    100_000,
                    CompactionPolicy {
                        target_tokens: 1,
                        ..CompactionPolicy::default()
                    },
                    None,
                    CompactionTrigger::Pressure,
                )
                .unwrap()
                .unwrap();
            assert!(request.source_entries.contains(&source.id));
            let request = writer
                .begin_compaction(request, UtcTimestamp::from_unix_millis(round * 10 + 2))
                .unwrap();
            writer
                .commit_compaction(
                    &request,
                    compaction_output(&request, &source.id, &format!("summary {round}")),
                    UtcTimestamp::from_unix_millis(round * 10 + 3),
                )
                .unwrap();
        }
        let index = store.load_index(&session_id).unwrap();
        let leaf = writer.manifest().active_leaf.clone().unwrap();
        let context = index.reconstruct_context(&leaf).unwrap();
        assert_eq!(context.compaction_summary.as_deref(), Some("summary 2"));
        assert_eq!(
            index
                .ancestry(&leaf)
                .unwrap()
                .iter()
                .filter(|entry| matches!(
                    entry.payload,
                    SessionEntryPayload::CompactionCheckpoint { .. }
                ))
                .count(),
            3
        );
        assert_eq!(writer.manifest().compaction_generation, 3);
    }

    proptest! {
        #[test]
        fn arbitrary_branch_parent_choices_reconstruct(
            parents in prop::collection::vec(0usize..32, 1..32)
        ) {
            let directory = tempdir().unwrap();
            let store = SessionStore::open(directory.path()).unwrap();
            let session_id = SessionId::new();
            store.create(new_session(session_id.clone())).unwrap();
            let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
            let root = writer.append(None, UtcTimestamp::UNIX_EPOCH, message("root")).unwrap();
            let mut ids = vec![root.id];
            for (offset, choice) in parents.into_iter().enumerate() {
                let parent = ids[choice % ids.len()].clone();
                let entry = writer.append(
                    Some(parent),
                    UtcTimestamp::from_unix_millis(i64::try_from(offset + 1).unwrap()),
                    message("node"),
                ).unwrap();
                ids.push(entry.id);
            }
            let index = store.load_index(&session_id).unwrap();
            for id in ids {
                let ancestry = index.ancestry(&id).unwrap();
                prop_assert_eq!(ancestry.last().map(|entry| &entry.id), Some(&id));
            }
        }
    }
}
