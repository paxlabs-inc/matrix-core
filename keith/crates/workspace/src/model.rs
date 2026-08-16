use std::collections::BTreeMap;
use std::path::PathBuf;

use keith_agent_types::{EntityId, Revision, SchemaVersion, UtcTimestamp};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct PersonalWorkspaceLimits {
    pub max_file_bytes: usize,
    pub max_files: usize,
    pub max_total_bytes: u64,
    pub watcher_interval_ms: u64,
}

impl Default for PersonalWorkspaceLimits {
    fn default() -> Self {
        Self {
            max_file_bytes: 16 * 1_024 * 1_024,
            max_files: 100_000,
            max_total_bytes: 512 * 1_024 * 1_024,
            watcher_interval_ms: 250,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkspaceLayout {
    pub root: PathBuf,
    pub agent: PathBuf,
    pub user: PathBuf,
    pub rules: PathBuf,
    pub memory: PathBuf,
    pub daily_memory: PathBuf,
    pub state: PathBuf,
    pub knowledge: PathBuf,
    pub skills: PathBuf,
    pub artifacts: PathBuf,
    pub backups: PathBuf,
    pub metadata: PathBuf,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WorkspaceActor {
    Human,
    Agent,
    MemoryTool,
    SkillTool,
    RefinementTool,
    System,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum VersionOrigin {
    Initial,
    External,
    Agent,
    Restore,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FileVersion {
    pub revision: Revision,
    pub path: PathBuf,
    pub digest: Option<String>,
    pub bytes: u64,
    pub origin: VersionOrigin,
    pub recorded_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FileToken {
    pub revision: Option<Revision>,
    pub digest: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct MergeProposal {
    pub path: PathBuf,
    pub expected: FileToken,
    pub current: Vec<u8>,
    pub proposed: Vec<u8>,
    pub merged: Option<Vec<u8>>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum EditOutcome {
    Written(FileVersion),
    Conflict(MergeProposal),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WorkspaceEvent {
    Changed {
        version: FileVersion,
        context_revision: Revision,
    },
    Rejected {
        path: PathBuf,
        reason: String,
    },
    WatcherError {
        reason: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SnapshotManifest {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub label: String,
    pub created_at: UtcTimestamp,
    pub files: BTreeMap<PathBuf, FileTokenSnapshot>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FileTokenSnapshot {
    pub revision: Revision,
    pub digest: String,
    pub bytes: u64,
}

#[derive(Debug, thiserror::Error)]
pub enum PersonalWorkspaceError {
    #[error("workspace I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("workspace JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("workspace path is unsafe or outside the profile root")]
    UnsafePath,
    #[error("workspace path is protected from this actor")]
    Protected,
    #[error("workspace contains a symbolic link")]
    Symlink,
    #[error("workspace file is not valid UTF-8")]
    NonUtf8,
    #[error("workspace size or entry limit was exceeded")]
    LimitExceeded,
    #[error("workspace metadata is corrupt: {0}")]
    Corrupt(String),
    #[error("workspace mutation lock was poisoned")]
    LockPoisoned,
    #[error("workspace watcher interval must be non-zero")]
    InvalidWatcherInterval,
    #[error("workspace watcher disconnected")]
    WatcherDisconnected,
    #[error("workspace watcher thread panicked")]
    WatcherPanicked,
    #[error("workspace version or snapshot was not found")]
    MissingVersion,
    #[error("workspace filesystem operation failed: {0}")]
    Filesystem(#[from] keith_tool_runner_core::WorkspaceError),
}
