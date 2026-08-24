#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};

use keith_agent_types::{
    ArtifactId, CURRENT_PROTOCOL_VERSION, ClientId, CommandId, ConversationId, EntityId, EventId,
    ProfileId, Revision, SessionId, StableKey, UtcTimestamp,
};
use keith_connection::{AgentTransport, ConnectionError};
use keith_protocol::{
    CancelTarget, ClientCommand, ClientHello, CommandEnvelope, CommandResult, DeliveryPolicy,
    EventEnvelope, Feature, ReplyRoute as ProtocolReplyRoute, SteerAction, SubmitPrompt,
    WireMessage,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Attachment {
    pub id: String,
    pub file_name: String,
    pub media_type: String,
    pub byte_length: u64,
    pub artifact_id: Option<ArtifactId>,
    #[serde(default)]
    pub download_url: Option<String>,
    #[serde(default)]
    pub staging_file: Option<String>,
    #[serde(default)]
    pub sha256: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReplyRoute {
    pub channel: String,
    pub external_account: String,
    pub conversation: String,
    pub thread: Option<String>,
    pub reply_to_message: Option<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum InboundIntent {
    Prompt,
    Steer,
    Cancel,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct InboundMessage {
    pub channel: String,
    pub external_account: String,
    pub conversation: String,
    pub thread: Option<String>,
    pub sender: String,
    pub message_id: String,
    pub reply_target: Option<String>,
    pub text: String,
    pub attachments: Vec<Attachment>,
    pub occurred_at: UtcTimestamp,
    pub intent: InboundIntent,
}

impl InboundMessage {
    pub fn reply_route(&self) -> ReplyRoute {
        ReplyRoute {
            channel: self.channel.clone(),
            external_account: self.external_account.clone(),
            conversation: self.conversation.clone(),
            thread: self.thread.clone(),
            reply_to_message: Some(self.message_id.clone()),
        }
    }

    fn deduplication_key(&self) -> String {
        [
            self.channel.as_str(),
            self.external_account.as_str(),
            self.conversation.as_str(),
            self.thread.as_deref().unwrap_or_default(),
            self.message_id.as_str(),
        ]
        .join("\0")
    }

    fn validate(&self) -> Result<(), GatewayError> {
        if self.channel.trim().is_empty()
            || self.external_account.trim().is_empty()
            || self.conversation.trim().is_empty()
            || self.sender.trim().is_empty()
            || self.message_id.trim().is_empty()
        {
            return Err(GatewayError::Malformed);
        }
        if self.intent != InboundIntent::Cancel
            && self.text.trim().is_empty()
            && self.attachments.is_empty()
        {
            return Err(GatewayError::Malformed);
        }
        Ok(())
    }

    /// Returns a platform-stable source key used for canonical append deduplication.
    pub fn stable_source_key(&self) -> Result<StableKey, GatewayError> {
        self.validate()?;
        StableKey::parse(format!(
            "channel:{}:{}:{}:{}:{}",
            self.channel,
            self.external_account,
            self.conversation,
            self.thread.as_deref().unwrap_or("root"),
            self.message_id
        ))
        .map_err(|_| GatewayError::Malformed)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutedInbound {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub message: InboundMessage,
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExternalConversationIdentity {
    pub channel: String,
    pub external_account: String,
    pub conversation: String,
    pub thread: Option<String>,
}

impl From<&InboundMessage> for ExternalConversationIdentity {
    fn from(message: &InboundMessage) -> Self {
        Self {
            channel: message.channel.clone(),
            external_account: message.external_account.clone(),
            conversation: message.conversation.clone(),
            thread: message.thread.clone(),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BoundMentionPolicy {
    Always,
    RequireMention,
    Ignore,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BoundMemoryPolicy {
    Shared,
    PrivatePerParticipant,
    Disabled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "authority", content = "principals")]
pub enum BoundChannelAuthority {
    Disabled,
    ProfileCallers,
    AllowList(BTreeSet<String>),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BoundProactivePostPolicy {
    Denied,
    Allowed,
}

/// Policy evidence copied from the durable binding. The daemon must revalidate its revisions and
/// digest before append or publication; gateways never widen it.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBindingPolicy {
    pub route_revision: Revision,
    pub conversation_revision: Revision,
    pub participant_revision: Revision,
    pub policy_digest_sha256: String,
    pub mention: BoundMentionPolicy,
    pub memory: BoundMemoryPolicy,
    pub tools: BoundChannelAuthority,
    pub schedules: BoundChannelAuthority,
    pub proactive_posts: BoundProactivePostPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBindingReference {
    pub binding_id: EntityId,
    pub stable_key: StableKey,
    pub binding_revision: Revision,
    pub external: ExternalConversationIdentity,
    pub conversation_id: ConversationId,
    pub participant_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub policy: ConversationBindingPolicy,
}

/// A routing-authorized, immutable snapshot of the binding used for one channel operation.
/// Callers cannot supply capabilities separately from this snapshot; the policy is always the
/// exact policy that was authorized with the binding revision.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelConversationBindingSnapshot {
    pub binding: ConversationBindingReference,
    pub state: ChannelConversationBindingState,
    pub revalidated_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelConversationBindingState {
    Active,
    Revoked,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthenticatedChannelParticipant {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBoundIngressRequest {
    pub request_id: EntityId,
    pub operation_key: StableKey,
    pub source_message_key: StableKey,
    pub binding: ChannelConversationBindingSnapshot,
    pub authenticated_participant: AuthenticatedChannelParticipant,
    pub sender_external_id: String,
    pub occurred_at: UtcTimestamp,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
    pub reply_route: ReplyRoute,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBoundOutboundRequest {
    pub request_id: EntityId,
    pub operation_key: StableKey,
    pub source_message_key: StableKey,
    pub binding: ChannelConversationBindingSnapshot,
    pub authenticated_participant: AuthenticatedChannelParticipant,
    pub event_id: EventId,
    pub stable_publication_key: StableKey,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
    pub route: ReplyRoute,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationBoundReceiptDisposition {
    Accepted,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBoundIngressReceipt {
    pub request_id: EntityId,
    pub operation_key: StableKey,
    pub source_message_key: StableKey,
    pub binding_id: EntityId,
    pub binding_stable_key: StableKey,
    pub binding_revision: Revision,
    pub conversation_id: ConversationId,
    pub authenticated_participant: AuthenticatedChannelParticipant,
    pub policy_snapshot: ConversationBindingPolicy,
    pub disposition: ConversationBoundReceiptDisposition,
    pub accepted_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationBoundOutboundReceipt {
    pub request_id: EntityId,
    pub operation_key: StableKey,
    pub source_message_key: StableKey,
    pub binding_id: EntityId,
    pub binding_stable_key: StableKey,
    pub binding_revision: Revision,
    pub conversation_id: ConversationId,
    pub authenticated_participant: AuthenticatedChannelParticipant,
    pub event_id: EventId,
    pub stable_publication_key: StableKey,
    pub policy_snapshot: ConversationBindingPolicy,
    pub disposition: ConversationBoundReceiptDisposition,
    pub accepted_at: UtcTimestamp,
}

impl ChannelConversationBindingSnapshot {
    pub fn validate_current(&self, current: &Self) -> Result<(), ConversationBoundChannelError> {
        validate_binding_snapshot(current)?;
        if current.state == ChannelConversationBindingState::Revoked {
            return Err(ConversationBoundChannelError::BindingRevoked);
        }
        if self.binding.binding_id != current.binding.binding_id
            || self.binding.stable_key != current.binding.stable_key
            || self.binding.binding_revision != current.binding.binding_revision
            || self.binding.external != current.binding.external
            || self.binding.conversation_id != current.binding.conversation_id
            || self.binding.participant_profile_id != current.binding.participant_profile_id
            || self.binding.participant_session_id != current.binding.participant_session_id
            || self.binding.policy != current.binding.policy
        {
            return Err(ConversationBoundChannelError::BindingStale);
        }
        if self.state != ChannelConversationBindingState::Active {
            return Err(ConversationBoundChannelError::BindingRevoked);
        }
        Ok(())
    }
}

impl ConversationBoundIngressRequest {
    pub fn validate_current(
        &self,
        current: &ChannelConversationBindingSnapshot,
    ) -> Result<(), ConversationBoundChannelError> {
        self.binding.validate_current(current)?;
        validate_authenticated_participant(&self.binding.binding, &self.authenticated_participant)?;
        validate_channel_text(&self.sender_external_id, MAX_EXTERNAL_SENDER_BYTES)?;
        validate_channel_text(&self.text, MAX_CONVERSATION_MESSAGE_BYTES)?;
        validate_channel_artifacts(&self.artifacts)?;
        validate_reply_route(&self.reply_route, &self.binding.binding.external)
    }

    pub fn accepted_receipt(
        &self,
        current: &ChannelConversationBindingSnapshot,
        disposition: ConversationBoundReceiptDisposition,
        accepted_at: UtcTimestamp,
    ) -> Result<ConversationBoundIngressReceipt, ConversationBoundChannelError> {
        self.validate_current(current)?;
        Ok(ConversationBoundIngressReceipt {
            request_id: self.request_id.clone(),
            operation_key: self.operation_key.clone(),
            source_message_key: self.source_message_key.clone(),
            binding_id: current.binding.binding_id.clone(),
            binding_stable_key: current.binding.stable_key.clone(),
            binding_revision: current.binding.binding_revision,
            conversation_id: current.binding.conversation_id.clone(),
            authenticated_participant: self.authenticated_participant.clone(),
            policy_snapshot: current.binding.policy.clone(),
            disposition,
            accepted_at,
        })
    }
}

impl ConversationBoundOutboundRequest {
    pub fn validate_current(
        &self,
        current: &ChannelConversationBindingSnapshot,
    ) -> Result<(), ConversationBoundChannelError> {
        self.binding.validate_current(current)?;
        validate_authenticated_participant(&self.binding.binding, &self.authenticated_participant)?;
        validate_channel_text(&self.text, MAX_CONVERSATION_MESSAGE_BYTES)?;
        validate_channel_artifacts(&self.artifacts)?;
        validate_reply_route(&self.route, &self.binding.binding.external)
    }

    pub fn accepted_receipt(
        &self,
        current: &ChannelConversationBindingSnapshot,
        disposition: ConversationBoundReceiptDisposition,
        accepted_at: UtcTimestamp,
    ) -> Result<ConversationBoundOutboundReceipt, ConversationBoundChannelError> {
        self.validate_current(current)?;
        Ok(ConversationBoundOutboundReceipt {
            request_id: self.request_id.clone(),
            operation_key: self.operation_key.clone(),
            source_message_key: self.source_message_key.clone(),
            binding_id: current.binding.binding_id.clone(),
            binding_stable_key: current.binding.stable_key.clone(),
            binding_revision: current.binding.binding_revision,
            conversation_id: current.binding.conversation_id.clone(),
            authenticated_participant: self.authenticated_participant.clone(),
            event_id: self.event_id.clone(),
            stable_publication_key: self.stable_publication_key.clone(),
            policy_snapshot: current.binding.policy.clone(),
            disposition,
            accepted_at,
        })
    }
}

fn validate_binding_snapshot(
    snapshot: &ChannelConversationBindingSnapshot,
) -> Result<(), ConversationBoundChannelError> {
    validate_external_identity(&snapshot.binding.external)?;
    if snapshot.binding.policy.policy_digest_sha256.len() != 64
        || !snapshot
            .binding
            .policy
            .policy_digest_sha256
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
    {
        return Err(ConversationBoundChannelError::InvalidPolicySnapshot);
    }
    Ok(())
}

fn validate_authenticated_participant(
    binding: &ConversationBindingReference,
    authenticated: &AuthenticatedChannelParticipant,
) -> Result<(), ConversationBoundChannelError> {
    if binding.participant_profile_id != authenticated.profile_id
        || binding.participant_session_id != authenticated.session_id
    {
        return Err(ConversationBoundChannelError::AuthenticatedIdentityMismatch);
    }
    Ok(())
}

fn validate_external_identity(
    external: &ExternalConversationIdentity,
) -> Result<(), ConversationBoundChannelError> {
    validate_channel_text(&external.channel, MAX_EXTERNAL_IDENTITY_BYTES)?;
    validate_channel_text(&external.external_account, MAX_EXTERNAL_IDENTITY_BYTES)?;
    validate_channel_text(&external.conversation, MAX_EXTERNAL_IDENTITY_BYTES)?;
    if let Some(thread) = &external.thread {
        validate_channel_text(thread, MAX_EXTERNAL_IDENTITY_BYTES)?;
    }
    Ok(())
}

fn validate_reply_route(
    route: &ReplyRoute,
    external: &ExternalConversationIdentity,
) -> Result<(), ConversationBoundChannelError> {
    if route.channel != external.channel
        || route.external_account != external.external_account
        || route.conversation != external.conversation
        || route.thread != external.thread
    {
        return Err(ConversationBoundChannelError::RouteMismatch);
    }
    if let Some(reply_to) = &route.reply_to_message {
        validate_channel_text(reply_to, MAX_EXTERNAL_IDENTITY_BYTES)?;
    }
    Ok(())
}

fn validate_channel_artifacts(
    artifacts: &[ArtifactId],
) -> Result<(), ConversationBoundChannelError> {
    if artifacts.len() > MAX_CONVERSATION_ARTIFACTS {
        return Err(ConversationBoundChannelError::ArtifactLimit);
    }
    let unique = artifacts.iter().collect::<BTreeSet<_>>();
    if unique.len() != artifacts.len() {
        return Err(ConversationBoundChannelError::DuplicateArtifact);
    }
    Ok(())
}

fn validate_channel_text(
    value: &str,
    max_bytes: usize,
) -> Result<(), ConversationBoundChannelError> {
    if value.is_empty()
        || value.len() > max_bytes
        || value.trim() != value
        || value.chars().any(char::is_control)
    {
        return Err(ConversationBoundChannelError::InvalidText);
    }
    Ok(())
}

const MAX_EXTERNAL_IDENTITY_BYTES: usize = 512;
const MAX_EXTERNAL_SENDER_BYTES: usize = 512;
const MAX_CONVERSATION_MESSAGE_BYTES: usize = 64 * 1024;
const MAX_CONVERSATION_ARTIFACTS: usize = 64;

#[derive(Clone, Copy, Debug, Eq, PartialEq, thiserror::Error)]
pub enum ConversationBoundChannelError {
    #[error("conversation binding has been revoked")]
    BindingRevoked,
    #[error("conversation binding revision or policy is stale")]
    BindingStale,
    #[error("authenticated participant does not match the conversation binding")]
    AuthenticatedIdentityMismatch,
    #[error("conversation binding policy snapshot is invalid")]
    InvalidPolicySnapshot,
    #[error("channel route does not match the conversation binding")]
    RouteMismatch,
    #[error("channel text is empty, non-canonical, or exceeds its bound")]
    InvalidText,
    #[error("channel artifact count exceeds its bound")]
    ArtifactLimit,
    #[error("channel artifact identifiers must be unique")]
    DuplicateArtifact,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationRoutedInbound {
    pub binding: ConversationBindingReference,
    pub source_key: StableKey,
    pub message: InboundMessage,
}

impl ConversationRoutedInbound {
    /// Validates that a resolver-bound message still has the same immutable external identity and
    /// stable source identity before it is admitted to the canonical append path.
    pub fn validate(&self) -> Result<(), GatewayError> {
        self.message.validate()?;
        if self.binding.external != ExternalConversationIdentity::from(&self.message)
            || self.source_key != self.message.stable_source_key()?
            || !valid_sha256(&self.binding.policy.policy_digest_sha256)
        {
            return Err(GatewayError::BindingMismatch);
        }
        Ok(())
    }

    pub fn dispatch_key(&self) -> ConversationDispatchKey {
        ConversationDispatchKey {
            conversation_id: self.binding.conversation_id.clone(),
            participant_profile_id: self.binding.participant_profile_id.clone(),
        }
    }
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub struct ConversationDispatchKey {
    pub conversation_id: ConversationId,
    pub participant_profile_id: ProfileId,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalChannelAppend {
    pub binding: ConversationBindingReference,
    pub source_key: StableKey,
    pub sender_external_id: String,
    pub occurred_at: UtcTimestamp,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
    pub reply_route: ReplyRoute,
}

impl TryFrom<ConversationRoutedInbound> for CanonicalChannelAppend {
    type Error = GatewayError;

    fn try_from(routed: ConversationRoutedInbound) -> Result<Self, Self::Error> {
        routed.validate()?;
        let reply_route = routed.message.reply_route();
        Ok(Self {
            binding: routed.binding,
            source_key: routed.source_key,
            sender_external_id: routed.message.sender,
            occurred_at: routed.message.occurred_at,
            text: routed.message.text,
            artifacts: routed
                .message
                .attachments
                .into_iter()
                .filter_map(|attachment| attachment.artifact_id)
                .collect(),
            reply_route,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalChannelAppendReceipt {
    pub binding_id: EntityId,
    pub binding_revision: Revision,
    pub conversation_id: ConversationId,
    pub event_id: EventId,
    pub source_key: StableKey,
    pub duplicate: bool,
}

/// Server-side adapter contract. Implementations resolve from authoritative storage at enqueue and
/// revalidate immediately before append; a caller-supplied binding is evidence, not authority.
pub trait CanonicalConversationIngress {
    type Error: std::error::Error + Send + Sync + 'static;

    fn resolve(
        &self,
        external: &ExternalConversationIdentity,
        now: UtcTimestamp,
    ) -> Result<ConversationBindingReference, Self::Error>;

    fn append(
        &self,
        append: CanonicalChannelAppend,
        now: UtcTimestamp,
    ) -> Result<CanonicalChannelAppendReceipt, Self::Error>;
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalChannelPublication {
    pub binding: ConversationBindingReference,
    pub event_id: EventId,
    pub stable_publication_key: StableKey,
    pub route: ReplyRoute,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OutboundMessage {
    pub route: ReplyRoute,
    pub idempotency_key: String,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
}

impl TryFrom<CanonicalChannelPublication> for OutboundMessage {
    type Error = GatewayError;

    fn try_from(publication: CanonicalChannelPublication) -> Result<Self, Self::Error> {
        let external = ExternalConversationIdentity {
            channel: publication.route.channel.clone(),
            external_account: publication.route.external_account.clone(),
            conversation: publication.route.conversation.clone(),
            thread: publication.route.thread.clone(),
        };
        if external != publication.binding.external
            || !valid_sha256(&publication.binding.policy.policy_digest_sha256)
        {
            return Err(GatewayError::BindingMismatch);
        }
        Ok(Self {
            route: publication.route,
            idempotency_key: publication.stable_publication_key.to_string(),
            text: publication.text,
            artifacts: publication.artifacts,
        })
    }
}

fn valid_sha256(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit() && !byte.is_ascii_uppercase())
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SendReceipt {
    pub platform_message_id: String,
    pub accepted_at: UtcTimestamp,
    pub duplicate_possible: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RetryClass {
    Retryable,
    RateLimited,
    Reconnect,
    Permanent,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AdapterFailure {
    pub class: RetryClass,
    pub safe_message: String,
    pub retry_after_ms: Option<u64>,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AdapterCapability {
    Attachments,
    Threads,
    Steering,
    Cancellation,
    IdempotentSend,
    Reconnect,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AdapterFeatures {
    pub capabilities: BTreeSet<AdapterCapability>,
    pub max_attachment_bytes: u64,
    pub requests_per_minute: Option<u32>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AdapterEvent {
    Inbound(Box<InboundMessage>),
    RateLimited { retry_after_ms: u64 },
    Disconnected { safe_reason: String },
}

pub trait ChannelAdapter {
    fn features(&self) -> AdapterFeatures;

    /// Receives the next normalized platform event.
    ///
    /// # Errors
    ///
    /// Returns a classified, safe adapter failure.
    fn receive(&mut self) -> Result<AdapterEvent, AdapterFailure>;

    /// Sends one outbound item and returns the platform acknowledgement.
    ///
    /// # Errors
    ///
    /// Returns a classified, safe adapter failure.
    fn send(&mut self, message: &OutboundMessage) -> Result<SendReceipt, AdapterFailure>;

    /// Re-establishes the platform stream when the adapter advertises reconnect support.
    ///
    /// # Errors
    ///
    /// Returns a classified failure suitable for bounded retry policy.
    fn reconnect(&mut self) -> Result<(), AdapterFailure>;
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct GatewayLimits {
    pub global_concurrency: usize,
    pub max_pending: usize,
    pub max_pending_per_session: usize,
    pub max_seen_messages: usize,
    pub max_attachments: usize,
    pub max_attachment_bytes: u64,
    pub max_total_attachment_bytes: u64,
}

impl Default for GatewayLimits {
    fn default() -> Self {
        Self {
            global_concurrency: 8,
            max_pending: 1_024,
            max_pending_per_session: 64,
            max_seen_messages: 4_096,
            max_attachments: 8,
            max_attachment_bytes: 25 * 1_024 * 1_024,
            max_total_attachment_bytes: 50 * 1_024 * 1_024,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EnqueueOutcome {
    Queued,
    Duplicate,
}

#[derive(Debug, Error, Eq, PartialEq)]
pub enum GatewayError {
    #[error("malformed platform event")]
    Malformed,
    #[error("gateway queue is applying backpressure")]
    Backpressure,
    #[error("attachment staging limit exceeded")]
    AttachmentLimit,
    #[error("gateway limits must all be positive")]
    InvalidLimits,
    #[error("session dispatch was not in flight")]
    NotInFlight,
    #[error("canonical conversation binding is missing or does not match the external route")]
    BindingMismatch,
    #[error("canonical conversation dispatch was not in flight")]
    ConversationNotInFlight,
}

/// Bounded fair queue for canonical conversation ingress. Ordering is serialized by canonical
/// conversation and participant, not by a legacy transcript session.
pub struct ConversationGatewayQueue {
    limits: GatewayLimits,
    pending: BTreeMap<ConversationDispatchKey, VecDeque<ConversationRoutedInbound>>,
    ready: VecDeque<ConversationDispatchKey>,
    in_flight: BTreeSet<ConversationDispatchKey>,
    seen: BTreeSet<StableKey>,
    seen_order: VecDeque<StableKey>,
    pending_count: usize,
}

impl ConversationGatewayQueue {
    pub fn new(limits: GatewayLimits) -> Result<Self, GatewayError> {
        if limits.global_concurrency == 0
            || limits.max_pending == 0
            || limits.max_pending_per_session == 0
            || limits.max_seen_messages == 0
            || limits.max_attachments == 0
            || limits.max_attachment_bytes == 0
            || limits.max_total_attachment_bytes == 0
        {
            return Err(GatewayError::InvalidLimits);
        }
        Ok(Self {
            limits,
            pending: BTreeMap::new(),
            ready: VecDeque::new(),
            in_flight: BTreeSet::new(),
            seen: BTreeSet::new(),
            seen_order: VecDeque::new(),
            pending_count: 0,
        })
    }

    pub fn enqueue(
        &mut self,
        routed: ConversationRoutedInbound,
    ) -> Result<EnqueueOutcome, GatewayError> {
        routed.validate()?;
        self.validate_attachments(&routed.message.attachments)?;
        if self.seen.contains(&routed.source_key) {
            return Ok(EnqueueOutcome::Duplicate);
        }
        let key = routed.dispatch_key();
        let queued = self.pending.get(&key).map_or(0, VecDeque::len);
        if self.pending_count >= self.limits.max_pending
            || queued >= self.limits.max_pending_per_session
        {
            return Err(GatewayError::Backpressure);
        }
        let queue = self.pending.entry(key.clone()).or_default();
        let was_empty = queue.is_empty();
        self.seen.insert(routed.source_key.clone());
        self.seen_order.push_back(routed.source_key.clone());
        queue.push_back(routed);
        self.pending_count += 1;
        if was_empty && !self.in_flight.contains(&key) {
            self.ready.push_back(key);
        }
        while self.seen_order.len() > self.limits.max_seen_messages {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        Ok(EnqueueOutcome::Queued)
    }

    pub fn take_ready(&mut self) -> Option<ConversationRoutedInbound> {
        if self.in_flight.len() >= self.limits.global_concurrency {
            return None;
        }
        while let Some(key) = self.ready.pop_front() {
            if self.in_flight.contains(&key) {
                continue;
            }
            let item = self.pending.get_mut(&key)?.pop_front()?;
            self.pending_count -= 1;
            self.in_flight.insert(key);
            return Some(item);
        }
        None
    }

    pub fn complete(&mut self, key: &ConversationDispatchKey) -> Result<(), GatewayError> {
        if !self.in_flight.remove(key) {
            return Err(GatewayError::ConversationNotInFlight);
        }
        if self.pending.get(key).is_some_and(|queue| !queue.is_empty()) {
            self.ready.push_back(key.clone());
        } else {
            self.pending.remove(key);
        }
        Ok(())
    }

    fn validate_attachments(&self, attachments: &[Attachment]) -> Result<(), GatewayError> {
        if attachments.len() > self.limits.max_attachments {
            return Err(GatewayError::AttachmentLimit);
        }
        let mut total = 0_u64;
        for attachment in attachments {
            if attachment.byte_length > self.limits.max_attachment_bytes {
                return Err(GatewayError::AttachmentLimit);
            }
            total = total
                .checked_add(attachment.byte_length)
                .ok_or(GatewayError::AttachmentLimit)?;
        }
        if total > self.limits.max_total_attachment_bytes {
            return Err(GatewayError::AttachmentLimit);
        }
        Ok(())
    }
}

pub struct GatewayQueue {
    limits: GatewayLimits,
    pending: BTreeMap<SessionId, VecDeque<RoutedInbound>>,
    ready: VecDeque<SessionId>,
    in_flight: BTreeSet<SessionId>,
    seen: BTreeSet<String>,
    seen_order: VecDeque<String>,
    pending_count: usize,
}

impl GatewayQueue {
    /// Creates a bounded per-session dispatcher.
    ///
    /// # Errors
    ///
    /// Returns an error if any capacity is zero.
    pub fn new(limits: GatewayLimits) -> Result<Self, GatewayError> {
        if limits.global_concurrency == 0
            || limits.max_pending == 0
            || limits.max_pending_per_session == 0
            || limits.max_seen_messages == 0
            || limits.max_attachments == 0
            || limits.max_attachment_bytes == 0
            || limits.max_total_attachment_bytes == 0
        {
            return Err(GatewayError::InvalidLimits);
        }
        Ok(Self {
            limits,
            pending: BTreeMap::new(),
            ready: VecDeque::new(),
            in_flight: BTreeSet::new(),
            seen: BTreeSet::new(),
            seen_order: VecDeque::new(),
            pending_count: 0,
        })
    }

    /// Validates, suppresses duplicates, and queues a normalized inbound message.
    ///
    /// # Errors
    ///
    /// Returns malformed, attachment-limit, or backpressure errors without enqueuing.
    pub fn enqueue(&mut self, routed: RoutedInbound) -> Result<EnqueueOutcome, GatewayError> {
        routed.message.validate()?;
        self.validate_attachments(&routed.message.attachments)?;
        let dedupe_key = routed.message.deduplication_key();
        if self.seen.contains(&dedupe_key) {
            return Ok(EnqueueOutcome::Duplicate);
        }
        let session_len = self
            .pending
            .get(&routed.session_id)
            .map_or(0, VecDeque::len);
        if self.pending_count >= self.limits.max_pending
            || session_len >= self.limits.max_pending_per_session
        {
            return Err(GatewayError::Backpressure);
        }
        let session_id = routed.session_id.clone();
        let queue = self.pending.entry(session_id.clone()).or_default();
        let was_empty = queue.is_empty();
        queue.push_back(routed);
        self.pending_count += 1;
        self.remember(dedupe_key);
        if was_empty && !self.in_flight.contains(&session_id) {
            self.ready.push_back(session_id);
        }
        Ok(EnqueueOutcome::Queued)
    }

    /// Takes the next message while preserving one in-flight item per session.
    pub fn take_ready(&mut self) -> Option<RoutedInbound> {
        if self.in_flight.len() >= self.limits.global_concurrency {
            return None;
        }
        while let Some(session_id) = self.ready.pop_front() {
            if self.in_flight.contains(&session_id) {
                continue;
            }
            let item = self.pending.get_mut(&session_id)?.pop_front()?;
            self.pending_count -= 1;
            self.in_flight.insert(session_id);
            return Some(item);
        }
        None
    }

    /// Releases a session and makes its next message eligible.
    ///
    /// # Errors
    ///
    /// Returns an error if the session had no active dispatch.
    pub fn complete(&mut self, session_id: &SessionId) -> Result<(), GatewayError> {
        if !self.in_flight.remove(session_id) {
            return Err(GatewayError::NotInFlight);
        }
        if self
            .pending
            .get(session_id)
            .is_some_and(|queue| !queue.is_empty())
        {
            self.ready.push_back(session_id.clone());
        } else {
            self.pending.remove(session_id);
        }
        Ok(())
    }

    pub const fn pending_count(&self) -> usize {
        self.pending_count
    }

    fn remember(&mut self, key: String) {
        self.seen.insert(key.clone());
        self.seen_order.push_back(key);
        while self.seen_order.len() > self.limits.max_seen_messages {
            if let Some(expired) = self.seen_order.pop_front() {
                self.seen.remove(&expired);
            }
        }
    }

    fn validate_attachments(&self, attachments: &[Attachment]) -> Result<(), GatewayError> {
        if attachments.len() > self.limits.max_attachments {
            return Err(GatewayError::AttachmentLimit);
        }
        let mut total = 0_u64;
        for attachment in attachments {
            if attachment.byte_length > self.limits.max_attachment_bytes {
                return Err(GatewayError::AttachmentLimit);
            }
            total = total
                .checked_add(attachment.byte_length)
                .ok_or(GatewayError::AttachmentLimit)?;
        }
        if total > self.limits.max_total_attachment_bytes {
            return Err(GatewayError::AttachmentLimit);
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ReconnectPolicy {
    pub initial_delay_ms: u64,
    pub max_delay_ms: u64,
    pub max_attempts: u32,
}

impl Default for ReconnectPolicy {
    fn default() -> Self {
        Self {
            initial_delay_ms: 100,
            max_delay_ms: 30_000,
            max_attempts: 8,
        }
    }
}

impl ReconnectPolicy {
    pub fn delay_ms(self, attempt: u32) -> Option<u64> {
        if attempt >= self.max_attempts || self.initial_delay_ms == 0 {
            return None;
        }
        let multiplier = 1_u64.checked_shl(attempt.min(63)).unwrap_or(u64::MAX);
        Some(
            self.initial_delay_ms
                .saturating_mul(multiplier)
                .min(self.max_delay_ms),
        )
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SessionAction {
    pub command_id: CommandId,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub message_id: String,
    pub command: ClientCommand,
}

impl From<RoutedInbound> for SessionAction {
    fn from(routed: RoutedInbound) -> Self {
        let route = routed.message.reply_route();
        let command = match routed.message.intent {
            InboundIntent::Prompt => ClientCommand::SubmitPrompt(SubmitPrompt {
                session_id: routed.session_id.clone(),
                text: routed.message.text,
                artifacts: routed
                    .message
                    .attachments
                    .into_iter()
                    .filter_map(|attachment| attachment.artifact_id)
                    .collect(),
                delivery: DeliveryPolicy::Immediate,
                reply_route: Some(ProtocolReplyRoute {
                    channel: route.channel,
                    external_account: Some(route.external_account),
                    conversation: route.conversation,
                    thread: route.thread,
                    reply_to_message: route.reply_to_message,
                }),
            }),
            InboundIntent::Steer => ClientCommand::Steer(SteerAction {
                session_id: routed.session_id.clone(),
                text: routed.message.text,
                delivery: DeliveryPolicy::NextTurnBoundary,
            }),
            InboundIntent::Cancel => {
                ClientCommand::Cancel(CancelTarget::Session(routed.session_id.clone()))
            }
        };
        Self {
            command_id: CommandId::new(),
            profile_id: routed.profile_id,
            session_id: routed.session_id,
            message_id: routed.message.message_id,
            command,
        }
    }
}

#[derive(Debug, Error)]
pub enum AgentConnectionError {
    #[error("agent transport failed: {0}")]
    Transport(#[from] ConnectionError),
    #[error("agent rejected the connection handshake")]
    Handshake,
    #[error("agent returned a result for a different command")]
    MismatchedResult,
}

pub struct AgentConnection<T> {
    transport: T,
    client_id: ClientId,
    protocol: keith_agent_types::ProtocolVersion,
    pending_events: VecDeque<EventEnvelope>,
}

impl<T: AgentTransport> AgentConnection<T> {
    /// Performs the real `AgentConnection` handshake over a framed transport.
    ///
    /// # Errors
    ///
    /// Returns an error when negotiation or transport fails.
    pub fn connect(mut transport: T) -> Result<Self, AgentConnectionError> {
        let client_id = ClientId::new();
        transport.send(&WireMessage::ClientHello(ClientHello {
            protocol: CURRENT_PROTOCOL_VERSION,
            client_id: client_id.clone(),
            client_name: "channel-gateway".to_owned(),
            client_version: env!("CARGO_PKG_VERSION").to_owned(),
            supported_features: BTreeSet::from([
                Feature::SessionLifecycle,
                Feature::Steering,
                Feature::DeliveryDispatch,
                Feature::AttachmentStaging,
            ]),
            resume: None,
        }))?;
        let WireMessage::ServerHello(server) = transport.receive()? else {
            return Err(AgentConnectionError::Handshake);
        };
        if server.protocol.major != CURRENT_PROTOCOL_VERSION.major
            || ![
                Feature::SessionLifecycle,
                Feature::DeliveryDispatch,
                Feature::AttachmentStaging,
            ]
            .iter()
            .all(|feature| server.supported_features.contains(feature))
        {
            return Err(AgentConnectionError::Handshake);
        }
        Ok(Self {
            transport,
            client_id,
            protocol: server.protocol,
            pending_events: VecDeque::new(),
        })
    }

    /// Submits one channel-derived action; it never invokes a model, tool, or memory reader.
    ///
    /// # Errors
    ///
    /// Returns an error for transport or command-correlation failure.
    pub fn submit(
        &mut self,
        action: &SessionAction,
        now: UtcTimestamp,
    ) -> Result<CommandResult, AgentConnectionError> {
        self.execute_with_id(
            &action.command_id,
            action.command.clone(),
            Some(action.session_id.clone()),
            now,
        )
    }

    /// Executes one gateway control command, including transactional delivery dispatch.
    ///
    /// # Errors
    ///
    /// Returns an error for transport or command-correlation failure.
    pub fn execute(
        &mut self,
        command: ClientCommand,
        session_id: Option<SessionId>,
        now: UtcTimestamp,
    ) -> Result<CommandResult, AgentConnectionError> {
        self.execute_with_id(&CommandId::new(), command, session_id, now)
    }

    /// Executes a control command with a caller-stable ID so reconnect retries remain idempotent.
    ///
    /// # Errors
    ///
    /// Returns an error for transport or command-correlation failure.
    pub fn execute_idempotent(
        &mut self,
        command_id: &CommandId,
        command: ClientCommand,
        session_id: Option<SessionId>,
        now: UtcTimestamp,
    ) -> Result<CommandResult, AgentConnectionError> {
        self.execute_with_id(command_id, command, session_id, now)
    }

    fn execute_with_id(
        &mut self,
        command_id: &CommandId,
        command: ClientCommand,
        session_id: Option<SessionId>,
        now: UtcTimestamp,
    ) -> Result<CommandResult, AgentConnectionError> {
        self.transport.send(&WireMessage::Command(CommandEnvelope {
            protocol: self.protocol,
            command_id: command_id.clone(),
            client_id: self.client_id.clone(),
            sent_at: now,
            session_id,
            command,
        }))?;
        loop {
            match self.transport.receive()? {
                WireMessage::CommandResult(result) if &result.command_id == command_id => {
                    return Ok(result.result);
                }
                WireMessage::CommandResult(_) => {
                    return Err(AgentConnectionError::MismatchedResult);
                }
                message @ (WireMessage::Event(_)
                | WireMessage::Snapshot(_)
                | WireMessage::Terminal(_)) => {
                    if let Some(event) = message.into_event() {
                        self.pending_events.push_back(event);
                    }
                }
                WireMessage::ClientHello(_)
                | WireMessage::ServerHello(_)
                | WireMessage::Command(_) => return Err(AgentConnectionError::Handshake),
            }
        }
    }

    pub fn take_event(&mut self) -> Option<EventEnvelope> {
        self.pending_events.pop_front()
    }
}

#[cfg(test)]
mod tests {
    use std::thread;

    use keith_agent_types::{EntityId, ProtocolVersion};
    use keith_connection::{FramedTransport, local_stream_pair};
    use keith_protocol::{CommandResultEnvelope, ResumeMode, ServerHello, WireFormat};

    use super::*;

    fn message(session_id: SessionId, message_id: &str, occurred_at: i64) -> RoutedInbound {
        RoutedInbound {
            profile_id: ProfileId::new(),
            session_id,
            message: InboundMessage {
                channel: "conformance".to_owned(),
                external_account: "account".to_owned(),
                conversation: "conversation".to_owned(),
                thread: None,
                sender: "sender".to_owned(),
                message_id: message_id.to_owned(),
                reply_target: None,
                text: message_id.to_owned(),
                attachments: Vec::new(),
                occurred_at: UtcTimestamp::from_unix_millis(occurred_at),
                intent: InboundIntent::Prompt,
            },
        }
    }

    #[test]
    fn queues_in_arrival_order_suppresses_duplicates_and_applies_backpressure() {
        let limits = GatewayLimits {
            global_concurrency: 2,
            max_pending: 3,
            max_pending_per_session: 2,
            max_seen_messages: 3,
            max_attachments: 1,
            max_attachment_bytes: 4,
            max_total_attachment_bytes: 4,
        };
        let mut queue = GatewayQueue::new(limits).expect("valid limits");
        let first_session = SessionId::new();
        let second_session = SessionId::new();
        queue
            .enqueue(message(first_session.clone(), "first", 20))
            .expect("first");
        queue
            .enqueue(message(first_session.clone(), "second", 10))
            .expect("reordered timestamp remains second");
        assert_eq!(
            queue
                .enqueue(message(first_session.clone(), "first", 20))
                .expect("duplicate"),
            EnqueueOutcome::Duplicate
        );
        queue
            .enqueue(message(second_session.clone(), "third", 30))
            .expect("second session");
        assert_eq!(
            queue.enqueue(message(second_session.clone(), "fourth", 40)),
            Err(GatewayError::Backpressure)
        );
        let first = queue.take_ready().expect("first session ready");
        let third = queue
            .take_ready()
            .expect("second session concurrently ready");
        assert_eq!(first.message.message_id, "first");
        assert_eq!(third.message.message_id, "third");
        assert!(queue.take_ready().is_none());
        queue.complete(&first_session).expect("release first");
        let second = queue.take_ready().expect("next first-session item");
        assert_eq!(second.message.message_id, "second");

        let mut oversized = message(SessionId::new(), "oversized", 50);
        oversized.message.attachments.push(Attachment {
            id: "attachment".to_owned(),
            file_name: "large.bin".to_owned(),
            media_type: "application/octet-stream".to_owned(),
            byte_length: 5,
            artifact_id: None,
            download_url: None,
            staging_file: None,
            sha256: None,
        });
        assert_eq!(queue.enqueue(oversized), Err(GatewayError::AttachmentLimit));
    }

    #[test]
    fn reconnect_backoff_is_bounded() {
        let policy = ReconnectPolicy {
            initial_delay_ms: 10,
            max_delay_ms: 25,
            max_attempts: 4,
        };
        assert_eq!(
            (0..=4)
                .map(|attempt| policy.delay_ms(attempt))
                .collect::<Vec<_>>(),
            vec![Some(10), Some(20), Some(25), Some(25), None]
        );
    }

    #[test]
    fn agent_connection_submits_over_real_framed_socket() {
        let (client, server) = local_stream_pair().expect("local socket pair");
        let server_thread = thread::spawn(move || {
            let mut transport = FramedTransport::new(server, WireFormat::Json);
            let WireMessage::ClientHello(hello) = transport.receive().expect("client hello") else {
                panic!("hello required");
            };
            transport
                .send(&WireMessage::ServerHello(ServerHello {
                    protocol: ProtocolVersion::new(1, 0),
                    server_instance_id: EntityId::new(),
                    supported_features: hello.supported_features,
                    current_generation: None,
                    resume_mode: ResumeMode::Fresh,
                }))
                .expect("server hello");
            let WireMessage::Command(command) = transport.receive().expect("command") else {
                panic!("command required");
            };
            assert!(matches!(command.command, ClientCommand::SubmitPrompt(_)));
            transport
                .send(&WireMessage::CommandResult(CommandResultEnvelope {
                    protocol: ProtocolVersion::new(1, 0),
                    command_id: command.command_id,
                    completed_at: UtcTimestamp::UNIX_EPOCH,
                    result: CommandResult::Accepted { action_id: None },
                }))
                .expect("command result");
        });
        let transport = FramedTransport::new(client, WireFormat::Json);
        let mut connection = AgentConnection::connect(transport).expect("connect");
        let action = SessionAction::from(message(SessionId::new(), "message", 0));
        assert!(matches!(
            connection
                .submit(&action, UtcTimestamp::UNIX_EPOCH)
                .expect("submit"),
            CommandResult::Accepted { .. }
        ));
        server_thread.join().expect("server completes");
    }
}
