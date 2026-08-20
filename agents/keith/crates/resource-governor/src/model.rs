use std::collections::BTreeMap;

use keith_agent_types::{
    ActionId, EntityId, GoalId, ProfileId, Revision, RootTreeId, SessionId, UtcTimestamp,
};
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "scope", content = "id")]
pub enum ResourceScope {
    Installation,
    Worker(RootTreeId),
    Profile(ProfileId),
    Tree(RootTreeId),
    Session(SessionId),
    Goal(GoalId),
    Action(ActionId),
    ProviderAccount { provider: String, account: String },
    Tool(String),
    Child(SessionId),
    Kernel(SessionId),
    Browser(SessionId),
    Process(SessionId),
    Channel(String),
    Scheduler(ProfileId),
    Background(ProfileId),
}

impl ResourceScope {
    pub fn safe_label(&self) -> String {
        match self {
            Self::Installation => "installation".into(),
            Self::Worker(id) => format!("worker:{id}"),
            Self::Profile(id) => format!("profile:{id}"),
            Self::Tree(id) => format!("tree:{id}"),
            Self::Session(id) => format!("session:{id}"),
            Self::Goal(id) => format!("goal:{id}"),
            Self::Action(id) => format!("action:{id}"),
            Self::ProviderAccount { provider, .. } => format!("provider:{provider}:<redacted>"),
            Self::Tool(name) => format!("tool:{name}"),
            Self::Child(id) => format!("child:{id}"),
            Self::Kernel(id) => format!("kernel:{id}"),
            Self::Browser(id) => format!("browser:{id}"),
            Self::Process(id) => format!("process:{id}"),
            Self::Channel(name) => format!("channel:{name}"),
            Self::Scheduler(id) => format!("scheduler:{id}"),
            Self::Background(id) => format!("background:{id}"),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ResourceKind {
    Workers,
    ActiveSessions,
    ProviderRequests,
    SafeParallelTools,
    Children,
    RecursiveDepth,
    Kernels,
    Browsers,
    Processes,
    Channels,
    Schedules,
    BackgroundInitiatives,
    McpSessions,
    Tokens,
    ModelCostMicros,
    WallTimeMs,
    ToolCalls,
    MemoryBytes,
    StorageBytes,
    NetworkBytes,
    Retries,
    Notifications,
}

impl ResourceKind {
    pub const fn is_concurrency(self) -> bool {
        matches!(
            self,
            Self::Workers
                | Self::ActiveSessions
                | Self::ProviderRequests
                | Self::SafeParallelTools
                | Self::Children
                | Self::RecursiveDepth
                | Self::Kernels
                | Self::Browsers
                | Self::Processes
                | Self::Channels
                | Self::Schedules
                | Self::BackgroundInitiatives
                | Self::McpSessions
        )
    }

    pub const fn concurrency_kinds() -> &'static [Self] {
        &[
            Self::Workers,
            Self::ActiveSessions,
            Self::ProviderRequests,
            Self::SafeParallelTools,
            Self::Children,
            Self::RecursiveDepth,
            Self::Kernels,
            Self::Browsers,
            Self::Processes,
            Self::Channels,
            Self::Schedules,
            Self::BackgroundInitiatives,
            Self::McpSessions,
        ]
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExhaustionBehavior {
    Pause,
    Fail,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResourceCeiling {
    pub maximum: u64,
    pub exhaustion: ExhaustionBehavior,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QueueCeiling {
    pub maximum: usize,
    pub interactive_reserve: usize,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResourcePolicy {
    ceilings: BTreeMap<(ResourceScope, ResourceKind), ResourceCeiling>,
    queue_ceilings: BTreeMap<ResourceScope, QueueCeiling>,
}

impl ResourcePolicy {
    /// # Errors
    ///
    /// Returns an error when a ceiling is zero or an installation concurrency limit is missing.
    pub fn new(
        ceilings: BTreeMap<(ResourceScope, ResourceKind), ResourceCeiling>,
    ) -> Result<Self, ResourceError> {
        if ceilings.values().any(|ceiling| ceiling.maximum == 0) {
            return Err(ResourceError::Invalid(
                "resource ceilings must be non-zero".into(),
            ));
        }
        for resource in ResourceKind::concurrency_kinds() {
            if !ceilings.contains_key(&(ResourceScope::Installation, *resource)) {
                return Err(ResourceError::Invalid(format!(
                    "installation ceiling is missing for {resource:?}"
                )));
            }
        }
        Ok(Self {
            ceilings,
            queue_ceilings: BTreeMap::from([(
                ResourceScope::Installation,
                QueueCeiling {
                    maximum: 4_096,
                    interactive_reserve: 256,
                },
            )]),
        })
    }

    /// Applies bounded admission queues to installation and narrower scopes.
    ///
    /// # Errors
    ///
    /// Returns an error for a zero bound, an oversized reserve, or a missing installation bound.
    pub fn with_queue_ceilings(
        mut self,
        queue_ceilings: BTreeMap<ResourceScope, QueueCeiling>,
    ) -> Result<Self, ResourceError> {
        if !queue_ceilings.contains_key(&ResourceScope::Installation)
            || queue_ceilings
                .values()
                .any(|limit| limit.maximum == 0 || limit.interactive_reserve > limit.maximum)
        {
            return Err(ResourceError::Invalid(
                "queue ceilings require a non-zero installation bound and valid reserves".into(),
            ));
        }
        self.queue_ceilings = queue_ceilings;
        Ok(self)
    }

    pub fn ceiling(
        &self,
        scope: &ResourceScope,
        resource: ResourceKind,
    ) -> Option<ResourceCeiling> {
        self.ceilings.get(&(scope.clone(), resource)).copied()
    }

    pub fn queue_ceiling(&self, scope: &ResourceScope) -> Option<QueueCeiling> {
        self.queue_ceilings.get(scope).copied()
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(transparent)]
pub struct ScopePath(Vec<ResourceScope>);

impl ScopePath {
    /// # Errors
    ///
    /// Returns an error unless the path is bounded, unique, and rooted at the installation.
    pub fn new(scopes: Vec<ResourceScope>) -> Result<Self, ResourceError> {
        if scopes.first() != Some(&ResourceScope::Installation)
            || scopes.len() > 32
            || scopes
                .iter()
                .collect::<std::collections::BTreeSet<_>>()
                .len()
                != scopes.len()
        {
            return Err(ResourceError::Invalid(
                "scope path must start at installation and contain unique bounded scopes".into(),
            ));
        }
        Ok(Self(scopes))
    }

    pub fn scopes(&self) -> &[ResourceScope] {
        &self.0
    }

    pub fn tree(&self) -> Option<&RootTreeId> {
        self.0.iter().find_map(|scope| match scope {
            ResourceScope::Tree(id) => Some(id),
            _ => None,
        })
    }

    pub fn session(&self) -> Option<&SessionId> {
        self.0.iter().find_map(|scope| match scope {
            ResourceScope::Session(id) => Some(id),
            _ => None,
        })
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkPriority {
    Background,
    Normal,
    Interactive,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ReclaimClass {
    Worker,
    Child,
    Kernel,
    Browser,
    McpSession,
    ToolProcess,
}

impl ReclaimClass {
    pub const fn resource(self) -> ResourceKind {
        match self {
            Self::Worker => ResourceKind::Workers,
            Self::Child => ResourceKind::Children,
            Self::Kernel => ResourceKind::Kernels,
            Self::Browser => ResourceKind::Browsers,
            Self::McpSession => ResourceKind::McpSessions,
            Self::ToolProcess => ResourceKind::Processes,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecoveryDescriptor {
    pub class: ReclaimClass,
    pub durable_state_id: EntityId,
    pub resume_marker: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcquireRequest {
    pub id: EntityId,
    pub path: ScopePath,
    pub resource: ResourceKind,
    pub units: u64,
    pub priority: WorkPriority,
    pub recovery: Option<RecoveryDescriptor>,
    pub submitted_at: UtcTimestamp,
    pub idle_timeout_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResourceLease {
    pub id: EntityId,
    pub request: AcquireRequest,
    pub acquired_at: UtcTimestamp,
    pub heartbeat_at: UtcTimestamp,
    pub expires_at: UtcTimestamp,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ScheduleOutcome {
    Granted(ResourceLease),
    Paused {
        request_id: EntityId,
        scope: ResourceScope,
        resource: ResourceKind,
    },
    Failed {
        request_id: EntityId,
        scope: ResourceScope,
        resource: ResourceKind,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReclaimedResource {
    pub lease_id: EntityId,
    pub recovery: RecoveryDescriptor,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct UsageDelta {
    pub path: ScopePath,
    pub resource: ResourceKind,
    pub units: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum UsageOutcome {
    Recorded,
    Paused {
        scope: ResourceScope,
        resource: ResourceKind,
    },
    Failed {
        scope: ResourceScope,
        resource: ResourceKind,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ResourceProjection {
    pub scope: String,
    pub resource: ResourceKind,
    pub consumed: u64,
    pub active: u64,
    pub ceiling: Option<u64>,
    pub remaining: Option<u64>,
}

#[derive(Debug, thiserror::Error)]
pub enum ResourceError {
    #[error("resource request is invalid: {0}")]
    Invalid(String),
    #[error("resource request {0} already exists")]
    Duplicate(EntityId),
    #[error("resource request or lease {0} does not exist")]
    Missing(EntityId),
    #[error("resource queue at {scope} is full for {priority:?} work")]
    QueueFull {
        scope: String,
        priority: WorkPriority,
    },
    #[error("resource governor lock was poisoned")]
    LockPoisoned,
    #[error("resource repository failed: {0}")]
    Repository(String),
    #[error("resource repository write conflicted: {0}")]
    RepositoryConflict(String),
    #[error("resource record serialization failed: {0}")]
    Serialize(#[from] serde_json::Error),
}
