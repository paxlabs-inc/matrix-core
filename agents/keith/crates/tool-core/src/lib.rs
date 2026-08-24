#![forbid(unsafe_code)]

/// Canonical contracts for the built-in peer tools. Authentication and effective
/// authority are deliberately supplied by the executor rather than accepted in
/// these inputs.
pub mod peer_tools {
    use keith_agent_types::{
        AssignmentId, ConversationId, DeliveryId, EventId, ProfileId, StableKey, UtcTimestamp,
    };
    use serde::{Deserialize, Deserializer, Serialize};
    use std::fmt;

    pub const PEER_TOOL_SCHEMA_VERSION: u16 = 1;
    pub const MAX_PEER_MESSAGE_CHARS: usize = 32_768;
    pub const MAX_ASSIGNMENT_OBJECTIVE_CHARS: usize = 4_096;
    pub const MAX_ASSIGNMENT_STATUS_CHARS: usize = 2_048;
    pub const MAX_ASSIGNMENT_DEPENDENCIES: usize = 64;

    pub const MESSAGE_AGENT_TOOL_NAME: &str = "message_agent";
    pub const ASSIGN_WORK_TOOL_NAME: &str = "assign_work";
    pub const HANDOFF_WORK_TOOL_NAME: &str = "handoff_work";
    pub const REPORT_ASSIGNMENT_TOOL_NAME: &str = "report_assignment";

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub struct BuiltInPeerToolSchema {
        pub name: &'static str,
        pub input_type: &'static str,
        pub receipt_type: &'static str,
        pub schema_version: u16,
    }

    pub const BUILT_IN_PEER_TOOL_SCHEMAS: [BuiltInPeerToolSchema; 4] = [
        BuiltInPeerToolSchema {
            name: MESSAGE_AGENT_TOOL_NAME,
            input_type: "MessageAgentInput",
            receipt_type: "MessageAgentReceipt",
            schema_version: PEER_TOOL_SCHEMA_VERSION,
        },
        BuiltInPeerToolSchema {
            name: ASSIGN_WORK_TOOL_NAME,
            input_type: "AssignWorkInput",
            receipt_type: "AssignWorkReceipt",
            schema_version: PEER_TOOL_SCHEMA_VERSION,
        },
        BuiltInPeerToolSchema {
            name: HANDOFF_WORK_TOOL_NAME,
            input_type: "HandoffWorkInput",
            receipt_type: "HandoffWorkReceipt",
            schema_version: PEER_TOOL_SCHEMA_VERSION,
        },
        BuiltInPeerToolSchema {
            name: REPORT_ASSIGNMENT_TOOL_NAME,
            input_type: "ReportAssignmentInput",
            receipt_type: "ReportAssignmentReceipt",
            schema_version: PEER_TOOL_SCHEMA_VERSION,
        },
    ];

    #[derive(Clone, Debug, Eq, PartialEq, Serialize)]
    #[serde(transparent)]
    pub struct PeerMessage(String);

    impl PeerMessage {
        pub fn new(value: impl Into<String>) -> Result<Self, PeerToolContractError> {
            canonical_bounded(value.into(), MAX_PEER_MESSAGE_CHARS, "message").map(Self)
        }

        pub fn as_str(&self) -> &str {
            &self.0
        }
    }

