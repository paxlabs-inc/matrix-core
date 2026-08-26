use std::collections::BTreeSet;
use std::path::PathBuf;

use keith_agent_types::{
    ChildId, GoalId, MessageId, ProfileId, Revision, RootTreeId, SessionId, UtcTimestamp,
    WorkspaceId,
};
use keith_artifacts::ArtifactError;
use keith_artifacts::ArtifactReference;
use keith_session::SessionError;
use keith_session_store::SessionStoreError;
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChildWorkspaceMode {
    ReadOnlyParent,
    IsolatedCopy,
    DedicatedWorkspace,
    SharedParent,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChildCancellation {
    Propagate,
    DetachAsOrphan,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "retention", content = "delay_ms")]
pub enum ChildRetention {
    Retain,
    ArchiveAfter(u64),
    DeleteAfter(u64),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChildLimits {
    pub max_depth: u16,
    pub max_direct_children: u16,
    pub max_descendants: u32,
    pub max_messages: u32,
    pub max_message_bytes: usize,
    pub max_artifacts_per_message: usize,
    pub message_window_ms: u64,
    pub max_messages_per_window: u32,
    pub max_runtime_ms: u64,
    pub heartbeat_timeout_ms: u64,
    pub orphan_grace_ms: u64,
    pub max_copy_bytes: u64,
    pub max_copy_files: u32,
}

impl Default for ChildLimits {
    fn default() -> Self {
        Self {
            max_depth: 4,
            max_direct_children: 8,
            max_descendants: 32,
            max_messages: 1_000,
            max_message_bytes: 256 * 1024,
            max_artifacts_per_message: 32,
            message_window_ms: 60_000,
            max_messages_per_window: 60,
            max_runtime_ms: 8 * 60 * 60 * 1_000,
            heartbeat_timeout_ms: 30_000,
            orphan_grace_ms: 5 * 60 * 1_000,
            max_copy_bytes: 1024 * 1024 * 1024,
            max_copy_files: 100_000,
        }
    }
}

impl ChildLimits {
    pub(crate) fn validate(self) -> Result<(), ChildError> {
        if self.max_depth == 0
            || self.max_direct_children == 0
            || self.max_descendants == 0
            || self.max_messages == 0
            || self.max_message_bytes == 0
            || self.max_artifacts_per_message == 0
            || self.message_window_ms == 0
            || self.max_messages_per_window == 0
            || self.max_runtime_ms == 0
            || self.heartbeat_timeout_ms == 0
            || self.orphan_grace_ms == 0
            || self.max_copy_bytes == 0
            || self.max_copy_files == 0
        {
            Err(ChildError::Invalid(
                "all child limits must be non-zero".into(),
            ))
        } else {
            Ok(())
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChildSpec {
    pub parent_session_id: SessionId,
    pub objective: String,
    pub workspace_mode: ChildWorkspaceMode,
    pub requested_tools: BTreeSet<String>,
    pub provider: String,
    pub model: String,
    pub limits: ChildLimits,
    pub cancellation: ChildCancellation,
    pub retention: ChildRetention,
    pub requested_authority: ChildAuthorityCeiling,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChildAutonomyCeiling {
    Off,
    Suggest,
    ConfirmSelected,
    Bounded,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChildModelRoute {
    pub provider: String,
    pub model: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChildAuthorityCeiling {
    pub tools: BTreeSet<String>,
    pub credential_scopes: BTreeSet<String>,
    pub model_routes: BTreeSet<ChildModelRoute>,
    pub network_origins: BTreeSet<String>,
    pub readable_roots: BTreeSet<PathBuf>,
    pub writable_roots: BTreeSet<PathBuf>,
    pub sandbox_required: bool,
    pub may_request_approval: bool,
    pub may_approve: bool,
    pub autonomy: ChildAutonomyCeiling,
    pub computer: bool,
    pub self_evolution: bool,
}

impl ChildAuthorityCeiling {
    pub fn denied() -> Self {
        Self {
            tools: BTreeSet::new(),
            credential_scopes: BTreeSet::new(),
            model_routes: BTreeSet::new(),
            network_origins: BTreeSet::new(),
            readable_roots: BTreeSet::new(),
            writable_roots: BTreeSet::new(),
            sandbox_required: true,
            may_request_approval: false,
            may_approve: false,
            autonomy: ChildAutonomyCeiling::Off,
            computer: false,
            self_evolution: false,
        }
    }

    pub fn is_within(&self, parent: &Self) -> bool {
        self.is_within_non_filesystem(parent)
            && roots_are_within(&self.readable_roots, &parent.readable_roots)
            && roots_are_within(&self.writable_roots, &parent.writable_roots)
    }

    fn is_within_non_filesystem(&self, parent: &Self) -> bool {
        self.tools.is_subset(&parent.tools)
            && self.credential_scopes.is_subset(&parent.credential_scopes)
            && self.model_routes.is_subset(&parent.model_routes)
            && self.network_origins.is_subset(&parent.network_origins)
            && (!parent.sandbox_required || self.sandbox_required)
            && (!self.may_request_approval || parent.may_request_approval)
            && !self.may_approve
            && self.autonomy <= parent.autonomy
            && (!self.computer || parent.computer)
            && !self.self_evolution
    }

    pub(crate) fn confined_to_workspace(
        mut self,
        mode: ChildWorkspaceMode,
        workspace_root: PathBuf,
    ) -> Self {
        self.readable_roots.clear();
        self.readable_roots.insert(workspace_root.clone());
        self.writable_roots.clear();
        if mode != ChildWorkspaceMode::ReadOnlyParent {
            self.writable_roots.insert(workspace_root);
        }
        self
    }

    pub fn validate(&self) -> Result<(), ChildError> {
        let invalid_text = self
            .tools
            .iter()
            .chain(&self.credential_scopes)
            .chain(&self.network_origins)
            .any(|value| value.trim().is_empty() || value.len() > 2_048);
        let invalid_route = self.model_routes.iter().any(|route| {
            route.provider.trim().is_empty()
                || route.provider.len() > 256
                || route.model.trim().is_empty()
                || route.model.len() > 512
        });
        if invalid_text
            || invalid_route
            || self.may_approve
            || self.self_evolution
            || !self.writable_roots.is_subset(&self.readable_roots)
        {
            Err(ChildError::AuthorityEscalation)
        } else {
            Ok(())
        }
    }
}

impl ChildSpec {
    pub fn validate_authority(&self, parent: &ParentAuthority) -> Result<(), ChildError> {
        self.requested_authority.validate()?;
        if self.requested_tools != self.requested_authority.tools
            || !self.requested_tools.is_subset(&parent.allowed_tools)
            || !self.requested_tools.is_subset(&parent.ceiling.tools)
        {
            return Err(ChildError::ToolEscalation);
        }
        let route = ChildModelRoute {
            provider: self.provider.clone(),
            model: self.model.clone(),
        };
        if self.parent_session_id != parent.session_id
            || !self.requested_authority.model_routes.contains(&route)
            || !self.requested_authority.is_within(&parent.ceiling)
        {
            return Err(ChildError::AuthorityEscalation);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ParentAuthority {
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub workspace_root: PathBuf,
    pub allowed_tools: BTreeSet<String>,
    pub ceiling: ChildAuthorityCeiling,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "principal")]
pub enum ChildPrincipal {
    DurableChild {
        child_id: ChildId,
        child_session_id: SessionId,
        parent_session_id: SessionId,
        root_tree_id: RootTreeId,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChildStatus {
    Starting,
    Running,
    Waiting,
    Complete,
    Failed,
    Cancelled,
    Orphaned,
    Archived,
}

impl ChildStatus {
    pub const fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Complete | Self::Failed | Self::Cancelled | Self::Archived
        )
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChildRecord {
    pub id: ChildId,
    pub session_id: SessionId,
    pub parent_session_id: SessionId,
    pub origin_parent_session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub objective: String,
    pub goal_id: GoalId,
    pub provider: String,
    pub model: String,
    pub artifact_directory: PathBuf,
    pub workspace_mode: ChildWorkspaceMode,
    pub workspace_root: PathBuf,
    pub allowed_tools: BTreeSet<String>,
    pub authority: ChildAuthorityCeiling,
    pub limits: ChildLimits,
    pub depth: u16,
    pub status: ChildStatus,
    pub heartbeat_at: UtcTimestamp,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub terminal_at: Option<UtcTimestamp>,
    pub terminal_summary: Option<String>,
    pub orphaned_at: Option<UtcTimestamp>,
    pub cancellation: ChildCancellation,
    pub retention: ChildRetention,
    pub revision: Revision,
}

impl ChildRecord {
    pub fn principal(&self) -> ChildPrincipal {
        ChildPrincipal::DurableChild {
            child_id: self.id.clone(),
            child_session_id: self.session_id.clone(),
            parent_session_id: self.parent_session_id.clone(),
            root_tree_id: self.root_tree_id.clone(),
        }
    }

    pub fn validate_parent_authority(&self, parent: &ParentAuthority) -> Result<(), ChildError> {
        self.authority.validate()?;
        if self.parent_session_id != parent.session_id
            || self.root_tree_id != parent.root_tree_id
            || self.profile_id != parent.profile_id
            || self.allowed_tools != self.authority.tools
            || !self.authority.is_within_non_filesystem(&parent.ceiling)
            || !self.allowed_tools.is_subset(&parent.allowed_tools)
            || !self.authority.model_routes.contains(&ChildModelRoute {
                provider: self.provider.clone(),
                model: self.model.clone(),
            })
            || !workspace_authority_matches(self, parent)
        {
            return Err(ChildError::AuthorityEscalation);
        }
        Ok(())
    }

    pub(crate) fn validate_intrinsic_authority(&self) -> Result<(), ChildError> {
        self.authority.validate()?;
        if self.allowed_tools != self.authority.tools
            || !self.authority.model_routes.contains(&ChildModelRoute {
                provider: self.provider.clone(),
                model: self.model.clone(),
            })
            || !workspace_record_is_confined(self)
        {
            return Err(ChildError::AuthorityEscalation);
        }
        Ok(())
    }
}

fn roots_are_within(children: &BTreeSet<PathBuf>, parents: &BTreeSet<PathBuf>) -> bool {
    children
        .iter()
        .all(|child| parents.iter().any(|parent| child.starts_with(parent)))
}

fn workspace_authority_matches(child: &ChildRecord, parent: &ParentAuthority) -> bool {
    if !workspace_record_is_confined(child) {
        return false;
    }
    match child.workspace_mode {
        ChildWorkspaceMode::ReadOnlyParent | ChildWorkspaceMode::SharedParent => {
            child.workspace_id == parent.workspace_id
                && child.workspace_root == parent.workspace_root
        }
        ChildWorkspaceMode::IsolatedCopy | ChildWorkspaceMode::DedicatedWorkspace => {
            child.workspace_id != parent.workspace_id
                && child.workspace_root != parent.workspace_root
        }
    }
}

fn workspace_record_is_confined(child: &ChildRecord) -> bool {
    let readable = child.authority.readable_roots.len() == 1
        && child
            .authority
            .readable_roots
            .contains(&child.workspace_root);
    let writable = child.authority.writable_roots.len() == 1
        && child
            .authority
            .writable_roots
            .contains(&child.workspace_root);
    match child.workspace_mode {
        ChildWorkspaceMode::ReadOnlyParent => {
            readable && !writable && child.authority.writable_roots.is_empty()
        }
        ChildWorkspaceMode::SharedParent => readable && writable,
        ChildWorkspaceMode::IsolatedCopy | ChildWorkspaceMode::DedicatedWorkspace => {
            readable && writable
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChildMessageSender {
    Parent,
    Child,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum ChildMessageKind {
    Text { text: String },
    Request { request: String },
    Status { status: String },
    Artifacts { references: Vec<ArtifactReference> },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChildMessage {
    pub id: MessageId,
    pub child_id: ChildId,
    pub parent_session_id: SessionId,
    pub child_session_id: SessionId,
    pub sender: ChildMessageSender,
    pub sequence: u64,
    pub created_at: UtcTimestamp,
    pub kind: ChildMessageKind,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChildProjection {
    pub id: ChildId,
    pub session_id: SessionId,
    pub parent_session_id: SessionId,
    pub objective: String,
    pub status: ChildStatus,
    pub depth: u16,
    pub workspace_mode: ChildWorkspaceMode,
    pub allowed_tools: BTreeSet<String>,
    pub heartbeat_at: UtcTimestamp,
    pub terminal_summary: Option<String>,
    pub orphaned: bool,
    pub retention: ChildRetention,
}

impl From<&ChildRecord> for ChildProjection {
    fn from(child: &ChildRecord) -> Self {
        Self {
            id: child.id.clone(),
            session_id: child.session_id.clone(),
            parent_session_id: child.parent_session_id.clone(),
            objective: child.objective.clone(),
            status: child.status,
            depth: child.depth,
            workspace_mode: child.workspace_mode,
            allowed_tools: child.allowed_tools.clone(),
            heartbeat_at: child.heartbeat_at,
            terminal_summary: child.terminal_summary.clone(),
            orphaned: child.status == ChildStatus::Orphaned,
            retention: child.retention,
        }
    }
}

#[derive(Debug, Error)]
pub enum ChildError {
    #[error("child repository failed: {0}")]
    Repository(String),
    #[error("child session store failed: {0}")]
    SessionStore(#[from] SessionStoreError),
    #[error("child actor failed: {0}")]
    Session(#[from] SessionError),
    #[error("child I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("child JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("child artifact service failed: {0}")]
    Artifact(#[from] ArtifactError),
    #[error("child {0} was not found")]
    NotFound(ChildId),
    #[error("child record is corrupt: {0}")]
    Corrupt(String),
    #[error("child request is invalid: {0}")]
    Invalid(String),
    #[error("child tool request exceeds parent authority")]
    ToolEscalation,
    #[error("child authority exceeds its authenticated parent ceiling")]
    AuthorityEscalation,
    #[error("child recursive depth or count limit was reached")]
    RecursiveLimit,
    #[error("child message rate or count limit was reached")]
    MessageLimit,
    #[error("child is not in a valid lifecycle state for this operation")]
    InvalidState,
    #[error("child profile, tree, parent, or artifact scope does not match")]
    ScopeDenied,
    #[error("child workspace contains a symbolic link or exceeds copy bounds")]
    WorkspaceDenied,
    #[error("child persistence revision overflow")]
    RevisionOverflow,
    #[error("child coordinator state lock was poisoned")]
    LockPoisoned,
}

pub(crate) struct StoredChild {
    pub record: ChildRecord,
    pub storage_revision: Revision,
}

pub(crate) struct StoredMessage {
    pub message: ChildMessage,
}
