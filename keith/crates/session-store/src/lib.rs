#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs::{self, File, OpenOptions};
use std::io::{Seek, SeekFrom, Write};
use std::path::{Path, PathBuf};

use fs2::FileExt;
use keith_agent_types::{
    ArtifactId, CURRENT_SCHEMA_VERSION, ChildId, EntityId, EntryId, Generation, GoalId, ProfileId,
    Revision, RootTreeId, SchemaVersion, SessionId, ToolCallId, UtcTimestamp, WorkerId,
    WorkspaceId, canonical_json_bytes,
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

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "payload")]
pub enum SessionEntryPayload {
    UserMessage {
        message: StoredMessage,
    },
    AssistantMessage {
        message: StoredMessage,
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
    ) -> Result<Option<CompactionRequest>, SessionStoreError> {
        validate_compaction_policy(policy)?;
        if estimated_tokens < policy.trigger_tokens {
            return Ok(None);
        }
        let Some(selected_leaf) = &manifest.active_leaf else {
            return Ok(None);
        };
        let ancestry = self.ancestry(selected_leaf)?;
        let previous_index = ancestry
            .iter()
            .rposition(|entry| matches!(entry.payload, SessionEntryPayload::Compaction { .. }));
        let range_index = previous_index.map_or(0, |index| index + 1);
        let Some(range_start) = ancestry.get(range_index) else {
            return Ok(None);
        };
        Ok(Some(CompactionRequest {
            id: EntityId::new(),
            session_id: manifest.session_id.clone(),
            selected_leaf: selected_leaf.clone(),
            range_start: range_start.id.clone(),
            range_end: selected_leaf.clone(),
            previous_boundary: previous_index.map(|index| ancestry[index].id.clone()),
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
                SessionEntryPayload::UserMessage { .. }
                | SessionEntryPayload::AssistantMessage { .. }
                | SessionEntryPayload::ToolCall { .. }
                | SessionEntryPayload::ToolResult { .. }
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
            |index| ancestry.iter().skip(index + 1).cloned().collect(),
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
    pub range_start: EntryId,
    pub range_end: EntryId,
    pub previous_boundary: Option<EntryId>,
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
    #[error("compaction selected leaf changed before commit")]
    StaleCompaction,
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

    /// # Errors
    ///
    /// Returns an error when the active ancestry is corrupt or the policy is invalid.
    pub fn request_compaction(
        &self,
        estimated_tokens: u64,
        policy: CompactionPolicy,
    ) -> Result<Option<CompactionRequest>, SessionStoreError> {
        self.ensure_writable()?;
        parse_complete_history(&self.directory.join(HISTORY_FILE))?.compaction_request(
            &self.manifest,
            estimated_tokens,
            policy,
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
        if request.session_id != self.manifest.session_id
            || self.manifest.active_leaf.as_ref() != Some(&request.selected_leaf)
        {
            return Err(SessionStoreError::StaleCompaction);
        }
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        validate_compaction_output(request, &output, &index)?;
        let boundary = self.append(
            Some(request.selected_leaf.clone()),
            timestamp,
            SessionEntryPayload::Compaction {
                summary: output.session_summary,
                compacted_through: request.range_end.clone(),
            },
        )?;
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
    if output.request_id != request.id {
        return Err(SessionStoreError::InvalidCompaction(
            "structured output belongs to another request".into(),
        ));
    }
    if output.session_summary.trim().is_empty()
        || output.session_summary.len() > request.max_summary_bytes
    {
        return Err(SessionStoreError::InvalidCompaction(
            "session summary is empty or oversized".into(),
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
    if start > end || request.range_end != request.selected_leaf {
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
        let message_entry = writer
            .append(
                Some(model.id),
                UtcTimestamp::from_unix_millis(1),
                message("long context"),
            )
            .unwrap();
        let policy = CompactionPolicy {
            trigger_tokens: 100,
            target_tokens: 40,
            ..CompactionPolicy::default()
        };
        assert!(writer.request_compaction(99, policy).unwrap().is_none());
        let request = writer.request_compaction(100, policy).unwrap().unwrap();
        let emission = writer
            .commit_compaction(
                &request,
                compaction_output(&request, &message_entry.id, "first summary"),
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(emission.memory_candidates.len(), 1);
        assert_eq!(
            writer.manifest().active_leaf,
            Some(emission.boundary.id.clone())
        );
        let continuation = writer
            .append(
                Some(emission.boundary.id),
                UtcTimestamp::from_unix_millis(3),
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
        assert_eq!(context.entries.len(), 1);
        assert_eq!(context.entries[0].id, continuation.id);
    }

    #[test]
    fn invalid_and_stale_compactions_leave_the_previous_leaf_active() {
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
        let request = writer
            .request_compaction(100_000, CompactionPolicy::default())
            .unwrap()
            .unwrap();
        let mut invalid = compaction_output(&request, &left.id, "");
        invalid.session_summary.clear();
        assert!(matches!(
            writer.commit_compaction(&request, invalid, UtcTimestamp::from_unix_millis(2)),
            Err(SessionStoreError::InvalidCompaction(_))
        ));
        assert_eq!(writer.manifest().active_leaf, Some(left.id.clone()));
        let right = writer
            .append(
                Some(root.id),
                UtcTimestamp::from_unix_millis(3),
                message("right"),
            )
            .unwrap();
        assert!(matches!(
            writer.commit_compaction(
                &request,
                compaction_output(&request, &left.id, "stale"),
                UtcTimestamp::from_unix_millis(4),
            ),
            Err(SessionStoreError::StaleCompaction)
        ));
        assert_eq!(writer.manifest().active_leaf, Some(right.id));
    }

    #[test]
    fn append_failure_exposes_no_candidates_and_keeps_the_manifest_leaf() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let leaf = writer
            .append(None, UtcTimestamp::UNIX_EPOCH, message("leaf"))
            .unwrap();
        let request = writer
            .request_compaction(100_000, CompactionPolicy::default())
            .unwrap()
            .unwrap();
        fs::remove_file(writer.directory.join(HISTORY_FILE)).unwrap();
        assert!(
            writer
                .commit_compaction(
                    &request,
                    compaction_output(&request, &leaf.id, "summary"),
                    UtcTimestamp::from_unix_millis(1),
                )
                .is_err()
        );
        assert_eq!(writer.manifest().active_leaf, Some(leaf.id.clone()));
        assert_eq!(
            store.manifest(&session_id).unwrap().active_leaf,
            Some(leaf.id)
        );
    }

    #[test]
    fn repeated_compaction_advances_ranges_without_sibling_drift() {
        let directory = tempdir().unwrap();
        let store = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        store.create(new_session(session_id.clone())).unwrap();
        let mut writer = store.acquire_writer(&session_id, identity(1)).unwrap();
        let mut leaf = writer
            .append(None, UtcTimestamp::UNIX_EPOCH, message("start"))
            .unwrap();
        for round in 0..3 {
            let request = writer
                .request_compaction(100_000, CompactionPolicy::default())
                .unwrap()
                .unwrap();
            assert_eq!(request.range_end, leaf.id);
            let emission = writer
                .commit_compaction(
                    &request,
                    compaction_output(&request, &leaf.id, &format!("summary {round}")),
                    UtcTimestamp::from_unix_millis(round * 2 + 1),
                )
                .unwrap();
            leaf = writer
                .append(
                    Some(emission.boundary.id),
                    UtcTimestamp::from_unix_millis(round * 2 + 2),
                    message("continuation"),
                )
                .unwrap();
        }
        let index = store.load_index(&session_id).unwrap();
        let context = index.reconstruct_context(&leaf.id).unwrap();
        assert_eq!(context.compaction_summary.as_deref(), Some("summary 2"));
        assert_eq!(context.entries.len(), 1);
        assert_eq!(
            index
                .children_of(Some(&leaf.parent_id.clone().unwrap()))
                .len(),
            1
        );
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
