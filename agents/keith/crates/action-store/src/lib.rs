#![forbid(unsafe_code)]

use std::collections::BTreeMap;

use keith_agent_types::{
    AssignmentId, ConversationId, DeliveryId, EventId, GrantId, ProfileId, RoundId, StableKey,
    WorkerId,
};

use std::cmp::Ordering;
use std::fmt::Display;
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    ActionId, ArtifactId, CURRENT_SCHEMA_VERSION, ChildId, ClientId, EntityId, GoalId, JobId,
    MessageId, Revision, SchemaVersion, SessionId, UtcTimestamp,
};
use keith_state_store_core::{ActionRepository, VersionedRecord, WritePrecondition};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "source")]
pub enum ActionSource {
    Interactive {
        client_id: ClientId,
    },
    Channel {
        channel: String,
        message_id: String,
    },
    Schedule {
        job_id: JobId,
        attempt: u32,
    },
    Child {
        child_id: ChildId,
        message_id: MessageId,
    },
    Steering {
        client_id: ClientId,
    },
    FollowUp,
    Waiting {
        wake_id: EntityId,
    },
    Awareness {
        event_id: EntityId,
    },
    Refinement {
        transaction_id: EntityId,
    },
    AutonomousContinuation {
        goal_id: GoalId,
    },
    PeerMessage {
        binding: RecipientActionBinding,
    },
    Coordination {
        binding: CoordinationActionBinding,
    },
    Evolution {
        generation_id: EntityId,
        ancestry: Vec<ActionAncestorKind>,
        execution: EvolutionExecution,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum CoordinationActionBinding {
    GroupRoundDelivery {
        stable_key: StableKey,
        delivery_id: DeliveryId,
        round_id: RoundId,
        round_revision: Revision,
        mention_policy: GroupMentionPolicy,
        conversation_id: ConversationId,
        source_event_id: EventId,
        destination_profile_id: ProfileId,
        participant_session_id: SessionId,
        policy_snapshot: RecipientPolicySnapshot,
        context_cursor: CanonicalConversationContextCursor,
    },
    AssignmentWork {
        stable_key: StableKey,
        delivery_id: DeliveryId,
        assignment_id: AssignmentId,
        assignment_revision: Revision,
        conversation_id: ConversationId,
        source_event_id: EventId,
        owner_profile_id: ProfileId,
        participant_session_id: SessionId,
        policy_snapshot: RecipientPolicySnapshot,
        context_cursor: CanonicalConversationContextCursor,
    },
    OwnershipTransferWake {
        stable_key: StableKey,
        new_owner_delivery_id: DeliveryId,
        ownership_transfer_id: EntityId,
        assignment_id: AssignmentId,
        expected_assignment_revision: Revision,
        new_assignment_revision: Revision,
        conversation_id: ConversationId,
        source_event_id: EventId,
        previous_owner_profile_id: ProfileId,
        new_owner_profile_id: ProfileId,
        participant_session_id: SessionId,
        obsolete_delivery_ids: Vec<DeliveryId>,
        policy_snapshot: RecipientPolicySnapshot,
        context_cursor: CanonicalConversationContextCursor,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GroupMentionPolicy {
    ExplicitOnly,
    AllParticipants,
    CoordinatorSelected,
}

impl CoordinationActionBinding {
    pub const fn stable_key(&self) -> &StableKey {
        match self {
            Self::GroupRoundDelivery { stable_key, .. }
            | Self::AssignmentWork { stable_key, .. }
            | Self::OwnershipTransferWake { stable_key, .. } => stable_key,
        }
    }

    pub const fn destination_session_id(&self) -> &SessionId {
        match self {
            Self::GroupRoundDelivery {
                participant_session_id,
                ..
            }
            | Self::AssignmentWork {
                participant_session_id,
                ..
            }
            | Self::OwnershipTransferWake {
                participant_session_id,
                ..
            } => participant_session_id,
        }
    }

    pub const fn conversation_id(&self) -> &ConversationId {
        match self {
            Self::GroupRoundDelivery {
                conversation_id, ..
            }
            | Self::AssignmentWork {
                conversation_id, ..
            }
            | Self::OwnershipTransferWake {
                conversation_id, ..
            } => conversation_id,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CoordinationActionEnqueue {
    pub action_id: ActionId,
    pub binding: CoordinationActionBinding,
    pub instruction: String,
    pub deadline: Option<UtcTimestamp>,
    pub limits: ActionLimits,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CoordinationActionDisposition {
    Accepted,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CoordinationActionReceipt {
    pub action_id: ActionId,
    pub stable_key: StableKey,
    pub accepted_at: UtcTimestamp,
    pub disposition: CoordinationActionDisposition,
}

pub const MAX_RECIPIENT_POLICY_GRANTS: usize = 256;
pub const MAX_PEER_MESSAGE_BYTES: usize = 64 * 1024;
pub const MAX_PEER_ATTACHMENTS: usize = 64;
pub const MAX_DEAD_LETTER_REASON_BYTES: usize = 1_024;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipientPolicySnapshot {
    pub conversation_revision: Revision,
    pub participant_revision: Revision,
    pub relevant_grant_revisions: BTreeMap<GrantId, Revision>,
    pub policy_digest_sha256: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalConversationContextCursor {
    pub applied_through_sequence: u64,
    pub source_event_sequence: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipientActionBinding {
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub sender_profile_id: ProfileId,
    pub destination_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub publication_key: StableKey,
    pub policy_snapshot: RecipientPolicySnapshot,
    pub context_cursor: CanonicalConversationContextCursor,
}

impl RecipientActionBinding {
    /// Returns the canonical publication key for one source event and exact recipient.
    ///
    /// # Errors
    ///
    /// Returns an error only if the canonical components exceed the stable-key bound.
    pub fn canonical_publication_key(
        conversation_id: &ConversationId,
        source_event_id: &EventId,
        destination_profile_id: &ProfileId,
    ) -> Result<StableKey, ActionStoreError> {
        StableKey::parse(format!(
            "peer:{conversation_id}:{source_event_id}:{destination_profile_id}"
        ))
        .map_err(|error| ActionStoreError::Invalid(error.to_string()))
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerMessageEnqueue {
    pub action_id: ActionId,
    pub binding: RecipientActionBinding,
    pub content: String,
    pub attachments: Vec<ArtifactId>,
    pub deadline: Option<UtcTimestamp>,
    pub limits: ActionLimits,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerMessageDisposition {
    Accepted,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerMessageReceipt {
    pub action_id: ActionId,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub publication_key: StableKey,
    pub sender_profile_id: ProfileId,
    pub destination_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub accepted_at: UtcTimestamp,
    pub disposition: PeerMessageDisposition,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum InternalDeliveryState {
    Pending,
    Claimed,
    Delivered,
    DeadLettered,
    Superseded,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InternalDeliveryClaim {
    pub token: EntityId,
    pub owner: WorkerId,
    pub fence: u64,
    pub attempt: u32,
    pub claimed_at: UtcTimestamp,
    pub expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InternalDeliveryProjection {
    pub state: InternalDeliveryState,
    pub attempt_count: u32,
    pub fence: u64,
    pub claim: Option<InternalDeliveryClaim>,
    pub dead_letter_reason: Option<String>,
    pub superseded_by: Option<ActionId>,
    pub delivered_at: Option<UtcTimestamp>,
}

impl Default for InternalDeliveryProjection {
    fn default() -> Self {
        Self {
            state: InternalDeliveryState::Pending,
            attempt_count: 0,
            fence: 0,
            claim: None,
            dead_letter_reason: None,
            superseded_by: None,
            delivered_at: None,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionAncestorKind {
    Ordinary,
    Evolution,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionExecution {
    OrdinarySession,
    DedicatedChild,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum EvolutionOperation {
    EvaluateHypothesis,
    PrepareShadow,
    BuildCandidate,
    RunCanary,
    ObservePromotion,
    ReclaimResources,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeliveryPolicy {
    Immediate,
    NextTurnBoundary,
    WhenIdle,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionPriority {
    Interrupt,
    User,
    ChildResult,
    Scheduled,
    Background,
}

impl ActionPriority {
    const fn rank(self) -> u8 {
        match self {
            Self::Interrupt => 0,
            Self::User => 1,
            Self::ChildResult => 2,
            Self::Scheduled => 3,
            Self::Background => 4,
        }
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActionLimits {
    pub max_turns: Option<u32>,
    pub max_tokens: Option<u64>,
    pub max_elapsed_ms: Option<u64>,
    pub max_tool_calls: Option<u32>,
    pub max_children: Option<u16>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "route")]
pub enum ReplyRoute {
    Client {
        client_id: ClientId,
    },
    Channel {
        channel: String,
        external_account: Option<String>,
        conversation_id: String,
        thread_id: Option<String>,
        reply_to_message: Option<String>,
    },
    Session {
        session_id: SessionId,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "payload")]
pub enum ActionPayload {
    Prompt {
        text: String,
    },
    ChannelMessage {
        text: String,
        attachments: Vec<ArtifactId>,
    },
    Scheduled {
        instruction: String,
    },
    ChildMessage {
        text: String,
        artifacts: Vec<ArtifactId>,
    },
    Steering {
        text: String,
    },
    FollowUp {
        text: String,
    },
    ResumeWaiting {
        waiting_id: EntityId,
    },
    ContinueGoal {
        goal_id: GoalId,
    },
    PeerMessage {
        content: String,
        attachments: Vec<ArtifactId>,
    },
    Coordination {
        instruction: String,
    },
    Awareness {
        event_id: EntityId,
        summary: String,
    },
    Refinement {
        transaction_id: EntityId,
    },
    SystemMaintenance {
        operation: String,
    },
    Evolution {
        operation: EvolutionOperation,
    },
}

impl ActionPayload {
    fn text(&self) -> Option<&str> {
        match self {
            Self::Prompt { text }
            | Self::ChannelMessage { text, .. }
            | Self::ChildMessage { text, .. }
            | Self::PeerMessage { content: text, .. }
            | Self::Coordination { instruction: text }
            | Self::Steering { text }
            | Self::FollowUp { text } => Some(text),
            Self::Scheduled { instruction } => Some(instruction),
            Self::Awareness { summary, .. } => Some(summary),
            Self::ResumeWaiting { .. } | Self::ContinueGoal { .. } | Self::Refinement { .. } => {
                None
            }
            Self::SystemMaintenance { operation } => Some(operation),
            Self::Evolution { .. } => None,
        }
    }

    const fn is_steering(&self) -> bool {
        matches!(self, Self::Steering { .. })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SessionAction {
    pub id: ActionId,
    pub session_id: SessionId,
    pub source: ActionSource,
    pub delivery: DeliveryPolicy,
    pub priority: ActionPriority,
    pub created_at: UtcTimestamp,
    pub not_before: Option<UtcTimestamp>,
    pub deadline: Option<UtcTimestamp>,
    pub limits: ActionLimits,
    pub reply_route: Option<ReplyRoute>,
    pub payload: ActionPayload,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionState {
    Queued,
    Admitted,
    Running,
    Waiting,
    Completed,
    Failed,
    Cancelled,
    Expired,
}

impl ActionState {
    pub const fn is_terminal(self) -> bool {
        matches!(
            self,
            Self::Completed | Self::Failed | Self::Cancelled | Self::Expired
        )
    }

    const fn allows(self, next: Self) -> bool {
        match self {
            Self::Queued => matches!(next, Self::Admitted | Self::Cancelled | Self::Expired),
            Self::Admitted => matches!(
                next,
                Self::Running | Self::Completed | Self::Cancelled | Self::Expired
            ),
            Self::Running => matches!(
                next,
                Self::Waiting | Self::Completed | Self::Failed | Self::Cancelled
            ),
            Self::Waiting => matches!(
                next,
                Self::Admitted | Self::Completed | Self::Failed | Self::Cancelled | Self::Expired
            ),
            Self::Completed | Self::Failed | Self::Cancelled | Self::Expired => false,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActionRecord {
    pub version: SchemaVersion,
    pub action: SessionAction,
    pub state: ActionState,
    pub enqueue_sequence: u64,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
    pub terminal_detail: Option<String>,
    #[serde(default)]
    pub peer_receipt: Option<PeerMessageReceipt>,
    #[serde(default)]
    pub internal_delivery: Option<InternalDeliveryProjection>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ActionInboxConfig {
    pub max_queued_per_session: usize,
    pub max_background_queued_per_session: usize,
}

impl Default for ActionInboxConfig {
    fn default() -> Self {
        Self {
            max_queued_per_session: 1_024,
            max_background_queued_per_session: 128,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct PumpContext {
    pub active_action: Option<ActionId>,
    pub at_turn_boundary: bool,
    pub session_idle: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AdmissionKind {
    StartTurn,
    ApplySteering,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SelectedAction {
    pub record: ActionRecord,
    pub kind: AdmissionKind,
}

#[derive(Debug, Error)]
pub enum ActionStoreError {
    #[error("action repository failed: {0}")]
    Repository(String),
    #[error("action {0} was not found")]
    NotFound(ActionId),
    #[error("action record is corrupt: {0}")]
    Corrupt(String),
    #[error("action is invalid: {0}")]
    Invalid(String),
    #[error("session action queue reached its configured limit")]
    QueueFull,
    #[error("session background queue reached its configured limit")]
    BackgroundQueueFull,
    #[error("evolution actions must use background priority and when-idle delivery")]
    EvolutionScheduling,
    #[error("recursive evolution actions are refused at admission")]
    RecursiveEvolution,
    #[error("children dedicated to evolution are refused at admission")]
    DedicatedEvolutionChild,
    #[error("action cannot transition from {from:?} to {to:?}")]
    IllegalTransition { from: ActionState, to: ActionState },
    #[error("session already has a running model turn")]
    TurnAlreadyRunning,
    #[error("action revision overflow")]
    RevisionOverflow,
    #[error("action enqueue sequence overflow")]
    SequenceOverflow,
    #[error("peer publication key conflicts with a different durable action: {0}")]
    PeerPublicationConflict(StableKey),
    #[error("action is not a peer-message recipient action")]
    NotPeerMessage,
    #[error("internal delivery claim is stale, expired, or owned by another worker")]
    DeliveryClaimLost,
    #[error("internal delivery fence or attempt counter overflow")]
    DeliveryCounterOverflow,
    #[error("action store lock was poisoned")]
    LockPoisoned,
}

struct StoredAction {
    value: ActionRecord,
    storage_revision: Revision,
}

pub struct PersistentActionInbox<R> {
    repository: R,
    config: ActionInboxConfig,
    serial: Mutex<()>,
}

impl<R> PersistentActionInbox<R>
where
    R: ActionRepository,
    R::Error: Display,
{
    /// # Errors
    ///
    /// Returns an error when the queue limits are inconsistent.
    pub fn new(repository: R, config: ActionInboxConfig) -> Result<Self, ActionStoreError> {
        if config.max_queued_per_session == 0
            || config.max_background_queued_per_session > config.max_queued_per_session
        {
            return Err(ActionStoreError::Invalid(
                "queue limits must be non-zero and background cannot exceed total".into(),
            ));
        }
        Ok(Self {
            repository,
            config,
            serial: Mutex::new(()),
        })
    }

    /// # Errors
    ///
    /// Returns an error for invalid, duplicate, over-limit, or unpersistable actions.
    pub fn submit(
        &self,
        action: SessionAction,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        if matches!(&action.source, ActionSource::PeerMessage { .. }) {
            return Err(ActionStoreError::Invalid(
                "peer messages must use enqueue_peer_message".into(),
            ));
        }
        validate_action(&action)?;
        let _guard = self.lock()?;
        let records = self.load_all()?;
        if records
            .iter()
            .any(|record| record.value.action.id == action.id)
        {
            return Err(ActionStoreError::Invalid("action ID already exists".into()));
        }
        let session_records = records
            .iter()
            .filter(|record| record.value.action.session_id == action.session_id)
            .collect::<Vec<_>>();
        let queued = session_records
            .iter()
            .filter(|record| !record.value.state.is_terminal())
            .count();
        if queued >= self.config.max_queued_per_session {
            return Err(ActionStoreError::QueueFull);
        }
        let background = session_records
            .iter()
            .filter(|record| {
                !record.value.state.is_terminal()
                    && record.value.action.priority == ActionPriority::Background
            })
            .count();
        if action.priority == ActionPriority::Background
            && background >= self.config.max_background_queued_per_session
        {
            return Err(ActionStoreError::BackgroundQueueFull);
        }
        let enqueue_sequence = session_records
            .iter()
            .map(|record| record.value.enqueue_sequence)
            .max()
            .unwrap_or(0)
            .checked_add(1)
            .ok_or(ActionStoreError::SequenceOverflow)?;
        let expired = action.deadline.is_some_and(|deadline| now >= deadline);
        let record = ActionRecord {
            version: CURRENT_SCHEMA_VERSION,
            action,
            state: if expired {
                ActionState::Expired
            } else {
                ActionState::Queued
            },
            enqueue_sequence,
            revision: Revision::ZERO,
            updated_at: now,
            terminal_detail: expired.then(|| "deadline elapsed before admission".into()),
            peer_receipt: None,
            internal_delivery: None,
        };
        self.put_new(&record)?;
        Ok(record)
    }

    /// # Errors
    ///
    /// Returns an error when the repository cannot read or decode the action.
    pub fn get(&self, id: &ActionId) -> Result<Option<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        self.load(id).map(|stored| stored.map(|value| value.value))
    }

    /// Enqueues one recipient turn from an already-persisted canonical peer source event.
    /// The receipt acknowledges durable queueing only; it never contains a recipient response.
    pub fn enqueue_peer_message(
        &self,
        request: PeerMessageEnqueue,
        now: UtcTimestamp,
    ) -> Result<PeerMessageReceipt, ActionStoreError> {
        validate_recipient_binding(&request.binding)?;
        if request.content.trim().is_empty()
            || request.content.len() > MAX_PEER_MESSAGE_BYTES
            || request.attachments.len() > MAX_PEER_ATTACHMENTS
        {
            return Err(ActionStoreError::Invalid(
                "peer content or attachment bounds are invalid".into(),
            ));
        }
        let action = SessionAction {
            id: request.action_id.clone(),
            session_id: request.binding.participant_session_id.clone(),
            source: ActionSource::PeerMessage {
                binding: request.binding.clone(),
            },
            delivery: DeliveryPolicy::WhenIdle,
            priority: ActionPriority::User,
            created_at: now,
            not_before: None,
            deadline: request.deadline,
            limits: request.limits,
            reply_route: None,
            payload: ActionPayload::PeerMessage {
                content: request.content,
                attachments: request.attachments,
            },
        };
        validate_action(&action)?;
        let _guard = self.lock()?;
        let records = self.load_all()?;
        for stored in &records {
            let ActionSource::PeerMessage { binding } = &stored.value.action.source else {
                continue;
            };
            let same_key = binding.publication_key == request.binding.publication_key;
            let same_source = binding.conversation_id == request.binding.conversation_id
                && binding.source_event_id == request.binding.source_event_id
                && binding.destination_profile_id == request.binding.destination_profile_id;
            if same_key || same_source {
                if binding == &request.binding
                    && stored.value.action.payload == action.payload
                    && stored.value.action.session_id == action.session_id
                {
                    let mut receipt =
                        stored.value.peer_receipt.clone().ok_or_else(|| {
                            ActionStoreError::Corrupt("peer receipt missing".into())
                        })?;
                    receipt.disposition = PeerMessageDisposition::Duplicate;
                    return Ok(receipt);
                }
                return Err(ActionStoreError::PeerPublicationConflict(
                    request.binding.publication_key,
                ));
            }
        }
        if records
            .iter()
            .any(|stored| stored.value.action.id == action.id)
        {
            return Err(ActionStoreError::Invalid("action ID already exists".into()));
        }
        let session_records = records
            .iter()
            .filter(|stored| stored.value.action.session_id == action.session_id)
            .collect::<Vec<_>>();
        if session_records
            .iter()
            .filter(|stored| !stored.value.state.is_terminal())
            .count()
            >= self.config.max_queued_per_session
        {
            return Err(ActionStoreError::QueueFull);
        }
        let enqueue_sequence = session_records
            .iter()
            .map(|stored| stored.value.enqueue_sequence)
            .max()
            .unwrap_or(0)
            .checked_add(1)
            .ok_or(ActionStoreError::SequenceOverflow)?;
        let receipt = PeerMessageReceipt {
            action_id: action.id.clone(),
            conversation_id: request.binding.conversation_id,
            source_event_id: request.binding.source_event_id,
            publication_key: request.binding.publication_key,
            sender_profile_id: request.binding.sender_profile_id,
            destination_profile_id: request.binding.destination_profile_id,
            participant_session_id: request.binding.participant_session_id,
            accepted_at: now,
            disposition: PeerMessageDisposition::Accepted,
        };
        let record = ActionRecord {
            version: CURRENT_SCHEMA_VERSION,
            action,
            state: ActionState::Queued,
            enqueue_sequence,
            revision: Revision::ZERO,
            updated_at: now,
            terminal_detail: None,
            peer_receipt: Some(receipt.clone()),
            internal_delivery: Some(InternalDeliveryProjection::default()),
        };
        self.put_new(&record)?;
        Ok(receipt)
    }

    pub fn enqueue_coordination_action(
        &self,
        request: CoordinationActionEnqueue,
        now: UtcTimestamp,
    ) -> Result<CoordinationActionReceipt, ActionStoreError> {
        validate_coordination_binding(&request.binding)?;
        if request.instruction.trim().is_empty()
            || request.instruction.len() > MAX_PEER_MESSAGE_BYTES
        {
            return Err(ActionStoreError::Invalid(
                "coordination instruction is empty or unbounded".into(),
            ));
        }
        let action = SessionAction {
            id: request.action_id.clone(),
            session_id: request.binding.destination_session_id().clone(),
            source: ActionSource::Coordination {
                binding: request.binding.clone(),
            },
            delivery: DeliveryPolicy::WhenIdle,
            priority: ActionPriority::User,
            created_at: now,
            not_before: None,
            deadline: request.deadline,
            limits: request.limits,
            reply_route: None,
            payload: ActionPayload::Coordination {
                instruction: request.instruction,
            },
        };
        validate_action(&action)?;
        let _guard = self.lock()?;
        let records = self.load_all()?;
        for stored in &records {
            let ActionSource::Coordination { binding } = &stored.value.action.source else {
                continue;
            };
            if binding.stable_key() != request.binding.stable_key() {
                continue;
            }
            if binding == &request.binding
                && stored.value.action.payload == action.payload
                && stored.value.action.session_id == action.session_id
            {
                return Ok(CoordinationActionReceipt {
                    action_id: stored.value.action.id.clone(),
                    stable_key: binding.stable_key().clone(),
                    accepted_at: stored.value.action.created_at,
                    disposition: CoordinationActionDisposition::Duplicate,
                });
            }
            return Err(ActionStoreError::PeerPublicationConflict(
                request.binding.stable_key().clone(),
            ));
        }
        if records
            .iter()
            .any(|stored| stored.value.action.id == action.id)
        {
            return Err(ActionStoreError::Invalid("action ID already exists".into()));
        }
        let session_records = records
            .iter()
            .filter(|stored| stored.value.action.session_id == action.session_id)
            .collect::<Vec<_>>();
        if session_records
            .iter()
            .filter(|stored| !stored.value.state.is_terminal())
            .count()
            >= self.config.max_queued_per_session
        {
            return Err(ActionStoreError::QueueFull);
        }
        let enqueue_sequence = session_records
            .iter()
            .map(|stored| stored.value.enqueue_sequence)
            .max()
            .unwrap_or(0)
            .checked_add(1)
            .ok_or(ActionStoreError::SequenceOverflow)?;
        let receipt = CoordinationActionReceipt {
            action_id: action.id.clone(),
            stable_key: request.binding.stable_key().clone(),
            accepted_at: now,
            disposition: CoordinationActionDisposition::Accepted,
        };
        self.put_new(&ActionRecord {
            version: CURRENT_SCHEMA_VERSION,
            action,
            state: ActionState::Queued,
            enqueue_sequence,
            revision: Revision::ZERO,
            updated_at: now,
            terminal_detail: None,
            peer_receipt: None,
            internal_delivery: Some(InternalDeliveryProjection::default()),
        })?;
        Ok(receipt)
    }

    pub fn peer_message_by_publication_key(
        &self,
        publication_key: &StableKey,
    ) -> Result<Option<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        Ok(self.load_all()?.into_iter().find_map(|stored| {
            matches!(
                &stored.value.action.source,
                ActionSource::PeerMessage { binding }
                    if &binding.publication_key == publication_key
            )
            .then_some(stored.value)
        }))
    }

    pub fn pending_peer_actions(
        &self,
        destination_profile_id: &ProfileId,
    ) -> Result<Vec<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        let mut records = self
            .load_all()?
            .into_iter()
            .filter_map(|stored| {
                let ActionSource::PeerMessage { binding } = &stored.value.action.source else {
                    return None;
                };
                (binding.destination_profile_id == *destination_profile_id
                    && stored
                        .value
                        .internal_delivery
                        .as_ref()
                        .is_some_and(|delivery| delivery.state == InternalDeliveryState::Pending))
                .then_some(stored.value)
            })
            .collect::<Vec<_>>();
        records.sort_by(action_order);
        Ok(records)
    }

    pub fn claim_internal_delivery(
        &self,
        id: &ActionId,
        owner: WorkerId,
        expires_at: UtcTimestamp,
        now: UtcTimestamp,
    ) -> Result<InternalDeliveryClaim, ActionStoreError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        let delivery = stored
            .value
            .internal_delivery
            .as_mut()
            .ok_or(ActionStoreError::NotPeerMessage)?;
        if expires_at <= now
            || matches!(
                delivery.state,
                InternalDeliveryState::Delivered
                    | InternalDeliveryState::DeadLettered
                    | InternalDeliveryState::Superseded
            )
            || (delivery.state == InternalDeliveryState::Claimed
                && delivery
                    .claim
                    .as_ref()
                    .is_some_and(|claim| claim.expires_at > now))
        {
            return Err(ActionStoreError::DeliveryClaimLost);
        }
        delivery.fence = delivery
            .fence
            .checked_add(1)
            .ok_or(ActionStoreError::DeliveryCounterOverflow)?;
        delivery.attempt_count = delivery
            .attempt_count
            .checked_add(1)
            .ok_or(ActionStoreError::DeliveryCounterOverflow)?;
        let claim = InternalDeliveryClaim {
            token: EntityId::new(),
            owner,
            fence: delivery.fence,
            attempt: delivery.attempt_count,
            claimed_at: now,
            expires_at,
        };
        delivery.state = InternalDeliveryState::Claimed;
        delivery.claim = Some(claim.clone());
        self.persist_delivery_mutation(&mut stored, now)?;
        Ok(claim)
    }

    pub fn complete_internal_delivery(
        &self,
        id: &ActionId,
        claim: &InternalDeliveryClaim,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.mutate_claimed_delivery(id, claim, now, |delivery| {
            delivery.state = InternalDeliveryState::Delivered;
            delivery.claim = None;
            delivery.delivered_at = Some(now);
            Ok(())
        })
    }

    pub fn renew_internal_delivery(
        &self,
        id: &ActionId,
        claim: &InternalDeliveryClaim,
        expires_at: UtcTimestamp,
        now: UtcTimestamp,
    ) -> Result<InternalDeliveryClaim, ActionStoreError> {
        if expires_at <= now {
            return Err(ActionStoreError::DeliveryClaimLost);
        }
        let mut renewed = None;
        self.mutate_claimed_delivery(id, claim, now, |delivery| {
            delivery.fence = delivery
                .fence
                .checked_add(1)
                .ok_or(ActionStoreError::DeliveryCounterOverflow)?;
            let next = InternalDeliveryClaim {
                token: EntityId::new(),
                owner: claim.owner.clone(),
                fence: delivery.fence,
                attempt: claim.attempt,
                claimed_at: now,
                expires_at,
            };
            delivery.claim = Some(next.clone());
            renewed = Some(next);
            Ok(())
        })?;
        renewed.ok_or_else(|| ActionStoreError::Corrupt("renewed claim missing".into()))
    }

    pub fn recover_expired_internal_deliveries(
        &self,
        now: UtcTimestamp,
    ) -> Result<Vec<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        let mut recovered = Vec::new();
        for mut stored in self.load_all()? {
            let expired = stored
                .value
                .internal_delivery
                .as_ref()
                .and_then(|delivery| delivery.claim.as_ref())
                .is_some_and(|claim| claim.expires_at <= now);
            if !expired {
                continue;
            }
            let delivery = stored.value.internal_delivery.as_mut().unwrap();
            delivery.state = InternalDeliveryState::Pending;
            delivery.claim = None;
            self.persist_delivery_mutation(&mut stored, now)?;
            recovered.push(stored.value);
        }
        Ok(recovered)
    }

    pub fn dead_letter_internal_delivery(
        &self,
        id: &ActionId,
        claim: &InternalDeliveryClaim,
        reason: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        let reason = reason.into();
        if reason.trim().is_empty() || reason.len() > MAX_DEAD_LETTER_REASON_BYTES {
            return Err(ActionStoreError::Invalid(
                "dead-letter reason is empty or unbounded".into(),
            ));
        }
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        let delivery = stored
            .value
            .internal_delivery
            .as_mut()
            .ok_or(ActionStoreError::NotPeerMessage)?;
        if delivery.state != InternalDeliveryState::Claimed
            || delivery.claim.as_ref() != Some(claim)
            || claim.expires_at <= now
        {
            return Err(ActionStoreError::DeliveryClaimLost);
        }
        delivery.state = InternalDeliveryState::DeadLettered;
        delivery.claim = None;
        delivery.dead_letter_reason = Some(reason.clone());
        stored.value.state = ActionState::Failed;
        stored.value.terminal_detail = Some(reason);
        self.persist_delivery_mutation(&mut stored, now)?;
        Ok(stored.value)
    }

    pub fn supersede_peer_action(
        &self,
        id: &ActionId,
        replacement: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        let _guard = self.lock()?;
        let replacement_record = self.required(replacement)?;
        let mut stored = self.required(id)?;
        let compatible_peer_route = match (
            &stored.value.action.source,
            &replacement_record.value.action.source,
        ) {
            (
                ActionSource::PeerMessage { binding: current },
                ActionSource::PeerMessage {
                    binding: replacement,
                },
            ) => {
                current.conversation_id == replacement.conversation_id
                    && current.destination_profile_id == replacement.destination_profile_id
                    && current.participant_session_id == replacement.participant_session_id
            }
            _ => false,
        };
        if id == replacement
            || !compatible_peer_route
            || stored.value.state != ActionState::Queued
            || stored
                .value
                .internal_delivery
                .as_ref()
                .is_none_or(|delivery| delivery.state != InternalDeliveryState::Pending)
            || replacement_record.value.internal_delivery.is_none()
        {
            return Err(ActionStoreError::Invalid(
                "supersession must target compatible pending peer actions".into(),
            ));
        }
        let delivery = stored.value.internal_delivery.as_mut().unwrap();
        delivery.state = InternalDeliveryState::Superseded;
        delivery.claim = None;
        delivery.superseded_by = Some(replacement.clone());
        stored.value.state = ActionState::Cancelled;
        stored.value.terminal_detail = Some("superseded by a newer targeted peer action".into());
        self.persist_delivery_mutation(&mut stored, now)?;
        Ok(stored.value)
    }

    pub fn supersede_ownership_transfer_deliveries(
        &self,
        wake_action_id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<Vec<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        let wake = self.required(wake_action_id)?;
        let ActionSource::Coordination {
            binding:
                CoordinationActionBinding::OwnershipTransferWake {
                    assignment_id,
                    expected_assignment_revision,
                    obsolete_delivery_ids,
                    ..
                },
        } = &wake.value.action.source
        else {
            return Err(ActionStoreError::Invalid(
                "supersession requires an ownership-transfer wake action".into(),
            ));
        };
        let obsolete = obsolete_delivery_ids
            .iter()
            .collect::<std::collections::BTreeSet<_>>();
        let records = self.load_all()?;
        let matched = records
            .iter()
            .filter_map(|stored| match &stored.value.action.source {
                ActionSource::Coordination {
                    binding: CoordinationActionBinding::GroupRoundDelivery { delivery_id, .. },
                } if obsolete.contains(delivery_id) => Some(delivery_id),
                ActionSource::Coordination {
                    binding:
                        CoordinationActionBinding::AssignmentWork {
                            delivery_id,
                            assignment_id: candidate,
                            assignment_revision,
                            ..
                        },
                } if candidate == assignment_id
                    && assignment_revision == expected_assignment_revision
                    && obsolete.contains(delivery_id) =>
                {
                    Some(delivery_id)
                }
                _ => None,
            })
            .collect::<std::collections::BTreeSet<_>>();
        if matched != obsolete {
            return Err(ActionStoreError::Invalid(
                "ownership transfer did not resolve every exact obsolete delivery".into(),
            ));
        }
        let mut changed = Vec::new();
        for mut stored in records {
            let delivery_id = match &stored.value.action.source {
                ActionSource::Coordination {
                    binding: CoordinationActionBinding::GroupRoundDelivery { delivery_id, .. },
                } => delivery_id,
                ActionSource::Coordination {
                    binding:
                        CoordinationActionBinding::AssignmentWork {
                            delivery_id,
                            assignment_id: candidate,
                            assignment_revision,
                            ..
                        },
                } if candidate == assignment_id
                    && assignment_revision == expected_assignment_revision =>
                {
                    delivery_id
                }
                _ => continue,
            };
            if !obsolete.contains(delivery_id) {
                continue;
            }
            let projection =
                stored.value.internal_delivery.as_mut().ok_or_else(|| {
                    ActionStoreError::Corrupt("delivery projection missing".into())
                })?;
            if projection.state == InternalDeliveryState::Superseded
                && projection.superseded_by.as_ref() == Some(wake_action_id)
            {
                changed.push(stored.value);
                continue;
            }
            if projection.state != InternalDeliveryState::Pending
                || stored.value.state != ActionState::Queued
            {
                return Err(ActionStoreError::Invalid(
                    "obsolete delivery is no longer pending at the ownership fence".into(),
                ));
            }
            projection.state = InternalDeliveryState::Superseded;
            projection.superseded_by = Some(wake_action_id.clone());
            projection.claim = None;
            stored.value.state = ActionState::Cancelled;
            stored.value.terminal_detail =
                Some("superseded by typed assignment ownership transfer".into());
            self.persist_delivery_mutation(&mut stored, now)?;
            changed.push(stored.value);
        }
        Ok(changed)
    }

    /// # Errors
    ///
    /// Returns an error when the repository cannot list or decode the session actions.
    pub fn list_session(
        &self,
        session_id: &SessionId,
    ) -> Result<Vec<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        let mut records = self
            .load_all()?
            .into_iter()
            .filter(|record| &record.value.action.session_id == session_id)
            .map(|record| record.value)
            .collect::<Vec<_>>();
        records.sort_by(action_order);
        Ok(records)
    }

    /// Returns queued or waiting peer and coordination actions for one canonical conversation.
    ///
    /// # Errors
    ///
    /// Returns an error when the repository cannot list or decode durable actions.
    pub fn pending_conversation(
        &self,
        conversation_id: &ConversationId,
    ) -> Result<Vec<ActionRecord>, ActionStoreError> {
        let _guard = self.lock()?;
        let mut records = self
            .load_all()?
            .into_iter()
            .filter(|record| {
                matches!(
                    record.value.state,
                    ActionState::Queued | ActionState::Waiting
                ) && match &record.value.action.source {
                    ActionSource::PeerMessage { binding } => {
                        &binding.conversation_id == conversation_id
                    }
                    ActionSource::Coordination { binding } => {
                        binding.conversation_id() == conversation_id
                    }
                    _ => false,
                }
            })
            .map(|record| record.value)
            .collect::<Vec<_>>();
        records.sort_by(action_order);
        Ok(records)
    }

    /// # Errors
    ///
    /// Returns an error when expiry or admission cannot be persisted.
    pub fn select_next(
        &self,
        session_id: &SessionId,
        now: UtcTimestamp,
        context: &PumpContext,
    ) -> Result<Option<SelectedAction>, ActionStoreError> {
        let _guard = self.lock()?;
        let mut records = self.load_all()?;
        for record in records.iter_mut().filter(|record| {
            record.value.action.session_id == *session_id
                && matches!(
                    record.value.state,
                    ActionState::Queued | ActionState::Waiting
                )
                && record
                    .value
                    .action
                    .deadline
                    .is_some_and(|deadline| now >= deadline)
        }) {
            self.transition_stored(
                record,
                ActionState::Expired,
                now,
                Some("deadline elapsed".into()),
            )?;
        }
        let mut eligible = records
            .into_iter()
            .filter(|record| {
                record.value.action.session_id == *session_id
                    && record.value.state == ActionState::Queued
                    && record
                        .value
                        .action
                        .not_before
                        .is_none_or(|not_before| now >= not_before)
                    && eligibility(&record.value.action, context).is_some()
            })
            .collect::<Vec<_>>();
        eligible.sort_by(|left, right| action_order(&left.value, &right.value));
        let Some(mut selected) = eligible.into_iter().next() else {
            return Ok(None);
        };
        let kind = eligibility(&selected.value.action, context)
            .ok_or_else(|| ActionStoreError::Invalid("selected action became ineligible".into()))?;
        self.transition_stored(&mut selected, ActionState::Admitted, now, None)?;
        Ok(Some(SelectedAction {
            record: selected.value,
            kind,
        }))
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition, concurrent turn, or persistence failure.
    pub fn mark_running(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        if self.load_all()?.iter().any(|record| {
            record.value.action.session_id == stored.value.action.session_id
                && record.value.action.id != *id
                && record.value.state == ActionState::Running
        }) {
            return Err(ActionStoreError::TurnAlreadyRunning);
        }
        self.transition_stored(&mut stored, ActionState::Running, now, None)?;
        Ok(stored.value)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn mark_waiting(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Waiting, now, None)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn resume_waiting(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Admitted, now, None)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn complete(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Completed, now, None)
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn fail(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
        detail: impl Into<String>,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Failed, now, Some(detail.into()))
    }

    /// # Errors
    ///
    /// Returns an error for an illegal transition or persistence failure.
    pub fn cancel(
        &self,
        id: &ActionId,
        now: UtcTimestamp,
        detail: impl Into<String>,
    ) -> Result<ActionRecord, ActionStoreError> {
        self.transition(id, ActionState::Cancelled, now, Some(detail.into()))
    }

    fn transition(
        &self,
        id: &ActionId,
        next: ActionState,
        now: UtcTimestamp,
        detail: Option<String>,
    ) -> Result<ActionRecord, ActionStoreError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        self.transition_stored(&mut stored, next, now, detail)?;
        Ok(stored.value)
    }

    fn transition_stored(
        &self,
        stored: &mut StoredAction,
        next: ActionState,
        now: UtcTimestamp,
        detail: Option<String>,
    ) -> Result<(), ActionStoreError> {
        if !stored.value.state.allows(next) {
            return Err(ActionStoreError::IllegalTransition {
                from: stored.value.state,
                to: next,
            });
        }
        let next_revision = stored
            .storage_revision
            .checked_next()
            .ok_or(ActionStoreError::RevisionOverflow)?;
        stored.value.state = next;
        stored.value.revision = next_revision;
        stored.value.updated_at = now;
        stored.value.terminal_detail = detail;
        self.repository
            .put_action(
                encode(&stored.value)?,
                WritePrecondition::Exact(stored.storage_revision),
            )
            .map_err(repository_error)?;
        stored.storage_revision = next_revision;
        Ok(())
    }

    fn mutate_claimed_delivery<F>(
        &self,
        id: &ActionId,
        claim: &InternalDeliveryClaim,
        now: UtcTimestamp,
        mutation: F,
    ) -> Result<ActionRecord, ActionStoreError>
    where
        F: FnOnce(&mut InternalDeliveryProjection) -> Result<(), ActionStoreError>,
    {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        let delivery = stored
            .value
            .internal_delivery
            .as_mut()
            .ok_or(ActionStoreError::NotPeerMessage)?;
        if delivery.state != InternalDeliveryState::Claimed
            || delivery.claim.as_ref() != Some(claim)
            || claim.expires_at <= now
        {
            return Err(ActionStoreError::DeliveryClaimLost);
        }
        mutation(delivery)?;
        self.persist_delivery_mutation(&mut stored, now)?;
        Ok(stored.value)
    }

    fn persist_delivery_mutation(
        &self,
        stored: &mut StoredAction,
        now: UtcTimestamp,
    ) -> Result<(), ActionStoreError> {
        let next_revision = stored
            .storage_revision
            .checked_next()
            .ok_or(ActionStoreError::RevisionOverflow)?;
        stored.value.revision = next_revision;
        stored.value.updated_at = now;
        self.repository
            .put_action(
                encode(&stored.value)?,
                WritePrecondition::Exact(stored.storage_revision),
            )
            .map_err(repository_error)?;
        stored.storage_revision = next_revision;
        Ok(())
    }

    fn required(&self, id: &ActionId) -> Result<StoredAction, ActionStoreError> {
        self.load(id)?
            .ok_or_else(|| ActionStoreError::NotFound(id.clone()))
    }

    fn load(&self, id: &ActionId) -> Result<Option<StoredAction>, ActionStoreError> {
        self.repository
            .get_action(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode)
            .transpose()
    }

    fn load_all(&self) -> Result<Vec<StoredAction>, ActionStoreError> {
        self.repository
            .list_actions()
            .map_err(repository_error)?
            .into_iter()
            .map(decode)
            .collect()
    }

    fn put_new(&self, record: &ActionRecord) -> Result<(), ActionStoreError> {
        self.repository
            .put_action(encode(record)?, WritePrecondition::Missing)
            .map_err(repository_error)?;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, ActionStoreError> {
        self.serial
            .lock()
            .map_err(|_| ActionStoreError::LockPoisoned)
    }
}

fn validate_action(action: &SessionAction) -> Result<(), ActionStoreError> {
    match (&action.source, &action.payload) {
        (
            ActionSource::PeerMessage { binding },
            ActionPayload::PeerMessage {
                content,
                attachments,
            },
        ) => {
            validate_recipient_binding(binding)?;
            if action.session_id != binding.participant_session_id
                || content.trim().is_empty()
                || content.len() > MAX_PEER_MESSAGE_BYTES
                || attachments.len() > MAX_PEER_ATTACHMENTS
            {
                return Err(ActionStoreError::Invalid(
                    "peer action binding or payload is invalid".into(),
                ));
            }
        }
        (ActionSource::PeerMessage { .. }, _) | (_, ActionPayload::PeerMessage { .. }) => {
            return Err(ActionStoreError::Invalid(
                "peer source and payload must be paired".into(),
            ));
        }
        (ActionSource::Coordination { binding }, ActionPayload::Coordination { instruction }) => {
            validate_coordination_binding(binding)?;
            if action.session_id != *binding.destination_session_id()
                || instruction.trim().is_empty()
                || instruction.len() > MAX_PEER_MESSAGE_BYTES
            {
                return Err(ActionStoreError::Invalid(
                    "coordination action binding or payload is invalid".into(),
                ));
            }
        }
        (ActionSource::Coordination { .. }, _) | (_, ActionPayload::Coordination { .. }) => {
            return Err(ActionStoreError::Invalid(
                "coordination source and payload must be paired".into(),
            ));
        }
        _ => {}
    }
    if action
        .payload
        .text()
        .is_some_and(|text| text.trim().is_empty())
    {
        return Err(ActionStoreError::Invalid(
            "text-bearing payloads cannot be empty".into(),
        ));
    }
    if action
        .not_before
        .zip(action.deadline)
        .is_some_and(|(not_before, deadline)| not_before >= deadline)
    {
        return Err(ActionStoreError::Invalid(
            "not-before must precede deadline".into(),
        ));
    }
    if action.delivery == DeliveryPolicy::NextTurnBoundary && !action.payload.is_steering() {
        return Err(ActionStoreError::Invalid(
            "turn-boundary delivery is reserved for steering payloads".into(),
        ));
    }
    match (&action.source, &action.payload) {
        (
            ActionSource::Evolution {
                ancestry,
                execution,
                ..
            },
            ActionPayload::Evolution { .. },
        ) => {
            if action.priority != ActionPriority::Background
                || action.delivery != DeliveryPolicy::WhenIdle
            {
                return Err(ActionStoreError::EvolutionScheduling);
            }
            if *execution == EvolutionExecution::DedicatedChild {
                return Err(ActionStoreError::DedicatedEvolutionChild);
            }
            if ancestry.len() > 32 || ancestry.contains(&ActionAncestorKind::Evolution) {
                return Err(ActionStoreError::RecursiveEvolution);
            }
        }
        (ActionSource::Evolution { .. }, _) | (_, ActionPayload::Evolution { .. }) => {
            return Err(ActionStoreError::Invalid(
                "evolution source and payload must be paired".into(),
            ));
        }
        _ => {}
    }
    Ok(())
}

fn validate_recipient_binding(binding: &RecipientActionBinding) -> Result<(), ActionStoreError> {
    if binding.sender_profile_id == binding.destination_profile_id
        || binding.context_cursor.applied_through_sequence == 0
        || binding.context_cursor.source_event_sequence == 0
        || binding.context_cursor.source_event_sequence
            > binding.context_cursor.applied_through_sequence
        || binding.policy_snapshot.relevant_grant_revisions.len() > MAX_RECIPIENT_POLICY_GRANTS
        || binding.policy_snapshot.policy_digest_sha256.len() != 64
        || !binding
            .policy_snapshot
            .policy_digest_sha256
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
        || binding.publication_key
            != RecipientActionBinding::canonical_publication_key(
                &binding.conversation_id,
                &binding.source_event_id,
                &binding.destination_profile_id,
            )?
    {
        return Err(ActionStoreError::Invalid(
            "recipient action authority or canonical source binding is invalid".into(),
        ));
    }
    Ok(())
}

fn validate_coordination_binding(
    binding: &CoordinationActionBinding,
) -> Result<(), ActionStoreError> {
    let (policy, cursor) = match binding {
        CoordinationActionBinding::GroupRoundDelivery {
            policy_snapshot,
            context_cursor,
            ..
        }
        | CoordinationActionBinding::AssignmentWork {
            policy_snapshot,
            context_cursor,
            ..
        }
        | CoordinationActionBinding::OwnershipTransferWake {
            policy_snapshot,
            context_cursor,
            ..
        } => (policy_snapshot, context_cursor),
    };
    if cursor.applied_through_sequence == 0
        || cursor.source_event_sequence == 0
        || cursor.source_event_sequence > cursor.applied_through_sequence
        || policy.relevant_grant_revisions.len() > MAX_RECIPIENT_POLICY_GRANTS
        || policy.policy_digest_sha256.len() != 64
        || !policy
            .policy_digest_sha256
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(ActionStoreError::Invalid(
            "coordination action authority or context cursor is invalid".into(),
        ));
    }
    if let CoordinationActionBinding::OwnershipTransferWake {
        expected_assignment_revision,
        new_assignment_revision,
        previous_owner_profile_id,
        new_owner_profile_id,
        obsolete_delivery_ids,
        ..
    } = binding
    {
        let unique = obsolete_delivery_ids
            .iter()
            .collect::<std::collections::BTreeSet<_>>();
        if previous_owner_profile_id == new_owner_profile_id
            || expected_assignment_revision.checked_next() != Some(*new_assignment_revision)
            || obsolete_delivery_ids.len() > 256
            || unique.len() != obsolete_delivery_ids.len()
        {
            return Err(ActionStoreError::Invalid(
                "ownership-transfer wake is stale, unbounded, or ambiguous".into(),
            ));
        }
    }
    Ok(())
}

fn validate_action_record(record: &ActionRecord) -> Result<(), ActionStoreError> {
    validate_action(&record.action)?;
    match &record.action.source {
        ActionSource::PeerMessage { binding } => {
            let receipt = record.peer_receipt.as_ref().ok_or_else(|| {
                ActionStoreError::Invalid("peer action is missing its durable receipt".into())
            })?;
            let delivery = record.internal_delivery.as_ref().ok_or_else(|| {
                ActionStoreError::Invalid("peer action is missing delivery state".into())
            })?;
            if receipt.disposition != PeerMessageDisposition::Accepted
                || receipt.action_id != record.action.id
                || receipt.conversation_id != binding.conversation_id
                || receipt.source_event_id != binding.source_event_id
                || receipt.publication_key != binding.publication_key
                || receipt.sender_profile_id != binding.sender_profile_id
                || receipt.destination_profile_id != binding.destination_profile_id
                || receipt.participant_session_id != binding.participant_session_id
                || delivery.claim.as_ref().is_some_and(|claim| {
                    delivery.state != InternalDeliveryState::Claimed
                        || claim.fence != delivery.fence
                        || claim.attempt != delivery.attempt_count
                        || claim.expires_at <= claim.claimed_at
                })
                || (delivery.state == InternalDeliveryState::Claimed && delivery.claim.is_none())
                || (delivery.state == InternalDeliveryState::Delivered
                    && delivery.delivered_at.is_none())
                || (delivery.state == InternalDeliveryState::DeadLettered
                    && delivery.dead_letter_reason.is_none())
                || (delivery.state == InternalDeliveryState::Superseded
                    && delivery.superseded_by.is_none())
            {
                return Err(ActionStoreError::Invalid(
                    "peer receipt or internal delivery projection is inconsistent".into(),
                ));
            }
        }
        ActionSource::Coordination { .. } => {
            if record.peer_receipt.is_some() || record.internal_delivery.is_none() {
                return Err(ActionStoreError::Invalid(
                    "coordination action delivery projection is inconsistent".into(),
                ));
            }
        }
        _ if record.peer_receipt.is_some() || record.internal_delivery.is_some() => {
            return Err(ActionStoreError::Invalid(
                "non-peer action contains peer delivery state".into(),
            ));
        }
        _ => {}
    }
    Ok(())
}

fn eligibility(action: &SessionAction, context: &PumpContext) -> Option<AdmissionKind> {
    match action.delivery {
        DeliveryPolicy::Immediate => {
            if action.priority == ActionPriority::Interrupt && action.payload.is_steering() {
                context
                    .active_action
                    .as_ref()
                    .map(|_| AdmissionKind::ApplySteering)
            } else if context.active_action.is_none() {
                Some(AdmissionKind::StartTurn)
            } else {
                None
            }
        }
        DeliveryPolicy::NextTurnBoundary => {
            if context.active_action.is_some() && context.at_turn_boundary {
                Some(AdmissionKind::ApplySteering)
            } else if context.active_action.is_none() {
                Some(AdmissionKind::StartTurn)
            } else {
                None
            }
        }
        DeliveryPolicy::WhenIdle => (context.active_action.is_none() && context.session_idle)
            .then_some(AdmissionKind::StartTurn),
    }
}

fn action_order(left: &ActionRecord, right: &ActionRecord) -> Ordering {
    left.action
        .priority
        .rank()
        .cmp(&right.action.priority.rank())
        .then_with(|| left.enqueue_sequence.cmp(&right.enqueue_sequence))
        .then_with(|| left.action.id.cmp(&right.action.id))
}

fn encode(record: &ActionRecord) -> Result<VersionedRecord, ActionStoreError> {
    validate_action_record(record)?;
    let payload = serde_json::to_value(record)
        .map_err(|error| ActionStoreError::Corrupt(error.to_string()))?;
    Ok(VersionedRecord {
        version: record.version,
        id: record.action.id.as_entity_id().clone(),
        revision: record.revision,
        updated_at: record.updated_at,
        payload,
    })
}

fn decode(record: VersionedRecord) -> Result<StoredAction, ActionStoreError> {
    let value: ActionRecord = serde_json::from_value(record.payload)
        .map_err(|error| ActionStoreError::Corrupt(error.to_string()))?;
    if value.version.major != CURRENT_SCHEMA_VERSION.major
        || value.version.minor > CURRENT_SCHEMA_VERSION.minor
        || value.action.id.as_entity_id() != &record.id
        || value.revision != record.revision
    {
        return Err(ActionStoreError::Corrupt(
            "record envelope does not match its action payload".into(),
        ));
    }
    validate_action_record(&value).map_err(|error| ActionStoreError::Corrupt(error.to_string()))?;
    Ok(StoredAction {
        value,
        storage_revision: record.revision,
    })
}

fn repository_error(error: impl Display) -> ActionStoreError {
    ActionStoreError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use keith_state_store::EmbeddedStore;
    use tempfile::tempdir;

    use super::*;

    fn action(
        session_id: &SessionId,
        priority: ActionPriority,
        delivery: DeliveryPolicy,
        text: &str,
        created_at: i64,
    ) -> SessionAction {
        let client_id = ClientId::new();
        SessionAction {
            id: ActionId::new(),
            session_id: session_id.clone(),
            source: if delivery == DeliveryPolicy::NextTurnBoundary {
                ActionSource::Steering {
                    client_id: client_id.clone(),
                }
            } else {
                ActionSource::Interactive {
                    client_id: client_id.clone(),
                }
            },
            delivery,
            priority,
            created_at: UtcTimestamp::from_unix_millis(created_at),
            not_before: None,
            deadline: None,
            limits: ActionLimits::default(),
            reply_route: Some(ReplyRoute::Client { client_id }),
            payload: if delivery == DeliveryPolicy::NextTurnBoundary {
                ActionPayload::Steering { text: text.into() }
            } else {
                ActionPayload::Prompt { text: text.into() }
            },
        }
    }

    fn inbox(path: &Path, config: ActionInboxConfig) -> PersistentActionInbox<EmbeddedStore> {
        PersistentActionInbox::new(EmbeddedStore::open(path, None).unwrap(), config).unwrap()
    }

    fn evolution_action(session_id: &SessionId) -> SessionAction {
        SessionAction {
            id: ActionId::new(),
            session_id: session_id.clone(),
            source: ActionSource::Evolution {
                generation_id: EntityId::new(),
                ancestry: vec![ActionAncestorKind::Ordinary],
                execution: EvolutionExecution::OrdinarySession,
            },
            delivery: DeliveryPolicy::WhenIdle,
            priority: ActionPriority::Background,
            created_at: UtcTimestamp::UNIX_EPOCH,
            not_before: None,
            deadline: None,
            limits: ActionLimits::default(),
            reply_route: None,
            payload: ActionPayload::Evolution {
                operation: EvolutionOperation::EvaluateHypothesis,
            },
        }
    }

    #[test]
    fn interleaved_sources_preserve_priority_and_fifo_across_restart() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("actions.sqlite");
        let session_id = SessionId::new();
        let config = ActionInboxConfig::default();
        let first_user = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "first user",
            10,
        );
        let second_user = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "second user",
            10,
        );
        let scheduled = action(
            &session_id,
            ActionPriority::Scheduled,
            DeliveryPolicy::Immediate,
            "scheduled",
            1,
        );
        let background = action(
            &session_id,
            ActionPriority::Background,
            DeliveryPolicy::WhenIdle,
            "background",
            0,
        );
        {
            let inbox = inbox(&path, config);
            inbox
                .submit(background.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .submit(scheduled.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .submit(first_user.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .submit(second_user.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        let inbox = inbox(&path, config);
        let context = PumpContext {
            active_action: None,
            at_turn_boundary: false,
            session_idle: true,
        };
        let mut selected = Vec::new();
        for _ in 0..4 {
            let next = inbox
                .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &context)
                .unwrap()
                .unwrap();
            selected.push(next.record.action.id.clone());
            inbox
                .mark_running(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .complete(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        assert_eq!(
            selected,
            vec![first_user.id, second_user.id, scheduled.id, background.id]
        );
    }

    #[test]
    fn steering_waits_for_a_turn_boundary_and_does_not_start_another_turn() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let inbox = PersistentActionInbox::new(store, ActionInboxConfig::default()).unwrap();
        let session_id = SessionId::new();
        let active_id = ActionId::new();
        let steering = action(
            &session_id,
            ActionPriority::Interrupt,
            DeliveryPolicy::NextTurnBoundary,
            "change course",
            0,
        );
        inbox
            .submit(steering.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let before_boundary = PumpContext {
            active_action: Some(active_id.clone()),
            at_turn_boundary: false,
            session_idle: false,
        };
        assert!(
            inbox
                .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &before_boundary)
                .unwrap()
                .is_none()
        );
        let at_boundary = PumpContext {
            active_action: Some(active_id),
            at_turn_boundary: true,
            session_idle: false,
        };
        let selected = inbox
            .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &at_boundary)
            .unwrap()
            .unwrap();
        assert_eq!(selected.kind, AdmissionKind::ApplySteering);
        assert_eq!(selected.record.action.id, steering.id);
        assert_eq!(selected.record.state, ActionState::Admitted);
        inbox
            .complete(&steering.id, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
    }

    #[test]
    fn queue_bounds_reserve_capacity_from_background_work() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig {
                max_queued_per_session: 2,
                max_background_queued_per_session: 1,
            },
        )
        .unwrap();
        let session_id = SessionId::new();
        inbox
            .submit(
                action(
                    &session_id,
                    ActionPriority::Background,
                    DeliveryPolicy::WhenIdle,
                    "one",
                    0,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let second_background = inbox.submit(
            action(
                &session_id,
                ActionPriority::Background,
                DeliveryPolicy::WhenIdle,
                "two",
                1,
            ),
            UtcTimestamp::UNIX_EPOCH,
        );
        assert!(matches!(
            second_background,
            Err(ActionStoreError::BackgroundQueueFull)
        ));
        inbox
            .submit(
                action(
                    &session_id,
                    ActionPriority::User,
                    DeliveryPolicy::Immediate,
                    "user",
                    2,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
    }

    #[test]
    fn expiry_cancellation_and_terminal_states_are_durable() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("actions.sqlite");
        let session_id = SessionId::new();
        let mut expired = action(
            &session_id,
            ActionPriority::Scheduled,
            DeliveryPolicy::Immediate,
            "late",
            0,
        );
        expired.deadline = Some(UtcTimestamp::from_unix_millis(5));
        let cancelled = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "cancel me",
            0,
        );
        {
            let inbox = inbox(&path, ActionInboxConfig::default());
            let record = inbox
                .submit(expired.clone(), UtcTimestamp::from_unix_millis(5))
                .unwrap();
            assert_eq!(record.state, ActionState::Expired);
            inbox
                .submit(cancelled.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .cancel(
                    &cancelled.id,
                    UtcTimestamp::from_unix_millis(6),
                    "user request",
                )
                .unwrap();
        }
        let inbox = inbox(&path, ActionInboxConfig::default());
        assert_eq!(
            inbox.get(&expired.id).unwrap().unwrap().state,
            ActionState::Expired
        );
        let record = inbox.get(&cancelled.id).unwrap().unwrap();
        assert_eq!(record.state, ActionState::Cancelled);
        assert_eq!(record.terminal_detail.as_deref(), Some("user request"));
    }

    #[test]
    fn only_one_model_turn_can_run_per_session() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig::default(),
        )
        .unwrap();
        let session_id = SessionId::new();
        let first = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "first",
            0,
        );
        let second = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "second",
            1,
        );
        inbox
            .submit(first.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        inbox
            .submit(second.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let idle = PumpContext {
            session_idle: true,
            ..PumpContext::default()
        };
        inbox
            .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &idle)
            .unwrap();
        inbox
            .mark_running(&first.id, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        assert!(matches!(
            inbox.mark_running(&second.id, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::TurnAlreadyRunning | ActionStoreError::IllegalTransition { .. })
        ));
    }

    #[test]
    fn evolution_requires_background_when_idle_and_typed_non_recursive_origin() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig::default(),
        )
        .unwrap();
        let session_id = SessionId::new();

        let mut foreground = evolution_action(&session_id);
        foreground.priority = ActionPriority::User;
        assert!(matches!(
            inbox.submit(foreground, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::EvolutionScheduling)
        ));

        let mut recursive = evolution_action(&session_id);
        if let ActionSource::Evolution { ancestry, .. } = &mut recursive.source {
            ancestry.push(ActionAncestorKind::Evolution);
        }
        assert!(matches!(
            inbox.submit(recursive, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::RecursiveEvolution)
        ));

        let mut child = evolution_action(&session_id);
        if let ActionSource::Evolution { execution, .. } = &mut child.source {
            *execution = EvolutionExecution::DedicatedChild;
        }
        assert!(matches!(
            inbox.submit(child, UtcTimestamp::UNIX_EPOCH),
            Err(ActionStoreError::DedicatedEvolutionChild)
        ));

        let accepted = evolution_action(&session_id);
        assert_eq!(
            inbox
                .submit(accepted, UtcTimestamp::UNIX_EPOCH)
                .unwrap()
                .state,
            ActionState::Queued
        );
    }

    #[test]
    fn user_channel_and_scheduled_work_precede_evolution() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig::default(),
        )
        .unwrap();
        let session_id = SessionId::new();
        let evolution = evolution_action(&session_id);
        let mut scheduled = action(
            &session_id,
            ActionPriority::Scheduled,
            DeliveryPolicy::Immediate,
            "scheduled",
            0,
        );
        scheduled.source = ActionSource::Schedule {
            job_id: JobId::new(),
            attempt: 1,
        };
        scheduled.payload = ActionPayload::Scheduled {
            instruction: "scheduled".into(),
        };
        let mut channel = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "channel",
            0,
        );
        channel.source = ActionSource::Channel {
            channel: "test".into(),
            message_id: "message".into(),
        };
        channel.payload = ActionPayload::ChannelMessage {
            text: "channel".into(),
            attachments: Vec::new(),
        };
        let user = action(
            &session_id,
            ActionPriority::User,
            DeliveryPolicy::Immediate,
            "user",
            0,
        );
        for candidate in [&evolution, &scheduled, &channel, &user] {
            inbox
                .submit(candidate.clone(), UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        let context = PumpContext {
            session_idle: true,
            ..PumpContext::default()
        };
        let mut selected = Vec::new();
        for _ in 0..4 {
            let next = inbox
                .select_next(&session_id, UtcTimestamp::UNIX_EPOCH, &context)
                .unwrap()
                .unwrap();
            selected.push(next.record.action.id.clone());
            inbox
                .mark_running(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            inbox
                .complete(&next.record.action.id, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
        }
        assert_eq!(
            selected,
            vec![channel.id, user.id, scheduled.id, evolution.id]
        );
    }

    #[test]
    fn pending_conversation_selects_only_live_peer_work_for_exact_conversation() {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ActionInboxConfig::default(),
        )
        .unwrap();
        let conversation_id = ConversationId::new();
        let other_conversation_id = ConversationId::new();
        let enqueue = |conversation_id: ConversationId, session_id: SessionId| {
            let source_event_id = EventId::new();
            let destination_profile_id = ProfileId::new();
            PeerMessageEnqueue {
                action_id: ActionId::new(),
                binding: RecipientActionBinding {
                    publication_key: RecipientActionBinding::canonical_publication_key(
                        &conversation_id,
                        &source_event_id,
                        &destination_profile_id,
                    )
                    .unwrap(),
                    conversation_id,
                    source_event_id,
                    sender_profile_id: ProfileId::new(),
                    destination_profile_id,
                    participant_session_id: session_id,
                    policy_snapshot: RecipientPolicySnapshot {
                        conversation_revision: Revision::new(1),
                        participant_revision: Revision::new(1),
                        relevant_grant_revisions: BTreeMap::new(),
                        policy_digest_sha256: "a".repeat(64),
                    },
                    context_cursor: CanonicalConversationContextCursor {
                        applied_through_sequence: 1,
                        source_event_sequence: 1,
                    },
                },
                content: "wake this participant".into(),
                attachments: Vec::new(),
                deadline: None,
                limits: ActionLimits::default(),
            }
        };
        let expected = enqueue(conversation_id.clone(), SessionId::new());
        let excluded = enqueue(other_conversation_id, SessionId::new());
        inbox
            .enqueue_peer_message(expected.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        inbox
            .enqueue_peer_message(excluded, UtcTimestamp::UNIX_EPOCH)
            .unwrap();

        let pending = inbox.pending_conversation(&conversation_id).unwrap();
        assert_eq!(pending.len(), 1);
        assert_eq!(pending[0].action.id, expected.action_id);
    }
}
