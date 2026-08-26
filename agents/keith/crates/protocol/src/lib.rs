#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use keith_agent_types::{
    ActionId, ArtifactId, CURRENT_PROTOCOL_VERSION, ChildId, ClientId, CommandId, CommitmentId,
    CommonError, ConversationId, DeliveryId, EntityId, EntryId, EventId, Generation, GoalId,
    GrantId, JobId, KernelId, MessageId, ProfileId, ProtocolVersion, Revision, RootTreeId,
    Sequence, SessionId, ToolCallId, TurnId, UtcTimestamp, WorkspaceId,
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
    AgentLifecycle,
    Conversations,
    TeammatesProtocol,
    ComputerStreaming,
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
    Evolution(EvolutionCommand),
    AgentLifecycle(AgentLifecycleCommand),
    Conversation(ConversationCommand),
    Computer(ComputerProtocolCommand),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "action", content = "parameters")]
pub enum AgentLifecycleCommand {
    List,
    Inspect { profile_id: ProfileId },
    Create(CreateAgent),
    Edit(EditAgent),
    Enable(ProfileRevisionCommand),
    Disable(ProfileRevisionCommand),
    Archive(ProfileRevisionCommand),
    Unarchive(ProfileRevisionCommand),
    Hide(ProfileRevisionCommand),
    Unhide(ProfileRevisionCommand),
    Duplicate(DuplicateAgent),
    PlanDelete(PlanAgentDelete),
    ConfirmDelete(ConfirmAgentDelete),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CreateAgent {
    pub display_name: String,
    pub role: String,
    pub description: String,
    pub avatar: Option<String>,
    pub model_route: Option<AgentModelRoute>,
    pub computer_policy: AgentComputerPolicy,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct EditAgent {
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub display_name: Option<String>,
    pub role: Option<String>,
    pub description: Option<String>,
    pub avatar: Option<Option<String>>,
    pub model_route: Option<AgentModelRoute>,
    pub computer_policy: Option<AgentComputerPolicy>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileRevisionCommand {
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DuplicateAgent {
    pub source_profile_id: ProfileId,
    pub expected_revision: Revision,
    pub display_name: String,
    pub role: Option<String>,
    pub description: Option<String>,
    pub avatar: Option<Option<String>>,
    pub selection: AgentDuplicateSelection,
}

#[derive(Clone, Debug, Default, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDuplicateSelection {
    pub model_route: bool,
    pub skills: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PlanAgentDelete {
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub owned_work: AgentOwnedWorkDisposition,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConfirmAgentDelete {
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub owned_work: AgentOwnedWorkDisposition,
    pub confirmation: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "disposition")]
pub enum AgentOwnedWorkDisposition {
    Cancel,
    Transfer { to_profile_id: ProfileId },
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentModelRoute {
    pub provider: String,
    pub model: String,
    pub fallbacks: Vec<AgentModelSelection>,
    pub credential_ref: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentModelSelection {
    pub provider: String,
    pub model: String,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct AgentComputerPolicy {
    pub enabled: bool,
    pub allow_downloads: bool,
    pub allow_uploads: bool,
    pub require_confirmation_for_consequential_actions: bool,
    pub max_idle_seconds: u32,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "action", content = "parameters")]
pub enum ConversationCommand {
    Page(ConversationPageRequest),
    Context(ConversationContextRequest),
    Search(ConversationSearchRequest),
    ChangeMembership(ConversationMembershipRequest),
    AdvanceRead(ConversationReadRequest),
    UpdateParticipant(ConversationParticipantUpdateRequest),
    SetArchived(ConversationArchiveRequest),
    PutGrant(ConversationGrantRequest),
    RevokeGrant(ConversationGrantRevokeRequest),
    Edit(ConversationEditRequest),
    Redact(ConversationRedactRequest),
    React(ConversationReactionRequest),
    SetPinned(ConversationPinRequest),
    AppendThread(ConversationThreadRequest),
    PromoteConversationArtifact(PromoteConversationArtifact),
    SearchSharedKnowledge(SharedKnowledgeSearchRequest),
    List(ConversationListRequest),
    ForProfile(ProfileConversationListRequest),
    ForProfiles(ProfileConversationListsRequest),
    Teammates(TeammatesCommand),
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationPageRequest {
    pub conversation_id: ConversationId,
    pub after_sequence: u64,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationContextRequest {
    pub conversation_id: ConversationId,
    pub applied_through_sequence: u64,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationSearchRequest {
    pub query: String,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationMembershipAction {
    Join,
    Leave,
    Rejoin,
}

#[derive(Clone, Debug, Eq, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "profile_id")]
pub enum ConversationParticipantPrincipal {
    Human,
    Agent(ProfileId),
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationParticipantRole {
    Owner,
    Member,
    Observer,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationMembershipRequest {
    pub conversation_id: ConversationId,
    pub target: ConversationParticipantPrincipal,
    pub role: ConversationParticipantRole,
    pub action: ConversationMembershipAction,
    pub expected_participant_revision: Revision,
    pub expected_conversation_revision: Revision,
    pub operation_key: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationReadRequest {
    pub conversation_id: ConversationId,
    pub expected_revision: Option<Revision>,
    pub read_through_sequence: u64,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationParticipantUpdateRequest {
    pub conversation_id: ConversationId,
    pub target: ConversationParticipantPrincipal,
    pub expected_revision: Revision,
    pub hidden: bool,
    pub muted: bool,
    pub mentions_only: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationArchiveRequest {
    pub conversation_id: ConversationId,
    pub expected_revision: Revision,
    pub archived: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationSharedResourceKind {
    Artifact,
    File,
    KnowledgeSpace,
    Conversation,
}

#[derive(
    Clone, Copy, Debug, Eq, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum ConversationGrantOperation {
    Read,
    Search,
    Append,
    Export,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationSharedDeletionPolicy {
    RetainUntilExplicitDelete,
    DeleteWhenSourceDeleted,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationGrantRequest {
    pub grant_id: GrantId,
    pub expected_revision: Option<Revision>,
    pub resource_kind: ConversationSharedResourceKind,
    pub resource_id: String,
    pub grantee: ProfileId,
    pub purpose: String,
    pub source_conversation_id: Option<ConversationId>,
    pub source_event_ids: Vec<EventId>,
    pub resource_policy_revision: Revision,
    pub deletion_policy: ConversationSharedDeletionPolicy,
    pub operations: BTreeSet<ConversationGrantOperation>,
    pub expires_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationGrantRevokeRequest {
    pub grant_id: GrantId,
    pub expected_revision: Revision,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationEditRequest {
    pub conversation_id: ConversationId,
    pub expected_revision: Revision,
    pub target_event_id: EventId,
    pub replacement: String,
    pub operation_key: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationRedactRequest {
    pub conversation_id: ConversationId,
    pub expected_revision: Revision,
    pub target_event_id: EventId,
    pub operation_key: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationReactionRequest {
    pub conversation_id: ConversationId,
    pub expected_revision: Revision,
    pub target_event_id: EventId,
    pub reaction: String,
    pub remove: bool,
    pub operation_key: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationPinRequest {
    pub conversation_id: ConversationId,
    pub expected_revision: Revision,
    pub target_event_id: EventId,
    pub pinned: bool,
    pub operation_key: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationThreadRequest {
    pub conversation_id: ConversationId,
    pub expected_revision: Revision,
    pub parent_event_id: EventId,
    pub content: String,
    pub artifacts: Vec<ConversationAttachmentRequest>,
    pub operation_key: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct PromoteConversationArtifact {
    pub artifact_id: ArtifactId,
    pub digest_sha256: String,
    pub conversation_id: ConversationId,
    pub expected_access_policy_revision: Revision,
    pub source_event_ids: Vec<EventId>,
    pub operation_key: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationAttachmentRequest {
    pub artifact_id: ArtifactId,
    pub digest_sha256: String,
    pub source_event_ids: Vec<EventId>,
}

impl<'de> Deserialize<'de> for PromoteConversationArtifact {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Wire {
            artifact_id: ArtifactId,
            digest_sha256: String,
            conversation_id: ConversationId,
            expected_access_policy_revision: Revision,
            source_event_ids: Vec<EventId>,
            operation_key: String,
        }
        let wire = Wire::deserialize(deserializer)?;
        validate_conversation_artifact_reference(&wire.digest_sha256, &wire.source_event_ids)
            .map_err(serde::de::Error::custom)?;
        Ok(Self {
            artifact_id: wire.artifact_id,
            digest_sha256: wire.digest_sha256,
            conversation_id: wire.conversation_id,
            expected_access_policy_revision: wire.expected_access_policy_revision,
            source_event_ids: wire.source_event_ids,
            operation_key: wire.operation_key,
        })
    }
}

impl<'de> Deserialize<'de> for ConversationAttachmentRequest {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Wire {
            artifact_id: ArtifactId,
            digest_sha256: String,
            source_event_ids: Vec<EventId>,
        }
        let wire = Wire::deserialize(deserializer)?;
        validate_conversation_artifact_reference(&wire.digest_sha256, &wire.source_event_ids)
            .map_err(serde::de::Error::custom)?;
        Ok(Self {
            artifact_id: wire.artifact_id,
            digest_sha256: wire.digest_sha256,
            source_event_ids: wire.source_event_ids,
        })
    }
}

fn validate_conversation_artifact_reference(
    digest_sha256: &str,
    source_event_ids: &[EventId],
) -> Result<(), &'static str> {
    if digest_sha256.len() != 64 || !digest_sha256.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err("artifact digest must be exactly 64 hexadecimal characters");
    }
    if source_event_ids.is_empty() || source_event_ids.len() > 64 {
        return Err("artifact provenance requires between 1 and 64 source events");
    }
    let unique = source_event_ids.iter().collect::<BTreeSet<_>>();
    if unique.len() != source_event_ids.len() {
        return Err("artifact provenance source events must be unique");
    }
    Ok(())
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub struct SharedKnowledgeSearchRequest {
    pub space_id: EntityId,
    pub query: String,
    pub limit: usize,
}

impl<'de> Deserialize<'de> for SharedKnowledgeSearchRequest {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        #[derive(Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Wire {
            space_id: EntityId,
            query: String,
            limit: usize,
        }
        let wire = Wire::deserialize(deserializer)?;
        if wire.query.trim().is_empty() || wire.query.len() > 4_096 {
            return Err(serde::de::Error::custom(
                "shared knowledge query must contain between 1 and 4096 bytes",
            ));
        }
        if wire.limit == 0 || wire.limit > 100 {
            return Err(serde::de::Error::custom(
                "shared knowledge result limit must be between 1 and 100",
            ));
        }
        Ok(Self {
            space_id: wire.space_id,
            query: wire.query,
            limit: wire.limit,
        })
    }
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
    pub parent_entry_id: Option<EntityId>,
    pub conversation_id: Option<ConversationId>,
    pub conversation_event_id: Option<EventId>,
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
    AgentRoster(Vec<AgentRosterProjection>),
    Agent(Box<AgentLifecycleProjection>),
    AgentDeletePlan(Box<AgentDeletePlanProjection>),
    AgentDelete(Box<AgentDeleteReportProjection>),
    Conversation(Box<ConversationProjection>),
    ConversationContext(Box<ConversationContextProjection>),
    ConversationSearch(Vec<ConversationSearchHitProjection>),
    ConversationMutation(Box<ConversationMutationReceipt>),
    ConversationArtifactPromotion(Box<ConversationArtifactPromotionReceipt>),
    SharedKnowledgeSearch(Vec<SharedKnowledgeSearchHit>),
    ConversationList(Box<ConversationListResponse>),
    ProfileConversationLists(Vec<ConversationListResponse>),
    TeammatesReceipt(Box<TeammatesCommandReceipt>),
    TeammatesSnapshot(Box<TeammatesSnapshot>),
    TeammatesEvent(Box<ConversationProtocolEnvelope>),
    Computer(Box<ComputerProtocolResponse>),
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
    Evolution(Box<EvolutionProjection>),
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
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub background: Option<BackgroundProjection>,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentLifecycleState {
    Provisioning,
    Draft,
    Enabled,
    Disabled,
    Archived,
    Deleted,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentRosterProjection {
    pub profile_id: ProfileId,
    pub name: String,
    pub role: String,
    #[serde(default)]
    pub description: String,
    pub avatar: Option<String>,
    pub lifecycle: AgentLifecycleState,
    pub hidden: bool,
    pub enabled: bool,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentLifecycleAuditProjection {
    pub sequence: u64,
    pub actor: String,
    pub action: String,
    pub revision: Revision,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentLifecycleProjection {
    pub roster: AgentRosterProjection,
    pub workspace_id: WorkspaceId,
    pub description: String,
    pub model_route: AgentModelRoute,
    pub skills: Vec<String>,
    pub tools: Vec<String>,
    pub channels: Vec<String>,
    pub computer_policy: AgentComputerPolicy,
    pub audit: Vec<AgentLifecycleAuditProjection>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeletePlanProjection {
    pub profile_id: ProfileId,
    pub expected_revision: Revision,
    pub replay_key: String,
    pub confirmation: String,
    pub private_resources: Vec<String>,
    pub owned_work: Vec<String>,
    pub lease_revocations: Vec<String>,
    pub retained_shared_data: Vec<String>,
    pub retained_audit: Vec<String>,
    pub externally_controlled_remnants: Vec<String>,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AgentDeleteExecutionState {
    Executed,
    Duplicate,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentDeleteReportProjection {
    pub profile_id: ProfileId,
    pub replay_key: String,
    pub status: AgentDeleteExecutionState,
    pub retained_shared_data: Vec<String>,
    pub retained_audit: Vec<String>,
    pub externally_controlled_remnants: Vec<String>,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationKindProjection {
    HumanAgentDm,
    AgentAgentDm,
    Group,
    Thread,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationLifecycleProjection {
    Active,
    Archived,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "profile_id")]
pub enum ConversationPrincipalProjection {
    Human,
    Agent(ProfileId),
    System,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationParticipantRoleProjection {
    Owner,
    Member,
    Observer,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationParticipantProjection {
    pub principal: ConversationPrincipalProjection,
    pub role: ConversationParticipantRoleProjection,
    pub joined_at: UtcTimestamp,
    pub left_at: Option<UtcTimestamp>,
    pub applied_through_sequence: u64,
    pub hidden: bool,
    pub muted: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationEventKindProjection {
    Message,
    Edit,
    Redaction,
    Reaction,
    MembershipChange,
    Pin,
    AssignmentChange,
    Handoff,
    RoutineResult,
    ComputerEvent,
    SystemNotice,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationArtifactProjection {
    pub artifact_id: ArtifactId,
    pub digest_sha256: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationEventProjection {
    pub event_id: EventId,
    pub conversation_id: ConversationId,
    pub sequence: u64,
    pub publication_key: String,
    pub author: ConversationPrincipalProjection,
    pub timestamp: UtcTimestamp,
    pub kind: ConversationEventKindProjection,
    pub content: Option<String>,
    pub artifacts: Vec<ConversationArtifactProjection>,
    pub reply_to: Option<EventId>,
    pub thread_parent: Option<EventId>,
    pub provenance_source: String,
    pub provenance_source_ids: Vec<String>,
    pub migration_version: Option<String>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationRecordProjection {
    pub conversation_id: ConversationId,
    pub kind: ConversationKindProjection,
    pub lifecycle: ConversationLifecycleProjection,
    pub title: String,
    pub creator: ConversationPrincipalProjection,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub revision: Revision,
    pub participant_revision: Revision,
    pub participant_profiles: Vec<ProfileId>,
    pub human_participant: bool,
    pub head_sequence: u64,
    pub head_event_id: Option<EventId>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationProjection {
    pub conversation: ConversationRecordProjection,
    pub participants: Vec<ConversationParticipantProjection>,
    pub events: Vec<ConversationEventProjection>,
    pub read_through_sequence: u64,
    pub unread_count: u64,
    pub pinned: bool,
    pub hidden: bool,
    pub archived: bool,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationContextProjection {
    pub conversation_id: ConversationId,
    pub applied_through_sequence: u64,
    pub visible_events: Vec<ConversationEventProjection>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationSearchHitProjection {
    pub conversation_id: ConversationId,
    pub event_id: EventId,
    pub sequence: u64,
    pub author: ConversationPrincipalProjection,
    pub timestamp: UtcTimestamp,
    pub content: String,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationMutationStatus {
    Applied,
    Replayed,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationMutationReceipt {
    pub conversation_id: Option<ConversationId>,
    pub event_id: Option<EventId>,
    pub grant_id: Option<GrantId>,
    pub revision: Option<Revision>,
    pub status: ConversationMutationStatus,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationArtifactPromotionReceipt {
    pub artifact_id: ArtifactId,
    pub conversation_id: ConversationId,
    pub access_policy_revision: Revision,
    pub source_event_ids: Vec<EventId>,
    pub status: ConversationMutationStatus,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SharedKnowledgeSourceKind {
    DurableMemory,
    DailyMemory,
    CurrentState,
    Knowledge,
    SessionSummary,
    Skill,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SharedKnowledgeSearchScore {
    pub lexical_millionths: u32,
    pub trigram_millionths: u32,
    pub vector_millionths: Option<u32>,
    pub merged_millionths: u32,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SharedKnowledgeAuthorizationProvenance {
    pub owner_profile_id: ProfileId,
    pub space_id: EntityId,
    pub observed_permission_revision: Revision,
    pub space_revision: Revision,
    pub membership_permission_revision: Revision,
    pub grant_id: GrantId,
    pub grant_revision: Revision,
    pub resource_policy_revision: Revision,
    pub source_conversation_id: Option<ConversationId>,
    pub source_event_ids: Vec<EventId>,
    pub source_policy_revision: Revision,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SharedKnowledgeSearchHit {
    pub document_id: EntityId,
    pub source_path: String,
    pub source_version: String,
    pub heading_path: Vec<String>,
    pub excerpt: String,
    pub source_kind: SharedKnowledgeSourceKind,
    pub modified_at: UtcTimestamp,
    pub score: SharedKnowledgeSearchScore,
    pub provenance: SharedKnowledgeAuthorizationProvenance,
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
    #[serde(default)]
    pub prompt: String,
    #[serde(default)]
    pub state: String,
    pub next_run: Option<UtcTimestamp>,
    #[serde(default)]
    pub last_run: Option<UtcTimestamp>,
    #[serde(default)]
    pub attempts: u32,
    #[serde(default)]
    pub failures: u32,
    #[serde(default)]
    pub safe_error: Option<String>,
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
    Teammates(Box<ConversationProtocolEnvelope>),
    Computer(Box<ComputerProtocolEventEnvelope>),
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
    fn negotiated_decoder_rejects_newer_minor_envelopes() {
        let message = WireMessage::ClientHello(hello(ProtocolVersion::new(1, 2)));
        let encoded = encode(WireFormat::Json, &message).unwrap();
        assert!(matches!(
            decode_negotiated(WireFormat::Json, &encoded, ProtocolVersion::new(1, 1)),
            Err(ProtocolError::IncompatibleEnvelope { .. })
        ));
    }

    fn command(command: ClientCommand) -> WireMessage {
        WireMessage::Command(CommandEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            client_id: ClientId::new(),
            sent_at: UtcTimestamp::from_unix_millis(7),
            session_id: None,
            command,
        })
    }

    #[test]
    fn agent_lifecycle_duplication_wire_is_closed_and_carries_no_authority_material() {
        let message = command(ClientCommand::AgentLifecycle(
            AgentLifecycleCommand::Duplicate(DuplicateAgent {
                source_profile_id: ProfileId::new(),
                expected_revision: Revision::ZERO,
                display_name: "Researcher copy".into(),
                role: None,
                description: None,
                avatar: None,
                selection: AgentDuplicateSelection {
                    model_route: true,
                    skills: true,
                },
            }),
        ));
        for format in [WireFormat::Json, WireFormat::Binary] {
            let encoded = encode(format, &message).unwrap();
            assert_eq!(decode(format, &encoded).unwrap(), message);
        }

        let encoded = String::from_utf8(encode(WireFormat::Json, &message).unwrap()).unwrap();
        for forbidden in [
            "tools",
            "channels",
            "autonomy",
            "notifications",
            "computer_policy",
            "credential_ref",
            "installation",
        ] {
            assert!(!encoded.contains(forbidden), "wire exposed {forbidden}");
        }
        assert!(
            serde_json::from_value::<AgentDuplicateSelection>(serde_json::json!({
                "model_route": true,
                "skills": true,
                "tools": true
            }))
            .is_err()
        );
    }

    #[test]
    fn conversation_store_context_and_search_commands_share_the_closed_wire_contract() {
        let conversation_id = ConversationId::new();
        let messages = [
            command(ClientCommand::Conversation(ConversationCommand::Page(
                ConversationPageRequest {
                    conversation_id: conversation_id.clone(),
                    after_sequence: 4,
                    limit: 50,
                },
            ))),
            command(ClientCommand::Conversation(ConversationCommand::Context(
                ConversationContextRequest {
                    conversation_id,
                    applied_through_sequence: 9,
                    limit: 100,
                },
            ))),
            command(ClientCommand::Conversation(ConversationCommand::Search(
                ConversationSearchRequest {
                    query: "handoff".into(),
                    limit: 20,
                },
            ))),
        ];
        for message in messages {
            for format in [WireFormat::Json, WireFormat::Binary] {
                let encoded = encode(format, &message).unwrap();
                assert_eq!(decode(format, &encoded).unwrap(), message);
            }
        }
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn conversation_mutations_are_closed_actor_free_commands() {
        let conversation_id = ConversationId::new();
        let profile_id = ProfileId::new();
        let target_event_id = EventId::new();
        let grant_id = GrantId::new();
        let artifact_id = ArtifactId::new();
        let operation_key = "client-operation-1".to_owned();
        let messages = vec![
            command(ClientCommand::Conversation(
                ConversationCommand::ChangeMembership(ConversationMembershipRequest {
                    conversation_id: conversation_id.clone(),
                    target: ConversationParticipantPrincipal::Agent(profile_id.clone()),
                    role: ConversationParticipantRole::Member,
                    action: ConversationMembershipAction::Join,
                    expected_participant_revision: Revision::ZERO,
                    expected_conversation_revision: Revision::ZERO,
                    operation_key: operation_key.clone(),
                }),
            )),
            command(ClientCommand::Conversation(
                ConversationCommand::AdvanceRead(ConversationReadRequest {
                    conversation_id: conversation_id.clone(),
                    expected_revision: None,
                    read_through_sequence: 4,
                }),
            )),
            command(ClientCommand::Conversation(
                ConversationCommand::UpdateParticipant(ConversationParticipantUpdateRequest {
                    conversation_id: conversation_id.clone(),
                    target: ConversationParticipantPrincipal::Agent(profile_id.clone()),
                    expected_revision: Revision::ZERO,
                    hidden: true,
                    muted: false,
                    mentions_only: true,
                }),
            )),
            command(ClientCommand::Conversation(
                ConversationCommand::SetArchived(ConversationArchiveRequest {
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    archived: true,
                }),
            )),
            command(ClientCommand::Conversation(ConversationCommand::PutGrant(
                ConversationGrantRequest {
                    grant_id: grant_id.clone(),
                    expected_revision: None,
                    resource_kind: ConversationSharedResourceKind::KnowledgeSpace,
                    resource_id: "shared/research".into(),
                    grantee: profile_id,
                    purpose: "review".into(),
                    source_conversation_id: Some(conversation_id.clone()),
                    source_event_ids: vec![target_event_id.clone()],
                    resource_policy_revision: Revision::ZERO,
                    deletion_policy: ConversationSharedDeletionPolicy::DeleteWhenSourceDeleted,
                    operations: BTreeSet::from([
                        ConversationGrantOperation::Read,
                        ConversationGrantOperation::Search,
                    ]),
                    expires_at: None,
                },
            ))),
            command(ClientCommand::Conversation(
                ConversationCommand::RevokeGrant(ConversationGrantRevokeRequest {
                    grant_id,
                    expected_revision: Revision::ZERO,
                }),
            )),
            command(ClientCommand::Conversation(ConversationCommand::Edit(
                ConversationEditRequest {
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    target_event_id: target_event_id.clone(),
                    replacement: "updated".into(),
                    operation_key: operation_key.clone(),
                },
            ))),
            command(ClientCommand::Conversation(ConversationCommand::Redact(
                ConversationRedactRequest {
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    target_event_id: target_event_id.clone(),
                    operation_key: operation_key.clone(),
                },
            ))),
            command(ClientCommand::Conversation(ConversationCommand::React(
                ConversationReactionRequest {
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    target_event_id: target_event_id.clone(),
                    reaction: "approved".into(),
                    remove: false,
                    operation_key: operation_key.clone(),
                },
            ))),
            command(ClientCommand::Conversation(ConversationCommand::SetPinned(
                ConversationPinRequest {
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    target_event_id: target_event_id.clone(),
                    pinned: true,
                    operation_key: operation_key.clone(),
                },
            ))),
            command(ClientCommand::Conversation(
                ConversationCommand::AppendThread(ConversationThreadRequest {
                    conversation_id: conversation_id.clone(),
                    expected_revision: Revision::ZERO,
                    parent_event_id: target_event_id.clone(),
                    content: "thread reply".into(),
                    artifacts: Vec::new(),
                    operation_key: operation_key.clone(),
                }),
            )),
            command(ClientCommand::Conversation(
                ConversationCommand::PromoteConversationArtifact(PromoteConversationArtifact {
                    artifact_id: artifact_id.clone(),
                    digest_sha256:
                        "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa".into(),
                    conversation_id: conversation_id.clone(),
                    expected_access_policy_revision: Revision::ZERO,
                    source_event_ids: vec![target_event_id.clone()],
                    operation_key,
                }),
            )),
            command(ClientCommand::Conversation(
                ConversationCommand::SearchSharedKnowledge(SharedKnowledgeSearchRequest {
                    space_id: EntityId::new(),
                    query: "release evidence".into(),
                    limit: 25,
                }),
            )),
        ];
        for message in messages {
            for format in [WireFormat::Json, WireFormat::Binary] {
                let encoded = encode(format, &message).unwrap();
                assert_eq!(decode(format, &encoded).unwrap(), message);
                let text = String::from_utf8(encode(WireFormat::Json, &message).unwrap()).unwrap();
                assert!(!text.contains("author"));
                assert!(!text.contains("actor"));
            }
        }

        let response = WireMessage::CommandResult(CommandResultEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            completed_at: UtcTimestamp::UNIX_EPOCH,
            result: CommandResult::Data(Box::new(ResponsePayload::ConversationMutation(Box::new(
                ConversationMutationReceipt {
                    conversation_id: Some(ConversationId::new()),
                    event_id: Some(EventId::new()),
                    grant_id: None,
                    revision: Some(Revision::new(1)),
                    status: ConversationMutationStatus::Applied,
                },
            )))),
        });
        for format in [WireFormat::Json, WireFormat::Binary] {
            let encoded = encode(format, &response).unwrap();
            assert_eq!(decode(format, &encoded).unwrap(), response);
        }

        let promotion_response = WireMessage::CommandResult(CommandResultEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            completed_at: UtcTimestamp::UNIX_EPOCH,
            result: CommandResult::Data(Box::new(ResponsePayload::ConversationArtifactPromotion(
                Box::new(ConversationArtifactPromotionReceipt {
                    artifact_id,
                    conversation_id,
                    access_policy_revision: Revision::new(1),
                    source_event_ids: vec![target_event_id],
                    status: ConversationMutationStatus::Replayed,
                }),
            ))),
        });
        for format in [WireFormat::Json, WireFormat::Binary] {
            let encoded = encode(format, &promotion_response).unwrap();
            assert_eq!(decode(format, &encoded).unwrap(), promotion_response);
        }

        let knowledge_response = WireMessage::CommandResult(CommandResultEnvelope {
            protocol: CURRENT_PROTOCOL_VERSION,
            command_id: CommandId::new(),
            completed_at: UtcTimestamp::UNIX_EPOCH,
            result: CommandResult::Data(Box::new(ResponsePayload::SharedKnowledgeSearch(vec![
                SharedKnowledgeSearchHit {
                    document_id: EntityId::new(),
                    source_path: "knowledge/release.md".into(),
                    source_version: "v2".into(),
                    heading_path: vec!["Release".into()],
                    excerpt: "signed evidence".into(),
                    source_kind: SharedKnowledgeSourceKind::Knowledge,
                    modified_at: UtcTimestamp::UNIX_EPOCH,
                    score: SharedKnowledgeSearchScore {
                        lexical_millionths: 800_000,
                        trigram_millionths: 700_000,
                        vector_millionths: Some(900_000),
                        merged_millionths: 820_000,
                    },
                    provenance: SharedKnowledgeAuthorizationProvenance {
                        owner_profile_id: ProfileId::new(),
                        space_id: EntityId::new(),
                        observed_permission_revision: Revision::new(2),
                        space_revision: Revision::new(4),
                        membership_permission_revision: Revision::new(4),
                        grant_id: GrantId::new(),
                        grant_revision: Revision::new(3),
                        resource_policy_revision: Revision::new(3),
                        source_conversation_id: Some(ConversationId::new()),
                        source_event_ids: vec![EventId::new()],
                        source_policy_revision: Revision::new(3),
                    },
                },
            ]))),
        });
        for format in [WireFormat::Json, WireFormat::Binary] {
            let encoded = encode(format, &knowledge_response).unwrap();
            assert_eq!(decode(format, &encoded).unwrap(), knowledge_response);
        }

        assert!(
            serde_json::from_value::<ConversationReadRequest>(serde_json::json!({
                "conversation_id": ConversationId::new(),
                "expected_revision": null,
                "read_through_sequence": 1,
                "actor": { "kind": "human" }
            }))
            .is_err()
        );
        let promotion = serde_json::json!({
            "artifact_id": ArtifactId::new(),
            "digest_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
            "conversation_id": ConversationId::new(),
            "expected_access_policy_revision": Revision::ZERO,
            "source_event_ids": [],
            "operation_key": "promote-1"
        });
        assert!(serde_json::from_value::<PromoteConversationArtifact>(promotion.clone()).is_err());
        let mut invalid_digest = promotion.clone();
        invalid_digest["source_event_ids"] = serde_json::json!([EventId::new()]);
        invalid_digest["digest_sha256"] = serde_json::json!("not-a-digest");
        assert!(serde_json::from_value::<PromoteConversationArtifact>(invalid_digest).is_err());
        let duplicate = EventId::new();
        let mut duplicate_sources = promotion.clone();
        duplicate_sources["source_event_ids"] = serde_json::json!([duplicate.clone(), duplicate]);
        assert!(serde_json::from_value::<PromoteConversationArtifact>(duplicate_sources).is_err());
        let mut forged = promotion;
        forged["source_event_ids"] = serde_json::json!([EventId::new()]);
        forged["actor"] = serde_json::json!({ "kind": "human" });
        assert!(serde_json::from_value::<PromoteConversationArtifact>(forged).is_err());
        for invalid in [
            serde_json::json!({ "query": "knowledge", "limit": 10 }),
            serde_json::json!({ "space_id": EntityId::new(), "query": "", "limit": 10 }),
            serde_json::json!({ "space_id": EntityId::new(), "query": "x".repeat(4_097), "limit": 10 }),
            serde_json::json!({ "space_id": EntityId::new(), "query": "knowledge", "limit": 0 }),
            serde_json::json!({ "space_id": EntityId::new(), "query": "knowledge", "limit": 101 }),
            serde_json::json!({ "space_id": EntityId::new(), "query": "knowledge", "limit": 10, "actor": "human" }),
        ] {
            assert!(serde_json::from_value::<SharedKnowledgeSearchRequest>(invalid).is_err());
        }
    }
}
#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct TeammatesProtocolVersion {
    pub major: u16,
    pub minor: u16,
}

pub const TEAMMATES_PROTOCOL_VERSION: TeammatesProtocolVersion =
    TeammatesProtocolVersion { major: 1, minor: 0 };
pub const TEAMMATES_PROTOCOL_PRODUCER: &str = "keith-agentd/conversation-authority/v1";

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct TeammatesProtocolFeatures {
    pub version: TeammatesProtocolVersion,
    pub roster: bool,
    pub canonical_conversations: bool,
    pub groups_and_rounds: bool,
    pub assignments_and_handoffs: bool,
    pub delivery_administration: bool,
    pub routines_channels_and_skills: bool,
    pub computers: bool,
    pub audit: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationResumeCursor {
    pub generation: u64,
    pub sequence: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationProtocolEnvelope {
    pub version: TeammatesProtocolVersion,
    pub subscription_id: keith_agent_types::EntityId,
    pub subject_profile_id: Option<keith_agent_types::ProfileId>,
    pub origin_server_instance_id: keith_agent_types::EntityId,
    pub authority_key: keith_agent_types::StableKey,
    pub producer: String,
    pub generation: u64,
    pub sequence: u64,
    pub event: ConversationProtocolEvent,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum ConversationProtocolEvent {
    Features(TeammatesProtocolFeatures),
    Receipt(TeammatesCommandReceipt),
    Snapshot(TeammatesSnapshot),
    Delta(TeammatesDelta),
    ResumeGap(ConversationResumeGap),
    Error(ConversationProtocolError),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum TeammatesReceiptStatus {
    Applied,
    Replayed,
    Accepted,
    Denied,
    DurableFailure,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct TeammatesCommandReceipt {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: Option<keith_agent_types::StableKey>,
    pub status: TeammatesReceiptStatus,
    pub conversation_id: Option<keith_agent_types::ConversationId>,
    pub event_id: Option<keith_agent_types::EventId>,
    pub profile_id: Option<keith_agent_types::ProfileId>,
    pub participant_session_id: Option<keith_agent_types::SessionId>,
    pub assignment_id: Option<keith_agent_types::EntityId>,
    pub delivery_id: Option<keith_agent_types::EntityId>,
    pub binding_id: Option<keith_agent_types::EntityId>,
    pub publication_id: Option<keith_agent_types::EntityId>,
    pub round_id: Option<keith_agent_types::EntityId>,
    pub skill_id: Option<keith_agent_types::EntityId>,
    pub routine_id: Option<keith_agent_types::EntityId>,
    pub computer_id: Option<keith_agent_types::EntityId>,
    pub resulting_revision: Option<keith_agent_types::Revision>,
    pub safe_reason: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "scope", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum TeammatesSnapshot {
    Roster(RosterSnapshot),
    Conversation(CanonicalConversationSnapshot),
    Coordination(CoordinationSnapshot),
    Routines(RoutineSnapshot),
    Computers(ComputerSnapshot),
    Audit(AuditSnapshot),
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum TeammatesDelta {
    RosterUpsert(RosterAgentProjection),
    RosterRemove {
        profile_id: keith_agent_types::ProfileId,
        revision: keith_agent_types::Revision,
    },
    ConversationUpsert(ConversationSummaryProjection),
    CanonicalEventAppended(CanonicalEventProjection),
    ParticipantChanged(TeammateParticipantProjection),
    ReadStateChanged(ConversationReadProjection),
    RoundChanged(RoundProjection),
    AssignmentChanged(AssignmentProjection),
    DeliveryChanged(TeammateDeliveryProjection),
    RoutineChanged(RoutineProjection),
    ComputerChanged(ComputerProjection),
    AuditAppended(AuditProjection),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum RosterLifecycleProjection {
    Draft,
    Enabled,
    Disabled,
    Archived,
    Deleted,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct RosterAgentProjection {
    pub profile_id: keith_agent_types::ProfileId,
    pub revision: keith_agent_types::Revision,
    pub lifecycle: RosterLifecycleProjection,
    pub name: String,
    pub role: String,
    pub avatar: Option<String>,
    pub hidden: bool,
    pub permanent_human_dm_id: Option<keith_agent_types::ConversationId>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct RosterSnapshot {
    pub roster_revision: keith_agent_types::Revision,
    pub agents: Vec<RosterAgentProjection>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum ProtocolPrincipal {
    Human,
    Agent {
        profile_id: keith_agent_types::ProfileId,
    },
    System,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ProtocolConversationKind {
    HumanAgentDm,
    AgentAgentDm,
    Group,
    Thread,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ProtocolConversationLifecycle {
    Active,
    Archived,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationSummaryProjection {
    pub conversation_id: keith_agent_types::ConversationId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub participant_session_id: Option<keith_agent_types::SessionId>,
    pub kind: ProtocolConversationKind,
    pub lifecycle: ProtocolConversationLifecycle,
    pub title: String,
    pub revision: keith_agent_types::Revision,
    pub participant_revision: keith_agent_types::Revision,
    pub head_sequence: u64,
    pub head_event_id: Option<keith_agent_types::EventId>,
    pub unread_count: u64,
    pub hidden: bool,
    pub muted: bool,
    pub pinned: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ProtocolParticipantRole {
    Owner,
    Member,
    Observer,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ProtocolEventKind {
    Message,
    Edit,
    Redaction,
    Reaction,
    MembershipChange,
    Pin,
    AssignmentChange,
    Handoff,
    RoutineResult,
    ComputerEvent,
    SystemNotice,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct CanonicalEventProjection {
    pub event_id: keith_agent_types::EventId,
    pub conversation_id: keith_agent_types::ConversationId,
    pub sequence: u64,
    pub publication_key: keith_agent_types::StableKey,
    pub author: ProtocolPrincipal,
    pub timestamp: keith_agent_types::UtcTimestamp,
    pub kind: ProtocolEventKind,
    pub effective_content: Option<String>,
    pub redacted: bool,
    pub artifact_ids: Vec<keith_agent_types::ArtifactId>,
    pub reply_to: Option<keith_agent_types::EventId>,
    pub thread_parent: Option<keith_agent_types::EventId>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct TeammateParticipantProjection {
    pub conversation_id: keith_agent_types::ConversationId,
    pub principal: ProtocolPrincipal,
    pub role: ProtocolParticipantRole,
    pub active: bool,
    pub revision: keith_agent_types::Revision,
    pub hidden: bool,
    pub muted: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationReadProjection {
    pub conversation_id: keith_agent_types::ConversationId,
    pub reader: ProtocolPrincipal,
    pub read_through_sequence: u64,
    pub unread_count: u64,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct CanonicalConversationSnapshot {
    pub summary: ConversationSummaryProjection,
    pub participants: Vec<TeammateParticipantProjection>,
    pub events: Vec<CanonicalEventProjection>,
    pub read_state: Option<ConversationReadProjection>,
    pub next_after_sequence: Option<u64>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum RoundStateProjection {
    Open,
    Active,
    Quiet,
    Blocked,
    Completed,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct RoundProjection {
    pub round_id: keith_agent_types::EntityId,
    pub conversation_id: keith_agent_types::ConversationId,
    pub trigger_event_id: keith_agent_types::EventId,
    pub state: RoundStateProjection,
    pub active_delivery_ids: Vec<keith_agent_types::EntityId>,
    pub depth_remaining: u32,
    pub turns_remaining: u32,
    pub terminal_reason: Option<String>,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum AssignmentStateProjection {
    Proposed,
    Ready,
    Claimed,
    Active,
    Blocked,
    Completed,
    Cancelled,
    Transferred,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct AssignmentProjection {
    pub assignment_id: keith_agent_types::EntityId,
    pub conversation_id: keith_agent_types::ConversationId,
    pub objective: String,
    pub owner_profile_id: keith_agent_types::ProfileId,
    pub state: AssignmentStateProjection,
    pub dependency_ids: Vec<keith_agent_types::EntityId>,
    pub priority: u8,
    pub due_at: Option<keith_agent_types::UtcTimestamp>,
    pub source_event_id: keith_agent_types::EventId,
    pub result_event_id: Option<keith_agent_types::EventId>,
    pub blocked_reason: Option<String>,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum DeliveryStateProjection {
    Pending,
    Claimed,
    Finalized,
    Published,
    RetryScheduled,
    DeadLetter,
    Cancelled,
    Superseded,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct TeammateDeliveryProjection {
    pub delivery_id: keith_agent_types::EntityId,
    pub conversation_id: keith_agent_types::ConversationId,
    pub source_event_id: keith_agent_types::EventId,
    pub source_profile_id: keith_agent_types::ProfileId,
    pub destination_profile_id: keith_agent_types::ProfileId,
    pub state: DeliveryStateProjection,
    pub attempt_count: u32,
    pub retry_at: Option<keith_agent_types::UtcTimestamp>,
    pub safe_error: Option<String>,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct CoordinationSnapshot {
    pub rounds: Vec<RoundProjection>,
    pub assignments: Vec<AssignmentProjection>,
    pub deliveries: Vec<TeammateDeliveryProjection>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum RoutineStateProjection {
    Enabled,
    Paused,
    Disabled,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct RoutineProjection {
    pub routine_id: keith_agent_types::EntityId,
    pub owner_profile_id: keith_agent_types::ProfileId,
    pub destination_conversation_id: keith_agent_types::ConversationId,
    pub state: RoutineStateProjection,
    pub next_run_at: Option<keith_agent_types::UtcTimestamp>,
    pub last_run_id: Option<keith_agent_types::EntityId>,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct RoutineSnapshot {
    pub routines: Vec<RoutineProjection>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerStateProjection {
    Stopped,
    Starting,
    Ready,
    Busy,
    Takeover,
    Quarantined,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerProjection {
    pub computer_id: keith_agent_types::EntityId,
    pub profile_id: keith_agent_types::ProfileId,
    pub state: ComputerStateProjection,
    pub active_task_id: Option<keith_agent_types::EntityId>,
    pub takeover_lease_id: Option<keith_agent_types::EntityId>,
    pub revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerSnapshot {
    pub computers: Vec<ComputerProjection>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct AuditProjection {
    pub audit_id: keith_agent_types::AuditId,
    pub actor: ProtocolPrincipal,
    pub action: String,
    pub conversation_id: Option<keith_agent_types::ConversationId>,
    pub event_id: Option<keith_agent_types::EventId>,
    pub correlation_key: String,
    pub occurred_at: keith_agent_types::UtcTimestamp,
    pub outcome: String,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct AuditSnapshot {
    pub records: Vec<AuditProjection>,
    pub next_after_audit_id: Option<keith_agent_types::AuditId>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationResumeGap {
    pub requested: ConversationResumeCursor,
    pub current_generation: u64,
    pub oldest_available_sequence: u64,
    pub replacement_snapshot_required: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ConversationProtocolErrorCode {
    UnsupportedVersion,
    Unauthorized,
    NotFound,
    Conflict,
    StaleRevision,
    InvalidCommand,
    ResumeGap,
    ResourceExhausted,
    DurableFailure,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationProtocolError {
    pub request_id: Option<keith_agent_types::EntityId>,
    pub code: ConversationProtocolErrorCode,
    pub safe_message: String,
    pub retryable: bool,
    pub current_revision: Option<keith_agent_types::Revision>,
    pub replacement_cursor: Option<ConversationResumeCursor>,
}
#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationListRequest {
    pub include_archived: bool,
    pub after_conversation_id: Option<keith_agent_types::ConversationId>,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ProfileConversationListRequest {
    pub profile_id: keith_agent_types::ProfileId,
    pub expected_profile_revision: keith_agent_types::Revision,
    pub include_archived: bool,
    pub after_conversation_id: Option<keith_agent_types::ConversationId>,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ProfileConversationListsRequest {
    pub profiles: Vec<ProfileConversationListRequest>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ConversationListResponse {
    pub profile_id: Option<keith_agent_types::ProfileId>,
    pub roster_revision: keith_agent_types::Revision,
    pub conversations: Vec<ConversationSummaryProjection>,
    pub next_after_conversation_id: Option<keith_agent_types::ConversationId>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "action", content = "parameters", rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum TeammatesCommand {
    PeerMessage(PeerMessageCommand),
    CreateGroup(CreateGroupCommand),
    UpdateGroupMentionPolicy(UpdateGroupMentionPolicyCommand),
    StartRound(StartRoundCommand),
    CreateAssignment(CreateAssignmentCommand),
    HandoffWork(HandoffWorkCommand),
    ReportAssignment(ReportAssignmentCommand),
    DeliveryAdmin(DeliveryAdminCommand),
    ResolveChannelBinding(ChannelBindingResolveCommand),
    AppendChannelMessage(ChannelMessageCommand),
    PublicationAdmin(PublicationAdminCommand),
    Routine(RoutineCommand),
    Computer(ComputerCommand),
    Audit(AuditCommand),
    Resume(ResumeConversationEventsCommand),
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct PeerMessageCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub conversation_id: keith_agent_types::ConversationId,
    pub sender_profile_id: keith_agent_types::ProfileId,
    pub recipient_profile_id: keith_agent_types::ProfileId,
    pub participant_session_id: keith_agent_types::SessionId,
    pub expected_conversation_revision: keith_agent_types::Revision,
    pub expected_policy_revision: keith_agent_types::Revision,
    pub content: String,
    pub deadline: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct CreateGroupCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub title: String,
    pub initial_profile_ids: Vec<keith_agent_types::ProfileId>,
    pub mention_mode: GroupMentionModeCommand,
    pub now: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct UpdateGroupMentionPolicyCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub conversation_id: keith_agent_types::ConversationId,
    pub expected_revision: keith_agent_types::Revision,
    pub mention_mode: GroupMentionModeCommand,
    pub allow_human_trigger: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum GroupMentionModeCommand {
    AllActive,
    ExplicitOnly,
    ExplicitOrOwners,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct StartRoundCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub conversation_id: keith_agent_types::ConversationId,
    pub trigger_event_id: keith_agent_types::EventId,
    pub explicit_mentions: Vec<keith_agent_types::ProfileId>,
    pub max_depth: u32,
    pub max_turns: u32,
    pub expected_conversation_revision: keith_agent_types::Revision,
    pub expected_policy_revision: keith_agent_types::Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct CreateAssignmentCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub assignment_id: keith_agent_types::EntityId,
    pub conversation_id: keith_agent_types::ConversationId,
    pub objective: String,
    pub owner_profile_id: keith_agent_types::ProfileId,
    pub dependency_ids: Vec<keith_agent_types::EntityId>,
    pub priority: u8,
    pub due_at: Option<keith_agent_types::UtcTimestamp>,
    pub source_event_id: keith_agent_types::EventId,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct HandoffWorkCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub assignment_id: keith_agent_types::EntityId,
    pub expected_assignment_revision: keith_agent_types::Revision,
    pub new_owner_profile_id: keith_agent_types::ProfileId,
    pub reason: String,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ReportAssignmentCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub assignment_id: keith_agent_types::EntityId,
    pub expected_assignment_revision: keith_agent_types::Revision,
    pub state: AssignmentStateProjection,
    pub summary: String,
    pub result_event_id: Option<keith_agent_types::EventId>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum DeliveryAdminAction {
    Retry,
    Cancel,
    DeadLetter,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct DeliveryAdminCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub delivery_id: keith_agent_types::EntityId,
    pub expected_revision: keith_agent_types::Revision,
    pub action: DeliveryAdminAction,
    pub safe_reason: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ChannelBindingResolveCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub adapter: String,
    pub external_channel_id: String,
    pub external_subject_id: String,
    pub authenticated_profile_id: keith_agent_types::ProfileId,
    pub expected_binding_revision: Option<keith_agent_types::Revision>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ChannelMessageCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub binding_id: keith_agent_types::EntityId,
    pub expected_binding_revision: keith_agent_types::Revision,
    pub conversation_id: keith_agent_types::ConversationId,
    pub participant_profile_id: keith_agent_types::ProfileId,
    pub external_message_id: String,
    pub content: String,
    pub artifact_ids: Vec<keith_agent_types::ArtifactId>,
    pub received_at: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum PublicationAdminAction {
    Retry,
    Cancel,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct PublicationAdminCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub publication_id: keith_agent_types::EntityId,
    pub expected_revision: keith_agent_types::Revision,
    pub action: PublicationAdminAction,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum RoutineAction {
    Create,
    Edit,
    TestRun,
    Enable,
    Pause,
    Resume,
    RunNow,
    History,
    Delete,
    SaveAsSkill,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum RoutineTriggerCommand {
    Schedule {
        expression: String,
        time_zone: String,
    },
    Event {
        source_kind: String,
        stable_source_key: keith_agent_types::StableKey,
        max_recursion_depth: u32,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct RoutineCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub action: RoutineAction,
    pub routine_id: Option<keith_agent_types::EntityId>,
    pub owner_profile_id: keith_agent_types::ProfileId,
    pub destination_conversation_id: keith_agent_types::ConversationId,
    pub expected_revision: Option<keith_agent_types::Revision>,
    pub trigger: Option<RoutineTriggerCommand>,
    pub bounded_input: Option<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerAction {
    Start,
    Stop,
    AcquireTask,
    ReleaseTask,
    AcquireTakeover,
    RenewTakeover,
    HandBack,
    Input,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub action: ComputerAction,
    pub computer_id: keith_agent_types::EntityId,
    pub profile_id: keith_agent_types::ProfileId,
    pub expected_revision: Option<keith_agent_types::Revision>,
    pub lease_token: Option<keith_agent_types::StableKey>,
    pub bounded_input: Option<String>,
}

pub const COMPUTER_PROTOCOL_VERSION: u16 = 1;
pub const COMPUTER_PROTOCOL_PRODUCER: &str = "keith-agentd/computer-authority/v1";
pub const COMPUTER_PROTOCOL_MAX_FRAME_BYTES: usize = 4 * 1_024 * 1_024;
pub const COMPUTER_PROTOCOL_MAX_INPUT_BYTES: usize = 64 * 1_024;
pub const COMPUTER_PROTOCOL_MAX_WIDTH: u32 = 7_680;
pub const COMPUTER_PROTOCOL_MAX_HEIGHT: u32 = 4_320;

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "action", content = "parameters", rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerProtocolCommand {
    Open(ComputerStreamOpenCommand),
    Input(ComputerStreamInputCommand),
    AcquireTakeover(ComputerTakeoverAcquireCommand),
    RenewTakeover(ComputerTakeoverRenewCommand),
    HandBack(ComputerTakeoverHandBackCommand),
    InjectSecret(ComputerSecretInjectionCommand),
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamSubjectProjection {
    pub profile_id: keith_agent_types::ProfileId,
    pub computer_id: keith_agent_types::ComputerId,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamOriginProjection {
    pub server_instance_id: keith_agent_types::EntityId,
    pub stream_instance_id: keith_agent_types::EntityId,
    pub authority_key: keith_agent_types::StableKey,
    pub generation: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamCursorProjection {
    pub generation: u64,
    pub sequence: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamResumeProjection {
    pub origin: ComputerStreamOriginProjection,
    pub cursor: ComputerStreamCursorProjection,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamLimitsProjection {
    pub max_frame_bytes: usize,
    pub max_input_bytes: usize,
    pub max_width: u32,
    pub max_height: u32,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "authority", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum ComputerStreamControllerProjection {
    Agent {
        profile_id: keith_agent_types::ProfileId,
        task_key: keith_agent_types::StableKey,
        fencing_token: u64,
    },
    Routine {
        profile_id: keith_agent_types::ProfileId,
        routine_id: keith_agent_types::EntityId,
        task_key: keith_agent_types::StableKey,
        fencing_token: u64,
    },
    Child {
        profile_id: keith_agent_types::ProfileId,
        child_id: keith_agent_types::ChildId,
        task_key: keith_agent_types::StableKey,
        fencing_token: u64,
    },
    UserTakeover {
        profile_id: keith_agent_types::ProfileId,
        lease_id: keith_agent_types::TakeoverLeaseId,
        task_key: keith_agent_types::StableKey,
        fencing_token: u64,
        lease_revision: keith_agent_types::Revision,
    },
}

impl ComputerStreamControllerProjection {
    pub fn profile_id(&self) -> &keith_agent_types::ProfileId {
        match self {
            Self::Agent { profile_id, .. }
            | Self::Routine { profile_id, .. }
            | Self::Child { profile_id, .. }
            | Self::UserTakeover { profile_id, .. } => profile_id,
        }
    }

    pub fn task_key(&self) -> &keith_agent_types::StableKey {
        match self {
            Self::Agent { task_key, .. }
            | Self::Routine { task_key, .. }
            | Self::Child { task_key, .. }
            | Self::UserTakeover { task_key, .. } => task_key,
        }
    }

    pub fn fencing_token(&self) -> u64 {
        match self {
            Self::Agent { fencing_token, .. }
            | Self::Routine { fencing_token, .. }
            | Self::Child { fencing_token, .. }
            | Self::UserTakeover { fencing_token, .. } => *fencing_token,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamOpenCommand {
    pub request_id: keith_agent_types::EntityId,
    pub profile_id: keith_agent_types::ProfileId,
    pub computer_id: keith_agent_types::ComputerId,
    pub resume: Option<ComputerStreamResumeProjection>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamDescriptorProjection {
    pub session_id: keith_agent_types::EntityId,
    pub subject: ComputerStreamSubjectProjection,
    pub computer_revision: keith_agent_types::Revision,
    pub origin: ComputerStreamOriginProjection,
    pub cursor: ComputerStreamCursorProjection,
    pub controller: ComputerStreamControllerProjection,
    pub takeover_lease_id: Option<keith_agent_types::TakeoverLeaseId>,
    pub limits: ComputerStreamLimitsProjection,
    pub connected_at: keith_agent_types::UtcTimestamp,
    pub liveness_deadline: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerFrameEncodingProjection {
    Png,
    Jpeg,
    WebP,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerPointerButtonProjection {
    Primary,
    Middle,
    Secondary,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerButtonStateProjection {
    Pressed,
    Released,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum ComputerInputPayloadProjection {
    PointerMove {
        x: u32,
        y: u32,
    },
    PointerButton {
        x: u32,
        y: u32,
        button: ComputerPointerButtonProjection,
        state: ComputerButtonStateProjection,
    },
    Scroll {
        delta_x: i32,
        delta_y: i32,
    },
    Key {
        code: String,
        state: ComputerButtonStateProjection,
        alt: bool,
        control: bool,
        meta: bool,
        shift: bool,
    },
    Text {
        text: String,
    },
    CredentialReference {
        grant_id: keith_agent_types::GrantId,
    },
    Focus,
    ReleaseAll,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamInputCommand {
    pub request_id: keith_agent_types::EntityId,
    pub session_id: keith_agent_types::EntityId,
    pub subject: ComputerStreamSubjectProjection,
    pub origin: ComputerStreamOriginProjection,
    pub sequence: u64,
    pub expected_computer_revision: keith_agent_types::Revision,
    pub controller: ComputerStreamControllerProjection,
    pub payload: ComputerInputPayloadProjection,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerStreamInputReceiptProjection {
    pub request_id: keith_agent_types::EntityId,
    pub sequence: u64,
    pub computer_revision: keith_agent_types::Revision,
    pub takeover_lease_id: Option<keith_agent_types::TakeoverLeaseId>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerTakeoverClaimProjection {
    pub takeover_lease_id: keith_agent_types::TakeoverLeaseId,
    pub computer_id: keith_agent_types::ComputerId,
    pub owner_profile_id: keith_agent_types::ProfileId,
    pub task_key: keith_agent_types::StableKey,
    pub fencing_token: u64,
    pub lease_revision: keith_agent_types::Revision,
    pub expires_at: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerTakeoverAcquireCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub session_id: keith_agent_types::EntityId,
    pub subject: ComputerStreamSubjectProjection,
    pub origin: ComputerStreamOriginProjection,
    pub expected_computer_revision: keith_agent_types::Revision,
    pub controller: ComputerStreamControllerProjection,
    pub task_key: keith_agent_types::StableKey,
    pub lease_millis: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerTakeoverRenewCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub session_id: keith_agent_types::EntityId,
    pub subject: ComputerStreamSubjectProjection,
    pub origin: ComputerStreamOriginProjection,
    pub expected_computer_revision: keith_agent_types::Revision,
    pub controller: ComputerStreamControllerProjection,
    pub claim: ComputerTakeoverClaimProjection,
    pub presented_token: keith_agent_types::StableKey,
    pub lease_millis: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerTakeoverHandBackCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub session_id: keith_agent_types::EntityId,
    pub subject: ComputerStreamSubjectProjection,
    pub origin: ComputerStreamOriginProjection,
    pub expected_computer_revision: keith_agent_types::Revision,
    pub controller: ComputerStreamControllerProjection,
    pub claim: ComputerTakeoverClaimProjection,
    pub presented_token: keith_agent_types::StableKey,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(
    tag = "owner",
    content = "id",
    rename_all = "snake_case",
    deny_unknown_fields
)]
#[derive(schemars::JsonSchema)]
pub enum ComputerCredentialOwnerProjection {
    Provider(String),
    Channel(String),
    Mcp(String),
    Tool(String),
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerCredentialReferenceProjection {
    pub name: String,
    pub owner: ComputerCredentialOwnerProjection,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub enum ComputerSecretInjectionTargetProjection {
    FocusedField {
        exact_origin: String,
        frame_origin: String,
        field_id: String,
        expected_focus_revision: keith_agent_types::Revision,
    },
    CredentialBroker {
        exact_origin: String,
        broker_id: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerSecretInjectionCommand {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub session_id: keith_agent_types::EntityId,
    pub subject: ComputerStreamSubjectProjection,
    pub origin: ComputerStreamOriginProjection,
    pub expected_computer_revision: keith_agent_types::Revision,
    pub expected_policy_revision: keith_agent_types::Revision,
    pub controller: ComputerStreamControllerProjection,
    pub task_key: keith_agent_types::StableKey,
    pub task_fencing_token: u64,
    pub credential_ref: ComputerCredentialReferenceProjection,
    pub target: ComputerSecretInjectionTargetProjection,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerTakeoverReceiptProjection {
    pub request_id: keith_agent_types::EntityId,
    pub claim: ComputerTakeoverClaimProjection,
    pub replacement_token: keith_agent_types::StableKey,
    pub computer_revision: keith_agent_types::Revision,
    pub descriptor: ComputerStreamDescriptorProjection,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerHandBackDispositionProjection {
    Resumed,
    OwningTaskFailed,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerHandBackReceiptProjection {
    pub request_id: keith_agent_types::EntityId,
    pub claim: ComputerTakeoverClaimProjection,
    pub disposition: ComputerHandBackDispositionProjection,
    pub refreshed_observation_key: Option<keith_agent_types::StableKey>,
    pub safe_reason: Option<String>,
    pub computer_revision: keith_agent_types::Revision,
    pub descriptor: ComputerStreamDescriptorProjection,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerSecretInjectionTargetKindProjection {
    FocusedField,
    CredentialBroker,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerSecretInjectionReceiptProjection {
    pub request_id: keith_agent_types::EntityId,
    pub operation_key: keith_agent_types::StableKey,
    pub profile_id: keith_agent_types::ProfileId,
    pub computer_id: keith_agent_types::ComputerId,
    pub task_key: keith_agent_types::StableKey,
    pub task_fencing_token: u64,
    pub computer_revision: keith_agent_types::Revision,
    pub policy_revision: keith_agent_types::Revision,
    pub target_kind: ComputerSecretInjectionTargetKindProjection,
    pub injected_at: keith_agent_types::UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(
    tag = "kind",
    content = "value",
    rename_all = "snake_case",
    deny_unknown_fields
)]
#[derive(schemars::JsonSchema)]
pub enum ComputerProtocolResponse {
    Opened(ComputerStreamDescriptorProjection),
    InputApplied(ComputerStreamInputReceiptProjection),
    TakeoverAcquired(ComputerTakeoverReceiptProjection),
    TakeoverRenewed(ComputerTakeoverReceiptProjection),
    HandedBack(ComputerHandBackReceiptProjection),
    SecretInjected(ComputerSecretInjectionReceiptProjection),
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerFrameProjection {
    pub captured_at: keith_agent_types::UtcTimestamp,
    pub width: u32,
    pub height: u32,
    pub encoding: ComputerFrameEncodingProjection,
    pub key_frame: bool,
    pub bytes_base64: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
#[derive(schemars::JsonSchema)]
pub enum ComputerProtocolErrorCode {
    Unauthorized,
    ScopeChanged,
    OriginChanged,
    GenerationChanged,
    InvalidCursor,
    StaleRevision,
    StaleController,
    Expired,
    BoundsExceeded,
    SequenceGap,
    PolicyDenied,
    Unavailable,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerProtocolErrorProjection {
    pub code: ComputerProtocolErrorCode,
    pub safe_reason: String,
    pub reconnectable: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(
    tag = "type",
    content = "value",
    rename_all = "snake_case",
    deny_unknown_fields
)]
#[derive(schemars::JsonSchema)]
pub enum ComputerProtocolEvent {
    KeyFrame(ComputerFrameProjection),
    Frame(ComputerFrameProjection),
    ControlChanged(ComputerStreamDescriptorProjection),
    Error(ComputerProtocolErrorProjection),
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ComputerProtocolEventEnvelope {
    pub version: u16,
    pub producer: String,
    pub session_id: keith_agent_types::EntityId,
    pub subject: ComputerStreamSubjectProjection,
    pub origin: ComputerStreamOriginProjection,
    pub computer_revision: keith_agent_types::Revision,
    pub controller: ComputerStreamControllerProjection,
    pub sequence: u64,
    pub event: ComputerProtocolEvent,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct AuditCommand {
    pub request_id: keith_agent_types::EntityId,
    pub profile_id: Option<keith_agent_types::ProfileId>,
    pub conversation_id: Option<keith_agent_types::ConversationId>,
    pub after_audit_id: Option<keith_agent_types::AuditId>,
    pub limit: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
#[derive(schemars::JsonSchema)]
pub struct ResumeConversationEventsCommand {
    pub request_id: keith_agent_types::EntityId,
    pub cursor: ConversationResumeCursor,
    pub profile_id: Option<keith_agent_types::ProfileId>,
    pub conversation_id: Option<keith_agent_types::ConversationId>,
}
