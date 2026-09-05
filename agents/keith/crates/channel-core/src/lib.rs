#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};

use keith_agent_types::{
    ArtifactId, CURRENT_PROTOCOL_VERSION, ClientId, CommandId, ProfileId, SessionId, UtcTimestamp,
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
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutedInbound {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub message: InboundMessage,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OutboundMessage {
    pub route: ReplyRoute,
    pub idempotency_key: String,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
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

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelContractVersion {
    pub major: u16,
    pub minor: u16,
}

impl ChannelContractVersion {
    pub const fn new(major: u16, minor: u16) -> Self {
        Self { major, minor }
    }

    pub const fn is_compatible_with(self, required: Self) -> bool {
        self.major == required.major && self.minor >= required.minor
    }
}

pub const CHANNEL_CONTRACT_V2: ChannelContractVersion = ChannelContractVersion::new(2, 0);

/// Selects the stable v2 contract only when the peer advertises a compatible version.
///
/// # Errors
///
/// Returns an unsupported-feature error rather than silently downgrading or translating versions.
pub fn negotiate_channel_contract_v2(
    peer_versions: &[ChannelContractVersion],
) -> Result<ChannelContractVersion, ChannelAdapterErrorV2> {
    if peer_versions
        .iter()
        .any(|version| version.is_compatible_with(CHANNEL_CONTRACT_V2))
    {
        Ok(CHANNEL_CONTRACT_V2)
    } else {
        Err(ChannelAdapterErrorV2::unsupported(
            "channel contract v2 is not supported by the peer",
        ))
    }
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelCapabilityV2 {
    InboundMessages,
    OutboundMessages,
    Threads,
    Replies,
    Mentions,
    Commands,
    MessageEdits,
    MessageDeletion,
    Reactions,
    Attachments,
    Voice,
    RichContent,
    Typing,
    DeliveryReceipts,
    ReadReceipts,
    RateLimits,
    Reconnect,
    Cancellation,
    IdempotentSend,
}

impl ChannelCapabilityV2 {
    pub const ALL: [Self; 19] = [
        Self::InboundMessages,
        Self::OutboundMessages,
        Self::Threads,
        Self::Replies,
        Self::Mentions,
        Self::Commands,
        Self::MessageEdits,
        Self::MessageDeletion,
        Self::Reactions,
        Self::Attachments,
        Self::Voice,
        Self::RichContent,
        Self::Typing,
        Self::DeliveryReceipts,
        Self::ReadReceipts,
        Self::RateLimits,
        Self::Reconnect,
        Self::Cancellation,
        Self::IdempotentSend,
    ];
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "support")]
pub enum ChannelCapabilitySupportV2 {
    Supported,
    Unsupported { safe_reason: String },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelCapabilitiesV2 {
    pub contract: ChannelContractVersion,
    pub declarations: BTreeMap<ChannelCapabilityV2, ChannelCapabilitySupportV2>,
    pub max_event_bytes: u64,
    pub max_attachment_bytes: u64,
    pub max_attachments: usize,
    pub max_rich_content_bytes: u64,
    pub requests_per_minute: Option<u32>,
}

impl ChannelCapabilitiesV2 {
    pub fn supports(&self, capability: ChannelCapabilityV2) -> bool {
        matches!(
            self.declarations.get(&capability),
            Some(ChannelCapabilitySupportV2::Supported)
        )
    }

    /// Validates that every v2 feature is truthfully declared and all bounds are operative.
    ///
    /// # Errors
    ///
    /// Returns a malformed-contract error for an incompatible version, missing declaration,
    /// blank limitation, or disabled resource bound.
    pub fn validate(&self) -> Result<(), ChannelAdapterErrorV2> {
        if !self.contract.is_compatible_with(CHANNEL_CONTRACT_V2)
            || self.max_event_bytes == 0
            || self.max_attachment_bytes == 0
            || self.max_attachments == 0
            || self.max_rich_content_bytes == 0
            || self.requests_per_minute == Some(0)
            || ChannelCapabilityV2::ALL
                .iter()
                .any(|capability| !self.declarations.contains_key(capability))
            || self.declarations.values().any(|support| {
                matches!(
                    support,
                    ChannelCapabilitySupportV2::Unsupported { safe_reason }
                        if safe_reason.trim().is_empty()
                )
            })
        {
            return Err(ChannelAdapterErrorV2::malformed(
                "channel capability declaration is incomplete or invalid",
            ));
        }
        Ok(())
    }

    /// Refuses an operation unless the adapter explicitly declared support.
    ///
    /// # Errors
    ///
    /// Returns an unsupported-feature error for a missing or unsupported declaration.
    pub fn require(&self, capability: ChannelCapabilityV2) -> Result<(), ChannelAdapterErrorV2> {
        match self.declarations.get(&capability) {
            Some(ChannelCapabilitySupportV2::Supported) => Ok(()),
            Some(ChannelCapabilitySupportV2::Unsupported { safe_reason }) => {
                Err(ChannelAdapterErrorV2::unsupported(safe_reason))
            }
            None => Err(ChannelAdapterErrorV2::unsupported(
                "channel capability was not declared",
            )),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelConnectionHealthV2 {
    Disconnected,
    Connected,
    RateLimited,
    Revoked,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct ChannelAccountSetupV2 {
    pub account_id: String,
    pub required_credential_names: BTreeSet<String>,
    pub required_scopes: BTreeSet<String>,
    pub webhook_configured: bool,
    pub socket_or_polling_configured: bool,
    pub connection_health: ChannelConnectionHealthV2,
    pub reconnect_cursor_present: bool,
    pub safe_test_supported: bool,
    pub metadata: BTreeMap<String, String>,
}

impl ChannelAccountSetupV2 {
    /// Ensures setup diagnostics contain stable names and never pretend an ingress path exists.
    ///
    /// # Errors
    ///
    /// Returns a malformed-contract error for blank account, credential, or scope identifiers,
    /// or when neither webhook nor socket/polling ingress is configured.
    pub fn validate(&self) -> Result<(), ChannelAdapterErrorV2> {
        if self.account_id.trim().is_empty()
            || self
                .required_credential_names
                .iter()
                .any(|name| name.trim().is_empty())
            || self
                .required_scopes
                .iter()
                .any(|scope| scope.trim().is_empty())
            || (!self.webhook_configured && !self.socket_or_polling_configured)
        {
            return Err(ChannelAdapterErrorV2::malformed(
                "channel account setup diagnostics are invalid",
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelAdapterErrorKindV2 {
    Authentication,
    Permission,
    MalformedEvent,
    RateLimit,
    TransientNetwork,
    PermanentDestination,
    UnsupportedFeature,
    UncertainAcknowledgement,
    StaleCursor,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAdapterErrorV2 {
    pub kind: ChannelAdapterErrorKindV2,
    pub safe_message: String,
    pub retry_after_ms: Option<u64>,
}

impl ChannelAdapterErrorV2 {
    pub fn malformed(message: impl Into<String>) -> Self {
        Self {
            kind: ChannelAdapterErrorKindV2::MalformedEvent,
            safe_message: message.into(),
            retry_after_ms: None,
        }
    }

    pub fn unsupported(message: impl Into<String>) -> Self {
        Self {
            kind: ChannelAdapterErrorKindV2::UnsupportedFeature,
            safe_message: message.into(),
            retry_after_ms: None,
        }
    }

    pub fn is_retryable(&self) -> bool {
        matches!(
            self.kind,
            ChannelAdapterErrorKindV2::RateLimit
                | ChannelAdapterErrorKindV2::TransientNetwork
                | ChannelAdapterErrorKindV2::UncertainAcknowledgement
        )
    }
}

impl From<AdapterFailure> for ChannelAdapterErrorV2 {
    fn from(failure: AdapterFailure) -> Self {
        let kind = match failure.class {
            RetryClass::Retryable | RetryClass::Reconnect => {
                ChannelAdapterErrorKindV2::TransientNetwork
            }
            RetryClass::RateLimited => ChannelAdapterErrorKindV2::RateLimit,
            RetryClass::Permanent => ChannelAdapterErrorKindV2::PermanentDestination,
        };
        Self {
            kind,
            safe_message: failure.safe_message,
            retry_after_ms: failure.retry_after_ms,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelConversationKindV2 {
    Direct,
    GroupDirect,
    Channel,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelIdentityV2 {
    pub platform_id: String,
    pub display_name: Option<String>,
    pub is_bot: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelConversationV2 {
    pub platform_id: String,
    pub kind: ChannelConversationKindV2,
    pub thread_id: Option<String>,
    pub reply_to_message_id: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelMentionV2 {
    pub identity: ChannelIdentityV2,
    pub start: Option<usize>,
    pub end: Option<usize>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelAttachmentKindV2 {
    File,
    Image,
    Audio,
    Voice,
    Video,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAttachmentV2 {
    pub attachment: Attachment,
    pub kind: ChannelAttachmentKindV2,
    pub duration_ms: Option<u64>,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelRichContentV2 {
    pub kind: String,
    pub text: String,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelMessageV2 {
    pub message_id: String,
    pub account_id: String,
    pub conversation: ChannelConversationV2,
    pub sender: ChannelIdentityV2,
    pub text: String,
    pub attachments: Vec<ChannelAttachmentV2>,
    pub rich_content: Vec<ChannelRichContentV2>,
    pub mentions: Vec<ChannelMentionV2>,
    pub occurred_at: UtcTimestamp,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelMessageEditV2 {
    pub message_id: String,
    pub account_id: String,
    pub conversation: ChannelConversationV2,
    pub editor: ChannelIdentityV2,
    pub text: String,
    pub occurred_at: UtcTimestamp,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelMessageDeleteV2 {
    pub message_id: String,
    pub account_id: String,
    pub conversation: ChannelConversationV2,
    pub actor: Option<ChannelIdentityV2>,
    pub occurred_at: UtcTimestamp,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelReactionActionV2 {
    Added,
    Removed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelReactionV2 {
    pub message_id: String,
    pub account_id: String,
    pub conversation: ChannelConversationV2,
    pub actor: ChannelIdentityV2,
    pub reaction: String,
    pub action: ChannelReactionActionV2,
    pub occurred_at: UtcTimestamp,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelCommandV2 {
    pub command_id: String,
    pub account_id: String,
    pub conversation: ChannelConversationV2,
    pub sender: ChannelIdentityV2,
    pub name: String,
    pub arguments: String,
    pub occurred_at: UtcTimestamp,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ChannelReceiptStateV2 {
    Accepted,
    Delivered,
    Read,
    Failed,
    Cancelled,
    Uncertain,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event", content = "payload")]
pub enum ChannelEventKindV2 {
    MessageCreated(ChannelMessageV2),
    MessageEdited(ChannelMessageEditV2),
    MessageDeleted(ChannelMessageDeleteV2),
    Reaction(ChannelReactionV2),
    Command(ChannelCommandV2),
    Typing {
        account_id: String,
        conversation: ChannelConversationV2,
        actor: ChannelIdentityV2,
    },
    Receipt {
        account_id: String,
        conversation: ChannelConversationV2,
        platform_message_id: String,
        state: ChannelReceiptStateV2,
    },
    RateLimited {
        retry_after_ms: u64,
    },
    ReconnectRequired {
        safe_reason: String,
    },
    CancellationRequested {
        cancellation_id: String,
    },
}

impl ChannelEventKindV2 {
    pub const fn required_capability(&self) -> ChannelCapabilityV2 {
        match self {
            Self::MessageCreated(_) => ChannelCapabilityV2::InboundMessages,
            Self::MessageEdited(_) => ChannelCapabilityV2::MessageEdits,
            Self::MessageDeleted(_) => ChannelCapabilityV2::MessageDeletion,
            Self::Reaction(_) => ChannelCapabilityV2::Reactions,
            Self::Command(_) => ChannelCapabilityV2::Commands,
            Self::Typing { .. } => ChannelCapabilityV2::Typing,
            Self::Receipt { .. } => ChannelCapabilityV2::DeliveryReceipts,
            Self::RateLimited { .. } => ChannelCapabilityV2::RateLimits,
            Self::ReconnectRequired { .. } => ChannelCapabilityV2::Reconnect,
            Self::CancellationRequested { .. } => ChannelCapabilityV2::Cancellation,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelEventV2 {
    pub contract: ChannelContractVersion,
    pub event_id: String,
    pub delivery_attempt: u32,
    pub event: ChannelEventKindV2,
    pub metadata: BTreeMap<String, String>,
}

impl ChannelEventV2 {
    /// Checks version, identity, delivery attempt, and feature declaration before admission.
    ///
    /// # Errors
    ///
    /// Returns malformed or unsupported-feature errors without dispatching the event.
    pub fn validate(
        &self,
        capabilities: &ChannelCapabilitiesV2,
    ) -> Result<(), ChannelAdapterErrorV2> {
        capabilities.validate()?;
        if !self.contract.is_compatible_with(CHANNEL_CONTRACT_V2)
            || self.event_id.trim().is_empty()
            || self.delivery_attempt == 0
        {
            return Err(ChannelAdapterErrorV2::malformed(
                "channel event envelope is malformed",
            ));
        }
        capabilities.require(self.event.required_capability())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelOutboundMessageV2 {
    pub route: ReplyRoute,
    pub idempotency_key: String,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
    pub rich_content: Vec<ChannelRichContentV2>,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "operation", content = "payload")]
pub enum ChannelOperationV2 {
    SendMessage(ChannelOutboundMessageV2),
    EditMessage {
        route: ReplyRoute,
        platform_message_id: String,
        text: String,
        rich_content: Vec<ChannelRichContentV2>,
    },
    DeleteMessage {
        route: ReplyRoute,
        platform_message_id: String,
    },
    AddReaction {
        route: ReplyRoute,
        platform_message_id: String,
        reaction: String,
    },
    RemoveReaction {
        route: ReplyRoute,
        platform_message_id: String,
        reaction: String,
    },
    SetTyping {
        route: ReplyRoute,
        active: bool,
    },
    Cancel {
        cancellation_id: String,
    },
}

impl ChannelOperationV2 {
    pub const fn required_capability(&self) -> ChannelCapabilityV2 {
        match self {
            Self::SendMessage(_) => ChannelCapabilityV2::OutboundMessages,
            Self::EditMessage { .. } => ChannelCapabilityV2::MessageEdits,
            Self::DeleteMessage { .. } => ChannelCapabilityV2::MessageDeletion,
            Self::AddReaction { .. } | Self::RemoveReaction { .. } => {
                ChannelCapabilityV2::Reactions
            }
            Self::SetTyping { .. } => ChannelCapabilityV2::Typing,
            Self::Cancel { .. } => ChannelCapabilityV2::Cancellation,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelOperationReceiptV2 {
    pub operation_id: String,
    pub platform_message_id: Option<String>,
    pub accepted_at: UtcTimestamp,
    pub state: ChannelReceiptStateV2,
    pub duplicate_possible: bool,
    pub metadata: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ReconnectCursorV2 {
    pub value: String,
    pub observed_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RawWebhookRequestV2<'a> {
    pub timestamp_seconds: i64,
    pub signature: &'a str,
    pub body: &'a [u8],
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VerifiedWebhookRequestV2<'a> {
    body: &'a [u8],
}

impl<'a> VerifiedWebhookRequestV2<'a> {
    /// Verifies timestamp freshness and signature against the raw body before parsing is possible.
    ///
    /// # Errors
    ///
    /// Returns authentication for a stale timestamp, unsupported signature version, or mismatch.
    pub fn verify(
        request: RawWebhookRequestV2<'a>,
        now_seconds: i64,
        max_clock_skew_seconds: i64,
        verifier: impl FnOnce(i64, &[u8], &str) -> bool,
    ) -> Result<Self, ChannelAdapterErrorV2> {
        if max_clock_skew_seconds <= 0
            || now_seconds.abs_diff(request.timestamp_seconds)
                > u64::try_from(max_clock_skew_seconds).unwrap_or(0)
            || !request.signature.starts_with("v0=")
            || !verifier(request.timestamp_seconds, request.body, request.signature)
        {
            return Err(ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::Authentication,
                safe_message: "channel webhook signature verification failed".to_owned(),
                retry_after_ms: None,
            });
        }
        Ok(Self { body: request.body })
    }

    pub const fn body(&self) -> &[u8] {
        self.body
    }
}

pub struct ChannelConformanceV2;

impl ChannelConformanceV2 {
    /// Applies the common contract checks to a normalized event.
    ///
    /// # Errors
    ///
    /// Returns malformed or unsupported-feature errors for non-conforming events.
    pub fn admit_event(
        capabilities: &ChannelCapabilitiesV2,
        event: &ChannelEventV2,
    ) -> Result<(), ChannelAdapterErrorV2> {
        event.validate(capabilities)
    }

    /// Rejects absent, blank, or stale reconnect cursors.
    ///
    /// # Errors
    ///
    /// Returns a stale-cursor error when durable reconnect cannot safely continue.
    pub fn admit_cursor(
        cursor: &ReconnectCursorV2,
        now: UtcTimestamp,
        max_age_ms: u64,
    ) -> Result<(), ChannelAdapterErrorV2> {
        let age = now.unix_millis().abs_diff(cursor.observed_at.unix_millis());
        if cursor.value.trim().is_empty() || max_age_ms == 0 || age > max_age_ms {
            return Err(ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::StaleCursor,
                safe_message: "channel reconnect cursor is stale".to_owned(),
                retry_after_ms: None,
            });
        }
        Ok(())
    }

    /// Checks that classified errors are safe and internally consistent.
    ///
    /// # Errors
    ///
    /// Returns a malformed error when a safe message is absent or retry metadata is invalid.
    pub fn admit_error(error: &ChannelAdapterErrorV2) -> Result<(), ChannelAdapterErrorV2> {
        if error.safe_message.trim().is_empty()
            || (error.retry_after_ms.is_some()
                && error.kind != ChannelAdapterErrorKindV2::RateLimit)
            || (error.kind == ChannelAdapterErrorKindV2::RateLimit
                && error.retry_after_ms == Some(0))
        {
            return Err(ChannelAdapterErrorV2::malformed(
                "channel adapter error classification is invalid",
            ));
        }
        Ok(())
    }
}

pub trait ChannelAdapterV2 {
    fn capabilities_v2(&self) -> ChannelCapabilitiesV2;

    /// Receives one verified and normalized v2 channel event.
    ///
    /// # Errors
    ///
    /// Returns a classified adapter error without invoking a model or tool.
    fn receive_v2(&mut self) -> Result<ChannelEventV2, ChannelAdapterErrorV2>;

    /// Executes one capability-checked outbox operation.
    ///
    /// # Errors
    ///
    /// Returns a classified adapter error, including uncertain acknowledgement.
    fn execute_v2(
        &mut self,
        operation: &ChannelOperationV2,
    ) -> Result<ChannelOperationReceiptV2, ChannelAdapterErrorV2>;

    /// Reconnects using adapter-owned durable state.
    ///
    /// # Errors
    ///
    /// Returns a classified connection or stale-cursor error.
    fn reconnect_v2(&mut self) -> Result<(), ChannelAdapterErrorV2>;

    fn reconnect_cursor_v2(&self) -> Option<ReconnectCursorV2>;
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

    fn slack_capabilities() -> ChannelCapabilitiesV2 {
        let declarations = ChannelCapabilityV2::ALL
            .into_iter()
            .map(|capability| {
                let support = if matches!(
                    capability,
                    ChannelCapabilityV2::Typing | ChannelCapabilityV2::ReadReceipts
                ) {
                    ChannelCapabilitySupportV2::Unsupported {
                        safe_reason: "Slack does not expose this feature to bot applications"
                            .to_owned(),
                    }
                } else {
                    ChannelCapabilitySupportV2::Supported
                };
                (capability, support)
            })
            .collect();
        ChannelCapabilitiesV2 {
            contract: CHANNEL_CONTRACT_V2,
            declarations,
            max_event_bytes: 1_024,
            max_attachment_bytes: 1_024,
            max_attachments: 4,
            max_rich_content_bytes: 1_024,
            requests_per_minute: Some(50),
        }
    }

    #[test]
    fn slack_v2_conformance_requires_complete_truthful_capability_declarations() {
        assert_eq!(
            negotiate_channel_contract_v2(&[
                ChannelContractVersion::new(1, 4),
                ChannelContractVersion::new(2, 1),
            ])
            .expect("compatible v2 peer"),
            CHANNEL_CONTRACT_V2
        );
        assert_eq!(
            negotiate_channel_contract_v2(&[ChannelContractVersion::new(1, 9)])
                .expect_err("no silent v1 downgrade")
                .kind,
            ChannelAdapterErrorKindV2::UnsupportedFeature
        );
        let capabilities = slack_capabilities();
        capabilities.validate().expect("complete declaration");
        let setup = ChannelAccountSetupV2 {
            account_id: "T123".to_owned(),
            required_credential_names: BTreeSet::from(["bot_token".to_owned()]),
            required_scopes: BTreeSet::from(["chat:write".to_owned()]),
            webhook_configured: true,
            socket_or_polling_configured: false,
            connection_health: ChannelConnectionHealthV2::Disconnected,
            reconnect_cursor_present: false,
            safe_test_supported: true,
            metadata: BTreeMap::new(),
        };
        setup.validate().expect("truthful account setup");
        let mut unavailable = setup;
        unavailable.webhook_configured = false;
        assert_eq!(
            unavailable
                .validate()
                .expect_err("account without ingress is invalid")
                .kind,
            ChannelAdapterErrorKindV2::MalformedEvent
        );
        assert!(capabilities.supports(ChannelCapabilityV2::Threads));
        assert_eq!(
            capabilities
                .require(ChannelCapabilityV2::Typing)
                .expect_err("typing must remain unsupported")
                .kind,
            ChannelAdapterErrorKindV2::UnsupportedFeature
        );

        let mut incomplete = capabilities;
        incomplete
            .declarations
            .remove(&ChannelCapabilityV2::MessageDeletion);
        assert_eq!(
            incomplete
                .validate()
                .expect_err("undeclared behavior denied")
                .kind,
            ChannelAdapterErrorKindV2::MalformedEvent
        );
    }

    #[test]
    fn slack_v2_webhook_must_be_verified_before_body_access() {
        let body = br#"{"type":"event_callback"}"#;
        let request = RawWebhookRequestV2 {
            timestamp_seconds: 100,
            signature: "v0=trusted",
            body,
        };
        let verified =
            VerifiedWebhookRequestV2::verify(request, 110, 300, |timestamp, raw, sig| {
                timestamp == 100 && raw == body && sig == "v0=trusted"
            })
            .expect("verified raw body");
        assert_eq!(verified.body(), body);

        let stale = RawWebhookRequestV2 {
            timestamp_seconds: 100,
            signature: "v0=trusted",
            body,
        };
        let mut verifier_called = false;
        let error = VerifiedWebhookRequestV2::verify(stale, 1_000, 300, |_, _, _| {
            verifier_called = true;
            true
        })
        .expect_err("stale request denied before verification");
        assert_eq!(error.kind, ChannelAdapterErrorKindV2::Authentication);
        assert!(!verifier_called);
    }

    #[test]
    fn slack_v2_conformance_covers_events_errors_and_stale_reconnect_cursors() {
        let capabilities = slack_capabilities();
        let event = ChannelEventV2 {
            contract: CHANNEL_CONTRACT_V2,
            event_id: "Ev01".to_owned(),
            delivery_attempt: 1,
            event: ChannelEventKindV2::RateLimited {
                retry_after_ms: 1_000,
            },
            metadata: BTreeMap::new(),
        };
        ChannelConformanceV2::admit_event(&capabilities, &event).expect("declared event");
        ChannelConformanceV2::admit_error(&ChannelAdapterErrorV2 {
            kind: ChannelAdapterErrorKindV2::RateLimit,
            safe_message: "Slack rate limit reached".to_owned(),
            retry_after_ms: Some(1_000),
        })
        .expect("classified rate limit");

        let stale = ReconnectCursorV2 {
            value: "Ev01".to_owned(),
            observed_at: UtcTimestamp::from_unix_millis(1_000),
        };
        assert_eq!(
            ChannelConformanceV2::admit_cursor(
                &stale,
                UtcTimestamp::from_unix_millis(11_001),
                10_000,
            )
            .expect_err("stale cursor denied")
            .kind,
            ChannelAdapterErrorKindV2::StaleCursor
        );
    }
}
