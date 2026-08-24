use std::path::{Path, PathBuf};

use keith_agent_types::{ChildId, ProfileId};
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum ComputerActor {
    Agent {
        profile_id: ProfileId,
    },
    Routine {
        profile_id: ProfileId,
    },
    Child {
        parent_profile_id: ProfileId,
        child_id: ChildId,
    },
    User,
}

impl ComputerActor {
    pub fn is_authorized_for(&self, owner: &ProfileId) -> bool {
        match self {
            Self::Agent { profile_id } | Self::Routine { profile_id } => profile_id == owner,
            Self::Child {
                parent_profile_id, ..
            } => parent_profile_id == owner,
            Self::User => true,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ComputerAction {
    Navigate {
        origin: String,
    },
    Download {
        destination: PathBuf,
    },
    Upload {
        source: PathBuf,
    },
    FilesystemWrite {
        destination: PathBuf,
    },
    Consequential {
        summary: String,
        approved: bool,
    },
    CredentialUse {
        credential_ref: String,
        approved: bool,
    },
    Input,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum BoundaryDecision {
    Allow,
    RequireApproval,
    Deny,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerResourcePolicy {
    pub cpu_quota_percent: u16,
    pub max_memory_bytes: u64,
    pub max_processes: u32,
    pub max_disk_bytes: u64,
    pub max_download_bytes: u64,
    pub max_network_requests_per_minute: u32,
    pub idle_timeout_seconds: u64,
    pub crash_limit: u32,
    pub crash_window_seconds: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerBoundaryPolicy {
    pub allowed_origins: Vec<String>,
    pub writable_roots: Vec<PathBuf>,
    pub download_root: PathBuf,
    pub allow_credentials: bool,
    pub resources: ComputerResourcePolicy,
}

impl ComputerBoundaryPolicy {
    pub fn evaluate(&self, action: &ComputerAction) -> BoundaryDecision {
        match action {
            ComputerAction::Navigate { origin } => {
                if origin.len() > 2_048 || !origin.starts_with("https://") {
                    BoundaryDecision::Deny
                } else if self.allowed_origins.is_empty()
                    || self.allowed_origins.iter().any(|allowed| allowed == origin)
                {
                    BoundaryDecision::Allow
                } else {
                    BoundaryDecision::Deny
                }
            }
            ComputerAction::Download { destination } => {
                path_within(destination, &self.download_root)
                    .then_some(BoundaryDecision::Allow)
                    .unwrap_or(BoundaryDecision::Deny)
            }
            ComputerAction::Upload { source } => self
                .writable_roots
                .iter()
                .any(|root| path_within(source, root))
                .then_some(BoundaryDecision::RequireApproval)
                .unwrap_or(BoundaryDecision::Deny),
            ComputerAction::FilesystemWrite { destination } => self
                .writable_roots
                .iter()
                .any(|root| path_within(destination, root))
                .then_some(BoundaryDecision::Allow)
                .unwrap_or(BoundaryDecision::Deny),
            ComputerAction::Consequential { approved, summary } => {
                if summary.trim().is_empty() || summary.len() > 2_048 {
                    BoundaryDecision::Deny
                } else if *approved {
                    BoundaryDecision::Allow
                } else {
                    BoundaryDecision::RequireApproval
                }
            }
            ComputerAction::CredentialUse {
                approved,
                credential_ref,
            } => {
                if !self.allow_credentials || credential_ref.trim().is_empty() {
                    BoundaryDecision::Deny
                } else if *approved {
                    BoundaryDecision::Allow
                } else {
                    BoundaryDecision::RequireApproval
                }
            }
            ComputerAction::Input => BoundaryDecision::Allow,
        }
    }

    pub fn validate(&self) -> Result<(), &'static str> {
        if self.resources.cpu_quota_percent == 0
            || self.resources.cpu_quota_percent > 1_000
            || self.resources.max_memory_bytes == 0
            || self.resources.max_processes == 0
            || self.resources.max_disk_bytes == 0
            || self.resources.max_download_bytes == 0
            || self.resources.max_network_requests_per_minute == 0
            || self.resources.idle_timeout_seconds == 0
            || self.resources.crash_limit == 0
            || self.resources.crash_window_seconds == 0
        {
            return Err("computer resource bounds must be nonzero");
        }
        if self
            .allowed_origins
            .iter()
            .any(|origin| !origin.starts_with("https://") || origin.len() > 2_048)
        {
            return Err("allowed origins must be bounded HTTPS origins");
        }
        if !self.download_root.is_absolute()
            || self.writable_roots.iter().any(|root| !root.is_absolute())
        {
            return Err("computer filesystem roots must be absolute");
        }
        Ok(())
    }
}

fn path_within(candidate: &Path, root: &Path) -> bool {
    candidate.is_absolute()
        && candidate.starts_with(root)
        && !candidate
            .components()
            .any(|part| matches!(part, std::path::Component::ParentDir))
}