    impl<'de> Deserialize<'de> for PeerMessage {
        fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
        where
            D: Deserializer<'de>,
        {
            let value = String::deserialize(deserializer)?;
            Self::new(value).map_err(serde::de::Error::custom)
        }
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize)]
    #[serde(transparent)]
    pub struct AssignmentObjective(String);

    impl AssignmentObjective {
        pub fn new(value: impl Into<String>) -> Result<Self, PeerToolContractError> {
            canonical_bounded(
                value.into(),
                MAX_ASSIGNMENT_OBJECTIVE_CHARS,
                "assignment objective",
            )
            .map(Self)
        }

        pub fn as_str(&self) -> &str {
            &self.0
        }
    }

    impl<'de> Deserialize<'de> for AssignmentObjective {
        fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
        where
            D: Deserializer<'de>,
        {
            let value = String::deserialize(deserializer)?;
            Self::new(value).map_err(serde::de::Error::custom)
        }
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize)]
    #[serde(transparent)]
    pub struct AssignmentStatusDetail(String);

    impl AssignmentStatusDetail {
        pub fn new(value: impl Into<String>) -> Result<Self, PeerToolContractError> {
            canonical_bounded(
                value.into(),
                MAX_ASSIGNMENT_STATUS_CHARS,
                "assignment status",
            )
            .map(Self)
        }

        pub fn as_str(&self) -> &str {
            &self.0
        }
    }

    impl<'de> Deserialize<'de> for AssignmentStatusDetail {
        fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
        where
            D: Deserializer<'de>,
        {
            let value = String::deserialize(deserializer)?;
            Self::new(value).map_err(serde::de::Error::custom)
        }
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct MessageAgentInput {
        pub schema_version: u16,
        pub idempotency_key: StableKey,
        pub correlation_key: StableKey,
        pub target_profile_id: ProfileId,
        pub conversation_id: ConversationId,
        pub expected_conversation_revision: u64,
        pub message: PeerMessage,
    }

    impl MessageAgentInput {
        pub fn validate(&self) -> Result<(), PeerToolContractError> {
            validate_version(self.schema_version)
        }
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct AssignWorkInput {
        pub schema_version: u16,
        pub idempotency_key: StableKey,
        pub correlation_key: StableKey,
        pub target_profile_id: ProfileId,
        pub conversation_id: ConversationId,
        pub expected_conversation_revision: u64,
        pub objective: AssignmentObjective,
        #[serde(default)]
        pub dependency_ids: Vec<AssignmentId>,
    }

    impl AssignWorkInput {
        pub fn validate(&self) -> Result<(), PeerToolContractError> {
            validate_version(self.schema_version)?;
            if self.dependency_ids.len() > MAX_ASSIGNMENT_DEPENDENCIES {
                return Err(PeerToolContractError::TooManyDependencies);
            }
            if !self.dependency_ids.windows(2).all(|ids| ids[0] < ids[1]) {
                return Err(PeerToolContractError::NonCanonicalDependencies);
            }
            Ok(())
        }
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct HandoffWorkInput {
        pub schema_version: u16,
        pub idempotency_key: StableKey,
        pub correlation_key: StableKey,
        pub assignment_id: AssignmentId,
        pub conversation_id: ConversationId,
        pub target_profile_id: ProfileId,
        pub expected_assignment_revision: u64,
    }

    impl HandoffWorkInput {
        pub fn validate(&self) -> Result<(), PeerToolContractError> {
            validate_version(self.schema_version)
        }
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(rename_all = "snake_case", tag = "status", deny_unknown_fields)]
    pub enum AssignmentReport {
        Active {
            detail: AssignmentStatusDetail,
        },
        Blocked {
            reason: AssignmentStatusDetail,
        },
        Completed {
            result_event_id: EventId,
            summary: AssignmentStatusDetail,
        },
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct ReportAssignmentInput {
        pub schema_version: u16,
        pub idempotency_key: StableKey,
        pub correlation_key: StableKey,
        pub assignment_id: AssignmentId,
        pub conversation_id: ConversationId,
        pub expected_assignment_revision: u64,
        pub report: AssignmentReport,
    }

    impl ReportAssignmentInput {
        pub fn validate(&self) -> Result<(), PeerToolContractError> {
            validate_version(self.schema_version)
        }
    }

    #[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(rename_all = "snake_case")]
    pub enum PeerToolReceiptDisposition {
        Applied,
        Replay,
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct PeerToolReceiptHeader {
        pub schema_version: u16,
        pub idempotency_key: StableKey,
        pub correlation_key: StableKey,
        pub actor_profile_id: ProfileId,
        pub disposition: PeerToolReceiptDisposition,
        pub accepted_at: UtcTimestamp,
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct MessageAgentReceipt {
        pub receipt: PeerToolReceiptHeader,
        pub conversation_id: ConversationId,
        pub event_id: EventId,
        pub delivery_id: DeliveryId,
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct AssignWorkReceipt {
        pub receipt: PeerToolReceiptHeader,
        pub conversation_id: ConversationId,
        pub assignment_id: AssignmentId,
        pub event_id: EventId,
        pub assignment_revision: u64,
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct HandoffWorkReceipt {
        pub receipt: PeerToolReceiptHeader,
        pub conversation_id: ConversationId,
        pub assignment_id: AssignmentId,
        pub event_id: EventId,
        pub assignment_revision: u64,
        pub owner_profile_id: ProfileId,
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct ReportAssignmentReceipt {
        pub receipt: PeerToolReceiptHeader,
        pub conversation_id: ConversationId,
        pub assignment_id: AssignmentId,
        pub event_id: EventId,
        pub assignment_revision: u64,
    }

    #[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(rename_all = "snake_case")]
    pub enum PeerToolErrorCode {
        Unauthorized,
        InvalidInput,
        NotFound,
        Conflict,
        StaleRevision,
        DependencyNotReady,
        RecipientUnavailable,
        CapacityExceeded,
        DurableWriteFailed,
    }

    #[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
    #[serde(deny_unknown_fields)]
    pub struct PeerToolErrorReceipt {
        pub schema_version: u16,
        pub idempotency_key: StableKey,
        pub correlation_key: StableKey,
        pub code: PeerToolErrorCode,
        pub retryable: bool,
        pub safe_detail: Option<AssignmentStatusDetail>,
    }

    #[derive(Clone, Debug, Eq, PartialEq)]
    pub enum PeerToolContractError {
        UnsupportedSchemaVersion(u16),
        Empty(&'static str),
        SurroundingWhitespace(&'static str),
        DisallowedControlCharacter(&'static str),
        TooLong {
            field: &'static str,
            max_chars: usize,
        },
        TooManyDependencies,
        NonCanonicalDependencies,
    }

    impl fmt::Display for PeerToolContractError {
        fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
            match self {
                Self::UnsupportedSchemaVersion(version) => {
                    write!(formatter, "unsupported peer tool schema version {version}")
                }
                Self::Empty(field) => write!(formatter, "{field} must not be empty"),
                Self::SurroundingWhitespace(field) => {
                    write!(formatter, "{field} must not contain surrounding whitespace")
                }
                Self::DisallowedControlCharacter(field) => {
                    write!(formatter, "{field} contains a disallowed control character")
                }
                Self::TooLong { field, max_chars } => {
                    write!(formatter, "{field} exceeds the {max_chars} character limit")
                }
                Self::TooManyDependencies => write!(formatter, "too many assignment dependencies"),
                Self::NonCanonicalDependencies => {
                    write!(
                        formatter,
                        "assignment dependencies must be sorted and unique"
                    )
                }
            }
        }
    }

    impl std::error::Error for PeerToolContractError {}

    fn validate_version(version: u16) -> Result<(), PeerToolContractError> {
        if version == PEER_TOOL_SCHEMA_VERSION {
            Ok(())
        } else {
            Err(PeerToolContractError::UnsupportedSchemaVersion(version))
        }
    }

    fn canonical_bounded(
        value: String,
        max_chars: usize,
        field: &'static str,
    ) -> Result<String, PeerToolContractError> {
        if value.is_empty() {
            return Err(PeerToolContractError::Empty(field));
        }
        if value.trim() != value {
            return Err(PeerToolContractError::SurroundingWhitespace(field));
        }
        if value
            .chars()
            .any(|character| character.is_control() && character != '\n' && character != '\t')
        {
            return Err(PeerToolContractError::DisallowedControlCharacter(field));
        }
        if value.chars().count() > max_chars {
            return Err(PeerToolContractError::TooLong { field, max_chars });
        }
        Ok(value)
    }
}

pub use peer_tools::*;

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex, MutexGuard, mpsc};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{
    EntityId, ToolCallId, ToolEffectState, ToolErrorCategory, ToolFailure, ToolFailureStatus,
    ToolRecoveryAction, ToolRecoveryActionKind,
};
use keith_provider_core::{CancellationToken, ToolBehavior as ProviderToolBehavior};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct ToolBehavior {
    pub reads_state: bool,
    pub writes_state: bool,
    pub uses_network: bool,
    pub starts_processes: bool,
    pub parallel_safe: bool,
}

impl ToolBehavior {
    pub const READ_ONLY: Self = Self {
        reads_state: true,
        writes_state: false,
        uses_network: false,
        starts_processes: false,
        parallel_safe: true,
    };

    pub const fn provider_behavior(self) -> ProviderToolBehavior {
        if self.writes_state || self.uses_network || self.starts_processes {
            ProviderToolBehavior::StateChanging
        } else {
            ProviderToolBehavior::ReadOnly
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Repeatability {
    Safe,
    CheckBeforeRetry,
    NeverAutomatic,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConfirmationMode {
    Never,
    OnRisk,
    Always,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolDefinition {
    pub name: String,
    pub version: String,
    pub description: String,
    pub input_schema: serde_json::Value,
    pub output_schema: serde_json::Value,
    pub behavior: ToolBehavior,
    pub repeatability: Repeatability,
    pub confirmation: ConfirmationMode,
    pub timeout_ms: u64,
    pub output_limit_bytes: usize,
}

impl ToolDefinition {
    /// # Errors
    ///
    /// Returns a definition error when stable identity, schemas, timeout, or limits are invalid.
    pub fn validate(&self) -> Result<(), ToolManagerError> {
        if !valid_name(&self.name)
            || self.version.trim().is_empty()
            || self.description.trim().is_empty()
            || !self.input_schema.is_object()
            || !self.output_schema.is_object()
            || self.timeout_ms == 0
            || self.output_limit_bytes == 0
        {
            return Err(ToolManagerError::InvalidDefinition(self.name.clone()));
        }
        Ok(())
    }

    pub fn model_definition(&self) -> keith_provider_core::ToolDefinition {
        keith_provider_core::ToolDefinition {
            name: self.name.clone(),
            description: self.description.clone(),
            input_schema: self.input_schema.clone(),
            behavior: self.behavior.provider_behavior(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum Readiness {
    Ready,
    Unready { reason: String },
}

#[derive(Clone, Debug, PartialEq)]
pub struct ToolInvocation {
    pub call_id: ToolCallId,
    pub name: String,
    pub arguments: serde_json::Value,
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
#[error("{message}")]
pub struct ToolExecutionError {
    pub message: String,
    pub retryable: bool,
    pub failure: Box<ToolFailure>,
}

impl ToolExecutionError {
    pub fn new(message: impl Into<String>) -> Self {
        let message = message.into();
        Self {
            failure: Box::new(ToolFailure::execution(message.clone(), false)),
            message,
            retryable: false,
        }
    }

    pub fn retryable(message: impl Into<String>) -> Self {
        let message = message.into();
        Self {
            failure: Box::new(ToolFailure::execution(message.clone(), true)),
            message,
            retryable: true,
        }
    }

    pub fn typed(failure: ToolFailure) -> Self {
        Self {
            message: failure.error.detail.clone(),
            retryable: failure.retry.automatic,
            failure: Box::new(failure),
        }
    }
}

pub trait ProgressSink {
    fn report(&mut self, message: &str);
}

pub trait ManagedTool: Send + Sync {
    fn definition(&self) -> &ToolDefinition;
    fn readiness(&self) -> Readiness;

    /// # Errors
    ///
    /// Returns a typed execution error for a tool failure.
    fn execute(
        &self,
        invocation: &ToolInvocation,
        progress: &mut dyn ProgressSink,
        cancellation: &CancellationToken,
    ) -> Result<Vec<u8>, ToolExecutionError>;

    fn retry_is_safe(&self, _invocation: &ToolInvocation) -> bool {
        false
    }
}

pub trait ToolExecutor: Send + Sync {
    /// # Errors
    ///
    /// Returns a typed tool error when execution cannot produce output.
    fn execute(
        &self,
        invocation: &ToolInvocation,
        cancellation: &CancellationToken,
    ) -> Result<Vec<u8>, ToolExecutionError>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExecutionDecision {
    Allow,
    Confirm,
    Deny,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ExecutionRules {
    pub default: ExecutionDecision,
    pub per_tool: BTreeMap<String, ExecutionDecision>,
}

impl ExecutionRules {
    pub fn decision(&self, name: &str) -> ExecutionDecision {
        self.per_tool.get(name).copied().unwrap_or(self.default)
    }
}

impl Default for ExecutionRules {
    fn default() -> Self {
        Self {
            default: ExecutionDecision::Confirm,
            per_tool: BTreeMap::new(),
        }
    }
}

pub trait ConfirmationGate: Send + Sync {
    fn confirm(&self, invocation: &ToolInvocation, definition: &ToolDefinition) -> bool;
}

impl<F> ConfirmationGate for F
where
    F: Fn(&ToolInvocation, &ToolDefinition) -> bool + Send + Sync,
{
    fn confirm(&self, invocation: &ToolInvocation, definition: &ToolDefinition) -> bool {
        self(invocation, definition)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TerminalState {
    Succeeded,
    Denied,
    Cancelled,
    TimedOut,
    Failed,
    OutputLimitExceeded,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event")]
pub enum ToolEventKind {
    Requested,
    AwaitingConfirmation,
    Started {
        attempt: u32,
    },
    Progress {
        message: String,
    },
    Terminal {
        state: TerminalState,
        detail: Option<String>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolEvent {
    pub invocation_id: EntityId,
    pub call_id: ToolCallId,
    pub name: String,
    pub sequence: u64,
    pub kind: ToolEventKind,
}

pub trait ToolEventSubscriber: Send + Sync {
    fn on_event(&self, event: &ToolEvent);
}

impl<F> ToolEventSubscriber for F
where
    F: Fn(&ToolEvent) + Send + Sync,
{
    fn on_event(&self, event: &ToolEvent) {
        self(event);
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ToolOutcome {
    pub state: TerminalState,
    pub output: Option<Vec<u8>>,
    pub attempts: u32,
    pub failure: Option<ToolFailure>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct UnreadyTool {
    pub name: String,
    pub reason: String,
}

#[derive(Clone, Debug, PartialEq)]
pub struct Discovery {
    pub available: Vec<ToolDefinition>,
    pub unready: Vec<UnreadyTool>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ToolManagerConfig {
    pub readiness_ttl: Duration,
    pub max_parallel: usize,
    pub max_progress_events: usize,
    pub max_progress_bytes: usize,
    pub max_safe_retries: u32,
    pub cancellation_poll: Duration,
}

impl Default for ToolManagerConfig {
    fn default() -> Self {
        Self {
            readiness_ttl: Duration::from_secs(30),
            max_parallel: 4,
            max_progress_events: 64,
            max_progress_bytes: 4_096,
            max_safe_retries: 2,
            cancellation_poll: Duration::from_millis(10),
        }
    }
}

#[derive(Debug, Error)]
pub enum ToolManagerError {
    #[error("tool definition is invalid: {0}")]
    InvalidDefinition(String),
    #[error("tool is already registered: {0}")]
    DuplicateTool(String),
    #[error("tool is not registered: {0}")]
    UnknownTool(String),
    #[error("tool registry lock was poisoned")]
    LockPoisoned,
    #[error("arguments do not match the tool schema: {0}")]
    Schema(String),
    #[error("tool is not ready: {0}")]
    Unready(String),
    #[error("tool worker channel disconnected")]
    WorkerDisconnected,
}

struct CachedReadiness {
    readiness: Readiness,
    checked_at: Instant,
}

pub struct ToolManager {
    tools: BTreeMap<String, Arc<dyn ManagedTool>>,
    readiness: Mutex<BTreeMap<String, CachedReadiness>>,
    installation_rules: ExecutionRules,
    profile_rules: ExecutionRules,
    confirmation: Arc<dyn ConfirmationGate>,
    subscribers: Vec<Arc<dyn ToolEventSubscriber>>,
    config: ToolManagerConfig,
}

impl ToolManager {
    pub fn new(
        installation_rules: ExecutionRules,
        profile_rules: ExecutionRules,
        confirmation: Arc<dyn ConfirmationGate>,
        config: ToolManagerConfig,
    ) -> Self {
        Self {
            tools: BTreeMap::new(),
            readiness: Mutex::new(BTreeMap::new()),
            installation_rules,
            profile_rules,
            confirmation,
            subscribers: Vec::new(),
            config,
        }
    }

    /// # Errors
    ///
    /// Returns an error for invalid or duplicate tool definitions.
    pub fn register(&mut self, tool: Arc<dyn ManagedTool>) -> Result<(), ToolManagerError> {
        tool.definition().validate()?;
        let name = tool.definition().name.clone();
        if self.tools.contains_key(&name) {
            return Err(ToolManagerError::DuplicateTool(name));
        }
        self.tools.insert(name, tool);
        Ok(())
    }

    pub fn subscribe(&mut self, subscriber: Arc<dyn ToolEventSubscriber>) {
        self.subscribers.push(subscriber);
    }

    /// # Errors
    ///
    /// Returns an error when the readiness cache lock is poisoned.
    pub fn discover(&self) -> Result<Discovery, ToolManagerError> {
        let mut available = Vec::new();
        let mut unready = Vec::new();
        for tool in self.tools.values() {
            match self.readiness(tool)? {
                Readiness::Ready => available.push(tool.definition().clone()),
                Readiness::Unready { reason } => unready.push(UnreadyTool {
                    name: tool.definition().name.clone(),
                    reason,
                }),
            }
        }
        Ok(Discovery { available, unready })
    }

    /// # Errors
    ///
    /// Returns a manager error only when registry or readiness state cannot be inspected.
    #[allow(clippy::needless_pass_by_value, clippy::too_many_lines)]
    pub fn invoke(
        &self,
        invocation: ToolInvocation,
        cancellation: &CancellationToken,
    ) -> Result<ToolOutcome, ToolManagerError> {
        let tool = self
            .tools
            .get(&invocation.name)
            .cloned()
            .ok_or_else(|| ToolManagerError::UnknownTool(invocation.name.clone()))?;
        let invocation_id = EntityId::new();
        let mut emitter = InvocationEmitter::new(
            invocation_id,
            invocation.call_id.clone(),
            invocation.name.clone(),
            &self.subscribers,
            self.config.max_progress_events,
            self.config.max_progress_bytes,
        );
        emitter.emit(ToolEventKind::Requested);
        if let Err(error) = validate_schema(&tool.definition().input_schema, &invocation.arguments)
        {
            emitter.terminal(TerminalState::Denied, Some(error.clone()));
            return Ok(ToolOutcome {
                state: TerminalState::Denied,
                output: None,
                attempts: 0,
                failure: Some(terminal_failure(
                    tool.definition(),
                    ToolErrorCategory::InvalidArguments,
                    "INVALID_ARGUMENTS",
                    "arguments_failed_schema_validation",
                    error,
                )),
            });
        }
        if let Readiness::Unready { reason } = self.readiness(&tool)? {
            emitter.terminal(TerminalState::Failed, Some(reason.clone()));
            return Err(ToolManagerError::Unready(reason));
        }
        match self.effective_decision(tool.definition()) {
            ExecutionDecision::Deny => {
                emitter.terminal(
                    TerminalState::Denied,
                    Some("execution policy denied the tool".into()),
                );
                return Ok(ToolOutcome {
                    state: TerminalState::Denied,
                    output: None,
                    attempts: 0,
                    failure: Some(terminal_failure(
                        tool.definition(),
                        ToolErrorCategory::PolicyDenied,
                        "TOOL_POLICY_DENIED",
                        "execution_policy_denied_tool",
                        "execution policy denied the tool",
                    )),
                });
            }
            ExecutionDecision::Confirm => {
                emitter.emit(ToolEventKind::AwaitingConfirmation);
                if !self.confirmation.confirm(&invocation, tool.definition()) {
                    emitter.terminal(
                        TerminalState::Denied,
                        Some("confirmation was declined".into()),
                    );
                    return Ok(ToolOutcome {
                        state: TerminalState::Denied,
                        output: None,
                        attempts: 0,
                        failure: Some(terminal_failure(
                            tool.definition(),
                            ToolErrorCategory::ConfirmationDeclined,
                            "TOOL_CONFIRMATION_DECLINED",
                            "confirmation_declined",
                            "confirmation was declined",
                        )),
                    });
                }
            }
            ExecutionDecision::Allow => {}
        }

        let max_attempts = self.max_attempts(tool.as_ref(), &invocation);
        for attempt in 1..=max_attempts {
            if cancellation.is_cancelled() {
                emitter.terminal(TerminalState::Cancelled, None);
                let mut failure = terminal_failure(
                    tool.definition(),
                    ToolErrorCategory::Cancelled,
                    "TOOL_CANCELLED",
                    "cancelled_before_attempt",
                    "tool invocation was cancelled before an attempt started",
                );
                failure.status = ToolFailureStatus::NotStarted;
                failure.effect_state = ToolEffectState::NotStarted;
                failure.retry.automatic = tool.definition().repeatability == Repeatability::Safe;
                failure.retry.reason = "The tool body did not start".into();
                return Ok(ToolOutcome {
                    state: TerminalState::Cancelled,
                    output: None,
                    attempts: attempt - 1,
                    failure: Some(failure),
                });
            }
            emitter.emit(ToolEventKind::Started { attempt });
            match self.run_attempt(
                Arc::clone(&tool),
                invocation.clone(),
                cancellation,
                &mut emitter,
            ) {
                AttemptResult::Completed(Ok(output))
                    if output.len() <= tool.definition().output_limit_bytes =>
                {
                    emitter.terminal(TerminalState::Succeeded, None);
                    return Ok(ToolOutcome {
                        state: TerminalState::Succeeded,
                        output: Some(output),
                        attempts: attempt,
                        failure: None,
                    });
                }
                AttemptResult::Completed(Ok(_)) => {
                    emitter.terminal(
                        TerminalState::OutputLimitExceeded,
                        Some("tool output exceeded its declared limit".into()),
                    );
                    return Ok(ToolOutcome {
                        state: TerminalState::OutputLimitExceeded,
                        output: None,
                        attempts: attempt,
                        failure: Some(terminal_failure(
                            tool.definition(),
                            ToolErrorCategory::OutputLimit,
                            "TOOL_OUTPUT_LIMIT_EXCEEDED",
                            "output_exceeded_declared_limit",
                            "tool output exceeded its declared limit",
                        )),
                    });
                }
                AttemptResult::Completed(Err(error))
                    if error.retryable
                        && attempt < max_attempts
                        && (!tool.definition().behavior.writes_state
                            || error.failure.effect_state == ToolEffectState::NotCommitted) => {}
                AttemptResult::Completed(Err(error)) => {
                    emitter.terminal(TerminalState::Failed, Some(error.message.clone()));
                    let mut failure = *error.failure;
                    normalize_effect_state(tool.definition(), &mut failure);
                    return Ok(ToolOutcome {
                        state: TerminalState::Failed,
                        output: None,
                        attempts: attempt,
                        failure: Some(failure),
                    });
                }
                AttemptResult::Cancelled => {
                    emitter.terminal(TerminalState::Cancelled, None);
                    return Ok(ToolOutcome {
                        state: TerminalState::Cancelled,
                        output: None,
                        attempts: attempt,
                        failure: Some(terminal_failure(
                            tool.definition(),
                            ToolErrorCategory::Cancelled,
                            "TOOL_CANCELLED",
                            "cancelled_during_execution",
                            "tool invocation was cancelled during execution",
                        )),
                    });
                }
                AttemptResult::TimedOut => {
                    emitter.terminal(TerminalState::TimedOut, None);
                    return Ok(ToolOutcome {
                        state: TerminalState::TimedOut,
                        output: None,
                        attempts: attempt,
                        failure: Some(terminal_failure(
                            tool.definition(),
                            ToolErrorCategory::Timeout,
                            "TOOL_TIMED_OUT",
                            "execution_deadline_exceeded",
                            "tool execution exceeded its declared timeout",
                        )),
                    });
                }
                AttemptResult::Disconnected => return Err(ToolManagerError::WorkerDisconnected),
            }
        }
        unreachable!("the attempt range is always non-empty")
    }

    /// # Errors
    ///
    /// Returns a manager error when any invocation cannot be admitted or its worker disconnects.
    pub fn invoke_batch(
        &self,
        invocations: &[ToolInvocation],
        cancellation: &CancellationToken,
    ) -> Result<Vec<ToolOutcome>, ToolManagerError> {
        let mut outcomes = Vec::with_capacity(invocations.len());
        let mut index = 0;
        while index < invocations.len() {
            let parallel = self
                .tools
                .get(&invocations[index].name)
                .is_some_and(|tool| tool.definition().behavior.parallel_safe);
            if parallel {
                let end = invocations[index..]
                    .iter()
                    .position(|invocation| {
                        !self
                            .tools
                            .get(&invocation.name)
                            .is_some_and(|tool| tool.definition().behavior.parallel_safe)
                    })
                    .map_or(invocations.len(), |offset| index + offset);
                for chunk in invocations[index..end].chunks(self.config.max_parallel.max(1)) {
                    let batch = thread::scope(|scope| {
                        chunk
                            .iter()
                            .map(|invocation| {
                                scope.spawn(move || self.invoke(invocation.clone(), cancellation))
                            })
                            .collect::<Vec<_>>()
                            .into_iter()
                            .map(|handle| {
                                handle
                                    .join()
                                    .map_err(|_| ToolManagerError::WorkerDisconnected)?
                            })
                            .collect::<Result<Vec<_>, _>>()
                    })?;
                    outcomes.extend(batch);
                }
                index = end;
            } else {
                outcomes.push(self.invoke(invocations[index].clone(), cancellation)?);
                index += 1;
            }
        }
        Ok(outcomes)
    }

    fn effective_decision(&self, definition: &ToolDefinition) -> ExecutionDecision {
        let installation = self.installation_rules.decision(&definition.name);
        let profile = self.profile_rules.decision(&definition.name);
        let declared = match definition.confirmation {
            ConfirmationMode::Never => ExecutionDecision::Allow,
            ConfirmationMode::Always => ExecutionDecision::Confirm,
            ConfirmationMode::OnRisk => {
                if definition.behavior.writes_state
                    || definition.behavior.uses_network
                    || definition.behavior.starts_processes
                {
                    ExecutionDecision::Confirm
                } else {
                    ExecutionDecision::Allow
                }
            }
        };
        strictest(installation, strictest(profile, declared))
    }

    fn max_attempts(&self, tool: &dyn ManagedTool, invocation: &ToolInvocation) -> u32 {
        let retries = match tool.definition().repeatability {
            Repeatability::Safe => self.config.max_safe_retries,
            Repeatability::CheckBeforeRetry if tool.retry_is_safe(invocation) => {
                self.config.max_safe_retries
            }
            Repeatability::CheckBeforeRetry | Repeatability::NeverAutomatic => 0,
        };
        retries.saturating_add(1)
    }

    fn run_attempt(
        &self,
        tool: Arc<dyn ManagedTool>,
        invocation: ToolInvocation,
        cancellation: &CancellationToken,
        emitter: &mut InvocationEmitter<'_>,
    ) -> AttemptResult {
        let timeout = Duration::from_millis(tool.definition().timeout_ms);
        let child_cancellation = CancellationToken::default();
        let worker_cancellation = child_cancellation.clone();
        let (sender, receiver) = mpsc::sync_channel(64);
        let worker = thread::spawn(move || {
            let mut progress = ChannelProgress {
                sender: sender.clone(),
            };
            let result = tool.execute(&invocation, &mut progress, &worker_cancellation);
            let _ = sender.send(WorkerMessage::Completed(result));
        });
        let started = Instant::now();
        loop {
            if cancellation.is_cancelled() {
                child_cancellation.cancel();
                drop(worker);
                return AttemptResult::Cancelled;
            }
            if started.elapsed() >= timeout {
                child_cancellation.cancel();
                drop(worker);
                return AttemptResult::TimedOut;
            }
            let remaining = timeout.saturating_sub(started.elapsed());
            let wait = self.config.cancellation_poll.min(remaining);
            match receiver.recv_timeout(wait) {
                Ok(WorkerMessage::Progress(message)) => emitter.progress(&message),
                Ok(WorkerMessage::Completed(result)) => {
                    let _ = worker.join();
                    return AttemptResult::Completed(result);
                }
                Err(mpsc::RecvTimeoutError::Timeout) => {}
                Err(mpsc::RecvTimeoutError::Disconnected) => {
                    let _ = worker.join();
                    return AttemptResult::Disconnected;
                }
            }
        }
    }

    fn readiness(&self, tool: &Arc<dyn ManagedTool>) -> Result<Readiness, ToolManagerError> {
        let name = &tool.definition().name;
        let mut cache = self.lock_readiness()?;
        if let Some(cached) = cache.get(name)
            && cached.checked_at.elapsed() < self.config.readiness_ttl
        {
            return Ok(cached.readiness.clone());
        }
        let readiness = tool.readiness();
        cache.insert(
            name.clone(),
            CachedReadiness {
                readiness: readiness.clone(),
                checked_at: Instant::now(),
            },
        );
        Ok(readiness)
    }

    fn lock_readiness(
        &self,
    ) -> Result<MutexGuard<'_, BTreeMap<String, CachedReadiness>>, ToolManagerError> {
        self.readiness
            .lock()
            .map_err(|_| ToolManagerError::LockPoisoned)
    }
}

impl ToolExecutor for ToolManager {
    fn execute(
        &self,
        invocation: &ToolInvocation,
        cancellation: &CancellationToken,
    ) -> Result<Vec<u8>, ToolExecutionError> {
        match self.invoke(invocation.clone(), cancellation) {
            Ok(ToolOutcome {
                state: TerminalState::Succeeded,
                output: Some(output),
                ..
            }) => Ok(output),
            Ok(outcome) => Err(ToolExecutionError::typed(outcome.failure.unwrap_or_else(
                || {
                    ToolFailure::not_committed(
                        ToolErrorCategory::Internal,
                        "TOOL_TERMINAL_FAILURE_MISSING",
                        "terminal_failure_missing",
                        format!("tool ended in {:?} without failure detail", outcome.state),
                    )
                },
            ))),
            Err(error) => Err(ToolExecutionError::typed(ToolFailure::not_committed(
                match &error {
                    ToolManagerError::UnknownTool(_) | ToolManagerError::Schema(_) => {
                        ToolErrorCategory::InvalidArguments
                    }
                    ToolManagerError::Unready(_) => ToolErrorCategory::NotReady,
                    ToolManagerError::WorkerDisconnected => ToolErrorCategory::WorkerDisconnected,
                    ToolManagerError::InvalidDefinition(_)
                    | ToolManagerError::DuplicateTool(_)
                    | ToolManagerError::LockPoisoned => ToolErrorCategory::Internal,
                },
                "TOOL_MANAGER_ERROR",
                "tool_manager_rejected_invocation",
                error.to_string(),
            ))),
        }
    }
}

fn terminal_failure(
    definition: &ToolDefinition,
    category: ToolErrorCategory,
    code: impl Into<String>,
    reason: impl Into<String>,
    detail: impl Into<String>,
) -> ToolFailure {
    let mut failure = ToolFailure::not_committed(category, code, reason, detail);
    failure.status = match category {
        ToolErrorCategory::InvalidArguments
        | ToolErrorCategory::PolicyDenied
        | ToolErrorCategory::ConfirmationDeclined
        | ToolErrorCategory::NotReady => ToolFailureStatus::Denied,
        ToolErrorCategory::Cancelled => ToolFailureStatus::Cancelled,
        ToolErrorCategory::Timeout => ToolFailureStatus::TimedOut,
        ToolErrorCategory::OutputLimit => ToolFailureStatus::OutputLimitExceeded,
        ToolErrorCategory::Provider4xx
        | ToolErrorCategory::Provider5xx
        | ToolErrorCategory::Unavailable
        | ToolErrorCategory::Execution
        | ToolErrorCategory::WorkerDisconnected
        | ToolErrorCategory::Internal => ToolFailureStatus::Error,
    };
    if definition.behavior.writes_state
        && matches!(
            category,
            ToolErrorCategory::Cancelled
                | ToolErrorCategory::Timeout
                | ToolErrorCategory::OutputLimit
                | ToolErrorCategory::Execution
                | ToolErrorCategory::WorkerDisconnected
                | ToolErrorCategory::Internal
        )
    {
        failure.effect_state = ToolEffectState::Unknown;
        failure.retry.automatic = false;
        failure.retry.reason =
            "The operation may have changed state; inspect state before any retry".into();
        failure.recovery.insert(
            0,
            ToolRecoveryAction {
                action: ToolRecoveryActionKind::InspectState,
                description: "Inspect the target state before attempting this operation again"
                    .into(),
            },
        );
    }
    failure
}

fn normalize_effect_state(definition: &ToolDefinition, failure: &mut ToolFailure) {
    if !definition.behavior.writes_state {
        failure.effect_state = ToolEffectState::NotCommitted;
    }
    if failure.effect_state == ToolEffectState::Unknown {
        failure.retry.automatic = false;
        failure.retry.reason =
            "The operation may have changed state; inspect state before any retry".into();
        if !failure
            .recovery
            .iter()
            .any(|action| action.action == ToolRecoveryActionKind::InspectState)
        {
            failure.recovery.insert(
                0,
                ToolRecoveryAction {
                    action: ToolRecoveryActionKind::InspectState,
                    description: "Inspect the target state before attempting this operation again"
                        .into(),
                },
            );
        }
    }
}

enum WorkerMessage {
    Progress(String),
    Completed(Result<Vec<u8>, ToolExecutionError>),
}

struct ChannelProgress {
    sender: mpsc::SyncSender<WorkerMessage>,
}

impl ProgressSink for ChannelProgress {
    fn report(&mut self, message: &str) {
        let _ = self
            .sender
            .try_send(WorkerMessage::Progress(message.to_owned()));
    }
}

enum AttemptResult {
    Completed(Result<Vec<u8>, ToolExecutionError>),
    Cancelled,
    TimedOut,
    Disconnected,
}

struct InvocationEmitter<'a> {
    invocation_id: EntityId,
    call_id: ToolCallId,
    name: String,
    subscribers: &'a [Arc<dyn ToolEventSubscriber>],
    sequence: u64,
    progress_events: usize,
    progress_bytes: usize,
    max_progress_events: usize,
    max_progress_bytes: usize,
    terminal: bool,
}

impl<'a> InvocationEmitter<'a> {
    fn new(
        invocation_id: EntityId,
        call_id: ToolCallId,
        name: String,
        subscribers: &'a [Arc<dyn ToolEventSubscriber>],
        max_progress_events: usize,
        max_progress_bytes: usize,
    ) -> Self {
        Self {
            invocation_id,
            call_id,
            name,
            subscribers,
            sequence: 0,
            progress_events: 0,
            progress_bytes: 0,
            max_progress_events,
            max_progress_bytes,
            terminal: false,
        }
    }

    fn progress(&mut self, message: &str) {
        if self.progress_events >= self.max_progress_events
            || self.progress_bytes.saturating_add(message.len()) > self.max_progress_bytes
        {
            return;
        }
        self.progress_events += 1;
        self.progress_bytes += message.len();
        self.emit(ToolEventKind::Progress {
            message: message.to_owned(),
        });
    }

    fn terminal(&mut self, state: TerminalState, detail: Option<String>) {
        if !self.terminal {
            self.terminal = true;
            self.emit(ToolEventKind::Terminal { state, detail });
        }
    }

    fn emit(&mut self, kind: ToolEventKind) {
        self.sequence = self.sequence.saturating_add(1);
        let event = ToolEvent {
            invocation_id: self.invocation_id.clone(),
            call_id: self.call_id.clone(),
            name: self.name.clone(),
            sequence: self.sequence,
            kind,
        };
        for subscriber in self.subscribers {
            subscriber.on_event(&event);
        }
    }
}

fn strictest(left: ExecutionDecision, right: ExecutionDecision) -> ExecutionDecision {
    match (left, right) {
        (ExecutionDecision::Deny, _) | (_, ExecutionDecision::Deny) => ExecutionDecision::Deny,
        (ExecutionDecision::Confirm, _) | (_, ExecutionDecision::Confirm) => {
            ExecutionDecision::Confirm
        }
        (ExecutionDecision::Allow, ExecutionDecision::Allow) => ExecutionDecision::Allow,
    }
}

fn valid_name(name: &str) -> bool {
    !name.is_empty()
        && name.len() <= 128
        && name
            .chars()
            .all(|character| character.is_ascii_alphanumeric() || matches!(character, '_' | '-'))
}

fn validate_schema(schema: &serde_json::Value, value: &serde_json::Value) -> Result<(), String> {
    if let Some(expected) = schema.get("type").and_then(serde_json::Value::as_str) {
        let valid = match expected {
            "object" => value.is_object(),
            "array" => value.is_array(),
            "string" => value.is_string(),
            "integer" => value.as_i64().is_some() || value.as_u64().is_some(),
            "number" => value.is_number(),
            "boolean" => value.is_boolean(),
            "null" => value.is_null(),
            _ => return Err(format!("unsupported schema type {expected}")),
        };
        if !valid {
            return Err(format!("expected {expected}"));
        }
    }
    if let (Some(required), Some(object)) = (
        schema.get("required").and_then(serde_json::Value::as_array),
        value.as_object(),
    ) {
        for field in required {
            let field = field
                .as_str()
                .ok_or_else(|| "required fields must be strings".to_owned())?;
            if !object.contains_key(field) {
                return Err(format!("missing required field {field}"));
            }
        }
    }
    if let (Some(properties), Some(object)) = (
        schema
            .get("properties")
            .and_then(serde_json::Value::as_object),
        value.as_object(),
    ) {
        for (name, property_schema) in properties {
            if let Some(property) = object.get(name) {
                validate_schema(property_schema, property)
                    .map_err(|error| format!("{name}: {error}"))?;
            }
        }
        if schema
            .get("additionalProperties")
            .and_then(serde_json::Value::as_bool)
            == Some(false)
            && object.keys().any(|name| !properties.contains_key(name))
        {
            return Err("additional properties are not allowed".into());
        }
    }
    if let Some(allowed) = schema.get("enum").and_then(serde_json::Value::as_array)
        && !allowed.contains(value)
    {
        return Err("value is outside the declared enum".into());
    }
    Ok(())
}

pub struct ReadOnlyEchoTool {
    definition: ToolDefinition,
}

impl Default for ReadOnlyEchoTool {
    fn default() -> Self {
        Self {
            definition: ToolDefinition {
                name: "echo".into(),
                version: "1.0.0".into(),
                description: "Returns the requested text".into(),
                input_schema: serde_json::json!({
                    "type": "object",
                    "required": ["text"],
                    "properties": {"text": {"type": "string"}},
                    "additionalProperties": false
                }),
                output_schema: serde_json::json!({"type": "string"}),
                behavior: ToolBehavior::READ_ONLY,
                repeatability: Repeatability::Safe,
                confirmation: ConfirmationMode::Never,
                timeout_ms: 1_000,
                output_limit_bytes: 64 * 1_024,
            },
        }
    }
}

impl ManagedTool for ReadOnlyEchoTool {
    fn definition(&self) -> &ToolDefinition {
        &self.definition
    }

    fn readiness(&self) -> Readiness {
        Readiness::Ready
    }

    fn execute(
        &self,
        invocation: &ToolInvocation,
        progress: &mut dyn ProgressSink,
        cancellation: &CancellationToken,
    ) -> Result<Vec<u8>, ToolExecutionError> {
        if cancellation.is_cancelled() {
            return Err(ToolExecutionError::new("cancelled"));
        }
        progress.report("echoing input");
        invocation
            .arguments
            .get("text")
            .and_then(serde_json::Value::as_str)
            .map(str::as_bytes)
            .map(<[u8]>::to_vec)
            .ok_or_else(|| ToolExecutionError::new("text is missing"))
    }
}

#[cfg(test)]
mod tests {
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    use serde_json::json;

    use super::*;

    #[derive(Clone, Copy)]
    enum Mode {
        Success,
        Fail,
        RetryOnce,
        Slow,
        Oversized,
        Parallel,
    }

    struct ConformanceTool {
        definition: ToolDefinition,
        ready: Arc<AtomicBool>,
        probes: Arc<AtomicUsize>,
        attempts: AtomicUsize,
        active: Arc<AtomicUsize>,
        peak: Arc<AtomicUsize>,
        mode: Mode,
    }

    impl ConformanceTool {
        fn new(name: &str, mode: Mode) -> Self {
            let mut definition = ReadOnlyEchoTool::default().definition().clone();
            definition.name = name.into();
            definition.repeatability = Repeatability::Safe;
            definition.confirmation = ConfirmationMode::Never;
            Self {
                definition,
                ready: Arc::new(AtomicBool::new(true)),
                probes: Arc::new(AtomicUsize::new(0)),
                attempts: AtomicUsize::new(0),
                active: Arc::new(AtomicUsize::new(0)),
                peak: Arc::new(AtomicUsize::new(0)),
                mode,
            }
        }
    }

    impl ManagedTool for ConformanceTool {
        fn definition(&self) -> &ToolDefinition {
            &self.definition
        }

        fn readiness(&self) -> Readiness {
            self.probes.fetch_add(1, Ordering::SeqCst);
            if self.ready.load(Ordering::SeqCst) {
                Readiness::Ready
            } else {
                Readiness::Unready {
                    reason: "dependency unavailable".into(),
                }
            }
        }

        fn execute(
            &self,
            invocation: &ToolInvocation,
            progress: &mut dyn ProgressSink,
            cancellation: &CancellationToken,
        ) -> Result<Vec<u8>, ToolExecutionError> {
            let attempt = self.attempts.fetch_add(1, Ordering::SeqCst);
            for index in 0..5 {
                progress.report(&format!("progress-{index}"));
            }
            match self.mode {
                Mode::Success => Ok(invocation
                    .arguments
                    .get("text")
                    .and_then(serde_json::Value::as_str)
                    .unwrap_or_default()
                    .as_bytes()
                    .to_vec()),
                Mode::Fail => Err(ToolExecutionError::new("execution failed")),
                Mode::RetryOnce if attempt == 0 => {
                    Err(ToolExecutionError::retryable("transient failure"))
                }
                Mode::RetryOnce => Ok(b"recovered".to_vec()),
                Mode::Oversized => Ok(vec![b'x'; 32]),
                Mode::Slow => {
                    for _ in 0..100 {
                        if cancellation.is_cancelled() {
                            return Err(ToolExecutionError::new("cancelled"));
                        }
                        thread::sleep(Duration::from_millis(2));
                    }
                    Ok(b"slow".to_vec())
                }
                Mode::Parallel => {
                    let active = self.active.fetch_add(1, Ordering::SeqCst) + 1;
                    self.peak.fetch_max(active, Ordering::SeqCst);
                    thread::sleep(Duration::from_millis(20));
                    self.active.fetch_sub(1, Ordering::SeqCst);
                    Ok(b"parallel".to_vec())
                }
            }
        }
    }

    fn invocation(name: &str, arguments: serde_json::Value) -> ToolInvocation {
        ToolInvocation {
            call_id: ToolCallId::new(),
            name: name.into(),
            arguments,
        }
    }

    fn rules(decision: ExecutionDecision) -> ExecutionRules {
        ExecutionRules {
            default: decision,
            per_tool: BTreeMap::new(),
        }
    }

    fn manager(config: ToolManagerConfig) -> ToolManager {
        ToolManager::new(
            rules(ExecutionDecision::Allow),
            rules(ExecutionDecision::Allow),
            Arc::new(|_: &ToolInvocation, _: &ToolDefinition| true),
            config,
        )
    }

    fn capture(manager: &mut ToolManager) -> Arc<Mutex<Vec<ToolEvent>>> {
        let events = Arc::new(Mutex::new(Vec::new()));
        let target = Arc::clone(&events);
        manager.subscribe(Arc::new(move |event: &ToolEvent| {
            target.lock().unwrap().push(event.clone());
        }));
        events
    }

    #[test]
    fn discovery_caches_readiness_and_recovers_after_ttl() {
        let config = ToolManagerConfig {
            readiness_ttl: Duration::from_millis(15),
            ..ToolManagerConfig::default()
        };
        let mut manager = manager(config);
        let tool = Arc::new(ConformanceTool::new("recovering", Mode::Success));
        tool.ready.store(false, Ordering::SeqCst);
        let ready = Arc::clone(&tool.ready);
        let probes = Arc::clone(&tool.probes);
        manager.register(tool).unwrap();
        let first = manager.discover().unwrap();
        assert!(first.available.is_empty());
        assert_eq!(first.unready[0].reason, "dependency unavailable");
        ready.store(true, Ordering::SeqCst);
        assert!(manager.discover().unwrap().available.is_empty());
        assert_eq!(probes.load(Ordering::SeqCst), 1);
        thread::sleep(Duration::from_millis(20));
        assert_eq!(manager.discover().unwrap().available[0].name, "recovering");
        assert_eq!(probes.load(Ordering::SeqCst), 2);
    }

    #[test]
    fn schema_denial_precedes_confirmation_and_installation_deny_cannot_be_relaxed() {
        let confirmations = Arc::new(AtomicUsize::new(0));
        let gate_count = Arc::clone(&confirmations);
        let mut installation = rules(ExecutionDecision::Allow);
        installation
            .per_tool
            .insert("guarded".into(), ExecutionDecision::Deny);
        let mut profile = rules(ExecutionDecision::Allow);
        profile
            .per_tool
            .insert("guarded".into(), ExecutionDecision::Allow);
        let mut manager = ToolManager::new(
            installation,
            profile,
            Arc::new(move |_: &ToolInvocation, _: &ToolDefinition| {
                gate_count.fetch_add(1, Ordering::SeqCst);
                true
            }),
            ToolManagerConfig::default(),
        );
        manager
            .register(Arc::new(ConformanceTool::new("guarded", Mode::Success)))
            .unwrap();
        let malformed = manager
            .invoke(
                invocation("guarded", json!({"wrong": true})),
                &CancellationToken::default(),
            )
            .unwrap();
        assert_eq!(malformed.state, TerminalState::Denied);
        assert_eq!(confirmations.load(Ordering::SeqCst), 0);
        let policy_denied = manager
            .invoke(
                invocation("guarded", json!({"text": "hello"})),
                &CancellationToken::default(),
            )
            .unwrap();
        assert_eq!(policy_denied.state, TerminalState::Denied);
        assert_eq!(confirmations.load(Ordering::SeqCst), 0);
    }

    #[test]
    fn confirmation_progress_retry_and_success_follow_one_lifecycle() {
        let mut profile = rules(ExecutionDecision::Allow);
        profile
            .per_tool
            .insert("retrying".into(), ExecutionDecision::Confirm);
        let mut manager = ToolManager::new(
            rules(ExecutionDecision::Allow),
            profile,
            Arc::new(|_: &ToolInvocation, _: &ToolDefinition| true),
            ToolManagerConfig {
                max_progress_events: 2,
                max_progress_bytes: 1_000,
                max_safe_retries: 2,
                ..ToolManagerConfig::default()
            },
        );
        manager
            .register(Arc::new(ConformanceTool::new("retrying", Mode::RetryOnce)))
            .unwrap();
        let events = capture(&mut manager);
        let outcome = manager
            .invoke(
                invocation("retrying", json!({"text": "hello"})),
                &CancellationToken::default(),
            )
            .unwrap();
        assert_eq!(outcome.state, TerminalState::Succeeded);
        assert_eq!(outcome.attempts, 2);
        let events = events.lock().unwrap();
        assert!(matches!(events[0].kind, ToolEventKind::Requested));
        assert!(matches!(
            events[1].kind,
            ToolEventKind::AwaitingConfirmation
        ));
        assert_eq!(
            events
                .iter()
                .filter(|event| matches!(event.kind, ToolEventKind::Progress { .. }))
                .count(),
            2
        );
        assert_eq!(
            events
                .iter()
                .filter(|event| matches!(event.kind, ToolEventKind::Terminal { .. }))
                .count(),
            1
        );
    }

    #[test]
    fn unknown_effect_state_changing_failure_is_not_automatically_repeated() {
        let mut manager = manager(ToolManagerConfig {
            max_safe_retries: 2,
            ..ToolManagerConfig::default()
        });
        let mut candidate = ConformanceTool::new("unknown-write", Mode::RetryOnce);
        candidate.definition.behavior.writes_state = true;
        let tool = Arc::new(candidate);
        let attempts = Arc::clone(&tool);
        manager.register(tool).unwrap();
        let outcome = manager
            .invoke(
                invocation("unknown-write", json!({"text": "one attempt"})),
                &CancellationToken::default(),
            )
            .unwrap();
        assert_eq!(attempts.attempts.load(Ordering::SeqCst), 1);
        assert_eq!(outcome.attempts, 1);
        let failure = outcome.failure.unwrap();
        assert_eq!(failure.effect_state, ToolEffectState::Unknown);
        assert!(!failure.retry.automatic);
        assert_eq!(failure.status, keith_agent_types::ToolFailureStatus::Error);
        assert!(!failure.success);
        assert!(
            failure
                .recovery
                .iter()
                .any(|action| action.action == ToolRecoveryActionKind::InspectState)
        );
    }

    #[test]
    fn timeout_output_limit_and_failure_are_distinct_terminal_states() {
        let mut manager = manager(ToolManagerConfig {
            cancellation_poll: Duration::from_millis(2),
            ..ToolManagerConfig::default()
        });
        let mut timeout = ConformanceTool::new("timeout", Mode::Slow);
        timeout.definition.timeout_ms = 15;
        let mut oversized = ConformanceTool::new("oversized", Mode::Oversized);
        oversized.definition.output_limit_bytes = 4;
        manager.register(Arc::new(timeout)).unwrap();
        manager.register(Arc::new(oversized)).unwrap();
        let mut failed_tool = ConformanceTool::new("failed", Mode::Fail);
        failed_tool.definition.behavior.writes_state = true;
        manager.register(Arc::new(failed_tool)).unwrap();
        let timeout = manager
            .invoke(
                invocation("timeout", json!({"text": "x"})),
                &CancellationToken::default(),
            )
            .unwrap();
        assert_eq!(timeout.state, TerminalState::TimedOut);
        assert_eq!(
            timeout.failure.as_ref().unwrap().effect_state,
            ToolEffectState::NotCommitted
        );
        assert_eq!(
            manager
                .invoke(
                    invocation("oversized", json!({"text": "x"})),
                    &CancellationToken::default()
                )
                .unwrap()
                .state,
            TerminalState::OutputLimitExceeded
        );
        let failed = manager
            .invoke(
                invocation("failed", json!({"text": "x"})),
                &CancellationToken::default(),
            )
            .unwrap();
        assert_eq!(failed.state, TerminalState::Failed);
        assert_eq!(
            failed.failure.as_ref().unwrap().error.code,
            "TOOL_EXECUTION_FAILED"
        );
        assert_eq!(
            failed.failure.as_ref().unwrap().effect_state,
            ToolEffectState::Unknown
        );
        assert!(!failed.failure.as_ref().unwrap().retry.automatic);
        assert!(
            failed
                .failure
                .as_ref()
                .unwrap()
                .recovery
                .iter()
                .any(|action| { action.action == ToolRecoveryActionKind::InspectState })
        );
        let surfaced = ToolExecutor::execute(
            &manager,
            &invocation("failed", json!({"text": "x"})),
            &CancellationToken::default(),
        )
        .unwrap_err();
        assert_eq!(surfaced.message, "execution failed");
        assert_eq!(surfaced.failure.error.code, "TOOL_EXECUTION_FAILED");
    }

    #[test]
    fn cancellation_reaches_a_running_tool_and_echo_is_model_ready() {
        let mut manager = manager(ToolManagerConfig {
            cancellation_poll: Duration::from_millis(2),
            ..ToolManagerConfig::default()
        });
        let mut slow = ConformanceTool::new("cancelled", Mode::Slow);
        slow.definition.timeout_ms = 2_000;
        manager.register(Arc::new(slow)).unwrap();
        manager
            .register(Arc::new(ReadOnlyEchoTool::default()))
            .unwrap();
        assert_eq!(manager.discover().unwrap().available.len(), 2);
        let manager = Arc::new(manager);
        let cancellation = CancellationToken::default();
        let worker_cancellation = cancellation.clone();
        let worker_manager = Arc::clone(&manager);
        let handle = thread::spawn(move || {
            worker_manager
                .invoke(
                    invocation("cancelled", json!({"text": "x"})),
                    &worker_cancellation,
                )
                .unwrap()
        });
        thread::sleep(Duration::from_millis(15));
        cancellation.cancel();
        assert_eq!(handle.join().unwrap().state, TerminalState::Cancelled);
        let echo = ToolExecutor::execute(
            manager.as_ref(),
            &invocation("echo", json!({"text": "real"})),
            &CancellationToken::default(),
        )
        .unwrap();
        assert_eq!(echo, b"real");
    }

    #[test]
    fn cancellation_before_dispatch_is_not_started_and_safe_to_distinguish_from_unknown() {
        let mut manager = manager(ToolManagerConfig::default());
        manager
            .register(Arc::new(ConformanceTool::new(
                "never-started",
                Mode::Success,
            )))
            .unwrap();
        let cancellation = CancellationToken::default();
        cancellation.cancel();
        let outcome = manager
            .invoke(
                invocation("never-started", json!({"text": "must not run"})),
                &cancellation,
            )
            .unwrap();
        let failure = outcome.failure.unwrap();
        assert_eq!(outcome.state, TerminalState::Cancelled);
        assert_eq!(failure.status, ToolFailureStatus::NotStarted);
        assert_eq!(failure.effect_state, ToolEffectState::NotStarted);
        assert_eq!(failure.error.code, "TOOL_CANCELLED");
    }

    #[test]
    fn batch_execution_bounds_parallel_safe_tools() {
        let mut manager = manager(ToolManagerConfig {
            max_parallel: 2,
            ..ToolManagerConfig::default()
        });
        let tool = Arc::new(ConformanceTool::new("parallel", Mode::Parallel));
        let peak = Arc::clone(&tool.peak);
        manager.register(tool).unwrap();
        let outcomes = manager
            .invoke_batch(
                &(0..3)
                    .map(|_| invocation("parallel", json!({"text": "x"})))
                    .collect::<Vec<_>>(),
                &CancellationToken::default(),
            )
            .unwrap();
        assert!(
            outcomes
                .iter()
                .all(|outcome| outcome.state == TerminalState::Succeeded)
        );
        assert_eq!(peak.load(Ordering::SeqCst), 2);
    }
}
