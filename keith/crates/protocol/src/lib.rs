#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use keith_agent_types::{
    ActionId, ArtifactId, CURRENT_PROTOCOL_VERSION, ChildId, ClientId, CommandId, CommitmentId,
    CommonError, DeliveryId, EntityId, EntryId, Generation, GoalId, JobId, KernelId, MessageId,
    ProfileId, ProtocolVersion, Revision, RootTreeId, Sequence, SessionId, ToolCallId,
    UtcTimestamp, WorkspaceId,
};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(
    Clone, Copy, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum Feature {
    SessionLifecycle,
    Branching,
    Steering,
    Goals,
    Children,
    Schedules,
    MemoryQueries,
    Confirmations,
    Export,
    BackgroundControls,
    Replay,
    Snapshots,
    FramedJson,
    LocalBinary,
    Stdio,
    WebSocket,
    DeliveryDispatch,
    AttachmentStaging,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ClientHello {
    pub protocol: ProtocolVersion,
    pub client_id: ClientId,
    pub client_name: String,
    pub client_version: String,
    pub supported_features: BTreeSet<Feature>,
    pub resume: Option<ResumeCursor>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ResumeCursor {
    pub root_tree_id: RootTreeId,
    pub generation: Generation,
    pub last_sequence: Sequence,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ServerHello {
    pub protocol: ProtocolVersion,
    pub server_instance_id: EntityId,
    pub supported_features: BTreeSet<Feature>,
    pub current_generation: Option<Generation>,
    pub resume_mode: ResumeMode,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ResumeMode {
    Fresh,
    Delta,
    SnapshotThenDelta,
    Incompatible,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CommandEnvelope {
    pub protocol: ProtocolVersion,
    pub command_id: CommandId,
    pub client_id: ClientId,
    pub sent_at: UtcTimestamp,
    pub session_id: Option<SessionId>,
    pub command: ClientCommand,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "command", content = "parameters")]
pub enum ClientCommand {
    ListProfiles,
    ListSessions(SessionFilter),
    CreateSession(CreateSession),
    AttachSession(AttachSession),
    DetachSession { session_id: SessionId },
    AcknowledgeEvents(EventAcknowledgement),
    ResumeSession { session_id: SessionId },
    BranchSession(BranchRequest),
    SelectBranch(SelectBranch),
    SubmitPrompt(SubmitPrompt),
    Steer(SteerAction),
    Cancel(CancelTarget),
    SelectModel(ModelSelection),
    CreateGoal(CreateGoal),
    UpdateGoal(UpdateGoal),
    ListGoals { session_id: SessionId },
    ListChildren { session_id: SessionId },
    CreateChild(CreateChild),
    SendChildMessage(ChildMessageRequest),
    ArchiveChild { child_id: ChildId },
    CreateSchedule(CreateSchedule),
    UpdateSchedule(UpdateSchedule),
    DeleteSchedule { job_id: JobId },
    QueryMemory(MemoryQuery),
    ResolveConfirmation(ConfirmationResolution),
    Export(ExportRequest),
    SetBackgroundControl(BackgroundControl),
    StageAttachment(StagedAttachment),
    ClaimDelivery { channel: String },
    AcknowledgeDelivery(DeliveryAcknowledgement),
    FailDelivery(DeliveryFailure),
}

#[derive(Clone, Debug, Default, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct SessionFilter {
    pub profile_id: Option<ProfileId>,
    pub include_archived: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CreateSession {
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub title: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct AttachSession {
    pub session_id: SessionId,
    pub resume: Option<ResumeCursor>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct EventAcknowledgement {
    pub root_tree_id: RootTreeId,
    pub generation: Generation,
    pub through_sequence: Sequence,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct BranchRequest {
    pub session_id: SessionId,
    pub parent_entry_id: EntityId,
    pub label: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct SelectBranch {
    pub session_id: SessionId,
    pub leaf_entry_id: EntityId,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct SubmitPrompt {
    pub session_id: SessionId,
    pub text: String,
    #[serde(default)]
    pub artifacts: Vec<ArtifactId>,
    pub delivery: DeliveryPolicy,
    pub reply_route: Option<ReplyRoute>,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeliveryPolicy {
    Immediate,
    NextTurnBoundary,
    WhenIdle,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ReplyRoute {
    pub channel: String,
    #[serde(default)]
    pub external_account: Option<String>,
    pub conversation: String,
    pub thread: Option<String>,
    #[serde(default)]
    pub reply_to_message: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct StagedAttachment {
    pub session_id: SessionId,
    pub staging_file: String,
    pub file_name: String,
    pub media_type: String,
    pub byte_length: u64,
    pub sha256: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct SteerAction {
    pub session_id: SessionId,
    pub text: String,
    pub delivery: DeliveryPolicy,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "target", content = "id")]
pub enum CancelTarget {
    Action(ActionId),
    Goal(GoalId),
    Session(SessionId),
    Child(ChildId),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ModelSelection {
    pub session_id: SessionId,
    pub provider: String,
    pub model: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CreateGoal {
    pub session_id: SessionId,
    pub objective: String,
    pub limits: GoalLimits,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct UpdateGoal {
    pub goal_id: GoalId,
    pub objective: Option<String>,
    pub state: Option<GoalState>,
    pub limits: Option<GoalLimits>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct GoalLimits {
    pub max_turns: Option<u32>,
    pub max_tokens: Option<u64>,
    pub deadline: Option<UtcTimestamp>,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GoalState {
    Draft,
    Ready,
    Running,
    Waiting,
    Reviewing,
    Paused,
    Blocked,
    Complete,
    Failed,
    Cancelled,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CreateChild {
    pub parent_session_id: SessionId,
    pub objective: String,
    pub workspace_mode: ChildWorkspaceMode,
    pub limits: GoalLimits,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChildWorkspaceMode {
    ReadOnlyParent,
    IsolatedCopy,
    DedicatedWorkspace,
    SharedWorkspace,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ChildMessageRequest {
    pub child_id: ChildId,
    pub text: String,
    pub artifact_ids: Vec<ArtifactId>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CreateSchedule {
    pub profile_id: ProfileId,
    pub session_id: Option<SessionId>,
    pub expression: ScheduleExpression,
    pub time_zone: String,
    pub prompt: String,
    pub reply_route: Option<ReplyRoute>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct UpdateSchedule {
    pub job_id: JobId,
    pub expression: Option<ScheduleExpression>,
    pub prompt: Option<String>,
    pub paused: Option<bool>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "value")]
pub enum ScheduleExpression {
    Once(UtcTimestamp),
    IntervalSeconds(u64),
    Calendar(String),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct MemoryQuery {
    pub profile_id: ProfileId,
    pub query: String,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ConfirmationResolution {
    pub confirmation_id: EntityId,
    pub decision: ConfirmationDecision,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConfirmationDecision {
    AllowOnce,
    AllowForSession,
    Deny,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ExportRequest {
    pub session_id: SessionId,
    pub format: ExportFormat,
    pub include_artifacts: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExportFormat {
    JsonLines,
    Markdown,
    PortableBundle,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct BackgroundControl {
    pub profile_id: ProfileId,
    pub mode: BackgroundMode,
    pub pause_until: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct DeliveryRoute {
    pub channel: String,
    pub external_account: String,
    pub conversation: String,
    pub thread: Option<String>,
    pub reply_to_message: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct DeliveryDispatch {
    pub delivery_id: DeliveryId,
    pub claim_token: EntityId,
    pub idempotency_key: String,
    pub route: DeliveryRoute,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
    #[serde(default)]
    pub staged_artifacts: Vec<StagedDeliveryArtifact>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct StagedDeliveryArtifact {
    pub artifact_id: ArtifactId,
    pub staging_file: String,
    pub file_name: String,
    pub media_type: String,
    pub byte_length: u64,
    pub sha256: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct DeliveryAcknowledgement {
    pub delivery_id: DeliveryId,
    pub claim_token: EntityId,
    pub platform_message_id: String,
    pub accepted_at: UtcTimestamp,
    pub duplicate_possible: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeliveryFailureClass {
    Retryable,
    RateLimited,
    Reconnect,
    Permanent,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct DeliveryFailure {
    pub delivery_id: DeliveryId,
    pub claim_token: EntityId,
    pub class: DeliveryFailureClass,
    pub safe_message: String,
    pub retry_after_ms: Option<u64>,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BackgroundMode {
    Disabled,
    Suggest,
    ConfirmSelected,
    Bounded,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CommandResultEnvelope {
    pub protocol: ProtocolVersion,
    pub command_id: CommandId,
    pub completed_at: UtcTimestamp,
    pub result: CommandResult,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "status", content = "payload")]
pub enum CommandResult {
    Accepted { action_id: Option<ActionId> },
    Data(Box<ResponsePayload>),
    Rejected(CommandError),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "value")]
pub enum ResponsePayload {
    Profiles(Vec<ProfileSummary>),
    Sessions(Vec<SessionSummary>),
    Snapshot(Box<SessionSnapshot>),
    Goal(GoalProjection),
    Child(ChildProjection),
    Schedule(ScheduleProjection),
    Memory(Vec<MemoryResult>),
    Export(ExportProjection),
    Background(BackgroundProjection),
    Artifact(ArtifactId),
    DeliveryClaim(Option<Box<DeliveryDispatch>>),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CommandError {
    pub error: CommonError,
    pub unsupported_feature: Option<Feature>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ProfileSummary {
    pub id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub display_name: String,
    pub enabled: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct SessionSummary {
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub profile_id: ProfileId,
    pub title: Option<String>,
    pub state: SessionState,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct SessionSnapshot {
    pub session: SessionSummary,
    pub generation: Generation,
    pub through_sequence: Sequence,
    pub active_action: Option<ActionProjection>,
    pub actions: Vec<ActionProjection>,
    pub messages: Vec<MessageProjection>,
    pub goals: Vec<GoalProjection>,
    pub plans: Vec<PlanProjection>,
    pub children: Vec<ChildProjection>,
    pub kernels: Vec<KernelProjection>,
    pub commitments: Vec<CommitmentProjection>,
    pub schedules: Vec<ScheduleProjection>,
    pub tools: Vec<ToolProjection>,
    pub confirmations: Vec<ConfirmationProjection>,
    pub waits: Vec<WaitProjection>,
    pub deliveries: Vec<DeliveryProjection>,
    pub memory_changes: Vec<MemoryChangeProjection>,
    pub usage: UsageProjection,
    pub presence: PresenceProjection,
    pub revision: Revision,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SessionState {
    Dormant,
    Ready,
    Running,
    WaitingTool,
    WaitingChild,
    WaitingExternal,
    Compacting,
    Paused,
    Failed,
    Archived,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ActionProjection {
    pub action_id: ActionId,
    pub source: String,
    pub state: String,
    pub created_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct MessageProjection {
    pub message_id: MessageId,
    pub role: MessageRole,
    pub text: String,
    pub committed: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MessageRole {
    User,
    Assistant,
    Tool,
    System,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct GoalProjection {
    pub goal_id: GoalId,
    pub objective: String,
    pub state: GoalState,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct PlanProjection {
    pub plan_id: EntityId,
    pub summary: String,
    pub state: String,
    pub revision: Revision,
    pub terminal: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ChildProjection {
    pub child_id: ChildId,
    pub session_id: SessionId,
    pub objective: String,
    pub state: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct KernelProjection {
    pub kernel_id: KernelId,
    pub runtime: String,
    pub state: String,
    pub terminal: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct CommitmentProjection {
    pub commitment_id: CommitmentId,
    pub summary: String,
    pub state: String,
    pub due_at: Option<UtcTimestamp>,
    pub terminal: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ScheduleProjection {
    pub job_id: JobId,
    pub expression: ScheduleExpression,
    pub next_run: Option<UtcTimestamp>,
    pub paused: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ToolProjection {
    pub tool_call_id: ToolCallId,
    pub state: String,
    pub terminal: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ConfirmationProjection {
    pub confirmation_id: EntityId,
    pub summary: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct WaitProjection {
    pub wait_id: EntityId,
    pub state: String,
    pub terminal: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct DeliveryProjection {
    pub delivery_id: DeliveryId,
    pub state: String,
    pub terminal: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MemoryChangeKind {
    Created,
    Updated,
    Deleted,
    Consolidated,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct MemoryChangeProjection {
    pub entry_id: EntryId,
    pub source: String,
    pub change: MemoryChangeKind,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Default, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct UsageProjection {
    pub input_tokens: u64,
    pub output_tokens: u64,
    pub cached_input_tokens: u64,
    pub estimated_cost_microunits: u64,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PresenceState {
    Available,
    Thinking,
    UsingTools,
    WaitingChild,
    WaitingExternal,
    PausedForUser,
    Scheduled,
    Completed,
    Failed,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct PresenceProjection {
    pub session_id: SessionId,
    pub goal_id: Option<GoalId>,
    pub state: PresenceState,
    pub updated_at: UtcTimestamp,
    pub next_wake: Option<UtcTimestamp>,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct MemoryResult {
    pub source: String,
    pub excerpt: String,
    pub score_micros: u32,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ExportProjection {
    pub artifact_id: ArtifactId,
    pub media_type: String,
    pub byte_length: u64,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct BackgroundProjection {
    pub profile_id: ProfileId,
    pub mode: BackgroundMode,
    pub pause_until: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct EventEnvelope {
    pub protocol: ProtocolVersion,
    pub root_tree_id: RootTreeId,
    pub generation: Generation,
    pub first_sequence: Sequence,
    pub sequence: Sequence,
    pub occurred_at: UtcTimestamp,
    pub event: DaemonEvent,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event", content = "payload")]
pub enum DaemonEvent {
    Snapshot(Box<SessionSnapshot>),
    CommandAccepted {
        command_id: CommandId,
    },
    CommandRejected(CommandError),
    SessionChanged(SessionSummary),
    ActionQueued(ActionProjection),
    ActionStarted(ActionProjection),
    ActionFinished(ActionProjection),
    AssistantDelta {
        message_id: MessageId,
        text: String,
    },
    MessageCommitted(MessageProjection),
    GoalChanged(GoalProjection),
    PlanChanged(PlanProjection),
    ChildChanged(ChildProjection),
    KernelChanged(KernelProjection),
    CommitmentChanged(CommitmentProjection),
    ScheduleChanged(ScheduleProjection),
    ToolChanged(ToolProjection),
    WaitChanged(WaitProjection),
    DeliveryChanged(DeliveryProjection),
    MemoryChanged(MemoryChangeProjection),
    UsageChanged(UsageProjection),
    PresenceChanged(PresenceProjection),
    ConfirmationRequested {
        confirmation_id: EntityId,
        summary: String,
    },
    ConfirmationResolved {
        confirmation_id: EntityId,
    },
    Warning(CommonError),
    Error(CommonError),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "message", content = "payload")]
pub enum WireMessage {
    ClientHello(ClientHello),
    ServerHello(ServerHello),
    Command(CommandEnvelope),
    CommandResult(CommandResultEnvelope),
    Event(EventEnvelope),
}

impl WireMessage {
    pub const fn protocol(&self) -> ProtocolVersion {
        match self {
            Self::ClientHello(value) => value.protocol,
            Self::ServerHello(value) => value.protocol,
            Self::Command(value) => value.protocol,
            Self::CommandResult(value) => value.protocol,
            Self::Event(value) => value.protocol,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WireFormat {
    Json,
    Binary,
}

#[derive(Debug, Error)]
pub enum ProtocolError {
    #[error("protocol major mismatch: client {client}, server {server}")]
    MajorMismatch {
        client: ProtocolVersion,
        server: ProtocolVersion,
    },
    #[error("JSON protocol encoding failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("binary protocol encoding failed: {0}")]
    BinaryEncode(#[from] rmp_serde::encode::Error),
    #[error("binary protocol decoding failed: {0}")]
    BinaryDecode(#[from] rmp_serde::decode::Error),
    #[error("message protocol {message} is incompatible with negotiated {negotiated}")]
    IncompatibleEnvelope {
        message: ProtocolVersion,
        negotiated: ProtocolVersion,
    },
    #[error("protocol message exceeds the configured {limit}-byte compatibility bound")]
    MessageTooLarge { limit: usize },
}

/// # Errors
///
/// Returns a major-version mismatch when no common protocol can be negotiated.
pub fn negotiate(
    client: &ClientHello,
    server_protocol: ProtocolVersion,
    server_instance_id: EntityId,
    server_features: &BTreeSet<Feature>,
) -> Result<ServerHello, ProtocolError> {
    let protocol =
        client
            .protocol
            .common_minor(server_protocol)
            .ok_or(ProtocolError::MajorMismatch {
                client: client.protocol,
                server: server_protocol,
            })?;
    let supported_features = client
        .supported_features
        .intersection(server_features)
        .copied()
        .collect();
    Ok(ServerHello {
        protocol,
        server_instance_id,
        supported_features,
        current_generation: client.resume.as_ref().map(|cursor| cursor.generation),
        resume_mode: if client.resume.is_some() {
            ResumeMode::SnapshotThenDelta
        } else {
            ResumeMode::Fresh
        },
    })
}

/// # Errors
///
/// Returns a format-specific serialization error.
pub fn encode(format: WireFormat, message: &WireMessage) -> Result<Vec<u8>, ProtocolError> {
    match format {
        WireFormat::Json => serde_json::to_vec(message).map_err(ProtocolError::from),
        WireFormat::Binary => rmp_serde::to_vec_named(message).map_err(ProtocolError::from),
    }
}

/// # Errors
///
/// Returns a format-specific serialization error for malformed or unsupported messages.
pub fn decode(format: WireFormat, bytes: &[u8]) -> Result<WireMessage, ProtocolError> {
    match format {
        WireFormat::Json => serde_json::from_slice(bytes).map_err(ProtocolError::from),
        WireFormat::Binary => rmp_serde::from_slice(bytes).map_err(ProtocolError::from),
    }
}

/// # Errors
///
/// Returns a decode error or an explicit incompatible-envelope error.
pub fn decode_negotiated(
    format: WireFormat,
    bytes: &[u8],
    negotiated: ProtocolVersion,
) -> Result<WireMessage, ProtocolError> {
    let message = decode(format, bytes)?;
    let version = message.protocol();
    if version.major != negotiated.major || version.minor > negotiated.minor {
        Err(ProtocolError::IncompatibleEnvelope {
            message: version,
            negotiated,
        })
    } else {
        Ok(message)
    }
}

/// Decodes a negotiated current or prior-minor envelope under an explicit compatibility bound.
///
/// # Errors
///
/// Returns an error before parsing when the message exceeds `max_bytes`, or for malformed and
/// incompatible envelopes.
pub fn decode_negotiated_bounded(
    format: WireFormat,
    bytes: &[u8],
    negotiated: ProtocolVersion,
    max_bytes: usize,
) -> Result<WireMessage, ProtocolError> {
    if max_bytes == 0 || bytes.len() > max_bytes {
        return Err(ProtocolError::MessageTooLarge { limit: max_bytes });
    }
    decode_negotiated(format, bytes, negotiated)
}

/// # Errors
///
/// Returns an error if the generated protocol schema cannot be represented as JSON.
pub fn schema_markdown() -> Result<String, serde_json::Error> {
    let schema = schemars::schema_for!(WireMessage);
    let json = serde_json::to_string_pretty(&schema)?;
    Ok(format!(
        "# AgentConnection protocol schema\n\nGenerated from `keith-protocol` {} for protocol {}. Do not edit by hand.\n\n```json\n{json}\n```\n",
        env!("CARGO_PKG_VERSION"),
        CURRENT_PROTOCOL_VERSION
    ))
}

#[cfg(test)]
mod tests {
    use super::*;

    fn hello(version: ProtocolVersion) -> ClientHello {
        ClientHello {
            protocol: version,
            client_id: ClientId::new(),
            client_name: "conformance".into(),
            client_version: "1.0.0".into(),
            supported_features: BTreeSet::from([
                Feature::SessionLifecycle,
                Feature::Replay,
                Feature::WebSocket,
            ]),
            resume: None,
        }
    }

    #[test]
    fn both_wire_formats_round_trip_one_closed_type_system() {
        let message = WireMessage::ClientHello(hello(CURRENT_PROTOCOL_VERSION));
        for format in [WireFormat::Json, WireFormat::Binary] {
            let encoded = encode(format, &message).unwrap();
            assert_eq!(decode(format, &encoded).unwrap(), message);
        }
    }

    #[test]
    fn minor_versions_intersect_features_and_major_mismatch_fails() {
        let client = hello(ProtocolVersion::new(1, 4));
        let server_features = BTreeSet::from([
            Feature::SessionLifecycle,
            Feature::Snapshots,
            Feature::WebSocket,
        ]);
        let result = negotiate(
            &client,
            ProtocolVersion::new(1, 2),
            EntityId::new(),
            &server_features,
        )
        .unwrap();
        assert_eq!(result.protocol, ProtocolVersion::new(1, 2));
        assert_eq!(
            result.supported_features,
            BTreeSet::from([Feature::SessionLifecycle, Feature::WebSocket])
        );

        let error = negotiate(
            &client,
            ProtocolVersion::new(2, 0),
            EntityId::new(),
            &server_features,
        )
        .unwrap_err();
        assert!(matches!(error, ProtocolError::MajorMismatch { .. }));
    }

    #[test]
    fn fixture_and_unknown_command_behavior_are_stable() {
        let fixture = include_bytes!("../tests/fixtures/client-hello-v1.json");
        let decoded = decode(WireFormat::Json, fixture).unwrap();
        assert_eq!(decoded.protocol(), CURRENT_PROTOCOL_VERSION);

        let unknown = br#"{"message":"command","payload":{"protocol":{"major":1,"minor":0},"command_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","client_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","sent_at":0,"session_id":null,"command":{"command":"future_command"}}}"#;
        assert!(decode(WireFormat::Json, unknown).is_err());
    }

    #[test]
    fn negotiated_decoder_rejects_newer_minor_envelopes() {
        let message = WireMessage::ClientHello(hello(ProtocolVersion::new(1, 2)));
        let encoded = encode(WireFormat::Json, &message).unwrap();
        assert!(matches!(
            decode_negotiated(WireFormat::Json, &encoded, ProtocolVersion::new(1, 1)),
            Err(ProtocolError::IncompatibleEnvelope { .. })
        ));
    }
}
