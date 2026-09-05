#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use keith_agent_types::{
    ActionId, ArtifactId, CURRENT_PROTOCOL_VERSION, ChildId, ClientId, CommandId, CommitmentId,
    CommonError, DeliveryId, EntityId, EntryId, Generation, GoalId, JobId, KernelId, MessageId,
    ProfileId, ProtocolVersion, Revision, RootTreeId, Sequence, SessionId, ToolCallId, TurnId,
    UtcTimestamp, WorkspaceId,
};
use keith_platform_contracts::{
    AuditCorrelationId, CancellationId, ExternalAction, LifecycleState, ResourceBounds,
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
    SelfEvolution,
    ChannelAccounts,
    AcpConnections,
    Plugins,
    ConnectedApps,
    Computers,
    Recordings,
    Recipes,
    HarnessRepairs,
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
    ForkSession(ForkSession),
    AttachSession(AttachSession),
    DetachSession {
        session_id: SessionId,
    },
    AcknowledgeEvents(EventAcknowledgement),
    ResumeSession {
        session_id: SessionId,
    },
    BranchSession(BranchRequest),
    SelectBranch(SelectBranch),
    SubmitPrompt(SubmitPrompt),
    Steer(SteerAction),
    Cancel(CancelTarget),
    SelectModel(ModelSelection),
    CreateGoal(CreateGoal),
    UpdateGoal(UpdateGoal),
    ListGoals {
        session_id: SessionId,
    },
    ListChildren {
        session_id: SessionId,
    },
    CreateChild(CreateChild),
    SendChildMessage(ChildMessageRequest),
    ArchiveChild {
        child_id: ChildId,
    },
    CreateSchedule(CreateSchedule),
    UpdateSchedule(UpdateSchedule),
    DeleteSchedule {
        job_id: JobId,
    },
    QueryMemory(MemoryQuery),
    ResolveConfirmation(ConfirmationResolution),
    Export(ExportRequest),
    SetBackgroundControl(BackgroundControl),
    StageAttachment(StagedAttachment),
    ClaimDelivery {
        channel: String,
        external_account: String,
    },
    AcknowledgeDelivery(DeliveryAcknowledgement),
    FailDelivery(DeliveryFailure),
    ChannelAccount(ChannelAccountCommand),
    Integration(IntegrationCommand),
    HarnessRepair(HarnessRepairCommand),
    Evolution(EvolutionCommand),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "action", content = "parameters")]
pub enum ChannelAccountCommand {
    List {
        profile_id: ProfileId,
    },
    Inspect {
        profile_id: ProfileId,
        account_id: String,
    },
    Connect(ChannelAccountConfiguration),
    Configure(ChannelAccountConfiguration),
    Test {
        profile_id: ProfileId,
        account_id: String,
    },
    Pause {
        profile_id: ProfileId,
        account_id: String,
        expected_revision: u64,
    },
    Resume {
        profile_id: ProfileId,
        account_id: String,
        expected_revision: u64,
    },
    RotateCredentials {
        profile_id: ProfileId,
        account_id: String,
        credential_references: Vec<String>,
        expected_generation: u64,
        expected_revision: u64,
    },
    Remove {
        profile_id: ProfileId,
        account_id: String,
        expected_revision: u64,
    },
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ChannelAccountConfiguration {
    pub profile_id: ProfileId,
    pub adapter: String,
    pub account_id: String,
    pub display_name: String,
    pub credential_references: Vec<String>,
    pub credential_generation: u64,
    pub requested_scopes: Vec<String>,
    pub callback_origin: Option<String>,
    pub expected_revision: Option<u64>,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum HarnessRepairMode {
    Advisory,
    Shadow,
    Autonomous,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "action", content = "parameters")]
pub enum HarnessRepairCommand {
    Refresh {
        profile_id: ProfileId,
    },
    SetMode {
        profile_id: ProfileId,
        mode: HarnessRepairMode,
    },
    Approve {
        profile_id: ProfileId,
        operation_id: EntityId,
    },
    Promote {
        profile_id: ProfileId,
        operation_id: EntityId,
    },
    Reverse {
        profile_id: ProfileId,
        operation_id: EntityId,
    },
    RetryCurrentTask {
        profile_id: ProfileId,
        operation_id: EntityId,
    },
}

#[derive(
    Clone, Copy, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum IntegrationService {
    ChannelAccount,
    AcpConnection,
    Plugin,
    ConnectedApp,
    ComputerSession,
    ControlLease,
    Recording,
    Recipe,
    HarnessRepair,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "action", content = "parameters")]
pub enum IntegrationCommand {
    List {
        profile_id: ProfileId,
        service: Option<IntegrationService>,
    },
    Inspect {
        profile_id: ProfileId,
        service: IntegrationService,
        resource_id: EntityId,
    },
    Mutate(Box<IntegrationMutation>),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct IntegrationMutation {
    pub profile_id: ProfileId,
    pub service: IntegrationService,
    pub resource_id: Option<EntityId>,
    pub native_resource_key: String,
    pub display_label: String,
    pub expected_revision: Option<Revision>,
    pub idempotency_key: String,
    pub operation: IntegrationOperation,
    pub authority: ExternalAction,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum IntegrationOperation {
    Connect,
    Configure,
    Test,
    Install,
    Start,
    Pause,
    Resume,
    Stop,
    Cancel,
    Delete,
    Export,
    TakeControl,
    ReleaseControl,
    StartRecording,
    StopRecording,
    Publish,
    Reverse,
}

/// Installation-scoped self-evolution commands. Authority and credentials deliberately never
/// cross the client wire; the daemon either performs a command with authority it already owns or
/// returns an authoritative refusal.
#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "action", content = "parameters")]
pub enum EvolutionCommand {
    Status,
    Enable {
        disclosure_acknowledged: bool,
    },
    Disable {
        reason: String,
    },
    Approve {
        hypothesis_id: EntityId,
    },
    Revert {
        promotion_id: EntityId,
        reason: String,
    },
    RestoreBaseline {
        reason: String,
    },
    BrowseLedger {
        before_sequence: Option<u64>,
        limit: u16,
    },
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
pub struct ForkSession {
    pub source_session_id: SessionId,
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
    ChannelAccounts(Vec<ChannelAccountProjection>),
    ChannelAccount(Box<ChannelAccountProjection>),
    ProfileIntegrations(Box<ProfileIntegrationsProjection>),
    IntegrationResource(Box<IntegrationResourceProjection>),
    IntegrationDeletion(IntegrationDeletionProjection),
    HarnessRepairs(Box<HarnessRepairsProjection>),
    Evolution(Box<EvolutionProjection>),
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelAccountLifecycle {
    Connecting,
    Healthy,
    Degraded,
    Throttled,
    Paused,
    Revoked,
    Failed,
    Removed,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ChannelAccountSetupProjection {
    pub required_credential_references: Vec<String>,
    pub required_scopes: Vec<String>,
    pub callback_state: Option<String>,
    pub webhook_configured: bool,
    pub polling_configured: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ChannelAccountQueueProjection {
    pub pending: u32,
    pub capacity: u32,
    pub notification_budget_remaining: u32,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ChannelAccountProjection {
    pub profile_id: ProfileId,
    pub adapter: String,
    pub account_id: String,
    pub display_name: String,
    pub enabled: bool,
    pub lifecycle: ChannelAccountLifecycle,
    pub setup: ChannelAccountSetupProjection,
    pub credential_references: Vec<String>,
    pub credential_generation: u64,
    pub capabilities: Vec<String>,
    pub queue: ChannelAccountQueueProjection,
    pub reconnect_attempt: u32,
    pub throttled_until: Option<UtcTimestamp>,
    pub cursor_present: bool,
    pub last_event_at: Option<UtcTimestamp>,
    pub last_delivery_at: Option<UtcTimestamp>,
    pub last_transition_at: UtcTimestamp,
    pub safe_error: Option<String>,
    pub restartable: bool,
    pub revision: u64,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state")]
pub enum IntegrationAvailabilityProjection {
    Available,
    Disabled,
    Unavailable { safe_reason: String },
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct IntegrationServiceProjection {
    pub service: IntegrationService,
    pub availability: IntegrationAvailabilityProjection,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct IntegrationResourceProjection {
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub owning_session_id: Option<SessionId>,
    pub service: IntegrationService,
    pub native_resource_key: String,
    pub display_label: String,
    pub lifecycle: LifecycleState,
    pub cancellation_id: CancellationId,
    pub audit_correlation: AuditCorrelationId,
    pub bounds: ResourceBounds,
    pub controls: BTreeSet<IntegrationControl>,
    pub safe_error: Option<String>,
    pub revision: Revision,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

#[derive(
    Clone, Copy, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum IntegrationControl {
    Restart,
    Cancel,
    Export,
    Delete,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct ProfileIntegrationsProjection {
    pub profile_id: ProfileId,
    pub through_sequence: Sequence,
    pub services: Vec<IntegrationServiceProjection>,
    pub resources: Vec<IntegrationResourceProjection>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct IntegrationDeletionProjection {
    pub profile_id: ProfileId,
    pub service: IntegrationService,
    pub resource_id: EntityId,
    pub deleted_records: u64,
    pub remaining_records: u64,
    pub remaining_derived_indexes: Option<u64>,
    pub remaining_media_objects: Option<u64>,
    pub retained_operation_records: u64,
    pub retained_audit_records: u64,
    pub retention_reason: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct HarnessRepairAvailabilityProjection {
    pub advisory: bool,
    pub shadow: bool,
    pub autonomous: bool,
    pub shadow_unavailable_reason: Option<String>,
    pub autonomous_unavailable_reason: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct HarnessRepairMetricsProjection {
    pub cases: u64,
    pub task_success_basis_points: u16,
    pub truthful_completion_basis_points: u16,
    pub safety_basis_points: u16,
    pub correction_adherence_basis_points: u16,
    pub tokens: u64,
    pub external_cost_micros: u64,
    pub latency_ms: u64,
    pub retries: u32,
    pub cpu_ms: u64,
    pub peak_memory_bytes: u64,
    pub disk_bytes: u64,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[allow(clippy::struct_excessive_bools)]
pub struct HarnessRepairProjection {
    pub id: EntityId,
    pub candidate_id: EntityId,
    pub mode: HarnessRepairMode,
    pub phase: String,
    pub headline: String,
    pub summary: String,
    pub metrics: HarnessRepairMetricsProjection,
    pub needs_approval: bool,
    pub can_retry_current_task: bool,
    pub can_promote: bool,
    pub can_reverse: bool,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct HarnessRepairsProjection {
    pub profile_id: ProfileId,
    pub availability: HarnessRepairAvailabilityProjection,
    pub selected_mode: HarnessRepairMode,
    pub repairs: Vec<HarnessRepairProjection>,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionAvailabilityProjection {
    Available { rustc: String, cargo: String },
    Unavailable { reasons: Vec<String> },
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct EvolutionDisclosureProjection {
    pub editable_surface: String,
    pub protected_surface: String,
    pub autonomy: String,
    pub reversal: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct EvolutionHypothesisProjection {
    pub hypothesis_id: EntityId,
    pub target: String,
    pub metric: String,
    pub state: String,
    pub evidence: Vec<String>,
    pub measured_result: Option<String>,
    pub readable_diff: Option<String>,
    pub approval_required: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct EvolutionLedgerProjection {
    pub sequence: u64,
    pub occurred_at: UtcTimestamp,
    pub kind: String,
    pub summary: String,
    pub state: String,
    pub evidence: Vec<String>,
    pub measured_result: Option<String>,
    pub readable_diff: Option<String>,
    pub hypothesis_id: Option<EntityId>,
    pub promotion_id: Option<EntityId>,
    pub reversible: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct EvolutionProjection {
    pub protocol_version: ProtocolVersion,
    pub enabled: bool,
    pub state: String,
    pub availability: EvolutionAvailabilityProjection,
    pub disclosure: EvolutionDisclosureProjection,
    pub active: Option<EvolutionHypothesisProjection>,
    pub ledger: Vec<EvolutionLedgerProjection>,
    pub has_more_ledger: bool,
    pub guidance: Option<String>,
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
    #[serde(default)]
    pub terminal: Option<TurnTerminalProjection>,
    pub revision: Revision,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TurnTerminalStatus {
    Completed,
    Failed,
    Cancelled,
    Exhausted,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[allow(clippy::struct_excessive_bools)]
pub struct TurnTerminalProjection {
    pub session_id: SessionId,
    pub turn_id: TurnId,
    pub final_id: EntryId,
    pub status: TurnTerminalStatus,
    pub execution_succeeded: bool,
    pub final_created: bool,
    pub artifacts_persisted: bool,
    pub delivery_enqueued: bool,
    pub delivery_acknowledged: bool,
    pub detail: Option<String>,
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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub final_id: Option<EntryId>,
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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub tool: Option<String>,
    pub state: String,
    pub terminal: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentActivityOutcome {
    Completed,
    Cancelled,
    Exhausted,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "activity", content = "payload")]
pub enum AgentActivityKind {
    AgentStarted,
    TurnStarted {
        number: u32,
    },
    AssistantStarted {
        message_id: MessageId,
    },
    AssistantCompleted {
        message_id: MessageId,
        complete: bool,
    },
    StrategyChanged {
        reason: String,
    },
    TurnEnded,
    AgentEnded {
        outcome: AgentActivityOutcome,
    },
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct AgentActivityProjection {
    pub session_id: SessionId,
    pub turn_id: TurnId,
    pub sequence: u64,
    pub kind: AgentActivityKind,
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
    #[serde(default)]
    pub turn_id: Option<TurnId>,
    #[serde(default)]
    pub final_id: Option<EntryId>,
    #[serde(default)]
    pub acknowledged: bool,
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
pub struct ProfileEventEnvelope {
    pub protocol: ProtocolVersion,
    pub profile_id: ProfileId,
    pub generation: Generation,
    pub sequence: Sequence,
    pub occurred_at: UtcTimestamp,
    pub event: ProfileIntegrationEvent,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event", content = "payload")]
pub enum ProfileIntegrationEvent {
    Snapshot(Box<ProfileIntegrationsProjection>),
    ResourceChanged(Box<IntegrationResourceProjection>),
    ResourceRemoved {
        service: IntegrationService,
        resource_id: EntityId,
    },
    ServiceAvailabilityChanged(IntegrationServiceProjection),
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
    AgentActivity(AgentActivityProjection),
    MessageCommitted(MessageProjection),
    TurnTerminal(TurnTerminalProjection),
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
    EvolutionChanged(Box<EvolutionProjection>),
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
pub struct SnapshotFrame {
    pub protocol: ProtocolVersion,
    pub root_tree_id: RootTreeId,
    pub generation: Generation,
    pub first_sequence: Sequence,
    pub sequence: Sequence,
    pub occurred_at: UtcTimestamp,
    pub snapshot: Box<SessionSnapshot>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
pub struct TerminalFrame {
    pub protocol: ProtocolVersion,
    pub root_tree_id: RootTreeId,
    pub generation: Generation,
    pub first_sequence: Sequence,
    pub sequence: Sequence,
    pub occurred_at: UtcTimestamp,
    pub terminal: TurnTerminalProjection,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "message", content = "payload")]
pub enum WireMessage {
    ClientHello(ClientHello),
    ServerHello(ServerHello),
    Command(CommandEnvelope),
    CommandResult(CommandResultEnvelope),
    Event(EventEnvelope),
    Snapshot(SnapshotFrame),
    Terminal(TerminalFrame),
}

impl WireMessage {
    pub fn from_event(envelope: EventEnvelope) -> Self {
        let EventEnvelope {
            protocol,
            root_tree_id,
            generation,
            first_sequence,
            sequence,
            occurred_at,
            event,
        } = envelope;
        match event {
            DaemonEvent::Snapshot(snapshot) => Self::Snapshot(SnapshotFrame {
                protocol,
                root_tree_id,
                generation,
                first_sequence,
                sequence,
                occurred_at,
                snapshot,
            }),
            DaemonEvent::TurnTerminal(terminal) => Self::Terminal(TerminalFrame {
                protocol,
                root_tree_id,
                generation,
                first_sequence,
                sequence,
                occurred_at,
                terminal,
            }),
            event => Self::Event(EventEnvelope {
                protocol,
                root_tree_id,
                generation,
                first_sequence,
                sequence,
                occurred_at,
                event,
            }),
        }
    }

    pub fn into_event(self) -> Option<EventEnvelope> {
        match self {
            Self::Event(envelope) => Some(envelope),
            Self::Snapshot(frame) => Some(EventEnvelope {
                protocol: frame.protocol,
                root_tree_id: frame.root_tree_id,
                generation: frame.generation,
                first_sequence: frame.first_sequence,
                sequence: frame.sequence,
                occurred_at: frame.occurred_at,
                event: DaemonEvent::Snapshot(frame.snapshot),
            }),
            Self::Terminal(frame) => Some(EventEnvelope {
                protocol: frame.protocol,
                root_tree_id: frame.root_tree_id,
                generation: frame.generation,
                first_sequence: frame.first_sequence,
                sequence: frame.sequence,
                occurred_at: frame.occurred_at,
                event: DaemonEvent::TurnTerminal(frame.terminal),
            }),
            Self::ClientHello(_)
            | Self::ServerHello(_)
            | Self::Command(_)
            | Self::CommandResult(_) => None,
        }
    }

    pub const fn protocol(&self) -> ProtocolVersion {
        match self {
            Self::ClientHello(value) => value.protocol,
            Self::ServerHello(value) => value.protocol,
            Self::Command(value) => value.protocol,
            Self::CommandResult(value) => value.protocol,
            Self::Event(value) => value.protocol,
            Self::Snapshot(value) => value.protocol,
            Self::Terminal(value) => value.protocol,
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
        assert_eq!(decoded.protocol(), ProtocolVersion::new(1, 0));

        let unknown = br#"{"message":"command","payload":{"protocol":{"major":1,"minor":0},"command_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","client_id":"01ARZ3NDEKTSV4RRFFQ69G5FAW","sent_at":0,"session_id":null,"command":{"command":"future_command"}}}"#;
        assert!(decode(WireFormat::Json, unknown).is_err());
    }

    #[test]
    fn evolution_wire_contains_intent_but_no_authority_material() {
        let message = WireMessage::Command(CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id: ClientId::new(),
            sent_at: UtcTimestamp::from_unix_millis(1),
            session_id: None,
            command: ClientCommand::Evolution(EvolutionCommand::Enable {
                disclosure_acknowledged: true,
            }),
        });
        let value: serde_json::Value =
            serde_json::from_slice(&encode(WireFormat::Json, &message).unwrap()).unwrap();
        assert_eq!(value["payload"]["command"]["command"], "evolution");
        assert_eq!(
            value["payload"]["command"]["parameters"]["action"],
            "enable"
        );
        let encoded = serde_json::to_string(&value).unwrap();
        assert!(!encoded.contains("credential"));
        assert!(!encoded.contains("authority"));
        assert!(!encoded.contains("identity"));
    }

    #[test]
    fn fork_session_wire_names_the_source_without_assigning_the_destination() {
        let source_session_id = SessionId::new();
        let message = WireMessage::Command(CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id: ClientId::new(),
            sent_at: UtcTimestamp::from_unix_millis(1),
            session_id: Some(source_session_id.clone()),
            command: ClientCommand::ForkSession(ForkSession {
                source_session_id: source_session_id.clone(),
                title: Some("Independent fork".into()),
            }),
        });
        for format in [WireFormat::Json, WireFormat::Binary] {
            let encoded = encode(format, &message).unwrap();
            assert_eq!(decode(format, &encoded).unwrap(), message);
        }
        let encoded = serde_json::to_value(&message).unwrap();
        assert_eq!(
            encoded["payload"]["command"]["parameters"]["source_session_id"],
            source_session_id.to_string()
        );
        assert!(
            encoded["payload"]["command"]["parameters"]
                .get("session_id")
                .is_none()
        );
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
